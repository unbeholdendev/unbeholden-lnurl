package config

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type Config struct {
	VerifyKeyFile      string                  `yaml:"verify_key_file,omitempty"`
	OutboundProxy      string                  `yaml:"outbound_proxy,omitempty"`
	ProxyURL           *url.URL                `yaml:"-"`
	Listen             string                  `yaml:"listen,omitempty"`
	TrustedProxies     []Prefix                `yaml:"trusted_proxies,omitempty"`
	RateLimit          RateLimit               `yaml:"rate_limit,omitempty"`
	LND                *LNDConfig              `yaml:"lnd,omitempty"`
	CLN                *CLNConfig              `yaml:"cln,omitempty"`
	StateDir           string                  `yaml:"state_dir,omitempty"`
	Notifications      NotificationsConfig     `yaml:"notifications,omitempty"`
	WithdrawSecretFile string                  `yaml:"withdraw_secret_file,omitempty"`
	WithdrawSecret     string                  `yaml:"withdraw_secret,omitempty"`
	Withdraw           []WithdrawConfig        `yaml:"withdraw,omitempty"`
	Addresses          map[string]AddressEntry `yaml:"addresses,omitempty"`
	Nostr              *NostrConfig            `yaml:"nostr,omitempty"`
}

// NostrConfig turns on zaps (NIP-57) for every address.
type NostrConfig struct {
	PrivateKeyFile string `yaml:"private_key_file"`
	// PrivateKey is read from PrivateKeyFile.
	PrivateKey string `yaml:"-"`
}

// Prefix is an IP address range, written as CIDR or as a single address.
type Prefix struct{ netip.Prefix }

func (p *Prefix) UnmarshalText(text []byte) error {
	s := string(text)
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		addr, addrErr := netip.ParseAddr(s)
		if addrErr != nil {
			return fmt.Errorf("trusted_proxies: %q is not an IP address or CIDR range", s)
		}
		addr = addr.Unmap()
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	p.Prefix = prefix.Masked()
	return nil
}

// RateLimit bounds requests per client address: Burst at once, then
// PerMinute a minute.
type RateLimit struct {
	PerMinute int `yaml:"per_minute"`
	Burst     int `yaml:"burst"`
}

// DefaultRateLimit lets a client poll once a second.
var DefaultRateLimit = RateLimit{PerMinute: 60, Burst: 30}

type LNDConfig struct {
	Host     string `yaml:"host"`
	TLSCert  string `yaml:"tls_cert"`
	Macaroon string `yaml:"macaroon"`
}

type CLNConfig struct {
	URL      string `yaml:"url"`
	RuneFile string `yaml:"rune_file"`
	CACert   string `yaml:"ca_cert,omitempty"`
}

type NotificationsConfig struct {
	Ntfy *NtfyConfig `yaml:"ntfy,omitempty"`
}

type NtfyConfig struct {
	URL       string `yaml:"url"`
	Topic     string `yaml:"topic"`
	Token     string `yaml:"token,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
}

type WithdrawConfig struct {
	ID          string `yaml:"id,omitempty"`
	Prefix      string `yaml:"prefix,omitempty"`
	Count       int    `yaml:"count,omitempty"`
	Domain      string `yaml:"domain"`
	Description string `yaml:"description"`
	Amount      int64  `yaml:"amount"`
}

type AddressEntry struct {
	Description     string `yaml:"description"`
	LongDescription string `yaml:"long_description,omitempty"`
	ImageFile       string `yaml:"image_file,omitempty"`
	SuccessMessage  string `yaml:"success_message,omitempty"`
	MinSendable     int64  `yaml:"min_sendable"`
	MaxSendable     int64  `yaml:"max_sendable"`

	// ImageType and Image are the LUD-06 metadata entry read from ImageFile.
	ImageType string `yaml:"-"`
	Image     string `yaml:"-"`
}

// Voucher is an expanded single withdraw voucher (after batch expansion).
type Voucher struct {
	ID          string
	Token       string // URL-safe opaque token derived from ID and secret
	Domain      string
	Description string
	Amount      int64 // millisatoshis
}

const (
	minWithdrawSecretLen = 32
	maxImageLen          = 136536
	maxSuccessMessageLen = 144
)

// tokenForVoucher generates a deterministic URL token from a voucher ID,
// unguessable without the secret.
func tokenForVoucher(secret, id string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// WithdrawK1Key returns the key for signing withdraw k1 values. It is keyed
// differently from voucher tokens, so a token never reveals it.
func (c *Config) WithdrawK1Key() []byte {
	mac := hmac.New(sha256.New, []byte("unbeholden-lnurl withdraw k1"))
	mac.Write([]byte(c.WithdrawSecret))
	return mac.Sum(nil)
}

// VerifyKey returns the key for signing LUD-21 verify URLs. Without a
// verify_key_file, it uses the node credential for backwards compatibility.
func (c *Config) VerifyKey() ([]byte, error) {
	path := c.VerifyKeyFile
	if path == "" {
		if c.LND != nil {
			path = c.LND.Macaroon
		} else {
			path = c.CLN.RuneFile
		}
	}
	credential, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read verify key source: %w", err)
	}
	if len(credential) == 0 {
		return nil, fmt.Errorf("verify key source is empty")
	}
	mac := hmac.New(sha256.New, []byte("unbeholden-lnurl verify"))
	mac.Write(credential)
	return mac.Sum(nil), nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Config{RateLimit: DefaultRateLimit}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("parse config: expected one YAML document")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if cfg.WithdrawSecretFile != "" {
		secret, err := os.ReadFile(cfg.WithdrawSecretFile)
		if err != nil {
			return nil, fmt.Errorf("read withdraw_secret_file: %w", err)
		}
		cfg.WithdrawSecret = strings.TrimSpace(string(secret))
		if len(cfg.Withdraw) > 0 && len(cfg.WithdrawSecret) < minWithdrawSecretLen {
			return nil, fmt.Errorf("withdraw_secret_file must contain at least %d characters", minWithdrawSecretLen)
		}
	}
	if n := cfg.Nostr; n != nil {
		key, err := os.ReadFile(n.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read nostr.private_key_file: %w", err)
		}
		n.PrivateKey = strings.TrimSpace(string(key))
	}
	if n := cfg.Notifications.Ntfy; n != nil && n.TokenFile != "" {
		token, err := os.ReadFile(n.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read notifications.ntfy.token_file: %w", err)
		}
		n.Token = strings.TrimSpace(string(token))
	}
	for name, addr := range cfg.Addresses {
		if addr.ImageFile == "" {
			continue
		}
		if err := addr.loadImage(); err != nil {
			return nil, fmt.Errorf("addresses.%s.image_file: %w", name, err)
		}
		cfg.Addresses[name] = addr
	}
	return &cfg, nil
}

func (a *AddressEntry) loadImage() error {
	data, err := os.ReadFile(a.ImageFile)
	if err != nil {
		return err
	}
	contentType := http.DetectContentType(data)
	if contentType != "image/png" && contentType != "image/jpeg" {
		return fmt.Errorf("must be PNG or JPEG, not %s", contentType)
	}
	a.ImageType = contentType + ";base64"
	a.Image = base64.StdEncoding.EncodeToString(data)
	if len(a.Image) > maxImageLen {
		return fmt.Errorf("must be at most 100 KB")
	}
	return nil
}

func (c *Config) Validate() error {
	if c.OutboundProxy != "" {
		proxy, err := url.Parse(c.OutboundProxy)
		if err != nil || proxy.Scheme != "socks5" || proxy.Hostname() == "" || proxy.Port() == "" || proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" {
			return fmt.Errorf("outbound_proxy must be a socks5://host:port URL")
		}
		c.ProxyURL = proxy
	}
	if (c.LND == nil) == (c.CLN == nil) {
		return fmt.Errorf("configure one node: lnd or cln")
	}
	if l := c.LND; l != nil && (l.Host == "" || l.TLSCert == "" || l.Macaroon == "") {
		return fmt.Errorf("lnd needs host, tls_cert and macaroon")
	}
	if n := c.CLN; n != nil && (n.URL == "" || n.RuneFile == "") {
		return fmt.Errorf("cln needs url and rune_file")
	}
	if !c.ServesHTTP() && !c.Notifies() {
		return fmt.Errorf("nothing to do: configure addresses, withdraw or notifications")
	}
	if c.ServesHTTP() && c.Listen == "" {
		return fmt.Errorf("listen is required with addresses or withdraw vouchers")
	}
	if c.RateLimit.PerMinute <= 0 || c.RateLimit.Burst <= 0 {
		return fmt.Errorf("rate_limit.per_minute and rate_limit.burst must be positive")
	}
	if (c.Notifies() || len(c.Withdraw) > 0 || c.Nostr != nil) && c.StateDir == "" {
		return fmt.Errorf("state_dir is required with notifications, withdraw vouchers or nostr")
	}
	if n := c.Nostr; n != nil {
		if len(c.Addresses) == 0 {
			return fmt.Errorf("nostr needs addresses")
		}
		if n.PrivateKeyFile == "" {
			return fmt.Errorf("nostr.private_key_file is required")
		}
	}
	if n := c.Notifications.Ntfy; n != nil {
		if n.URL == "" || n.Topic == "" {
			return fmt.Errorf("notifications.ntfy needs url and topic")
		}
		if n.Token != "" && n.TokenFile != "" {
			return fmt.Errorf("notifications.ntfy: set token or token_file, not both")
		}
	}
	if c.WithdrawSecret != "" && c.WithdrawSecretFile != "" {
		return fmt.Errorf("set withdraw_secret or withdraw_secret_file, not both")
	}
	if len(c.Withdraw) > 0 && c.WithdrawSecretFile == "" && len(c.WithdrawSecret) < minWithdrawSecretLen {
		return fmt.Errorf("withdraw_secret must be at least %d characters when withdraw vouchers are configured", minWithdrawSecretLen)
	}
	for i, w := range c.Withdraw {
		if w.Domain == "" {
			return fmt.Errorf("withdraw[%d].domain is required", i)
		}
		if w.ID == "" && w.Prefix == "" {
			return fmt.Errorf("withdraw[%d]: must have either id or prefix", i)
		}
		if w.ID != "" && w.Prefix != "" {
			return fmt.Errorf("withdraw[%d]: cannot have both id and prefix", i)
		}
		if w.Prefix != "" && w.Count <= 0 {
			return fmt.Errorf("withdraw[%d]: prefix requires count > 0", i)
		}
		if w.Amount <= 0 {
			return fmt.Errorf("withdraw[%d]: amount must be positive", i)
		}
	}
	for name, addr := range c.Addresses {
		if addr.MinSendable <= 0 {
			return fmt.Errorf("addresses.%s.min_sendable must be positive", name)
		}
		if addr.MaxSendable <= 0 {
			return fmt.Errorf("addresses.%s.max_sendable must be positive", name)
		}
		if addr.MinSendable > addr.MaxSendable {
			return fmt.Errorf("addresses.%s.min_sendable must be <= max_sendable", name)
		}
		if utf8.RuneCountInString(addr.SuccessMessage) > maxSuccessMessageLen {
			return fmt.Errorf("addresses.%s.success_message must be at most %d characters", name, maxSuccessMessageLen)
		}
	}
	return nil
}

// ServesHTTP reports whether any configured feature needs the HTTP listener.
func (c *Config) ServesHTTP() bool {
	return len(c.Addresses) > 0 || len(c.Withdraw) > 0
}

// Notifies reports whether any notification channel is configured.
func (c *Config) Notifies() bool {
	return c.Notifications.Ntfy != nil
}

// ExpandVouchers expands batch withdraw entries into individual Voucher structs.
func (c *Config) ExpandVouchers() []Voucher {
	var vouchers []Voucher
	for _, w := range c.Withdraw {
		if w.ID != "" {
			vouchers = append(vouchers, Voucher{
				ID:          w.ID,
				Token:       tokenForVoucher(c.WithdrawSecret, w.ID),
				Domain:      w.Domain,
				Description: w.Description,
				Amount:      w.Amount,
			})
		} else if w.Prefix != "" {
			for i := 1; i <= w.Count; i++ {
				id := fmt.Sprintf("%s-%03d", w.Prefix, i)
				vouchers = append(vouchers, Voucher{
					ID:          id,
					Token:       tokenForVoucher(c.WithdrawSecret, id),
					Domain:      w.Domain,
					Description: w.Description,
					Amount:      w.Amount,
				})
			}
		}
	}
	return vouchers
}
