// Package api serves the node's HTTP and WebSocket surface: every tier, behind
// the hub's per-node token, reachable from the hub's address alone.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"remoter/agent/internal/actions"
	"remoter/agent/internal/arming"
	"remoter/agent/internal/jobs"
	"remoter/agent/internal/platform"
)

const nonceTTL = 60 * time.Second

// Group selects which routes a server mounts. A standalone node mounts all of
// them; on Windows the service and its two helpers each mount their share.
type Group int

const (
	// GroupCore is health and arming: what the service answers itself.
	GroupCore Group = 1 << iota
	// GroupScreen is capture and input: the SYSTEM screen helper's job.
	GroupScreen
	// GroupUser is actions, jobs and the shell: the user helper's job, so they
	// keep the logged-on user's privilege.
	GroupUser

	GroupAll = GroupCore | GroupScreen | GroupUser
)

// Server wires the registry and job manager to HTTP.
type Server struct {
	Registry *actions.Registry
	Jobs     *jobs.Manager
	Token    string
	Version  string
	Name     string
	Mode     string // "standalone", "service", "screen-helper", "user-helper"

	// Tier 2 settings, resolved from config at startup.
	ConsoleEnabled bool
	ConsoleShell   string
	ConsoleArgs    []string

	// AllowedSources, when non-empty, is the complete list of addresses that may
	// connect. A node accepts the hub and loopback, and nothing else on the
	// tailnet: every other device has to go through the hub, where it is
	// authenticated, PIN-checked and logged.
	AllowedSources []netip.Addr

	// Arm is the arming controller; nil where arming is not offered.
	Arm *arming.Controller

	// Sessions tracks who is connected. Nil in the helpers, whose sessions are
	// already counted by the service in front of them.
	Sessions *Sessions

	// Tiers, when set, replaces local probing in /api/health. The Windows
	// service uses it: from session 0 it cannot probe a desktop itself.
	Tiers func() map[string]any

	// User reports who is logged on to the console, for /api/health.
	User func() string

	Started time.Time
	Log     *slog.Logger

	mu     sync.Mutex
	nonces map[string]nonce
}

type nonce struct {
	actionID string
	expires  time.Time
}

// New builds a Server.
func New(reg *actions.Registry, mgr *jobs.Manager, token, version string, log *slog.Logger) *Server {
	return &Server{
		Registry: reg, Jobs: mgr, Token: token, Version: version,
		Started: time.Now().UTC(), Log: log, Mode: "standalone",
		nonces: make(map[string]nonce),
	}
}

// Handler mounts the selected route groups behind the node's checks.
func (s *Server) Handler(groups Group) http.Handler {
	mux := http.NewServeMux()
	s.Mount(mux, groups)
	return s.Wrap(mux)
}

// Mount registers the selected route groups on mux, without the checks - for
// callers composing their own mux, like the Windows service.
func (s *Server) Mount(mux *http.ServeMux, groups Group) {
	if groups&GroupCore != 0 {
		mux.HandleFunc("GET /api/health", s.health)
		mux.HandleFunc("POST /api/arm", s.arm)
		mux.HandleFunc("POST /api/disarm", s.disarm)
	}
	if groups&GroupScreen != 0 {
		mux.HandleFunc("GET /api/monitors", s.monitors)
		mux.HandleFunc("GET /api/screenshot", s.screenshot)
		mux.HandleFunc("GET /api/live", s.live)
	}
	if groups&GroupUser != 0 {
		mux.HandleFunc("GET /api/actions", s.listActions)
		mux.HandleFunc("POST /api/actions/{id}/run", s.runAction)
		mux.HandleFunc("GET /api/jobs", s.listJobs)
		mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
		mux.HandleFunc("DELETE /api/jobs/{id}", s.cancelJob)
		mux.HandleFunc("GET /api/stream", s.stream)
		mux.HandleFunc("GET /api/console", s.console)
	}
}

// Wrap applies the node's checks, outermost first: source address, token,
// then session tracking.
func (s *Server) Wrap(next http.Handler) http.Handler {
	return s.restrictSources(s.authenticate(s.trackSessions(next)))
}

// restrictSources refuses every address not on the allow list - before auth,
// so a device that is not the hub learns nothing, not even that a token is
// expected.
func (s *Server) restrictSources(next http.Handler) http.Handler {
	if len(s.AllowedSources) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		addr, perr := netip.ParseAddr(host)
		if err != nil || perr != nil || !s.sourceAllowed(addr.Unmap()) {
			s.Log.Warn("refused connection from outside the hub", "remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sourceAllowed(a netip.Addr) bool {
	for _, ok := range s.AllowedSources {
		if a == ok {
			return true
		}
	}
	return false
}

// authenticate enforces the bearer token. Only the hub holds it, and the hub
// always sends it as a header - WebSocket handshakes included, because the hub
// is not a browser.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="remoter"`)
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	var tiers map[string]any
	if s.Tiers != nil {
		tiers = s.Tiers()
	} else {
		tiers = s.probeTiers()
	}

	body := map[string]any{
		"version":  s.Version,
		"name":     s.Name,
		"mode":     s.Mode,
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
		"elevated": platform.IsElevated(),
		"uptime":   time.Since(s.Started).Round(time.Second).String(),
		"tiers":    tiers,
	}
	if s.User != nil {
		body["user"] = s.User()
	}
	if s.Arm != nil {
		body["arm"] = s.Arm.State()
	} else {
		body["arm"] = arming.State{}
	}
	if ps, err := platform.Power(); err == nil {
		body["power"] = ps
	}
	if s.Sessions != nil {
		body["sessions"] = s.Sessions.List()
	}
	writeJSON(w, http.StatusOK, body)
}

// probeTiers asks the host directly. Glance is probed, not asserted: the same
// binary can capture from a user session and fail from session 0, so the honest
// answer depends on where it happens to be running.
func (s *Server) probeTiers() map[string]any {
	_, glanceErr := platform.Monitors()
	tiers := map[string]any{
		"actions": true,
		"glance":  glanceErr == nil,
		"console": s.ConsoleEnabled,
		"live":    glanceErr == nil && platform.InputSupported(),
	}
	if glanceErr != nil {
		tiers["glanceReason"] = glanceErr.Error()
	}
	if !s.ConsoleEnabled {
		tiers["consoleReason"] = "console is disabled in the node config"
	}
	return tiers
}

type armRequest struct {
	Hours float64 `json:"hours"`
}

func (s *Server) arm(w http.ResponseWriter, r *http.Request) {
	if s.Arm == nil || !s.Arm.Supported() {
		writeError(w, http.StatusNotImplemented, "this host cannot be armed")
		return
	}
	var req armRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	d, err := arming.ParseDuration(req.Hours)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Arm.Arm(d); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("armed by", "controller", controllerName(r), "hours", req.Hours)
	writeJSON(w, http.StatusOK, s.Arm.State())
}

func (s *Server) disarm(w http.ResponseWriter, r *http.Request) {
	if s.Arm == nil {
		writeError(w, http.StatusNotImplemented, "this host cannot be armed")
		return
	}
	if err := s.Arm.Disarm(arming.ReasonManual); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("disarmed by", "controller", controllerName(r))
	writeJSON(w, http.StatusOK, s.Arm.State())
}

type actionView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Icon      string `json:"icon,omitempty"`
	Confirm   bool   `json:"confirm"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Server) listActions(w http.ResponseWriter, _ *http.Request) {
	all := s.Registry.All()
	out := make([]actionView, 0, len(all))
	for _, a := range all {
		out = append(out, actionView{
			ID: a.ID, Label: a.Label, Icon: a.Icon, Confirm: a.Confirm,
			Available: a.Available, Reason: a.Reason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action, ok := s.Registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such action")
		return
	}
	if !action.Available {
		writeError(w, http.StatusConflict, action.Reason)
		return
	}

	if action.Confirm {
		var body struct {
			Nonce string `json:"nonce"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // absent body is the first step

		if !s.consumeNonce(body.Nonce, id) {
			issued := s.issueNonce(id)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":     "confirmation required",
				"nonce":     issued,
				"expiresIn": int(nonceTTL.Seconds()),
			})
			return
		}
	}

	job, err := s.Jobs.Start(action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("job started", "job", job.ID, "action", id, "controller", controllerName(r))
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": job.ID})
}

func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Jobs.List())
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	tail, dropped := job.Tail()
	writeJSON(w, http.StatusOK, map[string]any{
		"job":     job.Snapshot(),
		"tail":    tail,
		"dropped": dropped,
	})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if err := s.Jobs.Cancel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) monitors(w http.ResponseWriter, _ *http.Request) {
	list, err := platform.Monitors()
	if err != nil {
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin is moot: only the hub can connect at all
	})
	if err != nil {
		s.Log.Warn("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	events, release := s.Jobs.Subscribe()
	defer release()

	ctx, cancel := context.WithCancel(conn.CloseRead(r.Context())) // we never read; this detects hangup
	defer cancel()
	go keepAlive(ctx, conn, cancel)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(writeCtx, conn, ev)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// --- nonces -----------------------------------------------------------------

func (s *Server) issueNonce(actionID string) string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	n := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for k, v := range s.nonces { // opportunistic sweep
		if now.After(v.expires) {
			delete(s.nonces, k)
		}
	}
	s.nonces[n] = nonce{actionID: actionID, expires: now.Add(nonceTTL)}
	return n
}

func (s *Server) consumeNonce(n, actionID string) bool {
	if n == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	got, ok := s.nonces[n]
	if !ok || got.actionID != actionID || time.Now().After(got.expires) {
		return false
	}
	delete(s.nonces, n)
	return true
}

// --- responses --------------------------------------------------------------

// writeJSON marshals first so the exact byte count can be advertised: the
// clients maintain a per-tier data counter, which is a feature on a metered plan.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encoding failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Remoter-Bytes", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// WriteError is writeError for other packages composing on this surface.
func WriteError(w http.ResponseWriter, status int, msg string) { writeError(w, status, msg) }
