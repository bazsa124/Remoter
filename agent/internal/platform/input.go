package platform

// Input is the Tier 3 injection surface. Coordinates are normalised to the
// virtual desktop so callers never need the host's geometry. Every call goes
// through onDesktop, so in the screen helper it lands on whatever desktop has
// input - the lock screen included.

// InputSupported reports whether this host can inject input at all.
func InputSupported() bool { return inputSupported() }

// MoveMouse positions the cursor. x and y are 0..1 across the virtual desktop.
func MoveMouse(x, y float64) (err error) {
	onDesktop(func() { err = moveMouse(x, y) })
	return err
}

// ClickMouse presses or releases "left", "right" or "middle".
func ClickMouse(button string, down bool) (err error) {
	onDesktop(func() { err = clickMouse(button, down) })
	return err
}

// ScrollMouse scrolls by wheel detents (120 per notch, positive is away).
func ScrollMouse(delta int) (err error) {
	onDesktop(func() { err = scrollMouse(delta) })
	return err
}

// TypeText enters characters directly, bypassing the host keyboard layout.
func TypeText(text string) (err error) {
	onDesktop(func() { err = typeText(text) })
	return err
}

// PressKey sends a virtual key with its scancode filled in.
func PressKey(vk uint16, down bool) (err error) {
	onDesktop(func() { err = pressKey(vk, down) })
	return err
}

// CursorPosition returns the pointer location in pixels.
func CursorPosition() (x, y int, err error) {
	onDesktop(func() { x, y, err = cursorPosition() })
	return x, y, err
}

// VirtualScreenSize returns the bounding size of all monitors.
func VirtualScreenSize() (w, h int) {
	onDesktop(func() { w, h = virtualScreenSize() })
	return w, h
}
