package aggregator

import (
	"context"
	"time"

	"github.com/shindeamul76/optedge-live/internal/feed"
)

// TokenBar is a closed bar tagged with the instrument it belongs to.
type TokenBar struct {
	Token string
	Bar   Bar
}

// Config wires a Manager. Tokens lists the instruments to aggregate into bars
// (typically the front-month future and India VIX — options are NOT aggregated;
// they stay as live quotes in the chain). Loc/Now/CloseCheckInterval default.
type Config struct {
	Tokens             []string
	Loc                *time.Location
	Now                func() time.Time
	CloseCheckInterval time.Duration // how often the wall clock is checked for M+1:00 closes
}

// Manager routes ticks to per-token aggregators and emits closed bars on Bars().
// Ticks for tokens not in the configured set (e.g. option contracts) are ignored.
type Manager struct {
	loc      *time.Location
	now      func() time.Time
	interval time.Duration
	aggs     map[string]*Aggregator
	out      chan TokenBar
}

// NewManager builds a Manager with defaults applied.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Loc == nil {
		loc, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			return nil, err
		}
		cfg.Loc = loc
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CloseCheckInterval <= 0 {
		cfg.CloseCheckInterval = 200 * time.Millisecond
	}
	m := &Manager{
		loc:      cfg.Loc,
		now:      cfg.Now,
		interval: cfg.CloseCheckInterval,
		aggs:     make(map[string]*Aggregator, len(cfg.Tokens)),
		out:      make(chan TokenBar, 256),
	}
	for _, tok := range cfg.Tokens {
		m.aggs[tok] = New(cfg.Loc)
	}
	return m, nil
}

// Bars is the stream of closed bars, tagged by token.
func (m *Manager) Bars() <-chan TokenBar { return m.out }

// Run consumes ticks and emits bars until ctx is cancelled or the tick channel
// closes. A wall-clock ticker drives the M+1:00 closes so quiet minutes still close
// on time; incoming ticks for a new minute also close the prior one immediately.
func (m *Manager) Run(ctx context.Context, ticks <-chan feed.Tick) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ticks:
			if !ok {
				return
			}
			agg := m.aggs[t.Token]
			if agg == nil {
				continue // not an aggregated instrument (e.g. an option contract)
			}
			if bar, closed := agg.Add(t); closed {
				m.emit(ctx, t.Token, bar)
			}
		case <-ticker.C:
			now := m.now()
			for tok, agg := range m.aggs {
				if bar, closed := agg.CloseDue(now); closed {
					m.emit(ctx, tok, bar)
				}
			}
		}
	}
}

// Stats returns per-token (droppedLate, emitted) counters.
func (m *Manager) Stats() map[string][2]int {
	out := make(map[string][2]int, len(m.aggs))
	for tok, agg := range m.aggs {
		d, e := agg.Stats()
		out[tok] = [2]int{d, e}
	}
	return out
}

func (m *Manager) emit(ctx context.Context, tok string, bar Bar) {
	select {
	case m.out <- TokenBar{Token: tok, Bar: bar}:
	case <-ctx.Done():
	}
}
