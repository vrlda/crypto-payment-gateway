package service

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

var lockReleaseScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("DEL", KEYS[1])
	end
	return 0
`)

var lockRenewScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("PEXPIRE", KEYS[1], ARGV[2])
	end
	return 0
`)

type TransactionManager struct {
	rdb          *redis.Client
	webhook      *WebhookService
	depositRepo  *repository.DepositRepository
	merchantRepo *repository.MerchantRepository
	txRepo       *repository.TransactionRepository
	sweepRepo    *repository.SweepRepository
	hdRepo       *repository.HdWalletRepository
	ops          *OperationsService
	poolRepo     *repository.TRC20PoolRepository
	sweepEnsurer interface {
		EnsureSweepExists(context.Context, *model.CryptoDeposit) (bool, error)
	}
}

func NewTransactionManager(webhook *WebhookService, depRepo *repository.DepositRepository, merchRepo *repository.MerchantRepository, txRepo *repository.TransactionRepository, sweepRepo *repository.SweepRepository, hdRepo *repository.HdWalletRepository, ops *OperationsService, poolRepo *repository.TRC20PoolRepository) *TransactionManager {
	return &TransactionManager{
		rdb:          database.Rdb,
		webhook:      webhook,
		depositRepo:  depRepo,
		merchantRepo: merchRepo,
		txRepo:       txRepo,
		sweepRepo:    sweepRepo,
		hdRepo:       hdRepo,
		ops:          ops,
		poolRepo:     poolRepo,
	}
}

func (tm *TransactionManager) SetSweepEnsurer(sweepEnsurer interface {
	EnsureSweepExists(context.Context, *model.CryptoDeposit) (bool, error)
}) {
	if tm == nil {
		return
	}
	tm.sweepEnsurer = sweepEnsurer
}

// HandleDepositUpdate updates the deposit status and dispatches payment webhooks.
func (tm *TransactionManager) HandleDepositUpdate(ctx context.Context, depositID string, status string, txHash *string, amount *decimal.Decimal) error {
	depBefore, err := tm.depositRepo.FindByID(ctx, depositID)
	if err != nil || depBefore == nil {
		return fmt.Errorf("deposit not found before update: %w", err)
	}

	paymentID := ""
	if depBefore.PaymentID != nil {
		paymentID = *depBefore.PaymentID
	}
	// Always serialize concurrent updates to the same deposit. When a paymentID exists we lock
	// on it (covers all deposits under that payment). When there is no payment, lock on the
	// deposit ID directly — scanner goroutines can otherwise race and produce duplicate sweeps.
	lockKey := "deposit_update:" + depositID
	if paymentID != "" {
		lockKey = "payment_update:" + paymentID
	}
	var lockToken string
	{
		var (
			acquired bool
			lockErr  error
		)
		for attempt := 0; attempt < 5; attempt++ {
			lockToken, acquired, lockErr = tm.AcquireLock(ctx, lockKey, 2*time.Minute)
			if lockErr != nil {
				return lockErr
			}
			if acquired {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !acquired {
			return nil
		}

		// Renew the lock every 30 seconds so slow RPC paths (webhook dispatch,
		// EnsureSweepExists live-balance fetch) don't race past the TTL.
		stopRenewal := make(chan struct{})
		defer close(stopRenewal)
		defer tm.ReleaseLock(ctx, lockKey, lockToken)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopRenewal:
					return
				case <-ticker.C:
					if renewed, err := tm.RenewLock(ctx, lockKey, lockToken, 2*time.Minute); err != nil || !renewed {
						log.Printf("HandleDepositUpdate: failed to renew lock for %s: %v", lockKey, err)
					}
				}
			}
		}()

		depBefore, err = tm.depositRepo.FindByID(ctx, depositID)
		if err != nil || depBefore == nil {
			return fmt.Errorf("deposit not found before locked update: %w", err)
		}
	}

	var txBefore *repository.PaymentTransaction
	if depBefore.PaymentID != nil && *depBefore.PaymentID != "" {
		txBefore, err = tm.txRepo.FindByID(ctx, *depBefore.PaymentID)
		if err != nil {
			return err
		}
	}
	selectedBefore := paymentMethodMatchesDeposit(txBefore, depBefore)

	statusToStore := status
	amountToStore := amount
	shouldDispatchWebhook := true
	meetsExpected := true

	if status == "DETECTED" {
		aggregateAmount, changed, validationErr := tm.recordDetectedTransfer(ctx, depBefore, txHash, amount)
		if validationErr != nil {
			return validationErr
		}
		if !changed {
			return nil
		}

		amountToStore = &aggregateAmount
		meetsExpected = tm.amountMeetsExpectedThreshold(ctx, depBefore, aggregateAmount)

		if depBefore.Status != "PENDING" {
			statusToStore = depBefore.Status
			shouldDispatchWebhook = false
		} else if !meetsExpected && selectedBefore {
			statusToStore = "PENDING"
			shouldDispatchWebhook = false
			log.Printf(
				"Underpaid selected deposit held pending for %s: expected %s %s, got %s",
				depositID,
				depBefore.AmountExpected.Decimal,
				depBefore.Coin,
				aggregateAmount,
			)
		} else if !meetsExpected {
			statusToStore = "PENDING"
			shouldDispatchWebhook = false
			tm.RecordCounter(ctx, "underpaid_inactive_sibling_detected", map[string]string{
				"coin":    depBefore.Coin,
				"network": depBefore.Network,
			})
			log.Printf(
				"Underpaid inactive sibling held pending for %s: expected %s %s, got %s",
				depositID,
				depBefore.AmountExpected.Decimal,
				depBefore.Coin,
				aggregateAmount,
			)
		}
	}

	if (status == "CONFIRMED" || status == "FINALIZED") && !tm.currentDepositMeetsExpectation(ctx, depBefore) {
		log.Printf(
			"Blocking %s for underpaid deposit %s: expected %s %s, detected %s",
			status,
			depositID,
			depBefore.AmountExpected.Decimal,
			depBefore.Coin,
			depBefore.DetectedAmount.Decimal,
		)
		return nil
	}

	isLate := txBefore != nil && (txBefore.Status == "EXPIRED" || txBefore.Status == "EXPIRED_PAID")
	if isLate && !depBefore.IsLate {
		if err := tm.depositRepo.MarkLate(ctx, depositID, true); err != nil {
			return err
		}
	}

	if err := tm.depositRepo.UpdateStatus(ctx, depositID, statusToStore, txHash, amountToStore); err != nil {
		return err
	}

	// Release the TRC20 pool address back to available when the deposit reaches a terminal state.
	// FINALIZED means funds confirmed; EXPIRED/CANCELLED means payment abandoned.
	if tm.poolRepo != nil {
		switch statusToStore {
		case "FINALIZED", "EXPIRED", "CANCELLED", "FAILED":
			if releaseErr := tm.poolRepo.ReleaseByDepositID(ctx, depositID); releaseErr != nil {
				log.Printf("Failed to release TRC20 pool address for deposit %s: %v", depositID, releaseErr)
			}
		}
	}

	dep, err := tm.depositRepo.FindByID(ctx, depositID)
	if err != nil || dep == nil {
		return fmt.Errorf("deposit not found for webhook: %w", err)
	}

	bindPayment := shouldBindPaymentToDeposit(txBefore, depBefore, statusToStore, meetsExpected)
	detachSiblings := shouldDetachPendingSiblings(txBefore, depBefore, statusToStore, meetsExpected)

	if detachSiblings && dep.PaymentID != nil && *dep.PaymentID != "" && depositHasOnChainActivity(dep) {
		if _, err := tm.depositRepo.DetachPendingSiblingsFromPayment(ctx, *dep.PaymentID, dep.ID); err != nil {
			return err
		}
	}

	if status == "DETECTED" && dep.Status == "FINALIZED" && tm.sweepEnsurer != nil {
		if _, err := tm.sweepEnsurer.EnsureSweepExists(ctx, dep); err != nil {
			log.Printf("Failed to ensure residual sweep for finalized deposit %s: %v", dep.ID, err)
		}
	}

	if txBefore != nil && (bindPayment || selectedBefore) {
		switch {
		case dep.AmountExpected.Valid:
			expectedAmount := dep.AmountExpected.Decimal.String()
			if txBefore.Currency != dep.Coin || txBefore.ChainType != dep.Network || txBefore.AmountExpected != expectedAmount {
				if err := tm.txRepo.UpdatePaymentMethodAndAmount(ctx, txBefore.ID, dep.Network, dep.Coin, expectedAmount); err != nil {
					return err
				}
				txBefore.Currency = dep.Coin
				txBefore.ChainType = dep.Network
				txBefore.AmountExpected = expectedAmount
			}
		case txBefore.Currency != dep.Coin || txBefore.ChainType != dep.Network:
			if err := tm.txRepo.UpdatePaymentMethod(ctx, txBefore.ID, dep.Network, dep.Coin); err != nil {
				return err
			}
			txBefore.Currency = dep.Coin
			txBefore.ChainType = dep.Network
		}
	}

	var txStatus string
	if txBefore != nil && (bindPayment || selectedBefore) {
		txStatus = statusToStore
		if isLate {
			txStatus = "EXPIRED_PAID"
		}
		if err := tm.txRepo.UpdateStatus(ctx, txBefore.ID, txStatus, txHash); err != nil {
			return err
		}
	}

	if isLate && !depBefore.IsLate && txBefore != nil {
		tm.RecordCounter(ctx, "late_payment_detected", map[string]string{
			"coin":    dep.Coin,
			"network": dep.Network,
		})

		message := fmt.Sprintf(
			"Payment %s for merchant %s moved to EXPIRED_PAID after a late on-chain receipt.\nDeposit: %s\nAsset: %s/%s\nAmount: %s\nTx hash: %s",
			txBefore.ID,
			txBefore.MerchantID,
			dep.ID,
			dep.Coin,
			dep.Network,
			dep.DetectedAmount.Decimal.String(),
			firstNonEmptyPtr(dep.TxHash),
		)
		if err := tm.EmitAlert(ctx, "expired-paid:"+txBefore.ID, "Late payment received", message, "warning", 30*24*time.Hour); err != nil {
			log.Printf("Failed to notify admins about late payment %s: %v", txBefore.ID, err)
		}
	}

	if !shouldDispatchWebhook {
		return nil
	}
	if dep.PaymentID == nil || *dep.PaymentID == "" {
		return nil
	}

	merchantID := ""
	if dep.UserID != nil {
		merchantID = *dep.UserID
	}
	if merchantID == "" && txBefore != nil {
		merchantID = txBefore.MerchantID
	}
	if merchantID == "" {
		log.Printf("Warning: no merchant linked to deposit %s", depositID)
		return nil
	}

	resolvedPaymentID := dep.ID
	if dep.PaymentID != nil && *dep.PaymentID != "" {
		resolvedPaymentID = *dep.PaymentID
	}

	eventNames := paymentWebhookEventNames(statusToStore, isLate)
	if len(eventNames) == 0 {
		return nil
	}

	payload := WebhookPayload{
		Data: map[string]interface{}{
			"payment_id":      resolvedPaymentID,
			"deposit_id":      dep.ID,
			"status":          firstNonEmpty(txStatus, statusToStore),
			"deposit_status":  statusToStore,
			"amount":          dep.DetectedAmount,
			"amount_expected": dep.AmountExpected,
			"amount_received": dep.DetectedAmount,
			"currency":        dep.Coin,
			"network":         dep.Network,
			"tx_hash":         dep.TxHash,
			"is_late":         dep.IsLate,
		},
		Timestamp: time.Now().Unix(),
	}
	if txBefore != nil && txBefore.ExternalRefID != nil && *txBefore.ExternalRefID != "" {
		payload.Data.(map[string]interface{})["external_id"] = *txBefore.ExternalRefID
	}

	for _, eventName := range eventNames {
		eventPayload := payload
		eventPayload.Event = eventName
		go tm.webhook.Dispatch(context.Background(), merchantID, eventPayload)
	}
	return nil
}

func (tm *TransactionManager) ExpirePendingTransactions(ctx context.Context) error {
	txs, err := tm.txRepo.ListExpiredPending(ctx, time.Now())
	if err != nil {
		return err
	}

	for _, tx := range txs {
		if err := tm.txRepo.UpdateStatus(ctx, tx.ID, "EXPIRED", nil); err != nil {
			return err
		}

		// Release any TRC20 pool address held by the PENDING deposit for this payment.
		if tm.poolRepo != nil && tx.ChainType == "TRC20" && tx.Currency == "USDT" {
			if dep, depErr := tm.depositRepo.FindByPaymentID(ctx, tx.ID); depErr == nil && dep != nil && dep.Status == "PENDING" {
				if releaseErr := tm.poolRepo.ReleaseByDepositID(ctx, dep.ID); releaseErr != nil {
					log.Printf("Failed to release TRC20 pool address for expired payment %s deposit %s: %v", tx.ID, dep.ID, releaseErr)
				}
			}
		}

		payload := WebhookPayload{
			Event: "payment.expired",
			Data: map[string]interface{}{
				"payment_id":      tx.ID,
				"status":          "EXPIRED",
				"currency":        tx.Currency,
				"network":         tx.ChainType,
				"amount_expected": tx.AmountExpected,
				"amount_base":     tx.AmountBase,
				"fee_client":      tx.FeeClient,
				"expires_at":      tx.ExpiresAt,
			},
			Timestamp: time.Now().Unix(),
		}
		if tx.ExternalRefID != nil && *tx.ExternalRefID != "" {
			payload.Data.(map[string]interface{})["external_id"] = *tx.ExternalRefID
		}
		go tm.webhook.Dispatch(context.Background(), tx.MerchantID, payload)
	}

	return nil
}

func (tm *TransactionManager) recordDetectedTransfer(ctx context.Context, dep *model.CryptoDeposit, txHash *string, amount *decimal.Decimal) (decimal.Decimal, bool, error) {
	if dep == nil {
		return decimal.Zero, false, fmt.Errorf("deposit is required")
	}
	if txHash == nil || *txHash == "" {
		return decimal.Zero, false, fmt.Errorf("tx hash is required for detected deposits")
	}
	if amount == nil {
		return decimal.Zero, false, fmt.Errorf("detected amount is required")
	}

	changed, err := tm.depositRepo.UpsertIncomingTransferAmountMax(ctx, dep.ID, *txHash, *amount)
	if err != nil {
		return decimal.Zero, false, err
	}
	if !changed {
		currentTotal := dep.DetectedAmount.Decimal
		if !dep.DetectedAmount.Valid {
			currentTotal = decimal.Zero
		}
		return currentTotal, false, nil
	}

	if err := tm.depositRepo.TouchInboundActivity(ctx, dep.ID, *txHash, time.Now().UTC()); err != nil {
		return decimal.Zero, false, err
	}

	total, err := tm.depositRepo.SumIncomingTransfers(ctx, dep.ID)
	if err != nil {
		return decimal.Zero, false, err
	}

	return total, true, nil
}

func (tm *TransactionManager) currentDepositMeetsExpectation(ctx context.Context, dep *model.CryptoDeposit) bool {
	if dep == nil || !dep.AmountExpected.Valid {
		return true
	}
	if tm.merchantAcceptsAnyPaymentAmount(ctx, dep) {
		return true
	}
	if !dep.DetectedAmount.Valid {
		return false
	}

	return tm.amountMeetsExpectedThreshold(ctx, dep, dep.DetectedAmount.Decimal)
}

func depositHasOnChainActivity(dep *model.CryptoDeposit) bool {
	if dep == nil {
		return false
	}
	if dep.Status != "PENDING" {
		return true
	}
	if dep.TxHash != nil && strings.TrimSpace(*dep.TxHash) != "" {
		return true
	}
	if dep.DetectedAmount.Valid && dep.DetectedAmount.Decimal.GreaterThan(decimal.Zero) {
		return true
	}
	if dep.Confirmations > 0 {
		return true
	}
	return false
}

func (tm *TransactionManager) amountMeetsExpectedThreshold(ctx context.Context, dep *model.CryptoDeposit, detected decimal.Decimal) bool {
	if dep == nil || !dep.AmountExpected.Valid {
		return true
	}
	if tm.merchantAcceptsAnyPaymentAmount(ctx, dep) {
		return true
	}

	tolerance := decimal.Zero
	if tm.hdRepo != nil {
		wallet, err := tm.hdRepo.FindByID(ctx, dep.HDWalletID)
		if err == nil && wallet != nil {
			tolerance = wallet.AmountTolerancePercent
		}
	}

	minExpected := minimumAcceptedAmount(dep.AmountExpected.Decimal, tolerance)
	return detectedMeetsThreshold(detected, minExpected)
}

func (tm *TransactionManager) merchantAcceptsAnyPaymentAmount(ctx context.Context, dep *model.CryptoDeposit) bool {
	if tm == nil || tm.merchantRepo == nil || dep == nil || dep.UserID == nil || strings.TrimSpace(*dep.UserID) == "" {
		return false
	}

	merchant, err := tm.merchantRepo.FindByID(ctx, strings.TrimSpace(*dep.UserID))
	return err == nil && merchant != nil && merchant.AcceptAnyPaymentAmount
}

func minimumAcceptedAmount(expected decimal.Decimal, tolerancePercent decimal.Decimal) decimal.Decimal {
	if !expected.IsPositive() {
		return expected
	}

	if tolerancePercent.Sign() < 0 {
		tolerancePercent = decimal.Zero
	}
	if tolerancePercent.GreaterThan(decimal.NewFromInt(100)) {
		tolerancePercent = decimal.NewFromInt(100)
	}

	remainingPercent := decimal.NewFromInt(100).Sub(tolerancePercent)
	return expected.Mul(remainingPercent).Div(decimal.NewFromInt(100))
}

func detectedMeetsThreshold(detected, threshold decimal.Decimal) bool {
	return detected.Cmp(threshold) >= 0
}

// NotifyPaymentCreated triggers payment.created webhook.
func (tm *TransactionManager) NotifyPaymentCreated(ctx context.Context, merchantID string, data interface{}) {
	payload := WebhookPayload{
		Event:     "payment.created",
		Data:      data,
		Timestamp: time.Now().Unix(),
	}
	go tm.webhook.Dispatch(context.Background(), merchantID, payload)
}

func (tm *TransactionManager) RecordCounter(ctx context.Context, name string, labels map[string]string) {
	if tm == nil || tm.ops == nil {
		return
	}
	tm.ops.IncrementCounter(ctx, name, labels)
}

func (tm *TransactionManager) EmitAlert(ctx context.Context, dedupeKey, title, message, notificationType string, ttl time.Duration) error {
	if tm == nil || tm.ops == nil {
		return nil
	}
	return tm.ops.EmitAlert(ctx, dedupeKey, title, message, notificationType, ttl)
}

func (tm *TransactionManager) AuditWalletConfiguration(ctx context.Context) error {
	if tm == nil || tm.ops == nil {
		return nil
	}
	return tm.ops.AuditWalletConfiguration(ctx)
}

// AcquireLock acquires a distributed lock and returns its ownership token.
func (tm *TransactionManager) AcquireLock(ctx context.Context, resourceId string, ttl time.Duration) (string, bool, error) {
	lockKey := fmt.Sprintf("lock:%s", resourceId)
	token := uuid.NewString()

	success, err := tm.rdb.SetNX(ctx, lockKey, token, ttl).Result()
	if err != nil {
		return "", false, err
	}

	return token, success, nil
}

func (tm *TransactionManager) RenewLock(ctx context.Context, resourceId, token string, ttl time.Duration) (bool, error) {
	lockKey := fmt.Sprintf("lock:%s", resourceId)
	result, err := lockRenewScript.Run(ctx, tm.rdb, []string{lockKey}, token, strconv.FormatInt(ttl.Milliseconds(), 10)).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

// ReleaseLock releases the lock only if the caller still owns it.
func (tm *TransactionManager) ReleaseLock(ctx context.Context, resourceId, token string) error {
	lockKey := fmt.Sprintf("lock:%s", resourceId)
	result, err := lockReleaseScript.Run(ctx, tm.rdb, []string{lockKey}, token).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		log.Printf("lock_release_mismatch resource=%s", resourceId)
		tm.RecordCounter(ctx, "lock_release_mismatch", map[string]string{
			"scope": lockScope(resourceId),
		})
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyPtr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func lockScope(resourceID string) string {
	if resourceID == "" {
		return "unknown"
	}
	if idx := strings.Index(resourceID, ":"); idx > 0 {
		return resourceID[:idx]
	}
	return resourceID
}

func paymentWebhookEventNames(status string, isLate bool) []string {
	if isLate {
		switch status {
		case "DETECTED":
			return []string{"payment.late_detected"}
		case "FINALIZED":
			return []string{"payment.late_finalized", "payment.completed"}
		default:
			return nil
		}
	}

	eventName := fmt.Sprintf("payment.%s", strings.ToLower(status))
	if status == "FINALIZED" {
		return []string{eventName, "payment.completed"}
	}

	return []string{eventName}
}
