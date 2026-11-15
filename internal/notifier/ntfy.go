package notifier

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"unbeholden-lnurl/internal/config"
)

// Ntfy sends push notifications via an ntfy server.
type Ntfy struct {
	url    string
	topic  string
	token  string
	client *http.Client
}

// NewNtfy creates an Ntfy notifier.
func NewNtfy(cfg config.NtfyConfig) *Ntfy {
	return &Ntfy{
		url:   cfg.URL,
		topic: cfg.Topic,
		token: cfg.Token,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NotifyInvoiceCreated sends a notification when an invoice is created.
func (n *Ntfy) NotifyInvoiceCreated(amountSats int64, address string, comment string) {
	body := fmt.Sprintf("Invoice created: %d sats for %s", amountSats, address)
	if comment != "" {
		body = fmt.Sprintf("Invoice created: %d sats for %s: %s", amountSats, address, comment)
	}
	n.sendOrLog("Invoice created", body)
}

// NotifyPayment sends a notification for a settled payment.
func (n *Ntfy) NotifyPayment(amountSats int64, memo string) error {
	body := fmt.Sprintf("Received %d sats", amountSats)
	if memo != "" {
		body = fmt.Sprintf("Received %d sats: %s", amountSats, memo)
	}
	return n.send("Payment received", body)
}

// NotifyVoucherClaimed sends a notification for a claimed withdraw voucher.
func (n *Ntfy) NotifyVoucherClaimed(voucherID string, amountSats int64) {
	body := fmt.Sprintf("Voucher %s claimed (%d sats)", voucherID, amountSats)
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
