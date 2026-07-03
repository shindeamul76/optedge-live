package recovery

import (
	"strconv"
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge-live/internal/engine"
	"github.com/shindeamul76/optedge-live/internal/feed"
	"github.com/shindeamul76/optedge-live/internal/store"
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

// TestRecover_RestoresOpenPositionAndExitsLive drives an ORB entry in "session 1"
// (persisting bars + position.json), simulates a crash with the position still open,
// then restarts fresh runners, recovers via replay, and confirms the exit produces a
// trade with the RECORDED entry fill (102) and the live exit fill (100).
func TestRecover_RestoresOpenPositionAndExitsLive(t *testing.T) {
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
	ch.Reconcile(23050, now)
	for k := 22900.0; k <= 23200; k += 50 {
		ch.Update(feed.Tick{
			Token: exp.Format("0102") + "-" + strconv.Itoa(int(k)) + "-CE",
			Bids:  [5]feed.DepthLevel{{Price: 100}}, Asks: [5]feed.DepthLevel{{Price: 102}},
			LTP:   101, ExchTime: now,
		})
	}

	p, err := store.NewPaths(t.TempDir(), "2026-06-30")
	if err != nil {
		t.Fatalf("paths: %v", err)
	}

	bar := func(hms string, o, h, l, c float64, v int64) live.Bar {
		ts, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-06-30 "+hms, loc)
		return live.Bar{TS: ts, Open: o, High: h, Low: l, Close: c, Volume: v}
	}
	opening := []live.Bar{
		bar("09:15:00", 23050, 23060, 23040, 23050, 100),
		bar("09:20:00", 23050, 23060, 23040, 23050, 100),
		bar("09:29:00", 23050, 23060, 23040, 23050, 100),
		bar("09:30:00", 23050, 23060, 23040, 23050, 100),
	}
	breakout := bar("09:31:00", 23060, 23120, 23060, 23110, 1000)
	exitBar := bar("09:32:00", 23110, 23200, 23100, 23180, 500)

	// ── Session 1: enter, then "crash" with the position still open. ──
	rec1, err := store.NewRecorder(p, nil)
	if err != nil {
		t.Fatalf("recorder1: %v", err)
	}
	run1 := engine.NewRunners(engine.Config{
		Chain: ch, Loc: loc, Now: func() time.Time { return now },
		OnTrade: rec1.OnTrade, OnFill: rec1.OnFill, Logf: func(string, ...any) {},
	})
	run1.VIX().Set(13.9, now)
	for _, b := range opening {
		rec1.OnBar("FUT", b)
		run1.OnFutureBar(b)
	}
	rec1.OnBar("FUT", breakout)
	run1.OnFutureBar(breakout) // ORB enters here → position.json written
	rec1.Close()

	open, _ := store.OpenPositions(p).Read()
	if _, ok := open["ORB"]; !ok {
		t.Fatalf("expected an open ORB position after session 1, got %+v", open)
	}

	// ── Restart: fresh runners + recorder, recover from the log, then go live. ──
	rec2, err := store.NewRecorder(p, nil)
	if err != nil {
		t.Fatalf("recorder2: %v", err)
	}
	run2 := engine.NewRunners(engine.Config{
		Chain: ch, Loc: loc, Now: func() time.Time { return now },
		OnTrade: rec2.OnTrade, OnFill: rec2.OnFill, Logf: func(string, ...any) {},
	})
	run2.VIX().Set(13.9, now)

	res, err := Recover(run2, rec2, p, "FUT", "VIX")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Both runners may have entered on the breakout; at least the ORB position is open.
	if res.OpenPositions < 1 || res.BarsReplayed != len(opening)+1 {
		t.Fatalf("recover result = %+v, want >=1 open, %d bars", res, len(opening)+1)
	}

	// Live exit: the restored position closes on the target bar, filling live.
	rec2.OnBar("FUT", exitBar)
	run2.OnFutureBar(exitBar)
	rec2.Close()

	recs, err := store.ReadAudit(p)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var orb *store.AuditRecord
	for i := range recs {
		if recs[i].Runner == "ORB" {
			orb = &recs[i]
			break
		}
	}
	if orb == nil {
		t.Fatalf("no ORB trade after recovery+exit (audit: %+v)", recs)
	}
	if orb.EntryFill != 102 {
		t.Errorf("EntryFill = %.0f, want 102 (recorded entry restored via replay)", orb.EntryFill)
	}
	if orb.ExitFill != 100 {
		t.Errorf("ExitFill = %.0f, want 100 (live bid at exit)", orb.ExitFill)
	}
	// The ORB position is cleared after its exit (VWAP may still be open — it need not
	// hit its exit on the same bar).
	if open2, _ := store.OpenPositions(p).Read(); func() bool { _, ok := open2["ORB"]; return ok }() {
		t.Errorf("ORB still open after its exit; positions=%+v", open2)
	}
}
