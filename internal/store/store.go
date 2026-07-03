// Package store is the incubation record: an append-only JSONL audit log (one line
// per completed trade — also the fill-divergence dataset), an atomically-written
// open-position file (for crash recovery / square-off), and a bar log (every closed
// bar, enabling replay-to-resume and reproducibility). See lld-explained/G.md.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Paths locates a session's files under root/<day>/ so each trading day is
// self-contained (recovery reads "today's" bar log).
type Paths struct{ Dir string }

// NewPaths builds the per-day paths and ensures the directory exists.
func NewPaths(root, day string) (Paths, error) {
	p := Paths{Dir: filepath.Join(root, day)}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return Paths{}, fmt.Errorf("create session dir: %w", err)
	}
	return p, nil
}

func (p Paths) Audit() string    { return filepath.Join(p.Dir, "audit.jsonl") }
func (p Paths) Bars() string     { return filepath.Join(p.Dir, "bars.jsonl") }
func (p Paths) Position() string { return filepath.Join(p.Dir, "position.json") }

// Position is the open-position record. Direction is implied by OptType (Call=long,
// Put=short). Persisted on entry so a restart can resume or force square-off.
type Position struct {
	Runner    string    `json:"runner"`
	Dir       string    `json:"dir"`
	OptType   string    `json:"opt_type"`
	Strike    float64   `json:"strike"`
	Expiry    string    `json:"expiry"`
	EntryTS   time.Time `json:"entry_ts"`
	EntryFill float64   `json:"entry_fill"` // real ask paid
	EntryBid  float64   `json:"entry_bid"`
	EntryAsk  float64   `json:"entry_ask"`
}

// AuditRecord is one completed round-trip: the frozen engine's trade with real fills,
// the real bid/ask/mid at each leg, staleness flags, and P&L (rupees). Synthetic-
// shadow fields are added in phase 6 (omitempty until then).
type AuditRecord struct {
	Runner  string    `json:"runner"`
	Day     string    `json:"day"`
	Dir     string    `json:"dir"`
	OptType string    `json:"opt_type"`
	Strike  float64   `json:"strike"`
	Expiry  string    `json:"expiry"`
	Qty     int       `json:"qty"`
	EntryTS time.Time `json:"entry_ts"`
	ExitTS  time.Time `json:"exit_ts"`

	EntrySpot float64 `json:"entry_spot"`
	ExitSpot  float64 `json:"exit_spot"`
	EntryFill float64 `json:"entry_fill"` // real ask
	ExitFill  float64 `json:"exit_fill"`  // real bid
	EntryBid  float64 `json:"entry_bid"`
	EntryAsk  float64 `json:"entry_ask"`
	EntryMid  float64 `json:"entry_mid"`
	ExitBid   float64 `json:"exit_bid"`
	ExitAsk   float64 `json:"exit_ask"`
	ExitMid   float64 `json:"exit_mid"`

	EntryStale bool `json:"entry_stale"`
	ExitStale  bool `json:"exit_stale"`
	// Fallback = a leg filled without a real book price (missing quote/side); such
	// trades are excluded from the parity gate.
	EntryFallback bool `json:"entry_fallback,omitempty"`
	ExitFallback  bool `json:"exit_fallback,omitempty"`

	GrossRupees   float64 `json:"gross_rupees"`
	ChargesRupees float64 `json:"charges_rupees"`
	NetRupees     float64 `json:"net_rupees"`
	Reason        string  `json:"reason"`

	// Freeze provenance (phase 7): every trade is attributable to one frozen system.
	EngineVersion string `json:"engine_version,omitempty"`
	ConfigHash    string `json:"config_hash,omitempty"`

	// Synthetic-shadow fills + net (the fill-divergence baseline): the same trade priced
	// by the backtest's synthetic Black-76 + modeled spread. Divergence = real − synth.
	EntrySynth     float64 `json:"entry_synth,omitempty"`
	ExitSynth      float64 `json:"exit_synth,omitempty"`
	NetSynthRupees float64 `json:"net_synth_rupees,omitempty"`
}

// BarRecord is one closed bar tagged by instrument token (future or VIX). The bar log
// is the reproducibility artifact and the input to replay-to-resume.
type BarRecord struct {
	Token  string    `json:"token"`
	TS     time.Time `json:"ts"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume int64     `json:"volume"`
}

// jsonlWriter is a mutex-guarded append-only JSONL file (each Append is flushed so a
// crash loses at most the in-flight line; a torn final line is discarded on read).
type jsonlWriter struct {
	mu sync.Mutex
	f  *os.File
}

func openJSONL(path string) (*jsonlWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &jsonlWriter{f: f}, nil
}

func (w *jsonlWriter) append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.f.Sync()
}

func (w *jsonlWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// AuditWriter appends completed trades to audit.jsonl.
type AuditWriter struct{ w *jsonlWriter }

func OpenAudit(p Paths) (*AuditWriter, error) {
	w, err := openJSONL(p.Audit())
	if err != nil {
		return nil, err
	}
	return &AuditWriter{w: w}, nil
}

func (a *AuditWriter) Append(r AuditRecord) error { return a.w.append(r) }
func (a *AuditWriter) Close() error               { return a.w.Close() }

// BarLog appends closed bars to bars.jsonl.
type BarLog struct{ w *jsonlWriter }

func OpenBarLog(p Paths) (*BarLog, error) {
	w, err := openJSONL(p.Bars())
	if err != nil {
		return nil, err
	}
	return &BarLog{w: w}, nil
}

func (b *BarLog) Append(r BarRecord) error { return b.w.append(r) }
func (b *BarLog) Close() error             { return b.w.Close() }

// PositionStore holds the set of open positions keyed by runner, written atomically
// (temp file + rename) so a crash mid-write can't corrupt it.
type PositionStore struct {
	path string
	mu   sync.Mutex
}

func OpenPositions(p Paths) *PositionStore { return &PositionStore{path: p.Position()} }

// Write atomically persists the full open-position set (empty map clears it to {}).
func (s *PositionStore) Write(open map[string]Position) error {
	if open == nil {
		open = map[string]Position{}
	}
	b, err := json.MarshalIndent(open, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // Rename replaces an existing file on all platforms
}

// Read returns the open-position set, or an empty map if the file doesn't exist.
func (s *PositionStore) Read() (map[string]Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return map[string]Position{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Position{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadBars reads every closed bar from the bar log (empty if absent). A torn final
// line (from a crash) is skipped.
func ReadBars(p Paths) ([]BarRecord, error) {
	var out []BarRecord
	err := readJSONL(p.Bars(), func(line []byte) error {
		var r BarRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// ReadAllAudit reads and concatenates the audit logs across every session-day
// directory under root (chronological by directory name), returning the combined
// records and the number of days that had trades — the cumulative incubation record
// the exit gate is computed over. Missing root → empty.
func ReadAllAudit(root string) (recs []AuditRecord, days int, err error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, derr := ReadAudit(Paths{Dir: filepath.Join(root, e.Name())})
		if derr != nil {
			return nil, 0, derr
		}
		if len(day) > 0 {
			seen[e.Name()] = true
			recs = append(recs, day...)
		}
	}
	return recs, len(seen), nil
}

// ReadAudit reads every completed trade from the audit log (empty if absent).
func ReadAudit(p Paths) ([]AuditRecord, error) {
	var out []AuditRecord
	err := readJSONL(p.Audit(), func(line []byte) error {
		var r AuditRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// readJSONL streams a JSONL file line by line, tolerating a truncated final line
// (a crash mid-write): a line that fails to parse ends the read, keeping what parsed.
func readJSONL(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return nil
		}
	}
	return sc.Err()
}
