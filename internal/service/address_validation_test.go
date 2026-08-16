package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

func TestValidatePayoutSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		address string
		network string
		token   string
		want    PayoutSettings
		wantErr error
	}{
		{
			name: "all blank clears settings",
			want: PayoutSettings{},
		},
		{
			name:    "partial settings are rejected",
			address: "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
			network: "ERC20",
			wantErr: ErrIncompletePayoutSettings,
		},
		{
			name:    "valid evm settings are normalized",
			address: "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
			network: "ethereum",
			token:   " usdt ",
			want: PayoutSettings{
				Address: "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
				Network: "ERC20",
				Token:   "USDT",
			},
		},
		{
			name:    "network mismatch is rejected",
			address: "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
			network: "BTC",
			token:   "BTC",
			wantErr: errors.New("invalid payout address"),
		},
		{
			name:    "valid btc settings use btc params",
			address: "1BoatSLRHtKNngkdXEeobR76b53LETtpyT",
			network: "BTC",
			token:   "btc",
			want: PayoutSettings{
				Address: "1BoatSLRHtKNngkdXEeobR76b53LETtpyT",
				Network: "BTC",
				Token:   "BTC",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ValidatePayoutSettings(tt.address, tt.network, tt.token, &chaincfg.MainNetParams)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("ValidatePayoutSettings() error = nil, want %v", tt.wantErr)
				}
				if errors.Is(tt.wantErr, ErrIncompletePayoutSettings) {
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("ValidatePayoutSettings() error = %v, want %v", err, tt.wantErr)
					}
					return
				}
				if got := err.Error(); !strings.HasPrefix(got, "invalid payout address") {
					t.Fatalf("ValidatePayoutSettings() error = %q, want prefix %q", got, "invalid payout address")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePayoutSettings() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ValidatePayoutSettings() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateGasWalletAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chainType string
		address   string
		wantErr   bool
	}{
		{
			name:      "btc allows blank address",
			chainType: "BTC",
			address:   "",
		},
		{
			name:      "valid tron address passes",
			chainType: "TRON",
			address:   "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		},
		{
			name:      "legacy broken tron address is rejected",
			chainType: "TRON",
			address:   "1TBEezm1PnPCdXGin4KJ4w8jWvFjqtMaL2K",
			wantErr:   true,
		},
		{
			name:      "non-btc blank address is rejected",
			chainType: "TRON",
			address:   "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateGasWalletAddress(tt.chainType, tt.address, &chaincfg.MainNetParams)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateGasWalletAddress() error = %v", err)
			}
		})
	}
}
