package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shindeamul76/optedge-live/internal/store"
)

func TestBuildSnapshot_EquityAndDivergence(t *testing.T) {
	root := t.TempDir()
	p, err := store.NewPaths(root, "2026-06-30")
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	aw, _ := store.OpenAudit(p)
	// Two trades: net +500 (synth +400 → div +100) and net -200 (synth -150 → div -50).
	aw.Append(store.AuditRecord{Runner: "ORB", Day: "2026-06-30", Dir: "LONG", OptType: "CALL",
		Strike: 23050, EntryTS: time.Now(), ExitTS: time.Now(), EntryFill: 102, ExitFill: 110,
		NetRupees: 500, NetSynthRupees: 400, Reason: "TARGET"})
	aw.Append(store.AuditRecord{Runner: "VWAP", Day: "2026-06-30", Dir: "SHORT", OptType: "PUT",
		Strike: 23000, EntryTS: time.Now(), ExitTS: time.Now(), EntryFill: 90, ExitFill: 85,
		NetRupees: -200, NetSynthRupees: -150, Reason: "STOP"})
	aw.Close()

	s := BuildSnapshot(root, 50000, nil, "connected", 23050)

	if s.Days != 1 || len(s.Trades) != 2 {
		t.Fatalf("days=%d trades=%d, want 1/2", s.Days, len(s.Trades))
	}
	if s.Equity != 50000+300 { // +500 -200
		t.Errorf("equity = %.0f, want 50300", s.Equity)
	}
	if len(s.EquityCurve) != 3 || s.EquityCurve[0] != 50000 {
		t.Errorf("equity curve = %v, want [50000, ...] len 3", s.EquityCurve)
	}
	// Trades are in insertion order; first has +100 divergence.
	if s.Trades[0].Divergence != 100 {
		t.Errorf("trade[0] divergence = %.0f, want 100", s.Trades[0].Divergence)
	}
	if s.Gate.NTrades != 2 || s.Gate.MinSampleMet {
		t.Errorf("gate n=%d minMet=%v, want 2/false", s.Gate.NTrades, s.Gate.MinSampleMet)
	}
	if s.ConnState != "connected" || s.ChainCenter != 23050 {
		t.Errorf("live bits: conn=%s chain=%.0f", s.ConnState, s.ChainCenter)
	}
}

func TestServer_ServesPageAndState(t *testing.T) {
	srv := NewServer(func() Snapshot {
		return Snapshot{ConnState: "connected", Equity: 50123, Updated: "10:00:00"}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The embedded page.
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	body := readAll(t, res)
	if res.StatusCode != 200 || !strings.Contains(body, "optedge-live") {
		t.Errorf("index page: status %d, contains title? %v", res.StatusCode, strings.Contains(body, "optedge-live"))
	}

	// The JSON snapshot.
	res, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("get /api/state: %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal([]byte(readAll(t, res)), &got); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got.ConnState != "connected" || got.Equity != 50123 {
		t.Errorf("state = %+v", got)
	}
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
