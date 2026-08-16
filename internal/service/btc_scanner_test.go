package service

import (
	"testing"

	"crypto_payment_gateway_core/internal/model"

	"github.com/shopspring/decimal"
)

func TestAccumulateDetectedBTCPaymentAggregatesSameTransactionOutputs(t *testing.T) {
	t.Parallel()

	deposit := &model.CryptoDeposit{ID: "dep-1"}
	matches := make(map[string]*btcDetectedPayment)

	accumulateDetectedBTCPayment(matches, deposit, decimal.RequireFromString("0.001"))
	accumulateDetectedBTCPayment(matches, deposit, decimal.RequireFromString("0.0025"))

	match := matches[deposit.ID]
	if match == nil {
		t.Fatal("expected aggregated BTC payment match")
	}

	want := decimal.RequireFromString("0.0035")
	if !match.amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, match.amount)
	}
}
