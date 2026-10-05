// Command remoter-hub is the jump server between controllers and nodes.
//
//	remoter-hub [-dir /var/lib/remoter] [-web /opt/remoter/web]   serve
//	remoter-hub list                                              show devices
//	remoter-hub approve <name>                                    approve a waiting device
//	remoter-hub revoke <name>                                     remove a device
//	remoter-hub set-pin                                           set the PIN (read from stdin)
//	remoter-hub clear-pin                                         remove the PIN
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"remoter/agent/internal/hub"
	"remoter/agent/internal/webui"
)

var version = "0.2.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "remoter-hub:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "/var/lib/remoter", "state directory (hub.json, state.json, audit.log)")
	web := flag.String("web", "", "built web client directory (default: <binary dir>/web)")
	admin := flag.String("admin", "/run/remoter/admin.sock", "admin socket, for the CLI subcommands")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	if cmd := flag.Arg(0); cmd != "" {
		return cli(hub.NewAdminClient(*admin), cmd, flag.Args()[1:])
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	cfg, err := hub.LoadConfig(*dir)
	if err != nil {
		return err
	}
	webDir := *web
	if webDir == "" {
		webDir = cfg.WebDir
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Join(filepath.Dir(exe), "web")
	}
	webDir = webui.Resolve(webDir, exeDir)
	if webDir == "" {
		log.Warn("no built client found; serving the API only")
	}

	h, err := hub.New(cfg, *dir, webDir, version, log)
	if err != nil {
		return err
	}
	if cfg.NtfyTopic == "" {
		log.Warn("ntfyTopic is empty in hub.json: pushes are disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return h.Run(ctx)
}

func cli(a *hub.AdminClient, cmd string, args []string) error {
	switch cmd {
	case "list":
		out, err := a.Do("GET", "/admin/access", nil)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ROLE\tNAME\tSTATE\tOS\tID")
		for _, role := range []string{"controllers", "nodes"} {
			list, _ := out[role].([]any)
			for _, item := range list {
				d, _ := item.(map[string]any)
				fmt.Fprintf(tw, "%s\t%v\t%v\t%v\t%v\n", strings.TrimSuffix(role, "s"), d["name"], d["state"], d["os"], d["id"])
			}
		}
		return tw.Flush()

	case "approve", "revoke":
		if len(args) != 1 {
			return fmt.Errorf("usage: remoter-hub %s <name>", cmd)
		}
		out, err := a.Do("POST", "/admin/"+cmd, map[string]string{"name": args[0]})
		if err != nil {
			return err
		}
		fmt.Println(out["status"])
		return nil

	case "set-pin":
		// From stdin, never argv: a PIN on the command line lands in shell
		// history and in every process listing.
		fmt.Fprint(os.Stderr, "New PIN (4-12 digits): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		out, err := a.Do("PUT", "/admin/pin", map[string]string{"pin": strings.TrimSpace(line)})
		if err != nil {
			return err
		}
		fmt.Println(out["status"])
		return nil

	case "clear-pin":
		out, err := a.Do("DELETE", "/admin/pin", nil)
		if err != nil {
			return err
		}
		fmt.Println(out["status"])
		return nil
	}
	return fmt.Errorf("unknown command %q (list, approve, revoke, set-pin, clear-pin)", cmd)
}
