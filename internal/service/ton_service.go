package service

import (
	"bytes"
	"context"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/jetton"
)

type tonTxClient interface {
	CurrentMasterchainInfo(ctx context.Context) (*ton.BlockIDExt, error)
	GetAccount(ctx context.Context, block *ton.BlockIDExt, addr *address.Address) (*tlb.Account, error)
	ListTransactions(ctx context.Context, addr *address.Address, limit uint32, lt uint64, txHash []byte) ([]*tlb.Transaction, error)
}

const (
	tonHistoryPageLimit = 20
	tonHistoryPageCap   = 10
)

type TonService struct {
	api         ton.APIClientWrapped
	txClient    tonTxClient
	pool        *liteclient.ConnectionPool
	depositRepo *repository.DepositRepository
	hdRepo      *repository.HdWalletRepository
	txManager   *TransactionManager
	isTestnet   bool
}

func tonConfigURL(isTestnet bool) string {
	configURL := strings.TrimSpace(os.Getenv("TON_CONFIG_URL"))
	if configURL != "" {
		return configURL
	}
	if isTestnet {
		return "https://ton-blockchain.github.io/testnet-global.config.json"
	}
	return "https://ton.org/global.config.json"
}

func newTONAPIClient(ctx context.Context, isTestnet bool) (ton.APIClientWrapped, *liteclient.ConnectionPool, error) {
	pool := liteclient.NewConnectionPool()
	pool.SetOnDisconnect(pool.DefaultReconnect(500*time.Millisecond, 5))

	if err := pool.AddConnectionsFromConfigUrl(ctx, tonConfigURL(isTestnet)); err != nil {
		return nil, nil, err
	}

	api := ton.NewAPIClient(pool, ton.ProofCheckPolicyFast).WithRetry(2).WithTimeout(5 * time.Second)
	return api, pool, nil
}

func NewTonService(ctx context.Context, depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *TransactionManager, isTestnet bool) (*TonService, error) {
	api, pool, err := newTONAPIClient(ctx, isTestnet)
	if err != nil {
		return nil, err
	}

	return &TonService{
		api:         api,
		txClient:    api,
		pool:        pool,
		depositRepo: depositRepo,
		hdRepo:      hdRepo,
		txManager:   txManager,
		isTestnet:   isTestnet,
	}, nil
}

func (s *TonService) StartPoller(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollTransactions(ctx)
		}
	}
}

func isRetriableTonLiteError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "lite server error") &&
		(strings.Contains(msg, "cannot load block") ||
			strings.Contains(msg, "block handle not in db") ||
			strings.Contains(msg, "is not in db") ||
			strings.Contains(msg, "failed to get account state"))
}

func tonRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 250 * time.Millisecond
	case 1:
		return 500 * time.Millisecond
	default:
		return time.Second
	}
}

func tonCallWithRetry[T any](ctx context.Context, operation string, fn func() (T, error)) (T, error) {
	var zero T
	const maxAttempts = 4

	for attempt := 0; attempt < maxAttempts; attempt++ {
		value, err := fn()
		if err == nil {
			return value, nil
		}
		if !isRetriableTonLiteError(err) || attempt == maxAttempts-1 {
			return zero, err
		}

		delay := tonRetryDelay(attempt)
		log.Printf("TON transient failure during %s; retrying in %s: %v", operation, delay, err)

		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	return zero, fmt.Errorf("ton retries exhausted for %s", operation)
}

func (s *TonService) pollTransactions(ctx context.Context) {
	deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, "TON")
	if err != nil {
		log.Printf("TON poll: failed to fetch pending deposits: %v", err)
		return
	}

	for _, dep := range deposits {
		wallet, err := s.hdRepo.FindByID(ctx, dep.HDWalletID)
		if err != nil || wallet == nil {
			continue
		}

		addr, err := address.ParseAddr(dep.DepositAddress)
		if err != nil {
			continue
		}

		if err := s.walkTransactions(ctx, addr, func(tx *tlb.Transaction) (bool, error) {
			txHashHex := hex.EncodeToString(tx.Hash)
			if dep.TxHash != nil && strings.EqualFold(*dep.TxHash, txHashHex) {
				return true, nil
			}

			recorded, receiptErr := s.depositRepo.HasIncomingTransfer(ctx, dep.ID, txHashHex)
			if receiptErr == nil && recorded {
				return true, nil
			}

			if tx.IO.In == nil || tx.IO.In.MsgType != tlb.MsgTypeInternal {
				return false, nil
			}

			ti := tx.IO.In.AsInternal()

			var amount decimal.Decimal
			if wallet.ContractAddress != nil && strings.TrimSpace(*wallet.ContractAddress) != "" {
				expectedJettonWallet, err := s.expectedJettonWallet(ctx, *wallet.ContractAddress, dep.DepositAddress)
				if err != nil {
					log.Printf("TON poll: failed to resolve expected jetton wallet for deposit %s: %v", dep.ID, err)
					return false, nil
				}

				if ti.Payload() == nil {
					return false, nil
				}

				payload := ti.Payload().BeginParse()
				op, _ := payload.LoadUInt(32)
				if op != 0x7362d09c {
					return false, nil
				}

				sender := ti.SenderAddr()
				if sender == nil || !sender.Equals(expectedJettonWallet) {
					log.Printf("Ignoring TON jetton notification for %s from unexpected sender %v (expected %s)", dep.DepositAddress, sender, expectedJettonWallet.String())
					if s.txManager != nil {
						s.txManager.RecordCounter(ctx, "ton_jetton_sender_mismatch", map[string]string{
							"coin": wallet.Coin,
						})
					}
					return false, nil
				}

				_, _ = payload.LoadUInt(64) // query id
				jettonAmount, err := payload.LoadCoins()
				if err != nil {
					return false, nil
				}

				amount = decimal.NewFromBigInt(new(big.Int).SetUint64(jettonAmount), int32(-wallet.Decimals))
				log.Printf("TON Jetton Deposit Detected: %s, amount: %s", dep.DepositAddress, amount)
			} else {
				amount = decimal.NewFromBigInt(ti.Amount.Nano(), int32(-9))
				log.Printf("TON Native Deposit Detected: %s, amount: %s", dep.DepositAddress, amount)
			}

			if isOperationalPrefundTx(ctx, s.txManager, dep.Network, dep.DepositAddress, txHashHex) {
				log.Printf("Ignoring operational TON prefund for %s: %s", dep.ID, txHashHex)
				return false, nil
			}

			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txHashHex, &amount); err != nil {
				log.Printf("Failed to persist TON deposit update for %s (tx: %s): %v", dep.ID, txHashHex, err)
			}
			return false, nil
		}); err != nil {
			log.Printf("TON poll: failed to walk history for %s: %v", dep.DepositAddress, err)
		}
	}

	s.reconcileConfirmations(ctx)
}

func (s *TonService) reconcileConfirmations(ctx context.Context) {
	deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, "TON")
	if err != nil {
		log.Printf("TON confirmation check: failed to fetch deposits: %v", err)
		return
	}

	for _, dep := range deposits {
		if !shouldTrackDepositTx(dep) {
			continue
		}

		resolution, err := s.CheckDepositTxResolution(ctx, dep)
		if err != nil {
			log.Printf("TON confirmation check: failed to reconcile deposit %s: %v", dep.ID, err)
			continue
		}
		if handleErr := s.txManager.HandleDepositTxResolution(ctx, "TON", dep, resolution); handleErr != nil {
			log.Printf("TON confirmation check: failed to handle resolution for deposit %s: %v", dep.ID, handleErr)
			continue
		}
		if resolution != depositTxResolutionConfirmed {
			continue
		}

		wallet, _ := s.hdRepo.FindByID(ctx, dep.HDWalletID)
		finalizationThreshold := dep.RequiredConfirmations
		if wallet != nil && wallet.FinalizationConfirmations > 0 {
			finalizationThreshold = wallet.FinalizationConfirmations
		}
		if finalizationThreshold < dep.RequiredConfirmations {
			finalizationThreshold = dep.RequiredConfirmations
		}
		if finalizationThreshold < 1 {
			finalizationThreshold = 1
		}

		switch dep.Status {
		case "DETECTED":
			confirmations := dep.RequiredConfirmations
			if confirmations < 1 {
				confirmations = 1
			}
			if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, confirmations); err != nil {
				log.Printf("Failed to persist TON confirmations for %s: %v", dep.ID, err)
			}
			log.Printf("TON Deposit Confirmed: %s", *dep.TxHash)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "CONFIRMED", nil, nil); err != nil {
				log.Printf("Failed to persist TON deposit confirmation for %s: %v", dep.ID, err)
			}
		case "CONFIRMED":
			if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, finalizationThreshold); err != nil {
				log.Printf("Failed to persist TON finalization confirmations for %s: %v", dep.ID, err)
			}
			log.Printf("TON Deposit Finalized: %s", *dep.TxHash)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "FINALIZED", nil, nil); err != nil {
				log.Printf("Failed to persist TON deposit finalization for %s: %v", dep.ID, err)
			}
		case "FINALIZED":
			if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, finalizationThreshold); err != nil {
				log.Printf("Failed to persist TON steady-state confirmations for %s: %v", dep.ID, err)
			}
		}
	}
}

func (s *TonService) txLookupClient() tonTxClient {
	if s != nil && s.txClient != nil {
		return s.txClient
	}
	if s != nil {
		return s.api
	}
	return nil
}

func (s *TonService) walkTransactions(ctx context.Context, addr *address.Address, handler func(*tlb.Transaction) (bool, error)) error {
	if addr == nil {
		return fmt.Errorf("ton address is required")
	}

	client := s.txLookupClient()
	if client == nil {
		return fmt.Errorf("ton transaction client is not configured")
	}

	requestCtx := ctx
	if s != nil && s.api != nil {
		requestCtx = s.api.Client().StickyContext(ctx)
	}

	master, err := tonCallWithRetry(requestCtx, "CurrentMasterchainInfo", func() (*ton.BlockIDExt, error) {
		return client.CurrentMasterchainInfo(requestCtx)
	})
	if err != nil {
		return fmt.Errorf("failed to get TON masterchain info: %w", err)
	}

	acc, err := tonCallWithRetry(requestCtx, "GetAccount", func() (*tlb.Account, error) {
		return client.GetAccount(requestCtx, master, addr)
	})
	if err != nil {
		return fmt.Errorf("failed to get TON account state: %w", err)
	}
	if acc == nil || acc.LastTxHash == nil || acc.LastTxLT == 0 {
		return nil
	}

	lastHash := acc.LastTxHash
	lastLT := acc.LastTxLT

	for page := 0; page < tonHistoryPageCap; page++ {
		if lastLT == 0 || len(lastHash) == 0 {
			return nil
		}

		txs, err := tonCallWithRetry(requestCtx, "ListTransactions", func() ([]*tlb.Transaction, error) {
			return client.ListTransactions(requestCtx, addr, tonHistoryPageLimit, lastLT, lastHash)
		})
		if err != nil {
			return fmt.Errorf("failed to list TON transactions: %w", err)
		}
		if len(txs) == 0 {
			return nil
		}

		oldest := txs[0]
		sort.SliceStable(txs, func(i, j int) bool {
			return txs[i].LT > txs[j].LT
		})

		for _, tx := range txs {
			stop, err := handler(tx)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}

		if oldest.PrevTxLT == 0 || len(oldest.PrevTxHash) == 0 {
			return nil
		}
		if oldest.PrevTxLT == lastLT && bytes.Equal(oldest.PrevTxHash, lastHash) {
			return nil
		}

		lastLT = oldest.PrevTxLT
		lastHash = oldest.PrevTxHash
	}

	if s.txManager != nil {
		s.txManager.RecordCounter(ctx, "ton_history_page_cap_reached", map[string]string{
			"network": "TON",
		})
	}

	return nil
}

func normalizeTonTxHash(txHash string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(txHash)), "0x")
}

func tonTxStateFromTransaction(tx *tlb.Transaction) sweepTxCheckResult {
	if tx == nil {
		return sweepTxCheckResult{State: sweepTxStateNotFound}
	}

	switch desc := tx.Description.(type) {
	case tlb.TransactionDescriptionOrdinary:
		return tonLifecycleTxState(desc.Aborted, desc.ComputePhase, desc.ActionPhase, desc.BouncePhase)
	case tlb.TransactionDescriptionTickTock:
		return tonLifecycleTxState(desc.Aborted, desc.ComputePhase, desc.ActionPhase, nil)
	case tlb.TransactionDescriptionSplitPrepare:
		return tonLifecycleTxState(desc.Aborted, desc.ComputePhase, desc.ActionPhase, nil)
	case tlb.TransactionDescriptionMergeInstall:
		return tonLifecycleTxState(desc.Aborted, desc.ComputePhase, desc.ActionPhase, nil)
	case tlb.TransactionDescriptionMergePrepare:
		if desc.Aborted {
			return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "transaction aborted"}
		}
		return sweepTxCheckResult{State: sweepTxStateConfirmed}
	case tlb.TransactionDescriptionStorage, tlb.TransactionDescriptionSplitInstall:
		return sweepTxCheckResult{State: sweepTxStateConfirmed}
	default:
		return sweepTxCheckResult{State: sweepTxStatePending, Reason: "unsupported TON transaction description"}
	}
}

func tonLifecycleTxState(aborted bool, compute tlb.ComputePhase, action *tlb.ActionPhase, bounce *tlb.BouncePhase) sweepTxCheckResult {
	if aborted {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "transaction aborted"}
	}
	if bounce != nil {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: tonBounceReason(bounce)}
	}

	if failed, reason := tonComputePhaseFailureReason(compute); failed {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: reason}
	}
	if failed, reason := tonActionPhaseFailureReason(action); failed {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: reason}
	}

	return sweepTxCheckResult{State: sweepTxStateConfirmed}
}

func tonComputePhaseFailureReason(compute tlb.ComputePhase) (bool, string) {
	switch phase := compute.Phase.(type) {
	case tlb.ComputePhaseVM:
		if !phase.Success || phase.Details.ExitCode != 0 {
			return true, fmt.Sprintf("compute phase failed with exit code %d", phase.Details.ExitCode)
		}
	case tlb.ComputePhaseSkipped:
		return true, "compute phase skipped: " + string(phase.Reason.Type)
	}
	return false, ""
}

func tonActionPhaseFailureReason(action *tlb.ActionPhase) (bool, string) {
	if action == nil {
		return false, ""
	}
	if action.NoFunds {
		return true, "action phase reported no funds"
	}
	if !action.Valid {
		return true, "action phase invalid"
	}
	if !action.Success || action.ResultCode != 0 {
		return true, fmt.Sprintf("action phase failed with result code %d", action.ResultCode)
	}
	return false, ""
}

func tonBounceReason(bounce *tlb.BouncePhase) string {
	if bounce == nil {
		return "transaction bounced"
	}

	switch bounce.Phase.(type) {
	case tlb.BouncePhaseNoFunds:
		return "transaction bounced due to no funds"
	case tlb.BouncePhaseNegFunds:
		return "transaction bounced due to negative funds"
	default:
		return "transaction bounced"
	}
}

func (s *TonService) CheckAddressTxState(ctx context.Context, addr, txHash string) (sweepTxCheckResult, error) {
	parsedAddr, err := address.ParseAddr(addr)
	if err != nil {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "invalid TON address"}, fmt.Errorf("invalid TON address: %w", err)
	}

	targetHash := normalizeTonTxHash(txHash)
	if targetHash == "" {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "empty TON tx hash"}, nil
	}

	result := sweepTxCheckResult{State: sweepTxStateNotFound}
	err = s.walkTransactions(ctx, parsedAddr, func(tx *tlb.Transaction) (bool, error) {
		if normalizeTonTxHash(hex.EncodeToString(tx.Hash)) != targetHash {
			return false, nil
		}
		result = tonTxStateFromTransaction(tx)
		return true, nil
	})
	if err != nil {
		return sweepTxCheckResult{}, err
	}

	return result, nil
}

func (s *TonService) CheckDepositTxConfirmation(ctx context.Context, dep *model.CryptoDeposit) (bool, error) {
	resolution, err := s.CheckDepositTxResolution(ctx, dep)
	if err != nil {
		return false, err
	}
	return resolution == depositTxResolutionConfirmed, nil
}

func (s *TonService) CheckDepositTxResolution(ctx context.Context, dep *model.CryptoDeposit) (depositTxResolution, error) {
	if dep == nil || dep.TxHash == nil || *dep.TxHash == "" {
		return depositTxResolutionError, nil
	}

	status, err := s.CheckAddressTxState(ctx, dep.DepositAddress, *dep.TxHash)
	if err != nil {
		return depositTxResolutionError, err
	}

	switch status.State {
	case sweepTxStateConfirmed:
		return depositTxResolutionConfirmed, nil
	case sweepTxStateNotFound:
		return depositTxResolutionNotFound, nil
	case sweepTxStatePending:
		return depositTxResolutionPending, nil
	default:
		return depositTxResolutionError, nil
	}
}

func (s *TonService) CheckTxConfirmation(ctx context.Context, txHash string) (bool, error) {
	dep, err := s.depositRepo.FindByTxHash(ctx, txHash)
	if err != nil || dep == nil {
		return false, nil
	}

	return s.CheckDepositTxConfirmation(ctx, dep)
}

func (s *TonService) expectedJettonWallet(ctx context.Context, masterContractAddr string, ownerAddr string) (*address.Address, error) {
	masterAddr, err := address.ParseAddr(masterContractAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid jetton master address: %w", err)
	}
	owner, err := address.ParseAddr(ownerAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid owner address: %w", err)
	}

	jettonClient := jetton.NewJettonMasterClient(s.api, masterAddr)
	jettonWallet, err := tonCallWithRetry(ctx, "GetJettonWallet", func() (*jetton.WalletClient, error) {
		return jettonClient.GetJettonWallet(ctx, owner)
	})
	if err != nil {
		return nil, err
	}

	return jettonWallet.Address(), nil
}
