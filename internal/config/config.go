package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen         string                  `yaml:"listen,omitempty"`
	TrustedProxies []Prefix                `yaml:"trusted_proxies,omitempty"`
	LND            LNDConfig               `yaml:"lnd"`
	StateDir       string                  `yaml:"state_dir,omitempty"`
	Notifications  NotificationsConfig     `yaml:"notifications,omitempty"`
	WithdrawSecret string                  `yaml:"withdraw_secret,omitempty"`
	Withdraw       []WithdrawConfig        `yaml:"withdraw,omitempty"`
	Addresses      map[string]AddressEntry `yaml:"addresses,omitempty"`
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

type LNDConfig struct {
	Host     string `yaml:"host"`
	TLSCert  string `yaml:"tls_cert"`
	Macaroon string `yaml:"macaroon"`
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
	Description string `yaml:"description"`
	MinSendable int64  `yaml:"min_sendable"`
	MaxSendable int64  `yaml:"max_sendable"`
}

// Voucher is an expanded single withdraw voucher (after batch expansion).
type Voucher struct {
	ID          string
	Token       string // URL-safe opaque token derived from ID and secret
	Domain      string
	Description string
	Amount      int64 // millisatoshis
}

const minWithdrawSecretLen = 32

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

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if n := cfg.Notifications.Ntfy; n != nil && n.TokenFile != "" {
		token, err := os.ReadFile(n.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read notifications.ntfy.token_file: %w", err)
		}
		n.Token = strings.TrimSpace(string(token))
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.LND.Host == "" {
		return fmt.Errorf("lnd.host is required")
	}
	if c.LND.TLSCert == "" {
		return fmt.Errorf("lnd.tls_cert is required")
	}
	if c.LND.Macaroon == "" {
		return fmt.Errorf("lnd.macaroon is required")
	}
	if !c.ServesHTTP() && !c.Notifies() {
		return fmt.Errorf("nothing to do: configure addresses, withdraw or notifications")
	}
	if c.ServesHTTP() && c.Listen == "" {
		return fmt.Errorf("listen is required with addresses or withdraw vouchers")
	}
	if (c.Notifies() || len(c.Withdraw) > 0) && c.StateDir == "" {
		return fmt.Errorf("state_dir is required with notifications or withdraw vouchers")
	}
	if n := c.Notifications.Ntfy; n != nil {
		if n.URL == "" || n.Topic == "" {
			return fmt.Errorf("notifications.ntfy needs url and topic")
		}
		if n.Token != "" && n.TokenFile != "" {
			return fmt.Errorf("notifications.ntfy: set token or token_file, not both")
		}
	}
	if len(c.Withdraw) > 0 && len(c.WithdrawSecret) < minWithdrawSecretLen {
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
