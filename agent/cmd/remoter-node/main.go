// Command remoter-node makes this machine controllable through the hub.
//
//	remoter-node [run]                 serve every tier from this process
//	remoter-node enroll -hub URL       register with the hub (then approve it there)
//	remoter-node print-token           print the token the hub uses
//
// Windows only:
//
//	remoter-node install | uninstall   register / remove the boot-time service
//	remoter-node service               the service itself (started by Windows)
//	remoter-node screen-helper|user-helper   started by the service, never by hand
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"remoter/agent/internal/config"
	"remoter/agent/internal/node"
)

var version = "0.2.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "remoter-node:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// Windows starts the service with the "service" argument, but be robust to
	// a bare invocation by the service manager too.
	if cmd == "run" && node.IsService() {
		cmd = "service"
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configDir := fs.String("config", "", "override the configuration directory")
	logPath := fs.String("log", "", "append logs to this file instead of stdout")
	debugLog := fs.Bool("debug", false, "verbose logging")
	hubURL := fs.String("hub", "", "hub URL, for enroll (e.g. https://hub.example.ts.net)")
	port := fs.Int("port", 0, "loopback port, for helpers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configDir != "" {
		config.SetDir(*configDir)
	}

	switch cmd {
	case "service":
		if *logPath == "" {
			*logPath = defaultLog("node.log")
		}
	case node.RoleScreen:
		if *logPath == "" {
			*logPath = defaultLog(node.RoleScreen + ".log")
		}
	case node.RoleUser:
		// Runs as the signed-in user, who cannot write the SYSTEM-owned log
		// directory; log into their profile instead.
		if *logPath == "" {
			if local, err := os.UserCacheDir(); err == nil {
				*logPath = filepath.Join(local, "Remoter", "user-helper.log")
			}
		}
	}
	log, closeLog := openLog(*logPath, *debugLog)
	defer closeLog()
	// A service or helper has no console: an error returned from here would
	// vanish with stderr. Put it in the log, where someone will look.
	defer func() {
		if err != nil {
			log.Error("exiting", "cmd", cmd, "err", err)
		}
	}()

	switch cmd {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := node.InstallService(exe); err != nil {
			return err
		}
		fmt.Println("service", node.ServiceName, "registered for", exe)
		return nil
	case "uninstall":
		if err := node.UninstallService(); err != nil {
			return err
		}
		fmt.Println("service", node.ServiceName, "removed")
		return nil
	}

	var n *node.Node
	if cmd == node.RoleScreen || cmd == node.RoleUser {
		n, err = node.LoadConfig(version, log)
	} else {
		n, err = node.Load(version, log)
	}
	if err != nil {
		return err
	}

	switch cmd {
	case "print-token":
		fmt.Println(n.Token)
		return nil

	case "enroll":
		url := *hubURL
		if url == "" {
			url = n.Cfg.Hub.URL
		}
		if url == "" {
			return errors.New("usage: remoter-node enroll -hub https://<hub>.<tailnet>.ts.net")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := node.Enroll(ctx, n, url)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled with %s as %q: %s\n", res.Hub, res.Name, res.State)
		fmt.Printf("only %s may connect from now on\n", res.HubAddr)
		if res.State != "approved" {
			fmt.Printf("approve it in the Remoter app, or on the hub: sudo remoter-hub approve %s\n", res.Name)
		}
		return nil

	case "service":
		return node.RunService(n)

	case node.RoleScreen, node.RoleUser:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return node.RunHelper(ctx, n, cmd, *port, os.Getenv(node.HelperSecretEnv))

	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return node.RunStandalone(ctx, n)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func defaultLog(name string) string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "logs", name)
}

// openLog appends to path, or logs to stdout when it is empty.
//
// If the file cannot be opened, degrade to stderr and carry on. Losing logs is
// an inconvenience; refusing to start because of them makes the machine
// unreachable, which is the failure this whole project exists to avoid.
func openLog(path string, verbose bool) (*slog.Logger, func()) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	var out io.Writer = os.Stdout
	closeFn := func() {}
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		// Keep one previous file; a node runs for months.
		if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
			_ = os.Rename(path, path+".1")
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "remoter-node: cannot write %s (%v); logging to stderr\n", path, err)
			out = os.Stderr
		} else {
			out = f
			closeFn = func() { _ = f.Close() }
			// A service has no console: without this a panic vanishes and the
			// only trace is "the service terminated unexpectedly".
			_ = debug.SetCrashOutput(f, debug.CrashOptions{})
		}
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), closeFn
}
