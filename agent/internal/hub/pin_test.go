package hub

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPINHashRoundTrip(t *testing.T) {
	h, err := hashPIN("482913")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPIN("482913", h) {
		t.Fatal("correct PIN rejected")
	}
	if checkPIN("482914", h) {
		t.Fatal("wrong PIN accepted")
	}
	for _, bad := range []string{"123", "12ab", "1234567890123", ""} {
		if _, err := hashPIN(bad); err == nil {
			t.Errorf("hashPIN(%q) accepted an invalid PIN", bad)
		}
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// The grant must survive a long session and then last exactly the grace period
// after the last session closes - the "reopen within 5 minutes" promise.
func TestPINGraceFollowsSessions(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	g := newPINGate()
	g.now = clk.now
	hash, _ := hashPIN("1234")

	if g.valid("phone") {
		t.Fatal("valid before any PIN")
	}
	if err := g.verify("phone", "1234", hash); err != nil {
		t.Fatal(err)
	}
	g.open("phone")
	clk.advance(time.Hour) // a long Live session
	if !g.valid("phone") {
		t.Fatal("grant expired while a session was open")
	}
	g.close("phone")
	clk.advance(pinGrace - time.Second)
	if !g.valid("phone") {
		t.Fatal("grant expired inside the grace period")
	}
	clk.advance(2 * time.Second)
	if g.valid("phone") {
		t.Fatal("grant outlived the grace period")
	}
	if g.valid("laptop") {
		t.Fatal("grant leaked to another controller")
	}
}

func TestPINLockout(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	g := newPINGate()
	g.now = clk.now
	hash, _ := hashPIN("1234")

	for i := 1; i < pinMaxFails; i++ {
		var wrong errWrongPIN
		if err := g.verify("x", "0000", hash); !errors.As(err, &wrong) {
			t.Fatalf("attempt %d: got %v", i, err)
		}
	}
	var locked errLocked
	if err := g.verify("x", "0000", hash); !errors.As(err, &locked) {
		t.Fatalf("expected lockout, got %v", err)
	}
	// Even the right PIN is refused while locked: otherwise the lockout is
	// only a delay between guesses.
	if err := g.verify("x", "1234", hash); !errors.As(err, &locked) {
		t.Fatalf("correct PIN accepted during lockout: %v", err)
	}
	clk.advance(pinLockout + time.Second)
	if err := g.verify("x", "1234", hash); err != nil {
		t.Fatalf("correct PIN refused after lockout: %v", err)
	}
}

func TestSessionReconnectIsQuiet(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	s := newSessionTracker()
	s.now = clk.now
	k := sessionKey{"phone", "laptop", "live"}

	id, started := s.begin(k, "phone", func() {})
	if !started {
		t.Fatal("first connection did not start a session")
	}
	if ended, _ := s.end(k, id); !ended {
		t.Fatal("session did not end")
	}
	clk.advance(30 * time.Second) // LTE blip
	if _, started := s.begin(k, "phone", func() {}); started {
		t.Fatal("a quick reconnect counted as a new session")
	}
}

func TestRevokeCancelsSessions(t *testing.T) {
	s := newSessionTracker()
	ctx, cancel := context.WithCancel(context.Background())
	s.begin(sessionKey{"phone", "laptop", "console"}, "phone", cancel)
	s.begin(sessionKey{"tablet", "laptop", "live"}, "tablet", func() {})

	if n := s.cancelWhere(func(k sessionKey) bool { return k.ctrl == "phone" }); n != 1 {
		t.Fatalf("cancelled %d sessions, want 1", n)
	}
	if ctx.Err() == nil {
		t.Fatal("revoked controller's session is still open")
	}
}
