// Command authcheck is a phase-2 smoke test: it logs in to Angel One (reused
// optedge creds) and resolves the key instruments from the scrip master, proving
// the broker package works end to end before we touch the WebSocket. It prints
// token PRESENCE (lengths/prefixes), never the secrets themselves.
//
// Usage:
//
//	go run ./cmd/authcheck -secrets secrets.yaml
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
)

func main() {
	secretsPath := flag.String("secrets", "secrets.yaml", "path to credentials YAML")
	underlying := flag.String("underlying", "NIFTY", "index underlying name")
	flag.Parse()

	creds, err := broker.LoadCredentials(*secretsPath)
	if err != nil {
		fatal("load credentials", err)
	}

	sess, err := broker.Login(creds)
	if err != nil {
		fatal("login", err)
	}
	fmt.Println("login OK")
	fmt.Printf("  jwtToken:   %s… (%d chars)\n", prefix(sess.JWTToken, 8), len(sess.JWTToken))
	fmt.Printf("  feedToken:  %s… (%d chars)\n", prefix(sess.FeedToken, 8), len(sess.FeedToken))
	fmt.Printf("  clientCode: %s\n", sess.ClientCode)

	fmt.Println("\ndownloading scrip master…")
	insts, err := broker.DownloadScripMaster()
	if err != nil {
		fatal("scrip master", err)
	}
	fmt.Printf("  parsed %d instruments\n", len(insts))

	now := time.Now()
	fut, err := broker.FrontMonthFuture(insts, *underlying, now)
	if err != nil {
		fatal("front-month future", err)
	}
	exType, _ := fut.WSExchangeType()
	fmt.Printf("\nfront-month future: %s  token=%s  exType=%d  expiry=%s  lot=%d\n",
		fut.Symbol, fut.Token, exType, fut.Expiry.Format("2006-01-02"), fut.Lotsize)

	exp, err := broker.NearestOptionExpiry(insts, *underlying, now)
	if err != nil {
		fatal("nearest expiry", err)
	}
	fmt.Printf("current weekly expiry: %s\n", exp.Format("2006-01-02"))

	if vix, err := broker.FindIndiaVIX(insts); err != nil {
		fmt.Printf("India VIX: NOT FOUND (%v) — LiveVIX will need a REST fallback\n", err)
	} else {
		vixEx, _ := vix.WSExchangeType()
		fmt.Printf("India VIX: %s  token=%s  exType=%d  seg=%s\n", vix.Symbol, vix.Token, vixEx, vix.ExchSeg)
	}

	fmt.Println("\nauthcheck OK")
}

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", stage, err)
	os.Exit(1)
}
