package notifier

import (
	"slices"
	"testing"
)

func TestNtfyInvoiceAndVoucherEvents(t *testing.T) {
	n, delivered := ntfyServer(t, func(string) bool { return false })
	n.NotifyInvoiceCreated(1500, nil, "bob@example.com", "hi")
	n.NotifyVoucher("tip", 2500)
	want := []string{"Invoice created: 1 sats for bob@example.com: hi", "Voucher tip claimed (2 sats)"}
	if !slices.Equal(*delivered, want) {
		t.Fatalf("delivered = %q, want %q", *delivered, want)
	}
}
