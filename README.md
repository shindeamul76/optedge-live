# optedge-live

**Forward paper-testing service (v3) for the [optedge](https://github.com/shindeamul76/optedge) intraday NIFTY options engine.**

`optedge-live` runs the **frozen** backtest decision code on **live Angel One market data** at **₹0 risk**, to prove that real option fills match the walk-forward backtest before any real money is deployed. It is the "incubation" stage of a staged build (backtest → forward paper-test → live).

> **Principle: parity + freeze.** The live system runs the *exact same* decision code as the backtest. Any divergence in results must come from the market (real data/fills), not from code differences — so this service isolates and measures exactly one thing: **how well real fills hold up**, forward, on unseen data.

---

## The idea in one picture

Every trade **decision** — the signal, the strike + delta gate, the stop/target, the exits — is computed on the **underlying future and the clock**, never on the option price:

```
Angel One SmartWebSocketV2 ─┬─ NIFTY future ticks ─► 1-min bars ─► [FROZEN ENGINE] ─► paper trades
                            ├─ India VIX ──────────► live IV (delta gate)     │
                            └─ option chain (ATM±N) ► real bid/ask ───────────┘  (fills only)
```

Because decisions never touch the option price, the live path and the synthetic backtest produce **identical trade decisions** on the same data. The *only* thing that differs is the **fill P&L** (real chain vs the model). That's the single variable v3 measures.

The engine exposes three swappable **seams** (Go interfaces) so the same code runs in backtest and live:

| Seam | Backtest | Live (this repo) |
|---|---|---|
| `OptionPricer` | synthetic Black-76 | real option chain (bid/ask) |
| `FillModel` | modeled spread | real live bid/ask (buy@ask, sell@bid) |
| `IVSource` | historical VIX series | live India VIX |

---

## Architecture

```
feed.Supervisor ──ticks──► pipeline.Coordinator ──┬─ future/VIX ─► aggregator ─► bars ─► engine.Runners ─► trades
  (reconnect, re-auth,        (routes + reconciles) └─ options ────► chain (quote map)          ▲
   heartbeat, market-hours,                                            │                        │ RealPricer/RealFill
   dynamic subscriptions) ◄── feed.Modify(sub/unsub) ◄── chain.Reconcile(spot) ◄── on each future bar close
```

- **`internal/broker`** — Angel One infra: TOTP login (keeps the WS `feedToken`), scrip-master instrument resolution. Reimplemented here (infra, not decision code → zero parity impact).
- **`internal/feed`** — SmartWebSocketV2 client, the **binary mode-3 (SnapQuote) frame parser** (verified against captured frames), and a resilient **supervisor** (exponential-backoff reconnect, re-auth, re-subscribe, market-hours gating, dynamic subscriptions).
- **`internal/aggregator`** — ticks → 1-minute OHLCV bars, closing on the **wall clock at M+1:00** (not on the next tick) so a quiet minute still closes on time — matching the backtest's cadence for parity.
- **`internal/chain`** — the live option-chain window: `token → quote` map, ATM±N × CE/PE subscription that **re-centers on spot drift (with hysteresis)** and **rolls the weekly**.
- **`internal/engine`** — the three live seam implementations (`RealPricer`, `RealFill`, `LiveVIX`) and the harness that drives the two locked Runners (ORB + VWAP).
- **`internal/pipeline`** — the coordinator that ties it all together: routes ticks, drives chain re-centering on each future bar close, emits bars downstream.

The proven decision engine (`optedge`) is imported as a **pinned, versioned dependency** through a thin public facade package (`optedge/live`), so the frozen parameters live inside the engine and can't drift from here.

---

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Engine facade + `IVSource` seam (in `optedge`) | ✅ done |
| 2 | Live feed: WS client, binary parser, resilient supervisor | ✅ done |
| 3 | Bar aggregator + option-chain subscription + coordinator | ✅ done |
| 4 | Real seams (`RealPricer`/`RealFill`/`LiveVIX`) + wire the Runners | ✅ done |
| 5 | Persistence (jsonl audit + position.json + bar log) + crash recovery (replay-to-resume) | ✅ done |
| 6 | Parity engine (shadow synthetic runners, real-vs-synthetic divergence, KS/fill/frequency gate) + embedded dashboard | ✅ done |
| 7 | Freeze controller (engine + config-hash fingerprint, provenance stamping, reset-clock detection) | ✅ done |
| — | Run forward 3–6 months → gate → v4 or abandon | ▶ operational |

The full low-level design, explained section by section, lives in [`docs/lld-explained/`](docs/lld-explained/).

The whole pipeline is unit-tested offline (golden frames, injected clocks/transports, a
crafted breakout that fills against the live chain, replay-to-resume, KS/gate math). Run
`go test ./...`.

---

## Running it

**Requires the private `optedge` engine.** This module pins `github.com/shindeamul76/optedge` (the frozen decision engine, a separate private repo) via a thin facade. Without access to that dependency it won't compile — the code here is published to show the *live-infrastructure* design. In development it builds against a local checkout via a `replace` directive in [`go.mod`](go.mod).

```bash
# 1. Credentials (never committed; secrets.yaml is gitignored)
cp secrets.example.yaml secrets.yaml   # then fill in Angel One api_key/client_code/pin/totp_secret

# 2. Smoke-test login + scrip-master resolution (works any time)
go run ./cmd/authcheck

# 3. List instrument tokens for a given approximate NIFTY spot
go run ./cmd/tokens -spot 23500

# 4. Run the resilient live feed end to end (during market hours, 09:15–15:30 IST)
go run ./cmd/feedrun
```

Other tools: `cmd/recordframe` (capture raw WS frames to a fixture) and `cmd/decodeframe` (decode a capture to verify the parser).

---

## Testing

The whole live path is **unit-tested offline** — no market connection needed:

- **Golden-file parser test** — the binary SnapQuote parser is pinned against *real captured frames* (`internal/feed/testdata/`), cross-checked against live NIFTY and VIX levels.
- **Injected clocks + transports** — the reconnect state machine, market-hours gating, bar-close timing, and chain re-centering are driven by fake clocks and fake connections, so they're deterministic and fast.
- **End-to-end wiring proof** — a crafted opening-range breakout drives the frozen Runner and asserts the trade is filled at the *real chain bid/ask*, not the synthetic model.

```bash
go test ./...
go vet ./...
```

---

## Non-goals (these are later stages, not v3)

Real broker orders / OMS, real money, live kill-switch. v3 is **observe-and-compare**, not execute.

---

## Disclaimer

Research and engineering project. Paper trading on live data only — **no orders are placed and no capital is at risk**. Nothing here is investment advice.
