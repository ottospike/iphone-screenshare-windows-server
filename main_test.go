package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Real line from the user's log (curly apostrophe in the name).
const realLine = "connection request from Otavio Augusto’s iPhone (iPhone18,2) with deviceID = 22:C5:83:7E:F0:3F"

// Real geometry line from the user's UxPlay 1.74 log.
const realGeometry = "*** codec is h264: geometry is (w,h)=(498,1080) (source); (x,y)=(711,0); (w,h)=(498,1080)"

func withConfig(t *testing.T, c Config) {
	t.Helper()
	before, brokenBefore, readBefore, unsavedBefore, diskBefore := cfg, configBroken, readFailed, unsaved, diskCfg
	cfg, readFailed, unsaved, diskCfg = c, false, false, defaultConfig()
	t.Cleanup(func() {
		cfg, configBroken, readFailed, unsaved, diskCfg = before, brokenBefore, readBefore, unsavedBefore, diskBefore
	})
}

func testConfig(b map[string]Binding) Config {
	c := defaultConfig()
	c.Bindings = b
	return c
}

func withConfigFile(t *testing.T) {
	t.Helper()
	before, stampBefore := configPath, diskStamp
	configPath, diskStamp = filepath.Join(t.TempDir(), "config.json"), fileStamp{}
	t.Cleanup(func() { configPath, diskStamp = before, stampBefore })
}

func TestIdle(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 1, 20, 0, 0, time.Local)
	cases := []struct {
		name string
		s    Slot
		want bool
	}{
		{"no process", Slot{}, true},
		{"never connected", Slot{cmd: &exec.Cmd{}}, true},
		{"accepted 10 s ago, no video yet", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-10 * time.Second)}, false},
		{"mirroring", Slot{cmd: &exec.Cmd{}, mirroring: true, connected: t0.Add(-time.Hour)}, false},
		{"stopped 3 s ago", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-time.Hour), ended: t0.Add(-3 * time.Second)}, false},
		{"stopped 6 s ago", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-time.Hour), ended: t0.Add(-6 * time.Second)}, true},
		{"accepted 21 s ago without video", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-21 * time.Second)}, true},
	}
	for _, c := range cases {
		if got := c.s.idleAt(t0); got != c.want {
			t.Errorf("%s: idle=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestWantedSlots(t *testing.T) {
	b := func(ks ...string) map[string]Binding {
		m := map[string]Binding{}
		for _, k := range ks {
			m[k] = Binding{DeviceID: "id" + k}
		}
		return m
	}
	cases := []struct {
		name    string
		max     int
		b       map[string]Binding
		busy    map[int]bool
		current int
		want    []int
		free    int
	}{
		{"no binding", 8, b(), nil, 0, []int{1}, 1},
		{"slot 1 bound", 8, b("1"), nil, 1, []int{1, 2}, 2},
		{"real config: only slot 2", 8, b("2"), nil, 0, []int{1, 2}, 1},
		{"all bound", 2, b("1", "2"), nil, 0, []int{1, 2}, 0},
		{"single mode", 1, b("1"), nil, 1, []int{1}, 1},
		{"single mode with someone mirroring", 1, b(), map[int]bool{1: true}, 1, nil, 0},
		{"binding above the limit", 3, b("5"), nil, 0, []int{1}, 1},
		// unbound while the device is still mirroring: the free one stays slot 2
		{"slot 1 unbound and busy", 8, b(), map[int]bool{1: true}, 2, []int{2}, 2},
		{"slot 1 unbound busy, no free yet", 8, b(), map[int]bool{1: true}, 0, []int{2}, 2},
		// unbound and idle: slot 2 stays free (the free slot doesn't move), slot 1 goes down
		{"slot 1 unbound and idle", 8, b(), nil, 2, []int{2}, 2},
		{"current free got bound", 8, b("2"), nil, 2, []int{1, 2}, 1},
		{"current free above maxSlots", 2, b("1"), nil, 5, []int{1, 2}, 2},
		{"all bound, an unbound idle one becomes free", 2, b("2"), nil, 0, []int{1, 2}, 1},
	}
	for _, c := range cases {
		got, free := wantedSlots(c.max, c.b, c.busy, c.current)
		var ns []int
		for n := 1; n <= slotLimit; n++ {
			if got[n] {
				ns = append(ns, n)
			}
		}
		if !slices.Equal(ns, c.want) || free != c.free {
			t.Errorf("%s: %v free %d, want %v free %d", c.name, ns, free, c.want, c.free)
		}
	}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestSlotArgs(t *testing.T) {
	c := testConfig(map[string]Binding{"2": {DeviceID: "X"}, "3": {DeviceID: "Y"}, "9": {DeviceID: "Z"}})
	c.ExtraArgs = []string{"-reset", "30"}
	withConfig(t, c)

	owner := slotArgs(&Slot{N: 2})
	if !hasSeq(owner, "-restrict", "-allow", "X", "-nohold") || !hasSeq(owner, "-block", "Y") || hasSeq(owner, "-block", "X") {
		t.Errorf("owner slot: %v", owner)
	}
	if !hasSeq(owner, "-n", "LIVE 2") || !hasSeq(owner, "-p", "35020") || !hasSeq(owner, "-vs", "d3d11videosink") ||
		!hasSeq(owner, "-h265", "-s", "3840x3840", "-fps", "60") {
		t.Errorf("owner slot without name/port/resolution/sink: %v", owner)
	}
	if !hasSeq(owner[len(owner)-2:], "-reset", "30") {
		t.Errorf("extraArgs must come last: %v", owner)
	}

	free := slotArgs(&Slot{N: 1})
	if slices.Contains(free, "-allow") || slices.Contains(free, "-restrict") || slices.Contains(free, "-nohold") {
		t.Errorf("free slot can't have -restrict/-allow/-nohold: %v", free)
	}
	if !hasSeq(free, "-block", "X") || !hasSeq(free, "-block", "Y") || slices.Contains(free, "Z") {
		t.Errorf("free slot: blocks valid bindings and ignores the one above the limit: %v", free)
	}
	if !slices.Equal(free, slotArgs(&Slot{N: 1})) {
		t.Errorf("slotArgs must be deterministic (restart compares it)")
	}
	if !slices.Contains(free, slotMAC(1)) || slotMAC(1) == slotMAC(2) {
		t.Errorf("each slot with its own -m: %v", free)
	}

	cfg.MaxSlots = 1
	single := slotArgs(&Slot{N: 1})
	if !hasSeq(single, "-n", "LIVE") || slices.Contains(single, "-block") || slices.Contains(single, "-restrict") {
		t.Errorf("maxSlots 1: a single LIVE, no binding: %v", single)
	}
}

func TestResolutionAndFPSArgs(t *testing.T) {
	cases := []struct {
		res  string
		fps  int
		want []string
	}{
		{"native", 60, []string{"-h265", "-s", "3840x3840", "-fps", "60"}},
		{"fhd", 60, []string{"-s", "1920x1920", "-fps", "60"}},
		{"hd", 30, []string{"-s", "1280x1280", "-fps", "30"}},
		{"sd", 30, []string{"-s", "854x854", "-fps", "30"}},
	}
	if len(cases) != len(resolutionOrder) || len(resolutions) != len(resolutionOrder) || len(resolutionLabels) != len(resolutionOrder) {
		t.Fatalf("every preset must be tested, in the menu and labeled")
	}
	for _, c := range cases {
		cf := defaultConfig()
		cf.Resolution, cf.FPS = c.res, c.fps
		withConfig(t, cf)
		args := slotArgs(&Slot{N: 1})
		if !hasSeq(args, c.want...) {
			t.Errorf("%s/%d: %v", c.res, c.fps, args)
		}
		if c.res != "native" && slices.Contains(args, "-h265") {
			t.Errorf("%s must be H.264: %v", c.res, args)
		}
	}
}

func TestSlotMAC(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	seen := map[string]bool{}
	for n := 1; n <= slotLimit; n++ {
		m := slotMAC(n)
		if !re.MatchString(m) || m != slotMAC(n) || seen[m] {
			t.Fatalf("MAC of slot %d: %q", n, m)
		}
		seen[m] = true
		var b0 byte
		for i, c := range m[:2] {
			b0 |= byte(strings.IndexRune("0123456789abcdef", c)) << (4 * (1 - i))
		}
		if b0&0x01 != 0 || b0&0x02 == 0 {
			t.Errorf("MAC %s: must be unicast and locally administered", m)
		}
	}
}

func TestReConnection(t *testing.T) {
	m := reConnection.FindStringSubmatch(realLine)
	if m == nil || m[1] != "Otavio Augusto’s iPhone" || m[3] != "22:C5:83:7E:F0:3F" {
		t.Fatalf("real line: %q", m)
	}
	m = reConnection.FindStringSubmatch("connection request from Ana (2) (iPhone15,3) with deviceID = AA:BB:CC:DD:EE:FF")
	if m == nil || m[1] != "Ana (2)" || m[3] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("name with parentheses: %q", m)
	}
}

func TestParseGeometry(t *testing.T) {
	if w, h, ok := parseGeometry(realGeometry); !ok || w != 498 || h != 1080 {
		t.Fatalf("real line: %d %d %v", w, h, ok)
	}
	if w, h, ok := parseGeometry("*** codec is h265: geometry is (w,h)=(2868,1320) (source); (w,h)=(1920,884) (displayed)"); !ok || w != 2868 || h != 1320 {
		t.Fatalf("h265 landscape: %d %d %v", w, h, ok)
	}
	if _, _, ok := parseGeometry("Begin streaming to GStreamer video pipeline"); ok {
		t.Fatalf("matched an unrelated line")
	}
	if o := orientation(498, 1080); o != "portrait 498×1080" {
		t.Errorf("portrait: %q", o)
	}
	if o := orientation(2868, 1320); o != "landscape 2868×1320" {
		t.Errorf("landscape: %q", o)
	}
	if o := orientation(0, 0); o != "" {
		t.Errorf("unknown: %q", o)
	}
}

func TestGeometryInMenuTitleAndRotation(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{"2": {DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio's iPhone"}}))
	s := &Slot{N: 2, restartCh: make(chan struct{}, 1)}
	t0 := time.Now()
	s.processLine(realLine, t0)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t0)
	s.processLine(realGeometry, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if got, _ := slotTitle(s); got != "LIVE 2 · Otavio's iPhone · live · portrait 498×1080" {
		t.Fatalf("title: %q", got)
	}
	msg, _ := s.processLine("*** codec is h264: geometry is (w,h)=(1080,498) (source); (w,h)=(1080,498) (displayed)", t0)
	if got, _ := slotTitle(s); got != "LIVE 2 · Otavio's iPhone · live · landscape 1080×498" || !strings.Contains(msg, "landscape") {
		t.Fatalf("rotation: %q %q", got, msg)
	}
	s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if got, _ := slotTitle(s); strings.Contains(got, "landscape") {
		t.Fatalf("geometry shown after mirroring stopped: %q", got)
	}
}

// Real log sequence: request, accept, video, drop.
func TestProcessLineBindsAndSeesDrop(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{}))
	s := &Slot{N: 1}
	t0 := time.Now()
	for _, l := range []string{"Accepted IPv4 client on socket 10252, port 35011", "Local : 10.0.0.102", realLine} {
		if _, b := s.processLine(l, t0); b {
			t.Fatalf("bound before accept: %q", l)
		}
	}
	if !s.connected.IsZero() {
		t.Fatalf("a request not yet accepted can't count as a connection")
	}
	msg, b := s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t0)
	if !b || cfg.Bindings["1"].DeviceID != "22:C5:83:7E:F0:3F" || !strings.Contains(msg, "bound") {
		t.Fatalf("did not bind: %q %v", msg, cfg.Bindings)
	}
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if !s.mirroring || s.state != "live" {
		t.Fatalf("not live: %+v", s)
	}
	s.processLine("Connection closed on socket 999", t0) // another connection, e.g. a refused device
	if !s.mirroring {
		t.Fatalf("another connection's socket dropped the state")
	}
	s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if s.mirroring || s.ended.IsZero() {
		t.Fatalf("did not see the drop")
	}
}

func TestProcessLineRefused(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{"2": {DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio"}}))
	s := &Slot{N: 1, state: "waiting"}
	s.processLine(realLine, time.Now())
	msg, b := s.processLine("    *** attempt to connect by blocked client (clientID 22:C5:83:7E:F0:3F): DENIED", time.Now())
	if b || !strings.Contains(msg, "refused") || !s.connected.IsZero() || s.device != "" || s.state != "waiting" {
		t.Fatalf("refusal counted as a connection: %q %+v", msg, s)
	}
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", time.Now())
	if len(cfg.Bindings) != 1 || !s.connected.IsZero() {
		t.Fatalf("an accept after a refusal can't apply to the refused device")
	}
}

func TestProcessLineBindingAboveLimit(t *testing.T) {
	c := testConfig(map[string]Binding{"5": {DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio"}})
	c.MaxSlots = 2
	withConfig(t, c)
	s := &Slot{N: 1}
	s.processLine(realLine, time.Now())
	if _, b := s.processLine("raop_rtp_mirror starting mirroring", time.Now()); !b {
		t.Fatalf("a binding above the limit should free the device for a new slot")
	}
	if _, ok := cfg.Bindings["5"]; ok || cfg.Bindings["1"].DeviceID == "" {
		t.Fatalf("bindings: %v", cfg.Bindings)
	}
}

func TestParseConfig(t *testing.T) {
	if c, _, _ := parseConfig([]byte(`{"maxSlots":40,"resolution":"4K","fps":45,"name":""}`)); c.MaxSlots != slotLimit ||
		c.Resolution != "native" || c.FPS != 60 || c.Name != "LIVE" {
		t.Errorf("normalization: %+v", c)
	}
	if c, _, err := parseConfig([]byte("\xef\xbb\xbf{\"maxSlots\":3,\"resolution\":\"sd\",\"fps\":30}")); err != nil ||
		c.MaxSlots != 3 || c.Resolution != "sd" || c.FPS != 30 {
		t.Errorf("config with Notepad BOM: %+v %v", c, err)
	}
	// Values of earlier versions are renamed; the presets themselves are kept.
	for in, want := range map[string]string{"1920": "fhd", "1280": "hd", "native": "native", "fhd": "fhd",
		"hd": "hd", "sd": "sd", "FHD": "native", "720": "native"} {
		c, notes, err := parseConfig([]byte(`{"resolution":"` + in + `"}`))
		if err != nil || c.Resolution != want {
			t.Errorf("resolution %q: got %q, want %q (%v)", in, c.Resolution, want, err)
		}
		if renamed := in == "1920" || in == "1280"; renamed != (len(notes) == 1) {
			t.Errorf("resolution %q: notes %v", in, notes)
		}
	}
	if _, _, err := parseConfig([]byte(`{"name":"LIVE",}`)); err == nil {
		t.Errorf("broken JSON must fail")
	}
	if c, _, err := parseConfig([]byte(`{}`)); err != nil || c.Resolution != "native" || c.FPS != 60 || c.MaxSlots != slotLimit {
		t.Errorf("defaults: %+v %v", c, err)
	}
}

func TestMigrateLegacyConfig(t *testing.T) {
	// The user's real config.json from the first version ("slots", argsExtras null).
	v1 := `{"nome":"LIVE","slots":2,"qualidade":"FHD","uxplay":"","argsExtras":null,
	  "vinculos":{"2":{"deviceID":"22:C5:83:7E:F0:3F","iphone":"Otavio Augusto’s iPhone"}}}`
	c, notes, err := parseConfig([]byte(v1))
	if err != nil || c.MaxSlots != slotLimit || c.Name != "LIVE" || c.Resolution != "native" || c.FPS != 60 ||
		c.ExtraArgs == nil || len(notes) != 2 {
		t.Fatalf("v1 migration: %+v %v %v", c, notes, err)
	}
	if c.Bindings["2"] != (Binding{DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio Augusto’s iPhone"}) {
		t.Fatalf("binding lost: %+v", c.Bindings)
	}
	// Config written by the Portuguese overhaul (maxSlots, HD, extra args).
	v2 := `{"nome":"PALCO","maxSlots":4,"qualidade":"HD","uxplay":"C:\\ux\\uxplay.exe","argsExtras":["-reset","30"],
	  "vinculos":{"1":{"deviceID":"AA","iphone":"A"},"3":{"deviceID":"CC","iphone":"C"}}}`
	c, _, err = parseConfig([]byte(v2))
	if err != nil || c.Name != "PALCO" || c.MaxSlots != 4 || c.Resolution != "hd" || c.UxPlay != `C:\ux\uxplay.exe` ||
		!slices.Equal(c.ExtraArgs, []string{"-reset", "30"}) || len(c.Bindings) != 2 || c.Bindings["3"].Device != "C" {
		t.Fatalf("v2 migration: %+v %v", c, err)
	}
	if c, _, _ := parseConfig([]byte(`{"slots":1}`)); c.MaxSlots != 1 {
		t.Errorf("slots 1 must become maxSlots 1, got %d", c.MaxSlots)
	}
	// English keys win over leftovers of the old ones.
	c, _, _ = parseConfig([]byte(`{"name":"NEW","nome":"OLD","bindings":{},"vinculos":{"1":{"deviceID":"AA"}}}`))
	if c.Name != "NEW" || len(c.Bindings) != 0 {
		t.Errorf("English keys must win: %+v", c)
	}
}

func TestLoadConfigRewritesLegacyInEnglish(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","maxSlots":8,"qualidade":"FHD","argsExtras":[],
	  "vinculos":{"2":{"deviceID":"22:C5:83:7E:F0:3F","iphone":"Otavio"}}}`), 0o644)
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(configPath)
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"nome", "qualidade", "argsExtras", "vinculos"} {
		if _, ok := onDisk[k]; ok {
			t.Errorf("old key %q still on disk: %s", k, b)
		}
	}
	if !strings.Contains(string(b), `"bindings"`) || !strings.Contains(string(b), `"device": "Otavio"`) {
		t.Fatalf("binding not written in English: %s", b)
	}
	// The original stays in config.json.v1 for a downgrade to a version that only reads the old keys.
	if v1, _ := os.ReadFile(configPath + ".v1"); !strings.Contains(string(v1), `"vinculos"`) {
		t.Fatalf("original not backed up: %s", v1)
	}
	// Loading the rewritten file again keeps the binding.
	if err := loadConfig(); err != nil || cfg.Bindings["2"].DeviceID != "22:C5:83:7E:F0:3F" || cfg.Resolution != "native" {
		t.Fatalf("second load: %+v %v", cfg, err)
	}
}

func TestMissingConfigCreated(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	if err := loadConfig(); err != nil || configBroken {
		t.Fatalf("missing file must load the defaults: %v", err)
	}
	if b, err := os.ReadFile(configPath); err != nil || !strings.Contains(string(b), `"resolution": "native"`) {
		t.Fatalf("defaults not written: %s %v", b, err)
	}
	if _, err := os.Stat(configPath + ".v1"); err == nil {
		t.Errorf("no backup expected for a new config")
	}
}

func TestUnreadableConfigNotOverwritten(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Binding{"1": {DeviceID: "A"}}))
	configBroken = false
	os.Mkdir(configPath, 0o755) // reading a directory fails with an error other than "not found"
	err := loadConfig()
	var re *readError
	if !errors.As(err, &re) {
		t.Fatalf("expected a read error: %v", err)
	}
	// A read error is not a broken file: saving resumes once a read works (see TestReadErrorKeepsUserEditsAndBindings).
	if configBroken {
		t.Errorf("a read error must not mark the file as broken")
	}
	if cfg.Bindings["1"].DeviceID != "A" {
		t.Errorf("config in use was changed: %+v", cfg)
	}
	saveConfig()
	writeConfig() // held back until the file is read: nothing is replaced
	if st, err := os.Stat(configPath); err != nil || !st.IsDir() {
		t.Fatalf("config path was replaced: %v", err)
	}
	// At startup the popup says the file could not be read, and no .broken copy is claimed.
	if n := startupConfigNotice(err); !strings.Contains(n, "could not be read") || strings.Contains(n, ".broken") {
		t.Errorf("wrong notice: %s", n)
	}
	if _, err := os.Stat(configPath + ".broken"); err == nil {
		t.Errorf("unexpected .broken copy")
	}
}

func TestStartupNoticeForBrokenConfig(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	broken := []byte(`{"maxSlots": 3,}`)
	os.WriteFile(configPath, broken, 0o644)
	err := loadConfig()
	if err == nil || !configBroken {
		t.Fatalf("expected a parse error: %v", err)
	}
	if n := startupConfigNotice(err); !strings.Contains(n, "has an error") || !strings.Contains(n, "config.json.broken") {
		t.Errorf("wrong notice: %s", n)
	}
	if b, _ := os.ReadFile(configPath + ".broken"); string(b) != string(broken) {
		t.Errorf("copy not saved: %s", b)
	}
}

func TestSaveConfigAtomic(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Binding{"3": {DeviceID: "C", Device: "iPad"}}))
	configBroken = false
	os.WriteFile(configPath, []byte(`{"maxSlots": 2}`), 0o644)
	saveConfig()
	if err := writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(configPath)
	var c Config
	if err != nil || json.Unmarshal(b, &c) != nil || c.Bindings["3"].DeviceID != "C" {
		t.Fatalf("config not saved: %s %v", b, err)
	}
	if _, err := os.Stat(configPath + ".tmp"); err == nil {
		t.Errorf("temp file left behind")
	}
}

func TestBrokenConfigNotTouched(t *testing.T) {
	withConfigFile(t)
	c := testConfig(map[string]Binding{"1": {DeviceID: "A"}})
	c.Resolution = "hd"
	withConfig(t, c)

	broken := []byte(`{"bindings": {"1": {"deviceID": "A"},}}`)
	os.WriteFile(configPath, broken, 0o644)
	if err := loadConfig(); err == nil {
		t.Fatalf("expected an error")
	}
	if b, _ := os.ReadFile(configPath); string(b) != string(broken) {
		t.Errorf("the user's file was overwritten: %s", b)
	}
	if cfg.Bindings["1"].DeviceID != "A" || cfg.Resolution != "hd" {
		t.Errorf("config in use was changed: %+v", cfg)
	}
}

func TestBrokenConfigNotOverwrittenOnSave(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	broken := []byte(`{"vinculos": {"1": {"deviceID": "A"},}}`)
	os.WriteFile(configPath, broken, 0o644)
	if loadConfig() == nil {
		t.Fatalf("expected an error")
	}
	cfg.Bindings["2"] = Binding{DeviceID: "B"} // new binding while the file is broken
	saveConfig()
	writeConfig()
	if b, _ := os.ReadFile(configPath); string(b) != string(broken) {
		t.Fatalf("overwrote the user's config: %s", b)
	}
	os.WriteFile(configPath, []byte(`{"maxSlots": 3}`), 0o644)
	if err := loadConfig(); err != nil || configBroken || cfg.MaxSlots != 3 || cfg.Bindings["2"].DeviceID != "B" {
		t.Fatalf("once fixed it must save again: %v %v %+v", err, configBroken, cfg)
	}
}

func TestUxPlayCmdWithoutHideWindow(t *testing.T) {
	c := uxplayCmd(`C:\x\bin\uxplay.exe`, nil)
	if c.SysProcAttr.HideWindow || c.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW || c.Dir != `C:\x\bin` {
		t.Fatalf("uxplay must start with CREATE_NO_WINDOW only: %+v dir=%s", c.SysProcAttr, c.Dir)
	}
}

func TestBusy(t *testing.T) {
	t0 := time.Now()
	if !(&Slot{active: true, cmd: &exec.Cmd{}, mirroring: true}).busyAt(t0) {
		t.Errorf("an active mirroring slot must be busy")
	}
	if (&Slot{active: false, cmd: &exec.Cmd{}, mirroring: true}).busyAt(t0) {
		t.Errorf("an inactive slot doesn't count")
	}
	if (&Slot{active: true}).busyAt(t0) {
		t.Errorf("a slot without a process is not busy")
	}
}

// Device B gets into the slot bound to A before the slot restarts with -restrict.
func TestIntruderRestartsAtOnce(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{"1": {DeviceID: "AA", Device: "A"}, "3": {DeviceID: "CC", Device: "C"}}))
	for _, id := range []string{"BB", "CC"} { // B: unbound, on A's slot; C: owner of another slot
		s := &Slot{N: 1, restartCh: make(chan struct{}, 1)}
		s.processLine("connection request from X (iPhone1,1) with deviceID = "+id, time.Now())
		_, b := s.processLine("Client identified as User-Agent: AirPlay/960.13.1", time.Now())
		if b || len(s.restartCh) != 1 {
			t.Errorf("%s: intruder must restart the slot at once (bound=%v, restart=%d)", id, b, len(s.restartCh))
		}
	}
	if len(cfg.Bindings) != 2 {
		t.Errorf("bindings changed: %v", cfg.Bindings)
	}
}

func TestUnboundDeviceNotRebound(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{}))
	s := &Slot{N: 1, restartCh: make(chan struct{}, 1), unbound: "22:C5:83:7E:F0:3F"}
	s.processLine(realLine, time.Now())
	if _, b := s.processLine("Client identified as User-Agent: AirPlay/960.13.1", time.Now()); b || len(cfg.Bindings) != 0 || len(s.restartCh) != 0 {
		t.Fatalf("rebound the device the operator just unbound: %v", cfg.Bindings)
	}
	s.unbound = "" // slot restarted
	s.processLine(realLine, time.Now())
	if _, b := s.processLine("Client identified as User-Agent: AirPlay/960.13.1", time.Now()); !b {
		t.Fatalf("after the restart it must bind")
	}
}

// With -nohold, the end of the old session arrives after the new one's Begin streaming.
func TestTakenOverSessionEndIgnored(t *testing.T) {
	withConfig(t, testConfig(map[string]Binding{"1": {DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio"}}))
	s := &Slot{N: 1, restartCh: make(chan struct{}, 1)}
	t0 := time.Now()
	s.processLine("Accepted IPv4 client on socket 10, port 35011", t0)
	s.processLine(realLine, t0)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	// Wi-Fi blipped: reconnects on a new socket while the old one is still up
	t1 := t0.Add(time.Minute)
	s.processLine("Accepted IPv4 client on socket 20, port 35011", t1)
	s.processLine(realLine, t1)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t1)
	s.processLine("Begin streaming to GStreamer video pipeline", t1)
	s.processLine("raop_rtp_mirror->running is no longer true", t1.Add(500*time.Millisecond))
	s.processLine("Connection closed on socket 10", t1.Add(600*time.Millisecond))
	if !s.mirroring {
		t.Fatalf("the old session's end dropped the new one")
	}
	s.processLine("Connection closed on socket 20", t1.Add(time.Second))
	if s.mirroring {
		t.Fatalf("the new session's end (its own socket) must count")
	}
	// The old socket closes before the new request; the old mirror thread logs its end after the
	// new session's Begin streaming.
	t2 := t1.Add(30 * time.Second)
	s.processLine("Accepted IPv4 client on socket 30, port 35011", t2)
	s.processLine(realLine, t2)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t2)
	s.processLine("Begin streaming to GStreamer video pipeline", t2)
	t3 := t2.Add(time.Minute)
	s.processLine("Accepted IPv4 client on socket 40, port 35011", t3)
	s.processLine("Connection closed on socket 30", t3)
	s.processLine(realLine, t3)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t3)
	s.processLine("Begin streaming to GStreamer video pipeline", t3)
	s.processLine("raop_rtp_mirror->running is no longer true", t3.Add(500*time.Millisecond))
	if !s.mirroring || s.state != "live" {
		t.Fatalf("the old session's late end dropped the new one: %s", s.state)
	}
	s.processLine("Connection closed on socket 40", t3.Add(time.Minute))
	if s.mirroring {
		t.Fatalf("the new session's end must count")
	}
	// normal session (no takeover): an end without socket counts at once
	t4 := t3.Add(5 * time.Minute)
	s.processLine(realLine, t4)
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t4)
	s.processLine("Begin streaming to GStreamer video pipeline", t4)
	s.processLine("lost connection with client", t4.Add(time.Second))
	if s.mirroring {
		t.Fatalf("a normal session end can't be ignored")
	}
}

func TestMergeBindings(t *testing.T) {
	base := map[string]Binding{"1": {DeviceID: "A", Device: "a"}, "6": {DeviceID: "G"}, "7": {DeviceID: "H"}}
	mem := map[string]Binding{
		"1": {DeviceID: "A", Device: "a"}, // saved: the file decides (removed by hand stays removed)
		"3": {DeviceID: "C", Device: "c"}, // never saved: kept
		"4": {DeviceID: "D", Device: "d"}, // never saved, but the file binds D elsewhere: the file wins
		"5": {DeviceID: "E", Device: "e"}, // never saved, but the file uses slot 5: the file wins
		"7": {DeviceID: "B", Device: "b"}, // H unbound and B bound, not saved: B kept
		// 6 unbound, not saved: stays unbound
	}
	file := map[string]Binding{"2": {DeviceID: "D"}, "5": {DeviceID: "F"}, "6": {DeviceID: "G"}, "7": {DeviceID: "H"}}
	kept, unbound := mergeBindings(file, mem, base)
	if !slices.Equal(kept, []string{"3", "7"}) || !slices.Equal(unbound, []string{"6"}) {
		t.Fatalf("kept %v unbound %v", kept, unbound)
	}
	want := map[string]Binding{"2": {DeviceID: "D"}, "3": {DeviceID: "C", Device: "c"}, "5": {DeviceID: "F"}, "7": {DeviceID: "B", Device: "b"}}
	if !maps.Equal(file, want) {
		t.Fatalf("file %+v", file)
	}
	// The file changed slot 6 itself: the unbind made in memory doesn't override it.
	file = map[string]Binding{"6": {DeviceID: "Z"}}
	mergeBindings(file, map[string]Binding{}, map[string]Binding{"6": {DeviceID: "G"}})
	if file["6"].DeviceID != "Z" {
		t.Fatalf("hand edit overridden: %+v", file)
	}
}

// Resolution/FPS chosen in the tray while the file was broken survive the Restart that loads the
// fixed file, unless the file changed that setting itself.
func TestTraySettingsKeptOnReload(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	os.WriteFile(configPath, []byte(`{"fps": 60,}`), 0o644)
	if loadConfig() == nil {
		t.Fatalf("expected an error")
	}
	cfg.FPS, cfg.Resolution = 30, "fhd"
	os.WriteFile(configPath, []byte(`{"fps": 60, "resolution": "1280"}`), 0o644) // old value: renamed to hd
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.FPS != 30 || cfg.Resolution != "hd" {
		t.Fatalf("fps %d resolution %s", cfg.FPS, cfg.Resolution)
	}
}

// A hand edit saved before Restart is merged, not overwritten, when the app saves a new binding.
func TestHandEditNotOverwrittenBySave(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	configBroken = false
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // a different modification time
	os.WriteFile(configPath, []byte(`{"name": "STAGE", "maxSlots": 4, "extraArgs": ["-reset", "30"]}`), 0o644)
	cfg.Bindings["1"] = Binding{DeviceID: "A", Device: "a"} // a device binds before Restart
	saveConfig()
	if err := writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(configPath)
	c, _, err := parseConfig(b)
	if err != nil || c.Name != "STAGE" || c.MaxSlots != 4 || len(c.ExtraArgs) != 2 || c.Bindings["1"].DeviceID != "A" || unsaved {
		t.Fatalf("hand edit or binding lost: %s %v unsaved=%v", b, err, unsaved)
	}
	if cfg.Name != "STAGE" {
		t.Fatalf("edit not applied in memory: %+v", cfg)
	}
}

// A save that can't replace the file (read-only) fails without touching it and keeps the change.
func TestReadOnlyConfigSaveFails(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	configBroken = false
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(configPath)
	os.Chmod(configPath, 0o444)
	t.Cleanup(func() { os.Chmod(configPath, 0o644) })
	cfg.FPS = 30
	saveConfig()
	if err := writeConfig(); err == nil {
		t.Fatalf("expected the save to fail")
	}
	if b, _ := os.ReadFile(configPath); string(b) != string(before) || !unsaved {
		t.Fatalf("file changed or change dropped: %s unsaved=%v", b, unsaved)
	}
	if _, err := os.Stat(configPath + ".tmp"); err == nil {
		t.Errorf("temp file left behind")
	}
	os.Chmod(configPath, 0o644)
	if err := writeConfig(); err != nil || unsaved {
		t.Fatalf("save after the fix: %v unsaved=%v", err, unsaved)
	}
}

func TestReadErrorKeepsUserEditsAndBindings(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Binding{}))
	configBroken = false
	os.Mkdir(configPath, 0o755)
	if loadConfig() == nil || !readFailed {
		t.Fatalf("expected a read failure")
	}
	os.Remove(configPath)
	edited := `{"name": "STAGE", "maxSlots": 3}` // the user's edit, not read yet
	os.WriteFile(configPath, []byte(edited), 0o644)
	cfg.Bindings["2"] = Binding{DeviceID: "B", Device: "b"} // a device binds meanwhile
	saveConfig()
	writeConfig()
	if b, _ := os.ReadFile(configPath); string(b) != edited || !unsaved {
		t.Fatalf("the user's edit was overwritten: %s", b)
	}
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "STAGE" || cfg.MaxSlots != 3 || cfg.Bindings["2"].DeviceID != "B" || readFailed || unsaved {
		t.Fatalf("after Restart: %+v readFailed=%v unsaved=%v", cfg, readFailed, unsaved)
	}
	if b, _ := os.ReadFile(configPath); !strings.Contains(string(b), `"deviceID": "B"`) {
		t.Fatalf("binding not saved: %s", b)
	}
}

func TestFailedSaveKeepsBindingOnReload(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Binding{"1": {DeviceID: "A", Device: "a"}}))
	configBroken = false
	saveConfig()
	writeConfig()
	// The save of a new binding fails (the file is held): memory has it, the disk doesn't.
	cfg.Bindings["3"] = Binding{DeviceID: "C", Device: "c"}
	unsaved = true
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Bindings["3"].DeviceID != "C" || cfg.Bindings["1"].DeviceID != "A" {
		t.Fatalf("binding lost on reload: %+v", cfg.Bindings)
	}
}

func TestOldVersionResetRestoredFromBackup(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Binding{"2": {DeviceID: "22:C5:83:7E:F0:3F", Device: "Otavio"}}))
	configBroken = false
	cfg.Name = "STAGE"
	saveConfig()
	writeConfig() // also writes config.json.bak
	// The old exe started on the English file and rewrote it with its own keys and no bindings.
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","slots":8,"qualidade":"FHD","uxplay":"","argsExtras":[],"vinculos":{}}`), 0o644)
	cfg, diskCfg = defaultConfig(), defaultConfig() // a fresh start
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Bindings["2"].DeviceID != "22:C5:83:7E:F0:3F" || cfg.Name != "STAGE" {
		t.Fatalf("not restored from config.json.bak: %+v", cfg)
	}
	// An old config that really has no bindings and no .bak stays as it is.
	os.Remove(configPath + ".bak")
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","vinculos":{}}`), 0o644)
	if err := loadConfig(); err != nil || len(cfg.Bindings) != 0 {
		t.Fatalf("unexpected bindings: %+v %v", cfg.Bindings, err)
	}
}

// The old exe ran on the English file, bound one device and saved: the other bindings and the
// settings come back from config.json.bak, and the old exe's binding wins its slot and device.
func TestOldVersionBindingMergedWithBackup(t *testing.T) {
	withConfigFile(t)
	c := testConfig(map[string]Binding{
		"1": {DeviceID: "A", Device: "a"}, "2": {DeviceID: "B", Device: "b"}, "3": {DeviceID: "C", Device: "c"},
		"4": {DeviceID: "D", Device: "d"}, "5": {DeviceID: "X", Device: "x"},
	})
	c.Name, c.FPS, c.MaxSlots = "STAGE", 30, 6
	withConfig(t, c)
	configBroken = false
	saveConfig()
	writeConfig()
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","slots":2,"qualidade":"FHD","uxplay":"","argsExtras":null,
	  "vinculos":{"1":{"deviceID":"X","iphone":"x"}}}`), 0o644)
	cfg, diskCfg = defaultConfig(), defaultConfig() // a fresh start
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	want := map[string]Binding{
		"1": {DeviceID: "X", Device: "x"}, "2": {DeviceID: "B", Device: "b"}, "3": {DeviceID: "C", Device: "c"},
		"4": {DeviceID: "D", Device: "d"},
	}
	if !maps.Equal(cfg.Bindings, want) || cfg.Name != "STAGE" || cfg.FPS != 30 || cfg.MaxSlots != 6 {
		t.Fatalf("not merged with config.json.bak: %+v", cfg)
	}
}

func TestSettingChangeResumesCrashedSlots(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	configBroken = false
	before := slots
	t.Cleanup(func() { slots = before })
	crashed := &Slot{N: 1, active: true, crashStopped: true, restartCh: make(chan struct{}, 1)}
	other := &Slot{N: 2, active: true, restartCh: make(chan struct{}, 1)}
	slots = []*Slot{crashed, other}
	crashWarned = true
	setFPS(60) // same value: nothing to resume
	if len(crashed.restartCh) != 0 || !crashWarned {
		t.Fatalf("an unchanged setting must not wake anything")
	}
	setResolution("fhd")
	if len(crashed.restartCh) != 1 || len(other.restartCh) != 0 || crashWarned {
		t.Fatalf("crashed slot not woken, or a running one was touched")
	}
}

func TestFitClient(t *testing.T) {
	cases := []struct {
		name                     string
		vw, vh, maxW, maxH, w, h int
	}{
		{"portrait taller than the screen", 1320, 2868, 1712, 933, 429, 933},
		{"landscape wider than the screen", 2868, 1320, 1712, 933, 1712, 788},
		{"small video: its own size", 640, 360, 1712, 933, 640, 360},
		{"exact fit", 1712, 933, 1712, 933, 1712, 933},
		{"square on a wide area", 2000, 2000, 1712, 933, 933, 933},
		{"no room", 1320, 2868, 0, -5, 1, 1},
	}
	for _, c := range cases {
		if w, h := fitClient(c.vw, c.vh, c.maxW, c.maxH); w != c.w || h != c.h {
			t.Errorf("%s: got %d×%d, want %d×%d", c.name, w, h, c.w, c.h)
		}
	}
}

func TestFitWindow(t *testing.T) {
	const fw, fh = 16, 39 // WS_OVERLAPPEDWINDOW borders and title bar at 100%
	fhd := winRect{0, 0, 1920, 1080}
	cases := []struct {
		name      string
		vw, vh    int
		work, cur winRect
		want      winRect
	}{
		// 90% of 1080 = 972 for the window: client 933 high, width from 1320/2868.
		{"portrait on 1920x1080 keeps its corner", 1320, 2868, fhd, winRect{100, 50, 740, 449}, winRect{100, 50, 100 + 429 + fw, 50 + 933 + fh}},
		{"landscape on 1920x1080", 2868, 1320, fhd, winRect{100, 50, 740, 449}, winRect{100, 50, 100 + 1712 + fw, 50 + 788 + fh}},
		{"small video: its own size", 640, 360, fhd, winRect{10, 10, 20, 20}, winRect{10, 10, 10 + 640 + fw, 10 + 360 + fh}},
		{"would leave the screen: moved inside", 1320, 2868, fhd, winRect{1700, 600, 2000, 900}, winRect{1920 - 445, 1080 - 972, 1920, 1080}},
		{"above and left of the work area: moved in", 640, 360, winRect{0, 40, 1920, 1080}, winRect{-50, 0, 600, 300}, winRect{0, 40, 656, 439}},
		{"second monitor to the right stays there", 2868, 1320, winRect{1920, 0, 4480, 1400}, winRect{2000, 100, 2600, 500},
			winRect{2000, 100, 2000 + 2288 + fw, 100 + 1053 + fh}},
		{"monitor at negative coordinates", 1320, 2868, winRect{-1080, -400, 0, 1520}, winRect{-1000, -300, -500, 0},
			winRect{-1000, -300, -1000 + 777 + fw, -300 + 1689 + fh}},
	}
	for _, c := range cases {
		got := fitWindow(c.vw, c.vh, fw, fh, c.work, c.cur)
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			continue
		}
		cw, ch := got.w()-fw, got.h()-fh
		if d := float64(cw)/float64(ch) - float64(c.vw)/float64(c.vh); d > 0.005 || d < -0.005 || cw > c.vw || ch > c.vh {
			t.Errorf("%s: client %d×%d off the video's %d×%d", c.name, cw, ch, c.vw, c.vh)
		}
		if got.Left < c.work.Left || got.Top < c.work.Top || got.Right > c.work.Right || got.Bottom > c.work.Bottom ||
			got.w() > c.work.w()*9/10 || got.h() > c.work.h()*9/10 {
			t.Errorf("%s: %+v outside 90%% of the work area %+v", c.name, got, c.work)
		}
	}
}

// fitVideoWindow on a real window owned by another thread (like UxPlay's): the client area takes
// the video's aspect ratio, for portrait and then landscape.
func TestFitVideoWindowReal(t *testing.T) {
	var (
		registerClass   = user32.NewProc("RegisterClassExW")
		createWindow    = user32.NewProc("CreateWindowExW")
		showWindow      = user32.NewProc("ShowWindow")
		defWindowProc   = user32.NewProc("DefWindowProcW")
		getMessage      = user32.NewProc("GetMessageW")
		dispatchMessage = user32.NewProc("DispatchMessageW")
		postMessage     = user32.NewProc("PostMessageW")
		postQuit        = user32.NewProc("PostQuitMessage")
		getClientRect   = user32.NewProc("GetClientRect")
	)
	hc := make(chan windows.HWND)
	go func() {
		runtime.LockOSThread()
		class := ptr("FitTest")
		proc := windows.NewCallback(func(h windows.HWND, m uint32, w, l uintptr) uintptr {
			if m == 0x0002 { // WM_DESTROY
				postQuit.Call(0)
			}
			r, _, _ := defWindowProc.Call(uintptr(h), uintptr(m), w, l)
			return r
		})
		wc := struct {
			Size, Style                uint32
			WndProc                    uintptr
			ClsExtra, WndExtra         int32
			Instance, Icon, Cursor, Bg windows.Handle
			MenuName, ClassName        *uint16
			IconSm                     windows.Handle
		}{WndProc: proc, ClassName: class}
		wc.Size = uint32(unsafe.Sizeof(wc))
		registerClass.Call(uintptr(unsafe.Pointer(&wc)))
		h, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(ptr("Slot test"))),
			0x00CF0000, 100, 100, 640, 360, 0, 0, 0, 0)
		if h != 0 {
			showWindow.Call(h, 4) // SW_SHOWNOACTIVATE
		}
		hc <- windows.HWND(h)
		var m [48]byte
		for {
			if r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				return
			}
			dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	h := <-hc
	if h == 0 {
		t.Skip("no window station")
	}
	defer postMessage.Call(uintptr(h), 0x0010, 0, 0) // WM_CLOSE
	for _, v := range [][2]int{{1320, 2868}, {2868, 1320}} {
		res, retry := fitVideoWindow(h, v[0], v[1])
		if retry || res == "" {
			t.Fatalf("%v: nothing done (%q, retry %v)", v, res, retry)
		}
		var c winRect
		ok := false
		for i := 0; i < 40 && !ok; i++ { // SWP_ASYNCWINDOWPOS: applied by the window's thread
			time.Sleep(50 * time.Millisecond)
			getClientRect.Call(uintptr(h), uintptr(unsafe.Pointer(&c)))
			d := float64(c.w())/float64(max(c.h(), 1)) - float64(v[0])/float64(v[1])
			ok = d < 0.01 && d > -0.01
		}
		if !ok {
			t.Errorf("%v: client %d×%d (%s)", v, c.w(), c.h(), res)
		}
		t.Logf("%v: %s, client %d×%d", v, res, c.w(), c.h())
		if res2, _ := fitVideoWindow(h, v[0], v[1]); res2 != "" {
			t.Errorf("%v: fitted again with nothing to change: %s", v, res2)
		}
	}
}

// A new geometry is not fitted until it is a second old: the video sink may still be resizing its
// window to the new caps, and fitting before it would be overwritten.
func TestFitWindowsWaitsForGeometryToSettle(t *testing.T) {
	now := time.Now()
	s := &Slot{N: 1}
	if msg, _ := s.processLine("*** codec is h264: geometry is (w,h)=(2868,1320) (source); (w,h)=(2868,1320) (displayed)", now); msg == "" {
		t.Fatal("geometry not read")
	}
	h := windows.HWND(0x1234) // not a real window: fitting it would be a no-op anyway
	s.fitted = map[windows.HWND][2]int{h: {1320, 2868}}
	fitWindows(s, []windows.HWND{h}, now.Add(500*time.Millisecond))
	if got := s.fitted[h]; got != [2]int{1320, 2868} {
		t.Errorf("fitted %v while the geometry was settling", got)
	}
	fitWindows(s, []windows.HWND{h}, now.Add(1500*time.Millisecond))
	if got := s.fitted[h]; got == [2]int{1320, 2868} {
		t.Errorf("not fitted to the new geometry once settled: %v", got)
	}
}
