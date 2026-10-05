//go:build !windows

package platform

import "time"

// Wi-Fi management exists for Windows laptops whose adapter is slow to connect
// at boot. Linux nodes (the hub machine) are wired and managed by NetworkManager/systemd.

func wifiState() (WifiState, error)     { return WifiState{}, ErrUnsupported }
func wifiConnect(string) error          { return ErrUnsupported }
func wifiCandidates() ([]string, error) { return nil, ErrUnsupported }
func uptime() (time.Duration, error)    { return 0, ErrUnsupported }
