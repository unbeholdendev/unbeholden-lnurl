package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const lndYAML = `
lnd:
  host: "localhost:10009"
  tls_cert: "/tls.cert"
  macaroon: "/invoice.macaroon"
`

func TestLoad_MinimalConfig(t *testing.T) {
	yaml := lndYAML + `
listen: "127.0.0.1:8080"
addresses:
  "*":
    min_sendable: 1000
    max_sendable: 2000
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
	yaml := lndYAML + `
listen: "127.0.0.1:8080"
state_dir: "/var/lib/x"
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

func TestLoad_RejectsIncompleteFeatures(t *testing.T) {
	cases := map[string]string{
		"no features": "",
		"addresses without listen": `
addresses:
  "*":
    min_sendable: 1000
    max_sendable: 2000
`,
		"notifications without state_dir": `
notifications:
  ntfy:
    url: "https://ntfy.example.com"
    topic: "t"
`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTestConfig(t, lndYAML+extra)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoad_NotificationsOnly(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tk_secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeTestConfig(t, lndYAML+`
state_dir: "/var/lib/x"
notifications:
  ntfy:
    url: "https://ntfy.example.com"
    topic: "t"
    token_file: "`+tokenFile+`"
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ServesHTTP() {
		t.Error("serves HTTP without addresses or withdraw")
	}
	if got := cfg.Notifications.Ntfy.Token; got != "tk_secret" {
		t.Errorf("token = %q, want tk_secret", got)
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

func TestWithdrawK1Key_DiffersFromTokens(t *testing.T) {
	cfg := Config{WithdrawSecret: "0123456789abcdef0123456789abcdef"}
	key := hex.EncodeToString(cfg.WithdrawK1Key())
	if len(key) != 64 {
		t.Fatalf("key length = %d hex chars, want 64", len(key))
	}
	if tokenForVoucher(cfg.WithdrawSecret, "unbeholden-lnurl withdraw k1") == key[:32] {
		t.Fatal("k1 key equals a voucher token")
	}
	other := Config{WithdrawSecret: "fedcba9876543210fedcba9876543210"}
	if hex.EncodeToString(other.WithdrawK1Key()) == key {
		t.Fatal("k1 key does not depend on the secret")
	}
}

func TestLoad_TrustedProxies(t *testing.T) {
	cfg, err := Load(writeTestConfig(t, lndYAML+`
state_dir: "/var/lib/x"
notifications:
  ntfy:
    url: "https://ntfy.example.com"
    topic: "t"
trusted_proxies: ["10.0.0.0/8", "192.0.2.7", "::ffff:198.51.100.1", "fd00::/8"]
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.TrustedProxies {
		got = append(got, p.String())
	}
	if want := []string{"10.0.0.0/8", "192.0.2.7/32", "198.51.100.1/32", "fd00::/8"}; !slices.Equal(got, want) {
		t.Fatalf("trusted proxies = %v, want %v", got, want)
	}

	for _, bad := range []string{"proxy.example.com", "10.0.0.0/33", ""} {
		if _, err := Load(writeTestConfig(t, lndYAML+`
state_dir: "/var/lib/x"
notifications:
  ntfy:
    url: "https://ntfy.example.com"
    topic: "t"
trusted_proxies: ["`+bad+`"]
`)); err == nil {
			t.Errorf("trusted proxy %q accepted", bad)
		}
	}
}

func TestLoad_NeedsExactlyOneNode(t *testing.T) {
	const notify = `
state_dir: "/var/lib/x"
notifications:
  ntfy:
    url: "https://ntfy.example.com"
    topic: "t"
`
	const clnYAML = `
cln:
  url: "https://cln:3010"
  rune_file: "/rune"
`
	cfg, err := Load(writeTestConfig(t, clnYAML+notify))
	if err != nil {
		t.Fatalf("load cln config: %v", err)
	}
	if cfg.CLN.URL != "https://cln:3010" || cfg.LND != nil {
		t.Errorf("cln = %+v, lnd = %+v", cfg.CLN, cfg.LND)
	}

	cases := map[string]string{
		"no node":          notify,
		"both nodes":       lndYAML + clnYAML + notify,
		"cln without rune": "cln:\n  url: \"https://cln:3010\"\n" + notify,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTestConfig(t, yaml)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoad_AddressImage(t *testing.T) {
	dir := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	load := func(path string) (*Config, error) {
		return Load(writeTestConfig(t, lndYAML+`
listen: "127.0.0.1:8080"
addresses:
  bob:
    min_sendable: 1000
    max_sendable: 2000
    image_file: "`+path+`"
`))
	}

	cfg, err := load(write("avatar.png", png))
	if err != nil {
		t.Fatal(err)
	}
	if bob := cfg.Addresses["bob"]; bob.ImageType != "image/png;base64" || bob.Image != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("bob = %+v", bob)
	}
	for name, data := range map[string][]byte{
		"text.png":  []byte("not an image"),
		"large.png": append(png, make([]byte, 110_000)...),
	} {
		if _, err := load(write(name, data)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := load(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("missing file: loaded")
	}
}

func TestLoad_SuccessMessageLength(t *testing.T) {
	load := func(message string) error {
		_, err := Load(writeTestConfig(t, lndYAML+`
listen: "127.0.0.1:8080"
addresses:
  bob:
    min_sendable: 1000
    max_sendable: 2000
    success_message: "`+message+`"
`))
		return err
	}
	if err := load(strings.Repeat("é", 144)); err != nil {
		t.Fatalf("144 characters: %v", err)
	}
	if err := load(strings.Repeat("x", 145)); err == nil {
		t.Fatal("145 characters: loaded")
	}
}

func TestVerifyKey_FromNodeCredential(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	lnd := &Config{LND: &LNDConfig{Macaroon: write("macaroon", "m")}}
	cln := &Config{CLN: &CLNConfig{RuneFile: write("rune", "r")}}

	lndKey, err := lnd.VerifyKey()
	if err != nil {
		t.Fatal(err)
	}
	clnKey, err := cln.VerifyKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(lndKey) == string(clnKey) {
		t.Fatal("keys from different credentials match")
	}
}
