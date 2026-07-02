// Command recordframe captures raw SmartWebSocketV2 frames to a file so we can
// build and unit-test the binary parser against real bytes (lld-explained/A.md:
// capture-first). It does NOT parse the frames — it dumps them verbatim (hex) plus
// length and the leading mode/exchange bytes, which is enough to verify offsets by
// hand against the live app price.
//
// Get tokens from cmd/tokens, then run during market hours (09:15–15:30 IST):
//
//	go run ./cmd/recordframe -tokens "2:58721,1:26017" -n 50 -out future.captured.jsonl
//
// -tokens is a comma list of exchangeType:token (2=NSE_FO future/option, 1=NSE_CM
// index/VIX). Capture one future and one option (two runs, or both in one).
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shindeamul76/optedge-live/internal/broker"
	"github.com/shindeamul76/optedge-live/internal/feed"
)

func main() {
	secretsPath := flag.String("secrets", "secrets.yaml", "path to credentials YAML")
	tokensArg := flag.String("tokens", "", "comma list of exchangeType:token (e.g. 2:58721,1:26017)")
	mode := flag.Int("mode", feed.ModeSnapQuote, "subscription mode (3=SnapQuote)")
	n := flag.Int("n", 50, "number of binary frames to capture before stopping")
	out := flag.String("out", "frames.captured.jsonl", "output file (jsonl, one frame per line)")
	timeout := flag.Duration("timeout", 2*time.Minute, "give up if not enough frames arrive in this time")
	flag.Parse()

	lists, err := parseTokens(*tokensArg)
	if err != nil {
		fatal("parse -tokens", err)
	}

	creds, err := broker.LoadCredentials(*secretsPath)
	if err != nil {
		fatal("load credentials", err)
	}
	sess, err := broker.Login(creds)
	if err != nil {
		fatal("login", err)
	}
	fmt.Println("login OK; dialing stream…")

	cli, err := feed.Dial(feed.Auth{
		JWTToken:   sess.JWTToken,
		APIKey:     sess.APIKey,
		ClientCode: sess.ClientCode,
		FeedToken:  sess.FeedToken,
	})
	if err != nil {
		fatal("dial", err)
	}
	defer cli.Close()
	cli.Heartbeat(25 * time.Second)

	if err := cli.Subscribe("recordframe", *mode, lists); err != nil {
		fatal("subscribe", err)
	}
	fmt.Printf("subscribed mode=%d to %v\n", *mode, *tokensArg)

	f, err := os.Create(*out)
	if err != nil {
		fatal("create out", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	deadline := time.Now().Add(*timeout)
	captured := 0
	for captured < *n {
		if time.Now().After(deadline) {
			fmt.Printf("\ntimeout: captured %d/%d frames (market closed, or no ticks for these tokens)\n", captured, *n)
			break
		}
		_ = cli.SetReadDeadline(deadline)
		mt, data, err := cli.Read()
		if err != nil {
			fatal("read", err)
		}
		switch mt {
		case websocket.BinaryMessage:
			captured++
			rec := frameRecord{
				I:    captured,
				TS:   time.Now().Format(time.RFC3339Nano),
				Type: "binary",
				Len:  len(data),
				Hex:  hex.EncodeToString(data),
			}
			if len(data) >= 2 {
				rec.Mode = int(data[0])
				rec.ExType = int(data[1])
			}
			line, _ := json.Marshal(rec)
			w.Write(line)
			w.WriteByte('\n')
			fmt.Printf("frame %d: len=%d mode=%d exType=%d\n", captured, rec.Len, rec.Mode, rec.ExType)
		case websocket.TextMessage:
			fmt.Printf("text frame: %q\n", string(data)) // expect "pong"
		}
	}

	w.Flush()
	fmt.Printf("\nwrote %d frames to %s\n", captured, *out)
}

type frameRecord struct {
	I      int    `json:"i"`
	TS     string `json:"ts"`
	Type   string `json:"type"`
	Len    int    `json:"len"`
	Mode   int    `json:"mode"`
	ExType int    `json:"exType"`
	Hex    string `json:"hex"`
}

// parseTokens turns "2:58721,1:26017" into per-exchange TokenList groups.
func parseTokens(arg string) ([]feed.TokenList, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, fmt.Errorf("empty; pass -tokens exType:token,... (e.g. 2:58721)")
	}
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
	var lists []feed.TokenList
	for ex, toks := range groups {
		lists = append(lists, feed.TokenList{ExchangeType: ex, Tokens: toks})
	}
	return lists, nil
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", stage, err)
	os.Exit(1)
}
