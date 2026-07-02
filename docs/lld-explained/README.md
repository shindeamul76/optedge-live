# v3 Live Service (optedge-live) — LLD, explained section by section

Plain-language teaching notes for the LLD
(`intraday-trading/docs/concepts/10-v3-live-service-lld.md`). One file per section so you
can read and confirm them one at a time. Each file explains *why*, not just *what*, and
points at the exact engine code it relies on.

Read in order:

| File | Section | One-line |
|---|---|---|
| [A.md](A.md) | Live feed (SmartWebSocketV2) | One persistent socket; JSON out (auth/subscribe/ping), binary in (ticks). Mode 3 = bid/ask. |
| [B.md](B.md) | Bar aggregator | Ticks → 1-min bars; close on a wall-clock timer at M+1:00 for parity. |
| [C.md](C.md) | Option-chain subscription | Keep ATM±3 CE/PE of the current weekly live; roll on spot drift + expiry. |
| [D.md](D.md) | RealPricer + RealFill | The seam plug-ins; Price→Fill cache trick; per-runner pairs. |
| [E.md](E.md) | Driving the Runner | Wire the runners; seams are your observation tap; portfolio union. |
| [F.md](F.md) | Live IV source | `IVSource` seam (done in v1.1.0); LiveVIX; the VIX-feed risk. |
| [G.md](G.md) | Persistence + recovery | jsonl audit, position.json, bar log; replay-to-resume. |
| [H.md](H.md) | Parity engine + dashboard | Shadow divergence; the gate math; minimal web dashboard. |
| [I.md](I.md) | Freeze controller | Pin v1.2.0 + config hash; what resets the incubation clock. |

**The one idea that ties it all together:** every trade *decision* (signal, strike, stop,
target, exits) is computed on the **underlying future + the clock + VIX** — never on the
option price. So live and backtest agree on *what* to trade; v3 measures only the *fill P&L*
(real chain vs synthetic). Everything below serves that one measurement.

> Frozen engine reference: `optedge` module, tag **v1.2.0**, repo folder
> `c:\Users\Amul\Desktop\optedge`. Public door = the `optedge/live` facade package.
