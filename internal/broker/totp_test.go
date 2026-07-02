package broker

import (
	"testing"
	"time"
)

// TestTOTP_RFC6238 checks the SHA1 implementation against the canonical RFC 6238
// test vector: the ASCII seed "12345678901234567890" at T=59s yields 287082
// (6 digits). This pins the HMAC/truncation math we reimplemented.
func TestTOTP_RFC6238(t *testing.T) {
	// base32 of ASCII "12345678901234567890".
	const seed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	got, err := TOTP(seed, time.Unix(59, 0), 6, 30*time.Second)
	if err != nil {
		t.Fatalf("TOTP: %v", err)
	}
	if want := "287082"; got != want {
		t.Fatalf("TOTP at T=59 = %s, want %s", got, want)
	}
}

// TestDecodeBase32Secret_Lenient confirms the decoder tolerates the spacing,
// hyphens, lowercase, and missing padding that real authenticator setup pages show.
func TestDecodeBase32Secret_Lenient(t *testing.T) {
	variants := []string{
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		"gezd gnbv gy3t qojq gezd gnbv gy3t qojq",
		"GEZD-GNBV-GY3T-QOJQ-GEZD-GNBV-GY3T-QOJQ",
	}
	for _, v := range variants {
		if _, err := decodeBase32Secret(v); err != nil {
			t.Errorf("decode %q: %v", v, err)
		}
	}
	if _, err := decodeBase32Secret(""); err == nil {
		t.Error("empty secret should error")
	}
}
