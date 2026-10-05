// Package actions loads and resolves the action registry.
//
// The registry is a whitelist. There is deliberately no code path that executes
// an arbitrary caller-supplied string: an agent with such an endpoint is a
// remote shell on the tailnet wearing a JSON costume.
package actions

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Command is one concrete invocation. No shell, no interpolation: an explicit
// argv that is handed to exec as-is.
type Command struct {
	Cmd  string   `yaml:"cmd"`
	Args []string `yaml:"args"`
}

// Action is a registry entry. Run holds per-OS variants keyed by GOOS, plus an
// optional "default" used when the running OS has no specific entry.
type Action struct {
	ID      string             `yaml:"id"`
	Label   string             `yaml:"label"`
	Icon    string             `yaml:"icon"`
	Confirm bool               `yaml:"confirm"`
	Cwd     string             `yaml:"cwd"`
	Timeout time.Duration      `yaml:"timeout"`
	Notify  string             `yaml:"notify"`
	Run     map[string]Command `yaml:"run"`
}

// Resolved is an Action reduced to this host, ready to hand to the job runner.
type Resolved struct {
	Action
	Command   Command
	Cwd       string
	Available bool
	Reason    string
}

// Registry is the loaded, host-resolved action set.
type Registry struct {
	order []string
	byID  map[string]Resolved
}

// Load reads the YAML registry and resolves every action for the current host.
//
// Unresolvable actions are kept and marked unavailable rather than dropped, so
// the UI can say *why* a button is missing instead of silently omitting it.
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read actions: %w", err)
	}

	var list []Action
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse actions: %w", err)
	}

	reg := &Registry{byID: make(map[string]Resolved, len(list))}
	for i, a := range list {
		if a.ID == "" {
			return nil, fmt.Errorf("action %d: missing id", i)
		}
		if _, dup := reg.byID[a.ID]; dup {
			return nil, fmt.Errorf("duplicate action id %q", a.ID)
		}
		if a.Label == "" {
			a.Label = a.ID
		}
		reg.order = append(reg.order, a.ID)
		reg.byID[a.ID] = resolve(a)
	}
	return reg, nil
}

func resolve(a Action) Resolved {
	r := Resolved{Action: a}

	cmd, ok := a.Run[runtime.GOOS]
	if !ok {
		cmd, ok = a.Run["default"]
	}
	switch {
	case !ok:
		r.Reason = fmt.Sprintf("no command defined for %s and no default", runtime.GOOS)
		return r
	case cmd.Cmd == "":
		r.Reason = "command is empty"
		return r
	}

	cwd, err := expandHome(a.Cwd)
	if err != nil {
		r.Reason = err.Error()
		return r
	}

	r.Command = cmd
	r.Cwd = cwd
	r.Available = true
	return r
}

// expandHome resolves a leading ~ portably. Anything else is returned as-is so
// that relative paths keep working against the agent's working directory.
func expandHome(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, filepath.FromSlash(p[2:])), nil
}

// All returns actions in registry order.
func (r *Registry) All() []Resolved {
	out := make([]Resolved, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Get looks up a single action.
func (r *Registry) Get(id string) (Resolved, bool) {
	a, ok := r.byID[id]
	return a, ok
}

// Empty is a registry with no actions - a node without an actions.yaml still
// serves every other tier rather than refusing to start.
func Empty() *Registry { return &Registry{byID: map[string]Resolved{}} }
