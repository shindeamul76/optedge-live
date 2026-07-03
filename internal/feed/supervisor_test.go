package feed

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func ist(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	return loc
}

func TestInSession(t *testing.T) {
	loc := ist(t)
	at := func(wd, h, m, s int) time.Time {
		// 2026-06-29 is a Monday; add wd days (0=Mon … 6=Sun).
		return time.Date(2026, 6, 29+wd, h, m, s, 0, loc)
	}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"mon at open", at(0, 9, 15, 0), true},
		{"mon one sec before open", at(0, 9, 14, 59), false},
		{"tue mid session", at(1, 12, 0, 0), true},
		{"tue last sec", at(1, 15, 29, 59), true},
		{"tue at close exclusive", at(1, 15, 30, 0), false},
		{"sat midday", at(5, 12, 0, 0), false},
		{"sun midday", at(6, 12, 0, 0), false},
	}
	for _, c := range cases {
		if got := InSession(c.t, loc); got != c.want {
			t.Errorf("%s: InSession=%v want %v", c.name, got, c.want)
		}
	}
}

func TestNextOpen(t *testing.T) {
	loc := ist(t)
	open := func(y, mo, d int) time.Time { return time.Date(y, time.Month(mo), d, 9, 15, 0, 0, loc) }
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"tue before open → same day", time.Date(2026, 6, 30, 8, 0, 0, 0, loc), open(2026, 6, 30)},
		{"tue after close → wed", time.Date(2026, 6, 30, 16, 0, 0, 0, loc), open(2026, 7, 1)},
		{"fri after close → mon", time.Date(2026, 7, 3, 16, 0, 0, 0, loc), open(2026, 7, 6)},
		{"sat → mon", time.Date(2026, 7, 4, 10, 0, 0, 0, loc), open(2026, 7, 6)},
		{"sun → mon", time.Date(2026, 7, 5, 10, 0, 0, 0, loc), open(2026, 7, 6)},
	}
	for _, c := range cases {
		if got := NextOpen(c.now, loc); !got.Equal(c.want) {
			t.Errorf("%s: NextOpen=%s want %s", c.name, got, c.want)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	base, max := time.Second, 30*time.Second
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got := backoffDelay(i, base, max); got != w*time.Second {
			t.Errorf("attempt %d: backoff=%s want %ds", i, got, w)
		}
	}
}

// fakeConn is an in-memory Conn for driving the supervisor deterministically.
type fakeConn struct {
	frames   chan []byte
	closed   chan struct{}
	closeOne sync.Once
	subbed   atomic.Bool

	mu   sync.Mutex
	subs map[string]bool // tokens currently subscribed on THIS conn
}

func newFakeConn() *fakeConn {
	return &fakeConn{frames: make(chan []byte, 8), closed: make(chan struct{}), subs: map[string]bool{}}
}

func (f *fakeConn) Subscribe(_ string, _ int, lists []TokenList) error {
	f.subbed.Store(true)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range lists {
		for _, tok := range l.Tokens {
			f.subs[tok] = true
		}
	}
	return nil
}

func (f *fakeConn) Unsubscribe(_ string, _ int, lists []TokenList) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range lists {
		for _, tok := range l.Tokens {
			delete(f.subs, tok)
		}
	}
	return nil
}

func (f *fakeConn) subscribed(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[tok]
}

func (f *fakeConn) Heartbeat(time.Duration)         {}
func (f *fakeConn) SetReadDeadline(time.Time) error { return nil }
func (f *fakeConn) Close() error                    { f.closeOne.Do(func() { close(f.closed) }); return nil }

// Read blocks until a frame arrives or the connection is closed (matching a real
// socket). The supervisor's watcher closes the conn to unblock this on ctx/session.
func (f *fakeConn) Read() (int, []byte, error) {
	select {
	case b, ok := <-f.frames:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.BinaryMessage, b, nil
	case <-f.closed:
		return 0, nil, io.EOF
	}
}

// TestSupervisor_DeliversReconnectsReauths drives a full cycle with fakes: it
// delivers a tick, forces a drop, and confirms the supervisor reconnects (new
// dial), re-authenticates (login again), re-subscribes, and delivers again.
func TestSupervisor_DeliversReconnectsReauths(t *testing.T) {
	loc := ist(t)
	future := loadFrame(t, "snapquote_future.hex")

	c1, c2 := newFakeConn(), newFakeConn()
	conns := []*fakeConn{c1, c2}
	var dialN int32
	dial := func(Auth) (Conn, error) {
		i := atomic.AddInt32(&dialN, 1) - 1
		if int(i) >= len(conns) {
			return conns[len(conns)-1], nil
		}
		return conns[i], nil
	}
	var loginN int32
	login := func() (Auth, error) { atomic.AddInt32(&loginN, 1); return Auth{JWTToken: "x"}, nil }

	// Fixed in-session time (Tue 2026-06-30 10:00 IST) so the loop never idles.
	now := func() time.Time { return time.Date(2026, 6, 30, 10, 0, 0, 0, loc) }

	sup, err := NewSupervisor(Config{
		Login:       login,
		Dial:        dial,
		Sub:         Subscription{Mode: ModeSnapQuote, Lists: []TokenList{{ExchangeType: 2, Tokens: []string{"61093"}}}},
		Now:         now,
		Loc:         loc,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  2 * time.Millisecond,
		Logf:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	// First connection delivers a tick.
	c1.frames <- future
	tk := recvTick(t, sup)
	if tk.Token != "61093" {
		t.Fatalf("tick token = %q, want 61093", tk.Token)
	}

	// Force a drop → supervisor should reconnect onto c2.
	c1.Close()

	// Second connection delivers another tick after reconnect/re-auth/re-subscribe.
	c2.frames <- future
	tk2 := recvTick(t, sup)
	if tk2.Token != "61093" {
		t.Fatalf("post-reconnect tick token = %q, want 61093", tk2.Token)
	}

	if !c2.subbed.Load() {
		t.Error("reconnect did not re-subscribe")
	}
	if got := atomic.LoadInt32(&dialN); got < 2 {
		t.Errorf("dialN = %d, want >= 2 (reconnect)", got)
	}
	if got := atomic.LoadInt32(&loginN); got < 2 {
		t.Errorf("loginN = %d, want >= 2 (re-auth on reconnect)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestSupervisor_MarketClosedIdles confirms that outside the session the supervisor
// reports market-closed and never dials.
func TestSupervisor_MarketClosedIdles(t *testing.T) {
	loc := ist(t)
	var dialN int32
	sup, err := NewSupervisor(Config{
		Login: func() (Auth, error) { return Auth{}, nil },
		Dial:  func(Auth) (Conn, error) { atomic.AddInt32(&dialN, 1); return newFakeConn(), nil },
		Now:   func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, loc) }, // Saturday
		Loc:   loc,
		Logf:  func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)

	// Give the loop a moment to evaluate the window.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sup.State() == StateMarketClosed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sup.State() != StateMarketClosed {
		t.Errorf("state = %v, want market-closed", sup.State())
	}
	if got := atomic.LoadInt32(&dialN); got != 0 {
		t.Errorf("dialN = %d, want 0 (must not connect when closed)", got)
	}
	cancel()
}

// TestSupervisor_ModifyResubscribesOnReconnect proves dynamic subscriptions are sent
// live AND survive a reconnect: after adding an option token, a fresh connection
// re-subscribes the full evolving set (initial future + the added option).
func TestSupervisor_ModifyResubscribesOnReconnect(t *testing.T) {
	loc := ist(t)
	c1, c2 := newFakeConn(), newFakeConn()
	conns := []*fakeConn{c1, c2}
	var dialN int32
	dial := func(Auth) (Conn, error) {
		i := int(atomic.AddInt32(&dialN, 1) - 1)
		if i >= len(conns) {
			i = len(conns) - 1
		}
		return conns[i], nil
	}

	sup, err := NewSupervisor(Config{
		Login:       func() (Auth, error) { return Auth{}, nil },
		Dial:        dial,
		Sub:         Subscription{Mode: ModeSnapQuote, Lists: []TokenList{{ExchangeType: 2, Tokens: []string{"61093"}}}},
		Now:         func() time.Time { return time.Date(2026, 6, 30, 10, 0, 0, 0, loc) },
		Loc:         loc,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  2 * time.Millisecond,
		Logf:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	// Wait for the first connection to subscribe the initial future token.
	waitFor(t, "c1 subscribes future", func() bool { return c1.subscribed("61093") })

	// Add an option token dynamically — should be sent on the live c1.
	sup.Modify([]TokenList{{ExchangeType: 2, Tokens: []string{"OPT1"}}}, nil)
	waitFor(t, "c1 gets dynamic option", func() bool { return c1.subscribed("OPT1") })

	// Drop c1 → supervisor reconnects onto c2, which must re-subscribe BOTH tokens.
	c1.Close()
	waitFor(t, "c2 resubscribes future", func() bool { return c2.subscribed("61093") })
	waitFor(t, "c2 resubscribes option", func() bool { return c2.subscribed("OPT1") })

	cancel()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func recvTick(t *testing.T, sup *Supervisor) Tick {
	t.Helper()
	select {
	case tk := <-sup.Ticks():
		return tk
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tick")
		return Tick{}
	}
}
