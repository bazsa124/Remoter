package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Access states. A device the hub has never seen is "pending" on first contact;
// only the owner moves it to "approved".
const (
	Pending  = "pending"
	Approved = "approved"
)

// Kinds of device the hub knows. One tailnet device can be both: a laptop is a
// controller through its browser and a target through its node.
const (
	KindController = "controller"
	KindNode       = "node"
)

// Controller is a device allowed (or asking) to drive targets.
type Controller struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OS         string     `json:"os"`
	State      string     `json:"state"`
	FirstSeen  time.Time  `json:"firstSeen"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
}

// Node is a target running remoter-node.
type Node struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OS         string     `json:"os"`
	Addr       string     `json:"addr"`
	Port       int        `json:"port"`
	Token      string     `json:"token"`
	Version    string     `json:"version"`
	State      string     `json:"state"`
	EnrolledAt time.Time  `json:"enrolledAt"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
}

type persisted struct {
	Controllers map[string]*Controller `json:"controllers"`
	Nodes       map[string]*Node       `json:"nodes"`
	PINHash     string                 `json:"pinHash,omitempty"`
}

// Store is the hub's durable state, one JSON file written atomically.
//
// It is small and changes rarely (approvals, enrolments, PIN changes), so a
// file beats a database: it can be read, backed up and repaired by hand.
// Anything that changes per request - last-seen times, presence - lives in
// memory and is deliberately not persisted.
type Store struct {
	path string

	mu sync.Mutex
	st persisted
}

// ErrNotFound means no device matched.
var ErrNotFound = errors.New("no such device")

// OpenStore loads state from dir, creating an empty store on first run.
func OpenStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "state.json")}
	s.st = persisted{Controllers: map[string]*Controller{}, Nodes: map[string]*Node{}}

	data, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		return s, s.saveLocked()
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if s.st.Controllers == nil {
		s.st.Controllers = map[string]*Controller{}
	}
	if s.st.Nodes == nil {
		s.st.Nodes = map[string]*Node{}
	}
	return s, nil
}

// saveLocked writes via a temp file and rename, so a crash mid-write leaves the
// previous state intact rather than a truncated file that locks everyone out.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

// SeeController returns the controller for a peer, registering it as pending on
// first contact. isNew reports that first contact, so the caller can notify.
func (s *Store) SeeController(p *Peer) (c Controller, isNew bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if got, ok := s.st.Controllers[p.ID]; ok {
		// Follow renames in Tailscale; the stable ID is the identity.
		if got.Name != p.Name || got.OS != p.OS {
			got.Name, got.OS = p.Name, p.OS
			err = s.saveLocked()
		}
		return *got, false, err
	}
	c = Controller{ID: p.ID, Name: p.Name, OS: p.OS, State: Pending, FirstSeen: time.Now().UTC()}
	s.st.Controllers[p.ID] = &c
	cp := c
	return cp, true, s.saveLocked()
}

// Enroll registers or refreshes a node at addr. A re-enrolment (reinstall)
// keeps the approval but takes the new token: the node generated it, so it is
// current.
func (s *Store) Enroll(p *Peer, addr, token string, port int, version string) (n Node, isNew bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if got, ok := s.st.Nodes[p.ID]; ok {
		got.Name, got.OS, got.Addr = p.Name, p.OS, addr
		got.Token, got.Port, got.Version = token, port, version
		return *got, false, s.saveLocked()
	}
	n = Node{
		ID: p.ID, Name: p.Name, OS: p.OS, Addr: addr, Port: port,
		Token: token, Version: version, State: Pending, EnrolledAt: time.Now().UTC(),
	}
	s.st.Nodes[p.ID] = &n
	cp := n
	return cp, true, s.saveLocked()
}

// UpdateNodeAddr records a node's current tailnet address. Tailnet IPs are
// stable in practice, but a re-keyed device can change, and a stale address
// makes a working node look dead.
func (s *Store) UpdateNodeAddr(id, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.st.Nodes[id]; ok && addr != "" && n.Addr != addr {
		n.Addr = addr
		_ = s.saveLocked()
	}
}

// Controller returns a copy of one controller.
func (s *Store) Controller(id string) (Controller, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.st.Controllers[id]
	if !ok {
		return Controller{}, false
	}
	return *c, true
}

// Node returns a copy of one node.
func (s *Store) Node(id string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.st.Nodes[id]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Controllers lists all controllers, approved first, then by name.
func (s *Store) Controllers() []Controller {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Controller, 0, len(s.st.Controllers))
	for _, c := range s.st.Controllers {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == Approved
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Nodes lists all nodes, approved first, then by name.
func (s *Store) Nodes() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Node, 0, len(s.st.Nodes))
	for _, n := range s.st.Nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == Approved
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Approve moves a device to approved.
func (s *Store) Approve(kind, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	switch kind {
	case KindController:
		c, ok := s.st.Controllers[id]
		if !ok {
			return ErrNotFound
		}
		c.State, c.ApprovedAt = Approved, &now
	case KindNode:
		n, ok := s.st.Nodes[id]
		if !ok {
			return ErrNotFound
		}
		n.State, n.ApprovedAt = Approved, &now
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return s.saveLocked()
}

// Remove forgets a device entirely: rejecting a pending one and revoking an
// approved one are the same operation. A removed controller that calls again
// reappears as pending, which is the correct outcome.
func (s *Store) Remove(kind, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case KindController:
		if _, ok := s.st.Controllers[id]; !ok {
			return ErrNotFound
		}
		delete(s.st.Controllers, id)
	case KindNode:
		if _, ok := s.st.Nodes[id]; !ok {
			return ErrNotFound
		}
		delete(s.st.Nodes, id)
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return s.saveLocked()
}

// Find resolves a name or ID, as typed on the admin CLI, to a device.
func (s *Store) Find(kind, nameOrID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []string
	match := func(id, name string) {
		if id == nameOrID || strings.EqualFold(name, nameOrID) {
			ids = append(ids, id)
		}
	}
	switch kind {
	case KindController:
		for id, c := range s.st.Controllers {
			match(id, c.Name)
		}
	case KindNode:
		for id, n := range s.st.Nodes {
			match(id, n.Name)
		}
	}
	switch len(ids) {
	case 0:
		return "", ErrNotFound
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%q is ambiguous; use the device id", nameOrID)
	}
}

// PINHash returns the stored PIN hash, or "" when none is set.
func (s *Store) PINHash() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.PINHash
}

// SetPINHash replaces the PIN hash; "" clears it.
func (s *Store) SetPINHash(h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.PINHash = h
	return s.saveLocked()
}
