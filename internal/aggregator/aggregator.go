// Package aggregator folds the live tick stream into 1-minute OHLCV bars shaped
// exactly like the backtest's core.Bar, so the frozen engine can't tell it's
// running live. The parity-critical rule (see lld-explained/B.md): a minute's bar
// closes on the WALL CLOCK at M+1:00 (via CloseDue), not when the next tick
// arrives — so a quiet minute still closes on time and the engine's clock-based
// exits stay honest.
//
// The Aggregator itself is pure and single-instrument: Add folds ticks (bucketed
// by exchange timestamp), CloseDue closes on wall-clock time, and both are driven
// by the Manager (manager.go). Bar timestamps are the minute START in IST, matching
// Angel One's historical candles the backtest used.
package aggregator

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/feed"
	"github.com/shindeamul76/optedge/live"
)

// Bar is the engine's bar type (alias through the facade), so a bar produced here
// IS the value the frozen Runner consumes.
type Bar = live.Bar

// Aggregator builds 1-minute bars for one instrument. Not safe for concurrent use;
// the Manager drives it from a single goroutine.
type Aggregator struct {
	loc *time.Location

	cur *partial // the currently-forming minute, or nil

	lastVolume     int64     // cumulative day-volume at the last CLOSED bar's end
	haveLastVolume bool      // false until the first bar closes (seeds V=0)
	lastClosed     time.Time // minute of the last closed bar (drops late ticks)

	droppedLate int
	emitted     int
}

type partial struct {
	minute                  time.Time // floored to the minute, in loc
	open, high, low, closeP float64
	endVolume               int64 // cumulative day-volume as of the latest tick
	ticks                   int
}

// New builds an aggregator for the given IST location.
func New(loc *time.Location) *Aggregator { return &Aggregator{loc: loc} }

// Add folds one tick into the forming minute, bucketed by the tick's EXCHANGE
// timestamp. If the tick belongs to a later minute than the one forming, the old
// minute is closed on time (its M+1:00 has by definition passed) and returned. A
// tick for an already-closed minute is a late straggler and is dropped.
func (a *Aggregator) Add(t feed.Tick) (Bar, bool) {
	m := floorMinute(t.ExchTime, a.loc)

	// Late: a tick for a minute we've already closed.
	if !a.lastClosed.IsZero() && !m.After(a.lastClosed) {
		a.droppedLate++
		return Bar{}, false
	}

	var closed Bar
	didClose := false
	if a.cur != nil && m.After(a.cur.minute) {
		closed = a.finalize()
		didClose = true
	}

	if a.cur == nil {
		a.cur = &partial{minute: m, open: t.LTP, high: t.LTP, low: t.LTP, closeP: t.LTP, endVolume: t.Volume, ticks: 1}
		return closed, didClose
	}

	// Same minute: fold.
	a.cur.closeP = t.LTP
	if t.LTP > a.cur.high {
		a.cur.high = t.LTP
	}
	if t.LTP < a.cur.low {
		a.cur.low = t.LTP
	}
	a.cur.endVolume = t.Volume
	a.cur.ticks++
	return closed, didClose
}

// CloseDue closes and returns the forming bar if the wall clock `now` has reached
// its M+1:00 boundary. This is the primary close path: it fires for quiet minutes
// with no further ticks, keeping the one-bar-per-minute cadence.
func (a *Aggregator) CloseDue(now time.Time) (Bar, bool) {
	if a.cur == nil {
		return Bar{}, false
	}
	if now.In(a.loc).Before(a.cur.minute.Add(time.Minute)) {
		return Bar{}, false
	}
	return a.finalize(), true
}

// finalize builds the bar for the forming minute, computing volume as the delta of
// cumulative day-volume since the previous bar (V=0 for the first bar, since there
// is no prior baseline). It then clears the forming minute and records it as closed.
func (a *Aggregator) finalize() Bar {
	p := a.cur
	var vol int64
	if a.haveLastVolume {
		vol = p.endVolume - a.lastVolume
		if vol < 0 {
			vol = 0 // guard against a feed reset/rollover
		}
	}
	a.lastVolume = p.endVolume
	a.haveLastVolume = true
	a.lastClosed = p.minute
	a.cur = nil
	a.emitted++
	return Bar{TS: p.minute, Open: p.open, High: p.high, Low: p.low, Close: p.closeP, Volume: vol}
}

// Stats returns counters for observability (late ticks dropped, bars emitted).
func (a *Aggregator) Stats() (droppedLate, emitted int) { return a.droppedLate, a.emitted }

// floorMinute truncates t to the start of its minute in loc (bar timestamps are the
// minute start, matching the backtest's candle convention).
func floorMinute(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), 0, 0, loc)
}
