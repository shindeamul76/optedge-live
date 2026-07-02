# optedge-live — v3 Incubation Service — KICKOFF PROMPT

Paste the block below into a new chat opened in this folder (`c:\Users\Amul\Desktop\optedge-live`).
It carries all the context the new chat needs. **We start with the LLD, not code.**

---

I'm building **optedge-live**, the v3 (Incubation / forward paper-test) service for my intraday NIFTY
options system. This folder is a NEW, separate Go module. **Do NOT write code yet — we start by finalizing
the LLD (low-level design), then build in phases.** Read the context below first.

## What already exists (DO NOT rebuild — reuse it)
**optedge** = the frozen engine, at `c:\Users\Amul\Desktop\intraday-trading-go`, module
`github.com/shindeamul76/optedge`, tagged **v1.1.0** on GitHub (private). It is a complete, tested v1/v2
HISTORICAL backtest "verdict machine" for NIFTY weekly options:
- Black-76 + India-VIX option pricing; honest cost model (ask/bid fills + full charges); no-lookahead replay.
- Signals (`signal.Signal` iface): ORB, VWAP-reclaim, PDH/PDL, VWAP-fade.
- `strategy.Runner`: signal→strike→exit lifecycle; `filters`; `evaluate` (monkey test + walk-forward +
  Monte-Carlo + multi-signal portfolio).
- **Result:** locked working book = **ORB+VWAP**; walk-forward OOS **+₹112,303** (2021–2024), monkey 100%,
  risk-of-ruin 0.1% @ ₹50k → REAL ("worth forward-testing", not "deploy").
- **Pricing validated** vs real Angel One option prices (`cmd/pricecheck`): synthetic over-prices ~1 vol pt
  but it CANCELS in the buy/sell round trip → foundation trustworthy.

The engine exposes **4 seams** (Go interfaces) so the SAME decision code runs in backtest and live. v3 just
injects LIVE implementations of these into the UNCHANGED engine:
1. `backtest.BarSource` — data in (live WebSocket feed implements it)
2. `strategy.OptionPricer` — `Price(typ core.OptionType, spot, strike float64, ts time.Time) float64`
   (synthetic Black-76 in backtest; **real option chain** live)
3. `strategy.FillModel` — `Fill(mid float64, side core.Side) float64`
   (modeled spread in backtest; **real bid/ask** live)
4. `strategy.IVSource` — `At(t time.Time) float64` (IVSeries in backtest; **live VIX** live)

KEY parity insight: every trade DECISION (signal, strike+delta-gate, stop/target, exits) is computed on the
UNDERLYING and the CLOCK — never the option price. So live and backtest agree on WHAT to trade; the only
difference is the FILL P&L (real vs synthetic). v3 measures exactly that.

## What v3 is (the goal of THIS repo)
Forward paper-test the FROZEN engine on LIVE Angel One data for **3–6 months, ₹0 at risk**, and prove the
live record is statistically indistinguishable from the walk-forward backtest — above all that REAL fills
match the modeled ones. Pass → v4 (first real money). Principle: **parity + freeze** (pin optedge@v1.1.0;
any change to engine/config resets the clock).

## Design docs — READ THESE FIRST
- HLD: `c:\Users\Amul\Desktop\intraday-trading\docs\concepts\09-v3-incubation-hld.md`
- **LLD (we finalize this first):** `c:\Users\Amul\Desktop\intraday-trading\docs\concepts\10-v3-live-service-lld.md`
- Roadmap: `...\07-roadmap-v1-to-v5.md`
- Engine code to import/inspect: `c:\Users\Amul\Desktop\intraday-trading-go\internal\strategy\` (Runner,
  pricer.go seams, iv.go), `...\internal\data\` (Angel One SmartAPI login/scrip-master/candles — reuse).

## Locked decisions
- Broker: Angel One **SmartWebSocketV2**, **mode 3 (SnapQuote)** = best-5 bid/ask (binary frames). Auth
  reuses optedge login (TOTP → jwtToken + feedToken).
- **Real option chain from day one** (synthetic logged in parallel for fill-divergence).
- This repo **optedge-live** pins **optedge@v1.1.0**. Dev: `replace github.com/shindeamul76/optedge =>
  ../intraday-trading-go`. Before freeze: remove `replace`, set `GOPRIVATE=github.com/shindeamul76/*`.
- Runtime: **local, market-hours (09:15–15:30 IST), attended** (heartbeat + crash recovery).
- Dashboard: **minimal web page** (equity vs MC bands, divergence, connection state).
- Book: **ORB+VWAP**, 1 lot/leg (2 lots), ₹50k capital.
- Bars: 1-min, **bar closes on wall-clock at M+1:00** (not next tick) for parity.
- Gate thresholds: real round-trip spread ≤ modeled; KS test live-vs-WF per-trade returns p>0.05; live
  trades/day within WF band; **min 40 trades** before any pass/fail.

## Build phases (LLD FIRST, then code)
1. ✅ optedge `IVSource` seam — DONE (in v1.1.0)
2. **Feed:** SmartWebSocketV2 client + binary frame parser (fixture-tested) + reconnect/heartbeat/market-hours
3. **Aggregator** (ticks→1-min bars) + **option-chain subscription** (ATM±3 × CE/PE, roll on spot/expiry)
4. **RealPricer + RealFill + LiveVIX** (the seam impls; RealPricer caches the contract's bid/ask so RealFill
   can read it — engine calls Price→Fill atomically per OnBar) + wire the two Runners (ORB, VWAP)
5. **Persistence** (jsonl audit: real+synthetic fills per trade) + **crash recovery** (resume/square-off open pos)
6. **Parity engine** (shadow synthetic, per-trade divergence, gate metrics) + **web dashboard**
7. **Freeze** (pin v1.1.0 + config hash) + run forward 3–6 months → gate → v4 or abandon

## Setup for this module (when we get to code)
```
go mod init github.com/shindeamul76/optedge-live
# go.mod:  require github.com/shindeamul76/optedge v1.1.0
#          replace github.com/shindeamul76/optedge => ../intraday-trading-go   (dev only)
```
Reuse Angel One creds in a gitignored `secrets.yaml` (api_key/client_code/pin/totp_secret) — same as optedge.

## START HERE (your first task)
Do NOT scaffold or code yet. First **read the HLD + LLD docs above and the optedge seam code**, then walk me
through the LLD section by section — confirm/refine the open details (exact SmartWebSocketV2 binary frame
layout for mode 3, the reconnect/heartbeat state machine, chain-roll logic, the parity-gate math, the jsonl
persistence schema, and the web-dashboard stack) and surface any decisions for me. We finalize the LLD,
THEN build phase 2 (the feed).
