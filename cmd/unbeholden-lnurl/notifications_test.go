package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
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
