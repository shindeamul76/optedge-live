# C. Option-chain subscription management

**Goal of this section:** make sure that whatever strike the Runner picks, that contract's
live bid/ask is *already* streaming — by keeping a window of contracts around the money
permanently subscribed.

## Why a chain subscription exists at all

The Runner decides on the **future** (B). But the *fill* happens on a specific **option
contract** — e.g. "buy the 22100 CE of this week's expiry." `RealPricer`/`RealFill` need
that exact contract's live bid/ask the moment the trade fires. WebSocket data only arrives
for instruments you've **subscribed**, and you can't subscribe *after* the signal fires — the
tick wouldn't have arrived yet, and you'd fill on nothing.

So: **keep a window of contracts around the money permanently subscribed** — **ATM ± N
strikes (N ≈ 3) × CE/PE** for the current weekly expiry.

## What you actually maintain

A live quote map, conceptually `map[token]Quote{bid, ask, ltp, oi, ts}`, updated on every
option tick from the feed (A). When the Runner picks `(type=CALL, strike=22100,
expiry=thisWeek)`, `RealPricer`:

1. resolves that triple to a **token** (scrip master),
2. looks up `quote[token]`,
3. returns `mid = (bid+ask)/2`,

and `RealFill` reads the cached bid/ask for buy@ask / sell@bid.

Resolving uses the same scrip-master helpers as the backtest — `FrontMonthFuture` (the
underlying), `NearestOptionExpiry` (which weekly), `FindOption(underlying, expiry, strike,
type)` (the token) — reimplemented in optedge-live as infra.

## How the window is positioned: ATM

ATM = `round(spot / strikeStep) * strikeStep`, where `strikeStep = 50` for NIFTY and `spot`
= the future's latest price. If spot is 22072, ATM = 22050; the window ATM±3 is **21900,
21950, 22000, 22050, 22100, 22150, 22200**, each CE *and* PE → 14 contracts.

**Why N=3.** The strategy always picks **near ATM** — the delta gate (`MinDelta = 0.30`)
means the chosen option has delta ≥ 0.30, roughly ATM to a couple strikes out for a weekly.
ATM±3 covers the plausible range with margin. Bigger N wastes bandwidth and pulls in
**illiquid far strikes** whose quotes go stale (a parity hazard); smaller N risks the picked
strike not being subscribed. N=3 is the balance, confirmable against which strikes the
backtest actually picked.

This gives the **"should not happen" guarantee**: because the window is centered on ATM and
the strategy only picks near ATM, the contract it picks is *always already live*. The
"missing quote for picked strike" failure-table entry is a **guard, not an expected path** —
if it fires, you **skip the entry and log a parity warning** rather than fill on a guess.

## The two roll triggers

The window follows the market on two axes:

**(a) Spot drift → recenter the strikes.** As spot moves, ATM changes, so the window slides:
**subscribe** the new edge strike(s) entering and **unsubscribe** the far one(s) leaving. If
spot climbs 22072 → 22130, ATM goes 22050 → 22150, so add 22250/22300 and drop 21900/21950
(each ×CE/PE). The window stays centered so the pick is always inside it.

> **Hysteresis (the refinement).** Don't recenter on every tiny ATM flicker. If spot
> oscillates right on a strike boundary — say bouncing across 22075 — ATM flips
> 22050 ↔ 22100 on alternating ticks, firing a sub/unsub storm every second (rate-limit
> risk, log noise, churn). Add a **deadband**: only recenter when spot moves more than
> *half a strike* past the current center. Then you flip once and stay. Cheap; kills the
> thrash.

**(b) Weekly expiry passes → switch to the next weekly.** All current contracts expire; the
chain moves to the next expiry's tokens entirely. Because this book is **intraday** (one
trade/day, squared off ~15:15–15:30, no overnight positions), the roll is naturally a
**between-sessions** event: each session open you re-resolve "the current weekly" from the
calendar and subscribe its window. The one intraday subtlety is **expiry day itself** — those
options decay fast and you must **never hold into the 15:30 settlement** (the square-off exit
enforces this; the chain just must not carry a dead contract). On expiry-day morning,
pre-subscribe the next weekly's window so the next session's switch is seamless.

## The parity hook hiding in the roll

Subtle but important. The engine itself computes time-to-expiry inside the Runner:
`tte := calendar.TimeToExpiryYears(b.TS, loc)` (`optedge/internal/strategy/orb.go:182`), and
feeds it to `strike.Pick` and the delta gate. So the engine has an internal notion of *which
expiry is current*. Your live chain must roll to the **same weekly the engine's calendar
assumes** — if the chain subscribed next week's contracts while the engine priced against
this week's, the strike pick and the real fill would be on different expiries. Reusing the
**same `calendar` logic** for the chain's "current weekly" keeps them in lockstep. (This is
why the calendar lives in the frozen engine and why the live infra must mirror its expiry
rule, not invent its own.)

## Staleness guard

Each `Quote` carries a timestamp. When `RealPricer` reads a quote older than X seconds it
**flags a parity warning** — a fill on a stale price isn't trustworthy. ATM options are
liquid so this is rare, but the window's edge strikes can be thin, and on a feed hiccup any
quote can age. The warning doesn't block the trade (you still need *a* fill), but it marks
that trade in the audit log so the H parity analysis can exclude or scrutinize it.

## Subscription bookkeeping

Keep a **set of currently-subscribed tokens**. On each recenter or roll, compute the
**desired set**, diff against current, send one `subscribe` for additions and one
`unsubscribe` for removals (the JSON control messages from A). Critically — on a
**reconnect** (A's state machine), re-subscribe the *entire* current desired set, because a
dropped socket loses all subscriptions server-side. The future and VIX tokens live in this
same set; they just route to the aggregators (B) instead of the quote map.

---

**Mental model:** keep ATM±3 CE/PE of the current weekly permanently subscribed so the
picked strike is always live; recenter on spot drift (with a half-strike deadband to stop
thrash) and roll to the next weekly between sessions; resolve everything through the same
`calendar`/scrip-master the engine uses so live and backtest agree on expiry; guard stale
quotes and treat a missing picked-strike quote as skip-and-warn, never a guess.
