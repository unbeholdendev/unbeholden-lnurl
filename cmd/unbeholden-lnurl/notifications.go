package main

import (
	"encoding/hex"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/notifier"
	"unbeholden-lnurl/internal/payments"
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

func (n *notifications) NotifyInvoiceCreated(amountMsat int64, hash []byte, address, comment string, payer lnurl.PayerData) {
	paymentHash := hex.EncodeToString(hash)
	n.enqueue(webhook.Event{
		ID: "created_" + paymentHash, Type: "invoice.created", OccurredAt: time.Now().UTC(),
		Data: map[string]any{
			"amount_msat": amountMsat, "payment_hash": paymentHash, "address": address, "comment": comment,
			"payer_name": payer.Name, "payer_identifier": payer.Identifier,
		},
	})
	if n.ntfy != nil {
		n.ntfy.NotifyInvoiceCreated(amountMsat, hash, address, lnurl.Memo(comment, payer))
	}
}

func (n *notifications) NotifyPayment(client string, payment payments.Payment) {
	kind := "payment.sent"
	if payment.Status == lightning.PaymentFailed {
		kind = "payment.failed"
	}
	data := map[string]any{
		"amount_msat": payment.AmountMsat, "fee_msat": payment.FeeMsat,
		"payment_hash": payment.PaymentHash, "destination": payment.Destination,
	}
	if client == "" {
		data["voucher_id"] = payment.ID
	} else {
		data["client"], data["id"] = client, payment.ID
	}
	n.enqueue(webhook.Event{
		ID: kind + "/" + client + "/" + payment.ID + "/" + strconv.FormatInt(payment.CreatedAt.UnixNano(), 10), Type: kind, OccurredAt: *payment.CompletedAt,
		Data: data,
	})
	if n.ntfy != nil && client == "" && payment.Status == lightning.PaymentSucceeded {
		n.ntfy.NotifyVoucher(payment.ID, payment.AmountMsat)
	}
}

func paymentEvent(invoice lightning.Settled) webhook.Event {
	hash := hex.EncodeToString(invoice.PaymentHash)
	return webhook.Event{
		ID: "settled_" + hash, Type: "payment.received", OccurredAt: invoice.SettledAt.UTC(),
		Data: map[string]any{"amount_msat": invoice.AmountMsat, "payment_hash": hash},
	}
}
