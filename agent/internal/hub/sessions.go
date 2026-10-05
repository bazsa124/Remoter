package hub

import (
	"context"
	"sort"
	"sync"
	"time"
)

// sessionTracker knows which controller is using which tier on which device.
//
// It feeds three things: the audit log, the "session started" push, and the
// device list's "who is connected" column. Because every session is relayed,
// this view is complete - no device can be used without appearing here.
type sessionTracker struct {
	mu      sync.Mutex
	open    map[sessionKey]*liveSession
	ended   map[sessionKey]time.Time // last close, to suppress pushes on reconnect
	glances map[sessionKey]time.Time // last Glance frame
	nextID  int
	now     func() time.Time
}

type sessionKey struct{ ctrl, node, tier string }

type liveSession struct {
	ctrlName string
	since    time.Time
	cancels  map[int]context.CancelFunc
}

// SessionView is a session as the device list shows it.
type SessionView struct {
	Controller   string    `json:"controller"`
	ControllerID string    `json:"controllerId"`
	Tier         string    `json:"tier"`
	Since        time.Time `json:"since"`
}

// reconnectQuiet is how long after a close a reopen counts as the same session:
// a phone dropping off LTE for a moment should not page you twice.
const reconnectQuiet = 5 * time.Minute

// glanceActive is how long a Glance frame keeps its session listed.
const glanceActive = 2 * time.Minute

func newSessionTracker() *sessionTracker {
	return &sessionTracker{
		open:    map[sessionKey]*liveSession{},
		ended:   map[sessionKey]time.Time{},
		glances: map[sessionKey]time.Time{},
		now:     time.Now,
	}
}

// begin records a long-lived session (Live, Console). started is true for the
// first connection of a new session - the moment worth auditing and pushing.
func (t *sessionTracker) begin(k sessionKey, ctrlName string, cancel context.CancelFunc) (id int, started bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	id = t.nextID

	s, ok := t.open[k]
	if !ok {
		s = &liveSession{ctrlName: ctrlName, since: t.now(), cancels: map[int]context.CancelFunc{}}
		t.open[k] = s
		last, recent := t.ended[k]
		started = !recent || t.now().Sub(last) > reconnectQuiet
	}
	s.cancels[id] = cancel
	return id, started
}

// end closes one connection; ended reports the session as a whole finishing.
func (t *sessionTracker) end(k sessionKey, id int) (ended bool, lasted time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.open[k]
	if !ok {
		return false, 0
	}
	delete(s.cancels, id)
	if len(s.cancels) > 0 {
		return false, 0
	}
	delete(t.open, k)
	t.ended[k] = t.now()
	return true, t.now().Sub(s.since)
}

// glance records a Glance frame and reports whether it starts a new session.
func (t *sessionTracker) glance(k sessionKey) (started bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.glances[k]
	t.glances[k] = t.now()
	return !ok || t.now().Sub(last) > reconnectQuiet
}

// forNode lists who is on a device right now.
func (t *sessionTracker) forNode(node string, names func(ctrlID string) string) []SessionView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []SessionView{}
	for k, s := range t.open {
		if k.node == node {
			out = append(out, SessionView{Controller: s.ctrlName, ControllerID: k.ctrl, Tier: k.tier, Since: s.since})
		}
	}
	for k, at := range t.glances {
		if k.node == node && t.now().Sub(at) < glanceActive {
			out = append(out, SessionView{Controller: names(k.ctrl), ControllerID: k.ctrl, Tier: "glance", Since: at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// cancelWhere tears down every session matching pred - used when a device is
// revoked, so removing access actually ends what that device has open.
func (t *sessionTracker) cancelWhere(pred func(sessionKey) bool) int {
	t.mu.Lock()
	var cancels []context.CancelFunc
	for k, s := range t.open {
		if pred(k) {
			for _, c := range s.cancels {
				cancels = append(cancels, c)
			}
		}
	}
	t.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// prune drops stale bookkeeping so the maps do not grow forever.
func (t *sessionTracker) prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, at := range t.ended {
		if t.now().Sub(at) > reconnectQuiet {
			delete(t.ended, k)
		}
	}
	for k, at := range t.glances {
		if t.now().Sub(at) > reconnectQuiet {
			delete(t.glances, k)
		}
	}
}
