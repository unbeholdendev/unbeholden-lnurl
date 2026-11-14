package lnd

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

type fakeLightning struct {
	lnrpc.LightningClient
	listed    []*lnrpc.Invoice
	streamed  []*lnrpc.Invoice
	streamCtx context.Context
}

func (f *fakeLightning) ListInvoices(_ context.Context, req *lnrpc.ListInvoiceRequest, _ ...grpc.CallOption) (*lnrpc.ListInvoiceResponse, error) {
	if req.IndexOffset >= uint64(len(f.listed)) {
		return &lnrpc.ListInvoiceResponse{}, nil
	}
	next := req.IndexOffset + 1
	return &lnrpc.ListInvoiceResponse{Invoices: f.listed[req.IndexOffset:next], LastIndexOffset: next}, nil
}

func (f *fakeLightning) SubscribeInvoices(ctx context.Context, _ *lnrpc.InvoiceSubscription, _ ...grpc.CallOption) (lnrpc.Lightning_SubscribeInvoicesClient, error) {
	f.streamCtx = ctx
	return &fakeStream{invoices: f.streamed}, nil
}

type fakeStream struct {
	grpc.ClientStream
	invoices []*lnrpc.Invoice
}

func (s *fakeStream) Recv() (*lnrpc.Invoice, error) {
	if len(s.invoices) == 0 {
		return nil, io.EOF
	}
	inv := s.invoices[0]
	s.invoices = s.invoices[1:]
	return inv, nil
}

func settled(index uint64) *lnrpc.Invoice {
	return &lnrpc.Invoice{State: lnrpc.Invoice_SETTLED, SettleIndex: index, AmtPaidMsat: int64(index)}
}

type delivery struct{ amountMsat, resume uint64 }

func collect(t *testing.T, f *fakeLightning, from uint64) []delivery {
	t.Helper()
	var got []delivery
	(&Client{lightning: f}).SubscribeInvoices(context.Background(), from, func(amountMsat int64, _ string, resume uint64) error {
		got = append(got, delivery{uint64(amountMsat), resume})
		return nil
	})
	return got
}

func TestSubscribeInvoices_FromZeroReplaysSettledInvoices(t *testing.T) {
	f := &fakeLightning{
		listed:   []*lnrpc.Invoice{settled(2), {State: lnrpc.Invoice_OPEN}, settled(1)},
		streamed: []*lnrpc.Invoice{settled(3)},
	}
	want := []delivery{{1, 0}, {2, 0}, {3, 3}}
	if got := collect(t, f, 0); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

// A is listed while open, then settles at 1, and B at 2, before the next page.
func TestSubscribeInvoices_FromZeroKeepsSettlementsDuringListing(t *testing.T) {
	f := &fakeLightning{
		listed:   []*lnrpc.Invoice{{State: lnrpc.Invoice_OPEN}, settled(2)},
		streamed: []*lnrpc.Invoice{settled(1), settled(2), settled(3)},
	}
	want := []delivery{{2, 0}, {1, 1}, {3, 3}}
	if got := collect(t, f, 0); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

func TestSubscribeInvoices_CallbackErrorCancelsStream(t *testing.T) {
	f := &fakeLightning{streamed: []*lnrpc.Invoice{settled(4)}}
	c := &Client{lightning: f}

	errStop := errors.New("stop")
	err := c.SubscribeInvoices(context.Background(), 3, func(int64, string, uint64) error { return errStop })
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want %v", err, errStop)
	}
	if f.streamCtx.Err() == nil {
		t.Fatal("stream context still live after SubscribeInvoices returned")
	}
}
