package lnurl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"unbeholden-lnurl/internal/config"
)

const maxCommentLen = 140

// Invoicer creates invoices.
type Invoicer interface {
	AddInvoice(ctx context.Context, amountMsat int64, descriptionHash []byte, memo string) (string, error)
}

// AddressHandler handles Lightning Address (LUD-16) requests.
type AddressHandler struct {
	addresses map[string]config.AddressEntry
	invoicer  Invoicer
	ntfy      Notifier
}

type payRequest struct {
	Tag            string `json:"tag"`
	Callback       string `json:"callback"`
	Metadata       string `json:"metadata"`
	MinSendable    int64  `json:"minSendable"`
	MaxSendable    int64  `json:"maxSendable"`
	CommentAllowed int    `json:"commentAllowed"`
}

type invoiceResponse struct {
	PR     string     `json:"pr"`
	Routes []struct{} `json:"routes"`
}

// NewAddressHandler creates an AddressHandler. The "*" entry, if present,
// serves every username without an entry of its own. A nil ntfy sends no
// notifications.
func NewAddressHandler(addresses map[string]config.AddressEntry, invoicer Invoicer, ntfy Notifier) *AddressHandler {
	return &AddressHandler{addresses: addresses, invoicer: invoicer, ntfy: ntfy}
}

func (h *AddressHandler) resolveAddress(username string) (config.AddressEntry, bool) {
	if addr, ok := h.addresses[username]; ok {
		return addr, true
	}
	addr, ok := h.addresses["*"]
	return addr, ok
}

// HandleWellKnown handles GET /.well-known/lnurlp/{username}.
func (h *AddressHandler) HandleWellKnown(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	addr, ok := h.resolveAddress(username)
	if !ok {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, payRequest{
		Tag:            "payRequest",
		Callback:       fmt.Sprintf("https://%s/.well-known/lnurlp/%s/invoice", r.Host, username),
		Metadata:       addressMetadata(addr.Description, username, r.Host),
		MinSendable:    addr.MinSendable,
		MaxSendable:    addr.MaxSendable,
		CommentAllowed: maxCommentLen,
	})
}

// HandleInvoice handles GET /.well-known/lnurlp/{username}/invoice?amount=X.
func (h *AddressHandler) HandleInvoice(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	addr, ok := h.resolveAddress(username)
	if !ok {
		http.NotFound(w, r)
		return
	}

	amountStr := r.URL.Query().Get("amount")
	if amountStr == "" {
		writeError(w, "missing amount parameter")
		return
	}
	amountMsat, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil {
		writeError(w, "invalid amount")
		return
	}
	if amountMsat < addr.MinSendable || amountMsat > addr.MaxSendable {
		writeError(w, fmt.Sprintf("amount must be between %d and %d msats", addr.MinSendable, addr.MaxSendable))
		return
	}

	comment := r.URL.Query().Get("comment")
	if len(comment) > maxCommentLen {
		writeError(w, fmt.Sprintf("comment too long (max %d characters)", maxCommentLen))
		return
	}

	descHash := sha256.Sum256([]byte(addressMetadata(addr.Description, username, r.Host)))
	bolt11, err := h.invoicer.AddInvoice(r.Context(), amountMsat, descHash[:], comment)
	if err != nil {
		slog.Error("add invoice failed", "username", username, "err", err)
		writeError(w, "failed to create invoice")
		return
	}

	address := fmt.Sprintf("%s@%s", username, r.Host)
	if h.ntfy != nil {
		h.ntfy.NotifyInvoiceCreated(amountMsat/1000, address, comment)
	}
	slog.Info("invoice created", "sats", amountMsat/1000, "address", address)

	writeJSON(w, invoiceResponse{PR: bolt11, Routes: []struct{}{}})
}

func addressMetadata(description, username, host string) string {
	data, _ := json.Marshal([][]string{
		{"text/plain", description},
		{"text/identifier", fmt.Sprintf("%s@%s", username, host)},
	})
	return string(data)
}
