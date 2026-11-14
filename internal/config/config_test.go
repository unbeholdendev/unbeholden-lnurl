package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MinimalConfig(t *testing.T) {
	yaml := `
lnd:
  host: "localhost:10009"
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.LND.Host != "localhost:10009" {
		t.Errorf("expected host localhost:10009, got %s", cfg.LND.Host)
	}
}

func TestLoad_MissingRequiredFields(t *testing.T) {
	yaml := `
lnd:
  host: ""
`
	path := writeTestConfig(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty lnd.host")
	}
}

func TestLoad_WithdrawRequiresSecret(t *testing.T) {
	yaml := `
lnd:
  host: "localhost:10009"
withdraw:
  - id: "tip-001"
    domain: "example.com"
    amount: 1000
`
	path := writeTestConfig(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for withdraw without withdraw_secret")
	}
}

func TestExpandVouchers_TokenNotDerivableFromID(t *testing.T) {
	cfg := Config{
		WithdrawSecret: "0123456789abcdef0123456789abcdef",
		Withdraw:       []WithdrawConfig{{ID: "tip-001", Domain: "example.com", Amount: 1000}},
	}
	token := cfg.ExpandVouchers()[0].Token

	hash := sha256.Sum256([]byte("tip-001"))
	if token == hex.EncodeToString(hash[:6]) {
		t.Fatal("token is derived from the id alone")
	}

	other := cfg
	other.WithdrawSecret = "fedcba9876543210fedcba9876543210"
	if other.ExpandVouchers()[0].Token == token {
		t.Fatal("token does not depend on the secret")
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
