package feed

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Supervisor keeps the live feed up across the trading session: it connects at the
// open, re-authenticates and re-subscribes on every (re)connect, reconnects with
// exponential backoff after drops, idles outside market hours, and drops any tick
// that arrives outside the session window. Decoded ticks are delivered on Ticks().
// See lld-explained/A.md (§A reconnect/heartbeat/market-hours).
//
// The transport (Conn) and the clock (Now) are injected so the whole state machine
// is unit-testable without a real socket — see supervisor_test.go.

// ConnState is the connection lifecycle, surfaced for the dashboard.
type ConnState int

const (
	StateDisconnected ConnState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateMarketClosed
)

func (s ConnState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateMarketClosed:
		return "market-closed"
	default:
		return "disconnected"
	}
}

// Session window (IST). Open inclusive at 09:15:00; close exclusive at 15:30:00.
const (
	sessionOpenSec  = 9*3600 + 15*60  // 33300
	sessionCloseSec = 15*3600 + 30*60 // 55800
)

// deadConnTimeout is the read deadline used purely as a DEAD-SOCKET detector: it is
// extended on every received frame (market data or the ~25s heartbeat pong), so a
// healthy connection never hits it. If it actually fires, the socket has been silent
// too long and is treated as a genuine drop → reconnect. It must NOT be used as a
// short periodic "wake": gorilla/websocket leaves the connection permanently unusable
// after a read-deadline timeout (it stores the error in c.readErr forever).
const deadConnTimeout = 45 * time.Second

// sessionCheckPeriod is how often the readLoop watcher re-checks ctx-cancel and the
// session window; on either it unblocks a blocked Read by closing the connection.
const sessionCheckPeriod = 1 * time.Second

const correlationID = "optedge-live"

// Subscription is the set of tokens to (re)subscribe on every connect.
type Subscription struct {
	Mode  int
	Lists []TokenList
}

// Config wires the supervisor. Login and Dial are required; the rest default.
type Config struct {
	Login func() (Auth, error)   // called on EVERY connect → fresh jwt+feedToken (re-auth)
	Dial  func(Auth) (Conn, error)
	Sub   Subscription

	Now            func() time.Time // default time.Now
	Loc            *time.Location   // default Asia/Kolkata
	HeartbeatEvery time.Duration    // default 25s
	BaseBackoff    time.Duration    // default 1s
	MaxBackoff     time.Duration    // default 30s
	Logf           func(string, ...any)
}

// Supervisor is created with NewSupervisor and driven by Run.
type Supervisor struct {
	cfg   Config
	ticks chan Tick

	mu    sync.RWMutex
	state ConnState

	// connMu guards the evolving desired subscription set and the current live conn.
	// The desired set (not cfg.Sub) is what gets (re)subscribed on every connect, so
	// dynamic chain changes survive reconnects.
	connMu  sync.Mutex
	mode    int
	desired map[int]map[string]struct{} // exchangeType → token set
	conn    Conn                         // current live connection, nil when down
}

// NewSupervisor validates config and applies defaults.
func NewSupervisor(cfg Config) (*Supervisor, error) {
	if cfg.Login == nil || cfg.Dial == nil {
		return nil, fmt.Errorf("supervisor: Login and Dial are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Loc == nil {
		loc, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			return nil, fmt.Errorf("supervisor: load IST: %w", err)
		}
		cfg.Loc = loc
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 25 * time.Second
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	mode := cfg.Sub.Mode
	if mode == 0 {
		mode = ModeSnapQuote
	}
	s := &Supervisor{
		cfg:     cfg,
		ticks:   make(chan Tick, 1024),
		mode:    mode,
		desired: make(map[int]map[string]struct{}),
	}
	// Seed the desired set from the initial subscription.
	for _, l := range cfg.Sub.Lists {
		for _, tok := range l.Tokens {
			s.addToken(l.ExchangeType, tok)
		}
	}
	return s, nil
}

// addToken records a token in the desired set (caller holds connMu, or during init).
func (s *Supervisor) addToken(exType int, token string) {
	if s.desired[exType] == nil {
		s.desired[exType] = make(map[string]struct{})
	}
	s.desired[exType][token] = struct{}{}
}

// Modify updates the desired subscription set and, if currently connected, sends the
// incremental subscribe/unsubscribe on the live socket. If disconnected, the change
// is remembered and applied on the next connect. This is how the chain (§C) shifts
// its ATM window and rolls the weekly without losing subscriptions across reconnects.
func (s *Supervisor) Modify(add, remove []TokenList) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for _, l := range add {
		for _, tok := range l.Tokens {
			s.addToken(l.ExchangeType, tok)
		}
	}
	for _, l := range remove {
		for _, tok := range l.Tokens {
			if s.desired[l.ExchangeType] != nil {
				delete(s.desired[l.ExchangeType], tok)
			}
		}
	}
	if s.conn == nil {
		return // will be applied from the desired set on next connect
	}
	if hasTokens(add) {
		if err := s.conn.Subscribe(correlationID, s.mode, add); err != nil {
			s.cfg.Logf("feed: dynamic subscribe: %v", err)
		}
	}
	if hasTokens(remove) {
		if err := s.conn.Unsubscribe(correlationID, s.mode, remove); err != nil {
			s.cfg.Logf("feed: dynamic unsubscribe: %v", err)
		}
	}
}

// desiredLists snapshots the desired set as TokenList groups (caller holds connMu).
func (s *Supervisor) desiredLists() []TokenList {
	var lists []TokenList
	for ex, toks := range s.desired {
		if len(toks) == 0 {
			continue
		}
		l := TokenList{ExchangeType: ex}
		for tok := range toks {
			l.Tokens = append(l.Tokens, tok)
		}
		lists = append(lists, l)
	}
	return lists
}

func hasTokens(lists []TokenList) bool {
	for _, l := range lists {
		if len(l.Tokens) > 0 {
			return true
		}
	}
	return false
}

// Ticks is the stream of decoded in-session ticks.
func (s *Supervisor) Ticks() <-chan Tick { return s.ticks }

// State returns the current connection state (for the dashboard).
func (s *Supervisor) State() ConnState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Supervisor) setState(st ConnState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *Supervisor) now() time.Time { return s.cfg.Now() }

// Run drives the lifecycle until ctx is cancelled. It owns connect/reconnect; the
// caller just consumes Ticks() and reads State().
func (s *Supervisor) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			s.setState(StateDisconnected)
			return ctx.Err()
		}

		now := s.now()
		if !InSession(now, s.cfg.Loc) {
			s.setState(StateMarketClosed)
			attempt = 0
			// Re-check at least once a minute so we notice the open promptly and
			// stay responsive to cancellation.
			wait := capDur(NextOpen(now, s.cfg.Loc).Sub(now), time.Minute)
			if !s.sleep(ctx, wait) {
				s.setState(StateDisconnected)
				return ctx.Err()
			}
			continue
		}

		if attempt == 0 {
			s.setState(StateConnecting)
		} else {
			s.setState(StateReconnecting)
		}

		conn, err := s.connect()
		if err != nil {
			s.cfg.Logf("feed: connect failed (attempt %d): %v", attempt+1, err)
			if !s.sleep(ctx, backoffDelay(attempt, s.cfg.BaseBackoff, s.cfg.MaxBackoff)) {
				s.setState(StateDisconnected)
				return ctx.Err()
			}
			attempt++
			continue
		}
		s.setState(StateConnected)
		attempt = 0

		err = s.readLoop(ctx, conn)
		conn.Close()
		s.clearConn()

		if ctx.Err() != nil {
			s.setState(StateDisconnected)
			return ctx.Err()
		}
		if err != nil {
			// A real read error → reconnect with backoff. A nil return means we left
			// cleanly because the session ended; the loop top handles market-closed.
			s.cfg.Logf("feed: connection lost: %v; reconnecting", err)
			s.setState(StateReconnecting)
			if !s.sleep(ctx, backoffDelay(attempt, s.cfg.BaseBackoff, s.cfg.MaxBackoff)) {
				s.setState(StateDisconnected)
				return ctx.Err()
			}
			attempt++
		}
	}
}

// connect re-authenticates (fresh tokens), dials, starts the heartbeat, and
// subscribes the CURRENT desired set (not the initial cfg.Sub) so dynamic chain
// changes survive reconnects. Re-logging in on every connect is the simplest correct
// handling of jwt expiry — a single REST call per reconnect. Snapshotting the desired
// set, subscribing, and publishing s.conn happen under connMu so a concurrent Modify
// either lands in the snapshot or is sent incrementally afterwards — never lost.
func (s *Supervisor) connect() (Conn, error) {
	auth, err := s.cfg.Login()
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	conn, err := s.cfg.Dial(auth)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	conn.Heartbeat(s.cfg.HeartbeatEvery)

	s.connMu.Lock()
	defer s.connMu.Unlock()
	lists := s.desiredLists()
	if hasTokens(lists) {
		if err := conn.Subscribe(correlationID, s.mode, lists); err != nil {
			conn.Close()
			return nil, fmt.Errorf("subscribe: %w", err)
		}
	}
	s.conn = conn
	return conn, nil
}

// clearConn drops the current live conn reference (called after a disconnect so
// Modify stops trying to write to a dead socket).
func (s *Supervisor) clearConn() {
	s.connMu.Lock()
	s.conn = nil
	s.connMu.Unlock()
}

// readLoop reads until a real error (→ reconnect, non-nil return) or until the
// session ends / ctx cancels (→ clean nil return).
//
// A blocked Read can't observe ctx-cancel or session-end on its own, and a gorilla
// read-deadline timeout can't be used as a wake (it kills the connection). So a
// watcher goroutine unblocks the Read by CLOSING the connection on ctx-cancel or
// session-end; the resulting read error is then recognized as a clean shutdown (not a
// drop). The read deadline is only a dead-socket detector, extended on every frame.
func (s *Supervisor) readLoop(ctx context.Context, conn Conn) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(sessionCheckPeriod)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				conn.Close() // unblock a blocked Read
				return
			case <-t.C:
				if !InSession(s.now(), s.cfg.Loc) {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		// Extend the dead-socket deadline on every frame; an actual expiry means the
		// socket has been silent past the heartbeat interval → dead → reconnect.
		_ = conn.SetReadDeadline(s.now().Add(deadConnTimeout))
		mt, data, err := conn.Read()
		if err != nil {
			// If we're shutting down or the session ended, the watcher closed the
			// connection: that's a clean exit, not a drop.
			if ctx.Err() != nil || !InSession(s.now(), s.cfg.Loc) {
				return nil
			}
			return err // genuine drop → reconnect
		}
		if mt != websocket.BinaryMessage {
			continue // heartbeat "pong" / control text — resets the deadline next iteration
		}

		tk, perr := ParseSnapQuote(data)
		if perr != nil {
			s.cfg.Logf("feed: parse: %v", perr)
			continue
		}
		select {
		case s.ticks <- tk:
		case <-ctx.Done():
			return nil
		}
	}
}

// sleep waits d or until ctx is cancelled. Returns false if cancelled.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// InSession reports whether t is within the trading window (Mon–Fri,
// 09:15:00 ≤ t < 15:30:00 IST). It does NOT know NSE holidays — on a holiday the
// socket simply receives no ticks (harmless idle), which is acceptable for v3.
func InSession(t time.Time, loc *time.Location) bool {
	lt := t.In(loc)
	switch lt.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	sec := lt.Hour()*3600 + lt.Minute()*60 + lt.Second()
	return sec >= sessionOpenSec && sec < sessionCloseSec
}

// NextOpen returns the next session-open instant (09:15:00 IST on the next trading
// weekday) strictly relevant to t: today's open if t is before it on a weekday,
// otherwise the following weekday's open.
func NextOpen(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	open := time.Date(lt.Year(), lt.Month(), lt.Day(), 9, 15, 0, 0, loc)
	if !lt.Before(open) || isWeekend(lt.Weekday()) {
		// Advance to the next day until we land on a weekday's open after t.
		for {
			open = open.AddDate(0, 0, 1)
			if !isWeekend(open.Weekday()) && open.After(lt) {
				break
			}
		}
	}
	return open
}

func isWeekend(d time.Weekday) bool { return d == time.Saturday || d == time.Sunday }

// backoffDelay is the exponential schedule base<<attempt, capped at max:
// 1,2,4,8,16,30,30,… for base=1s,max=30s. attempt starts at 0.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base << attempt
	if d <= 0 || d > max { // <=0 guards the shift overflowing
		return max
	}
	return d
}

func capDur(d, max time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > max {
		return max
	}
	return d
}
