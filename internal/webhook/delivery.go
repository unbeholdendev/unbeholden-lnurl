package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const lifetime = 72 * time.Hour

func (ep *endpoint) run(ctx context.Context, client *http.Client) {
	for ctx.Err() == nil {
		wakeAt := time.Now().Add(lifetime)
		files, err := os.ReadDir(filepath.Join(ep.dir, "pending"))
		if err != nil {
			slog.Error("read webhook queue", "err", err)
			wakeAt = time.Now().Add(time.Minute)
		}
		for _, f := range files {
			if ctx.Err() != nil {
				return
			}
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			next, err := ep.process(ctx, client, f.Name())
			if err != nil {
				slog.Error("process webhook", "file", f.Name(), "err", err)
				next = time.Now().Add(time.Minute)
			}
			if !next.IsZero() && next.Before(wakeAt) {
				wakeAt = next
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ep.wake:
		case <-time.After(time.Until(wakeAt)):
		}
	}
}

func (ep *endpoint) process(ctx context.Context, client *http.Client, file string) (time.Time, error) {
	path := filepath.Join(ep.dir, "pending", file)
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	var item delivery
	if err := json.Unmarshal(b, &item); err != nil {
		return time.Time{}, err
	}
	expiry := item.Enqueued.Add(lifetime)

	if time.Now().Before(expiry) {
		if time.Now().Before(item.Next) {
			return item.Next, nil
		}
		attempt, cancel := context.WithDeadline(ctx, expiry)
		status := ep.send(attempt, client, item)
		cancel()
		if ctx.Err() != nil {
			return time.Time{}, nil
		}
		if status >= 200 && status < 300 {
			ep.mu.Lock()
			defer ep.mu.Unlock()
			if err := os.Remove(path); err != nil {
				return time.Time{}, err
			}
			return time.Time{}, syncDir(filepath.Dir(path))
		}
		if status == 0 || status == 408 || status == 429 || status >= 500 {
			item.Attempts++
			delay := min(time.Hour, time.Second<<min(item.Attempts-1, 12))
			item.Next = time.Now().Add(delay)
			if item.Next.After(expiry) {
				item.Next = expiry
			}
			return item.Next, writeJSON(path, item)
		}
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	dst := filepath.Join(ep.dir, "failed", file)
	if err := os.Rename(path, dst); err != nil {
		return time.Time{}, err
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return time.Time{}, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return time.Time{}, err
	}
	slog.Warn("webhook delivery failed", "file", dst, "event", item.ID)
	return time.Time{}, nil
}

func (ep *endpoint) send(ctx context.Context, client *http.Client, item delivery) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.cfg.URL, bytes.NewReader(item.Body))
	if err != nil {
		return 0
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, ep.cfg.Key)
	fmt.Fprintf(mac, "%s.%s.", item.ID, ts)
	mac.Write(item.Body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", item.ID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}
