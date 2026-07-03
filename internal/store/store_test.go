package store

import (
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/engine"
	"github.com/shindeamul76/optedge/live"
)

func paths(t *testing.T) Paths {
	t.Helper()
	p, err := NewPaths(t.TempDir(), "2026-06-30")
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	return p
}

func TestRecorder_EntryPositionThenAudit(t *testing.T) {
	p := paths(t)
	r, err := NewRecorder(p, nil)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	ts := time.Date(2026, 6, 30, 9, 31, 0, 0, time.UTC)
	r.OnBar("FUT", live.Bar{TS: ts, Open: 23050, High: 23120, Low: 23040, Close: 23100, Volume: 1000})

	// Entry: buy leg → position.json written.
	r.OnFill(engine.FillLeg{
		Runner: "ORB", Side: live.Buy, Typ: live.Call, Strike: 23050, Expiry: "2026-07-02",
		Bid: 100, Ask: 102, Mid: 101, Fill: 102, TS: ts,
	})
	open, err := OpenPositions(p).Read()
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	pos, ok := open["ORB"]
	if !ok {
		t.Fatal("ORB position not persisted on entry")
	}
	if pos.Strike != 23050 || pos.EntryFill != 102 || pos.EntryAsk != 102 || pos.Dir != string(live.Long) {
		t.Errorf("position = %+v, want strike 23050 fill 102 ask 102 LONG", pos)
	}

	// Exit leg + completed trade → audit line, position cleared.
	r.OnFill(engine.FillLeg{
		Runner: "ORB", Side: live.Sell, Typ: live.Call, Strike: 23050,
		Bid: 100, Ask: 102, Mid: 101, Fill: 100, TS: ts.Add(time.Minute),
	})
	r.OnTrade(engine.TradeEvent{Runner: "ORB", Trade: live.Trade{
		Day: "2026-06-30", Dir: live.Long, OptType: live.Call, Strike: 23050, Qty: 75,
		EntryTS: ts, ExitTS: ts.Add(time.Minute), EntrySpot: 23060, ExitSpot: 23090,
		EntryPrem: 102, ExitPrem: 100,
	}})

	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Audit captured the trade + correlated leg quotes.
	recs, err := ReadAudit(p)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(recs))
	}
	got := recs[0]
	if got.Runner != "ORB" || got.Strike != 23050 || got.Qty != 75 {
		t.Errorf("audit id = %+v", got)
	}
	if got.EntryFill != 102 || got.ExitFill != 100 {
		t.Errorf("audit fills entry/exit = %.0f/%.0f, want 102/100", got.EntryFill, got.ExitFill)
	}
	if got.EntryAsk != 102 || got.ExitBid != 100 || got.Expiry != "2026-07-02" {
		t.Errorf("audit legs entryAsk/exitBid/expiry = %.0f/%.0f/%s", got.EntryAsk, got.ExitBid, got.Expiry)
	}

	// Position cleared after completion.
	open2, _ := OpenPositions(p).Read()
	if len(open2) != 0 {
		t.Errorf("open positions after exit = %d, want 0", len(open2))
	}

	// Bar log captured the bar.
	bars, err := ReadBars(p)
	if err != nil {
		t.Fatalf("read bars: %v", err)
	}
	if len(bars) != 1 || bars[0].Token != "FUT" || bars[0].Close != 23100 {
		t.Errorf("bars = %+v, want 1 FUT close 23100", bars)
	}
}

func TestRecorder_MutedWritesNothing(t *testing.T) {
	p := paths(t)
	r, err := NewRecorder(p, nil)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	r.Mute()

	ts := time.Now()
	r.OnBar("FUT", live.Bar{TS: ts, Close: 23100})
	r.OnFill(engine.FillLeg{Runner: "ORB", Side: live.Buy, Typ: live.Call, Strike: 23050, Fill: 102, TS: ts})
	r.OnTrade(engine.TradeEvent{Runner: "ORB", Trade: live.Trade{Day: "2026-06-30", EntryPrem: 102, ExitPrem: 100}})
	r.Close()

	if recs, _ := ReadAudit(p); len(recs) != 0 {
		t.Errorf("muted audit = %d, want 0", len(recs))
	}
	if bars, _ := ReadBars(p); len(bars) != 0 {
		t.Errorf("muted bars = %d, want 0", len(bars))
	}
	if open, _ := OpenPositions(p).Read(); len(open) != 0 {
		t.Errorf("muted positions = %d, want 0", len(open))
	}
}

func TestPositions_EmptyWhenAbsent(t *testing.T) {
	p := paths(t)
	open, err := OpenPositions(p).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("absent positions = %d, want 0", len(open))
	}
}
