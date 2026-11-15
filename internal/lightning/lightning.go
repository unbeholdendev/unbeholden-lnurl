// Package lightning holds what the node backends share.
package lightning

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
)

// ErrPaymentFailed is returned by a backend's SendPayment when the node
// reports the payment as failed, so no part of it can still settle.
var ErrPaymentFailed = errors.New("payment failed")

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
