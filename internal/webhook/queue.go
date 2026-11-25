package webhook

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"unbeholden-lnurl/internal/atomicfile"
	"unbeholden-lnurl/internal/config"
)

type Event struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Data       map[string]any `json:"data"`
}

type Queue struct {
	endpoints map[string]*endpoint
	client    *http.Client
}

type endpoint struct {
	dir    string
	cursor string
	cfg    config.WebhookConfig
	wake   chan struct{}
	mu     sync.Mutex
}

type metadata struct {
	URLHash string
	Payment bool
	Cursor  string
}

type delivery struct {
	ID       string
	Body     json.RawMessage
	Enqueued time.Time
	Next     time.Time
	Attempts int
}

func hash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func writeJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b)
}

func Open(dir string, endpoints map[string]config.WebhookConfig, proxy *url.URL) (*Queue, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	active := make(map[string]string)
	for name, cfg := range endpoints {
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, fmt.Errorf("webhook %q requires an HTTPS URL", name)
		}
		active["active-"+hash(name)] = name
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	states := make(map[string]metadata)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "active-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		name, keep := active[entry.Name()]
		if keep {
			b, err := os.ReadFile(filepath.Join(path, "endpoint.json"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if err == nil {
				var m metadata
				if err := json.Unmarshal(b, &m); err != nil {
					return nil, err
				}
				if m.URLHash == hash(endpoints[name].URL) {
					states[name] = m
					continue
				}
			}
		}
		if err := os.Rename(path, filepath.Join(dir, "archive-"+strings.TrimPrefix(entry.Name(), "active-")+"-"+rand.Text())); err != nil {
			return nil, err
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	q := &Queue{endpoints: make(map[string]*endpoint), client: client}
	for base, name := range active {
		cfg := endpoints[name]
		path := filepath.Join(dir, base)
		m := states[name]
		payment := slices.Contains(cfg.Events, "payment.received")
		if m.Cursor == "" || m.Payment != payment {
			m = metadata{hash(cfg.URL), payment, "cursor-" + rand.Text()}
			if err := writeJSON(filepath.Join(path, "endpoint.json"), m); err != nil {
				return nil, err
			}
		}
		for _, sub := range []string{"pending", "failed"} {
			if err := os.MkdirAll(filepath.Join(path, sub), 0700); err != nil {
				return nil, err
			}
		}
		if err := syncDir(path); err != nil {
			return nil, err
		}
		q.endpoints[name] = &endpoint{dir: path, cursor: filepath.Join(path, m.Cursor), cfg: cfg, wake: make(chan struct{}, 1)}
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	return q, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (q *Queue) Cursor(name string) string {
	if ep := q.endpoints[name]; ep != nil {
		return ep.cursor
	}
	return ""
}

func (q *Queue) Enqueue(name string, event Event) error {
	ep := q.endpoints[name]
	if ep == nil {
		return fmt.Errorf("unknown webhook %q", name)
	}
	if event.ID == "" {
		return errors.New("webhook event ID is empty")
	}
	file := hash(event.ID) + ".json"
	ep.mu.Lock()
	defer ep.mu.Unlock()
	for _, sub := range []string{"pending", "failed"} {
		_, err := os.Stat(filepath.Join(ep.dir, sub, file))
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	item := delivery{ID: event.ID, Body: body, Enqueued: time.Now().UTC()}
	if err := writeJSON(filepath.Join(ep.dir, "pending", file), item); err != nil {
		return err
	}
	select {
	case ep.wake <- struct{}{}:
	default:
	}
	return nil
}

func (q *Queue) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, ep := range q.endpoints {
		workers.Go(func() { ep.run(ctx, q.client) })
	}
	<-ctx.Done()
	workers.Wait()
	q.client.CloseIdleConnections()
}
