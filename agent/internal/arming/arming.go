// Package arming keeps a laptop reachable while its owner is away.
//
// A laptop on Wi-Fi cannot be woken remotely, so "reachable" means "never went
// to sleep". Arming switches to a never-sleep power configuration; disarming
// restores the owner's. Because a never-sleeping laptop in a bag is a hazard,
// arming also disarms itself:
//
//   - when the timer chosen at arm time runs out;
//   - on battery below lowBattery percent - sleeping beats dying;
//   - when the owner is back: physical input after the machine sat untouched
//     for awayAfter, while no remote session is open.
//
// All of it runs on the node, so an armed laptop behaves correctly even while
// the hub is unreachable.
package arming

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"remoter/agent/internal/platform"
)

const (
	lowBattery = 20
	checkEvery = 15 * time.Second

	// Input this soon after a remote session ends is still the session's tail
	// (a last keystroke, a release event), not the owner coming back.
	sessionSettle = 10 * time.Second

	// The owner counts as gone only after the machine sat untouched this long
	// while armed. Without it, arming from a phone while still at the laptop
	// would disarm on the very next keypress.
	awayAfter = 10 * time.Minute
)

// Reasons recorded for a disarm; the hub turns the automatic ones into pushes.
const (
	ReasonManual  = "manual"
	ReasonTimer   = "timer"
	ReasonBattery = "low battery"
	ReasonBack    = "local activity"
)

// Disarm is the last time arming ended, and why.
type Disarm struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// State is the arming status, as /api/health reports it.
type State struct {
	Supported  bool       `json:"supported"`
	Armed      bool       `json:"armed"`
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	LastDisarm *Disarm    `json:"lastDisarm,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type persisted struct {
	Armed      bool       `json:"armed"`
	Previous   string     `json:"previous"` // power configuration to restore
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	LastDisarm *Disarm    `json:"lastDisarm,omitempty"`
}

// Controller owns the arming state machine for one host.
type Controller struct {
	path string
	log  *slog.Logger

	// Sessions reports how many remote sessions are open right now.
	Sessions func() int
	// Idle reports time since the last input in the console session. Nil, or an
	// error, disables the "you're back" check rather than guessing.
	Idle func() (time.Duration, error)

	mu          sync.Mutex
	st          persisted
	lastSession time.Time // when the last remote session ended
	awayMark    time.Time // last local input before the owner left; zero = not away yet
	lastErr     string
}

// New loads the arming state stored at path.
func New(path string, log *slog.Logger) *Controller {
	c := &Controller{path: path, log: log}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c.st)
	}
	return c
}

// Supported reports whether this host can arm at all.
func (c *Controller) Supported() bool {
	_, err := platform.Power()
	return !errors.Is(err, platform.ErrUnsupported)
}

// State returns the current status.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return State{
		Supported: c.Supported(), Armed: c.st.Armed, Since: c.st.Since, Until: c.st.Until,
		LastDisarm: c.st.LastDisarm, Error: c.lastErr,
	}
}

func (c *Controller) saveLocked() {
	data, _ := json.MarshalIndent(c.st, "", "  ")
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		c.log.Warn("arming: cannot persist state", "err", err)
	}
}

// Arm keeps the host awake, for d if d > 0, otherwise until disarmed.
func (c *Controller) Arm(d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev, err := platform.ArmPower()
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	if !c.st.Armed {
		c.st.Previous = prev
		now := time.Now().UTC()
		c.st.Since = &now
	} else if prev != "" {
		c.st.Previous = prev // someone switched schemes underneath; follow them
	}
	c.st.Armed = true
	c.awayMark = time.Time{}
	c.st.Until = nil
	if d > 0 {
		until := time.Now().UTC().Add(d)
		c.st.Until = &until
	}
	c.lastErr = ""
	c.saveLocked()
	c.log.Info("armed", "until", c.st.Until)
	return nil
}

// Disarm restores normal power behaviour.
func (c *Controller) Disarm(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disarmLocked(reason)
}

func (c *Controller) disarmLocked(reason string) error {
	if !c.st.Armed {
		return nil
	}
	if err := platform.DisarmPower(c.st.Previous); err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.st = persisted{LastDisarm: &Disarm{At: time.Now().UTC(), Reason: reason}}
	c.awayMark = time.Time{}
	c.lastErr = ""
	c.saveLocked()
	c.log.Info("disarmed", "reason", reason)
	return nil
}

// SessionEnded records that the last remote session closed, so its trailing
// input is not mistaken for the owner returning.
func (c *Controller) SessionEnded() {
	c.mu.Lock()
	c.lastSession = time.Now()
	c.mu.Unlock()
}

// Run applies the automatic disarm rules until stop closes.
func (c *Controller) Run(stop <-chan struct{}) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if reason := c.shouldDisarm(); reason != "" {
				if err := c.Disarm(reason); err != nil {
					c.log.Warn("auto-disarm failed", "reason", reason, "err", err)
				}
			}
		}
	}
}

func (c *Controller) shouldDisarm() string {
	c.mu.Lock()
	armed, until, lastSession := c.st.Armed, c.st.Until, c.lastSession
	c.mu.Unlock()
	if !armed {
		return ""
	}
	if until != nil && time.Now().After(*until) {
		return ReasonTimer
	}
	if ps, err := platform.Power(); err == nil && ps.HasBattery && !ps.OnAC && ps.Percent >= 0 && ps.Percent < lowBattery {
		return ReasonBattery
	}
	if c.Idle == nil || c.Sessions == nil || c.Sessions() > 0 {
		return ""
	}
	idle, err := c.Idle()
	if err != nil {
		return ""
	}
	lastInput := time.Now().Add(-idle)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.awayMark.IsZero() {
		if idle >= awayAfter {
			c.awayMark = lastInput // the owner has left
		}
		return ""
	}
	// Away, no session open - so nothing is injecting input. Input newer than
	// the moment the owner left, and newer than any remote session's tail, is a
	// person at the keyboard.
	if lastInput.After(c.awayMark.Add(time.Second)) && lastInput.After(lastSession.Add(sessionSettle)) {
		return ReasonBack
	}
	return ""
}

// ParseDuration reads an arm duration in hours from a client request.
func ParseDuration(hours float64) (time.Duration, error) {
	if hours < 0 || hours > 24*30 {
		return 0, fmt.Errorf("hours must be between 0 and %d", 24*30)
	}
	return time.Duration(hours * float64(time.Hour)), nil
}
