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

// Real H.265 geometry line from an iPhone 17 Pro Max with v0.2.0 (UxPlay 1.74).
const realGeometryH265 = "*** codec is h265: geometry is (w,h)=(1768,3840) (source); (x,y)=(1036,0); (w,h)=(1768,3840)"

func withConfig(t *testing.T, c Config) {
	t.Helper()
	before, brokenBefore, readBefore, unsavedBefore, diskBefore := cfg, configBroken, readFailed, unsaved, diskCfg
	cfg, readFailed, unsaved, diskCfg = c, false, false, defaultConfig()
	t.Cleanup(func() {
		cfg, configBroken, readFailed, unsaved, diskCfg = before, brokenBefore, readBefore, unsavedBefore, diskBefore
	})
}

func testConfig(d map[string]Device) Config {
	c := defaultConfig()
	c.Devices = d
	return c
}

// withSlots makes n instances (as onReady does, without the tray) for tests that look at the pool.
func withSlots(t *testing.T, n int) []*Slot {
	t.Helper()
	before, freeBefore := slots, currentFree
	slots = nil
	for i := 1; i <= n; i++ {
		slots = append(slots, &Slot{N: i, restartCh: make(chan struct{}, 1), state: "waiting"})
	}
	t.Cleanup(func() { slots, currentFree = before, freeBefore })
	return slots
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
		{"accepted 10 s ago, ended 6 s ago, advertised again", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-10 * time.Second), ended: t0.Add(-6 * time.Second), readvertised: true}, true},
		{"accepted 10 s ago, ended 3 s ago, advertised again", Slot{cmd: &exec.Cmd{}, connected: t0.Add(-10 * time.Second), ended: t0.Add(-3 * time.Second), readvertised: true}, false},
		{"connection request 2 s ago", Slot{cmd: &exec.Cmd{}, candID: "22:C5:83:7E:F0:3F", candAt: t0.Add(-2 * time.Second)}, false},
		{"connection request 11 s ago, never accepted", Slot{cmd: &exec.Cmd{}, candID: "22:C5:83:7E:F0:3F", candAt: t0.Add(-11 * time.Second)}, true},
	}
	for _, c := range cases {
		if got := c.s.idleAt(t0); got != c.want {
			t.Errorf("%s: idle=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestPoolPlan(t *testing.T) {
	t0 := time.Now()
	at := func(x slotView, s int) slotView { x.announced = t0.Add(time.Duration(s) * time.Second); return x }
	free := slotView{active: true, running: true, ready: true}              // up and advertised, nobody connected since it started
	booting := slotView{active: true, running: true}                        // up, not advertised yet (or restarting)
	busy := slotView{active: true, running: true, busy: true, used: true}   // a session in progress
	over := slotView{active: true, running: true, used: true, hidden: true} // session over, window frozen, still hidden
	down := slotView{}
	going := slotView{running: true}                               // stopping: its ports are still taken
	crashed := slotView{active: true, running: true, broken: true} // stopped by a crash loop
	crashedOut := slotView{broken: true}                           // crash-stopped earlier, now down
	cases := []struct {
		name    string
		max     int
		v       []slotView
		current int
		want    []int
		free    int
		restart bool
	}{
		{"startup", 8, []slotView{down, down, down}, 0, []int{1}, 1, false},
		{"one free, nobody connected", 8, []slotView{free, down, down}, 1, []int{1}, 1, false},
		{"device accepted on the free one: the next comes up", 8, []slotView{busy, down, down}, 1, []int{1, 2}, 2, false},
		{"two devices live, one free", 8, []slotView{busy, busy, free}, 3, []int{1, 2, 3}, 3, false},
		{"extra idle ones go down: the one announced last stays", 8, []slotView{at(free, 1), busy, at(free, 5)}, 1, []int{2, 3}, 3, false},
		// The ended instance announced itself again (the patch): the devices resolve LIVE to it,
		// so it is the free one, without a restart, and the previous free one goes down.
		{"session over, advertised again: it is the free one", 8, []slotView{at(free, 9), at(free, 5), busy}, 2, []int{1, 3}, 1, false},
		{"session over, advertised again, still in its grace: the free one stays", 8, []slotView{at(slotView{active: true, running: true, busy: true, ready: true}, 9), at(free, 5)}, 2, []int{1, 2}, 2, false},
		// Announcing again failed: the ended instance stays hidden, even though its end came
		// after the free one started: the free one stays and the ended one goes down.
		{"session over after the free one started: it goes down", 8, []slotView{at(over, 9), at(free, 5)}, 2, []int{2}, 2, false},
		{"session over, the free one still booting: the free one stays", 8, []slotView{at(over, 9), booting}, 2, []int{2}, 2, false},
		{"restarting after the session: the other free one stays", 8, []slotView{at(booting, 9), at(free, 5)}, 1, []int{1, 2}, 1, false},
		{"restarted and advertised: the other free one goes down", 8, []slotView{at(free, 12), at(free, 5)}, 1, []int{1}, 1, false},
		{"a new free one not advertised yet keeps nothing else", 8, []slotView{busy, booting, down}, 2, []int{1, 2}, 2, false},
		// A device reconnected on the free one: its old instance restarts and is the next free one.
		{"reconnect: the restarting old instance is the free one", 8, []slotView{at(booting, 1), at(busy, 5), down}, 2, []int{1, 2}, 1, false},
		{"a crash-stopped free one is replaced", 8, []slotView{busy, crashed, down}, 2, []int{1, 3}, 3, false},
		{"a crash-stopped number is skipped", 8, []slotView{busy, crashedOut, down}, 0, []int{1, 3}, 3, false},
		{"session over, the free one announced last: it stays", 8, []slotView{at(over, 1), at(free, 5)}, 2, []int{2}, 2, false},
		{"session over and nothing else is free: it restarts", 2, []slotView{over, busy}, 0, []int{1, 2}, 1, true},
		{"full: no free one", 2, []slotView{busy, busy, down}, 0, []int{1, 2}, 0, false},
		{"maxSlots 1 with someone live", 1, []slotView{busy, down}, 1, []int{1}, 0, false},
		{"maxSlots 1, session over: restarts for the next", 1, []slotView{over, down}, 1, []int{1}, 1, true},
		{"a stopping instance's number is skipped", 8, []slotView{busy, going, down}, 0, []int{1, 3}, 3, false},
		{"tie: the current free one stays", 8, []slotView{free, busy, free}, 3, []int{2, 3}, 3, false},
	}
	for _, c := range cases {
		want, free, restart := poolPlan(c.max, c.v, c.current)
		var ns []int
		for n := 1; n <= len(c.v); n++ {
			if want[n] {
				ns = append(ns, n)
			}
		}
		if !slices.Equal(ns, c.want) || free != c.free || restart != c.restart {
			t.Errorf("%s: %v free %d restart %v, want %v free %d restart %v", c.name, ns, free, restart, c.want, c.free, c.restart)
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
	c := testConfig(map[string]Device{"X": {Name: "x"}})
	c.ExtraArgs = []string{"-reset", "30"}
	withConfig(t, c)

	a := slotArgs(&Slot{N: 2})
	if !hasSeq(a, "-n", "LIVE") || !hasSeq(a, "-p", "35020") || !hasSeq(a, "-vs", "d3d11videosink") ||
		!hasSeq(a, "-h265", "-s", "2880x2880", "-fps", "60", "-FPSdata") {
		t.Errorf("without name/port/resolution/sink/-FPSdata: %v", a)
	}
	if !hasSeq(a[len(a)-2:], "-reset", "30") {
		t.Errorf("extraArgs must come last: %v", a)
	}
	for _, f := range []string{"-restrict", "-allow", "-block", "-nohold"} {
		if slices.Contains(a, f) {
			t.Errorf("hidden busy instances can't be taken over: no %s, got %v", f, a)
		}
	}
	b := slotArgs(&Slot{N: 1})
	if !hasSeq(b, "-n", "LIVE") {
		t.Errorf("every instance has the same name: %v", b)
	}
	if !slices.Equal(b, slotArgs(&Slot{N: 1})) {
		t.Errorf("slotArgs must be deterministic (restart compares it)")
	}
	if !slices.Contains(b, slotMAC(1)) || slotMAC(1) == slotMAC(2) {
		t.Errorf("each instance with its own -m: %v", b)
	}
}

func TestResolutionAndFPSArgs(t *testing.T) {
	cases := []struct {
		res  string
		fps  int
		want []string
	}{
		{"native", 60, []string{"-h265", "-s", "2880x2880", "-fps", "60"}},
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
	if c, w, h, ok := parseGeometry(realGeometry); !ok || c != "H.264" || w != 498 || h != 1080 {
		t.Fatalf("real line: %s %d %d %v", c, w, h, ok)
	}
	if c, w, h, ok := parseGeometry(realGeometryH265); !ok || c != "H.265" || w != 1768 || h != 3840 {
		t.Fatalf("real h265 line: %s %d %d %v", c, w, h, ok)
	}
	if c, w, h, ok := parseGeometry("*** codec is h265: geometry is (w,h)=(2868,1320) (source); (w,h)=(1920,884) (displayed)"); !ok || c != "H.265" || w != 2868 || h != 1320 {
		t.Fatalf("h265 landscape: %s %d %d %v", c, w, h, ok)
	}
	if _, _, _, ok := parseGeometry("Begin streaming to GStreamer video pipeline"); ok {
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

// connect feeds the real log lines of a device connecting and starting to mirror.
func connect(s *Slot, request string, t0 time.Time) (accepted bool) {
	s.processLine("Accepted IPv4 client on socket 10252, port 35011", t0)
	s.processLine(request, t0)
	_, accepted, _ = s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t0)
	s.processLine("raop_rtp_mirror starting mirroring", t0)
	return accepted
}

func TestTrayLineAndRotation(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 2)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	if got := slotLine(s); got != "LIVE · waiting" {
		t.Fatalf("free line: %q", got)
	}
	t0 := time.Now()
	connect(s, realLine, t0)
	s.processLine(realGeometry, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if got := slotLine(s); got != "Otavio Augusto’s iPhone · live · H.264 · portrait 498×1080" {
		t.Fatalf("line: %q", got)
	}
	msg, _, _ := s.processLine("*** codec is h264: geometry is (w,h)=(1080,498) (source); (w,h)=(1080,498) (displayed)", t0)
	if got := slotLine(s); got != "Otavio Augusto’s iPhone · live · H.264 · landscape 1080×498" || !strings.Contains(msg, "landscape") {
		t.Fatalf("rotation: %q %q", got, msg)
	}
	s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if got := slotLine(s); strings.Contains(got, "landscape") || !strings.HasPrefix(got, "Otavio Augusto’s iPhone · stopped") {
		t.Fatalf("after mirroring stopped: %q", got)
	}
}

// The codec comes from the geometry line: shown in the menu while live and logged with the size.
func TestCodecInTrayLineAndLog(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	t0 := time.Now()
	connect(s, realLine, t0)
	msg, _, _ := s.processLine(realGeometryH265, t0)
	if msg != "slot 1: video H.265 portrait 1768×3840" {
		t.Fatalf("log: %q", msg)
	}
	if got := slotLine(s); strings.Contains(got, "H.265") {
		t.Fatalf("codec shown before the video started: %q", got)
	}
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if got := slotLine(s); got != "Otavio Augusto’s iPhone · live · H.265 · portrait 1768×3840" {
		t.Fatalf("line: %q", got)
	}
	if msg, _, _ := s.processLine(realGeometryH265, t0); msg != "" {
		t.Errorf("same geometry logged again: %q", msg)
	}
	s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if got := slotLine(s); strings.Contains(got, "H.265") {
		t.Fatalf("codec shown after mirroring stopped: %q", got)
	}
	connect(s, realLine, t0)
	if s.codec != "" || s.width != 0 {
		t.Errorf("new session kept the old codec/size: %q %d", s.codec, s.width)
	}
}

// Real log sequence: request, accept, video, drop. The device is remembered with the name it sent.
func TestProcessLineSessionAndDrop(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	t0 := time.Now()
	for _, l := range []string{"Accepted IPv4 client on socket 10252, port 35011", "Local : 10.0.0.102", realLine} {
		if _, a, keep := s.processLine(l, t0); a || !keep {
			t.Fatalf("accepted before the accept, or line left out of the log: %q", l)
		}
	}
	if !s.connected.IsZero() || s.used() {
		t.Fatalf("a request not yet accepted can't count as a connection")
	}
	msg, a, _ := s.processLine("Client identified as User-Agent: AirPlay/960.13.1", t0)
	d := cfg.Devices["22:C5:83:7E:F0:3F"]
	if !a || d.Name != "Otavio Augusto’s iPhone" || d.Window != "Otavio Augusto’s iPhone" || !s.used() ||
		!strings.Contains(msg, `window "Otavio Augusto’s iPhone"`) || !unsaved {
		t.Fatalf("not accepted/remembered: %q %+v", msg, cfg.Devices)
	}
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if !s.mirroring || s.state != "live" {
		t.Fatalf("not live: %+v", s)
	}
	s.processLine("Connection closed on socket 999", t0) // another connection, e.g. a device that only looked
	if !s.mirroring {
		t.Fatalf("another connection's socket dropped the state")
	}
	s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if s.mirroring || s.ended.IsZero() {
		t.Fatalf("did not see the drop")
	}
}

func TestProcessLineRefused(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.state = "waiting"
	s.processLine(realLine, time.Now())
	msg, a, _ := s.processLine("rejecting new connection request from 10.0.0.7", time.Now())
	if a || !strings.Contains(msg, "refused") || !s.connected.IsZero() || s.device != "" || s.state != "waiting" {
		t.Fatalf("refusal counted as a connection: %q %+v", msg, s)
	}
	s.processLine("Client identified as User-Agent: AirPlay/960.13.1", time.Now())
	if len(cfg.Devices) != 0 || !s.connected.IsZero() {
		t.Fatalf("an accept after a refusal can't apply to the refused device")
	}
}

// The device reconnects on the free instance (its old one is hidden while it still thinks the
// device mirrors): the old instance restarts at once, so no frozen window keeps the device's title.
func TestReconnectRestartsOldInstance(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 3)
	old, next, other := slots[0], slots[1], slots[2]
	for _, s := range slots {
		s.cmd, s.active = &exec.Cmd{}, true
	}
	t0 := time.Now()
	connect(old, realLine, t0)
	old.processLine("Begin streaming to GStreamer video pipeline", t0)
	connect(other, "connection request from Ana’s iPad (iPad16,3) with deviceID = AA:AA:AA:AA:AA:AA", t0)
	other.processLine("Begin streaming to GStreamer video pipeline", t0)
	title := old.title
	t1 := t0.Add(time.Minute)
	if !connect(next, realLine, t1) {
		t.Fatal("not accepted")
	}
	if len(old.restartCh) != 1 || len(other.restartCh) != 0 {
		t.Fatalf("old instance not restarted (%d) or another device's touched (%d)", len(old.restartCh), len(other.restartCh))
	}
	if next.title != title || old.title != "" {
		t.Fatalf("the title must follow the device: new %q old %q", next.title, old.title)
	}
	// The old instance is going down at once: the pool must take it as the next free one, not
	// start another instance while its process is still exiting.
	if old.busyAt(t1) || !next.busyAt(t1) || !other.busyAt(t1) {
		t.Fatalf("old instance still busy (%v), or a live one not busy", old.busyAt(t1))
	}
}

func TestPickTitle(t *testing.T) {
	devs := map[string]Device{
		"A": {Name: "iPhone", Window: "iPhone"},
		"B": {Name: "iPhone", Window: "iPhone 2"},
		"C": {Name: "Ana", Label: "Player 1", Window: "Ana"},
	}
	cases := []struct {
		name, id, base string
		open           map[string]string
		want           string
	}{
		{"remembered title kept", "B", "iPhone", nil, "iPhone 2"},
		{"remembered title kept after an app restart, other one offline", "A", "iPhone", nil, "iPhone"},
		{"new device with the same name: reserved titles skipped", "N", "iPhone", nil, "iPhone 3"},
		{"label changed: new title from the label", "C", "Player 1", nil, "Player 1"},
		{"taken by an open session of another device", "N", "Bob", map[string]string{"Bob": "X"}, "Bob 2"},
		{"its own open session doesn't count", "A", "iPhone", map[string]string{"iPhone": "A"}, "iPhone"},
		{"name ending in a number is not a suffix", "N", "iPhone 1", nil, "iPhone 1"},
	}
	for _, c := range cases {
		if got := pickTitle(c.id, c.base, devs, c.open); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if !hasBase("iPhone 2", "iPhone") || hasBase("iPhone 02", "iPhone") || hasBase("iPhone X", "iPhone") || hasBase("iPhone 1", "iPhone") {
		t.Errorf("hasBase")
	}
}

// Same deviceID across sessions and app restarts: the same window title (the OBS source follows the
// person). The label replaces the name the device sends.
func TestWindowTitleStableAndLabel(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 2)
	for _, s := range slots {
		s.cmd = &exec.Cmd{}
	}
	t0 := time.Now()
	connect(slots[0], "connection request from iPhone (iPhone18,2) with deviceID = 11:11:11:11:11:11", t0)
	connect(slots[1], "connection request from iPhone (iPhone17,1) with deviceID = 22:22:22:22:22:22", t0)
	if slots[0].title != "iPhone" || slots[1].title != "iPhone 2" {
		t.Fatalf("same names: %q %q", slots[0].title, slots[1].title)
	}
	// App restart: only the second device comes back, on instance 1. It keeps "iPhone 2".
	b, _ := json.Marshal(cfg)
	c, _, err := parseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	withConfig(t, c)
	withSlots(t, 1)
	slots[0].cmd = &exec.Cmd{}
	connect(slots[0], "connection request from iPhone (iPhone17,1) with deviceID = 22:22:22:22:22:22", t0)
	if slots[0].title != "iPhone 2" {
		t.Fatalf("title changed across a restart: %q", slots[0].title)
	}
	cfg.Devices["22:22:22:22:22:22"] = Device{Name: "iPhone", Label: "Player 2", Window: "iPhone 2"}
	connect(slots[0], "connection request from iPhone (iPhone17,1) with deviceID = 22:22:22:22:22:22", t0)
	if slots[0].title != "Player 2" || cfg.Devices["22:22:22:22:22:22"].Window != "Player 2" {
		t.Fatalf("label not used: %q %+v", slots[0].title, cfg.Devices)
	}
}

// A client report as UxPlay prints it with -FPSdata: libplist's plist_to_xml of the binary plist
// in the video stream's type 5 packets, one log call, so one line per XML line. The stage names
// are the ones the client lists in fpsInfo when it sets up the stream (SubS ... Sent).
const fpsReport = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>fpsInfo</key>
	<array>
		<dict>
			<key>name</key>
			<string>SubS</string>
			<key>value</key>
			<real>59.940000000000005</real>
		</dict>
		<dict>
			<key>name</key>
			<string>B4En</string>
			<key>value</key>
			<real>59.880000000000003</real>
		</dict>
		<dict>
			<key>name</key>
			<string>EnDp</string>
			<key>value</key>
			<real>0</real>
		</dict>
		<dict>
			<key>name</key>
			<string>QueF</string>
			<key>value</key>
			<integer>2</integer>
		</dict>
		<dict>
			<key>name</key>
			<string>Sent</string>
			<key>value</key>
			<real>58.719999999999999</real>
		</dict>
	</array>
	<key>latencyMs</key>
	<integer>90</integer>
	<key>timestampInfo</key>
	<array>
		<dict>
			<key>name</key>
			<string>SubSu</string>
			<key>value</key>
			<real>0.0011</real>
		</dict>
	</array>
	<key>maxFPS</key>
	<integer>60</integer>
</dict>
</plist>
`

func TestParseFPSReport(t *testing.T) {
	if v, ok := parseFPSReport(fpsReport); !ok || v < 58.7 || v > 58.73 {
		t.Fatalf("report: %v %v", v, ok)
	}
	flat := "<plist version=\"1.0\">\n<dict>\n\t<key>maxFPS</key>\n\t<integer>60</integer>\n\t<key>fpsInfo</key>\n\t<dict>\n" +
		"\t\t<key>SubS</key>\n\t\t<real>30.0</real>\n\t\t<key>Sent</key>\n\t\t<real>29.5</real>\n\t</dict>\n</dict>\n</plist>"
	if v, ok := parseFPSReport(flat); !ok || v != 29.5 {
		t.Errorf("stages as keys: %v %v", v, ok)
	}
	if v, ok := parseFPSReport("<plist><dict><key>FPS</key><real>24</real><key>maxFPS</key><integer>60</integer></dict></plist>"); !ok || v != 24 {
		t.Errorf("plain fps key: %v %v", v, ok)
	}
	if _, ok := parseFPSReport("<plist><dict><key>maxFPS</key><integer>60</integer><key>Sent</key><integer>123456</integer></dict></plist>"); ok {
		t.Errorf("maxFPS (the receiver's request) and a bare Sent (not in fpsInfo) are not the frame rate")
	}
	// A line of another thread printed in the middle of the report doesn't break it.
	cut := strings.Replace(fpsReport, "\t\t<dict>\n\t\t\t<key>name</key>\n\t\t\t<string>QueF", "raop_ntp receive timeout\n\t\t<dict>\n\t\t\t<key>name</key>\n\t\t\t<string>QueF", 1)
	if v, ok := parseFPSReport(cut); !ok || v < 58.7 || v > 58.73 {
		t.Errorf("interleaved line: %v %v", v, ok)
	}
	if _, ok := parseFPSReport("<plist><dict><key>latencyMs</key><integer>90</integer></dict></plist>"); ok {
		t.Errorf("no frame rate in the report")
	}
}

// The first FPS report of a session goes to the log (for diagnosis), the ones after it don't; the
// tray shows the frame rate.
func TestFPSReportsLeftOutOfLog(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	t0 := time.Now()
	connect(s, realLine, t0)
	s.processLine(realGeometryH265, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	feed := func() (kept int, msgs []string) {
		for _, l := range strings.Split(fpsReport, "\n") { // ends with the empty line after </plist>
			msg, _, keep := s.processLine(l, t0)
			if keep {
				kept++
			}
			if msg != "" {
				msgs = append(msgs, msg)
			}
		}
		return kept, msgs
	}
	lines := len(strings.Split(fpsReport, "\n"))
	if kept, msgs := feed(); kept != lines || len(msgs) != 1 || !strings.Contains(msgs[0], "client reports 59 fps") {
		t.Fatalf("first report: %d of %d lines kept, %q", kept, lines, msgs)
	}
	if got := slotLine(s); got != "Otavio Augusto’s iPhone · live · H.265 · portrait 1768×3840 · 59 fps" {
		t.Fatalf("line: %q", got)
	}
	if kept, msgs := feed(); kept != 0 || len(msgs) != 0 {
		t.Fatalf("second report: %d lines kept, %q", kept, msgs)
	}
	if _, _, keep := s.processLine("raop_rtp_mirror->running is no longer true", t0); !keep {
		t.Fatalf("a normal line after a report left out")
	}
	connect(s, realLine, t0) // a new session logs its first report again
	if kept, _ := feed(); kept != lines {
		t.Fatalf("new session's first report: %d lines kept", kept)
	}
}

func TestMDNSPatchLines(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd, s.args = &exec.Cmd{}, slotArgs(s)
	t0 := time.Now()
	if msg, _, _ := s.processLine("mDNS: advertised at startup", t0); !s.ready || !s.announced.Equal(t0) || !strings.Contains(msg, "in Screen Mirroring") {
		t.Fatalf("startup: %q ready %v announced %v", msg, s.ready, s.announced)
	}
	if msg, _, _ := s.processLine("mDNS: announced again", t0.Add(time.Second)); msg != "" || !s.announced.Equal(t0.Add(time.Second)) {
		t.Fatalf("repeat: %q announced %v", msg, s.announced)
	}
	connect(s, realLine, time.Now())
	if msg, _, _ := s.processLine("mDNS: hidden while mirroring", time.Now()); !s.hidden || !strings.Contains(msg, "out of Screen Mirroring") {
		t.Fatalf("hidden: %q", msg)
	}
	s.processLine("Begin streaming to GStreamer video pipeline", time.Now())
	// Only the patch line says the mirror thread ended (e.g. "tcp socket was closed by client"):
	// the session is over and the instance is in Screen Mirroring again, free as it is.
	t1 := t0.Add(time.Minute)
	msg, _, _ := s.processLine("mDNS: mirroring ended, advertised again", t1)
	if s.mirroring || !s.ended.Equal(t1) || s.hidden || !s.ready || !s.announced.Equal(t1) ||
		msg != "slot 1: Otavio Augusto’s iPhone stopped mirroring; back in Screen Mirroring" {
		t.Fatalf("ended: %q mirroring %v ended %v hidden %v ready %v", msg, s.mirroring, s.ended, s.hidden, s.ready)
	}
	if s.used() || s.deviceID == "" || s.title != "Otavio Augusto’s iPhone" || slotLine(s) != "LIVE · waiting" {
		t.Fatalf("after the session: used %v device %q title %q line %q", s.used(), s.deviceID, s.title, slotLine(s))
	}
	// The next device on it is a new session.
	connect(s, realLine, t1.Add(time.Second))
	if !s.used() || s.readvertised || !strings.Contains(slotLine(s), "Otavio") {
		t.Fatalf("next session: used %v line %q", s.used(), slotLine(s))
	}
	s.processLine("mDNS: hidden while mirroring", t1)
	s.processLine("Begin streaming to GStreamer video pipeline", t1)
	// If announcing again fails the instance stays hidden and holds the session: it restarts
	// before serving another device.
	t2 := t1.Add(time.Minute)
	msg, _, _ = s.processLine("mDNS: mirroring ended, advertising again failed: still hidden", t2)
	if s.mirroring || !s.ended.Equal(t2) || !s.hidden || !s.announced.Equal(t1) || !s.used() || !strings.Contains(msg, "stopped mirroring") || strings.Contains(msg, "back in") {
		t.Fatalf("ended, still hidden: %q mirroring %v hidden %v used %v", msg, s.mirroring, s.hidden, s.used())
	}
	if s.state != "stopped: mirror again on the device" {
		t.Fatalf("state %q", s.state)
	}
	if msg, _, _ := s.processLine("mDNS: mirroring ended, advertising again failed: still hidden", t2); msg != "" || !s.hidden {
		t.Fatalf("a second end line: %q", msg)
	}
}

// A device that reconnects on another instance while its last instance is free again (advertised
// after the session, its frozen window may still be open under the device's name): that instance
// restarts, so no window is left with the same title.
func TestReconnectAfterReadvertise(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 2)
	a, b := slots[0], slots[1]
	a.cmd, b.cmd, a.args, b.args = &exec.Cmd{}, &exec.Cmd{}, slotArgs(a), slotArgs(b)
	t0 := time.Now()
	connect(a, realLine, t0)
	a.processLine("Begin streaming to GStreamer video pipeline", t0)
	a.processLine("mDNS: mirroring ended, advertised again", t0.Add(time.Minute))
	connect(b, realLine, t0.Add(2*time.Minute))
	if a.title != "" || a.state != "restarting" || len(a.restartCh) != 1 || b.title != "Otavio Augusto’s iPhone" {
		t.Fatalf("old instance: title %q state %q restart %d; new title %q", a.title, a.state, len(a.restartCh), b.title)
	}
}

// New settings that wait for a live instance are applied as soon as its session ends and it is
// advertised again: it announced last, so the device that reconnects right away gets it, and must
// get the new settings, not the old ones (nor a restart in the middle of its handshake).
func TestReadvertiseAppliesPendingConfig(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 2)
	a, b := slots[0], slots[1]
	a.cmd, a.args, b.cmd, b.args = &exec.Cmd{}, slotArgs(a), &exec.Cmd{}, slotArgs(b)
	t0 := time.Now()
	connect(a, realLine, t0)
	a.processLine("mDNS: hidden while mirroring", t0)
	a.processLine("Begin streaming to GStreamer video pipeline", t0)
	cfg.Resolution = "sd"
	msg, _, _ := a.processLine("mDNS: mirroring ended, advertised again", t0.Add(time.Minute))
	if len(a.restartCh) != 1 || !strings.Contains(msg, "restarting to apply the config") {
		t.Fatalf("new resolution: restart %d, %q", len(a.restartCh), msg)
	}
	// Same settings: free as it is, no restart. A pending restart (network change) also restarts.
	connect(b, realLine, t0)
	b.processLine("Begin streaming to GStreamer video pipeline", t0)
	b.args = slotArgs(b)
	if msg, _, _ := b.processLine("mDNS: mirroring ended, advertised again", t0.Add(time.Minute)); len(b.restartCh) != 0 || strings.Contains(msg, "restarting") {
		t.Fatalf("same settings: restart %d, %q", len(b.restartCh), msg)
	}
	connect(b, realLine, t0.Add(2*time.Minute))
	b.processLine("Begin streaming to GStreamer video pipeline", t0.Add(2*time.Minute))
	b.pending = true
	b.processLine("mDNS: mirroring ended, advertised again", t0.Add(3*time.Minute))
	if len(b.restartCh) != 1 || b.pending {
		t.Fatalf("pending: restart %d pending %v", len(b.restartCh), b.pending)
	}
}

// After a Resolution/FPS change a live session shows what it gets on reconnect; Apply now is only
// offered then.
func TestPendingNote(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd, s.args, s.argsRes, s.argsFPS = &exec.Cmd{}, slotArgs(s), cfg.Resolution, cfg.FPS
	t0 := time.Now()
	connect(s, realLine, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if p := pendingNote(s); p != "" {
		t.Fatalf("nothing changed: %q", p)
	}
	cfg.Resolution = "sd"
	if p := pendingNote(s); p != "SD on reconnect" || !strings.HasSuffix(slotLine(s), " · SD on reconnect") {
		t.Fatalf("resolution: %q / %q", p, slotLine(s))
	}
	cfg.FPS = 30
	if p := pendingNote(s); p != "SD 30 fps on reconnect" {
		t.Fatalf("both: %q", p)
	}
	cfg.Resolution, cfg.FPS = "native", 60
	if p := pendingNote(s); p != "" {
		t.Fatalf("changed back: %q", p)
	}
	cfg.ExtraArgs = []string{"-reset", "30"}
	if p := pendingNote(s); p != "new settings on reconnect" {
		t.Fatalf("extra args: %q", p)
	}
	cfg.ExtraArgs = []string{}
	cfg.FPS = 30
	applyNow(s)
	if len(s.restartCh) != 1 {
		t.Fatalf("Apply now must restart the instance")
	}
}

// Only the GStreamer video window is fitted, renamed and hidden: not the close button's tooltip
// (its window got the video's size and title in v0.2.0), tool windows or IME windows.
func TestVideoWindowFilter(t *testing.T) {
	cases := []struct {
		class string
		ex    uintptr
		want  bool
	}{
		{"GSTD3D11", 0x100, true},  // WS_EX_WINDOWEDGE
		{"GstD3D12Hwnd", 0, false}, // d3d12videosink: only the d3d11videosink the app runs with
		{"GSTD3D12", 0, false},
		{"tooltips_class32", 0x88, false}, // WS_EX_TOOLWINDOW | WS_EX_TOPMOST
		{"GSTD3D11", 0x80, false},         // a tool window, even of the sink's class
		{"GSTD3D11", 0x08000000, false},   // WS_EX_NOACTIVATE
		{"IME", 0, false},
		{"MSCTFIME UI", 0, false},
		{"gstd3d11", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		if got := videoClass(c.class, c.ex); got != c.want {
			t.Errorf("%q ex %#x: %v, want %v", c.class, c.ex, got, c.want)
		}
	}
}

// Real windows: a tooltip window of the same process is not a video window, the sink's is.
func TestVideoWindowFilterReal(t *testing.T) {
	var (
		registerClass = user32.NewProc("RegisterClassExW")
		createWindow  = user32.NewProc("CreateWindowExW")
		destroyWindow = user32.NewProc("DestroyWindow")
		defWindowProc = user32.NewProc("DefWindowProcW")
	)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	proc := windows.NewCallback(func(h windows.HWND, m uint32, w, l uintptr) uintptr {
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
	}{WndProc: proc, ClassName: ptr("GSTD3D11")}
	wc.Size = uint32(unsafe.Sizeof(wc))
	registerClass.Call(uintptr(unsafe.Pointer(&wc)))
	video, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(ptr("GSTD3D11"))), uintptr(unsafe.Pointer(ptr("video"))),
		0x00CF0000, 0, 0, 100, 100, 0, 0, 0, 0)
	tip, _, _ := createWindow.Call(0x88, uintptr(unsafe.Pointer(ptr("tooltips_class32"))), 0,
		0x80000000|0x02, 0, 0, 10, 10, video, 0, 0, 0) // WS_POPUP | TTS_NOPREFIX, owned by the video window
	if video == 0 || tip == 0 {
		t.Skip("no window station")
	}
	defer destroyWindow.Call(video)
	defer destroyWindow.Call(tip)
	if !isVideoWindow(windows.HWND(video)) || isVideoWindow(windows.HWND(tip)) {
		t.Fatalf("video %v (%s), tooltip %v (%s)", isVideoWindow(windows.HWND(video)), windowClass(windows.HWND(video)),
			isVideoWindow(windows.HWND(tip)), windowClass(windows.HWND(tip)))
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
		c.ExtraArgs == nil || len(notes) != 3 {
		t.Fatalf("v1 migration: %+v %v %v", c, notes, err)
	}
	if c.Devices["22:C5:83:7E:F0:3F"] != (Device{Name: "Otavio Augusto’s iPhone"}) {
		t.Fatalf("device lost: %+v", c.Devices)
	}
	// Config written by the Portuguese overhaul (maxSlots, HD, extra args).
	v2 := `{"nome":"PALCO","maxSlots":4,"qualidade":"HD","uxplay":"C:\\ux\\uxplay.exe","argsExtras":["-reset","30"],
	  "vinculos":{"1":{"deviceID":"AA","iphone":"A"},"3":{"deviceID":"CC","iphone":"C"}}}`
	c, _, err = parseConfig([]byte(v2))
	if err != nil || c.Name != "PALCO" || c.MaxSlots != 4 || c.Resolution != "hd" || c.UxPlay != `C:\ux\uxplay.exe` ||
		!slices.Equal(c.ExtraArgs, []string{"-reset", "30"}) || len(c.Devices) != 2 || c.Devices["CC"].Name != "C" {
		t.Fatalf("v2 migration: %+v %v", c, err)
	}
	if c, _, _ := parseConfig([]byte(`{"slots":1}`)); c.MaxSlots != 1 {
		t.Errorf("slots 1 must become maxSlots 1, got %d", c.MaxSlots)
	}
	// The owner's v0.2.0 config: slot bindings become remembered devices.
	v020 := `{"name":"LIVE","maxSlots":8,"resolution":"native","fps":60,"uxplay":"","extraArgs":[],
	  "bindings":{"1":{"deviceID":"22:C5:83:7E:F0:3F","device":"Otavio Augusto’s iPhone"},"2":{"deviceID":"BB","device":"Ana"}}}`
	c, notes, err = parseConfig([]byte(v020))
	if err != nil || len(c.Devices) != 2 || c.Devices["22:C5:83:7E:F0:3F"].Name != "Otavio Augusto’s iPhone" ||
		c.Devices["BB"].Name != "Ana" || len(notes) != 1 || !strings.HasPrefix(notes[0], bindingsNote) || slices.Contains(notes, migratedNote) {
		t.Fatalf("v0.2.0 migration: %+v %v %v", c, notes, err)
	}
	// English keys win over leftovers of the old ones; "devices" wins over "bindings".
	c, _, _ = parseConfig([]byte(`{"name":"NEW","nome":"OLD","bindings":{},"vinculos":{"1":{"deviceID":"AA"}}}`))
	if c.Name != "NEW" || len(c.Devices) != 0 {
		t.Errorf("English keys must win: %+v", c)
	}
	c, notes, _ = parseConfig([]byte(`{"devices":{"X":{"name":"x","label":"Player"}},"bindings":{"1":{"deviceID":"AA","device":"a"}}}`))
	if len(c.Devices) != 1 || c.Devices["X"].Label != "Player" || len(notes) != 0 {
		t.Errorf("devices must win over bindings: %+v %v", c, notes)
	}
}

func TestLoadConfigRewritesLegacy(t *testing.T) {
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
	for _, k := range []string{"nome", "qualidade", "argsExtras", "vinculos", "bindings"} {
		if _, ok := onDisk[k]; ok {
			t.Errorf("old key %q still on disk: %s", k, b)
		}
	}
	if !strings.Contains(string(b), `"devices"`) || !strings.Contains(string(b), `"22:C5:83:7E:F0:3F": {`) ||
		!strings.Contains(string(b), `"name": "Otavio"`) || !strings.Contains(string(b), `"label": ""`) {
		t.Fatalf("device not written: %s", b)
	}
	// The original stays in config.json.v1 for a downgrade to a version that only reads the old keys.
	if v1, _ := os.ReadFile(configPath + ".v1"); !strings.Contains(string(v1), `"vinculos"`) {
		t.Fatalf("original not backed up: %s", v1)
	}
	// Loading the rewritten file again keeps the device.
	if err := loadConfig(); err != nil || cfg.Devices["22:C5:83:7E:F0:3F"].Name != "Otavio" || cfg.Resolution != "native" {
		t.Fatalf("second load: %+v %v", cfg, err)
	}
	// v0.2.0's bindings: kept in config.json.v2 for a downgrade.
	os.WriteFile(configPath, []byte(`{"name":"LIVE","bindings":{"1":{"deviceID":"AA","device":"a"}}}`), 0o644)
	if err := loadConfig(); err != nil || cfg.Devices["AA"].Name != "a" {
		t.Fatalf("bindings load: %+v %v", cfg, err)
	}
	if v2, _ := os.ReadFile(configPath + ".v2"); !strings.Contains(string(v2), `"bindings"`) {
		t.Fatalf("v0.2.0 original not backed up: %s", v2)
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
	withConfig(t, testConfig(map[string]Device{"A": {Name: "a"}}))
	configBroken = false
	os.Mkdir(configPath, 0o755) // reading a directory fails with an error other than "not found"
	err := loadConfig()
	var re *readError
	if !errors.As(err, &re) {
		t.Fatalf("expected a read error: %v", err)
	}
	// A read error is not a broken file: saving resumes once a read works (see TestReadErrorKeepsUserEditsAndDevices).
	if configBroken {
		t.Errorf("a read error must not mark the file as broken")
	}
	if cfg.Devices["A"].Name != "a" {
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
	withConfig(t, testConfig(map[string]Device{"C": {Name: "iPad", Label: "Host"}}))
	configBroken = false
	os.WriteFile(configPath, []byte(`{"maxSlots": 2}`), 0o644)
	saveConfig()
	if err := writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(configPath)
	var c Config
	if err != nil || json.Unmarshal(b, &c) != nil || c.Devices["C"].Label != "Host" {
		t.Fatalf("config not saved: %s %v", b, err)
	}
	if _, err := os.Stat(configPath + ".tmp"); err == nil {
		t.Errorf("temp file left behind")
	}
}

func TestBrokenConfigNotTouched(t *testing.T) {
	withConfigFile(t)
	c := testConfig(map[string]Device{"A": {Name: "a"}})
	c.Resolution = "hd"
	withConfig(t, c)

	broken := []byte(`{"devices": {"A": {"name": "a"},}}`)
	os.WriteFile(configPath, broken, 0o644)
	if err := loadConfig(); err == nil {
		t.Fatalf("expected an error")
	}
	if b, _ := os.ReadFile(configPath); string(b) != string(broken) {
		t.Errorf("the user's file was overwritten: %s", b)
	}
	if cfg.Devices["A"].Name != "a" || cfg.Resolution != "hd" {
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
	cfg.Devices["B"] = Device{Name: "b"} // a new device while the file is broken
	saveConfig()
	writeConfig()
	if b, _ := os.ReadFile(configPath); string(b) != string(broken) {
		t.Fatalf("overwrote the user's config: %s", b)
	}
	os.WriteFile(configPath, []byte(`{"maxSlots": 3}`), 0o644)
	if err := loadConfig(); err != nil || configBroken || cfg.MaxSlots != 3 || cfg.Devices["B"].Name != "b" {
		t.Fatalf("once fixed it must save again: %v %v %+v", err, configBroken, cfg)
	}
}

func TestMergeDevices(t *testing.T) {
	base := map[string]Device{"A": {Name: "a"}, "G": {Name: "g"}, "H": {Name: "h"}}
	mem := map[string]Device{
		"A": {Name: "a"},                // saved: the file decides (removed by hand stays removed)
		"C": {Name: "c", Window: "c"},   // never saved: kept
		"H": {Name: "h", Window: "h 2"}, // updated, not saved: kept
		"F": {Name: "f-mem"},            // never saved, but the file has F: the file wins
		// G forgotten, not saved: stays forgotten
	}
	file := map[string]Device{"F": {Name: "f", Label: "Player"}, "G": {Name: "g"}, "H": {Name: "h"}}
	kept, forgotten := mergeDevices(file, mem, base)
	if !slices.Equal(kept, []string{"C", "H"}) || !slices.Equal(forgotten, []string{"G"}) {
		t.Fatalf("kept %v forgotten %v", kept, forgotten)
	}
	want := map[string]Device{"C": {Name: "c", Window: "c"}, "F": {Name: "f", Label: "Player"}, "H": {Name: "h", Window: "h 2"}}
	if !maps.Equal(file, want) {
		t.Fatalf("file %+v", file)
	}
	// The file changed G itself (a label typed by hand): forgetting it in memory doesn't override it.
	file = map[string]Device{"G": {Name: "g", Label: "Host"}}
	mergeDevices(file, map[string]Device{}, map[string]Device{"G": {Name: "g"}})
	if file["G"].Label != "Host" {
		t.Fatalf("hand edit overridden: %+v", file)
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

// A hand edit saved before Restart is merged, not overwritten, when the app saves a new device.
func TestHandEditNotOverwrittenBySave(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	configBroken = false
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // a different modification time
	os.WriteFile(configPath, []byte(`{"name": "STAGE", "maxSlots": 4, "extraArgs": ["-reset", "30"],
	  "devices": {"L": {"name": "l", "label": "Player 1"}}}`), 0o644)
	cfg.Devices["A"] = Device{Name: "a"} // a device connects before Restart
	saveConfig()
	if err := writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(configPath)
	c, _, err := parseConfig(b)
	if err != nil || c.Name != "STAGE" || c.MaxSlots != 4 || len(c.ExtraArgs) != 2 || c.Devices["A"].Name != "a" ||
		c.Devices["L"].Label != "Player 1" || unsaved {
		t.Fatalf("hand edit or device lost: %s %v unsaved=%v", b, err, unsaved)
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

func TestReadErrorKeepsUserEditsAndDevices(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Device{}))
	configBroken = false
	os.Mkdir(configPath, 0o755)
	if loadConfig() == nil || !readFailed {
		t.Fatalf("expected a read failure")
	}
	os.Remove(configPath)
	edited := `{"name": "STAGE", "maxSlots": 3}` // the user's edit, not read yet
	os.WriteFile(configPath, []byte(edited), 0o644)
	cfg.Devices["B"] = Device{Name: "b"} // a device connects meanwhile
	saveConfig()
	writeConfig()
	if b, _ := os.ReadFile(configPath); string(b) != edited || !unsaved {
		t.Fatalf("the user's edit was overwritten: %s", b)
	}
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "STAGE" || cfg.MaxSlots != 3 || cfg.Devices["B"].Name != "b" || readFailed || unsaved {
		t.Fatalf("after Restart: %+v readFailed=%v unsaved=%v", cfg, readFailed, unsaved)
	}
	if b, _ := os.ReadFile(configPath); !strings.Contains(string(b), `"B": {`) {
		t.Fatalf("device not saved: %s", b)
	}
}

func TestFailedSaveKeepsDeviceOnReload(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Device{"A": {Name: "a"}}))
	configBroken = false
	saveConfig()
	writeConfig()
	// The save of a new device fails (the file is held): memory has it, the disk doesn't.
	cfg.Devices["C"] = Device{Name: "c"}
	unsaved = true
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Devices["C"].Name != "c" || cfg.Devices["A"].Name != "a" {
		t.Fatalf("device lost on reload: %+v", cfg.Devices)
	}
}

func TestOldVersionResetRestoredFromBackup(t *testing.T) {
	withConfigFile(t)
	withConfig(t, testConfig(map[string]Device{"22:C5:83:7E:F0:3F": {Name: "Otavio"}}))
	configBroken = false
	cfg.Name = "STAGE"
	saveConfig()
	writeConfig() // also writes config.json.bak
	// The old exe started on the English file and rewrote it with its own keys and no devices.
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","slots":8,"qualidade":"FHD","uxplay":"","argsExtras":[],"vinculos":{}}`), 0o644)
	cfg, diskCfg = defaultConfig(), defaultConfig() // a fresh start
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Devices["22:C5:83:7E:F0:3F"].Name != "Otavio" || cfg.Name != "STAGE" {
		t.Fatalf("not restored from config.json.bak: %+v", cfg)
	}
	// An old config that really has no devices and no .bak stays as it is.
	os.Remove(configPath + ".bak")
	os.WriteFile(configPath, []byte(`{"nome":"LIVE","vinculos":{}}`), 0o644)
	if err := loadConfig(); err != nil || len(cfg.Devices) != 0 {
		t.Fatalf("unexpected devices: %+v %v", cfg.Devices, err)
	}
}

// The old exe ran on the English file, bound one device and saved: the other devices and the
// settings come back from config.json.bak, and the old exe's device is kept too.
func TestOldVersionDeviceMergedWithBackup(t *testing.T) {
	withConfigFile(t)
	c := testConfig(map[string]Device{"A": {Name: "a"}, "B": {Name: "b", Label: "Player"}, "X": {Name: "x-old"}})
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
	want := map[string]Device{"A": {Name: "a"}, "B": {Name: "b", Label: "Player"}, "X": {Name: "x"}}
	if !maps.Equal(cfg.Devices, want) || cfg.Name != "STAGE" || cfg.FPS != 30 || cfg.MaxSlots != 6 {
		t.Fatalf("not merged with config.json.bak: %+v", cfg)
	}
}

func TestSettingChangeResumesCrashedSlots(t *testing.T) {
	withConfigFile(t)
	withConfig(t, defaultConfig())
	configBroken = false
	before := slots
	t.Cleanup(func() { slots = before })
	crashed := &Slot{N: 1, active: true, crashStopped: true, crashedOut: true, restartCh: make(chan struct{}, 1)}
	other := &Slot{N: 2, active: true, restartCh: make(chan struct{}, 1)}
	slots = []*Slot{crashed, other}
	crashWarned = true
	setFPS(60) // same value: nothing to resume
	if len(crashed.restartCh) != 0 || !crashWarned {
		t.Fatalf("an unchanged setting must not wake anything")
	}
	setResolution("fhd")
	if len(crashed.restartCh) != 1 || len(other.restartCh) != 0 || crashWarned || crashed.crashedOut {
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
	if msg, _, _ := s.processLine("*** codec is h264: geometry is (w,h)=(2868,1320) (source); (w,h)=(2868,1320) (displayed)", now); msg == "" {
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

// A report cut short (UxPlay died mid-print, or another thread's line in the middle) never hides
// the session's own lines.
func TestCutFPSReportDoesNotHideLines(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	t0 := time.Now()
	connect(s, realLine, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	half := strings.Split(fpsReport, "\n")[:8]
	for _, l := range half {
		s.processLine(l, t0)
	}
	msg, _, keep := s.processLine("raop_rtp_mirror->running is no longer true", t0)
	if s.mirroring || !keep || !strings.Contains(msg, "stopped mirroring") {
		t.Fatalf("the end of mirroring was taken as part of the report: %q keep=%v", msg, keep)
	}
}

// The patched UxPlay's privacy lines: the window is black while the device's screen is off and
// cfg.WakeBlack after it comes back on (lock screen, passcode keypad), and whenever nobody mirrors.
func TestPrivacyScreenOff(t *testing.T) {
	withConfig(t, testConfig(map[string]Device{}))
	withSlots(t, 1)
	s := slots[0]
	s.cmd = &exec.Cmd{}
	t0 := time.Now()
	if !s.privateAt(t0) {
		t.Fatalf("nobody mirroring: a frozen window must be black")
	}
	connect(s, realLine, t0)
	s.processLine("Begin streaming to GStreamer video pipeline", t0)
	if s.privateAt(t0) || strings.Contains(slotLine(s), "black") {
		t.Fatalf("live session hidden: %q", slotLine(s))
	}
	msg, _, keep := s.processLine("privacy: video paused, device asleep", t0)
	if !s.asleep || !keep || msg != "slot 1: Otavio Augusto’s iPhone screen off: window black" ||
		!s.privateAt(t0.Add(time.Hour)) || !strings.Contains(slotLine(s), "screen off: black") {
		t.Fatalf("screen off: %q asleep %v line %q", msg, s.asleep, slotLine(s))
	}
	if msg, _, _ := s.processLine("privacy: video paused, device asleep", t0); msg != "" {
		t.Fatalf("a repeated pause line: %q", msg)
	}
	t1 := t0.Add(time.Minute)
	msg, _, _ = s.processLine("privacy: video resumed, device awake", t1)
	if s.asleep || !strings.Contains(msg, "black for 10 s more") ||
		!s.privateAt(t1.Add(9*time.Second)) || s.privateAt(t1.Add(10*time.Second)) {
		t.Fatalf("woke: %q asleep %v", msg, s.asleep)
	}
	if msg, _, _ := s.processLine("privacy: video resumed, device awake", t1); msg != "" || !s.wokeAt.Equal(t1) {
		t.Fatalf("a resume without a pause must not restart the wait: %q", msg)
	}
	cfg.WakeBlack = 0
	if s.privateAt(t1) {
		t.Fatalf("wakeBlackSeconds 0: shown right when it wakes")
	}
	cfg.WakeBlack = 10

	blackAll = true
	t.Cleanup(func() { blackAll = false })
	if !s.privateAt(t1.Add(time.Hour)) || !strings.Contains(slotLine(s), "black screen") {
		t.Fatalf("Black screen on: %q", slotLine(s))
	}
	blackAll = false

	// A new session starts awake.
	s.processLine("privacy: video paused, device asleep", t1)
	s.processLine("raop_rtp_mirror->running is no longer true", t1)
	if !s.privateAt(t1) {
		t.Fatalf("session over: the frozen window must stay black")
	}
	connect(s, realLine, t1.Add(time.Minute))
	if s.asleep || !s.wokeAt.IsZero() {
		t.Fatalf("new session kept the old one's sleep: asleep %v woke %v", s.asleep, s.wokeAt)
	}
	// A pause line without a session (nobody mirroring) changes nothing.
	if msg, _, _ := s.processLine("privacy: video paused, device asleep", t1); msg != "" || s.asleep {
		t.Fatalf("pause without a session: %q", msg)
	}
}

func TestParseConfigWakeBlack(t *testing.T) {
	for in, want := range map[string]int{`{}`: 10, `{"wakeBlackSeconds": 3}`: 3, `{"wakeBlackSeconds": 999}`: 60, `{"wakeBlackSeconds": -5}`: 0} {
		n, _, err := parseConfig([]byte(in))
		if err != nil || n.WakeBlack != want {
			t.Fatalf("%s: wakeBlackSeconds %d, want %d (%v)", in, n.WakeBlack, want, err)
		}
	}
}
