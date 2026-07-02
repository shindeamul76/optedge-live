# E. Driving the frozen Runner

**Goal of this section:** plug B–D into the engine and start producing paper trades — and
explain how the live service *observes* what the frozen Runner did.

## What gets constructed

For each of the two signals, build a Runner from its **locked config** and inject the live
seams:

- `cfg := live.LiveConfigORB(loc)` → set `cfg.Pricer = ` this runner's RealPricer,
  `cfg.Fill = ` its RealFill → `runner := live.NewRunner(cfg, liveVIX)`
- same with `live.LiveConfigVWAP(loc)` for the VWAP runner

Each runner gets its **own** `{RealPricer, RealFill}` pair (the D anti-clobber rule), all
pointing at one shared Chain. Mirror this with two **synthetic-seam** runners on the same
bars for the shadow — so concretely **four runners**: two "real" (the live record) and two
"synthetic" (the divergence baseline). Constructed identically; only the injected seams
differ.

## How a closed bar reaches all the runners

B gives you a bar-close event at M+1:00. The cleanest way to fan it to all runners while
**reusing the frozen engine**:

1. Wrap the runners in a **composite Strategy** — an `OnBar(b, hist)` that calls each
   runner's `OnBar(b, hist)` in turn.
2. Back the `BarSource` with a **channel**: the B close-timer pushes each finished bar onto a
   channel; `Next()` blocks reading it.
3. Run the unchanged `Engine.Run(composite)` over that channel source.

Payoff: `Engine.Run` builds the no-lookahead `History` exactly as in backtest, and the *same
loop* drives live and historical. (Sharing one `History` across runners is safe — they ignore
it anyway, `orb.go:123`, and never mutate it.) The alternative — bypassing the Engine and
calling each `OnBar` directly with a hand-built History — works too, but reusing `Engine.Run`
removes one more spot where live could drift from backtest.

## The key observability insight: the seams are your tap

How does the live service observe what the frozen Runner did? The Runner's open position
(`s.pos`) is unexported, and a `Trade` is only appended on **exit** (`orb.go:230`, inside
`manage()`), never on entry. So from outside you'd be blind to entries.

Except — **the seams are called *inside* the engine, and you own the seams.** On entry the
Runner calls `RealFill.Fill(price, Buy)`; on exit it calls `RealFill.Fill(price, Sell)`
(`orb.go:192`, `orb.go:225`). Since each runner has its own RealFill, **every leg flows
through your code**:

- `Fill(_, Buy)` → an **entry** just happened (the contract is in RealPricer's cache from the
  immediately-preceding `Price`) → write `position.json`, log the entry leg.
- `Fill(_, Sell)` → an **exit** just happened → the round-trip is complete → clear
  `position.json`, and the new `Trade` is now in `runner.Trades`.

So the seams are simultaneously the **fill mechanism** (D) and the **observation point**
(E/G). This is how the live service knows entries, exits, real bid/ask, and contract identity
without touching the frozen engine. The completed `Trade` you then read off `runner.Trades`
(its length grew) gives you the engine's authoritative P&L to cross-check against your leg
log.

## The portfolio

The two real runners run **independently** — no position interaction — and the incubation
record is the **union** of their trades, exactly like `evaluate.RunPortfolio` in the
backtest. **1 lot/leg**, so if both fire the same day you're holding 2 lots (one per signal).
Capital ₹50k is shared at the portfolio level. Each runner enforces **one trade per day**
(`tradedToday`, `orb.go:144`), so the combined book does **at most 2 trades/day**, averaging
~1.5 historically — which sets the H frequency band and the ~4–6-weeks-to-40-trades timeline.

## The property that makes all of this trustworthy

Because **decisions depend only on bars + clock + VIX** (never the fills), the same bar
stream produces the *same* decisions every time — live, backtest, or replay. The only
nondeterminism is the real fills. That gives a powerful artifact: the **live bar log**
(persisted in G) is a complete reproducibility record. Replay it through fresh synthetic
runners offline and you reproduce every live decision exactly — for debugging, for the
shadow, and for proving parity.

## The catch this raises for restarts (sets up G)

That same statefulness is the hard part of crash recovery. The Runner is **stateful per
session**:

- the **signal** carries session state — ORB needs the 09:15–09:30 opening range; VWAP needs
  cumulative VWAP from the session open;
- the Runner's `s.recent` buffer holds this session's prior bars for filter averages, reset
  each day (`orb.go:130`).

So you **cannot** restart mid-session into a fresh Runner and just resume — it would have no
opening range, no VWAP, no filter history, and make different (wrong) decisions. The
parity-correct recovery is to **replay the day's bars** (from the persisted bar log) through
fresh runners to rebuild all that state, *then* hand over to the live stream. That
replay-to-resume mechanic is exactly what G specifies — and it's only possible *because* of
the reproducibility property above.

---

**Mental model:** build one Runner per signal from its locked config with its own live seam
pair; fan each closed bar to all four runners via a composite Strategy over a channel-backed
BarSource so the frozen `Engine.Run` drives live exactly as it drove backtest; observe
entries/exits through your own RealFill (Buy=entry, Sell=exit) since the engine exposes
nothing else; take the union of the two real runners as the 1-lot/leg portfolio, ≤2
trades/day; and remember the runners are per-session stateful, so a mid-day restart must
replay the day's bars to rebuild signal/VWAP/filter state before resuming.
