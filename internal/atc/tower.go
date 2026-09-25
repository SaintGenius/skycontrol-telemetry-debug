package atc

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skycontrol/skycontrol/internal/airfield"
	"github.com/skycontrol/skycontrol/internal/radio"
	"github.com/skycontrol/skycontrol/internal/telemetry"
	"github.com/skycontrol/skycontrol/internal/weather"
)

// Role is the ATC position.
type Role string

const (
	RoleTower    Role = "Tower"
	RoleGround   Role = "Ground"
	RoleApproach Role = "Approach"
	RoleCenter   Role = "Center"
)

// AircraftState tracks what we know about a single aircraft for ATC purposes.
type AircraftState struct {
	ID            string
	Pilot         string
	Callsign      string // derived / spoken callsign
	Type          string
	Latitude      float64
	Longitude     float64
	AltitudeFt    float64
	Heading       float64
	Nearest       *airfield.Airfield
	DistanceNM    float64
	OnGround      bool
	LastSeen      time.Time
	Spawned       time.Time
	ClearedTakeoff bool
	ClearedLand    bool
	LastClearance  time.Time

	Owner        *airfield.Airfield // who we currently are on the radio
	Dest         *airfield.Airfield // named destination / pending handoff
	Prev         *airfield.Airfield
	HandoffAt    time.Time
	LastAutoHO   time.Time
	LastRemind   time.Time
	Reminders    int
	JustSwitched bool
	NeedContact  bool
	hoTick       time.Time
	SpeedMS      float64
	LandingGear  float64
	AGLFt        float64
	Throttle     float64
	HasThrottle  bool
	EngineRPM    float64
	HasRPM       bool
	Afterburner  float64
	Flaps        float64
	Pitch        float64
	MotionVS     float64
	Phase        string // parked, taxi, runway, departure, downwind, base, final, airborne
	Pattern      string // overhead leg: initial, break, downwind, base, final
	Emergency    bool
	EmergKind    string
	EmergAt      time.Time
	Souls        int
	FuelState    string
	RadioGUID    string
	LastIntent   Intent
	LastIntentAt time.Time
}

// Tower is a simple Tower controller.
// It watches aircraft near airfields and can issue basic clearances.
type Tower struct {
	log       *slog.Logger
	airfields *airfield.Database
	radio     radio.Client

	mu        sync.RWMutex
	lastIntent   Intent
	lastIntentAt time.Time
	aircraft  map[string]*AircraftState
	binds     map[string]string // SRS GUID -> aircraft ID
	lastMu       sync.Mutex
	lastFreq     radio.Frequency
	lastCallsign string
	lastText     string

	speakMu sync.Mutex
	speakFn func(string)
	agent   *Agent
	wind    *weather.Reader
	voices  map[string]string

	started           time.Time
	lastCenterRelease time.Time
	centerReleases    int
}

// NewTower creates a Tower controller.
func NewTower(log *slog.Logger, db *airfield.Database, rad radio.Client) *Tower {
	if log == nil {
		log = slog.Default()
	}
	return &Tower{
		log:       log,
		airfields: db,
		radio:     rad,
		aircraft:  make(map[string]*AircraftState),
		binds:     make(map[string]string),
		started:   time.Now(),
	}
}

// SetSpeaker plays TTS on the PC speakers whenever ATC talks.
func (t *Tower) SetSpeaker(fn func(string)) {
	t.speakFn = fn
}

func (t *Tower) SetAgent(a *Agent) {
	t.agent = a
}

func (t *Tower) SetWind(w *weather.Reader) {
	t.wind = w
}

func (t *Tower) SetVoices(m map[string]string) {
	t.voices = configNormalizeVoices(m)
	n := len(t.voices)
	if n > 0 {
		fmt.Printf("  field voices: %d (Ground/Tower share)\n", n)
		for _, name := range []string{"senaki", "batumi", "kutaisi", "nellis"} {
			if _, ok := t.voices[name]; ok {
				fmt.Printf("    %s: yes\n", name)
			} else {
				fmt.Printf("    %s: MISSING\n", name)
			}
		}
	} else {
		fmt.Println("  field voices: 0 — all fields use default Adam. Check data/elevenlabs-voices.yaml")
	}
}

func configNormalizeVoices(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, id := range m {
		k = strings.ToLower(strings.TrimSpace(k))
		k = strings.TrimSuffix(k, " tower")
		k = strings.TrimSuffix(k, " ground")
		k = strings.TrimSuffix(k, " approach")
		id = strings.TrimSpace(id)
		if k != "" && id != "" {
			out[k] = id
		}
	}
	return out
}

func (t *Tower) voiceID(af *airfield.Airfield) string {
	if af == nil {
		return ""
	}
	for _, n := range []string{af.Name, af.FullName, af.ID, af.ICAO} {
		if id := t.voiceIDNamed(n); id != "" {
			return id
		}
	}
	return ""
}

func (t *Tower) voiceIDNamed(name string) string {
	if t == nil || len(t.voices) == 0 {
		return ""
	}
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, " tower")
	n = strings.TrimSuffix(n, " ground")
	n = strings.TrimSuffix(n, " approach")
	n = strings.TrimSuffix(n, " information")
	if id, ok := t.voices[n]; ok {
		return id
	}
	if i := strings.IndexAny(n, "-/"); i > 2 {
		if id, ok := t.voices[n[:i]]; ok {
			return id
		}
	}
	best, bestLen := "", 0
	for k, id := range t.voices {
		if len(k) < 4 {
			continue
		}
		if strings.Contains(n, k) || strings.Contains(k, n) {
			if len(k) > bestLen {
				best, bestLen = id, len(k)
			}
		}
	}
	return best
}

func (t *Tower) WindStatus() string {
	if t == nil || t.wind == nil {
		return "no wind reader"
	}
	return t.wind.String()
}

func (t *Tower) windSample() weather.Sample {
	if t == nil || t.wind == nil {
		return weather.Sample{Source: "none"}
	}
	return t.wind.Current()
}

func (t *Tower) activeName(af *airfield.Airfield, heading float64) string {
	return pickActiveRunway(af, heading, t.windSample())
}

func (t *Tower) activeSpoken(af *airfield.Airfield, st *AircraftState) string {
	hdg := 0.0
	if st != nil {
		hdg = st.Heading
	}
	n := t.activeName(af, hdg)
	if n == "" {
		return runwayLabel(af)
	}
	return SpeakRunway(n)
}

// UpdateAircraft is called when telemetry provides a new/updated object.
func (t *Tower) UpdateAircraft(obj *telemetry.Object) {
	if obj == nil {
		return
	}
	if !strings.Contains(obj.Type, "Air") && !strings.Contains(obj.Type, "FixedWing") && !strings.Contains(obj.Type, "Rotor") {
		return
	}

	t.mu.Lock()

	st, ok := t.aircraft[obj.ID]
	if !ok {
		st = &AircraftState{ID: obj.ID, Spawned: time.Now()}
		t.aircraft[obj.ID] = st
	}

	st.Pilot = obj.Pilot
	if st.Pilot == "" {
		st.Pilot = obj.CallSign
	}
	if st.Pilot == "" {
		st.Pilot = obj.Name
	}
	st.Callsign = SpeakCallsign(st.Pilot)
	st.Type = obj.Name
	st.Latitude = obj.Latitude
	st.Longitude = obj.Longitude
	st.AltitudeFt = obj.Altitude * 3.28084 // m → ft
	st.Heading = obj.Heading
	st.LastSeen = time.Now()
	st.SpeedMS = obj.Speed
	st.LandingGear = obj.LandingGear
	st.Throttle = obj.Throttle
	st.HasThrottle = obj.HasThrottle
	st.EngineRPM = obj.EngineRPM
	st.HasRPM = obj.HasRPM
	st.Afterburner = obj.Afterburner
	st.Flaps = obj.Flaps
	st.Pitch = obj.Pitch
	st.MotionVS = obj.MotionVS
	if obj.AGL > 0 {
		st.AGLFt = obj.AGL * 3.28084
	}

	if st.Latitude != 0 || st.Longitude != 0 {
		nearest, dist := t.airfields.Nearest(st.Latitude, st.Longitude)
		st.Nearest = nearest
		st.DistanceNM = dist
	}
	agl := st.AGLFt
	if agl == 0 {
		elev := 0.0
		if st.Nearest != nil {
			elev = st.Nearest.ElevationFt
		}
		agl = st.AltitudeFt - elev
	}
	// Altitude wins. Tacview often sends OnGround=1 at spawn and never 0.
	if agl > 120 {
		st.OnGround = false
	} else if agl < 40 {
		st.OnGround = true
	} else if obj.HasOnGround {
		st.OnGround = obj.OnGround
	} else {
		st.OnGround = obj.MotionGS < 30 && obj.Speed < 30
	}
	// Once we know they are on the pad, never keep a cruise IAS.
	// Under 12 kt on the ground is jitter / chocks, not taxi.
	// Cold RPM or idle + slow is parked even if the pin twitches.
	if st.OnGround {
		cold := obj.HasRPM && obj.EngineRPM < 0.5
		idle := obj.HasThrottle && obj.Throttle <= 0.22 && obj.Afterburner < 0.2
		if obj.MotionGS < parkedMS || cold || (idle && obj.MotionGS < 7.2) {
			st.SpeedMS = 0
		} else {
			st.SpeedMS = obj.MotionGS
		}
	}
	st.Phase = classifyPhase(st)

	t.seedOwnerLocked(st)
	t.mu.Unlock()
	t.TickHandoff()
}

// HandleRadioCall processes a transcribed radio call from a pilot.
// Returns true if the tower handled it.
func (t *Tower) HandleRadioCall(call radio.ReceivedCall) bool {
	text := strings.ToLower(strings.TrimSpace(call.Transcript))
	if text == "" {
		return false
	}
	if call.Frequency.Hz >= 1_000_000 {
		t.lastMu.Lock()
		t.lastFreq = call.Frequency
		t.lastMu.Unlock()
	}

	t.mu.Lock()
	st := t.identifyCaller(call)
	if st != nil {
		t.bindGUID(call.GUID, st)
		// SRS often names the client "player". Do not replace the Tacview callsign with that.
		if right := radioCallsign(call.Pilot); usableCallsign(right) {
			st.Callsign = right
		}
		if st.Callsign != "" {
			call.Pilot = st.Callsign
		}
		if DetectIntent(text) != IntentSayAgain {
			st.JustSwitched = false
		}
	}
	t.mu.Unlock()

	if st == nil && strings.TrimSpace(call.GUID) != "" {
		cs := heardCallsign(call)
		if cs == "" {
			fmt.Println("  (unknown caller — say callsign)")
			if IsCenterFreq(call.Frequency) {
				t.sayCenter("Station calling, say your callsign.")
			} else {
				t.say(call.Frequency, t.cfgCallsign(nil, RoleTower),
					"Station calling, say your callsign.")
			}
			return true
		}
		call.Pilot = cs
		fmt.Printf("  callsign %s — not in Tacview yet, answering anyway\n", cs)
		if names := t.trackedNames(); names != "" {
			fmt.Printf("  tacview sees: %s\n", names)
		} else {
			fmt.Println("  tacview sees: nobody")
		}
	}

	if t.handleCenterCall(call) {
		return true
	}
	if t.handleHandoffCall(call) {
		return true
	}
	if t.handleEmergencyCall(call) {
		return true
	}
	if t.handlePatternCall(call) {
		return true
	}

	aiOn := t.agent != nil && t.agent.Enabled()
	fmt.Printf("  radio: %q  chatgpt=%v\n", text, aiOn)

	if DetectIntent(text) == IntentATIS {
		return t.handleATISRequest(call)
	}
	if DetectIntent(text) == IntentRadioCheck {
		return t.handleRadioCheck(call)
	}

	if aiOn {
		fmt.Println("  asking ChatGPT...")
		snap := t.snapshot(call)
		d, err := t.agent.Decide(snap)
		if err != nil {
			fmt.Printf("  AI down, using backup: %v\n", err)
			t.log.Warn("agent fallback to keywords", "error", err)
		} else if strings.TrimSpace(d.Text) != "" {
			d = t.fixDepartureDecision(call, snap, d)
			t.log.Info("agent decision", "intent", d.Intent, "role", d.Role, "text", d.Text)
			fmt.Printf("  AI: %s\n", d.Intent)
			t.applyDecision(call, snap, d)
			return true
		}
	}

	intent := DetectIntent(text)
	t.mu.Lock()
	dup := false
	if st != nil {
		dup = intent != IntentSayAgain && intent != IntentUnknown &&
			intent == st.LastIntent && time.Since(st.LastIntentAt) < 10*time.Second
		if !dup {
			st.LastIntent = intent
			st.LastIntentAt = time.Now()
		}
	} else {
		dup = intent != IntentSayAgain && intent != IntentUnknown &&
			intent == t.lastIntent && time.Since(t.lastIntentAt) < 10*time.Second
		if !dup {
			t.lastIntent = intent
			t.lastIntentAt = time.Now()
		}
	}
	t.mu.Unlock()
	if dup {
		fmt.Println("  (ignored repeat)")
		return false
	}

	switch intent {
	case IntentTakeoff:
		return t.handleTakeoffRequest(call)
	case IntentLanding, IntentInbound:
		return t.handleLandingRequest(call)
	case IntentTouchAndGo:
		return t.handleTouchAndGo(call)
	case IntentTaxi:
		return t.handleTaxiRequest(call)
	case IntentParking:
		return t.handleParkingRequest(call)
	case IntentStartup:
		return t.handleStartupRequest(call)
	case IntentRadioCheck:
		return t.handleRadioCheck(call)
	case IntentGoAround:
		return t.handleGoAround(call)
	case IntentHoldShort:
		return t.handleHoldShort(call)
	case IntentTraffic:
		return t.handleTrafficRequest(call)
	case IntentATIS:
		return t.handleATISRequest(call)
	case IntentSayAgain:
		return t.handleSayAgain(call)
	case IntentUnable:
		t.mu.Lock()
		st := t.identifyCaller(call)
		if st == nil {
			st = t.findByPilot(call.Pilot)
		}
		af := ownerOrNearest(st)
		t.mu.Unlock()
		t.sayAs(af, RoleTower, fmt.Sprintf("%s, %s, roger, remain this frequency.",
			func() string {
				if st != nil && st.Callsign != "" {
					return st.Callsign
				}
				return "Aircraft"
			}(), t.cfgCallsign(af, RoleTower)))
		return true
	case IntentContact:
		t.say(call.Frequency, t.cfgCallsign(nil, RoleTower),
			"Say again facility.")
		return true
	default:
		t.say(call.Frequency, t.cfgCallsign(nil, RoleTower),
			"Station calling, say again your request.")
		return true
	}
}

func (t *Tower) snapshot(call radio.ReceivedCall) Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	st, af, pilot := t.resolveCaller(call, true)
	s := Snapshot{
		Transcript: call.Transcript,
		Pilot:      pilot,
		Callsign:   pilot,
		OnCenter:   IsCenterFreq(call.Frequency),
	}
	if s.OnCenter {
		s.CenterName = CenterCallsign
	}
	t.lastMu.Lock()
	s.LastClearance = t.lastText
	t.lastMu.Unlock()
	if st != nil {
		s.Type = st.Type
		s.OnGround = st.OnGround
		s.AltitudeFt = st.AltitudeFt
		s.Heading = st.Heading
		s.DistanceNM = st.DistanceNM
		s.SpeedKt = math.Round(st.SpeedMS * 1.94384)
		s.GearDown = st.LandingGear >= 0.5
		s.Phase = st.Phase
		s.PatternLeg = st.Pattern
		s.ClearedLand = st.ClearedLand
		s.ClearedTakeoff = st.ClearedTakeoff
		s.Emergency = st.Emergency
		s.EmergKind = st.EmergKind
		s.Souls = st.Souls
		s.Throttle = st.Throttle
		s.Flaps = st.Flaps
		s.EngineOff = st.HasRPM && st.EngineRPM < 0.5
		if st.Callsign != "" {
			s.Callsign = st.Callsign
			s.Pilot = st.Callsign
		}
		if st.Owner != nil {
			s.OwnerField = st.Owner.Name
			s.DistanceNM = airfield.DistanceNM(st.Latitude, st.Longitude, st.Owner.Latitude, st.Owner.Longitude)
			s.DistanceNM = math.Round(s.DistanceNM*10) / 10
		}
		if st.Dest != nil && (st.Owner == nil || st.Dest.Name != st.Owner.Name) {
			s.PendingField = st.Dest.Name
			s.PendingFreq = st.Dest.PrimaryTowerFreq()
			s.HandoffPending = !st.HandoffAt.IsZero()
		}
	}
	if af != nil {
		s.Airfield = af.Name
		s.ICAO = af.ICAO
		s.TowerCallsign = af.Callsign(airfield.RoleTower)
		s.GroundCallsign = af.Callsign(airfield.RoleGround)
		s.Runway = t.activeSpoken(af, st)
		s.Runways = strings.TrimPrefix(s.Runway, "runway ")
		w := t.windSample()
		s.WindFromDeg = w.FromDeg
		s.WindKt = math.Round(w.SpeedKt*10) / 10
		s.WindSource = w.Source
		if !w.OK {
			s.WindSource = "heading"
		}
		s.TowerFreq = af.PrimaryTowerFreq()
		s.GroundFreq = af.PrimaryGroundFreq()
		if len(af.Frequencies.Approach) > 0 {
			s.ApproachFreq = af.Frequencies.Approach[0]
		}
		s.ATISFreq = primaryATIS(af)
		s.TACAN = af.TACAN
		s.FieldElevFt = af.ElevationFt
	} else {
		s.TowerCallsign = "Tower"
		s.GroundCallsign = "Ground"
		s.Runway = "runway"
	}
	if st != nil && t.airfields != nil && (st.Latitude != 0 || st.Longitude != 0) {
		mapName := ""
		if af != nil {
			mapName = af.Map
		}
		s.Nearby = t.airfields.NearbyOnMap(mapName, st.Latitude, st.Longitude, 10)
		if asked, ok := t.airfields.LookupNear(guessFieldName(call.Transcript, s.Nearby, af), st.Latitude, st.Longitude); ok {
			if af == nil || !strings.EqualFold(asked.Name, af.Name) {
				a := asked
				s.AskedField = &a
			}
		}
		s.Traffic = t.trafficNear(st, 6)
		intent := "inbound"
		if st.OnGround {
			intent = "takeoff"
		}
		if time.Since(t.lastIntentAt) < 45*time.Second && t.lastIntent != "" && t.lastIntent != IntentSayAgain {
			intent = string(t.lastIntent)
		}
		s.TrafficCall = t.trafficPhraseLocked(st, af, intent)
		s.RunwayClear = true
		if af != nil {
			for _, o := range t.aircraft {
				if o == nil || o.ID == st.ID || strings.HasPrefix(o.ID, "demo-") {
					continue
				}
				if time.Since(o.LastSeen) > 12*time.Second {
					continue
				}
				if strings.EqualFold(o.Callsign, st.Callsign) || strings.EqualFold(o.Pilot, st.Pilot) {
					continue
				}
				if !o.OnGround || o.Phase == "parked" || o.SpeedMS < parkedMS {
					continue
				}
				d := airfield.DistanceNM(o.Latitude, o.Longitude, af.Latitude, af.Longitude)
				if d < 0.4 {
					s.RunwayClear = false
					break
				}
			}
		}
	}
	return s
}

func runwayList(af *airfield.Airfield) string {
	if af == nil || len(af.Runways) == 0 {
		return ""
	}
	var p []string
	for _, r := range af.Runways {
		if r.Name != "" {
			p = append(p, r.Name)
		}
	}
	return strings.Join(p, "/")
}

func guessFieldName(transcript string, nearby []airfield.Nearby, home *airfield.Airfield) string {
	t := compactField(foldFieldAliases(strings.ToLower(transcript)))
	homeName := ""
	if home != nil {
		homeName = strings.ToLower(home.Name)
	}
	for _, n := range nearby {
		name := strings.ToLower(n.Name)
		if name == "" || name == homeName {
			continue
		}
		if fieldNameHits(t, name) {
			return n.Name
		}
	}
	return ""
}

func fieldNameHits(compactTranscript, fieldName string) bool {
	n := compactField(foldFieldAliases(strings.ToLower(fieldName)))
	if n != "" && strings.Contains(compactTranscript, n) {
		return true
	}
	words := strings.Fields(strings.ToLower(fieldName))
	if len(words) == 0 {
		return false
	}
	last := compactField(foldFieldAliases(words[len(words)-1]))
	if len(last) >= 4 && strings.Contains(compactTranscript, last) {
		return true
	}
	return false
}

func compactField(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

var fieldAliasPairs = [][2]string{
	{"al maynad", "al minhad"},
	{"al-minad", "al minhad"},
	{"al minad", "al minhad"},
	{"aminad", "minhad"},
	{"amaynad", "minhad"},
	{"maynad", "minhad"},
	{"minad", "minhad"},
	{"maktaum", "maktoum"},
	{"maktoom", "maktoum"},
	{"maktum", "maktoum"},
	{"sanaki", "senaki"},
	{"senahkee", "senaki"},
	{"senakee", "senaki"},
	{"kutasi", "kutaisi"},
	{"katase", "kutaisi"},
	{"kitasi", "kutaisi"},
	{"kitassee", "kutaisi"},
	{"kootysee", "kutaisi"},
	{"kootisee", "kutaisi"},
	{"kootasi", "kutaisi"},
	{"bahtoomee", "batumi"},
	{"batoomi", "batumi"},
	{"kobeleti", "kobuleti"},
	{"kobeletty", "kobuleti"},
	{"cobuleti", "kobuleti"},
	{"coboleti", "kobuleti"},
	{"nelliss", "nellis"},
	{"tuhbeeleesee", "tbilisi"},
	{"vahzeeahnee", "vaziani"},
}

func foldFieldAliases(t string) string {
	for _, p := range fieldAliasPairs {
		if strings.Contains(t, p[0]) {
			t = strings.ReplaceAll(t, p[0], p[1])
		}
	}
	return t
}

func (t *Tower) trafficNear(self *AircraftState, limit int) []TrafficBrief {
	if self == nil || limit <= 0 {
		return nil
	}
	type pair struct {
		b TrafficBrief
		d float64
	}
	var all []pair
	for _, o := range t.aircraft {
		if o == nil || o.ID == self.ID {
			continue
		}
		if strings.HasPrefix(o.ID, "demo-") {
			continue
		}
		if time.Since(o.LastSeen) > 12*time.Second {
			continue
		}
		if strings.EqualFold(o.Callsign, self.Callsign) || (o.Pilot != "" && strings.EqualFold(o.Pilot, self.Pilot)) {
			continue
		}
		if o.Phase == "parked" || (o.OnGround && o.SpeedMS < parkedMS && o.Afterburner < 0.2) {
			continue
		}
		d := airfield.DistanceNM(self.Latitude, self.Longitude, o.Latitude, o.Longitude)
		if d > 12 {
			continue
		}
		cs := o.Callsign
		if cs == "" {
			cs = o.Pilot
		}
		all = append(all, pair{
			b: TrafficBrief{
				Callsign:   cs,
				Type:       o.Type,
				AltitudeFt: o.AltitudeFt,
				DistanceNM: math.Round(d*10) / 10,
				Heading:    o.Heading,
				OnGround:   o.OnGround,
				SpeedKt:    math.Round(o.SpeedMS * 1.94384),
				GearDown:   o.LandingGear >= 0.5,
				Phase:      o.Phase,
			},
			d: d,
		})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].d < all[j].d })
	if limit > len(all) {
		limit = len(all)
	}
	out := make([]TrafficBrief, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, all[i].b)
	}
	return out
}

func (t *Tower) fixDepartureDecision(call radio.ReceivedCall, snap Snapshot, d Decision) Decision {
	text := strings.ToLower(call.Transcript)
	ready := containsAny(text, "ready for departure", "ready for takeoff", "holding short",
		"request takeoff", "requesting takeoff", "cleared for takeoff")
	if !ready {
		return d
	}
	loop := d.Intent == "taxi" || d.Intent == "hold_short" ||
		strings.Contains(strings.ToLower(d.Text), "call ready")
	if !loop && d.Intent == "takeoff" {
		return d
	}
	d.Intent = "takeoff"
	d.Role = "Tower"
	cs := snap.Callsign
	if cs == "" {
		cs = "Aircraft"
	}
	twr := snap.TowerCallsign
	if twr == "" {
		twr = "Tower"
	}
	rwy := snap.Runway
	if rwy == "" {
		rwy = "the active"
	}
	d.Text = fmt.Sprintf("%s, %s, %s, cleared for takeoff. Altimeter two niner niner two, fly runway heading.",
		cs, twr, rwy)
	return d
}

func (t *Tower) applyDecision(call radio.ReceivedCall, snap Snapshot, d Decision) {
	role := Role(d.Role)
	if role != RoleGround && role != RoleApproach {
		role = RoleTower
	}

	t.mu.Lock()
	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	t.seedOwnerLocked(st)

	if IsCenterFreq(call.Frequency) {
		dest := (*airfield.Airfield)(nil)
		if snap.AskedField != nil {
			if af, ok := t.airfields.GetByName(snap.AskedField.Name); ok {
				dest = af
			}
		}
		if dest == nil && st != nil {
			dest = st.Dest
			if dest == nil {
				dest = ownerOrNearest(st)
			}
		}
		if dest != nil && st != nil {
			st.Dest = dest
		}
		pilot := "Aircraft"
		if st != nil && st.Callsign != "" {
			pilot = st.Callsign
		}
		onGround := st == nil || st.OnGround
		t.mu.Unlock()
		switch d.Intent {
		case "taxi", "startup", "parking", "hold_short":
			t.sayCenter(t.centerContactLine(pilot, dest, RoleGround))
		case "takeoff", "landing", "inbound", "contact", "check_in", "go_around", "touch_and_go":
			t.sayCenter(t.centerContactLine(pilot, dest, RoleTower))
		case "radio_check":
			t.sayCenter(t.centerRadioCheck(pilot, dest, onGround))
		case "atis":
			t.sayCenter(t.centerATISLine(pilot, dest))
		default:
			t.sayCenter(d.Text)
		}
		return
	}

	switchTalk := containsAny(strings.ToLower(call.Transcript),
		"switch to", "switching to", "contacting", "going over", "hand off", "handoff")
	if snap.AskedField != nil && st != nil {
		askedName := snap.AskedField.Name
		ownerName := ""
		if st.Owner != nil {
			ownerName = st.Owner.Name
		}
		if askedName != "" && !strings.EqualFold(askedName, ownerName) &&
			(d.Intent == "check_in" || d.Intent == "contact" || switchTalk) {
			d.Intent = "contact"
		}
	}

	switch d.Intent {
	case "contact":
		if snap.AskedField != nil {
			if af, ok := t.airfields.GetByName(snap.AskedField.Name); ok {
				act := t.beginHandoff(st, af)
				t.mu.Unlock()
				if act != nil {
					t.execHandoff(act)
				} else if st != nil && st.Owner != nil {
					t.sayAs(st.Owner, RoleTower, fmt.Sprintf("%s, %s, remain this frequency.",
						st.Callsign, st.Owner.Callsign(airfield.RoleTower)))
				}
				return
			}
		}
	case "check_in":
		t.completeHandoffLocked(st)
	case "radio_check", "inbound", "landing":
		pending := st != nil && st.Dest != nil && st.Owner != nil &&
			st.Dest.Name != st.Owner.Name && !st.HandoffAt.IsZero()
		if pending && !fieldNameIn(call.Transcript, st.Owner.Name) {
			t.completeHandoffLocked(st)
		}
	}
	af := (*airfield.Airfield)(nil)
	if st != nil {
		af = st.Owner
		if af == nil {
			af = st.Nearest
		}
	}
	if af != nil && freqsSplit(af) && onRoleFreq(call, af, RoleGround) {
		heard := DetectIntent(call.Transcript)
		low := strings.ToLower(call.Transcript)
		askedTaxi := heard == IntentTaxi || containsAny(low, "request taxi", "requesting taxi", "need a taxi", "need taxi", "taxi to")
		if askedTaxi {
			d.Intent = "taxi"
			if !strings.Contains(strings.ToLower(d.Text), "taxi") {
				rwy := t.activeSpoken(af, st)
				pilot := "Aircraft"
				if st != nil && st.Callsign != "" {
					pilot = st.Callsign
				}
				d.Text = fmt.Sprintf("%s, %s, taxi to %s via alpha, hold short.%s",
					pilot, af.Callsign(airfield.RoleGround), rwy, contactTowerTail(af))
			} else if !strings.Contains(strings.ToLower(d.Text), "contact") {
				d.Text = strings.TrimRight(d.Text, ".") + "." + contactTowerTail(af)
			}
			role = RoleGround
		} else if heard == IntentTakeoff || containsAny(low, "ready for departure", "ready for takeoff", "holding short") {
			pilot := "Aircraft"
			if st != nil && st.Callsign != "" {
				pilot = st.Callsign
			}
			t.mu.Unlock()
			t.sayAs(af, RoleGround, sendToTower(af, pilot))
			return
		}
	}
	if st != nil {
		st.LastClearance = time.Now()
		switch d.Intent {
		case "takeoff":
			st.ClearedTakeoff = true
		case "landing":
			st.ClearedLand = true
		case "go_around":
			st.ClearedLand = false
			st.Pattern = ""
		}
	}
	if role == RoleGround && (st == nil || !st.OnGround) {
		role = t.airRole(af, false)
	}
	if role == RoleTower && st != nil && st.OnGround &&
		d.Intent != "takeoff" && d.Intent != "landing" && d.Intent != "inbound" {
		role = t.airRole(af, true)
	}
	if af != nil && onRoleFreq(call, af, RoleGround) &&
		(d.Intent == "radio_check" || d.Intent == "taxi" || d.Intent == "startup" || d.Intent == "parking") {
		role = RoleGround
	}
	if af != nil && onRoleFreq(call, af, RoleTower) && d.Intent == "taxi" {
		role = RoleTower
		if !strings.Contains(strings.ToLower(d.Text), "holding short") {
			pilot := "Aircraft"
			if st != nil && st.Callsign != "" {
				pilot = st.Callsign
			}
			d.Text = fmt.Sprintf("%s, %s, roger, continue taxi, report holding short.",
				pilot, af.Callsign(airfield.RoleTower))
		}
	}
	if st != nil && af != nil {
		d.Text = rewriteHoldShort(d.Text, t.activeName(af, st.Heading))
	}
	if msg := phaseRefuse(st, Intent(d.Intent), t.cfgCallsign(af, role)); msg != "" {
		t.mu.Unlock()
		t.sayAs(af, role, msg)
		return
	}
	d.Text = t.attachTraffic(d.Text, st, af, d.Intent)
	t.mu.Unlock()
	t.sayAs(af, role, d.Text)
}

func (t *Tower) handleTakeoffRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Find the aircraft that most likely made the call (by pilot name match for now)
	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	if st == nil || ownerOrNearest(st) == nil {
		t.say(call.Frequency, t.cfgCallsign(nil, RoleTower), "Say again, aircraft requesting takeoff.")
		return true
	}

	af := ownerOrNearest(st)
	callsign := af.Callsign(airfield.RoleTower)
	pilot := st.Callsign
	if pilot == "" {
		pilot = "Aircraft"
	}
	if msg := phaseRefuse(st, IntentTakeoff, callsign); msg != "" {
		t.say(call.Frequency, callsign, msg)
		return true
	}
	if freqsSplit(af) && onRoleFreq(call, af, RoleGround) {
		t.sayAs(af, RoleGround, sendToTower(af, pilot))
		return true
	}

	// Simple clearance
	runway := t.activeSpoken(af, st)

	msg := fmt.Sprintf("%s, %s, wind calm, %s, cleared for takeoff.", pilot, callsign, runway)
	msg = t.attachTraffic(msg, st, af, "takeoff")
	t.say(call.Frequency, callsign, msg)

	st.ClearedTakeoff = true
	st.LastClearance = time.Now()
	t.log.Info("issued takeoff clearance",
		"pilot", pilot,
		"airfield", af.Name,
		"runway", runway,
	)
	return true
}

func (t *Tower) handleLandingRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	if st == nil || ownerOrNearest(st) == nil {
		t.say(call.Frequency, t.cfgCallsign(nil, RoleTower), "Say again, aircraft requesting landing.")
		return true
	}

	af := ownerOrNearest(st)
	callsign := af.Callsign(airfield.RoleTower)
	pilot := st.Callsign
	if pilot == "" {
		pilot = "Aircraft"
	}
	if msg := phaseRefuse(st, IntentLanding, callsign); msg != "" {
		t.say(call.Frequency, callsign, msg)
		return true
	}

	runway := t.activeSpoken(af, st)

	msg := fmt.Sprintf("%s, %s, %s, cleared to land.", pilot, callsign, runway)
	msg = t.attachTraffic(msg, st, af, "landing")
	t.say(call.Frequency, callsign, msg)

	st.ClearedLand = true
	st.LastClearance = time.Now()
	t.log.Info("issued landing clearance",
		"pilot", pilot,
		"airfield", af.Name,
		"runway", runway,
	)
	return true
}

func (t *Tower) handleTouchAndGo(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, false)
	cs := t.cfgCallsign(af, RoleTower)
	if af == nil {
		t.say(call.Frequency, cs, "Say again, aircraft requesting the option.")
		return true
	}
	runway := t.activeSpoken(af, st)
	msg := fmt.Sprintf("%s, %s, %s, cleared touch and go.", pilot, cs, runway)
	msg = t.attachTraffic(msg, st, af, "touch_and_go")
	t.say(call.Frequency, cs, msg)
	if st != nil {
		st.LastClearance = time.Now()
	}
	t.log.Info("issued touch and go", "pilot", pilot, "airfield", af.Name)
	return true
}

func (t *Tower) handleParkingRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, true)
	cs := t.cfgCallsign(af, RoleGround)
	if af == nil {
		t.say(call.Frequency, cs, "Say again, aircraft requesting parking.")
		return true
	}
	msg := fmt.Sprintf("%s, %s, taxi to parking via alpha, remain this frequency.", pilot, cs)
	if st != nil && st.Emergency {
		msg = fmt.Sprintf("%s, %s, taxi as able, vehicles will meet you.", pilot, cs)
	}
	t.say(call.Frequency, cs, msg)
	if st != nil {
		st.LastClearance = time.Now()
	}
	t.log.Info("issued parking taxi", "pilot", pilot, "airfield", af.Name)
	return true
}

func (t *Tower) handleStartupRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, true)
	cs := t.cfgCallsign(af, RoleGround)
	if af == nil {
		t.say(call.Frequency, cs, "Say again, aircraft requesting startup.")
		return true
	}
	msg := fmt.Sprintf("%s, %s, startup approved, advise ready to taxi.", pilot, cs)
	t.say(call.Frequency, cs, msg)
	if st != nil {
		st.LastClearance = time.Now()
	}
	t.log.Info("approved startup", "pilot", pilot, "airfield", af.Name)
	return true
}

func (t *Tower) handleRadioCheck(call radio.ReceivedCall) bool {
	t.mu.Lock()
	st, af, pilot := t.resolveCaller(call, true)
	role := RoleTower
	onGround := st == nil || st.OnGround
	low := strings.ToLower(call.Transcript)
	if af != nil && (onGround || containsAny(low, "ground")) {
		role = RoleGround
	}
	if containsAny(low, "tower") {
		role = RoleTower
	}
	cs := t.cfgCallsign(af, role)
	freq := call.Frequency
	t.mu.Unlock()

	msg := fmt.Sprintf("%s, %s, loud and clear.", pilot, cs)
	if IsCenterFreq(freq) {
		t.sayCenter(msg)
	} else {
		t.say(freq, cs, msg)
	}
	return true
}

func (t *Tower) handleATISRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	st, af, pilot := t.resolveCaller(call, true)
	role := RoleGround
	if st != nil && !st.OnGround {
		role = RoleTower
	}
	cs := t.cfgCallsign(af, role)
	raw := ""
	if af != nil {
		raw = primaryATIS(af)
	}
	name := ""
	if af != nil {
		name = af.Name
	}
	t.mu.Unlock()

	if af == nil {
		t.say(call.Frequency, cs, fmt.Sprintf("%s, %s, say again your request.", pilot, cs))
		return true
	}
	if raw == "" {
		t.say(call.Frequency, cs, fmt.Sprintf("%s, %s, no ATIS this field, stay this frequency.", pilot, cs))
		return true
	}
	msg := fmt.Sprintf("%s, %s, ATIS is %s. Tune to comm two and copy information.",
		pilot, cs, SpeakFrequency(raw))
	t.say(call.Frequency, cs, msg)
	t.log.Info("issued ATIS freq", "pilot", pilot, "airfield", name, "freq", raw)
	return true
}

func (t *Tower) handleGoAround(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, false)
	cs := t.cfgCallsign(af, RoleTower)
	if af == nil {
		t.say(call.Frequency, cs, "Roger go around, remain this frequency.")
		return true
	}
	runway := t.activeSpoken(af, st)
	msg := fmt.Sprintf("%s, %s, roger go around, climb and maintain pattern altitude, re-enter left downwind %s.",
		pilot, cs, runway)
	msg = t.attachTraffic(msg, st, af, "go_around")
	t.say(call.Frequency, cs, msg)
	if st != nil {
		st.ClearedLand = false
		st.Pattern = ""
		st.LastClearance = time.Now()
	}
	t.log.Info("acknowledged go around", "pilot", pilot)
	return true
}

func (t *Tower) handleHoldShort(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, true)
	cs := t.cfgCallsign(af, RoleTower)
	runway := t.activeSpoken(af, st)
	msg := fmt.Sprintf("%s, %s, roger, hold short %s.", pilot, cs, runway)
	t.say(call.Frequency, cs, msg)
	return true
}

func (t *Tower) handleTrafficRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, af, pilot := t.resolveCaller(call, false)
	cs := t.cfgCallsign(af, RoleTower)
	phrase := t.trafficPhraseLocked(st, af, "traffic")
	if phrase == "" {
		phrase = "no other traffic observed."
	}
	msg := fmt.Sprintf("%s, %s, %s", pilot, cs, phrase)
	t.say(call.Frequency, cs, msg)
	return true
}

func (t *Tower) handleSayAgain(call radio.ReceivedCall) bool {
	t.lastMu.Lock()
	text := t.lastText
	cs := t.lastCallsign
	freq := t.lastFreq
	t.lastMu.Unlock()

	if text == "" {
		t.say(call.Frequency, t.cfgCallsign(nil, RoleTower), "Nothing copied, say again your request.")
		return true
	}
	if !strings.Contains(strings.ToLower(text), "i say again") {
		text = "I say again, " + text
	}
	if cs == "" {
		cs = t.cfgCallsign(nil, RoleTower)
	}
	if freq.Hz == 0 {
		freq = call.Frequency
	}
	t.say(freq, cs, text)
	return true
}

func (t *Tower) handleTaxiRequest(call radio.ReceivedCall) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	if st == nil || ownerOrNearest(st) == nil {
		t.say(call.Frequency, t.cfgCallsign(nil, RoleGround), "Say again, aircraft requesting taxi.")
		return true
	}

	af := ownerOrNearest(st)
	callsign := af.Callsign(airfield.RoleGround)
	pilot := st.Callsign
	if pilot == "" {
		pilot = "Aircraft"
	}
	if msg := phaseRefuse(st, IntentTaxi, callsign); msg != "" {
		t.say(call.Frequency, callsign, msg)
		return true
	}

	runway := t.activeSpoken(af, st)
	msg := fmt.Sprintf("%s, %s, taxi to %s via alpha, hold short.%s",
		pilot, callsign, runway, contactTowerTail(af))
	t.sayAs(af, RoleGround, msg)
	t.log.Info("issued taxi clearance", "pilot", pilot, "airfield", af.Name)
	return true
}

func (t *Tower) say(freq radio.Frequency, callsign, text string) {
	if freq.Hz < 1_000_000 {
		t.lastMu.Lock()
		if t.lastFreq.Hz >= 1_000_000 {
			freq = t.lastFreq
		}
		t.lastMu.Unlock()
	}
	tx := radio.Transmission{
		Callsign:  callsign,
		Text:      SpeakForRadio(text),
		Spoken:    SpeakForRadio(text),
		Frequency: freq,
		Coalition: 0,
		VoiceID:   t.voiceIDNamed(callsign),
	}
	if tx.VoiceID == "" && t.mu.TryRLock() {
		st := t.primaryLocked()
		t.mu.RUnlock()
		if st != nil {
			tx.VoiceID = t.voiceID(ownerOrNearest(st))
		}
	}
	if freq.Hz < 1_000_000 {
		tx.ExtraFreqs = t.fieldFreqs()
	}
	t.sayTX(tx, text)
}

func (t *Tower) sayTX(tx radio.Transmission, text string) {
	low := strings.ToLower(text)
	keepTape := !strings.HasPrefix(low, "station calling") &&
		!strings.HasPrefix(low, "nothing copied") &&
		!strings.HasPrefix(low, "i say again")
	if keepTape {
		t.lastMu.Lock()
		t.lastFreq = tx.Frequency
		t.lastCallsign = tx.Callsign
		t.lastText = text
		t.lastMu.Unlock()
	}
	fmt.Printf("ATC: %s\n\n", text)
	t.radio.Transmit(tx)
	if t.speakFn == nil {
		fmt.Println("  (no speaker hooked)")
		return
	}
	t.speakFn(SpeakForRadio(text))
}

func (t *Tower) fieldFreqs() []radio.Frequency {
	st := t.PrimaryAircraft()
	if st == nil {
		return nil
	}
	af := st.Owner
	if af == nil {
		af = st.Nearest
	}
	if af == nil {
		return nil
	}
	if f := uhfOf(af); f.Hz > 0 {
		return []radio.Frequency{f}
	}
	return nil
}

// HomePlate is the live jet's field UHF and tower name. Empty until Tacview has a jet.
func (t *Tower) HomePlate() (callsign string, freq radio.Frequency) {
	st := t.PrimaryAircraft()
	if st == nil {
		return "", radio.Frequency{}
	}
	af := st.Owner
	if af == nil {
		af = st.Nearest
	}
	if af == nil {
		return "", radio.Frequency{}
	}
	f := uhfOf(af)
	if f.Hz == 0 {
		return "", radio.Frequency{}
	}
	return af.Callsign(airfield.RoleTower), f
}

func (t *Tower) cfgCallsign(af *airfield.Airfield, role Role) string {
	if role == RoleCenter {
		return CenterCallsign
	}
	if af != nil {
		return af.Callsign(airfield.Role(role))
	}
	return string(role)
}

func (t *Tower) findByPilot(pilot string) *AircraftState {
	if pilot == "" {
		return nil
	}
	pilot = strings.ToLower(pilot)
	var best *AircraftState
	for _, st := range t.aircraft {
		if !strings.Contains(strings.ToLower(st.Pilot), pilot) &&
			!strings.Contains(strings.ToLower(st.Callsign), pilot) {
			continue
		}
		if betterAC(st, best) {
			best = st
		}
	}
	return best
}

func (t *Tower) nearestOnGround() *AircraftState {
	var best, demo *AircraftState
	bestDist := 1e9
	for _, st := range t.aircraft {
		if !st.OnGround || st.Nearest == nil {
			continue
		}
		if strings.HasPrefix(st.ID, "demo-") {
			demo = st
			continue
		}
		if st.DistanceNM < bestDist {
			bestDist = st.DistanceNM
			best = st
		}
	}
	if best != nil {
		return best
	}
	return demo
}

func (t *Tower) nearestInAir() *AircraftState {
	var best, demo *AircraftState
	bestDist := 1e9
	for _, st := range t.aircraft {
		if st.OnGround || st.Nearest == nil {
			continue
		}
		if strings.HasPrefix(st.ID, "demo-") {
			demo = st
			continue
		}
		if st.DistanceNM < bestDist {
			bestDist = st.DistanceNM
			best = st
		}
	}
	if best != nil {
		return best
	}
	return demo
}

// PrimaryAircraft is the live Tacview jet, not the seeded dummy.
func (t *Tower) PrimaryAircraft() *AircraftState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.primaryLocked()
}

// OccupiedField is a field that has a live player nearby.
type OccupiedField struct {
	Field      *airfield.Airfield
	DistanceNM float64
	Heading    float64
}

// OccupiedFields is every airfield a live player is using (you and friends).
func (t *Tower) OccupiedFields() []OccupiedField {
	t.mu.RLock()
	defer t.mu.RUnlock()
	primary := t.primaryLocked()
	seen := map[string]bool{}
	var out []OccupiedField
	add := func(st *AircraftState) {
		if st == nil || !liveAC(st) || strings.HasPrefix(st.ID, "demo-") {
			return
		}
		af := st.Owner
		if af == nil {
			af = st.Nearest
		}
		if af == nil || seen[af.ID] {
			return
		}
		seen[af.ID] = true
		out = append(out, OccupiedField{Field: af, DistanceNM: st.DistanceNM, Heading: st.Heading})
	}
	add(primary)
	for _, st := range t.aircraft {
		if st == primary {
			continue
		}
		if !playerish(st) && st.RadioGUID == "" {
			continue
		}
		add(st)
	}
	return out
}

func decisionFits(intent Intent, text string, d Decision) bool {
	switch d.Intent {
	case "unknown", "say_again", "info", "radio_check", "contact", "check_in", "traffic":
		return true
	case "taxi":
		return intent == IntentTaxi || containsAny(text, "taxi", "need a cab")
	case "takeoff":
		return intent == IntentTakeoff || containsAny(text, "takeoff", "take off", "departure", "kickoff", "ready")
	case "landing", "inbound":
		return intent == IntentLanding || intent == IntentInbound ||
			containsAny(text, "inbound", "landing", "land", "final")
	case "startup":
		return intent == IntentStartup || containsAny(text, "start", "startup", "engine")
	case "parking":
		return intent == IntentParking || containsAny(text, "park", "parking", "ramp")
	case "hold_short":
		return intent == IntentHoldShort || containsAny(text, "hold short")
	case "go_around":
		return intent == IntentGoAround || containsAny(text, "go around", "going around")
	default:
		return true
	}
}

func containsAny(text string, phrases ...string) bool {
	for _, p := range phrases {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func runwayLabel(af *airfield.Airfield) string {
	if af != nil && len(af.Runways) > 0 {
		return SpeakRunway(af.Runways[0].Name)
	}
	return "runway"
}

func (t *Tower) resolveCaller(call radio.ReceivedCall, preferGround bool) (*AircraftState, *airfield.Airfield, string) {
	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	pilot := "Aircraft"
	var af *airfield.Airfield
	if st != nil {
		if st.Callsign != "" {
			pilot = st.Callsign
		} else if st.Pilot != "" {
			pilot = st.Pilot
		}
		af = st.Owner
		if af == nil {
			af = st.Nearest
		}
	} else if cs := heardCallsign(call); cs != "" {
		pilot = cs
	}
	return st, af, pilot
}

func liveAC(st *AircraftState) bool {
	return st != nil && time.Since(st.LastSeen) < 45*time.Second
}

func compactCS(s string) string {
	s = strings.ToLower(SpeakCallsign(s))
	repl := []struct{ from, to string }{
		{"niner", "9"}, {"zero", "0"}, {"one", "1"}, {"two", "2"},
		{"three", "3"}, {"four", "4"}, {"five", "5"}, {"six", "6"},
		{"seven", "7"}, {"eight", "8"}, {"nine", "9"},
	}
	for _, p := range repl {
		s = strings.ReplaceAll(s, p.from, p.to)
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (t *Tower) matchCallsignInText(text string) *AircraftState {
	blob := compactCS(text)
	if len(blob) < 5 {
		return nil
	}
	var hits []*AircraftState
	for _, st := range t.aircraft {
		if strings.HasPrefix(st.ID, "demo-") || !liveAC(st) {
			continue
		}
		key := compactCS(st.Callsign)
		if key == "" {
			key = compactCS(st.Pilot)
		}
		if len(key) < 5 {
			continue
		}
		if strings.Contains(blob, key) {
			hits = append(hits, st)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return nil
}

func (t *Tower) identifyCaller(call radio.ReceivedCall) *AircraftState {
	guid := strings.TrimSpace(call.GUID)
	if guid != "" {
		if id := t.binds[guid]; id != "" {
			if st := t.aircraft[id]; liveAC(st) {
				return st
			}
		}
		for _, st := range t.aircraft {
			if st.RadioGUID == guid && liveAC(st) {
				return st
			}
		}
	}
	if st := t.matchCallsignInText(call.Transcript); st != nil {
		return st
	}
	name := strings.TrimSpace(call.Pilot)
	if name != "" && !strings.EqualFold(name, "Pilot") {
		for _, part := range splitRadioName(name) {
			if st := t.findByPilot(part); st != nil && liveAC(st) {
				a := compactCS(st.Callsign)
				b := compactCS(part)
				pa := compactCS(st.Pilot)
				if a != "" && b != "" && (a == b || strings.Contains(a, b) || strings.Contains(b, a)) {
					return st
				}
				if pa != "" && b != "" && (pa == b || strings.Contains(pa, b) || strings.Contains(b, pa)) {
					return st
				}
			}
		}
	}
	// Xbox / typed calls have no SRS GUID — local jet only.
	if guid == "" {
		return t.primaryLocked()
	}
	if st := t.soleUnboundOtherLocked(); st != nil {
		fmt.Printf("  binding %s to the other jet Tacview has (%s)\n", radioCallsign(call.Pilot), st.Callsign)
		return st
	}
	return nil
}

func heardCallsign(call radio.ReceivedCall) string {
	cs := radioCallsign(call.Pilot)
	if !usableCallsign(cs) {
		return ""
	}
	return cs
}

func usableCallsign(cs string) bool {
	cs = strings.TrimSpace(cs)
	if cs == "" {
		return false
	}
	switch strings.ToLower(cs) {
	case "player", "pilot", "aircraft", "unknown", "station", "station calling":
		return false
	}
	return true
}

// radioCallsign prefers the right side of "Player | Sabre 1-1".
func radioCallsign(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "|"); i >= 0 {
		if right := strings.TrimSpace(name[i+1:]); right != "" {
			return right
		}
	}
	return SpeakCallsign(name)
}

// splitRadioName breaks "Spaz619 | Sabre 1-1" into the full string, the callsign, then the player name.
func splitRadioName(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	out := []string{name}
	if i := strings.LastIndex(name, "|"); i >= 0 {
		right := strings.TrimSpace(name[i+1:])
		left := strings.TrimSpace(name[:i])
		if right != "" {
			out = append(out, right)
		}
		if left != "" {
			out = append(out, left)
		}
	}
	return out
}

func (t *Tower) soleUnboundOtherLocked() *AircraftState {
	primary := t.primaryLocked()
	var only *AircraftState
	n := 0
	for _, st := range t.aircraft {
		if st == nil || st == primary || !liveAC(st) || strings.HasPrefix(st.ID, "demo-") {
			continue
		}
		if st.RadioGUID != "" {
			continue
		}
		n++
		only = st
	}
	if n == 1 {
		return only
	}
	return nil
}

func (t *Tower) trackedNames() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var parts []string
	for _, st := range t.aircraft {
		if st == nil || strings.HasPrefix(st.ID, "demo-") || !liveAC(st) {
			continue
		}
		cs := st.Callsign
		if cs == "" {
			cs = st.Pilot
		}
		if cs == "" {
			cs = st.ID
		}
		parts = append(parts, cs)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (t *Tower) bindGUID(guid string, st *AircraftState) {
	guid = strings.TrimSpace(guid)
	if guid == "" || st == nil {
		return
	}
	if st.RadioGUID == guid && t.binds[guid] == st.ID {
		return
	}
	for _, o := range t.aircraft {
		if o != nil && o.RadioGUID == guid && o.ID != st.ID {
			o.RadioGUID = ""
		}
	}
	st.RadioGUID = guid
	t.binds[guid] = st.ID
	fmt.Printf("  locked SRS -> %s\n", SpeakCallsign(st.Callsign))
}

func (t *Tower) SeedDemoAircraft(airfieldID, pilot string) error {
	af, ok := t.airfields.GetByID(airfieldID)
	if !ok {
		af, ok = t.airfields.GetByName(airfieldID)
	}
	if !ok {
		return fmt.Errorf("unknown airfield %q", airfieldID)
	}
	if pilot == "" {
		pilot = "Viper 1-1"
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	id := "demo-" + strings.ToLower(strings.ReplaceAll(pilot, " ", "-"))
	t.aircraft[id] = &AircraftState{
		ID:         id,
		Pilot:      pilot,
		Callsign:   SpeakCallsign(pilot),
		Type:       "F-16C_50",
		Latitude:   af.Latitude,
		Longitude:  af.Longitude,
		AltitudeFt: af.ElevationFt + 5,
		OnGround:   true,
		Nearest:    af,
		DistanceNM: 0.1,
		LastSeen:   time.Now(),
		Phase:      "parked",
	}
	t.log.Info("seeded demo aircraft", "pilot", pilot, "airfield", af.Name)
	return nil
}

func (t *Tower) LastCall() (callsign, text string) {
	t.lastMu.Lock()
	defer t.lastMu.Unlock()
	return t.lastCallsign, t.lastText
}

func (t *Tower) Stats() (total, onGround, airborne int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, st := range t.aircraft {
		total++
		if st.OnGround {
			onGround++
		} else {
			airborne++
		}
	}
	return
}

