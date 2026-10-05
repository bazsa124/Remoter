//go:build windows

package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"remoter/agent/internal/actions"
	"remoter/agent/internal/api"
	"remoter/agent/internal/arming"
	"remoter/agent/internal/jobs"
	"remoter/agent/internal/platform"
)

// ServiceName is the Windows service the installer registers.
const ServiceName = "RemoterNode"

// IsService reports whether the process was started by the service manager.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// RunService runs the node under the service control manager.
func RunService(n *Node) error { return svc.Run(ServiceName, &service{n: n}) }

type service struct{ n *Node }

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runService(ctx, s.n) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(8 * time.Second):
				}
				return false, 0
			}
		case err := <-done:
			cancel()
			if err != nil {
				s.n.Log.Error("service stopped", "err", err)
				return false, 1
			}
			return false, 0
		}
	}
}

// runService is the network-facing half: health, arming, and a relay to the
// helpers that actually touch the desktop.
func runService(ctx context.Context, n *Node) error {
	job, err := platform.NewJob()
	if err != nil {
		return err
	}
	sup := &supervisor{n: n, job: job, helpers: map[string]*helper{}, backoff: map[string]*startBackoff{}}
	if sup.exe, err = os.Executable(); err != nil {
		return err
	}

	srv := n.newServer(actions.Empty(), jobs.NewManager(1, nil))
	srv.Mode = "service"
	sessions := api.NewSessions()
	srv.Sessions = sessions

	arm := arming.New(filepath.Join(n.Dir, "arm.json"), n.Log)
	arm.Sessions = sessions.Count
	arm.Idle = sup.idle
	srv.Arm = arm
	sup.armed = func() bool { return arm.State().Armed }

	// The Wi-Fi safety net: get this machine onto the network at boot, and keep
	// it there while armed, even when the adapter is slow to do it by itself.
	go newWifiNudger(n.Dir, sup.armed, n.Log).run(ctx)

	srv.Tiers = sup.tiers
	srv.User = sup.user

	var awake func()
	sessions.OnChange = func(list []api.Session, started *api.Session) {
		sup.pushSessions(list, started)
		sup.mu.Lock()
		defer sup.mu.Unlock()
		switch {
		case len(list) > 0 && awake == nil:
			awake = platform.HoldAwake(false)
		case len(list) == 0 && awake != nil:
			awake()
			awake = nil
			arm.SessionEnded()
		}
	}

	mux := http.NewServeMux()
	srv.Mount(mux, api.GroupCore)
	mux.Handle("/api/", http.HandlerFunc(sup.relay))

	go sup.run(ctx)
	go sweepSessions(ctx, sessions)
	stop := make(chan struct{})
	defer close(stop)
	go arm.Run(stop)

	n.Log.Info("node ready", "name", n.Name(), "mode", "service", "hub", n.Cfg.Hub.URL, "allowed", n.Cfg.Hub.Addrs)
	return Serve(ctx, n.Cfg.Listen, srv.Wrap(mux), n.Log, n.Hello)
}

// roleFor routes a node path to the helper that serves it.
func roleFor(path string) string {
	switch {
	case path == "/api/monitors", path == "/api/screenshot", path == "/api/live":
		return RoleScreen
	case path == "/api/actions", strings.HasPrefix(path, "/api/actions/"),
		path == "/api/jobs", strings.HasPrefix(path, "/api/jobs/"),
		path == "/api/stream", path == "/api/console":
		return RoleUser
	}
	return ""
}

type helper struct {
	role    string
	session uint32
	proc    *platform.Process
	port    int
	secret  string
	proxy   *httputil.ReverseProxy
	ready   chan struct{}
	err     error // set before ready closes when start-up failed

	inflight int
	lastUsed time.Time
}

func (h *helper) base() string { return "http://127.0.0.1:" + strconv.Itoa(h.port) }

type supervisor struct {
	n     *Node
	exe   string
	job   *platform.Job
	armed func() bool

	mu      sync.Mutex
	helpers map[string]*helper
	lastMsg []byte // last session list, replayed to a fresh user helper
	backoff map[string]*startBackoff
}

// startBackoff spaces out restarts of a helper that keeps failing, whether the
// retry comes from the reconcile loop or from a request.
type startBackoff struct {
	failures int
	retryAt  time.Time
	lastErr  error
}

const (
	helperIdle  = 30 * time.Second // screen helper lingers this long after use
	helperStart = 15 * time.Second
)

// errNoConsole means the console session is in transition (fast user switch).
var errNoConsole = errors.New("no console session right now; try again in a moment")

// ensure returns a ready helper for role, starting one if needed.
func (s *supervisor) ensure(ctx context.Context, role string) (*helper, error) {
	s.mu.Lock()
	cs, ok := platform.ConsoleSession()
	if !ok {
		s.mu.Unlock()
		return nil, errNoConsole
	}
	h := s.helpers[role]
	if h != nil && (h.session != cs || !h.alive()) {
		h.proc.Kill()
		delete(s.helpers, role)
		h = nil
	}
	if h == nil {
		if b := s.backoff[role]; b != nil && time.Now().Before(b.retryAt) {
			s.mu.Unlock()
			return nil, b.lastErr
		}
		var err error
		if h, err = s.startLocked(role, cs); err != nil {
			s.failedLocked(role, err)
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()

	select {
	case <-h.ready:
		if h.err != nil {
			return nil, h.err
		}
		return h, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// alive is true while the process runs; a helper still starting counts as alive.
func (h *helper) alive() bool { return h.proc.Alive() }

func (s *supervisor) startLocked(role string, session uint32) (*helper, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	secretBytes := make([]byte, 32)
	_, _ = rand.Read(secretBytes)
	h := &helper{
		role: role, session: session, port: port,
		secret: hex.EncodeToString(secretBytes), ready: make(chan struct{}), lastUsed: time.Now(),
	}

	logPath := filepath.Join(s.n.Dir, "logs", role+".log")
	if role == RoleUser {
		// The user helper runs as the user and cannot write to the
		// SYSTEM-owned log directory; it logs into its own profile instead.
		logPath = ""
	}
	args := []string{role, "-port", strconv.Itoa(port)}
	if logPath != "" {
		args = append(args, "-log", logPath)
	}
	proc, err := platform.SpawnInSession(platform.SpawnOptions{
		Session: session,
		AsUser:  role == RoleUser,
		Exe:     s.exe,
		Args:    args,
		Env:     []string{HelperSecretEnv + "=" + h.secret},
		Dir:     filepath.Dir(s.exe),
		Job:     s.job,
	})
	if err != nil {
		return nil, err
	}
	h.proc = proc
	h.proxy = s.newProxy(h)
	s.helpers[role] = h
	s.n.Log.Info("helper started", "role", role, "session", session, "pid", proc.Pid)

	go s.awaitReady(h)
	return h, nil
}

func (s *supervisor) awaitReady(h *helper) {
	deadline := time.Now().Add(helperStart)
	for time.Now().Before(deadline) {
		if !h.proc.Alive() {
			h.err = fmt.Errorf("%s exited during start-up; see its log", h.role)
			break
		}
		req, _ := http.NewRequest(http.MethodGet, h.base()+"/internal/ready", nil)
		req.Header.Set("Authorization", "Bearer "+h.secret)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusNoContent {
				s.mu.Lock()
				delete(s.backoff, h.role)
				s.mu.Unlock()
				close(h.ready)
				if h.role == RoleUser {
					s.replaySessions(h)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if h.err == nil {
		h.err = fmt.Errorf("%s did not become ready", h.role)
	}
	s.n.Log.Warn("helper failed to start", "role", h.role, "err", h.err)
	s.mu.Lock()
	if s.helpers[h.role] == h {
		h.proc.Kill()
		delete(s.helpers, h.role)
	}
	s.failedLocked(h.role, h.err)
	s.mu.Unlock()
	close(h.ready)
}

// failedLocked records a failed start and pushes the next attempt out: 5 s,
// 10 s, 15 s ... capped at two minutes.
func (s *supervisor) failedLocked(role string, err error) {
	b := s.backoff[role]
	if b == nil {
		b = &startBackoff{}
		s.backoff[role] = b
	}
	b.failures++
	b.lastErr = err
	b.retryAt = time.Now().Add(min(time.Duration(b.failures)*5*time.Second, 2*time.Minute))
}

func (s *supervisor) newProxy(h *helper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "127.0.0.1:" + strconv.Itoa(h.port)
			pr.Out.Host = pr.Out.URL.Host
			// The hub's token stops here; helpers only know their own secret.
			pr.Out.Header.Set("Authorization", "Bearer "+h.secret)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			api.WriteError(w, http.StatusBadGateway, h.role+" is not responding: "+err.Error())
		},
	}
}

// relay hands a tier request to its helper.
func (s *supervisor) relay(w http.ResponseWriter, r *http.Request) {
	role := roleFor(r.URL.Path)
	if role == "" {
		api.WriteError(w, http.StatusNotFound, "no such endpoint")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), helperStart+time.Second)
	h, err := s.ensure(ctx, role)
	cancel()
	if err != nil {
		if errors.Is(err, platform.ErrNoUser) {
			api.WriteError(w, http.StatusServiceUnavailable,
				"nobody is signed in on "+s.n.Name()+" - sign in through Live first")
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	s.mu.Lock()
	h.inflight++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		h.inflight--
		h.lastUsed = time.Now()
		s.mu.Unlock()
	}()
	h.proxy.ServeHTTP(w, r)
}

// run reconciles helpers with the console session every two seconds.
//
// The user helper lives as long as someone is signed in: it owns job history
// and the event stream the hub listens to, and restarting it would lose both.
// The screen helper is started on demand and stopped when idle - except while
// armed, when it is the only thing that can notice the owner coming back.
func (s *supervisor) run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, h := range s.helpers {
				h.proc.Kill()
			}
			s.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (s *supervisor) reconcile(ctx context.Context) {
	cs, haveConsole := platform.ConsoleSession()
	signedIn := haveConsole && platform.SessionUser(cs) != ""
	armed := s.armed != nil && s.armed()

	s.mu.Lock()
	for role, h := range s.helpers {
		stale := haveConsole && h.session != cs
		select {
		case <-h.ready:
		default:
			continue // still starting; awaitReady owns it
		}
		switch {
		case !h.alive(), stale, role == RoleUser && !signedIn:
			s.n.Log.Info("helper stopped", "role", role, "alive", h.alive(), "stale", stale)
			h.proc.Kill()
			delete(s.helpers, role)
		case role == RoleScreen && !armed && h.inflight == 0 && time.Since(h.lastUsed) > helperIdle:
			s.n.Log.Info("screen helper idle; stopping")
			h.proc.Kill()
			delete(s.helpers, role)
		}
	}
	_, haveUser := s.helpers[RoleUser]
	_, haveScreen := s.helpers[RoleScreen]
	s.mu.Unlock()

	// ensure applies the start backoff itself, so the loop can simply ask.
	if signedIn && !haveUser {
		go func() {
			if _, err := s.ensure(ctx, RoleUser); err != nil {
				s.n.Log.Debug("user helper unavailable", "err", err)
			}
		}()
	}
	if armed && !haveScreen && haveConsole {
		go func() { _, _ = s.ensure(ctx, RoleScreen) }()
	}
}

// pushSessions forwards the session list to the user helper's indicator.
func (s *supervisor) pushSessions(list []api.Session, started *api.Session) {
	body, _ := json.Marshal(sessionsMessage{Sessions: list, Started: started})
	replay, _ := json.Marshal(sessionsMessage{Sessions: list})
	s.mu.Lock()
	s.lastMsg = replay
	h := s.helpers[RoleUser]
	s.mu.Unlock()
	if h != nil {
		go postSessions(h, body)
	}
}

func (s *supervisor) replaySessions(h *helper) {
	s.mu.Lock()
	body := s.lastMsg
	s.mu.Unlock()
	if body != nil {
		go postSessions(h, body)
	}
}

func postSessions(h *helper, body []byte) {
	select {
	case <-h.ready:
	case <-time.After(helperStart):
		return
	}
	if h.err != nil {
		return
	}
	req, _ := http.NewRequest(http.MethodPost, h.base()+"/internal/sessions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+h.secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	if res, err := client.Do(req); err == nil {
		res.Body.Close()
	}
}

// idle asks the screen helper how long the console has gone without input.
func (s *supervisor) idle() (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helperStart)
	defer cancel()
	h, err := s.ensure(ctx, RoleScreen)
	if err != nil {
		return 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.base()+"/internal/idle", nil)
	req.Header.Set("Authorization", "Bearer "+h.secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var out struct {
		IdleMs int64 `json:"idleMs"`
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK || json.Unmarshal(data, &out) != nil {
		return 0, fmt.Errorf("idle: %s", strings.TrimSpace(string(data)))
	}
	return time.Duration(out.IdleMs) * time.Millisecond, nil
}

func (s *supervisor) tiers() map[string]any {
	cs, ok := platform.ConsoleSession()
	user := ""
	if ok {
		user = platform.SessionUser(cs)
	}
	t := map[string]any{
		"glance":  ok,
		"live":    ok,
		"actions": user != "",
		"console": user != "" && s.n.Cfg.Console.Enabled,
	}
	if !ok {
		t["glanceReason"] = errNoConsole.Error()
	}
	switch {
	case user == "":
		reason := "nobody is signed in - sign in through Live first"
		t["actionsReason"], t["consoleReason"] = reason, reason
	case !s.n.Cfg.Console.Enabled:
		t["consoleReason"] = "console is disabled in the node config"
	}
	return t
}

func (s *supervisor) user() string {
	cs, ok := platform.ConsoleSession()
	if !ok {
		return ""
	}
	u := platform.SessionUser(cs)
	if i := strings.LastIndexByte(u, '\\'); i >= 0 {
		u = u[i+1:]
	}
	return u
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// InstallService registers the node as an automatic LocalSystem service, set to
// restart itself if it ever dies: an unreachable node is the failure this whole
// project exists to prevent.
func InstallService(exe string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run elevated): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(ServiceName); err == nil {
		defer s.Close()
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = `"` + exe + `" service`
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			return err
		}
		return setRecovery(s)
	}

	s, err := m.CreateService(ServiceName, exe, mgr.Config{
		DisplayName: "Remoter Node",
		Description: "Lets the Remoter hub reach this computer: screen, input, shell and actions on request.",
		StartType:   mgr.StartAutomatic,
	}, "service")
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	return setRecovery(s)
}

func setRecovery(s *mgr.Service) error {
	return s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, 24*60*60)
}

// UninstallService stops and removes the service.
func UninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run elevated): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", ServiceName)
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	for i := 0; i < 20; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return s.Delete()
}
