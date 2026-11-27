package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/payments"
	"unbeholden-lnurl/internal/webhook"
)

func TestNotifyPayment_DistinctEventIDs(t *testing.T) {
	dir := t.TempDir()
	endpoints := map[string]config.WebhookConfig{"ledger": {URL: "https://ledger.example", Events: []string{"payment.sent"}, Key: make([]byte, 32)}}
	queue, err := webhook.Open(dir, endpoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &notifications{queue: queue, endpoints: endpoints}
	now := time.Now()
	n.NotifyPayment("shop", payments.Payment{ID: "office_order", Status: lightning.PaymentSucceeded, CompletedAt: &now})
	n.NotifyPayment("shop_office", payments.Payment{ID: "order", Status: lightning.PaymentSucceeded, CompletedAt: &now})
	pending, err := filepath.Glob(filepath.Join(dir, "active-*", "pending", "*.json"))
	if err != nil || len(pending) != 2 {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("queued %d events, want 2 (%v, %v)", len(pending), err, entries)
	}
}

func TestNotifyPayment_VoucherPayout(t *testing.T) {
	dir := t.TempDir()
	endpoints := map[string]config.WebhookConfig{"alerts": {URL: "https://alerts.example", Events: []string{"payment.failed"}, Key: make([]byte, 32)}}
	queue, err := webhook.Open(dir, endpoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &notifications{queue: queue, endpoints: endpoints}
	now := time.Now()
	for _, created := range []time.Time{now.Add(-time.Minute), now} {
		n.NotifyPayment("", payments.Payment{ID: "tip-001", Status: lightning.PaymentFailed, AmountMsat: 1000, PaymentHash: "aa", CreatedAt: created, CompletedAt: &now})
	}
	pending, err := filepath.Glob(filepath.Join(dir, "active-*", "pending", "*.json"))
	if err != nil || len(pending) != 2 {
		t.Fatalf("queued %d events, want one per attempt (%v)", len(pending), err)
	}
	var item struct{ Body json.RawMessage }
	data, _ := os.ReadFile(pending[0])
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type string
		Data map[string]any
	}
	if err := json.Unmarshal(item.Body, &event); err != nil {
		t.Fatal(err)
	}
	if _, hasClient := event.Data["client"]; event.Type != "payment.failed" || event.Data["voucher_id"] != "tip-001" || hasClient {
		t.Fatalf("event = %+v", event)
	}
}

func TestNotifyInvoiceCreated_Payer(t *testing.T) {
	dir := t.TempDir()
	endpoints := map[string]config.WebhookConfig{"shop": {URL: "https://shop.example", Events: []string{"invoice.created"}, Key: make([]byte, 32)}}
	queue, err := webhook.Open(dir, endpoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &notifications{queue: queue, endpoints: endpoints}
	n.NotifyInvoiceCreated(1500, []byte{1}, "bob@example.com", "hi", lnurl.PayerData{Name: "Alice", Identifier: "alice@example.com"})
	pending, err := filepath.Glob(filepath.Join(dir, "active-*", "pending", "*.json"))
	if err != nil || len(pending) != 1 {
		t.Fatalf("queued %d events, want 1 (%v)", len(pending), err)
	}
	raw, err := os.ReadFile(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	var queued struct{ Body webhook.Event }
	if err := json.Unmarshal(raw, &queued); err != nil {
		t.Fatal(err)
	}
	if data := queued.Body.Data; data["comment"] != "hi" || data["payer_name"] != "Alice" || data["payer_identifier"] != "alice@example.com" {
		t.Fatalf("data = %v", data)
	}
}
