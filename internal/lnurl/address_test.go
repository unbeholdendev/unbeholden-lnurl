package lnurl

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/nostr"
	"unbeholden-lnurl/internal/zap"
)

var testVerifyKey = []byte("verify key")

func TestAddressMetadata_SimpleValues(t *testing.T) {
	meta := addressMetadata(config.AddressEntry{Description: "Lightning Address"}, "satoshi", "pay.example.com")

	var parsed [][]string
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("metadata is not valid JSON: %v\nraw: %s", err, meta)
	}

	if len(parsed) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(parsed))
	}
	if parsed[0][0] != "text/plain" || parsed[0][1] != "Lightning Address" {
		t.Errorf("unexpected text/plain entry: %v", parsed[0])
	}
	if parsed[1][0] != "text/identifier" || parsed[1][1] != "satoshi@pay.example.com" {
		t.Errorf("unexpected text/identifier entry: %v", parsed[1])
	}
}

func TestAddressMetadata_SpecialCharsInUsername(t *testing.T) {
	meta := addressMetadata(config.AddressEntry{Description: "desc"}, `user"inject`, "example.com")

	var parsed [][]string
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("metadata with special chars is not valid JSON: %v\nraw: %s", err, meta)
	}

	if parsed[1][1] != `user"inject@example.com` {
		t.Errorf("username not preserved correctly: got %s", parsed[1][1])
	}
}

func TestAddressMetadata_SpecialCharsInDescription(t *testing.T) {
	meta := addressMetadata(config.AddressEntry{Description: `He said "hello"`}, "user", "example.com")

	var parsed [][]string
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("metadata with quotes in description is not valid JSON: %v\nraw: %s", err, meta)
	}

	if parsed[0][1] != `He said "hello"` {
		t.Errorf("description not preserved correctly: got %s", parsed[0][1])
	}
}

func TestAddressMetadata_BackslashInUsername(t *testing.T) {
	meta := addressMetadata(config.AddressEntry{Description: "desc"}, `user\name`, "example.com")

	var parsed [][]string
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("metadata with backslash is not valid JSON: %v\nraw: %s", err, meta)
	}

	if parsed[1][1] != `user\name@example.com` {
		t.Errorf("username not preserved correctly: got %s", parsed[1][1])
	}
}

func TestAddressMetadata_LongDescriptionAndImage(t *testing.T) {
	meta := addressMetadata(config.AddressEntry{
		Description:     "short",
		LongDescription: "long\ntext",
		ImageType:       "image/png;base64",
		Image:           "iVBO",
	}, "user", "example.com")

	var parsed [][]string
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 4 || parsed[2][0] != "text/long-desc" || parsed[2][1] != "long\ntext" ||
		parsed[3][0] != "image/png;base64" || parsed[3][1] != "iVBO" {
		t.Fatalf("metadata = %v", parsed)
	}
}

func TestHandleWellKnown_DomainFromHost(t *testing.T) {
	h := NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000},
	}, nil, nil, testVerifyKey, nil)

	req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob", nil)
	req.SetPathValue("username", "bob")
	rec := httptest.NewRecorder()
	h.HandleWellKnown(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := "https://shop.example.com/.well-known/lnurlp/bob/invoice"; resp["callback"] != want {
		t.Errorf("callback = %v, want %s", resp["callback"], want)
	}
	if !strings.Contains(resp["metadata"].(string), "bob@shop.example.com") {
		t.Errorf("metadata = %v", resp["metadata"])
	}
}

func TestHandleInvoice(t *testing.T) {
	node := &fakeNode{}
	h := NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000},
	}, node, node, testVerifyKey, nil)

	get := func(query string) map[string]any {
		req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob/invoice?"+query, nil)
		req.SetPathValue("username", "bob")
		rec := httptest.NewRecorder()
		h.HandleInvoice(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get("amount=1500&comment=hi")
	if resp["pr"] != "lnbc-1500-hi" || resp["routes"] == nil || resp["disposable"] != false {
		t.Fatalf("response = %v", resp)
	}
	if _, ok := resp["successAction"]; ok {
		t.Fatalf("successAction without success_message: %v", resp)
	}
	if want := "https://shop.example.com/.well-known/lnurlp/bob/verify/" + node.lastHash(); !strings.HasPrefix(resp["verify"].(string), want) {
		t.Fatalf("verify = %v, want %s", resp["verify"], want)
	}
	if len(node.notified) != 1 || node.notified[0] != "invoice 1500 "+node.lastHash()+" bob@shop.example.com hi" {
		t.Fatalf("notified = %v", node.notified)
	}
	if resp := get("amount=999"); resp["status"] != "ERROR" {
		t.Fatalf("below min: %v", resp)
	}
	if resp := get("amount=1500&comment=" + strings.Repeat("x", maxCommentLen+1)); resp["status"] != "ERROR" {
		t.Fatalf("long comment: %v", resp)
	}
}

func TestHandleInvoice_WithoutNotifier(t *testing.T) {
	h := NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000},
	}, &fakeNode{}, nil, testVerifyKey, nil)
	req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob/invoice?amount=1500", nil)
	req.SetPathValue("username", "bob")
	rec := httptest.NewRecorder()
	h.HandleInvoice(rec, req)
	if !strings.Contains(rec.Body.String(), "lnbc-1500") {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestHandleInvoice_SuccessMessage(t *testing.T) {
	h := NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000, SuccessMessage: "Thanks!"},
	}, &fakeNode{}, nil, testVerifyKey, nil)
	req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob/invoice?amount=1500", nil)
	req.SetPathValue("username", "bob")
	rec := httptest.NewRecorder()
	h.HandleInvoice(rec, req)
	if want := `"successAction":{"tag":"message","message":"Thanks!"}`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("response = %s, want %s", rec.Body.String(), want)
	}
}

func TestHandleVerify(t *testing.T) {
	node := &fakeNode{}
	h := NewAddressHandler(map[string]config.AddressEntry{
		"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000},
	}, node, nil, testVerifyKey, nil)

	call := func(handler http.HandlerFunc, path, username, token string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "https://shop.example.com"+path, nil)
		req.SetPathValue("username", username)
		req.SetPathValue("token", token)
		rec := httptest.NewRecorder()
		handler(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v: %s", path, err, rec.Body.String())
		}
		return resp
	}
	verify := func(username, token string) map[string]any {
		return call(h.HandleVerify, "/verify", username, token)
	}

	issued := call(h.HandleInvoice, "/invoice?amount=1500", "bob", "")
	hash := node.lastHash()
	token := issued["verify"].(string)[strings.LastIndex(issued["verify"].(string), "/")+1:]

	resp := verify("bob", token)
	if resp["status"] != "OK" || resp["settled"] != false || resp["preimage"] != nil || resp["pr"] != "lnbc-1500-" {
		t.Fatalf("unpaid: %v", resp)
	}
	if _, ok := resp["preimage"]; !ok {
		t.Fatalf("unpaid: preimage missing, want null: %v", resp)
	}

	node.settle(hash, []byte{0xab, 0xcd})
	if resp := verify("bob", token); resp["settled"] != true || resp["preimage"] != "abcd" {
		t.Fatalf("settled: %v", resp)
	}

	for name, args := range map[string][2]string{
		"other address": {"alice", token},
		"unsigned hash": {"bob", hash},
		"forged token":  {"bob", hash + strings.Repeat("00", 16)},
		"unknown hash":  {"bob", hex.EncodeToString(h.verifyToken("bob", make([]byte, 32)))},
		"not hex":       {"bob", "xyz"},
	} {
		if resp := verify(args[0], args[1]); resp["status"] != "ERROR" {
			t.Errorf("%s: %v", name, resp)
		}
	}
}

func TestHandleVerify_AfterMetadataChange(t *testing.T) {
	node := &fakeNode{}
	entry := config.AddressEntry{Description: "before", MinSendable: 1000, MaxSendable: 2000}
	before := NewAddressHandler(map[string]config.AddressEntry{"*": entry}, node, nil, testVerifyKey, nil)
	req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob/invoice?amount=1500", nil)
	req.SetPathValue("username", "bob")
	rec := httptest.NewRecorder()
	before.HandleInvoice(rec, req)
	var issued map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	node.settle(node.lastHash(), []byte{1})

	entry.Description = "after"
	after := NewAddressHandler(map[string]config.AddressEntry{"*": entry}, node, nil, testVerifyKey, nil)
	req = httptest.NewRequest("GET", issued["verify"].(string), nil)
	req.SetPathValue("username", "bob")
	req.SetPathValue("token", strings.TrimPrefix(issued["verify"].(string), "https://shop.example.com/.well-known/lnurlp/bob/verify/"))
	rec = httptest.NewRecorder()
	after.HandleVerify(rec, req)
	if !strings.Contains(rec.Body.String(), `"settled":true`) {
		t.Fatalf("verify after metadata change = %s", rec.Body.String())
	}
}

func zapRequest(t *testing.T, content string, amountMsat int64) string {
	t.Helper()
	sender, err := nostr.ParseKey(strings.Repeat("02", 32))
	if err != nil {
		t.Fatal(err)
	}
	e := nostr.Event{CreatedAt: 1700000000, Kind: 9734, Content: content, Tags: [][]string{
		{"p", strings.Repeat("ab", 32)},
		{"relays", "wss://relay.example"},
		{"amount", strconv.FormatInt(amountMsat, 10)},
	}}
	if err := sender.Sign(&e); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestZaps(t *testing.T) {
	key, err := nostr.ParseKey(strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	zapper := zap.New(key, nil, dir)
	node := &fakeNode{}
	addresses := map[string]config.AddressEntry{"*": {Description: "any", MinSendable: 1000, MaxSendable: 2000}}
	h := NewAddressHandler(addresses, node, nil, testVerifyKey, zapper)

	get := func(h *AddressHandler, handler func(*AddressHandler) http.HandlerFunc, query string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "https://shop.example.com/.well-known/lnurlp/bob?"+query, nil)
		req.SetPathValue("username", "bob")
		rec := httptest.NewRecorder()
		handler(h)(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	wellKnown := func(h *AddressHandler) http.HandlerFunc { return h.HandleWellKnown }
	invoice := func(h *AddressHandler) http.HandlerFunc { return h.HandleInvoice }

	if resp := get(h, wellKnown, ""); resp["allowsNostr"] != true || resp["nostrPubkey"] != key.PubKey() {
		t.Fatalf("payRequest = %v", resp)
	}

	long := strings.Repeat("é", maxCommentLen)
	raw := zapRequest(t, long, 1500)
	if resp := get(h, invoice, "amount=1500&nostr="+url.QueryEscape(raw)); resp["pr"] == nil {
		t.Fatalf("zap invoice: %v", resp)
	}
	if node.description != raw {
		t.Fatalf("invoice description = %q, want the zap request", node.description)
	}
	if node.memo != long[:maxCommentLen] {
		t.Fatalf("memo = %q, want the content cut to %d bytes", node.memo, maxCommentLen)
	}
	if stored, err := os.ReadFile(filepath.Join(dir, node.lastHash())); err != nil || string(stored) != raw {
		t.Fatalf("stored zap request = %q, %v", stored, err)
	}

	if resp := get(h, invoice, "amount=1000&nostr="+url.QueryEscape(raw)); resp["status"] != "ERROR" {
		t.Fatalf("zap with wrong amount: %v", resp)
	}

	off := NewAddressHandler(addresses, node, nil, testVerifyKey, nil)
	if resp := get(off, wellKnown, ""); resp["allowsNostr"] != nil || resp["nostrPubkey"] != nil {
		t.Fatalf("payRequest without zaps = %v", resp)
	}
	get(off, invoice, "amount=1500&nostr="+url.QueryEscape(raw))
	if !strings.HasPrefix(node.description, `[["text/plain"`) {
		t.Fatalf("description without zaps = %q, want the metadata", node.description)
	}
}

func TestOnionCallbacks(t *testing.T) {
	h := NewAddressHandler(map[string]config.AddressEntry{"*": {MinSendable: 1000, MaxSendable: 2000}}, &fakeNode{}, nil, testVerifyKey, nil)
	for _, endpoint := range []string{"", "/invoice?amount=1500"} {
		req := httptest.NewRequest("GET", "http://test.onion:80/.well-known/lnurlp/bob"+endpoint, nil)
		req.SetPathValue("username", "bob")
		rec := httptest.NewRecorder()
		if endpoint == "" {
			h.HandleWellKnown(rec, req)
		} else {
			h.HandleInvoice(rec, req)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		field := "callback"
		if endpoint != "" {
			field = "verify"
		}
		if got, _ := resp[field].(string); !strings.HasPrefix(got, "http://test.onion:80/") {
			t.Fatalf("%s = %v", field, resp[field])
		}
	}
}
