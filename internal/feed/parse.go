package feed

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// SnapQuoteLen is the exact byte length of a mode-3 (SnapQuote) frame, verified
// against captured frames (see parse_test.go). Header(2) + token(25) + 15×int64
// fields(120) + best-5 depth(200) + circuit/52w(32) = 379.
const SnapQuoteLen = 379

const depthOffset = 147 // first byte of the best-5 block

// DepthLevel is one rung of the order book: aggregate quantity at a price and the
// number of orders making it up. Price is in rupees (raw feed paise ÷ 100).
type DepthLevel struct {
	Qty    int64
	Price  float64
	Orders int16
}

// Tick is a fully decoded SnapQuote frame. Prices are rupees; quantities are raw.
// Bids are best-first (Bids[0] = highest bid); Asks are best-first (Asks[0] =
// lowest ask). HasDepth is false for instruments with no order book (indices send
// an all-0xFF depth block), in which case Bids/Asks are zero.
type Tick struct {
	Mode          int
	ExchangeType  int
	Token         string
	SeqNo         int64
	ExchTime      time.Time // exchange timestamp (epoch ms)
	LTP           float64
	LastQty       int64
	AvgPrice      float64
	Volume        int64 // cumulative volume for the day
	TotalBuyQty   float64
	TotalSellQty  float64
	Open          float64
	High          float64
	Low           float64
	Close         float64
	LastTradeTime time.Time // epoch seconds
	OI            int64
	OIChangePct   float64
	HasDepth      bool
	Bids          [5]DepthLevel
	Asks          [5]DepthLevel
}

// BestBid returns the top bid price (Bids[0].Price), or 0 if no depth.
func (t Tick) BestBid() float64 { return t.Bids[0].Price }

// BestAsk returns the top ask price (Asks[0].Price), or 0 if no depth.
func (t Tick) BestAsk() float64 { return t.Asks[0].Price }

// ParseSnapQuote decodes a mode-3 binary frame. All multi-byte integers are
// little-endian; prices are stored in paise (÷100 → rupees); the two total-qty
// fields and OI-change% are IEEE-754 float64 (NOT scaled). Layout is fixed-offset
// — see lld-explained/A.md and the offsets below.
func ParseSnapQuote(b []byte) (Tick, error) {
	if len(b) < SnapQuoteLen {
		return Tick{}, fmt.Errorf("snapquote: frame too short: %d bytes (want >= %d)", len(b), SnapQuoteLen)
	}
	if b[0] != ModeSnapQuote {
		return Tick{}, fmt.Errorf("snapquote: mode is %d, not %d", b[0], ModeSnapQuote)
	}

	t := Tick{
		Mode:          int(b[0]),
		ExchangeType:  int(b[1]),
		Token:         parseToken(b[2:27]),
		SeqNo:         i64(b, 27),
		ExchTime:      time.UnixMilli(i64(b, 35)),
		LTP:           paise(b, 43),
		LastQty:       i64(b, 51),
		AvgPrice:      paise(b, 59),
		Volume:        i64(b, 67),
		TotalBuyQty:   f64(b, 75),
		TotalSellQty:  f64(b, 83),
		Open:          paise(b, 91),
		High:          paise(b, 99),
		Low:           paise(b, 107),
		Close:         paise(b, 115),
		LastTradeTime: time.Unix(i64(b, 123), 0),
		OI:            i64(b, 131),
		OIChangePct:   f64(b, 139),
	}

	// Best-5 depth: 10 packets of 20 bytes — int16 flag (1=buy,0=sell), int64 qty,
	// int64 price(paise), int16 orders. Packets 0–4 are bids, 5–9 are asks. Indices
	// have no book and send an all-0xFF block; detect that via an out-of-range flag
	// and leave HasDepth false.
	flag0 := binary.LittleEndian.Uint16(b[depthOffset:])
	t.HasDepth = flag0 == 0 || flag0 == 1
	if t.HasDepth {
		for i := 0; i < 5; i++ {
			t.Bids[i] = depthAt(b, depthOffset+i*20)
			t.Asks[i] = depthAt(b, depthOffset+(i+5)*20)
		}
	}
	return t, nil
}

func depthAt(b []byte, off int) DepthLevel {
	return DepthLevel{
		Qty:    i64(b, off+2),
		Price:  float64(i64(b, off+10)) / 100.0,
		Orders: int16(binary.LittleEndian.Uint16(b[off+18:])),
	}
}

// i64 reads a little-endian int64 at off.
func i64(b []byte, off int) int64 { return int64(binary.LittleEndian.Uint64(b[off:])) }

// f64 reads a little-endian IEEE-754 float64 at off.
func f64(b []byte, off int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b[off:])) }

// paise reads a little-endian int64 price in paise and returns rupees.
func paise(b []byte, off int) float64 { return float64(i64(b, off)) / 100.0 }

// parseToken reads the null-padded ASCII token from the 25-byte field.
func parseToken(raw []byte) string {
	return strings.TrimRight(string(raw), "\x00")
}
