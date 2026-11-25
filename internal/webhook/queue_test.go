package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"unbeholden-lnurl/internal/config"
)

func openQueue(t *testing.T, dir string, cfg map[string]config.WebhookConfig) *Queue {
	t.Helper()
	q, err := Open(dir, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEndpointLifecycle(t *testing.T) {
	for _, change := range []string{"remove", "url", "subscription"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			hook := config.WebhookConfig{URL: "https://first.example", Events: []string{"payment.received"}}
			cfg := map[string]config.WebhookConfig{"one": hook, "two": hook}
			q := openQueue(t, dir, cfg)
			cursor := q.Cursor("one")
			if !filepath.IsAbs(cursor) || cursor == q.Cursor("two") {
				t.Fatal(cursor)
			}
			if err := os.WriteFile(cursor, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := q.Enqueue("one", Event{ID: "a"}); err != nil {
				t.Fatal(err)
			}
			if got := openQueue(t, dir, cfg).Cursor("one"); got != cursor {
				t.Fatal(got)
			}
			switch change {
			case "remove":
				openQueue(t, dir, nil)
			case "url":
				other := hook
				other.URL = "https://second.example"
				openQueue(t, dir, map[string]config.WebhookConfig{"one": other})
			case "subscription":
				other := hook
				other.Events = nil
				openQueue(t, dir, map[string]config.WebhookConfig{"one": other})
			}
			q = openQueue(t, dir, cfg)
			fresh := q.Cursor("one")
			if fresh == cursor {
				t.Fatal("reused cursor")
			}
			if _, err := os.Stat(fresh); !os.IsNotExist(err) {
				t.Fatalf("fresh cursor: %v", err)
			}
			prefix := "archive-*"
			if change == "subscription" {
				prefix = filepath.Base(q.endpoints["one"].dir)
			}
			files, err := filepath.Glob(filepath.Join(dir, prefix, "pending", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("queued event: %v, %v", files, err)
			}
			if change != "subscription" {
				entries, err := os.ReadDir(filepath.Join(q.endpoints["one"].dir, "pending"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("fresh queue: %v, %v", entries, err)
				}
			}
		})
	}
}

func TestRetryAndDuplicateEnqueue(t *testing.T) {
	var tries atomic.Int32
	received := make(chan string, 10)
	key := []byte("test signing key")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		id := r.Header.Get("webhook-id")
		ts := r.Header.Get("webhook-timestamp")
		stamp, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || time.Since(time.Unix(stamp, 0)).Abs() > time.Minute {
			t.Errorf("timestamp %q", ts)
		}
		mac := hmac.New(sha256.New, key)
		fmt.Fprintf(mac, "%s.%s.", id, ts)
		mac.Write(b)
		if r.Header.Get("webhook-signature") != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Error("signature")
		}
		want := fmt.Sprintf(`{"id":%q,"type":"payment.received","occurred_at":"1970-01-01T00:00:00Z","data":{"amount":9007199254740993}}`, id)
		if string(b) != want || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, b)
		}
		if id == "first" && tries.Add(1) == 1 {
			w.WriteHeader(503)
		}
		received <- id
	}))
	defer server.Close()
	cfg := map[string]config.WebhookConfig{"one": {URL: server.URL, Key: key}}
	dir := t.TempDir()
	q := openQueue(t, dir, cfg)
	enqueue := func(id string) {
		t.Helper()
		if err := q.Enqueue("one", Event{id, "payment.received", time.Unix(0, 0).UTC(), map[string]any{"amount": int64(9007199254740993)}}); err != nil {
			t.Error(err)
		}
	}
	enqueue("first")
	q = openQueue(t, dir, cfg)
	q.client.Transport = server.Client().Transport
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); q.Run(ctx) }()
	defer func() { cancel(); <-done }()
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("received %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing %s", want)
		}
	}
	expect("first")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { enqueue("first") })
	}
	wg.Wait()
	enqueue("second")
	expect("second")
	expect("first")
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(q.endpoints["one"].dir, "pending"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acknowledged deliveries remain on disk")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

}

func TestExpiredFailure(t *testing.T) {
	q := openQueue(t, t.TempDir(), map[string]config.WebhookConfig{"one": {URL: "https://unused.example"}})
	ep := q.endpoints["one"]
	file := hash("old") + ".json"
	item := delivery{ID: "old", Enqueued: time.Now().Add(-lifetime), Body: json.RawMessage(`{"id":"old"}`)}
	if err := writeJSON(filepath.Join(ep.dir, "pending", file), item); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.process(t.Context(), nil, file); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ep.dir, "failed", file))
	if err != nil {
		t.Fatal(err)
	}
	var got delivery
	if err := json.Unmarshal(b, &got); err != nil || string(got.Body) != string(item.Body) {
		t.Fatalf("failed event %s, %v", b, err)
	}
}

func TestIndependentWorkersAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	delivered := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/slow" {
			close(entered)
			<-r.Context().Done()
			return
		}
		close(delivered)
	}))
	defer server.Close()
	q := openQueue(t, t.TempDir(), map[string]config.WebhookConfig{
		"slow": {URL: server.URL + "/slow"}, "fast": {URL: server.URL + "/fast"},
	})
	q.client.Transport = server.Client().Transport
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); q.Run(ctx) }()
	defer cancel()
	if err := q.Enqueue("slow", Event{ID: "same"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow worker did not start")
	}
	if err := q.Enqueue("fast", Event{ID: "same"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow endpoint blocked fast endpoint")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	b, err := os.ReadFile(filepath.Join(q.endpoints["slow"].dir, "pending", hash("same")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var item delivery
	if err := json.Unmarshal(b, &item); err != nil || item.Attempts != 0 {
		t.Fatalf("cancelled attempt: %s, %v", b, err)
	}
}

func TestRequiresHTTPS(t *testing.T) {
	if _, err := Open(t.TempDir(), map[string]config.WebhookConfig{"plain": {URL: "http://localhost"}}, nil); err == nil {
		t.Fatal("accepted HTTP")
	}
}
