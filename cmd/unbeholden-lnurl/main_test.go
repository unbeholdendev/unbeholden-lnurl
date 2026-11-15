package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServe_WaitsForInFlightRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, ln) }()
	go http.Get("http://" + ln.Addr().String())
	<-started

	cancel()
	select {
	case err := <-done:
		t.Fatalf("serve returned %v with a request in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVersionFlag(t *testing.T) {
	for _, tc := range []struct{ flags, want string }{{"", "dev"}, {"-X main.version=v1.2.3", "v1.2.3"}} {
		out, err := exec.Command("go", "run", "-ldflags="+tc.flags, ".", "-config", "/nonexistent/config.yaml", "-version").CombinedOutput()
		if err != nil {
			t.Fatalf("version: %v: %s", err, out)
		}
		if strings.TrimSpace(string(out)) != tc.want {
			t.Fatalf("version = %q, want %q", out, tc.want)
		}
	}
}

func TestListVouchers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "listen: :8080\nstate_dir: " + dir + "\nlnd:\n  host: unused\n  tls_cert: unused\n  macaroon: unused\nwithdraw_secret: " + strings.Repeat("x", 32) + "\nwithdraw:\n  - id: tip\n    domain: test.onion\n    amount: 1000\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "run", ".", "-config", path, "-list-vouchers").CombinedOutput()
	if err != nil {
		t.Fatalf("list vouchers: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "http://test.onion/lnurl/withdraw/") || !strings.Contains(string(out), "LNURL") {
		t.Fatalf("vouchers = %s", out)
	}
}
