package notifier

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"unbeholden-lnurl/internal/config"
)

type fakeInvoices struct {
	lastSettleIndex uint64
	settled         []uint64
	from            []uint64
	cancel          context.CancelFunc
}

func (f *fakeInvoices) LastSettleIndex(context.Context) (uint64, error) {
	return f.lastSettleIndex, nil
}

func (f *fakeInvoices) SubscribeInvoices(ctx context.Context, settleIndex uint64, onSettled func(int64, string, uint64) error) error {
	f.from = append(f.from, settleIndex)
	for _, i := range f.settled {
		if i <= settleIndex {
			continue
		}
		if err := onSettled(1000*int64(i), "", i); err != nil {
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
	WatchSettled(ctx, f, statePath, n)
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
	return NewNtfy(&config.NtfyConfig{URL: srv.URL, Topic: "t"}), &delivered
}

func TestWatchSettled_FirstStartSkipsHistoryAndRecordsBaseline(t *testing.T) {
	n, delivered := ntfyServer(t, func(string) bool { return false })
	statePath := filepath.Join(t.TempDir(), "settle_index")

	f := &fakeInvoices{lastSettleIndex: 7, settled: []uint64{5, 6, 7}}
	f.watch(t, statePath, n)
	if f.from[0] != 7 {
		t.Fatalf("first start subscribed from %d, want 7", f.from[0])
	}

	f.lastSettleIndex = 9
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
