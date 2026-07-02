# B. Bar aggregator (ticks → 1-min bars)

**Goal of this section:** turn the raw tick stream from A into the exact `core.Bar`
(1-min OHLCV) shape the frozen engine expects — one bar per minute, in order — so the Runner
can't tell it's running live.

## The job: manufacture backtest-identical bars from live ticks

The engine only ever consumes a stream of `core.Bar`, one per minute, chronological. In the
backtest those came from a CSV (`SliceSource`). Live has no CSV — it has a firehose of
ticks. The aggregator folds ticks into bars **byte-for-byte the same shape the engine was
validated on.** This is the `BarSource` seam's live implementation.

Scope point: **only the underlying (NIFTY future) and India VIX become bars.** Option
contracts do *not* get aggregated — they stay as a live "latest quote" map that
`RealPricer` reads at the instant of a fill (see C). So you run **two aggregators** (future
→ drives the Runner; VIX → feeds `LiveVIX`), and the option chain is handled separately.

## The fold (ticks → one OHLCV)

For one instrument, within one minute bucket:

- **O** = price of the **first** tick in the minute
- **H** = **max** LTP seen in the minute
- **L** = **min** LTP seen in the minute
- **C** = price of the **last** tick in the minute
- **V** = **Δ cumulative day-volume** over the minute = (day-volume on the last tick) −
  (day-volume at the minute's start)

That last one is subtle: the feed's volume field (byte 67) is **cumulative for the whole
day**, not per-tick. A minute's volume is the *difference* between where the running total
ended and began. The first bar of the session has no "previous total" to subtract from —
seed it from the opening snapshot or emit V=0 for 09:15 and log it.

### Which minute does a tick belong to?

Bucket by the **exchange timestamp** (byte 35, the authoritative trade time), floored to the
minute, in **Asia/Kolkata**. The bar labeled `09:15` covers the half-open interval
**[09:15:00, 09:16:00)**. Tick at 09:15:59.8 → minute 09:15. Tick at 09:16:00.0 → minute
09:16.

## The parity-critical part: *when* does a bar close?

This is the whole reason B is a design decision and not just "sum some ticks." When do you
declare minute M's bar final and hand it to the Runner?

**The naive way (close on next tick) is wrong.** "Close minute M when the first tick of M+1
arrives" breaks on quiet minutes: if no tick comes for 90 seconds (an illiquid stretch or a
brief feed gap), minute M's bar closes *late*, and worse, the Runner's **clock-based logic
fires late** — its time-stop, stall exit, and 15:15 square-off all key off bar timestamps.
In the backtest there is always exactly one bar per minute, deterministically. If live
cadence wobbles with liquidity, live and backtest diverge on *timing*, which can change which
exit fires — a parity break.

**The choice: close on the wall clock at M+1:00, via a timer.** At wall-clock 09:16:00,
minute 09:15's bar is finalized and fired — **regardless of whether a new tick has
arrived.** A dead-quiet minute still closes exactly on schedule. This reproduces the
backtest's rigid one-bar-per-minute cadence and keeps the engine's clock honest. It's also
inherently no-lookahead: the bar closes because *time passed*, never because you peeked at a
future tick.

So the timer fires at M+1:00 and asks: *did I receive any ticks during minute M?*

- **Yes** → build O/H/L/C/V, emit the bar, call `OnBar`.
- **No ticks at all** → log a gap and **skip** the bar (failure table: "no synthetic
  fill-in" — never fabricate a fake bar). The engine's clock simply advances at the next real
  bar. Extremely rare for the liquid front-month future; harmless for VIX.

### Late ticks

A tick stamped for minute M that arrives *after* M's bar already closed is **dropped and
logged**. You can't rewrite a bar the engine already acted on — that would be retroactive
lookahead. (For a liquid future, late ticks are a few stragglers; logging them watches feed
health.)

### Why this needs NTP (the clock-skew failure mode)

Note the split: you **bucket** ticks by *exchange* timestamp, but **trigger the close** on
your *local* wall clock. If your machine's clock drifts ahead of the exchange's, the timer
fires M's bar early and legitimate ticks for M arrive "late" and get dropped — silently
shrinking bars. That's why the failure-modes table requires an **NTP-synced clock and aborts
if skew > 2 s**. The two clocks must agree for the trigger and the bucketing to line up.

## How this connects back to the `BarSource` seam

There's a shape mismatch. The backtest `BarSource` is **pull-based**: `Engine.Run` calls
`Next()` and `Next` returns the next bar. But live is **push-based** — ticks and the
close-timer arrive asynchronously. Two clean ways to bridge it:

1. **Implement `BarSource` over a channel.** The timer pushes each closed bar onto a Go
   channel; `Next()` blocks reading from it. Then the *unchanged* `Engine.Run` drives the
   Runner exactly as in backtest. Maximum parity — same engine loop, live or historical.
2. **Bypass the Engine and call `runner.OnBar(bar, hist)` directly** when a bar closes (what
   §E literally says). Simpler with two runners, but you build your own `History`.

Either works because the Runner **ignores `hist`** (`optedge/internal/strategy/orb.go:123`
— "History is unused: the runner keeps its own minimal per-day state"). Lean toward option 1
(channel-backed `BarSource`) precisely *because* it reuses the frozen `Engine.Run` and keeps
the live and backtest call paths identical — one less place for parity to drift. Locked in E.

---

**Mental model:** bucket ticks by exchange time into the IST minute; O/H/L/C from prices,
V from cumulative-volume deltas; close each bar on a wall-clock timer at M+1:00 (not on the
next tick) so cadence is liquidity-independent and matches the backtest; drop late ticks,
skip dead minutes, require NTP so the trigger and the timestamps agree.
