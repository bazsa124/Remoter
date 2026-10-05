package platform

import "time"

// WifiState is the machine's Wi-Fi connection, as the OS sees it.
type WifiState struct {
	Present   bool   // a Wi-Fi interface exists at all
	Connected bool   // associated and authenticated
	Profile   string // the saved profile in use, when connected
}

// Wifi reports the first Wi-Fi interface's state. ErrUnsupported where the
// platform has no API for it; Present is false on machines without Wi-Fi.
func Wifi() (WifiState, error) { return wifiState() }

// WifiConnect asks the OS to connect to a saved network profile now.
func WifiConnect(profile string) error { return wifiConnect(profile) }

// WifiCandidates lists saved profiles whose networks are currently in range,
// strongest first. It also asks the adapter for a fresh scan, so the next call
// sees networks that were not in the cached list yet.
func WifiCandidates() ([]string, error) { return wifiCandidates() }

// Uptime is how long the OS has been running.
func Uptime() (time.Duration, error) { return uptime() }
