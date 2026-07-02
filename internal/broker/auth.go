package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	smartAPIRoot = "https://apiconnect.angelone.in"
	routeLogin   = "/rest/auth/angelbroking/user/v1/loginByPassword"
)

// Session holds the authenticated tokens from a SmartAPI login. Unlike the optedge
// fetch client (which kept only the REST jwt), this KEEPS the feedToken and the
// api/client identifiers too — the SmartWebSocketV2 handshake needs all four:
// Authorization=jwtToken, x-api-key, x-client-code, x-feed-token. That gap in the
// original Login is exactly why login is reimplemented here.
type Session struct {
	APIKey       string
	ClientCode   string
	JWTToken     string
	RefreshToken string
	FeedToken    string
}

// envelope is the standard SmartAPI response wrapper.
type envelope struct {
	Status    bool            `json:"status"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"errorcode"`
	Data      json.RawMessage `json:"data"`
}

// Login authenticates with PIN + a freshly-generated TOTP and returns a Session
// carrying the jwt and feed tokens.
func Login(creds Credentials) (Session, error) {
	code, err := SmartAPITOTP(creds.TOTPSecret, time.Now())
	if err != nil {
		return Session{}, err
	}
	body := map[string]string{
		"clientcode": creds.ClientCode,
		"password":   creds.PIN,
		"totp":       code,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Session{}, err
	}

	req, err := http.NewRequest(http.MethodPost, smartAPIRoot+routeLogin, bytes.NewReader(raw))
	if err != nil {
		return Session{}, err
	}
	setAuthHeaders(req.Header, creds.APIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return Session{}, fmt.Errorf("parse login response (http %d): %w: %s", resp.StatusCode, err, truncate(respBody))
	}
	if !env.Status {
		return Session{}, fmt.Errorf("login failed: %s (%s)", env.Message, env.ErrorCode)
	}

	var tokens struct {
		JWTToken     string `json:"jwtToken"`
		RefreshToken string `json:"refreshToken"`
		FeedToken    string `json:"feedToken"`
	}
	if err := json.Unmarshal(env.Data, &tokens); err != nil {
		return Session{}, fmt.Errorf("parse login tokens: %w", err)
	}
	if tokens.JWTToken == "" || tokens.FeedToken == "" {
		return Session{}, fmt.Errorf("login succeeded but missing jwtToken/feedToken")
	}

	return Session{
		APIKey:       creds.APIKey,
		ClientCode:   creds.ClientCode,
		JWTToken:     tokens.JWTToken,
		RefreshToken: tokens.RefreshToken,
		FeedToken:    tokens.FeedToken,
	}, nil
}

// setAuthHeaders applies the headers SmartAPI requires on the login request.
// SmartAPI does not strictly validate the client IP/MAC, so they are best-effort.
func setAuthHeaders(h http.Header, apiKey string) {
	ip := localIP()
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("X-UserType", "USER")
	h.Set("X-SourceID", "WEB")
	h.Set("X-ClientLocalIP", ip)
	h.Set("X-ClientPublicIP", ip)
	h.Set("X-MACAddress", "00:00:00:00:00:00")
	h.Set("X-PrivateKey", apiKey)
}

// localIP returns the machine's outbound local IP (best effort; falls back to
// loopback). No packets are sent — a UDP socket only resolves the route.
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func truncate(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
