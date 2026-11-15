package lnurl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

type fakeNode struct {
	mu       sync.Mutex
	pay      func(ctx context.Context) error
	payments int
	notified []string
	invoices map[string]lightning.Invoice
	last     string
}

func (f *fakeNode) SendPayment(ctx context.Context, _ string, _ int64) error {
	f.mu.Lock()
	f.payments++
	f.mu.Unlock()
	if f.pay != nil {
		return f.pay(ctx)
	}
	return nil
}

func (f *fakeNode) AddInvoice(_ context.Context, amountMsat int64, _, memo string) (lightning.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invoices == nil {
		f.invoices = map[string]lightning.Invoice{}
	}
	hash := make([]byte, 32)
	hash[0] = byte(len(f.invoices) + 1)
	invoice := lightning.Invoice{PaymentRequest: fmt.Sprintf("lnbc-%d-%s", amountMsat, memo), PaymentHash: hash}
	f.last = hex.EncodeToString(hash)
	f.invoices[f.last] = invoice
	return invoice, nil
}

func (f *fakeNode) LookupInvoice(_ context.Context, paymentHash []byte) (lightning.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	invoice, ok := f.invoices[hex.EncodeToString(paymentHash)]
	if !ok {
		return lightning.Invoice{}, lightning.ErrInvoiceNotFound
	}
	return invoice, nil
}

func (f *fakeNode) lastHash() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeNode) settle(hash string, preimage []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	invoice := f.invoices[hash]
	invoice.Settled = true
	invoice.Preimage = preimage
	f.invoices[hash] = invoice
}

func (f *fakeNode) NotifyVoucherClaimed(voucherID string, amountSats int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, fmt.Sprintf("claimed %s %d", voucherID, amountSats))
}

func (f *fakeNode) NotifyInvoiceCreated(amountSats int64, address, comment string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, fmt.Sprintf("invoice %d %s %s", amountSats, address, comment))
}

var testVoucher = config.Voucher{ID: "tip-001", Token: "abc123", Domain: "example.com", Description: "tip", Amount: 1_000_000}

func newWithdraw(t *testing.T, node *fakeNode, statePath string) *WithdrawHandler {
	t.Helper()
	h, err := NewWithdrawHandler([]config.Voucher{testVoucher}, []byte("k1 key"), &chaincfg.MainNetParams, node, node, statePath)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func invoice(t *testing.T, net *chaincfg.Params, amountMsat int64) string {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	rand.Read(hash[:])
	inv, err := zpay32.NewInvoice(net, hash, time.Now(), zpay32.Amount(lnwire.MilliSatoshi(amountMsat)), zpay32.Description("tip"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := inv.Encode(zpay32.MessageSigner{SignCompact: func(msg []byte) ([]byte, error) {
		return ecdsa.SignCompact(key, chainhash.HashB(msg), true), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func call(t *testing.T, ctx context.Context, handler http.HandlerFunc, target, token string) map[string]any {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, "GET", target, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	handler(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

func first(t *testing.T, h *WithdrawHandler) map[string]any {
	t.Helper()
	return call(t, context.Background(), h.HandleFirst, "https://example.com/lnurl/withdraw/abc123", testVoucher.Token)
}

func claimWith(t *testing.T, ctx context.Context, h *WithdrawHandler, k1, pr string) map[string]any {
	t.Helper()
	q := url.Values{"k1": {k1}, "pr": {pr}}
	return call(t, ctx, h.HandleClaim, "https://example.com/lnurl/withdraw/abc123/claim?"+q.Encode(), testVoucher.Token)
}

func claim(t *testing.T, h *WithdrawHandler) map[string]any {
	t.Helper()
	resp := first(t, h)
	k1, _ := resp["k1"].(string)
	if k1 == "" {
		return resp
	}
	return claimWith(t, context.Background(), h, k1, invoice(t, &chaincfg.MainNetParams, testVoucher.Amount))
}

func claimedOnDisk(t *testing.T, statePath string) bool {
	t.Helper()
	h, err := NewWithdrawHandler([]config.Voucher{testVoucher}, nil, nil, nil, nil, statePath)
	if err != nil {
		t.Fatal(err)
	}
	return len(h.UnclaimedVouchers()) == 0
}

func wantStatus(t *testing.T, resp map[string]any, status, reason string) {
	t.Helper()
	if resp["status"] != status || (reason != "" && resp["reason"] != reason) {
		t.Fatalf("response = %v, want status %s reason %q", resp, status, reason)
	}
}

func TestWithdraw_ClaimPaysOnce(t *testing.T) {
	node := &fakeNode{}
	statePath := filepath.Join(t.TempDir(), "state.json")
	h := newWithdraw(t, node, statePath)

	resp := first(t, h)
	if resp["tag"] != "withdrawRequest" || resp["callback"] != "https://example.com/lnurl/withdraw/abc123/claim" || resp["maxWithdrawable"] != float64(testVoucher.Amount) {
		t.Fatalf("withdraw request = %v", resp)
	}
	wantStatus(t, claimWith(t, context.Background(), h, resp["k1"].(string), invoice(t, &chaincfg.MainNetParams, testVoucher.Amount)), "OK", "")
	wantStatus(t, claim(t, h), "ERROR", "this voucher has already been claimed")

	if node.payments != 1 {
		t.Fatalf("payments = %d, want 1", node.payments)
	}
	if len(node.notified) != 1 || node.notified[0] != "claimed tip-001 1000" {
		t.Fatalf("notified = %v", node.notified)
	}
	if !claimedOnDisk(t, statePath) {
		t.Fatal("claim not persisted")
	}
}

func TestWithdraw_ConcurrentClaimsPayOnce(t *testing.T) {
	node := &fakeNode{}
	h := newWithdraw(t, node, filepath.Join(t.TempDir(), "state.json"))
	k1 := first(t, h)["k1"].(string)
	pr := invoice(t, &chaincfg.MainNetParams, testVoucher.Amount)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { claimWith(t, context.Background(), h, k1, pr) })
	}
	wg.Wait()
	if node.payments != 1 {
		t.Fatalf("payments = %d, want 1", node.payments)
	}
}

func TestWithdraw_UnknownOutcomeKeepsClaim(t *testing.T) {
	node := &fakeNode{pay: func(context.Context) error { return errors.New("recv payment update: stream reset") }}
	statePath := filepath.Join(t.TempDir(), "state.json")
	h := newWithdraw(t, node, statePath)

	wantStatus(t, claim(t, h), "ERROR", "")
	wantStatus(t, claim(t, h), "ERROR", "this voucher has already been claimed")
	if node.payments != 1 {
		t.Fatalf("payments = %d, want 1", node.payments)
	}
	if !claimedOnDisk(t, statePath) {
		t.Fatal("claim with unknown outcome not persisted")
	}
}

func TestWithdraw_PaymentOutlivesRequest(t *testing.T) {
	var payCtxErr error
	node := &fakeNode{pay: func(ctx context.Context) error {
		payCtxErr = ctx.Err()
		return nil
	}}
	h := newWithdraw(t, node, filepath.Join(t.TempDir(), "state.json"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	claimWith(t, ctx, h, first(t, h)["k1"].(string), invoice(t, &chaincfg.MainNetParams, testVoucher.Amount))
	if node.payments != 1 || payCtxErr != nil {
		t.Fatalf("payments = %d, payment context error = %v; want 1, nil", node.payments, payCtxErr)
	}
}

func TestWithdraw_DefiniteFailureReleases(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	node := &fakeNode{pay: func(context.Context) error {
		return fmt.Errorf("%w: FAILURE_REASON_NO_ROUTE", lightning.ErrPaymentFailed)
	}}
	h := newWithdraw(t, node, statePath)

	wantStatus(t, claim(t, h), "ERROR", "payment failed")
	if claimedOnDisk(t, statePath) {
		t.Fatal("failed claim still persisted")
	}
	node.pay = nil
	wantStatus(t, claim(t, h), "OK", "")
}

func TestWithdraw_ClaimPersistedBeforePaying(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	var persisted bool
	node := &fakeNode{pay: func(context.Context) error {
		persisted = claimedOnDisk(t, statePath)
		return nil
	}}
	wantStatus(t, claim(t, newWithdraw(t, node, statePath)), "OK", "")
	if !persisted {
		t.Fatal("claim was not on disk while paying")
	}
}

func TestWithdraw_NoPaymentWhenClaimCannotBePersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	node := &fakeNode{}
	h := newWithdraw(t, node, filepath.Join(dir, "state.json"))
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, claim(t, h), "ERROR", "")
	if node.payments != 0 {
		t.Fatalf("payments = %d, want 0", node.payments)
	}
	if len(h.UnclaimedVouchers()) != 1 {
		t.Fatal("voucher left claimed though nothing was paid")
	}
}

func TestNewWithdrawHandler_CorruptStateFails(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"claimed":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithdrawHandler([]config.Voucher{testVoucher}, nil, nil, nil, nil, statePath); err == nil {
		t.Fatal("expected error for corrupt state")
	}
}

func TestWithdraw_RejectsBadClaims(t *testing.T) {
	h := newWithdraw(t, &fakeNode{}, filepath.Join(t.TempDir(), "state.json"))
	k1 := first(t, h)["k1"].(string)
	pr := invoice(t, &chaincfg.MainNetParams, testVoucher.Amount)
	tampered := []byte(k1)
	tampered[len(tampered)-1] ^= 1

	cases := map[string]struct{ k1, pr, reason string }{
		"tampered k1":   {string(tampered), pr, "invalid k1"},
		"other voucher": {hex.EncodeToString(h.signK1("tip-002", uint64(time.Now().Add(time.Minute).Unix()))), pr, "invalid k1"},
		"expired k1":    {hex.EncodeToString(h.signK1(testVoucher.ID, uint64(time.Now().Add(-time.Second).Unix()))), pr, "k1 expired, please try again"},
		"short k1":      {"abcd", pr, "invalid k1"},
		"wrong amount":  {k1, invoice(t, &chaincfg.MainNetParams, 2000), ""},
		"wrong network": {k1, invoice(t, &chaincfg.TestNet3Params, testVoucher.Amount), ""},
		"garbage":       {k1, "lnbc1invalid", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, claimWith(t, context.Background(), h, tc.k1, tc.pr), "ERROR", tc.reason)
		})
	}
	wantStatus(t, claimWith(t, context.Background(), h, k1, pr), "OK", "")
}

func TestWithdraw_WithoutNotifier(t *testing.T) {
	node := &fakeNode{}
	h, err := NewWithdrawHandler([]config.Voucher{testVoucher}, []byte("k1 key"), &chaincfg.MainNetParams, node, nil, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, claim(t, h), "OK", "")
}
