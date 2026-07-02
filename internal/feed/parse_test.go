package feed

import (
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Golden frames captured live on 2026-06-30 ~09:31 IST via cmd/recordframe
// (NIFTY front-month future token 61093, an NFO option 44512, India VIX 99926017).
// These pin the mode-3 byte layout against REAL bytes so the parser cannot drift —
// the values below were cross-checked against live NIFTY (~23,981) and VIX (~13.9).

func loadFrame(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decode hex %s: %v", name, err)
	}
	return b
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestParseSnapQuote_Future(t *testing.T) {
	tk, err := ParseSnapQuote(loadFrame(t, "snapquote_future.hex"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tk.Mode != 3 || tk.ExchangeType != 2 {
		t.Errorf("mode/exType = %d/%d, want 3/2", tk.Mode, tk.ExchangeType)
	}
	if tk.Token != "61093" {
		t.Errorf("token = %q, want 61093", tk.Token)
	}
	if !approx(tk.LTP, 23981.90) {
		t.Errorf("LTP = %.2f, want 23981.90", tk.LTP)
	}
	if !approx(tk.Open, 24057.70) || !approx(tk.High, 24090.50) ||
		!approx(tk.Low, 23970.00) || !approx(tk.Close, 24037.70) {
		t.Errorf("OHLC = %.2f/%.2f/%.2f/%.2f, want 24057.70/24090.50/23970.00/24037.70",
			tk.Open, tk.High, tk.Low, tk.Close)
	}
	if tk.OI != 13142740 {
		t.Errorf("OI = %d, want 13142740", tk.OI)
	}
	if tk.Volume != 527345 {
		t.Errorf("Volume = %d, want 527345", tk.Volume)
	}
	// Depth: bids best-first descending, asks best-first ascending, bid < ask.
	if !tk.HasDepth {
		t.Fatal("HasDepth = false, want true")
	}
	if !approx(tk.BestBid(), 23977.40) || !approx(tk.BestAsk(), 23981.30) {
		t.Errorf("bid/ask = %.2f/%.2f, want 23977.40/23981.30", tk.BestBid(), tk.BestAsk())
	}
	if tk.BestBid() >= tk.BestAsk() {
		t.Errorf("bid %.2f not < ask %.2f", tk.BestBid(), tk.BestAsk())
	}
	for i := 1; i < 5; i++ {
		if tk.Bids[i].Price > tk.Bids[i-1].Price {
			t.Errorf("bids not descending at %d: %.2f > %.2f", i, tk.Bids[i].Price, tk.Bids[i-1].Price)
		}
		if tk.Asks[i].Price < tk.Asks[i-1].Price {
			t.Errorf("asks not ascending at %d: %.2f < %.2f", i, tk.Asks[i].Price, tk.Asks[i-1].Price)
		}
	}
	// A specific rung pins the depth-packet field offsets (qty at +2, price at +10).
	if !approx(tk.Bids[3].Price, 23976.30) || tk.Bids[3].Qty != 650 {
		t.Errorf("bid[3] = %.2f x %d, want 23976.30 x 650", tk.Bids[3].Price, tk.Bids[3].Qty)
	}
}

func TestParseSnapQuote_VIX_Index_NoDepth(t *testing.T) {
	tk, err := ParseSnapQuote(loadFrame(t, "snapquote_vix.hex"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tk.ExchangeType != 1 || tk.Token != "99926017" {
		t.Errorf("exType/token = %d/%q, want 1/99926017", tk.ExchangeType, tk.Token)
	}
	if !approx(tk.LTP, 13.91) {
		t.Errorf("VIX LTP = %.2f, want 13.91", tk.LTP)
	}
	if tk.HasDepth {
		t.Error("index frame should have no depth (all-0xFF block)")
	}
	if tk.BestBid() != 0 || tk.BestAsk() != 0 {
		t.Errorf("index bid/ask should be 0, got %.2f/%.2f", tk.BestBid(), tk.BestAsk())
	}
}

func TestParseSnapQuote_Option(t *testing.T) {
	tk, err := ParseSnapQuote(loadFrame(t, "snapquote_option.hex"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tk.ExchangeType != 2 || tk.Token != "44512" {
		t.Errorf("exType/token = %d/%q, want 2/44512", tk.ExchangeType, tk.Token)
	}
	if !approx(tk.LTP, 1530.90) {
		t.Errorf("option LTP = %.2f, want 1530.90", tk.LTP)
	}
	if !tk.HasDepth || tk.BestBid() >= tk.BestAsk() {
		t.Errorf("option depth invalid: hasDepth=%v bid=%.2f ask=%.2f", tk.HasDepth, tk.BestBid(), tk.BestAsk())
	}
}

func TestParseSnapQuote_Rejects(t *testing.T) {
	if _, err := ParseSnapQuote(make([]byte, 100)); err == nil {
		t.Error("short frame should error")
	}
	bad := make([]byte, SnapQuoteLen)
	bad[0] = 1 // LTP mode, not SnapQuote
	if _, err := ParseSnapQuote(bad); err == nil {
		t.Error("wrong mode should error")
	}
}
