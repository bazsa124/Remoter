package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The admin socket is how the owner bootstraps and recovers: approving the very
// first device, setting a PIN before any device can, or locking everyone out
// after a lost phone. It is a unix socket readable only by root and the hub's
// group, so reaching it already requires a shell on the hub machine.

func (h *Hub) serveAdmin(ctx context.Context) error {
	path := h.cfg.AdminSocket
	_ = os.Remove(path) // a stale socket from a crash would block the bind
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/access", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, h.accessLists(""))
	})
	mux.HandleFunc("POST /admin/approve", h.adminApprove)
	mux.HandleFunc("POST /admin/revoke", h.adminRevoke)
	mux.HandleFunc("PUT /admin/pin", h.adminSetPIN)
	mux.HandleFunc("DELETE /admin/pin", func(w http.ResponseWriter, _ *http.Request) {
		if err := h.store.SetPINHash(""); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		h.audit.write(AuditEntry{Event: "pin.clear", Controller: "admin"})
		writeJSON(w, http.StatusOK, map[string]string{"status": "PIN cleared"})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve(ln) }()
	h.log.Info("admin socket ready", "path", path)
	return nil
}

type adminName struct {
	Name string `json:"name"`
}

// matchBoth finds a device by name or ID in both roles. "approve laptop-a"
// should approve the laptop as a controller and as a target alike - asking the
// owner to know which list it sits in would only cause half-approvals.
func (h *Hub) matchBoth(name string) (map[string]string, error) {
	found := map[string]string{}
	for _, kind := range []string{KindController, KindNode} {
		id, err := h.store.Find(kind, name)
		switch {
		case err == nil:
			found[kind] = id
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no device named %q - run 'remoter-hub list'", name)
	}
	return found, nil
}

func (h *Hub) adminApprove(w http.ResponseWriter, r *http.Request) {
	var b adminName
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b); err != nil || b.Name == "" {
		writeError(w, http.StatusBadRequest, "name required", "")
		return
	}
	found, err := h.matchBoth(b.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}
	var done []string
	for kind, id := range found {
		if err := h.store.Approve(kind, id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		done = append(done, kind)
		h.audit.write(AuditEntry{Event: "access.approve", Controller: "admin", Device: b.Name, Detail: kind})
	}
	h.presence.poke(context.Background())
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved " + b.Name + " as " + strings.Join(done, " and ")})
}

func (h *Hub) adminRevoke(w http.ResponseWriter, r *http.Request) {
	var b adminName
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b); err != nil || b.Name == "" {
		writeError(w, http.StatusBadRequest, "name required", "")
		return
	}
	found, err := h.matchBoth(b.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}
	for kind, id := range found {
		if err := h.store.Remove(kind, id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		h.afterRemove(kind, id)
		h.audit.write(AuditEntry{Event: "access.remove", Controller: "admin", Device: b.Name, Detail: kind})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed " + b.Name})
}

func (h *Hub) adminSetPIN(w http.ResponseWriter, r *http.Request) {
	var b pinBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "")
		return
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
	h.audit.write(AuditEntry{Event: "pin.set", Controller: "admin"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "PIN set"})
}

// AdminClient talks to a running hub's admin socket - the CLI side.
type AdminClient struct {
	client *http.Client
}

// NewAdminClient dials the socket at path for every request.
func NewAdminClient(path string) *AdminClient {
	return &AdminClient{client: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}}
}

// Do sends one admin request and returns the decoded JSON body.
func (a *AdminClient) Do(method, path string, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, "http://remoter-hub"+path, rd)
	if err != nil {
		return nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the hub's admin socket (is the hub running, and are you root?): %w", err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode >= 300 {
		if msg, ok := out["error"].(string); ok {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("hub returned %s", res.Status)
	}
	return out, nil
}
