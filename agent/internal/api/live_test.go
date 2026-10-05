package api

import (
	"image"
	"testing"
)

// The tile hashes are the server's model of what the client is displaying. If a
// frame is not delivered, that model must not advance - otherwise the tiles it
// carried are never resent and the client shows a stale screen until the next
// keyframe.
func TestDiscardedFrameResendsTiles(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 128))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	cols, rows := 2, 1
	hashes := make([]uint64, cols*rows)

	var coder tileCoder

	// First frame: everything is new.
	_, first := coder.encode(img, 35, cols, rows, hashes, true)
	if first != 2 {
		t.Fatalf("first frame sent %d tiles, want 2", first)
	}

	// Pretend the write failed.
	coder.discard()

	// The same unchanged image must be sent again in full.
	_, afterDiscard := coder.encode(img, 35, cols, rows, hashes, false)
	if afterDiscard != 2 {
		t.Errorf("after a failed send, %d tiles resent, want 2", afterDiscard)
	}

	// Now let it land.
	coder.commit(hashes)

	// With the frame acknowledged, an unchanged image sends nothing.
	_, afterCommit := coder.encode(img, 35, cols, rows, hashes, false)
	if afterCommit != 0 {
		t.Errorf("after a successful send, %d tiles resent, want 0", afterCommit)
	}
}
