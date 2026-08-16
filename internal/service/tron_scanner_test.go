package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestAggregateTRC20TransfersForDeposit(t *testing.T) {
	t.Parallel()

	transfers := []TRC20Transfer{
		{
			TransactionID: "tx-1",
			To:            "TRecipient",
			Value:         "1500000",
			TokenInfo: struct {
				Address  string `json:"address"`
				Decimals int    `json:"decimals"`
			}{
				Address:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
				Decimals: 6,
			},
		},
		{
			TransactionID: "tx-1",
			To:            "TRecipient",
			Value:         "2250000",
			TokenInfo: struct {
				Address  string `json:"address"`
				Decimals int    `json:"decimals"`
			}{
				Address:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
				Decimals: 6,
			},
		},
		{
			TransactionID: "tx-2",
			To:            "TRecipient",
			Value:         "1000000",
			TokenInfo: struct {
				Address  string `json:"address"`
				Decimals int    `json:"decimals"`
			}{
				Address:  "OTHERCONTRACT",
				Decimals: 6,
			},
		},
		{
			TransactionID: "tx-3",
			To:            "OtherRecipient",
			Value:         "1000000",
			TokenInfo: struct {
				Address  string `json:"address"`
				Decimals int    `json:"decimals"`
			}{
				Address:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
				Decimals: 6,
			},
		},
		{
			TransactionID: "tx-4",
			To:            "TRecipient",
			Value:         "not-a-number",
			TokenInfo: struct {
				Address  string `json:"address"`
				Decimals int    `json:"decimals"`
			}{
				Address:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
				Decimals: 6,
			},
		},
	}

	aggregated := aggregateTRC20TransfersForDeposit(transfers, "TRecipient", "tr7nhqjekqxgtci8q8zy4pl8otszgjlj6t", 6)
	if len(aggregated) != 1 {
		t.Fatalf("expected 1 aggregated transfer, got %d", len(aggregated))
	}

	got, ok := aggregated["tx-1"]
	if !ok {
		t.Fatal("expected tx-1 aggregate to exist")
	}

	wantAmount := decimal.RequireFromString("3.75")
	if !got.Amount.Equal(wantAmount) {
		t.Fatalf("aggregate amount = %s, want %s", got.Amount, wantAmount)
	}
}

func TestShouldProcessAggregatedTRC20Transfer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		recordedAmount   string
		aggregatedAmount string
		exists           bool
		want             bool
	}{
		{
			name:             "missing record is processed",
			recordedAmount:   "0",
			aggregatedAmount: "1",
			exists:           false,
			want:             true,
		},
		{
			name:             "larger aggregate is processed",
			recordedAmount:   "1.5",
			aggregatedAmount: "2.25",
			exists:           true,
			want:             true,
		},
		{
			name:             "equal aggregate is skipped",
			recordedAmount:   "2.25",
			aggregatedAmount: "2.25",
			exists:           true,
			want:             false,
		},
		{
			name:             "smaller aggregate is skipped",
			recordedAmount:   "3",
			aggregatedAmount: "2.25",
			exists:           true,
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldProcessAggregatedTRC20Transfer(
				decimal.RequireFromString(tt.recordedAmount),
				tt.exists,
				decimal.RequireFromString(tt.aggregatedAmount),
			)
			if got != tt.want {
				t.Fatalf("shouldProcessAggregatedTRC20Transfer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTronGridTRC20TransactionsEndpointPreservesContractCase(t *testing.T) {
	t.Parallel()

	endpoint := tronGridTRC20TransactionsEndpoint(
		"TRecipient",
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"",
	)

	if want := "contract_address=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"; !strings.Contains(endpoint, want) {
		t.Fatalf("endpoint %q does not contain %q", endpoint, want)
	}
}
