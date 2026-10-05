package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cgnat is the range Tailscale assigns tailnet addresses from.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// TailnetIP returns this machine's tailnet IPv4, or false if Tailscale is not up.
//
// Found by address range rather than by interface name: the adapter is called
// "Tailscale" on Windows and "tailscale0" on Linux, and the range is the one
// thing both agree on.
func TailnetIP() (netip.Addr, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			pfx, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			if ip := pfx.Addr().Unmap(); ip.Is4() && cgnat.Contains(ip) {
				return ip, true
			}
		}
	}
	return netip.Addr{}, false
}

// Serve runs handler on every address until ctx ends.
//
// "tailscale:PORT" entries bind the tailnet address once it exists and re-bind
// if it changes. At boot the service starts before Tailscale has an address;
// waiting for it - rather than failing, or binding 0.0.0.0 to avoid the
// question - is what makes the node reachable after an unattended reboot.
//
// onTailnet, if set, is called each time the tailnet listener binds - the
// moment the hub can reach this node again. A node with no tailnet listener
// (one on the hub's own machine, reached over loopback) gets the call when its
// first listener binds instead.
func Serve(ctx context.Context, addrs []string, handler http.Handler, log *slog.Logger, onTailnet func()) error {
	if len(addrs) == 0 {
		return errors.New("no listen addresses configured")
	}
	hasTailnet := false
	for _, a := range addrs {
		if host, _, err := net.SplitHostPort(a); err == nil && strings.EqualFold(host, "tailscale") {
			hasTailnet = true
		}
	}
	var once sync.Once
	firstBind := func() {
		if onTailnet != nil && !hasTailnet {
			once.Do(onTailnet)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(addrs))
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return fmt.Errorf("bad listen address %q: %w", a, err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" || strings.EqualFold(host, "[::]") {
			return fmt.Errorf("listen address %q binds all interfaces; use loopback and tailscale:%s", a, port)
		}
		wg.Add(1)
		if strings.EqualFold(host, "tailscale") {
			go func() { defer wg.Done(); serveTailnet(ctx, port, handler, log, onTailnet) }()
			continue
		}
		go func() {
			defer wg.Done()
			if err := serveOn(ctx, a, handler, log, firstBind); err != nil {
				errCh <- err
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case err := <-errCh:
		return err
	case <-done:
		return nil
	}
}

func serveOn(ctx context.Context, addr string, handler http.Handler, log *slog.Logger, ready func()) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if ready != nil {
		go ready()
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func serveTailnet(ctx context.Context, port string, handler http.Handler, log *slog.Logger, onUp func()) {
	var (
		current netip.Addr
		cancel  context.CancelFunc = func() {}
		warned  bool
		failed  = make(chan netip.Addr, 1)
	)
	defer func() { cancel() }()
	for {
		ip, ok := TailnetIP()
		switch {
		case !ok && !warned:
			log.Warn("waiting for a tailnet address")
			warned = true
		case ok && ip != current:
			cancel()
			sctx, stopListener := context.WithCancel(ctx)
			cancel = stopListener
			addr := net.JoinHostPort(ip.String(), port)
			current, warned = ip, false
			go func(ip netip.Addr) {
				// A bind can fail briefly while the adapter settles; report it
				// and the loop retries on the next tick.
				if err := serveOn(sctx, addr, handler, log, onUp); err != nil {
					log.Warn("tailnet listener stopped", "addr", addr, "err", err)
					select {
					case failed <- ip:
					default:
					}
				}
			}(ip)
		}
		delay := 30 * time.Second
		if !current.IsValid() {
			delay = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case ip := <-failed:
			if ip == current {
				current = netip.Addr{}
			}
		case <-time.After(delay):
		}
	}
}

// PortOf returns the port of the first "tailscale:PORT" listen entry, which is
// the one the hub connects to.
func PortOf(addrs []string) int {
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err == nil && strings.EqualFold(host, "tailscale") {
			if p, err := strconv.Atoi(port); err == nil {
				return p
			}
		}
	}
	return 8737
}
