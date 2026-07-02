# F. Live IV source

**Goal of this section:** feed the engine a live implied-volatility number per bar, through a
seam that's already in place — and understand the one feed risk attached to it.

## What needs IV, and why

Two consumers need a per-bar sigma (`σ = VIX / 100`):

1. **The delta gate** — `strike.Pick` uses σ to compute the option delta so it can reject a
   strike whose delta < 0.30 (`orb.go:171,184`). Without σ it can't pick a strike.
2. **The synthetic shadow** (D/H) — `SyntheticPricer` prices Black-76 from spot + σ. The
   shadow's whole job is to be the divergence baseline, so its σ must be the *real* live VIX,
   not a guess.

## The seam (already done in v1.1.0)

```go
type IVSource interface { At(t time.Time) float64 }   // optedge/internal/strategy/iv.go:15
```

**Why this was "the one blocking gap."** Originally `NewRunner` took a concrete `*IVSeries`
fixed at construction — a historical lookup table that **can't be fed live VIX mid-session.**
The fix widened it to the tiny `IVSource` interface and threaded it through `NewRunner`,
`Run`, and `SyntheticPricer`. `*IVSeries` already satisfies the interface, so **the backtest
is unchanged** (re-verified: identical +₹112,303). Live plugs in a `LiveVIX` instead. This
is the change that justified re-tagging **v1.1.0** before any live work. ✅ Done.

## What `LiveVIX` does

`LiveVIX` is the live `IVSource`. It holds the latest India-VIX values from the VIX
aggregator (B) and answers `At(t)` with the **last value at or before `t`** — the exact
carry-forward semantics of `IVSeries.At` (`iv.go:51`). So the seam behaves identically; only
the data source differs.

Two unit details to copy from `IVSeries` so live matches backtest:

- **VIX is an annualized percentage** (e.g. 14.84). `σ = VIX / 100 = 0.1484`. `LiveVIX` must
  divide by 100, just as `NewIVSeries` does (`b.Close/100.0`, `iv.go:43`).
- **Clamp positive** — `IVSeries` floors σ at `1e-4`; `LiveVIX` should too, so a zero/garbage
  VIX tick can't produce a nonsensical price.

## Where the VIX data comes from — the open risk

India VIX is an **index**, not an F&O instrument. Two open questions, both to settle when we
capture a live session:

1. **Does India VIX stream on SmartWebSocketV2?** It would be exchangeType **1 (NSE_CM)** with
   the token from `FindIndiaVIX`. If it streams, the VIX aggregator (B) builds 1-min VIX bars
   → `LiveVIX`, identical pipeline to the future.
2. **If it does *not* stream** (some index feeds are restricted), the fallback is **REST**:
   poll the quote/LTP endpoint ~once a minute (reusing the optedge SmartAPI client pattern),
   or in the worst case carry forward the previous close. Lower resolution, but acceptable —
   see the next point on why.

## Why an imperfect σ barely moves decisions (but still must be real)

The delta gate operates **at/near ATM**, where the option's delta is least sensitive to a
1-vol-point error in σ. So even a slightly stale or coarse VIX changes the strike pick almost
never. That's the good news — a once-a-minute REST VIX would be fine for *decisions*.

But two reasons it must still be a **real, live source**, not a hardcoded constant:

- **Parity demands it.** The frozen book used the real India VIX series; a constant would be
  a different system and would (rightly) reset the freeze clock.
- **The shadow is sensitive.** `SyntheticPricer`'s Black-76 *price* moves materially with σ,
  and the shadow is the divergence baseline. A wrong σ would bias the real-vs-synthetic
  comparison — corrupting the very metric v3 exists to produce.

## Parity note on the σ adjustment

`NewIVSeriesAdjusted` exists (`iv.go:36`) to apply the −1 vol-point correction that
`cmd/pricecheck` found (India VIX runs ~1 vol point above weekly ATM IV). **For live, use raw
VIX (adjust = 0)** to match the locked config, which used raw VIX. If you want the corrected
number for analysis, *log* it alongside — don't feed it to the live delta gate, or you've
changed the frozen book.

## Staleness

`At(t)` returns the last value ≤ t, so a stalled VIX feed silently returns an old number.
Track the VIX quote's age and **log a staleness warning** when it exceeds a threshold, same as
the chain quotes (C). Decisions tolerate it; the shadow shouldn't quietly drift on a frozen
σ.

---

**Mental model:** the `IVSource` interface is the already-built seam; `LiveVIX` is just a
carry-forward lookup over live India-VIX values (÷100, clamped positive) instead of a
historical table. Decisions barely care about σ accuracy (delta gate is at ATM), but parity
and the synthetic shadow demand a *real* live source — so the open work is purely "can we get
live VIX off the WS, or must we REST-poll it?"
