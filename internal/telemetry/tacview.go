package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Object struct {
	ID          string
	Type        string
	Name        string
	Pilot       string
	CallSign    string
	Coalition   string
	Latitude    float64
	Longitude   float64
	Altitude    float64
	AGL         float64
	Heading     float64
	Speed       float64 // m/s — ground speed on the pad/taxi; IAS once airborne
	IAS         float64 // m/s, 0 if never sent
	GS          float64 // m/s from Tacview property (often wrong on the ground)
	MotionGS    float64 // m/s from lat/lon movement — truth for parked/taxi
	MotionVS    float64 // m/s climb (+) / descent (−) from altitude change
	OnGround    bool
	HasOnGround bool
	LandingGear float64
	Throttle    float64 // 0–1+, 0 if never sent
	HasThrottle bool
	EngineRPM   float64
	HasRPM      bool
	Afterburner float64
	Flaps       float64
	Pitch       float64
	LastSeen    time.Time
	Raw         map[string]string

	prevLat float64
	prevLon float64
	prevAlt float64
	prevT   time.Time
	hasPrev bool
}

type Client struct {
	addr     string
	password string
	log      *slog.Logger

	mu      sync.RWMutex
	objects map[string]*Object
	refLat  float64
	refLon  float64
	hasRef  bool

	onUpdate func(*Object)

	// announced remembers the last type|name|pilot printed for each id.
	announced map[string]string
}

func NewClient(addr, password string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		addr:     addr,
		password: password,
		log:      log,
		objects:   make(map[string]*Object),
		announced: make(map[string]string),
	}
}

func (c *Client) OnUpdate(fn func(*Object)) {
	c.onUpdate = fn
}

func (c *Client) Objects() []*Object {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Object, 0, len(c.objects))
	for _, o := range c.objects {
		out = append(out, o)
	}
	return out
}

func (c *Client) Get(id string) (*Object, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.objects[id]
	return o, ok
}

func (c *Client) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := c.connectAndRead(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("telemetry connection error, reconnecting in 5s", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (c *Client) connectAndRead(ctx context.Context) error {
	c.log.Info("connecting to Tacview real-time telemetry", "addr", c.addr)

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	handshake := "XtraLib.Stream.0\nTacview.RealTimeTelemetry.0\nSkyControl\n0\x00"
	if _, err := conn.Write([]byte(handshake)); err != nil {
		return fmt.Errorf("sending handshake: %w", err)
	}

	c.log.Info("handshake sent, waiting for ACMI stream")

	reader := bufio.NewReader(conn)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

		c.handleLine(line)
	}
}

func (c *Client) handleLine(line string) {
	if strings.HasPrefix(line, "FileType=") ||
		strings.HasPrefix(line, "FileVersion=") ||
		strings.HasPrefix(line, "#") {
		return
	}

	if strings.HasPrefix(line, "0,") {
		c.parseGlobal(strings.TrimPrefix(line, "0,"))
		return
	}

	if strings.HasPrefix(line, "-") {
		id := strings.TrimPrefix(line, "-")
		c.mu.Lock()
		delete(c.objects, id)
		delete(c.announced, id)
		c.mu.Unlock()
		return
	}

	parts := strings.SplitN(line, ",", 2)
	if len(parts) < 1 {
		return
	}
	id := parts[0]
	if id == "" {
		return
	}

	c.mu.Lock()
	obj, exists := c.objects[id]
	if !exists {
		obj = &Object{
			ID:  id,
			Raw: make(map[string]string),
		}
		c.objects[id] = obj
	}
	obj.LastSeen = time.Now()

	if len(parts) > 1 {
		c.parseProperties(obj, parts[1])
	}
	c.noteObjectLocked(obj)
	c.mu.Unlock()

	if c.onUpdate != nil {
		c.onUpdate(obj)
	}
}

func (c *Client) parseGlobal(props string) {
	for _, p := range strings.Split(props, ",") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ReferenceLatitude":
			if v, err := strconv.ParseFloat(kv[1], 64); err == nil {
				c.mu.Lock()
				c.refLat = v
				c.hasRef = true
				c.fixOffsetObjectsLocked()
				c.mu.Unlock()
				c.log.Info("telemetry map reference latitude", "lat", v)
			}
		case "ReferenceLongitude":
			if v, err := strconv.ParseFloat(kv[1], 64); err == nil {
				c.mu.Lock()
				c.refLon = v
				c.hasRef = true
				c.fixOffsetObjectsLocked()
				c.mu.Unlock()
				c.log.Info("telemetry map reference longitude", "lon", v)
			}
		}
	}
}

func (c *Client) fixOffsetObjectsLocked() {
	if !c.hasRef {
		return
	}
	for _, obj := range c.objects {
		if obj.Latitude > -20 && obj.Latitude < 20 {
			obj.Latitude += c.refLat
		}
		if obj.Longitude > -20 && obj.Longitude < 20 {
			obj.Longitude += c.refLon
		}
	}
}

func (c *Client) parseProperties(obj *Object, props string) {
	for _, p := range strings.Split(props, ",") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		obj.Raw[key] = val

		switch key {
		case "T":
			c.parseTransform(obj, val)
		case "Type":
			obj.Type = val
		case "Name":
			obj.Name = val
		case "Pilot":
			obj.Pilot = val
		case "CallSign":
			obj.CallSign = val
			if obj.Pilot == "" {
				obj.Pilot = val
			}
		case "Coalition":
			obj.Coalition = val
		case "Color":
			if obj.Coalition == "" {
				obj.Coalition = val
			}
		case "IAS", "CAS":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.IAS = v
			}
		case "TAS":
			if obj.IAS == 0 {
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					obj.IAS = v
				}
			}
		case "GS":
			if v, err := strconv.ParseFloat(val, 64); err == nil && v > 0 {
				obj.GS = v
			}
		case "OnGround":
			obj.HasOnGround = true
			obj.OnGround = val == "1" || strings.EqualFold(val, "true")
		case "LandingGear":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.LandingGear = v
			}
		case "AGL":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.AGL = v
			}
		case "HDG", "HDM":
			if obj.Heading == 0 {
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					obj.Heading = v
				}
			}
		case "Throttle", "Throttle2":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.HasThrottle = true
				if v > obj.Throttle {
					obj.Throttle = v
				}
			}
		case "EngineRPM", "EngineRPM2":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.HasRPM = true
				if v > obj.EngineRPM {
					obj.EngineRPM = v
				}
			}
		case "Afterburner":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.Afterburner = v
			}
		case "Flaps":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				obj.Flaps = v
			}
		}
	}
	obj.refreshSpeed()
}

func (c *Client) parseTransform(obj *Object, t string) {
	parts := strings.Split(t, "|")
	if len(parts) >= 1 && parts[0] != "" {
		if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
			obj.Longitude = v
		}
	}
	if len(parts) >= 2 && parts[1] != "" {
		if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
			obj.Latitude = v
		}
	}
	if c.hasRef {
		if obj.Latitude > -20 && obj.Latitude < 20 {
			obj.Latitude += c.refLat
		}
		if obj.Longitude > -20 && obj.Longitude < 20 {
			obj.Longitude += c.refLon
		}
	}
	if len(parts) >= 3 && parts[2] != "" {
		if v, err := strconv.ParseFloat(parts[2], 64); err == nil {
			obj.Altitude = v
		}
	}
	if len(parts) >= 5 && parts[4] != "" {
		if v, err := strconv.ParseFloat(parts[4], 64); err == nil {
			obj.Pitch = v
		}
	}
	if len(parts) >= 6 && parts[5] != "" {
		if v, err := strconv.ParseFloat(parts[5], 64); err == nil {
			obj.Heading = v
		}
	}
	obj.noteMotion()
}

func (obj *Object) noteMotion() {
	now := time.Now()
	if !obj.hasPrev {
		obj.prevLat = obj.Latitude
		obj.prevLon = obj.Longitude
		obj.prevAlt = obj.Altitude
		obj.prevT = now
		obj.hasPrev = true
		return
	}
	dt := now.Sub(obj.prevT).Seconds()
	// Tacview often ticks at 10–15 Hz. Do not reset the sample
	// until we have enough time on the clock, or GS stays 0.
	if dt < 0.12 {
		return
	}
	if dt < 8 {
		d := distMeters(obj.prevLat, obj.prevLon, obj.Latitude, obj.Longitude)
		da := obj.Altitude - obj.prevAlt
		raw := math.Sqrt(d*d+da*da) / dt
		if raw >= 0 && raw < 700 {
			if obj.MotionGS == 0 {
				obj.MotionGS = raw
			} else {
				obj.MotionGS = 0.4*obj.MotionGS + 0.6*raw
			}
		}
		vs := da / dt
		if vs > -200 && vs < 200 {
			if obj.MotionVS == 0 {
				obj.MotionVS = vs
			} else {
				obj.MotionVS = 0.4*obj.MotionVS + 0.6*vs
			}
		}
	}
	obj.prevLat = obj.Latitude
	obj.prevLon = obj.Longitude
	obj.prevAlt = obj.Altitude
	obj.prevT = now
}

func (obj *Object) refreshSpeed() {
	gs := obj.MotionGS
	if gs < 0.5 && obj.GS > 0 {
		gs = obj.GS
	}
	low := (obj.HasOnGround && obj.OnGround) || (obj.AGL > 0 && obj.AGL < 15)
	iasLie := obj.IAS > gs+20 && gs < 15 // stale cruise IAS while the airframe is not moving
	cold := obj.HasRPM && obj.EngineRPM < 0.5
	idle := obj.HasThrottle && obj.Throttle <= 0.22 && obj.Afterburner < 0.2
	if low || iasLie {
		// Tacview lat/lon jitters ~8 kt while chocked. Below 12 kt on the pad = parked.
		if gs < 6.2 || cold || (idle && gs < 7.2) {
			obj.Speed = 0
			return
		}
		obj.Speed = gs
		return
	}
	if obj.IAS > 2 {
		obj.Speed = obj.IAS
	} else {
		obj.Speed = gs
	}
}

func distMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1 := lat1 * math.Pi / 180
	p2 := lat2 * math.Pi / 180
	dlat := (lat2 - lat1) * math.Pi / 180
	dlon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * r * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func (c *Client) Aircraft() []*Object {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []*Object
	for _, o := range c.objects {
		if strings.Contains(o.Type, "Air") {
			out = append(out, o)
		}
	}
	return out
}

// Census lists every object updated in the last 30 seconds.
// Air objects come first. This does not change tracking. It only
// shows whether a second pilot is in the Tacview feed.
func (c *Client) Census() (air int, lines []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cutoff := time.Now().Add(-30 * time.Second)
	var airLines, other []string
	for _, o := range c.objects {
		if o.LastSeen.Before(cutoff) {
			continue
		}
		line := fmt.Sprintf("id=%s type=%q name=%q pilot=%q", o.ID, o.Type, o.Name, o.Pilot)
		if strings.Contains(o.Type, "Air") || strings.Contains(o.Pilot, "|") {
			air++
			airLines = append(airLines, line)
			continue
		}
		other = append(other, line)
	}
	lines = append(airLines, other...)
	if len(lines) > 30 {
		extra := len(lines) - 30
		lines = append(lines[:30], fmt.Sprintf("... %d more", extra))
	}
	return air, lines
}

func (c *Client) noteObjectLocked(obj *Object) {
	if c.announced == nil {
		c.announced = map[string]string{}
	}
	sig := obj.Type + "|" + obj.Name + "|" + obj.Pilot
	if c.announced[obj.ID] == sig {
		return
	}
	c.announced[obj.ID] = sig
	c.log.Info("tacview object",
		"id", obj.ID,
		"type", obj.Type,
		"name", obj.Name,
		"pilot", obj.Pilot,
	)
}
