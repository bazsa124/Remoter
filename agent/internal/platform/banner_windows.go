//go:build windows

package platform

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// The session banner: a slim strip at the top of the screen - red dot, who is
// connected - for as long as a session lasts.
//
// The tray icon alone is not a visible indicator on Windows 11: new icons land
// in the hidden overflow until the user pins them, and Do Not Disturb swallows
// the "session started" toast. A topmost banner cannot be hidden that way. It is
// click-through and never takes focus, so it marks the screen without getting
// in the way - the same convention as Windows' own "you are sharing" bar.

var (
	procShowWindow      = user32.NewProc("ShowWindow")
	procSetWindowPos    = user32.NewProc("SetWindowPos")
	procSetLayeredAttrs = user32.NewProc("SetLayeredWindowAttributes")
	procBeginPaint      = user32.NewProc("BeginPaint")
	procEndPaint        = user32.NewProc("EndPaint")
	procInvalidateRect  = user32.NewProc("InvalidateRect")
	procFillRect        = user32.NewProc("FillRect")
	procDrawText        = user32.NewProc("DrawTextW")
	procCreateSolidBr   = gdi32.NewProc("CreateSolidBrush")
	procCreateFont      = gdi32.NewProc("CreateFontW")
	procSetTextColor    = gdi32.NewProc("SetTextColor")
	procSetBkMode       = gdi32.NewProc("SetBkMode")
	procEllipse         = gdi32.NewProc("Ellipse")
	procGetStockObject  = gdi32.NewProc("GetStockObject")
)

const (
	wmPaint = 0x000F

	wsPopup         = 0x80000000
	wsExTopmost     = 0x00000008
	wsExTransparent = 0x00000020
	wsExToolWindow  = 0x00000080
	wsExLayered     = 0x00080000
	wsExNoActivate  = 0x08000000

	swHide           = 0
	swShowNoActivate = 4
	swpNoActivate    = 0x0010
	lwaAlpha         = 0x2

	dtVCenter     = 0x0004
	dtSingleLine  = 0x0020
	dtCalcRect    = 0x0400
	dtNoPrefix    = 0x0800
	dtEndEllipsis = 0x8000

	nullPen       = 8
	bkTransparent = 1
	smCXScreen    = 0

	// Colours as COLORREF (0x00BBGGRR), from the client's palette.
	colorBackground = 0x001D1814 // #14181D
	colorDot        = 0x003539E5 // #E53935
	colorText       = 0x00F0EAE6 // #E6EAF0
)

// hwndTopmost is (HWND)-1.
var hwndTopmost = ^uintptr(0)

type rect struct{ Left, Top, Right, Bottom int32 }

type paintStruct struct {
	hdc       uintptr
	erase     int32
	paint     rect
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

type banner struct {
	hwnd     uintptr
	font     uintptr
	bgBrush  uintptr
	dotBrush uintptr
	text     []uint16
	w, h     int32
	visible  bool
}

const (
	bannerHeight = 30
	bannerPad    = 12
	bannerDot    = 10
	bannerGap    = 8
	bannerMargin = 6 // from the top edge
	bannerMaxW   = 640
)

var theBanner banner

// newBanner creates the (hidden) banner window on the calling thread, which
// must be the tray's window thread.
func newBanner(instance windows.Handle) *banner {
	b := &theBanner
	className := windows.StringToUTF16Ptr("RemoterSessionBanner")
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   windows.NewCallback(bannerWndProc),
		Instance:  instance,
		ClassName: className,
	}
	if atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return nil
	}
	hwnd, _, _ := procCreateWindowEx.Call(
		wsExTopmost|wsExTransparent|wsExToolWindow|wsExLayered|wsExNoActivate,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Remoter session"))),
		wsPopup, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
	if hwnd == 0 {
		return nil
	}
	procSetLayeredAttrs.Call(hwnd, 0, 235, lwaAlpha)

	face := windows.StringToUTF16Ptr("Segoe UI")
	b.font, _, _ = procCreateFont.Call(
		^uintptr(15), // height -16: character height in logical units
		0, 0, 0,
		600, // FW_SEMIBOLD
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(face)))
	b.bgBrush, _, _ = procCreateSolidBr.Call(colorBackground)
	b.dotBrush, _, _ = procCreateSolidBr.Call(colorDot)
	b.hwnd = hwnd
	return b
}

// show displays text, resizing and centring the strip for it.
func (b *banner) show(text string) {
	if b == nil || b.hwnd == 0 {
		return
	}
	b.text, _ = windows.UTF16FromString(text)

	// Measure the text in the banner's own font.
	dc, _, _ := procGetDC.Call(b.hwnd)
	old, _, _ := procSelectObject.Call(dc, b.font)
	measure := rect{Right: bannerMaxW}
	procDrawText.Call(dc, uintptr(unsafe.Pointer(&b.text[0])), ^uintptr(0),
		uintptr(unsafe.Pointer(&measure)), dtSingleLine|dtCalcRect|dtNoPrefix)
	procSelectObject.Call(dc, old)
	procReleaseDC.Call(b.hwnd, dc)

	b.w = bannerPad + bannerDot + bannerGap + min(measure.Right, bannerMaxW) + bannerPad
	b.h = bannerHeight
	screenW, _, _ := getMetrics.Call(smCXScreen)
	x := (int32(screenW) - b.w) / 2

	procSetWindowPos.Call(b.hwnd, hwndTopmost, uintptr(x), bannerMargin,
		uintptr(b.w), uintptr(b.h), swpNoActivate)
	procInvalidateRect.Call(b.hwnd, 0, 1)
	if !b.visible {
		procShowWindow.Call(b.hwnd, swShowNoActivate)
		b.visible = true
	}
}

func (b *banner) hide() {
	if b == nil || b.hwnd == 0 || !b.visible {
		return
	}
	procShowWindow.Call(b.hwnd, swHide)
	b.visible = false
}

func (b *banner) destroy() {
	if b == nil || b.hwnd == 0 {
		return
	}
	procDestroyWindow.Call(b.hwnd)
	for _, h := range []uintptr{b.font, b.bgBrush, b.dotBrush} {
		if h != 0 {
			procDeleteObject.Call(h)
		}
	}
	b.hwnd = 0
}

func bannerWndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	if uint32(message) == wmPaint {
		theBanner.paint(hwnd)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
	return r
}

func (b *banner) paint(hwnd uintptr) {
	var ps paintStruct
	dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	whole := rect{Right: b.w, Bottom: b.h}
	procFillRect.Call(dc, uintptr(unsafe.Pointer(&whole)), b.bgBrush)

	// The red dot.
	oldBrush, _, _ := procSelectObject.Call(dc, b.dotBrush)
	pen, _, _ := procGetStockObject.Call(nullPen)
	oldPen, _, _ := procSelectObject.Call(dc, pen)
	top := (b.h - bannerDot) / 2
	procEllipse.Call(dc, bannerPad, uintptr(top), bannerPad+bannerDot+1, uintptr(top+bannerDot+1))
	procSelectObject.Call(dc, oldPen)
	procSelectObject.Call(dc, oldBrush)

	if len(b.text) == 0 {
		return
	}
	procSetBkMode.Call(dc, bkTransparent)
	procSetTextColor.Call(dc, colorText)
	oldFont, _, _ := procSelectObject.Call(dc, b.font)
	text := rect{Left: bannerPad + bannerDot + bannerGap, Right: b.w - bannerPad, Bottom: b.h}
	procDrawText.Call(dc, uintptr(unsafe.Pointer(&b.text[0])), ^uintptr(0),
		uintptr(unsafe.Pointer(&text)), dtSingleLine|dtVCenter|dtNoPrefix|dtEndEllipsis)
	procSelectObject.Call(dc, oldFont)
}
