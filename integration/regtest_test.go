package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/zpay32"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/nostr"
)

const addressYAML = `
addresses:
  alice:
    description: "Regtest tips"
    long_description: "A longer regtest description"
    success_message: "Thank you!"
    min_sendable: 1000
    max_sendable: 1000000
  "*":
    description: "Wildcard tips"
    min_sendable: 1000
    max_sendable: 1000000
`

type response struct {
	Status        string  `json:"status"`
	Reason        string  `json:"reason"`
	Tag           string  `json:"tag"`
	Callback      string  `json:"callback"`
	Metadata      string  `json:"metadata"`
	MinSendable   int64   `json:"minSendable"`
	MaxSendable   int64   `json:"maxSendable"`
	PR            string  `json:"pr"`
	Verify        string  `json:"verify"`
	Settled       bool    `json:"settled"`
	Preimage      *string `json:"preimage"`
	Disposable    bool    `json:"disposable"`
	SuccessAction *struct {
		Tag     string `json:"tag"`
		Message string `json:"message"`
	} `json:"successAction"`
	AllowsNostr     bool   `json:"allowsNostr"`
	NostrPubkey     string `json:"nostrPubkey"`
	K1              string `json:"k1"`
	MinWithdrawable int64  `json:"minWithdrawable"`
	MaxWithdrawable int64  `json:"maxWithdrawable"`
}

func TestRegtest(t *testing.T) {
	if os.Getenv("LNURL_REGTEST") != "1" {
		t.Skip("set LNURL_REGTEST=1 to start disposable Bitcoin, LND and Core Lightning nodes")
	}
	binary := filepath.Join(t.TempDir(), "unbeholden-lnurl")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../cmd/unbeholden-lnurl")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build service: %v\n%s", err, out)
	}
	t.Run("InvalidConfigurationFailsBeforeConnecting", func(t *testing.T) { testInvalidConfig(t, binary) })
	t.Run("Backends", func(t *testing.T) {
		n := startNetwork(t)
		for _, backend := range []string{"lnd", "cln"} {
			t.Run(backend, func(t *testing.T) {
				node := n.backendYAML(t, backend)
				t.Run("NotificationsRecoverAfterDowntimeAndDeliveryFailure", func(t *testing.T) { testNotifications(t, n, binary, node) })
				t.Run("AddressOptionsAndVerificationSurviveRestart", func(t *testing.T) { testAddress(t, n, binary, node) })
				t.Run("ConfiguredRateLimitAndProxyTrust", func(t *testing.T) { testRateLimit(t, binary, node) })
				t.Run("WithdrawClaimSurvivesRestart", func(t *testing.T) { testWithdraw(t, n, binary, node) })
				t.Run("ZapRecoveryWithoutNotifications", func(t *testing.T) { testZap(t, n, binary, node) })
			})
		}
	})
}

type service struct {
	binary     string
	configPath string
	stateDir   string
	address    string
	logPath    string
	cmd        *exec.Cmd
	done       chan error
}

func newService(t *testing.T, binary, node, extra string) *service {
	t.Helper()
	dir := t.TempDir()
	s := &service{binary: binary, configPath: filepath.Join(dir, "config.yaml"), stateDir: filepath.Join(dir, "state"), address: freeAddress(t), logPath: filepath.Join(dir, "service.log")}
	s.configure(t, node, extra)
	t.Cleanup(func() {
		s.stop(t)
		if t.Failed() {
			data, _ := os.ReadFile(s.logPath)
			t.Logf("service log:\n%s", data)
		}
	})
	return s
}

func (s *service) configure(t *testing.T, node, extra string) {
	t.Helper()
	data := fmt.Sprintf("listen: %q\nstate_dir: %q\n%s\n%s", s.address, s.stateDir, node, extra)
	write(t, s.configPath, []byte(data))
}

func (s *service) start(t *testing.T, httpEnabled bool) {
	t.Helper()
	log, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	s.cmd = exec.Command(s.binary, "-config", s.configPath)
	s.cmd.Stdout, s.cmd.Stderr = log, log
	if err := s.cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	log.Close()
	s.done = make(chan error, 1)
	go func() { s.done <- s.cmd.Wait() }()
	eventually(t, "service startup", func() bool {
		select {
		case err := <-s.done:
			s.cmd = nil
			t.Fatalf("service exited at startup: %v", err)
		default:
		}
		if !httpEnabled {
			_, err := os.Stat(filepath.Join(s.stateDir, "settle_index"))
			return err == nil
		}
		conn, err := net.DialTimeout("tcp", s.address, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
}

func (s *service) stop(t *testing.T) {
	t.Helper()
	if s.cmd == nil {
		return
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Error(err)
	}
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("service exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		s.cmd.Process.Kill()
		<-s.done
		t.Error("service did not stop within 15 seconds")
	}
	s.cmd = nil
}

func (s *service) get(t *testing.T, path string, headers map[string]string) (int, response) {
	t.Helper()
	if strings.HasPrefix(path, "https://") {
		u, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		path = u.RequestURI()
	}
	req, err := http.NewRequest("GET", "http://"+s.address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "tips.example.test"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got response
	if resp.StatusCode == http.StatusOK && path != "/health" {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, body)
		}
	}
	return resp.StatusCode, got
}

func (s *service) invoice(t *testing.T, query string) response {
	t.Helper()
	code, got := s.get(t, "/.well-known/lnurlp/alice/invoice?amount=100000"+query, nil)
	if code != 200 || got.PR == "" || got.Status == "ERROR" {
		t.Fatalf("invoice: HTTP %d %+v", code, got)
	}
	return got
}

func testAddress(t *testing.T, n *network, binary, node string) {
	var avatar bytes.Buffer
	if err := png.Encode(&avatar, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "avatar.png")
	write(t, imagePath, avatar.Bytes())
	options := strings.Replace(addressYAML, "    success_message:", fmt.Sprintf("    image_file: %q\n    success_message:", imagePath), 1)
	s := newService(t, binary, node, options)
	s.start(t, true)
	_, discovery := s.get(t, "/.well-known/lnurlp/alice", nil)
	if discovery.Tag != "payRequest" || discovery.MinSendable != 1000 || discovery.MaxSendable != 1000000 || discovery.AllowsNostr {
		t.Fatalf("discovery: %+v", discovery)
	}
	var metadata [][2]string
	if err := json.Unmarshal([]byte(discovery.Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	for _, pair := range metadata {
		entries[pair[0]] = pair[1]
	}
	if entries["text/plain"] != "Regtest tips" || entries["text/long-desc"] != "A longer regtest description" || entries["text/identifier"] != "alice@tips.example.test" || entries["image/png;base64"] != base64.StdEncoding.EncodeToString(avatar.Bytes()) {
		t.Fatalf("metadata: %v", entries)
	}
	_, wildcard := s.get(t, "/.well-known/lnurlp/someone", nil)
	if !strings.Contains(wildcard.Metadata, "Wildcard tips") {
		t.Fatalf("wildcard: %+v", wildcard)
	}
	invoice := s.invoice(t, "&comment=integration-test")
	if invoice.Disposable || invoice.SuccessAction == nil || invoice.SuccessAction.Tag != "message" || invoice.SuccessAction.Message != "Thank you!" {
		t.Fatalf("invoice options: %+v", invoice)
	}
	checkInvoice(t, invoice.PR, discovery.Metadata)
	_, unpaid := s.get(t, invoice.Verify, nil)
	if unpaid.Status != "OK" || unpaid.Settled || unpaid.Preimage != nil {
		t.Fatalf("unpaid verify: %+v", unpaid)
	}
	for _, amount := range []string{"999", "1000001", "bad"} {
		_, got := s.get(t, "/.well-known/lnurlp/alice/invoice?amount="+amount, nil)
		if got.Status != "ERROR" {
			t.Fatalf("invalid amount %s accepted: %+v", amount, got)
		}
	}
	s.stop(t)
	s.configure(t, node, strings.ReplaceAll(options, "Regtest tips", "Changed tips"))
	s.start(t, true)
	preimage := n.pay(t, invoice.PR)
	_, settled := s.get(t, invoice.Verify, nil)
	if settled.Status != "OK" || !settled.Settled || settled.Preimage == nil || *settled.Preimage != preimage || settled.PR != invoice.PR {
		t.Fatalf("settled verify after config edit: %+v", settled)
	}
	_, forged := s.get(t, invoice.Verify[:len(invoice.Verify)-2]+"ff00", nil)
	if forged.Status != "ERROR" {
		t.Fatalf("forged verify token accepted: %+v", forged)
	}
	_, wrongUser := s.get(t, strings.Replace(invoice.Verify, "/alice/", "/someone/", 1), nil)
	if wrongUser.Status != "ERROR" {
		t.Fatalf("verify token accepted for another username: %+v", wrongUser)
	}
}

func checkInvoice(t *testing.T, bolt11, description string) {
	t.Helper()
	inv, err := zpay32.Decode(bolt11, &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(description))
	if inv.MilliSat == nil || int64(*inv.MilliSat) != 100000 || inv.DescriptionHash == nil || *inv.DescriptionHash != hash {
		t.Fatalf("invoice does not commit to amount and description: %s", bolt11)
	}
}

func testNotifications(t *testing.T, n *network, binary, node string) {
	var fail atomic.Bool
	fail.Store(true)
	attempts := make(chan string, 32)
	received := make(chan string, 32)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/regtest" || r.Header.Get("Authorization") != "Bearer regtest-token" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Title") == "Payment received" {
			attempts <- string(body)
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			received <- string(body)
		}
	}))
	defer sink.Close()
	tokenFile := filepath.Join(t.TempDir(), "ntfy-token")
	write(t, tokenFile, []byte("regtest-token\n"))
	notifications := fmt.Sprintf("\nnotifications:\n  ntfy:\n    url: %q\n    topic: regtest\n    token_file: %q\n", sink.URL, tokenFile)
	s := newService(t, binary, node, addressYAML+notifications)
	s.start(t, true)
	first := s.invoice(t, "&comment=paid-during-downtime")
	s.stop(t)
	n.pay(t, first.PR)
	s.start(t, true)
	select {
	case <-attempts:
	case <-time.After(15 * time.Second):
		t.Fatal("downtime payment was not retried")
	}
	second := s.invoice(t, "&comment=paid-after-restart")
	n.pay(t, second.PR)
	fail.Store(false)
	seen := map[string]bool{}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for len(seen) < 2 {
		select {
		case body := <-received:
			for _, memo := range []string{"paid-during-downtime", "paid-after-restart"} {
				if strings.Contains(body, memo) {
					seen[memo] = true
				}
			}
		case <-timer.C:
			t.Fatalf("missing notifications after recovery: %v", seen)
		}
	}

	third := s.invoice(t, "&comment=notifications-only")
	s.stop(t)
	s.configure(t, node, notifications)
	s.start(t, false)
	n.pay(t, third.PR)
	if conn, err := net.DialTimeout("tcp", s.address, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("notifications-only service opened HTTP listener")
	}
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case body := <-received:
			if strings.Contains(body, "notifications-only") {
				return
			}
		case <-timeout.C:
			t.Fatal("notifications-only service did not deliver payment")
		}
	}
}

func testRateLimit(t *testing.T, binary, node string) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%t", trusted), func(t *testing.T) {
			extra := addressYAML + "\nrate_limit:\n  per_minute: 60\n  burst: 2\n"
			if trusted {
				extra += "trusted_proxies: [127.0.0.1]\n"
			}
			s := newService(t, binary, node, extra)
			s.start(t, true)
			h := map[string]string{"X-Forwarded-For": "192.0.2.1", "X-Forwarded-Host": "forwarded.example.test"}
			code, got := s.get(t, "/.well-known/lnurlp/alice", h)
			if code != 200 {
				t.Fatalf("first request: %d", code)
			}
			host := "tips.example.test"
			if trusted {
				host = "forwarded.example.test"
			}
			if !strings.HasPrefix(got.Callback, "https://"+host+"/") {
				t.Fatalf("forwarded host: %s", got.Callback)
			}
			if code, _ := s.get(t, "/health", h); code != 200 {
				t.Fatalf("second request: %d", code)
			}
			if code, _ := s.get(t, "/health", h); code != 429 {
				t.Fatalf("exhausted burst: %d", code)
			}
			h["X-Forwarded-For"] = "192.0.2.2"
			want := 429
			if trusted {
				want = 200
			}
			if code, _ := s.get(t, "/health", h); code != want {
				t.Fatalf("different forwarded client: got %d want %d", code, want)
			}
			h["X-Forwarded-For"] = "192.0.2.1"
			eventually(t, "rate-limit refill", func() bool { code, _ := s.get(t, "/health", h); return code == 200 })
		})
	}
}

func testWithdraw(t *testing.T, n *network, binary, node string) {
	extra := `
withdraw_secret: "regtest-secret-at-least-32-characters"
withdraw:
  - id: tip
    domain: tips.example.test
    description: Regtest voucher
    amount: 100000
  - id: disconnected
    domain: tips.example.test
    description: Disconnected voucher
    amount: 100000
`
	s := newService(t, binary, node, extra)
	s.start(t, true)
	cfg, err := config.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	token := cfg.ExpandVouchers()[0].Token
	path := "/lnurl/withdraw/" + token
	_, discovery := s.get(t, path, nil)
	if discovery.Tag != "withdrawRequest" || discovery.K1 == "" || discovery.MinWithdrawable != 100000 || discovery.MaxWithdrawable != 100000 {
		t.Fatalf("withdraw discovery: %+v", discovery)
	}
	invoice := n.invoice(t, 100000)
	claim := discovery.Callback + "?" + url.Values{"k1": {discovery.K1}, "pr": {invoice}}.Encode()
	_, got := s.get(t, claim, nil)
	if got.Status != "OK" {
		t.Fatalf("withdraw: %+v", got)
	}
	s.stop(t)
	s.start(t, true)
	_, got = s.get(t, claim, nil)
	if got.Status != "ERROR" || !strings.Contains(got.Reason, "already been claimed") {
		t.Fatalf("repeated withdraw: %+v", got)
	}
	_, got = s.get(t, path, nil)
	if got.Status != "ERROR" {
		t.Fatalf("claimed voucher reopened: %+v", got)
	}

	disconnectedPath := "/lnurl/withdraw/" + cfg.ExpandVouchers()[1].Token
	_, disconnected := s.get(t, disconnectedPath, nil)
	disconnectedInvoice := n.invoice(t, 100000)
	u, err := url.Parse(disconnected.Callback)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", s.address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := u.Path + "?" + url.Values{"k1": {disconnected.K1}, "pr": {disconnectedInvoice}}.Encode()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: tips.example.test\r\nConnection: close\r\n\r\n", requestPath); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	eventually(t, "withdraw payment after client disconnect", func() bool {
		invoices := run(t, n.payer, "-k", "listinvoices", "invstring="+disconnectedInvoice)["invoices"].([]any)
		return len(invoices) == 1 && invoices[0].(map[string]any)["status"] == "paid"
	})
	s.stop(t)
	s.start(t, true)
	_, got = s.get(t, disconnectedPath, nil)
	if got.Status != "ERROR" {
		t.Fatalf("disconnected voucher reopened: %+v", got)
	}
	s.stop(t)
	write(t, filepath.Join(s.stateDir, "state.json"), []byte("broken JSON"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-config", s.configPath).CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "load withdraw state") {
		t.Fatalf("corrupt state did not fail startup: %v\n%s", err, out)
	}
}

func testZap(t *testing.T, n *network, binary, node string) {
	keyPath := filepath.Join(t.TempDir(), "nostr.key")
	write(t, keyPath, []byte(strings.Repeat("01", 32)))
	extra := addressYAML + fmt.Sprintf("\nnostr:\n  private_key_file: %q\n", keyPath)
	s := newService(t, binary, node, extra)
	s.start(t, true)
	_, discovery := s.get(t, "/.well-known/lnurlp/alice", nil)
	if !discovery.AllowsNostr || len(discovery.NostrPubkey) != 64 {
		t.Fatalf("zap discovery: %+v", discovery)
	}
	key, err := nostr.ParseKey(strings.Repeat("02", 32))
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: 9734, CreatedAt: time.Now().Unix(), Tags: [][]string{{"p", key.PubKey()}, {"P", discovery.NostrPubkey}, {"amount", "100000"}, {"relays", "wss://127.0.0.1:1"}}, Content: "regtest zap"}
	if err := key.Sign(&event); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	invoice := s.invoice(t, "&nostr="+url.QueryEscape(string(raw)))
	checkInvoice(t, invoice.PR, string(raw))
	s.stop(t)
	preimage := n.pay(t, invoice.PR)
	s.start(t, true)
	_, got := s.get(t, invoice.Verify, nil)
	if !got.Settled || got.Preimage == nil || *got.Preimage != preimage {
		t.Fatalf("zap verification: %+v", got)
	}
	eventually(t, "recovered zap receipt and private relay refusal", func() bool {
		log, _ := os.ReadFile(s.logPath)
		return bytes.Contains(log, []byte("zap receipt published to no relay")) && bytes.Contains(log, []byte("not a public address"))
	})
}

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	return address
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testInvalidConfig(t *testing.T, binary string) {
	node := "lnd:\n  host: 127.0.0.1:1\n  tls_cert: /missing/cert\n  macaroon: /missing/macaroon\n"
	valid := "listen: 127.0.0.1:0\n" + node + addressYAML
	for name, tc := range map[string]struct{ yaml, reason string }{
		"no backend":           {"listen: 127.0.0.1:0\n" + addressYAML, "configure one node"},
		"two backends":         {valid + "cln:\n  url: https://127.0.0.1:1\n  rune_file: /missing/rune\n", "configure one node"},
		"no features":          {node, "nothing to do"},
		"missing listen":       {node + addressYAML, "listen is required"},
		"zero rate":            {valid + "rate_limit:\n  per_minute: 0\n", "must be positive"},
		"negative burst":       {valid + "rate_limit:\n  burst: -1\n", "must be positive"},
		"nostr without state":  {valid + "nostr:\n  private_key_file: /missing/key\n", "state_dir is required"},
		"missing nostr key":    {valid + "state_dir: /unused\nnostr:\n  private_key_file: /missing/key\n", "read nostr.private_key_file"},
		"missing ntfy token":   {valid + "state_dir: /unused\nnotifications:\n  ntfy:\n    url: http://127.0.0.1:1\n    topic: test\n    token_file: /missing/token\n", "read notifications.ntfy.token_file"},
		"long success message": {strings.Replace(valid, "Thank you!", strings.Repeat("x", 145), 1), "success_message must be at most 144"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			write(t, path, []byte(tc.yaml))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, "-config", path).CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(out), tc.reason) {
				t.Fatalf("invalid config: %v\n%s; want %q", err, out, tc.reason)
			}
		})
	}
}
