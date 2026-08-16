package service

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
)

type SolanaService struct {
	client      *rpc.Client
	depositRepo *repository.DepositRepository
	hdRepo      *repository.HdWalletRepository
	txManager   *TransactionManager
	isTestnet   bool
}

func solanaSignatureIsConfirmed(status *rpc.SignatureStatusesResult) bool {
	if status == nil {
		return false
	}
	switch status.ConfirmationStatus {
	case rpc.ConfirmationStatusConfirmed, rpc.ConfirmationStatusFinalized:
		return true
	default:
		return false
	}
}

func solanaSignatureIsFinalized(status *rpc.SignatureStatusesResult) bool {
	return status != nil && status.ConfirmationStatus == rpc.ConfirmationStatusFinalized
}

func NewSolanaService(rpcURL string, depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *TransactionManager, isTestnet bool) *SolanaService {
	if rpcURL == "" {
		if isTestnet {
			rpcURL = rpc.DevNet_RPC
		} else {
			rpcURL = rpc.MainNetBeta_RPC
		}
	}
	return &SolanaService{
		client:      rpc.New(rpcURL),
		depositRepo: depositRepo,
		hdRepo:      hdRepo,
		txManager:   txManager,
		isTestnet:   isTestnet,
	}
}

func solanaDepositAcceptsNativeTransfer(wallet *model.HDWallet) bool {
	return wallet == nil || wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == ""
}

func (s *SolanaService) StartPoller(ctx context.Context) {
	if err := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, "SOLANA"); err != nil {
		log.Printf("Failed to rebuild SOLANA watched-address cache on startup: %v", err)
	}

	ticker := time.NewTicker(10 * time.Second)
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

func (s *SolanaService) pollTransactions(ctx context.Context) {
	// 1. Batch Confirmation Check for DETECTED deposits
	watched, _ := s.depositRepo.FindWatchedByNetwork(ctx, "SOLANA")

	detectedDeposits := make([]*model.CryptoDeposit, 0)
	sigsToCheck := make([]solana.Signature, 0)

	for _, dep := range watched {
		if (dep.Status == "DETECTED" || dep.Status == "CONFIRMED") && dep.TxHash != nil && *dep.TxHash != "" {
			sig, err := solana.SignatureFromBase58(*dep.TxHash)
			if err == nil {
				detectedDeposits = append(detectedDeposits, dep)
				sigsToCheck = append(sigsToCheck, sig)
			}
		}
	}

	if len(sigsToCheck) > 0 {
		// Batch call for all signatures at once
		statuses, err := s.client.GetSignatureStatuses(ctx, false, sigsToCheck...)
		if err == nil && statuses != nil {
			for i, status := range statuses.Value {
				if !solanaSignatureIsConfirmed(status) {
					continue
				}

				dep := detectedDeposits[i]
				wallet, _ := s.hdRepo.FindByID(ctx, dep.HDWalletID)
				finalizationThreshold := dep.RequiredConfirmations
				if wallet != nil && wallet.FinalizationConfirmations > 0 {
					finalizationThreshold = wallet.FinalizationConfirmations
				}

				switch dep.Status {
				case "DETECTED":
					if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, dep.RequiredConfirmations); err != nil {
						log.Printf("Failed to persist SOLANA confirmations for %s: %v", dep.ID, err)
					}
					log.Printf("Solana Deposit Confirmed: %s", *dep.TxHash)
					if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "CONFIRMED", nil, nil); err != nil {
						log.Printf("Failed to persist SOLANA deposit confirmation for %s: %v", dep.ID, err)
					}
				case "CONFIRMED":
					if !solanaSignatureIsFinalized(status) {
						continue
					}
					if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, finalizationThreshold); err != nil {
						log.Printf("Failed to persist SOLANA finalization confirmations for %s: %v", dep.ID, err)
					}
					log.Printf("Solana Deposit Finalized: %s", *dep.TxHash)
					if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "FINALIZED", nil, nil); err != nil {
						log.Printf("Failed to persist SOLANA deposit finalization for %s: %v", dep.ID, err)
					}
				}
			}
		}
	}

	// 2. Optimized Detection (Balance-First Strategy)
	addresses := make([]string, 0, len(watched))
	seen := make(map[string]struct{}, len(watched))
	for _, dep := range watched {
		if dep == nil {
			continue
		}
		cacheWatchedDeposit(ctx, "SOLANA", dep)
		addr := dep.PaymentAddress
		if addr == "" {
			addr = dep.DepositAddress
		}
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addresses = append(addresses, addr)
	}

	// Process addresses in batches of 100 (Solana RPC limit for GetMultipleAccounts)
	for i := 0; i < len(addresses); i += 100 {
		end := i + 100
		if end > len(addresses) {
			end = len(addresses)
		}
		batch := addresses[i:end]
		s.checkAddressBatch(ctx, batch)
	}
}

func (s *SolanaService) checkAddressBatch(ctx context.Context, addresses []string) {
	pubKeys := make([]solana.PublicKey, 0, len(addresses))
	for _, addr := range addresses {
		pk, err := solana.PublicKeyFromBase58(addr)
		if err == nil {
			pubKeys = append(pubKeys, pk)
		}
	}

	if len(pubKeys) == 0 {
		return
	}

	// Get current balances for the entire batch in ONE call
	accounts, err := s.client.GetMultipleAccounts(ctx, pubKeys...)
	if err != nil || accounts == nil {
		return
	}

	for i, acc := range accounts.Value {
		if acc == nil {
			continue
		}

		addr := addresses[i]
		if dep, err := resolveWatchedDepositByAddress(ctx, s.depositRepo, s.txManager, "SOLANA", addr); err == nil && dep != nil {
			if dep.PaymentAddress != "" && dep.PaymentAddress != dep.DepositAddress {
				s.handlePotentialDeposit(ctx, addr)
				continue
			}
		}

		currentLamports := acc.Lamports

		if database.Rdb == nil {
			if currentLamports > 0 {
				s.handlePotentialDeposit(ctx, addr)
			}
			continue
		}

		balanceKey := fmt.Sprintf("last_bal:SOLANA:%s", addr)
		lastBalStr, _ := database.Rdb.Get(ctx, balanceKey).Result()
		lastBal, _ := strconv.ParseUint(lastBalStr, 10, 64)

		if currentLamports > lastBal {
			s.handlePotentialDeposit(ctx, addr)
			database.Rdb.Set(ctx, balanceKey, strconv.FormatUint(currentLamports, 10), 24*time.Hour)
		} else if lastBal == 0 {
			database.Rdb.Set(ctx, balanceKey, strconv.FormatUint(currentLamports, 10), 24*time.Hour)
		}
	}
}

func (s *SolanaService) handlePotentialDeposit(ctx context.Context, addr string) {
	dep, err := resolveWatchedDepositByAddress(ctx, s.depositRepo, s.txManager, "SOLANA", addr)
	if err != nil || dep == nil {
		return
	}

	pubKey, _ := solana.PublicKeyFromBase58(addr)
	sigs, err := s.client.GetSignaturesForAddress(ctx, pubKey)
	if err != nil {
		return
	}

	for _, sigInfo := range sigs {
		if sigInfo.Err != nil || (dep.TxHash != nil && *dep.TxHash == sigInfo.Signature.String()) {
			continue
		}
		s.processTransaction(ctx, dep, sigInfo.Signature)
	}
}

func (s *SolanaService) processTransaction(ctx context.Context, dep *model.CryptoDeposit, sig solana.Signature) {
	tx, err := s.client.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return
	}

	if tx.Meta.Err != nil {
		return
	}

	var accountKeys []solana.PublicKey
	solanaTx, err := tx.Transaction.GetTransaction()
	if err == nil {
		accountKeys = solanaTx.Message.AccountKeys
	}

	// 1. Get Decimals from Wallet
	decimals := 9 // Default for SOL
	wallet, _ := s.hdRepo.FindByID(ctx, dep.HDWalletID)
	if wallet != nil {
		decimals = wallet.Decimals
	}

	// 2. SPL Token Detection (if contract_address is set)
	if wallet != nil && wallet.ContractAddress != nil && *wallet.ContractAddress != "" {
		expectedMint := *wallet.ContractAddress
		var preAmount, postAmount decimal.Decimal
		found := false

		for _, tb := range tx.Meta.PreTokenBalances {
			if tb.Mint.String() == expectedMint && s.tokenBalanceMatchesDeposit(tb, dep, accountKeys) {
				preAmount, _ = decimal.NewFromString(tb.UiTokenAmount.Amount)
				break
			}
		}
		for _, tb := range tx.Meta.PostTokenBalances {
			if tb.Mint.String() == expectedMint && s.tokenBalanceMatchesDeposit(tb, dep, accountKeys) {
				postAmount, _ = decimal.NewFromString(tb.UiTokenAmount.Amount)
				found = true
				break
			}
		}

		if found && postAmount.GreaterThan(preAmount) {
			diff := postAmount.Sub(preAmount)
			amount := diff.Div(decimal.NewFromInt(10).Pow(decimal.NewFromInt(int64(decimals))))

			log.Printf("SPL Token Deposit Detected: %s %s, amount: %s", dep.Coin, dep.DepositAddress, amount)
			if dep.PaymentAddress != "" && dep.PaymentAddress != dep.DepositAddress && s.txManager != nil {
				s.txManager.RecordCounter(ctx, "solana_token_ata_detected", map[string]string{
					"coin":    dep.Coin,
					"network": dep.Network,
				})
			}
			txHash := sig.String()
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txHash, &amount); err != nil {
				log.Printf("Failed to persist SOLANA token deposit update for %s (tx: %s): %v", dep.ID, txHash, err)
			}
			return
		}

		// Token invoices must ignore native inbound and operational prefunds.
		return
	}

	// 3. Native SOL Detection (Fallback)
	if !solanaDepositAcceptsNativeTransfer(wallet) {
		return
	}

	addrIdx := -1
	if len(accountKeys) == 0 {
		return
	}
	for idx, key := range accountKeys {
		if key.String() == dep.DepositAddress {
			addrIdx = idx
			break
		}
	}

	if addrIdx == -1 {
		return
	}

	postBal := tx.Meta.PostBalances[addrIdx]
	preBal := tx.Meta.PreBalances[addrIdx]

	if postBal > preBal {
		diff := postBal - preBal
		amount := decimal.NewFromUint64(diff).Div(decimal.NewFromInt(10).Pow(decimal.NewFromInt(int64(decimals))))

		log.Printf("SOL Deposit Detected: %s, amount: %s", dep.DepositAddress, amount)
		txHash := sig.String()
		if isOperationalPrefundTx(ctx, s.txManager, dep.Network, dep.DepositAddress, txHash) {
			log.Printf("Ignoring operational SOL prefund for %s: %s", dep.ID, txHash)
			return
		}
		if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txHash, &amount); err != nil {
			log.Printf("Failed to persist SOLANA native deposit update for %s (tx: %s): %v", dep.ID, txHash, err)
		}
	}
}

func (s *SolanaService) tokenBalanceMatchesDeposit(tb rpc.TokenBalance, dep *model.CryptoDeposit, accountKeys []solana.PublicKey) bool {
	if tb.Owner != nil && tb.Owner.String() == dep.DepositAddress {
		return true
	}

	targetAddress := dep.PaymentAddress
	if targetAddress == "" {
		targetAddress = dep.DepositAddress
	}
	idx := int(tb.AccountIndex)
	return idx >= 0 && idx < len(accountKeys) && accountKeys[idx].String() == targetAddress
}

func (s *SolanaService) GetBalance(ctx context.Context, addr string) (uint64, error) {
	pubKey, err := solana.PublicKeyFromBase58(addr)
	if err != nil {
		return 0, err
	}
	res, err := s.client.GetBalance(ctx, pubKey, rpc.CommitmentFinalized)
	if err != nil {
		return 0, err
	}
	return res.Value, nil
}

func (s *SolanaService) CheckTxConfirmation(ctx context.Context, txHash string) (bool, error) {
	sig, err := solana.SignatureFromBase58(txHash)
	if err != nil {
		return false, fmt.Errorf("invalid signature: %w", err)
	}

	out, err := s.client.GetSignatureStatuses(ctx, true, sig)
	if err != nil {
		return false, err
	}

	if out == nil || len(out.Value) == 0 || out.Value[0] == nil {
		return false, nil // Not found or pending
	}

	status := out.Value[0]
	if status.Err != nil {
		return false, fmt.Errorf("transaction failed: %v", status.Err)
	}

	if status.ConfirmationStatus == rpc.ConfirmationStatusFinalized || status.ConfirmationStatus == rpc.ConfirmationStatusConfirmed {
		return true, nil
	}

	return false, nil
}
