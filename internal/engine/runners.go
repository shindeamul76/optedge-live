package engine

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge/live"
)

// TradeEvent is a completed round-trip emitted by one of the runners.
type TradeEvent struct {
	Runner string // "ORB" or "VWAP"
	Trade  live.Trade
}

// Config wires the Runners harness.
type Config struct {
	Chain      *chain.Chain
	Loc        *time.Location
	Now        func() time.Time
	StaleAfter time.Duration       // quote age beyond which a fill is flagged (0 = off)
	OnTrade    func(TradeEvent)    // called for each completed trade (E.md observation tap)
	Logf       func(string, ...any)
}

// Runners drives the two locked Runners (ORB + VWAP) on live bars, each with its own
// RealPricer/RealFill pair over the shared chain, and a shared LiveVIX. Feed it the
// future bars (OnFutureBar) and VIX bars (OnVIXBar). Completed trades surface via
// OnTrade. Not safe for concurrent use — drive it from one goroutine.
type Runners struct {
	orb, vwap *live.Runner
	vix       *LiveVIX
	onTrade   func(TradeEvent)

	orbSeen, vwapSeen int // number of trades already reported per runner
}

// NewRunners builds both runners from the locked configs with live seams injected.
func NewRunners(cfg Config) *Runners {
	vix := NewLiveVIX()

	orbCfg := live.LiveConfigORB(cfg.Loc)
	op, of := NewSeamPair(cfg.Chain, cfg.Now, cfg.StaleAfter, cfg.Logf)
	orbCfg.Pricer, orbCfg.Fill = op, of

	vwapCfg := live.LiveConfigVWAP(cfg.Loc)
	vp, vf := NewSeamPair(cfg.Chain, cfg.Now, cfg.StaleAfter, cfg.Logf)
	vwapCfg.Pricer, vwapCfg.Fill = vp, vf

	return &Runners{
		orb:     live.NewRunner(orbCfg, vix),
		vwap:    live.NewRunner(vwapCfg, vix),
		vix:     vix,
		onTrade: cfg.OnTrade,
	}
}

// VIX exposes the live IV source so the caller can feed VIX bars (or set it directly).
func (r *Runners) VIX() *LiveVIX { return r.vix }

// OnVIXBar updates the live IV from a closed India-VIX bar.
func (r *Runners) OnVIXBar(bar live.Bar) { r.vix.Set(bar.Close, bar.TS) }

// OnFutureBar drives both runners with a closed underlying (future) bar, then reports
// any newly-completed trades. History is nil: the frozen Runner ignores it and keeps
// its own per-session state (orb.go OnBar), and we feed bars in close order so
// no-lookahead holds. A trade is appended by the engine only on EXIT, so the length
// growth of Trades is exactly the set of round-trips that just completed.
func (r *Runners) OnFutureBar(bar live.Bar) {
	r.orb.OnBar(bar, nil)
	r.vwap.OnBar(bar, nil)
	r.drain()
}

func (r *Runners) drain() {
	for i := r.orbSeen; i < len(r.orb.Trades); i++ {
		r.emit(TradeEvent{Runner: "ORB", Trade: r.orb.Trades[i]})
	}
	r.orbSeen = len(r.orb.Trades)

	for i := r.vwapSeen; i < len(r.vwap.Trades); i++ {
		r.emit(TradeEvent{Runner: "VWAP", Trade: r.vwap.Trades[i]})
	}
	r.vwapSeen = len(r.vwap.Trades)
}

func (r *Runners) emit(ev TradeEvent) {
	if r.onTrade != nil {
		r.onTrade(ev)
	}
}
