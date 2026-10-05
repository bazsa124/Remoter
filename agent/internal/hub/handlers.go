package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const controllerKey ctxKey = iota

// allowPending lets a not-yet-approved device reach an endpoint. Only /hub/me
// uses it: a waiting device must be able to learn that it is waiting.
const allowPending = true

// guard rejects requests that did not come from the hub's own pages.
//
// Identity here is ambient - the caller's tailnet address - exactly like a
// cookie. So the classic cookie attacks apply: any web page open in a browser
// on an approved laptop could POST to the hub or open a WebSocket to a shell,
// and the request would arrive looking like the owner. Browsers always send
// Origin on cross-origin requests and WebSocket handshakes, and Sec-Fetch-Site
// on everything modern; requiring them to name this hub closes that hole.
// Native clients (the Android app's OkHttp) send neither, and are allowed.
func (h *Hub) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		self := h.Self()
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		// DNS rebinding: a foreign name pointed at our address. TLS already
		// fails for it, but refuse explicitly rather than rely on that alone.
		if !strings.EqualFold(host, self.DNSName) && host != self.IPv4 {
			writeError(w, http.StatusMisdirectedRequest, "unknown host "+host, "")
			return
		}

		api := strings.HasPrefix(r.URL.Path, "/hub/") || strings.HasPrefix(r.URL.Path, "/d/")
		if api {
			origin := r.Header.Get("Origin")
			if origin != "" && !strings.EqualFold(origin, "https://"+self.DNSName) {
				writeError(w, http.StatusForbidden, "cross-origin request refused", "")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				writeError(w, http.StatusForbidden, "cross-site request refused", "")
				return
			}
		}

		// The hub's pages hold approve buttons; nobody else may frame them.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// withPeer resolves the caller's tailnet identity and enforces approval.
func (h *Hub) withPeer(next func(http.ResponseWriter, *http.Request, Controller), opts ...bool) http.HandlerFunc {
	pendingOK := len(opts) > 0 && opts[0]
	return func(w http.ResponseWriter, r *http.Request) {
		peer, err := h.ts.whois(r.Context(), r.RemoteAddr)
		if err != nil {
			h.log.Warn("whois failed", "remote", r.RemoteAddr, "err", err)
			writeError(w, http.StatusForbidden, "this connection did not come through the tailnet", "")
			return
		}
		c, isNew, err := h.store.SeeController(peer)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		if isNew {
			h.log.Info("new device waiting", "name", c.Name, "id", c.ID)
			h.audit.write(AuditEntry{Event: "access.request", Controller: c.Name, Detail: "controller " + c.ID})
			h.notify(fmt.Sprintf("%s is asking for access", c.Name), 4)
		}
		if c.State != Approved && !pendingOK {
			writeError(w, http.StatusForbidden,
				c.Name+" is waiting for approval. Approve it from a device you already use, or on the hub machine: sudo remoter-hub approve "+c.Name,
				"pending")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), controllerKey, c)), c)
	}
}

// withPIN requires a fresh PIN for security-critical hub operations: approving
// a device grants it everything, so a borrowed unlocked laptop must not be
// enough. Before the first PIN exists there is nothing to check against, and the
// owner has to be able to bootstrap.
func (h *Hub) withPIN(next func(http.ResponseWriter, *http.Request, Controller)) func(http.ResponseWriter, *http.Request, Controller) {
	return func(w http.ResponseWriter, r *http.Request, c Controller) {
		if h.store.PINHash() != "" && !h.pins.valid(c.ID) {
			writeError(w, http.StatusUnauthorized, "enter the PIN first", "pin_required")
			return
		}
		next(w, r, c)
	}
}

// --- /hub/me ---------------------------------------------------------------

func (h *Hub) me(w http.ResponseWriter, _ *http.Request, c Controller) {
	remaining := h.pins.remaining(c.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"device": map[string]string{"id": c.ID, "name": c.Name, "os": c.OS},
		"state":  c.State,
		"pin": map[string]any{
			"set":     h.store.PINHash() != "",
			"granted": h.pins.valid(c.ID),
			"held":    remaining < 0,
			"seconds": int(max(0, remaining).Seconds()),
		},
		"hub": map[string]string{"name": h.Self().DNSName, "version": h.version},
	})
}

// --- /hub/devices ----------------------------------------------------------

type deviceView struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	OS       string          `json:"os"`
	Online   bool            `json:"online"`
	LastSeen *time.Time      `json:"lastSeen,omitempty"`
	Version  string          `json:"version"`
	Health   json.RawMessage `json:"health,omitempty"`
	Sessions []SessionView   `json:"sessions"`
}

func (h *Hub) devices(w http.ResponseWriter, _ *http.Request, _ Controller) {
	names := func(id string) string {
		if c, ok := h.store.Controller(id); ok {
			return c.Name
		}
		return id
	}
	out := []deviceView{}
	for _, n := range h.store.Nodes() {
		if n.State != Approved {
			continue
		}
		online, seen, health := h.presence.snapshot(n.ID)
		os := n.OS
		if os == "" && health != nil {
			// tailscaled reports no OS when the hub looks itself up; the
			// node's own report is just as good.
			var hh struct {
				OS string `json:"os"`
			}
			_ = json.Unmarshal(health, &hh)
			os = hh.OS
		}
		v := deviceView{
			ID: n.ID, Name: n.Name, OS: os, Online: online, Version: n.Version,
			Health: health, Sessions: h.sessions.forNode(n.ID, names),
		}
		if !seen.IsZero() {
			v.LastSeen = &seen
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- /hub/access -----------------------------------------------------------

type accessView struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	OS    string    `json:"os"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
	Self  bool      `json:"self,omitempty"`
	Addr  string    `json:"addr,omitempty"`
}

func (h *Hub) accessLists(selfID string) map[string][]accessView {
	ctrls := []accessView{}
	for _, c := range h.store.Controllers() {
		ctrls = append(ctrls, accessView{ID: c.ID, Name: c.Name, OS: c.OS, State: c.State, Since: c.FirstSeen, Self: c.ID == selfID})
	}
	nodes := []accessView{}
	for _, n := range h.store.Nodes() {
		// Never the token: it is the node's key, and only the hub holds it.
		nodes = append(nodes, accessView{ID: n.ID, Name: n.Name, OS: n.OS, State: n.State, Since: n.EnrolledAt, Addr: n.Addr})
	}
	return map[string][]accessView{"controllers": ctrls, "nodes": nodes}
}

func (h *Hub) access(w http.ResponseWriter, _ *http.Request, c Controller) {
	writeJSON(w, http.StatusOK, h.accessLists(c.ID))
}

func (h *Hub) approve(w http.ResponseWriter, r *http.Request, c Controller) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	name := h.deviceName(kind, id)
	if err := h.store.Approve(kind, id); err != nil {
		writeError(w, statusFor(err), err.Error(), "")
		return
	}
	h.audit.write(AuditEntry{Event: "access.approve", Controller: c.Name, Device: name, Detail: kind})
	h.log.Info("approved", "kind", kind, "name", name, "by", c.Name)
	if kind == KindNode {
		h.presence.poke(context.Background())
	}
	writeJSON(w, http.StatusOK, h.accessLists(c.ID))
}

func (h *Hub) removeAccess(w http.ResponseWriter, r *http.Request, c Controller) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	name := h.deviceName(kind, id)
	if err := h.store.Remove(kind, id); err != nil {
		writeError(w, statusFor(err), err.Error(), "")
		return
	}
	h.afterRemove(kind, id)
	h.audit.write(AuditEntry{Event: "access.remove", Controller: c.Name, Device: name, Detail: kind})
	h.log.Info("removed", "kind", kind, "name", name, "by", c.Name)
	writeJSON(w, http.StatusOK, h.accessLists(c.ID))
}

// afterRemove makes revocation immediate: open sessions end now, not whenever
// the device next reconnects.
func (h *Hub) afterRemove(kind, id string) {
	switch kind {
	case KindController:
		h.pins.revoke(id)
		h.sessions.cancelWhere(func(k sessionKey) bool { return k.ctrl == id })
	case KindNode:
		h.sessions.cancelWhere(func(k sessionKey) bool { return k.node == id })
		h.presence.poke(context.Background())
	}
}

func (h *Hub) deviceName(kind, id string) string {
	switch kind {
	case KindController:
		if c, ok := h.store.Controller(id); ok {
			return c.Name
		}
	case KindNode:
		if n, ok := h.store.Node(id); ok {
			return n.Name
		}
	}
	return id
}

func statusFor(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// --- PIN -------------------------------------------------------------------

type pinBody struct {
	PIN     string `json:"pin"`
	Current string `json:"current"`
}

func readPINBody(r *http.Request) (pinBody, error) {
	var b pinBody
	err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b)
	return b, err
}

func (h *Hub) enterPIN(w http.ResponseWriter, r *http.Request, c Controller) {
	b, err := readPINBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "")
		return
	}
	err = h.pins.verify(c.ID, b.PIN, h.store.PINHash())
	var locked errLocked
	var wrong errWrongPIN
	switch {
	case err == nil:
		h.audit.write(AuditEntry{Event: "pin.ok", Controller: c.Name})
		writeJSON(w, http.StatusOK, map[string]int{"seconds": int(pinGrace.Seconds())})
	case errors.Is(err, errNoPIN):
		writeError(w, http.StatusConflict, "no PIN is set yet - set one first", "pin_unset")
	case errors.As(err, &locked):
		h.audit.write(AuditEntry{Event: "pin.locked", Controller: c.Name})
		h.notify(fmt.Sprintf("Too many wrong PINs from %s", c.Name), 4)
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, err.Error(), "pin_locked")
	case errors.As(err, &wrong):
		h.audit.write(AuditEntry{Event: "pin.wrong", Controller: c.Name})
		writeError(w, http.StatusUnauthorized, err.Error(), "pin_wrong")
	default:
		writeError(w, http.StatusInternalServerError, err.Error(), "")
	}
}

// changePIN sets the first PIN, or replaces it given the current one.
func (h *Hub) changePIN(w http.ResponseWriter, r *http.Request, c Controller) {
	b, err := readPINBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "")
		return
	}
	if current := h.store.PINHash(); current != "" {
		if err := h.pins.verify(c.ID, b.Current, current); err != nil {
			writeError(w, http.StatusUnauthorized, "current PIN: "+err.Error(), "pin_wrong")
			return
		}
	}
	hash, err := hashPIN(b.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	if err := h.store.SetPINHash(hash); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	h.pins.touch(c.ID) // the person who just set it obviously knows it
	h.audit.write(AuditEntry{Event: "pin.set", Controller: c.Name})
	writeJSON(w, http.StatusOK, map[string]int{"seconds": int(pinGrace.Seconds())})
}

func (h *Hub) lock(w http.ResponseWriter, _ *http.Request, c Controller) {
	h.pins.revoke(c.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) auditList(w http.ResponseWriter, r *http.Request, _ Controller) {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 || n > 500 {
		n = 100
	}
	writeJSON(w, http.StatusOK, h.audit.recent(n))
}

// --- enrolment -------------------------------------------------------------

// nodeAddr is where the hub dials a node: its tailnet address - or loopback
// when the node runs on the hub's own machine. That node then needs no network
// listener at all, so nothing else on the tailnet can even try to reach it.
func (h *Hub) nodeAddr(p *Peer) string {
	if p.IP == h.Self().IPv4 {
		return "127.0.0.1"
	}
	return p.IP
}

// hello is a node saying "I am reachable again" - after a reboot, or once
// Tailscale comes up. The hub checks it at once instead of waiting for the next
// poll, so the device is back in every client's list within a second.
func (h *Hub) hello(w http.ResponseWriter, r *http.Request) {
	peer, err := h.ts.whois(r.Context(), r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusForbidden, "this connection did not come through the tailnet", "")
		return
	}
	n, ok := h.store.Node(peer.ID)
	if !ok || n.State != Approved {
		writeError(w, http.StatusNotFound, "not an approved node", "")
		return
	}
	if addr := h.nodeAddr(peer); addr != n.Addr {
		h.store.UpdateNodeAddr(n.ID, addr)
		n.Addr = addr
	}
	h.log.Info("node says hello", "name", n.Name)
	h.presence.pokeNode(context.Background(), n)
	w.WriteHeader(http.StatusNoContent)
}

type enrollBody struct {
	Token   string `json:"token"`
	Port    int    `json:"port"`
	Version string `json:"version"`
}

// enroll registers a node. The caller is identified by tailscaled exactly like a
// controller, so a node cannot claim to be another machine; it lands pending and
// receives nothing until the owner approves it.
func (h *Hub) enroll(w http.ResponseWriter, r *http.Request) {
	peer, err := h.ts.whois(r.Context(), r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusForbidden, "this connection did not come through the tailnet", "")
		return
	}
	var b enrollBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "")
		return
	}
	if len(b.Token) < 32 {
		writeError(w, http.StatusBadRequest, "token too short", "")
		return
	}
	if b.Port <= 0 || b.Port > 65535 {
		b.Port = h.cfg.NodePort
	}
	n, isNew, err := h.store.Enroll(peer, h.nodeAddr(peer), b.Token, b.Port, b.Version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	if isNew {
		h.log.Info("node enrolled, waiting for approval", "name", n.Name, "addr", n.Addr)
		h.audit.write(AuditEntry{Event: "access.request", Device: n.Name, Detail: "node " + n.ID})
		h.notify(fmt.Sprintf("%s wants to be controllable", n.Name), 4)
	} else {
		h.log.Info("node re-enrolled", "name", n.Name, "state", n.State)
		h.presence.poke(context.Background())
	}
	self := h.Self()
	writeJSON(w, http.StatusOK, map[string]string{
		"state":   n.State,
		"id":      n.ID,
		"name":    n.Name,
		"hub":     self.DNSName,
		"hubAddr": self.IPv4, // the node allows connections from this address only
	})
}
