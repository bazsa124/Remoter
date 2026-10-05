package platform

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/aymanbagabas/go-pty"
)

// Console is a running shell attached to a pseudo-terminal.
//
// The plan said not to write a PTY server, on the grounds that SSH already
// solves this portably. That holds for a native client - but a browser cannot
// speak SSH, and Tier 2 has to work from the PWA. Rather than teaching the agent
// to be an SSH client (which would mean it holding a key that grants a full
// shell), it owns the PTY directly. go-pty wraps ConPTY on Windows and the
// classic forkpty path elsewhere, so this stays one implementation.
type Console interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() error
}

type ptyConsole struct {
	pty pty.Pty
	cmd *pty.Cmd
}

func (c *ptyConsole) Read(p []byte) (int, error)  { return c.pty.Read(p) }
func (c *ptyConsole) Write(p []byte) (int, error) { return c.pty.Write(p) }
func (c *ptyConsole) Resize(cols, rows int) error { return c.pty.Resize(cols, rows) }
func (c *ptyConsole) Wait() error                 { return c.cmd.Wait() }

func (c *ptyConsole) Close() error {
	// Closing the pty is what actually terminates the shell; the child holds
	// the slave end and exits when it goes away.
	return c.pty.Close()
}

// DefaultShell returns the interactive shell for this host.
//
// Honours the usual environment overrides first so that a user who has chosen
// a shell gets it, rather than whatever this function thinks is normal.
func DefaultShell() (string, []string) {
	if runtime.GOOS == "windows" {
		if ps, err := exeLookup("pwsh.exe"); err == nil {
			return ps, []string{"-NoLogo"}
		}
		return "powershell.exe", []string{"-NoLogo"}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh, []string{"-l"}
	}
	return "/bin/sh", nil
}

// StartConsole launches a shell on a new pseudo-terminal.
func StartConsole(name string, args []string, cols, rows int) (Console, error) {
	p, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("%w: allocate pty: %v", ErrUnsupported, err)
	}

	if err := p.Resize(cols, rows); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("size pty: %w", err)
	}

	// Resolve the shell on PATH first: once Dir is set below, go-pty looks a
	// bare name up relative to Dir ("C:\Users\me\powershell.exe") and fails.
	if path, err := exeLookup(name); err == nil {
		name = path
	}
	cmd := p.Command(name, args...)
	// Start where an interactive shell would: the user's home, not wherever the
	// node binary happens to live.
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	// Ask for UTF-8 explicitly. Without it a Windows console falls back to a
	// legacy codepage and mangles accented output - the same trap the job
	// runner already guards against.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "LANG=en_US.UTF-8")

	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	return &ptyConsole{pty: p, cmd: cmd}, nil
}
