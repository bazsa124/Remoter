//go:build !windows

package platform

import (
	"fmt"
	"os/exec"
	"syscall"
)

func harden(cmd *exec.Cmd) {
	// Own process group, so the whole tree can be signalled at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid signals the process group set up by harden.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process group: %w", err)
	}
	return nil
}
