package lnurl

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

// Notifier is told about voucher payout outcomes and created invoices.
type Notifier interface {
	NotifyVoucher(event, voucherID string, amountMsat int64)
	NotifyInvoiceCreated(amountMsat int64, hash []byte, address, comment string)
}

type status struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, reason string) {
	writeJSON(w, status{Status: "ERROR", Reason: reason})
}

// LNURLEncode bech32-encodes a URL with the "lnurl" human-readable part.
func LNURLEncode(url string) (string, error) {
	s, err := bech32.EncodeFromBase256("lnurl", []byte(url))
	if err != nil {
		return "", err
	}
	return strings.ToUpper(s), nil
}
