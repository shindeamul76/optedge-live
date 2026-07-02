package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shindeamul76/optedge/live"
)

const scripMasterURL = "https://margincalculator.angelbroking.com/OpenAPI_File/files/OpenAPIScripMaster.json"

// SmartWebSocketV2 exchangeType codes (the numeric values the subscribe message
// wants). The scrip master uses string segments (ExchSeg) instead, so WSExchangeType
// maps one to the other.
const (
	ExchNSECM = 1 // NSE cash / indices (India VIX, NIFTY 50 index)
	ExchNSEFO = 2 // NSE futures & options (NIFTY future, weekly options)
)

// Instrument is one row of the SmartAPI scrip master, with the fields we need.
// Mirrors optedge's data.Instrument (infra, intentionally duplicated).
type Instrument struct {
	Token          string
	Symbol         string
	Name           string
	Expiry         time.Time // zero for non-expiring instruments (e.g. indices)
	HasExpiry      bool
	Lotsize        int
	InstrumentType string          // e.g. FUTIDX (index future), OPTIDX, AMXIDX (index)
	ExchSeg        string          // e.g. NFO, NSE
	Strike         float64         // option strike in rupees (0 for non-options)
	OptType        live.OptionType // CALL/PUT for options, "" otherwise
}

// WSExchangeType maps an instrument's scrip-master segment to the numeric
// exchangeType the WebSocket subscribe message expects.
func (in Instrument) WSExchangeType() (int, error) {
	switch in.ExchSeg {
	case "NFO":
		return ExchNSEFO, nil
	case "NSE":
		return ExchNSECM, nil
	default:
		return 0, fmt.Errorf("no WS exchangeType mapping for segment %q", in.ExchSeg)
	}
}

// scripRow mirrors the raw master JSON, where every field is a string.
type scripRow struct {
	Token          string `json:"token"`
	Symbol         string `json:"symbol"`
	Name           string `json:"name"`
	Expiry         string `json:"expiry"`
	Strike         string `json:"strike"`
	Lotsize        string `json:"lotsize"`
	InstrumentType string `json:"instrumenttype"`
	ExchSeg        string `json:"exch_seg"`
}

// expiryLayouts are the formats seen in the master's expiry field across versions.
var expiryLayouts = []string{"02Jan2006", "2006-01-02"}

func parseExpiry(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	for _, layout := range expiryLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ParseScripMaster parses the raw master JSON into typed instruments.
func ParseScripMaster(raw []byte) ([]Instrument, error) {
	var rows []scripRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse scrip master: %w", err)
	}
	out := make([]Instrument, 0, len(rows))
	for _, r := range rows {
		exp, hasExp := parseExpiry(r.Expiry)
		lot, _ := strconv.Atoi(r.Lotsize)

		// Options: the master stores strike in paise (e.g. "2200000" = ₹22000) and
		// encodes CE/PE in the symbol suffix.
		var strike float64
		var ot live.OptionType
		if strings.HasSuffix(r.Symbol, "CE") || strings.HasSuffix(r.Symbol, "PE") {
			if s, err := strconv.ParseFloat(r.Strike, 64); err == nil {
				strike = s / 100.0
			}
			if strings.HasSuffix(r.Symbol, "CE") {
				ot = live.Call
			} else {
				ot = live.Put
			}
		}

		out = append(out, Instrument{
			Token:          r.Token,
			Symbol:         r.Symbol,
			Name:           r.Name,
			Expiry:         exp,
			HasExpiry:      hasExp,
			Lotsize:        lot,
			InstrumentType: r.InstrumentType,
			ExchSeg:        r.ExchSeg,
			Strike:         strike,
			OptType:        ot,
		})
	}
	return out, nil
}

// DownloadScripMaster fetches the public scrip master JSON (no auth required).
// The file is large (~10-20 MB), so it uses a generous timeout and one retry.
func DownloadScripMaster() ([]Instrument, error) {
	client := &http.Client{Timeout: 3 * time.Minute}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := fetchScripMaster(client)
		if err != nil {
			lastErr = err
			continue
		}
		return ParseScripMaster(raw)
	}
	return nil, fmt.Errorf("download scrip master: %w", lastErr)
}

func fetchScripMaster(client *http.Client) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scripMasterURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// FrontMonthFuture returns the underlying's front-month index future as of asOf:
// the FUTIDX/NFO contract with the nearest expiry on or after asOf. This is the
// series the backtest used and the live signals must run on (futures, not spot).
func FrontMonthFuture(insts []Instrument, underlying string, asOf time.Time) (Instrument, error) {
	var candidates []Instrument
	for _, in := range insts {
		if in.InstrumentType == "FUTIDX" && in.ExchSeg == "NFO" &&
			strings.EqualFold(in.Name, underlying) && in.HasExpiry &&
			!in.Expiry.Before(asOf) {
			candidates = append(candidates, in)
		}
	}
	if len(candidates) == 0 {
		return Instrument{}, fmt.Errorf("no %s FUTIDX future expiring on/after %s",
			underlying, asOf.Format("2006-01-02"))
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Expiry.Before(candidates[j].Expiry)
	})
	return candidates[0], nil
}

// NearestOptionExpiry returns the earliest underlying OPTIDX expiry on or after
// asOf — the current weekly expiry. The live chain and the engine's calendar must
// agree on this (see lld-explained/C.md).
func NearestOptionExpiry(insts []Instrument, underlying string, asOf time.Time) (time.Time, error) {
	var best time.Time
	found := false
	for _, in := range insts {
		if in.InstrumentType == "OPTIDX" && in.ExchSeg == "NFO" &&
			strings.EqualFold(in.Name, underlying) && in.HasExpiry && !in.Expiry.Before(asOf) {
			if !found || in.Expiry.Before(best) {
				best, found = in.Expiry, true
			}
		}
	}
	if !found {
		return time.Time{}, fmt.Errorf("no %s OPTIDX expiry on/after %s", underlying, asOf.Format("2006-01-02"))
	}
	return best, nil
}

// FindOption locates the NFO index option matching underlying/expiry/strike/type.
func FindOption(insts []Instrument, underlying string, expiry time.Time, strike float64, ot live.OptionType) (Instrument, error) {
	y, m, d := expiry.Date()
	for _, in := range insts {
		if in.InstrumentType == "OPTIDX" && in.ExchSeg == "NFO" &&
			strings.EqualFold(in.Name, underlying) && in.OptType == ot && in.HasExpiry {
			ey, em, ed := in.Expiry.Date()
			if ey == y && em == m && ed == d && math.Abs(in.Strike-strike) < 0.01 {
				return in, nil
			}
		}
	}
	return Instrument{}, fmt.Errorf("no %s %s %.0f expiring %s", underlying, ot, strike, expiry.Format("2006-01-02"))
}

// FindIndiaVIX locates the India VIX index instrument. It is a non-expiring NSE
// index; match by name/symbol containing "VIX".
func FindIndiaVIX(insts []Instrument) (Instrument, error) {
	for _, in := range insts {
		if in.ExchSeg == "NSE" && !in.HasExpiry &&
			(strings.Contains(strings.ToUpper(in.Name), "VIX") ||
				strings.Contains(strings.ToUpper(in.Symbol), "VIX")) {
			return in, nil
		}
	}
	return Instrument{}, fmt.Errorf("India VIX not found in scrip master")
}

// ATMStrikes returns the 2N+1 strikes centered on the ATM for spot at the given
// strike step: [ATM-N*step … ATM … ATM+N*step]. ATM = round(spot/step)*step.
func ATMStrikes(spot, step float64, n int) []float64 {
	atm := math.Round(spot/step) * step
	out := make([]float64, 0, 2*n+1)
	for i := -n; i <= n; i++ {
		out = append(out, atm+float64(i)*step)
	}
	return out
}
