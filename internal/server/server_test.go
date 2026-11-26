package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/payments"
)

func TestNew_ShutdownBeforeServe(t *testing.T) {
	srv := New(nil, config.DefaultRateLimit, nil, nil, nil)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server kept serving after Shutdown")
	}
}

func TestNew_Timeouts(t *testing.T) {
	srv := New(nil, config.DefaultRateLimit, nil, nil, nil)
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("read header %s, read %s, idle %s: want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout <= lnurl.PayTimeout {
		t.Fatalf("write timeout %s cuts off a payment of up to %s", srv.WriteTimeout, lnurl.PayTimeout)
	}
}

func trustedProxies(t *testing.T, entries ...string) proxies {
	t.Helper()
	var p proxies
	for _, e := range entries {
		var prefix config.Prefix
		if err := prefix.UnmarshalText([]byte(e)); err != nil {
			t.Fatal(err)
		}
		p = append(p, prefix)
	}
	return p
}

func TestClientIP(t *testing.T) {
	p := trustedProxies(t, "10.0.0.0/8", "192.0.2.1")
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer", "203.0.113.9:1", []string{"198.51.100.1"}, "203.0.113.9"},
		{"trusted peer without header", "10.0.0.1:1", nil, "10.0.0.1"},
		{"trusted peer", "10.0.0.1:1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"spoofed left-most entry", "10.0.0.1:1", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"proxy chain", "10.0.0.1:1", []string{"1.1.1.1, 198.51.100.1, 192.0.2.1", "10.0.0.2"}, "198.51.100.1"},
		{"garbage entry", "10.0.0.1:1", []string{"198.51.100.1, nonsense"}, "10.0.0.1"},
		{"mapped peer", "[::ffff:10.0.0.1]:1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"ipv6 peer", "[2001:db8::1]:1", []string{"198.51.100.1"}, "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := p.clientIP(req); got != tc.want {
				t.Fatalf("clientIP = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNew_ForwardedHostOnlyFromTrustedProxy(t *testing.T) {
	address := lnurl.NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000},
	}, nil, nil, []byte("verify key"), nil)
	srv := New(trustedProxies(t, "10.0.0.0/8"), config.DefaultRateLimit, nil, address, nil)

	for remote, want := range map[string]string{
		"10.0.0.1:1234":    "https://shop.example.com/.well-known/lnurlp/bob/invoice",
		"203.0.113.9:1234": "https://192.168.4.12:7233/.well-known/lnurlp/bob/invoice",
	} {
		req := httptest.NewRequest("GET", "http://192.168.4.12:7233/.well-known/lnurlp/bob", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-Host", "shop.example.com")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if resp["callback"] != want {
			t.Errorf("from %s: callback = %v, want %s", remote, resp["callback"], want)
		}
	}
}

func TestNew_CrossOriginLNURL(t *testing.T) {
	address := lnurl.NewAddressHandler(map[string]config.AddressEntry{
		"bob": {Description: "tips", MinSendable: 1000, MaxSendable: 2000},
	}, nil, nil, []byte("verify key"), nil)
	for _, path := range []string{
		"/.well-known/lnurlp/bob",
		"/.well-known/lnurlp/bob/invoice",
		"/.well-known/lnurlp/bob/verify/invalid",
		"/.well-known/lnurlp/unknown",
	} {
		t.Run(path, func(t *testing.T) {
			srv := New(nil, config.RateLimit{PerMinute: 1, Burst: 1}, nil, address, nil)
			for i := range 2 {
				req := httptest.NewRequest(http.MethodGet, "https://pay.example.com"+path, nil)
				req.Header.Set("Origin", "https://jumble.social")
				rec := httptest.NewRecorder()
				srv.Handler.ServeHTTP(rec, req)
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
					t.Errorf("request %d: Access-Control-Allow-Origin = %q, want * (status %d)", i, got, rec.Code)
				}
				if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Error("unexpected credentialed CORS")
				}
				if i == 1 && rec.Code != http.StatusTooManyRequests {
					t.Errorf("second request status = %d, want 429", rec.Code)
				}
			}
		})
	}
}

func TestNew_PaymentsWithoutCORS(t *testing.T) {
	const token = "shop-token-at-least-32-characters"
	pay, err := payments.New(context.Background(), map[string]config.PaymentClient{
		"shop": {Token: token, AllowedIPs: trustedProxies(t, "192.0.2.9")},
	}, nil, nil, nil, &chaincfg.MainNetParams, filepath.Join(t.TempDir(), "payments.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(trustedProxies(t, "10.0.0.0/8"), config.DefaultRateLimit, nil, nil, pay)
	for _, tc := range []struct {
		path, remote, forwarded string
		want                    int
	}{
		{"/payments", "10.0.0.1:1", "192.0.2.9", http.StatusOK},
		{"/payments", "192.0.2.10:1", "192.0.2.9", http.StatusForbidden},
		{"/payments/x", "10.0.0.1:1", "192.0.2.9", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", tc.forwarded)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != tc.want || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s from %s: status %d, CORS %q", tc.path, tc.remote, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}
