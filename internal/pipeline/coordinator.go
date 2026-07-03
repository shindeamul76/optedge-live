// Package pipeline wires the live data path together: it consumes the feed's tick
// stream, routes ticks (future/VIX → the bar aggregator, options → the chain quote
// map), and on every future bar close re-centers the option-chain window and pushes
// the resulting subscribe/unsubscribe diff back to the feed. It is the glue that
// makes the feed actually manage the chain live (phases 3a+3b integrated).
//
// Downstream (phase 4) consumes Bars() to drive the Runners and reads Chain() for the
// RealPricer/RealFill; LiveVIX reads the VIX bars off the same Bars() stream.
package pipeline

import (
	"context"
	"time"

	"github.com/shindeamul76/optedge-live/internal/aggregator"
	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/chain"
	"github.com/shindeamul76/optedge-live/internal/feed"
)

// Feed is the subset of the feed supervisor the coordinator needs. The concrete
// *feed.Supervisor satisfies it; tests inject a fake.
type Feed interface {
	Run(ctx context.Context) error
	Ticks() <-chan feed.Tick
	Modify(add, remove []feed.TokenList)
}

// Config wires a Coordinator.
type Config struct {
	Feed            Feed
	Chain           *chain.Chain
	FutureToken     string   // the instrument whose bar close drives chain re-centering
	AggregateTokens []string // tokens to fold into bars (future + VIX)
	Loc             *time.Location
	Now             func() time.Time
	CloseInterval   time.Duration // wall-clock check cadence for bar closes
	Logf            func(string, ...any)
}

// Coordinator runs the integrated live pipeline.
type Coordinator struct {
	feed        Feed
	ch          *chain.Chain
	mgr         *aggregator.Manager
	futureToken string
	logf        func(string, ...any)
	bars        chan aggregator.TokenBar
}

// New builds a Coordinator, creating the bar aggregator manager internally.
func New(cfg Config) (*Coordinator, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	mgr, err := aggregator.NewManager(aggregator.Config{
		Tokens:             cfg.AggregateTokens,
		Loc:                cfg.Loc,
		Now:                cfg.Now,
		CloseCheckInterval: cfg.CloseInterval,
	})
	if err != nil {
		return nil, err
	}
	return &Coordinator{
		feed:        cfg.Feed,
		ch:          cfg.Chain,
		mgr:         mgr,
		futureToken: cfg.FutureToken,
		logf:        cfg.Logf,
		bars:        make(chan aggregator.TokenBar, 256),
	}, nil
}

// Bars is the stream of closed bars (future + VIX), tagged by token, for downstream
// consumers (the Runners and LiveVIX in phase 4).
func (c *Coordinator) Bars() <-chan aggregator.TokenBar { return c.bars }

// Chain exposes the live chain for the RealPricer/RealFill (phase 4).
func (c *Coordinator) Chain() *chain.Chain { return c.ch }

// Run starts the feed and the aggregator, then routes ticks and drives chain
// re-centering until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	mgrTicks := make(chan feed.Tick, 1024)

	go func() {
		if err := c.feed.Run(ctx); err != nil && ctx.Err() == nil {
			c.logf("pipeline: feed stopped: %v", err)
		}
	}()
	go c.mgr.Run(ctx, mgrTicks)
	go c.consumeBars(ctx)

	// Tick router: options update the chain; future/VIX are forwarded to the
	// aggregator (which ignores tokens it wasn't asked to aggregate).
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tick, ok := <-c.feed.Ticks():
			if !ok {
				return nil
			}
			c.ch.Update(tick)
			select {
			case mgrTicks <- tick:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (c *Coordinator) consumeBars(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tb, ok := <-c.mgr.Bars():
			if !ok {
				return
			}
			c.onBar(ctx, tb)
		}
	}
}

// onBar re-centers the chain window for a future bar (applying the sub/unsub diff to
// the feed) BEFORE emitting the bar downstream, so the runner consuming Bars() sees a
// chain already centered on this bar's spot. VIX/other bars are just forwarded.
func (c *Coordinator) onBar(ctx context.Context, tb aggregator.TokenBar) {
	if tb.Token == c.futureToken {
		if sub, unsub, err := c.ch.Reconcile(tb.Bar.Close, tb.Bar.TS); err != nil {
			c.logf("pipeline: chain reconcile: %v", err)
		} else if len(sub) > 0 || len(unsub) > 0 {
			c.feed.Modify(contractsToLists(sub), contractsToLists(unsub))
			c.logf("pipeline: chain center=%.0f exp=%s +%d/-%d contracts",
				c.ch.Center(), c.ch.Expiry().Format("2006-01-02"), len(sub), len(unsub))
		}
	}

	select {
	case c.bars <- tb:
	case <-ctx.Done():
	}
}

// contractsToLists groups option contracts into a single NSE_FO subscribe/unsubscribe
// group (all weekly options are on NFO / exchangeType 2).
func contractsToLists(cs []chain.Contract) []feed.TokenList {
	if len(cs) == 0 {
		return nil
	}
	toks := make([]string, 0, len(cs))
	for _, c := range cs {
		toks = append(toks, c.Token)
	}
	return []feed.TokenList{{ExchangeType: broker.ExchNSEFO, Tokens: toks}}
}
