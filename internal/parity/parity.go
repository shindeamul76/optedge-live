// Package parity turns the incubation record (store.AuditRecord) into the exit-gate
// verdict: per-trade real-vs-synthetic fill divergence, a two-sample KS test of live
// vs walk-forward returns, a trade-frequency check, and the ≥40-trade min-sample gate.
// See lld-explained/H.md.
package parity

import (
	"encoding/json"
	"math"
	"os"
	"sort"

	"github.com/shindeamul76/optedge-live/internal/store"
)

// MinSample is the number of live trades required before any pass/fail verdict.
const MinSample = 40

// DefaultCapital is the book's capital (₹), used to express per-trade returns as a
// scale-free fraction of capital-at-risk (matches how the MC bands are built).
const DefaultCapital = 50000.0

// Divergences returns real − synthetic net P&L (₹) per trade — the fill divergence.
// Positive means real fills were better than the model assumed.
func Divergences(recs []store.AuditRecord) []float64 {
	out := make([]float64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.NetRupees-r.NetSynthRupees)
	}
	return out
}

// PerTradeReturns expresses each trade's net P&L as a fraction of capital.
func PerTradeReturns(recs []store.AuditRecord, capital float64) []float64 {
	if capital <= 0 {
		capital = DefaultCapital
	}
	out := make([]float64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.NetRupees/capital)
	}
	return out
}

// FillReport is the fill-parity result: real round-trip fills should be no worse than
// the modeled ones, i.e. the median real−synth divergence ≥ 0.
type FillReport struct {
	N                int
	MedianDivergence float64
	Pass             bool
}

// Fill computes the fill-parity report.
func Fill(recs []store.AuditRecord) FillReport {
	div := Divergences(recs)
	m := median(div)
	return FillReport{N: len(div), MedianDivergence: m, Pass: len(div) > 0 && m >= 0}
}

// WFReference is the frozen walk-forward baseline, exported once from optedge/evaluate
// at freeze time and shipped as a fixture. Nil until provided → outcome/frequency
// gates are deferred (fill-parity still runs).
type WFReference struct {
	PerTradeReturns []float64 `json:"per_trade_returns"` // same unit as PerTradeReturns (fraction of capital)
	TradesPerDayP10 float64   `json:"trades_per_day_p10"`
	TradesPerDayP90 float64   `json:"trades_per_day_p90"`
}

// LoadWFReference reads the WF fixture, returning (nil, nil) if the file is absent.
func LoadWFReference(path string) (*WFReference, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wf WFReference
	if err := json.Unmarshal(b, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// valid drops trades that filled on a fallback (missing real book price) — they don't
// reflect real fills and must not enter the gate statistics.
func valid(recs []store.AuditRecord) []store.AuditRecord {
	out := recs[:0:0]
	for _, r := range recs {
		if r.EntryFallback || r.ExitFallback {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Report is the full gate verdict.
type Report struct {
	NTrades      int
	Flagged      int // trades excluded for filling on a fallback (missing quote)
	MinSampleMet bool

	Fill FillReport

	KSDone      bool
	KSStat      float64
	KSPValue    float64
	OutcomePass bool // KS p > 0.05 ⇒ can't reject "same distribution"

	FreqDone     bool
	TradesPerDay float64
	FreqPass     bool

	Verdict string // PENDING (n<40) | PARTIAL (no WF ref) | PASS | FAIL
}

// Gate computes the exit-gate report from the audit records, optional WF reference,
// and the number of trading days observed (for the frequency metric).
func Gate(all []store.AuditRecord, capital float64, days int, wf *WFReference) Report {
	recs := valid(all)
	r := Report{
		NTrades:      len(recs),
		Flagged:      len(all) - len(recs),
		MinSampleMet: len(recs) >= MinSample,
		Fill:         Fill(recs),
	}

	if wf != nil {
		if len(wf.PerTradeReturns) > 0 && len(recs) > 0 {
			d, p := KS(PerTradeReturns(recs, capital), wf.PerTradeReturns)
			r.KSDone, r.KSStat, r.KSPValue, r.OutcomePass = true, d, p, p > 0.05
		}
		if days > 0 {
			r.FreqDone = true
			r.TradesPerDay = float64(len(recs)) / float64(days)
			r.FreqPass = r.TradesPerDay >= wf.TradesPerDayP10 && r.TradesPerDay <= wf.TradesPerDayP90
		}
	}

	r.Verdict = verdict(r, wf)
	return r
}

func verdict(r Report, wf *WFReference) string {
	if !r.MinSampleMet {
		return "PENDING (n<40)"
	}
	if wf == nil {
		return "PARTIAL (no WF reference; fill-parity only)"
	}
	if r.Fill.Pass && r.OutcomePass && r.FreqPass {
		return "PASS"
	}
	return "FAIL"
}

// KS runs the two-sample Kolmogorov–Smirnov test, returning the D statistic and an
// asymptotic p-value (Numerical Recipes kstwo). A high p means the two samples are
// consistent with the same distribution.
func KS(a, b []float64) (d, p float64) {
	x := append([]float64(nil), a...)
	y := append([]float64(nil), b...)
	sort.Float64s(x)
	sort.Float64s(y)
	n1, n2 := len(x), len(y)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}
	var i, j int
	var fn1, fn2 float64
	for i < n1 && j < n2 {
		d1, d2 := x[i], y[j]
		if d1 <= d2 {
			for i < n1 && x[i] == d1 {
				i++
			}
			fn1 = float64(i) / float64(n1)
		}
		if d2 <= d1 {
			for j < n2 && y[j] == d2 {
				j++
			}
			fn2 = float64(j) / float64(n2)
		}
		if dt := math.Abs(fn2 - fn1); dt > d {
			d = dt
		}
	}
	en := math.Sqrt(float64(n1*n2) / float64(n1+n2))
	p = ksProb((en + 0.12 + 0.11/en) * d)
	return d, p
}

// ksProb is the Kolmogorov distribution Q_ks(λ) = 2Σ(-1)^{j-1} e^{-2j²λ²}.
func ksProb(lam float64) float64 {
	const eps1, eps2 = 0.001, 1e-8
	a2 := -2.0 * lam * lam
	fac, sum, termbf := 2.0, 0.0, 0.0
	for j := 1; j <= 100; j++ {
		term := fac * math.Exp(a2*float64(j)*float64(j))
		sum += term
		if math.Abs(term) <= eps1*termbf || math.Abs(term) <= eps2*sum {
			return clamp01(sum)
		}
		fac = -fac
		termbf = math.Abs(term)
	}
	return 1.0 // failed to converge → treat as "same"
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
