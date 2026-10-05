// Package jobs runs registry actions and tracks their state.
package jobs

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"remoter/agent/internal/actions"
	"remoter/agent/internal/platform"
)

// State is a job's lifecycle position.
type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Done      State = "done"
	Failed    State = "failed"
	Cancelled State = "cancelled"
	TimedOut  State = "timeout"
)

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool {
	switch s {
	case Done, Failed, Cancelled, TimedOut:
		return true
	}
	return false
}

const (
	maxLines     = 2000     // per job
	maxLineBytes = 8 * 1024 // truncate pathological lines

	defaultLimit = 15 * time.Minute
)

// Snapshot is a job's state at an instant: the wire representation, and the
// only form that may be copied. Job itself holds a mutex and is always handled
// by pointer.
type Snapshot struct {
	ID       string     `json:"id"`
	ActionID string     `json:"actionId"`
	Label    string     `json:"label"`
	State    State      `json:"state"`
	Exit     *int       `json:"exit,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`

	// Notify carries the action's push policy so the completion hook can honour
	// it. Not serialised: it is host policy, not client state.
	Notify string `json:"-"`
}

// Job is one execution of an action. The leading fields are immutable after
// construction; everything below the mutex is guarded by it.
type Job struct {
	ID       string
	ActionID string
	Label    string
	Notify   string
	Started  time.Time

	mu      sync.Mutex
	state   State
	exit    *int
	errMsg  string
	ended   *time.Time
	lines   []string
	dropped int
	cmd     *exec.Cmd
	cancel  context.CancelFunc
}

// Snapshot returns a copyable view of the job.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

func (j *Job) snapshotLocked() Snapshot {
	return Snapshot{
		ID: j.ID, ActionID: j.ActionID, Label: j.Label, Notify: j.Notify,
		State: j.state, Exit: j.exit, Error: j.errMsg,
		Started: j.Started, Ended: j.ended,
	}
}

// Tail returns retained output lines plus how many were discarded.
func (j *Job) Tail() ([]string, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.lines))
	copy(out, j.lines)
	return out, j.dropped
}

func (j *Job) append(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.lines) == maxLines {
		j.lines = j.lines[1:]
		j.dropped++
	}
	j.lines = append(j.lines, line)
}

// Event is a change broadcast to connected clients.
type Event struct {
	Type  string    `json:"type"` // "job" | "log"
	Job   *Snapshot `json:"job,omitempty"`
	JobID string    `json:"jobId,omitempty"`
	Line  string    `json:"line,omitempty"`

	// Push marks a terminal job event the action's notify policy wants
	// announced. The node decides, the hub publishes: policy stays with the
	// action definition, and only one machine talks to ntfy.
	Push bool `json:"push,omitempty"`
}

// ShouldNotify applies an action's push policy to a finished job.
//
// Silence is the default. A push for every two-second command trains you to
// ignore the channel, which costs more than the notification is worth.
func ShouldNotify(j Snapshot) bool {
	switch j.Notify {
	case "on-complete", "always":
		return true
	case "on-failure":
		return j.State != Done
	default: // "", "never", anything unrecognised
		return false
	}
}

// Manager owns job history and the event bus.
type Manager struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	order    []string
	max      int
	subs     map[chan Event]struct{}
	onFinish func(Snapshot)
}

// NewManager builds a manager retaining at most max jobs. onFinish, if set, is
// called once per terminal transition - the ntfy hook.
func NewManager(max int, onFinish func(Snapshot)) *Manager {
	return &Manager{
		jobs:     make(map[string]*Job),
		max:      max,
		subs:     make(map[chan Event]struct{}),
		onFinish: onFinish,
	}
}

// Subscribe returns a channel of events and a function to release it.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, ch)
			close(ch)
			m.mu.Unlock()
		})
	}
}

// publish fans out without blocking: a slow client loses events, never stalls
// the job runner.
func (m *Manager) publish(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// List returns snapshots, newest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if j, ok := m.jobs[m.order[i]]; ok {
			out = append(out, j.Snapshot())
		}
	}
	return out
}

// Get returns one job.
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// Cancel stops a running job.
func (m *Manager) Cancel(id string) error {
	j, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no such job %q", id)
	}
	j.mu.Lock()
	cancel, state := j.cancel, j.state
	j.mu.Unlock()

	if state.Terminal() {
		return fmt.Errorf("job already %s", state)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// Start launches an action and returns immediately.
func (m *Manager) Start(a actions.Resolved) (*Job, error) {
	if !a.Available {
		return nil, fmt.Errorf("action %q unavailable: %s", a.ID, a.Reason)
	}

	limit := a.Timeout
	if limit <= 0 {
		limit = defaultLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)

	j := &Job{
		ID:       newID(),
		ActionID: a.ID,
		Label:    a.Label,
		Notify:   a.Notify,
		Started:  time.Now().UTC(),
		state:    Queued,
		cancel:   cancel,
	}

	m.mu.Lock()
	m.jobs[j.ID] = j
	m.order = append(m.order, j.ID)
	for len(m.order) > m.max {
		delete(m.jobs, m.order[0])
		m.order = m.order[1:]
	}
	m.mu.Unlock()

	go m.run(ctx, cancel, j, a, limit)
	return j, nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, j *Job, a actions.Resolved, limit time.Duration) {
	defer cancel()

	cmd := exec.Command(a.Command.Cmd, a.Command.Args...)
	cmd.Dir = a.Cwd
	platform.Harden(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.finish(j, Failed, nil, fmt.Errorf("stdout pipe: %w", err))
		return
	}
	cmd.Stderr = cmd.Stdout // interleave; the tail is for humans, not parsing

	if err := cmd.Start(); err != nil {
		m.finish(j, Failed, nil, fmt.Errorf("start %s: %w", a.Command.Cmd, err))
		return
	}

	j.mu.Lock()
	j.cmd = cmd
	j.state = Running
	snap := j.snapshotLocked()
	j.mu.Unlock()
	m.publish(Event{Type: "job", Job: &snap})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.pump(j, stdout) }()

	// Kill the tree if the context ends first (cancel or timeout).
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = platform.KillTree(cmd)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()

	code := cmd.ProcessState.ExitCode()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		m.finish(j, TimedOut, &code, fmt.Errorf("exceeded %s", limit))
	case ctx.Err() == context.Canceled:
		m.finish(j, Cancelled, &code, nil)
	case waitErr != nil:
		m.finish(j, Failed, &code, nil)
	default:
		m.finish(j, Done, &code, nil)
	}
}

// pump reads child output line by line.
//
// Windows consoles emit legacy-codepage bytes unless the child opts into UTF-8,
// which mangles Hungarian text. Decoding every codepage would mean pulling in
// x/text, so invalid sequences are replaced rather than allowed to produce
// malformed JSON downstream. Actions should still force UTF-8 themselves.
func (m *Manager) pump(j *Job, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for sc.Scan() {
		line := sanitize(sc.Text())
		j.append(line)
		m.publish(Event{Type: "log", JobID: j.ID, Line: line})
	}
}

func sanitize(s string) string {
	s = strings.TrimRight(s, "\r")
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func (m *Manager) finish(j *Job, state State, exit *int, err error) {
	now := time.Now().UTC()

	j.mu.Lock()
	j.state = state
	j.ended = &now
	if exit != nil && *exit >= 0 {
		j.exit = exit
	}
	if err != nil {
		j.errMsg = err.Error()
	}
	snap := j.snapshotLocked()
	j.mu.Unlock()

	m.publish(Event{Type: "job", Job: &snap, Push: ShouldNotify(snap)})
	if m.onFinish != nil {
		m.onFinish(snap)
	}
}

var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newID returns a lexicographically sortable, time-prefixed identifier -
// ULID-shaped without taking a dependency for it.
func newID() string {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UTC().UnixMilli()))
	_, _ = rand.Read(buf[8:])
	return idEncoding.EncodeToString(buf[:])
}
