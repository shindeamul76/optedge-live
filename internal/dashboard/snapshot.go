// Package dashboard is the minimal web view of the incubation: a single embedded
// page polling a /api/state JSON snapshot (equity vs the gate, per-trade divergence,
// connection state). Zero build step — the HTML is compiled into the binary via
// go:embed and served in-process from cmd/live, so it reads live in-memory state
// (connection, chain center) alongside the persisted audit record. See H.md.
package dashboard

import (
	"time"

	"github.com/shindeamul76/optedge-live/internal/parity"
	"github.com/shindeamul76/optedge-live/internal/store"
)

// TradeRow is one completed trade for the dashboard table.
type TradeRow struct {
	Runner     string  `json:"runner"`
	Dir        string  `json:"dir"`
	OptType    string  `json:"opt_type"`
	Strike     float64 `json:"strike"`
	EntryTS    string  `json:"entry_ts"`
	ExitTS     string  `json:"exit_ts"`
	EntryFill  float64 `json:"entry_fill"`
	ExitFill   float64 `json:"exit_fill"`
	NetRupees  float64 `json:"net_rupees"`
	Divergence float64 `json:"divergence"` // real − synthetic net
	Reason     string  `json:"reason"`
}

// Snapshot is the full dashboard state serialized to /api/state.
type Snapshot struct {
	Updated     string        `json:"updated"`
	ConnState   string        `json:"conn_state"`
	ChainCenter float64       `json:"chain_center"`
	Capital     float64       `json:"capital"`
	Equity      float64       `json:"equity"`
	EquityCurve []float64     `json:"equity_curve"` // capital, then equity after each trade
	Days        int           `json:"days"`
	Trades      []TradeRow    `json:"trades"`
	Gate        parity.Report `json:"gate"`

	FreezeOK    bool   `json:"freeze_ok"`    // false if engine/config drifted from the baseline
	FreezeLabel string `json:"freeze_label"` // e.g. "frozen v1.2.0 · a1b2… since 2026-06-30" or a violation notice
}

// BuildSnapshot assembles the snapshot from the cumulative audit under root plus the
// live in-memory bits (connection state, chain center). WF reference may be nil.
func BuildSnapshot(root string, capital float64, wf *parity.WFReference, connState string, chainCenter float64) Snapshot {
	if capital <= 0 {
		capital = parity.DefaultCapital
	}
	recs, days, _ := store.ReadAllAudit(root)
	gate := parity.Gate(recs, capital, days, wf)

	curve := make([]float64, 0, len(recs)+1)
	curve = append(curve, capital)
	rows := make([]TradeRow, 0, len(recs))
	var cum float64
	for _, r := range recs {
		cum += r.NetRupees
		curve = append(curve, capital+cum)
		rows = append(rows, TradeRow{
			Runner: r.Runner, Dir: r.Dir, OptType: r.OptType, Strike: r.Strike,
			EntryTS: fmtTS(r.EntryTS), ExitTS: fmtTS(r.ExitTS),
			EntryFill: r.EntryFill, ExitFill: r.ExitFill, NetRupees: r.NetRupees,
			Divergence: r.NetRupees - r.NetSynthRupees, Reason: r.Reason,
		})
	}

	return Snapshot{
		Updated: time.Now().Format("15:04:05"), ConnState: connState, ChainCenter: chainCenter,
		Capital: capital, Equity: capital + cum, EquityCurve: curve, Days: days,
		Trades: rows, Gate: gate,
	}
}

func fmtTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}
