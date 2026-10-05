package api

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is one controller using one screen or shell tier on this node.
type Session struct {
	Controller string    `json:"controller"`
	Tier       string    `json:"tier"`
	Since      time.Time `json:"since"`
}

// Sessions is the node's own view of who is connected. The hub keeps the
// authoritative log; this copy drives what happens ON the device - the
// indicator in the notification area, keeping the machine awake, and telling
// the arming logic which input was remote.
type Sessions struct {
	mu      sync.Mutex
	open    map[int]Session
	glances map[string]time.Time // controller -> last Glance frame
	nextID  int

	// OnChange is called, outside the lock, whenever the set changes. started
	// is the session that just began, if any - for the "someone connected"
	// notification.
	OnChange func(active []Session, started *Session)
}

// glanceWindow is how long a single Glance frame counts as "connected".
const glanceWindow = 90 * time.Second

// NewSessions builds an empty table.
func NewSessions() *Sessions {
	return &Sessions{open: map[int]Session{}, glances: map[string]time.Time{}}
}

func (t *Sessions) begin(controller, tier string) int {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	s := Session{Controller: controller, Tier: tier, Since: time.Now().UTC()}
	first := true
	for _, o := range t.open {
		if o.Controller == controller && o.Tier == tier {
			first = false // a reconnect, not news
		}
	}
	t.open[id] = s
	list := t.listLocked()
	t.mu.Unlock()

	var started *Session
	if first {
		started = &s
	}
	t.changed(list, started)
	return id
}

func (t *Sessions) end(id int) {
	t.mu.Lock()
	delete(t.open, id)
	list := t.listLocked()
	t.mu.Unlock()
	t.changed(list, nil)
}

func (t *Sessions) glance(controller string) {
	t.mu.Lock()
	last, seen := t.glances[controller]
	t.glances[controller] = time.Now()
	fresh := !seen || time.Since(last) > glanceWindow
	list := t.listLocked()
	t.mu.Unlock()
	if fresh {
		t.changed(list, &Session{Controller: controller, Tier: "glance", Since: time.Now().UTC()})
	}
}

func (t *Sessions) changed(list []Session, started *Session) {
	if t.OnChange != nil {
		t.OnChange(list, started)
	}
}

// List returns who is connected now.
func (t *Sessions) List() []Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.listLocked()
}

// Count is the number of live sessions, Glance included.
func (t *Sessions) Count() int { return len(t.List()) }

func (t *Sessions) listLocked() []Session {
	out := make([]Session, 0, len(t.open)+len(t.glances))
	for _, s := range t.open {
		out = append(out, s)
	}
	for c, at := range t.glances {
		if time.Since(at) < glanceWindow {
			out = append(out, Session{Controller: c, Tier: "glance", Since: at.UTC()})
		} else {
			delete(t.glances, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Sweep expires Glance sessions whose window has passed, firing OnChange so the
// indicator turns off without waiting for some unrelated event.
func (t *Sessions) Sweep() {
	t.mu.Lock()
	before := len(t.glances)
	list := t.listLocked()
	after := len(t.glances)
	t.mu.Unlock()
	if after != before {
		t.changed(list, nil)
	}
}

// sessionTier maps a path to the tier it opens, or "" for paths that do not
// count as someone being on the machine.
func sessionTier(path string) string {
	switch path {
	case "/api/live":
		return "live"
	case "/api/console":
		return "console"
	case "/api/screenshot":
		return "glance"
	}
	return ""
}

// controllerName is who the hub says is calling.
func controllerName(r *http.Request) string {
	if name := strings.TrimSpace(r.Header.Get("X-Remoter-Controller")); name != "" {
		return name
	}
	return "local"
}

// trackSessions brackets the screen and shell tiers in the session table.
func (s *Server) trackSessions(next http.Handler) http.Handler {
	if s.Sessions == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tier := sessionTier(r.URL.Path)
		switch {
		case tier == "glance":
			s.Sessions.glance(controllerName(r))
		case tier != "":
			id := s.Sessions.begin(controllerName(r), tier)
			defer s.Sessions.end(id)
		}
		next.ServeHTTP(w, r)
	})
}
