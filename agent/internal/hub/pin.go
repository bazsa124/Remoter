package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// The PIN is the second lock on the tiers that show the screen or hand out a
// shell. Tailscale identity says which device is calling; the PIN says the
// person holding it is the owner - which is what matters for a laptop left
// unlocked or a phone handed to someone.

const (
	pinGrace    = 5 * time.Minute // reopening within this window does not ask again
	pinMaxFails = 5
	pinLockout  = 5 * time.Minute
	pinMinLen   = 4
	pinMaxLen   = 12
)

// Argon2id parameters: interactive-login strength. A PIN has little entropy, so
// the hash is not the real defence - the attempt limit is - but a stolen
// state.json should still cost real work per guess.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// ErrBadPIN rejects a PIN that is not 4-12 digits.
var ErrBadPIN = fmt.Errorf("PIN must be %d-%d digits", pinMinLen, pinMaxLen)

func validPIN(pin string) bool {
	if len(pin) < pinMinLen || len(pin) > pinMaxLen {
		return false
	}
	return strings.IndexFunc(pin, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func hashPIN(pin string) (string, error) {
	if !validPIN(pin) {
		return "", ErrBadPIN
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pin), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPIN(pin, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var version int
	var mem, iters uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &mem, &iters, &threads); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[3])
	want, err2 := enc.DecodeString(parts[4])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pin), salt, iters, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pinGate tracks, per controller, whether the PIN has been entered recently.
//
// A grant stays valid while any guarded session from that controller is open,
// and for pinGrace after the last one closes. It lives in memory only: a hub
// restart asking for the PIN again is the safe direction to fail.
type pinGate struct {
	mu     sync.Mutex
	grants map[string]*grant
	fails  map[string]*failures
	now    func() time.Time
}

type grant struct {
	until time.Time
	open  int
}

type failures struct {
	count       int
	lockedUntil time.Time
}

func newPINGate() *pinGate {
	return &pinGate{grants: map[string]*grant{}, fails: map[string]*failures{}, now: time.Now}
}

func (g *pinGate) valid(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.grants[id]
	return ok && (gr.open > 0 || g.now().Before(gr.until))
}

// remaining reports how long the grant lasts if nothing else happens; 0 means
// no grant, and a negative value means "held open by a live session".
func (g *pinGate) remaining(id string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.grants[id]
	if !ok {
		return 0
	}
	if gr.open > 0 {
		return -1
	}
	return max(0, gr.until.Sub(g.now()))
}

func (g *pinGate) entry(id string) *grant {
	gr, ok := g.grants[id]
	if !ok {
		gr = &grant{}
		g.grants[id] = gr
	}
	return gr
}

// errLocked means too many wrong PINs; retry after the duration.
type errLocked struct{ wait time.Duration }

func (e errLocked) Error() string {
	return fmt.Sprintf("too many wrong PINs; try again in %d s", int(e.wait.Seconds())+1)
}

// errWrongPIN carries how many attempts are left before a lockout.
type errWrongPIN struct{ left int }

func (e errWrongPIN) Error() string { return fmt.Sprintf("wrong PIN (%d attempts left)", e.left) }

// errNoPIN means the owner has not set a PIN yet.
var errNoPIN = errors.New("no PIN is set yet")

// verify checks a PIN attempt for a controller and grants on success.
func (g *pinGate) verify(id, pin, hash string) error {
	if hash == "" {
		return errNoPIN
	}

	g.mu.Lock()
	f := g.fails[id]
	if f != nil && g.now().Before(f.lockedUntil) {
		wait := f.lockedUntil.Sub(g.now())
		g.mu.Unlock()
		return errLocked{wait}
	}
	g.mu.Unlock()

	// Hash outside the lock: argon2 takes tens of milliseconds by design.
	ok := checkPIN(pin, hash)

	g.mu.Lock()
	defer g.mu.Unlock()
	if !ok {
		if f == nil {
			f = &failures{}
			g.fails[id] = f
		}
		f.count++
		if f.count >= pinMaxFails {
			f.count = 0
			f.lockedUntil = g.now().Add(pinLockout)
			return errLocked{pinLockout}
		}
		return errWrongPIN{pinMaxFails - f.count}
	}
	delete(g.fails, id)
	gr := g.entry(id)
	gr.until = g.now().Add(pinGrace)
	return nil
}

// touch extends a grant for a one-shot guarded request (a Glance frame).
func (g *pinGate) touch(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.entry(id)
	if until := g.now().Add(pinGrace); until.After(gr.until) {
		gr.until = until
	}
}

// open and close bracket a guarded long-lived session (Live, Console).
func (g *pinGate) open(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entry(id).open++
}

func (g *pinGate) close(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.entry(id)
	if gr.open > 0 {
		gr.open--
	}
	if gr.open == 0 {
		gr.until = g.now().Add(pinGrace)
	}
}

// revoke ends a controller's grant now - "lock" in the UI, or device removal.
func (g *pinGate) revoke(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gr, ok := g.grants[id]; ok {
		gr.until = time.Time{}
		if gr.open == 0 {
			delete(g.grants, id)
		}
	}
}
