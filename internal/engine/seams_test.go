package engine

import (
	"strconv"
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

func tokenFor(exp time.Time, strike float64, ot live.OptionType) string {
	suf := "CE"
	if ot == live.Put {
		suf = "PE"
	}
	return exp.Format("0102") + "-" + strconv.Itoa(int(strike)) + "-" + suf
}

func setQuote(ch *chain.Chain, exp time.Time, strike float64, ot live.OptionType, bid, ask float64, ts time.Time) {
	ch.Update(feed.Tick{
		Token: tokenFor(exp, strike, ot),
		Bids:  [5]feed.DepthLevel{{Price: bid}},
		Asks:  [5]feed.DepthLevel{{Price: ask}},
		LTP:   (bid + ask) / 2, ExchTime: ts,
	})
}

func TestLiveVIX(t *testing.T) {
	v := NewLiveVIX()
	if got := v.At(time.Now()); got != 0 {
		t.Errorf("empty At = %v, want 0", got)
	}
	v.Set(13.91, time.Now())
	if got := v.At(time.Now()); got < 0.1390 || got > 0.1392 {
		t.Errorf("At = %v, want ~0.1391", got)
	}
	v.Set(0, time.Now()) // clamp positive
	if got := v.At(time.Now()); got != 1e-4 {
		t.Errorf("clamp At = %v, want 1e-4", got)
	}
}

func TestSeamPair_PriceThenFill(t *testing.T) {
	loc := istLoc(t)
	exp := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	ch, _ := chain.New(chain.Config{Instruments: makeInsts(exp, 22000, 24000, 50), Underlying: "NIFTY", StrikeStep: 50, N: 3, Loc: loc, Logf: func(string, ...any) {}})
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)
	ch.Reconcile(23050, now)
	setQuote(ch, exp, 23050, live.Call, 119, 121, now)

	p, f := NewSeamPair(ch, func() time.Time { return now }, 0, nil, "", nil)

	mid := p.Price(live.Call, 99999 /*ignored spot*/, 23050, now)
	if mid != 120 {
		t.Fatalf("mid = %v, want 120", mid)
	}
	if got := f.Fill(mid, live.Buy); got != 121 {
		t.Errorf("buy fill = %v, want 121 (ask)", got)
	}
	if got := f.Fill(mid, live.Sell); got != 119 {
		t.Errorf("sell fill = %v, want 119 (bid)", got)
	}
}

func TestSeamPair_MissingQuoteFallsBack(t *testing.T) {
	loc := istLoc(t)
	exp := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	ch, _ := chain.New(chain.Config{Instruments: makeInsts(exp, 22000, 24000, 50), Underlying: "NIFTY", StrikeStep: 50, N: 3, Loc: loc, Logf: func(string, ...any) {}})
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)
	ch.Reconcile(23050, now) // window built, but no quote set for 23050

	p, f := NewSeamPair(ch, func() time.Time { return now }, 0, nil, "", nil)
	if mid := p.Price(live.Call, 0, 23050, now); mid != 0 {
		t.Errorf("missing-quote mid = %v, want 0", mid)
	}
	if got := f.Fill(42, live.Buy); got != 42 {
		t.Errorf("fallback fill = %v, want mid 42", got)
	}
}

// TestTwoPairsNoClobber proves each runner's pair caches independently over one chain.
func TestTwoPairsNoClobber(t *testing.T) {
	loc := istLoc(t)
	exp := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	ch, _ := chain.New(chain.Config{Instruments: makeInsts(exp, 22000, 24000, 50), Underlying: "NIFTY", StrikeStep: 50, N: 3, Loc: loc, Logf: func(string, ...any) {}})
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)
	ch.Reconcile(23050, now)
	setQuote(ch, exp, 23050, live.Call, 119, 121, now) // pair A's contract
	setQuote(ch, exp, 23100, live.Put, 200, 205, now)  // pair B's contract

	pa, fa := NewSeamPair(ch, func() time.Time { return now }, 0, nil, "", nil)
	pb, fb := NewSeamPair(ch, func() time.Time { return now }, 0, nil, "", nil)

	pa.Price(live.Call, 0, 23050, now)
	pb.Price(live.Put, 0, 23100, now) // B quotes a different contract after A

	if got := fa.Fill(0, live.Buy); got != 121 {
		t.Errorf("pair A buy = %v, want 121 (its own ask, not clobbered)", got)
	}
	if got := fb.Fill(0, live.Sell); got != 200 {
		t.Errorf("pair B sell = %v, want 200 (its own bid)", got)
	}
}
