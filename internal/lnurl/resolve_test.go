package lnurl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/bolt11/bolt11test"
)

const resolveMetadata = `[["text/plain","tips"]]`

// lightningAddress serves an address whose callback answers with invoice,
// given the requested amount and metadata.
func lightningAddress(t *testing.T, invoice func(amount int64, metadata string) string) (*Resolver, string, *url.Values) {
	t.Helper()
	var callback url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/lnurlp/alice":
			json.NewEncoder(w).Encode(map[string]any{
				"tag": "payRequest", "callback": "https://" + r.Host + "/cb?k=v", "metadata": resolveMetadata,
				"minSendable": 1000, "maxSendable": 100000, "commentAllowed": 5,
			})
		case "/cb":
			callback = r.URL.Query()
			var amount int64
			json.Unmarshal([]byte(callback.Get("amount")), &amount)
			json.NewEncoder(w).Encode(map[string]any{"pr": invoice(amount, resolveMetadata), "routes": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	r := NewResolver(nil, &chaincfg.MainNetParams)
	r.client.Transport = srv.Client().Transport
	return r, "alice@" + strings.TrimPrefix(srv.URL, "https://"), &callback
}

func TestResolver_Invoice(t *testing.T) {
	honest := func(amount int64, metadata string) string { return bolt11test.Invoice(amount, [32]byte{1}, metadata) }
	var served string
	r, address, callback := lightningAddress(t, func(amount int64, metadata string) string {
		served = honest(amount, metadata)
		return served
	})
	pr, err := r.Invoice(context.Background(), address, 21000, "hi")
	if err != nil || pr != served {
		t.Fatalf("Invoice = %q, %v", pr, err)
	}
	if callback.Get("amount") != "21000" || callback.Get("comment") != "hi" || callback.Get("k") != "v" {
		t.Fatalf("callback query = %v", *callback)
	}

	for name, tc := range map[string]struct {
		address func(served string) string
		amount  int64
		comment string
		invoice func(int64, string) string
	}{
		"unknown user":     {address: func(a string) string { return strings.Replace(a, "alice", "bob", 1) }, amount: 21000},
		"below minimum":    {amount: 999},
		"above maximum":    {amount: 100010},
		"comment too long": {amount: 21000, comment: "hello!"},
		"other amount":     {amount: 21000, invoice: func(_ int64, m string) string { return honest(22000, m) }},
		"other metadata":   {amount: 21000, invoice: func(a int64, _ string) string { return honest(a, "other") }},
		"other network": {amount: 21000, invoice: func(a int64, m string) string {
			return bolt11test.InvoiceFor(&chaincfg.TestNet3Params, a, [32]byte{1}, m)
		}},
		"not an address":    {address: func(string) string { return "alice" }, amount: 21000},
		"path in domain":    {address: func(a string) string { return a + "/x" }, amount: 21000},
		"onion without tor": {address: func(string) string { return "alice@example.onion" }, amount: 21000},
	} {
		t.Run(name, func(t *testing.T) {
			invoice := tc.invoice
			if invoice == nil {
				invoice = honest
			}
			r, address, _ := lightningAddress(t, invoice)
			if tc.address != nil {
				address = tc.address(address)
			}
			if pr, err := r.Invoice(context.Background(), address, tc.amount, tc.comment); err == nil {
				t.Fatalf("accepted %q", pr)
			}
		})
	}
}

func TestResolver_RequiresHTTPS(t *testing.T) {
	r := NewResolver(nil, &chaincfg.MainNetParams)
	for _, u := range []string{"http://example.com/cb", "https://user@example.com/cb", "http://example.onion/cb"} {
		if _, err := r.endpoint(u); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	proxied := NewResolver(&url.URL{Scheme: "socks5", Host: "127.0.0.1:9050"}, &chaincfg.MainNetParams)
	if _, err := proxied.endpoint("http://example.onion/cb"); err != nil {
		t.Errorf("onion over proxy: %v", err)
	}
}
