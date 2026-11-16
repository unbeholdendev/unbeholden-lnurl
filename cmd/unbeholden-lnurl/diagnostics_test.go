package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/nostr"
)

func TestCheckStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := checkStateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(dir, "settle_index")
	if err := os.WriteFile(cursor, []byte("399"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkStateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "settle_index" {
		t.Fatalf("probe left files behind: %v, %v", entries, err)
	}
	data, _ := os.ReadFile(cursor)
	if string(data) != "399" {
		t.Fatalf("cursor changed: %q", data)
	}
	if err := checkStateDirectory(cursor); err == nil {
		t.Fatal("accepted a file as state directory")
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
		if err := checkStateDirectory(dir); err == nil {
			t.Fatal("accepted unwritable state directory")
		}
	}
}

func TestCheckPublicEndpoint(t *testing.T) {
	key, err := nostr.ParseKey(strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Addresses: map[string]config.AddressEntry{"alice": {MinSendable: 1000, MaxSendable: 2000}},
		Nostr:     &config.NostrConfig{PrivateKey: strings.Repeat("01", 32)},
	}
	for _, tc := range []struct {
		name, want   string
		change       func(map[string]any, http.ResponseWriter)
		withoutNostr bool
	}{
		{"valid", "", func(map[string]any, http.ResponseWriter) {}, false},
		{"wrong host", "trusted_proxies", func(m map[string]any, _ http.ResponseWriter) { m["callback"] = "https://192.0.2.1/invoice" }, false},
		{"missing CORS", "CORS", func(_ map[string]any, w http.ResponseWriter) { w.Header().Del("Access-Control-Allow-Origin") }, false},
		{"wrong identity", "identifier", func(m map[string]any, _ http.ResponseWriter) {
			m["metadata"] = `[["text/identifier","alice@internal"]]`
		}, false},
		{"zaps disabled", "zap support", func(m map[string]any, _ http.ResponseWriter) { delete(m, "allowsNostr") }, false},
		{"wrong zap key", "zap support", func(m map[string]any, _ http.ResponseWriter) { m["nostrPubkey"] = strings.Repeat("ff", 32) }, false},
		{"wrong limits", "amount limits", func(m map[string]any, _ http.ResponseWriter) { m["maxSendable"] = 3000 }, false},
		{"rate limited", "429", func(_ map[string]any, w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }, false},
		{"zaps unexpectedly enabled", "zap support", func(map[string]any, http.ResponseWriter) {}, true},
		{"both disabled", "", func(m map[string]any, _ http.ResponseWriter) { delete(m, "allowsNostr"); delete(m, "nostrPubkey") }, true},
		{"explicitly disabled", "", func(m map[string]any, _ http.ResponseWriter) { m["allowsNostr"] = false; delete(m, "nostrPubkey") }, true},
		{"stale key while disabled", "zap support", func(m map[string]any, _ http.ResponseWriter) { m["allowsNostr"] = false }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := *cfg
			if tc.withoutNostr {
				local.Nostr = nil
			}
			calls := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/.well-known/lnurlp/alice" || r.URL.RawQuery != "" || r.Header.Get("Origin") == "" {
					t.Errorf("unexpected diagnostic request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Access-Control-Allow-Origin", "*")
				m := map[string]any{
					"tag": "payRequest", "callback": "https://" + r.Host + r.URL.Path + "/invoice",
					"metadata":    `[["text/identifier","alice@` + r.Host + `"]]`,
					"minSendable": 1000, "maxSendable": 2000,
					"allowsNostr": true, "nostrPubkey": key.PubKey(),
				}
				tc.change(m, w)
				json.NewEncoder(w).Encode(m)
			}))
			defer srv.Close()
			err := checkPublicEndpoint(context.Background(), &local, srv.URL+"/.well-known/lnurlp/alice", srv.Client())
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("check = %v, want %q", err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("made %d requests, want only metadata lookup", calls)
			}
		})
	}
}

func TestDiagnosticNodeReadOnlyAndCancellation(t *testing.T) {
	secret := "private-rune-sentinel"
	path := filepath.Join(t.TempDir(), "rune")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/listinvoices" || r.Header.Get("Rune") != secret {
			t.Errorf("unexpected node request: %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"invoices":[]}`))
	}))
	defer srv.Close()
	cfg := &config.Config{
		CLN:       &config.CLNConfig{URL: srv.URL, RuneFile: path},
		Addresses: map[string]config.AddressEntry{"*": {MinSendable: 1000, MaxSendable: 2000}},
		StateDir:  t.TempDir(),
	}
	var out bytes.Buffer
	if err := diagnose(context.Background(), cfg, "", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "OK node") || !strings.Contains(out.String(), "SKIP public endpoint") || strings.Contains(out.String(), secret) {
		t.Fatalf("unexpected output: %s", &out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkDiagnosticNode(ctx, cfg); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestPublicEndpointRejectsInvalidURLsAndHonoursDeadline(t *testing.T) {
	cfg := &config.Config{Addresses: map[string]config.AddressEntry{"*": {MinSendable: 1000, MaxSendable: 2000}}}
	for _, u := range []string{"not-a-url", "https://user:secret@example.com/.well-known/lnurlp/alice", "https://example.com/", "http://example.com/.well-known/lnurlp/alice", "https://example.com/.well-known/lnurlp/alice?secret=x", "https://example.com/.well-known/lnurlp/alice/invoice"} {
		if err := checkPublicEndpoint(context.Background(), cfg, u, &http.Client{}); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := checkPublicEndpoint(ctx, cfg, srv.URL+"/.well-known/lnurlp/alice", srv.Client()); err == nil {
		t.Fatal("ignored deadline")
	}
}
