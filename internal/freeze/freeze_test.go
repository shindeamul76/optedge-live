package freeze

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loc(t *testing.T) *time.Location {
	t.Helper()
	l, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	return l
}

func TestConfigHash_Deterministic(t *testing.T) {
	l := loc(t)
	h1, err := ConfigHash(l, 0)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h2, _ := ConfigHash(l, 0)
	if h1 == "" || h1 != h2 {
		t.Errorf("hash not stable: %q vs %q", h1, h2)
	}
}

func TestConfigHash_LotOverrideChangesHash(t *testing.T) {
	l := loc(t)
	base, _ := ConfigHash(l, 0)  // engine default (75)
	same, _ := ConfigHash(l, 75) // explicit 75 == default
	diff, _ := ConfigHash(l, 65) // real live lot
	if base != same {
		t.Errorf("explicit-default lot changed hash: %q vs %q", base, same)
	}
	if base == diff {
		t.Error("lot override (65) did not change the config hash")
	}
}

func TestCheck_FirstRunThenMatch(t *testing.T) {
	l := loc(t)
	root := t.TempDir()

	st, err := Check(root, l, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !st.FirstRun {
		t.Fatal("expected FirstRun on a clean dir")
	}

	bl, err := Start(root, l, "2026-06-30", 0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if bl.ConfigHash == "" || bl.ClockStart != "2026-06-30" {
		t.Errorf("baseline = %+v", bl)
	}

	st2, _ := Check(root, l, 0)
	if st2.FirstRun || !st2.Match {
		t.Errorf("after Start: FirstRun=%v Match=%v, want false/true", st2.FirstRun, st2.Match)
	}
	if st2.Baseline == nil || st2.Baseline.ClockStart != "2026-06-30" {
		t.Errorf("stored baseline not read back: %+v", st2.Baseline)
	}
}

func TestCheck_DetectsDrift(t *testing.T) {
	l := loc(t)
	root := t.TempDir()
	if _, err := Start(root, l, "2026-06-30", 0); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Tamper with the stored baseline (simulate a param/engine change on restart).
	tampered := Baseline{ConfigHash: "deadbeef", EngineVersion: "v0.0.0", ClockStart: "2026-06-30"}
	b, _ := json.MarshalIndent(tampered, "", "  ")
	if err := os.WriteFile(baselinePath(root), b, 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	st, _ := Check(root, l, 0)
	if st.Match {
		t.Error("expected drift detection (Match=false) after baseline change")
	}
}
