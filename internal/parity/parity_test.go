package parity

import (
	"math"
	"testing"

	"github.com/shindeamul76/optedge-live/internal/store"
)

func TestKS_IdenticalAndDisjoint(t *testing.T) {
	same := []float64{1, 2, 3, 4, 5}
	d, p := KS(same, same)
	if d != 0 || p < 0.99 {
		t.Errorf("identical samples: D=%.3f p=%.3f, want D=0 p≈1", d, p)
	}

	a := make([]float64, 60)
	b := make([]float64, 60)
	for i := range a {
		a[i] = float64(i)      // 0..59
		b[i] = float64(i) + 200 // clearly shifted, disjoint
	}
	d2, p2 := KS(a, b)
	if d2 != 1 || p2 > 0.01 {
		t.Errorf("disjoint samples: D=%.3f p=%.3f, want D=1 p≈0", d2, p2)
	}
}

func TestFill_ParityDirection(t *testing.T) {
	// Real net ≥ synth net on every trade ⇒ real fills as good/better ⇒ pass.
	good := []store.AuditRecord{
		{NetRupees: 100, NetSynthRupees: 90},
		{NetRupees: -50, NetSynthRupees: -60},
		{NetRupees: 10, NetSynthRupees: 10},
	}
	fr := Fill(good)
	if !fr.Pass || fr.MedianDivergence < 0 {
		t.Errorf("good fills: %+v, want Pass median≥0", fr)
	}

	// Real systematically worse ⇒ fail.
	bad := []store.AuditRecord{
		{NetRupees: 80, NetSynthRupees: 100},
		{NetRupees: -70, NetSynthRupees: -50},
	}
	if fr := Fill(bad); fr.Pass {
		t.Errorf("bad fills should fail: %+v", fr)
	}
}

func TestGate_MinSampleAndVerdict(t *testing.T) {
	// Under 40 trades → PENDING regardless of anything else.
	few := makeRecs(10, 100, 90)
	if r := Gate(few, DefaultCapital, 7, nil); r.Verdict != "PENDING (n<40)" || r.MinSampleMet {
		t.Errorf("10 trades: %q minMet=%v, want PENDING", r.Verdict, r.MinSampleMet)
	}

	// ≥40 trades, no WF reference → PARTIAL (fill-parity only).
	many := makeRecs(45, 100, 95)
	r := Gate(many, DefaultCapital, 30, nil)
	if r.Verdict != "PARTIAL (no WF reference; fill-parity only)" {
		t.Errorf("no WF ref verdict = %q", r.Verdict)
	}
	if !r.Fill.Pass {
		t.Errorf("expected fill-parity pass, got %+v", r.Fill)
	}

	// ≥40 trades WITH a WF reference the live sample matches → PASS.
	wfReturns := make([]float64, 200)
	for i := range wfReturns {
		wfReturns[i] = math.Sin(float64(i)) * 0.01 // some stable distribution
	}
	live := recsFromReturns(wfReturns[:50], DefaultCapital) // live drawn from same shape
	wf := &WFReference{PerTradeReturns: wfReturns, TradesPerDayP10: 1, TradesPerDayP90: 3}
	r2 := Gate(live, DefaultCapital, 30, wf) // 50 trades / 30 days ≈ 1.67 per day, in [1,3]
	if !r2.KSDone || !r2.FreqDone {
		t.Fatalf("KS/freq not computed: %+v", r2)
	}
	if r2.Verdict != "PASS" {
		t.Errorf("matching sample verdict = %q (KS p=%.3f freqPass=%v fillPass=%v)",
			r2.Verdict, r2.KSPValue, r2.FreqPass, r2.Fill.Pass)
	}
}

func TestGate_ExcludesFallbackTrades(t *testing.T) {
	// 3 clean trades + 2 fallback trades (missing-quote fills). The gate must count
	// only the 3 clean ones and report 2 flagged.
	recs := []store.AuditRecord{
		{NetRupees: 100, NetSynthRupees: 90},
		{NetRupees: 100, NetSynthRupees: 90},
		{NetRupees: 100, NetSynthRupees: 90},
		{NetRupees: 5000, NetSynthRupees: 0, EntryFallback: true}, // bogus 0-fill
		{NetRupees: -5000, NetSynthRupees: 0, ExitFallback: true},
	}
	r := Gate(recs, DefaultCapital, 3, nil)
	if r.NTrades != 3 || r.Flagged != 2 {
		t.Errorf("NTrades/Flagged = %d/%d, want 3/2", r.NTrades, r.Flagged)
	}
	// Fill parity must be computed on the clean trades only (median divergence +10).
	if !r.Fill.Pass || r.Fill.MedianDivergence != 10 {
		t.Errorf("fill parity = %+v, want Pass median 10 (fallbacks excluded)", r.Fill)
	}
}

func makeRecs(n int, real, synth float64) []store.AuditRecord {
	out := make([]store.AuditRecord, n)
	for i := range out {
		out[i] = store.AuditRecord{NetRupees: real, NetSynthRupees: synth}
	}
	return out
}

// recsFromReturns builds audit records whose net = return×capital and whose synth net
// equals real net (zero divergence → fill parity passes) for the outcome-gate test.
func recsFromReturns(returns []float64, capital float64) []store.AuditRecord {
	out := make([]store.AuditRecord, len(returns))
	for i, ret := range returns {
		net := ret * capital
		out[i] = store.AuditRecord{NetRupees: net, NetSynthRupees: net}
	}
	return out
}
