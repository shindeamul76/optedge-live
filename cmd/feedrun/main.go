// Command feedrun runs the live feed supervisor end to end: it logs in, connects to
// SmartWebSocketV2, subscribes (mode 3), and streams decoded ticks — reconnecting,
// re-authenticating, re-subscribing, and idling outside market hours on its own.
// This is the phase-2 capstone (the resilient feed); the bar aggregator and chain
// management come in phase 3.
//
//	go run ./cmd/feedrun                       # auto: front-month future + India VIX
//	go run ./cmd/feedrun -tokens "2:61093,1:99926017"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/feed"
)

func main() {
	secretsPath := flag.String("secrets", "secrets.yaml", "path to credentials YAML")
	tokensArg := flag.String("tokens", "", "comma list exType:token; empty = auto (future + VIX)")
	underlying := flag.String("underlying", "NIFTY", "index underlying for auto-resolve")
	flag.Parse()

	creds, err := broker.LoadCredentials(*secretsPath)
	if err != nil {
		fatal("load credentials", err)
	}

	lists, err := resolveTokens(*tokensArg, *underlying)
	if err != nil {
		fatal("resolve tokens", err)
	}

	// Login is called on every (re)connect → always-fresh jwt + feedToken.
	loginFn := func() (feed.Auth, error) {
		sess, err := broker.Login(creds)
		if err != nil {
			return feed.Auth{}, err
		}
		return feed.Auth{
			JWTToken:   sess.JWTToken,
			APIKey:     sess.APIKey,
			ClientCode: sess.ClientCode,
			FeedToken:  sess.FeedToken,
		}, nil
	}

	sup, err := feed.NewSupervisor(feed.Config{
		Login: loginFn,
		Dial:  feed.DialConn,
		Sub:   feed.Subscription{Mode: feed.ModeSnapQuote, Lists: lists},
	})
	if err != nil {
		fatal("new supervisor", err)
	}

	ctx, cancel := signalContext()
	defer cancel()

	go func() {
		if err := sup.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "supervisor stopped:", err)
		}
	}()

	fmt.Printf("feedrun started; subscribed %d token group(s). Ctrl+C to stop.\n", len(lists))
	watchState(ctx, sup)

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nshutting down…")
			return
		case tk := <-sup.Ticks():
			printTick(tk)
		}
	}
}

func printTick(t feed.Tick) {
	if t.HasDepth {
		fmt.Printf("%s  %-9s LTP=%-9.2f bid=%-9.2f ask=%-9.2f spread=%.2f vol=%d\n",
			t.ExchTime.Format("15:04:05"), t.Token, t.LTP, t.BestBid(), t.BestAsk(),
			t.BestAsk()-t.BestBid(), t.Volume)
	} else {
		fmt.Printf("%s  %-9s LTP=%-9.2f (index)\n", t.ExchTime.Format("15:04:05"), t.Token, t.LTP)
	}
}

// watchState prints connection-state transitions in the background.
func watchState(ctx context.Context, sup *feed.Supervisor) {
	go func() {
		last := feed.ConnState(-1)
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if st := sup.State(); st != last {
					fmt.Printf("[conn] %s\n", st)
					last = st
				}
			}
		}
	}()
}

// resolveTokens returns the subscribe groups: explicit -tokens if given, otherwise
// the auto front-month future (exType 2) + India VIX (exType 1).
func resolveTokens(arg, underlying string) ([]feed.TokenList, error) {
	if strings.TrimSpace(arg) != "" {
		return parseTokens(arg)
	}
	fmt.Println("auto-resolving front-month future + India VIX from scrip master…")
	insts, err := broker.DownloadScripMaster()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	fut, err := broker.FrontMonthFuture(insts, underlying, now)
	if err != nil {
		return nil, err
	}
	groups := map[int][]string{broker.ExchNSEFO: {fut.Token}}
	fmt.Printf("  future: %s token=%s\n", fut.Symbol, fut.Token)
	if vix, err := broker.FindIndiaVIX(insts); err == nil {
		groups[broker.ExchNSECM] = append(groups[broker.ExchNSECM], vix.Token)
		fmt.Printf("  vix:    %s token=%s\n", vix.Symbol, vix.Token)
	} else {
		fmt.Printf("  vix:    not found (%v)\n", err)
	}
	return toLists(groups), nil
}

func parseTokens(arg string) ([]feed.TokenList, error) {
	groups := map[int][]string{}
	for _, pair := range strings.Split(arg, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad token spec %q (want exType:token)", pair)
		}
		ex, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("bad exchangeType in %q: %w", pair, err)
		}
		tok := strings.TrimSpace(parts[1])
		if tok == "" {
			return nil, fmt.Errorf("empty token in %q", pair)
		}
		groups[ex] = append(groups[ex], tok)
	}
	return toLists(groups), nil
}

func toLists(groups map[int][]string) []feed.TokenList {
	var lists []feed.TokenList
	for ex, toks := range groups {
		lists = append(lists, feed.TokenList{ExchangeType: ex, Tokens: toks})
	}
	return lists
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", stage, err)
	os.Exit(1)
}
