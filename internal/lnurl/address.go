package lnurl

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/nostr"
)

const maxCommentLen = 140

// Invoicer creates and looks up invoices.
type Invoicer interface {
	// AddInvoice creates an invoice for amountMsat that commits to the
	// hash of description, with memo kept on the node.
	AddInvoice(ctx context.Context, amountMsat int64, description, memo string) (lightning.Invoice, error)
	// LookupInvoice returns lightning.ErrInvoiceNotFound for an unknown hash.
	LookupInvoice(ctx context.Context, paymentHash []byte) (lightning.Invoice, error)
}

// Zapper checks zap requests (NIP-57) and keeps them until paid.
type Zapper interface {
	PubKey() string
	Request(raw string, amountMsat int64) (nostr.Event, error)
	Track(paymentHash []byte, raw string) error
}

// AddressHandler handles Lightning Address (LUD-16) requests.
type AddressHandler struct {
	addresses map[string]config.AddressEntry
	invoicer  Invoicer
	ntfy      Notifier
	verifyKey []byte
	zapper    Zapper
}

type payRequest struct {
	Tag            string `json:"tag"`
	Callback       string `json:"callback"`
	Metadata       string `json:"metadata"`
	MinSendable    int64  `json:"minSendable"`
	MaxSendable    int64  `json:"maxSendable"`
	CommentAllowed int    `json:"commentAllowed"`
	AllowsNostr    bool   `json:"allowsNostr,omitempty"`
	NostrPubkey    string `json:"nostrPubkey,omitempty"`
}

type invoiceResponse struct {
	PR            string         `json:"pr"`
	Routes        []struct{}     `json:"routes"`
	Disposable    bool           `json:"disposable"`
	SuccessAction *successAction `json:"successAction,omitempty"`
	Verify        string         `json:"verify"`
}

type successAction struct {
	Tag     string `json:"tag"`
	Message string `json:"message"`
}

type verifyResponse struct {
	Status   string  `json:"status"`
	Settled  bool    `json:"settled"`
	Preimage *string `json:"preimage"`
	PR       string  `json:"pr"`
}

// NewAddressHandler creates an AddressHandler. The "*" entry, if present,
// serves every username without an entry of its own. A nil ntfy sends no
// notifications. verifyKey signs verify URLs, so changing it breaks those
// already issued. A nil zapper turns zaps off.
func NewAddressHandler(addresses map[string]config.AddressEntry, invoicer Invoicer, ntfy Notifier, verifyKey []byte, zapper Zapper) *AddressHandler {
	return &AddressHandler{addresses: addresses, invoicer: invoicer, ntfy: ntfy, verifyKey: verifyKey, zapper: zapper}
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

	resp := payRequest{
		Tag:            "payRequest",
		Callback:       fmt.Sprintf("%s/.well-known/lnurlp/%s/invoice", hostURL(r.Host), username),
		Metadata:       addressMetadata(addr, username, r.Host),
		MinSendable:    addr.MinSendable,
		MaxSendable:    addr.MaxSendable,
		CommentAllowed: maxCommentLen,
	}
	if h.zapper != nil {
		resp.AllowsNostr = true
		resp.NostrPubkey = h.zapper.PubKey()
	}
	writeJSON(w, resp)
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

	// A zap's invoice commits to the zap request instead of the metadata.
	description := addressMetadata(addr, username, r.Host)
	zapRequest := r.URL.Query().Get("nostr")
	if h.zapper == nil {
		zapRequest = ""
	}
	if zapRequest != "" {
		request, err := h.zapper.Request(zapRequest, amountMsat)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		description = zapRequest
		comment = truncate(request.Content, maxCommentLen)
	}

	invoice, err := h.invoicer.AddInvoice(r.Context(), amountMsat, description, comment)
	if err != nil {
		slog.Error("add invoice failed", "username", username, "err", err)
		writeError(w, "failed to create invoice")
		return
	}
	if zapRequest != "" {
		if err := h.zapper.Track(invoice.PaymentHash, zapRequest); err != nil {
			slog.Error("store zap request failed", "username", username, "err", err)
			writeError(w, "failed to create invoice")
			return
		}
	}

	address := fmt.Sprintf("%s@%s", username, r.Host)
	if h.ntfy != nil {
		h.ntfy.NotifyInvoiceCreated(amountMsat, invoice.PaymentHash, address, comment)
	}
	slog.Info("invoice created", "sats", amountMsat/1000, "address", address)

	resp := invoiceResponse{
		PR:     invoice.PaymentRequest,
		Routes: []struct{}{},
		Verify: fmt.Sprintf("%s/.well-known/lnurlp/%s/verify/%x", hostURL(r.Host), username, h.verifyToken(username, invoice.PaymentHash)),
	}
	if addr.SuccessMessage != "" {
		resp.SuccessAction = &successAction{Tag: "message", Message: addr.SuccessMessage}
	}
	writeJSON(w, resp)
}

// HandleVerify handles GET /.well-known/lnurlp/{username}/verify/{token}
// (LUD-21). The token is signed, so it reveals no invoice the handler did not
// issue for that username.
func (h *AddressHandler) HandleVerify(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if _, ok := h.resolveAddress(username); !ok {
		http.NotFound(w, r)
		return
	}
	token, err := hex.DecodeString(r.PathValue("token"))
	if err != nil || len(token) < sha256.Size || !hmac.Equal(token, h.verifyToken(username, token[:sha256.Size])) {
		writeError(w, "Not found")
		return
	}
	invoice, err := h.invoicer.LookupInvoice(r.Context(), token[:sha256.Size])
	if errors.Is(err, lightning.ErrInvoiceNotFound) {
		writeError(w, "Not found")
		return
	}
	if err != nil {
		slog.Error("lookup invoice failed", "username", username, "err", err)
		writeError(w, "failed to look up invoice")
		return
	}
	resp := verifyResponse{Status: "OK", Settled: invoice.Settled, PR: invoice.PaymentRequest}
	if invoice.Settled {
		preimage := hex.EncodeToString(invoice.Preimage)
		resp.Preimage = &preimage
	}
	writeJSON(w, resp)
}

// verifyToken is paymentHash followed by a MAC binding it to username.
func (h *AddressHandler) verifyToken(username string, paymentHash []byte) []byte {
	mac := hmac.New(sha256.New, h.verifyKey)
	mac.Write(paymentHash)
	mac.Write([]byte(username))
	return mac.Sum(append([]byte{}, paymentHash...))[:sha256.Size+16]
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func addressMetadata(addr config.AddressEntry, username, host string) string {
	entries := [][]string{
		{"text/plain", addr.Description},
		{"text/identifier", fmt.Sprintf("%s@%s", username, host)},
	}
	if addr.LongDescription != "" {
		entries = append(entries, []string{"text/long-desc", addr.LongDescription})
	}
	if addr.Image != "" {
		entries = append(entries, []string{addr.ImageType, addr.Image})
	}
	data, _ := json.Marshal(entries)
	return string(data)
}

func hostURL(host string) string {
	u := url.URL{Scheme: "https", Host: host}
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), ".onion") {
		u.Scheme = "http"
	}
	return u.String()
}
