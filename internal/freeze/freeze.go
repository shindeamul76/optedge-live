// Package freeze is the incubation freeze controller: it fingerprints the locked
// engine (version) + strategy config (hash), records that as the baseline when the
// clock starts, and on every startup checks whether either has drifted — because the
// gate verdict is only valid if all trades came from ONE unchanging system. Any
// change must reset the clock. See lld-explained/I.md.
package freeze

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/shindeamul76/optedge/live"
)

const enginePath = "github.com/shindeamul76/optedge"

// Baseline is the frozen fingerprint recorded when the incubation clock starts.
type Baseline struct {
	ConfigHash    string `json:"config_hash"`
	EngineVersion string `json:"engine_version"`
	Pinned        bool   `json:"pinned"` // false when built via a local replace (not a real freeze)
	ClockStart    string `json:"clock_start"`
}

// Status is the result of checking the current build against the stored baseline.
type Status struct {
	Current  Baseline
	Baseline *Baseline // nil on first run
	FirstRun bool
	Match    bool
}

// ConfigHash is a deterministic fingerprint of the LOCKED strategy parameters (from
// the frozen engine's LiveConfigORB/VWAP). Funcs/interfaces/seams are excluded; the
// numeric tunables and the exit/filter/cost/spread sub-configs are hashed. lotOverride
// (>0) is applied to the sizer before hashing so the fingerprint reflects the actual
// deployed lot size (a live deployment parameter), keeping provenance honest.
func ConfigHash(loc *time.Location, lotOverride int) (string, error) {
	h := sha256.New()
	for _, c := range []live.Config{live.LiveConfigORB(loc), live.LiveConfigVWAP(loc)} {
		if lotOverride > 0 {
			c.Sizer.LotSize = lotOverride
		}
		b, err := json.Marshal(fingerprint(c))
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// configFP is the hashable projection of a Config (only deterministic fields).
type configFP struct {
	Rate       float64
	StrikeStep float64
	MinDelta   float64
	StopMult   float64
	TargetMult float64
	UseIVCheap bool
	RVRatio    float64
	LotQty     int
	Exit       any
	Filters    any
	Rates      any
	Spread     any
	Loc        string
}

func fingerprint(c live.Config) configFP {
	loc := ""
	if c.Loc != nil {
		loc = c.Loc.String()
	}
	return configFP{
		Rate: c.Rate, StrikeStep: c.StrikeStep, MinDelta: c.MinDelta,
		StopMult: c.StopMult, TargetMult: c.TargetMult,
		UseIVCheap: c.UseIVCheap, RVRatio: c.RVRatio, LotQty: c.Sizer.Qty(),
		Exit: c.Exit, Filters: c.Filters, Rates: c.Rates, Spread: c.Spread, Loc: loc,
	}
}

// EngineVersion reads the pinned optedge version from the build info. When the module
// is wired via a local `replace` (dev), it is NOT a real pin — pinned is false and the
// version is labeled accordingly, so the operator knows to remove the replace before
// the real freeze run.
func EngineVersion() (version string, pinned bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false
	}
	for _, d := range bi.Deps {
		if d.Path == enginePath {
			if d.Replace != nil {
				return "local-replace(" + d.Replace.Path + ")", false
			}
			return d.Version, true
		}
	}
	return "not-a-dependency", false
}

// current computes the live build's fingerprint (ClockStart left blank).
func current(loc *time.Location, lotOverride int) (Baseline, error) {
	hash, err := ConfigHash(loc, lotOverride)
	if err != nil {
		return Baseline{}, err
	}
	ver, pinned := EngineVersion()
	return Baseline{ConfigHash: hash, EngineVersion: ver, Pinned: pinned}, nil
}

func baselinePath(root string) string { return filepath.Join(root, "freeze_baseline.json") }

// Check compares the current build against the stored baseline under root.
func Check(root string, loc *time.Location, lotOverride int) (Status, error) {
	cur, err := current(loc, lotOverride)
	if err != nil {
		return Status{}, err
	}
	stored, err := load(root)
	if err != nil {
		return Status{}, err
	}
	if stored == nil {
		return Status{Current: cur, FirstRun: true}, nil
	}
	match := stored.ConfigHash == cur.ConfigHash && stored.EngineVersion == cur.EngineVersion
	return Status{Current: cur, Baseline: stored, Match: match}, nil
}

// Start (re)establishes the freeze baseline at day, resetting the incubation clock.
func Start(root string, loc *time.Location, day string, lotOverride int) (Baseline, error) {
	cur, err := current(loc, lotOverride)
	if err != nil {
		return Baseline{}, err
	}
	cur.ClockStart = day
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Baseline{}, err
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return Baseline{}, err
	}
	tmp := baselinePath(root) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return Baseline{}, err
	}
	if err := os.Rename(tmp, baselinePath(root)); err != nil {
		return Baseline{}, err
	}
	return cur, nil
}

func load(root string) (*Baseline, error) {
	b, err := os.ReadFile(baselinePath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bl Baseline
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, err
	}
	return &bl, nil
}
