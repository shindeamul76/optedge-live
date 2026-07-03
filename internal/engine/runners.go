package engine

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge/live"
)

// TradeEvent is a completed round-trip. Trade carries the REAL fills; Synth is its
// synthetic-model twin (identical decision, synthetic Black-76 fills) — the two differ
// only in fill P&L, which is exactly the divergence v3 measures. HasSynth is false if
// the twin isn't available (shouldn't happen in normal operation).
type TradeEvent struct {
	Runner   string
	Trade    live.Trade
	Synth    live.Trade
	HasSynth bool
}

// Config wires the Runners harness.
type Config struct {
	Chain      *chain.Chain
	Loc        *time.Location
	Now        func() time.Time
	StaleAfter time.Duration       // quote age beyond which a fill is flagged (0 = off)
	LotSize    int                 // override the frozen lot size (0 = keep the engine default)
	OnTrade    func(TradeEvent)    // called for each completed trade (E.md observation tap)
	OnFill     func(FillLeg)       // called for every executed leg (entry Buy / exit Sell)
	Logf       func(string, ...any)
}

// Runners drives four locked Runners on the same live bars: ORB + VWAP with REAL seams
// (the live paper record) and ORB + VWAP with the SYNTHETIC seams (the shadow used for
// fill-divergence). Because decisions never touch the pricer/fill, each real runner and
// its synthetic twin fire identical trades in lockstep, so they pair by index. Feed
// future bars via OnFutureBar and VIX bars via OnVIXBar. Not safe for concurrent use.
type Runners struct {
	orbReal, vwapReal   *live.Runner
	orbSynth, vwapSynth *live.Runner
	orbPricer           *RealPricer
	vwapPricer          *RealPricer
	vix                 *LiveVIX
	onTrade             func(TradeEvent)

	orbSeen, vwapSeen int // real trades already reported per runner
}

// NewRunners builds the real and synthetic runner pairs from the locked configs. The
// real runners get injected RealPricer/RealFill seams; the synthetic runners leave the
// seams nil so NewRunner falls back to the synthetic Black-76 pricer + modeled spread —
// the exact backtest fill model, now driven on live bars. All four share one LiveVIX.
func NewRunners(cfg Config) *Runners {
	vix := NewLiveVIX()

	// Real ORB.
	orbCfg := live.LiveConfigORB(cfg.Loc)
	op, of := NewSeamPair(cfg.Chain, cfg.Now, cfg.StaleAfter, cfg.Logf, "ORB", cfg.OnFill)
	orbCfg.Pricer, orbCfg.Fill = op, of

	// Real VWAP.
	vwapCfg := live.LiveConfigVWAP(cfg.Loc)
	vp, vf := NewSeamPair(cfg.Chain, cfg.Now, cfg.StaleAfter, cfg.Logf, "VWAP", cfg.OnFill)
	vwapCfg.Pricer, vwapCfg.Fill = vp, vf

	// Synthetic shadows: seams left nil → synthetic Black-76 + modeled spread.
	orbSynthCfg := live.LiveConfigORB(cfg.Loc)
	vwapSynthCfg := live.LiveConfigVWAP(cfg.Loc)

	// Optional lot-size override for the live deployment (e.g. NSE revised the lot).
	// Applied to ALL FOUR runners so real and synthetic stay the same scale — the
	// Sizer is an exported struct on the frozen Config, mutated in place here.
	if cfg.LotSize > 0 {
		orbCfg.Sizer.LotSize = cfg.LotSize
		vwapCfg.Sizer.LotSize = cfg.LotSize
		orbSynthCfg.Sizer.LotSize = cfg.LotSize
		vwapSynthCfg.Sizer.LotSize = cfg.LotSize
	}

	return &Runners{
		orbReal:    live.NewRunner(orbCfg, vix),
		vwapReal:   live.NewRunner(vwapCfg, vix),
		orbSynth:   live.NewRunner(orbSynthCfg, vix),
		vwapSynth:  live.NewRunner(vwapSynthCfg, vix),
		orbPricer:  op,
		vwapPricer: vp,
		vix:        vix,
		onTrade:    cfg.OnTrade,
	}
}

// VIX exposes the live IV source so the caller can feed VIX bars (or set it directly).
func (r *Runners) VIX() *LiveVIX { return r.vix }

// LoadReplay preloads recorded fills per runner for crash-recovery replay (real runners
// only — the synthetic shadows recompute deterministically from the replayed bars).
func (r *Runners) LoadReplay(orbFills, vwapFills []float64) {
	r.orbPricer.LoadReplay(orbFills)
	r.vwapPricer.LoadReplay(vwapFills)
}

// OnVIXBar updates the live IV from a closed India-VIX bar (feeds both real and synth).
func (r *Runners) OnVIXBar(bar live.Bar) { r.vix.Set(bar.Close, bar.TS) }

// OnFutureBar drives all four runners with a closed underlying bar, then reports any
// newly-completed trades paired with their synthetic twins. History is nil: the frozen
// Runner ignores it and keeps its own per-session state; bars are fed in close order.
func (r *Runners) OnFutureBar(bar live.Bar) {
	r.orbReal.OnBar(bar, nil)
	r.vwapReal.OnBar(bar, nil)
	r.orbSynth.OnBar(bar, nil)
	r.vwapSynth.OnBar(bar, nil)
	r.drain()
}

func (r *Runners) drain() {
	r.drainOne("ORB", r.orbReal, r.orbSynth, &r.orbSeen)
	r.drainOne("VWAP", r.vwapReal, r.vwapSynth, &r.vwapSeen)
}

// drainOne emits each newly-completed real trade paired with its synthetic twin (same
// index — the real and synthetic runners complete trades in lockstep).
func (r *Runners) drainOne(name string, real, synth *live.Runner, seen *int) {
	for i := *seen; i < len(real.Trades); i++ {
		ev := TradeEvent{Runner: name, Trade: real.Trades[i]}
		if i < len(synth.Trades) {
			ev.Synth = synth.Trades[i]
			ev.HasSynth = true
		}
		r.emit(ev)
	}
	*seen = len(real.Trades)
}

func (r *Runners) emit(ev TradeEvent) {
	if r.onTrade != nil {
		r.onTrade(ev)
	}
}
