//go:build !windows

package platform

// Arming is a laptop concern and only Windows hosts are laptops here so far.
// systemd-inhibit would be the Linux route if that changes.

func armPower() (string, error)         { return "", ErrUnsupported }
func restorePower(string) error         { return ErrUnsupported }
func powerStatus() (PowerStatus, error) { return PowerStatus{}, ErrUnsupported }
func idleTime() (uint32, error)         { return 0, ErrUnsupported }
func holdAwake(display bool) func()     { return func() {} }
