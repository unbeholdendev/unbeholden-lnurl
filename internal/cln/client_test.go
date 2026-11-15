package cln

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"unbeholden-lnurl/internal/lightning"
)

type reply struct {
	status int
	body   any
}

func rpcFail(code int, message string) reply {
	return reply{http.StatusInternalServerError, map[string]any{"code": code, "message": message}}
}

func ok(body any) reply { return reply{http.StatusCreated, body} }

// fakeNode serves each method from a queue of replies, recording requests.
type fakeNode struct {
	replies map[string][]reply
	calls   []map[string]any
	server  *httptest.Server
}

func newFakeNode(t *testing.T, replies map[string][]reply) (*fakeNode, *Client) {
	t.Helper()
	f := &fakeNode{replies: replies}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Rune") != "test-rune" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/v1/")
		var params map[string]any
		json.NewDecoder(r.Body).Decode(&params)
		params["method"] = method
		f.calls = append(f.calls, params)
		queue := f.replies[method]
		if len(queue) == 0 {
			t.Errorf("unexpected call to %s", method)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.replies[method] = queue[1:]
		w.WriteHeader(queue[0].status)
		json.NewEncoder(w).Encode(queue[0].body)
	}))
	t.Cleanup(f.server.Close)

	return f, connect(t, f.server.URL, f.server.Certificate().Raw)
}

func connect(t *testing.T, url string, caDER []byte) *Client {
	t.Helper()
	dir := t.TempDir()
	runePath := filepath.Join(dir, "rune")
	caPath := filepath.Join(dir, "ca.pem")
	os.WriteFile(runePath, []byte("test-rune\n"), 0600)
	os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600)
	c, err := Connect(url, runePath, caPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConnect_VerifiesAgainstCAWhateverTheHostName(t *testing.T) {
	_, c := newFakeNode(t, map[string][]reply{"getinfo": {ok(map[string]any{"network": "bitcoin"})}})
	c.url = strings.Replace(c.url, "127.0.0.1", "localhost", 1)
	params, err := c.Network(context.Background())
	if err != nil || params.Name != "mainnet" {
		t.Fatalf("Network() = %v, %v; want mainnet", params, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = connect(t, c.url, der).Network(context.Background())
	if errors.As(err, new(x509.UnknownAuthorityError)) {
		return
	}
	t.Fatalf("Network() with another CA: err = %v, want unknown authority", err)
}

func TestSendPayment_Outcomes(t *testing.T) {
	noRoute := rpcFail(205, "could not find a usable set of paths")
	pays := func(statuses ...string) reply {
		var p []map[string]string
		for _, s := range statuses {
			p = append(p, map[string]string{"status": s})
		}
		return ok(map[string]any{"pays": p})
	}
	cases := map[string]struct {
		replies    map[string][]reply
		wantErr    bool
		wantFailed bool
	}{
		"complete":              {replies: map[string][]reply{"pay": {ok(map[string]string{"status": "complete"})}}},
		"pending":               {replies: map[string][]reply{"pay": {ok(map[string]string{"status": "pending"})}}, wantErr: true},
		"failed, nothing sent":  {replies: map[string][]reply{"pay": {noRoute}, "listpays": {pays()}}, wantErr: true, wantFailed: true},
		"failed, parts failed":  {replies: map[string][]reply{"pay": {noRoute}, "listpays": {pays("failed", "failed")}}, wantErr: true, wantFailed: true},
		"error, part pending":   {replies: map[string][]reply{"pay": {noRoute}, "listpays": {pays("failed", "pending")}}, wantErr: true},
		"error, part complete":  {replies: map[string][]reply{"pay": {noRoute}, "listpays": {pays("failed", "complete")}}},
		"error, listpays fails": {replies: map[string][]reply{"pay": {noRoute}, "listpays": {rpcFail(-1, "boom")}}, wantErr: true},
		"not an RPC error":      {replies: map[string][]reply{"pay": {{http.StatusBadGateway, "proxy down"}}}, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := newFakeNode(t, tc.replies)
			err := c.SendPayment(context.Background(), "lnbc1", 10)
			if (err != nil) != tc.wantErr || errors.Is(err, lightning.ErrPaymentFailed) != tc.wantFailed {
				t.Fatalf("err = %v, want error %v, definite failure %v", err, tc.wantErr, tc.wantFailed)
			}
			if got := f.calls[0]["maxfee"]; got != float64(10000) {
				t.Errorf("maxfee = %v, want 10000 msat", got)
			}
		})
	}
}

func TestInvoices_MemoRoundTripsThroughLabel(t *testing.T) {
	f, c := newFakeNode(t, map[string][]reply{"invoice": {ok(map[string]string{"bolt11": "lnbc1", "payment_hash": "abcd"})}})
	inv, err := c.AddInvoice(context.Background(), 5000, `[["text/plain","a<b"]]`, "thanks for the tip")
	if err != nil || inv.PaymentRequest != "lnbc1" || string(inv.PaymentHash) != "\xab\xcd" {
		t.Fatalf("AddInvoice() = %+v, %v", inv, err)
	}
	call := f.calls[0]
	if call["description"] != `[["text/plain","a<b"]]` || call["deschashonly"] != true || call["amount_msat"] != float64(5000) {
		t.Fatalf("invoice params = %v", call)
	}

	label := call["label"].(string)
	if got := (invoice{Label: label, Description: "metadata"}).memo(); got != "thanks for the tip" {
		t.Errorf("memo of our invoice = %q", got)
	}
	if got := (invoice{Label: "other", Description: "coffee"}).memo(); got != "coffee" {
		t.Errorf("memo of another invoice = %q", got)
	}
}

func TestSubscribeInvoices_FollowsPayIndexAcrossTimeouts(t *testing.T) {
	paid := func(index int) reply {
		return ok(map[string]any{"pay_index": index, "amount_received_msat": 1000 * index, "label": "l", "description": "d"})
	}
	errStop := errors.New("stop")
	f, c := newFakeNode(t, map[string][]reply{
		"waitanyinvoice": {paid(4), rpcFail(904, "Timed out"), paid(5)},
	})
	var got []uint64
	err := c.SubscribeInvoices(context.Background(), 3, func(amountMsat int64, memo string, resume uint64) error {
		got = append(got, resume)
		if amountMsat != 1000*int64(resume) || memo != "d" {
			t.Errorf("invoice %d: amount %d, memo %q", resume, amountMsat, memo)
		}
		if resume == 5 {
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want %v", err, errStop)
	}
	if !slices.Equal(got, []uint64{4, 5}) {
		t.Fatalf("delivered %v, want [4 5]", got)
	}
	var from []any
	for _, call := range f.calls {
		from = append(from, call["lastpay_index"])
	}
	if want := []any{float64(3), float64(4), float64(4)}; !slices.Equal(from, want) {
		t.Fatalf("waited from %v, want %v", from, want)
	}
}

func TestLastPaidIndex(t *testing.T) {
	_, c := newFakeNode(t, map[string][]reply{"listinvoices": {ok(map[string]any{"invoices": []map[string]any{
		{"pay_index": 2}, {}, {"pay_index": 7}, {"pay_index": 3},
	}})}})
	if got, err := c.LastPaidIndex(context.Background()); err != nil || got != 7 {
		t.Fatalf("LastPaidIndex() = %d, %v; want 7", got, err)
	}
}

func TestLookupInvoice(t *testing.T) {
	f, c := newFakeNode(t, map[string][]reply{"listinvoices": {
		ok(map[string]any{"invoices": []map[string]string{{"bolt11": "lnbc1", "status": "unpaid"}}}),
		ok(map[string]any{"invoices": []map[string]string{{"bolt11": "lnbc1", "status": "paid", "payment_preimage": "beef"}}}),
		ok(map[string]any{"invoices": []any{}}),
	}})
	ctx := context.Background()

	unpaid, err := c.LookupInvoice(ctx, []byte{0xab})
	if err != nil || unpaid.Settled || unpaid.Preimage != nil || unpaid.PaymentRequest != "lnbc1" {
		t.Fatalf("unpaid = %+v, %v", unpaid, err)
	}
	if f.calls[0]["payment_hash"] != "ab" {
		t.Fatalf("listinvoices params = %v", f.calls[0])
	}
	paid, err := c.LookupInvoice(ctx, []byte{0xab})
	if err != nil || !paid.Settled || string(paid.Preimage) != "\xbe\xef" {
		t.Fatalf("paid = %+v, %v", paid, err)
	}
	if _, err := c.LookupInvoice(ctx, []byte{0xab}); !errors.Is(err, lightning.ErrInvoiceNotFound) {
		t.Fatalf("missing: err = %v, want ErrInvoiceNotFound", err)
	}
}
