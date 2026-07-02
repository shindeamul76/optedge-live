package aggregator

import (
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/feed"
)

func istLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	return loc
}

// tk builds a tick at HH:MM:SS on 2026-06-30 IST with the given LTP and cumulative
// day-volume.
func tk(loc *time.Location, hms string, ltp float64, vol int64) feed.Tick {
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-06-30 "+hms, loc)
	if err != nil {
		panic(err)
	}
	return feed.Tick{Token: "FUT", ExchTime: ts, LTP: ltp, Volume: vol}
}

func at(loc *time.Location, hms string) time.Time {
	ts, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-06-30 "+hms, loc)
	return ts
}

func TestAggregator_FoldAndCloseOnTimer(t *testing.T) {
	loc := istLoc(t)
	a := New(loc)

	for _, tick := range []feed.Tick{
		tk(loc, "09:15:05", 100, 1000),
		tk(loc, "09:15:20", 105, 1010),
		tk(loc, "09:15:40", 95, 1025),
		tk(loc, "09:15:59", 102, 1040),
	} {
		if _, closed := a.Add(tick); closed {
			t.Fatal("no bar should close mid-minute")
		}
	}

	// Not yet due at 09:15:30.
	if _, closed := a.CloseDue(at(loc, "09:15:30")); closed {
		t.Fatal("bar closed before M+1:00")
	}
	// Due at 09:16:00.
	bar, closed := a.CloseDue(at(loc, "09:16:00"))
	if !closed {
		t.Fatal("bar did not close at M+1:00")
	}
	if !bar.TS.Equal(at(loc, "09:15:00")) {
		t.Errorf("TS = %s, want 09:15:00", bar.TS)
	}
	if bar.Open != 100 || bar.High != 105 || bar.Low != 95 || bar.Close != 102 {
		t.Errorf("OHLC = %.0f/%.0f/%.0f/%.0f, want 100/105/95/102", bar.Open, bar.High, bar.Low, bar.Close)
	}
	if bar.Volume != 0 {
		t.Errorf("first bar Volume = %d, want 0 (no prior baseline)", bar.Volume)
	}
}

func TestAggregator_VolumeDelta(t *testing.T) {
	loc := istLoc(t)
	a := New(loc)
	a.Add(tk(loc, "09:15:05", 100, 1000))
	a.Add(tk(loc, "09:15:59", 102, 1040))
	a.CloseDue(at(loc, "09:16:00")) // first bar closes; baseline = 1040

	a.Add(tk(loc, "09:16:10", 103, 1050))
	a.Add(tk(loc, "09:16:50", 104, 1075))
	bar, closed := a.CloseDue(at(loc, "09:17:00"))
	if !closed {
		t.Fatal("second bar did not close")
	}
	if bar.Volume != 35 { // 1075 - 1040
		t.Errorf("Volume = %d, want 35", bar.Volume)
	}
}

func TestAggregator_NewMinuteTickClosesPrior(t *testing.T) {
	loc := istLoc(t)
	a := New(loc)
	a.Add(tk(loc, "09:15:05", 100, 1000))
	a.Add(tk(loc, "09:15:40", 108, 1030))

	// A tick for 09:16 arrives before the timer — the 09:15 bar closes on time.
	bar, closed := a.Add(tk(loc, "09:16:02", 109, 1035))
	if !closed {
		t.Fatal("prior minute did not close on new-minute tick")
	}
	if !bar.TS.Equal(at(loc, "09:15:00")) || bar.Close != 108 {
		t.Errorf("closed bar = TS %s close %.0f, want 09:15:00 close 108", bar.TS, bar.Close)
	}
	// The 09:16 minute is now forming; closing it yields the new tick's values.
	next, closed := a.CloseDue(at(loc, "09:17:00"))
	if !closed || !next.TS.Equal(at(loc, "09:16:00")) || next.Open != 109 {
		t.Errorf("next bar = TS %s open %.0f, want 09:16:00 open 109", next.TS, next.Open)
	}
}

func TestAggregator_DropsLateTick(t *testing.T) {
	loc := istLoc(t)
	a := New(loc)
	a.Add(tk(loc, "09:15:05", 100, 1000))
	a.CloseDue(at(loc, "09:16:00")) // 09:15 closed

	// A straggler for the closed 09:15 minute must be dropped, not reopen it.
	if _, closed := a.Add(tk(loc, "09:15:30", 999, 1001)); closed {
		t.Fatal("late tick should not close a bar")
	}
	if dropped, _ := a.Stats(); dropped != 1 {
		t.Errorf("droppedLate = %d, want 1", dropped)
	}
	// And it must not have started a new 09:15 partial.
	if _, closed := a.CloseDue(at(loc, "09:16:30")); closed {
		t.Fatal("late tick wrongly created a new bar")
	}
}

func TestAggregator_DeadMinuteSkipped(t *testing.T) {
	loc := istLoc(t)
	a := New(loc)
	a.Add(tk(loc, "09:15:05", 100, 1000))
	a.Add(tk(loc, "09:15:59", 102, 1040))
	a.CloseDue(at(loc, "09:16:00")) // 09:15 closes, baseline 1040

	// No ticks in 09:16 — the minute simply produces no bar.
	if _, closed := a.CloseDue(at(loc, "09:17:00")); closed {
		t.Fatal("dead minute should not emit a bar")
	}

	// 09:17 has a tick; its volume carries the baseline across the skipped minute.
	a.Add(tk(loc, "09:17:05", 106, 1100))
	bar, closed := a.CloseDue(at(loc, "09:18:00"))
	if !closed || !bar.TS.Equal(at(loc, "09:17:00")) {
		t.Fatalf("expected 09:17 bar, got closed=%v TS=%s", closed, bar.TS)
	}
	if bar.Volume != 60 { // 1100 - 1040, spanning the empty 09:16
		t.Errorf("Volume = %d, want 60", bar.Volume)
	}
}
