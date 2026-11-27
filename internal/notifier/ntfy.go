package notifier

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
)

// Ntfy sends push notifications via an ntfy server.
type Ntfy struct {
	url    string
	topic  string
	token  string
	client *http.Client
}

// NewNtfy creates an Ntfy notifier.
func NewNtfy(cfg config.NtfyConfig, proxy ...*url.URL) *Ntfy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(proxy) > 0 && proxy[0] != nil {
		transport.Proxy = http.ProxyURL(proxy[0])
	}
	return &Ntfy{
		url:   cfg.URL,
		topic: cfg.Topic,
		token: cfg.Token,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}
}

// NotifyInvoiceCreated sends a notification when an invoice is created.
func (n *Ntfy) NotifyInvoiceCreated(amountMsat int64, _ []byte, address, comment string) {
	amountSats := amountMsat / 1000
	body := fmt.Sprintf("Invoice created: %d sats for %s", amountSats, address)
	if comment != "" {
		body = fmt.Sprintf("Invoice created: %d sats for %s: %s", amountSats, address, comment)
	}
	n.sendOrLog("Invoice created", body)
}

// NotifyPayment sends a notification for a settled payment.
func (n *Ntfy) NotifyPayment(invoice lightning.Settled) error {
	amountSats := invoice.AmountMsat / 1000
	body := fmt.Sprintf("Received %d sats", amountSats)
	if invoice.Memo != "" {
		body = fmt.Sprintf("Received %d sats: %s", amountSats, invoice.Memo)
	}
	return n.send("Payment received", body)
}

// NotifyVoucher sends a notification for a claimed withdraw voucher.
func (n *Ntfy) NotifyVoucher(voucherID string, amountMsat int64) {
	body := fmt.Sprintf("Voucher %s claimed (%d sats)", voucherID, amountMsat/1000)
	n.sendOrLog("Voucher claimed", body)
}

func (n *Ntfy) send(title, body string) error {
	url := fmt.Sprintf("%s/%s", n.url, n.topic)
	req, err := http.NewRequest("POST", url, bytes.NewBufferString(body))
	if err != nil {
		return fmt.Errorf("ntfy: create request: %w", err)
	}
	req.Header.Set("Title", title)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func (n *Ntfy) sendOrLog(title, body string) {
	if err := n.send(title, body); err != nil {
		slog.Warn("notification failed", "title", title, "err", err)
	}
}
