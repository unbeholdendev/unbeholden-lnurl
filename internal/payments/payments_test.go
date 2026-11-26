package payments

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/bolt11/bolt11test"
	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
)

const (
	shopToken  = "shop-token-at-least-32-characters"
	otherToken = "other-token-at-least-32-characters"
)

var preimage = make([]byte, 32)

type fakeNode struct {
	mu      sync.Mutex
	send    func(bolt11 string) (lightning.Payment, error)
	lookups map[string]lightning.Payment
	sent    []string
	maxFees []int64
}

func (f *fakeNode) SendPayment(_ context.Context, bolt11 string, maxFeeMsat int64) (lightning.Payment, error) {
	f.mu.Lock()
	f.sent = append(f.sent, bolt11)
	f.maxFees = append(f.maxFees, maxFeeMsat)
	send := f.send
	f.mu.Unlock()
	if send != nil {
		return send(bolt11)
	}
	return lightning.Payment{Status: lightning.PaymentSucceeded, Preimage: preimage, FeeMsat: 3}, nil
}

func (f *fakeNode) LookupPayment(_ context.Context, hash []byte) (lightning.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payment, ok := f.lookups[hex.EncodeToString(hash)]
	if !ok {
		return lightning.Payment{}, lightning.ErrPaymentNotFound
	}
	return payment, nil
}

type fakeResolver struct{ invoice string }

func (f fakeResolver) Invoice(_ context.Context, address string, amountMsat int64, comment string) (string, error) {
	if address != "alice@example.com" {
		return "", errors.New("unknown address")
	}
	return f.invoice, nil
}

type notified struct {
	client string
	status lightning.PaymentStatus
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []notified
}

func (f *fakeNotifier) NotifyPayment(client string, payment Payment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, notified{client, payment.Status})
}

type harness struct {
	t        *testing.T
	node     *fakeNode
	notifier *fakeNotifier
	resolver fakeResolver
	clients  map[string]config.PaymentClient
	path     string
	handler  *Handler
	mux      *http.ServeMux
}

func newHarness(t *testing.T, clients map[string]config.PaymentClient) *harness {
	t.Helper()
	if clients == nil {
		clients = map[string]config.PaymentClient{"shop": {Token: shopToken}, "other": {Token: otherToken}}
	}
	h := &harness{t: t, node: &fakeNode{lookups: map[string]lightning.Payment{}}, notifier: &fakeNotifier{}, clients: clients, path: filepath.Join(t.TempDir(), "payments.json")}
	h.restart()
	return h
}

func (h *harness) restart() {
	h.t.Helper()
	handler, err := New(context.Background(), h.clients, h.node, &h.resolver, h.notifier, &chaincfg.MainNetParams, h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	h.handler = handler
	h.mux = http.NewServeMux()
	handler.Register(h.mux, func(r *http.Request) string { return r.Header.Get("X-Test-IP") })
}

func (h *harness) do(method, path, token, body string) (int, map[string]any) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Test-IP", "100.64.0.5")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		h.t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
	}
	return rec.Code, got
}

func (h *harness) pay(token, id, invoice string) (int, map[string]any) {
	h.t.Helper()
	return h.do("POST", "/payments", token, fmt.Sprintf(`{"id":%q,"invoice":%q}`, id, invoice))
}

func invoice(n byte, amountMsat int64) string {
	return bolt11test.Invoice(amountMsat, [32]byte{n}, "test")
}

func hash(n byte) string {
	h := [32]byte{n}
	return hex.EncodeToString(h[:])
}

func TestPay_Invoice(t *testing.T) {
	h := newHarness(t, nil)
	code, got := h.pay(shopToken, "order-1", invoice(1, 21000))
	if code != 200 || got["status"] != "succeeded" || got["amount_msat"] != 21000.0 || got["fee_msat"] != 3.0 ||
		got["preimage"] != hex.EncodeToString(preimage) || got["payment_hash"] != hash(1) || got["id"] != "order-1" || got["completed_at"] == nil {
		t.Fatalf("pay: %d %v", code, got)
	}
	if h.node.maxFees[0] != 10000 {
		t.Fatalf("max fee = %d, want the 10 sat floor", h.node.maxFees[0])
	}
	if len(h.notifier.events) != 1 || h.notifier.events[0] != (notified{"shop", lightning.PaymentSucceeded}) {
		t.Fatalf("notified %v", h.notifier.events)
	}

	h.restart()
	if code, again := h.do("GET", "/payments/order-1", shopToken, ""); code != 200 || again["preimage"] != got["preimage"] {
		t.Fatalf("get after restart: %d %v", code, again)
	}
	if code, _ := h.do("GET", "/payments/order-1", otherToken, ""); code != 404 {
		t.Fatalf("another client's payment: %d", code)
	}
}

func TestPay_Address(t *testing.T) {
	h := newHarness(t, nil)
	h.resolver.invoice = invoice(1, 5000)
	code, got := h.do("POST", "/payments", shopToken, `{"id":"tip","address":"alice@example.com","amount_msat":5000,"comment":"hi"}`)
	if code != 200 || got["status"] != "succeeded" || got["destination"] != "alice@example.com" {
		t.Fatalf("pay address: %d %v", code, got)
	}
	if code, got := h.do("POST", "/payments", shopToken, `{"id":"tip2","address":"bob@example.com","amount_msat":5000}`); code != 502 || len(h.node.sent) != 1 {
		t.Fatalf("unresolvable address: %d %v", code, got)
	}
}

func TestPay_Authentication(t *testing.T) {
	h := newHarness(t, map[string]config.PaymentClient{
		"shop":   {Token: shopToken},
		"office": {Token: otherToken, AllowedIPs: []config.Prefix{prefix(t, "192.0.2.0/24")}},
	})
	for _, token := range []string{"", "wrong", shopToken + "x"} {
		if code, _ := h.do("GET", "/payments", token, ""); code != 401 {
			t.Errorf("token %q: %d", token, code)
		}
	}
	if code, _ := h.do("GET", "/payments", otherToken, ""); code != 403 {
		t.Errorf("disallowed address: %d", code)
	}
	if code, _ := h.do("GET", "/payments", shopToken, ""); code != 200 {
		t.Errorf("valid token: %d", code)
	}
}

func prefix(t *testing.T, s string) config.Prefix {
	t.Helper()
	var p config.Prefix
	if err := p.UnmarshalText([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPay_Idempotency(t *testing.T) {
	h := newHarness(t, nil)
	first := invoice(1, 1000)
	h.pay(shopToken, "a", first)
	if code, got := h.pay(shopToken, "a", first); code != 200 || got["status"] != "succeeded" || len(h.node.sent) != 1 {
		t.Fatalf("retry: %d %v, sent %d", code, got, len(h.node.sent))
	}
	if code, _ := h.pay(shopToken, "a", invoice(2, 1000)); code != 409 {
		t.Fatalf("reused id: %d", code)
	}
	if code, _ := h.pay(otherToken, "a", invoice(2, 1000)); code != 200 {
		t.Fatalf("same id for another client: %d", code)
	}
	if code, _ := h.pay(otherToken, "b", first); code != 409 || len(h.node.sent) != 2 {
		t.Fatalf("paid invoice under a new id: %d", code)
	}
}

func TestPay_FailedPaymentCanBeRetried(t *testing.T) {
	h := newHarness(t, nil)
	h.node.send = func(string) (lightning.Payment, error) {
		return lightning.Payment{}, fmt.Errorf("%w: no route", lightning.ErrPaymentFailed)
	}
	if code, got := h.pay(shopToken, "a", invoice(1, 1000)); code != 200 || got["status"] != "failed" {
		t.Fatalf("failed payment: %d %v", code, got)
	}
	h.node.send = nil
	if code, got := h.pay(shopToken, "b", invoice(1, 1000)); code != 200 || got["status"] != "succeeded" {
		t.Fatalf("retry under new id: %d %v", code, got)
	}
	want := []notified{{"shop", lightning.PaymentFailed}, {"shop", lightning.PaymentSucceeded}}
	if fmt.Sprint(h.notifier.events) != fmt.Sprint(want) {
		t.Fatalf("notified %v, want %v", h.notifier.events, want)
	}
}

func TestPay_UnknownOutcomeIsReconciled(t *testing.T) {
	h := newHarness(t, nil)
	h.node.send = func(string) (lightning.Payment, error) { return lightning.Payment{}, errors.New("connection reset") }
	if code, got := h.pay(shopToken, "a", invoice(1, 1000)); code != 200 || got["status"] != "pending" {
		t.Fatalf("unknown outcome: %d %v", code, got)
	}
	if _, got := h.do("GET", "/payments/a", shopToken, ""); got["status"] != "pending" {
		t.Fatalf("still in flight on the node: %v", got)
	}
	h.node.lookups[hash(1)] = lightning.Payment{Status: lightning.PaymentSucceeded, Preimage: preimage, FeeMsat: 4}
	if _, got := h.do("GET", "/payments", shopToken, ""); got["payments"].([]any)[0].(map[string]any)["fee_msat"] != 4.0 {
		t.Fatalf("list after settling: %v", got)
	}
	if len(h.notifier.events) != 1 {
		t.Fatalf("notified %v", h.notifier.events)
	}
}

func TestNew_ReconcilesPendingPayments(t *testing.T) {
	h := newHarness(t, nil)
	old := time.Now().Add(-time.Hour).UTC()
	state := stateFile{Payments: []*record{
		{Client: "shop", Payment: Payment{ID: "settled", Status: lightning.PaymentPending, PaymentHash: hash(1), CreatedAt: old}},
		{Client: "shop", Payment: Payment{ID: "never-sent", Status: lightning.PaymentPending, PaymentHash: hash(2), CreatedAt: old}},
		{Client: "shop", Payment: Payment{ID: "just-sent", Status: lightning.PaymentPending, PaymentHash: hash(3), CreatedAt: time.Now().UTC()}},
		{Client: "shop", Payment: Payment{ID: "expired", Status: lightning.PaymentSucceeded, PaymentHash: hash(4), CreatedAt: old.AddDate(0, 0, -100), CompletedAt: new(old.AddDate(0, 0, -100))}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(h.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	h.node.lookups[hash(1)] = lightning.Payment{Status: lightning.PaymentSucceeded, Preimage: preimage}
	h.restart()
	for id, want := range map[string]string{"settled": "succeeded", "never-sent": "failed", "just-sent": "pending"} {
		if _, got := h.do("GET", "/payments/"+id, shopToken, ""); got["status"] != want {
			t.Errorf("%s: %v, want %s", id, got, want)
		}
	}
	if code, _ := h.do("GET", "/payments/expired", shopToken, ""); code != 404 {
		t.Errorf("payment past retention: %d", code)
	}
}

func TestPay_Limits(t *testing.T) {
	h := newHarness(t, map[string]config.PaymentClient{
		"shop": {Token: shopToken, MaxPayment: 50000, DailyBudget: 100000, MaxFee: 1000},
	})
	if code, _ := h.pay(shopToken, "big", invoice(1, 60000)); code != 403 {
		t.Fatalf("over max_payment: %d", code)
	}
	h.node.send = func(string) (lightning.Payment, error) { return lightning.Payment{}, errors.New("timeout") }
	if code, got := h.pay(shopToken, "pending", invoice(2, 49000)); code != 200 || got["status"] != "pending" {
		t.Fatalf("pending: %d %v", code, got)
	}
	h.node.send = nil
	if code, _ := h.pay(shopToken, "a", invoice(3, 49000)); code != 200 {
		t.Fatalf("within budget: %d", code)
	}
	if code, got := h.pay(shopToken, "b", invoice(4, 500)); code != 403 {
		t.Fatalf("pending payment's maximum fee not reserved: %d %v", code, got)
	}
	if h.node.maxFees[0] != 1000 {
		t.Fatalf("max fee = %d, want configured 1000", h.node.maxFees[0])
	}
}

func TestPay_RejectsInvalidRequests(t *testing.T) {
	h := newHarness(t, nil)
	amountless := invoice(1, 0)
	for name, body := range map[string]string{
		"not JSON":            `nope`,
		"unknown field":       `{"id":"a","invoice":"` + invoice(1, 1000) + `","extra":1}`,
		"bad id":              `{"id":"a b","invoice":"` + invoice(1, 1000) + `"}`,
		"no destination":      `{"id":"a"}`,
		"both destinations":   `{"id":"a","invoice":"` + invoice(1, 1000) + `","address":"alice@example.com","amount_msat":1000}`,
		"amount with invoice": `{"id":"a","invoice":"` + invoice(1, 1000) + `","amount_msat":1000}`,
		"address no amount":   `{"id":"a","address":"alice@example.com"}`,
		"amountless invoice":  `{"id":"a","invoice":"` + amountless + `"}`,
		"other network":       `{"id":"a","invoice":"` + bolt11test.InvoiceFor(&chaincfg.TestNet3Params, 1000, [32]byte{1}, "x") + `"}`,
	} {
		if code, got := h.do("POST", "/payments", shopToken, body); code != 400 {
			t.Errorf("%s: %d %v", name, code, got)
		}
	}
	if len(h.node.sent) != 0 {
		t.Fatalf("sent %d payments", len(h.node.sent))
	}
}

func TestList_Since(t *testing.T) {
	h := newHarness(t, nil)
	h.pay(shopToken, "a", invoice(1, 1000))
	h.pay(otherToken, "b", invoice(2, 1000))
	_, got := h.do("GET", "/payments", shopToken, "")
	if list := got["payments"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != "a" {
		t.Fatalf("list: %v", got)
	}
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	if _, got := h.do("GET", "/payments?since="+future, shopToken, ""); len(got["payments"].([]any)) != 0 {
		t.Fatalf("since filter: %v", got)
	}
	if code, _ := h.do("GET", "/payments?since=yesterday", shopToken, ""); code != 400 {
		t.Fatalf("bad since: %d", code)
	}
}

func TestPay_OldPendingPaymentStillReservesBudget(t *testing.T) {
	h := newHarness(t, map[string]config.PaymentClient{"shop": {Token: shopToken, DailyBudget: 100000, MaxFee: 1000}})
	state := stateFile{Payments: []*record{{
		Client: "shop", MaxFeeMsat: 1000,
		Payment: Payment{ID: "stuck", Status: lightning.PaymentPending, AmountMsat: 90000, PaymentHash: hash(9), CreatedAt: time.Now().Add(-25 * time.Hour).UTC()},
	}}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(h.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	h.node.lookups[hash(9)] = lightning.Payment{Status: lightning.PaymentPending}
	h.restart()
	if code, got := h.pay(shopToken, "next", invoice(1, 50000)); code != 403 {
		t.Fatalf("budget ignored a pending payment older than a day: %d %v", code, got)
	}
}
