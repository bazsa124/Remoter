//go:build windows

package platform

import (
	"math"
	"testing"
	"unsafe"
)

// The Win32 INPUT union has a fixed layout; a wrong offset here does not error,
// it just moves the cursor nowhere. Pin the sizes.
func TestInputStructLayout(t *testing.T) {
	if got := unsafe.Sizeof(rawInput{}); got != 40 {
		t.Errorf("INPUT size = %d, want 40 on amd64", got)
	}
	// dwExtraInfo is ULONG_PTR and aligns to offset 24, so MOUSEINPUT is 32.
	if got := unsafe.Sizeof(mouseInput{}); got != 32 {
		t.Errorf("MOUSEINPUT size = %d, want 32", got)
	}
	if got := unsafe.Sizeof(keyboardInput{}); got != 24 {
		t.Errorf("KEYBDINPUT size = %d, want 24", got)
	}
}

// Prove injection actually reaches the desktop rather than merely returning nil.
func TestMoveMouseMovesCursor(t *testing.T) {
	w, h := VirtualScreenSize()
	if w == 0 || h == 0 {
		t.Skip("no virtual screen (headless session)")
	}

	startX, startY, err := CursorPosition()
	if err != nil {
		t.Skip("no cursor available:", err)
	}
	t.Cleanup(func() {
		_ = MoveMouse(float64(startX)/float64(w), float64(startY)/float64(h))
	})

	const wantX, wantY = 0.25, 0.75
	if err := MoveMouse(wantX, wantY); err != nil {
		t.Fatalf("MoveMouse: %v", err)
	}

	gotX, gotY, err := CursorPosition()
	if err != nil {
		t.Fatalf("CursorPosition: %v", err)
	}

	// Absolute positioning quantises to 1/65535 of the virtual desktop, so allow
	// a few pixels rather than demanding an exact hit.
	const tolerance = 4
	expX, expY := int(wantX*float64(w)), int(wantY*float64(h))
	if math.Abs(float64(gotX-expX)) > tolerance || math.Abs(float64(gotY-expY)) > tolerance {
		t.Errorf("cursor at (%d,%d), want ~(%d,%d)", gotX, gotY, expX, expY)
	}
}
