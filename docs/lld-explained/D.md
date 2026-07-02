# D. RealPricer + RealFill (the seam plug-ins)

**Goal of this section:** give the two swappable interfaces their live bodies — and explain
the cleverest (and most fragile-looking) trick in the whole design.

## Recap: the two seams and their backtest bodies

```
OptionPricer.Price(typ, spot, strike, ts) float64   // returns the option's MID
FillModel.Fill(mid, side) float64                    // turns a MID into an executed price
```

- Backtest `SyntheticPricer.Price` → Black-76 from `spot` + VIX
  (`optedge/internal/strategy/pricer.go:42`).
- Backtest `SpreadModel.Fill` → `mid ± modeled half-spread`
  (`optedge/internal/costs/fills.go:32`).

Live swaps both for implementations that read the real order book from the C quote map.
**Neither swap can change which trades fire** — the Runner's decisions never call these
(signal/strike/stop/target are all on the underlying+clock). They only affect the fill P&L.
That's the parity guarantee the whole project rests on.

## RealPricer — why it can ignore `spot`

`RealPricer.Price(typ, spot, strike, ts)` **ignores `spot` entirely.** In the backtest,
`spot` is essential — Black-76 *computes* the option value from the underlying. But the live
order book has already done that: the real bid/ask *is* the market's valuation given the
current spot. So RealPricer just:

1. resolves `(typ, strike, currentWeekly)` → token (it holds the Chain + current expiry),
2. reads `quote[token]` from the C map,
3. returns `mid = (bid + ask) / 2` (fallback to LTP if a book side is empty).

The existing signature is **sufficient** — `typ`/`strike` come in as args, the expiry comes
from RealPricer's own state, the quote from the shared Chain. No interface change for the
pricer.

## RealFill — the signature problem, and the trick

`Fill(mid, side)` is where it gets interesting. Fill receives **only a number (`mid`) and the
side** — *not* the contract. For `SpreadModel` that's fine; it just adds a modeled spread
around whatever `mid` it's handed. But `RealFill` needs the **actual bid/ask of the specific
contract**, and the contract identity isn't in its arguments. It has a price but doesn't know
*which option* that price belongs to.

**The resolution — no engine change.** The key observation, true by construction: **`Price`
and `Fill` are always called on the same contract, back-to-back, within one `OnBar`.** The
two call sites:

- entry — `orb.go:191-192`: `entryPrice := pricer.Price(...)` immediately followed by
  `entryFill := fill.Fill(entryPrice, Buy)`
- exit — `orb.go:224-225`: `exitPrice := pricer.Price(...)` immediately followed by
  `exitFill := fill.Fill(exitPrice, Sell)`

So: when `RealPricer.Price` runs, it **caches the bid/ask of the contract it just looked up**
(the "last-quoted contract"). Then `RealFill.Fill` ignores its `mid` argument and **reads
that cached bid/ask** → ask for Buy, bid for Sell. Because `Price` *always* runs immediately
before `Fill`, the cache is guaranteed fresh and correct. The two paired objects communicate
through shared state under a guaranteed call order.

**Why not just change the interface?** The alternative is widening `FillModel` to
`Fill(typ, strike, mid, side)`. But that ripples through `costs.SpreadModel` and every
caller, churns the **frozen** engine, forces a re-tag + full re-verification, and enlarges
the freeze surface for zero behavioral gain. The cache trick keeps the engine byte-identical.
(LLD: "widen FillModel… rejected.")

## The per-runner caching caveat

The subtlety the LLD's single-cache description glosses over. We run **two runners**
(ORB + VWAP). Suppose they **shared one RealPricer** with a single "last contract" field.

In strictly sequential, single-threaded driving it's actually safe: the driver calls
`runnerA.OnBar` (Price→cache=A→Fill reads A, complete) *then* `runnerB.OnBar`
(Price→cache=B→Fill reads B, complete). Each Price→Fill pair finishes before the next
`OnBar` starts, so nothing clobbers.

But it's **fragile** — it silently breaks the day someone parallelizes the runners, or any
path calls `Price` twice before `Fill`. The robust design removes the hazard structurally:

- **One shared `Chain`** (the quote map) — read-only from the runners, written by the feed.
- **Each runner gets its own `{RealPricer, RealFill}` pair**, and the **"last contract"
  cache lives inside that pair.** Both pairs point at the same Chain, but each has its own
  private cache.

Now cross-runner clobber is impossible by construction, not by luck. Cheap (two tiny structs
instead of one), and it's the version to build.

## A concurrency point the seams force

The feed goroutine writes the `Chain` quote map continuously while a runner reads it during
`OnBar`. That's concurrent map access → the **Chain needs a mutex**. Pattern:
`RealPricer.Price` takes the lock, snapshots `(bid, ask, ts)` into its private cache,
releases; `RealFill.Fill` reads the snapshot with no lock (only ever touched on the bar-close
goroutine). Locking is confined to the Chain read; the cache stays lock-free.

## What gets recorded, and the synthetic shadow

Per fill you capture both the **real mid** (`(bid+ask)/2`) and the **real fill** (ask or
bid) — plus the raw bid/ask and the quote's age (so a stale-flagged trade can be excluded
from the H gate).

The **synthetic shadow** (the divergence baseline) falls out of the parity insight: run a
**second pair of runners** with `SyntheticPricer`/`SpreadModel` on the *same* bars. Because
decisions don't depend on the pricer, those shadow runners fire the **identical trades** —
only the fills differ. So you literally run **four runners**: ORB+VWAP with Real seams (the
live record) and ORB+VWAP with Synthetic seams (the shadow). Per trade,
`real_fill − synthetic_fill` *is* the fill divergence v3 exists to measure. The shadow
runners need only the underlying bars + VIX — exactly the backtest path, replayed live.

## The one honest limitation

If a picked strike's quote is genuinely missing (the C guarantee fails), the engine's
`enter()` has **no "abort because no price" path** — after `strike.Pick` succeeds it calls
Price→Fill→creates the position unconditionally. RealPricer can't cleanly veto the entry
without an engine change. Pragmatic handling: RealPricer falls back **bid/ask mid → LTP**,
and **flags the trade** as priced-on-fallback so the parity analysis can exclude it. In
normal operation C makes this never happen; it's a record-and-flag safety net, not a relied-on
path.

---

**Mental model:** RealPricer reads the live book (ignoring `spot`, because the book already
reflects it) and caches the contract's bid/ask; RealFill reads that cache because Price always
runs immediately before Fill — communicating through shared state under a guaranteed call
order, so the frozen interface never changes. Give each runner its own pricer/fill pair over
one shared, mutex-guarded Chain to make cross-runner clobber structurally impossible; run
synthetic-seam shadow runners alongside to get the fill divergence for free.
