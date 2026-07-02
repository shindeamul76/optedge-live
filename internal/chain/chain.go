// Package chain manages the live option-chain window: it keeps ATM±N × CE/PE of the
// current weekly expiry subscribed so whatever strike the strategy picks is already
// streaming, maintains a token→quote map updated from option ticks, and computes the
// subscribe/unsubscribe diffs as spot drifts (with hysteresis) and the weekly rolls.
// See lld-explained/C.md.
//
// This file is the PURE logic (no socket, no goroutines): Reconcile returns the diff
// to apply, Update folds an option tick, Quote answers the RealPricer lookup. The
// live wiring (sending sub/unsub on the socket, re-subscribing on reconnect) lives
// in the supervisor + the run loop.
package chain

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/feed"
	"github.com/shindeamul76/optedge/live"
)

// hysteresisFrac is the sticky band for recentering: the window only re-centers once
// spot moves more than this fraction of a strike from the current center. 0.75 > 0.5
// (the rounding boundary), so spot oscillating on a strike edge won't thrash sub/unsub.
const hysteresisFrac = 0.75

// Contract identifies one option in the window.
type Contract struct {
	Token  string
	Typ    live.OptionType
	Strike float64
	Expiry time.Time
	Symbol string
}

// Quote is the latest market snapshot for one option contract. Prices in rupees.
type Quote struct {
	Token string
	Bid   float64
	Ask   float64
	LTP   float64
	OI    int64
	TS    time.Time // exchange timestamp of the tick that set this quote
}

// Mid returns (bid+ask)/2, falling back to LTP when a book side is missing. This is
// what RealPricer will return (phase 4).
func (q Quote) Mid() float64 {
	if q.Bid > 0 && q.Ask > 0 {
		return (q.Bid + q.Ask) / 2
	}
	return q.LTP
}

// Age reports how stale the quote is relative to now.
func (q Quote) Age(now time.Time) time.Duration { return now.Sub(q.TS) }

// Config wires a Chain.
type Config struct {
	Instruments []broker.Instrument
	Underlying  string // e.g. "NIFTY"
	StrikeStep  float64
	N           int // strikes each side of ATM
	Loc         *time.Location
	Logf        func(string, ...any)
}

// Chain is safe for concurrent Update (feed goroutine) and Quote (engine goroutine).
type Chain struct {
	insts      []broker.Instrument
	underlying string
	step       float64
	n          int
	loc        *time.Location
	logf       func(string, ...any)

	mu         sync.RWMutex
	expiry     time.Time
	center     float64
	haveCenter bool
	byToken    map[string]Contract // current window: token → contract
	byKey      map[string]string   // (typ,strike) → token
	quotes     map[string]Quote    // token → latest quote
}

// New builds an empty chain (no window yet — call Reconcile to populate it).
func New(cfg Config) (*Chain, error) {
	if cfg.Underlying == "" || cfg.StrikeStep <= 0 || cfg.N < 0 {
		return nil, fmt.Errorf("chain: underlying, positive StrikeStep, and N>=0 required")
	}
	if cfg.Loc == nil {
		loc, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			return nil, err
		}
		cfg.Loc = loc
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Chain{
		insts:      cfg.Instruments,
		underlying: cfg.Underlying,
		step:       cfg.StrikeStep,
		n:          cfg.N,
		loc:        cfg.Loc,
		logf:       cfg.Logf,
		byToken:    map[string]Contract{},
		byKey:      map[string]string{},
		quotes:     map[string]Quote{},
	}, nil
}

// Reconcile recomputes the desired window for the given spot and time, returning the
// contracts to subscribe and unsubscribe. It handles BOTH triggers from C.md:
//   - spot drift → recenter (only past the hysteresis band, to avoid thrash);
//   - weekly expiry change → full roll to the new expiry's contracts.
//
// asOf resolves the current weekly via the same scrip master the engine's calendar
// uses, keeping live and backtest on the same expiry. Returns empty slices when
// nothing changed.
func (c *Chain) Reconcile(spot float64, asOf time.Time) (sub, unsub []Contract, err error) {
	exp, err := broker.NearestOptionExpiry(c.insts, c.underlying, asOf)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve weekly expiry: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	expiryChanged := !exp.Equal(c.expiry)
	newATM := roundStep(spot, c.step)
	need := !c.haveCenter || expiryChanged || math.Abs(spot-c.center) > c.step*hysteresisFrac
	if !need {
		return nil, nil, nil
	}

	// Build the desired token set for (newATM ± N) × CE/PE at the resolved expiry.
	desired := make(map[string]Contract)
	for _, k := range broker.ATMStrikes(newATM, c.step, c.n) {
		for _, ot := range []live.OptionType{live.Call, live.Put} {
			inst, ferr := broker.FindOption(c.insts, c.underlying, exp, k, ot)
			if ferr != nil {
				c.logf("chain: no contract for %s %.0f exp %s: %v", ot, k, exp.Format("2006-01-02"), ferr)
				continue
			}
			desired[inst.Token] = Contract{Token: inst.Token, Typ: ot, Strike: k, Expiry: exp, Symbol: inst.Symbol}
		}
	}

	// Diff against the current window.
	for tok, ct := range desired {
		if _, ok := c.byToken[tok]; !ok {
			sub = append(sub, ct)
		}
	}
	for tok, ct := range c.byToken {
		if _, ok := desired[tok]; !ok {
			unsub = append(unsub, ct)
			delete(c.quotes, tok) // stop tracking a contract we no longer hold
		}
	}

	// Commit the new window.
	c.byToken = desired
	c.byKey = make(map[string]string, len(desired))
	for tok, ct := range desired {
		c.byKey[key(ct.Typ, ct.Strike)] = tok
	}
	c.center = newATM
	c.haveCenter = true
	c.expiry = exp
	return sub, unsub, nil
}

// Update folds an option tick into the quote map. Returns false if the token is not
// in the current window (e.g. the future, VIX, or a just-unsubscribed contract).
func (c *Chain) Update(t feed.Tick) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byToken[t.Token]; !ok {
		return false
	}
	c.quotes[t.Token] = Quote{
		Token: t.Token, Bid: t.BestBid(), Ask: t.BestAsk(), LTP: t.LTP, OI: t.OI, TS: t.ExchTime,
	}
	return true
}

// Quote returns the latest quote for the contract at (typ, strike) in the current
// weekly window. ok is false if the strike isn't in the window or has no quote yet.
func (c *Chain) Quote(typ live.OptionType, strike float64) (Quote, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tok, ok := c.byKey[key(typ, strike)]
	if !ok {
		return Quote{}, false
	}
	q, ok := c.quotes[tok]
	return q, ok
}

// Desired returns the current window's contracts — used to re-subscribe the full set
// after a reconnect.
func (c *Chain) Desired() []Contract {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Contract, 0, len(c.byToken))
	for _, ct := range c.byToken {
		out = append(out, ct)
	}
	return out
}

// Expiry returns the current weekly expiry (zero until the first Reconcile).
func (c *Chain) Expiry() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expiry
}

// Center returns the current ATM center strike.
func (c *Chain) Center() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.center
}

func roundStep(spot, step float64) float64 { return math.Round(spot/step) * step }

func key(typ live.OptionType, strike float64) string { return fmt.Sprintf("%s|%.2f", typ, strike) }
