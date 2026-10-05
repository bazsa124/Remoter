package platform

import "os/exec"

// exeLookup resolves a program on PATH. Kept here so pty.go does not import
// os/exec directly for one call, and so tests can reason about it in isolation.
func exeLookup(name string) (string, error) { return exec.LookPath(name) }
