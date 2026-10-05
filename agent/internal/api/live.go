package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"image"
	"image/jpeg"
	"net/http"
	"time"

	"github.com/coder/websocket"
	xdraw "golang.org/x/image/draw"

	"remoter/agent/internal/platform"
)

// Tier 3 - "Live", the top of the ladder.
//
// The gap in the ladder was between Glance (one still, ~60 KB) and full video
// (Mbps). This fills it: a low frame rate, heavily scaled, tile-differenced
// desktop you can actually click.
//
// The trick that makes it cheap is that a desktop is mostly static. Each frame
// is cut into tiles, and only tiles whose contents changed are encoded and sent.
// Typing in an editor touches a handful of tiles; an idle screen sends nothing
// at all. That is what keeps this in the hundreds of kbps rather than Mbps, and
// it is why this tier is worth having instead of just running Glance on a timer.

const (
	tileSize      = 128 // pixels, on the scaled image
	liveMaxFPS    = 30
	liveMinFPS    = 1
	keyframeEvery = 60 // resend everything periodically to heal any lost tile
)

type liveConfig struct {
	FPS     int     `json:"fps"`
	Quality int     `json:"quality"`
	Scale   float64 `json:"scale"`
	Monitor int     `json:"monitor"`
}

// liveMessage is what the client sends: a settings change or an input event.
type liveMessage struct {
	Type string `json:"type"`

	// config
	Config *liveConfig `json:"config,omitempty"`

	// input
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"`
	Down   bool    `json:"down,omitempty"`
	Delta  int     `json:"delta,omitempty"`
	Text   string  `json:"text,omitempty"`
	Key    uint16  `json:"key,omitempty"`

	// drag: a path in normalised coordinates, an optional rest before moving,
	// and how long the movement takes.
	Points [][]float64 `json:"points,omitempty"`
	Hold   int         `json:"hold,omitempty"`
	Ms     int         `json:"ms,omitempty"`
}

func (c *liveConfig) clamp() {
	if c.FPS < liveMinFPS {
		c.FPS = 2
	}
	if c.FPS > liveMaxFPS {
		c.FPS = liveMaxFPS
	}
	if c.Quality < 10 || c.Quality > 90 {
		c.Quality = 35
	}
	if c.Scale < 0.15 || c.Scale > 1 {
		c.Scale = 0.4
	}
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	if !platform.InputSupported() {
		// Video without input is just Glance on a timer, which is not a tier.
		writeError(w, http.StatusNotImplemented, "input injection is not supported on this host")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  []string{"*"},
		CompressionMode: websocket.CompressionDisabled, // JPEG is already compressed
	})
	if err != nil {
		s.Log.Warn("live: accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	cfg := liveConfig{FPS: 3, Quality: 35, Scale: 0.4}
	cfg.clamp()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go keepAlive(ctx, conn, cancel)

	settings := make(chan liveConfig, 4)
	resync := make(chan struct{}, 1)

	// Reader goroutine: config changes and input events.
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}

			var msg liveMessage
			if json.Unmarshal(data, &msg) != nil {
				continue
			}

			switch msg.Type {
			case "config":
				if msg.Config != nil {
					msg.Config.clamp()
					select {
					case settings <- *msg.Config:
					default:
					}
				}
			case "move":
				_ = platform.MoveMouse(msg.X, msg.Y)
			case "click":
				// Move first so a tap lands where the finger was, not where the
				// pointer happened to be.
				_ = platform.MoveMouse(msg.X, msg.Y)
				_ = platform.ClickMouse(msg.Button, msg.Down)
			case "scroll":
				_ = platform.ScrollMouse(msg.Delta)
			case "text":
				_ = platform.TypeText(msg.Text)
			case "key":
				_ = platform.PressKey(msg.Key, msg.Down)
			case "drag":
				drag(msg.Points, msg.Hold, msg.Ms)
			case "resync":
				// The client lost a frame - it dropped one to stay inside its
				// memory budget, or the socket reconnected. Without this the
				// missing tiles stay missing until the next scheduled keyframe.
				select {
				case resync <- struct{}{}:
				default:
				}
			}
		}
	}()

	s.Log.Info("live opened", "fps", cfg.FPS, "quality", cfg.Quality, "scale", cfg.Scale)
	defer s.Log.Info("live closed")

	// A display that blanks mid-session leaves nothing worth streaming, and a
	// machine that sleeps takes the session with it.
	release := platform.HoldAwake(true)
	defer release()

	var (
		hashes   []uint64
		frameNo  int
		cols     int
		rows     int
		forceKey bool
		coder    tileCoder
	)

	ticker := time.NewTicker(time.Second / time.Duration(cfg.FPS))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case next := <-settings:
			// A geometry change invalidates the tile cache.
			if next.Scale != cfg.Scale || next.Monitor != cfg.Monitor {
				hashes = nil
			}
			cfg = next
			ticker.Reset(time.Second / time.Duration(cfg.FPS))

		case <-resync:
			forceKey = true

		case <-ticker.C:
			scaled, err := grabScaled(cfg)
			if err != nil {
				_ = conn.Close(websocket.StatusInternalError, truncateReason(err.Error()))
				return
			}
			b := scaled.Bounds()
			newCols := (b.Dx() + tileSize - 1) / tileSize
			newRows := (b.Dy() + tileSize - 1) / tileSize

			if newCols != cols || newRows != rows {
				cols, rows = newCols, newRows
				hashes = make([]uint64, cols*rows)
				frameNo = 0 // force a keyframe
			}

			keyframe := forceKey || frameNo%keyframeEvery == 0
			forceKey = false
			frameNo++

			payload, sent := coder.encode(scaled, cfg.Quality, cols, rows, hashes, keyframe)
			if sent == 0 {
				continue // nothing moved; send nothing at all
			}

			// Bound the write: a client that stops reading must drop the
			// connection cleanly rather than wedge this loop forever. The client
			// reconnects with backoff, so a stall costs a blip, not the tier.
			header := frameHeader(b.Dx(), b.Dy(), cols, rows, sent, keyframe)
			writeCtx, cancelWrite := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(writeCtx, websocket.MessageBinary, append(header, payload...))
			cancelWrite()

			if err != nil {
				// The frame never reached the client, so the tiles it carried are
				// NOT what the client is showing. Leave the hashes untouched and
				// they will be resent.
				coder.discard()
				return
			}
			coder.commit(hashes)
		}
	}
}

// frameHeader describes the frame the following tiles belong to.
//
// Binary rather than JSON: at a few frames a second with small tiles, a JSON
// envelope per frame would be a measurable share of the bandwidth this tier
// exists to save.
func frameHeader(w, h, cols, rows, tiles int, keyframe bool) []byte {
	buf := make([]byte, 13)
	binary.BigEndian.PutUint16(buf[0:], uint16(w))
	binary.BigEndian.PutUint16(buf[2:], uint16(h))
	binary.BigEndian.PutUint16(buf[4:], uint16(cols))
	binary.BigEndian.PutUint16(buf[6:], uint16(rows))
	binary.BigEndian.PutUint16(buf[8:], uint16(tileSize))
	binary.BigEndian.PutUint16(buf[10:], uint16(tiles))
	if keyframe {
		buf[12] = 1
	}
	return buf
}

// grabScaled captures one frame at the requested scale.
//
// Where the platform can downscale during capture it does, which is far cheaper
// than moving a full-resolution frame and shrinking it afterwards. Above 10 fps
// the caller has asked for speed over smoothness, so the cheaper scaling filter
// is used; below that, text stays properly antialiased.
func grabScaled(cfg liveConfig) (*image.RGBA, error) {
	if platform.CaptureScaledSupported() {
		img, err := platform.CaptureScaled(cfg.Monitor, cfg.Scale, cfg.FPS > 10)
		if err == nil {
			if rgba, ok := img.(*image.RGBA); ok {
				return rgba, nil
			}
			return scaleImage(img, 1), nil
		}
		if !errors.Is(err, platform.ErrUnsupported) {
			return nil, err
		}
		// Fall through: the fast path is unavailable, not broken.
	}

	img, err := platform.Capture(cfg.Monitor)
	if err != nil {
		return nil, err
	}
	return scaleImage(img, cfg.Scale), nil
}

// stagedTile is a tile encoded into the current frame but not yet acknowledged
// by a successful write.
type stagedTile struct {
	idx int
	sum uint64
}

// tileCoder holds the scratch buffers for one connection.
//
// The encode loop runs continuously, so allocating a fresh image and buffer per
// tile means the GC is permanently chasing this tier. One goroutine owns a
// coder, so plain reuse is safe and needs no pool.
//
// It also owns the commit discipline. The tile hashes are the server's model of
// what the CLIENT is currently displaying, so updating them at encode time is a
// lie until the frame actually lands: a failed write, or a frame the client
// discards, would leave those tiles marked as delivered and they would never be
// resent until the next keyframe - up to 60 frames of visibly stale screen.
type tileCoder struct {
	pix    []byte       // one tile, tightly packed
	img    image.RGBA   // header pointing at pix
	enc    bytes.Buffer // JPEG output for the current tile
	out    bytes.Buffer // the assembled payload
	staged []stagedTile // pending until commit
}

// encode returns the changed tiles, each as: index(uint16) length(uint32) JPEG
// bytes. Hashes are read but NOT written - call commit once the frame is sent.
func (c *tileCoder) encode(
	src *image.RGBA,
	quality, cols, rows int,
	hashes []uint64,
	keyframe bool,
) ([]byte, int) {
	c.out.Reset()
	c.staged = c.staged[:0]

	width := src.Bounds().Dx()
	height := src.Bounds().Dy()

	for ty := 0; ty < rows; ty++ {
		for tx := 0; tx < cols; tx++ {
			x0, y0 := tx*tileSize, ty*tileSize
			w := min(tileSize, width-x0)
			h := min(tileSize, height-y0)
			if w <= 0 || h <= 0 {
				continue
			}

			// Pack the tile into a contiguous buffer: it makes the hash a single
			// pass and lets the image header be reused as-is.
			need := w * h * 4
			if cap(c.pix) < need {
				c.pix = make([]byte, need)
			}
			c.pix = c.pix[:need]

			rowBytes := w * 4
			for y := 0; y < h; y++ {
				srcOff := src.PixOffset(x0, y0+y)
				copy(c.pix[y*rowBytes:(y+1)*rowBytes], src.Pix[srcOff:srcOff+rowBytes])
			}

			sum := hashPixels(c.pix)
			idx := ty*cols + tx
			if !keyframe && hashes[idx] == sum {
				continue
			}

			c.img.Pix = c.pix
			c.img.Stride = rowBytes
			c.img.Rect = image.Rect(0, 0, w, h)

			c.enc.Reset()
			if jpeg.Encode(&c.enc, &c.img, &jpeg.Options{Quality: quality}) != nil {
				continue
			}

			var head [6]byte
			binary.BigEndian.PutUint16(head[0:], uint16(idx))
			binary.BigEndian.PutUint32(head[2:], uint32(c.enc.Len()))
			c.out.Write(head[:])
			c.out.Write(c.enc.Bytes())
			c.staged = append(c.staged, stagedTile{idx: idx, sum: sum})
		}
	}
	return c.out.Bytes(), len(c.staged)
}

// commit records that the client has received the staged tiles.
func (c *tileCoder) commit(hashes []uint64) {
	for _, t := range c.staged {
		if t.idx < len(hashes) {
			hashes[t.idx] = t.sum
		}
	}
	c.staged = c.staged[:0]
}

// discard forgets the staged tiles, so they are encoded again next frame.
func (c *tileCoder) discard() { c.staged = c.staged[:0] }

func hashPixels(pix []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(pix)
	return h.Sum64()
}

func scaleImage(src image.Image, scale float64) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	return dst
}

// drag presses at the first point, moves through the rest over ms and
// releases: a real mouse drag, for moving windows, selecting text and pulling
// sliders - none of which a click can do. It runs on the reader goroutine, so
// input arriving meanwhile waits its turn rather than interleaving with it.
func drag(points [][]float64, holdMs, ms int) {
	if len(points) < 2 {
		return
	}
	at := func(p []float64) {
		if len(p) >= 2 {
			_ = platform.MoveMouse(p[0], p[1])
		}
	}
	at(points[0])
	_ = platform.ClickMouse("left", true)
	if holdMs > 0 {
		time.Sleep(time.Duration(min(holdMs, 1500)) * time.Millisecond)
	}
	step := time.Duration(max(5, min(max(ms, 50), 3000)/len(points))) * time.Millisecond
	for _, p := range points[1:] {
		time.Sleep(step)
		at(p)
	}
	_ = platform.ClickMouse("left", false)
}
