package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/nostr"
)

func diagnose(ctx context.Context, cfg *config.Config, publicURL string, out io.Writer) error {
	var failures []error
	report := func(name string, err error) {
		if err != nil {
			fmt.Fprintf(out, "FAIL %s: %v\n", name, err)
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		} else {
			fmt.Fprintf(out, "OK %s\n", name)
		}
	}
	report("config and feature files", nil)
	if len(cfg.Addresses) > 0 {
		_, err := cfg.VerifyKey()
		report("verification key", err)
	}
	if cfg.Nostr != nil {
		_, err := nostr.ParseKey(cfg.Nostr.PrivateKey)
		report("zap signing key", err)
	}
	if cfg.StateDir != "" {
		report("state directory", checkStateDirectory(cfg.StateDir))
	}
	report("node", checkDiagnosticNode(ctx, cfg))
	fmt.Fprintln(out, "NOTE invoice creation, payment and subscription permissions are not exercised; no payments or notifications are sent")
	if len(cfg.Addresses) > 0 {
		if len(cfg.TrustedProxies) == 0 {
			fmt.Fprintln(out, "NOTE trusted_proxies is empty: forwarded host and client IP headers will be ignored")
		}
		if publicURL == "" {
			fmt.Fprintln(out, "SKIP public endpoint: supply -public-url with a full /.well-known/lnurlp/name URL while the service is running")
		}
	}
	if publicURL != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.ProxyURL != nil {
			transport.Proxy = http.ProxyURL(cfg.ProxyURL)
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		report("public endpoint", checkPublicEndpoint(ctx, cfg, publicURL, client))
	}
	return errors.Join(failures...)
}

func checkStateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".diagnose-*")
	if err != nil {
		return fmt.Errorf("cannot create a state file as the current user; check mount ownership and permissions: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("diagnostic probe\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	renamed := f.Name() + ".renamed"
	if err := os.Rename(f.Name(), renamed); err != nil {
		return fmt.Errorf("cannot replace a state file: %w", err)
	}
	return os.Remove(renamed)
}

func checkDiagnosticNode(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := connect(cfg)
	if err != nil {
		return err
	}
	defer n.Close()
	if len(cfg.Addresses) > 0 || cfg.Notifies() || cfg.Nostr != nil {
		if _, err := n.LastPaidIndex(ctx); err != nil {
			return fmt.Errorf("read invoices: %w", err)
		}
	}
	if len(cfg.Withdraw) > 0 || len(cfg.Payments.Clients) > 0 {
		if _, err := n.Network(ctx); err != nil {
			return fmt.Errorf("read node network: %w", err)
		}
	}
	return nil
}

func checkPublicEndpoint(ctx context.Context, cfg *config.Config, rawURL string, client *http.Client) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("use a full http(s) LNURL-pay URL without credentials, query or fragment")
	}
	if u.Scheme == "http" && !strings.HasSuffix(strings.ToLower(u.Hostname()), ".onion") {
		return errors.New("public URLs need HTTPS, except for onion addresses")
	}
	const prefix = "/.well-known/lnurlp/"
	name := strings.TrimPrefix(u.Path, prefix)
	if !strings.HasPrefix(u.Path, prefix) || name == "" || strings.Contains(name, "/") {
		return errors.New("URL must end with /.well-known/lnurlp/name")
	}
	addr, ok := cfg.Addresses[name]
	if !ok {
		addr, ok = cfg.Addresses["*"]
	}
	if !ok {
		return errors.New("URL names an address absent from this config")
	}
	const origin = "https://diagnostics.invalid"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", origin)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d; check routing and rate limits", resp.StatusCode)
	}
	var metadata struct {
		Tag         string `json:"tag"`
		Callback    string `json:"callback"`
		Metadata    string `json:"metadata"`
		MinSendable int64  `json:"minSendable"`
		MaxSendable int64  `json:"maxSendable"`
		AllowsNostr bool   `json:"allowsNostr"`
		NostrPubkey string `json:"nostrPubkey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&metadata); err != nil {
		return errors.New("response is not LNURL-pay JSON; check proxy routing")
	}
	var failures []error
	if metadata.Tag != "payRequest" || metadata.MinSendable != addr.MinSendable || metadata.MaxSendable != addr.MaxSendable {
		failures = append(failures, errors.New("LNURL-pay type or amount limits do not match this config"))
	}
	callback := *u
	callback.Path += "/invoice"
	callback.RawPath = ""
	if metadata.Callback != callback.String() {
		failures = append(failures, errors.New("callback does not match the public URL; check Host, X-Forwarded-Host and trusted_proxies"))
	}
	var entries [][]string
	identifier := false
	if json.Unmarshal([]byte(metadata.Metadata), &entries) == nil {
		for _, entry := range entries {
			if len(entry) == 2 && entry[0] == "text/identifier" && entry[1] == name+"@"+u.Host {
				identifier = true
			}
		}
	}
	if !identifier {
		failures = append(failures, errors.New("metadata identifier does not match the public address; check proxy host headers"))
	}
	if allow := resp.Header.Get("Access-Control-Allow-Origin"); allow != "*" && allow != origin {
		failures = append(failures, errors.New("missing or incompatible CORS header; browser wallets cannot read this response"))
	}
	expectedZapKey := ""
	if cfg.Nostr != nil {
		key, err := nostr.ParseKey(cfg.Nostr.PrivateKey)
		if err != nil {
			failures = append(failures, fmt.Errorf("configured zap signing key: %w", err))
		} else {
			expectedZapKey = key.PubKey()
		}
	}
	if metadata.AllowsNostr != (cfg.Nostr != nil) || metadata.NostrPubkey != expectedZapKey {
		failures = append(failures, errors.New("zap support or signing public key does not match this config"))
	}
	return errors.Join(failures...)
}
