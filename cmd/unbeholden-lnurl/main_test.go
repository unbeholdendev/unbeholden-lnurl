package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/notifier"
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

type settlementInvoices struct {
	last uint64
}

func (f *settlementInvoices) LastPaidIndex(context.Context) (uint64, error) {
	return f.last, nil
}

func (f *settlementInvoices) SubscribeInvoices(ctx context.Context, after uint64, handle func(lightning.Settled, uint64) error) error {
	for i := after + 1; i <= f.last; i++ {
		if err := handle(lightning.Settled{AmountMsat: int64(i) * 1000}, i); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func runSettlementWatchers(t *testing.T, f *settlementInvoices, watchers []settlementWatcher, finished <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range watchers {
		wg.Go(func() { notifier.WatchSettled(ctx, f, w.path, w.handle) })
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Error("later zap receipt blocked by failing notification")
	}
	cancel()
	wg.Wait()
}

func TestSettlementsNotificationFailureDoesNotBlockZaps(t *testing.T) {
	dir := t.TempDir()
	f := &settlementInvoices{last: 7}
	if err := os.WriteFile(filepath.Join(dir, "settle_index"), []byte("4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	receipts := make(chan int64, 10)
	done := make(chan struct{})
	payment := func(lightning.Settled) error { return errors.New("ntfy unavailable") }
	receipt := func(i lightning.Settled) error {
		receipts <- i.AmountMsat
		if i.AmountMsat == 7000 {
			close(done)
		}
		return nil
	}
	watchers, err := prepareSettlements(context.Background(), f, dir, payment, receipt)
	if err != nil {
		t.Fatal(err)
	}
	runSettlementWatchers(t, f, watchers, done)
	if t.Failed() {
		return
	}
	if len(receipts) != 3 {
		t.Fatalf("receipts = %d, want 3 from legacy cursor", len(receipts))
	}
	for _, tc := range []struct{ name, want string }{{"settle_index", "4\n"}, {"zap_settle_index", "7\n"}} {
		data, err := os.ReadFile(filepath.Join(dir, tc.name))
		if err != nil || string(data) != tc.want {
			t.Fatalf("%s = %q, %v", tc.name, data, err)
		}
	}
	f.last = 8
	done = make(chan struct{})
	var payments []int64
	payment = func(i lightning.Settled) error { payments = append(payments, i.AmountMsat); return nil }
	receipt = func(i lightning.Settled) error { receipts <- i.AmountMsat; close(done); return nil }
	watchers, err = prepareSettlements(context.Background(), f, dir, payment, receipt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range watchers {
		wg.Go(func() { notifier.WatchSettled(ctx, f, w.path, w.handle) })
	}
	select {
	case <-done:
	case <-ctx.Done():
		cancel()
		wg.Wait()
		t.Fatal("zap receipt did not resume after restart")
	}
	for {
		data, _ := os.ReadFile(filepath.Join(dir, "settle_index"))
		if string(data) == "8\n" {
			break
		}
		select {
		case <-ctx.Done():
			cancel()
			wg.Wait()
			t.Fatal("notification retry did not recover")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if !slices.Equal(payments, []int64{5000, 6000, 7000, 8000}) {
		t.Fatalf("retried payments = %v", payments)
	}
	if len(receipts) != 4 {
		t.Fatalf("receipts after restart = %d, want 4", len(receipts))
	}
}

func TestPrepareSettlementsModesAndBaseline(t *testing.T) {
	for _, mode := range []struct {
		name             string
		payment, receipt bool
		want             int
	}{
		{"disabled", false, false, 0}, {"notifications", true, false, 1},
		{"zaps", false, true, 1}, {"both", true, true, 2},
	} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			f := &settlementInvoices{last: 7}
			var payment, receipt func(lightning.Settled) error
			handle := func(lightning.Settled) error { return nil }
			if mode.payment {
				payment = handle
			}
			if mode.receipt {
				receipt = handle
			}
			watchers, err := prepareSettlements(context.Background(), f, dir, payment, receipt)
			if err != nil {
				t.Fatal(err)
			}
			if len(watchers) != mode.want {
				t.Fatalf("watchers = %d, want %d", len(watchers), mode.want)
			}
			f.last = 8
			watchers, err = prepareSettlements(context.Background(), f, dir, payment, receipt)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range watchers {
				data, err := os.ReadFile(w.path)
				if err != nil || string(data) != "7\n" {
					t.Fatalf("baseline = %q, %v", data, err)
				}
			}
			if !mode.receipt {
				if _, err := os.Stat(filepath.Join(dir, "zap_settle_index")); !os.IsNotExist(err) {
					t.Fatalf("unexpected zap cursor: %v", err)
				}
			}
		})
	}
}
