//go:build !windows

package platform

import "os"

// isElevated reports whether the process runs as root.
func isElevated() bool { return os.Geteuid() == 0 }
