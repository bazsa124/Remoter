//go:build windows

package platform

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Following the input desktop.
//
// Windows switches the *input desktop* whenever the lock screen, the sign-in
// screen or a UAC prompt appears: those live on the secure "Winlogon" desktop,
// not on "Default". GDI capture and SendInput only reach the desktop the
// calling thread is attached to, so a helper that stays on Default sees a frozen
// picture and types into nothing exactly when it matters most.
//
// The screen helper therefore funnels every capture and input call through one
// OS thread that re-attaches to the current input desktop before each call. A
// thread can only switch desktops while it owns no windows or hooks, which is
// why this is a dedicated thread doing nothing else.

var (
	procOpenInputDesktop  = user32.NewProc("OpenInputDesktop")
	procSetThreadDesktop  = user32.NewProc("SetThreadDesktop")
	procCloseDesktop      = user32.NewProc("CloseDesktop")
	procGetUserObjectInfo = user32.NewProc("GetUserObjectInformationW")
	desktopFollowing      atomic.Bool
	desktopOnce           sync.Once
	desktopCalls          chan func()
)

const (
	desktopAllAccess = 0x01FF // DESKTOP_* rights: read, write, enumerate, switch
	uoiName          = 2
)

func followInputDesktop() {
	desktopOnce.Do(func() {
		desktopCalls = make(chan func())
		started := make(chan struct{})
		go func() {
			// Never unlocked: the thread belongs to this loop for the life of
			// the process, so its desktop attachment cannot leak to other code.
			runtime.LockOSThread()
			close(started)
			var current uintptr
			for fn := range desktopCalls {
				current = attachInputDesktop(current)
				fn()
			}
		}()
		<-started
		desktopFollowing.Store(true)
	})
}

// onDesktop runs fn on the desktop-following thread when following is on, and
// inline otherwise (the normal case: an interactive process already sits on the
// user's desktop).
func onDesktop(fn func()) {
	if !desktopFollowing.Load() {
		fn()
		return
	}
	done := make(chan struct{})
	desktopCalls <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// attachInputDesktop moves the calling thread to the current input desktop if
// it changed, returning the handle now attached.
func attachInputDesktop(current uintptr) uintptr {
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopAllAccess)
	if h == 0 {
		return current // transient during a switch; keep what we have
	}
	if current != 0 && desktopName(h) == desktopName(current) {
		procCloseDesktop.Call(h)
		return current
	}
	if ok, _, _ := procSetThreadDesktop.Call(h); ok == 0 {
		procCloseDesktop.Call(h)
		return current
	}
	if current != 0 {
		procCloseDesktop.Call(current)
	}
	return h
}

func desktopName(h uintptr) string {
	var buf [64]uint16
	var needed uint32
	ok, _, _ := procGetUserObjectInfo.Call(h, uoiName,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&needed)))
	if ok == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// inputDesktopName reports which desktop has input right now: "Default" for the
// normal desktop, "Winlogon" for the lock screen, sign-in and UAC prompts.
func inputDesktopName() string {
	h, _, _ := procOpenInputDesktop.Call(0, 0, 0x0001) // DESKTOP_READOBJECTS is enough to read the name
	if h == 0 {
		return ""
	}
	defer procCloseDesktop.Call(h)
	return desktopName(h)
}
