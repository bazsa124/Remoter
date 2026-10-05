package api

import (
	"testing"
	"time"

	"remoter/agent/internal/platform"
)

// What frame rate can this host actually sustain?
//
// Measures exactly what the server runs, so the FPS cap is chosen from evidence
// rather than hope. Reports both scaling filters, because the handler picks
// between them by frame rate.
func TestLivePipelineBudget(t *testing.T) {
	if _, err := platform.Monitors(); err != nil {
		t.Skip("no desktop:", err)
	}

	for _, fast := range []bool{false, true} {
		t.Logf("--- fast=%v ---", fast)
		measurePipeline(t, fast)
	}
}

func measurePipeline(t *testing.T, fast bool) {
	const runs = 12

	// FPS only selects the scaling filter here; scale is what sets the frame size.
	cfg := liveConfig{FPS: 3, Quality: 35, Scale: 0.4}
	if fast {
		cfg.FPS = 20
	}

	var capture, encoding time.Duration
	var cols, rows int
	var hashes []uint64
	var coder tileCoder

	for i := 0; i < runs; i++ {
		t0 := time.Now()
		frame, err := grabScaled(cfg)
		if err != nil {
			t.Fatalf("grabScaled: %v", err)
		}
		capture += time.Since(t0)

		b := frame.Bounds()
		if hashes == nil {
			cols = (b.Dx() + tileSize - 1) / tileSize
			rows = (b.Dy() + tileSize - 1) / tileSize
			hashes = make([]uint64, cols*rows)
			t.Logf("frame %dx%d, %d tiles", b.Dx(), b.Dy(), cols*rows)
		}

		t1 := time.Now()
		coder.encode(frame, cfg.Quality, cols, rows, hashes, i == 0)
		coder.commit(hashes)
		encoding += time.Since(t1)
	}

	avg := func(d time.Duration) time.Duration { return d / runs }
	total := avg(capture) + avg(encoding)

	t.Logf("capture+scale  %v", avg(capture))
	t.Logf("tiles          %v", avg(encoding))
	t.Logf("TOTAL          %v  -> sustainable %.1f fps", total, float64(time.Second)/float64(total))
}
