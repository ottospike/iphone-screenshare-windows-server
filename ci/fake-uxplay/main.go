//go:build windows

// Stand-in for uxplay.exe in CI, without a device. The app uses it through the "uxplay" config key.
//   - reports if the app started the process with SW_HIDE in STARTUPINFO (video window hidden);
//   - on the free slot "LIVE 1" it simulates a device that connects, is accepted and starts
//     mirroring, with the window showing up only a few seconds after the accept (the case where
//     the slot was restarted before the window existed), then rotates to landscape;
//   - prints its client size on every resize ("FAKE: client WxH"), so CI sees the app fit the
//     window to the portrait video and then to the landscape one;
//   - the other slots just stay up, like UxPlay waiting for a device.
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

	name := flagValue(args, "-n")
	if name != "LIVE 1" || slices.Contains(args, "-restrict") {
		for {
			time.Sleep(time.Hour)
		}
	}
	time.Sleep(3 * time.Second)
	fmt.Println("Accepted IPv4 client on socket 100, port 35011")
	fmt.Println("connection request from CI iPhone (iPhone18,2) with deviceID = AA:BB:CC:DD:EE:01")
	fmt.Println("Client identified as User-Agent: AirPlay/960.13.1")
	fmt.Println("raop_rtp_mirror starting mirroring")
	time.Sleep(4 * time.Second) // video is slow: no window yet
	fmt.Println("*** codec is h264: geometry is (w,h)=(1320,2868) (source); (w,h)=(1320,2868) (displayed)")
	fmt.Println("Begin streaming to GStreamer video pipeline")
	go func() {
		time.Sleep(5 * time.Second) // the app fits the window to portrait first
		fmt.Println("*** codec is h264: geometry is (w,h)=(2868,1320) (source); (w,h)=(2868,1320) (displayed)")
	}()
	openWindow(name)
}

// openWindow creates the video window the way the sink does (normal ShowWindow) and runs the
// message loop.
func openWindow(title string) {
	runtime.LockOSThread()
	class, _ := windows.UTF16PtrFromString("FakeUxPlay")
	t, _ := windows.UTF16PtrFromString(title)
	proc := windows.NewCallback(func(h windows.HWND, m uint32, w, l uintptr) uintptr {
		if m == 0x0005 { // WM_SIZE: client width and height in lParam
			fmt.Printf("FAKE: client %dx%d\n", l&0xFFFF, (l>>16)&0xFFFF)
		}
		r, _, _ := defWindowProc.Call(uintptr(h), uintptr(m), w, l)
		return r
	})
	wc := wndClassEx{WndProc: proc, ClassName: class}
	wc.Size = uint32(unsafe.Sizeof(wc))
	registerClass.Call(uintptr(unsafe.Pointer(&wc)))
	const wsOverlappedWindow, cwUseDefault, swShowNormal = 0x00CF0000, 0x80000000, 1
	h, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(t)), wsOverlappedWindow,
		cwUseDefault, cwUseDefault, 640, 360, 0, 0, 0, 0)
	if h == 0 {
		fmt.Println("FAKE: CreateWindowEx failed:", err)
		os.Exit(4)
	}
	showWindow.Call(h, swShowNormal)
	v := 0
	if windows.IsWindowVisible(windows.HWND(h)) {
		v = 1
	}
	fmt.Printf("FAKE: visible=%d\n", v)
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
