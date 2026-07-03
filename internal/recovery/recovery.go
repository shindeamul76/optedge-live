// Package recovery implements replay-to-resume: on a mid-session restart it rebuilds
// the frozen Runners' per-session state (opening range, VWAP, filter buffer,
// tradedToday) — and any open position — by replaying today's bar log through fresh
// runners, serving RECORDED fills so reconstructed trades match the originals. Only
// after that does the caller go live. See lld-explained/E.md (why resume == replay)
// and G.md (the persisted artifacts).
package recovery

import (
	"github.com/shindeamul76/optedge-live/internal/engine"
	"github.com/shindeamul76/optedge-live/internal/store"
	"github.com/shindeamul76/optedge/live"
)

// Result summarizes what recovery reconstructed.
type Result struct {
	BarsReplayed   int
	OpenPositions  int
	CompletedToday int
}

// Recover replays today's persisted bars through the given runners to restore state.
// The recorder MUST be muted by the caller around this call (or pass it and let
// Recover mute/unmute) so the reconstructed entries/exits aren't re-persisted.
//
// futureToken/vixToken identify how logged bars route. Returns a zero Result (no-op)
// when there's no bar log yet (a fresh start).
func Recover(runners *engine.Runners, rec *store.Recorder, p store.Paths, futureToken, vixToken string) (Result, error) {
	bars, err := store.ReadBars(p)
	if err != nil {
		return Result{}, err
	}
	if len(bars) == 0 {
		return Result{}, nil // fresh start, nothing to replay
	}
	audit, err := store.ReadAudit(p)
	if err != nil {
		return Result{}, err
	}
	open, err := store.OpenPositions(p).Read()
	if err != nil {
		return Result{}, err
	}

	runners.LoadReplay(replayQueue("ORB", audit, open), replayQueue("VWAP", audit, open))

	rec.Mute()
	for _, br := range bars {
		bar := live.Bar{TS: br.TS, Open: br.Open, High: br.High, Low: br.Low, Close: br.Close, Volume: br.Volume}
		switch br.Token {
		case futureToken:
			runners.OnFutureBar(bar)
		case vixToken:
			runners.OnVIXBar(bar)
		}
	}
	rec.Unmute()

	return Result{BarsReplayed: len(bars), OpenPositions: len(open), CompletedToday: countRunner(audit)}, nil
}

// replayQueue builds a runner's recorded-fill queue: [entry, exit] for a trade already
// completed today, [entry] for an open position. Because the book takes at most one
// trade per day per runner, at most one of these applies.
func replayQueue(runner string, audit []store.AuditRecord, open map[string]store.Position) []float64 {
	var q []float64
	for _, a := range audit {
		if a.Runner == runner {
			q = append(q, a.EntryFill, a.ExitFill)
		}
	}
	if p, ok := open[runner]; ok {
		q = append(q, p.EntryFill)
	}
	return q
}

func countRunner(audit []store.AuditRecord) int { return len(audit) }
