// Package config resolves on-disk locations and loads node settings.
//
// Path resolution is the single place in the agent that knows about OS
// differences in filesystem layout. Nothing else may build a path by hand.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the node's runtime settings, loaded from config.json.
type Config struct {
	// Name is how the node introduces itself; empty means the host name. The
	// hub labels devices by their Tailscale name regardless.
	Name string `json:"name,omitempty"`

	// Listen holds host:port pairs to serve on. "tailscale:PORT" means the
	// tailnet address, discovered at runtime and re-bound if it appears late -
	// at boot the service usually starts before Tailscale is up. Never 0.0.0.0.
	Listen []string `json:"listen"`

	// Hub is the jump server this node answers to.
	Hub HubConfig `json:"hub"`

	// ActionsFile is the registry path, relative to Dir() if not absolute.
	ActionsFile string `json:"actionsFile"`

	// MaxJobs bounds retained job history.
	MaxJobs int `json:"maxJobs"`

	// Console is Tier 2. It is a real shell and therefore strictly more
	// powerful than the action whitelist, so it is switchable.
	Console ConsoleConfig `json:"console"`
}

// HubConfig names the hub and the addresses it connects from.
type HubConfig struct {
	// URL is where the node enrols, e.g. https://hub.example.ts.net.
	URL string `json:"url"`

	// Addrs are the only remote addresses allowed to connect. Filled in from the
	// hub's answer at enrolment; loopback is always allowed in addition.
	Addrs []string `json:"addrs"`
}

// ConsoleConfig controls the Tier 2 shell.
type ConsoleConfig struct {
	Enabled bool     `json:"enabled"`
	Shell   string   `json:"shell"` // empty: pick the host default
	Args    []string `json:"args"`
}

func defaults() Config {
	return Config{
		Listen:      []string{"tailscale:8737"},
		ActionsFile: "actions.yaml",
		MaxJobs:     100,
		Console:     ConsoleConfig{Enabled: true},
	}
}

// Save writes cfg back to config.json in dir, without a BOM.
func Save(dir string, cfg Config) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Store the actions path as the user wrote it: relative stays relative.
	return os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600)
}

// override replaces the OS-derived location when set. It exists so a second
// agent can run against a scratch config without touching the real one.
var override string

// SetDir pins the configuration directory, overriding the OS default.
func SetDir(dir string) { override = dir }

// Dir returns the agent's configuration directory, creating it if needed.
//
// Windows: %ProgramData%\Remoter (readable by a service in session 0).
// Others:  $XDG_CONFIG_HOME/remoter, falling back to ~/.config/remoter.
func Dir() (string, error) {
	if override != "" {
		if err := os.MkdirAll(override, 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", override, err)
		}
		return override, nil
	}

	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("ProgramData")
	}
	if base == "" {
		var err error
		if base, err = os.UserConfigDir(); err != nil {
			return "", fmt.Errorf("resolve config dir: %w", err)
		}
	}

	name := "Remoter"
	if runtime.GOOS != "windows" {
		name = "remoter"
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// Load reads config.json, writing defaults on first run. ActionsFile comes back
// resolved to an absolute path; use LoadRaw to edit and Save the file.
func Load() (Config, string, error) {
	cfg, dir, err := LoadRaw()
	if err != nil {
		return cfg, dir, err
	}
	if !filepath.IsAbs(cfg.ActionsFile) {
		cfg.ActionsFile = filepath.Join(dir, cfg.ActionsFile)
	}
	return cfg, dir, nil
}

// LoadRaw reads config.json exactly as written, for read-modify-write.
func LoadRaw() (Config, string, error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(dir, "config.json")

	cfg := defaults()
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return Config{}, "", fmt.Errorf("write default config: %w", err)
		}
	case err != nil:
		return Config{}, "", fmt.Errorf("read %s: %w", path, err)
	default:
		if err := json.Unmarshal(stripBOM(data), &cfg); err != nil {
			return Config{}, "", fmt.Errorf("parse %s: %w", path, err)
		}
	}

	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = defaults().MaxJobs
	}
	return cfg, dir, nil
}

// Token returns the bearer token, generating and persisting one on first run.
//
// The plan calls for the OS credential store where available; a 0600 file is
// the documented fallback and is what ships first. Tailscale is the real
// network boundary - this is defence in depth.
func Token(dir string) (string, error) {
	path := filepath.Join(dir, "token")

	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		return string(trimSpace(data)), nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read token: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return tok, nil
}

// stripBOM removes a leading UTF-8 byte-order mark.
//
// Windows tooling adds one routinely - PowerShell 5.1's `Set-Content -Encoding
// utf8` always does - and Go's JSON parser treats it as a syntax error. Config
// files get hand-edited on this platform, so tolerate it rather than making the
// agent fail to start over three invisible bytes.
func stripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
