// Command decodeframe reads a capture produced by cmd/recordframe and prints the
// decoded SnapQuote fields, so we can eyeball that the parser reads real bytes
// correctly before locking a golden test. Dev tool — not part of the live service.
//
//	go run ./cmd/decodeframe -in frames.captured.jsonl
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/shindeamul76/optedge-live/internal/feed"
)

type frameRecord struct {
	I      int    `json:"i"`
	Type   string `json:"type"`
	ExType int    `json:"exType"`
	Hex    string `json:"hex"`
}

func main() {
	in := flag.String("in", "frames.captured.jsonl", "capture file from cmd/recordframe")
	limit := flag.Int("limit", 6, "max frames to print")
	flag.Parse()

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	printed := 0
	seen := map[string]bool{}
	for sc.Scan() && printed < *limit {
		var rec frameRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Type != "binary" {
			continue
		}
		raw, err := hex.DecodeString(rec.Hex)
		if err != nil {
			fmt.Printf("frame %d: bad hex: %v\n", rec.I, err)
			continue
		}
		t, err := feed.ParseSnapQuote(raw)
		if err != nil {
			fmt.Printf("frame %d: parse error: %v\n", rec.I, err)
			continue
		}
		// Print one frame per distinct token so we cover future/option/VIX.
		if seen[t.Token] {
			continue
		}
		seen[t.Token] = true
		printed++

		fmt.Printf("── frame %d  token=%s  exType=%d  mode=%d\n", rec.I, t.Token, t.ExchangeType, t.Mode)
		fmt.Printf("   ExchTime=%s  seq=%d\n", t.ExchTime.Format("15:04:05.000"), t.SeqNo)
		fmt.Printf("   LTP=%.2f  ATP=%.2f  lastQty=%d  vol=%d\n", t.LTP, t.AvgPrice, t.LastQty, t.Volume)
		fmt.Printf("   OHLC=%.2f/%.2f/%.2f/%.2f  OI=%d  OIchg%%=%.4f\n", t.Open, t.High, t.Low, t.Close, t.OI, t.OIChangePct)
		fmt.Printf("   totBuyQty=%.0f  totSellQty=%.0f\n", t.TotalBuyQty, t.TotalSellQty)
		if t.HasDepth {
			fmt.Printf("   bestBid=%.2f (qty %d)  bestAsk=%.2f (qty %d)  spread=%.2f\n",
				t.BestBid(), t.Bids[0].Qty, t.BestAsk(), t.Asks[0].Qty, t.BestAsk()-t.BestBid())
			fmt.Printf("   bids: ")
			for _, d := range t.Bids {
				fmt.Printf("%.2f(%d) ", d.Price, d.Qty)
			}
			fmt.Printf("\n   asks: ")
			for _, d := range t.Asks {
				fmt.Printf("%.2f(%d) ", d.Price, d.Qty)
			}
			fmt.Println()
		} else {
			fmt.Printf("   (no depth — index)\n")
		}
	}
}
