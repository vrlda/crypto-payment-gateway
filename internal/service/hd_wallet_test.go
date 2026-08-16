package service

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/ethereum/go-ethereum/crypto"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
)

func TestCreateAccountKeyFromSeedUsesDistinctAccounts(t *testing.T) {
	t.Parallel()

	svc := NewHDWalletService(false)
	seed := []byte("01234567890123456789012345678901")

	xpub0, _, err := svc.CreateAccountKeyFromSeed(seed, "ERC20", 0)
	if err != nil {
		t.Fatalf("CreateAccountKeyFromSeed(account=0) failed: %v", err)
	}

	xpub1, _, err := svc.CreateAccountKeyFromSeed(seed, "ERC20", 1)
	if err != nil {
		t.Fatalf("CreateAccountKeyFromSeed(account=1) failed: %v", err)
	}

	if xpub0 == xpub1 {
		t.Fatalf("expected different xpubs for distinct derivation accounts")
	}
}

func TestEd25519AccountDerivationUsesAccountNamespace(t *testing.T) {
	t.Parallel()

	svc := NewHDWalletService(false)
	seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
	if err != nil {
		t.Fatalf("GenerateSeed failed: %v", err)
	}
	seedHex := hex.EncodeToString(seed)

	solana0, err := svc.DeriveSolanaPrivateKey(seedHex, 0, 1)
	if err != nil {
		t.Fatalf("DeriveSolanaPrivateKey(account=0) failed: %v", err)
	}

	solana1, err := svc.DeriveSolanaPrivateKey(seedHex, 1, 1)
	if err != nil {
		t.Fatalf("DeriveSolanaPrivateKey(account=1) failed: %v", err)
	}

	if string(solana0) == string(solana1) {
		t.Fatalf("expected distinct Solana private keys for distinct derivation accounts")
	}

	ton0, err := svc.DeriveTonPrivateKey(seedHex, 0, 1)
	if err != nil {
		t.Fatalf("DeriveTonPrivateKey(account=0) failed: %v", err)
	}

	ton1, err := svc.DeriveTonPrivateKey(seedHex, 1, 1)
	if err != nil {
		t.Fatalf("DeriveTonPrivateKey(account=1) failed: %v", err)
	}

	if string(ton0) == string(ton1) {
		t.Fatalf("expected distinct TON private keys for distinct derivation accounts")
	}
}

func TestTronAddressFromECDSAPublicKeyProducesValidBase58(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	address := TronAddressFromECDSAPublicKey(key.PublicKey)
	if address == "" {
		t.Fatal("expected TRON address, got empty string")
	}
	if address[0] != 'T' {
		t.Fatalf("expected TRON address to start with T, got %q", address)
	}
	if _, err := tronaddress.Base58ToAddress(address); err != nil {
		t.Fatalf("expected valid TRON address, got error: %v", err)
	}
}

func TestGenerateTronAddressProducesValidBase58(t *testing.T) {
	t.Parallel()

	svc := NewHDWalletService(false)
	seed := []byte("01234567890123456789012345678901")

	xpub, _, err := svc.CreateAccountKeyFromSeed(seed, "TRC20", 0)
	if err != nil {
		t.Fatalf("CreateAccountKeyFromSeed(TRC20) failed: %v", err)
	}

	address, err := svc.GenerateAddress("TRC20", xpub, 0)
	if err != nil {
		t.Fatalf("GenerateAddress(TRC20) failed: %v", err)
	}
	if address == "" {
		t.Fatal("expected TRON address, got empty string")
	}
	if address[0] != 'T' {
		t.Fatalf("expected TRON address to start with T, got %q", address)
	}
	if _, err := tronaddress.Base58ToAddress(address); err != nil {
		t.Fatalf("expected valid TRON address, got error: %v", err)
	}
}

func TestDerivePrivateKeyUsesGethSecp256k1Curve(t *testing.T) {
	t.Parallel()

	svc := NewHDWalletService(false)
	seed := []byte("01234567890123456789012345678901")

	_, xpriv, err := svc.CreateAccountKeyFromSeed(seed, "TRC20", 0)
	if err != nil {
		t.Fatalf("CreateAccountKeyFromSeed(TRC20) failed: %v", err)
	}

	priv, err := svc.DerivePrivateKey(xpriv, 22)
	if err != nil {
		t.Fatalf("DerivePrivateKey failed: %v", err)
	}

	if priv.Curve != crypto.S256() {
		t.Fatalf("expected geth secp256k1 curve, got %T", priv.Curve)
	}
}
