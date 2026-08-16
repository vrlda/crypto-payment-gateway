package service

import (
	"testing"

	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
)

func TestPaymentMethodMatchesDeposit(t *testing.T) {
	t.Parallel()

	tx := &repository.PaymentTransaction{
		Currency:  "eth",
		ChainType: "erc20",
	}
	dep := &model.CryptoDeposit{
		Coin:    "ETH",
		Network: "ERC20",
	}

	if !paymentMethodMatchesDeposit(tx, dep) {
		t.Fatal("expected payment method to match deposit")
	}
}

func TestDepositBindingPolicies(t *testing.T) {
	t.Parallel()

	paymentID := "pay-1"
	selectedTx := &repository.PaymentTransaction{
		Currency:  "BTC",
		ChainType: "BTC",
	}
	selectedDep := &model.CryptoDeposit{
		Coin:      "BTC",
		Network:   "BTC",
		Status:    "PENDING",
		PaymentID: &paymentID,
	}
	inactiveSibling := &model.CryptoDeposit{
		Coin:      "ETH",
		Network:   "ERC20",
		Status:    "PENDING",
		PaymentID: &paymentID,
	}

	if shouldBindPaymentToDeposit(selectedTx, inactiveSibling, "PENDING", false) {
		t.Fatal("expected underpaid inactive sibling not to seize invoice ownership")
	}
	if shouldDetachPendingSiblings(selectedTx, inactiveSibling, "PENDING", false) {
		t.Fatal("expected underpaid inactive sibling not to detach pending siblings")
	}

	if shouldBindPaymentToDeposit(selectedTx, selectedDep, "PENDING", false) {
		t.Fatal("expected selected underpaid deposit not to require rebinding")
	}
	if !shouldDetachPendingSiblings(selectedTx, selectedDep, "PENDING", false) {
		t.Fatal("expected selected underpaid deposit to detach pending siblings")
	}

	if !shouldBindPaymentToDeposit(selectedTx, inactiveSibling, "DETECTED", true) {
		t.Fatal("expected qualifying inactive sibling to bind payment to deposit")
	}
	if !shouldDetachPendingSiblings(selectedTx, inactiveSibling, "DETECTED", true) {
		t.Fatal("expected qualifying inactive sibling to detach pending siblings")
	}
}

func TestShouldTrackDepositTx(t *testing.T) {
	t.Parallel()

	txHash := "0xabc"
	dep := &model.CryptoDeposit{
		Status:      "FINALIZED",
		WatchStatus: model.DepositWatchStatusActive,
		TxHash:      &txHash,
	}

	if !shouldTrackDepositTx(dep) {
		t.Fatal("expected finalized active-watch deposit to keep tx tracking enabled")
	}
}
