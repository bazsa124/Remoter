//go:build !windows

package platform

import "errors"

// Session plumbing exists for the Windows service, which must reach from
// session 0 into the console session. Elsewhere the node runs where the user is.

// ConsoleSession reports no console session.
func ConsoleSession() (uint32, bool) { return 0, false }

// SessionUser reports nobody.
func SessionUser(uint32) string { return "" }

// Process is a placeholder; nothing spawns helpers off Windows.
type Process struct{ Pid uint32 }

// Alive is always false.
func (p *Process) Alive() bool { return false }

// Kill does nothing.
func (p *Process) Kill() {}

// SpawnOptions mirrors the Windows type so callers compile everywhere.
type SpawnOptions struct {
	Session uint32
	AsUser  bool
	Exe     string
	Args    []string
	Env     []string
	Dir     string
	Job     *Job
}

// ErrNoUser mirrors the Windows sentinel.
var ErrNoUser = errors.New("nobody is logged on to the console session")

// SpawnInSession is unsupported.
func SpawnInSession(SpawnOptions) (*Process, error) { return nil, ErrUnsupported }

// Job is a placeholder.
type Job struct{}

// NewJob is unsupported.
func NewJob() (*Job, error) { return nil, ErrUnsupported }
