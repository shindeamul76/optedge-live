package broker

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP computes an RFC 6238 time-based one-time password from a base32 secret,
// using the SmartAPI defaults: HMAC-SHA1, 30-second step, 6 digits. The secret is
// the seed you get when enabling TOTP for the API account.
//
// t is the moment to generate the code for (use time.Now()). digits/period are
// configurable for testing against the RFC vectors; pass 6 / 30s for SmartAPI.
func TOTP(secret string, t time.Time, digits int, period time.Duration) (string, error) {
	key, err := decodeBase32Secret(secret)
	if err != nil {
		return "", err
	}

	// Counter = number of whole periods since the Unix epoch.
	counter := uint64(t.Unix()) / uint64(period.Seconds())

	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	// Dynamic truncation (RFC 4226 §5.3).
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod), nil
}

// SmartAPITOTP is the SmartAPI-specific convenience: 6 digits, 30-second step.
func SmartAPITOTP(secret string, t time.Time) (string, error) {
	return TOTP(secret, t, 6, 30*time.Second)
}

// decodeBase32Secret accepts a base32 secret with or without padding and with the
// spaces/hyphens that setup pages and authenticator apps use for grouping,
// case-insensitively. The TOTP seed is base32: only A-Z and 2-7 are valid.
func decodeBase32Secret(secret string) ([]byte, error) {
	r := strings.NewReplacer(" ", "", "-", "")
	s := strings.ToUpper(r.Replace(secret))
	if s == "" {
		return nil, fmt.Errorf("totp_secret is empty")
	}
	// Pad to a multiple of 8 so the standard decoder accepts it.
	if m := len(s) % 8; m != 0 {
		s += strings.Repeat("=", 8-m)
	}
	key, err := base32.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("totp_secret is not valid base32 (allowed chars A-Z and 2-7): "+
			"it must be the TOTP *seed* from smartapi.angelbroking.com/enable-totp, "+
			"not the 6-digit code, API key, or PIN: %w", err)
	}
	return key, nil
}
