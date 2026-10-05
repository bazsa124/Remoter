//go:build windows

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Input injection for Tier 3.
//
// SendInput only, per the spec: keybd_event and mouse_event are deprecated and
// cannot batch atomically. Two keyboard paths, also per the spec:
//
//   - characters go through KEYEVENTF_UNICODE, which bypasses the host keyboard
//     layout entirely, so Hungarian text arrives correctly without the host
//     being set to a Hungarian layout;
//   - modifiers and named keys go through scancodes with the virtual key filled
//     in, because DirectInput applications ignore VK-only events.
var (
	user32      = windows.NewLazySystemDLL("user32.dll")
	sendInput   = user32.NewProc("SendInput")
	getCursor   = user32.NewProc("GetCursorPos")
	getMetrics  = user32.NewProc("GetSystemMetrics")
	inputStruct = int32(unsafe.Sizeof(rawInput{}))
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	moveAbsolute = 0x8000
	moveRelative = 0x0001

	eventLeftDown   = 0x0002
	eventLeftUp     = 0x0004
	eventRightDown  = 0x0008
	eventRightUp    = 0x0010
	eventMiddleDown = 0x0020
	eventMiddleUp   = 0x0040
	eventWheel      = 0x0800

	keyUp      = 0x0002
	keyUnicode = 0x0004
	keyScan    = 0x0008

	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// rawInput mirrors Win32 INPUT. The union payload is kept as a byte array so
// the layout is explicit rather than dependent on Go's struct packing.
type rawInput struct {
	kind    uint32
	_       uint32 // explicit padding: the union is 8-byte aligned on amd64
	payload [32]byte
}

type mouseInput struct {
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extraInfo uintptr
}

type keyboardInput struct {
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	extraInfo uintptr
}

func mouseEvent(m mouseInput) rawInput {
	in := rawInput{kind: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&in.payload[0])) = m
	return in
}

func keyEvent(k keyboardInput) rawInput {
	in := rawInput{kind: inputKeyboard}
	*(*keyboardInput)(unsafe.Pointer(&in.payload[0])) = k
	return in
}

func send(events ...rawInput) error {
	if len(events) == 0 {
		return nil
	}
	n, _, err := sendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		uintptr(inputStruct),
	)
	if int(n) != len(events) {
		return fmt.Errorf("SendInput accepted %d of %d events: %w", n, len(events), err)
	}
	return nil
}

// moveMouse positions the cursor using normalised coordinates (0..1) across the
// whole virtual desktop, so the caller never needs to know the host's geometry.
func moveMouse(x, y float64) error {
	return send(mouseEvent(mouseInput{
		dx:    int32(clamp01(x) * 65535),
		dy:    int32(clamp01(y) * 65535),
		flags: moveAbsolute | moveRelative,
	}))
}

func clickMouse(button string, down bool) error {
	var flag uint32
	switch button {
	case "left":
		flag = pick(down, eventLeftDown, eventLeftUp)
	case "right":
		flag = pick(down, eventRightDown, eventRightUp)
	case "middle":
		flag = pick(down, eventMiddleDown, eventMiddleUp)
	default:
		return fmt.Errorf("unknown button %q", button)
	}
	return send(mouseEvent(mouseInput{flags: flag}))
}

func scrollMouse(delta int) error {
	return send(mouseEvent(mouseInput{mouseData: uint32(int32(delta)), flags: eventWheel}))
}

// typeText sends characters as UTF-16 code units. Surrogate pairs are emitted as
// two events, which is what Windows expects for anything outside the BMP.
func typeText(text string) error {
	units := windows.StringToUTF16(text)
	units = units[:len(units)-1] // drop the terminating NUL

	events := make([]rawInput, 0, len(units)*2)
	for _, u := range units {
		events = append(events,
			keyEvent(keyboardInput{scan: u, flags: keyUnicode}),
			keyEvent(keyboardInput{scan: u, flags: keyUnicode | keyUp}),
		)
	}
	return send(events...)
}

// pressKey uses the scancode path with the virtual key filled in, so that both
// well-behaved applications and DirectInput ones see the event.
func pressKey(vk uint16, down bool) error {
	scan := uint16(mapVirtualKey(uint32(vk)))
	flags := uint32(keyScan)
	if down {
		return send(keyEvent(keyboardInput{vk: vk, scan: scan, flags: flags}))
	}
	return send(keyEvent(keyboardInput{vk: vk, scan: scan, flags: flags | keyUp}))
}

var mapVirtualKeyProc = user32.NewProc("MapVirtualKeyW")

func mapVirtualKey(vk uint32) uint32 {
	// 0 = MAPVK_VK_TO_VSC
	r, _, _ := mapVirtualKeyProc.Call(uintptr(vk), 0)
	return uint32(r)
}

// cursorPosition is used by the tests and by the health probe to prove that
// injection actually reached the desktop.
func cursorPosition() (int, int, error) {
	var pt struct{ X, Y int32 }
	r, _, err := getCursor.Call(uintptr(unsafe.Pointer(&pt)))
	if r == 0 {
		return 0, 0, fmt.Errorf("GetCursorPos: %w", err)
	}
	return int(pt.X), int(pt.Y), nil
}

func virtualScreenSize() (int, int) {
	w, _, _ := getMetrics.Call(smCXVirtualScreen)
	h, _, _ := getMetrics.Call(smCYVirtualScreen)
	return int(w), int(h)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func pick(cond bool, a, b uint32) uint32 {
	if cond {
		return a
	}
	return b
}

func inputSupported() bool { return true }
