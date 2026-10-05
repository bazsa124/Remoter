package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"strconv"

	"golang.org/x/image/draw"

	"remoter/agent/internal/platform"
)

// Tier 1.5 defaults. The plan's target is a frame under 150 KB: half scale at
// quality 60 hits that on a 1080p panel while staying readable enough to answer
// "is a dialog waiting for me?", which is the only question this tier exists for.
const (
	defaultQuality = 60
	defaultScale   = 0.5
	minScale       = 0.1
	maxScale       = 1.0
)

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	monitor, err := intParam(q.Get("monitor"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "monitor: "+err.Error())
		return
	}
	quality, err := intParam(q.Get("quality"), defaultQuality)
	if err != nil {
		writeError(w, http.StatusBadRequest, "quality: "+err.Error())
		return
	}
	if quality < 1 || quality > 100 {
		writeError(w, http.StatusBadRequest, "quality must be 1-100")
		return
	}
	scale, err := floatParam(q.Get("scale"), defaultScale)
	if err != nil {
		writeError(w, http.StatusBadRequest, "scale: "+err.Error())
		return
	}
	if scale < minScale || scale > maxScale {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("scale must be %g-%g", minScale, maxScale))
		return
	}

	img, err := platform.Capture(monitor)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, platform.ErrUnsupported):
			status = http.StatusNotImplemented
		case errors.Is(err, platform.ErrNoSuchMonitor):
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}

	if scale < 1.0 {
		img = resize(img, scale)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		writeError(w, http.StatusInternalServerError, "encode: "+err.Error())
		return
	}

	b := img.Bounds()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("X-Remoter-Bytes", strconv.Itoa(buf.Len()))
	w.Header().Set("X-Remoter-Dimensions", fmt.Sprintf("%dx%d", b.Dx(), b.Dy()))
	// A glance is a point-in-time answer; a cached one is worse than useless.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// resize downsamples with a bilinear kernel: cheap, and smooth enough that JPEG
// does not then waste bits encoding aliasing artefacts.
func resize(src image.Image, scale float64) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func intParam(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func floatParam(raw string, fallback float64) (float64, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseFloat(raw, 64)
}
