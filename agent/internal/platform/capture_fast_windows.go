//go:build windows

package platform

import (
	"fmt"
	"image"
	"unsafe"

	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows"
)

// Scaled capture for Tier 3.
//
// The straightforward path - grab the full desktop, then downscale on the CPU -
// costs ~76 ms per frame on a 2560x1600 panel, because BitBlt plus GetDIBits
// moves all 16 MB of it every time, and the CPU downscale adds ~13 ms more.
//
// StretchBlt does the downscale inside GDI, so only the scaled image crosses the
// boundary: at 0.4 scale that is 2.6 MB instead of 16 MB, and the separate CPU
// scaling pass disappears entirely.

var (
	gdi32 = windows.NewLazySystemDLL("gdi32.dll")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procStretchBlt         = gdi32.NewProc("StretchBlt")
	procSetStretchBltMode  = gdi32.NewProc("SetStretchBltMode")

	procGetDC     = user32.NewProc("GetDC")
	procReleaseDC = user32.NewProc("ReleaseDC")
)

const (
	srcCopy = 0x00CC0020

	// HALFTONE averages the pixels it discards; COLORONCOLOR simply drops them.
	// Measured on a 2560x1600 panel: 72 ms versus 51 ms per frame. Halftone keeps
	// text readable, which is the whole point of the tier, so it is the default;
	// speed is chosen only when the caller asks for a rate halftone cannot reach.
	stretchHalftone     = 4
	stretchColorOnColor = 3

	biRGB = 0
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// captureScaled grabs one display already downscaled by the given factor.
//
// The factor is applied here rather than by the caller because only this layer
// knows the true source size. GetSystemMetrics reports DPI-scaled logical pixels
// - 1280x800 for a 2560x1600 panel at 200% - so a caller computing the target
// itself silently asks for half the resolution it intended.
func captureScaled(index int, scale float64, fast bool) (image.Image, error) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil, fmt.Errorf("%w: no active display", ErrUnsupported)
	}
	if index < 0 || index >= n {
		return nil, fmt.Errorf("%w: %d requested, %d active", ErrNoSuchMonitor, index, n)
	}

	src := screenshot.GetDisplayBounds(index)
	sw, sh := src.Dx(), src.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, fmt.Errorf("%w: display %d has no area", ErrUnsupported, index)
	}
	if scale <= 0 || scale > 1 {
		scale = 1
	}
	dw := max(16, int(float64(sw)*scale))
	dh := max(16, int(float64(sh)*scale))

	screenDC, _, err := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("GetDC: %w", err)
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, err := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC: %w", err)
	}
	defer procDeleteDC.Call(memDC)

	// Negative height requests a top-down DIB, so rows arrive in the order Go's
	// image types expect rather than bottom-up.
	header := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(dw),
		Height:      -int32(dh),
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}

	var bits unsafe.Pointer
	bitmap, _, err := procCreateDIBSection.Call(
		memDC,
		uintptr(unsafe.Pointer(&header)),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if bitmap == 0 || bits == nil {
		return nil, fmt.Errorf("CreateDIBSection: %w", err)
	}
	defer procDeleteObject.Call(bitmap)

	old, _, _ := procSelectObject.Call(memDC, bitmap)
	defer procSelectObject.Call(memDC, old)

	mode := uintptr(stretchHalftone)
	if fast {
		mode = stretchColorOnColor
	}
	procSetStretchBltMode.Call(memDC, mode)

	ok, _, err := procStretchBlt.Call(
		memDC, 0, 0, uintptr(dw), uintptr(dh),
		screenDC, uintptr(src.Min.X), uintptr(src.Min.Y), uintptr(sw), uintptr(sh),
		srcCopy,
	)
	if ok == 0 {
		return nil, fmt.Errorf("StretchBlt: %w", err)
	}

	// The DIB is BGRA and owned by GDI; copy it out before the bitmap is freed.
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	pixels := unsafe.Slice((*byte)(bits), dw*dh*4)
	for i := 0; i < len(pixels); i += 4 {
		dst.Pix[i+0] = pixels[i+2] // R
		dst.Pix[i+1] = pixels[i+1] // G
		dst.Pix[i+2] = pixels[i+0] // B
		dst.Pix[i+3] = 0xFF        // GDI leaves alpha zeroed
	}
	return dst, nil
}

func captureScaledSupported() bool { return true }
