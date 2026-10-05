//go:build !windows

package platform

// Input injection is Windows-only so far. Linux would use XTest or uinput;
// until then the feature reports itself as unavailable rather than failing to
// compile, per the platform rule.

func moveMouse(x, y float64) error           { return ErrUnsupported }
func clickMouse(button string, d bool) error { return ErrUnsupported }
func scrollMouse(delta int) error            { return ErrUnsupported }
func typeText(text string) error             { return ErrUnsupported }
func pressKey(vk uint16, down bool) error    { return ErrUnsupported }
func cursorPosition() (int, int, error)      { return 0, 0, ErrUnsupported }
func virtualScreenSize() (int, int)          { return 0, 0 }
func inputSupported() bool                   { return false }
