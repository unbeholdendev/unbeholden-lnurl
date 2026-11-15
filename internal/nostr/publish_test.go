package nostr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// relay answers EVENT messages with a NOTICE, then OK as accept says.
func relay(t *testing.T, accept bool) *httptest.Server {
	t.Helper()
	var upgrader websocket.Upgrader
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var msg []any
		if conn.ReadJSON(&msg) != nil || len(msg) != 2 || msg[0] != "EVENT" {
			return
		}
		id := msg[1].(map[string]any)["id"]
		conn.WriteJSON([]any{"NOTICE", "hello"})
		conn.WriteJSON([]any{"OK", "other", true, ""})
		conn.WriteJSON([]any{"OK", id, accept, "blocked: test"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPublish(t *testing.T) {
	good, bad := relay(t, true), relay(t, false)
	roots := x509.NewCertPool()
	roots.AddCert(good.Certificate())
	roots.AddCert(bad.Certificate())
	p := &Publisher{dialer: &websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots}}}

	wss := func(srv *httptest.Server) string { return strings.Replace(srv.URL, "https://", "wss://", 1) }
	insecure := strings.Replace(good.URL, "https://", "ws://", 1)
	got := p.Publish(context.Background(), []string{wss(good), wss(bad), insecure, "wss://127.0.0.1:1"}, Event{ID: "abc"})
	if got != 1 {
		t.Fatalf("accepted by %d relays, want 1", got)
	}
}

func TestNewPublisher_RefusesNonPublicAddresses(t *testing.T) {
	srv := relay(t, true)
	got := NewPublisher().Publish(context.Background(), []string{strings.Replace(srv.URL, "https://", "wss://", 1)}, Event{ID: "abc"})
	if got != 0 {
		t.Fatal("published to a loopback relay")
	}
	for addr, public := range map[string]bool{
		"1.1.1.1:443":           true,
		"[2606:4700::1]:443":    true,
		"127.0.0.1:443":         false,
		"10.1.2.3:443":          false,
		"192.168.1.1:443":       false,
		"100.64.0.1:443":        false,
		"169.254.1.1:443":       false,
		"[::1]:443":             false,
		"[fd00::1]:443":         false,
		"[::ffff:10.0.0.1]:443": false,
	} {
		if err := publicOnly("tcp", addr, nil); (err == nil) != public {
			t.Errorf("%s: err = %v, want public %v", addr, err, public)
		}
	}
}

func TestPublish_RejectsOversizedResponse(t *testing.T) {
	var upgrader websocket.Upgrader
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var msg []any
		if conn.ReadJSON(&msg) != nil {
			return
		}
		conn.WriteJSON([]any{"NOTICE", strings.Repeat("x", 128<<10)})
		conn.WriteJSON([]any{"OK", "abc", true, ""})
	}))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	p := &Publisher{dialer: &websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	if got := p.Publish(context.Background(), []string{strings.Replace(srv.URL, "https://", "wss://", 1)}, Event{ID: "abc"}); got != 0 {
		t.Fatal("accepted oversized relay response")
	}
}
