// Command tokens resolves the instrument tokens we want to capture/subscribe and
// prints them with their WebSocket exchangeType. Use it to pick tokens for
// cmd/recordframe. Pass an approximate NIFTY spot (eyeball it) so it can list the
// ATM±N option strikes for the current weekly.
//
// Usage:
//
//	go run ./cmd/tokens -spot 23500
//	go run ./cmd/tokens -spot 23500 -n 3 -underlying NIFTY
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge/live"
)

func main() {
	spot := flag.Float64("spot", 0, "approximate underlying spot (to compute ATM strikes); 0 = skip options")
	step := flag.Float64("step", 50, "strike step")
	n := flag.Int("n", 3, "strikes each side of ATM")
	underlying := flag.String("underlying", "NIFTY", "index underlying name")
	flag.Parse()

	fmt.Println("downloading scrip master…")
	insts, err := broker.DownloadScripMaster()
	if err != nil {
		fatal("scrip master", err)
	}
	now := time.Now()

	fut, err := broker.FrontMonthFuture(insts, *underlying, now)
	if err != nil {
		fatal("front-month future", err)
	}
	printInst("FUTURE", fut)

	if vix, err := broker.FindIndiaVIX(insts); err == nil {
		printInst("VIX", vix)
	} else {
		fmt.Printf("VIX: not found (%v)\n", err)
	}

	if *spot <= 0 {
		fmt.Println("\n(no -spot given; skipping option strikes)")
		return
	}

	exp, err := broker.NearestOptionExpiry(insts, *underlying, now)
	if err != nil {
		fatal("nearest expiry", err)
	}
	fmt.Printf("\ncurrent weekly expiry: %s\n", exp.Format("2006-01-02"))
	fmt.Printf("ATM±%d strikes around spot %.0f (step %.0f):\n", *n, *spot, *step)

	for _, k := range broker.ATMStrikes(*spot, *step, *n) {
		for _, ot := range []live.OptionType{live.Call, live.Put} {
			opt, err := broker.FindOption(insts, *underlying, exp, k, ot)
			if err != nil {
				fmt.Printf("  %.0f %-4s  NOT FOUND (%v)\n", k, ot, err)
				continue
			}
			printInst(fmt.Sprintf("%.0f %s", k, ot), opt)
		}
	}
}

func printInst(label string, in broker.Instrument) {
	exType, err := in.WSExchangeType()
	exStr := fmt.Sprintf("%d", exType)
	if err != nil {
		exStr = "?"
	}
	fmt.Printf("  %-12s %-22s token=%-8s exType=%s\n", label, in.Symbol, in.Token, exStr)
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", stage, err)
	os.Exit(1)
}
