package pipeline

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge-live/internal/feed"
	"github.com/shindeamul76/optedge/live"
)

func istLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	return loc
}

func makeInsts(exp time.Time, lo, hi, step float64) []broker.Instrument {
	tag := exp.Format("0102")
	var out []broker.Instrument
	for k := lo; k <= hi; k += step {
		ks := strconv.Itoa(int(k))
		for _, ot := range []live.OptionType{live.Call, live.Put} {
			suf := "CE"
			if ot == live.Put {
				suf = "PE"
			}
			out = append(out, broker.Instrument{
				Token: tag + "-" + ks + "-" + suf, Symbol: "NIFTY" + tag + ks + suf, Name: "NIFTY",
				Expiry: exp, HasExpiry: true, InstrumentType: "OPTIDX", ExchSeg: "NFO", Strike: k, OptType: ot,
			})
		}
	}
	return out
}

// fakeFeed satisfies pipeline.Feed: it emits ticks on a channel and records Modify.
type fakeFeed struct {
	ticks chan feed.Tick
	mu    sync.Mutex
	added []string
}

func (f *fakeFeed) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (f *fakeFeed) Ticks() <-chan feed.Tick        { return f.ticks }
func (f *fakeFeed) Modify(add, _ []feed.TokenList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range add {
		f.added = append(f.added, l.Tokens...)
	}
}
func (f *fakeFeed) addedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.added)
}

func TestCoordinator_FutureBarDrivesChainAndRoutesOptions(t *testing.T) {
	loc := istLoc(t)
	exp := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	insts := makeInsts(exp, 22000, 24000, 50)

	ch, err := chain.New(chain.Config{
		Instruments: insts, Underlying: "NIFTY", StrikeStep: 50, N: 3, Loc: loc,
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	ff := &fakeFeed{ticks: make(chan feed.Tick, 16)}

	var mu sync.Mutex
	cur := time.Date(2026, 6, 30, 9, 15, 10, 0, loc)
	nowFn := func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	setNow := func(v time.Time) { mu.Lock(); cur = v; mu.Unlock() }

	co, err := New(Config{
		Feed:            ff,
		Chain:           ch,
		FutureToken:     "FUT",
		AggregateTokens: []string{"FUT", "VIX"},
		Loc:             loc,
		Now:             nowFn,
		CloseInterval:   5 * time.Millisecond,
		Logf:            func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go co.Run(ctx)

	at := func(hms string) time.Time {
		ts, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-06-30 "+hms, loc)
		return ts
	}

	// Future ticks in the 09:15 minute at spot 23050.
	ff.ticks <- feed.Tick{Token: "FUT", ExchTime: at("09:15:10"), LTP: 23050, Volume: 1000}
	ff.ticks <- feed.Tick{Token: "FUT", ExchTime: at("09:15:50"), LTP: 23050, Volume: 1020}

	// Advance the wall clock past M+1:00 → the future bar closes, driving Reconcile.
	setNow(at("09:16:01"))

	// A future bar should be emitted…
	select {
	case tb := <-co.Bars():
		if tb.Token != "FUT" || tb.Bar.Close != 23050 {
			t.Fatalf("bar = %s close %.0f, want FUT 23050", tb.Token, tb.Bar.Close)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no future bar emitted")
	}

	// …and the chain window (ATM 23050 ±3 × CE/PE = 14) subscribed via feed.Modify.
	waitFor(t, "chain window subscribed", func() bool { return ff.addedCount() == 14 })
	if ch.Center() != 23050 {
		t.Errorf("chain center = %.0f, want 23050", ch.Center())
	}

	// Now an option tick for the ATM CE (in the window) must update the chain quote.
	atmCE := exp.Format("0102") + "-23050-CE"
	ff.ticks <- feed.Tick{
		Token: atmCE, ExchTime: at("09:16:05"), LTP: 120,
		Bids: [5]feed.DepthLevel{{Price: 119}}, Asks: [5]feed.DepthLevel{{Price: 121}},
	}
	waitFor(t, "chain quote for ATM CE", func() bool {
		q, ok := ch.Quote(live.Call, 23050)
		return ok && q.Mid() == 120
	})

	cancel()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
