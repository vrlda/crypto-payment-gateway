package service

import (
	"context"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
	tronapi "github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	troncore "github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/shopspring/decimal"
)

type TronScanner struct {
	depositRepo *repository.DepositRepository
	hdRepo      *repository.HdWalletRepository
	txManager   *TransactionManager
	tronClient  *client.GrpcClient
	apiKey      string
}

type TRC20Transfer struct {
	TransactionID string `json:"transaction_id"`
	From          string `json:"from"`
	To            string `json:"to"`
	TokenInfo     struct {
		Address  string `json:"address"`
		Decimals int    `json:"decimals"`
	} `json:"token_info"`
	Value string `json:"value"`
}

type TRC20Response struct {
	Data    []TRC20Transfer `json:"data"`
	Success bool            `json:"success"`
	Meta    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"meta"`
}

type tronNativeResponseItem struct {
	TxID string `json:"txID"`
	Ret  []struct {
		ContractRet string `json:"contractRet"`
	} `json:"ret"`
	RawData struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value struct {
					Amount int64  `json:"amount"`
					To     string `json:"to_address"`
				} `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
	} `json:"raw_data"`
}

type tronNativeResponse struct {
	Data    []tronNativeResponseItem `json:"data"`
	Success bool                     `json:"success"`
	Meta    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"meta"`
}

type aggregatedTRC20Transfer struct {
	TxHash string
	Amount decimal.Decimal
}

const (
	tronHistoryPageLimit = 200
	tronHistoryPageCap   = 20
	tronGridHTTPTimeout  = 10 * time.Second
)

var tronGridHTTPClient = &http.Client{Timeout: tronGridHTTPTimeout}

func NewTronScanner(depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *TransactionManager, tronClient *client.GrpcClient, apiKey string) *TronScanner {
	return &TronScanner{
		depositRepo: depositRepo,
		hdRepo:      hdRepo,
		txManager:   txManager,
		tronClient:  tronClient,
		apiKey:      apiKey,
	}
}

func (s *TronScanner) Start(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

func (s *TronScanner) poll(ctx context.Context) {
	deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, "TRC20")
	if err != nil {
		log.Printf("Tron poll: failed to fetch watched deposits: %v", err)
		return
	}

	for _, dep := range deposits {
		s.checkAddress(ctx, dep)
		s.checkConfirmations(ctx, dep)
	}
}

func (s *TronScanner) checkAddress(ctx context.Context, dep *model.CryptoDeposit) {
	if dep.Status != "PENDING" && dep.Status != "DETECTED" && dep.Status != "CONFIRMED" && dep.Status != "FINALIZED" {
		return
	}

	wallet, err := s.hdRepo.FindByID(ctx, dep.HDWalletID)
	if err != nil || wallet == nil {
		return
	}

	if wallet.ContractAddress != nil && *wallet.ContractAddress != "" {
		s.checkTRC20Deposit(ctx, dep, wallet)
		return
	}

	s.checkNativeTRXDeposit(ctx, dep, wallet)
}

func (s *TronScanner) checkNativeTRXDeposit(ctx context.Context, dep *model.CryptoDeposit, wallet *model.HDWallet) {
	addr := dep.PaymentAddress
	if addr == "" {
		addr = dep.DepositAddress
	}

	s.walkNativeTransactions(ctx, addr, func(txID string, tx tronNativeResponseItem) bool {
		alreadyRecorded, err := s.depositRepo.HasIncomingTransfer(ctx, dep.ID, txID)
		if err == nil && alreadyRecorded {
			return true
		}
		if dep.TxHash != nil && *dep.TxHash == txID {
			return true
		}
		if len(tx.Ret) > 0 && tx.Ret[0].ContractRet != "" && tx.Ret[0].ContractRet != "SUCCESS" {
			return false
		}
		if len(tx.RawData.Contract) == 0 || tx.RawData.Contract[0].Type != "TransferContract" {
			return false
		}
		if !strings.EqualFold(tx.RawData.Contract[0].Parameter.Value.To, addr) {
			return false
		}

		amount := decimal.NewFromInt(tx.RawData.Contract[0].Parameter.Value.Amount).Div(decimal.NewFromInt(10).Pow(decimal.NewFromInt(int64(wallet.Decimals))))
		log.Printf("tron_native_detected address=%s amount=%s", addr, amount)
		if s.txManager != nil {
			s.txManager.RecordCounter(ctx, "tron_native_detected", map[string]string{
				"coin":    dep.Coin,
				"network": dep.Network,
			})
		}
		if isOperationalPrefundTx(ctx, s.txManager, dep.Network, addr, txID) {
			log.Printf("Ignoring operational TRON prefund for %s: %s", dep.ID, txID)
			return false
		}
		if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txID, &amount); err != nil {
			log.Printf("Failed to persist TRON native deposit update for %s (tx: %s): %v", dep.ID, txID, err)
		}
		return false
	})
}

func (s *TronScanner) checkTRC20Deposit(ctx context.Context, dep *model.CryptoDeposit, wallet *model.HDWallet) {
	addr := dep.PaymentAddress
	if addr == "" {
		addr = dep.DepositAddress
	}

	requestContract := strings.TrimSpace(*wallet.ContractAddress)
	expectedContract := strings.ToLower(requestContract)
	var transfers []TRC20Transfer
	s.walkTRC20Transactions(ctx, addr, requestContract, func(tx TRC20Transfer) bool {
		transfers = append(transfers, tx)
		return false
	})

	aggregated := aggregateTRC20TransfersForDeposit(transfers, addr, expectedContract, wallet.Decimals)
	txIDs := make([]string, 0, len(aggregated))
	for txID := range aggregated {
		txIDs = append(txIDs, txID)
	}
	sort.Strings(txIDs)

	for _, txID := range txIDs {
		amount := aggregated[txID].Amount
		recordedAmount, exists, err := s.depositRepo.GetIncomingTransferAmount(ctx, dep.ID, txID)
		if err != nil {
			log.Printf("Failed to load recorded TRC20 amount for deposit %s tx %s: %v", dep.ID, txID, err)
			exists = false
		}
		if !shouldProcessAggregatedTRC20Transfer(recordedAmount, exists, amount) {
			continue
		}

		// Pool-address guard: if this txHash is already attributed to a different deposit
		// at this same address (possible because pool addresses are reused), skip it.
		// This prevents old transactions from prior payments being re-credited to new deposits.
		if claimed, claimErr := s.depositRepo.TxHashAlreadyClaimed(ctx, txID); claimErr == nil && claimed {
			log.Printf("TRC20 scan: skipping tx %s for deposit %s — already claimed by another deposit", txID, dep.ID)
			continue
		}

		log.Printf("TRC20 Deposit Detected: %s, tx: %s, amount: %s", addr, txID, amount)
		txIDCopy := txID
		amountCopy := amount
		if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txIDCopy, &amountCopy); err != nil {
			log.Printf("Failed to persist TRC20 deposit update for %s (tx: %s): %v", dep.ID, txID, err)
		}
	}
}

func aggregateTRC20TransfersForDeposit(transfers []TRC20Transfer, paymentAddress, expectedContract string, defaultDecimals int) map[string]aggregatedTRC20Transfer {
	aggregated := make(map[string]aggregatedTRC20Transfer)
	normalizedAddress := strings.TrimSpace(paymentAddress)
	normalizedContract := strings.ToLower(strings.TrimSpace(expectedContract))

	for _, tx := range transfers {
		if !strings.EqualFold(strings.TrimSpace(tx.To), normalizedAddress) {
			continue
		}
		if normalizedContract != "" && strings.ToLower(strings.TrimSpace(tx.TokenInfo.Address)) != normalizedContract {
			continue
		}

		value, err := decimal.NewFromString(tx.Value)
		if err != nil {
			continue
		}

		decimals := defaultDecimals
		if tx.TokenInfo.Decimals > 0 {
			decimals = tx.TokenInfo.Decimals
		}

		amount := value.Div(decimal.NewFromInt(10).Pow(decimal.NewFromInt(int64(decimals))))
		current, exists := aggregated[tx.TransactionID]
		if !exists {
			aggregated[tx.TransactionID] = aggregatedTRC20Transfer{
				TxHash: tx.TransactionID,
				Amount: amount,
			}
			continue
		}

		current.Amount = current.Amount.Add(amount)
		aggregated[tx.TransactionID] = current
	}

	return aggregated
}

func shouldProcessAggregatedTRC20Transfer(recordedAmount decimal.Decimal, exists bool, aggregatedAmount decimal.Decimal) bool {
	if !exists {
		return true
	}
	return aggregatedAmount.GreaterThan(recordedAmount)
}

func (s *TronScanner) walkNativeTransactions(ctx context.Context, addr string, handler func(string, tronNativeResponseItem) bool) {
	fingerprint := ""
	for page := 0; page < tronHistoryPageCap; page++ {
		endpoint := tronGridNativeTransactionsEndpoint(addr, fingerprint)

		result, err := tronHTTPGetWithFallback(ctx, "tron_native_history", s.apiKey, endpoint)
		if err != nil {
			log.Printf("Tron native history request failed for %s: %v", addr, err)
			return
		}
		body := result.Body
		statusCode := result.StatusCode
		if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
			log.Printf("Tron native history request failed for %s with status %d: %s", addr, statusCode, truncateTronGridLogBody(body))
			return
		}

		var res tronNativeResponse
		if err := json.Unmarshal(body, &res); err != nil {
			log.Printf("Failed to decode Tron native history response for %s: %v body=%s", addr, err, truncateTronGridLogBody(body))
			return
		}
		if !res.Success {
			log.Printf("Tron native history request reported unsuccessful response for %s: %s", addr, truncateTronGridLogBody(body))
			return
		}
		if len(res.Data) == 0 {
			return
		}

		for _, tx := range res.Data {
			if handler(tx.TxID, tx) {
				return
			}
		}

		next := strings.TrimSpace(res.Meta.Fingerprint)
		if next == "" || next == fingerprint {
			return
		}
		fingerprint = next
	}

	if s.txManager != nil {
		s.txManager.RecordCounter(ctx, "tron_history_page_cap_reached", map[string]string{
			"network": "TRC20",
		})
	}
}

func (s *TronScanner) walkTRC20Transactions(ctx context.Context, addr, expectedContract string, handler func(TRC20Transfer) bool) {
	requestContract := strings.TrimSpace(expectedContract)
	normalizedContract := strings.ToLower(requestContract)
	fingerprint := ""
	for page := 0; page < tronHistoryPageCap; page++ {
		endpoint := tronGridTRC20TransactionsEndpoint(addr, requestContract, fingerprint)

		result, err := tronHTTPGetWithFallback(ctx, "tron_trc20_history", s.apiKey, endpoint)
		if err != nil {
			log.Printf("Tron TRC20 history request failed for %s contract=%s: %v", addr, requestContract, err)
			return
		}
		body := result.Body
		statusCode := result.StatusCode
		if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
			log.Printf("Tron TRC20 history request failed for %s contract=%s with status %d: %s", addr, requestContract, statusCode, truncateTronGridLogBody(body))
			return
		}

		var res TRC20Response
		if err := json.Unmarshal(body, &res); err != nil {
			log.Printf("Failed to decode Tron TRC20 history response for %s contract=%s: %v body=%s", addr, requestContract, err, truncateTronGridLogBody(body))
			return
		}
		if !res.Success {
			log.Printf("Tron TRC20 history request reported unsuccessful response for %s contract=%s: %s", addr, requestContract, truncateTronGridLogBody(body))
			return
		}
		if len(res.Data) == 0 {
			return
		}

		for _, tx := range res.Data {
			if normalizedContract != "" && strings.ToLower(strings.TrimSpace(tx.TokenInfo.Address)) != normalizedContract {
				continue
			}
			if handler(tx) {
				return
			}
		}

		next := strings.TrimSpace(res.Meta.Fingerprint)
		if next == "" || next == fingerprint {
			return
		}
		fingerprint = next
	}

	if s.txManager != nil {
		s.txManager.RecordCounter(ctx, "tron_history_page_cap_reached", map[string]string{
			"network": "TRC20",
		})
	}
}

func tronGridNativeTransactionsEndpoint(addr, fingerprint string) string {
	endpoint := fmt.Sprintf("/v1/accounts/%s/transactions?only_to=true&only_confirmed=true&limit=%d&order_by=block_timestamp,desc&visible=true", addr, tronHistoryPageLimit)
	if fingerprint != "" {
		endpoint += "&fingerprint=" + url.QueryEscape(fingerprint)
	}
	return endpoint
}

func tronGridTRC20TransactionsEndpoint(addr, contractAddress, fingerprint string) string {
	endpoint := fmt.Sprintf(
		"/v1/accounts/%s/transactions/trc20?only_confirmed=true&limit=%d&contract_address=%s",
		addr,
		tronHistoryPageLimit,
		url.QueryEscape(strings.TrimSpace(contractAddress)),
	)
	if fingerprint != "" {
		endpoint += "&fingerprint=" + url.QueryEscape(fingerprint)
	}
	return endpoint
}

func truncateTronGridLogBody(body []byte) string {
	const maxLen = 240
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "..."
}

func (s *TronScanner) checkConfirmations(ctx context.Context, dep *model.CryptoDeposit) {
	if !shouldTrackDepositTx(dep) {
		return
	}

	resolution, confs, err := s.classifyDepositTxConfirmation(ctx, dep)
	if err != nil {
		log.Printf("Failed to reconcile TRON deposit %s tx %s: %v", dep.ID, *dep.TxHash, err)
		return
	}
	if handleErr := s.txManager.HandleDepositTxResolution(ctx, dep.Network, dep, resolution); handleErr != nil {
		log.Printf("Failed to handle TRON deposit resolution for %s: %v", dep.ID, handleErr)
		return
	}
	if resolution != depositTxResolutionConfirmed || confs <= 0 {
		return
	}

	if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, confs); err != nil {
		log.Printf("Failed to persist TRON confirmations for %s: %v", dep.ID, err)
	}

	wallet, _ := s.hdRepo.FindByID(ctx, dep.HDWalletID)
	finalizationThreshold := dep.RequiredConfirmations
	if wallet != nil && wallet.FinalizationConfirmations > 0 {
		finalizationThreshold = wallet.FinalizationConfirmations
	}

	switch {
	case dep.Status == "DETECTED" && confs >= dep.RequiredConfirmations:
		if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "CONFIRMED", nil, nil); err != nil {
			log.Printf("Failed to persist TRON deposit confirmation for %s: %v", dep.ID, err)
		}
	case dep.Status == "CONFIRMED" && confs >= finalizationThreshold:
		if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "FINALIZED", nil, nil); err != nil {
			log.Printf("Failed to persist TRON deposit finalization for %s: %v", dep.ID, err)
		}
	case dep.Status == "FINALIZED":
		// Keep confirmations fresh during the active watch window.
	}
}

func (s *TronScanner) classifyDepositTxConfirmation(ctx context.Context, dep *model.CryptoDeposit) (depositTxResolution, int, error) {
	if dep == nil || dep.TxHash == nil || *dep.TxHash == "" {
		return depositTxResolutionError, 0, nil
	}
	if s.tronClient == nil {
		return depositTxResolutionError, 0, fmt.Errorf("tron client not initialized")
	}

	tx, err := tronCallWithRetry(ctx, "GetTransactionByID", s.tronClient, func(c *client.GrpcClient) (*troncore.Transaction, error) {
		return c.GetTransactionByID(*dep.TxHash)
	})
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "not found") {
			return depositTxResolutionNotFound, 0, nil
		}
		return depositTxResolutionError, 0, err
	}
	if tx == nil {
		return depositTxResolutionNotFound, 0, nil
	}

	info, err := tronCallWithRetry(ctx, "GetTransactionInfoByID", s.tronClient, func(c *client.GrpcClient) (*troncore.TransactionInfo, error) {
		return c.GetTransactionInfoByID(*dep.TxHash)
	})
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "not found") {
			return depositTxResolutionNotFound, 0, nil
		}
		return depositTxResolutionError, 0, err
	}
	if info == nil {
		return depositTxResolutionPending, 0, nil
	}

	nowBlock, err := tronCallWithRetry(ctx, "GetNowBlock", s.tronClient, func(c *client.GrpcClient) (*tronapi.BlockExtention, error) {
		return c.GetNowBlock()
	})
	if err != nil {
		return depositTxResolutionError, 0, err
	}
	latest := nowBlock.BlockHeader.RawData.Number
	if info.BlockNumber <= 0 || latest < info.BlockNumber {
		return depositTxResolutionPending, 0, nil
	}

	return depositTxResolutionConfirmed, int(latest-info.BlockNumber) + 1, nil
}
