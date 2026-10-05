//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

func harden(cmd *exec.Cmd) {
	// A new process group makes the child killable as a unit, and keeps a
	// console Ctrl+C from propagating into the agent itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Windows has no process-group signal that reliably reaches grandchildren,
	// so taskkill /T is the supported way to take down the whole tree.
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	harden(kill)
	if out, err := kill.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill: %w: %s", err, out)
	}
	return nil
}
