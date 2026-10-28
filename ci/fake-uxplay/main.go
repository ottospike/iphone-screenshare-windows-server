//go:build windows

// Stand-in for uxplay.exe in CI, without a device. The app uses it through the "uxplay" config key.
//   - reports if the app started the process with SW_HIDE in STARTUPINFO (video window hidden);
//   - every instance prints the patched UxPlay's "mDNS: advertised at startup";
//   - on the first instance (ports 35010, "-p 35010"), once, it simulates a device that connects, is
//     accepted and starts mirroring, with the window showing up only a few seconds after the
//     accept, then rotates to landscape; it prints the patch's "mDNS: hidden while mirroring" and
//     the client's FPS reports (-FPSdata: a plist in XML once a second); 28 s after starting the
//     device's mirror stream closes (a line the app doesn't take as an end by itself) and the
//     instance prints the patch's "mDNS: mirroring ended, advertised again";
//   - creates its window like d3d11videosink: hidden, then on the "first frame" sized to the full
//     video at (0,0) and shown; it prints the client size it has the first time it is seen, visible
//     and not transparent ("FAKE: seen client WxH"): the app must have fitted it by then;
//   - prints its client size on every resize ("FAKE: client WxH"), so CI sees the app fit the
//     window to the portrait video and then to the landscape one;
//   - like GStreamer's close button, opens a tooltip window ("tooltips_class32") owned by the video
//     window, and later prints both windows' titles and the tooltip's size ("FAKE: titles ..."):
//     the app must rename and fit only the video window;
//   - the other instances just stay up, like UxPlay waiting for a device.
package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	registerClass    = user32.NewProc("RegisterClassExW")
	createWindow     = user32.NewProc("CreateWindowExW")
	showWindow       = user32.NewProc("ShowWindow")
	getClientRect    = user32.NewProc("GetClientRect")
	getWindowText    = user32.NewProc("GetWindowTextW")
	setTimer         = user32.NewProc("SetTimer")
	killTimer        = user32.NewProc("KillTimer")
	moveWindow       = user32.NewProc("MoveWindow")
	adjustWindowRect = user32.NewProc("AdjustWindowRect")
	getWindowLongPtr = user32.NewProc("GetWindowLongPtrW")
	getLayeredAttr   = user32.NewProc("GetLayeredWindowAttributes")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	getMessage       = user32.NewProc("GetMessageW")
	translateMessage = user32.NewProc("TranslateMessage")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// fpsReport is a client report as the real UxPlay prints it with -FPSdata (libplist's XML, one
// print, so several lines).
func fpsReport(sent float64) string {
	stage := func(name string, v float64) string {
		return fmt.Sprintf("\t\t<dict>\n\t\t\t<key>name</key>\n\t\t\t<string>%s</string>\n\t\t\t<key>value</key>\n\t\t\t<real>%g</real>\n\t\t</dict>\n", name, v)
	}
	return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n\t<key>fpsInfo</key>\n\t<array>\n" +
		stage("SubS", 60) + stage("B4En", 60) + stage("EnDp", 0) + stage("Sent", sent) +
		"\t</array>\n\t<key>latencyMs</key>\n\t<integer>90</integer>\n</dict>\n</plist>\n"
}

func main() {
	args := os.Args[1:]
	fmt.Println("FAKE: args " + strings.Join(args, " "))
	var si windows.StartupInfo
	windows.GetStartupInfo(&si)
	if si.Flags&windows.STARTF_USESHOWWINDOW != 0 && si.ShowWindow == windows.SW_HIDE {
		fmt.Println("FAKE: inherited SW_HIDE")
	}
	fmt.Println("using user-set MAC address " + flagValue(args, "-m"))
	fmt.Println("Initialized server socket(s)")
	fmt.Println("mDNS: advertised at startup")

	// One simulated session in all: the first instance, restarted for the next device after the
	// session, then waits like the others. The marker sits next to the exe (the smoke test removes it).
	exe, _ := os.Executable()
	marker := exe + ".session"
	if _, err := os.Stat(marker); flagValue(args, "-p") != "35010" || slices.Contains(args, "-restrict") || err == nil {
		for {
			time.Sleep(time.Hour)
		}
	}
	os.WriteFile(marker, nil, 0o644)
	start := time.Now()
	time.Sleep(3 * time.Second)
	fmt.Println("Accepted IPv4 client on socket 100, port 35011")
	fmt.Println("connection request from CI iPhone (iPhone18,2) with deviceID = AA:BB:CC:DD:EE:01")
	fmt.Println("Client identified as User-Agent: AirPlay/960.13.1")
	fmt.Println("raop_rtp_mirror starting mirroring")
	fmt.Println("mDNS: hidden while mirroring")
	time.Sleep(4 * time.Second) // video is slow: no window yet
	fmt.Println("*** codec is h264: geometry is (w,h)=(1320,2868) (source); (w,h)=(1320,2868) (displayed)")
	fmt.Println("Begin streaming to GStreamer video pipeline")
	time.Sleep(500 * time.Millisecond) // the sink creates its window after the geometry line
	go func() {
		for i := 0; i < 8; i++ { // the client reports once a second
			fmt.Print(fpsReport(59.6 + float64(i%2)*0.2))
			time.Sleep(time.Second)
		}
	}()
	go func() {
		time.Sleep(6 * time.Second) // the app fits the window to portrait first
		fmt.Println("*** codec is h264: geometry is (w,h)=(2868,1320) (source); (w,h)=(2868,1320) (displayed)")
	}()
	go func() {
		time.Sleep(28*time.Second - time.Since(start)) // after the smoke test's first look
		// Only the patch line tells the app that the session is over.
		fmt.Println("raop_rtp_mirror tcp socket was closed by client (recv returned 0)")
		fmt.Println("mDNS: mirroring ended, advertised again")

	}()
	openWindow("LIVE", 1320, 2868)
}

func text(h uintptr) string {
	buf := make([]uint16, 256)
	getWindowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func clientSize(h uintptr) (int32, int32) {
	var r [4]int32
	getClientRect.Call(h, uintptr(unsafe.Pointer(&r)))
	return r[2] - r[0], r[3] - r[1]
}

// openWindow creates the video window the way d3d11videosink does: hidden, with its class name and
// no size; 700 ms later, as the sink does with its first frame, it sizes the window to the full
// video at (0,0) and shows it. It prints the client size the window has the first time it is seen,
// visible and not transparent ("FAKE: seen client WxH"), and runs the message loop. Then it opens
// the close button's tooltip and, 12 s later, prints both titles and the tooltip's size.
func openWindow(title string, vw, vh int32) {
	const (
		wsOverlappedWindow, cwUseDefault         = 0x00CF0000, 0x80000000
		wmSize, wmShowWindow, wmStyleChanged     = 0x0005, 0x0018, 0x007D
		wmTimer, swShow, gwlExStyle, wsExLayered = 0x0113, 5, ^uintptr(19), 0x00080000
		lwaAlpha                                 = 2
		wsPopup, wsExToolWindow, wsExTopmost     = 0x80000000, 0x00000080, 0x00000008
		swShowNoActivate                         = 4
		tipW, tipH                               = 120, 24
	)
	runtime.LockOSThread()
	class, _ := windows.UTF16PtrFromString("GSTD3D11")
	t, _ := windows.UTF16PtrFromString(title)
	shown, seen := false, false
	var tip uintptr
	report := func(h windows.HWND) {
		if !shown || seen {
			return
		}
		if ex, _, _ := getWindowLongPtr.Call(uintptr(h), gwlExStyle); ex&wsExLayered != 0 {
			var alpha byte
			var flags uint32
			r, _, _ := getLayeredAttr.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&alpha)), uintptr(unsafe.Pointer(&flags)))
			if r != 0 && flags&lwaAlpha != 0 && alpha == 0 {
				return // transparent: not seen
			}
		}
		seen = true
		w, hh := clientSize(uintptr(h))
		fmt.Printf("FAKE: seen client %dx%d\n", w, hh)
	}
	proc := windows.NewCallback(func(h windows.HWND, m uint32, w, l uintptr) uintptr {
		switch m {
		case wmSize: // client width and height in lParam
			fmt.Printf("FAKE: client %dx%d\n", l&0xFFFF, (l>>16)&0xFFFF)
			report(h)
		case wmShowWindow: // sent right before the window becomes visible
			if w != 0 {
				shown = true
				report(h)
			}
		case wmStyleChanged:
			report(h)
		case wmTimer:
			killTimer.Call(uintptr(h), w)
			switch w {
			case 1: // first frame: size to the video, then show, like the sink
				r := [4]int32{0, 0, vw, vh}
				adjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), wsOverlappedWindow, 0)
				moveWindow.Call(uintptr(h), 0, 0, uintptr(r[2]-r[0]), uintptr(r[3]-r[1]), 0)
				showWindow.Call(uintptr(h), swShow)
				v := 0
				if windows.IsWindowVisible(h) {
					v = 1
				}
				fmt.Printf("FAKE: visible=%d\n", v)
				setTimer.Call(uintptr(h), 2, 1500, 0)
			case 2: // the mouse is over the close button: its tooltip shows
				tc, _ := windows.UTF16PtrFromString("tooltips_class32")
				tt, _ := windows.UTF16PtrFromString("Close")
				tip, _, _ = createWindow.Call(wsExToolWindow|wsExTopmost, uintptr(unsafe.Pointer(tc)), uintptr(unsafe.Pointer(tt)),
					wsPopup, 300, 300, tipW, tipH, uintptr(h), 0, 0, 0)
				if tip == 0 {
					fmt.Println("FAKE: tooltip not created")
				} else {
					showWindow.Call(tip, swShowNoActivate)
				}
				setTimer.Call(uintptr(h), 3, 12000, 0)
			case 3:
				tw, th := clientSize(tip)
				fmt.Printf("FAKE: titles video=%q tooltip=%q tooltip size %dx%d\n", text(uintptr(h)), text(tip), tw, th)
			}
			return 0
		}
		r, _, _ := defWindowProc.Call(uintptr(h), uintptr(m), w, l)
		return r
	})
	wc := wndClassEx{WndProc: proc, ClassName: class}
	wc.Size = uint32(unsafe.Sizeof(wc))
	registerClass.Call(uintptr(unsafe.Pointer(&wc)))
	tipProc := windows.NewCallback(func(h windows.HWND, m uint32, w, l uintptr) uintptr {
		r, _, _ := defWindowProc.Call(uintptr(h), uintptr(m), w, l)
		return r
	})
	tc := wndClassEx{WndProc: tipProc}
	tc.ClassName, _ = windows.UTF16PtrFromString("tooltips_class32")
	tc.Size = uint32(unsafe.Sizeof(tc))
	registerClass.Call(uintptr(unsafe.Pointer(&tc)))
	h, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(t)), wsOverlappedWindow,
		cwUseDefault, cwUseDefault, 0, 0, 0, 0, 0, 0)
	if h == 0 {
		fmt.Println("FAKE: CreateWindowEx failed:", err)
		os.Exit(4)
	}
	setTimer.Call(h, 1, 700, 0)
	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}
