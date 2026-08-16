package service

import (
	"crypto/ecdsa"

	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
)

// TronAddressFromECDSAPublicKey encodes an ECDSA public key using TRON's
// native Base58Check format. The TRON payload already includes the 0x41
// prefix, so callers must not wrap it in a second Bitcoin-style version byte.
func TronAddressFromECDSAPublicKey(pub ecdsa.PublicKey) string {
	return tronaddress.PubkeyToAddress(pub).String()
}
