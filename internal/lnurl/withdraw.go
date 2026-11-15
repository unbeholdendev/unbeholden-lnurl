package lnurl

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"unbeholden-lnurl/internal/atomicfile"
	"unbeholden-lnurl/internal/bolt11"
	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"

	"github.com/btcsuite/btcd/chaincfg"
)

const k1TTL = 5 * time.Minute

// PayTimeout bounds how long a withdraw claim waits for its payment.
const PayTimeout = 2 * time.Minute

// Payer pays BOLT-11 invoices.
type Payer interface {
	// SendPayment pays bolt11. An error wrapping lightning.ErrPaymentFailed means
	// the payment definitely failed; any other error leaves it unknown.
	SendPayment(ctx context.Context, bolt11 string, maxFeeSat int64) error
}

// WithdrawHandler handles LNURL-withdraw protocol requests.
type WithdrawHandler struct {
	vouchers  []config.Voucher
	byToken   map[string]config.Voucher
	k1Key     []byte
	network   *chaincfg.Params
	payer     Payer
	ntfy      Notifier
	statePath string

	mu      sync.Mutex
	claimed map[string]ClaimedInfo
}

// ClaimedInfo records when a voucher was claimed.
type ClaimedInfo struct {
	ClaimedAt  time.Time `json:"claimed_at"`
	AmountMsat int64     `json:"amount_msat"`
}

type stateFile struct {
	Claimed map[string]ClaimedInfo `json:"claimed"`
}

type withdrawRequest struct {
	Tag                string `json:"tag"`
	Callback           string `json:"callback"`
	K1                 string `json:"k1"`
	DefaultDescription string `json:"defaultDescription"`
	MinWithdrawable    int64  `json:"minWithdrawable"`
	MaxWithdrawable    int64  `json:"maxWithdrawable"`
}

// NewWithdrawHandler creates a WithdrawHandler that signs k1 values with
// k1Key, accepts invoices for network and records claims in statePath. It
// fails if statePath exists but cannot be read. A nil ntfy sends no
// notifications.
func NewWithdrawHandler(vouchers []config.Voucher, k1Key []byte, network *chaincfg.Params, payer Payer, ntfy Notifier, statePath string) (*WithdrawHandler, error) {
	h := &WithdrawHandler{
		vouchers:  vouchers,
		byToken:   make(map[string]config.Voucher, len(vouchers)),
		k1Key:     k1Key,
		network:   network,
		payer:     payer,
		ntfy:      ntfy,
		statePath: statePath,
		claimed:   make(map[string]ClaimedInfo),
	}
	for _, v := range vouchers {
		h.byToken[v.Token] = v
	}
	if err := h.loadState(); err != nil {
		return nil, fmt.Errorf("load withdraw state: %w", err)
	}
	return h, nil
}

// HandleFirst handles GET /lnurl/withdraw/{token}.
func (h *WithdrawHandler) HandleFirst(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	voucher, ok := h.byToken[token]
	if !ok {
		http.NotFound(w, r)
		return
	}

	h.mu.Lock()
	_, claimed := h.claimed[voucher.ID]
	h.mu.Unlock()
	if claimed {
		writeError(w, "this voucher has already been claimed")
		return
	}

	writeJSON(w, withdrawRequest{
		Tag:                "withdrawRequest",
		Callback:           fmt.Sprintf("%s/lnurl/withdraw/%s/claim", hostURL(r.Host), token),
		K1:                 hex.EncodeToString(h.signK1(voucher.ID, uint64(time.Now().Add(k1TTL).Unix()))),
		DefaultDescription: voucher.Description,
		MinWithdrawable:    voucher.Amount,
		MaxWithdrawable:    voucher.Amount,
	})
}

// HandleClaim handles GET /lnurl/withdraw/{token}/claim?k1=X&pr=X.
func (h *WithdrawHandler) HandleClaim(w http.ResponseWriter, r *http.Request) {
	k1 := r.URL.Query().Get("k1")
	pr := r.URL.Query().Get("pr")
	if k1 == "" || pr == "" {
		writeError(w, "missing k1 or pr parameter")
		return
	}

	voucher, ok := h.byToken[r.PathValue("token")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.verifyK1(voucher.ID, k1); err != nil {
		writeError(w, err.Error())
		return
	}
	if err := validateBolt11(pr, voucher.Amount, h.network); err != nil {
		slog.Info("withdraw invoice rejected", "voucher", voucher.ID, "err", err)
		writeError(w, fmt.Sprintf("invalid invoice: %v", err))
		return
	}

	h.mu.Lock()
	if _, claimed := h.claimed[voucher.ID]; claimed {
		h.mu.Unlock()
		writeError(w, "this voucher has already been claimed")
		return
	}
	h.claimed[voucher.ID] = ClaimedInfo{ClaimedAt: time.Now().UTC(), AmountMsat: voucher.Amount}
	err := h.saveStateLocked()
	if err != nil {
		delete(h.claimed, voucher.ID)
	}
	h.mu.Unlock()
	if err != nil {
		slog.Error("withdraw not paid: failed to save claim", "voucher", voucher.ID, "err", err)
		writeError(w, "failed to record claim")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), PayTimeout)
	defer cancel()
	maxFeeSat := max(voucher.Amount/100000, 10)
	err = h.payer.SendPayment(ctx, pr, maxFeeSat)
	switch {
	case errors.Is(err, lightning.ErrPaymentFailed):
		slog.Warn("withdraw payment failed", "voucher", voucher.ID, "err", err)
		if err := h.release(voucher.ID); err != nil {
			slog.Error("withdraw release not saved", "voucher", voucher.ID, "err", err)
		}
		writeError(w, "payment failed")
		return
	case err != nil:
		slog.Error("withdraw payment outcome unknown, voucher left claimed", "voucher", voucher.ID, "err", err)
		writeError(w, "payment outcome unknown")
		return
	}

	if h.ntfy != nil {
		h.ntfy.NotifyVoucherClaimed(voucher.ID, voucher.Amount/1000)
	}
	writeJSON(w, status{Status: "OK"})
}

func (h *WithdrawHandler) release(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.claimed, id)
	return h.saveStateLocked()
}

// signK1 returns expiry as big-endian Unix seconds followed by a MAC binding
// it to the voucher, so claims need no server-side k1 state.
func (h *WithdrawHandler) signK1(voucherID string, expiry uint64) []byte {
	b := binary.BigEndian.AppendUint64(make([]byte, 0, 40), expiry)
	mac := hmac.New(sha256.New, h.k1Key)
	mac.Write(b)
	mac.Write([]byte(voucherID))
	return mac.Sum(b)[:32]
}

func (h *WithdrawHandler) verifyK1(voucherID, k1 string) error {
	b, err := hex.DecodeString(k1)
	if err != nil || len(b) != 32 {
		return errors.New("invalid k1")
	}
	expiry := binary.BigEndian.Uint64(b)
	if !hmac.Equal(b, h.signK1(voucherID, expiry)) {
		return errors.New("invalid k1")
	}
	if uint64(time.Now().Unix()) > expiry {
		return errors.New("k1 expired, please try again")
	}
	return nil
}

// validateBolt11 checks that payReq is an unexpired invoice for network whose
// amount, if set, is expectedAmountMsat.
func validateBolt11(payReq string, expectedAmountMsat int64, network *chaincfg.Params) error {
	invoice, err := bolt11.Decode(payReq, network)
	if err != nil {
		return fmt.Errorf("invalid invoice: %w", err)
	}
	if time.Since(invoice.Timestamp) > invoice.Expiry {
		return fmt.Errorf("invoice has expired")
	}
	if invoice.AmountMsat != nil {
		if amount := int64(*invoice.AmountMsat); amount != expectedAmountMsat {
			return fmt.Errorf("invoice amount %d msat does not match voucher amount %d msat", amount, expectedAmountMsat)
		}
	}
	return nil
}

func (h *WithdrawHandler) loadState() error {
	data, err := os.ReadFile(h.statePath)
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
	if state.Claimed != nil {
		h.claimed = state.Claimed
	}
	return nil
}

func (h *WithdrawHandler) saveStateLocked() error {
	data, err := json.MarshalIndent(stateFile{Claimed: h.claimed}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(h.statePath, data)
}

// UnclaimedVouchers returns all vouchers that have not been claimed, with their full URLs.
func (h *WithdrawHandler) UnclaimedVouchers() []VoucherStatus {
	h.mu.Lock()
	defer h.mu.Unlock()

	var result []VoucherStatus
	for _, v := range h.vouchers {
		if _, claimed := h.claimed[v.ID]; !claimed {
			url := fmt.Sprintf("%s/lnurl/withdraw/%s", hostURL(v.Domain), v.Token)
			lnurl, _ := LNURLEncode(url)
			result = append(result, VoucherStatus{
				ID:     v.ID,
				Amount: v.Amount,
				URL:    url,
				LNURL:  lnurl,
			})
		}
	}
	return result
}

// VoucherStatus represents an unclaimed voucher with its public URL.
type VoucherStatus struct {
	ID     string
	Amount int64
	URL    string
	LNURL  string // bech32-encoded lnurl
}
