package notifier

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"unbeholden-lnurl/internal/atomicfile"
	"unbeholden-lnurl/internal/lightning"
)

// Invoices is the part of a node WatchSettled needs. An index is the node's
// count of paid invoices, rising with each one.
type Invoices interface {
	// LastPaidIndex returns the index of the last invoice paid on the node.
	LastPaidIndex(ctx context.Context) (uint64, error)
	// SubscribeInvoices streams invoices paid after index after to
	// onSettled until the stream ends or onSettled returns an error.
	// resumeFrom is a safe index to resubscribe from.
	SubscribeInvoices(ctx context.Context, after uint64, onSettled func(invoice lightning.Settled, resumeFrom uint64) error) error
}

// WatchSettled passes every invoice settled after its first start to
// onSettled, until ctx is cancelled. It records the last handled index in
// statePath and resumes after it, so invoices settled while it was down, or
// for which onSettled failed, are passed again on the next attempt.
func WatchSettled(ctx context.Context, invoices Invoices, statePath string, onSettled func(lightning.Settled) error) {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		start := time.Now()
		err := watchOnce(ctx, invoices, statePath, onSettled)
		if ctx.Err() != nil {
			return
		}

		if time.Since(start) > time.Minute {
			backoff = 1 * time.Second
		}
		slog.Warn("invoice subscriber stopped", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// InitSettled records the initial settlement position before invoices are served.
// An existing position is preserved for recovery.
func InitSettled(ctx context.Context, invoices Invoices, statePath string) error {
	_, err := loadIndex(statePath)
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	from, err := invoices.LastPaidIndex(ctx)
	if err != nil {
		return err
	}
	return saveIndex(statePath, from)
}

func watchOnce(ctx context.Context, invoices Invoices, statePath string, onSettled func(lightning.Settled) error) error {
	if err := InitSettled(ctx, invoices, statePath); err != nil {
		return err
	}
	from, err := loadIndex(statePath)
	if err != nil {
		return err
	}

	slog.Info("invoice subscriber starting", "after_index", from)
	return invoices.SubscribeInvoices(ctx, from, func(invoice lightning.Settled, resumeFrom uint64) error {
		slog.Info("invoice settled", "sats", invoice.AmountMsat/1000)
		if err := onSettled(invoice); err != nil {
			return err
		}
		return saveIndex(statePath, resumeFrom)
	})
}

func loadIndex(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func saveIndex(path string, index uint64) error {
	return atomicfile.Write(path, []byte(strconv.FormatUint(index, 10)+"\n"))
}
