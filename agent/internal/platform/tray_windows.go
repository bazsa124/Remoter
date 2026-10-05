//go:build windows

package platform

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The session indicator: a red dot in the notification area for as long as
// anyone is connected, plus a one-off notification when a session starts.
//
// Hand-written against Shell_NotifyIcon rather than pulled from a tray library:
// it is one window, one icon and a message loop, and the libraries bring menus,
// cgo on other platforms and far more surface than a status light needs.

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostMessage      = user32.NewProc("PostMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procRegisterWinMsg   = user32.NewProc("RegisterWindowMessageW")
	procCreateIconInd    = user32.NewProc("CreateIconIndirect")
	procDestroyIcon      = user32.NewProc("DestroyIcon")
	procCreateBitmap     = gdi32.NewProc("CreateBitmap")
)

const (
	wmClose   = 0x0010
	wmDestroy = 0x0002
	wmApp     = 0x8000
	wmUpdate  = wmApp + 1
	wmTrayCB  = wmApp + 2

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifWarning = 0x02
)

type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     uintptr
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Wnd     uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

type tray struct {
	mu      sync.Mutex
	pending TrayState
	shown   bool
	hwnd    uintptr
	icon    uintptr
	created uint32 // "TaskbarCreated": Explorer restarted, re-add the icon
	banner  *banner
}

// One process, one indicator; the window procedure needs to find it.
var theTray tray

func runTray(ctx context.Context, updates <-chan TrayState) error {
	runtime.LockOSThread() // a window belongs to the thread that created it
	defer runtime.UnlockOSThread()

	t := &theTray
	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return fmt.Errorf("GetModuleHandleEx: %w", err)
	}
	className := windows.StringToUTF16Ptr("RemoterSessionIndicator")
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   windows.NewCallback(trayWndProc),
		Instance:  instance,
		ClassName: className,
	}
	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}
	hwnd, _, err := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Remoter"))),
		0, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	created, _, _ := procRegisterWinMsg.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("TaskbarCreated"))))

	t.mu.Lock()
	t.hwnd, t.created, t.icon = hwnd, uint32(created), makeDotIcon()
	t.banner = newBanner(instance)
	t.mu.Unlock()
	defer func() {
		if t.icon != 0 {
			procDestroyIcon.Call(t.icon)
		}
	}()

	// State arrives from other goroutines; the window thread applies it.
	go func() {
		for {
			select {
			case <-ctx.Done():
				procPostMessage.Call(hwnd, wmClose, 0, 0)
				return
			case st, ok := <-updates:
				if !ok {
					procPostMessage.Call(hwnd, wmClose, 0, 0)
					return
				}
				t.mu.Lock()
				t.pending = st
				t.mu.Unlock()
				procPostMessage.Call(hwnd, wmUpdate, 0, 0)
			}
		}
	}()

	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func trayWndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	t := &theTray
	switch uint32(message) {
	case wmUpdate:
		t.apply(false)
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		t.remove()
		t.banner.destroy()
		procPostQuitMessage.Call(0)
		return 0
	}
	if t.created != 0 && uint32(message) == t.created {
		t.apply(true)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
	return r
}

func (t *tray) data() notifyIconData {
	return notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             t.hwnd,
		ID:              1,
		CallbackMessage: wmTrayCB,
		Icon:            t.icon,
	}
}

// apply runs on the window thread. reAdd forces NIM_ADD after Explorer restarts.
func (t *tray) apply(reAdd bool) {
	t.mu.Lock()
	st := t.pending
	t.pending.Notice, t.pending.NoticeTitle = "", "" // a notice is shown once
	t.mu.Unlock()

	if !st.Active {
		t.remove()
		t.banner.hide()
		return
	}
	text := st.Banner
	if text == "" {
		text = st.Tooltip
	}
	t.banner.show(text)

	nid := t.data()
	nid.Flags = nifMessage | nifIcon | nifTip
	copyUTF16(nid.Tip[:], st.Tooltip)
	if st.Notice != "" {
		nid.Flags |= nifInfo
		nid.InfoFlags = niifWarning
		copyUTF16(nid.InfoTitle[:], st.NoticeTitle)
		copyUTF16(nid.Info[:], st.Notice)
	}
	op := uintptr(nimModify)
	if !t.shown || reAdd {
		op = nimAdd
	}
	if ok, _, _ := procShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&nid))); ok != 0 {
		t.shown = true
	} else if op == nimModify {
		// The icon vanished underneath us; add it back.
		if ok, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ok != 0 {
			t.shown = true
		}
	}
}

func (t *tray) remove() {
	if !t.shown {
		return
	}
	nid := t.data()
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	t.shown = false
}

func copyUTF16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

type iconInfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     uintptr
	Color    uintptr
}

// makeDotIcon draws an anti-aliased red dot: the universal "you are being
// recorded" light, which is the message this icon exists to send.
func makeDotIcon() uintptr {
	const n = 32
	header := bitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: n, Height: -n, Planes: 1, BitCount: 32,
	}
	screen, _, _ := procGetDC.Call(0)
	var bits unsafe.Pointer
	color, _, _ := procCreateDIBSection.Call(screen, uintptr(unsafe.Pointer(&header)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	procReleaseDC.Call(0, screen)
	if color == 0 || bits == nil {
		return 0
	}
	defer procDeleteObject.Call(color)

	px := unsafe.Slice((*byte)(bits), n*n*4)
	const cx, cy, radius = 15.5, 15.5, 13.0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			a := math.Max(0, math.Min(1, radius-d+0.5))
			i := (y*n + x) * 4
			// Premultiplied BGRA.
			px[i+0] = byte(0x35 * a)
			px[i+1] = byte(0x39 * a)
			px[i+2] = byte(0xE5 * a)
			px[i+3] = byte(0xFF * a)
		}
	}

	maskBits := make([]byte, n*n/8) // ignored for 32-bit icons, but required
	mask, _, _ := procCreateBitmap.Call(n, n, 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if mask == 0 {
		return 0
	}
	defer procDeleteObject.Call(mask)

	ii := iconInfo{Icon: 1, Mask: mask, Color: color}
	icon, _, _ := procCreateIconInd.Call(uintptr(unsafe.Pointer(&ii)))
	return icon
}
