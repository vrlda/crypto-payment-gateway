package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
)

type depositTxResolution string

const (
	depositTxResolutionPending   depositTxResolution = "PENDING"
	depositTxResolutionConfirmed depositTxResolution = "CONFIRMED"
	depositTxResolutionNotFound  depositTxResolution = "NOT_FOUND"
	depositTxResolutionError     depositTxResolution = "ERROR"
)

const (
	depositTxMissingThreshold = 3
	depositTxMissingTTL       = 24 * time.Hour
)

func shouldTrackDepositTx(dep *model.CryptoDeposit) bool {
	if dep == nil || dep.TxHash == nil || strings.TrimSpace(*dep.TxHash) == "" {
		return false
	}
	if dep.WatchStatus != "" && dep.WatchStatus != model.DepositWatchStatusActive {
		return false
	}

	switch dep.Status {
	case "DETECTED", "CONFIRMED", "FINALIZED":
		return true
	default:
		return false
	}
}

func paymentMethodMatchesDeposit(tx *repository.PaymentTransaction, dep *model.CryptoDeposit) bool {
	if tx == nil || dep == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tx.Currency), strings.TrimSpace(dep.Coin)) &&
		strings.EqualFold(strings.TrimSpace(tx.ChainType), strings.TrimSpace(dep.Network))
}

func shouldBindPaymentToDeposit(txBefore *repository.PaymentTransaction, depBefore *model.CryptoDeposit, statusToStore string, meetsExpected bool) bool {
	if txBefore == nil || depBefore == nil {
		return false
	}
	if paymentMethodMatchesDeposit(txBefore, depBefore) {
		return false
	}
	if statusToStore == "PENDING" && depBefore.Status == "PENDING" && !meetsExpected {
		return false
	}
	return true
}

func shouldDetachPendingSiblings(txBefore *repository.PaymentTransaction, depBefore *model.CryptoDeposit, statusToStore string, meetsExpected bool) bool {
	if txBefore == nil || depBefore == nil || depBefore.PaymentID == nil || *depBefore.PaymentID == "" {
		return false
	}
	if statusToStore == "PENDING" && depBefore.Status == "PENDING" && !meetsExpected {
		return paymentMethodMatchesDeposit(txBefore, depBefore)
	}
	return true
}

func normalizeDepositTxWatchNetwork(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "TRON":
		return "TRC20"
	case "ETHEREUM":
		return "ERC20"
	case "BSC":
		return "BEP20"
	case "POL":
		return "POLYGON"
	case "ARB":
		return "ARBITRUM"
	default:
		return strings.ToUpper(strings.TrimSpace(network))
	}
}

func normalizeDepositTxWatchHash(network, txHash string) string {
	normalized := strings.TrimSpace(txHash)
	switch normalizeDepositTxWatchNetwork(network) {
	case "SOLANA", "BTC":
		return normalized
	default:
		return strings.ToLower(normalized)
	}
}

func depositTxMissingKey(network, depositID, txHash string) string {
	return fmt.Sprintf(
		"deposit_tx_missing:%s:%s:%s",
		normalizeDepositTxWatchNetwork(network),
		strings.TrimSpace(depositID),
		normalizeDepositTxWatchHash(network, txHash),
	)
}

func (tm *TransactionManager) clearMissingDepositTxCounter(ctx context.Context, network, depositID, txHash string) {
	if tm == nil || tm.rdb == nil || strings.TrimSpace(depositID) == "" || strings.TrimSpace(txHash) == "" {
		return
	}
	_ = tm.rdb.Del(ctx, depositTxMissingKey(network, depositID, txHash)).Err()
}

func (tm *TransactionManager) recordMissingDepositTx(ctx context.Context, network, depositID, txHash string) (bool, int64, error) {
	if tm == nil || tm.rdb == nil || strings.TrimSpace(depositID) == "" || strings.TrimSpace(txHash) == "" {
		return false, 0, nil
	}

	key := depositTxMissingKey(network, depositID, txHash)
	count, err := tm.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, 0, err
	}
	if err := tm.rdb.Expire(ctx, key, depositTxMissingTTL).Err(); err != nil {
		return false, count, err
	}

	tm.RecordCounter(ctx, "deposit_tx_missing", map[string]string{
		"network": normalizeDepositTxWatchNetwork(network),
	})

	return count >= depositTxMissingThreshold, count, nil
}

func (tm *TransactionManager) HandleDepositTxResolution(ctx context.Context, network string, dep *model.CryptoDeposit, resolution depositTxResolution) error {
	if tm == nil || dep == nil || dep.TxHash == nil || strings.TrimSpace(*dep.TxHash) == "" {
		return nil
	}

	switch resolution {
	case depositTxResolutionPending, depositTxResolutionConfirmed:
		tm.clearMissingDepositTxCounter(ctx, network, dep.ID, *dep.TxHash)
		return nil
	case depositTxResolutionNotFound:
		shouldQuarantine, count, err := tm.recordMissingDepositTx(ctx, network, dep.ID, *dep.TxHash)
		if err != nil {
			return err
		}
		if !shouldQuarantine {
			log.Printf("Deposit tx temporarily missing for %s on %s (%d/%d)", dep.ID, dep.Network, count, depositTxMissingThreshold)
			return nil
		}
		tm.clearMissingDepositTxCounter(ctx, network, dep.ID, *dep.TxHash)
		return tm.QuarantineMissingDepositTx(ctx, dep.ID, *dep.TxHash)
	default:
		return nil
	}
}

func (tm *TransactionManager) QuarantineMissingDepositTx(ctx context.Context, depositID, txHash string) error {
	if tm == nil || tm.depositRepo == nil || tm.txRepo == nil || strings.TrimSpace(depositID) == "" || strings.TrimSpace(txHash) == "" {
		return nil
	}

	dep, err := tm.depositRepo.FindByID(ctx, depositID)
	if err != nil {
		return err
	}
	if dep == nil {
		return nil
	}

	paymentID := ""
	if dep.PaymentID != nil {
		paymentID = strings.TrimSpace(*dep.PaymentID)
	}

	if paymentID != "" {
		lockKey := "payment_update:" + paymentID
		lockToken, acquired, err := tm.AcquireLock(ctx, lockKey, 15*time.Second)
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		defer tm.ReleaseLock(ctx, lockKey, lockToken)

		dep, err = tm.depositRepo.FindByID(ctx, depositID)
		if err != nil {
			return err
		}
		if dep == nil {
			return nil
		}
	}

	txHashMatched := dep.TxHash != nil && strings.EqualFold(strings.TrimSpace(*dep.TxHash), strings.TrimSpace(txHash))

	if _, err := tm.depositRepo.DeleteIncomingTransfer(ctx, dep.ID, txHash); err != nil {
		return err
	}

	recomputedTotal, err := tm.depositRepo.SumIncomingTransfers(ctx, dep.ID)
	if err != nil {
		return err
	}

	if err := tm.depositRepo.ResetOnchainState(ctx, dep.ID, recomputedTotal, txHashMatched); err != nil {
		return err
	}

	var txBefore *repository.PaymentTransaction
	if paymentID != "" {
		txBefore, err = tm.txRepo.FindByID(ctx, paymentID)
		if err != nil {
			return err
		}
	}

	if txBefore != nil {
		nextStatus := "PENDING"
		if txBefore.ExpiresAt != nil && !txBefore.ExpiresAt.After(time.Now().UTC()) {
			nextStatus = "EXPIRED"
		}

		nextTxHash := txBefore.TxHash
		if txBefore.TxHash != nil && strings.EqualFold(strings.TrimSpace(*txBefore.TxHash), strings.TrimSpace(txHash)) {
			nextTxHash = nil
		}

		if err := tm.txRepo.ClearOrUpdateStatus(ctx, txBefore.ID, nextStatus, nextTxHash); err != nil {
			return err
		}
	}

	if tm.sweepRepo != nil {
		activeSweeps, err := tm.sweepRepo.ListActiveDepositFundSweepsByDepositID(ctx, dep.ID)
		if err != nil {
			return err
		}
		errMsg := "source_deposit_tx_disappeared"
		for _, sw := range activeSweeps {
			if updateErr := tm.sweepRepo.UpdateStatus(ctx, sw.ID, model.SweepStatusFailed, nil, &errMsg); updateErr != nil {
				return updateErr
			}
		}

		hasConfirmedSweep, err := tm.sweepRepo.HasConfirmedDepositFundSweepByDepositID(ctx, dep.ID)
		if err != nil {
			return err
		}
		if hasConfirmedSweep {
			message := fmt.Sprintf(
				"Deposit %s on %s was quarantined because tx %s disappeared, but a deposit-fund sweep is already CONFIRMED.\nPayment: %s\nDetected total after quarantine: %s",
				dep.ID,
				dep.Network,
				txHash,
				paymentID,
				recomputedTotal.String(),
			)
			if err := tm.EmitAlert(ctx, "deposit-quarantine-confirmed-sweep:"+dep.ID, "Quarantined deposit already swept", message, "error", 24*time.Hour); err != nil {
				log.Printf("Failed to notify admins about quarantined swept deposit %s: %v", dep.ID, err)
			}
		}
	}

	tm.RecordCounter(ctx, "deposit_tx_quarantined", map[string]string{
		"network": normalizeDepositTxWatchNetwork(dep.Network),
	})

	message := fmt.Sprintf(
		"Deposit %s on %s was quarantined because tracked tx %s disappeared during active watch.\nPayment: %s\nRecomputed amount: %s\nPrevious deposit status: %s",
		dep.ID,
		dep.Network,
		txHash,
		paymentID,
		recomputedTotal.String(),
		dep.Status,
	)
	if err := tm.EmitAlert(ctx, "deposit-tx-quarantined:"+dep.ID+":"+normalizeDepositTxWatchHash(dep.Network, txHash), "Deposit tx disappeared", message, "warning", 12*time.Hour); err != nil {
		log.Printf("Failed to notify admins about quarantined deposit %s: %v", dep.ID, err)
	}

	return nil
}
