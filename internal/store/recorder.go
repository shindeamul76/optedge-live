package store

import (
	"sync"

	"github.com/shindeamul76/optedge-live/internal/engine"
	"github.com/shindeamul76/optedge/live"
)

// Recorder turns the engine's observation taps (OnFill, OnTrade) and the coordinator's
// bar stream into the durable incubation record: it writes position.json on entry,
// an audit line on completion (correlating the entry/exit fill legs to the trade),
// and the bar log per closed bar. It can be Muted during crash-recovery replay so the
// reconstructed entries/exits aren't double-written.
type Recorder struct {
	audit *AuditWriter
	bars  *BarLog
	pos   *PositionStore

	mu            sync.Mutex
	muted         bool
	open          map[string]Position       // runner → open position
	entry         map[string]engine.FillLeg // runner → last entry leg (for audit correlation)
	exit          map[string]engine.FillLeg // runner → last exit leg
	engineVersion string                    // freeze provenance stamped on every audit line
	configHash    string
	logf          func(string, ...any)
}

// SetProvenance records the frozen engine version + config hash to stamp on every
// subsequent audit line (phase 7). Call once at startup after the freeze check.
func (r *Recorder) SetProvenance(engineVersion, configHash string) {
	r.mu.Lock()
	r.engineVersion, r.configHash = engineVersion, configHash
	r.mu.Unlock()
}

// NewRecorder opens the session files and loads any existing open positions (so a
// restart starts from the persisted state).
func NewRecorder(p Paths, logf func(string, ...any)) (*Recorder, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	aw, err := OpenAudit(p)
	if err != nil {
		return nil, err
	}
	bl, err := OpenBarLog(p)
	if err != nil {
		aw.Close()
		return nil, err
	}
	ps := OpenPositions(p)
	open, err := ps.Read()
	if err != nil {
		aw.Close()
		bl.Close()
		return nil, err
	}
	return &Recorder{
		audit: aw, bars: bl, pos: ps,
		open: open, entry: map[string]engine.FillLeg{}, exit: map[string]engine.FillLeg{},
		logf: logf,
	}, nil
}

// Mute/Unmute gate writes during recovery replay.
func (r *Recorder) Mute()   { r.mu.Lock(); r.muted = true; r.mu.Unlock() }
func (r *Recorder) Unmute() { r.mu.Lock(); r.muted = false; r.mu.Unlock() }

// OnBar appends a closed bar to the bar log.
func (r *Recorder) OnBar(token string, b live.Bar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muted {
		return
	}
	if err := r.bars.Append(BarRecord{
		Token: token, TS: b.TS, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume,
	}); err != nil {
		r.logf("store: bar log append: %v", err)
	}
}

// OnFill records a fill leg. On a Buy it writes the open position; on a Sell it stashes
// the exit leg for the audit record that OnTrade will emit next.
func (r *Recorder) OnFill(leg engine.FillLeg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muted {
		return
	}
	if leg.Side == live.Buy {
		r.entry[leg.Runner] = leg
		r.open[leg.Runner] = Position{
			Runner: leg.Runner, Dir: dirOf(leg.Typ), OptType: string(leg.Typ),
			Strike: leg.Strike, Expiry: leg.Expiry, EntryTS: leg.TS,
			EntryFill: leg.Fill, EntryBid: leg.Bid, EntryAsk: leg.Ask,
		}
		if err := r.pos.Write(r.open); err != nil {
			r.logf("store: position write: %v", err)
		}
		return
	}
	r.exit[leg.Runner] = leg
}

// OnTrade writes the completed trade to the audit log (with the correlated entry/exit
// legs) and clears the open position.
func (r *Recorder) OnTrade(ev engine.TradeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muted {
		return
	}
	tr := ev.Trade
	rec := AuditRecord{
		Runner: ev.Runner, Day: tr.Day, Dir: string(tr.Dir), OptType: string(tr.OptType),
		Strike: tr.Strike, Qty: tr.Qty, EntryTS: tr.EntryTS, ExitTS: tr.ExitTS,
		EntrySpot: tr.EntrySpot, ExitSpot: tr.ExitSpot,
		EntryFill: tr.EntryPrem, ExitFill: tr.ExitPrem,
		GrossRupees: tr.GrossPnL.Float(), ChargesRupees: tr.Charges.Float(), NetRupees: tr.NetPnL.Float(),
		Reason:        string(tr.Reason),
		EngineVersion: r.engineVersion, ConfigHash: r.configHash,
	}
	if leg, ok := r.entry[ev.Runner]; ok {
		rec.Expiry, rec.EntryBid, rec.EntryAsk, rec.EntryMid = leg.Expiry, leg.Bid, leg.Ask, leg.Mid
		rec.EntryStale, rec.EntryFallback = leg.Stale, leg.Fallback
	}
	if leg, ok := r.exit[ev.Runner]; ok {
		rec.ExitBid, rec.ExitAsk, rec.ExitMid = leg.Bid, leg.Ask, leg.Mid
		rec.ExitStale, rec.ExitFallback = leg.Stale, leg.Fallback
	}
	// Synthetic twin fills → the fill-divergence data (real − synth).
	if ev.HasSynth {
		rec.EntrySynth = ev.Synth.EntryPrem
		rec.ExitSynth = ev.Synth.ExitPrem
		rec.NetSynthRupees = ev.Synth.NetPnL.Float()
	}
	if err := r.audit.Append(rec); err != nil {
		r.logf("store: audit append: %v", err)
	}
	delete(r.open, ev.Runner)
	delete(r.entry, ev.Runner)
	delete(r.exit, ev.Runner)
	if err := r.pos.Write(r.open); err != nil {
		r.logf("store: position clear: %v", err)
	}
}

// Close flushes and closes the log files.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.audit.Close()
	if e := r.bars.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

func dirOf(typ live.OptionType) string {
	if typ == live.Call {
		return string(live.Long)
	}
	return string(live.Short)
}
