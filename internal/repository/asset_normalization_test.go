package repository

import "testing"

func TestNormalizeAssetCoin(t *testing.T) {
	t.Parallel()

	if got := NormalizeAssetCoin(" usdt "); got != "USDT" {
		t.Fatalf("NormalizeAssetCoin() = %q, want USDT", got)
	}
}

func TestNormalizeAssetNetwork(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		" ethereum ": "ERC20",
		"bsc":        "BEP20",
		"polygon":    "POLYGON",
		"arb":        "ARBITRUM",
		"tron":       "TRC20",
		"bitcoin":    "BTC",
	}

	for input, want := range tests {
		if got := NormalizeAssetNetwork(input); got != want {
			t.Fatalf("NormalizeAssetNetwork(%q) = %q, want %q", input, got, want)
		}
	}
}
