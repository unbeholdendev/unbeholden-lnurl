package zap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/nostr"
)

type fakePublisher struct {
	relays []string
	events []nostr.Event
}

func (f *fakePublisher) Publish(_ context.Context, relays []string, e nostr.Event) int {
	f.relays = relays
	f.events = append(f.events, e)
	return len(relays)
}

func key(t *testing.T, b byte) nostr.Key {
	t.Helper()
	k, err := nostr.ParseKey(strings.Repeat(hex.EncodeToString([]byte{b}), 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newZapper(t *testing.T) (*Zapper, *fakePublisher) {
	t.Helper()
	p := &fakePublisher{}
	return New(key(t, 1), p, filepath.Join(t.TempDir(), "zaps")), p
}

// request returns a zap request signed by the sender, with tags.
func request(t *testing.T, tags ...[]string) string {
	t.Helper()
	e := nostr.Event{CreatedAt: 1700000000, Kind: requestKind, Tags: tags, Content: "great post"}
	if err := key(t, 2).Sign(&e); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var (
	recipient = []string{"p", strings.Repeat("ab", 32)}
	relays    = []string{"relays", "wss://b.example", "wss://a.example", "wss://b.example"}
)

func TestRequest(t *testing.T) {
	z, _ := newZapper(t)
	coordinate := "30023:" + strings.Repeat("cd", 32) + ":post"

	valid := request(t, recipient, relays, []string{"amount", "21000"}, []string{"e", "id"}, []string{"a", coordinate}, []string{"P", z.PubKey()})
	if _, err := z.Request(valid, 21000); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	tampered := strings.Replace(valid, "great post", "other post", 1)
	invalid := map[string]struct {
		raw    string
		amount int64
	}{
		"wrong amount":        {valid, 1000},
		"bad signature":       {tampered, 21000},
		"not json":            {"{", 21000},
		"too long":            {strings.Repeat(" ", maxRequestLen+1) + valid, 21000},
		"no p":                {request(t, relays), 21000},
		"two p":               {request(t, recipient, recipient, relays), 21000},
		"two e":               {request(t, recipient, relays, []string{"e", "1"}, []string{"e", "2"}), 21000},
		"no relays":           {request(t, recipient), 21000},
		"bad coordinate":      {request(t, recipient, relays, []string{"a", "30023:xyz:post"}), 21000},
		"P names another key": {request(t, recipient, relays, []string{"P", strings.Repeat("ef", 32)}), 21000},
	}
	for name, tc := range invalid {
		if _, err := z.Request(tc.raw, tc.amount); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	var wrongKind nostr.Event
	json.Unmarshal([]byte(valid), &wrongKind)
	wrongKind.Kind = 1
	key(t, 2).Sign(&wrongKind)
	raw, _ := json.Marshal(wrongKind)
	if _, err := z.Request(string(raw), 21000); err == nil {
		t.Error("kind 1: accepted")
	}
}

func TestSettled_PublishesReceipt(t *testing.T) {
	z, p := newZapper(t)
	raw := request(t, recipient, relays, []string{"e", "event-id"}, []string{"k", "1"})
	hash := []byte{0xaa, 0xbb}
	if err := z.Track(hash, raw); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Unix(1700000100, 0)
	if err := z.Settled(context.Background(), lightning.Settled{
		PaymentHash: hash, PaymentRequest: "lnbc1", Preimage: []byte{0x01}, SettledAt: paidAt,
	}); err != nil {
		t.Fatal(err)
	}

	if len(p.events) != 1 {
		t.Fatalf("published %d events, want 1", len(p.events))
	}
	receipt := p.events[0]
	if err := receipt.Verify(); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	var sender nostr.Event
	json.Unmarshal([]byte(raw), &sender)
	want := [][]string{recipient, {"e", "event-id"}, {"k", "1"}, {"P", sender.PubKey}, {"bolt11", "lnbc1"}, {"description", raw}, {"preimage", "01"}}
	if receipt.Kind != receiptKind || receipt.PubKey != z.PubKey() || receipt.CreatedAt != paidAt.Unix() ||
		receipt.Content != "" || !slices.EqualFunc(receipt.Tags, want, slices.Equal) {
		t.Fatalf("receipt = %+v", receipt)
	}
	if !slices.Equal(p.relays, []string{"wss://a.example", "wss://b.example"}) {
		t.Fatalf("relays = %v", p.relays)
	}

	if err := z.Settled(context.Background(), lightning.Settled{PaymentHash: hash}); err != nil || len(p.events) != 1 {
		t.Fatalf("second settle: err %v, %d events", err, len(p.events))
	}
}

func TestSettled_IgnoresOtherInvoices(t *testing.T) {
	z, p := newZapper(t)
	if err := z.Settled(context.Background(), lightning.Settled{PaymentHash: []byte{1}}); err != nil || len(p.events) != 0 {
		t.Fatalf("err %v, %d events", err, len(p.events))
	}
}

func TestTrack_KeepsOldRequestsForRecovery(t *testing.T) {
	z, _ := newZapper(t)
	z.Track([]byte{1}, "old")
	old := time.Now().Add(-9 * 24 * time.Hour)
	os.Chtimes(z.path([]byte{1}), old, old)

	z.Track([]byte{2}, "new")
	if _, err := os.Stat(z.path([]byte{1})); err != nil {
		t.Fatalf("old request lost: %v", err)
	}
	if _, err := os.Stat(z.path([]byte{2})); err != nil {
		t.Fatalf("new request: %v", err)
	}
}

func TestSettled_KeepsRequestWhenCancelled(t *testing.T) {
	z, _ := newZapper(t)
	hash := []byte{0xcc}
	if err := z.Track(hash, request(t, recipient, relays)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := z.Settled(ctx, lightning.Settled{PaymentHash: hash}); err == nil {
		t.Fatal("cancelled settle returned no error")
	}
	if _, err := os.Stat(z.path(hash)); err != nil {
		t.Fatalf("zap request dropped on cancel: %v", err)
	}
}

type lookupInvoices struct{ invoices map[string]lightning.Invoice }

func (f lookupInvoices) LookupInvoice(_ context.Context, hash []byte) (lightning.Invoice, error) {
	invoice, ok := f.invoices[string(hash)]
	if !ok {
		return lightning.Invoice{}, errors.New("node unavailable")
	}
	return invoice, nil
}

func TestPrune_DeletesOnlyOldExpiredUnpaidRequests(t *testing.T) {
	z, _ := newZapper(t)
	old := time.Now().Add(-9 * 24 * time.Hour)
	for _, hash := range []string{"expired", "paid", "open", "unknown", "recent"} {
		if err := z.Track([]byte(hash), "request"); err != nil {
			t.Fatal(err)
		}
		if hash != "recent" {
			if err := os.Chtimes(z.path([]byte(hash)), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	invoices := lookupInvoices{map[string]lightning.Invoice{
		"expired": {Expired: true}, "paid": {Settled: true, Expired: true},
		"open": {}, "recent": {Expired: true},
	}}
	z.Prune(context.Background(), invoices)
	for _, hash := range []string{"expired", "paid", "open", "unknown", "recent"} {
		_, err := os.Stat(z.path([]byte(hash)))
		if hash == "expired" {
			if !os.IsNotExist(err) {
				t.Fatalf("expired request kept: %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s request lost: %v", hash, err)
		}
	}
}
