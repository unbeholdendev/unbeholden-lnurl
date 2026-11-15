package notifier

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
)

type fakeInvoices struct {
	lastPaidIndex uint64
	settled       []uint64
	from          []uint64
	cancel        context.CancelFunc
}

func (f *fakeInvoices) LastPaidIndex(context.Context) (uint64, error) {
	return f.lastPaidIndex, nil
}

func (f *fakeInvoices) SubscribeInvoices(ctx context.Context, after uint64, onSettled func(lightning.Settled, uint64) error) error {
	f.from = append(f.from, after)
	for _, i := range f.settled {
		if i <= after {
			continue
		}
		if err := onSettled(lightning.Settled{AmountMsat: 1000 * int64(i)}, i); err != nil {
			return err
		}
	}
	if len(f.from) >= 2 {
		f.cancel()
	}
	return ctx.Err()
}

func (f *fakeInvoices) watch(t *testing.T, statePath string, n *Ntfy) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.from = nil
	WatchSettled(ctx, f, statePath, n.NotifyPayment)
}

func ntfyServer(t *testing.T, fail func(body string) bool) (*Ntfy, *[]string) {
	var mu sync.Mutex
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if fail(string(b)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		delivered = append(delivered, string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return NewNtfy(config.NtfyConfig{URL: srv.URL, Topic: "t"}), &delivered
}

func TestWatchSettled_FirstStartSkipsHistoryAndRecordsBaseline(t *testing.T) {
	n, delivered := ntfyServer(t, func(string) bool { return false })
	statePath := filepath.Join(t.TempDir(), "settle_index")

	f := &fakeInvoices{lastPaidIndex: 7, settled: []uint64{5, 6, 7}}
	f.watch(t, statePath, n)
	if f.from[0] != 7 {
		t.Fatalf("first start subscribed from %d, want 7", f.from[0])
	}

	f.lastPaidIndex = 9
	f.settled = append(f.settled, 8, 9)
	f.watch(t, statePath, n)
	if f.from[0] != 7 {
		t.Fatalf("restart subscribed from %d, want 7", f.from[0])
	}
	if got := *delivered; len(got) != 2 || got[0] != "Received 8 sats" || got[1] != "Received 9 sats" {
		t.Fatalf("delivered = %q", got)
	}
}

func TestWatchSettled_RetriesFailedDelivery(t *testing.T) {
	failOnce := true
	n, delivered := ntfyServer(t, func(body string) bool {
		if body == "Received 2 sats" && failOnce {
			failOnce = false
			return true
		}
		return false
	})
	statePath := filepath.Join(t.TempDir(), "settle_index")

	f := &fakeInvoices{settled: []uint64{1, 2, 3}}
	f.watch(t, statePath, n)
	if len(f.from) != 2 || f.from[1] != 1 {
		t.Fatalf("subscribed from %v, want [0 1]", f.from)
	}
	if got := *delivered; len(got) != 3 || got[1] != "Received 2 sats" || got[2] != "Received 3 sats" {
		t.Fatalf("delivered = %q", got)
	}
}

func TestInitSettled_RecordsBaselineBeforeNewPayments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settle_index")
	f := &fakeInvoices{lastPaidIndex: 7}
	if err := InitSettled(context.Background(), f, path); err != nil {
		t.Fatal(err)
	}
	f.lastPaidIndex = 8
	f.settled = []uint64{8}
	if err := InitSettled(context.Background(), f, path); err != nil {
		t.Fatal(err)
	}
	var delivered int
	watchOnce(context.Background(), f, path, func(lightning.Settled) error { delivered++; return nil })
	if delivered != 1 {
		t.Fatalf("delivered %d payments, want 1", delivered)
	}
}

func TestNtfyUsesSOCKSProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		conn.Write([]byte{5, 0})
		head := make([]byte, 5)
		if _, err := io.ReadFull(conn, head); err != nil || head[3] != 3 {
			return
		}
		name := make([]byte, int(head[4])+2)
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		target <- string(name[:len(name)-2])
	}()
	proxy, _ := url.Parse("socks5://" + ln.Addr().String())
	n := NewNtfy(config.NtfyConfig{URL: "http://ntfy.invalid", Topic: "t"}, proxy)
	n.NotifyPayment(lightning.Settled{AmountMsat: 1000})
	select {
	case got := <-target:
		if got != "ntfy.invalid" {
			t.Fatalf("proxy target = %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ntfy did not use SOCKS proxy")
	}
}

func TestSettledSharedPositionRetriesFirstDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settle_index")
	invoices := &fakeInvoices{settled: []uint64{1, 2, 3}}
	var receipts []int64
	onSettled := func(invoice lightning.Settled) error {
		receipts = append(receipts, invoice.AmountMsat)
		return fmt.Errorf("ntfy unavailable")
	}
	for range 2 {
		watchOnce(context.Background(), invoices, path, onSettled)
	}
	if !slices.Equal(receipts, []int64{1000, 1000}) {
		t.Fatalf("shared position delivered %v", receipts)
	}
	index, err := loadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if index != 0 {
		t.Fatalf("position = %d, want 0 after failed delivery", index)
	}
}

func TestForkSettled(t *testing.T) {
	for _, tc := range []struct {
		name, previous, current, want string
		fail                          bool
	}{
		{"migration", "4\n", "", "4\n", false},
		{"preserve newer", "4\n", "9\n", "9\n", false},
		{"preserve older", "9\n", "4\n", "4\n", false},
		{"invalid source", "broken", "", "", true},
		{"missing source", "", "", "", true},
		{"invalid target", "4\n", "broken", "broken", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source, target := filepath.Join(dir, "settle_index"), filepath.Join(dir, "zap_settle_index")
			for path, data := range map[string]string{source: tc.previous, target: tc.current} {
				if data != "" {
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := ForkSettled(target, source)
			if (err != nil) != tc.fail {
				t.Fatalf("ForkSettled = %v", err)
			}
			data, readErr := os.ReadFile(target)
			if string(data) != tc.want {
				t.Fatalf("target = %q, want %q", data, tc.want)
			}
			if tc.want == "" && !os.IsNotExist(readErr) {
				t.Fatalf("target created after failed migration: %v", readErr)
			}
		})
	}
}
