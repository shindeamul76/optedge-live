package engine

import (
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge/live"
)

// TestRunners_ORBBreakoutFillsFromChain is the phase-4 end-to-end proof: the FROZEN
// ORB Runner, driven live via OnBar(bar,nil) with the RealPricer/RealFill seams
// injected, produces a trade whose fills come from the LIVE chain (buy@ask 102,
// sell@bid 100) — not from the synthetic model. This validates the whole wiring.
func TestRunners_ORBBreakoutFillsFromChain(t *testing.T) {
	loc := istLoc(t)
	exp := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	ch, err := chain.New(chain.Config{
		Instruments: makeInsts(exp, 22000, 24000, 50), Underlying: "NIFTY",
		StrikeStep: 50, N: 3, Loc: loc, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	now := time.Date(2026, 6, 30, 9, 35, 0, 0, loc)
	// Build the ATM window and quote every window CE at bid100/ask102 so whichever
	// ATM strike the delta gate picks (23050 for spot ~23060) is covered.
	ch.Reconcile(23050, now)
	for k := 22900.0; k <= 23200; k += 50 {
		setQuote(ch, exp, k, live.Call, 100, 102, now)
	}

	var trades []TradeEvent
	r := NewRunners(Config{
		Chain: ch, Loc: loc, Now: func() time.Time { return now },
		OnTrade: func(ev TradeEvent) { trades = append(trades, ev) },
		Logf:    func(string, ...any) {},
	})
	r.VIX().Set(13.9, now) // sigma for the delta gate

	bar := func(hms string, o, h, l, c float64, v int64) live.Bar {
		ts, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-06-30 "+hms, loc)
		return live.Bar{TS: ts, Open: o, High: h, Low: l, Close: c, Volume: v}
	}

	// Opening range 09:15–09:30: tight 23040–23060, low volume.
	for _, hms := range []string{"09:15:00", "09:20:00", "09:29:00", "09:30:00"} {
		r.OnFutureBar(bar(hms, 23050, 23060, 23040, 23050, 100))
	}
	// 09:31 breakout above 23060: high volume + wide range + close beyond → confluence.
	r.OnFutureBar(bar("09:31:00", 23060, 23120, 23060, 23110, 1000))
	// 09:32 spikes past the target (23060 + 1.5×20 = 23090) → exit.
	r.OnFutureBar(bar("09:32:00", 23110, 23200, 23100, 23180, 500))
	// Fallback square-off much later, in case the exit didn't fire above.
	r.OnFutureBar(bar("15:16:00", 23180, 23185, 23175, 23180, 100))

	// Find the ORB trade.
	var orb *live.Trade
	for i := range trades {
		if trades[i].Runner == "ORB" {
			orb = &trades[i].Trade
			break
		}
	}
	if orb == nil {
		t.Fatalf("no ORB trade produced (events: %+v)", trades)
	}
	if orb.Dir != live.Long || orb.OptType != live.Call || orb.Strike != 23050 {
		t.Errorf("trade = %s %s %.0f, want LONG CALL 23050", orb.Dir, orb.OptType, orb.Strike)
	}
	if orb.Qty != 75 {
		t.Errorf("qty = %d, want 75", orb.Qty)
	}
	// The whole point: fills came from the real chain, not the synthetic model.
	if orb.EntryPrem != 102 {
		t.Errorf("EntryPrem = %.2f, want 102 (real ask)", orb.EntryPrem)
	}
	if orb.ExitPrem != 100 {
		t.Errorf("ExitPrem = %.2f, want 100 (real bid)", orb.ExitPrem)
	}
}
