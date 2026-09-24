package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skycontrol/skycontrol/internal/airfield"
	"github.com/skycontrol/skycontrol/internal/atc"
	"github.com/skycontrol/skycontrol/internal/config"
	"github.com/skycontrol/skycontrol/internal/radio"
	"github.com/skycontrol/skycontrol/internal/stt"
	"github.com/skycontrol/skycontrol/internal/telemetry"
	"github.com/skycontrol/skycontrol/internal/ui"
	"github.com/skycontrol/skycontrol/internal/voice"
	"github.com/skycontrol/skycontrol/internal/weather"
)

const Version = "12"

type App struct {
	cfg       *config.Config
	log       *slog.Logger
	airfields *airfield.Database
	tacview   *telemetry.Client
	radio     radio.Client
	tower     *atc.Tower
	speaker   voice.Speaker
	stt       *stt.Pipeline
	mic       *stt.Mic
	atis      *atc.ATIS
	atisRadio radio.Client
	listen    *radio.NativeClient
}

func New(cfg *config.Config) (*App, error) {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	logFile, err := os.OpenFile("skycontrol.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logFile = os.Stdout
	}
	log := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: level}))

	db := airfield.NewDatabase()
	airfieldDir := filepath.Join("data", "airfields")
	if err := db.LoadDir(airfieldDir); err != nil {
		log.Warn("could not load airfield database (continuing anyway)", "error", err, "dir", airfieldDir)
	} else {
		log.Info("airfield database loaded", "count", db.Count())
	}

	tv := telemetry.NewClient(cfg.TelemetryAddress, cfg.TelemetryPassword, log)

	var freqs []radio.Frequency
	for _, s := range cfg.SRSFrequencies {
		f, err := radio.ParseFrequency(s)
		if err != nil {
			log.Warn("invalid frequency in config, skipping", "value", s, "error", err)
			continue
		}
		freqs = append(freqs, f)
	}

	speaker, err := voice.New(voice.Config{
		Provider: cfg.VoiceProvider,
		Voice:    cfg.VoiceName,
		Speed:    cfg.VoiceSpeed,
		ModelDir: "data/voices",
		APIKey:   cfg.ElevenLabsAPIKey,
		VoiceID:  cfg.ElevenLabsVoiceID,
		Model:    cfg.ElevenLabsModel,
		Log:      log,
	})
	if err != nil {
		log.Warn("voice init issue", "error", err)
	}
	log.Info("voice ready", "name", speaker.Name(), "provider", cfg.VoiceProvider)

	// Second string is the station callsign. This speaker still uses one voice.
	textToWav := func(text, _ string) (string, error) {
		if speaker == nil {
			return "", fmt.Errorf("no speaker")
		}
		return speaker.SayTempFile(text)
	}

	srs := radio.NewClientFromConfig(radio.Config{
		Address:     cfg.SRSAddress,
		ClientName:  "Sky Control",
		EAMPassword: cfg.SRSPassword,
		Coalition:   cfg.SRSCoalition,
		Frequencies: freqs,
		Log:         log,
	}, cfg.SRSExternalAudio, textToWav)

	tower := atc.NewTower(log, db, srs)
	wind := weather.NewReader(log, weather.Config{
		File:    cfg.WindFile,
		From:    cfg.WindFrom,
		SpeedKt: cfg.WindSpeedKt,
	})
	tower.SetWind(wind)
	fmt.Println("  wind:", wind.String())
	agent := atc.NewAgent(atc.AgentConfig{
		Enabled: cfg.AIEnabled,
		APIKey:  cfg.AIAPIKey,
		BaseURL: cfg.AIBaseURL,
		Model:   cfg.AIModel,
		Log:     log,
	})
	tower.SetAgent(agent)
	if agent.Enabled() {
		log.Info("ATC AI agent ready", "model", cfg.AIModel)
		fmt.Println("  AI agent ON —", cfg.AIModel)
	} else {
		log.Info("ATC AI agent off — keyword ATC only (set ai-api-key in config.yaml)")
		fmt.Println("  AI agent OFF — put your OpenAI key in config.yaml")
	}
	if _, isStub := srs.(*radio.StubClient); isStub {
		tower.SetSpeaker(func(text string) {
			path, err := speaker.SayTempFile(text)
			if err != nil {
				fmt.Printf("  tts failed: %v\n", err)
				return
			}
			if path == "" {
				return
			}
			defer os.Remove(path)
			_ = voice.PlayWAV(path)
		})
		fmt.Println("  ATC radio: speakers (SRS not found)")
	} else {
		log.Info("ATC audio will go out over SRS native")
	}

	tv.OnUpdate(func(obj *telemetry.Object) {
		tower.UpdateAircraft(obj)
	})

	recognizer, _ := stt.New(stt.Config{
		Provider: "stub",
		Language: "en",
		Log:      log,
	})
	sttPipe := stt.NewPipeline(recognizer, log)
	mic := stt.NewMic(log, cfg.PTTKey)
	mic.SetWhisper(cfg.AIAPIKey, cfg.AIBaseURL)
	if b, ok := srs.(interface{ SetBusy(func(bool)) }); ok {
		b.SetBusy(func(on bool) {
			if on {
				mic.Deaf(30 * time.Second)
			} else {
				mic.Hear()
			}
		})
	}

	var atisRadio radio.Client
	var atisSvc *atc.ATIS
	if _, isStub := srs.(*radio.StubClient); !isStub {
		atisRadio = radio.NewNativeClient(radio.Config{
			Address:     cfg.SRSAddress,
			ClientName:  "Information",
			EAMPassword: cfg.SRSPassword,
			Coalition:   cfg.SRSCoalition,
			Log:         log,
		}, textToWav)
		atisSvc = atc.NewATIS(log, tower, atisRadio, speaker)
	}

	var listen *radio.NativeClient
	if nc, ok := srs.(*radio.NativeClient); ok {
		listen = nc
		if w := stt.NewWhisper(cfg.AIAPIKey, cfg.AIBaseURL, log); w.Enabled() {
			listen.SetTranscriber(w.Transcribe)
			fmt.Println("  SRS listen ON — PTT your SRS radio; ATC answers on that freq")
		} else {
			fmt.Println("  SRS listen needs Whisper (ai-api-key) to hear you")
		}
	}

	return &App{
		cfg:       cfg,
		log:       log,
		airfields: db,
		tacview:   tv,
		radio:     srs,
		tower:     tower,
		speaker:   speaker,
		stt:       sttPipe,
		mic:       mic,
		atis:      atisSvc,
		atisRadio: atisRadio,
		listen:    listen,
	}, nil
}

func (a *App) Run() error {
	a.log.Info("Sky Control starting",
		"version", Version,
		"voice-provider", a.cfg.VoiceProvider,
		"voice-name", a.cfg.VoiceName,
		"default-callsign", a.cfg.DefaultCallsign,
		"telemetry", a.cfg.TelemetryAddress,
		"srs", a.cfg.SRSAddress,
		"airfields", a.airfields.Count(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	radio.KillOrphanExternalAudio()
	fmt.Printf("  Sky Control v%s\n", Version)
	fmt.Println("  radio hygiene: cleared leftover ExternalAudio")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		a.log.Info("shutdown signal received", "signal", sig)
		cancel()
	}()

	go func() {
		if err := a.radio.Run(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("SRS client error", "error", err)
		}
	}()
	if a.atisRadio != nil {
		go func() {
			if err := a.atisRadio.Run(ctx); err != nil && ctx.Err() == nil {
				a.log.Error("ATIS radio error", "error", err)
			}
		}()
	}
	if a.atis != nil {
		go a.atis.Run(ctx)
	}
	go a.radioReceiveLoop(ctx)
	go a.sttLoop(ctx)
	go a.micLoop(ctx)
	go a.statusLoop(ctx)
	go a.listenTuneLoop(ctx)
	go ui.Start(ctx, ":8080", ui.Hooks{
		Log:     a.log,
		Status:  a.guiStatus,
		Command: a.guiCommand,
	})
	fmt.Println("  GUI: http://127.0.0.1:8080")

	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
			a.radio.Transmit(radio.Transmission{
				Callsign:  atc.CenterCallsign,
				Text:      "Sky Control is online.",
				Frequency: atc.CenterFrequency(),
			})
		}
	}()

	go func() {
		a.log.Info("starting Tacview real-time telemetry listener")
		if err := a.tacview.Run(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("telemetry error", "error", err)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	a.runConsole(ctx)
	cancel()
	radio.KillOrphanExternalAudio()
	a.log.Info("Sky Control stopped")
	return nil
}

func (a *App) micLoop(ctx context.Context) {
	if a.mic == nil {
		return
	}
	ch := make(chan string, 8)
	if err := a.mic.Start(ctx, ch); err != nil {
		a.log.Warn("mic not started — typed commands still work", "error", err)
		fmt.Println("  mic off (typed radio still works)")
		return
	}
	fmt.Println("  SRS PTT — talk on your radio")
	if a.cfg.AIEnabled && strings.TrimSpace(a.cfg.AIAPIKey) != "" && a.cfg.AIAPIKey != "sk-YOUR_KEY_HERE" {
		fmt.Println("  AI agent ON —", a.cfg.AIModel)
	} else {
		fmt.Println("  AI agent OFF — put your OpenAI key in config.yaml")
	}
	_ = os.Stdout.Sync()
	for {
		select {
		case <-ctx.Done():
			return
		case text, ok := <-ch:
			if !ok {
				return
			}
			a.stt.InjectTranscript(text)
		}
	}
}

func (a *App) radioReceiveLoop(ctx context.Context) {
	if a.listen == nil {
		return
	}
	ch := a.listen.Received()
	for {
		select {
		case <-ctx.Done():
			return
		case call, ok := <-ch:
			if !ok {
				return
			}
			if call.Transcript == "" {
				continue
			}
			if a.mic != nil {
				a.mic.SetSRSPrimary(true)
				a.mic.Deaf(8 * time.Second)
			}
			a.log.Info("SRS listen transcript", "pilot", call.Pilot, "freq", call.Frequency.String(), "text", call.Transcript)
			go func(call radio.ReceivedCall) {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("  radio handler crashed: %v\n", r)
					}
				}()
				if a.tower.HandleRadioCall(call) {
					a.log.Info("tower handled SRS call", "pilot", call.Pilot, "transcript", call.Transcript)
				}
				fmt.Print("skycontrol> ")
			}(call)
		}
	}
}

func (a *App) sttLoop(ctx context.Context) {
	if a.stt == nil {
		return
	}
	ch := a.stt.Transcripts()
	for {
		select {
		case <-ctx.Done():
			a.stt.Close()
			return
		case tr, ok := <-ch:
			if !ok {
				return
			}
			a.log.Info("STT transcript", "text", tr.Text, "confidence", tr.Confidence)
			fmt.Printf("\n  YOU SAID: %s\n", tr.Text)
			if a.mic != nil {
				a.mic.Deaf(8 * time.Second)
			}
			call := radio.ReceivedCall{
				Transcript: tr.Text,
				ReceivedAt: time.Now(),
			}
			if st := a.tower.PrimaryAircraft(); st != nil {
				call.Pilot = st.Callsign
			}
			if a.tower.HandleRadioCall(call) {
				a.log.Info("tower handled STT call", "transcript", tr.Text)
			}
			fmt.Print("skycontrol> ")
		}
	}
}

func (a *App) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.tower.TickHandoff()
			aircraft := a.tacview.Aircraft()
			total, onGround, airborne := a.tower.Stats()
			a.log.Info("status",
				"tracked_aircraft", len(aircraft),
				"tower_total", total,
				"tower_ground", onGround,
				"tower_airborne", airborne,
				"airfields_loaded", a.airfields.Count(),
				"wind", a.tower.WindStatus(),
			)
			airN, lines := a.tacview.Census()
			fmt.Printf("  telemetry: %d air\n", airN)
			for _, line := range lines {
				fmt.Println("   ", line)
			}
		}
	}
}

func (a *App) listenSet() []radio.Frequency {
	seen := map[int64]bool{}
	var out []radio.Frequency
	add := func(f radio.Frequency) {
		if f.Hz < 1_000_000 {
			return
		}
		key := int64(f.Hz / 1000)
		if seen[key] {
			return
		}
		seen[key] = true
		if f.Modulation == "" {
			f.Modulation = "AM"
		}
		out = append(out, f)
	}
	addField := func(af *airfield.Airfield) {
		if af == nil {
			return
		}
		if f, err := radio.ParseFrequency(af.PrimaryTowerFreq()); err == nil {
			add(f)
		}
		if f, err := radio.ParseFrequency(af.PrimaryGroundFreq()); err == nil {
			add(f)
		}
	}
	add(atc.CenterFrequency())
	for _, s := range a.cfg.SRSFrequencies {
		if f, err := radio.ParseFrequency(s); err == nil {
			add(f)
		}
	}
	// Friends first: every live player's field stays in the 11-slot radio.
	occupied := a.tower.OccupiedFields()
	for _, of := range occupied {
		addField(of.Field)
	}
	st := a.tower.PrimaryAircraft()
	if st != nil {
		addField(st.Owner)
		addField(st.Nearest)
		addField(st.Dest)
	}
	if st != nil && a.airfields != nil {
		m := ""
		if st.Nearest != nil {
			m = st.Nearest.Map
		}
		for _, n := range a.airfields.NearbyOnMap(m, st.Latitude, st.Longitude, 10) {
			if n.TowerFreq != "" {
				if f, err := radio.ParseFrequency(n.TowerFreq); err == nil {
					add(f)
				}
			}
			if n.GroundFreq != "" {
				if f, err := radio.ParseFrequency(n.GroundFreq); err == nil {
					add(f)
				}
			}
		}
	}
	if len(out) > 11 {
		out = out[:11]
	}
	return out
}

func (a *App) listenTuneLoop(ctx context.Context) {
	if a.listen == nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			freqs := a.listenSet()
			if len(freqs) > 0 {
				a.listen.SetFrequencies(freqs)
			}
			if a.listen.Connected() && a.mic != nil {
				a.mic.SetSRSPrimary(true)
			}
			if st := a.tower.PrimaryAircraft(); st != nil {
				a.listen.SetPosition(st.Latitude, st.Longitude, st.AltitudeFt)
			}
			var b strings.Builder
			for i, f := range freqs {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "%.3f", f.Hz/1_000_000)
			}
			sig := b.String()
			if sig != last && sig != "" {
				last = sig
				fmt.Printf("  SRS listen freqs: %s\n", sig)
			}
		}
	}
}

func (a *App) Config() *config.Config        { return a.cfg }
func (a *App) Airfields() *airfield.Database { return a.airfields }
func (a *App) Radio() radio.Client           { return a.radio }
func (a *App) Tower() *atc.Tower             { return a.tower }

func (a *App) guiStatus() ui.Status {
	st := a.tower.PrimaryAircraft()
	cs, last := a.tower.LastCall()
	s := ui.Status{
		LastATC: last,
		LastCS:  cs,
	}
	s.Aircraft, _, _ = a.tower.Stats()
	if st != nil {
		s.Callsign = st.Callsign
		s.OnGround = st.OnGround
		s.DistNM = st.DistanceNM
		if st.Nearest != nil {
			s.Airfield = st.Nearest.Name
		}
	}
	if a.atis != nil {
		s.ATIS = a.atis.Status()
	}
	tv := "waiting"
	if st != nil {
		tv = "connected"
	}
	srs := "offline"
	if _, stub := a.radio.(*radio.StubClient); !stub {
		srs = "live"
	}
	if a.listen != nil && a.listen.Connected() {
		srs = "listen"
	}
	mic := "typed"
	if a.mic != nil && a.cfg.PTTKey != "OFF" {
		mic = "ptt"
	}
	ai := "off"
	if a.cfg.AIEnabled && strings.TrimSpace(a.cfg.AIAPIKey) != "" && a.cfg.AIAPIKey != "sk-YOUR_KEY_HERE" {
		ai = "on"
	}
	s.Connections = []ui.Connection{
		{ID: "telemetry", Label: "Position", State: tv},
		{ID: "radio", Label: "Radio", State: srs},
		{ID: "mic", Label: "Mic", State: mic},
		{ID: "ai", Label: "Controller", State: ai},
	}
	return s
}

func (a *App) guiCommand(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	call := radio.ReceivedCall{Transcript: text, ReceivedAt: time.Now()}
	if st := a.tower.PrimaryAircraft(); st != nil {
		call.Pilot = st.Callsign
	}
	_ = a.tower.HandleRadioCall(call)
	_, last := a.tower.LastCall()
	return last
}
