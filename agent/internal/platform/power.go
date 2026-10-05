package platform

import "time"

// PowerStatus is the host's supply: on mains or battery, and how full.
type PowerStatus struct {
	OnAC       bool `json:"onAC"`
	HasBattery bool `json:"hasBattery"`
	Percent    int  `json:"percent"` // -1 when unknown
}

// ArmPower switches to the never-sleep "armed" power configuration and returns
// the configuration that was active before, for DisarmPower to restore. An empty
// return with a nil error means the host was already armed.
func ArmPower() (previous string, err error) { return armPower() }

// DisarmPower restores the configuration ArmPower replaced.
func DisarmPower(previous string) error { return restorePower(previous) }

// Power reports the power supply.
func Power() (PowerStatus, error) { return powerStatus() }

// IdleTime is how long since the last keyboard or mouse input in this session.
// Injected input counts too - callers that care tell the two apart by timing.
func IdleTime() (time.Duration, error) {
	ms, err := idleTime()
	return time.Duration(ms) * time.Millisecond, err
}

// HoldAwake keeps the machine (and, with display, the screen) awake until the
// returned release function is called. Calls nest.
func HoldAwake(display bool) (release func()) { return holdAwake(display) }
