package service

import (
	"errors"
	"fmt"
	"strings"

	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/common"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/gagliardetto/solana-go"
	tonaddress "github.com/xssnick/tonutils-go/address"
)

func normalizeAddressValidationNetwork(network string) string {
	normalized := strings.ToUpper(strings.TrimSpace(network))
	switch normalized {
	case "ETHEREUM":
		return "ERC20"
	case "BSC":
		return "BEP20"
	case "POL":
		return "POLYGON"
	case "TRON":
		return "TRC20"
	default:
		return normalized
	}
}

var ErrIncompletePayoutSettings = errors.New("address, network, and token must all be provided together")

type PayoutSettings struct {
	Address string
	Network string
	Token   string
}

func ValidateChainAddress(network, value string, btcParams *chaincfg.Params) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("address is required")
	}

	switch normalizeAddressValidationNetwork(network) {
	case "BTC":
		if btcParams == nil {
			return fmt.Errorf("BTC address validation is unavailable")
		}
		if _, err := btcutil.DecodeAddress(value, btcParams); err != nil {
			return fmt.Errorf("invalid BTC address: %w", err)
		}
	case "TRC20":
		if _, err := tronaddress.Base58ToAddress(value); err != nil {
			return fmt.Errorf("invalid TRON address: %w", err)
		}
	case "SOLANA":
		if _, err := solana.PublicKeyFromBase58(value); err != nil {
			return fmt.Errorf("invalid Solana address: %w", err)
		}
	case "TON":
		if _, err := tonaddress.ParseAddr(value); err != nil {
			return fmt.Errorf("invalid TON address: %w", err)
		}
	default:
		if !common.IsHexAddress(value) {
			return fmt.Errorf("invalid EVM address")
		}
	}

	return nil
}

func ValidateGasWalletAddress(chainType, value string, btcParams *chaincfg.Params) error {
	normalizedChainType := repository.NormalizeGasWalletChainType(chainType)
	address := strings.TrimSpace(value)

	if normalizedChainType == "BTC" && address == "" {
		return nil
	}
	if address == "" {
		return fmt.Errorf("wallet address is required")
	}

	return ValidateChainAddress(normalizedChainType, address, btcParams)
}

func ValidatePayoutSettings(address, network, token string, btcParams *chaincfg.Params) (PayoutSettings, error) {
	settings := PayoutSettings{
		Address: strings.TrimSpace(address),
		Network: repository.NormalizeAssetNetwork(network),
		Token:   repository.NormalizeAssetCoin(token),
	}

	filledFields := 0
	if settings.Address != "" {
		filledFields++
	}
	if settings.Network != "" {
		filledFields++
	}
	if settings.Token != "" {
		filledFields++
	}

	if filledFields == 0 {
		return PayoutSettings{}, nil
	}
	if filledFields != 3 {
		return PayoutSettings{}, ErrIncompletePayoutSettings
	}

	if err := ValidateChainAddress(settings.Network, settings.Address, btcParams); err != nil {
		return PayoutSettings{}, fmt.Errorf("invalid payout address: %w", err)
	}

	return settings, nil
}

func ValidateWalletAddresses(wallet *model.HDWallet, btcParams *chaincfg.Params) error {
	if wallet == nil {
		return fmt.Errorf("wallet is required")
	}

	if err := ValidateChainAddress(wallet.Network, wallet.HotWalletAddress, btcParams); err != nil {
		return fmt.Errorf("invalid hot wallet address: %w", err)
	}

	if wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == "" {
		return nil
	}

	if err := ValidateChainAddress(wallet.Network, *wallet.ContractAddress, btcParams); err != nil {
		return fmt.Errorf("invalid contract address: %w", err)
	}

	return nil
}
