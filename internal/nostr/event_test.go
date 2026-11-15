package nostr

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// signed is a real event, with tags and escapes, from go-nostr's tests.
const signed = `{"kind":3,"id":"9e662bdd7d8abc40b5b15ee1ff5e9320efc87e9274d8d440c58e6eed2dddfbe2","pubkey":"373ebe3d45ec91977296a178d9f19f326c70631d2a1b0bbba5c5ecc2eb53b9e7","created_at":1644844224,"tags":[["p","3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],["p","75fc5ac2487363293bd27fb0d14fb966477d0f1dbc6361d37806a6a740eda91e"],["p","46d0dfd3a724a302ca9175163bdf788f3606b3fd1bb12d5fe055d1e418cb60ea"]],"content":"{\"wss://nostr-pub.wellorder.net\":{\"read\":true,\"write\":true},\"wss://nostr.bitcoiner.social\":{\"read\":false,\"write\":true},\"wss://expensive-relay.fiatjaf.com\":{\"read\":true,\"write\":true},\"wss://relayer.fiatjaf.com\":{\"read\":true,\"write\":true},\"wss://relay.bitid.nz\":{\"read\":true,\"write\":true},\"wss://nostr.rocks\":{\"read\":true,\"write\":true}}","sig":"811355d3484d375df47581cb5d66bed05002c2978894098304f20b595e571b7e01b2efd906c5650080ffe49cf1c62b36715698e9d88b9e8be43029a2f3fa66be"}`

func TestVerify_SignedEvent(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(signed), &e); err != nil {
		t.Fatal(err)
	}
	if err := e.Verify(); err != nil {
		t.Fatal(err)
	}
	e.Content = "changed"
	if err := e.Verify(); err == nil {
		t.Fatal("verified a changed event")
	}
}

func TestSign_RoundTrip(t *testing.T) {
	key, err := ParseKey(strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	e := Event{CreatedAt: 1, Kind: 1, Tags: [][]string{{"t", "a\"b\\c\nd <&>\x01"}}, Content: "hi"}
	if err := key.Sign(&e); err != nil {
		t.Fatal(err)
	}
	if e.PubKey != key.PubKey() {
		t.Fatalf("pubkey = %s, want %s", e.PubKey, key.PubKey())
	}
	if err := e.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestParseKey(t *testing.T) {
	// From NIP-19.
	const (
		nsec = "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"
		hex  = "67dea2ed018072d675f5415ecfaed7d2597555e202d85b3d65ea4e58d2d92ffa"
	)
	a, err := ParseKey(nsec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseKey(hex + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.PubKey() != b.PubKey() {
		t.Fatalf("nsec and hex give %s and %s", a.PubKey(), b.PubKey())
	}
	for _, bad := range []string{"", "nsec1xyz", "abcd", "npub1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestCountTags(t *testing.T) {
	e := Event{Tags: [][]string{{"p", "a"}, {}, {"p", "b"}, {"e", "c"}}}
	if e.CountTags("p") != 2 || e.CountTags("e") != 1 || e.CountTags("a") != 0 {
		t.Fatal("wrong counts")
	}
	if tag := e.Tag("e"); len(tag) != 2 || tag[1] != "c" {
		t.Fatalf("Tag(e) = %v", tag)
	}
}

func TestHash_EscapesOnlyWhatNIP01Allows(t *testing.T) {
	e := Event{PubKey: "ab", CreatedAt: 1, Kind: 1, Tags: [][]string{{"t", "a\"b\\c\nd <&>\x01\b\f\r\t é"}}, Content: "hi"}
	hash := e.hash()
	if got, want := hex.EncodeToString(hash[:]), "d8a0fb8f10d8edb4d42a33892cce58ef79b1a3a34588949b17ae830e61acd449"; got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}
