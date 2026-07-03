# Known issues (triaged)

Robustness/hardening items found in an internal review, to address before the real
multi-month freeze run. None block development; the two confirmed bugs found alongside
these (feed read-deadline wedge; missing-quote ₹0 fills) are already fixed. Each item
below is impact-rated and has a proposed fix.

| # | Area | Impact | Summary |
|---|------|--------|---------|
| 1 | aggregator | medium | No clock-skew/NTP guard — wall-clock bar close can truncate bars |
| 2 | aggregator | low | Nondeterministic future-vs-VIX bar emit order |
| 3 | recovery | low | Crash between logging a bar and processing it can diverge replay |
| 4 | feed | low | `Modify` vs `Close`/`clearConn` race drops an incremental (un)subscribe |
| 5 | aggregator | low | Cumulative-volume reset clamps a bar's volume to 0 |
| 6 | dashboard | low | Concurrent audit read races the appender + discards the read error |
| 7 | freeze | low | Config hash should hash an explicit projection, not json of internal sub-configs |

---

### 1 — Aggregator: no clock-skew/NTP guard (medium)
**Where:** `internal/aggregator/aggregator.go` (`CloseDue`), driven by `manager.go`.
**Scenario:** `CloseDue` finalizes minute *M* at *wall-clock* M+1:00 while ticks are
bucketed by *exchange* timestamp. If the host clock runs ahead of the exchange, ticks
still within minute *M* arrive after `lastClosed=M` and are dropped, so the bar closes
early with truncated high/low/close/volume and clock-based exits act on a stale close.
`lld-explained/B.md` mandates an NTP/skew guard (abort if skew > 2s) that isn't
implemented. Only the timer close path is affected (the new-minute-tick path is
exchange-time driven).
**Fix:** verify NTP/clock skew at startup and abort if > 2s; optionally track skew
continuously and flag bars closed under high skew.

### 2 — Aggregator manager: nondeterministic future-vs-VIX emit order (low)
**Where:** `internal/aggregator/manager.go` — ticker branch iterates
`for tok, agg := range m.aggs` (Go map = random order).
**Scenario:** when the future and VIX bars for the same minute close in one timer fire,
they're emitted in random order. If the future bar is processed before `OnVIXBar`
updates LiveVIX, the synthetic pricer uses the prior minute's sigma; the order varies
run-to-run on identical data. Live trade *decisions* are unaffected (the ATM
strike/delta-gate is insensitive to a one-minute sigma change) — this only adds noise to
the synthetic-shadow divergence and hurts replay reproducibility.
**Fix:** iterate a stable, ordered token slice (VIX first) instead of the map.

### 3 — Recovery: crash between logging a bar and processing it (low)
**Where:** `cmd/live/main.go` logs `rec.OnBar` before `runners.OnFutureBar`; replay in
`internal/recovery/recovery.go`.
**Scenario:** if the process dies between logging bar *N* and processing it, the bar is
in `bars.jsonl` but its decision isn't in `audit.jsonl`/`position.json`. On restart,
`Recover` replays bar *N* and re-decides it; if it exits an open position, the replay
queue (`[entry]` only) is exhausted so `Fill` serves the empty live chain → exit at 0,
trade completed muted → the runner ends flat while `position.json` still shows it open.
**Fix:** process the bar *before* logging it, or reconcile `position.json` against the
reconstructed state after replay and square-off/flag on mismatch.

### 4 — Supervisor: `Modify` vs `Close`/`clearConn` race (low)
**Where:** `internal/feed/supervisor.go` — `conn.Close()` then `clearConn()` are two
non-atomic steps; `Modify` checks `s.conn != nil` under `connMu`.
**Scenario:** readLoop returns after a drop; Run calls `conn.Close()` but is preempted
before `clearConn()`. A concurrent `Modify` (from a chain reconcile) sees
`s.conn != nil` and writes on the closed socket → error logged, incremental change not
delivered live. Recoverable: the change survives in the desired set and is re-sent on
the next connect.
**Fix:** clear `s.conn` under `connMu` as part of closing (single critical section).

### 5 — Aggregator: cumulative-volume reset clamps volume to 0 (low)
**Where:** `internal/aggregator/aggregator.go` `finalize` —
`vol = endVolume - lastVolume`, clamped to 0 when negative.
**Scenario:** if the feed's cumulative day-volume restarts mid-session, the boundary bar
computes a negative delta and reports V=0 while real volume traded, misfiring the ORB
volume-surge filter for that minute.
**Fix:** detect the decrease and re-seed `lastVolume` from the new cumulative (treat as a
reset) rather than silently zeroing.

### 6 — Dashboard: concurrent audit read + discarded error (low)
**Where:** `internal/dashboard/snapshot.go` — `recs, days, _ := store.ReadAllAudit(root)`
(error discarded), read on every `/api/state` request while the recorder appends.
**Scenario:** a request reads mid-append; the torn final line is skipped so the newest
trade is momentarily missing → `NTrades` reads one short and the verdict can flicker at
the 40-trade boundary across 2s refreshes. The discarded error also hides real failures.
**Fix:** surface the read error (log it) and/or read a consistent snapshot.

### 7 — Freeze: hash an explicit projection, not json of internal sub-configs (low)
**Where:** `internal/freeze/freeze.go` `fingerprint` — Exit/Filters/Rates/Spread hashed
via `json.Marshal(any)`, which silently omits `*time.Location`/unexported fields.
**Scenario:** a drift confined to a json-invisible sub-config field wouldn't change the
hash, so the freeze controller wouldn't detect it or reset the clock. (The
engine-version check is the primary guard and the main tunables are hashed explicitly,
so impact is limited.) Go-toolchain float-formatting changes could also flip the hash
across rebuilds.
**Fix:** hash an explicit, deterministically-formatted projection of every meaningful
sub-config field instead of relying on json of internal types.
