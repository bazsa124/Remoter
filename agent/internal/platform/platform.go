// Package platform isolates every OS-specific call in the agent.
//
// Rule: no other package may import "syscall", branch on runtime.GOOS, or build
// a platform-specific path. Implementations live in build-tagged files, and an
// unimplemented platform must still COMPILE - returning ErrUnsupported - so that
// a broken port shows up as a failing feature, not a failing build.
package platform

import (
	"errors"
	"image"
	"os/exec"
)

// ErrUnsupported is returned by capabilities absent on the current OS.
var ErrUnsupported = errors.New("not supported on this platform")

// ErrNoSuchMonitor means the caller asked for a display that does not exist -
// a bad request, not a host failure.
var ErrNoSuchMonitor = errors.New("no such monitor")

// Harden applies OS-specific process attributes before start, so that the whole
// child tree can be killed later. Without it, cancelling a job kills the shell
// and orphans everything it spawned.
func Harden(cmd *exec.Cmd) { harden(cmd) }

// KillTree terminates a process and its descendants.
func KillTree(cmd *exec.Cmd) error { return killTree(cmd) }

// IsElevated reports whether the agent runs with administrative privilege.
//
// Tier 2 grants a shell, so this decides whether that shell is an admin shell.
// Surfaced through /api/health rather than left to assumption.
func IsElevated() bool { return isElevated() }

// Monitor describes one display.
type Monitor struct {
	Index   int  `json:"index"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Primary bool `json:"primary"`
}

// Monitors enumerates displays. Phase 2.
func Monitors() (list []Monitor, err error) {
	onDesktop(func() { list, err = monitors() })
	return list, err
}

// FollowInputDesktop makes every capture and input call track the desktop that
// currently has input - including the lock screen and UAC prompts on Windows.
// Only the screen helper, running as SYSTEM in the console session, may use it:
// an ordinary process lacks the access to attach to the secure desktop.
func FollowInputDesktop() { followInputDesktop() }

// InputDesktop names the desktop with input ("Default", "Winlogon"), or "" when
// the concept does not apply. It is how the client learns it is looking at the
// lock screen rather than at a frozen desktop.
func InputDesktop() string { return inputDesktopName() }

// CaptureScaled grabs one display already downscaled by scale (0 < scale <= 1).
//
// Scaling during capture is far cheaper than capturing full resolution and
// shrinking afterwards, because the full-size frame never leaves the graphics
// layer. The scale factor is passed rather than a pixel size because only the
// platform knows the display's true dimensions - GetSystemMetrics reports
// DPI-scaled logical pixels and would halve the result on this panel. Returns
// ErrUnsupported where no fast path exists; callers fall back to Capture plus
// their own scaling. Set fast to trade downscale quality for speed.
func CaptureScaled(index int, scale float64, fast bool) (img image.Image, err error) {
	onDesktop(func() { img, err = captureScaled(index, scale, fast) })
	return img, err
}

// CaptureScaledSupported reports whether the fast path exists here.
func CaptureScaledSupported() bool { return captureScaledSupported() }

// Capture grabs one display. Phase 2.
//
// On Windows this is expected to fail from session 0: a service does not own a
// desktop, and the capture must be delegated to a helper running in the active
// console session. The error is the signal to do that, not a bug to paper over.
func Capture(index int) (img image.Image, err error) {
	onDesktop(func() { img, err = capture(index) })
	return img, err
}
