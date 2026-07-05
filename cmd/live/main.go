// Command live runs the full v3 forward paper-test stack: it logs in, connects the
// resilient feed, aggregates 1-minute bars, manages the live option-chain window, and
// drives the two FROZEN Runners (ORB + VWAP) on live data — filling against the real
// chain bid/ask. It prints bar closes, connection state, and completed paper trades.
// No orders are placed; ₹0 is at risk.
//
//	go run ./cmd/live                 # auto-resolve NIFTY front-month future + India VIX
//	go run ./cmd/live -stale 5s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge-live/internal/dashboard"
	"github.com/shindeamul76/optedge-live/internal/engine"
	"github.com/shindeamul76/optedge-live/internal/feed"
	"github.com/shindeamul76/optedge-live/internal/freeze"
	"github.com/shindeamul76/optedge-live/internal/parity"
	"github.com/shindeamul76/optedge-live/internal/pipeline"
	"github.com/shindeamul76/optedge-live/internal/recovery"
	"github.com/shindeamul76/optedge-live/internal/store"
)

func main() {
	secretsPath := flag.String("secrets", "secrets.yaml", "path to credentials YAML")
	underlying := flag.String("underlying", "NIFTY", "index underlying name")
	step := flag.Float64("step", 50, "strike step")
	n := flag.Int("n", 3, "strikes each side of ATM to keep subscribed")
	stale := flag.Duration("stale", 5*time.Second, "quote age beyond which a fill is flagged")
	dataDir := flag.String("data", "data", "root directory for the incubation record")
	dashAddr := flag.String("dashboard", ":8080", "dashboard listen address (empty to disable)")
	wfPath := flag.String("wf", "wf_reference.json", "walk-forward reference fixture for the gate (optional)")
	resetFreeze := flag.Bool("reset-freeze", false, "reset the incubation clock to the current engine+config")
	lotFlag := flag.Int("lot", 0, "position lot size; 0 = auto-detect from the Angel One scrip master")
	flag.Parse()

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		fatal("load IST", err)
	}

	creds, err := broker.LoadCredentials(*secretsPath)
	
	if err != nil {
		fatal("load credentials", err)
	}

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
	fmt.Printf("future: %s token=%s expiry=%s\n", fut.Symbol, fut.Token, fut.Expiry.Format("2006-01-02"))

	// Position lot size: auto-detect from the live scrip master (self-corrects if NSE
	// revises it) unless forced with -lot. It's a deployment parameter — it participates
	// in the freeze hash but does not modify the frozen engine.
	effLot := *lotFlag
	src := "forced via -lot"
	if effLot <= 0 {
		effLot, src = fut.Lotsize, "auto from scrip master"
	}
	if effLot <= 0 {
		effLot, src = 65, "fallback (scrip master had none)"
	}
	fmt.Printf("lot size: %d (%s)\n", effLot, src)
	if *lotFlag > 0 && *lotFlag != fut.Lotsize {
		fmt.Printf("  ⚠ forced lot %d differs from the live scrip-master lot %d\n", *lotFlag, fut.Lotsize)
	}

	aggregate := []string{fut.Token}
	initial := []feed.TokenList{{ExchangeType: broker.ExchNSEFO, Tokens: []string{fut.Token}}}
	vixToken := ""
	if vix, err := broker.FindIndiaVIX(insts); err == nil {
		vixToken = vix.Token
		aggregate = append(aggregate, vixToken)
		initial = append(initial, feed.TokenList{ExchangeType: broker.ExchNSECM, Tokens: []string{vixToken}})
		fmt.Printf("vix:    %s token=%s\n", vix.Symbol, vix.Token)
	} else {
		fmt.Printf("vix:    NOT FOUND (%v) — delta gate will run without live IV\n", err)
	}

	// Feed: login on every (re)connect → fresh jwt+feedToken.
	loginFn := func() (feed.Auth, error) {
		s, err := broker.Login(creds)
		if err != nil {
			return feed.Auth{}, err
		}
		return feed.Auth{JWTToken: s.JWTToken, APIKey: s.APIKey, ClientCode: s.ClientCode, FeedToken: s.FeedToken}, nil
	}
	sup, err := feed.NewSupervisor(feed.Config{
		Login: loginFn,
		Dial:  feed.DialConn,
		Sub:   feed.Subscription{Mode: feed.ModeSnapQuote, Lists: initial},
		Loc:   loc,
	})
	if err != nil {
		fatal("supervisor", err)
	}

	// Chain: the live ATM window over the current weekly.
	ch, err := chain.New(chain.Config{
		Instruments: insts, Underlying: *underlying, StrikeStep: *step, N: *n, Loc: loc, Logf: log.Printf,
	})
	if err != nil {
		fatal("chain", err)
	}

	// Coordinator: routes ticks, drives chain re-centering on each future bar.
	co, err := pipeline.New(pipeline.Config{
		Feed: sup, Chain: ch, FutureToken: fut.Token, AggregateTokens: aggregate,
		Loc: loc, Now: time.Now, Logf: log.Printf,
	})
	if err != nil {
		fatal("coordinator", err)
	}

	// Persistence: the durable incubation record for today's session.
	day := time.Now().In(loc).Format("2006-01-02")
	paths, err := store.NewPaths(*dataDir, day)
	if err != nil {
		fatal("session paths", err)
	}
	rec, err := store.NewRecorder(paths, log.Printf)
	if err != nil {
		fatal("recorder", err)
	}
	defer rec.Close()

	// Freeze controller: the gate verdict is only valid if every trade came from ONE
	// unchanging system. Check the engine+config against the baseline, stamp provenance
	// on every trade, and refuse to validate results if it drifted (unless -reset-freeze).
	engVer, pinned := freeze.EngineVersion()
	cfgHash, err := freeze.ConfigHash(loc, effLot)
	if err != nil {
		fatal("config hash", err)
	}
	st, err := freeze.Check(*dataDir, loc, effLot)
	if err != nil {
		fatal("freeze check", err)
	}
	var freezeOK bool
	var freezeLabel string
	switch {
	case *resetFreeze || st.FirstRun:
		bl, err := freeze.Start(*dataDir, loc, day, effLot)
		if err != nil {
			fatal("freeze start", err)
		}
		what := "started"
		if *resetFreeze {
			what = "RESET"
		}
		freezeOK, freezeLabel = true, fmt.Sprintf("frozen %s · %s since %s", bl.EngineVersion, bl.ConfigHash, bl.ClockStart)
		fmt.Printf("incubation clock %s: engine=%s hash=%s\n", what, bl.EngineVersion, bl.ConfigHash)
	case !st.Match:
		freezeOK, freezeLabel = false, fmt.Sprintf("FREEZE VIOLATION since %s", st.Baseline.ClockStart)
		fmt.Fprintf(os.Stderr,
			"\n⚠ FREEZE VIOLATION: engine/config changed since %s.\n  baseline: engine=%s hash=%s\n  current:  engine=%s hash=%s\n  Gate results are INVALID until you run with -reset-freeze.\n\n",
			st.Baseline.ClockStart, st.Baseline.EngineVersion, st.Baseline.ConfigHash, st.Current.EngineVersion, st.Current.ConfigHash)
	default:
		freezeOK, freezeLabel = true, fmt.Sprintf("frozen %s · %s since %s", st.Current.EngineVersion, st.Current.ConfigHash, st.Baseline.ClockStart)
		fmt.Printf("freeze OK: engine=%s hash=%s since %s\n", st.Current.EngineVersion, st.Current.ConfigHash, st.Baseline.ClockStart)
	}
	if !pinned {
		fmt.Println("note: engine not pinned (local replace) — remove the replace directive + set GOPRIVATE before the real freeze run")
	}
	rec.SetProvenance(engVer, cfgHash)

	// Runners: the two frozen Runners with live seams. Trades are persisted AND printed;
	// every fill leg feeds the recorder (position.json on entry, audit on exit).
	runners := engine.NewRunners(engine.Config{
		Chain: ch, Loc: loc, Now: time.Now, StaleAfter: *stale, LotSize: effLot,
		OnTrade: func(ev engine.TradeEvent) { rec.OnTrade(ev); printTrade(ev) },
		OnFill:  rec.OnFill,
		Logf:    log.Printf,
	})

	// Crash recovery: if today's log exists, replay it to rebuild runner state and any
	// open position BEFORE going live (no-op on a fresh session).
	if res, err := recovery.Recover(runners, rec, paths, fut.Token, vixToken); err != nil {
		fatal("recovery", err)
	} else if res.BarsReplayed > 0 {
		fmt.Printf("recovered: replayed %d bars, %d open position(s), %d completed today\n",
			res.BarsReplayed, res.OpenPositions, res.CompletedToday)
	}

	ctx, cancel := signalContext()
	defer cancel()

	go func() {
		if err := co.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "pipeline stopped:", err)
		}
	}()
	watchState(ctx, sup)

	// Dashboard: minimal embedded web view of the incubation (equity vs gate,
	// divergence, connection state). Reads the cumulative audit + live in-memory bits.
	if *dashAddr != "" {
		wf, err := parity.LoadWFReference(*wfPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wf reference:", err)
		}
		dash := dashboard.NewServer(func() dashboard.Snapshot {
			s := dashboard.BuildSnapshot(*dataDir, parity.DefaultCapital, wf, sup.State().String(), ch.Center())
			s.FreezeOK, s.FreezeLabel = freezeOK, freezeLabel
			return s
		})
		go func() {
			if err := dash.Start(ctx, *dashAddr); err != nil {
				fmt.Fprintln(os.Stderr, "dashboard:", err)
			}
		}()
		fmt.Printf("dashboard: http://localhost%s\n", *dashAddr)
	}

	fmt.Println("live paper-test running (₹0 at risk). Ctrl+C to stop.")
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nshutting down…")
			return
		case tb := <-co.Bars():
			rec.OnBar(tb.Token, tb.Bar) // log every closed bar (reproducibility + recovery)
			switch tb.Token {
			case fut.Token:
				runners.OnFutureBar(tb.Bar)
				b := tb.Bar
				fmt.Printf("%s FUT  O/H/L/C %.2f/%.2f/%.2f/%.2f  V%d  chain@%.0f\n",
					b.TS.Format("15:04"), b.Open, b.High, b.Low, b.Close, b.Volume, ch.Center())
			case vixToken:
				runners.OnVIXBar(tb.Bar)
			}
		}
	}
}

func printTrade(ev engine.TradeEvent) {
	tr := ev.Trade
	fmt.Printf(">>> TRADE [%s] %s %s %.0f x%d | entry %.2f exit %.2f | net %s (%s)\n",
		ev.Runner, tr.Dir, tr.OptType, tr.Strike, tr.Qty, tr.EntryPrem, tr.ExitPrem, tr.NetPnL.String(), tr.Reason)
}

func watchState(ctx context.Context, sup *feed.Supervisor) {
	go func() {
		last := feed.ConnState(-1)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if st := sup.State(); st != last {
					fmt.Printf("[conn] %s\n", st)
					last = st
				}
			}
		}
	}()
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() { <-ch; cancel() }()
	return ctx, cancel
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", stage, err)
	os.Exit(1)
}
