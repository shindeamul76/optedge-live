package chain

import (
	"strconv"
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
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

// makeInsts builds a synthetic scrip master: NIFTY options across a strike range for
// two weekly expiries, with deterministic tokens "<expTag>-<strike>-<CE|PE>".
func makeInsts(loc *time.Location, expiries []time.Time, lo, hi, step float64) []broker.Instrument {
	var out []broker.Instrument
	for _, exp := range expiries {
		tag := exp.Format("0102")
		for k := lo; k <= hi; k += step {
			for _, ot := range []live.OptionType{live.Call, live.Put} {
				suf := "CE"
				if ot == live.Put {
					suf = "PE"
				}
				ks := strconv.Itoa(int(k))
				out = append(out, broker.Instrument{
					Token:          tag + "-" + ks + "-" + suf,
					Symbol:         "NIFTY" + tag + ks + suf,
					Name:           "NIFTY",
					Expiry:         exp,
					HasExpiry:      true,
					InstrumentType: "OPTIDX",
					ExchSeg:        "NFO",
					Strike:         k,
					OptType:        ot,
				})
			}
		}
	}
	return out
}

func newChain(t *testing.T, insts []broker.Instrument, loc *time.Location) *Chain {
	t.Helper()
	c, err := New(Config{
		Instruments: insts, Underlying: "NIFTY", StrikeStep: 50, N: 3, Loc: loc,
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	return c
}

func TestReconcile_InitialWindow(t *testing.T) {
	loc := istLoc(t)
	e1 := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	insts := makeInsts(loc, []time.Time{e1}, 22000, 24000, 50)
	c := newChain(t, insts, loc)

	sub, unsub, err := c.Reconcile(23072, time.Date(2026, 6, 30, 10, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// ATM(23072)=23050; N=3 → 7 strikes × CE/PE = 14 contracts to subscribe, none off.
	if len(sub) != 14 || len(unsub) != 0 {
		t.Fatalf("sub/unsub = %d/%d, want 14/0", len(sub), len(unsub))
	}
	if c.Center() != 23050 {
		t.Errorf("center = %.0f, want 23050", c.Center())
	}
	// The picked ATM strike must be resolvable for both CE and PE.
	for _, ot := range []live.OptionType{live.Call, live.Put} {
		if _, ok := c.byKey[key(ot, 23050)]; !ok {
			t.Errorf("ATM %s 23050 not in window", ot)
		}
	}
}

func TestReconcile_HysteresisNoThrash(t *testing.T) {
	loc := istLoc(t)
	e1 := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	insts := makeInsts(loc, []time.Time{e1}, 22000, 24000, 50)
	c := newChain(t, insts, loc)
	asOf := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)

	c.Reconcile(23050, asOf) // center 23050

	// Drift to 23074 — ATM rounds to 23050 still (within 0.5), definitely no change.
	if sub, unsub, _ := c.Reconcile(23074, asOf); len(sub)+len(unsub) != 0 {
		t.Fatalf("small drift caused churn: sub=%d unsub=%d", len(sub), len(unsub))
	}
	// Drift to 23080 — that's 30 past center (<0.75*50=37.5) → still held (hysteresis).
	if sub, unsub, _ := c.Reconcile(23080, asOf); len(sub)+len(unsub) != 0 {
		t.Fatalf("within-band drift caused churn: sub=%d unsub=%d", len(sub), len(unsub))
	}
	if c.Center() != 23050 {
		t.Errorf("center drifted to %.0f, want held at 23050", c.Center())
	}
}

func TestReconcile_RecenterOnDrift(t *testing.T) {
	loc := istLoc(t)
	e1 := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	insts := makeInsts(loc, []time.Time{e1}, 22000, 24000, 50)
	c := newChain(t, insts, loc)
	asOf := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)

	c.Reconcile(23050, asOf) // center 23050, window 22900..23200

	// Move well past the band → recenter to 23100. Window 22950..23300.
	sub, unsub, err := c.Reconcile(23100, asOf)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c.Center() != 23100 {
		t.Fatalf("center = %.0f, want 23100", c.Center())
	}
	// One-strike shift (23050→23100): 22900 leaves the window, 23250 enters, each
	// CE+PE → 2 subscribe, 2 unsubscribe.
	if len(sub) != 2 || len(unsub) != 2 {
		t.Fatalf("sub/unsub = %d/%d, want 2/2", len(sub), len(unsub))
	}
}

func TestReconcile_WeeklyRoll(t *testing.T) {
	loc := istLoc(t)
	e1 := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	e2 := time.Date(2026, 7, 9, 0, 0, 0, 0, loc)
	insts := makeInsts(loc, []time.Time{e1, e2}, 22000, 24000, 50)
	c := newChain(t, insts, loc)

	// Before e1 → weekly is e1.
	c.Reconcile(23050, time.Date(2026, 6, 30, 10, 0, 0, 0, loc))
	if !c.Expiry().Equal(e1) {
		t.Fatalf("expiry = %s, want e1", c.Expiry())
	}

	// After e1 → weekly rolls to e2: entire window swaps (14 out, 14 in), same center.
	sub, unsub, err := c.Reconcile(23050, time.Date(2026, 7, 3, 10, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !c.Expiry().Equal(e2) {
		t.Fatalf("expiry did not roll: %s", c.Expiry())
	}
	if len(sub) != 14 || len(unsub) != 14 {
		t.Fatalf("roll sub/unsub = %d/%d, want 14/14", len(sub), len(unsub))
	}
}

func TestUpdateAndQuote(t *testing.T) {
	loc := istLoc(t)
	e1 := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	insts := makeInsts(loc, []time.Time{e1}, 22000, 24000, 50)
	c := newChain(t, insts, loc)
	c.Reconcile(23050, time.Date(2026, 6, 30, 10, 0, 0, 0, loc))

	tok := c.byKey[key(live.Call, 23050)]
	ts := time.Date(2026, 6, 30, 10, 0, 1, 0, loc)
	if !c.Update(feed.Tick{Token: tok, LTP: 120, Bids: [5]feed.DepthLevel{{Price: 119}}, Asks: [5]feed.DepthLevel{{Price: 121}}, OI: 500, ExchTime: ts}) {
		t.Fatal("Update returned false for an in-window token")
	}
	q, ok := c.Quote(live.Call, 23050)
	if !ok {
		t.Fatal("Quote not found after Update")
	}
	if q.Bid != 119 || q.Ask != 121 || q.Mid() != 120 {
		t.Errorf("quote bid/ask/mid = %.0f/%.0f/%.0f, want 119/121/120", q.Bid, q.Ask, q.Mid())
	}
	// A tick for a token not in the window is ignored.
	if c.Update(feed.Tick{Token: "not-in-window", LTP: 1}) {
		t.Error("Update accepted an out-of-window token")
	}
}
