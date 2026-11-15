// Package nostr signs, verifies and publishes Nostr events (NIP-01).
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

// Event is a Nostr event.
type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// Tag returns the first tag named name, or nil.
func (e *Event) Tag(name string) []string {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			return tag
		}
	}
	return nil
}

// CountTags returns how many tags are named name.
func (e *Event) CountTags(name string) int {
	n := 0
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			n++
		}
	}
	return n
}

// hash returns the event's ID as NIP-01 defines it.
func (e *Event) hash() [32]byte {
	var b strings.Builder
	b.WriteString(`[0,"`)
	b.WriteString(e.PubKey)
	b.WriteString(`",`)
	b.WriteString(strconv.FormatInt(e.CreatedAt, 10))
	b.WriteByte(',')
	b.WriteString(strconv.Itoa(e.Kind))
	b.WriteString(",[")
	for i, tag := range e.Tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for j, s := range tag {
			if j > 0 {
				b.WriteByte(',')
			}
			writeString(&b, s)
		}
		b.WriteByte(']')
	}
	b.WriteString("],")
	writeString(&b, e.Content)
	b.WriteByte(']')
	return sha256.Sum256([]byte(b.String()))
}

// writeString writes s as a JSON string with only the escapes NIP-01 allows,
// which encoding/json does not limit itself to.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Verify checks the event's ID and signature.
func (e *Event) Verify() error {
	hash := e.hash()
	if e.ID != hex.EncodeToString(hash[:]) {
		return errors.New("id does not match event")
	}
	pubKey, err := decodeHex(e.PubKey, schnorr.PubKeyBytesLen)
	if err != nil {
		return fmt.Errorf("pubkey: %w", err)
	}
	key, err := schnorr.ParsePubKey(pubKey)
	if err != nil {
		return fmt.Errorf("pubkey: %w", err)
	}
	sigBytes, err := decodeHex(e.Sig, schnorr.SignatureSize)
	if err != nil {
		return fmt.Errorf("sig: %w", err)
	}
	sig, err := schnorr.ParseSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("sig: %w", err)
	}
	if !sig.Verify(hash[:], key) {
		return errors.New("invalid signature")
	}
	return nil
}

// Key is a Nostr private key.
type Key struct{ priv *btcec.PrivateKey }

// ParseKey parses a private key given as 64 hex characters or as nsec.
func ParseKey(s string) (Key, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if strings.HasPrefix(s, "nsec1") {
		hrp, data, err := bech32.Decode(s)
		if err != nil || hrp != "nsec" {
			return Key{}, errors.New("invalid nsec")
		}
		if raw, err = bech32.ConvertBits(data, 5, 8, false); err != nil {
			return Key{}, errors.New("invalid nsec")
		}
	} else {
		var err error
		if raw, err = hex.DecodeString(s); err != nil {
			return Key{}, errors.New("not hex or nsec")
		}
	}
	if len(raw) != 32 {
		return Key{}, fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	priv, _ := btcec.PrivKeyFromBytes(raw)
	return Key{priv}, nil
}

// PubKey returns the key's public key in hex, as events carry it.
func (k Key) PubKey() string {
	return hex.EncodeToString(schnorr.SerializePubKey(k.priv.PubKey()))
}

// Sign sets the event's pubkey, ID and signature.
func (k Key) Sign(e *Event) error {
	e.PubKey = k.PubKey()
	hash := e.hash()
	sig, err := schnorr.Sign(k.priv, hash[:])
	if err != nil {
		return err
	}
	e.ID = hex.EncodeToString(hash[:])
	e.Sig = hex.EncodeToString(sig.Serialize())
	return nil
}

func decodeHex(s string, size int) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != size {
		return nil, fmt.Errorf("%d bytes, want %d", len(b), size)
	}
	return b, nil
}
