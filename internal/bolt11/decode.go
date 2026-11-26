package bolt11

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
)

// Invoice contains the fields needed to validate and track a payment.
type Invoice struct {
	PaymentHash     [32]byte
	DescriptionHash *[32]byte
	Timestamp       time.Time
	AmountMsat      *int64
	Expiry          time.Duration
}

// Decode checks the invoice checksum, signature, network and required fields.
func Decode(paymentRequest string, network *chaincfg.Params) (Invoice, error) {
	var invoice Invoice
	if len(paymentRequest) > 7089 {
		return invoice, fmt.Errorf("invoice too large")
	}
	hrp, data, err := bech32.DecodeNoLimit(paymentRequest)
	if err != nil {
		return invoice, err
	}
	encoded, err := bech32.Encode(hrp, data)
	if err != nil || encoded != strings.ToLower(paymentRequest) {
		return invoice, fmt.Errorf("invoice must use Bech32 checksum")
	}
	prefix := "ln" + network.Bech32HRPSegwit
	if network.Name == chaincfg.SigNetParams.Name {
		prefix = "lntbs"
	}
	if !strings.HasPrefix(hrp, prefix) {
		return invoice, fmt.Errorf("invoice is for another network")
	}
	amount := strings.TrimPrefix(hrp, prefix)
	if amount != "" {
		multiplier := int64(100000000000)
		switch amount[len(amount)-1] {
		case 'm':
			multiplier = 100000000
		case 'u':
			multiplier = 100000
		case 'n':
			multiplier = 100
		case 'p':
			multiplier = 0
		default:
			if amount[len(amount)-1] < '0' || amount[len(amount)-1] > '9' {
				return invoice, fmt.Errorf("unknown amount multiplier")
			}
		}
		if amount[len(amount)-1] < '0' || amount[len(amount)-1] > '9' {
			amount = amount[:len(amount)-1]
		}
		n, e := strconv.ParseUint(amount, 10, 64)
		if e != nil {
			return invoice, fmt.Errorf("invalid amount")
		}
		if multiplier == 0 {
			if n < 10 || n%10 != 0 {
				return invoice, fmt.Errorf("amount below one millisatoshi")
			}
			n /= 10
		} else {
			if n > math.MaxInt64/uint64(multiplier) {
				return invoice, fmt.Errorf("amount overflow")
			}
			n *= uint64(multiplier)
		}
		if n > math.MaxInt64 {
			return invoice, fmt.Errorf("amount overflow")
		}
		value := int64(n)
		invoice.AmountMsat = &value
	}
	if len(data) < 7+104 {
		return invoice, fmt.Errorf("invoice too short")
	}
	unsigned := data[:len(data)-104]
	signature, err := bech32.ConvertBits(data[len(data)-104:], 5, 8, false)
	if err != nil || len(signature) != 65 {
		return invoice, fmt.Errorf("invalid signature")
	}
	payload, err := bech32.ConvertBits(unsigned, 5, 8, true)
	if err != nil {
		return invoice, err
	}
	digest := sha256.Sum256(append([]byte(hrp), payload...))
	timestamp, _ := integer(unsigned[:7])
	invoice.Timestamp = time.Unix(int64(timestamp), 0)
	invoice.Expiry = time.Hour
	var paymentHash, description, descriptionHash, expiry bool
	var destination *btcec.PublicKey
	seen := make(map[byte]bool)
	var routeHints, blindedPaths bool
	for fields := unsigned[7:]; len(fields) > 0; {
		if len(fields) < 3 {
			return invoice, fmt.Errorf("truncated field")
		}
		tag, length := fields[0], int(fields[1])*32+int(fields[2])
		fields = fields[3:]
		if length > len(fields) {
			return invoice, fmt.Errorf("truncated field")
		}
		value := fields[:length]
		fields = fields[length:]
		switch tag {
		case 1:
			if length == 52 && !paymentHash {
				decoded, err := bech32.ConvertBits(value, 5, 8, false)
				if err != nil {
					return invoice, err
				}
				invoice.PaymentHash = [32]byte(decoded)
				paymentHash = true
			}
		case 13:
			if !description {
				text, e := bech32.ConvertBits(value, 5, 8, false)
				if e != nil || !utf8.Valid(text) {
					return invoice, fmt.Errorf("invalid description")
				}
				description = true
			}
		case 23:
			if length == 52 && !descriptionHash {
				decoded, err := bech32.ConvertBits(value, 5, 8, false)
				if err != nil {
					return invoice, err
				}
				invoice.DescriptionHash = (*[32]byte)(decoded)
				descriptionHash = true
			}
		case 6:
			if expiry {
				continue
			}
			expiry = true
			seconds, e := integer(value)
			if e != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
				return invoice, fmt.Errorf("expiry overflow")
			}
			invoice.Expiry = time.Duration(seconds) * time.Second

		case 16, 27:
			if seen[tag] || tag == 16 && length != 52 {
				continue
			}
			if _, err := bech32.ConvertBits(value, 5, 8, false); err != nil {
				return invoice, err
			}
			seen[tag] = true
		case 24:
			if seen[tag] {
				continue
			}
			if len(value) > 13 {
				return invoice, fmt.Errorf("CLTV overflow")
			}
			if _, err := integer(value); err != nil {
				return invoice, err
			}
			seen[tag] = true
		case 9:
			if seen[tag] {
				continue
			}
			if length < 2 {
				return invoice, fmt.Errorf("empty fallback address")
			}
			version := value[0]
			if version != 0 && version != 1 && version != 17 && version != 18 {
				continue
			}
			address, err := bech32.ConvertBits(value[1:], 5, 8, false)
			if err != nil {
				return invoice, err
			}
			if version == 0 && len(address) != 20 && len(address) != 32 || version == 1 && len(address) != 32 || (version == 17 || version == 18) && len(address) != 20 {
				return invoice, fmt.Errorf("invalid fallback address length")
			}
			seen[tag] = true
		case 3:
			route, err := bech32.ConvertBits(value, 5, 8, false)
			if err != nil {
				return invoice, err
			}
			if len(route) == 0 || len(route)%51 != 0 {
				return invoice, fmt.Errorf("invalid route hint length")
			}
			for len(route) > 0 {
				if _, err := btcec.ParsePubKey(route[:33]); err != nil {
					return invoice, err
				}
				route = route[51:]
			}
			routeHints = true
		case 20:
			path, err := bech32.ConvertBits(value, 5, 8, false)
			if err != nil {
				return invoice, err
			}
			if err := validateBlindedPath(path); err != nil {
				return invoice, err
			}
			blindedPaths = true
		case 19:
			if length == 53 && destination == nil {
				key, e := bech32.ConvertBits(value, 5, 8, false)
				if e != nil {
					return invoice, e
				}
				destination, e = btcec.ParsePubKey(key)
				if e != nil {
					return invoice, e
				}
			}
		}
	}
	if routeHints && blindedPaths {
		return invoice, fmt.Errorf("cannot combine route hints and blinded paths")
	}
	if !paymentHash || description == descriptionHash {
		return invoice, fmt.Errorf("missing payment hash or description")
	}
	if destination != nil {
		var r, s btcec.ModNScalar
		if r.SetByteSlice(signature[:32]) || s.SetByteSlice(signature[32:64]) || r.IsZero() || s.IsZero() || !ecdsa.NewSignature(&r, &s).Verify(digest[:], destination) {
			return invoice, fmt.Errorf("invalid invoice signature")
		}
	} else {
		if signature[64] > 3 {
			return invoice, fmt.Errorf("invalid recovery ID")
		}
		compact := append([]byte{signature[64] + 31}, signature[:64]...)
		if _, _, err := ecdsa.RecoverCompact(compact, digest[:]); err != nil {
			return invoice, fmt.Errorf("invalid signature: %w", err)
		}
	}
	return invoice, nil
}

func integer(words []byte) (uint64, error) {
	var n uint64
	for _, word := range words {
		if n > math.MaxUint64>>5 {
			return 0, fmt.Errorf("integer overflow")
		}
		n = n<<5 | uint64(word)
	}
	return n, nil
}

func validateBlindedPath(path []byte) error {
	if len(path) < 28 {
		return fmt.Errorf("truncated blinded path")
	}
	featureLength := int(binary.BigEndian.Uint16(path[26:28]))
	if len(path) < 28+featureLength+34 {
		return fmt.Errorf("truncated blinded path")
	}
	path = path[28+featureLength:]
	if _, err := btcec.ParsePubKey(path[:33]); err != nil {
		return err
	}
	hops := int(path[33])
	path = path[34:]
	for i := 0; i < hops; i++ {
		if len(path) < 34 {
			return fmt.Errorf("truncated blinded hop")
		}
		if _, err := btcec.ParsePubKey(path[:33]); err != nil {
			return err
		}
		length := int(path[33])
		path = path[34:]
		switch length {
		case 253:
			if len(path) < 2 {
				return fmt.Errorf("truncated blinded hop length")
			}
			length = int(binary.BigEndian.Uint16(path[:2]))
			path = path[2:]
			if length < 253 {
				return fmt.Errorf("non-canonical blinded hop length")
			}
		case 254, 255:
			return fmt.Errorf("blinded hop ciphertext too large")
		}
		if length > 535 || length > len(path) {
			return fmt.Errorf("invalid blinded hop ciphertext length")
		}
		path = path[length:]
	}
	return nil
}
