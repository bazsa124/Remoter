package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"remoter/agent/internal/actions"
	"remoter/agent/internal/api"
	"remoter/agent/internal/jobs"
	"remoter/agent/internal/platform"
)

// Helper roles, as the Windows service starts them.
const (
	RoleScreen = "screen-helper"
	RoleUser   = "user-helper"
)

// HelperSecretEnv carries the per-launch secret from service to helper. The
// environment rather than the command line: a SYSTEM process's environment is
// unreadable to a standard user, and the screen helper's secret is the key to
// SYSTEM-level input on the secure desktop.
const HelperSecretEnv = "REMOTER_HELPER_SECRET"

// sessionsMessage is what the service pushes to the user helper's indicator.
type sessionsMessage struct {
	Sessions []api.Session `json:"sessions"`
	Started  *api.Session  `json:"started,omitempty"`
}

// RunHelper serves one helper role on loopback until ctx ends.
func RunHelper(ctx context.Context, n *Node, role string, port int, secret string) error {
	if len(secret) < 32 {
		return errors.New("helper secret missing; helpers are started by the service, not by hand")
	}
	mux := http.NewServeMux()
	var srv *api.Server

	switch role {
	case RoleScreen:
		// Must precede the first capture: from here on every capture and input
		// call follows the input desktop, lock screen included.
		platform.FollowInputDesktop()
		srv = api.New(actions.Empty(), jobs.NewManager(1, nil), secret, n.Version, n.Log)
		srv.Mount(mux, api.GroupScreen)
		mux.HandleFunc("GET /internal/idle", func(w http.ResponseWriter, _ *http.Request) {
			idle, err := platform.IdleTime()
			if err != nil {
				api.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{"idleMs": idle.Milliseconds(), "desktop": platform.InputDesktop()})
		})

	case RoleUser:
		srv = api.New(n.loadRegistry(), jobs.NewManager(n.Cfg.MaxJobs, nil), secret, n.Version, n.Log)
		srv.ConsoleEnabled = n.Cfg.Console.Enabled
		srv.ConsoleShell = n.Cfg.Console.Shell
		srv.ConsoleArgs = n.Cfg.Console.Args
		srv.Mount(mux, api.GroupUser)

		tray := make(chan platform.TrayState, 4)
		go func() {
			if err := platform.RunTray(ctx, tray); err != nil {
				n.Log.Warn("session indicator unavailable", "err", err)
			}
		}()
		mux.HandleFunc("POST /internal/sessions", func(w http.ResponseWriter, r *http.Request) {
			var msg sessionsMessage
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&msg); err != nil {
				api.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			sendTray(tray, msg.Sessions, msg.Started)
			w.WriteHeader(http.StatusNoContent)
		})

	default:
		return fmt.Errorf("unknown helper role %q", role)
	}

	srv.Mode = role
	srv.AllowedSources = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	mux.HandleFunc("GET /internal/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	n.Log.Info("helper ready", "role", role, "port", port, "elevated", platform.IsElevated())
	return serveOn(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), srv.Wrap(mux), n.Log, nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
