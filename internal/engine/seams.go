package engine

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge/live"
)

// RealPricer implements live.OptionPricer by reading the live option chain. It
// ignores spot (the real book already reflects it), returns the contract's mid, and
// caches that contract's bid/ask for the paired RealFill — the engine always calls
// Price then Fill on the same contract within one OnBar, so the cache is fresh (D.md).
type RealPricer struct {
	ch         *chain.Chain
	now        func() time.Time
	staleAfter time.Duration
	logf       func(string, ...any)

	// cache of the just-quoted contract, read by the paired RealFill.
	haveCache          bool
	cacheBid, cacheAsk float64
	lastStale          bool
}

// RealFill implements live.FillModel: buy pays the real ask, sell receives the real
// bid (penetration-not-touch). It reads the bid/ask the paired RealPricer just cached.
type RealFill struct {
	p *RealPricer
}

// NewSeamPair builds a paired RealPricer + RealFill sharing one chain. Each Runner
// gets its OWN pair (the cache lives in the pair) so runners can't clobber each
// other's cached contract — see D.md.
func NewSeamPair(ch *chain.Chain, now func() time.Time, staleAfter time.Duration, logf func(string, ...any)) (*RealPricer, *RealFill) {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &RealPricer{ch: ch, now: now, staleAfter: staleAfter, logf: logf}
	return p, &RealFill{p: p}
}

// Price returns the live mid for (typ, strike); spot and ts are ignored (the book is
// the valuation). On a missing quote it clears the cache and returns 0 — a parity
// warning, not an expected path (the ATM window always covers the picked strike).
func (p *RealPricer) Price(typ live.OptionType, spot, strike float64, ts time.Time) float64 {
	q, ok := p.ch.Quote(typ, strike)
	if !ok {
		p.haveCache = false
		p.logf("RealPricer: no quote for %s %.0f — filling on fallback (parity warning)", typ, strike)
		return 0
	}
	p.cacheBid, p.cacheAsk = q.Bid, q.Ask
	p.haveCache = true
	p.lastStale = p.staleAfter > 0 && q.Age(p.now()) > p.staleAfter
	if p.lastStale {
		p.logf("RealPricer: STALE quote %s %.0f age=%s (parity warning)", typ, strike, q.Age(p.now()))
	}
	return q.Mid()
}

// LastStale reports whether the most recent Price read a stale quote (for the audit
// log's stale_flag in phase 5).
func (p *RealPricer) LastStale() bool { return p.lastStale }

// Fill returns the executed price for a side from the cached bid/ask. Falls back to
// mid only if there is no cached quote (the missing-quote safety net).
func (f *RealFill) Fill(mid float64, side live.Side) float64 {
	if !f.p.haveCache {
		return mid
	}
	if side == live.Buy {
		if f.p.cacheAsk > 0 {
			return f.p.cacheAsk
		}
		return mid
	}
	if f.p.cacheBid > 0 {
		return f.p.cacheBid
	}
	return mid
}
