# A. Live feed — Angel One SmartWebSocketV2

**Goal of this section:** get a live stream of NIFTY-future, option-chain, and India-VIX
ticks into the process, decoded correctly, with a connection that stays up all session.

## What a WebSocket feed actually is (vs the REST you already use)

The historical fetch in optedge uses **REST**: you ask (`GetCandleData`), the server
answers, done. Request → response, one shot.

A **WebSocket** is the opposite: you open *one* long-lived TCP connection and keep it open
all session. After it's open, **the server pushes data to you** whenever a tick happens —
you don't ask per tick. That's the only way to get live ticks; polling REST 100×/second
would get you banned and lag badly.

Over that one connection, two kinds of messages flow:

- **Outbound (you → Angel One): JSON text.** Three things only — authenticate (in the
  opening handshake), subscribe/unsubscribe instruments, and heartbeat.
- **Inbound (Angel One → you): raw binary.** Every market tick. *Not* JSON — packed bytes.
  This is the part that needs the byte map.

## 1. Opening the connection (auth lives in the handshake)

A WebSocket starts life as a normal HTTP `GET` with an `Upgrade: websocket` header. Angel
One requires you to attach your auth on **that handshake request** as HTTP headers:

- `Authorization: <jwtToken>` (the login JWT — possibly with a `Bearer ` prefix; verify at capture)
- `x-api-key: <apiKey>`
- `x-client-code: <clientCode>`
- `x-feed-token: <feedToken>`

Endpoint: `wss://smartapisocket.angelone.in/smart-stream`. If the headers are good, the
server upgrades the connection and it's authenticated for the whole session. **This is
exactly why login reimplemented in optedge-live must expose all four values** —
`feedToken` especially, which the old `Login()` threw away.

## 2. Subscribing (JSON control message)

Once open, you tell it what you want by sending a JSON text frame:

```json
{ "action": 1,
  "params": { "mode": 3,
              "tokenList": [ { "exchangeType": 2, "tokens": ["58721"] } ] } }
```

- `action`: 1 = subscribe, 0 = unsubscribe. (Chain-roll in C is just: subscribe new
  strikes / unsubscribe far ones with these.)
- `mode`: **1 = LTP**, **2 = Quote**, **3 = SnapQuote**. We use **3** — see below.
- `exchangeType`: 1 = NSE_CM (cash/index), **2 = NSE_FO** (futures & options), 3/4 BSE,
  5 MCX… Our NIFTY future and option contracts are all exchangeType **2**. India VIX is an
  index → likely exchangeType **1** (or not on the socket at all — the open risk, see F).
- `tokens`: the numeric instrument tokens from the scrip master (`FindOption`,
  `FrontMonthFuture` give you these).

**Why mode 3 specifically.** Mode 1 gives only last price — no bid/ask, so you literally
cannot model a fill. Mode 2 adds OHLC + volume but **still no order book**. Only **mode 3
(SnapQuote)** carries the **best-5 bid/ask depth**, and the `FillModel` seam's entire job is
"buy at the real ask, sell at the real bid." No depth → no v3. That's why it's a locked
decision.

## 3. The binary inbound frame — how to read packed bytes

Every tick arrives as a binary blob. There's no field name anywhere in it — meaning comes
**purely from byte position**. Three rules to read it:

1. **Fixed offsets.** Field X always starts at byte N and is M bytes long. The `mode` byte
   (byte 0) tells you which layout and therefore the total length (LTP=51 bytes, Quote=123,
   **SnapQuote=379**).
2. **Little-endian.** Multi-byte numbers have the least-significant byte first. Byte
   sequence `2C 01 00 …` = `0x012C` = 300. (Go reads this with `binary.LittleEndian`.)
3. **Two number formats, and mixing them up gives garbage:**
   - Most numeric fields are **int64 in paise** → read 8 bytes as a signed integer, then
     **÷100** for rupees. (LTP `4350` paise = ₹43.50.)
   - A few fields — **total buy qty, total sell qty, OI-change%** — are **IEEE-754
     float64**, read directly as floating point, *no* ÷100. Read those as int (or a price as
     float) and you get a meaningless number.

This int-vs-float distinction per field is exactly the kind of thing the spec is fuzzy on —
hence capture-first (below).

### ✅ VERIFIED against real frames (2026-06-30 ~09:31 IST)

Captured live via `cmd/recordframe` and decoded by `internal/feed/parse.go`, pinned by
the golden test `parse_test.go` (frames in `internal/feed/testdata/`). Confirmed: every
frame is **379 bytes, mode 3**; the future (token 61093) decoded to **LTP 23981.90**,
OHLC ≈ 24,000, **best bid 23977.40 < best ask 23981.30** with bids descending / asks
ascending; India VIX (99926017) → **LTP 13.91** with an all-`0xFF` (absent) depth block;
an option (44512) → LTP 1530.90. Heartbeat replies arrived as text `"pong"`. The byte map
below is therefore confirmed, not assumed.

### The SnapQuote (mode 3) layout, grouped by purpose

| Offset | Size | Field | What we do with it |
|---|---|---|---|
| 0 | 1 | mode (=3) | sanity-check the layout |
| 1 | 1 | exchange type (=2) | sanity-check |
| 2 | 25 | token (ASCII, null-padded) | which instrument this tick is — route to the right `Quote` |
| 27 | 8 | sequence number | ordering / drop duplicates |
| 35 | 8 | **exchange timestamp (ms)** | authoritative tick time (IST) — feeds the aggregator |
| 43 | 8 | **LTP** (paise) | **future's last price → 1-min bars**; option LTP = fallback |
| 51 | 8 | last traded qty | logged |
| 59 | 8 | average traded price | logged |
| 67 | 8 | **day volume** | bar volume = Δ(this) over the minute |
| 75 | 8 | total buy qty (**float64**) | book imbalance (optional) |
| 83 | 8 | total sell qty (**float64**) | book imbalance (optional) |
| 91/99/107/115 | 8 ea | day Open/High/Low/Close (paise) | logged / sanity |
| 123 | 8 | last traded timestamp | staleness check |
| 131 | 8 | **open interest** | logged per LLD; OI sanity |
| 139 | 8 | OI change % (**float64**) | logged |
| **147** | **200** | **best-5 depth** | **the FillModel input** (next) |
| 347… | 8 ea | upper/lower circuit, 52w H/L | ignore |

Total **379 bytes** for SnapQuote.

### The best-5 depth block (bytes 147–346) — the part that matters most

200 bytes = **10 records of 20 bytes each**. Each 20-byte record:

```
int16  flag      (1 = buy/bid side, 0 = sell/ask side)
int64  quantity
int64  price      (paise)
int16  number of orders
```

Layout: **records 0–4 = the 5 best BIDs** (highest first → record 0 is the best bid),
**records 5–9 = the 5 best ASKs** (lowest first → record 5 is the best ask). So:

- **best bid = record[0].price ÷ 100** → what you receive when you **SELL** (exit)
- **best ask = record[5].price ÷ 100** → what you pay when you **BUY** (entry)
- **mid = (best bid + best ask) ÷ 2** → what `RealPricer.Price` returns
- `RealFill.Fill(mid, Buy)` → the ask; `Fill(mid, Sell)` → the bid (penetration-not-touch)

The other 4 levels + quantities let you measure the *real spread* (the H gate) and check
whether a 75-qty lot would "walk the book" past level 0 — useful colour, not required for
the basic fill.

### How the three instruments map through this

- **NIFTY front-month future** (exType 2): mainly **LTP** + **day volume** → the aggregator
  builds 1-min OHLCV → drives both Runners' decisions. (Futures, not spot — matches the
  backtest.)
- **Option contracts** ATM±3 CE/PE (exType 2): **best bid/ask** → `RealPricer` mid +
  `RealFill`. LTP only as a fallback when depth is empty.
- **India VIX** (exType 1, or REST fallback): **LTP** → `LiveVIX.At(t)` for the delta gate.

## 4. Heartbeat

WebSocket has a *protocol-level* ping/pong (control frames). Angel One **does not rely on
that** — it uses an **application-level** heartbeat: you must send a **text frame containing
the literal string `"ping"`** every ~25–30 s, and the server replies with text `"pong"`. Go
silent and the server drops you as idle.

"Confirm it's a text frame, not a control ping" matters because the Go WS library treats
them completely differently — a text `"ping"` is `WriteMessage(TextMessage, []byte("ping"))`,
whereas a protocol ping is a separate control-frame API. Getting this wrong = silent
disconnects every 30 s. Confirm which it is the moment we capture a session.

## 5. Why capture a real frame before writing the parser

Everything above is assembled from Angel One's published SmartStream spec plus community
SDKs — but that spec has genuinely bitten people: ambiguity on whether depth is 5+5 or
interleaved, which fields are float vs int, and Angel One has changed layouts between
versions. The **only ground truth is a real frame.**

Sequence: a ~30-line throwaway recorder connects, subscribes mode 3 to **one future + one
option**, and dumps the raw bytes to a file. We verify against reality — decoded LTP must
match the live app price, token bytes must decode to the right instrument, best-bid <
best-ask — *then* lock the offsets and write the parser with that frame as a **golden-file
unit test.**

This matters more here than in a normal project: if we silently misread the bid/ask offset,
every fill is wrong, and the **fill-parity experiment is the entire point of v3**. A
corrupted parser wouldn't crash — it would quietly produce a plausible but false verdict.
Capture-first removes that risk.

---

**Mental model:** one persistent connection; JSON out (auth/subscribe/ping); binary in
(ticks decoded by fixed byte offsets); mode 3 because only it carries the bid/ask the
FillModel needs; verify the byte map against a real frame because a wrong offset silently
poisons the experiment.
