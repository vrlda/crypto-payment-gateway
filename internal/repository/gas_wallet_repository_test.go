package repository

import "testing"

func TestNormalizeGasWalletChainType(t *testing.T) {
	tests := map[string]string{
		"EVM":      "EVM",
		"erc20":    "EVM",
		"BEP20":    "EVM",
		"Polygon":  "EVM",
		"Arbitrum": "EVM",
		"TRON":     "TRON",
		"trc20":    "TRON",
		"BTC":      "BTC",
		"bitcoin":  "BTC",
		"SOLANA":   "SOLANA",
		"Ton":      "TON",
	}

	for input, expected := range tests {
		if actual := NormalizeGasWalletChainType(input); actual != expected {
			t.Fatalf("NormalizeGasWalletChainType(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestGasWalletChainTypeAliases(t *testing.T) {
	aliases := gasWalletChainTypeAliases("ERC20")
	expected := map[string]bool{
		"EVM":      true,
		"ERC20":    true,
		"BEP20":    true,
		"POLYGON":  true,
		"ARBITRUM": true,
	}

	for _, alias := range aliases {
		delete(expected, alias)
	}

	if len(expected) != 0 {
		t.Fatalf("gasWalletChainTypeAliases did not include aliases: %#v", expected)
	}
}
