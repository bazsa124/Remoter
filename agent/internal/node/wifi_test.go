package node

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"remoter/agent/internal/platform"
)

type fakeWifi struct {
	t         time.Time
	up        time.Duration
	connected bool
	profile   string
	inRange   []string
	asked     []string
}

func (f *fakeWifi) ops() wifiOps {
	return wifiOps{
		state: func() (platform.WifiState, error) {
			return platform.WifiState{Present: true, Connected: f.connected, Profile: f.profile}, nil
		},
		connect:    func(p string) error { f.asked = append(f.asked, p); return nil },
		candidates: func() ([]string, error) { return f.inRange, nil },
		uptime:     func() (time.Duration, error) { return f.up, nil },
		now:        func() time.Time { return f.t },
	}
}

func (f *fakeWifi) advance(d time.Duration) {
	f.t = f.t.Add(d)
	f.up += d
}

func newTestNudger(t *testing.T, f *fakeWifi, armed bool) *wifiNudger {
	return &wifiNudger{
		ops:   f.ops(),
		armed: func() bool { return armed },
		path:  filepath.Join(t.TempDir(), "wifi.json"),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The scenario that failed the reboot test: Wi-Fi down at boot. Windows gets
// its grace period, then the remembered network is requested.
func TestWifiNudgedAfterGraceAtBoot(t *testing.T) {
	f := &fakeWifi{t: time.Unix(1_700_000_000, 0), up: 15 * time.Second, inRange: []string{"Guest", "Home"}}
	w := newTestNudger(t, f, false)
	w.remembered = "Home"

	w.tick()
	f.advance(wifiGrace - time.Second)
	w.tick()
	if len(f.asked) != 0 {
		t.Fatalf("nudged inside the grace period: %v", f.asked)
	}
	f.advance(2 * time.Second)
	w.tick()
	if len(f.asked) != 1 || f.asked[0] != "Home" {
		t.Fatalf("want one request for the remembered network, got %v", f.asked)
	}

	// Not retried before wifiRetry; then the next choice gets a turn.
	f.advance(wifiRetry - time.Second)
	w.tick()
	if len(f.asked) != 1 {
		t.Fatalf("retried too soon: %v", f.asked)
	}
	f.advance(2 * time.Second)
	w.tick()
	if len(f.asked) != 2 || f.asked[1] != "Guest" {
		t.Fatalf("want the in-range fallback second, got %v", f.asked)
	}
}

// Someone at the machine who disconnects on purpose must be left alone.
func TestWifiLeftAloneWhenNotBootingOrArmed(t *testing.T) {
	f := &fakeWifi{t: time.Unix(1_700_000_000, 0), up: time.Hour, inRange: []string{"Home"}}
	w := newTestNudger(t, f, false)
	w.remembered = "Home"
	for i := 0; i < 20; i++ {
		w.tick()
		f.advance(10 * time.Second)
	}
	if len(f.asked) != 0 {
		t.Fatalf("nudged a machine that is neither booting nor armed: %v", f.asked)
	}
}

func TestWifiNudgedWhileArmed(t *testing.T) {
	f := &fakeWifi{t: time.Unix(1_700_000_000, 0), up: time.Hour, inRange: []string{"Home"}}
	w := newTestNudger(t, f, true)
	w.tick()
	f.advance(wifiGrace + time.Second)
	w.tick()
	if len(f.asked) != 1 || f.asked[0] != "Home" {
		t.Fatalf("armed machine not nudged: %v", f.asked)
	}
}

// A connection is remembered, so the next boot knows where to reconnect.
func TestWifiRemembersProfile(t *testing.T) {
	f := &fakeWifi{t: time.Unix(1_700_000_000, 0), connected: true, profile: "Home"}
	w := newTestNudger(t, f, false)
	w.tick()
	reloaded := newWifiNudger(filepath.Dir(w.path), nil, w.log)
	if reloaded.remembered != "Home" {
		t.Fatalf("remembered %q after reload", reloaded.remembered)
	}
}
