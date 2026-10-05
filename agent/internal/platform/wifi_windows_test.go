//go:build windows

package platform

import (
	"testing"
	"unsafe"
)

// The WLAN structs are read straight out of memory the API allocates; a wrong
// size here does not error, it silently reads garbage.
func TestWlanStructSizes(t *testing.T) {
	for name, got := range map[string]uintptr{
		"WLAN_INTERFACE_INFO":        unsafe.Sizeof(wlanInterfaceInfo{}),
		"WLAN_AVAILABLE_NETWORK":     unsafe.Sizeof(wlanAvailableNetwork{}),
		"WLAN_CONNECTION_PARAMETERS": unsafe.Sizeof(wlanConnectionParameters{}),
	} {
		want := map[string]uintptr{
			"WLAN_INTERFACE_INFO":        532,
			"WLAN_AVAILABLE_NETWORK":     628,
			"WLAN_CONNECTION_PARAMETERS": 40,
		}[name]
		if got != want {
			t.Errorf("%s: %d bytes, want %d", name, got, want)
		}
	}
}

// Read-only: reports the live state without changing any connection.
func TestWifiReadOnly(t *testing.T) {
	st, err := Wifi()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("present=%v connected=%v profile=%q", st.Present, st.Connected, st.Profile)
	if !st.Present {
		t.Skip("no Wi-Fi interface on this machine")
	}
	c, err := WifiCandidates()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("saved networks in range: %q", c)
	up, _ := Uptime()
	t.Logf("uptime %s", up.Round(1e9))
}
