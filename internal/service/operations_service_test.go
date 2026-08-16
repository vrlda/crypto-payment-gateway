package service

import (
	"crypto_payment_gateway_core/internal/model"
	"testing"
)

func TestWalletNeedsContractAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		coin    string
		network string
		want    bool
	}{
		{name: "native trx does not require contract", coin: "TRX", network: "TRC20", want: false},
		{name: "trc20 stablecoin requires contract", coin: "USDT", network: "TRC20", want: true},
		{name: "native sol does not require contract", coin: "SOL", network: "SOLANA", want: false},
		{name: "spl stablecoin requires contract", coin: "USDC", network: "SOLANA", want: true},
		{name: "native eth does not require contract", coin: "ETH", network: "ERC20", want: false},
		{name: "erc20 stablecoin requires contract", coin: "USDT", network: "ERC20", want: true},
		{name: "btc never requires contract", coin: "BTC", network: "BTC", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := walletNeedsContractAddress(tt.coin, tt.network); got != tt.want {
				t.Fatalf("walletNeedsContractAddress(%q, %q) = %v, want %v", tt.coin, tt.network, got, tt.want)
			}
		})
	}
}

func TestWalletRootReuseFamilies(t *testing.T) {
	t.Parallel()

	wallets := []model.HDWallet{
		{Coin: "ETH", Network: "ERC20"},
		{Coin: "MATIC", Network: "POLYGON"},
		{Coin: "BNB", Network: "BEP20"},
		{Coin: "ETH", Network: "ARBITRUM"},
	}

	got := walletRootReuseFamilies(wallets)
	if len(got) != 1 || got[0] != "EVM" {
		t.Fatalf("walletRootReuseFamilies() = %v, want [EVM]", got)
	}
}

func TestWalletGroupHasDuplicateSelectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wallets []model.HDWallet
		want    bool
	}{
		{
			name: "shared evm root across distinct networks is allowed",
			wallets: []model.HDWallet{
				{Coin: "ETH", Network: "ERC20"},
				{Coin: "MATIC", Network: "POLYGON"},
				{Coin: "ETH", Network: "ARBITRUM"},
				{Coin: "BNB", Network: "BEP20"},
			},
			want: false,
		},
		{
			name: "duplicate same asset and network is flagged",
			wallets: []model.HDWallet{
				{Coin: "ETH", Network: "ERC20"},
				{Coin: "ETH", Network: "Ethereum"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := walletGroupHasDuplicateSelectors(tt.wallets); got != tt.want {
				t.Fatalf("walletGroupHasDuplicateSelectors() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWalletRootReuseAlertRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wallets []model.HDWallet
		want    bool
	}{
		{
			name: "shared evm root does not alert",
			wallets: []model.HDWallet{
				{Coin: "ETH", Network: "ERC20"},
				{Coin: "MATIC", Network: "POLYGON"},
			},
			want: false,
		},
		{
			name: "shared tron root across native and token does not alert",
			wallets: []model.HDWallet{
				{Coin: "TRX", Network: "TRC20"},
				{Coin: "USDT", Network: "TRC20"},
			},
			want: false,
		},
		{
			name: "cross family reuse alerts",
			wallets: []model.HDWallet{
				{Coin: "ETH", Network: "ERC20"},
				{Coin: "BTC", Network: "BTC"},
			},
			want: true,
		},
		{
			name: "duplicate selectors alert even in same family",
			wallets: []model.HDWallet{
				{Coin: "ETH", Network: "ERC20"},
				{Coin: "ETH", Network: "ERC20"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			families := walletRootReuseFamilies(tt.wallets)
			got := walletRootReuseShouldAlert(tt.wallets)
			if got != tt.want {
				t.Fatalf("alert rule = %v, want %v (families=%v)", got, tt.want, families)
			}
		})
	}
}

func TestWalletSweepabilityIssue(t *testing.T) {
	t.Parallel()

	contract := "0xcontract"
	seed := "encrypted-seed"

	tests := []struct {
		name   string
		wallet model.HDWallet
		want   string
	}{
		{
			name: "enabled token wallet without contract is unsweepable",
			wallet: model.HDWallet{
				Coin:             "USDT",
				Network:          "ERC20",
				IsEnabled:        true,
				HotWalletAddress: "0x1",
				MasterPublicKey:  "xpub",
			},
			want: "contract address is missing",
		},
		{
			name: "enabled watch-only btc wallet is unsweepable",
			wallet: model.HDWallet{
				Coin:             "BTC",
				Network:          "BTC",
				IsEnabled:        true,
				HotWalletAddress: "bc1qtest",
				MasterPublicKey:  "xpub",
			},
			want: "encrypted spend key is missing",
		},
		{
			name: "disabled wallet is ignored",
			wallet: model.HDWallet{
				Coin:             "BTC",
				Network:          "BTC",
				IsEnabled:        false,
				HotWalletAddress: "bc1qtest",
			},
			want: "",
		},
		{
			name: "fully configured wallet has no issue",
			wallet: model.HDWallet{
				Coin:                "USDT",
				Network:             "TRC20",
				IsEnabled:           true,
				HotWalletAddress:    "TAddress",
				MasterPublicKey:     "xpub",
				EncryptedMasterSeed: &seed,
				ContractAddress:     &contract,
			},
			want: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := walletSweepabilityIssue(&tt.wallet); got != tt.want {
				t.Fatalf("walletSweepabilityIssue() = %q, want %q", got, tt.want)
			}
		})
	}
}
