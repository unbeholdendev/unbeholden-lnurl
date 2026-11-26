// Package lightning holds what the node backends share.
package lightning

import (
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// ErrPaymentFailed is returned by a backend's SendPayment when the node
// reports the payment as failed, so no part of it can still settle.
var ErrPaymentFailed = errors.New("payment failed")

// ErrPaymentNotFound is returned by a backend's LookupPayment when the node
// has no payment for the hash.
var ErrPaymentNotFound = errors.New("payment not found")

// PaymentStatus is where an outgoing payment stands on the node.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
)

// Payment is an outgoing payment on the node. Preimage and FeeMsat are set
// once it has succeeded.
type Payment struct {
	Status   PaymentStatus
	Preimage []byte
	FeeMsat  int64
}

// Invoice is an invoice on the node.
type Invoice struct {
	PaymentRequest string
	PaymentHash    []byte
	Expired        bool
	Settled        bool
	Preimage       []byte // nil until settled
}

// Settled is an invoice paid on the node.
type Settled struct {
	AmountMsat     int64
	Memo           string
	PaymentHash    []byte
	PaymentRequest string
	Preimage       []byte
	SettledAt      time.Time
}

// DefaultMaxFee is the fee limit for a payment of amountMsat: 1%, but at
// least 10 sat.
func DefaultMaxFee(amountMsat int64) int64 {
	return max(amountMsat/100, 10000)
}

// ErrInvoiceNotFound is returned by a backend's LookupInvoice for an unknown
// payment hash.
var ErrInvoiceNotFound = errors.New("invoice not found")

// ChainParams returns the parameters of the chain a node names network.
func ChainParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet", "bitcoin":
		return &chaincfg.MainNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "testnet4":
		return &chaincfg.TestNet4Params, nil
	case "signet":
		return &chaincfg.SigNetParams, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "simnet":
		return &chaincfg.SimNetParams, nil
	default:
		return nil, fmt.Errorf("unsupported network %q", network)
	}
}
