// Package feed is the live market-data feed over Angel One SmartWebSocketV2.
//
// This file is the minimal connection layer the capture tool (cmd/recordframe)
// needs: dial with the auth headers, send a subscribe control message, run the
// application-level "ping" heartbeat, and read raw frames. The binary frame parser
// (mode 3 / SnapQuote) is written AFTER we have a captured fixture to test it
// against — see lld-explained/A.md. Reconnect/state-machine come later too.
package feed

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSURL is the SmartWebSocketV2 stream endpoint.
const WSURL = "wss://smartapisocket.angelone.in/smart-stream"

// Subscription modes.
const (
	ModeLTP       = 1
	ModeQuote     = 2
	ModeSnapQuote = 3 // best-5 bid/ask — what the FillModel needs (locked decision)
)

// Auth carries the four values the SmartWebSocketV2 handshake requires. The
// Authorization header is the raw jwtToken (no "Bearer " prefix for the stream).
type Auth struct {
	JWTToken   string
	APIKey     string
	ClientCode string
	FeedToken  string
}

// Conn is the transport the Supervisor drives. *Client is the real implementation;
// tests inject a fake. It is exactly the surface the reconnect/heartbeat state
// machine needs — nothing more.
type Conn interface {
	Subscribe(correlationID string, mode int, lists []TokenList) error
	Unsubscribe(correlationID string, mode int, lists []TokenList) error
	Heartbeat(d time.Duration)
	Read() (messageType int, data []byte, err error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// DialConn is the production Dialer: it opens a real *Client as a Conn. Returning a
// typed nil through the interface is avoided by the explicit error branch.
func DialConn(a Auth) (Conn, error) {
	c, err := Dial(a)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Client is a single SmartWebSocketV2 connection. It is not safe for concurrent
// reads, but writes (subscribe + heartbeat) are serialized internally.
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	stop    chan struct{}
}

// Dial opens and authenticates the stream connection.
func Dial(a Auth) (*Client, error) {
	h := http.Header{}
	h.Set("Authorization", a.JWTToken)
	h.Set("x-api-key", a.APIKey)
	h.Set("x-client-code", a.ClientCode)
	h.Set("x-feed-token", a.FeedToken)

	conn, resp, err := websocket.DefaultDialer.Dial(WSURL, h)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("ws dial: %w (http %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	return &Client{conn: conn, stop: make(chan struct{})}, nil
}

// TokenList is one exchange's token group inside a subscribe request.
type TokenList struct {
	ExchangeType int      `json:"exchangeType"`
	Tokens       []string `json:"tokens"`
}

// Subscribe sends an action=1 (subscribe) control message for the given mode and
// token groups.
func (c *Client) Subscribe(correlationID string, mode int, lists []TokenList) error {
	return c.control(correlationID, 1, mode, lists)
}

// Unsubscribe sends an action=0 (unsubscribe) control message — used when the chain
// window drops far strikes as spot drifts or the weekly rolls.
func (c *Client) Unsubscribe(correlationID string, mode int, lists []TokenList) error {
	return c.control(correlationID, 0, mode, lists)
}

func (c *Client) control(correlationID string, action, mode int, lists []TokenList) error {
	msg := map[string]any{
		"correlationID": correlationID,
		"action":        action,
		"params": map[string]any{
			"mode":      mode,
			"tokenList": lists,
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

// Heartbeat starts a goroutine sending the literal "ping" TEXT frame every d. Angel
// One uses an application-level heartbeat (text "ping" → server replies "pong"),
// not a WebSocket protocol ping; a silent socket is dropped as idle. Stops on Close.
func (c *Client) Heartbeat(d time.Duration) {
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.writeMu.Lock()
				err := c.conn.WriteMessage(websocket.TextMessage, []byte("ping"))
				c.writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
}

// Read returns the next message. messageType is websocket.BinaryMessage for market
// ticks and websocket.TextMessage for the "pong" heartbeat reply / control text.
func (c *Client) Read() (messageType int, data []byte, err error) {
	return c.conn.ReadMessage()
}

// SetReadDeadline bounds how long the next Read may block. A zero time clears it.
func (c *Client) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// Close stops the heartbeat and closes the connection.
func (c *Client) Close() error {
	select {
	case <-c.stop:
		// already closed
	default:
		close(c.stop)
	}
	return c.conn.Close()
}
