package service

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha512"
	"errors"
	"fmt"

	pkg_crypto "crypto_payment_gateway_core/pkg/crypto"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gagliardetto/solana-go"
	tonwallet "github.com/xssnick/tonutils-go/ton/wallet"
)

type HDWalletService struct {
	Params *chaincfg.Params
}

func NewHDWalletService(isTestnet bool) *HDWalletService {
	params := &chaincfg.MainNetParams
	if isTestnet {
		params = &chaincfg.TestNet3Params
	}
	return &HDWalletService{
		Params: params,
	}
}

func (s *HDWalletService) GenerateAddress(chainType string, xpub string, index uint32) (string, error) {
	switch chainType {
	case "SOLANA":
		return s.generateSolanaAddress(xpub, index)
	case "TON":
		return s.generateTonAddress(xpub, index)
	case "BTC":
		return s.generateBTCAddress(xpub, index)
	case "TRC20":
		return s.generateTronAddress(xpub, index)
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM":
		return s.generateEVMAddress(xpub, index)
	default:
		return "", fmt.Errorf("unsupported chain type: %s", chainType)
	}
}

// GenerateAddressWithSeed is the secure method for Ed25519 chains that require hardened derivation
func (s *HDWalletService) GenerateAddressWithSeed(chainType string, xpub string, masterSeedEnc string, account uint32, index uint32) (string, error) {
	// For arrays/slices, Go passes by value (copy of header), but underlying is ref.
	// We decrypt only when needed.
	switch chainType {
	case "SOLANA":
		seed, err := s.decryptAndDeriveSeed(masterSeedEnc, chainType, account, index)
		if err != nil {
			return "", err
		}
		return s.generateSolanaAddressFromSeed(seed)
	case "TON":
		seed, err := s.decryptAndDeriveSeed(masterSeedEnc, chainType, account, index)
		if err != nil {
			return "", err
		}
		return s.generateTonAddressFromSeed(seed)
	default:
		// Fallback for chains that support xpub derivation (BTC, EVM)
		return s.GenerateAddress(chainType, xpub, index)
	}
}

func (s *HDWalletService) decryptAndDeriveSeed(encSeed, chain string, account uint32, index uint32) ([]byte, error) {
	if encSeed == "" {
		return nil, errors.New("encrypted master seed required for hardened derivation")
	}
	// Decrypt
	masterSeedHex, err := pkg_crypto.DecryptWithEnv(encSeed, "APP_SECRET")
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt master seed: %w", err)
	}

	// Derive SLIP-0010 / BIP-44 equivalent seed
	// For Ed25519 (Solana, TON), we use a robust HMAC-SHA512 derivation from the decrypted master seed.
	// This ensures deterministic, hardened-equivalent child keys without leaking the master seed.
	return s.deriveHardenedSeed(masterSeedHex, s.ed25519Namespace(chain, account, index)), nil
}

func (s *HDWalletService) deriveHardenedSeed(masterSeedHex string, namespace string) []byte {
	h := hmac.New(sha512.New, []byte(masterSeedHex))
	h.Write([]byte(namespace))
	digest := h.Sum(nil)
	return digest[:32]
}

func (s *HDWalletService) ed25519Namespace(chain string, account uint32, index uint32) string {
	switch chain {
	case "TON":
		return fmt.Sprintf("TON:TON:TON:%d:%d", account, index)
	default:
		return fmt.Sprintf("SOLANA:SOL:SOLANA:%d:%d", account, index)
	}
}

// generateEVMAddress handles ERC20, BEP20, POLYGON, ARBITRUM
func (s *HDWalletService) generateEVMAddress(xpub string, index uint32) (string, error) {
	key, err := s.deriveChildKey(xpub, index)
	if err != nil {
		return "", err
	}

	pubKey, err := key.ECPubKey()
	if err != nil {
		return "", err
	}

	address := crypto.PubkeyToAddress(*pubKey.ToECDSA())
	return address.Hex(), nil
}

func (s *HDWalletService) generateTronAddress(xpub string, index uint32) (string, error) {
	key, err := s.deriveChildKey(xpub, index)
	if err != nil {
		return "", err
	}

	pubKey, err := key.ECPubKey()
	if err != nil {
		return "", err
	}

	return TronAddressFromECDSAPublicKey(*pubKey.ToECDSA()), nil
}

func (s *HDWalletService) generateBTCAddress(xpub string, index uint32) (string, error) {
	key, err := s.deriveChildKey(xpub, index)
	if err != nil {
		return "", err
	}

	// Segwit address (P2WPKH) using BIP-84 style or standard xpub
	addr, err := key.Address(s.Params)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func (s *HDWalletService) deriveChildKey(xpub string, index uint32) (*hdkeychain.ExtendedKey, error) {
	key, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, fmt.Errorf("invalid xpub: %w", err)
	}

	// Derive External Chain (0)
	externalChain, err := key.Derive(0)
	if err != nil {
		return nil, fmt.Errorf("failed to derive external chain: %w", err)
	}

	// Derive Index
	childKey, err := externalChain.Derive(index)
	if err != nil {
		return nil, fmt.Errorf("failed to derive child key at index %d: %w", index, err)
	}
	return childKey, nil
}

// GenerateMasterKey generates a new random master seed and returns xpub/xpriv
func (s *HDWalletService) GenerateMasterKey() (string, string, error) {
	seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
	if err != nil {
		return "", "", err
	}
	return s.CreateMasterKeyFromSeed(seed)
}

// CreateMasterKeyFromSeed creates a master key from a provided seed (e.g. from mnemonic)
func (s *HDWalletService) CreateMasterKeyFromSeed(seed []byte) (string, string, error) {
	key, err := hdkeychain.NewMaster(seed, s.Params)
	if err != nil {
		return "", "", err
	}

	xpriv := key.String()

	// Neuter to get public key
	pubKey, err := key.Neuter()
	if err != nil {
		return "", "", err
	}

	xpub := pubKey.String()

	return xpub, xpriv, nil
}

func (s *HDWalletService) CreateAccountKeyFromSeed(seed []byte, chainType string, account uint32) (string, string, error) {
	key, err := hdkeychain.NewMaster(seed, s.Params)
	if err != nil {
		return "", "", err
	}

	path, err := s.accountDerivationPath(chainType, account)
	if err != nil {
		return "", "", err
	}
	for _, child := range path {
		key, err = key.Derive(child)
		if err != nil {
			return "", "", err
		}
	}

	xpriv := key.String()
	pubKey, err := key.Neuter()
	if err != nil {
		return "", "", err
	}

	return pubKey.String(), xpriv, nil
}

func (s *HDWalletService) accountDerivationPath(chainType string, account uint32) ([]uint32, error) {
	hardened := func(index uint32) uint32 {
		return index + hdkeychain.HardenedKeyStart
	}

	switch chainType {
	case "BTC":
		return []uint32{hardened(84), hardened(0), hardened(account)}, nil
	case "TRC20", "TRON":
		return []uint32{hardened(44), hardened(195), hardened(account)}, nil
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "EVM":
		return []uint32{hardened(44), hardened(60), hardened(account)}, nil
	default:
		return nil, fmt.Errorf("unsupported chain type for account derivation: %s", chainType)
	}
}

func (s *HDWalletService) generateSolanaAddress(xpub string, index uint32) (string, error) {
	// For Ed25519, xpub is not sufficient for child derivation without chain code.
	// We MUST use the seed-based derivation to maintain security and consistency.
	return "", errors.New("unsafe derivation: use GenerateAddressWithSeed for Solana")
}

func (s *HDWalletService) generateSolanaAddressFromSeed(seed []byte) (string, error) {
	privKey := ed25519.NewKeyFromSeed(seed)
	pubKey := privKey.Public().(ed25519.PublicKey)

	solanaPub := solana.PublicKeyFromBytes(pubKey)
	return solanaPub.String(), nil
}

func (s *HDWalletService) generateTonAddress(xpub string, index uint32) (string, error) {
	return "", errors.New("unsafe derivation: use GenerateAddressWithSeed for TON")
}

func (s *HDWalletService) generateTonAddressFromSeed(seed []byte) (string, error) {
	privKey := ed25519.NewKeyFromSeed(seed)
	pubKey := privKey.Public().(ed25519.PublicKey)

	addr, err := tonwallet.AddressFromPubKey(pubKey, tonwallet.V4R2, tonwallet.DefaultSubwallet, 0)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// DerivePrivateKey derives the private key for a specific index for EVM signing
func (s *HDWalletService) DerivePrivateKey(xpriv string, index uint32) (*ecdsa.PrivateKey, error) {
	key, err := hdkeychain.NewKeyFromString(xpriv)
	if err != nil {
		return nil, fmt.Errorf("invalid xpriv: %w", err)
	}

	if !key.IsPrivate() {
		return nil, errors.New("cannot derive private key from public key")
	}

	extKey, err := key.Derive(0)
	if err != nil {
		return nil, err
	}

	childKey, err := extKey.Derive(index)
	if err != nil {
		return nil, err
	}

	privKey, err := childKey.ECPrivKey()
	if err != nil {
		return nil, err
	}

	// Normalize onto geth's secp256k1 curve type so signers that require
	// pointer-equality with crypto.S256() accept derived keys.
	normalized, err := crypto.ToECDSA(privKey.Serialize())
	if err != nil {
		return nil, err
	}

	return normalized, nil
}

// deriveEd25519Seed generates a deterministic 32-byte seed for Ed25519 chains.
func (s *HDWalletService) deriveEd25519Seed(masterSeed string, chain string, account uint32, index uint32) []byte {
	return s.deriveHardenedSeed(masterSeed, s.ed25519Namespace(chain, account, index))
}

func (s *HDWalletService) DeriveSolanaPrivateKey(masterSeed string, account uint32, index uint32) (ed25519.PrivateKey, error) {
	seed := s.deriveEd25519Seed(masterSeed, "SOLANA", account, index)
	return ed25519.NewKeyFromSeed(seed), nil
}

func (s *HDWalletService) DeriveTonPrivateKey(masterSeed string, account uint32, index uint32) (ed25519.PrivateKey, error) {
	seed := s.deriveEd25519Seed(masterSeed, "TON", account, index)
	return ed25519.NewKeyFromSeed(seed), nil
}
