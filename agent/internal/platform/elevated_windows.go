//go:build windows

package platform

import "golang.org/x/sys/windows"

// isElevated reports whether the process holds an elevated token.
//
// This matters because Tier 2 hands out a shell: an elevated agent hands out an
// ADMIN shell. The answer belongs in /api/health so the privilege the token
// grants is visible, not folklore.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
