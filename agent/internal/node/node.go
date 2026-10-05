// Package node runs a controllable device: the "very small service" that
// listens for the hub and starts capture, input and shells on request.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"remoter/agent/internal/actions"
	"remoter/agent/internal/api"
	"remoter/agent/internal/arming"
	"remoter/agent/internal/config"
	"remoter/agent/internal/jobs"
	"remoter/agent/internal/platform"
)

// Node is the loaded configuration shared by every role.
type Node struct {
	Cfg     config.Config
	Dir     string
	Token   string
	Version string
	Log     *slog.Logger
}

// Load reads the node's config and token.
func Load(version string, log *slog.Logger) (*Node, error) {
	n, err := LoadConfig(version, log)
	if err != nil {
		return nil, err
	}
	if n.Token, err = config.Token(n.Dir); err != nil {
		return nil, err
	}
	return n, nil
}

// LoadConfig reads the config without the token - for the helpers. The token
// is readable by SYSTEM and Administrators only, and the user helper is
// neither; it authenticates with the per-launch secret instead.
func LoadConfig(version string, log *slog.Logger) (*Node, error) {
	cfg, dir, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &Node{Cfg: cfg, Dir: dir, Version: version, Log: log}, nil
}

// Name is how the node introduces itself.
func (n *Node) Name() string {
	if n.Cfg.Name != "" {
		return n.Cfg.Name
	}
	host, _ := os.Hostname()
	return strings.ToLower(host)
}

// AllowedSources is loopback plus the hub. Never empty: a node that has not
// enrolled yet answers loopback only, not the whole tailnet.
func (n *Node) AllowedSources() []netip.Addr {
	out := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}
	for _, a := range n.Cfg.Hub.Addrs {
		if ip, err := netip.ParseAddr(a); err == nil {
			out = append(out, ip.Unmap())
		} else {
			n.Log.Warn("ignoring bad hub address", "addr", a)
		}
	}
	return out
}

func (n *Node) loadRegistry() *actions.Registry {
	reg, err := actions.Load(n.Cfg.ActionsFile)
	switch {
	case err == nil:
		return reg
	case errors.Is(err, fs.ErrNotExist):
		n.Log.Info("no actions file; the Actions tier will be empty", "path", n.Cfg.ActionsFile)
	default:
		// A typo in actions.yaml must not take the screen and shell down with it.
		n.Log.Warn("actions unavailable", "err", err)
	}
	return actions.Empty()
}

func (n *Node) newServer(reg *actions.Registry, mgr *jobs.Manager) *api.Server {
	srv := api.New(reg, mgr, n.Token, n.Version, n.Log)
	srv.Name = n.Name()
	srv.ConsoleEnabled = n.Cfg.Console.Enabled
	srv.ConsoleShell = n.Cfg.Console.Shell
	srv.ConsoleArgs = n.Cfg.Console.Args
	srv.AllowedSources = n.AllowedSources()
	return srv
}

// RunStandalone serves every tier from this one process: the Linux node, and
// a Windows node run by hand for development.
func RunStandalone(ctx context.Context, n *Node) error {
	srv := n.newServer(n.loadRegistry(), jobs.NewManager(n.Cfg.MaxJobs, nil))
	sessions := api.NewSessions()
	srv.Sessions = sessions
	// Standalone, the shell and actions run as whoever runs the node.
	if u, err := user.Current(); err == nil {
		name := u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 { // DOMAIN\user on Windows
			name = name[i+1:]
		}
		srv.User = func() string { return name }
	}

	arm := arming.New(filepath.Join(n.Dir, "arm.json"), n.Log)
	arm.Sessions = sessions.Count
	arm.Idle = platform.IdleTime
	srv.Arm = arm

	tray := make(chan platform.TrayState, 4)
	sessions.OnChange = func(list []api.Session, started *api.Session) {
		if len(list) == 0 {
			arm.SessionEnded()
		}
		sendTray(tray, list, started)
	}
	go func() {
		if err := platform.RunTray(ctx, tray); err != nil && !errors.Is(err, platform.ErrUnsupported) {
			n.Log.Warn("session indicator unavailable", "err", err)
		}
	}()
	go sweepSessions(ctx, sessions)

	stop := make(chan struct{})
	defer close(stop)
	go arm.Run(stop)

	n.Log.Info("node ready", "name", n.Name(), "mode", "standalone", "hub", n.Cfg.Hub.URL,
		"allowed", n.Cfg.Hub.Addrs, "elevated", platform.IsElevated())
	return Serve(ctx, n.Cfg.Listen, srv.Handler(api.GroupAll), n.Log, n.Hello)
}

// Hello tells the hub this node is reachable again, so it shows as online at
// once instead of at the hub's next poll. Best effort: a few tries over half a
// minute (DNS and routes can lag the tailnet address by a few seconds), and
// silence on failure - the hub's own polling is the fallback.
func (n *Node) Hello() {
	if n.Cfg.Hub.URL == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
		}
		res, err := client.Post(strings.TrimRight(n.Cfg.Hub.URL, "/")+"/hub/hello", "application/json", nil)
		if err != nil {
			continue
		}
		res.Body.Close()
		if res.StatusCode == http.StatusNoContent {
			n.Log.Info("told the hub we are back")
			return
		}
		n.Log.Warn("hub refused hello", "status", res.Status)
		return
	}
}

func sweepSessions(ctx context.Context, s *api.Sessions) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep()
		}
	}
}

// sendTray turns the session list into indicator state. Never blocks: where no
// indicator runs, nobody drains the channel, and updates are simply dropped.
func sendTray(ch chan<- platform.TrayState, list []api.Session, started *api.Session) {
	st := TrayStateFor(list, started)
	select {
	case ch <- st:
	default:
	}
}

// TrayStateFor builds what the indicator shows for a session list.
func TrayStateFor(list []api.Session, started *api.Session) platform.TrayState {
	if len(list) == 0 {
		return platform.TrayState{}
	}
	seen := map[string]bool{}
	var who []string
	for _, s := range list {
		label := s.Controller + " (" + tierName(s.Tier) + ")"
		if !seen[label] {
			seen[label] = true
			who = append(who, label)
		}
	}
	st := platform.TrayState{
		Active:  true,
		Tooltip: "Remoter - connected: " + strings.Join(who, ", "),
		Banner:  "Remote session · " + strings.Join(who, ", "),
	}
	if started != nil {
		st.NoticeTitle = "Remote session started"
		st.Notice = fmt.Sprintf("%s opened %s on this computer.", started.Controller, tierName(started.Tier))
	}
	return st
}

func tierName(t string) string {
	switch t {
	case "live":
		return "Live"
	case "console":
		return "Console"
	case "glance":
		return "Glance"
	}
	return t
}

// EnrollResult is the hub's answer to an enrolment.
type EnrollResult struct {
	State   string `json:"state"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Hub     string `json:"hub"`
	HubAddr string `json:"hubAddr"`
}

// Enroll registers this node with the hub and records the hub's address as the
// only remote address allowed to connect. Re-running it is harmless: the hub
// keeps an existing approval and takes the current token.
func Enroll(ctx context.Context, n *Node, hubURL string) (EnrollResult, error) {
	hubURL = strings.TrimRight(hubURL, "/")
	if !strings.HasPrefix(hubURL, "https://") {
		return EnrollResult{}, errors.New("hub URL must start with https://")
	}
	body, _ := json.Marshal(map[string]any{
		"token": n.Token, "port": PortOf(n.Cfg.Listen), "version": n.Version,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/hub/enroll", bytes.NewReader(body))
	if err != nil {
		return EnrollResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("cannot reach the hub (is Tailscale up on this machine?): %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return EnrollResult{}, fmt.Errorf("hub refused enrolment: %s: %s", res.Status, strings.TrimSpace(string(data)))
	}
	var out EnrollResult
	if err := json.Unmarshal(data, &out); err != nil {
		return EnrollResult{}, fmt.Errorf("unexpected hub reply: %w", err)
	}
	if _, err := netip.ParseAddr(out.HubAddr); err != nil {
		return out, fmt.Errorf("hub sent no usable address (%q)", out.HubAddr)
	}

	raw, dir, err := config.LoadRaw()
	if err != nil {
		return out, err
	}
	raw.Hub = config.HubConfig{URL: hubURL, Addrs: []string{out.HubAddr}}
	if err := config.Save(dir, raw); err != nil {
		return out, fmt.Errorf("save config: %w", err)
	}
	n.Cfg.Hub = raw.Hub
	return out, nil
}
