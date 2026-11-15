package lnd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
	"unbeholden-lnurl/internal/lnd/lnrpc"
	"unbeholden-lnurl/internal/lnd/routerrpc"

	"unbeholden-lnurl/internal/lightning"
)

// Client wraps the LND gRPC connection and exposes the methods we need.
type Client struct {
	conn      *grpc.ClientConn
	lightning lnrpc.LightningClient
	router    routerrpc.RouterClient
}

// Connect establishes a gRPC connection to LND using TLS and macaroon auth.
func Connect(host, tlsCertPath, macaroonPath string) (*Client, error) {
	tlsCert, err := os.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("read tls cert: %w", err)
	}
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(tlsCert) {
		return nil, fmt.Errorf("failed to append tls cert")
	}
	tlsCreds := credentials.NewClientTLSFromCert(certPool, "")

	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("read macaroon: %w", err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unmarshal macaroon: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(&macaroonCredential{mac: mac}),
	}
	conn, err := grpc.NewClient("passthrough:///"+host, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to lnd: %w", err)
	}

	return &Client{
		conn:      conn,
		lightning: lnrpc.NewLightningClient(conn),
		router:    routerrpc.NewRouterClient(conn),
	}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// AddInvoice creates an invoice for amountMsat committing to the hash of
// description, with memo stored on the node.
func (c *Client) AddInvoice(ctx context.Context, amountMsat int64, description, memo string) (lightning.Invoice, error) {
	descriptionHash := sha256.Sum256([]byte(description))
	resp, err := c.lightning.AddInvoice(ctx, &lnrpc.Invoice{
		ValueMsat:       amountMsat,
		DescriptionHash: descriptionHash[:],
		Memo:            memo,
	})
	if err != nil {
		return lightning.Invoice{}, fmt.Errorf("add invoice: %w", err)
	}
	return lightning.Invoice{PaymentRequest: resp.PaymentRequest, PaymentHash: resp.RHash}, nil
}

// LookupInvoice returns the invoice with paymentHash, or
// lightning.ErrInvoiceNotFound.
func (c *Client) LookupInvoice(ctx context.Context, paymentHash []byte) (lightning.Invoice, error) {
	resp, err := c.lightning.LookupInvoice(ctx, &lnrpc.PaymentHash{RHash: paymentHash})
	if status.Code(err) == codes.NotFound {
		return lightning.Invoice{}, lightning.ErrInvoiceNotFound
	}
	if err != nil {
		return lightning.Invoice{}, fmt.Errorf("lookup invoice: %w", err)
	}
	invoice := lightning.Invoice{
		PaymentRequest: resp.PaymentRequest,
		PaymentHash:    resp.RHash,
		Settled:        resp.State == lnrpc.Invoice_SETTLED,
		Expired:        resp.State == lnrpc.Invoice_CANCELED,
	}
	if invoice.Settled {
		invoice.Preimage = resp.RPreimage
	}
	return invoice, nil
}

// SendPayment pays a BOLT-11 invoice. An error wrapping
// lightning.ErrPaymentFailed means
// the payment definitely failed; any other error leaves the outcome unknown.
// Cancelling ctx stops waiting but does not stop the payment.
func (c *Client) SendPayment(ctx context.Context, bolt11 string, maxFeeSat int64) error {
	stream, err := c.router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: bolt11,
		TimeoutSeconds: 60,
		FeeLimitSat:    maxFeeSat,
		MaxParts:       16,
	})
	if err != nil {
		return fmt.Errorf("send payment: %w", err)
	}

	for {
		payment, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv payment update: %w", err)
		}
		switch payment.Status {
		case lnrpc.Payment_SUCCEEDED:
			return nil
		case lnrpc.Payment_FAILED:
			return fmt.Errorf("%w: %s", lightning.ErrPaymentFailed, payment.FailureReason)
		}
	}
}

// Network returns the parameters of the chain LND runs on.
func (c *Client) Network(ctx context.Context) (*chaincfg.Params, error) {
	info, err := c.lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("get info: %w", err)
	}
	if len(info.Chains) == 0 {
		return nil, fmt.Errorf("get info: no chain reported")
	}
	return lightning.ChainParams(info.Chains[0].Network)
}

// LastPaidIndex returns the highest settle index on the node, or 0 if no
// invoice has settled.
func (c *Client) LastPaidIndex(ctx context.Context) (uint64, error) {
	invoices, err := c.settledInvoices(ctx)
	if err != nil || len(invoices) == 0 {
		return 0, err
	}
	return invoices[len(invoices)-1].SettleIndex, nil
}

// settledInvoices returns every settled invoice on the node, in settle order.
func (c *Client) settledInvoices(ctx context.Context) ([]*lnrpc.Invoice, error) {
	var settled []*lnrpc.Invoice
	var offset uint64
	for {
		resp, err := c.lightning.ListInvoices(ctx, &lnrpc.ListInvoiceRequest{
			IndexOffset:    offset,
			NumMaxInvoices: 1000,
		})
		if err != nil {
			return nil, fmt.Errorf("list invoices: %w", err)
		}
		if len(resp.Invoices) == 0 {
			slices.SortFunc(settled, func(a, b *lnrpc.Invoice) int {
				return cmp.Compare(a.SettleIndex, b.SettleIndex)
			})
			return settled, nil
		}
		for _, invoice := range resp.Invoices {
			if invoice.State == lnrpc.Invoice_SETTLED {
				settled = append(settled, invoice)
			}
		}
		offset = resp.LastIndexOffset
	}
}

// SubscribeInvoices streams invoices settled after the settle index after to onSettled,
// until the stream ends or onSettled returns an error. resumeFrom is a settle
// index to resubscribe from that loses no invoice not yet passed to onSettled.
func (c *Client) SubscribeInvoices(ctx context.Context, after uint64, onSettled func(invoice lightning.Settled, resumeFrom uint64) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.lightning.SubscribeInvoices(ctx, &lnrpc.InvoiceSubscription{
		SettleIndex: after,
	})
	if err != nil {
		return fmt.Errorf("subscribe invoices: %w", err)
	}

	// LND replays nothing for a settle index of 0, so list every settled
	// invoice. The list is not a snapshot: an invoice settled while it is read
	// may be missing from it while a later one is present. Listed invoices
	// therefore resume from 0. The stream, opened first, delivers the rest in
	// settle order, so each of its invoices is a safe point to resume from.
	listed := map[uint64]bool{}
	if after == 0 {
		invoices, err := c.settledInvoices(ctx)
		if err != nil {
			return err
		}
		for _, invoice := range invoices {
			listed[invoice.SettleIndex] = true
			if err := onSettled(toSettled(invoice), 0); err != nil {
				return err
			}
		}
	}

	for {
		invoice, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv invoice: %w", err)
		}
		if invoice.State != lnrpc.Invoice_SETTLED || listed[invoice.SettleIndex] {
			continue
		}
		if err := onSettled(toSettled(invoice), invoice.SettleIndex); err != nil {
			return err
		}
	}
}

func toSettled(invoice *lnrpc.Invoice) lightning.Settled {
	return lightning.Settled{
		AmountMsat:     invoice.AmtPaidMsat,
		Memo:           invoice.Memo,
		PaymentHash:    invoice.RHash,
		PaymentRequest: invoice.PaymentRequest,
		Preimage:       invoice.RPreimage,
		SettledAt:      time.Unix(invoice.SettleDate, 0),
	}
}

// macaroonCredential implements credentials.PerRPCCredentials.
type macaroonCredential struct {
	mac *macaroon.Macaroon
}

func (m *macaroonCredential) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	macBytes, err := m.mac.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"macaroon": hex.EncodeToString(macBytes),
	}, nil
}

func (m *macaroonCredential) RequireTransportSecurity() bool {
	return true
}
