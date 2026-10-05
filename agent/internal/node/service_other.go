//go:build !windows

package node

import "errors"

// The service/helper split exists because of Windows session isolation. On
// Linux the node runs standalone under systemd, as the user whose shell it
// offers.

// ServiceName matches the Windows service name, for messages.
const ServiceName = "RemoterNode"

var errNotWindows = errors.New("only Windows nodes run as a service; run the node standalone under systemd")

// IsService is always false off Windows.
func IsService() bool { return false }

// RunService is unsupported off Windows.
func RunService(*Node) error { return errNotWindows }

// InstallService is unsupported off Windows.
func InstallService(string) error { return errNotWindows }

// UninstallService is unsupported off Windows.
func UninstallService() error { return errNotWindows }
