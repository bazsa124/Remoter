// Package hub is the jump server: the one machine every controller talks to,
// and the only machine any target accepts connections from.
//
// It authenticates callers by their Tailscale identity, gates the screen and
// shell tiers behind a PIN, and relays every tier to the chosen node. Nothing
// about the tiers themselves lives here - the node still owns capture, input
// and shells - which is why relaying is cheap to add on top of what existed.
package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"remoter/agent/internal/push"
	"remoter/agent/internal/webui"
)

// Config is hub.json in the hub's state directory.
type Config struct {
	// Port is the HTTPS port, bound on the tailnet address only.
	Port int `json:"port"`

	// NodePort is the default port nodes listen on.
	NodePort int `json:"nodePort"`

	// Ntfy carries every push: job results, sessions, approvals, outages.
	NtfyServer string `json:"ntfyServer"`
	NtfyTopic  string `json:"ntfyTopic"`

	TailscaleSocket string `json:"tailscaleSocket"`
	AdminSocket     string `json:"adminSocket"`

	// WebDir holds the built client and the downloadable node and app builds.
	WebDir string `json:"webDir"`
}

func defaultConfig() Config {
	return Config{
		Port:            443,
		NodePort:        8737,
		NtfyServer:      "https://ntfy.sh",
		TailscaleSocket: "/var/run/tailscale/tailscaled.sock",
		AdminSocket:     "/run/remoter/admin.sock",
	}
}

// LoadConfig reads hub.json from dir, writing defaults on first run.
func LoadConfig(dir string) (Config, error) {
	cfg := defaultConfig()
	path := filepath.Join(dir, "hub.json")
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return cfg, fmt.Errorf("write default config: %w", err)
		}
		return cfg, nil
	case err != nil:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Port == 0 {
		cfg.Port = 443
	}
	if cfg.NodePort == 0 {
		cfg.NodePort = 8737
	}
	return cfg, nil
}

// Hub is the running jump server.
type Hub struct {
	cfg     Config
	dir     string
	webDir  string
	version string
	log     *slog.Logger

	ts       *tsapi
	store    *Store
	pins     *pinGate
	audit    *auditLog
	push     *push.Publisher
	sessions *sessionTracker
	presence *presence

	nodeTransport *http.Transport
	nodeClient    *http.Client

	selfMu sync.RWMutex
	self   SelfInfo

	certMu sync.Mutex
	cert   *tls.Certificate
}

// New builds a hub over the state directory dir.
func New(cfg Config, dir, webDir, version string, log *slog.Logger) (*Hub, error) {
	store, err := OpenStore(dir)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	h := &Hub{
		cfg: cfg, dir: dir, webDir: webDir, version: version, log: log,
		ts:            newTSAPI(cfg.TailscaleSocket),
		store:         store,
		pins:          newPINGate(),
		audit:         newAuditLog(dir),
		push:          push.New(cfg.NtfyServer, cfg.NtfyTopic),
		sessions:      newSessionTracker(),
		nodeTransport: transport,
		nodeClient:    &http.Client{Transport: transport},
	}
	h.presence = newPresence(h)
	return h, nil
}

// Self reports the hub's own tailnet identity.
func (h *Hub) Self() SelfInfo {
	h.selfMu.RLock()
	defer h.selfMu.RUnlock()
	return h.self
}

// Run serves until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	// Tailscale may still be coming up at boot. Waiting beats exiting: systemd
	// would restart us anyway, but a crash loop buries the real cause in logs.
	self, err := h.waitForTailscale(ctx)
	if err != nil {
		return err
	}
	h.selfMu.Lock()
	h.self = self
	h.selfMu.Unlock()
	h.log.Info("tailnet identity", "name", self.DNSName, "ip", self.IPv4)

	if err := h.refreshCert(ctx); err != nil {
		return fmt.Errorf("TLS certificate for %s: %w (enable HTTPS in the Tailscale admin console, and set TS_PERMIT_CERT_UID for this user)", self.DNSName, err)
	}

	go h.presence.run(ctx)
	go h.certLoop(ctx)

	if h.cfg.AdminSocket != "" {
		if err := h.serveAdmin(ctx); err != nil {
			h.log.Warn("admin socket unavailable", "err", err)
		}
	}

	addr := net.JoinHostPort(self.IPv4, strconv.Itoa(h.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return h.currentCert() },
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
	}
	h.log.Info("hub ready", "url", "https://"+self.DNSName, "listen", addr, "version", h.version)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(ln, "", "") }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (h *Hub) waitForTailscale(ctx context.Context) (SelfInfo, error) {
	warned := false
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		self, err := h.ts.self(cctx)
		cancel()
		if err == nil {
			return self, nil
		}
		if !warned {
			h.log.Warn("waiting for tailscale", "err", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return SelfInfo{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (h *Hub) refreshCert(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // first issuance goes to Let's Encrypt
	defer cancel()
	cert, err := h.ts.certificate(cctx, h.Self().DNSName)
	if err != nil {
		return err
	}
	h.certMu.Lock()
	h.cert = cert
	h.certMu.Unlock()
	return nil
}

// certLoop re-asks tailscaled twice a day. tailscaled renews well before
// expiry and hands back its cached pair otherwise, so this is cheap.
func (h *Hub) certLoop(ctx context.Context) {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.refreshCert(ctx); err != nil {
				h.log.Warn("certificate refresh failed; keeping the current one", "err", err)
			}
		}
	}
}

func (h *Hub) currentCert() (*tls.Certificate, error) {
	h.certMu.Lock()
	defer h.certMu.Unlock()
	if h.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return h.cert, nil
}

// Handler is the public HTTPS surface.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /hub/me", h.withPeer(h.me, allowPending))
	mux.HandleFunc("GET /hub/devices", h.withPeer(h.devices))
	mux.HandleFunc("GET /hub/access", h.withPeer(h.access))
	mux.HandleFunc("POST /hub/access/{kind}/{id}/approve", h.withPeer(h.withPIN(h.approve)))
	mux.HandleFunc("DELETE /hub/access/{kind}/{id}", h.withPeer(h.withPIN(h.removeAccess)))
	mux.HandleFunc("POST /hub/pin", h.withPeer(h.enterPIN))
	mux.HandleFunc("PUT /hub/pin", h.withPeer(h.changePIN))
	mux.HandleFunc("DELETE /hub/pin/grant", h.withPeer(h.lock))
	mux.HandleFunc("GET /hub/audit", h.withPeer(h.withPIN(h.auditList)))
	mux.HandleFunc("POST /hub/enroll", h.enroll)
	mux.HandleFunc("POST /hub/hello", h.hello)
	mux.HandleFunc("/hub/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint", "")
	})
	mux.Handle("/d/", h.withPeer(h.relay))

	if h.webDir != "" {
		mux.Handle("/", webui.Handler(h.webDir))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"hub": "remoter", "ui": "not installed"})
		})
	}
	return h.guard(mux)
}

func (h *Hub) notify(msg string, priority int) {
	if !h.push.Enabled() {
		return
	}
	click := "https://" + h.Self().DNSName
	go func() {
		if err := h.push.SendLink("Remoter", msg, priority, click); err != nil {
			h.log.Warn("ntfy publish failed", "err", err)
		}
	}()
}

// --- responses --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encoding failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Remoter-Bytes", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError sends {"error", "code"}. The code is what clients branch on -
// "pending", "pin_required", "pin_unset" - so the message can stay human.
func writeError(w http.ResponseWriter, status int, msg, code string) {
	body := map[string]string{"error": msg}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}
