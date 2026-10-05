//go:build windows

package platform

import (
	"fmt"
	"sort"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Native Wifi API (wlanapi.dll), used directly rather than through
// `netsh wlan`, whose output is localised - on a Hungarian Windows "connected"
// is "csatlakoztatva" - and so cannot be parsed reliably. It works from the
// SYSTEM service before anyone signs in, which is the whole point: an
// All-User profile can be connected at the boot screen.

var (
	wlanapi                  = windows.NewLazySystemDLL("wlanapi.dll")
	procWlanOpenHandle       = wlanapi.NewProc("WlanOpenHandle")
	procWlanCloseHandle      = wlanapi.NewProc("WlanCloseHandle")
	procWlanEnumInterfaces   = wlanapi.NewProc("WlanEnumInterfaces")
	procWlanQueryInterface   = wlanapi.NewProc("WlanQueryInterface")
	procWlanGetAvailNetworks = wlanapi.NewProc("WlanGetAvailableNetworkList")
	procWlanScan             = wlanapi.NewProc("WlanScan")
	procWlanConnect          = wlanapi.NewProc("WlanConnect")
	procWlanFreeMemory       = wlanapi.NewProc("WlanFreeMemory")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
)

const (
	wlanStateConnected    = 1
	wlanOpcodeConnection  = 7 // wlan_intf_opcode_current_connection
	wlanModeProfile       = 0 // wlan_connection_mode_profile
	dot11BssInfrastruct   = 1
	wlanNetHasProfile     = 0x2 // WLAN_AVAILABLE_NETWORK_HAS_PROFILE
	wlanNetConnected      = 0x1 // WLAN_AVAILABLE_NETWORK_CONNECTED
	errorServiceNotActive = 1062
)

// wlanInterfaceInfo mirrors WLAN_INTERFACE_INFO (532 bytes).
type wlanInterfaceInfo struct {
	GUID        windows.GUID
	Description [256]uint16
	State       uint32
}

// wlanConnectionAttributesHead is the leading part of WLAN_CONNECTION_ATTRIBUTES;
// nothing after the profile name is needed.
type wlanConnectionAttributesHead struct {
	State   uint32
	Mode    uint32
	Profile [256]uint16
}

// wlanAvailableNetwork mirrors WLAN_AVAILABLE_NETWORK (628 bytes).
type wlanAvailableNetwork struct {
	Profile          [256]uint16
	SSIDLength       uint32
	SSID             [32]byte
	BssType          uint32
	NumberOfBssids   uint32
	Connectable      int32
	NotConnectable   uint32
	NumberOfPhyTypes uint32
	PhyTypes         [8]uint32
	MorePhyTypes     int32
	SignalQuality    uint32
	SecurityEnabled  int32
	AuthAlgorithm    uint32
	CipherAlgorithm  uint32
	Flags            uint32
	Reserved         uint32
}

// wlanConnectionParameters mirrors WLAN_CONNECTION_PARAMETERS (40 bytes on amd64).
type wlanConnectionParameters struct {
	Mode    uint32
	_       uint32
	Profile *uint16
	SSID    uintptr
	Bssids  uintptr
	BssType uint32
	Flags   uint32
}

func wlanErr(fn string, code uintptr) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", fn, windows.Errno(code))
}

// withWifi opens a WLAN client handle, finds the first interface, and runs fn.
func withWifi(fn func(h windows.Handle, iface *wlanInterfaceInfo) error) (present bool, err error) {
	var negotiated uint32
	var h windows.Handle
	if r, _, _ := procWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&h))); r != 0 {
		if r == errorServiceNotActive {
			return false, nil // WLAN AutoConfig not running: no Wi-Fi to manage
		}
		return false, wlanErr("WlanOpenHandle", r)
	}
	defer procWlanCloseHandle.Call(uintptr(h), 0)

	var list unsafe.Pointer
	if r, _, _ := procWlanEnumInterfaces.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&list))); r != 0 {
		return false, wlanErr("WlanEnumInterfaces", r)
	}
	defer procWlanFreeMemory.Call(uintptr(list))

	count := *(*uint32)(list)
	if count == 0 {
		return false, nil
	}
	// The items start after two DWORDs: dwNumberOfItems, dwIndex.
	iface := (*wlanInterfaceInfo)(unsafe.Add(list, 8))
	return true, fn(h, iface)
}

func wifiState() (WifiState, error) {
	var st WifiState
	present, err := withWifi(func(h windows.Handle, iface *wlanInterfaceInfo) error {
		if iface.State != wlanStateConnected {
			return nil
		}
		st.Connected = true
		var size, valueType uint32
		var data unsafe.Pointer
		r, _, _ := procWlanQueryInterface.Call(uintptr(h), uintptr(unsafe.Pointer(&iface.GUID)), wlanOpcodeConnection, 0,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&valueType)))
		if r != 0 {
			return nil // connected, profile unknown: not worth failing for
		}
		defer procWlanFreeMemory.Call(uintptr(data))
		attrs := (*wlanConnectionAttributesHead)(data)
		st.Profile = windows.UTF16ToString(attrs.Profile[:])
		return nil
	})
	st.Present = present
	return st, err
}

func wifiConnect(profile string) error {
	name, err := windows.UTF16PtrFromString(profile)
	if err != nil {
		return err
	}
	present, err := withWifi(func(h windows.Handle, iface *wlanInterfaceInfo) error {
		params := wlanConnectionParameters{Mode: wlanModeProfile, Profile: name, BssType: dot11BssInfrastruct}
		r, _, _ := procWlanConnect.Call(uintptr(h), uintptr(unsafe.Pointer(&iface.GUID)), uintptr(unsafe.Pointer(&params)), 0)
		return wlanErr("WlanConnect", r)
	})
	if err == nil && !present {
		return fmt.Errorf("no Wi-Fi interface")
	}
	return err
}

func wifiCandidates() ([]string, error) {
	type seen struct {
		profile string
		signal  uint32
	}
	var found []seen
	_, err := withWifi(func(h windows.Handle, iface *wlanInterfaceInfo) error {
		// A scan is asynchronous; this call reports the cached list, and the
		// next one benefits from the scan requested here.
		procWlanScan.Call(uintptr(h), uintptr(unsafe.Pointer(&iface.GUID)), 0, 0, 0)

		var list unsafe.Pointer
		r, _, _ := procWlanGetAvailNetworks.Call(uintptr(h), uintptr(unsafe.Pointer(&iface.GUID)), 0, 0, uintptr(unsafe.Pointer(&list)))
		if r != 0 {
			return wlanErr("WlanGetAvailableNetworkList", r)
		}
		defer procWlanFreeMemory.Call(uintptr(list))
		count := *(*uint32)(list)
		items := unsafe.Slice((*wlanAvailableNetwork)(unsafe.Add(list, 8)), count)
		for i := range items {
			n := &items[i]
			// Saved, connectable and secured only. An open network is usually a
			// guest Wi-Fi behind a captive portal: joining it gains nothing.
			if n.Flags&wlanNetHasProfile == 0 || n.Connectable == 0 || n.SecurityEnabled == 0 {
				continue
			}
			found = append(found, seen{windows.UTF16ToString(n.Profile[:]), n.SignalQuality})
		}
		return nil
	})
	sort.SliceStable(found, func(i, j int) bool { return found[i].signal > found[j].signal })
	out := make([]string, 0, len(found))
	dup := map[string]bool{}
	for _, f := range found {
		if f.profile != "" && !dup[f.profile] {
			dup[f.profile] = true
			out = append(out, f.profile)
		}
	}
	return out, err
}

func uptime() (time.Duration, error) {
	ms, _, _ := procGetTickCount64.Call()
	return time.Duration(ms) * time.Millisecond, nil
}
