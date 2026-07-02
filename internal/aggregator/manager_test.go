package aggregator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/feed"
)

// TestManager_RoutesAndClosesOnClock drives the Manager with a controllable clock:
// it folds two future ticks, advances the wall clock past M+1:00, and expects the
// ticker to close the bar. A tick for an unregistered token must be ignored.
func TestManager_RoutesAndClosesOnClock(t *testing.T) {
	loc := istLoc(t)

	var mu sync.Mutex
	cur := at(loc, "09:15:10")
	nowFn := func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	setNow := func(v time.Time) { mu.Lock(); cur = v; mu.Unlock() }

	m, err := NewManager(Config{
		Tokens:             []string{"FUT"},
		Loc:                loc,
		Now:                nowFn,
		CloseCheckInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	ticks := make(chan feed.Tick, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx, ticks); close(done) }()

	// Two future ticks in 09:15, plus one for an unregistered option token.
	ticks <- tk(loc, "09:15:10", 100, 1000)
	ticks <- tk(loc, "09:15:50", 104, 1020)
	ticks <- feed.Tick{Token: "OPT", ExchTime: at(loc, "09:15:55"), LTP: 55, Volume: 5}

	// Advance the wall clock past M+1:00 so the ticker closes the bar.
	setNow(at(loc, "09:16:01"))

	select {
	case tb := <-m.Bars():
		if tb.Token != "FUT" {
			t.Fatalf("bar token = %q, want FUT", tb.Token)
		}
		if tb.Bar.Open != 100 || tb.Bar.Close != 104 {
			t.Errorf("bar OHLC open/close = %.0f/%.0f, want 100/104", tb.Bar.Open, tb.Bar.Close)
		}
		if !tb.Bar.TS.Equal(at(loc, "09:15:00")) {
			t.Errorf("bar TS = %s, want 09:15:00", tb.Bar.TS)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bar emitted after clock advanced past M+1:00")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
