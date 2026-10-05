//go:build windows

package platform

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Session plumbing for the Windows service.
//
// A service lives in session 0, which has no desktop: it cannot see the screen
// or type into it. So it starts helpers inside the *console* session - the one
// attached to the physical screen and keyboard:
//
//   - as SYSTEM, for capture and input, because only SYSTEM may attach to the
//     secure Winlogon desktop that hosts the lock screen and UAC prompts;
//   - as the logged-on user, for the shell and actions, so those keep exactly
//     the user's privilege - never SYSTEM's.

const noSession = 0xFFFFFFFF

// ConsoleSession returns the session attached to the physical console, and
// false while there is none (mid fast-user-switch, for instance).
func ConsoleSession() (uint32, bool) {
	id := windows.WTSGetActiveConsoleSessionId()
	return id, id != noSession
}

// SessionUser returns "DOMAIN\user" logged on to a session, or "" if nobody is.
func SessionUser(session uint32) string {
	var tok windows.Token
	if err := windows.WTSQueryUserToken(session, &tok); err != nil {
		return ""
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return ""
	}
	account, domain, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return ""
	}
	return domain + `\` + account
}

// Process is a helper started into another session.
type Process struct {
	handle windows.Handle
	Pid    uint32
}

// Alive reports whether the process is still running.
func (p *Process) Alive() bool {
	if p == nil || p.handle == 0 {
		return false
	}
	ev, err := windows.WaitForSingleObject(p.handle, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// Kill terminates the process and releases its handle.
func (p *Process) Kill() {
	if p == nil || p.handle == 0 {
		return
	}
	_ = windows.TerminateProcess(p.handle, 1)
	_ = windows.CloseHandle(p.handle)
	p.handle = 0
}

// SpawnOptions describes a helper launch.
type SpawnOptions struct {
	Session uint32
	AsUser  bool // the session's logged-on user; otherwise SYSTEM
	Exe     string
	Args    []string
	Env     []string // added to the base environment
	Dir     string
	Job     *Job // optional: tie the helper's lifetime to the service
}

// ErrNoUser means a user-token launch was asked for a session nobody is logged
// on to - at boot, before sign-in. Not a failure: there is just no user yet.
var ErrNoUser = errors.New("nobody is logged on to the console session")

// SpawnInSession starts a process on winsta0\default of the given session.
//
// Requires SYSTEM (SeTcbPrivilege): retargeting a token's session and reading
// another user's token are exactly the powers a service has and a user lacks.
func SpawnInSession(o SpawnOptions) (*Process, error) {
	var tok windows.Token
	if o.AsUser {
		if err := windows.WTSQueryUserToken(o.Session, &tok); err != nil {
			if errors.Is(err, windows.ERROR_NO_TOKEN) {
				return nil, ErrNoUser
			}
			return nil, fmt.Errorf("WTSQueryUserToken: %w", err)
		}
	} else {
		var self windows.Token
		if err := windows.OpenProcessToken(windows.CurrentProcess(),
			windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|
				windows.TOKEN_ADJUST_SESSIONID|windows.TOKEN_ADJUST_DEFAULT, &self); err != nil {
			return nil, fmt.Errorf("OpenProcessToken: %w", err)
		}
		err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil,
			windows.SecurityImpersonation, windows.TokenPrimary, &tok)
		self.Close()
		if err != nil {
			return nil, fmt.Errorf("DuplicateTokenEx: %w", err)
		}
		session := o.Session
		if err := windows.SetTokenInformation(tok, windows.TokenSessionId,
			(*byte)(unsafe.Pointer(&session)), uint32(unsafe.Sizeof(session))); err != nil {
			tok.Close()
			return nil, fmt.Errorf("SetTokenInformation(TokenSessionId): %w", err)
		}
	}
	defer tok.Close()

	// The user's own environment (profile paths, TEMP), not the service's.
	env, err := tok.Environ(false)
	if err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	env = append(env, o.Env...)

	cmdline := syscall.EscapeArg(o.Exe)
	for _, a := range o.Args {
		cmdline += " " + syscall.EscapeArg(a)
	}

	si := windows.StartupInfo{
		Cb:      uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop: windows.StringToUTF16Ptr(`winsta0\default`),
	}
	var pi windows.ProcessInformation
	var dir *uint16
	if o.Dir != "" {
		dir = windows.StringToUTF16Ptr(o.Dir)
	}
	err = windows.CreateProcessAsUser(tok,
		windows.StringToUTF16Ptr(o.Exe),
		windows.StringToUTF16Ptr(cmdline),
		nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.CREATE_SUSPENDED,
		envBlock(env), dir, &si, &pi)
	if err != nil {
		return nil, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	// Join the job before the first instruction runs, so even a helper that
	// crashes instantly cannot outlive the service as an orphan.
	if o.Job != nil {
		if err := windows.AssignProcessToJobObject(o.Job.handle, pi.Process); err != nil {
			_ = windows.TerminateProcess(pi.Process, 1)
			windows.CloseHandle(pi.Thread)
			windows.CloseHandle(pi.Process)
			return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
		}
	}
	_, _ = windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	return &Process{handle: pi.Process, Pid: pi.ProcessId}, nil
}

// envBlock builds a CREATE_UNICODE_ENVIRONMENT block: NUL-separated
// "key=value" entries ending in an extra NUL. utf16.Encode is used rather than
// windows.StringToUTF16, which refuses the embedded NULs the format is made of.
func envBlock(env []string) *uint16 {
	var b strings.Builder
	for _, kv := range env {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	if len(env) == 0 {
		b.WriteByte(0)
	}
	b.WriteByte(0)
	u := utf16.Encode([]rune(b.String()))
	return &u[0]
}

// Job ties child processes to the service: when the service exits - cleanly or
// not - Windows kills every process in the job. No orphaned SYSTEM helper can
// survive a crashed service.
type Job struct{ handle windows.Handle }

// NewJob creates a kill-on-close job object.
func NewJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{handle: h}, nil
}
