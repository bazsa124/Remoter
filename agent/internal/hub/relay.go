package hub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// guardedTier names the tier a node path belongs to when that tier shows the
// screen or hands out a shell - the ones behind the PIN. Everything else
// (actions, jobs, health, arming) is "".
func guardedTier(path string) string {
	switch path {
	case "/api/screenshot":
		return "glance"
	case "/api/live":
		return "live"
	case "/api/console":
		return "console"
	}
	return ""
}

var tierLabel = map[string]string{"glance": "Glance", "live": "Live", "console": "Console"}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// relay forwards /d/{node}/api/... to that node.
//
// The controller's own credentials never travel further: the node sees the
// hub's per-node token and a header naming the controller, and accepts
// connections from the hub's address alone.
func (h *Hub) relay(w http.ResponseWriter, r *http.Request, c Controller) {
	id, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/d/"), "/")
	sub = "/" + sub
	n, ok := h.store.Node(id)
	if !ok || n.State != Approved {
		writeError(w, http.StatusNotFound, "no such device", "")
		return
	}
	if !strings.HasPrefix(sub, "/api/") {
		writeError(w, http.StatusNotFound, "no such endpoint", "")
		return
	}

	tier := guardedTier(sub)
	if tier != "" {
		if h.store.PINHash() == "" {
			writeError(w, http.StatusUnauthorized, "set a PIN before using "+tierLabel[tier], "pin_unset")
			return
		}
		if !h.pins.valid(c.ID) {
			writeError(w, http.StatusUnauthorized, "enter the PIN to open "+tierLabel[tier], "pin_required")
			return
		}
	}

	ctx := r.Context()
	key := sessionKey{ctrl: c.ID, node: n.ID, tier: tier}
	switch {
	case tier != "" && isWebSocket(r):
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		sid, started := h.sessions.begin(key, c.Name, cancel)
		h.pins.open(c.ID)
		if started {
			h.sessionStarted(c, n, tier)
		}
		defer func() {
			h.pins.close(c.ID)
			if ended, lasted := h.sessions.end(key, sid); ended {
				h.audit.write(AuditEntry{
					Event: "session.close", Controller: c.Name, Device: n.Name, Tier: tier,
					Detail: lasted.Round(time.Second).String(),
				})
			}
		}()
	case tier == "glance":
		h.pins.touch(c.ID)
		if h.sessions.glance(key) {
			h.sessionStarted(c, n, tier)
		}
	}

	var audit string
	if r.Method == http.MethodPost {
		switch {
		case sub == "/api/arm":
			audit = "device.arm"
		case sub == "/api/disarm":
			audit = "device.disarm"
		case strings.HasPrefix(sub, "/api/actions/"):
			audit = "action.run"
		}
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = nodeHostPort(n)
			pr.Out.URL.Path, pr.Out.URL.RawPath = sub, ""
			q := pr.Out.URL.Query()
			q.Del("token") // a client must never be able to smuggle its own credential
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = pr.Out.URL.Host

			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Set("Authorization", "Bearer "+n.Token)
			pr.Out.Header.Set("X-Remoter-Controller", c.Name)
			pr.Out.Header.Set("X-Remoter-Controller-Id", c.ID)
		},
		Transport: h.nodeTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the controller left, or the session was revoked
			}
			h.log.Debug("relay failed", "node", n.Name, "path", sub, "err", err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach %s: is it on and connected?", n.Name), "unreachable")
		},
	}

	if audit == "" {
		proxy.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	proxy.ServeHTTP(rec, r.WithContext(ctx))
	// Only what actually happened: a confirm action's first step is a 409
	// challenge, not a run.
	if rec.status < 300 {
		detail := strings.TrimPrefix(sub, "/api/actions/")
		detail = strings.TrimSuffix(detail, "/run")
		if audit != "action.run" {
			detail = ""
		}
		h.audit.write(AuditEntry{Event: audit, Controller: c.Name, Device: n.Name, Detail: detail})
	}
}

func (h *Hub) sessionStarted(c Controller, n Node, tier string) {
	h.audit.write(AuditEntry{Event: "session.open", Controller: c.Name, Device: n.Name, Tier: tier})
	h.notify(fmt.Sprintf("%s session on %s from %s", tierLabel[tier], n.Name, c.Name), 3)
}

// statusRecorder captures the status code for the audit log while staying
// transparent to the proxy.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := s.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}
