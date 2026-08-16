package service

import (
	"context"
	"crypto_payment_gateway_core/internal/model"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/ton/jetton"
)

type LiveDepositAuditFinding struct {
	DepositID      string     `json:"deposit_id"`
	PaymentID      *string    `json:"payment_id,omitempty"`
	Coin           string     `json:"coin"`
	Network        string     `json:"network"`
	Status         string     `json:"status"`
	PaymentAddress string     `json:"payment_address"`
	DepositAddress string     `json:"deposit_address"`
	AmountExpected *string    `json:"amount_expected,omitempty"`
	ObservedAmount string     `json:"observed_amount"`
	LiveBalance    string     `json:"live_balance"`
	LastInboundAt  *time.Time `json:"last_inbound_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type LiveDepositAuditResult struct {
	AuditedAt    time.Time                 `json:"audited_at"`
	ScannedCount int                       `json:"scanned_count"`
	Findings     []LiveDepositAuditFinding `json:"findings"`
	Errors       []string                  `json:"errors,omitempty"`
}

const liveDepositAuditPerAddressTimeout = 6 * time.Second
const liveDepositAuditBTCPerAddressTimeout = 12 * time.Second
const liveDepositAuditTRC20PerAddressTimeout = 12 * time.Second
const liveDepositAuditConcurrency = 8
const liveDepositAuditBTCConcurrency = 1
const liveDepositAuditTRC20TokenConcurrency = 1
const liveDepositAuditTRC20Spacing = 1200 * time.Millisecond
const liveDepositAuditTRC20RetryLimit = 4
const liveDepositAuditTRC20RetryBaseDelay = 1200 * time.Millisecond

func (s *SweeperService) AuditGeneratedDepositAddresses(ctx context.Context) (*LiveDepositAuditResult, error) {
	if s == nil || s.DepositRepository == nil || s.HdWalletRepository == nil {
		return nil, fmt.Errorf("audit dependencies are not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	deposits, err := s.DepositRepository.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	wallets, err := s.HdWalletRepository.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	walletByID := make(map[string]*model.HDWallet, len(wallets))
	for i := range wallets {
		wallet := wallets[i]
		walletByID[wallet.ID] = &wallet
	}

	result := &LiveDepositAuditResult{
		AuditedAt:    time.Now().UTC(),
		ScannedCount: len(deposits),
		Findings:     make([]LiveDepositAuditFinding, 0),
	}

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, liveDepositAuditConcurrency)
		btcSem   = make(chan struct{}, liveDepositAuditBTCConcurrency)
		tronSem  = make(chan struct{}, liveDepositAuditTRC20TokenConcurrency)
		mu       sync.Mutex
		errors   []string
		findings []LiveDepositAuditFinding
	)

	for _, dep := range deposits {
		dep := dep
		if dep == nil {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			walletCfg := walletByID[dep.HDWalletID]
			if walletCfg == nil {
				mu.Lock()
				if len(errors) < 25 {
					errors = append(errors, fmt.Sprintf("%s: missing wallet configuration", dep.ID))
				}
				mu.Unlock()
				return
			}

			if auditNetworkCode(walletCfg.Network) == "BTC" {
				btcSem <- struct{}{}
				defer func() { <-btcSem }()
			}

			if auditNetworkCode(walletCfg.Network) == "TRC20" && walletUsesTokenBalance(walletCfg) {
				tronSem <- struct{}{}
				defer func() {
					time.Sleep(liveDepositAuditTRC20Spacing)
					<-tronSem
				}()
			}

			liveBalance, err := s.auditDepositLiveBalanceWithTimeout(ctx, dep, walletCfg)
			if err != nil {
				mu.Lock()
				if len(errors) < 25 {
					errors = append(errors, fmt.Sprintf("%s (%s/%s): %v", dep.ID, dep.Coin, dep.Network, err))
				}
				mu.Unlock()
				return
			}
			if !liveBalance.GreaterThan(decimal.Zero) {
				return
			}

			observedAmount, observedErr := s.DepositRepository.SumIncomingTransfers(ctx, dep.ID)
			if observedErr != nil {
				mu.Lock()
				if len(errors) < 25 {
					errors = append(errors, fmt.Sprintf("%s: failed to load observed amount: %v", dep.ID, observedErr))
				}
				mu.Unlock()
				return
			}
			if dep.DetectedAmount.Valid && dep.DetectedAmount.Decimal.GreaterThan(observedAmount) {
				observedAmount = dep.DetectedAmount.Decimal
			}

			var amountExpected *string
			if dep.AmountExpected.Valid {
				value := dep.AmountExpected.Decimal.String()
				amountExpected = &value
			}

			finding := LiveDepositAuditFinding{
				DepositID:      dep.ID,
				PaymentID:      dep.PaymentID,
				Coin:           dep.Coin,
				Network:        dep.Network,
				Status:         dep.Status,
				PaymentAddress: depositAuditTargetAddress(dep),
				DepositAddress: dep.DepositAddress,
				AmountExpected: amountExpected,
				ObservedAmount: observedAmount.String(),
				LiveBalance:    liveBalance.String(),
				LastInboundAt:  dep.LastInboundAt,
				CreatedAt:      dep.CreatedAt,
			}

			mu.Lock()
			findings = append(findings, finding)
			mu.Unlock()
		}()
	}

	wg.Wait()

	sort.Slice(findings, func(i, j int) bool {
		left, _ := decimal.NewFromString(findings[i].LiveBalance)
		right, _ := decimal.NewFromString(findings[j].LiveBalance)
		if !left.Equal(right) {
			return left.GreaterThan(right)
		}

		leftTime := findings[i].CreatedAt
		if findings[i].LastInboundAt != nil {
			leftTime = *findings[i].LastInboundAt
		}
		rightTime := findings[j].CreatedAt
		if findings[j].LastInboundAt != nil {
			rightTime = *findings[j].LastInboundAt
		}
		return leftTime.After(rightTime)
	})

	result.Findings = findings
	if len(errors) > 0 {
		result.Errors = compressLiveAuditErrors(errors)
	}

	return result, nil
}

func depositAuditTargetAddress(dep *model.CryptoDeposit) string {
	if dep == nil {
		return ""
	}
	if strings.TrimSpace(dep.PaymentAddress) != "" {
		return strings.TrimSpace(dep.PaymentAddress)
	}
	return strings.TrimSpace(dep.DepositAddress)
}

func auditNetworkCode(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "TRON", "TRC20":
		return "TRC20"
	case "ETHEREUM", "ERC20":
		return "ERC20"
	case "BSC", "BEP20":
		return "BEP20"
	case "POL", "POLYGON":
		return "POLYGON"
	case "ARB", "ARBITRUM":
		return "ARBITRUM"
	default:
		return strings.ToUpper(strings.TrimSpace(network))
	}
}

func walletUsesTokenBalance(wallet *model.HDWallet) bool {
	if wallet == nil || wallet.ContractAddress == nil {
		return false
	}
	return strings.TrimSpace(*wallet.ContractAddress) != ""
}

func (s *SweeperService) auditDepositLiveBalance(ctx context.Context, dep *model.CryptoDeposit, walletCfg *model.HDWallet) (decimal.Decimal, error) {
	network := auditNetworkCode(walletCfg.Network)
	targetAddress := depositAuditTargetAddress(dep)

	switch network {
	case "BTC":
		return s.auditBTCBalance(targetAddress)
	case "TRC20":
		if walletUsesTokenBalance(walletCfg) {
			units, err := s.auditTRC20TokenBalance(ctx, targetAddress, walletCfg)
			if err != nil {
				return decimal.Zero, err
			}
			if units == nil {
				return decimal.Zero, nil
			}
			return decimal.NewFromBigInt(units, int32(-walletCfg.Decimals)), nil
		}
		balanceSun, err := s.tronAccountBalanceSun(ctx, targetAddress)
		if err != nil {
			return decimal.Zero, err
		}
		return decimal.NewFromInt(balanceSun).Shift(-6), nil
	case "SOLANA":
		return s.auditSolanaBalance(ctx, dep, walletCfg)
	case "TON":
		return s.auditTonBalance(ctx, dep, walletCfg)
	default:
		return s.auditEVMBalance(ctx, targetAddress, walletCfg)
	}
}

func (s *SweeperService) auditTRC20TokenBalance(ctx context.Context, targetAddress string, walletCfg *model.HDWallet) (*big.Int, error) {
	if walletCfg == nil || walletCfg.ContractAddress == nil {
		return nil, fmt.Errorf("tron token contract is not configured")
	}

	contractAddress := strings.TrimSpace(*walletCfg.ContractAddress)
	var lastErr error
	for attempt := 0; attempt < liveDepositAuditTRC20RetryLimit; attempt++ {
		units, err := s.auditTRC20TokenBalanceHTTP(ctx, targetAddress, contractAddress)
		if err == nil {
			return units, nil
		}
		lastErr = err
		if !isTronRateLimitError(err) || attempt == liveDepositAuditTRC20RetryLimit-1 {
			break
		}

		delay := liveDepositAuditTRC20RetryBaseDelay * time.Duration(attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	return nil, lastErr
}

type tronTriggerConstantContractRequest struct {
	ContractAddress  string `json:"contract_address"`
	FunctionSelector string `json:"function_selector"`
	Parameter        string `json:"parameter"`
	OwnerAddress     string `json:"owner_address"`
	Visible          bool   `json:"visible"`
}

type tronTriggerConstantContractResponse struct {
	ConstantResult []string `json:"constant_result"`
	Result         struct {
		Result  bool   `json:"result"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"result"`
}

func (s *SweeperService) auditTRC20TokenBalanceHTTP(ctx context.Context, targetAddress, contractAddress string) (*big.Int, error) {
	encodedAddress, err := tronTriggerConstantContractBalanceParam(targetAddress)
	if err != nil {
		return nil, err
	}

	payload := tronTriggerConstantContractRequest{
		ContractAddress:  contractAddress,
		FunctionSelector: "balanceOf(address)",
		Parameter:        encodedAddress,
		OwnerAddress:     targetAddress,
		Visible:          true,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.trongrid.io/wallet/triggerconstantcontract", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := strings.TrimSpace(os.Getenv("TRON_PRO_API_KEY")); apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", apiKey)
	}

	resp, err := tronGridHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("trongrid triggerconstantcontract returned status %d: %s", resp.StatusCode, truncateTronGridLogBody(respBody))
	}

	var result tronTriggerConstantContractResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to decode trongrid triggerconstantcontract response: %w", err)
	}

	if !result.Result.Result {
		message := decodeTronTriggerMessage(result.Result.Message)
		if message == "" {
			message = result.Result.Code
		}
		if message == "" {
			message = "unknown triggerconstantcontract error"
		}
		return nil, fmt.Errorf("trongrid triggerconstantcontract rejected request: %s", message)
	}

	if len(result.ConstantResult) == 0 {
		return nil, nil
	}

	units := new(big.Int)
	trimmed := strings.TrimLeft(strings.TrimSpace(result.ConstantResult[0]), "0")
	if trimmed == "" {
		return big.NewInt(0), nil
	}
	if _, ok := units.SetString(trimmed, 16); !ok {
		return nil, fmt.Errorf("invalid trongrid constant_result %q", result.ConstantResult[0])
	}
	return units, nil
}

func isTronRateLimitError(err error) bool {
	if err == nil {
		return false
	}

	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "429") ||
		strings.Contains(errText, "too many requests") ||
		strings.Contains(errText, "resource exhausted")
}

func tronTriggerConstantContractBalanceParam(base58Address string) (string, error) {
	addr, err := tronaddress.Base58ToAddress(strings.TrimSpace(base58Address))
	if err != nil {
		return "", fmt.Errorf("invalid tron address %q: %w", base58Address, err)
	}

	return fmt.Sprintf("%064x", addr.Bytes()), nil
}

func decodeTronTriggerMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return ""
	}

	decoded, err := hex.DecodeString(trimmed)
	if err != nil {
		return trimmed
	}
	return strings.TrimSpace(string(decoded))
}

func compressLiveAuditErrors(errors []string) []string {
	if len(errors) == 0 {
		return nil
	}

	type groupedAuditError struct {
		detail     string
		depositIDs []string
	}

	groupedByDetail := make(map[string]*groupedAuditError, len(errors))
	order := make([]string, 0, len(errors))

	for _, raw := range errors {
		depositID, detail := splitLiveAuditError(raw)
		group, exists := groupedByDetail[detail]
		if !exists {
			group = &groupedAuditError{detail: detail}
			groupedByDetail[detail] = group
			order = append(order, detail)
		}
		if depositID != "" {
			group.depositIDs = append(group.depositIDs, depositID)
		}
	}

	compressed := make([]string, 0, len(order))
	for _, detail := range order {
		group := groupedByDetail[detail]
		if group == nil {
			continue
		}
		if len(group.depositIDs) <= 1 {
			if len(group.depositIDs) == 1 {
				compressed = append(compressed, fmt.Sprintf("%s: %s", group.depositIDs[0], group.detail))
			} else {
				compressed = append(compressed, group.detail)
			}
			continue
		}

		sample := strings.Join(group.depositIDs[:minInt(len(group.depositIDs), 3)], ", ")
		remaining := len(group.depositIDs) - minInt(len(group.depositIDs), 3)
		suffix := ""
		if remaining > 0 {
			suffix = fmt.Sprintf(" (+%d more)", remaining)
		}

		compressed = append(
			compressed,
			fmt.Sprintf("%d deposits hit the same audit error [%s%s]: %s", len(group.depositIDs), sample, suffix, group.detail),
		)
	}

	return compressed
}

func splitLiveAuditError(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}

	firstSeparator := strings.Index(raw, ": ")
	if firstSeparator == -1 {
		return "", raw
	}

	prefix := raw[:firstSeparator]
	detail := raw[firstSeparator+2:]
	depositID := prefix
	if spaceIdx := strings.Index(prefix, " "); spaceIdx != -1 {
		depositID = prefix[:spaceIdx]
	}

	return strings.TrimSpace(depositID), strings.TrimSpace(detail)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *SweeperService) auditDepositLiveBalanceWithTimeout(ctx context.Context, dep *model.CryptoDeposit, walletCfg *model.HDWallet) (decimal.Decimal, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	timeout := liveDepositAuditTimeoutForWallet(walletCfg)
	auditCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type auditResult struct {
		balance decimal.Decimal
		err     error
	}

	resultCh := make(chan auditResult, 1)
	go func() {
		balance, err := s.auditDepositLiveBalance(auditCtx, dep, walletCfg)
		resultCh <- auditResult{balance: balance, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.balance, result.err
	case <-auditCtx.Done():
		if ctx.Err() != nil {
			return decimal.Zero, ctx.Err()
		}
		return decimal.Zero, fmt.Errorf("live balance check timed out after %s", timeout)
	}
}

func liveDepositAuditTimeoutForWallet(walletCfg *model.HDWallet) time.Duration {
	if walletCfg == nil {
		return liveDepositAuditPerAddressTimeout
	}

	switch auditNetworkCode(walletCfg.Network) {
	case "BTC":
		return liveDepositAuditBTCPerAddressTimeout
	case "TRC20":
		if walletUsesTokenBalance(walletCfg) {
			return liveDepositAuditTRC20PerAddressTimeout
		}
	}

	return liveDepositAuditPerAddressTimeout
}

func (s *SweeperService) auditBTCBalance(address string) (decimal.Decimal, error) {
	if s.MempoolClient != nil {
		utxos, err := s.MempoolClient.GetUTXOs(address)
		if err != nil {
			return decimal.Zero, err
		}
		total := decimal.Zero
		for _, utxo := range utxos {
			total = total.Add(decimal.NewFromInt(utxo.Value).Shift(-8))
		}
		return total, nil
	}

	if s.BTCClient == nil {
		return decimal.Zero, fmt.Errorf("btc audit client not configured")
	}

	decoded, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return decimal.Zero, err
	}
	txs, err := s.BTCClient.SearchRawTransactionsVerbose(decoded, 0, 200, true, true, nil)
	if err != nil {
		return decimal.Zero, err
	}

	totalSat := int64(0)
	for _, tx := range txs {
		for _, output := range tx.Vout {
			if output.ScriptPubKey.Address == address {
				totalSat += int64(output.Value * 1e8)
			}
		}
		for _, input := range tx.Vin {
			if input.PrevOut != nil && input.PrevOut.Addresses != nil {
				for _, prevAddress := range input.PrevOut.Addresses {
					if prevAddress == address {
						totalSat -= int64(input.PrevOut.Value * 1e8)
					}
				}
			}
		}
	}
	if totalSat < 0 {
		totalSat = 0
	}
	return decimal.NewFromInt(totalSat).Shift(-8), nil
}

func (s *SweeperService) auditEVMBalance(ctx context.Context, address string, walletCfg *model.HDWallet) (decimal.Decimal, error) {
	client, err := s.getEVMClient(walletCfg.Network)
	if err != nil {
		return decimal.Zero, err
	}
	if !common.IsHexAddress(address) {
		return decimal.Zero, fmt.Errorf("invalid evm address")
	}

	holder := common.HexToAddress(address)
	if walletUsesTokenBalance(walletCfg) {
		contractAddress := strings.TrimSpace(*walletCfg.ContractAddress)
		if !common.IsHexAddress(contractAddress) {
			return decimal.Zero, fmt.Errorf("invalid token contract address")
		}

		callData := append(common.FromHex("0x70a08231"), common.LeftPadBytes(holder.Bytes(), 32)...)
		contract := common.HexToAddress(contractAddress)
		result, err := client.CallContract(ctx, ethereum.CallMsg{
			To:   &contract,
			Data: callData,
		}, nil)
		if err != nil {
			return decimal.Zero, err
		}

		units := new(big.Int).SetBytes(result)
		return decimal.NewFromBigInt(units, int32(-walletCfg.Decimals)), nil
	}

	balance, err := client.BalanceAt(ctx, holder, nil)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromBigInt(balance, -18), nil
}

func (s *SweeperService) auditSolanaBalance(ctx context.Context, dep *model.CryptoDeposit, walletCfg *model.HDWallet) (decimal.Decimal, error) {
	client := s.getSolanaRPCClient()

	if walletUsesTokenBalance(walletCfg) {
		targetAddress := depositAuditTargetAddress(dep)
		account, err := solana.PublicKeyFromBase58(targetAddress)
		if err != nil {
			return decimal.Zero, err
		}
		balance, err := client.GetTokenAccountBalance(ctx, account, rpc.CommitmentFinalized)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "could not find") || strings.Contains(strings.ToLower(err.Error()), "not found") {
				return decimal.Zero, nil
			}
			return decimal.Zero, err
		}
		if balance == nil || balance.Value == nil || balance.Value.Amount == "" {
			return decimal.Zero, nil
		}
		units, err := parseTokenBalanceUnits(balance.Value.Amount)
		if err != nil {
			return decimal.Zero, err
		}
		return decimal.NewFromBigInt(units, int32(-walletCfg.Decimals)), nil
	}

	balanceLamports, err := s.SolanaService.GetBalance(ctx, dep.DepositAddress)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromUint64(balanceLamports).Shift(-9), nil
}

func (s *SweeperService) auditTonBalance(ctx context.Context, dep *model.CryptoDeposit, walletCfg *model.HDWallet) (decimal.Decimal, error) {
	api, err := s.getTonAPIClient(ctx)
	if err != nil {
		return decimal.Zero, err
	}

	if walletUsesTokenBalance(walletCfg) {
		masterAddr, err := address.ParseAddr(strings.TrimSpace(*walletCfg.ContractAddress))
		if err != nil {
			return decimal.Zero, err
		}
		ownerAddr, err := address.ParseAddr(dep.DepositAddress)
		if err != nil {
			return decimal.Zero, err
		}

		jettonClient := jetton.NewJettonMasterClient(api, masterAddr)
		jettonWallet, err := jettonClient.GetJettonWallet(ctx, ownerAddr)
		if err != nil {
			return decimal.Zero, err
		}
		balance, err := jettonWallet.GetBalance(ctx)
		if err != nil {
			return decimal.Zero, err
		}
		if balance == nil {
			return decimal.Zero, nil
		}
		return decimal.NewFromBigInt(balance, int32(-walletCfg.Decimals)), nil
	}

	addr, err := address.ParseAddr(dep.DepositAddress)
	if err != nil {
		return decimal.Zero, err
	}
	master, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	account, err := api.GetAccount(ctx, master, addr)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromBigInt(tonAccountBalanceNano(account), -9), nil
}
