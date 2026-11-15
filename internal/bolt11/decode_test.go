package bolt11

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
)

func field(tag byte, data []byte) []byte {
	return append([]byte{tag, byte(len(data) >> 5), byte(len(data) & 31)}, data...)
}

func signed(t *testing.T, hrp string, fields []byte) string {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	data := append([]byte{1, 2, 3, 4, 5, 6, 7}, fields...)
	payload, _ := bech32.ConvertBits(data, 5, 8, true)
	digest := sha256.Sum256(append([]byte(hrp), payload...))
	sig := ecdsa.SignCompact(key, digest[:], true)
	sig = append(sig[1:], sig[0]-31)
	words, _ := bech32.ConvertBits(sig, 8, 5, true)
	encoded, err := bech32.Encode(hrp, append(data, words...))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDecode(t *testing.T) {
	hash, _ := bech32.ConvertBits(bytes.Repeat([]byte{255}, 32), 8, 5, true)
	base := append(field(1, hash), field(13, nil)...)
	for _, tc := range []struct {
		name, hrp string
		fields    []byte
		network   *chaincfg.Params
		amount    int64
		expiry    time.Duration
		invalid   bool
	}{
		{name: "amountless", hrp: "lnbc", fields: base},
		{name: "bitcoin", hrp: "lnbc1", fields: base, amount: 100000000000},
		{name: "milli", hrp: "lnbc1m", fields: base, amount: 100000000},
		{name: "micro", hrp: "lnbc1u", fields: base, amount: 100000},
		{name: "nano", hrp: "lnbc1n", fields: base, amount: 100},
		{name: "pico", hrp: "lnbc10p", fields: base, amount: 1},
		{name: "description hash", hrp: "lnbc", fields: append(field(1, hash), field(23, hash)...)},
		{name: "expiry", hrp: "lnbc", fields: append(append([]byte{}, base...), field(6, []byte{1, 28})...), expiry: 60 * time.Second},
		{name: "testnet", hrp: "lntb", fields: base, network: &chaincfg.TestNet3Params},
		{name: "signet", hrp: "lntbs", fields: base, network: &chaincfg.SigNetParams},
		{name: "regtest", hrp: "lnbcrt", fields: base, network: &chaincfg.RegressionNetParams},
		{name: "wrong network", hrp: "lntb", fields: base, invalid: true},
		{name: "testnet is not signet", hrp: "lntb", fields: base, network: &chaincfg.SigNetParams, invalid: true},
		{name: "unknown multiplier", hrp: "lnbc1z", fields: base, invalid: true},
		{name: "fractional millisatoshi", hrp: "lnbc1p", fields: base, invalid: true},
		{name: "overflow", hrp: "lnbc18446744073709551615", fields: base, invalid: true},
		{name: "missing hash", hrp: "lnbc", fields: field(13, nil), invalid: true},
		{name: "missing description", hrp: "lnbc", fields: field(1, hash), invalid: true},
		{name: "both descriptions", hrp: "lnbc", fields: append(append([]byte{}, base...), field(23, hash)...), invalid: true},
		{name: "broken field", hrp: "lnbc", fields: append(append([]byte{}, base...), 1), invalid: true},
		{name: "bad UTF8", hrp: "lnbc", fields: append(field(1, hash), field(13, []byte{31, 28})...), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := tc.network
			if network == nil {
				network = &chaincfg.MainNetParams
			}
			invoice, err := Decode(signed(t, tc.hrp, tc.fields), network)
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted invalid invoice")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.amount != 0 && (invoice.AmountMsat == nil || *invoice.AmountMsat != tc.amount) {
				t.Fatalf("amount = %v, want %d", invoice.AmountMsat, tc.amount)
			}
			if tc.amount == 0 && invoice.AmountMsat != nil {
				t.Fatal("amount set on amountless invoice")
			}
			expiry := tc.expiry
			if expiry == 0 {
				expiry = time.Hour
			}
			if invoice.Expiry != expiry {
				t.Fatalf("expiry = %v, want %v", invoice.Expiry, expiry)
			}
		})
	}
}

func TestPublishedInvoice(t *testing.T) {
	const request = "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq8rkx3yf5tcsyz3d73gafnh3cax9rn449d9p5uxz9ezhhypd0elx87sjle52x86fux2ypatgddc6k63n7erqz25le42c4u4ecky03ylcqca784w"
	for _, pr := range []string{request, strings.ToUpper(request)} {
		invoice, err := Decode(pr, &chaincfg.MainNetParams)
		if err != nil || invoice.AmountMsat != nil || invoice.Timestamp.Unix() != 1496314658 {
			t.Fatalf("invoice = %+v, %v", invoice, err)
		}
	}
	if _, err := Decode(request[:len(request)-1]+"q", &chaincfg.MainNetParams); err == nil {
		t.Fatal("accepted invalid checksum")
	}
}

func TestSignature(t *testing.T) {
	hash := make([]byte, 52)
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	destination, _ := bech32.ConvertBits(key.PubKey().SerializeCompressed(), 8, 5, true)
	fields := append(append(field(1, hash), field(13, nil)...), field(19, destination)...)
	request := signed(t, "lnbc", fields)
	hrp, data, _ := bech32.DecodeNoLimit(request)
	data[0] ^= 1
	invalid, _ := bech32.Encode(hrp, data)
	if _, err := Decode(invalid, &chaincfg.MainNetParams); err == nil {
		t.Fatal("accepted invalid signature")
	}
	if _, err := Decode(request, &chaincfg.MainNetParams); err != nil {
		t.Fatal(err)
	}
}

func TestRejectBech32m(t *testing.T) {
	request := signed(t, "lnbc", append(field(1, make([]byte, 52)), field(13, nil)...))
	hrp, data, _ := bech32.DecodeNoLimit(request)
	request, err := bech32.EncodeM(hrp, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(request, &chaincfg.MainNetParams); err == nil {
		t.Fatal("accepted Bech32m invoice")
	}
}

func TestRoutingFields(t *testing.T) {
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	pubkey := key.PubKey().SerializeCompressed()
	route := append(append([]byte{}, pubkey...), make([]byte, 18)...)
	path := append(make([]byte, 28), pubkey...)
	path = append(path, 1)
	path = append(path, pubkey...)
	path = append(path, 1, 0)
	words := func(data []byte) []byte {
		out, err := bech32.ConvertBits(data, 8, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		tag     byte
		value   []byte
		invalid bool
	}{
		{name: "route", tag: 3, value: words(route)},
		{name: "empty route", tag: 3, invalid: true},
		{name: "short route", tag: 3, value: make([]byte, 8), invalid: true},
		{name: "invalid route key", tag: 3, value: words(make([]byte, 51)), invalid: true},
		{name: "fallback", tag: 9, value: append([]byte{0}, words(make([]byte, 20))...)},
		{name: "taproot fallback", tag: 9, value: append([]byte{1}, words(make([]byte, 32))...)},
		{name: "empty fallback", tag: 9, invalid: true},
		{name: "short fallback", tag: 9, value: []byte{0, 0}, invalid: true},
		{name: "unknown fallback", tag: 9, value: []byte{31, 0}},
		{name: "blinded path", tag: 20, value: words(path)},
		{name: "truncated blinded path", tag: 20, value: words(path[:len(path)-1]), invalid: true},
		{name: "empty blinded path", tag: 20, invalid: true},
		{name: "payment secret", tag: 16, value: make([]byte, 52)},
		{name: "invalid secret padding", tag: 16, value: append(make([]byte, 51), 1), invalid: true},
		{name: "invalid metadata padding", tag: 27, value: []byte{1}, invalid: true},
		{name: "CLTV", tag: 24, value: []byte{1, 0}},
		{name: "CLTV overflow", tag: 24, value: make([]byte, 14), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := append(field(1, make([]byte, 52)), field(13, nil)...)
			fields = append(fields, field(tc.tag, tc.value)...)
			_, err := Decode(signed(t, "lnbc", fields), &chaincfg.MainNetParams)
			if (err != nil) != tc.invalid {
				t.Fatalf("invalid=%v, err=%v", tc.invalid, err)
			}
		})
	}
	fields := append(field(1, make([]byte, 52)), field(13, nil)...)
	fields = append(fields, field(3, words(route))...)
	fields = append(fields, field(20, words(path))...)
	if _, err := Decode(signed(t, "lnbc", fields), &chaincfg.MainNetParams); err == nil {
		t.Fatal("accepted both route hints and blinded paths")
	}
}
