# I. Freeze controller

**Goal of this section:** guarantee the 3–6-month incubation measures **one unchanging
system**, so the gate's statistics actually mean something.

## Why freeze at all

The H gate (KS test, MC bands, fill parity) is only valid if every trade in the sample came
from the **same** system. If you tweak a parameter or fix a signal bug halfway through, the
live sample becomes a mix of two systems — the distributions blur and the test is
meaningless. The freeze is what lets you say "these 40+ trades are all from the frozen v2
brain." That's the **parity + freeze** principle.

## What gets pinned

- **Engine version.** `go.mod` pins `github.com/shindeamul76/optedge v1.2.0`. The dev
  `replace … => ../optedge` is **removed before freeze**, and `GOPRIVATE=github.com/shindeamul76/*`
  is set so the real tagged module is fetched. From then on the engine is immutable bytes.
- **Config hash.** At startup, compute a hash of the **locked strategy config** — every
  tunable in `LiveConfigORB`/`LiveConfigVWAP`: `StopMult`, `TargetMult`, `MinDelta`,
  `StrikeStep`, the `exit`, `filters`, `sizer`, `rates`, and `spread` settings. Because the
  facade keeps these values **inside the frozen engine** (the live repo can't re-declare a
  number — see the `live` package), the hash is computed over the engine's own locked values
  and is tamper-evident from outside.

Both the engine version and the config hash are recorded at startup **and stamped into every
audit line** (G), so each trade is provably attributable to one frozen configuration.

## What resets the incubation clock — and what doesn't

The dividing line is a single question: **could this change which trades fire?**

**Resets the clock** (start the 3–6 months over):

- any change to the **engine version** or the **config hash**;
- anything that could alter a **trade decision**: the signal logic, strike pick, delta gate,
  exits — *and* the live components that feed decisions: the **feed parser, the aggregator,
  or the bar-close timing** (B). A bug there changes *what* the engine sees, hence what it
  trades.

**Allowed without reset** (logged, but the clock keeps running):

- **live-infra fixes that don't change decisions**: the dashboard, reconnect/backoff logic,
  logging, the persistence format, the recovery mechanics — as long as the *bars and fills
  the engine acts on* are unchanged.

When in doubt, ask "does this change a bar the Runner sees, or a parameter it uses?" Yes →
reset. No → log and continue.

## How it's enforced

At startup the controller:

1. computes `config_hash` and reads the `engine_version` (from build info / `go.mod`),
2. compares them against the **stored freeze baseline** (the hash + version recorded when the
   clock started),
3. if either changed, it **refuses to count days toward the gate** until the operator
   explicitly acknowledges and resets the clock (a deliberate, logged action — not silent).

This makes a parameter tweak impossible to slip in unnoticed: the very next startup flags the
hash change and stops the incubation count.

## Why this is the natural payoff of the facade

The whole reason the locked parameters live in `optedge/live` (constructors, not numbers the
live repo declares) is this section: the freeze config-hash is computed over values the live
service **cannot drift**. The facade isn't just an import convenience — it's the mechanism
that makes the freeze enforceable.

---

**Mental model:** pin the engine (v1.2.0, no `replace`) and hash the locked config; stamp
both into every trade for provenance; reset the 3–6-month clock for anything that could change
a trade decision (engine, config, feed/aggregator/bar-timing) but allow logged infra fixes
that don't; enforce it at startup by refusing to count days when the baseline hash changes.
The facade keeping the params inside the frozen engine is what makes the hash tamper-proof.
