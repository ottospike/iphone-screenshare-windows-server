// iPhone Screenshare Windows Server: a Windows tray icon that keeps UxPlay (an AirPlay receiver)
// running so OBS can capture its window. UxPlay and GStreamer are embedded in this .exe
// (bundle/uxplay.zip, built by CI) and extracted on first run. It restarts UxPlay if it crashes and
// stops retrying if it crashes in a loop (bad flag, port in use), pointing to the log.
// A pool of UxPlay instances that all use the same name ("LIVE"): exactly one idle instance is
// advertised for the next device. UxPlay is patched (ci/uxplay-pool.patch) to remove its mDNS
// advertisement while a device mirrors and add it again when mirroring stops, so busy instances are
// hidden and can't be taken over. The video window is named after the device (OBS follows the person),
// stable for the same deviceID.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

const (
	// New rule name (v2 = any network profile, local subnet only); the old one is deleted with it.
	firewallRule    = "iPhone Screenshare Windows Server v2"
	oldFirewallRule = "iPhone Screenshare Windows Server"
	slotLimit       = 8 // instances (menu items created at startup); maxSlots in the config goes up to this
	appTitle        = "iPhone Screenshare"
)

//go:embed bundle/uxplay.zip
var bundleZip []byte

// Device is a remembered device, keyed by the deviceID it sends over AirPlay (UxPlay logs it).
type Device struct {
	Name   string `json:"name"`             // name the device sends
	Label  string `json:"label"`            // set by hand: replaces the name in the window title and the tray
	Window string `json:"window,omitempty"` // window title it got last time (kept so it doesn't change)
}

type Config struct {
	Name       string            `json:"name"`             // the one name in Screen Mirroring
	MaxSlots   int               `json:"maxSlots"`         // max devices at once (1 to 8)
	Resolution string            `json:"resolution"`       // "native", "fhd", "hd" or "sd"
	FPS        int               `json:"fps"`              // 60 or 30
	UxPlay     string            `json:"uxplay"`           // path to another uxplay.exe; empty = the embedded one
	ExtraArgs  []string          `json:"extraArgs"`        // e.g. ["-reset", "30"]; added last, so they override the app's
	Devices    map[string]Device `json:"devices"`          // deviceID -> remembered device
	WakeBlack  int               `json:"wakeBlackSeconds"` // the window stays black this long after the device's screen comes back on (lock screen); 0 to 60
}

// legacyConfig holds the keys of earlier versions (Portuguese, then slot bindings); they are
// migrated on load.
type legacyConfig struct {
	Nome       string   `json:"nome"`
	Slots      int      `json:"slots"`
	Qualidade  string   `json:"qualidade"`
	ArgsExtras []string `json:"argsExtras"`
	Vinculos   map[string]struct {
		DeviceID string `json:"deviceID"`
		Iphone   string `json:"iphone"`
	} `json:"vinculos"`
	Bindings map[string]struct {
		DeviceID string `json:"deviceID"`
		Device   string `json:"device"`
	} `json:"bindings"`
}

// The receiver must declare a display size in AirPlay; the sender scales its screen to fit it.
// A square request lets portrait and landscape devices both fit at their full size. The sender
// scales up too: 3840 made a 1320×2868 iPhone send 1768×3840. 2880 is about a recent iPhone's
// native height and still fits an iPad Pro's 2752.
var resolutions = map[string][]string{
	"native": {"-h265", "-s", "2880x2880"},
	"fhd":    {"-s", "1920x1920"},
	"hd":     {"-s", "1280x1280"},
	"sd":     {"-s", "854x854"},
}

// Values of earlier versions, renamed on load.
var oldResolutions = map[string]string{"1920": "fhd", "1280": "hd"}

var (
	resolutionOrder  = []string{"native", "fhd", "hd", "sd"}
	resolutionLabels = map[string]string{"native": "Native", "fhd": "FHD", "hd": "HD", "sd": "SD"}
	fpsOrder         = []int{60, 30}
)

var (
	colors = map[string]color.RGBA{
		"gray": {120, 120, 120, 255}, "blue": {50, 130, 220, 255}, "green": {40, 190, 90, 255},
		"yellow": {230, 170, 30, 255}, "red": {220, 50, 50, 255},
	}
	reConnection = regexp.MustCompile(`connection request from (.*) \((.*)\) with deviceID = (\S+)`)
	reSocket     = regexp.MustCompile(`Accepted IPv\d client on socket (\d+)`)
	reClosed     = regexp.MustCompile(`Connection closed on socket (\d+)`)
	reGeometry   = regexp.MustCompile(`codec is (h26[45]): geometry is \(w,h\)=\((\d+),(\d+)\) \(source\)`)
	// Lines UxPlay writes when it refuses the connection it just logged.
	refusals = []string{"DENIED", "have been restricted", "rejecting new connection", "Conflict: Server is connected"}
	// Lines that end mirroring (the window keeps the last frame frozen, so it can't be used).
	endings = []string{"raop_rtp_mirror->running is no longer true", "lost connection with client", "Output window was closed"}
)

var (
	appDir       = filepath.Join(os.Getenv("LOCALAPPDATA"), "iPhoneScreenshareServer")
	configPath   = filepath.Join(appDir, "config.json")
	logPath      = filepath.Join(appDir, "app.log")
	hostname, _  = os.Hostname()
	cfg          = defaultConfig()
	mu           sync.Mutex // guards cfg, quitting, the slot fields and the icon cache
	ioMu         sync.Mutex // one read or write of config.json at a time; taken before mu, never inside it
	diskStamp    fileStamp  // config.json as last read or written by the app; guarded by ioMu
	saveReq      = make(chan struct{}, 1)
	adjustMu     sync.Mutex // one adjustSlots run at a time (Show/Hide in the right order)
	quitting     bool       // true once the user asked to quit
	slots        []*Slot    // slotLimit slots; only active ones run UxPlay and show in the menu
	uxplayExe    string     // uxplay.exe in use (empty if setup failed)
	mStatus      *systray.MenuItem
	resItems     = map[string]*systray.MenuItem{} // Resolution submenu
	fpsItems     = map[int]*systray.MenuItem{}    // FPS submenu
	logFile      *os.File
	configNotice string // broken config warning at startup (shown once the tray exists)
	lastIcon     string
	lastTooltip  string
	lastStatus   string
	currentFree  int               // slot currently serving as the free one (0 = none); computed in adjustSlots
	configBroken bool              // config.json on disk is broken: don't overwrite it until the user fixes it
	readFailed   bool              // the last read of config.json failed: don't save over the user's edits until one works
	unsaved      bool              // memory has changes config.json doesn't; keepConfig saves them
	saveGen      int               // bumped by every saveConfig, to know whether a write saved the latest
	diskCfg      = defaultConfig() // config as last read from or written to config.json (merge base)
	crashWarned  bool              // crash loop popup already shown; reset on Restart or a settings change
	blackAll     bool              // "Black screen" in the tray: every video window is hidden
)

// Slot is one UxPlay instance of the pool, with its own ports and MAC (the name is the same for
// all). It is either the free one (advertised, waiting for the next device), busy with a device's
// session (hidden from Screen Mirroring by the patched UxPlay), or down.
type Slot struct {
	N         int
	active    bool // should be running (busy, or the current free one)
	running   bool // a supervisor goroutine owns this slot
	cmd       *exec.Cmd
	args      []string  // args the current UxPlay started with (nil without a process)
	argsRes   string    // resolution and fps the current UxPlay started with
	argsFPS   int       //
	started   time.Time // when the current UxPlay started
	failed    time.Time // last start error; retried 10 s later
	restartCh chan struct{}
	state     string
	color     string

	newSock      string // socket of the last connection accepted by UxPlay's httpd
	candName     string // device that asked to connect and was not yet accepted or refused
	candID       string
	candSock     string
	candAt       time.Time // when that device asked to connect
	sock         string    // socket of the accepted session
	device       string    // name the accepted device sent
	deviceID     string    // deviceID of the accepted device; empty while nobody connected since the start
	title        string    // window title of the session (the device's display name, unique)
	connected    time.Time // when it was accepted
	mirroring    bool      // UxPlay is receiving video (from the log)
	asleep       bool      // the device's screen is off: the window would freeze on its last frame (patched UxPlay: "privacy: video paused")
	wokeAt       time.Time // when the device's screen came back on; the window stays black cfg.WakeBlack after it
	videoStart   time.Time // when video started
	ended        time.Time // when the last mirroring session ended
	width        int       // source video size from the last geometry line
	height       int
	codec        string    // "H.264" or "H.265", from the last geometry line
	geometryAt   time.Time // when the geometry last changed
	fps          float64   // frame rate the client reports (-FPSdata); 0 = unknown
	plist        []string  // lines of the FPS report being read
	plistShown   bool      // the first FPS report of the session was written to the log
	dropBlank    bool      // skip the empty line UxPlay prints after a report left out of the log
	hidden       bool      // the patched UxPlay removed its advertisement (a device mirrors, or announcing again failed)
	announced    time.Time // when its UxPlay last announced itself over mDNS, from the log (see poolPlan)
	readvertised bool      // the session ended and UxPlay announced itself again: free, the last device kept for its window
	ready        bool      // the current UxPlay announced itself ("mDNS: advertised at startup")
	pending      bool      // restart when idle even without arg changes (network changed)
	crashStopped bool      // stopped after 3 quick crashes; waits for Restart or a settings change
	crashedOut   bool      // crash-stopped once: not used for the pool until Restart or a settings change

	// fitted is the video size each of the slot's windows was last fitted to, so a window is fitted
	// when it shows up and on each new geometry, not in between. Guarded by fitMu.
	fitted map[windows.HWND][2]int

	line   string // last text set on the menu item (empty = forces SetTitle when shown)
	item   *systray.MenuItem
	apply  *systray.MenuItem // "Apply now": shown while new settings wait for this instance's restart
	forget *systray.MenuItem // "Forget device"
}

func defaultConfig() Config {
	return Config{Name: "LIVE", MaxSlots: slotLimit, Resolution: "native", FPS: 60, ExtraArgs: []string{}, Devices: map[string]Device{}, WakeBlack: 10}
}

// idleAt tells whether the slot can be restarted or stopped without dropping anyone: nobody
// mirroring (from UxPlay's log), no device asking to connect in the last 10 s (the handshake), no
// device accepted in the last 20 s (video only starts after) unless its session already ended and
// UxPlay advertised again, and 5 s since the last session ended. Call with mu held.
func (s *Slot) idleAt(now time.Time) bool {
	if s.cmd == nil {
		return true
	}
	handshake := s.candID != "" && now.Sub(s.candAt) <= 10*time.Second
	accepted := !s.readvertised && now.Sub(s.connected) <= 20*time.Second
	return !s.mirroring && !handshake && !accepted && now.Sub(s.ended) > 5*time.Second
}

// busyAt tells whether the slot is up with a session in progress (mirroring or in the grace
// period). Such a slot can't be the free one. Call with mu held.
func (s *Slot) busyAt(now time.Time) bool { return s.active && !s.idleAt(now) }

// used tells whether the slot holds a device's session: a device connected to its UxPlay since it
// started and the patched UxPlay did not advertise it again after the mirroring. Its window
// (frozen on the last frame) and state belong to that session, so it restarts before serving
// another device. Once advertised again, UxPlay serves the next device as it is. Call with mu held.
func (s *Slot) used() bool { return s.cmd != nil && s.deviceID != "" && !s.readvertised }

// privateAt tells whether the slot's video window must not be seen (made transparent, so OBS gets
// nothing): Black screen is on, nobody is mirroring (the window would hold the last frame frozen),
// the device's screen is off (frozen too, often on the lock screen), or it came back on less than
// cfg.WakeBlack ago (the lock screen and passcode keypad show live until it is unlocked). Call with
// mu held.
func (s *Slot) privateAt(now time.Time) bool {
	return blackAll || !s.mirroring || s.asleep || now.Sub(s.wokeAt) < time.Duration(cfg.WakeBlack)*time.Second
}

// privacyNote says why a live session's window is black, for the tray line; empty if it isn't.
// Call with mu held.
func (s *Slot) privacyNote(now time.Time) string {
	switch {
	case !s.mirroring:
		return ""
	case blackAll:
		return "black screen"
	case s.asleep:
		return "screen off: black"
	case s.privateAt(now):
		return "just woke: black"
	}
	return ""
}

// orientation describes the source video, e.g. "portrait 1320×2868"; empty if unknown.
func orientation(w, h int) string {
	switch {
	case w <= 0 || h <= 0:
		return ""
	case h > w:
		return fmt.Sprintf("portrait %d×%d", w, h)
	case w > h:
		return fmt.Sprintf("landscape %d×%d", w, h)
	}
	return fmt.Sprintf("square %d×%d", w, h)
}

// parseGeometry reads the codec ("H.264" or "H.265") and the source size from UxPlay's
// "codec is h264: geometry is ..." line.
func parseGeometry(line string) (codec string, w, h int, ok bool) {
	m := reGeometry.FindStringSubmatch(line)
	if m == nil {
		return "", 0, 0, false
	}
	w, _ = strconv.Atoi(m[2])
	h, _ = strconv.Atoi(m[3])
	return "H." + m[1][1:], w, h, true
}

// rePlistToken reads the parts of a plist in XML (libplist's plist_to_xml, as UxPlay prints the
// client's reports with -FPSdata) that matter here: keys, scalar values and dict/array bounds.
var rePlistToken = regexp.MustCompile(`<(key|string|integer|real)>([^<]*)</(?:key|string|integer|real)>|<(/?)(dict|array)>|<(dict|array)/>`)

// plistNode is a value of a parsed plist: a dict (keys and vals in order), an array (vals) or a scalar.
type plistNode struct {
	kind string // "dict", "array", "string", "integer", "real"
	text string
	keys []string
	vals []*plistNode
}

// parsePlistXML reads a plist in XML loosely: it never fails, a cut or interleaved line just loses
// the values around it.
func parsePlistXML(xml string) *plistNode {
	root := &plistNode{kind: "array"}
	stack := []*plistNode{root}
	pendingKey := ""
	add := func(n *plistNode) {
		top := stack[len(stack)-1]
		if top.kind == "dict" {
			top.keys = append(top.keys, pendingKey)
		}
		top.vals = append(top.vals, n)
		pendingKey = ""
	}
	for _, m := range rePlistToken.FindAllStringSubmatch(xml, -1) {
		switch {
		case m[1] == "key":
			pendingKey = strings.TrimSpace(m[2])
		case m[1] != "":
			add(&plistNode{kind: m[1], text: strings.TrimSpace(m[2])})
		case m[5] != "": // <dict/>, <array/>
			add(&plistNode{kind: m[5]})
		case m[3] == "": // opening
			n := &plistNode{kind: m[4]}
			add(n)
			stack = append(stack, n)
		case len(stack) > 1 && stack[len(stack)-1].kind == m[4]: // matching close
			stack = stack[:len(stack)-1]
			pendingKey = ""
		}
	}
	return root
}

func (n *plistNode) number() (float64, bool) {
	if n == nil || (n.kind != "integer" && n.kind != "real") {
		return 0, false
	}
	v, err := strconv.ParseFloat(n.text, 64)
	return v, err == nil
}

// parseFPSReport reads the frame rate from one of the client's -FPSdata reports (a plist, about
// once a second). UxPlay prints it as the client sends it and its keys are the client's, so they are
// matched loosely: a number labeled by its key or by the "name" of its dict, preferring the frame
// rate the client sent ("Sent" in its fpsInfo stages), then any label with "fps" or "frame rate".
func parseFPSReport(xml string) (float64, bool) {
	best, bestScore := 0.0, 0
	consider := func(label string, path []string, v float64) {
		if v < 0 || v > 1000 {
			return
		}
		l := strings.ToLower(strings.TrimSpace(label))
		inFPS := false
		for _, p := range path {
			inFPS = inFPS || strings.Contains(strings.ToLower(p), "fps")
		}
		score := 0
		switch {
		case inFPS && (l == "sent" || l == "sndfr"):
			score = 5
		case l == "fps" || strings.HasSuffix(l, "fps") && strings.Contains(l, "sent"):
			score = 4
		case strings.Contains(l, "fps") && !strings.Contains(l, "max"):
			score = 3
		case strings.Contains(l, "framerate") || strings.Contains(l, "frame rate"):
			score = 2
		}
		if score > bestScore {
			best, bestScore = v, score
		}
	}
	var walk func(n *plistNode, path []string)
	walk = func(n *plistNode, path []string) {
		if n.kind == "dict" {
			// A {name: "Sent", value: 59.9}-style entry: the number is labeled by the name.
			name, nums := "", []int{}
			for i, k := range n.keys {
				switch {
				case strings.EqualFold(k, "name") && n.vals[i].kind == "string":
					name = n.vals[i].text
				case n.vals[i].kind == "integer" || n.vals[i].kind == "real":
					nums = append(nums, i)
				}
			}
			for _, i := range nums {
				v, _ := n.vals[i].number()
				k := strings.ToLower(n.keys[i])
				if name != "" && (len(nums) == 1 || k == "value" || k == "fps" || k == "val") {
					consider(name, path, v)
				}
				consider(n.keys[i], path, v)
			}
		}
		for i, c := range n.vals {
			p := path
			if n.kind == "dict" && i < len(n.keys) {
				p = append(slices.Clone(path), n.keys[i])
			}
			walk(c, p)
		}
	}
	walk(parsePlistXML(xml), nil)
	return best, bestScore > 0
}

func main() {
	// Two running copies would fight over the same names and ports and crash in a loop.
	if !singleInstance() {
		return
	}
	createJob()
	os.MkdirAll(appDir, 0o755)
	if st, err := os.Stat(logPath); err == nil && st.Size() > 5<<20 {
		os.Rename(logPath, logPath+".old")
	}
	logFile, _ = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err := loadConfig(); err != nil {
		// Defaults in memory; the file is not overwritten until it reads fine (configBroken or
		// readFailed, set by loadConfig).
		configNotice = startupConfigNotice(err)
	}
	systray.Run(onReady, onExit)
}

// startupConfigNotice handles a config.json that could not be loaded at startup: it keeps a copy of
// a broken file (so the remembered devices are not lost), logs, and returns the popup text.
func startupConfigNotice(err error) string {
	const after = "\n\nThe app started with the defaults, without the remembered devices, and will not overwrite the file. "
	var re *readError
	if errors.As(err, &re) {
		logf("config.json could not be read: %v; using defaults in memory only", err)
		return "config.json could not be read:\n" + err.Error() + after +
			"Close the program that holds it and use Restart."
	}
	note := "Fix it in Notepad and use Restart."
	if b, rerr := os.ReadFile(configPath); rerr == nil && os.WriteFile(configPath+".broken", b, 0o644) == nil {
		note = "A copy was saved to config.json.broken. " + note
		logf("config.json is invalid: %v; copy saved to config.json.broken, using defaults in memory only", err)
	} else {
		logf("config.json is invalid: %v; using defaults in memory only", err)
	}
	return "config.json has an error:\n" + err.Error() + after + note
}

// singleInstance holds the app mutex. If another copy is running, it asks whether to close it
// (through the quit event every copy listens to) and takes its place; its icon may be hidden in
// the tray overflow.
func singleInstance() bool {
	const mutexName, quitName = `Local\iPhoneScreenshareServer`, `Local\iPhoneScreenshareServerQuit`
	m, err := windows.CreateMutex(nil, false, ptr(mutexName))
	if err == windows.ERROR_ACCESS_DENIED {
		// The running copy is elevated: this one can neither open its mutex nor ask it to quit.
		warn("iPhone Screenshare is already running as administrator. Quit it from its tray menu " +
			"and start this one again. Its icon may be under the ^ arrow.")
		return false
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(m)
		r, _ := windows.MessageBox(0, ptr("iPhone Screenshare is already running. Its icon is in the tray near the clock, "+
			"maybe under the ^ arrow.\n\nClose the running copy and start this one?"),
			ptr(appTitle), windows.MB_YESNO|windows.MB_ICONQUESTION)
		if r != 6 { // 6 = IDYES
			return false
		}
		ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, ptr(quitName))
		if err != nil {
			warn("The running copy is an older version: quit it from the tray menu and start again.")
			return false
		}
		windows.SetEvent(ev)
		windows.CloseHandle(ev)
		for i := 0; ; i++ {
			m, err = windows.CreateMutex(nil, false, ptr(mutexName))
			if err != windows.ERROR_ALREADY_EXISTS && err != windows.ERROR_ACCESS_DENIED {
				break
			}
			windows.CloseHandle(m)
			if i == 50 {
				warn("The running copy did not close. Quit it from the tray menu and start again.")
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// The event may still exist for a moment after the old copy exits: the handle is valid then too.
	ev, err := windows.CreateEvent(nil, 0, 0, ptr(quitName))
	if ev != 0 && (err == nil || err == windows.ERROR_ALREADY_EXISTS) {
		go func() {
			windows.WaitForSingleObject(ev, windows.INFINITE)
			systray.Quit()
		}()
	}
	return true
}

// createJob puts the app in a job object that kills everything inside when the app exits, however
// it exits. Children (uxplay.exe and its gst-plugin-scanner) join the job at CreateProcess, so no
// orphan UxPlay keeps the ports even if the app dies right after starting one.
func createJob() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	windows.AssignProcessToJobObject(h, windows.CurrentProcess())
	// the handle stays open until the app exits; closing it is what kills the children
}

func logf(f string, a ...any) {
	fmt.Fprintf(logFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(f, a...))
}

const (
	migratedNote = "config: old keys migrated to English; original kept in config.json.v1"
	bindingsNote = "config: slot bindings converted to devices; original kept in config.json.v2"
)

// parseConfig decodes config.json over the defaults, migrates the old Portuguese keys and
// normalizes. It also returns what is worth logging (migrations).
func parseConfig(b []byte) (Config, []string, error) {
	n := defaultConfig()
	n.MaxSlots = 0                                  // to know whether the file has the field
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // BOM from old Notepad
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return n, nil, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, nil, err
	}
	var old legacyConfig
	json.Unmarshal(b, &old) // same document, already known to be valid JSON
	has := func(k string) bool { _, ok := raw[k]; return ok }

	var notes []string
	migrated := false
	if !has("name") && has("nome") {
		n.Name, migrated = old.Nome, true
	}
	if !has("resolution") && has("qualidade") {
		n.Resolution, migrated = "native", true
		if old.Qualidade == "HD" {
			n.Resolution = "hd"
		}
	}
	if r, ok := oldResolutions[n.Resolution]; ok {
		notes = append(notes, fmt.Sprintf("config: resolution %q renamed to %q", n.Resolution, r))
		n.Resolution = r
	}
	if !has("extraArgs") && has("argsExtras") {
		n.ExtraArgs, migrated = old.ArgsExtras, true
	}
	if !has("devices") {
		// Slot bindings of earlier versions: the bound devices are remembered, so their window keeps
		// following them (named after the device now).
		type bound struct{ id, name string }
		var bs []bound
		for _, v := range old.Bindings {
			bs = append(bs, bound{v.DeviceID, v.Device})
		}
		if !has("bindings") {
			for _, v := range old.Vinculos {
				bs = append(bs, bound{v.DeviceID, v.Iphone})
			}
		}
		for _, b := range bs {
			if id := strings.TrimSpace(b.id); id != "" {
				n.Devices[id] = Device{Name: b.name}
			}
		}
		switch {
		case has("bindings"):
			notes = append(notes, fmt.Sprintf("%s: %d device(s) remembered", bindingsNote, len(n.Devices)))
		case has("vinculos"):
			migrated = true
			notes = append(notes, fmt.Sprintf("config: \"vinculos\" converted to devices: %d device(s) remembered", len(n.Devices)))
		}
	}
	if _, ok := resolutions[n.Resolution]; !ok {
		n.Resolution = "native"
	}
	if !slices.Contains(fpsOrder, n.FPS) {
		n.FPS = 60
	}
	if strings.TrimSpace(n.Name) == "" {
		n.Name = "LIVE"
	}
	if n.MaxSlots == 0 {
		// Old config: "slots" was the number of fixed slots; the pool now grows on demand, so it
		// goes to the limit (a single slot stays a single device).
		n.MaxSlots = slotLimit
		if old.Slots == 1 {
			n.MaxSlots = 1
		}
		if old.Slots > 0 {
			migrated = true
			notes = append(notes, fmt.Sprintf("config: \"slots\": %d converted to \"maxSlots\": %d", old.Slots, n.MaxSlots))
		}
	}
	if migrated {
		notes = append(notes, migratedNote)
	}
	n.MaxSlots = min(max(n.MaxSlots, 1), slotLimit)
	n.WakeBlack = min(max(n.WakeBlack, 0), 60)
	if n.ExtraArgs == nil {
		n.ExtraArgs = []string{}
	}
	if n.Devices == nil {
		n.Devices = map[string]Device{}
	}
	delete(n.Devices, "")
	return n, notes, nil
}

// readError means config.json exists but could not be read (locked, access denied), as opposed to
// a file with invalid JSON.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// fileStamp identifies one version of config.json on disk (zero if it doesn't exist).
type fileStamp struct {
	mod  time.Time
	size int64
}

func statConfig() fileStamp {
	st, err := os.Stat(configPath)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{st.ModTime(), st.Size()}
}

// loadConfig reads config.json (or creates it with the defaults) and merges into it the changes
// made in the app that never reached the disk. If the file is broken or can't be read it leaves the
// current config and the file alone and returns the error; saving waits until a load works
// (configBroken, readFailed). Call without mu held.
func loadConfig() error {
	ioMu.Lock()
	defer ioMu.Unlock()
	return loadConfigLocked()
}

func loadConfigLocked() error {
	var b []byte
	var err error
	for i := 0; i < 5; i++ { // antivirus or sync tools may hold the file for a moment
		if b, err = os.ReadFile(configPath); err == nil || errors.Is(err, fs.ErrNotExist) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if errors.Is(err, fs.ErrNotExist) {
		b = []byte("{}")
	} else if err != nil {
		mu.Lock()
		readFailed = true // the file may hold the user's edits: don't save over it until it is read
		mu.Unlock()
		return &readError{err}
	}
	diskStamp = statConfig()
	n, notes, err := parseConfig(b)
	if err != nil {
		mu.Lock()
		configBroken = true // the config in use lives only in memory until the file is fixed
		mu.Unlock()
		return err
	}
	if slices.Contains(notes, migratedNote) {
		if bak, err := os.ReadFile(configPath + ".bak"); err == nil {
			if r, ok := fromBackup(n, bak); ok {
				n = r
				notes = append(notes, "config: config.json was rewritten by an older version; settings and devices restored from config.json.bak")
			}
		}
		// Older versions only read the old keys: keep the original for a downgrade.
		if _, err := os.Stat(configPath + ".v1"); errors.Is(err, fs.ErrNotExist) {
			os.WriteFile(configPath+".v1", b, 0o644)
		}
	}
	if slices.ContainsFunc(notes, func(t string) bool { return strings.HasPrefix(t, bindingsNote) }) {
		if _, err := os.Stat(configPath + ".v2"); errors.Is(err, fs.ErrNotExist) {
			os.WriteFile(configPath+".v2", b, 0o644)
		}
	}
	mu.Lock()
	onDisk := n
	onDisk.Devices = maps.Clone(n.Devices)
	notes = append(notes, mergeMemory(&n, cfg, diskCfg)...)
	cfg, configBroken, readFailed, diskCfg = n, false, false, onDisk
	saveConfig()
	mu.Unlock()
	for _, t := range notes {
		logf("%s", t)
	}
	writeConfigLocked(false) // on failure keepConfig retries
	return nil
}

// fromBackup handles a config.json rewritten by an older version started by mistake: it read none
// of the English keys, so the settings come from config.json.bak, and the devices it remembered are
// merged with the backup's (the file wins on a conflict).
func fromBackup(n Config, bak []byte) (Config, bool) {
	r, _, err := parseConfig(bak)
	if err != nil {
		return n, false
	}
	merged := maps.Clone(n.Devices)
	mergeDevices(merged, r.Devices, nil)
	r.Devices = merged
	return r, true
}

// mergeMemory applies to n (just read from config.json) the changes made in the app since the
// disk last had base, which never reached the disk: Resolution, FPS, devices remembered and
// forgotten. What the file changed itself wins. Returns lines for the log. Call with mu held.
func mergeMemory(n *Config, mem, base Config) []string {
	var notes []string
	if mem.Resolution != base.Resolution && n.Resolution == base.Resolution {
		n.Resolution = mem.Resolution
		notes = append(notes, "config: resolution "+mem.Resolution+" chosen in the tray kept: it was not saved yet")
	}
	if mem.FPS != base.FPS && n.FPS == base.FPS {
		n.FPS = mem.FPS
		notes = append(notes, fmt.Sprintf("config: fps %d chosen in the tray kept: it was not saved yet", mem.FPS))
	}
	kept, forgotten := mergeDevices(n.Devices, mem.Devices, base.Devices)
	for _, k := range kept {
		notes = append(notes, fmt.Sprintf("config: device %s, %s, kept: it was not saved yet", n.Devices[k].Name, k))
	}
	for _, k := range forgotten {
		notes = append(notes, fmt.Sprintf("config: device %s kept forgotten: it was not saved yet", k))
	}
	return notes
}

// mergeDevices applies to file the device changes in mem since base (devices remembered or
// updated, devices forgotten). A device the file changed itself is left to the file. Returns the
// deviceIDs taken from mem and the ones forgotten.
func mergeDevices(file, mem, base map[string]Device) (kept, forgotten []string) {
	orig := maps.Clone(file)
	untouched := func(k string) bool { // the file still has what the disk had
		a, inFile := orig[k]
		b, inBase := base[k]
		return inFile == inBase && a == b
	}
	for _, k := range sortedKeys(base) {
		if _, ok := mem[k]; !ok && untouched(k) {
			delete(file, k)
			forgotten = append(forgotten, k)
		}
	}
	for _, k := range sortedKeys(mem) {
		if x, ok := base[k]; (ok && x == mem[k]) || !untouched(k) {
			continue
		}
		file[k] = mem[k]
		kept = append(kept, k)
	}
	return kept, forgotten
}

// reloadConfig rereads a hand-edited config.json and says whether it worked. Changes apply to each
// slot when it restarts (watchWindows compares the args).
func reloadConfig() bool {
	err := loadConfig()
	if err != nil {
		const kept = " Nothing was deleted, the file is not overwritten and no slot was restarted.\n"
		var re *readError
		if errors.As(err, &re) {
			logf("config.json could not be read, nothing changed: %v", err)
			go warn("config.json could not be read and was not applied." + kept + err.Error() +
				"\n\nClose the program that holds it and use Restart.")
			return false
		}
		logf("config.json is invalid, nothing changed: %v", err)
		go warn("config.json has an error and was not applied." + kept + err.Error() +
			"\n\nFix it in Notepad and use Restart.")
		return false
	}
	mu.Lock()
	resumeCrashedLocked()
	mu.Unlock()
	logf("config reloaded")
	afterReload()
	return true
}

// afterReload refreshes the tray and the slots after a config was loaded.
func afterReload() {
	syncChecks()
	adjustSlots()
	for _, s := range slots {
		updateMenu(s)
	}
}

// saveConfig asks keepConfig to write the config; call with mu held. It doesn't touch the disk, so
// callers never wait on a locked file with mu held.
func saveConfig() {
	unsaved = true
	saveGen++
	select {
	case saveReq <- struct{}{}:
	default:
	}
}

// writeConfig writes the config if memory has changes the disk doesn't. While the user's file is
// broken or unreadable it doesn't write: they would lose what they typed (and the devices). If the
// file changed on disk since the app last read or wrote it (edited by hand), it is read and merged
// first instead of being overwritten. It also keeps config.json.bak, which an older version started
// by mistake won't touch. Call without mu held.
func writeConfig() error {
	ioMu.Lock()
	defer ioMu.Unlock()
	return writeConfigLocked(true)
}

func writeConfigLocked(mergeEdits bool) error {
	mu.Lock()
	skip := !unsaved || configBroken || readFailed // only loadConfigLocked clears the last two
	mu.Unlock()
	if skip {
		return nil
	}
	if mergeEdits && statConfig() != diskStamp {
		logf("config.json changed on disk: reading it before saving")
		if err := loadConfigLocked(); err != nil {
			logf("config.json can't be used, so it is not overwritten: %v; changes kept in memory until it reads fine", err)
		}
		return nil
	}
	mu.Lock()
	b, _ := json.MarshalIndent(cfg, "", "  ")
	gen, snap := saveGen, cfg
	snap.Devices = maps.Clone(cfg.Devices)
	mu.Unlock()

	err := writeFileAtomic(configPath, b)
	if err != nil {
		return err
	}
	diskStamp = statConfig()
	mu.Lock()
	diskCfg = snap
	unsaved = gen != saveGen // changed again while writing: keepConfig writes again
	mu.Unlock()
	writeFileAtomic(configPath+".bak", b)
	return nil
}

// keepConfig runs for the whole session: it writes what saveConfig asks for, retries a failed save
// with a growing delay, up to 30 s, and warns once, reads config.json again every 5 s after a
// failed read, and reads a broken config.json again as soon as it changes on disk.
func keepConfig() {
	tick := time.NewTicker(time.Second)
	var delay time.Duration
	var nextWrite, nextRead time.Time
	lastErr, warned := "", false
	for {
		select {
		case <-saveReq:
			nextWrite = time.Time{} // a new change: try now
		case <-tick.C:
		}
		now := time.Now()
		mu.Lock()
		failedRead, broken, pending := readFailed, configBroken, unsaved
		mu.Unlock()
		ioMu.Lock()
		changed := statConfig() != diskStamp
		ioMu.Unlock()
		switch {
		case failedRead || (broken && changed):
			if failedRead && now.Before(nextRead) {
				continue
			}
			nextRead = now.Add(5 * time.Second)
			if loadConfig() == nil {
				logf("config.json read again and applied")
				afterReload()
			}
		case pending && !broken && !now.Before(nextWrite):
			err := writeConfig()
			if err == nil {
				if lastErr != "" {
					logf("config.json saved again")
				}
				delay, nextWrite, lastErr = 0, time.Time{}, ""
				continue
			}
			if err.Error() != lastErr {
				logf("could not save config.json, retrying: %v", err)
			}
			if lastErr != "" && !warned { // failed twice in a row: not a passing lock
				warned = true
				go warn("config.json can't be saved:\n" + err.Error() + "\n\nDevices and settings are kept in memory " +
					"and saved as soon as it works. If the file is read-only, clear that in its Properties.")
			}
			lastErr = err.Error()
			delay = min(max(delay*5, time.Second), 30*time.Second)
			nextWrite = now.Add(delay)
		}
	}
}

// writeFileAtomic writes a temp file, flushes it to disk and renames it over path, so a crash or
// power loss mid-write leaves either the old file or the new one, never a cut-off one.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	for i := 0; err == nil; i++ { // antivirus or backup tools may hold the file for a moment
		if err = os.Rename(tmp, path); err == nil || i == 4 {
			break
		}
		err = nil
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func sortedKeys[V any](v map[string]V) []string {
	ks := make([]string, 0, len(v))
	for k := range v {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// displayName is the name a device goes by: its label if set, else the name it sends (or sent last
// time). Call with mu held.
func displayName(id, sent string) string {
	d := cfg.Devices[id]
	switch {
	case strings.TrimSpace(d.Label) != "":
		return strings.TrimSpace(d.Label)
	case strings.TrimSpace(sent) != "":
		return strings.TrimSpace(sent)
	case strings.TrimSpace(d.Name) != "":
		return strings.TrimSpace(d.Name)
	}
	return id
}

// hasBase tells whether the title t is base or base followed by " N" (N >= 2).
func hasBase(t, base string) bool {
	if t == base {
		return true
	}
	rest, ok := strings.CutPrefix(t, base+" ")
	n, err := strconv.Atoi(rest)
	return ok && err == nil && n >= 2 && strconv.Itoa(n) == rest
}

// pickTitle gives the window title of device id, whose display name is base. It is unique among the
// open sessions (open: title -> deviceID) and the titles remembered for other devices, and the same
// as last time whenever it can be, so the OBS source keeps following the device across reconnects
// and app restarts. Two devices with the same name get "name" and "name 2".
func pickTitle(id, base string, devices map[string]Device, open map[string]string) string {
	taken := func(t string) bool {
		if o, ok := open[t]; ok && o != id {
			return true
		}
		for k, d := range devices {
			if k != id && d.Window == t {
				return true
			}
		}
		return false
	}
	if w := devices[id].Window; w != "" && hasBase(w, base) && !taken(w) {
		return w
	}
	for n := 1; ; n++ {
		t := base
		if n > 1 {
			t = fmt.Sprintf("%s %d", base, n)
		}
		if !taken(t) {
			return t
		}
	}
}

func onReady() {
	systray.SetIcon(icon(colors["gray"]))
	systray.SetTooltip(appTitle)

	mStatus = systray.AddMenuItem("Preparing…", "")
	mStatus.Disable()
	systray.AddSeparator()
	// The menu can't insert items in the middle later: create every instance now and show the active ones.
	for n := 1; n <= slotLimit; n++ {
		s := &Slot{N: n, restartCh: make(chan struct{}, 1), state: "starting", color: "gray"}
		s.item = systray.AddMenuItem("", "")
		s.apply = s.item.AddSubMenuItem("Apply now", "")
		s.forget = s.item.AddSubMenuItem("Forget device", "")
		s.apply.Hide()
		s.forget.Hide()
		s.item.Hide()
		slots = append(slots, s)
	}
	systray.AddSeparator()
	mRes := systray.AddMenuItem("Resolution", "")
	for _, r := range resolutionOrder {
		resItems[r] = mRes.AddSubMenuItemCheckbox(resolutionLabels[r], "", false)
	}
	mFPS := systray.AddMenuItem("FPS", "")
	for _, f := range fpsOrder {
		fpsItems[f] = mFPS.AddSubMenuItemCheckbox(strconv.Itoa(f), "", false)
	}
	syncChecks()
	mBlack := systray.AddMenuItemCheckbox("Black screen", "hide every video window from the screen and OBS", false)
	systray.AddSeparator()
	mRestart := systray.AddMenuItem("Restart", "")
	mLog := systray.AddMenuItem("Open log", "")
	mConfig := systray.AddMenuItem("Open config", "")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "")

	// The menu must respond right away: systray drops the click if nobody is reading. All the
	// Resolution/FPS clicks go through one goroutine, so they apply in order.
	settings := make(chan func(), 8)
	for r, it := range resItems {
		go func() {
			for range it.ClickedCh {
				settings <- func() { setResolution(r) }
			}
		}()
	}
	for f, it := range fpsItems {
		go func() {
			for range it.ClickedCh {
				settings <- func() { setFPS(f) }
			}
		}()
	}
	go func() {
		for apply := range settings {
			apply()
		}
	}()
	go func() {
		for {
			select {
			case <-mBlack.ClickedCh:
				mu.Lock()
				on := !blackAll
				mu.Unlock()
				setBlackAll(on, mBlack)
			case <-mRestart.ClickedCh:
				if reloadConfig() { // a config that didn't load changes nothing: keep live sessions
					restartAll()
				}
			case <-mLog.ClickedCh:
				openInNotepad(logPath)
			case <-mConfig.ClickedCh:
				openInNotepad(configPath)
			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()
	for _, s := range slots {
		go func(s *Slot) {
			for {
				select {
				case <-s.apply.ClickedCh:
					applyNow(s)
				case <-s.forget.ClickedCh:
					forgetDevice(s)
				}
			}
		}(s)
	}
	if configNotice != "" {
		go warn(configNotice)
	}
	go keepConfig()

	exe, err := setup()
	if err != nil {
		logf("ERROR: %v", err)
		mStatus.SetTitle("Error: see the log")
		systray.SetIcon(icon(colors["red"]))
		go warn(err.Error())
		return
	}
	mu.Lock()
	uxplayExe = exe
	mu.Unlock()
	adjustSlots()
	go watchNetwork()
	go watchNewWindows()
	go watchWindows()
	if publicNetwork() {
		// The firewall rule applies to any profile (local subnet only), so just note it.
		logf("note: the Windows network is set to Public")
	}
}

func onExit() {
	mu.Lock()
	quitting = true
	for _, s := range slots {
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
	}
	failedRead := readFailed
	mu.Unlock()
	// Last chance for changes that are only in memory (a save that kept failing, an unreadable file).
	if failedRead {
		loadConfig()
	} else {
		writeConfig()
	}
	logf("app closed")
}

// openInNotepad opens a file in Notepad outside the app's job, so it doesn't close along with the
// app (with unsaved edits).
func openInNotepad(path string) {
	cmd := exec.Command("notepad.exe", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	if cmd.Start() != nil {
		exec.Command("notepad.exe", path).Start() // outer job without breakaway: open inside it
	}
}

// pendingNote says what a session will get on reconnect when the settings changed since its
// UxPlay started, e.g. "SD on reconnect"; empty if nothing is waiting. Call with mu held.
func pendingNote(s *Slot) string {
	if s.cmd == nil || s.args == nil || slices.Equal(slotArgs(s), s.args) {
		return ""
	}
	var parts []string
	if cfg.Resolution != s.argsRes {
		parts = append(parts, resolutionLabels[cfg.Resolution])
	}
	if cfg.FPS != s.argsFPS {
		parts = append(parts, fmt.Sprintf("%d fps", cfg.FPS))
	}
	if parts == nil {
		parts = []string{"new settings"}
	}
	return strings.Join(parts, " ") + " on reconnect"
}

// slotLine is the menu line of an instance, e.g.
// "Otavio Augusto’s iPhone · live · H.265 · portrait 1325×2880 · 60 fps", or "LIVE · waiting" for the
// free one. Call with mu held.
func slotLine(s *Slot) string {
	if s.deviceID == "" || s.readvertised { // nobody since the start, or free again after a session
		return cfg.Name + " · " + s.state
	}
	who := s.title
	if who == "" {
		who = displayName(s.deviceID, s.device)
	}
	t := who + " · " + s.state
	if s.mirroring {
		if s.codec != "" {
			t += " · " + s.codec
		}
		if o := orientation(s.width, s.height); o != "" {
			t += " · " + o
		}
		if s.fps > 0 {
			t += fmt.Sprintf(" · %.0f fps", s.fps)
		}
		if p := s.privacyNote(time.Now()); p != "" {
			t += " · " + p
		}
	}
	if p := pendingNote(s); p != "" {
		t += " · " + p
	}
	return t
}

// updateMenu rewrites the instance's line, its submenu, the status line, the icon and the tray
// tooltip (only when they change).
func updateMenu(s *Slot) {
	mu.Lock()
	defer mu.Unlock()
	if mStatus == nil || s.item == nil {
		return // the tray is not up yet
	}
	t := slotLine(s)
	_, known := cfg.Devices[s.deviceID]
	dev := s.deviceID != "" && !s.readvertised // the line is the device's
	apply, forget := dev && pendingNote(s) != "", dev && known
	// Instance not running: the item is hidden and any SetTitle/Show from systray shows it again.
	// When it comes up, adjustSlots calls this again with s.line empty.
	if k := fmt.Sprint(t, apply, forget); s.active && k != s.line {
		s.line = k
		s.item.SetTitle(t)
		showItem(s.apply, apply)
		showItem(s.forget, forget)
	}

	// Icon: red if any instance has an error; else green if any is live; else yellow if any needs
	// attention (connecting, reconnecting, stopped); else blue (ready, waiting).
	failed, live, attention, someActive, free := false, 0, false, false, false
	for _, o := range slots {
		if !o.active {
			continue
		}
		someActive = true
		switch o.color {
		case "red":
			failed = true
		case "yellow":
			attention = true
		}
		if o.mirroring {
			live++
		}
		free = free || o.N == currentFree
	}
	c := "gray"
	switch {
	case failed:
		c = "red"
	case live > 0:
		c = "green"
	case attention:
		c = "yellow"
	case someActive:
		c = "blue"
	}
	tip := fmt.Sprintf("%s: %d live", appTitle, live)
	status := fmt.Sprintf("%d live · up to %d", live, cfg.MaxSlots)
	if !free && someActive {
		tip += " · no free " + cfg.Name
		status = fmt.Sprintf("%d live · full: maxSlots is %d", live, cfg.MaxSlots)
	}
	if c != lastIcon {
		lastIcon = c
		systray.SetIcon(icon(colors[c]))
	}
	if tip != lastTooltip {
		lastTooltip = tip
		systray.SetTooltip(tip)
	}
	if status != lastStatus && uxplayExe != "" {
		lastStatus = status
		mStatus.SetTitle(status)
	}
}

func showItem(it *systray.MenuItem, show bool) {
	if show {
		it.Show()
	} else {
		it.Hide()
	}
}

func setState(s *Slot, state, c string) {
	mu.Lock()
	s.state, s.color = state, c
	mu.Unlock()
	updateMenu(s)
}

// slotView is what poolPlan needs to know about an instance.
type slotView struct {
	active    bool      // should be running
	running   bool      // a supervisor still owns it (it may be going down)
	busy      bool      // a session in progress
	used      bool      // a device connected since its UxPlay started
	ready     bool      // its current UxPlay is up and advertised
	broken    bool      // stopped by a crash loop: another instance number is used instead
	hidden    bool      // its UxPlay withdrew its advertisement (a session, even if over)
	announced time.Time // when its UxPlay last announced itself over mDNS (at startup or again), from the log
}

// poolPlan says which instances should run: every busy one plus one free one for the next device,
// at most max in all. free is that one (0 when the pool is full) and restart tells whether it must
// restart first (a used instance whose session is over: its window holds the last frame). v[n-1] is
// instance n.
//
// All instances share the service name, so the devices' mDNS caches keep the address of the
// instance that announced itself last (its records flush the others'), and an instance stopped by
// the app sends no goodbye. The patched UxPlay announces at startup and again when a mirroring
// session ends, so an instance whose session just ended takes LIVE over from the free one in the
// devices' caches (if announcing again failed it stays hidden and restarts when it is needed). So
// the free one is the idle instance that is not hidden and announced last, and the other idle one
// goes down; the current free one wins a tie. A new instance is started only when no running one is idle. While
// the free one is restarting or not yet advertised, the other idle ones that are not hidden stay
// up: they still answer devices that query LIVE again, and one of them takes over if the free one
// fails.
func poolPlan(max int, v []slotView, current int) (want map[int]bool, free int, restart bool) {
	want = map[int]bool{}
	busy := 0
	for i, x := range v {
		if x.active && x.busy {
			want[i+1] = true
			busy++
		}
	}
	if busy >= max {
		return want, 0, false
	}
	idle := func(n int) bool { return n >= 1 && n <= len(v) && v[n-1].active && !v[n-1].busy && !v[n-1].broken }
	if idle(current) {
		free = current
	}
	for n := 1; n <= len(v); n++ {
		if !idle(n) {
			continue
		}
		if free == 0 {
			free = n
			continue
		}
		if x, f := v[n-1], v[free-1]; f.hidden && !x.hidden || x.hidden == f.hidden && x.announced.After(f.announced) {
			free = n
		}
	}
	if free > 0 {
		restart = v[free-1].used
	}
	for n := 1; n <= len(v) && free == 0; n++ {
		if x := v[n-1]; !x.active && !x.running && !x.broken {
			free = n
		}
	}
	if free == 0 {
		return want, 0, false
	}
	want[free] = true
	if restart || !v[free-1].ready {
		for n := 1; n <= len(v); n++ {
			if idle(n) && !v[n-1].hidden { // a hidden one answers nobody
				want[n] = true
			}
		}
	}
	return want, free, restart
}

// adjustSlots keeps the pool: it starts the wanted instances, restarts a used free one and stops
// the extra idle ones. Runs at startup, on every accepted device and every second.
func adjustSlots() {
	adjustMu.Lock()
	defer adjustMu.Unlock()
	mu.Lock()
	if quitting || uxplayExe == "" {
		mu.Unlock()
		return
	}
	now := time.Now()
	v := make([]slotView, len(slots))
	for i, s := range slots {
		v[i] = slotView{active: s.active, running: s.running, busy: s.busyAt(now), used: s.used(),
			ready: s.cmd != nil && s.ready && !s.hidden, broken: s.crashStopped || s.crashedOut, hidden: s.cmd != nil && s.hidden, announced: s.announced}
	}
	current := currentFree
	if current > 0 && !slots[current-1].active {
		current = 0
	}
	want, free, restart := poolPlan(cfg.MaxSlots, v, current)
	currentFree = free
	var show, start, hide []*Slot
	var renew *Slot
	for _, s := range slots {
		switch {
		case want[s.N] && !s.active:
			s.active, s.state, s.color = true, "starting", "gray"
			show = append(show, s)
			if !s.running { // if the old supervisor hasn't exited yet, it carries on
				s.running = true
				start = append(start, s)
			}
		case want[s.N] && !s.running && now.Sub(s.failed) > 10*time.Second:
			s.running = true // UxPlay failed to start last time: try again
			start = append(start, s)
		case !want[s.N] && s.active && s.idleAt(now):
			s.active, s.line = false, "" // empty line: updateMenu rewrites it next time it comes up
			hide = append(hide, s)
		}
	}
	if restart && free > 0 {
		renew = slots[free-1]
		requestRestartLocked(renew)
	}
	exe := uxplayExe
	mu.Unlock()

	for _, s := range show {
		logf("slot %d: up", s.N)
		updateMenu(s)
		s.item.Show()
	}
	for _, s := range start {
		go supervise(s, exe)
	}
	for _, s := range hide {
		logf("slot %d: down, not needed", s.N)
		s.item.Hide()
		requestRestart(s) // the supervisor sees !active and exits
	}
	if renew != nil {
		logf("slot %d: session over; restarting it for the next device", renew.N)
	}
}

// syncChecks marks the Resolution and FPS items that match cfg.
func syncChecks() {
	mu.Lock()
	defer mu.Unlock()
	for k, it := range resItems {
		if k == cfg.Resolution {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
	for k, it := range fpsItems {
		if k == cfg.FPS {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
}

// setResolution and setFPS save the choice. They don't kill anything: watchWindows restarts each
// instance with the new args as soon as nobody is mirroring on it (idle ones within a few seconds),
// so a misclick never drops a live device; a live one shows "… on reconnect" and offers Apply now.
// Instances stopped by a crash loop start again.
func setResolution(r string) {
	mu.Lock()
	same := cfg.Resolution == r
	cfg.Resolution = r
	saveConfig()
	if !same {
		resumeCrashedLocked()
	}
	mu.Unlock()
	syncChecks()
	if !same {
		logf("resolution -> %s; applies to each instance when nobody is mirroring on it, or with Apply now", r)
		refreshMenus()
	}
}

func setFPS(f int) {
	mu.Lock()
	same := cfg.FPS == f
	cfg.FPS = f
	saveConfig()
	if !same {
		resumeCrashedLocked()
	}
	mu.Unlock()
	syncChecks()
	if !same {
		logf("fps -> %d; applies to each instance when nobody is mirroring on it, or with Apply now", f)
		refreshMenus()
	}
}

func refreshMenus() {
	for _, s := range slots {
		updateMenu(s)
	}
}

// applyNow restarts one live instance right away with the new settings; the device mirrors again
// to LIVE. Only offered while a change is waiting for it.
func applyNow(s *Slot) {
	mu.Lock()
	note, who := pendingNote(s), s.title
	if note != "" {
		requestRestartLocked(s)
	}
	mu.Unlock()
	if note != "" {
		logf("slot %d: Apply now: restarting %s's instance with the new settings", s.N, who)
	}
}

// forgetDevice removes the instance's device from the remembered devices (its window title is
// then free for another device). The session in progress carries on.
func forgetDevice(s *Slot) {
	mu.Lock()
	id := s.deviceID
	d, ok := cfg.Devices[id]
	if ok {
		delete(cfg.Devices, id)
		saveConfig()
	}
	mu.Unlock()
	if ok {
		logf("device forgotten: %s, %s", d.Name, id)
	}
	updateMenu(s)
}

// requestRestartLocked kills the instance's UxPlay; the supervisor starts another with the current
// config. Call with mu held.
func requestRestartLocked(s *Slot) {
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
}

func requestRestart(s *Slot) {
	mu.Lock()
	requestRestartLocked(s)
	mu.Unlock()
}

// resumeCrashedLocked starts again the instances stopped by a crash loop: the change may be the
// fix. Call with mu held.
func resumeCrashedLocked() {
	crashWarned = false
	for _, s := range slots {
		s.crashedOut = false
		if s.active && s.crashStopped {
			requestRestartLocked(s)
		}
	}
}

func restartAll() {
	mu.Lock()
	crashWarned = false
	for _, s := range slots {
		s.crashedOut = false
		if s.active {
			requestRestartLocked(s)
		}
	}
	mu.Unlock()
}

// slotMAC gives each instance a fixed MAC of its own (AirPlay uses it as the receiver's deviceID):
// two instances with the same MAC would be one receiver to the device. Fixed per PC and instance.
// Locally administered bit.
func slotMAC(n int) string {
	h := sha256.Sum256([]byte("iPhoneScreenshare|" + hostname + "|" + strconv.Itoa(n)))
	b := h[:6]
	b[0] = (b[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// slotArgs builds the instance's UxPlay command line from the current config; call with mu held.
// The order is always the same (watchWindows compares it with the running process).
func slotArgs(s *Slot) []string {
	// -n: the same name for every instance (only the free one is advertised). -p: each instance
	// with its own ports (n, n+1, n+2). -m: its own MAC. -vs d3d11videosink: the sink UxPlay
	// handles on Windows and CI tests (autovideosink would pick d3d12). -FPSdata: the client's
	// frame rate reports, for the tray. extraArgs come last, so they override.
	args := []string{"-n", cfg.Name, "-nh", "-p", strconv.Itoa(35000 + 10*s.N), "-m", slotMAC(s.N), "-vs", "d3d11videosink"}
	args = append(args, resolutions[cfg.Resolution]...)
	args = append(args, "-fps", strconv.Itoa(cfg.FPS), "-FPSdata")
	return append(args, cfg.ExtraArgs...)
}

func containsAny(line string, parts []string) bool {
	for _, t := range parts {
		if strings.Contains(line, t) {
			return true
		}
	}
	return false
}

// processLine updates the instance with one line of UxPlay output. UxPlay logs the connection
// request before deciding whether to accept it; the device only counts (state, grace period, its
// window title) once the connection is accepted. Returns what to write to the app log, whether a
// device was accepted (the pool brings up the next free instance) and whether the raw line goes
// to the log (FPS reports after the first one of the session don't). Call with mu held.
func (s *Slot) processLine(line string, now time.Time) (msg string, accepted, keep bool) {
	// Inside a report, a line that isn't XML is another thread's (UxPlay logs from several): it is
	// handled as usual, so a report cut short never hides the session's lines.
	xml := strings.HasPrefix(strings.TrimSpace(line), "<")
	if (s.plist != nil && xml) || strings.Contains(line, "<?xml") || strings.Contains(line, "<plist") {
		keep = !s.plistShown
		s.plist = append(s.plist, line)
		if strings.Contains(line, "</plist>") || len(s.plist) > 1000 {
			fps, ok := parseFPSReport(strings.Join(s.plist, "\n"))
			first := !s.plistShown
			s.plist, s.plistShown, s.dropBlank = nil, true, !keep
			switch {
			case ok:
				s.fps = fps
				if first {
					msg = fmt.Sprintf("slot %d: client reports %.0f fps (later reports are left out of the log)", s.N, fps)
				}
			case first:
				msg = fmt.Sprintf("slot %d: client FPS report without a frame rate; later reports are left out of the log", s.N)
			}
		}
		return msg, false, keep
	}
	if s.dropBlank {
		s.dropBlank = false
		if strings.TrimSpace(line) == "" {
			return "", false, false
		}
	}
	keep = true
	switch {
	case strings.Contains(line, "privacy: video paused"):
		if !s.mirroring || s.asleep {
			return "", false, true
		}
		s.asleep = true
		return fmt.Sprintf("slot %d: %s screen off: window black", s.N, s.title), false, true
	case strings.Contains(line, "privacy: video resumed"):
		if !s.asleep {
			return "", false, true
		}
		s.asleep, s.wokeAt = false, now
		return fmt.Sprintf("slot %d: %s screen on: window black for %d s more (lock screen)", s.N, s.title, cfg.WakeBlack), false, true
	case strings.Contains(line, "mDNS: hidden while mirroring"):
		s.hidden = true
		return fmt.Sprintf("slot %d: out of Screen Mirroring while %s mirrors", s.N, s.title), false, true
	case strings.Contains(line, "mDNS: advertised at startup"):
		s.ready, s.announced = true, now
		return fmt.Sprintf("slot %d: in Screen Mirroring", s.N), false, true
	case strings.Contains(line, "mDNS: announced again"):
		// One of the patch's repeats (+1, +3 and +8 s after each announcement): the devices'
		// caches hold the instance whose packet came last, so poolPlan compares this time.
		if !s.hidden {
			s.announced = now
		}
		return "", false, true
	case strings.Contains(line, "mDNS: mirroring ended"):
		// The mirror thread ended (the most reliable stop point, whatever the reason): the session
		// is over even if no other line said so.
		var notes []string
		if s.mirroring {
			s.mirroring, s.ended = false, now
			s.state, s.color = "stopped: mirror again on the device", "yellow"
			notes = append(notes, s.title+" stopped mirroring")
		}
		if strings.Contains(line, "advertised again") {
			// Back in Screen Mirroring, and the devices now resolve LIVE to this instance (it
			// announced last): it is free for the next device, the same one or another, without a
			// restart; the pool takes the other idle one down. The device and title stay: a frozen
			// window UxPlay may leave open keeps its name, and if that device reconnects on
			// another instance, accept restarts this one so there is no duplicate.
			s.hidden, s.ready, s.announced, s.readvertised = false, true, now, true
			s.fps, s.width, s.height, s.codec = 0, 0, 0, ""
			s.state, s.color = "waiting", "blue"
			notes = append(notes, "back in Screen Mirroring")
			if s.pending || !slices.Equal(slotArgs(s), s.args) {
				// New settings wait for this instance ("SD on reconnect"): it announced last, so a
				// device that reconnects now gets it. Restart at once, while nobody is connected,
				// instead of after the grace period (watchWindows), which would leave the old args
				// for the next device or drop its handshake. The ports stay, so caches stay valid.
				s.pending = false
				requestRestartLocked(s)
				notes = append(notes, "restarting to apply the config")
			}
		} else {
			s.hidden = true // announcing again failed: the instance restarts before serving another device
		}
		if notes == nil {
			return "", false, true
		}
		return fmt.Sprintf("slot %d: %s", s.N, strings.Join(notes, "; ")), false, true
	}
	if m := reSocket.FindStringSubmatch(line); m != nil {
		s.newSock = m[1]
		return "", false, true
	}
	if codec, w, h, ok := parseGeometry(line); ok {
		if w == s.width && h == s.height && codec == s.codec {
			return "", false, true
		}
		if w != s.width || h != s.height {
			s.geometryAt = now
		}
		s.width, s.height, s.codec = w, h, codec
		return fmt.Sprintf("slot %d: video %s %s", s.N, codec, orientation(w, h)), false, true
	}
	if m := reConnection.FindStringSubmatch(line); m != nil {
		s.candName, s.candID, s.candSock, s.candAt = m[1], m[3], s.newSock, now
		return "", false, true
	}
	if s.candID != "" && containsAny(line, refusals) {
		msg = fmt.Sprintf("slot %d: refused %s, %s", s.N, s.candName, s.candID)
		s.candName, s.candID = "", ""
		return msg, false, true
	}
	if s.candID != "" && containsAny(line, []string{"Client identified as", "starting mirroring", "Begin streaming"}) {
		msg, accepted = s.accept(now), true
	}
	if strings.Contains(line, "Begin streaming to GStreamer video pipeline") {
		s.mirroring, s.videoStart = true, now
		s.state, s.color = "live", "green"
		return msg, accepted, true
	}
	if s.mirroring {
		m := reClosed.FindStringSubmatch(line)
		if containsAny(line, endings) || (m != nil && m[1] == s.sock) {
			s.mirroring, s.ended = false, now
			s.state, s.color = "stopped: mirror again on the device", "yellow"
			return fmt.Sprintf("slot %d: %s stopped mirroring", s.N, s.title), accepted, true
		}
	}
	return msg, accepted, true
}

// accept takes the device that asked to connect: it remembers the device, gives the session its
// window title and, if the same device still has an older session on another instance (it
// reconnected after a network blip, and that window is frozen), restarts that instance at once.
// Call with mu held.
func (s *Slot) accept(now time.Time) string {
	device, id := s.candName, s.candID
	s.candName, s.candID = "", ""
	var notes []string
	for _, o := range slots {
		if o != s && o.cmd != nil && o.deviceID == id {
			if o.mirroring {
				notes = append(notes, fmt.Sprintf("; its old session on slot %d is restarted", o.N))
			}
			// The window is going away: its title goes to the new session. Its session is over now
			// (not when the process is gone), so the pool takes the restarting instance as the
			// free one instead of starting another.
			o.title, o.mirroring, o.ready = "", false, false
			o.connected, o.ended = time.Time{}, time.Time{}
			o.state, o.color = "restarting", "gray"
			requestRestartLocked(o)
		}
	}
	s.sock, s.device, s.deviceID, s.connected, s.readvertised = s.candSock, device, id, now, false
	s.width, s.height, s.codec, s.fps, s.plistShown = 0, 0, "", 0, false
	s.asleep, s.wokeAt = false, time.Time{}
	s.state, s.color = "connecting", "yellow"
	open := map[string]string{}
	for _, o := range slots {
		if o != s && o.cmd != nil && o.deviceID != "" && o.title != "" {
			open[o.title] = o.deviceID
		}
	}
	d, known := cfg.Devices[id]
	s.title = pickTitle(id, displayName(id, device), cfg.Devices, open)
	n := d
	if strings.TrimSpace(device) != "" {
		n.Name = device
	}
	n.Window = s.title
	if !known || n != d {
		cfg.Devices[id] = n
		saveConfig()
	}
	return fmt.Sprintf("slot %d: %s connected, deviceID %s, window %q%s", s.N, device, id, s.title, strings.Join(notes, ""))
}

// readOutput copies UxPlay's output to the log and tracks connections, mirroring and the client's
// frame rate. It closes done when finished: the supervisor waits for it, so no line from the old
// process lands in the new process's state.
func readOutput(s *Slot, r io.Reader, done chan<- struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		mu.Lock()
		before := [6]any{s.state, s.width, s.height, s.codec, math.Round(s.fps), s.title}
		size, wasPrivate := [2]int{s.width, s.height}, s.privateAt(time.Now())
		msg, accepted, keep := s.processLine(line, time.Now())
		changed := before != [6]any{s.state, s.width, s.height, s.codec, math.Round(s.fps), s.title}
		resized := size != [2]int{s.width, s.height} && s.width > 0 && s.height > 0
		hide := !wasPrivate && s.privateAt(time.Now())
		mu.Unlock()
		if hide {
			curtainSlot(s) // right away; watchWindows also keeps it every second
		}
		if keep {
			fmt.Fprintf(logFile, "[slot %d] %s\n", s.N, line)
		}
		if resized {
			fitSoon(s) // rotation: fit as soon as the geometry settles, not on the next tick
		}
		if msg != "" {
			logf("%s", msg)
		}
		if changed || accepted || hide {
			updateMenu(s)
		}
		if accepted {
			adjustSlots() // brings up the next free instance
			refreshMenus()
		}
	}
}

// uxplayCmd builds the UxPlay process. Only CREATE_NO_WINDOW (hides the console). Not HideWindow:
// Windows applies the STARTUPINFO SW_HIDE to the process's first ShowWindow, which is the video
// window, and OBS gets nothing.
func uxplayCmd(exe string, args []string) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe) // GStreamer DLLs sit next to uxplay.exe
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func supervise(s *Slot, exe string) {
	quickCrashes := 0
	for {
		select { // a restart requested while nothing was running is served by this start
		case <-s.restartCh:
		default:
		}
		mu.Lock()
		if quitting || !s.active {
			s.running, s.cmd, s.args = false, nil, nil
			mu.Unlock()
			return
		}
		args := slotArgs(s)
		s.pending, s.hidden, s.ready, s.readvertised, s.argsRes, s.argsFPS = false, false, false, false, cfg.Resolution, cfg.FPS

		s.device, s.deviceID, s.title, s.fps, s.plist, s.plistShown, s.dropBlank = "", "", "", 0, nil, false, false
		s.connected, s.ended, s.videoStart = time.Time{}, time.Time{}, time.Time{}
		s.mirroring, s.candID, s.candName, s.sock, s.newSock = false, "", "", "", ""
		s.width, s.height, s.codec = 0, 0, ""
		mu.Unlock()

		cmd := uxplayCmd(exe, args)
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw

		logf("slot %d: starting %s %s", s.N, exe, strings.Join(args, " "))
		if err := cmd.Start(); err != nil {
			logf("slot %d: ERROR starting: %v", s.N, err)
			pw.Close()
			mu.Lock()
			s.running, s.cmd, s.args, s.failed = false, nil, nil, time.Now() // adjustSlots retries
			mu.Unlock()
			setState(s, "start error, retrying in 10 s: see the log", "red")
			return
		}
		done := make(chan struct{})
		go readOutput(s, pr, done)
		mu.Lock()
		s.cmd, s.args, s.started = cmd, args, time.Now()
		s.state, s.color = "waiting", "blue"
		// A quit/stop/restart request that arrived while the process was starting hit the previous
		// (already dead) process: serve it now, or this UxPlay would keep the old config.
		if quitting || !s.active || len(s.restartCh) > 0 {
			cmd.Process.Kill()
		}
		mu.Unlock()
		updateMenu(s)

		err := cmd.Wait()
		pw.Close()
		<-done // process the lines still buffered before touching the state
		mu.Lock()
		// Process gone: without cmd, watchWindows won't rename windows of a PID Windows may reuse,
		// and the slot counts as idle (nothing to drop).
		s.mirroring, s.cmd, s.args = false, nil, nil
		exit, started := quitting || !s.active, s.started
		if exit {
			s.running = false
		}
		mu.Unlock()
		if exit {
			return
		}

		requested := false
		select {
		case <-s.restartCh:
			requested = true
		default:
		}
		if requested {
			quickCrashes = 0
			continue
		}

		logf("slot %d: UxPlay crashed: %v, after %s", s.N, err, time.Since(started).Round(time.Second))
		if time.Since(started) < 10*time.Second {
			quickCrashes++
		} else {
			quickCrashes = 0
		}
		if quickCrashes >= 3 {
			mu.Lock()
			s.crashStopped, s.crashedOut = true, true
			first := !crashWarned // one popup for all the slots that stop for the same cause
			crashWarned = true
			mu.Unlock()
			setState(s, "stopped: crashed 3 times, see the log", "red")
			if first {
				go warn(fmt.Sprintf("UxPlay for slot %d crashed 3 times in a row right after starting.\n"+
					"Use Open log in the tray menu to see why, then change Resolution or FPS, or use Restart.", s.N))
			}
			<-s.restartCh
			mu.Lock()
			s.crashStopped = false
			mu.Unlock()
			quickCrashes = 0
			continue
		}
		setState(s, "reconnecting", "yellow")
		time.Sleep(2 * time.Second)
	}
}

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	sendMessageTimeout    = user32.NewProc("SendMessageTimeoutW")
	internalGetWindowText = user32.NewProc("InternalGetWindowText")
	enumMu                sync.Mutex // one EnumWindows at a time (windowsByPID)
	fitMu                 sync.Mutex // guards Slot.fitted and transparent; one fit at a time
	windowsByPID          map[uint32][]windows.HWND
	enumWindowCallback    = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		var pid uint32
		windows.GetWindowThreadProcessId(h, &pid)
		if windows.IsWindowVisible(h) && isVideoWindow(h) {
			windowsByPID[pid] = append(windowsByPID[pid], h)
		}
		return 1
	})
)

// watchWindows gives each video window the title of its session, the device's name (OBS finds the
// source by title), flags video that arrived without a window and restarts idle instances whose
// args changed (config) or that are pending (network changed).
func watchWindows() {
	for range time.Tick(time.Second) {
		byPID := visibleWindows()
		revealStale()
		adjustSlots()
		for _, s := range slots {
			mu.Lock()
			cmd, active, title := s.cmd, s.active, s.title
			mu.Unlock()
			if !active || cmd == nil || cmd.Process == nil {
				continue
			}
			wins := byPID[uint32(cmd.Process.Pid)]
			for _, h := range wins {
				if title != "" && windowTitle(h) != title {
					setWindowTitle(h, title)
				}
			}
			fitWindows(s, wins, time.Now())
			mu.Lock()
			private := s.privateAt(time.Now())
			mu.Unlock()
			for _, h := range wins {
				if isVideoWindow(h) {
					setCurtain(h, private)
				}
			}
			mu.Lock()
			now := time.Now()
			switch {
			case s.mirroring && len(wins) == 0 && now.Sub(s.videoStart) > 5*time.Second:
				s.state, s.color = "receiving video but no window: see the log", "yellow"
			case s.mirroring:
				s.state, s.color = "live", "green"
			}
			idle := s.idleAt(now)
			if idle && s.state == "connecting" {
				s.state, s.color = "waiting", "blue" // accepted, but the video never came
			}
			restart := s.cmd == cmd && idle && now.Sub(s.started) > 5*time.Second &&
				(s.pending || !slices.Equal(slotArgs(s), s.args))
			if restart {
				s.pending = false
				requestRestartLocked(s)
			}
			mu.Unlock()
			if restart {
				logf("slot %d: applying the config, nobody mirroring", s.N)
			}
			updateMenu(s)
		}
	}
}

// visibleWindows lists the visible top-level windows by process ID.
func visibleWindows() map[uint32][]windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	windowsByPID = map[uint32][]windows.HWND{}
	windows.EnumWindows(enumWindowCallback, nil)
	return windowsByPID
}

// fitSoon fits the slot's windows to a new geometry (rotation) as soon as it has settled, instead
// of waiting for watchWindows' next tick.
func fitSoon(s *Slot) {
	mu.Lock()
	wait := time.Until(s.geometryAt.Add(time.Second))
	mu.Unlock()
	time.AfterFunc(max(wait, 0), func() {
		mu.Lock()
		cmd, active := s.cmd, s.active
		mu.Unlock()
		if active && cmd != nil && cmd.Process != nil {
			fitWindows(s, visibleWindows()[uint32(cmd.Process.Pid)], time.Now())
		}
	})
}

var (
	setWinEventHook  = user32.NewProc("SetWinEventHook")
	getAncestor      = user32.NewProc("GetAncestor")
	getDesktopWindow = user32.NewProc("GetDesktopWindow")
	getMessage       = user32.NewProc("GetMessageW")
	translateMessage = user32.NewProc("TranslateMessage")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
)

const (
	eventObjectCreate = 0x8000
	eventObjectShow   = 0x8002
)

// watchNewWindows hears from Windows as soon as a UxPlay window is created or shown and gives it
// its session's title and the video's shape right then (onWindowEvent), so the window isn't seen at
// the sink's size and resized afterwards. The hook is out of context (no DLL in UxPlay): its events
// arrive on this thread's message loop. watchWindows stays as the fallback.
func watchNewWindows() {
	const wineventOutOfContext, wineventSkipOwnProcess = 0, 2
	runtime.LockOSThread() // the hook belongs to this thread and its message loop
	cb := windows.NewCallback(func(_, event, hwnd, idObject, idChild, _, _ uintptr) uintptr {
		// OBJID_WINDOW and CHILDID_SELF: the window itself, not a part of it
		if (event == eventObjectCreate || event == eventObjectShow) && int32(idObject) == 0 && int32(idChild) == 0 {
			onWindowEvent(windows.HWND(hwnd), event == eventObjectShow)
		}
		return 0
	})
	h, _, err := setWinEventHook.Call(eventObjectCreate, eventObjectShow, 0, cb, 0, 0,
		wineventOutOfContext|wineventSkipOwnProcess)
	if h == 0 {
		logf("window events unavailable: %v; video windows are fitted every second", err)
		return
	}
	var m struct {
		Hwnd    windows.HWND
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      [2]int32
	}
	for {
		if r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
			return
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// onWindowEvent handles a window just created (still hidden) or shown. Only the video sink's own
// window (isVideoWindow) of an instance's UxPlay is touched: it gets the session's title. The video
// sink resizes its window to the full video size right before it shows it, which undoes any fit
// made earlier, and the show event arrives once the window is already on screen. So when the
// geometry is already known (UxPlay logs it before the window exists), the sink's window is made
// transparent while it is still hidden, fitted when it is shown, and made opaque once the fit has
// landed: it is first seen in shape.
func onWindowEvent(h windows.HWND, shown bool) {
	const gwlStyle, wsCaption = ^uintptr(15), 0x00C00000 // -16
	parent, _, _ := getAncestor.Call(uintptr(h), 1)      // GA_PARENT
	if desk, _, _ := getDesktopWindow.Call(); parent != desk {
		return
	}
	var pid uint32
	windows.GetWindowThreadProcessId(h, &pid)
	var s *Slot
	var video [2]int
	var title string
	var private bool
	mu.Lock()
	for _, o := range slots {
		if o.active && o.cmd != nil && o.cmd.Process != nil && uint32(o.cmd.Process.Pid) == pid {
			s, video, title, private = o, [2]int{o.width, o.height}, o.title, o.privateAt(time.Now())
		}
	}
	mu.Unlock()
	if s == nil || !isVideoWindow(h) {
		return // tooltips (the close button's), IME and tool windows are left alone
	}
	if shown && private {
		setCurtain(h, true) // before any reveal below: it keeps a curtained window transparent
	}
	if style, _, _ := getWindowLongPtr.Call(uintptr(h), gwlStyle); style&wsCaption == 0 {
		if shown {
			revealWindow(h) // went full screen before its show event was handled
		}
		return // already full screen
	}
	if title != "" && windowTitle(h) != title {
		setWindowTitle(h, title)
	}
	known := video[0] > 0 && video[1] > 0
	if !shown {
		if known && !windows.IsWindowVisible(h) {
			hideWindow(h)
		}
		return
	}
	if !known {
		revealWindow(h) // fitted when the geometry arrives (fitSoon)
		return
	}
	fitMu.Lock()
	res, retry, target := fitVideoWindowRect(h, video[0], video[1])
	if !retry {
		if s.fitted == nil {
			s.fitted = map[windows.HWND][2]int{}
		}
		s.fitted[h] = video
	}
	fitMu.Unlock()
	if res != "" {
		logf("slot %d: window %s for video %s", s.N, res, orientation(video[0], video[1]))
	}
	go func() {
		if target != (winRect{}) {
			waitWindowRect(h, target, 500*time.Millisecond) // the move is asynchronous
		}
		revealWindow(h)
	}()
}

// videoWindowClasses are the window classes of the video sink the app runs UxPlay with
// (d3d11videosink, -vs in slotArgs). UxPlay's other windows (the close button's tooltip, IME
// windows) are not video, and neither are the windows of another sink set in extraArgs.
var videoWindowClasses = []string{"GSTD3D11"}

const (
	gwlExStyle   = ^uintptr(19) // -20
	wsExLayered  = 0x00080000
	wsExToolWin  = 0x00000080
	wsExNoActive = 0x08000000
)

// videoClass tells whether a window with this class and extended style is the video sink's own
// window: the only one that is fitted, renamed, hidden and revealed.
func videoClass(class string, ex uintptr) bool {
	return slices.Contains(videoWindowClasses, class) && ex&(wsExToolWin|wsExNoActive) == 0
}

func isVideoWindow(h windows.HWND) bool {
	ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
	return videoClass(windowClass(h), ex)
}

var (
	getClassName      = user32.NewProc("GetClassNameW")
	setWindowLongPtr  = user32.NewProc("SetWindowLongPtrW")
	setLayeredWinAttr = user32.NewProc("SetLayeredWindowAttributes")
	isWindow          = user32.NewProc("IsWindow")
	// transparent holds the video windows made transparent at creation, until their fit at show has
	// landed, and whether revealStale already saw them visible. Guarded by fitMu.
	transparent = map[windows.HWND]bool{}
	// curtained holds the video windows made transparent because their session is private
	// (privateAt). revealWindow leaves them transparent. Guarded by fitMu.
	curtained = map[windows.HWND]bool{}
)

func windowClass(h windows.HWND) string {
	buf := make([]uint16, 64)
	getClassName.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func windowExists(h windows.HWND) bool {
	r, _, _ := isWindow.Call(uintptr(h))
	return r != 0
}

// hideWindow makes a not yet visible window fully transparent (layered, alpha 0). The video sink
// keeps rendering into it; only the screen and captures don't show it until revealWindow. The
// style change waits for the window's thread, so fitMu is held only for the bookkeeping.
func hideWindow(h windows.HWND) {
	const lwaAlpha = 2
	ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
	if ex&wsExLayered != 0 {
		return // layered by its owner: leave it alone
	}
	setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex|wsExLayered)
	if r, _, _ := setLayeredWinAttr.Call(uintptr(h), 0, 0, lwaAlpha); r == 0 {
		setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex)
		return
	}
	fitMu.Lock()
	transparent[h] = false
	fitMu.Unlock()
}

// revealWindow undoes hideWindow, if it hid the window and it isn't curtained.
func revealWindow(h windows.HWND) {
	fitMu.Lock()
	_, ok := transparent[h]
	delete(transparent, h)
	keep := curtained[h]
	fitMu.Unlock()
	if ok && !keep {
		ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
		setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex&^wsExLayered)
	}
}

// setCurtain makes a visible video window transparent (on) or shows it again (off), the same way
// hideWindow does: the video sink keeps rendering, the screen and OBS get nothing. A window still
// transparent from hideWindow only gets marked, and revealWindow then leaves it transparent.
func setCurtain(h windows.HWND, on bool) {
	const lwaAlpha = 2
	fitMu.Lock()
	was := curtained[h]
	_, fitting := transparent[h]
	if on {
		curtained[h] = true
	} else {
		delete(curtained, h)
	}
	fitMu.Unlock()
	if on == was || fitting {
		return
	}
	ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
	if !on {
		setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex&^wsExLayered)
		return
	}
	setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex|wsExLayered)
	if r, _, err := setLayeredWinAttr.Call(uintptr(h), 0, 0, lwaAlpha); r == 0 {
		fitMu.Lock()
		delete(curtained, h) // tried again on the next tick
		fitMu.Unlock()
		logf("could not black out window %q: %v", windowTitle(h), err)
	}
}

// curtainSlot applies privateAt to the slot's video windows now, instead of on watchWindows' next tick.
func curtainSlot(s *Slot) {
	mu.Lock()
	cmd, private := s.cmd, s.privateAt(time.Now())
	mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	for _, h := range visibleWindows()[uint32(cmd.Process.Pid)] {
		if isVideoWindow(h) {
			setCurtain(h, private)
		}
	}
}

// setBlackAll turns Black screen on or off: every video window hidden, whatever the session.
func setBlackAll(on bool, item *systray.MenuItem) {
	mu.Lock()
	blackAll = on
	mu.Unlock()
	if on {
		item.Check()
		logf("black screen on: every video window hidden")
	} else {
		item.Uncheck()
		logf("black screen off")
	}
	for _, s := range slots {
		curtainSlot(s)
		updateMenu(s)
	}
}

// revealStale reveals the transparent windows that were already visible on the previous call (a
// show event that never arrived) and forgets the ones that are gone. Called every second.
func revealStale() {
	var stale []windows.HWND
	fitMu.Lock()
	for h := range curtained {
		if !windowExists(h) {
			delete(curtained, h)
		}
	}
	for h, seen := range transparent {
		switch {
		case !windowExists(h):
			delete(transparent, h)
		case !windows.IsWindowVisible(h):
		case seen:
			stale = append(stale, h)
		default:
			transparent[h] = true
		}
	}
	fitMu.Unlock()
	for _, h := range stale {
		go revealWindow(h) // the style change waits for the window's thread, which may be hung
	}
}

// waitWindowRect waits until the window's rect, in its own DPI context, is r, or the timeout.
func waitWindowRect(h windows.HWND, r winRect, timeout time.Duration) {
	defer useWindowDPI(h)()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		var cur winRect
		if ok, _, _ := getWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&cur))); ok == 0 || cur == r {
			return
		}
	}
}

// windowTitle reads the title without sending a message to the window (won't hang if it's busy).
func windowTitle(h windows.HWND) string {
	buf := make([]uint16, 256)
	internalGetWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

// setWindowTitle changes the title with a timeout: the window belongs to another process and its
// thread may be stuck in a GStreamer reset.
func setWindowTitle(h windows.HWND, t string) {
	const wmSetText, smtoAbortIfHung = 0x000C, 0x0002
	var res uintptr
	sendMessageTimeout.Call(uintptr(h), wmSetText, 0, uintptr(unsafe.Pointer(ptr(t))),
		smtoAbortIfHung, 200, uintptr(unsafe.Pointer(&res)))
}

// fitWindows fits each of the slot's video windows to the video when the window is first seen with
// a known geometry and again on each new geometry (rotation), and leaves manual resizes alone in
// between. UxPlay logs the geometry before the frame is decoded and the video sink may still resize
// its window to the new caps, so a geometry is only fitted once it is a second old (the sink's own
// resize is done by then). wins are all the visible windows of the slot's UxPlay. Called by
// watchWindows and fitSoon; a window that shows up is fitted by onWindowEvent.
func fitWindows(s *Slot, wins []windows.HWND, now time.Time) {
	fitMu.Lock()
	defer fitMu.Unlock()
	mu.Lock()
	video := [2]int{s.width, s.height}
	settling := now.Sub(s.geometryAt) < time.Second
	mu.Unlock()
	seen := map[windows.HWND][2]int{}
	for _, h := range wins {
		done, ok := s.fitted[h]
		if video[0] <= 0 || video[1] <= 0 || settling || (ok && done == video) {
			if ok {
				seen[h] = done
			}
			continue
		}
		res, retry := fitVideoWindow(h, video[0], video[1])
		if !retry {
			seen[h] = video
		}
		if res != "" {
			logf("slot %d: window %s for video %s", s.N, res, orientation(video[0], video[1]))
		}
	}
	for h, done := range s.fitted {
		if _, ok := seen[h]; !ok && windowExists(h) {
			seen[h] = done // not in wins yet (shown and fitted after wins was listed)
		}
	}
	s.fitted = seen // windows that are gone are forgotten (a new window is fitted again)
}

// winRect is a Win32 RECT.
type winRect struct{ Left, Top, Right, Bottom int32 }

func (r winRect) w() int { return int(r.Right - r.Left) }
func (r winRect) h() int { return int(r.Bottom - r.Top) }

// fitClient gives the client size for a vw×vh video: the video's aspect ratio, as large as fits in
// maxW×maxH, but never larger than the video itself.
func fitClient(vw, vh, maxW, maxH int) (int, int) {
	maxW, maxH = max(maxW, 1), max(maxH, 1)
	switch {
	case vw <= maxW && vh <= maxH:
		return vw, vh
	case vw*maxH >= vh*maxW: // limited by the width
		return maxW, max((maxW*vh+vw/2)/vw, 1)
	default:
		return max((maxH*vw+vh/2)/vh, 1), maxH
	}
}

// fitWindow gives the window rect whose client area fits a vw×vh video in 90% of the work area.
// frameW/frameH are what the borders and title bar add to the client size. The window keeps its
// top-left corner if the new size fits there, otherwise it moves inside the work area.
func fitWindow(vw, vh, frameW, frameH int, work, cur winRect) winRect {
	cw, ch := fitClient(vw, vh, work.w()*9/10-frameW, work.h()*9/10-frameH)
	w, h := int32(cw+frameW), int32(ch+frameH)
	x := min(max(cur.Left, work.Left), work.Right-w)
	y := min(max(cur.Top, work.Top), work.Bottom-h)
	return winRect{x, y, x + w, y + h}
}

var (
	getWindowLongPtr             = user32.NewProc("GetWindowLongPtrW")
	adjustWindowRectEx           = user32.NewProc("AdjustWindowRectEx")
	adjustWindowRectExForDpi     = user32.NewProc("AdjustWindowRectExForDpi")
	getDpiForWindow              = user32.NewProc("GetDpiForWindow")
	setThreadDpiAwarenessContext = user32.NewProc("SetThreadDpiAwarenessContext")
	getWindowDpiAwarenessContext = user32.NewProc("GetWindowDpiAwarenessContext")
	monitorFromWindow            = user32.NewProc("MonitorFromWindow")
	getMonitorInfo               = user32.NewProc("GetMonitorInfoW")
	getWindowRect                = user32.NewProc("GetWindowRect")
	setWindowPos                 = user32.NewProc("SetWindowPos")
	isIconic                     = user32.NewProc("IsIconic")
	isZoomed                     = user32.NewProc("IsZoomed")
)

// fitVideoWindow resizes a video window (of another process) so its client area fits the video,
// on the monitor it is on. It never sends the window a message that waits: a window stuck in a
// GStreamer reset doesn't block the app. Returns what it did for the log (empty if nothing) and
// whether to try again later (minimized). Maximized or full screen (Alt+Enter) is the user's
// choice and is left alone.
func fitVideoWindow(h windows.HWND, vw, vh int) (string, bool) {
	res, retry, _ := fitVideoWindowRect(h, vw, vh)
	return res, retry
}

// useWindowDPI switches the calling thread to the window's DPI awareness context, until the
// returned function is called. UxPlay's window is DPI unaware, so on a scaled display its
// borders, its rect, the work area and the move are then all in the same logical units, which are
// also the units the video sink renders in (the "never larger than the video" cap is in these
// units too).
func useWindowDPI(h windows.HWND) func() {
	runtime.LockOSThread()
	if getWindowDpiAwarenessContext.Find() == nil && setThreadDpiAwarenessContext.Find() == nil {
		if ctx, _, _ := getWindowDpiAwarenessContext.Call(uintptr(h)); ctx != 0 {
			if old, _, _ := setThreadDpiAwarenessContext.Call(ctx); old != 0 {
				return func() {
					setThreadDpiAwarenessContext.Call(old)
					runtime.UnlockOSThread()
				}
			}
		}
	}
	return runtime.UnlockOSThread
}

// fitVideoWindowRect is fitVideoWindow that also gives the rect the window is being moved to, in
// the window's DPI context (zero if it isn't moved). The move is asynchronous.
func fitVideoWindowRect(h windows.HWND, vw, vh int) (string, bool, winRect) {
	const (
		gwlStyle                   = ^uintptr(15) // -16
		wsCaption                  = 0x00C00000
		monitorDefaultToNearest    = 2
		swpNoZOrder, swpNoActivate = 0x0004, 0x0010
		swpAsyncWindowPos          = 0x4000
	)
	if r, _, _ := isIconic.Call(uintptr(h)); r != 0 {
		return "", true, winRect{}
	}
	if r, _, _ := isZoomed.Call(uintptr(h)); r != 0 {
		return "", false, winRect{}
	}
	defer useWindowDPI(h)() // measure in the window's own DPI context
	style, _, _ := getWindowLongPtr.Call(uintptr(h), gwlStyle)
	ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
	if style&wsCaption == 0 {
		return "", false, winRect{} // full screen
	}
	var frame winRect // client 0×0 -> window rect: the borders' size
	dpi := uintptr(0)
	if getDpiForWindow.Find() == nil && adjustWindowRectExForDpi.Find() == nil {
		dpi, _, _ = getDpiForWindow.Call(uintptr(h))
	}
	if dpi != 0 {
		adjustWindowRectExForDpi.Call(uintptr(unsafe.Pointer(&frame)), style&0xFFFFFFFF, 0, ex&0xFFFFFFFF, dpi)
	} else {
		adjustWindowRectEx.Call(uintptr(unsafe.Pointer(&frame)), style&0xFFFFFFFF, 0, ex&0xFFFFFFFF)
	}
	mon, _, _ := monitorFromWindow.Call(uintptr(h), monitorDefaultToNearest)
	mi := struct {
		Size          uint32
		Monitor, Work winRect
		Flags         uint32
	}{Size: 40}
	var cur winRect
	if r, _, _ := getMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return "", true, winRect{}
	}
	if r, _, _ := getWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&cur))); r == 0 {
		return "", true, winRect{}
	}
	n := fitWindow(vw, vh, frame.w(), frame.h(), mi.Work, cur)
	if n == cur {
		return "", false, winRect{}
	}
	setWindowPos.Call(uintptr(h), 0, uintptr(n.Left), uintptr(n.Top), uintptr(n.w()), uintptr(n.h()),
		swpNoZOrder|swpNoActivate|swpAsyncWindowPos)
	return fmt.Sprintf("fitted: client %d×%d", n.w()-frame.w(), n.h()-frame.h()), false, n
}

// setup extracts the embedded UxPlay (once per version) and creates the firewall rule once.
func setup() (string, error) {
	exe := cfg.UxPlay
	if exe == "" {
		dir, err := extractBundle()
		if err != nil {
			return "", fmt.Errorf("Could not extract the embedded UxPlay: %v", err)
		}
		exe = filepath.Join(dir, "bin", "uxplay.exe")
		os.Setenv("PATH", filepath.Join(dir, "bin")+";"+os.Getenv("PATH"))
		os.Setenv("GST_PLUGIN_SYSTEM_PATH_1_0", filepath.Join(dir, "lib", "gstreamer-1.0"))
		os.Setenv("GST_PLUGIN_PATH_1_0", filepath.Join(dir, "lib", "gstreamer-1.0"))
		os.Setenv("GST_PLUGIN_SCANNER_1_0", filepath.Join(dir, "libexec", "gstreamer-1.0", "gst-plugin-scanner.exe"))
		os.Setenv("GST_REGISTRY_1_0", filepath.Join(dir, "registry.bin"))
		os.Setenv("FONTCONFIG_FILE", filepath.Join(dir, "etc", "fonts", "fonts.conf"))
	} else if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("The \"uxplay\" path in the config does not exist: %s\nClear it to use the embedded one.\nConfig: %s", exe, configPath)
	}
	logf("uxplay: %s", exe)

	// Any profile (event networks are often set to Public), but local subnet only.
	rule, _ := hidden("netsh", "advfirewall", "firewall", "show", "rule", "name="+firewallRule, "verbose").CombinedOutput()
	if !strings.Contains(strings.ToLower(string(rule)), strings.ToLower(exe)) {
		logf("creating/updating firewall rule, asks for admin")
		params := fmt.Sprintf(`/c netsh advfirewall firewall delete rule name="%[1]s" & `+
			`netsh advfirewall firewall delete rule name="%[3]s" & `+
			`netsh advfirewall firewall add rule name="%[1]s" dir=in action=allow program="%[2]s" profile=any remoteip=localsubnet`,
			firewallRule, exe, oldFirewallRule)
		windows.ShellExecute(0, ptr("runas"), ptr("cmd"), ptr(params), nil, windows.SW_HIDE)
	}
	return exe, nil
}

// hidden builds a console command that doesn't open a black window (the app is GUI).
func hidden(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func publicNetwork() bool {
	out, _ := hidden("powershell", "-NoProfile", "-Command", "(Get-NetConnectionProfile).NetworkCategory").Output()
	return strings.Contains(string(out), "Public")
}

// watchNetwork marks the slots for restart when the PC's IPs change (cable/Wi-Fi dropped and came
// back, new IP from the router) or when the PC resumes from sleep: the mDNS announcement stays
// tied to the old network and the device stops seeing the "LIVE". Only idle slots restart
// (watchWindows): live ones carry on, and if the network really dropped their session ends and
// the slot restarts right after.
func watchNetwork() {
	before, tick := localIPs(), time.Now().Round(0) // Round(0): wall clock, counts sleep time
	for range time.Tick(5 * time.Second) {
		now, ips := time.Now().Round(0), localIPs()
		reason := ""
		switch {
		case now.Sub(tick) > 30*time.Second:
			reason = "PC resumed from sleep"
		case ips != before && ips != "":
			reason = fmt.Sprintf("network changed: %s -> %s", before, ips)
		}
		if reason != "" {
			logf("%s; restarting the slots nobody is mirroring on", reason)
			mu.Lock()
			for _, s := range slots {
				s.pending = true
			}
			mu.Unlock()
		}
		if ips != "" {
			before = ips
		}
		tick = now
	}
}

func localIPs() string {
	var ips []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ips = append(ips, n.IP.String())
			}
		}
	}
	sort.Strings(ips)
	return strings.Join(ips, ",")
}

// extractBundle unzips the embedded zip into a folder named after its hash; if the folder is
// already complete (.ok file), it is reused. New app version = new folder.
func extractBundle() (string, error) {
	h := sha256.Sum256(bundleZip)
	dir := filepath.Join(appDir, "uxplay-"+hex.EncodeToString(h[:6]))
	ok := filepath.Join(dir, ".ok")
	if _, err := os.Stat(ok); err == nil {
		return dir, nil
	}
	logf("extracting UxPlay to %s", dir)
	os.RemoveAll(dir)
	zr, err := zip.NewReader(bytes.NewReader(bundleZip), int64(len(bundleZip)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		dest := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(dest, dir+string(os.PathSeparator)) {
			return "", fmt.Errorf("invalid path in zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(dest, 0o755)
			continue
		}
		if err := extractFile(f, dest); err != nil {
			return "", err
		}
	}
	return dir, os.WriteFile(ok, nil, 0o644)
}

func extractFile(f *zip.File, dest string) error {
	os.MkdirAll(filepath.Dir(dest), 0o755)
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = io.Copy(w, r)
	return err
}

func warn(msg string) {
	windows.MessageBox(0, ptr(msg), ptr(appTitle), windows.MB_OK|windows.MB_ICONWARNING)
}

func ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// icon draws a dot in the status color and wraps the PNG in an .ico (accepted since Vista).
func icon(c color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			if dx*dx+dy*dy <= 14*14 {
				img.Set(x, y, c)
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)

	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})                 // header: icon type, 1 image
	ico.Write([]byte{n, n, 0, 0})                                              // width, height, colors, reserved
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})                   // planes, bits
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(p.Len()), 6 + 16}) // size, offset
	ico.Write(p.Bytes())
	return ico.Bytes()
}
