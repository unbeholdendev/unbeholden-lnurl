// Package payments lets configured clients pay invoices and Lightning
// Addresses from the node.
package payments

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/atomicfile"
	"unbeholden-lnurl/internal/bolt11"
	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/lnurl"
)

const (
	budgetWindow = 24 * time.Hour
	retention    = 90 * 24 * time.Hour
	maxBodyLen   = 64 << 10
)

var paymentID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// Node pays invoices and reports on payments.
type Node interface {
	// SendPayment pays bolt11, spending at most maxFeeMsat in fees. An error
	// wrapping lightning.ErrPaymentFailed means the payment definitely failed;
	// any other error leaves it unknown.
	SendPayment(ctx context.Context, bolt11 string, maxFeeMsat int64) (lightning.Payment, error)
	// LookupPayment returns lightning.ErrPaymentNotFound for an unknown hash.
	LookupPayment(ctx context.Context, paymentHash []byte) (lightning.Payment, error)
}

// Resolver fetches invoices from Lightning Addresses.
type Resolver interface {
	Invoice(ctx context.Context, address string, amountMsat int64, comment string) (string, error)
}

// Notifier is told when a client's payment succeeds or fails.
type Notifier interface {
	NotifyPayment(client string, payment Payment)
}

// Payment is a client's payment as the API reports it.
type Payment struct {
	ID          string                  `json:"id"`
	Status      lightning.PaymentStatus `json:"status"`
	AmountMsat  int64                   `json:"amount_msat"`
	FeeMsat     int64                   `json:"fee_msat"`
	PaymentHash string                  `json:"payment_hash"`
	Preimage    string                  `json:"preimage,omitempty"`
	Destination string                  `json:"destination"`
	CreatedAt   time.Time               `json:"created_at"`
	CompletedAt *time.Time              `json:"completed_at,omitempty"`
}

type record struct {
	Payment
	Client string `json:"client"`
	// Request identifies the request body, so a retry can be told from
	// another payment reusing the ID.
	Request    string `json:"request"`
	MaxFeeMsat int64  `json:"max_fee_msat"`
}

// spent is what the record counts against the daily budget.
func (r *record) spent() int64 {
	switch r.Status {
	case lightning.PaymentSucceeded:
		return r.AmountMsat + r.FeeMsat
	case lightning.PaymentPending:
		return r.AmountMsat + r.MaxFeeMsat
	}
	return 0
}

type key struct{ client, id string }

type client struct {
	name   string
	token  [32]byte
	limits config.PaymentClient
}

// Handler serves the payments API.
type Handler struct {
	clients  []client
	node     Node
	resolver Resolver
	notify   Notifier
	network  *chaincfg.Params
	path     string

	mu       sync.Mutex
	records  map[key]*record
	inFlight map[key]bool
}

// New returns a Handler for clients that keeps payments in path and accepts
// invoices for network. It settles payments left pending by an earlier run
// where the node knows their outcome. A nil notify sends no notifications.
func New(ctx context.Context, clients map[string]config.PaymentClient, node Node, resolver Resolver, notify Notifier, network *chaincfg.Params, path string) (*Handler, error) {
	h := &Handler{
		node:     node,
		resolver: resolver,
		notify:   notify,
		network:  network,
		path:     path,
		records:  make(map[key]*record),
		inFlight: make(map[key]bool),
	}
	for name, c := range clients {
		h.clients = append(h.clients, client{name: name, token: sha256.Sum256([]byte(c.Token)), limits: c})
	}
	if err := h.load(); err != nil {
		return nil, fmt.Errorf("load payments: %w", err)
	}
	for k, r := range h.records {
		if r.Status == lightning.PaymentPending {
			h.refresh(ctx, k)
		}
	}
	return h, nil
}

// Register adds the API's routes to mux. clientIP gives a request's client
// address, which a client's allowed_ips must contain.
func (h *Handler) Register(mux *http.ServeMux, clientIP func(*http.Request) string) {
	auth := func(next func(http.ResponseWriter, *http.Request, client)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, ok := h.authenticate(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			if !c.allows(clientIP(r)) {
				slog.Warn("payment client used from a disallowed address", "client", c.name, "ip", clientIP(r))
				writeError(w, http.StatusForbidden, "address not allowed")
				return
			}
			next(w, r, c)
		}
	}
	mux.HandleFunc("POST /payments", auth(h.handlePay))
	mux.HandleFunc("GET /payments", auth(h.handleList))
	mux.HandleFunc("GET /payments/{id}", auth(h.handleGet))
}

func (h *Handler) authenticate(r *http.Request) (client, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return client{}, false
	}
	sum := sha256.Sum256([]byte(token))
	for _, c := range h.clients {
		if subtle.ConstantTimeCompare(sum[:], c.token[:]) == 1 {
			return c, true
		}
	}
	return client{}, false
}

func (c client) allows(ip string) bool {
	if len(c.limits.AllowedIPs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return slices.ContainsFunc(c.limits.AllowedIPs, func(p config.Prefix) bool { return p.Contains(addr) })
}

type payRequest struct {
	ID         string `json:"id"`
	Invoice    string `json:"invoice,omitempty"`
	Address    string `json:"address,omitempty"`
	AmountMsat int64  `json:"amount_msat,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

// handlePay handles POST /payments.
func (h *Handler) handlePay(w http.ResponseWriter, r *http.Request, c client) {
	var req payRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyLen))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fingerprint, _ := json.Marshal(payRequest{Invoice: req.Invoice, Address: req.Address, AmountMsat: req.AmountMsat, Comment: req.Comment})
	k := key{c.name, req.ID}
	if h.replay(w, r, k, string(fingerprint)) {
		return
	}

	invoice, destination := req.Invoice, req.Invoice
	if req.Address != "" {
		var err error
		invoice, err = h.resolver.Invoice(r.Context(), req.Address, req.AmountMsat, req.Comment)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		destination = req.Address
	}
	decoded, err := bolt11.Decode(invoice, h.network)
	switch {
	case err != nil:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid invoice: %v", err))
		return
	case decoded.AmountMsat == nil:
		writeError(w, http.StatusBadRequest, "invoice has no amount")
		return
	case time.Since(decoded.Timestamp) > decoded.Expiry:
		writeError(w, http.StatusBadRequest, "invoice has expired")
		return
	}
	amount := *decoded.AmountMsat
	if c.limits.MaxPayment > 0 && amount > c.limits.MaxPayment {
		writeError(w, http.StatusForbidden, fmt.Sprintf("amount exceeds max_payment of %d msat", c.limits.MaxPayment))
		return
	}
	maxFee := c.limits.MaxFee
	if maxFee == 0 {
		maxFee = lightning.DefaultMaxFee(amount)
	}

	h.mu.Lock()
	if _, ok := h.records[k]; ok {
		h.mu.Unlock()
		h.replay(w, r, k, string(fingerprint))
		return
	}
	if err := h.duplicateLocked(decoded.PaymentHash[:]); err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := h.budgetLocked(c, amount+maxFee); err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	rec := &record{
		Payment: Payment{
			ID:          req.ID,
			Status:      lightning.PaymentPending,
			AmountMsat:  amount,
			PaymentHash: hex.EncodeToString(decoded.PaymentHash[:]),
			Destination: destination,
			CreatedAt:   time.Now().UTC(),
		},
		Client:     c.name,
		Request:    string(fingerprint),
		MaxFeeMsat: maxFee,
	}
	h.records[k] = rec
	if err := h.saveLocked(); err != nil {
		delete(h.records, k)
		h.mu.Unlock()
		slog.Error("payment not sent: failed to save it", "client", c.name, "id", req.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to record payment")
		return
	}
	h.inFlight[k] = true
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lnurl.PayTimeout)
	defer cancel()
	payment, err := h.node.SendPayment(ctx, invoice, maxFee)
	switch {
	case errors.Is(err, lightning.ErrPaymentFailed):
		slog.Warn("payment failed", "client", c.name, "id", req.ID, "err", err)
		payment = lightning.Payment{Status: lightning.PaymentFailed}
	case err != nil:
		slog.Error("payment outcome unknown, left pending", "client", c.name, "id", req.ID, "err", err)
		payment = lightning.Payment{Status: lightning.PaymentPending}
	}
	h.mu.Lock()
	delete(h.inFlight, k)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, h.complete(k, payment))
}

func (req payRequest) validate() error {
	switch {
	case !paymentID.MatchString(req.ID):
		return errors.New("id must be 1 to 64 letters, digits or ._:-")
	case (req.Invoice == "") == (req.Address == ""):
		return errors.New("set exactly one of invoice or address")
	case req.Invoice != "" && (req.AmountMsat != 0 || req.Comment != ""):
		return errors.New("amount_msat and comment apply only to an address")
	case req.Address != "" && req.AmountMsat <= 0:
		return errors.New("amount_msat must be positive")
	}
	return nil
}

// replay answers a request whose ID k already has a payment, and reports
// whether there was one.
func (h *Handler) replay(w http.ResponseWriter, r *http.Request, k key, fingerprint string) bool {
	h.mu.Lock()
	rec, ok := h.records[k]
	same := ok && rec.Request == fingerprint
	h.mu.Unlock()
	switch {
	case !ok:
		return false
	case !same:
		writeError(w, http.StatusConflict, "id already used for another payment")
	default:
		payment, _ := h.current(r.Context(), k)
		writeJSON(w, http.StatusOK, payment)
	}
	return true
}

// current returns payment k, refreshing it if pending, and whether it exists.
func (h *Handler) current(ctx context.Context, k key) (Payment, bool) {
	h.mu.Lock()
	rec, ok := h.records[k]
	if !ok {
		h.mu.Unlock()
		return Payment{}, false
	}
	payment := rec.Payment
	h.mu.Unlock()
	if payment.Status == lightning.PaymentPending {
		payment = h.refresh(ctx, k)
	}
	return payment, true
}

// duplicateLocked refuses an invoice another payment has paid or is paying.
func (h *Handler) duplicateLocked(paymentHash []byte) error {
	hash := hex.EncodeToString(paymentHash)
	for _, rec := range h.records {
		if rec.PaymentHash == hash && rec.Status != lightning.PaymentFailed {
			return errors.New("invoice already paid or being paid")
		}
	}
	return nil
}

// budgetLocked refuses a payment costing at most cost that could exceed c's
// daily budget. Pending payments count whatever their age, since they can
// still settle.
func (h *Handler) budgetLocked(c client, cost int64) error {
	budget := c.limits.DailyBudget
	if budget == 0 {
		return nil
	}
	since := time.Now().Add(-budgetWindow)
	var spent int64
	for _, rec := range h.records {
		if rec.Client == c.name && (rec.Status == lightning.PaymentPending || rec.CreatedAt.After(since)) {
			spent += rec.spent()
		}
	}
	if spent+cost > budget {
		return fmt.Errorf("payment and maximum fee of %d msat exceed the remaining daily budget of %d msat", cost, max(budget-spent, 0))
	}
	return nil
}

// complete records the node's outcome for payment k and returns the payment.
func (h *Handler) complete(k key, payment lightning.Payment) Payment {
	h.mu.Lock()
	rec := h.records[k]
	if rec.Status != lightning.PaymentPending || payment.Status == lightning.PaymentPending {
		defer h.mu.Unlock()
		return rec.Payment
	}
	now := time.Now().UTC()
	rec.Status = payment.Status
	rec.FeeMsat = payment.FeeMsat
	rec.Preimage = hex.EncodeToString(payment.Preimage)
	rec.CompletedAt = &now
	if err := h.saveLocked(); err != nil {
		slog.Error("save payment outcome", "client", k.client, "id", k.id, "err", err)
	}
	result := rec.Payment
	h.mu.Unlock()
	slog.Info("payment completed", "client", k.client, "id", k.id, "status", result.Status, "amount_msat", result.AmountMsat, "fee_msat", result.FeeMsat)
	if h.notify != nil {
		h.notify.NotifyPayment(k.client, result)
	}
	return result
}

// refresh returns pending payment k, first asking the node for its outcome
// unless this process is still sending it. Only completed payments are
// pruned, so k still exists.
func (h *Handler) refresh(ctx context.Context, k key) Payment {
	h.mu.Lock()
	rec := h.records[k]
	if rec.Status != lightning.PaymentPending || h.inFlight[k] {
		defer h.mu.Unlock()
		return rec.Payment
	}
	hash, _ := hex.DecodeString(rec.PaymentHash)
	created := rec.CreatedAt
	h.mu.Unlock()

	payment, err := h.node.LookupPayment(ctx, hash)
	switch {
	case errors.Is(err, lightning.ErrPaymentNotFound) && time.Since(created) > lnurl.PayTimeout:
		payment = lightning.Payment{Status: lightning.PaymentFailed}
	case err != nil:
		if !errors.Is(err, lightning.ErrPaymentNotFound) {
			slog.Warn("look up pending payment", "client", k.client, "id", k.id, "err", err)
		}
		payment = lightning.Payment{Status: lightning.PaymentPending}
	}
	return h.complete(k, payment)
}

// handleGet handles GET /payments/{id}.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, c client) {
	payment, ok := h.current(r.Context(), key{c.name, r.PathValue("id")})
	if !ok {
		writeError(w, http.StatusNotFound, "no such payment")
		return
	}
	writeJSON(w, http.StatusOK, payment)
}

// handleList handles GET /payments?since=RFC3339.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request, c client) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, s); err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 time")
			return
		}
	}
	h.mu.Lock()
	var keys []key
	for k, rec := range h.records {
		if k.client == c.name && !rec.CreatedAt.Before(since) {
			keys = append(keys, k)
		}
	}
	h.mu.Unlock()
	list := make([]Payment, 0, len(keys))
	for _, k := range keys {
		if payment, ok := h.current(r.Context(), k); ok {
			list = append(list, payment)
		}
	}
	slices.SortFunc(list, func(a, b Payment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"payments": list})
}

type stateFile struct {
	Payments []*record `json:"payments"`
}

func (h *Handler) load() error {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	for _, rec := range state.Payments {
		h.records[key{rec.Client, rec.ID}] = rec
	}
	return nil
}

// saveLocked writes every payment not completed before the retention period.
func (h *Handler) saveLocked() error {
	cutoff := time.Now().Add(-retention)
	state := stateFile{Payments: []*record{}}
	for k, rec := range h.records {
		if rec.CompletedAt != nil && rec.CompletedAt.Before(cutoff) {
			delete(h.records, k)
			continue
		}
		state.Payments = append(state.Payments, rec)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicfile.Write(h.path, data)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}
