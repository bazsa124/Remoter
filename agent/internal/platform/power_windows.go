//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Arming, Windows edition.
//
// Power settings are never mutated in place. Arming switches to a dedicated
// "Remoter Armed" scheme - a copy of whatever was active, with sleep,
// hibernate and lid-close disabled on AC and battery - and disarming switches
// back. Nothing to restore, nothing to get wrong, the owner's scheme untouched.
// It is the same scheme the old remote-mode.ps1 created, so an existing one is
// reused as-is.

const armedSchemeName = "Remoter Armed"

// The stock Balanced scheme: the fallback when the scheme active before arming
// is unknown (state lost, or arming happened outside the node).
const balancedScheme = "381b4222-f694-41f0-9685-ff5bb260df2e"

const (
	subSleep      = "238c9fa8-0aad-41ed-83f4-97be242c8f20"
	standbyIdle   = "29f6c1db-86da-48c5-9fdb-f2b67b1f44da"
	hibernateIdle = "9d7815a6-7ee4-497e-8888-515a05f02364"
	subButtons    = "4f971e89-eebd-4455-a8de-9e59040e7347"
	lidAction     = "5ca83367-6e45-459f-a27b-476b1d01c936"
)

var guidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func powercfg(args ...string) (string, error) {
	cmd := exec.Command("powercfg.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("powercfg %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// activeScheme parses the GUID only: powercfg's prose is localised, the GUID is not.
func activeScheme() (string, error) {
	out, err := powercfg("/getactivescheme")
	if err != nil {
		return "", err
	}
	g := guidPattern.FindString(out)
	if g == "" {
		return "", fmt.Errorf("powercfg: no active scheme in %q", out)
	}
	return strings.ToLower(g), nil
}

func findScheme(name string) (string, error) {
	out, err := powercfg("/list")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "("+name+")") {
			if g := guidPattern.FindString(line); g != "" {
				return strings.ToLower(g), nil
			}
		}
	}
	return "", nil
}

func createArmedScheme(from string) (string, error) {
	out, err := powercfg("/duplicatescheme", from)
	if err != nil {
		return "", err
	}
	g := strings.ToLower(guidPattern.FindString(strings.ReplaceAll(out, from, "")))
	if g == "" {
		return "", fmt.Errorf("powercfg: no GUID in %q", out)
	}
	if _, err := powercfg("/changename", g, armedSchemeName, "Keeps the host reachable while away. Created by Remoter."); err != nil {
		return "", err
	}
	// Never sleep, never hibernate, lid close does nothing - on AC and battery
	// both: "armed" means reachable whatever was done with the lid. The battery
	// side is bounded by the node's low-battery auto-disarm.
	for _, mode := range []string{"/setacvalueindex", "/setdcvalueindex"} {
		for _, kv := range [][2]string{{subSleep, standbyIdle}, {subSleep, hibernateIdle}, {subButtons, lidAction}} {
			if _, err := powercfg(mode, g, kv[0], kv[1], "0"); err != nil {
				return "", err
			}
		}
	}
	return g, nil
}

func armPower() (string, error) {
	active, err := activeScheme()
	if err != nil {
		return "", err
	}
	armed, err := findScheme(armedSchemeName)
	if err != nil {
		return "", err
	}
	if armed == "" {
		if armed, err = createArmedScheme(active); err != nil {
			return "", err
		}
	}
	if active == armed {
		return "", nil // already on it; the caller keeps whatever it recorded before
	}
	if _, err := powercfg("/setactive", armed); err != nil {
		return "", err
	}
	return active, nil
}

func restorePower(previous string) error {
	if previous == "" {
		previous = balancedScheme
	}
	_, err := powercfg("/setactive", previous)
	return err
}

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
	procGetTickCount         = kernel32.NewProc("GetTickCount")
	procSetThreadExecState   = kernel32.NewProc("SetThreadExecutionState")
	procGetLastInputInfo     = user32.NewProc("GetLastInputInfo")
)

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

func powerStatus() (PowerStatus, error) {
	var s systemPowerStatus
	if ok, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s))); ok == 0 {
		return PowerStatus{}, fmt.Errorf("GetSystemPowerStatus: %w", err)
	}
	ps := PowerStatus{
		OnAC:       s.ACLineStatus == 1,
		HasBattery: s.BatteryFlag != 128 && s.BatteryFlag != 255,
		Percent:    -1,
	}
	if s.BatteryLifePercent <= 100 {
		ps.Percent = int(s.BatteryLifePercent)
	}
	return ps, nil
}

// idleTime is per session, so it must be asked from inside the console session:
// from the screen helper or an interactive process, never from session 0.
func idleTime() (uint32, error) {
	info := struct{ size, time uint32 }{size: 8}
	if ok, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}
	now, _, _ := procGetTickCount.Call()
	return uint32(now) - info.time, nil // uint32 arithmetic survives the 49-day wrap
}

// Keeping awake.
//
// SetThreadExecutionState is per THREAD and lapses when that thread exits. Go
// moves goroutines between threads freely, so the request is held by one locked
// thread that owns it for the life of the process.
var awake struct {
	once    sync.Once
	mu      sync.Mutex
	system  int
	display int
	changed chan struct{}
}

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

func holdAwake(display bool) func() {
	awake.once.Do(func() {
		awake.changed = make(chan struct{}, 1)
		go func() {
			runtime.LockOSThread()
			for range awake.changed {
				awake.mu.Lock()
				flags := uintptr(esContinuous)
				if awake.system > 0 {
					flags |= esSystemRequired
				}
				if awake.display > 0 {
					flags |= esDisplayRequired
				}
				awake.mu.Unlock()
				procSetThreadExecState.Call(flags)
			}
		}()
	})

	adjust := func(delta int) {
		awake.mu.Lock()
		awake.system += delta
		if display {
			awake.display += delta
		}
		awake.mu.Unlock()
		select {
		case awake.changed <- struct{}{}:
		default:
		}
	}
	adjust(+1)
	var once sync.Once
	return func() { once.Do(func() { adjust(-1) }) }
}
