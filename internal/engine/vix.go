// Package engine plugs the live data into the frozen optedge engine: it implements
// the three seams (RealPricer, RealFill, LiveVIX) and wires the two locked Runners
// (ORB + VWAP), driving them bar by bar. Because trade DECISIONS run only on the
// underlying + clock + VIX, injecting these live seams cannot change which trades
// fire — only the fill P&L. See lld-explained/D.md, E.md, F.md.
package engine

import (
	"sync"
	"time"
)

// LiveVIX implements live.IVSource from the live India-VIX 1-minute bars. It keeps
// the latest value and answers At(t) with it (carry-forward, matching the backtest's
// IVSeries semantics), converting VIX% → sigma decimal (÷100) and clamping positive.
type LiveVIX struct {
	mu    sync.RWMutex
	sigma float64
	ts    time.Time
	have  bool
}

// NewLiveVIX builds an empty source (At returns 0 until the first Set, matching
// IVSeries on an empty series).
func NewLiveVIX() *LiveVIX { return &LiveVIX{} }

// Set records the latest VIX close (annualized percentage, e.g. 13.91) at ts.
func (v *LiveVIX) Set(vixClose float64, ts time.Time) {
	s := vixClose / 100.0
	if s < 1e-4 {
		s = 1e-4 // clamp positive, as IVSeries does
	}
	v.mu.Lock()
	v.sigma, v.ts, v.have = s, ts, true
	v.mu.Unlock()
}

// At returns the latest sigma (decimal). t is unused because we hold only the latest
// value; bars arrive in order, so the latest is the value effective at or before t.
func (v *LiveVIX) At(t time.Time) float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.have {
		return 0
	}
	return v.sigma
}

// LastUpdate returns the timestamp of the latest VIX value (for staleness checks).
func (v *LiveVIX) LastUpdate() (time.Time, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.ts, v.have
}
