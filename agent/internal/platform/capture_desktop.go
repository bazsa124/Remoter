//go:build windows || linux

package platform

import (
	"fmt"
	"image"

	"github.com/kbinani/screenshot"
)

// One capture implementation covers Windows (GDI) and Linux (X11), which is the
// point of choosing this library: Tier 1.5 is portable on day one rather than
// being a Windows-only feature that later has to be ported.
//
// macOS is excluded on purpose - its backend needs cgo, which would break the
// darwin cross-compile check that keeps the rest of the agent honest. It falls
// through to the stub and reports ErrUnsupported.

func monitors() ([]Monitor, error) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		// No desktop to capture. On Windows this is what session 0 looks like:
		// a service has no window station, so there is nothing to enumerate.
		return nil, fmt.Errorf("%w: no active display (running without a desktop session?)", ErrUnsupported)
	}

	out := make([]Monitor, 0, n)
	for i := 0; i < n; i++ {
		b := screenshot.GetDisplayBounds(i)
		out = append(out, Monitor{
			Index:   i,
			Width:   b.Dx(),
			Height:  b.Dy(),
			Primary: b.Min.X == 0 && b.Min.Y == 0,
		})
	}
	return out, nil
}

func capture(index int) (image.Image, error) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil, fmt.Errorf("%w: no active display (running without a desktop session?)", ErrUnsupported)
	}
	if index < 0 || index >= n {
		return nil, fmt.Errorf("%w: %d requested, %d active", ErrNoSuchMonitor, index, n)
	}

	img, err := screenshot.CaptureDisplay(index)
	if err != nil {
		return nil, fmt.Errorf("capture display %d: %w", index, err)
	}
	return img, nil
}
