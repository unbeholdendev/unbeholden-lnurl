// Package zap implements Lightning zaps (NIP-57): it checks zap requests,
// keeps them until their invoice is paid, then publishes zap receipts.
package zap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"unbeholden-lnurl/internal/atomicfile"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/nostr"
)

const (
	requestKind = 9734
	receiptKind = 9735

	maxRequestLen      = 16 << 10
	maxRelays          = 20
	maxPendingRequests = 4096
)

// Publisher sends events to relays and returns how many accepted them.
type Publisher interface {
	Publish(ctx context.Context, relays []string, e nostr.Event) int
}

// Zapper checks zap requests and publishes their receipts once paid.
type Zapper struct {
	key       nostr.Key
	publisher Publisher
	dir       string
	mu        sync.Mutex
}

// New returns a Zapper signing receipts with key and keeping zap requests
// for unpaid invoices in dir.
func New(key nostr.Key, publisher Publisher, dir string) *Zapper {
	return &Zapper{key: key, publisher: publisher, dir: dir}
}

// PubKey returns the key receipts are signed with, in hex.
func (z *Zapper) PubKey() string {
	return z.key.PubKey()
}

// Request checks raw as a zap request for amountMsat, as NIP-57 Appendix D
// requires, and returns it.
func (z *Zapper) Request(raw string, amountMsat int64) (nostr.Event, error) {
	var e nostr.Event
	z.mu.Lock()
	err := z.checkCapacity()
	z.mu.Unlock()
	if err != nil {
		return e, err
	}
	if len(raw) > maxRequestLen {
		return e, errors.New("zap request too long")
	}
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return e, errors.New("zap request is not a nostr event")
	}
	if e.Kind != requestKind {
		return e, fmt.Errorf("zap request has kind %d, want %d", e.Kind, requestKind)
	}
	if err := e.Verify(); err != nil {
		return e, fmt.Errorf("zap request: %w", err)
	}
	if e.CountTags("p") != 1 || len(e.Tag("p")) < 2 {
		return e, errors.New("zap request must have one p tag")
	}
	if e.CountTags("e") > 1 {
		return e, errors.New("zap request must have at most one e tag")
	}
	if len(e.Tag("relays")) < 2 {
		return e, errors.New("zap request has no relays")
	}
	if tag := e.Tag("amount"); tag != nil && (len(tag) < 2 || tag[1] != strconv.FormatInt(amountMsat, 10)) {
		return e, errors.New("zap request amount does not match amount")
	}
	if tag := e.Tag("a"); tag != nil && (len(tag) < 2 || !isCoordinate(tag[1])) {
		return e, errors.New("zap request a tag is not an event coordinate")
	}
	if n := e.CountTags("P"); n > 1 || n == 1 && (len(e.Tag("P")) < 2 || e.Tag("P")[1] != z.PubKey()) {
		return e, errors.New("zap request P tag must name this server's key")
	}
	return e, nil
}

// isCoordinate reports whether s is kind:pubkey:d-tag.
func isCoordinate(s string) bool {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return false
	}
	if _, err := strconv.ParseUint(parts[0], 10, 16); err != nil {
		return false
	}
	pubKey, err := hex.DecodeString(parts[1])
	return err == nil && len(pubKey) == 32
}

// Track keeps raw, a zap request checked by Request, until the invoice with
// paymentHash is paid.
func (z *Zapper) Track(paymentHash []byte, raw string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if len(raw) > maxRequestLen {
		return errors.New("zap request too long")
	}
	if _, err := os.Stat(z.path(paymentHash)); errors.Is(err, fs.ErrNotExist) {
		if err := z.checkCapacity(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return atomicfile.Write(z.path(paymentHash), []byte(raw))
}

func (z *Zapper) checkCapacity() error {
	entries, err := os.ReadDir(z.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) >= maxPendingRequests {
		return errors.New("pending zap request limit reached")
	}
	return nil
}

// Settled publishes the zap receipt for invoice, if it was issued for a zap
// request. Relays that refuse it are logged, not retried.
func (z *Zapper) Settled(ctx context.Context, invoice lightning.Settled) error {
	path := z.path(invoice.PaymentHash)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var request nostr.Event
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("zap request %s: %w", filepath.Base(path), err)
	}

	receipt := nostr.Event{
		CreatedAt: invoice.SettledAt.Unix(),
		Kind:      receiptKind,
		Tags:      [][]string{request.Tag("p")},
		Content:   "",
	}
	for _, name := range []string{"e", "a", "k"} {
		if tag := request.Tag(name); len(tag) >= 2 {
			receipt.Tags = append(receipt.Tags, tag[:2])
		}
	}
	receipt.Tags = append(receipt.Tags,
		[]string{"P", request.PubKey},
		[]string{"bolt11", invoice.PaymentRequest},
		[]string{"description", string(raw)},
		[]string{"preimage", hex.EncodeToString(invoice.Preimage)},
	)
	if err := z.key.Sign(&receipt); err != nil {
		return err
	}

	relays := slices.Compact(slices.Sorted(slices.Values(request.Tag("relays")[1:])))
	if len(relays) > maxRelays {
		relays = relays[:maxRelays]
	}
	accepted := z.publisher.Publish(ctx, relays, receipt)
	if err := ctx.Err(); err != nil {
		return err
	}
	if accepted == 0 {
		slog.Warn("zap receipt published to no relay", "receipt", receipt.ID, "relays", len(relays))
	} else {
		slog.Info("zap receipt published", "receipt", receipt.ID, "relays", accepted)
	}
	return os.Remove(path)
}

func (z *Zapper) path(paymentHash []byte) string {
	return filepath.Join(z.dir, hex.EncodeToString(paymentHash))
}

// Invoices looks up whether a pending zap invoice has expired unpaid.
type Invoices interface {
	LookupInvoice(context.Context, []byte) (lightning.Invoice, error)
}

// Prune removes expired unpaid requests and requests missing from the node
// after eight days. Other outcomes remain available for settlement recovery.
func (z *Zapper) Prune(ctx context.Context, invoices Invoices) {
	entries, err := os.ReadDir(z.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("read pending zaps", "err", err)
		}
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		hash, err := hex.DecodeString(entry.Name())
		if err != nil {
			continue
		}
		invoice, err := invoices.LookupInvoice(ctx, hash)
		if errors.Is(err, lightning.ErrInvoiceNotFound) {
			if time.Since(info.ModTime()) < 8*24*time.Hour {
				continue
			}
		} else if err != nil {
			slog.Warn("check pending zap", "hash", entry.Name(), "err", err)
			continue
		}
		if err == nil && (!invoice.Expired || invoice.Settled) {
			continue
		}
		if err := os.Remove(filepath.Join(z.dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove expired zap", "hash", entry.Name(), "err", err)
		}
	}
}
