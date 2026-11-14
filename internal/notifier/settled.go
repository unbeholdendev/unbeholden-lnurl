package notifier

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Invoices is the part of the LND client WatchSettled needs.
type Invoices interface {
	// LastSettleIndex returns the highest settle index on the node.
	LastSettleIndex(ctx context.Context) (uint64, error)
	// SubscribeInvoices streams invoices settled after settleIndex to
	// onSettled until the stream ends or onSettled returns an error.
	// resumeFrom is a safe settle index to resubscribe from.
	SubscribeInvoices(ctx context.Context, settleIndex uint64, onSettled func(amountMsat int64, memo string, resumeFrom uint64) error) error
}

// WatchSettled notifies about every invoice settled after its first start,
// until ctx is cancelled. It records the last notified settle index in
// statePath and resumes after it, so invoices settled while it was down, or
// whose notification failed, are notified on the next attempt.
func WatchSettled(ctx context.Context, invoices Invoices, statePath string, n *Ntfy) {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		start := time.Now()
		err := watchOnce(ctx, invoices, statePath, n)
		if ctx.Err() != nil {
			return
		}

		if time.Since(start) > time.Minute {
			backoff = 1 * time.Second
		}
		log.Printf("invoice subscriber stopped: %v, retrying in %s...", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func watchOnce(ctx context.Context, invoices Invoices, statePath string, n *Ntfy) error {
	from, err := loadSettleIndex(statePath)
	if errors.Is(err, fs.ErrNotExist) {
		if from, err = invoices.LastSettleIndex(ctx); err != nil {
			return err
		}
		err = saveSettleIndex(statePath, from)
	}
	if err != nil {
		return err
	}

	log.Printf("invoice subscriber: starting after settle index %d", from)
	return invoices.SubscribeInvoices(ctx, from, func(amountMsat int64, memo string, resumeFrom uint64) error {
		amountSats := amountMsat / 1000
		log.Printf("invoice settled: %d sats", amountSats)
		if err := n.NotifyPayment(amountSats, memo); err != nil {
			return err
		}
		return saveSettleIndex(statePath, resumeFrom)
	})
}

func loadSettleIndex(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func saveSettleIndex(path string, index uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(index, 10)+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
