//go:build !windows && !linux

package platform

import "image"

// Platforms without a capture backend still compile - a missing feature must
// surface as a 501 at runtime, never as a broken build.

func monitors() ([]Monitor, error) { return nil, ErrUnsupported }

func capture(int) (image.Image, error) { return nil, ErrUnsupported }
