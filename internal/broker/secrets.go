// Package broker is optedge-live's Angel One infrastructure: credentials, TOTP,
// SmartAPI login, and scrip-master instrument resolution.
//
// It is deliberately reimplemented here rather than imported from the frozen
// optedge engine. Login and instrument lookup are INFRA, not trade-decision code,
// so duplicating them keeps the engine untouched and carries zero parity impact —
// nothing here can change which trades fire. (The optedge data package is also
// under internal/ and not importable from this separate module anyway.)
package broker

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Credentials are the Angel One SmartAPI login secrets. Load from a gitignored
// YAML file (see secrets.example.yaml); never commit real values. Same four fields
// as optedge — reuse the same account.
type Credentials struct {
	APIKey     string `yaml:"api_key"`
	ClientCode string `yaml:"client_code"`
	PIN        string `yaml:"pin"`
	// TOTPSecret is the base32 seed shown when you enable TOTP for the API. The
	// client derives the 6-digit code from it on each login, so an unattended
	// market-hours run never needs a hand-typed code.
	TOTPSecret string `yaml:"totp_secret"`
}

// LoadCredentials reads and validates a Credentials YAML file.
func LoadCredentials(path string) (Credentials, error) {
	var c Credentials
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read credentials %q: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse credentials %q: %w", path, err)
	}
	if c.APIKey == "" || c.ClientCode == "" || c.PIN == "" || c.TOTPSecret == "" {
		return c, fmt.Errorf("credentials %q missing one of api_key/client_code/pin/totp_secret", path)
	}
	return c, nil
}
