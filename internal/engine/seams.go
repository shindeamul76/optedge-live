package engine

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge/live"
)

// FillLeg is one executed leg captured via the fill tap: Buy = an entry, Sell = an
// exit (E.md — the seams are the only place the frozen engine exposes fills). It
// carries the contract and the full quote so persistence (position.json) and the
// parity gate can record real bid/ask/mid and staleness, not just the fill. Rupees.
type FillLeg struct {
	Runner              string
	Side                live.Side
	Typ                 live.OptionType
	Strike              float64
	Expiry              string
	Bid, Ask, Mid, Fill float64
	Stale               bool
	Fallback            bool // fill did NOT come from a real book price (missing quote/side)
	TS                  time.Time
}

// RealPricer implements live.OptionPricer by reading the live option chain. It
// ignores spot (the real book already reflects it), returns the contract's mid, and
// caches that contract's quote for the paired RealFill — the engine always calls
// Price then Fill on the same contract within one OnBar, so the cache is fresh (D.md).
type RealPricer struct {
	ch         *chain.Chain
	now        func() time.Time
	staleAfter time.Duration
	logf       func(string, ...any)

	// cache of the just-quoted contract, read by the paired RealFill.
	haveCache        bool
	cTyp             live.OptionType
	cStrike          float64
	cExpiry          string
	cBid, cAsk, cMid float64
	cStale           bool

	// replay queue: during crash-recovery replay, Price/Fill serve RECORDED fills (in
	// Price→Fill pairs) so a reconstructed position carries its original entry price.
	// Once exhausted, the seam transitions automatically to the live chain. See G.md.
	replay []float64
	ridx   int
}

// LoadReplay preloads recorded fills to be served (in order, one per Price→Fill pair)
// during recovery replay. After they're consumed the pricer reads the live chain.
func (p *RealPricer) LoadReplay(fills []float64) { p.replay = fills; p.ridx = 0 }

func (p *RealPricer) replaying() bool { return p.ridx < len(p.replay) }

// RealFill implements live.FillModel: buy pays the real ask, sell receives the real
// bid, from the quote the paired RealPricer just cached. Each fill is also emitted to
// onFill (the observation tap) so the caller can persist entries/exits.
type RealFill struct {
	p      *RealPricer
	runner string
	onFill func(FillLeg)
	now    func() time.Time
}

// NewSeamPair builds a paired RealPricer + RealFill sharing one chain. Each Runner
// gets its OWN pair (cache lives in the pair) so runners can't clobber each other's
// cached contract. onFill (may be nil) receives every executed leg, tagged runner.
func NewSeamPair(ch *chain.Chain, now func() time.Time, staleAfter time.Duration, logf func(string, ...any), runner string, onFill func(FillLeg)) (*RealPricer, *RealFill) {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &RealPricer{ch: ch, now: now, staleAfter: staleAfter, logf: logf}
	return p, &RealFill{p: p, runner: runner, onFill: onFill, now: now}
}

// Price returns the live mid for (typ, strike); spot and ts are ignored. On a missing
// quote it clears the cache and returns 0 — a parity warning, not an expected path.
func (p *RealPricer) Price(typ live.OptionType, spot, strike float64, ts time.Time) float64 {
	if p.replaying() {
		return p.replay[p.ridx] // peek; the paired Fill consumes it
	}
	q, ok := p.ch.Quote(typ, strike)
	if !ok {
		p.haveCache = false
		p.logf("RealPricer: no quote for %s %.0f — filling on fallback (parity warning)", typ, strike)
		return 0
	}
	p.cTyp, p.cStrike = typ, strike
	p.cExpiry = p.ch.Expiry().Format("2006-01-02")
	p.cBid, p.cAsk, p.cMid = q.Bid, q.Ask, q.Mid()
	p.cStale = p.staleAfter > 0 && q.Age(p.now()) > p.staleAfter
	p.haveCache = true
	if p.cStale {
		p.logf("RealPricer: STALE quote %s %.0f age=%s (parity warning)", typ, strike, q.Age(p.now()))
	}
	return p.cMid
}

// Fill returns the executed price for a side from the cached quote (falling back to
// mid only when there is no cached quote), and emits the leg to the tap.
func (f *RealFill) Fill(mid float64, side live.Side) float64 {
	if f.p.replaying() {
		price := f.p.replay[f.p.ridx]
		f.p.ridx++
		return price // recovery replay: recorded fill, no tap (recorder is muted)
	}
	// Fill from the real book when available; otherwise fall back to mid and FLAG it,
	// so the parity gate can exclude a trade priced on a missing quote/side rather than
	// let a ₹0 (or mid) fill silently corrupt the incubation record.
	price := mid
	fallback := true
	if f.p.haveCache {
		if side == live.Buy && f.p.cAsk > 0 {
			price, fallback = f.p.cAsk, false
		} else if side == live.Sell && f.p.cBid > 0 {
			price, fallback = f.p.cBid, false
		}
	}
	if fallback {
		f.p.logf("RealFill: %s fill on FALLBACK (no real book price) for %s %.0f — trade will be flagged",
			side, f.p.cTyp, f.p.cStrike)
	}
	if f.onFill != nil {
		f.onFill(FillLeg{
			Runner: f.runner, Side: side, Typ: f.p.cTyp, Strike: f.p.cStrike, Expiry: f.p.cExpiry,
			Bid: f.p.cBid, Ask: f.p.cAsk, Mid: f.p.cMid, Fill: price, Stale: f.p.cStale, Fallback: fallback, TS: f.now(),
		})
	}
	return price
}
