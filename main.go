// iPhone Screenshare Windows Server: a Windows tray icon that keeps UxPlay (an AirPlay receiver)
// running so OBS can capture its window. UxPlay and GStreamer are embedded in this .exe
// (bundle/uxplay.zip, built by CI) and extracted on first run. It restarts UxPlay if it crashes and
// stops retrying if it crashes in a loop (bad flag, port in use), pointing to the log.
// Dynamic slots: there is always a free "LIVE N"; the device that connects to it is bound to the
// slot (window "Slot N" in OBS) by its deviceID and a new free slot comes up for the next device.
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
	slotLimit       = 8 // menu items created at startup; maxSlots in the config goes up to this
	appTitle        = "iPhone Screenshare"
)

//go:embed bundle/uxplay.zip
var bundleZip []byte

type Binding struct {
	DeviceID string `json:"deviceID"` // deviceID the device sends over AirPlay (UxPlay logs it)
	Device   string `json:"device"`   // device name, display only
}

type Config struct {
	Name       string             `json:"name"`       // name in Screen Mirroring: becomes "LIVE 1", "LIVE 2"...
	MaxSlots   int                `json:"maxSlots"`   // max devices at once (1 to 8); 1 = a single LIVE, no binding
	Resolution string             `json:"resolution"` // "native", "fhd", "hd" or "sd"
	FPS        int                `json:"fps"`        // 60 or 30
	UxPlay     string             `json:"uxplay"`     // path to another uxplay.exe; empty = the embedded one
	ExtraArgs  []string           `json:"extraArgs"`  // e.g. ["-reset", "30"]; added last, so they override the app's
	Bindings   map[string]Binding `json:"bindings"`   // "1" -> device bound to slot 1
}

// legacyConfig holds the Portuguese keys of earlier versions; they are migrated on load.
type legacyConfig struct {
	Nome       string   `json:"nome"`
	Slots      int      `json:"slots"`
	Qualidade  string   `json:"qualidade"`
	ArgsExtras []string `json:"argsExtras"`
	Vinculos   map[string]struct {
		DeviceID string `json:"deviceID"`
		Iphone   string `json:"iphone"`
	} `json:"vinculos"`
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
	currentFree  int               // slot currently serving as the free one (0 = none); computed in adjustSlots
	configBroken bool              // config.json on disk is broken: don't overwrite it until the user fixes it
	readFailed   bool              // the last read of config.json failed: don't save over the user's edits until one works
	unsaved      bool              // memory has changes config.json doesn't; keepConfig saves them
	saveGen      int               // bumped by every saveConfig, to know whether a write saved the latest
	diskCfg      = defaultConfig() // config as last read from or written to config.json (merge base)
	crashWarned  bool              // crash loop popup already shown; reset on Restart or a settings change
)

// Slot is a UxPlay of its own (name, ports, MAC and window "Slot N" in OBS). The first device that
// connects to a free slot is bound to it: the slot then only accepts that device (-restrict -allow)
// and the other slots refuse it (-block). So device X always comes back to slot X.
type Slot struct {
	N         int
	active    bool // should be running (bound, or the current free one)
	running   bool // a supervisor goroutine owns this slot
	cmd       *exec.Cmd
	args      []string  // args the current UxPlay started with (nil without a process)
	started   time.Time // when the current UxPlay started
	failed    time.Time // last start error; retried 10 s later
	restartCh chan struct{}
	state     string
	color     string

	newSock    string // socket of the last connection accepted by UxPlay's httpd
	candName   string // device that asked to connect and was not yet accepted or refused
	candID     string
	candSock   string
	sock       string    // socket of the accepted session
	device     string    // last accepted device
	connected  time.Time // when it was accepted
	mirroring  bool      // UxPlay is receiving video (from the log)
	videoStart time.Time // when video started
	ended      time.Time // when the last mirroring session ended
	width      int       // source video size from the last geometry line
	height     int
	codec      string    // "H.264" or "H.265", from the last geometry line
	geometryAt time.Time // when the geometry last changed
	pending    bool      // restart when idle even without arg changes (network changed)
	// With -nohold, a reconnecting device takes over the session still running; UxPlay may log the
	// end of the old session after the new one's "Begin streaming". takeover marks the switch and
	// endings without a socket that arrive before ignoreEndsUntil belong to the old session. The old
	// socket may also close first: sockEnded is when a session last ended by its socket, whose mirror
	// thread may still log its end after the next session starts.
	takeover        bool
	ignoreEndsUntil time.Time
	sockEnded       time.Time
	unbound         string // deviceID unbound while UxPlay was still running; not rebound until restart
	crashStopped    bool   // stopped after 3 quick crashes; waits for Restart or a settings change

	// fitted is the video size each of the slot's windows was last fitted to, so a window is fitted
	// when it shows up and on each new geometry, not in between. Guarded by fitMu.
	fitted map[windows.HWND][2]int

	title  string // last text set on the menu item (empty = forces SetTitle when shown)
	item   *systray.MenuItem
	unbind *systray.MenuItem
}

func defaultConfig() Config {
	return Config{Name: "LIVE", MaxSlots: slotLimit, Resolution: "native", FPS: 60, ExtraArgs: []string{}, Bindings: map[string]Binding{}}
}

func (s *Slot) key() string { return strconv.Itoa(s.N) }

// name is what shows in Screen Mirroring; with maxSlots 1 it is just "LIVE". Call with mu held.
func (s *Slot) name() string {
	if cfg.MaxSlots == 1 {
		return cfg.Name
	}
	return fmt.Sprintf("%s %d", cfg.Name, s.N)
}

func (s *Slot) window() string { return fmt.Sprintf("Slot %d", s.N) }

// idleAt tells whether the slot can be restarted or stopped without dropping anyone: nobody
// mirroring (from UxPlay's log), no device accepted in the last 20 s (video only starts after)
// and 5 s since the last session ended. Call with mu held.
func (s *Slot) idleAt(now time.Time) bool {
	return s.cmd == nil || (!s.mirroring && now.Sub(s.connected) > 20*time.Second && now.Sub(s.ended) > 5*time.Second)
}

// busyAt tells whether the slot is up with a session in progress (mirroring or in the grace
// period). Such a slot can't be the free one: UxPlay refuses a second device. Call with mu held.
func (s *Slot) busyAt(now time.Time) bool { return s.active && !s.idleAt(now) }

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
// a broken file (so the bindings are not lost), logs, and returns the popup text.
func startupConfigNotice(err error) string {
	const after = "\n\nThe app started with the defaults, without bindings, and will not overwrite the file. "
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

const migratedNote = "config: old keys migrated to English; original kept in config.json.v1"

// parseConfig decodes config.json over the defaults, migrates the old Portuguese keys and
// normalizes. It also returns what is worth logging (migration, ignored bindings).
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
	if !has("bindings") && has("vinculos") {
		n.Bindings, migrated = map[string]Binding{}, true
		for k, v := range old.Vinculos {
			n.Bindings[k] = Binding{DeviceID: v.DeviceID, Device: v.Iphone}
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
		// Old config: "slots": 1 was single mode (one LIVE, no binding); 2+ were fixed slots, which
		// are now dynamic (only bound slots plus one free run), so it goes to the limit.
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
	if n.ExtraArgs == nil {
		n.ExtraArgs = []string{}
	}
	if n.Bindings == nil {
		n.Bindings = map[string]Binding{}
	}
	for _, k := range sortedKeys(n.Bindings) {
		if num, err := strconv.Atoi(k); err != nil || num < 1 || num > n.MaxSlots || n.MaxSlots == 1 {
			notes = append(notes, fmt.Sprintf("config: binding of slot %s, %s, ignored: outside maxSlots %d",
				k, n.Bindings[k].Device, n.MaxSlots))
		}
	}
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
				notes = append(notes, "config: config.json was rewritten by an older version; settings and bindings restored from config.json.bak")
			}
		}
		// Older versions only read the old keys: keep the original for a downgrade.
		if _, err := os.Stat(configPath + ".v1"); errors.Is(err, fs.ErrNotExist) {
			os.WriteFile(configPath+".v1", b, 0o644)
		}
	}
	mu.Lock()
	onDisk := n
	onDisk.Bindings = maps.Clone(n.Bindings)
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
// of the English keys, so the settings come from config.json.bak, and the bindings it made are
// merged with the backup's (the file wins on a slot or device conflict).
func fromBackup(n Config, bak []byte) (Config, bool) {
	r, _, err := parseConfig(bak)
	if err != nil {
		return n, false
	}
	merged := maps.Clone(n.Bindings)
	mergeBindings(merged, r.Bindings, nil)
	r.Bindings = merged
	return r, true
}

// mergeMemory applies to n (just read from config.json) the changes made in the app since the
// disk last had base, which never reached the disk: Resolution, FPS, bindings and unbinds. What the
// file changed itself wins. Returns lines for the log. Call with mu held.
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
	kept, unbound := mergeBindings(n.Bindings, mem.Bindings, base.Bindings)
	for _, k := range kept {
		notes = append(notes, fmt.Sprintf("config: binding of slot %s, %s, kept: it was not saved yet", k, n.Bindings[k].Device))
	}
	for _, k := range unbound {
		notes = append(notes, fmt.Sprintf("config: slot %s kept unbound: the unbind was not saved yet", k))
	}
	return notes
}

// mergeBindings applies to file the binding changes in mem since base (bindings added or replaced,
// unbinds). A slot the file changed itself is left to the file, and so is a device the file binds
// to another slot. Returns the slots taken from mem and the slots unbound.
func mergeBindings(file, mem, base map[string]Binding) (kept, unbound []string) {
	orig := maps.Clone(file)
	untouched := func(k string) bool { // the file still has what the disk had
		a, inFile := orig[k]
		b, inBase := base[k]
		return inFile == inBase && a == b
	}
	for _, k := range sortedKeys(base) {
		if m, ok := mem[k]; (!ok || m != base[k]) && untouched(k) {
			delete(file, k)
			if !ok {
				unbound = append(unbound, k)
			}
		}
	}
	for _, k := range sortedKeys(mem) {
		b := mem[k]
		if x, ok := base[k]; (ok && x == b) || !untouched(k) {
			continue
		}
		taken := false
		for _, x := range file {
			taken = taken || x.DeviceID == b.DeviceID
		}
		if !taken {
			file[k] = b
			kept = append(kept, k)
		}
	}
	return kept, unbound
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
	updateStatus()
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
// broken or unreadable it doesn't write: they would lose what they typed (and the bindings). If the
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
	snap.Bindings = maps.Clone(cfg.Bindings)
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
				go warn("config.json can't be saved:\n" + err.Error() + "\n\nBindings and settings are kept in memory " +
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

func sortedKeys(v map[string]Binding) []string {
	ks := make([]string, 0, len(v))
	for k := range v {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// validBindings are the bindings that apply with the current maxSlots (none with 1 slot).
// Call with mu held.
func validBindings() map[string]Binding {
	v := map[string]Binding{}
	if cfg.MaxSlots == 1 {
		return v
	}
	for k, x := range cfg.Bindings {
		if n, err := strconv.Atoi(k); err == nil && n >= 1 && n <= cfg.MaxSlots {
			v[k] = x
		}
	}
	return v
}

func onReady() {
	systray.SetIcon(icon(colors["gray"]))
	systray.SetTooltip(appTitle)

	mStatus = systray.AddMenuItem("Preparing…", "")
	mStatus.Disable()
	systray.AddSeparator()
	// The menu can't insert items in the middle later: create every slot now and show the active ones.
	for n := 1; n <= slotLimit; n++ {
		s := &Slot{N: n, restartCh: make(chan struct{}, 1), state: "starting", color: "gray"}
		s.item = systray.AddMenuItem("", "")
		s.unbind = s.item.AddSubMenuItem("Unbind this slot", "")
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
			case <-mRestart.ClickedCh:
				if reloadConfig() { // a config that didn't load changes nothing: keep live slots
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
			for range s.unbind.ClickedCh {
				unbindSlot(s)
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
	updateStatus()
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

// slotTitle is the menu line of a slot, e.g. "LIVE 2 · Otavio's iPhone · live · H.265 · portrait 1320×2868".
// Call with mu held.
func slotTitle(s *Slot) (string, bool) {
	b, bound := validBindings()[s.key()]
	who := b.Device
	if !bound {
		switch {
		case s.mirroring || s.state == "connecting":
			who = s.device
		case cfg.MaxSlots == 1:
			who = "any device"
		default:
			who = "free"
		}
	}
	t := fmt.Sprintf("%s · %s · %s", s.name(), who, s.state)
	if o := orientation(s.width, s.height); o != "" && s.mirroring {
		if s.codec != "" {
			t += " · " + s.codec
		}
		t += " · " + o
	}
	return t, bound
}

// updateMenu rewrites the slot's line, the icon and the tray tooltip (only when they change).
func updateMenu(s *Slot) {
	mu.Lock()
	defer mu.Unlock()
	t, bound := slotTitle(s)
	// Slot not running: the item is hidden and any SetTitle/SetTooltip/Enable/Disable from systray
	// shows it again. When it comes up, adjustSlots calls this again with s.title empty.
	if k := fmt.Sprint(t, bound); s.active && k != s.title {
		s.title = k
		s.item.SetTitle(t)
		if bound {
			s.unbind.Enable()
		} else {
			s.unbind.Disable()
		}
	}

	// Icon: red if any slot has an error; else green if any is live; else yellow if any needs
	// attention (connecting, reconnecting, stopped); else blue (ready, waiting).
	failed, live, attention, someActive, free := false, 0, false, false, ""
	if currentFree > 0 {
		free = slots[currentFree-1].name()
	}
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
	tip := fmt.Sprintf("%s: %d live · free: %s", appTitle, live, free)
	switch {
	case cfg.MaxSlots == 1:
		tip = fmt.Sprintf("%s: %d live", appTitle, live)
	case free == "":
		tip = fmt.Sprintf("%s: %d live · all slots bound", appTitle, live)
	}
	if c != lastIcon {
		lastIcon = c
		systray.SetIcon(icon(colors[c]))
	}
	if tip != lastTooltip {
		lastTooltip = tip
		systray.SetTooltip(tip)
	}
}

func setState(s *Slot, state, c string) {
	mu.Lock()
	s.state, s.color = state, c
	mu.Unlock()
	updateMenu(s)
}

func updateStatus() {
	mu.Lock()
	n := len(validBindings())
	var t string
	switch {
	case cfg.MaxSlots == 1:
		t = "Single LIVE · any device"
	case n >= cfg.MaxSlots:
		t = fmt.Sprintf("All %d slots bound: unbind one to free a slot", cfg.MaxSlots)
	default:
		t = fmt.Sprintf("%d of %d slots bound", n, cfg.MaxSlots)
	}
	mu.Unlock()
	mStatus.SetTitle(t)
}

// wantedSlots says which slots should run: one per bound device plus a free one for the next
// device, up to max. It also returns the free one (0 if none). An unbound slot with a session in
// progress (busy; e.g. unbound while the device is still mirroring) can't be the free one: UxPlay
// would refuse the new device. The current free slot stays free while it qualifies, so nobody
// connecting to it is dropped (e.g. after unbinding a lower-numbered slot, which only goes down).
func wantedSlots(max int, v map[string]Binding, busy map[int]bool, current int) (map[int]bool, int) {
	want, free := map[int]bool{}, 0
	qualifies := func(n int) bool {
		_, bound := v[strconv.Itoa(n)]
		return n >= 1 && n <= max && n <= slotLimit && (!bound || max == 1) && !busy[n]
	}
	for n := 1; n <= max && n <= slotLimit; n++ {
		if _, ok := v[strconv.Itoa(n)]; ok && max > 1 {
			want[n] = true
		}
	}
	if qualifies(current) {
		free = current
	} else {
		for n := 1; n <= max && n <= slotLimit && free == 0; n++ {
			if qualifies(n) {
				free = n
			}
		}
	}
	if free > 0 {
		want[free] = true
	}
	return want, free
}

// adjustSlots starts the wanted slots and stops the extra ones (unbound, or a spare free one) when
// nobody is mirroring on them. Runs at startup, on every new binding and every second.
func adjustSlots() {
	adjustMu.Lock()
	defer adjustMu.Unlock()
	mu.Lock()
	if quitting || uxplayExe == "" {
		mu.Unlock()
		return
	}
	now := time.Now()
	busy := map[int]bool{}
	for _, s := range slots {
		busy[s.N] = s.busyAt(now)
	}
	current := currentFree
	if current > 0 && !slots[current-1].active {
		current = 0
	}
	want, free := wantedSlots(cfg.MaxSlots, cfg.Bindings, busy, current)
	currentFree = free
	var show, start, hide []*Slot
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
			s.active, s.title = false, "" // empty title: updateMenu rewrites it next time it comes up
			hide = append(hide, s)
		}
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
	if len(show)+len(hide) > 0 {
		updateStatus()
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
// slot with the new args as soon as nobody is mirroring on it (idle ones within a few seconds),
// so a misclick never drops a live device. Slots stopped by a crash loop start again.
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
		logf("resolution -> %s; applies to each slot when nobody is mirroring on it", r)
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
		logf("fps -> %d; applies to each slot when nobody is mirroring on it", f)
	}
}

func unbindSlot(s *Slot) {
	mu.Lock()
	b := cfg.Bindings[s.key()]
	delete(cfg.Bindings, s.key())
	if s.cmd != nil {
		// The running UxPlay still has the binding: if the same device comes back before the slot
		// restarts, don't bind it again (the operator just released the slot).
		s.unbound = b.DeviceID
	}
	saveConfig()
	mu.Unlock()
	logf("slot %d: unbound from %s, %s", s.N, b.Device, b.DeviceID)
	updateMenu(s)
	updateStatus()
	adjustSlots()
}

// requestRestartLocked kills the slot's UxPlay; the supervisor starts another with the current
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

// resumeCrashedLocked starts again the slots stopped by a crash loop: the change may be the fix.
// Call with mu held.
func resumeCrashedLocked() {
	crashWarned = false
	for _, s := range slots {
		if s.active && s.crashStopped {
			requestRestartLocked(s)
		}
	}
}

func restartAll() {
	mu.Lock()
	crashWarned = false
	for _, s := range slots {
		if s.active {
			requestRestartLocked(s)
		}
	}
	mu.Unlock()
}

// slotMAC gives each slot a fixed MAC of its own (AirPlay uses it as the receiver's deviceID):
// with the same MAC everywhere, the device merges the receivers and shows a single LIVE. Fixed per
// PC and slot so the device recognizes the same "LIVE N" across restarts. Locally administered bit.
func slotMAC(n int) string {
	h := sha256.Sum256([]byte("iPhoneScreenshare|" + hostname + "|" + strconv.Itoa(n)))
	b := h[:6]
	b[0] = (b[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// slotArgs builds the slot's UxPlay command line from the current config; call with mu held.
// The order is always the same (watchWindows compares it with the running process).
func slotArgs(s *Slot) []string {
	// -p: each slot with its own ports (n, n+1, n+2). -m: the slot's own MAC.
	// -vs d3d11videosink: the sink UxPlay handles on Windows and CI tests (autovideosink would
	// pick d3d12). It can be changed in extraArgs, which come last.
	args := []string{"-n", s.name(), "-nh", "-p", strconv.Itoa(35000 + 10*s.N), "-m", slotMAC(s.N), "-vs", "d3d11videosink"}
	args = append(args, resolutions[cfg.Resolution]...)
	args = append(args, "-fps", strconv.Itoa(cfg.FPS))
	v := validBindings()
	for _, k := range sortedKeys(v) {
		if k == s.key() {
			// -nohold only on the slot locked to its owner: if Wi-Fi blips, it reconnects at once
			// instead of waiting for the old session to expire (15 s). On the free slot the first
			// device keeps the screen and a second one is refused (goes to the next LIVE) instead
			// of dropping the first.
			args = append(args, "-restrict", "-allow", v[k].DeviceID, "-nohold")
		} else {
			args = append(args, "-block", v[k].DeviceID)
		}
	}
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

// processLine updates the slot with one line of UxPlay output. UxPlay logs the connection request
// before deciding whether to accept it; the device only counts (state, grace period, binding) once
// the connection is accepted. Returns what to write to the app log and whether a new binding was
// made. Call with mu held.
func (s *Slot) processLine(line string, now time.Time) (msg string, bound bool) {
	if m := reSocket.FindStringSubmatch(line); m != nil {
		s.newSock = m[1]
		return "", false
	}
	if codec, w, h, ok := parseGeometry(line); ok {
		if w == s.width && h == s.height && codec == s.codec {
			return "", false
		}
		if w != s.width || h != s.height {
			s.geometryAt = now
		}
		s.width, s.height, s.codec = w, h, codec
		return fmt.Sprintf("slot %d: video %s %s", s.N, codec, orientation(w, h)), false
	}
	if m := reConnection.FindStringSubmatch(line); m != nil {
		s.candName, s.candID, s.candSock = m[1], m[3], s.newSock
		return "", false
	}
	if s.candID != "" && containsAny(line, refusals) {
		msg = fmt.Sprintf("slot %d: refused %s, %s", s.N, s.candName, s.candID)
		s.candName, s.candID = "", ""
		return msg, false
	}
	if s.candID != "" && containsAny(line, []string{"Client identified as", "starting mirroring", "Begin streaming"}) {
		if s.mirroring || now.Sub(s.sockEnded) < 5*time.Second {
			s.takeover = true // new session while the previous one was still live or just closed (-nohold)
		}
		msg, bound = s.accept(now)
	}
	if strings.Contains(line, "Begin streaming to GStreamer video pipeline") {
		s.mirroring, s.videoStart = true, now
		s.state, s.color = "live", "green"
		s.ignoreEndsUntil = time.Time{}
		if s.takeover {
			s.takeover, s.ignoreEndsUntil = false, now.Add(3*time.Second)
		}
		return msg, bound
	}
	if s.mirroring {
		m := reClosed.FindStringSubmatch(line)
		if containsAny(line, endings) && now.Before(s.ignoreEndsUntil) {
			// end of the session that was taken over, logged after the new one started; the new
			// one's end is still seen through its socket (Connection closed on socket).
			return fmt.Sprintf("slot %d: end of the previous session ignored, %s is still live", s.N, s.device), bound
		}
		if containsAny(line, endings) || (m != nil && m[1] == s.sock) {
			s.mirroring, s.ended = false, now
			if m != nil {
				s.sockEnded = now
			}
			s.state, s.color = "stopped: mirror again on the device", "yellow"
			return fmt.Sprintf("slot %d: %s stopped mirroring", s.N, s.device), bound
		}
	}
	return msg, bound
}

// accept takes the device that asked to connect and binds the slot if it is free.
// Call with mu held.
func (s *Slot) accept(now time.Time) (string, bool) {
	device, id := s.candName, s.candID
	s.candName, s.candID = "", ""
	s.sock, s.device, s.connected = s.candSock, device, now
	s.width, s.height, s.codec = 0, 0, ""
	s.state, s.color = "connecting", "yellow"
	v := validBindings()
	owner := ""
	for _, k := range sortedKeys(v) {
		if v[k].DeviceID == id {
			owner = k
		}
	}
	_, taken := v[s.key()]
	// UxPlay only accepts a device that isn't the slot's while it still runs with the args from
	// before the binding (no -restrict/-block). That connection doesn't count: restart now,
	// without waiting for the grace period, or it would stay live on another device's OBS source.
	switch {
	case cfg.MaxSlots == 1 || owner == s.key():
		return fmt.Sprintf("slot %d: %s connected", s.N, device), false
	case owner != "":
		requestRestartLocked(s)
		return fmt.Sprintf("slot %d: %s connected but belongs to slot %s; restarting this slot to refuse it",
			s.N, device, owner), false
	case taken:
		requestRestartLocked(s)
		return fmt.Sprintf("slot %d: %s connected but the slot belongs to %s; restarting the slot to refuse it",
			s.N, device, v[s.key()].Device), false
	case id == s.unbound:
		return fmt.Sprintf("slot %d: %s connected; it was unbound from this slot and is only bound again after the slot restarts",
			s.N, device), false
	}
	for k, x := range cfg.Bindings { // old binding of the same device outside maxSlots
		if x.DeviceID == id {
			delete(cfg.Bindings, k)
		}
	}
	cfg.Bindings[s.key()] = Binding{DeviceID: id, Device: device}
	return fmt.Sprintf("slot %d: bound to %s, deviceID %s", s.N, device, id), true
}

// readOutput copies UxPlay's output to the log and tracks connections, mirroring and bindings.
// It closes done when finished: the supervisor waits for it, so no line from the old process
// lands in the new process's state.
func readOutput(s *Slot, r io.Reader, done chan<- struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintf(logFile, "[slot %d] %s\n", s.N, line)
		mu.Lock()
		before := [4]any{s.state, s.width, s.height, s.codec}
		size := [2]int{s.width, s.height}
		msg, bound := s.processLine(line, time.Now())
		if bound {
			saveConfig()
		}
		changed := before != [4]any{s.state, s.width, s.height, s.codec}
		resized := size != [2]int{s.width, s.height} && s.width > 0 && s.height > 0
		mu.Unlock()
		if resized {
			fitSoon(s) // rotation: fit as soon as the geometry settles, not on the next tick
		}
		if msg != "" {
			logf("%s", msg)
		}
		if changed || bound {
			updateMenu(s)
		}
		if bound {
			updateStatus()
			adjustSlots() // brings up the next free slot
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
		s.pending, s.unbound, s.takeover, s.ignoreEndsUntil, s.sockEnded = false, "", false, time.Time{}, time.Time{}
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
			s.crashStopped = true
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
		if windows.IsWindowVisible(h) {
			windowsByPID[pid] = append(windowsByPID[pid], h)
		}
		return 1
	})
)

// watchWindows gives each video window the fixed title "Slot N" (OBS finds the source by title),
// flags video that arrived without a window and restarts idle slots whose args changed (new
// binding, unbinding, config) or that are pending (network changed).
func watchWindows() {
	for range time.Tick(time.Second) {
		byPID := visibleWindows()
		revealStale()
		adjustSlots()
		for _, s := range slots {
			mu.Lock()
			cmd, active := s.cmd, s.active
			mu.Unlock()
			if !active || cmd == nil || cmd.Process == nil {
				continue
			}
			wins := byPID[uint32(cmd.Process.Pid)]
			for _, h := range wins {
				if windowTitle(h) != s.window() {
					setWindowTitle(h, s.window())
				}
			}
			fitWindows(s, wins, time.Now())
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
				logf("slot %d: applying config/bindings, nobody mirroring", s.N)
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
// the "Slot N" title and the video's shape right then (onWindowEvent), so the window isn't seen at
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

// onWindowEvent handles a window just created (still hidden) or shown. A top-level window with a
// title bar of a slot's UxPlay gets the slot's title. The video sink resizes its window to the full
// video size right before it shows it, which undoes any fit made earlier, and the show event
// arrives once the window is already on screen. So when the geometry is already known (UxPlay logs
// it before the window exists), the sink's window is made transparent while it is still hidden,
// fitted when it is shown, and made opaque once the fit has landed: it is first seen in shape.
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
	mu.Lock()
	for _, o := range slots {
		if o.active && o.cmd != nil && o.cmd.Process != nil && uint32(o.cmd.Process.Pid) == pid {
			s, video = o, [2]int{o.width, o.height}
		}
	}
	mu.Unlock()
	if s == nil {
		return
	}
	if style, _, _ := getWindowLongPtr.Call(uintptr(h), gwlStyle); style&wsCaption == 0 {
		if shown {
			revealWindow(h) // went full screen before its show event was handled
		}
		return // not a video window, or already full screen
	}
	if windowTitle(h) != s.window() {
		setWindowTitle(h, s.window())
	}
	known := video[0] > 0 && video[1] > 0
	if !shown {
		if known && windowClass(h) == sinkWindowClass && !windows.IsWindowVisible(h) {
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

// sinkWindowClass is d3d11videosink's window class.
const sinkWindowClass = "GSTD3D11"

const (
	gwlExStyle  = ^uintptr(19) // -20
	wsExLayered = 0x00080000
)

var (
	getClassName      = user32.NewProc("GetClassNameW")
	setWindowLongPtr  = user32.NewProc("SetWindowLongPtrW")
	setLayeredWinAttr = user32.NewProc("SetLayeredWindowAttributes")
	isWindow          = user32.NewProc("IsWindow")
	// transparent holds the video windows made transparent at creation, until their fit at show has
	// landed, and whether revealStale already saw them visible. Guarded by fitMu.
	transparent = map[windows.HWND]bool{}
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

// revealWindow undoes hideWindow, if it hid the window.
func revealWindow(h windows.HWND) {
	fitMu.Lock()
	_, ok := transparent[h]
	delete(transparent, h)
	fitMu.Unlock()
	if ok {
		ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle)
		setWindowLongPtr.Call(uintptr(h), gwlExStyle, ex&^wsExLayered)
	}
}

// revealStale reveals the transparent windows that were already visible on the previous call (a
// show event that never arrived) and forgets the ones that are gone. Called every second.
func revealStale() {
	var stale []windows.HWND
	fitMu.Lock()
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
