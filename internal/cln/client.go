// Package cln talks to Core Lightning through its REST interface, CLNRest.
package cln

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/lightning"
)

const labelPrefix = "unbeholden-lnurl "

// waitTimeout is under the 60s idle timeout common to reverse proxies.
const waitTimeout = 50

const errWaitTimeout = 904

// Client calls Core Lightning RPC methods over CLNRest.
type Client struct {
	url  string
	rune string
	http *http.Client
}

// rpcError is an error returned by a Core Lightning RPC method.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

// Connect returns a client for the CLNRest server at url, authorised by the
// rune in runePath. With caCertPath, the server's certificate must be issued
// by that CA, whatever host name it carries; without, the system roots apply.
func Connect(url, runePath, caCertPath string) (*Client, error) {
	r, err := os.ReadFile(runePath)
	if err != nil {
		return nil, fmt.Errorf("read rune: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read ca cert: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificate in %s", caCertPath)
		}
		transport.TLSClientConfig = &tls.Config{
			// Core Lightning names its certificate "cln" and "localhost", not
			// the host it is reached at, so VerifyConnection checks the chain
			// alone.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
				for _, cert := range cs.PeerCertificates[1:] {
					opts.Intermediates.AddCert(cert)
				}
				_, err := cs.PeerCertificates[0].Verify(opts)
				return err
			},
		}
	}
	return &Client{
		url:  strings.TrimSuffix(url, "/"),
		rune: strings.TrimSpace(string(r)),
		http: &http.Client{Transport: transport},
	}, nil
}

// Close releases idle connections.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Rune", c.rune)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var rpcErr rpcError
		if json.NewDecoder(resp.Body).Decode(&rpcErr) != nil || rpcErr.Message == "" {
			return fmt.Errorf("%s: %s", method, resp.Status)
		}
		return fmt.Errorf("%s: %w", method, &rpcErr)
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	return nil
}

// AddInvoice creates an invoice for amountMsat committing to the hash of
// description. Core Lightning keeps no memo, so memo goes in the label.
func (c *Client) AddInvoice(ctx context.Context, amountMsat int64, description, memo string) (lightning.Invoice, error) {
	var resp struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
	}
	err := c.call(ctx, "invoice", map[string]any{
		"amount_msat":  amountMsat,
		"label":        labelPrefix + rand.Text() + " " + memo,
		"description":  description,
		"deschashonly": true,
	}, &resp)
	if err != nil {
		return lightning.Invoice{}, err
	}
	hash, err := hex.DecodeString(resp.PaymentHash)
	if err != nil {
		return lightning.Invoice{}, fmt.Errorf("invoice: payment hash: %w", err)
	}
	return lightning.Invoice{PaymentRequest: resp.Bolt11, PaymentHash: hash}, nil
}

// LookupInvoice returns the invoice with paymentHash, or
// lightning.ErrInvoiceNotFound.
func (c *Client) LookupInvoice(ctx context.Context, paymentHash []byte) (lightning.Invoice, error) {
	var resp struct {
		Invoices []struct {
			Bolt11          string `json:"bolt11"`
			Status          string `json:"status"`
			PaymentPreimage string `json:"payment_preimage"`
		} `json:"invoices"`
	}
	if err := c.call(ctx, "listinvoices", map[string]any{"payment_hash": hex.EncodeToString(paymentHash)}, &resp); err != nil {
		return lightning.Invoice{}, err
	}
	if len(resp.Invoices) == 0 {
		return lightning.Invoice{}, lightning.ErrInvoiceNotFound
	}
	found := resp.Invoices[0]
	invoice := lightning.Invoice{PaymentRequest: found.Bolt11, PaymentHash: paymentHash, Settled: found.Status == "paid", Expired: found.Status == "expired"}
	if invoice.Settled {
		preimage, err := hex.DecodeString(found.PaymentPreimage)
		if err != nil {
			return lightning.Invoice{}, fmt.Errorf("listinvoices: payment preimage: %w", err)
		}
		invoice.Preimage = preimage
	}
	return invoice, nil
}

// SendPayment pays a BOLT-11 invoice, spending at most maxFeeMsat in fees.
// An error wrapping lightning.ErrPaymentFailed means the payment definitely
// failed; any other error leaves the outcome unknown. Cancelling ctx stops
// waiting but does not stop the payment.
func (c *Client) SendPayment(ctx context.Context, bolt11 string, maxFeeMsat int64) (lightning.Payment, error) {
	var paid pay
	err := c.call(ctx, "pay", map[string]any{
		"bolt11":    bolt11,
		"maxfee":    maxFeeMsat,
		"retry_for": 60,
	}, &paid)
	var rpcErr *rpcError
	switch {
	case err == nil && paid.Status == "complete":
		return paid.payment()
	case err == nil:
		return lightning.Payment{}, fmt.Errorf("pay: status %s", paid.Status)
	case !errors.As(err, &rpcErr):
		return lightning.Payment{}, err
	}

	// pay answered with an error, so it no longer runs, but parts it sent may
	// still be in flight or have settled.
	payment, lerr := c.listPays(ctx, map[string]any{"bolt11": bolt11})
	switch {
	case errors.Is(lerr, lightning.ErrPaymentNotFound) || lerr == nil && payment.Status == lightning.PaymentFailed:
		return lightning.Payment{}, fmt.Errorf("%w: %w", lightning.ErrPaymentFailed, err)
	case lerr != nil:
		return lightning.Payment{}, fmt.Errorf("%w; %w", err, lerr)
	case payment.Status == lightning.PaymentPending:
		return lightning.Payment{}, fmt.Errorf("%w; payment still pending", err)
	}
	return payment, nil
}

// LookupPayment returns the outgoing payment for paymentHash, or
// lightning.ErrPaymentNotFound.
func (c *Client) LookupPayment(ctx context.Context, paymentHash []byte) (lightning.Payment, error) {
	return c.listPays(ctx, map[string]any{"payment_hash": hex.EncodeToString(paymentHash)})
}

// pay is a payment as pay and listpays report it.
type pay struct {
	Status          string `json:"status"`
	PaymentPreimage string `json:"payment_preimage"`
	Preimage        string `json:"preimage"`
	AmountMsat      int64  `json:"amount_msat"`
	AmountSentMsat  int64  `json:"amount_sent_msat"`
}

func (p pay) payment() (lightning.Payment, error) {
	preimage, err := hex.DecodeString(p.PaymentPreimage + p.Preimage)
	if err != nil || len(preimage) != 32 {
		return lightning.Payment{}, fmt.Errorf("pay: invalid payment preimage")
	}
	return lightning.Payment{Status: lightning.PaymentSucceeded, Preimage: preimage, FeeMsat: p.AmountSentMsat - p.AmountMsat}, nil
}

// listPays returns the outcome of the attempts listpays reports for filter.
func (c *Client) listPays(ctx context.Context, filter map[string]any) (lightning.Payment, error) {
	var resp struct {
		Pays []pay `json:"pays"`
	}
	if err := c.call(ctx, "listpays", filter, &resp); err != nil {
		return lightning.Payment{}, err
	}
	if len(resp.Pays) == 0 {
		return lightning.Payment{}, lightning.ErrPaymentNotFound
	}
	pending := false
	for _, p := range resp.Pays {
		switch p.Status {
		case "complete":
			return p.payment()
		case "pending":
			pending = true
		}
	}
	if pending {
		return lightning.Payment{Status: lightning.PaymentPending}, nil
	}
	return lightning.Payment{Status: lightning.PaymentFailed}, nil
}

// Network returns the parameters of the chain Core Lightning runs on.
func (c *Client) Network(ctx context.Context) (*chaincfg.Params, error) {
	var info struct {
		Network string `json:"network"`
	}
	if err := c.call(ctx, "getinfo", struct{}{}, &info); err != nil {
		return nil, err
	}
	return lightning.ChainParams(info.Network)
}

type invoice struct {
	Label              string `json:"label"`
	Description        string `json:"description"`
	PayIndex           uint64 `json:"pay_index"`
	AmountReceivedMsat int64  `json:"amount_received_msat"`
	PaymentHash        string `json:"payment_hash"`
	Bolt11             string `json:"bolt11"`
	PaymentPreimage    string `json:"payment_preimage"`
	PaidAt             int64  `json:"paid_at"`
}

// memo returns the memo AddInvoice put in the label, or the description of
// an invoice made elsewhere.
func (inv invoice) memo() string {
	rest, ok := strings.CutPrefix(inv.Label, labelPrefix)
	if !ok {
		return inv.Description
	}
	_, memo, _ := strings.Cut(rest, " ")
	return memo
}

func (inv invoice) settled() (lightning.Settled, error) {
	hash, err := hex.DecodeString(inv.PaymentHash)
	if err != nil {
		return lightning.Settled{}, fmt.Errorf("payment hash: %w", err)
	}
	preimage, err := hex.DecodeString(inv.PaymentPreimage)
	if err != nil {
		return lightning.Settled{}, fmt.Errorf("payment preimage: %w", err)
	}
	return lightning.Settled{
		AmountMsat:     inv.AmountReceivedMsat,
		Memo:           inv.memo(),
		PaymentHash:    hash,
		PaymentRequest: inv.Bolt11,
		Preimage:       preimage,
		SettledAt:      time.Unix(inv.PaidAt, 0),
	}, nil
}

// LastPaidIndex returns the highest pay index on the node, or 0 if no
// invoice has been paid.
func (c *Client) LastPaidIndex(ctx context.Context) (uint64, error) {
	var resp struct {
		Invoices []invoice `json:"invoices"`
	}
	if err := c.call(ctx, "listinvoices", struct{}{}, &resp); err != nil {
		return 0, err
	}
	var last uint64
	for _, inv := range resp.Invoices {
		last = max(last, inv.PayIndex)
	}
	return last, nil
}

// SubscribeInvoices passes invoices paid after the pay index after to
// onSettled in pay order, until a call fails or onSettled returns an error.
// resumeFrom is the invoice's pay index.
func (c *Client) SubscribeInvoices(ctx context.Context, after uint64, onSettled func(invoice lightning.Settled, resumeFrom uint64) error) error {
	for {
		var inv invoice
		err := c.call(ctx, "waitanyinvoice", map[string]any{"lastpay_index": after, "timeout": waitTimeout}, &inv)
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && rpcErr.Code == errWaitTimeout {
			continue
		}
		if err != nil {
			return err
		}
		if inv.PayIndex <= after {
			return fmt.Errorf("waitanyinvoice: pay index %d not after %d", inv.PayIndex, after)
		}
		settled, err := inv.settled()
		if err != nil {
			return fmt.Errorf("waitanyinvoice: %w", err)
		}
		if err := onSettled(settled, inv.PayIndex); err != nil {
			return err
		}
		after = inv.PayIndex
	}
}
