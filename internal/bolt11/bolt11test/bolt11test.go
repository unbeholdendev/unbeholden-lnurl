// Package bolt11test makes signed BOLT-11 invoices for tests.
package bolt11test

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
)

// Invoice returns a mainnet invoice for amountMsat, created now with
// paymentHash and committing to the hash of description. A zero amountMsat
// leaves the amount out.
func Invoice(amountMsat int64, paymentHash [32]byte, description string) string {
	return InvoiceFor(&chaincfg.MainNetParams, amountMsat, paymentHash, description)
}

// InvoiceFor is Invoice for network.
func InvoiceFor(network *chaincfg.Params, amountMsat int64, paymentHash [32]byte, description string) string {
	hrp := "ln" + network.Bech32HRPSegwit
	if amountMsat != 0 {
		hrp += strconv.FormatInt(amountMsat*10, 10) + "p"
	}
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(time.Now().Unix()))
	words, _ := bech32.ConvertBits(timestamp[:], 8, 5, true)
	data := words[len(words)-7:]
	descriptionHash := sha256.Sum256([]byte(description))
	data = append(data, field(1, paymentHash[:])...)
	data = append(data, field(23, descriptionHash[:])...)
	payload, _ := bech32.ConvertBits(data, 5, 8, true)
	digest := sha256.Sum256(append([]byte(hrp), payload...))
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	sig := ecdsa.SignCompact(key, digest[:], true)
	sig = append(sig[1:], sig[0]-31)
	sigWords, _ := bech32.ConvertBits(sig, 8, 5, true)
	encoded, err := bech32.Encode(hrp, append(data, sigWords...))
	if err != nil {
		panic(err)
	}
	return encoded
}

func field(tag byte, data []byte) []byte {
	words, _ := bech32.ConvertBits(data, 8, 5, true)
	return append([]byte{tag, byte(len(words) >> 5), byte(len(words) & 31)}, words...)
}
