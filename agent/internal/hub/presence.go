package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// presence keeps the hub's picture of every node current.
//
// Two channels per node: a health poll, which is what "online" means, and the
// node's job event stream, which is how job-finished pushes reach ntfy now
// that nodes no longer publish themselves.
//
// An online node is polled every 30 s. A node that is offline - or just
// failed a check - is polled every 5 s, so a laptop coming back from a reboot
// shows as online within seconds rather than half a minute later, when its
// owner has long concluded that it did not work. A node also announces itself
// (/hub/hello) the moment it is reachable, which closes the gap entirely.
type presence struct {
	h *Hub

	mu      sync.Mutex
	nodes   map[string]*nodeState
	streams map[string]context.CancelFunc
}

type nodeState struct {
	online    bool
	known     bool // at least one check has completed
	fails     int
	lastSeen  time.Time
	health    json.RawMessage
	armed     bool
	nextCheck time.Time
	checking  bool
}

// nodeHealth is the subset of a node's /api/health the hub acts on.
type nodeHealth struct {
	Arm struct {
		Armed bool `json:"armed"`
	} `json:"arm"`
}

const (
	healthEvery   = 30 * time.Second // online nodes
	retryEvery    = 5 * time.Second  // offline nodes, and after any failed check
	healthTimeout = 4 * time.Second
	offlineAfter  = 2 // consecutive failed checks
)

func newPresence(h *Hub) *presence {
	return &presence{h: h, nodes: map[string]*nodeState{}, streams: map[string]context.CancelFunc{}}
}

func (p *presence) run(ctx context.Context) {
	t := time.NewTicker(retryEvery)
	defer t.Stop()
	p.sweep(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweep(ctx, false)
			p.h.sessions.prune()
		}
	}
}

// sweep checks every approved node that is due (or all of them, with force)
// and reconciles the stream subscriptions.
func (p *presence) sweep(ctx context.Context, force bool) {
	approved := map[string]bool{}
	now := time.Now()
	for _, n := range p.h.store.Nodes() {
		if n.State != Approved {
			continue
		}
		approved[n.ID] = true
		if p.claim(n.ID, now, force) {
			go p.check(ctx, n)
		}

		p.mu.Lock()
		if _, ok := p.streams[n.ID]; !ok {
			sctx, cancel := context.WithCancel(ctx)
			p.streams[n.ID] = cancel
			go p.stream(sctx, n.ID)
		}
		p.mu.Unlock()
	}

	p.mu.Lock()
	for id, cancel := range p.streams {
		if !approved[id] {
			cancel()
			delete(p.streams, id)
			delete(p.nodes, id)
		}
	}
	p.mu.Unlock()
}

// claim marks a node as being checked, if it is due and not already in flight.
func (p *presence) claim(id string, now time.Time, force bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.nodes[id]
	if !ok {
		st = &nodeState{}
		p.nodes[id] = st
	}
	if st.checking || (!force && now.Before(st.nextCheck)) {
		return false
	}
	st.checking = true
	return true
}

// poke checks every node immediately - after approval or enrolment, so the UI
// does not show a working device as offline.
func (p *presence) poke(ctx context.Context) { go p.sweep(ctx, true) }

// pokeNode checks one node now: it has just said it is back.
func (p *presence) pokeNode(ctx context.Context, n Node) {
	if p.claim(n.ID, time.Now(), true) {
		go p.check(ctx, n)
	}
}

func (p *presence) check(ctx context.Context, n Node) {
	cctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	body, err := p.fetchHealth(cctx, n)

	p.mu.Lock()
	st, ok := p.nodes[n.ID]
	if !ok {
		st = &nodeState{}
		p.nodes[n.ID] = st
	}
	wasOnline, wasKnown, wasArmed := st.online, st.known, st.armed
	st.known = true
	st.checking = false
	if err == nil {
		st.nextCheck = time.Now().Add(healthEvery)
	} else {
		st.nextCheck = time.Now().Add(retryEvery)
	}

	if err == nil {
		st.online, st.fails, st.lastSeen, st.health = true, 0, time.Now().UTC(), body
		var hh nodeHealth
		if json.Unmarshal(body, &hh) == nil {
			st.armed = hh.Arm.Armed
		}
	} else {
		st.fails++
		if st.fails >= offlineAfter {
			st.online = false
		}
	}
	nowOnline, armed := st.online, st.armed
	p.mu.Unlock()

	// Only an ARMED device going quiet is news: an unarmed laptop asleep in a
	// bag is the normal case, and paging for it would train you to ignore pages.
	switch {
	case wasKnown && wasOnline && !nowOnline && wasArmed:
		p.h.notify(fmt.Sprintf("%s stopped answering while armed", n.Name), 4)
		p.h.audit.write(AuditEntry{Event: "device.offline", Device: n.Name, Detail: errString(err)})
	case wasKnown && !wasOnline && nowOnline && armed:
		p.h.notify(fmt.Sprintf("%s is back online", n.Name), 3)
		p.h.audit.write(AuditEntry{Event: "device.online", Device: n.Name})
	}
}

func (p *presence) fetchHealth(ctx context.Context, n Node) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeURL(n, "/api/health"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	res, err := p.h.nodeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health: %s", res.Status)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("health: invalid JSON")
	}
	return body, nil
}

// snapshot returns what is known about one node.
func (p *presence) snapshot(id string) (online bool, lastSeen time.Time, health json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.nodes[id]
	if !ok {
		return false, time.Time{}, nil
	}
	return st.online, st.lastSeen, st.health
}

// streamEvent is a node job event; Push is the node's own verdict on whether
// its action's notify policy wants this terminal state announced.
type streamEvent struct {
	Type string `json:"type"`
	Push bool   `json:"push"`
	Job  *struct {
		Label string `json:"label"`
		State string `json:"state"`
	} `json:"job"`
}

// stream holds the node's event socket open, reconnecting with backoff.
func (p *presence) stream(ctx context.Context, id string) {
	backoff := time.Second
	for ctx.Err() == nil {
		n, ok := p.h.store.Node(id)
		if !ok || n.State != Approved {
			return
		}
		start := time.Now()
		err := p.streamOnce(ctx, n)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // it was a healthy connection; retry promptly
		}
		p.h.log.Debug("node stream ended", "node", n.Name, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (p *presence) streamOnce(ctx context.Context, n Node) error {
	dctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+n.Token)
	conn, _, err := websocket.Dial(dctx, "ws://"+nodeHostPort(n)+"/api/stream", &websocket.DialOptions{
		HTTPClient: p.h.nodeClient,
		HTTPHeader: hdr,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	for {
		var ev streamEvent
		if err := wsjson.Read(ctx, conn, &ev); err != nil {
			return err
		}
		if ev.Type != "job" || ev.Job == nil || !ev.Push {
			continue
		}
		priority := 3
		if ev.Job.State != "done" {
			priority = 4
		}
		// Opaque on purpose, as before: the ntfy topic may be on a public
		// server, so the push names the device and job, nothing more.
		p.h.notify(fmt.Sprintf("%s · %s: %s", n.Name, ev.Job.Label, ev.Job.State), priority)
	}
}

func nodeHostPort(n Node) string {
	port := n.Port
	if port == 0 {
		port = 8737
	}
	return n.Addr + ":" + strconv.Itoa(port)
}

func nodeURL(n Node, path string) string { return "http://" + nodeHostPort(n) + path }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
