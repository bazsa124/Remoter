package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"remoter/agent/internal/platform"
)

// The Wi-Fi safety net.
//
// Some adapters (a MediaTek MT7921 on its 2022 driver) are slow and
// erratic about auto-connecting at boot: Windows' own logs show it waiting
// anything from 30 seconds to over four minutes before even trying, with the
// profile set to connect automatically. A node that cannot get on the network
// is unreachable, so the service nudges Windows itself: once Wi-Fi has been
// down for wifiGrace, it asks for the network last used, then for other saved,
// secured networks in range.
//
// It only acts when being reachable is the point - during the first minutes
// after boot, and while armed - so it never fights someone who disconnected on
// purpose while using the machine.

const (
	wifiGrace      = 20 * time.Second // let Windows try on its own first
	wifiRetry      = 30 * time.Second // between nudges
	wifiBootWindow = 10 * time.Minute
	wifiTick       = 5 * time.Second
)

// wifiOps is the platform surface the nudger uses; tests replace it.
type wifiOps struct {
	state      func() (platform.WifiState, error)
	connect    func(profile string) error
	candidates func() ([]string, error)
	uptime     func() (time.Duration, error)
	now        func() time.Time
}

type wifiNudger struct {
	ops   wifiOps
	armed func() bool
	path  string // remembered profile, so a reboot knows where to reconnect
	log   *slog.Logger

	remembered  string
	downSince   time.Time
	lastNudge   time.Time
	attempt     int
	unsupported bool
}

type wifiMemory struct {
	Profile string `json:"profile"`
}

func newWifiNudger(dir string, armed func() bool, log *slog.Logger) *wifiNudger {
	w := &wifiNudger{
		ops: wifiOps{
			state:      platform.Wifi,
			connect:    platform.WifiConnect,
			candidates: platform.WifiCandidates,
			uptime:     platform.Uptime,
			now:        time.Now,
		},
		armed: armed,
		path:  filepath.Join(dir, "wifi.json"),
		log:   log,
	}
	if data, err := os.ReadFile(w.path); err == nil {
		var m wifiMemory
		if json.Unmarshal(data, &m) == nil {
			w.remembered = m.Profile
		}
	}
	return w
}

func (w *wifiNudger) run(ctx context.Context) {
	t := time.NewTicker(wifiTick)
	defer t.Stop()
	for {
		w.tick()
		if w.unsupported {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *wifiNudger) tick() {
	st, err := w.ops.state()
	switch {
	case errors.Is(err, platform.ErrUnsupported):
		w.unsupported = true
		return
	case err != nil, !st.Present:
		return // no Wi-Fi (wired, or the adapter is not up yet)
	}
	now := w.ops.now()

	if st.Connected {
		if !w.downSince.IsZero() && !w.lastNudge.IsZero() {
			w.log.Info("Wi-Fi connected", "profile", st.Profile, "after", now.Sub(w.downSince).Round(time.Second))
		}
		w.downSince, w.lastNudge, w.attempt = time.Time{}, time.Time{}, 0
		if st.Profile != "" && st.Profile != w.remembered {
			w.remembered = st.Profile
			w.save()
		}
		return
	}

	if w.downSince.IsZero() {
		w.downSince = now
	}
	if now.Sub(w.downSince) < wifiGrace || (!w.lastNudge.IsZero() && now.Sub(w.lastNudge) < wifiRetry) {
		return
	}
	if !w.mayAct() {
		return
	}
	w.lastNudge = now

	profiles := w.choices()
	if len(profiles) == 0 {
		w.log.Warn("Wi-Fi not connected and no saved network in range")
		return
	}
	// Rotate through the choices on successive nudges: the remembered network
	// first, but not forever if it is gone.
	p := profiles[w.attempt%len(profiles)]
	w.attempt++
	w.log.Info("Wi-Fi not connected; asking Windows to connect",
		"profile", p, "down", now.Sub(w.downSince).Round(time.Second), "attempt", w.attempt)
	if err := w.ops.connect(p); err != nil {
		w.log.Warn("Wi-Fi connect request failed", "profile", p, "err", err)
	}
}

// mayAct: shortly after boot, or while armed. Otherwise a person is probably
// at the machine, and a disconnected Wi-Fi may be exactly what they want.
func (w *wifiNudger) mayAct() bool {
	if up, err := w.ops.uptime(); err == nil && up < wifiBootWindow {
		return true
	}
	return w.armed != nil && w.armed()
}

// choices is the remembered profile, then other saved networks in range.
func (w *wifiNudger) choices() []string {
	inRange, _ := w.ops.candidates()
	out := make([]string, 0, len(inRange)+1)
	if w.remembered != "" {
		out = append(out, w.remembered)
	}
	for _, p := range inRange {
		if p != w.remembered {
			out = append(out, p)
		}
	}
	return out
}

func (w *wifiNudger) save() {
	data, _ := json.Marshal(wifiMemory{Profile: w.remembered})
	if err := os.WriteFile(w.path, data, 0o600); err != nil {
		w.log.Debug("cannot remember Wi-Fi profile", "err", err)
	}
}
