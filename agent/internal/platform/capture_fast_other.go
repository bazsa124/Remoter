//go:build !windows

package platform

import "image"

// Without a platform fast path the caller falls back to full capture plus a CPU
// downscale, which is correct but slower.

func captureScaled(index int, scale float64, fast bool) (image.Image, error) {
	return nil, ErrUnsupported
}

func captureScaledSupported() bool { return false }
