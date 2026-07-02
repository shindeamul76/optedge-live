# H. Parity engine + dashboard

**Goal of this section:** turn the live record into the actual verdict — is the live system
statistically indistinguishable from the walk-forward backtest? — and show it on a page.

## The shadow, restated

From D/E you run **four runners**: two with real seams (the live record), two with synthetic
seams (the shadow). They fire the **same trades** (decisions ignore fills), so each live trade
has a synthetic twin. The per-trade **divergence** is `real − synthetic`, both at the leg
level (fill prices) and the trade level (net P&L). This is the continuous, forward version of
`cmd/pricecheck` — the static check that already passed.

## The gate — four concrete tests

**1. Fill parity (the headline question).** Compare the **real round-trip spread** you paid
to the **modeled spread** the backtest charged. Real round-trip spread per trade ≈
`(entry_ask − entry_bid)/2 + (exit_ask − exit_bid)/2` (what the bid/ask cost you), versus the
`SpreadModel` half-spreads the backtest assumed (`fills.go`). Gate: **median real spread ≤
modeled spread** ⇒ pass (real fills are as good as, or better than, the backtest assumed).
Flag if real fills are *systematically worse* — that's the abandon signal (synthetic pricing
was too kind).

**2. Outcome parity.** Two-sample **KS test** on live per-trade returns vs the **stored
walk-forward per-trade return distribution** ⇒ **p > 0.05** (can't reject "same
distribution"). Plus: live equity stays **within the Monte-Carlo p5–p95 band**.

- *KS in one line*: it compares the two empirical CDFs and returns a `D` statistic + p-value;
  a high p means the live sample is consistent with being drawn from the WF distribution.
- *Small-sample caveat*: at 40 trades KS has low power, so `p > 0.05` means "**not
  contradicted**," not "proven identical." That's why the min-sample gate exists and why
  fill-parity (test 1) carries the real weight.

**3. Frequency parity.** Live **trades/day within [WF p10, WF p90]**. Too few ⇒ the live feed
is missing signals; too many ⇒ something fires that the backtest wouldn't (a parity bug).

**4. Min-sample gate.** **No pass/fail verdict until ≥ 40 live trades** (~4–6 weeks at
~1.5/day). The full 3–6-month freeze yields ~120–270 trades.

## Two refinements to lock before building

- **Export the WF reference sample at freeze time.** The KS test needs the stored
  walk-forward per-trade returns as a fixture. Export it once from `evaluate` (the
  walk-forward / Monte-Carlo machinery) and ship it inside optedge-live, frozen alongside the
  engine version. Without this fixture there's nothing to KS *against*.
- **Define the "return" unit precisely, the same way on both sides.** Use **% of
  capital-at-risk per trade** (scale-free, and it lines up with how the MC bands are built),
  not raw ₹. Live and WF must use the identical definition or the KS test compares apples to
  oranges.

## The Monte-Carlo bands

The p5–p95 envelope comes from the backtest's Monte-Carlo (`evaluate/montecarlo.go`):
resample the trade sequence many times → a distribution of equity paths → percentile bands.
Live equity plotted against that envelope is the single most intuitive parity check — you
should not be able to see the WF→live boundary, and the live curve should stay inside the
band.

## Dashboard — minimal web (the locked decision)

**Stack (zero build step):** Go `net/http` + `go:embed` of a single `index.html`, a
`/api/state` JSON endpoint polled every ~2 s (or Server-Sent Events for push), charts via
**uPlot** or **Chart.js** from a CDN. Single binary, no npm, no framework. The page is a
**read-only view** over the persisted artifacts (`audit.jsonl`, `session_summary.jsonl`) plus
the live in-memory connection state.

**Panels:**

- **Connection state** — the `connState` from A (connected / reconnecting / re-auth /
  market-closed) + last heartbeat.
- **Today's trades** — entries/exits, real fills, reasons, open position if any.
- **Equity vs MC bands** — live equity curve overlaid on the WF p5–p95 envelope.
- **Divergence** — per-trade `real − synthetic` and the cumulative drift.
- **Frequency** — live trades/day vs the [WF p10, p90] band.
- **Gate progress** — `n / 40` toward the min-sample gate, plus current KS p-value and
  fill-parity median once n ≥ 40.

**Why web over terminal:** it's attended and local, but a browser gives proper charts (the
equity-vs-bands overlay especially) that a terminal can't, while staying zero-infra via
`go:embed`.

---

**Mental model:** the synthetic shadow gives every live trade a twin, so divergence is
measured per trade; the verdict is four tests — fill spread ≤ modeled (the one that matters
most), KS p>0.05 + equity in the MC band, trades/day in band, and no verdict before 40 trades;
lock the WF reference sample and the return unit up front; render it all on a single embedded
web page that just reads the jsonl logs.
