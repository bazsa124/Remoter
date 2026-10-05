//go:build !windows

package platform

// Only Windows splits the interactive session into separately attachable
// desktops; everywhere else there is nothing to follow.

func followInputDesktop()      {}
func onDesktop(fn func())      { fn() }
func inputDesktopName() string { return "" }
