# G. Paper P&L, persistence, recovery

**Goal of this section:** never lose the incubation record, and be able to restart mid-session
without breaking parity. The hard part is that the Runner is **per-session stateful** (E), so
recovery means *replay*, not *resume-from-nothing*.

## The four persisted artifacts

**1. Audit log — `audit.jsonl` (append-only, one line per completed trade).** This is *both*
the incubation record *and* the fill-divergence dataset, in one file. The LLD's base line,
refined with the fields the H gate and the shadow need:

```
ts_entry, ts_exit, runner (ORB|VWAP), dir, opt, strike, expiry, qty,
entry_real, exit_real,            // real fills (ask in / bid out)
entry_bid, entry_ask, exit_bid, exit_ask,   // raw book at each leg
entry_synth, exit_synth,          // what SyntheticPricer/SpreadModel would have filled
entry_spot, exit_spot,            // underlying at each leg
gross, charges, net, reason,
real_spread_entry, real_spread_exit,        // for the fill-parity gate
stale_flag, fallback_flag,        // quote-health markers (C, D)
engine_version, config_hash       // provenance / freeze (I)
```

Why **jsonl, append-only**: crash-safe (flush per line; a torn final line is simply
discarded on read), human-readable, trivial to post-process for the gate stats. Same pattern
as the volatility-engine logs.

**2. Open-position record — `position.json` (one per open leg).** Written the instant
`RealFill.Fill(_, Buy)` fires (the E tap), cleared on the Sell. Holds enough to resume
managing: `runner`, contract (`typ/strike/expiry/token`), `entry_ts`, `entry_real`, `qty`,
the `stop`/`target` levels, and the `exit.Position` state. **Atomic write** (temp file +
rename) so a crash mid-write can't corrupt it.

**3. Bar log — `bars.jsonl` (every closed 1-min bar for future + VIX).** Not in the original
LLD, but **required**, because recovery replays the session (below), and because it's the
reproducibility artifact (E): replay it through fresh runners offline and you reproduce every
decision exactly.

**4. Session rollup — `session_summary.jsonl`.** Per-session equity points, trade counts, and
divergence aggregates for the dashboard (H).

## Crash recovery — why "resume" is actually "replay"

The naive idea ("reload `position.json` and keep going") is **not enough**, because the
Runner's decisions for the rest of the day depend on session state that a fresh Runner
doesn't have:

- ORB's **opening range** (09:15–09:30),
- VWAP's **cumulative VWAP** since the open,
- the Runner's **`s.recent`** filter buffer and **`tradedToday`** flag.

A fresh Runner restarted at 12:30 has none of this and would make different (wrong)
decisions. So recovery must **rebuild that state by replaying the day's bars**:

1. On restart mid-session, load **today's `bars.jsonl`**.
2. **Replay** all of today's bars through **fresh runners** to rebuild signal state, VWAP,
   `s.recent`, and `tradedToday`. This deterministically reconstructs the engine to exactly
   where it was (decisions don't depend on fills, E).
3. Because replay is deterministic, the runner will **re-enter the same position** that's in
   `position.json`. To make the reconstructed `pos.entryPrem` match reality, the replay seams
   return the **recorded fills** (from `audit.jsonl`/`position.json`) rather than live quotes,
   and the **persistence taps are muted** during replay (no double-logging).
4. **Cross-check**: the re-created open position must match `position.json`. Mismatch →
   alert + halt rather than trade on a bad reconstruction.
5. **Resume** the live stream from the current bar; un-mute the taps.

This is only possible *because* of the reproducibility property in E: same bars → same
decisions.

## Recovery failure handling (the failure-modes table, made concrete)

At the moment of resume, before handing back to the live stream:

- **Option no longer subscribable** (token gone, expiry passed): **force square-off** the
  open position at last known price + log. (You can't manage what you can't quote.)
- **Past 15:15 on restart**: **square-off** — don't resume into the late session; the book's
  own square-off window has effectively passed.
- **Quotes stale > X**: **square-off at last known** + log a parity warning rather than
  manage on stale data.
- **Flat at crash** (no open position): nothing to recover — just replay to rebuild state and
  resume.

## Paper P&L

There are no broker orders (v3 is observe-and-compare). "P&L" is purely the sum of completed
`Trade.NetPnL` from the engine — the same `core.Trade` the backtest produces, with real fills
substituted. Live equity = ₹50k + cumulative net. That equity curve is what the H dashboard
overlays on the Monte-Carlo bands.

---

**Mental model:** append every completed trade to a crash-safe jsonl that doubles as the
divergence dataset; persist the open position atomically via the RealFill Buy/Sell tap; keep a
bar log because recovery is **replay-to-resume**, not resume-from-nothing — replay today's
bars through fresh runners (with recorded fills, taps muted) to rebuild the per-session state,
cross-check against `position.json`, then go live; and on recovery, square off anything you
can't safely quote or that's past the session window.
