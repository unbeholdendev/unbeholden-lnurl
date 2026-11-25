package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"slices"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/notifier"
	"unbeholden-lnurl/internal/webhook"
)

type notifications struct {
	ntfy      *notifier.Ntfy
	queue     *webhook.Queue
	endpoints map[string]config.WebhookConfig
}

func (n *notifications) enqueue(event webhook.Event) {
	for name, endpoint := range n.endpoints {
		if slices.Contains(endpoint.Events, event.Type) {
			if err := n.queue.Enqueue(name, event); err != nil {
				slog.Error("queue webhook", "endpoint", name, "event", event.ID, "err", err)
			}
		}
	}
}

func (n *notifications) NotifyInvoiceCreated(amountMsat int64, hash []byte, address, comment string) {
	paymentHash := hex.EncodeToString(hash)
	n.enqueue(webhook.Event{
		ID: "created_" + paymentHash, Type: "invoice.created", OccurredAt: time.Now().UTC(),
		Data: map[string]any{"amount_msat": amountMsat, "payment_hash": paymentHash, "address": address, "comment": comment},
	})
	if n.ntfy != nil {
		n.ntfy.NotifyInvoiceCreated(amountMsat, hash, address, comment)
	}
}

func (n *notifications) NotifyVoucher(kind, voucherID string, amountMsat int64) {
	n.enqueue(webhook.Event{
		ID: rand.Text(), Type: kind, OccurredAt: time.Now().UTC(),
		Data: map[string]any{"amount_msat": amountMsat, "voucher_id": voucherID},
	})
	if n.ntfy != nil {
		n.ntfy.NotifyVoucher(kind, voucherID, amountMsat)
	}
}

func paymentEvent(invoice lightning.Settled) webhook.Event {
	hash := hex.EncodeToString(invoice.PaymentHash)
	return webhook.Event{
		ID: "settled_" + hash, Type: "payment.received", OccurredAt: invoice.SettledAt.UTC(),
		Data: map[string]any{"amount_msat": invoice.AmountMsat, "payment_hash": hash},
	}
}
