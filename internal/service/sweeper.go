package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/ecdsa"
	"crypto/ed25519"

	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	crypto_pkg "crypto_payment_gateway_core/pkg/crypto"
	"crypto_payment_gateway_core/pkg/mempool"

	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/ton/wallet"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/client/transaction"
	tronapi "github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	troncore "github.com/fbsobreira/gotron-sdk/pkg/proto/core"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	ErrPrefundInitiated    = errors.New("prefund initiated")
	ErrSweepWaitingForGas  = errors.New("waiting for acceptable gas conditions")
	ErrEnergyRentalPending = errors.New("energy rental pending")
)

type prefundInitiatedError struct {
	txHash string
}

func (e *prefundInitiatedError) Error() string {
	return ErrPrefundInitiated.Error()
}

func (e *prefundInitiatedError) Unwrap() error {
	return ErrPrefundInitiated
}

func prefundInitiatedWithHash(txHash string) error {
	return &prefundInitiatedError{txHash: strings.TrimSpace(txHash)}
}

func prefundTxHashFromError(err error) string {
	var prefundErr *prefundInitiatedError
	if errors.As(err, &prefundErr) {
		return strings.TrimSpace(prefundErr.txHash)
	}
	return ""
}

type SweeperService struct {
	EVMClients          map[string]*ethclient.Client
	BTCClient           *rpcclient.Client
	MempoolClient       *mempool.Client
	SolanaService       *SolanaService
	TonService          *TonService
	TronClient          *client.GrpcClient
	TxManager           *TransactionManager
	SweepRepository     *repository.SweepRepository
	GasWalletRepository *repository.GasWalletRepository
	HdWalletRepository  *repository.HdWalletRepository
	DepositRepository   *repository.DepositRepository
	HDWallet            *HDWalletService
	SettingsRepository  *repository.SystemSettingsRepository
	TronEnergyProvider  TronEnergyProvider
	PoolRepository      *repository.TRC20PoolRepository
}

type SweepRescanResult struct {
	MatchedDeposits  int
	MatchedAddresses int
	QueuedSweeps     int
}

type GasWalletBalanceCheck struct {
	CurrentBalance    decimal.Decimal
	CheckedAt         time.Time
	IsBelowMinBalance bool
}

type sweepTxState string

const (
	sweepTxStatePending   sweepTxState = "PENDING"
	sweepTxStateConfirmed sweepTxState = "CONFIRMED"
	sweepTxStateFailed    sweepTxState = "FAILED"
	sweepTxStateNotFound  sweepTxState = "NOT_FOUND"
)

type sweepTxCheckResult struct {
	State   sweepTxState
	Reason  string
	Receipt *repository.SweepReceiptData // populated for confirmed TRON sweeps
}

// isUniqueViolation returns true for PostgreSQL unique constraint errors (SQLSTATE 23505).
// Used to safely handle SELECT-then-INSERT races.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// pgx wraps pgconn.PgError; check code 23505 via error string as a lightweight check.
	return strings.Contains(err.Error(), "23505") || strings.Contains(err.Error(), "unique_violation")
}

func isTronAccountNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "account not found") ||
		strings.Contains(msg, "account does not exist") ||
		strings.Contains(msg, "account not exists")
}

func resolveTronAccountBalanceSun(acc *troncore.Account, err error) (int64, error) {
	if err != nil {
		if isTronAccountNotFoundError(err) {
			return 0, nil
		}
		return 0, err
	}
	if acc == nil {
		return 0, nil
	}
	return acc.Balance, nil
}

func resolveTronAccountActivated(acc *troncore.Account, err error) (bool, error) {
	if err != nil {
		if isTronAccountNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	if acc == nil {
		return false, nil
	}
	return len(acc.Address) > 0 || acc.CreateTime > 0, nil
}

func (s *SweeperService) tronAccountActivated(ctx context.Context, address string) (bool, error) {
	if s.TronClient == nil {
		return false, fmt.Errorf("tron client not initialized")
	}

	acc, err := tronCallWithRetry(ctx, "GetAccount", s.TronClient, func(c *client.GrpcClient) (*troncore.Account, error) {
		return c.GetAccount(address)
	})
	return resolveTronAccountActivated(acc, err)
}

func (s *SweeperService) tronAccountBalanceSun(ctx context.Context, address string) (int64, error) {
	if s.TronClient == nil {
		return 0, fmt.Errorf("tron client not initialized")
	}

	acc, err := tronCallWithRetry(ctx, "GetAccount", s.TronClient, func(c *client.GrpcClient) (*troncore.Account, error) {
		return c.GetAccount(address)
	})
	return resolveTronAccountBalanceSun(acc, err)
}

func (s *SweeperService) tronAccountAvailableEnergy(ctx context.Context, address string) (int64, error) {
	if s.TronClient == nil {
		return 0, fmt.Errorf("tron client not initialized")
	}
	res, err := tronCallWithRetry(ctx, "GetAccountResource", s.TronClient, func(c *client.GrpcClient) (*tronapi.AccountResourceMessage, error) {
		return c.GetAccountResource(address)
	})
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	available := res.GetEnergyLimit() - res.GetEnergyUsed()
	if available < 0 {
		available = 0
	}
	return available, nil
}

func normalizeSweepPurpose(sw *model.Sweep) model.SweepPurpose {
	if sw == nil || sw.Purpose == "" {
		return model.SweepPurposeDepositFunds
	}
	return sw.Purpose
}

func isRetryableSweepStatus(status model.SweepStatus) bool {
	switch status {
	case model.SweepStatusPending,
		model.SweepStatusCheckingActivation,
		model.SweepStatusRequestingEnergy,
		model.SweepStatusWaitingForGas,
		model.SweepStatusSkippedNoGas,
		model.SweepStatusFailed:
		return true
	default:
		return false
	}
}

func isDepositFundSweep(sw *model.Sweep) bool {
	return normalizeSweepPurpose(sw) == model.SweepPurposeDepositFunds
}

func isGasResidueSweep(sw *model.Sweep) bool {
	return normalizeSweepPurpose(sw) == model.SweepPurposeGasResidue
}

func needsGasResidueSweep(sw *model.Sweep) bool {
	if sw == nil || !sw.IsToken || !isDepositFundSweep(sw) {
		return false
	}

	switch strings.ToUpper(strings.TrimSpace(sw.Network)) {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "TRC20", "TRON", "SOLANA", "TON":
		return true
	default:
		return false
	}
}

func nativeCoinForNetwork(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "ERC20", "ARBITRUM", "ETHEREUM":
		return "ETH"
	case "BEP20", "BSC":
		return "BNB"
	case "POLYGON", "POL":
		return "MATIC"
	case "TRC20", "TRON":
		return "TRX"
	case "SOLANA":
		return "SOL"
	case "TON":
		return "TON"
	case "BTC", "BITCOIN":
		return "BTC"
	default:
		return strings.TrimSpace(network)
	}
}

func exactNativeUnitsFromSweepAmount(amount decimal.Decimal, decimals int, asset string) (*big.Int, error) {
	if !amount.IsPositive() {
		return nil, fmt.Errorf("sweep amount must be greater than zero")
	}
	if decimals < 0 {
		return nil, fmt.Errorf("%s decimals must be non-negative", asset)
	}

	scaled := amount.Shift(int32(decimals))
	if !scaled.Equal(scaled.Truncate(0)) {
		return nil, fmt.Errorf("sweep amount %s exceeds %s precision for %d decimals", amount.String(), asset, decimals)
	}

	units := scaled.BigInt()
	if units.Sign() <= 0 {
		return nil, fmt.Errorf("sweep amount must be greater than zero")
	}
	return units, nil
}

func exactUint64UnitsFromSweepAmount(amount decimal.Decimal, decimals int, asset string) (uint64, error) {
	units, err := exactNativeUnitsFromSweepAmount(amount, decimals, asset)
	if err != nil {
		return 0, err
	}
	if !units.IsUint64() {
		return 0, fmt.Errorf("queued %s sweep amount exceeds uint64 limits", asset)
	}
	return units.Uint64(), nil
}

func exactSatoshisFromSweepAmount(amount decimal.Decimal) (int64, error) {
	units, err := exactNativeUnitsFromSweepAmount(amount, 8, "BTC")
	if err != nil {
		return 0, err
	}
	if !units.IsInt64() {
		return 0, fmt.Errorf("queued BTC sweep amount exceeds int64 limits")
	}
	return units.Int64(), nil
}

func exactTRXSunFromSweepAmount(amount decimal.Decimal) (int64, error) {
	units, err := exactNativeUnitsFromSweepAmount(amount, 6, "TRX")
	if err != nil {
		return 0, err
	}
	if !units.IsInt64() {
		return 0, fmt.Errorf("queued TRX sweep amount exceeds int64 limits")
	}
	return units.Int64(), nil
}

func subtractFeeFromQueuedUnits(queuedUnits, feeUnits *big.Int, asset string) (*big.Int, error) {
	if queuedUnits == nil || feeUnits == nil {
		return nil, fmt.Errorf("%s fee calculation requires queued amount and fee", asset)
	}
	if queuedUnits.Cmp(feeUnits) <= 0 {
		return nil, fmt.Errorf("queued %s sweep amount does not cover network fee", asset)
	}
	return new(big.Int).Sub(new(big.Int).Set(queuedUnits), feeUnits), nil
}

func subtractFeeFromQueuedUint64(queuedUnits, feeUnits uint64, asset string) (uint64, error) {
	if queuedUnits <= feeUnits {
		return 0, fmt.Errorf("queued %s sweep amount does not cover network fee", asset)
	}
	return queuedUnits - feeUnits, nil
}

func subtractFeeFromQueuedInt64(queuedUnits, feeUnits int64, asset string) (int64, error) {
	if queuedUnits <= feeUnits {
		return 0, fmt.Errorf("queued %s sweep amount does not cover network fee", asset)
	}
	return queuedUnits - feeUnits, nil
}

func exactTokenUnitsFromSweepAmount(amount decimal.Decimal, decimals int) (*big.Int, error) {
	if !amount.IsPositive() {
		return nil, fmt.Errorf("sweep amount must be greater than zero")
	}
	if decimals < 0 {
		return nil, fmt.Errorf("token decimals must be non-negative")
	}

	scaled := amount.Shift(int32(decimals))
	if !scaled.Equal(scaled.Truncate(0)) {
		return nil, fmt.Errorf("sweep amount %s exceeds token precision for %d decimals", amount.String(), decimals)
	}

	units := scaled.BigInt()
	if units.Sign() <= 0 {
		return nil, fmt.Errorf("sweep amount must be greater than zero")
	}
	return units, nil
}

func parseTokenBalanceUnits(raw string) (*big.Int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("token balance is empty")
	}

	units, ok := new(big.Int).SetString(trimmed, 10)
	if !ok {
		return nil, fmt.Errorf("invalid token balance %q", raw)
	}
	if units.Sign() < 0 {
		return nil, fmt.Errorf("token balance cannot be negative")
	}
	return units, nil
}

func shouldCloseTokenAccountAfterSweep(currentUnits, queuedUnits *big.Int) bool {
	return currentUnits != nil && queuedUnits != nil && currentUnits.Cmp(queuedUnits) == 0
}

func (s *SweeperService) recordTokenSweepBalanceBelowQueuedAmount(ctx context.Context, sweep *model.Sweep, currentUnits, queuedUnits *big.Int) {
	if sweep == nil {
		return
	}

	log.Printf(
		"Token sweep balance below queued amount for %s on %s: live=%s queued=%s",
		sweep.Coin,
		sweep.Network,
		firstBigIntString(currentUnits),
		firstBigIntString(queuedUnits),
	)

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "token_sweep_balance_below_queued_amount", map[string]string{
			"coin":    sweep.Coin,
			"network": sweep.Network,
		})
	}
}

func (s *SweeperService) recordNativeExactSweepPrefundInitiated(ctx context.Context, sweep *model.Sweep) {
	if s == nil || s.TxManager == nil || sweep == nil {
		return
	}
	s.TxManager.RecordCounter(ctx, "native_exact_sweep_prefund_initiated", map[string]string{
		"coin":    sweep.Coin,
		"network": sweep.Network,
	})
}

func (s *SweeperService) recordNativeExactSweepConstructionBlocked(ctx context.Context, sweep *model.Sweep) {
	if s == nil || s.TxManager == nil || sweep == nil {
		return
	}
	s.TxManager.RecordCounter(ctx, "native_exact_sweep_construction_blocked", map[string]string{
		"coin":    sweep.Coin,
		"network": sweep.Network,
	})
}

func firstBigIntString(value *big.Int) string {
	if value == nil {
		return "0"
	}
	return value.String()
}

func solanaFeeEstimateLamports() uint64 {
	if feeStr := os.Getenv("SOLANA_FEE_ESTIMATE"); feeStr != "" {
		if parsed, err := strconv.ParseUint(feeStr, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 5000
}

func tonFeeEstimateNano() *big.Int {
	feeStr := os.Getenv("TON_FEE_ESTIMATE")
	if feeStr == "" {
		feeStr = "0.05"
	}
	return tlb.MustFromTON(feeStr).Nano()
}

func isTONNetwork(network string) bool {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "TON":
		return true
	default:
		return false
	}
}

func NewSweeperService(
	evmClients map[string]*ethclient.Client,
	btcClient *rpcclient.Client,
	mempoolClient *mempool.Client,
	solanaService *SolanaService,
	tonService *TonService,
	tronClient *client.GrpcClient,
	txManager *TransactionManager,
	sweepRepo *repository.SweepRepository,
	gasRepo *repository.GasWalletRepository,
	hdRepo *repository.HdWalletRepository,
	depositRepo *repository.DepositRepository,
	hdService *HDWalletService,
	settingsRepo *repository.SystemSettingsRepository,
	tronEnergyProvider TronEnergyProvider,
) *SweeperService {
	return &SweeperService{
		EVMClients:          evmClients,
		BTCClient:           btcClient,
		MempoolClient:       mempoolClient,
		SolanaService:       solanaService,
		TonService:          tonService,
		TronClient:          tronClient,
		TxManager:           txManager,
		SweepRepository:     sweepRepo,
		GasWalletRepository: gasRepo,
		HdWalletRepository:  hdRepo,
		DepositRepository:   depositRepo,
		HDWallet:            hdService,
		SettingsRepository:  settingsRepo,
		TronEnergyProvider:  tronEnergyProvider,
	}
}

func (s *SweeperService) getEVMClient(network string) (*ethclient.Client, error) {
	client, ok := s.EVMClients[network]
	if !ok {
		// Fallback to default "EVM" if present
		client, ok = s.EVMClients["EVM"]
		if !ok {
			return nil, fmt.Errorf("no EVM client configured for network %s", network)
		}
	}
	return client, nil
}

func tonAccountBalanceNano(acc *tlb.Account) *big.Int {
	if acc == nil || acc.State == nil || !acc.State.IsValid {
		return big.NewInt(0)
	}
	return new(big.Int).Set(acc.State.Balance.Nano())
}

func (s *SweeperService) CheckGasWalletBalance(ctx context.Context, gw *model.GasWallet) (result *GasWalletBalanceCheck, err error) {
	if gw == nil {
		return nil, fmt.Errorf("gas wallet not provided")
	}

	chainType := repository.NormalizeGasWalletChainType(gw.ChainType)
	walletAddress := strings.TrimSpace(gw.WalletAddress)
	if walletAddress == "" {
		return nil, fmt.Errorf("wallet address is empty")
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic while checking gas wallet balance for %s: %v", chainType, recovered)
			result = nil
		}
	}()

	var balance decimal.Decimal

	switch chainType {
	case "TRON":
		balanceSun, err := s.tronAccountBalanceSun(ctx, walletAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to check gas wallet balance: %w", err)
		}

		balance = decimal.NewFromInt(balanceSun).Shift(-6)
	case "SOLANA":
		client := s.getSolanaRPCClient()

		pubKey, err := solana.PublicKeyFromBase58(walletAddress)
		if err != nil {
			return nil, fmt.Errorf("invalid solana wallet address: %w", err)
		}

		res, err := client.GetBalance(ctx, pubKey, rpc.CommitmentFinalized)
		if err != nil {
			return nil, fmt.Errorf("failed to check gas wallet balance: %w", err)
		}

		balance = decimal.NewFromUint64(res.Value).Shift(-9)
	case "TON":
		api, err := s.getTonAPIClient(ctx)
		if err != nil {
			return nil, err
		}

		addr, err := address.ParseAddr(walletAddress)
		if err != nil {
			return nil, fmt.Errorf("invalid ton wallet address: %w", err)
		}

		master, err := api.CurrentMasterchainInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch ton masterchain info: %w", err)
		}

		acc, err := api.GetAccount(ctx, master, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to check gas wallet balance: %w", err)
		}

		balance = decimal.NewFromBigInt(tonAccountBalanceNano(acc), -9)
	default:
		if !common.IsHexAddress(walletAddress) {
			return nil, fmt.Errorf("invalid evm wallet address")
		}

		client, err := s.getEVMClient(chainType)
		if err != nil {
			return nil, err
		}

		rawBalance, err := client.BalanceAt(ctx, common.HexToAddress(walletAddress), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to check gas wallet balance: %w", err)
		}

		balance = decimal.NewFromBigInt(rawBalance, -18)
	}

	result = &GasWalletBalanceCheck{
		CurrentBalance:    balance,
		CheckedAt:         time.Now().UTC(),
		IsBelowMinBalance: balance.LessThan(gw.MinBalance),
	}
	return result, nil
}

func (s *SweeperService) RefreshGasWalletBalance(ctx context.Context, gw *model.GasWallet) error {
	if s == nil || s.GasWalletRepository == nil || gw == nil || strings.TrimSpace(gw.ID) == "" {
		return nil
	}

	chainType := repository.NormalizeGasWalletChainType(gw.ChainType)
	if chainType == "BTC" || strings.TrimSpace(gw.WalletAddress) == "" {
		return s.GasWalletRepository.ClearBalanceSnapshot(ctx, gw.ID)
	}

	balanceCheck, err := s.CheckGasWalletBalance(ctx, gw)
	if err != nil {
		checkedAt := time.Now().UTC()
		if updateErr := s.GasWalletRepository.UpdateBalanceCheckError(ctx, gw.ID, checkedAt, err.Error()); updateErr != nil {
			return fmt.Errorf("failed to persist gas wallet balance error: %w", updateErr)
		}
		return nil
	}

	return s.GasWalletRepository.UpdateBalanceSnapshot(
		ctx,
		gw.ID,
		balanceCheck.CurrentBalance,
		balanceCheck.CheckedAt.UTC(),
		balanceCheck.IsBelowMinBalance,
	)
}

func (s *SweeperService) RefreshGasWalletBalances(ctx context.Context) error {
	if s == nil || s.GasWalletRepository == nil {
		return nil
	}

	wallets, err := s.GasWalletRepository.ListAll(ctx)
	if err != nil {
		return err
	}

	var refreshErrors []string
	for _, wallet := range wallets {
		if err := s.RefreshGasWalletBalance(ctx, &wallet); err != nil {
			refreshErrors = append(refreshErrors, fmt.Sprintf("%s: %v", wallet.ChainType, err))
		}
	}

	if len(refreshErrors) > 0 {
		return fmt.Errorf("failed to refresh some gas wallet balances: %s", strings.Join(refreshErrors, "; "))
	}

	return nil
}

func (s *SweeperService) loadGasWalletWithDailyReset(ctx context.Context, chainType string) (*model.GasWallet, error) {
	if s.GasWalletRepository == nil {
		return nil, nil
	}

	gw, err := s.GasWalletRepository.GetByChainType(ctx, chainType)
	if err != nil || gw == nil {
		return gw, err
	}

	now := time.Now().UTC()
	lastReset := gw.DailyResetAt.UTC()
	if lastReset.Year() == now.Year() && lastReset.YearDay() == now.YearDay() {
		return gw, nil
	}

	if err := s.GasWalletRepository.UpdateDailyReset(ctx, chainType); err != nil {
		return nil, err
	}
	gw.DailyGasUsed = decimal.Zero
	gw.DailyResetAt = now
	return gw, nil
}

// StartPoller runs periodically to find FINALIZED deposits to sweep
func (s *SweeperService) StartPoller(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	prefundTicker := time.NewTicker(30 * time.Second)
	defer prefundTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ProcessSweeps(ctx)
		case <-prefundTicker.C:
			s.CheckEnergyRentalProgress(ctx)
			s.CheckPrefundConfirmations(ctx)
			s.CheckSweepConfirmations(ctx)
		}
	}
}

func filterProcessableSweepsForMode(autoSweepEnabled bool, sweeps []*model.Sweep) []*model.Sweep {
	filtered := make([]*model.Sweep, 0, len(sweeps))
	for _, sweep := range sweeps {
		if sweep == nil {
			continue
		}
		if autoSweepEnabled || sweep.Attempts > 0 || sweep.Status != model.SweepStatusPending {
			filtered = append(filtered, sweep)
		}
	}
	return filtered
}

// ProcessSweeps fetches finalized deposits and processes them
func (s *SweeperService) ProcessSweeps(ctx context.Context) {
	if err := s.backfillFailedSweepRetries(ctx); err != nil {
		log.Printf("Failed to backfill failed sweep retries: %v", err)
	}

	// 0. Check Auto Sweep Toggle
	autoSweepEnabled := true
	val, err := s.SettingsRepository.Get(ctx, "auto_sweep_enabled")
	if err == nil && val == "false" {
		autoSweepEnabled = false
	}

	if autoSweepEnabled {
		if _, err := s.ensurePendingSweepsForFinalizedDeposits(ctx); err != nil {
			log.Printf("Failed to ensure pending sweeps for finalized deposits: %v", err)
		}
		if created, err := s.BackfillGasResidueSweeps(ctx); err != nil {
			log.Printf("Failed to backfill gas residue sweeps: %v", err)
		} else if created > 0 {
			log.Printf("Backfilled %d gas residue sweeps", created)
		}
		if closed, err := s.DepositRepository.CloseExpiredWatchWindows(ctx, time.Now().UTC()); err != nil {
			log.Printf("Failed to close expired deposit watch windows: %v", err)
		} else if closed > 0 && s.TxManager != nil {
			s.TxManager.RecordCounter(ctx, "residual_watch_closed", map[string]string{})
		}
	}

	// 1. Fetch Candidates (Pending Sweeps)
	sweeps, err := s.SweepRepository.FindPending(ctx)
	if err != nil {
		log.Printf("Failed to fetch pending sweeps: %v", err)
		return
	}

	sweeps = filterProcessableSweepsForMode(autoSweepEnabled, sweeps)
	if len(sweeps) == 0 {
		return
	}

	s.processSweepList(ctx, sweeps)
}

func (s *SweeperService) backfillFailedSweepRetries(ctx context.Context) error {
	if s == nil || s.SweepRepository == nil {
		return nil
	}

	sweeps, err := s.SweepRepository.FindAllUnswept(ctx)
	if err != nil {
		return err
	}

	for _, sw := range sweeps {
		if sw == nil || sw.Status != model.SweepStatusFailed {
			continue
		}
		if normalizeSweepPurpose(sw) != model.SweepPurposeDepositFunds {
			continue
		}
		// Pool address sweeps are refreshed via live balance on the next deposit event.
		// Don't create stale retry attempts for them — the amount would be outdated.
		if s.PoolRepository != nil {
			if isPool, _ := s.PoolRepository.IsPoolAddress(ctx, sw.FromAddress); isPool {
				continue
			}
		}
		if _, err := s.createRetryAttempt(ctx, sw); err != nil {
			log.Printf("Failed to backfill retry attempt for failed sweep %s: %v", sw.ID, err)
		}
	}

	return nil
}

func (s *SweeperService) RetryAllUnswept(ctx context.Context) error {
	sweeps, err := s.SweepRepository.FindAllUnswept(ctx)
	if err != nil {
		return err
	}
	// Run async to avoid timeout
	go s.processSweepList(context.Background(), sweeps)
	return nil
}

func (s *SweeperService) RetrySweepByID(ctx context.Context, sweepID string) error {
	if s.SweepRepository == nil {
		return fmt.Errorf("sweep repository is not configured")
	}

	sweep, err := s.SweepRepository.FindByID(ctx, sweepID)
	if err != nil {
		return err
	}
	if sweep == nil {
		return fmt.Errorf("sweep not found")
	}
	if !isRetryableSweepStatus(sweep.Status) {
		return fmt.Errorf("sweep status %s is not retryable", sweep.Status)
	}

	targetSweep := sweep
	if sweep.Status == model.SweepStatusFailed {
		retryAttempt, err := s.createRetryAttempt(ctx, sweep)
		if err != nil {
			return err
		}
		targetSweep = retryAttempt
	}

	go s.processSingleSweep(context.Background(), targetSweep)
	return nil
}

func (s *SweeperService) processSweepList(ctx context.Context, sweeps []*model.Sweep) {
	var wg sync.WaitGroup
	// Limit concurrency to avoid spamming usage limits (e.g. 5 concurrent sweeps)
	sem := make(chan struct{}, 5)

	for _, sweep := range sweeps {
		wg.Add(1)
		go func(sw *model.Sweep) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore
			targetSweep := sw
			if sw != nil && sw.Status == model.SweepStatusFailed {
				retryAttempt, err := s.createRetryAttempt(ctx, sw)
				if err != nil {
					log.Printf("Failed to create retry attempt for sweep %s: %v", sw.ID, err)
					return
				}
				targetSweep = retryAttempt
			}
			s.processSingleSweep(ctx, targetSweep)
		}(sweep)
	}
	wg.Wait()
}

func (s *SweeperService) createRetryAttempt(ctx context.Context, sweep *model.Sweep) (*model.Sweep, error) {
	if sweep == nil {
		return nil, fmt.Errorf("sweep is required")
	}

	latest, err := s.SweepRepository.FindLatestRoundByDepositID(ctx, sweep.CryptoDepositID)
	if err != nil {
		return nil, err
	}

	nextSequence := sweep.Sequence + 1
	if latest != nil && latest.Sequence >= nextSequence {
		nextSequence = latest.Sequence + 1
		if latest.Status != model.SweepStatusFailed &&
			latest.SourceReceiptsCount == sweep.SourceReceiptsCount &&
			latest.SourceTotalAmount.Equal(sweep.SourceTotalAmount) &&
			latest.Purpose == sweep.Purpose &&
			((latest.OriginSweepID == nil && sweep.OriginSweepID == nil) ||
				(latest.OriginSweepID != nil && sweep.OriginSweepID != nil && *latest.OriginSweepID == *sweep.OriginSweepID)) {
			return latest, nil
		}
	}

	retrySweep := &model.Sweep{
		CryptoDepositID:     sweep.CryptoDepositID,
		FromAddress:         sweep.FromAddress,
		ToHotWallet:         sweep.ToHotWallet,
		Amount:              sweep.Amount,
		Coin:                sweep.Coin,
		Network:             sweep.Network,
		IsToken:             sweep.IsToken,
		Sequence:            nextSequence,
		SourceReceiptsCount: sweep.SourceReceiptsCount,
		SourceTotalAmount:   sweep.SourceTotalAmount,
		Purpose:             sweep.Purpose,
		OriginSweepID:       sweep.OriginSweepID,
		Status:              model.SweepStatusPending,
		Fee:                 decimal.Zero,
		Attempts:            0,
	}
	if err := s.SweepRepository.Create(ctx, retrySweep); err != nil {
		return nil, err
	}
	return retrySweep, nil
}

const maxSweepAttempts = 50

func (s *SweeperService) processSingleSweep(ctx context.Context, sweep *model.Sweep) {
	// 2. Distributed Lock
	lockKey := "sweep:" + sweep.ID
	lockToken, locked, _ := s.TxManager.AcquireLock(ctx, lockKey, 5*time.Minute)
	if !locked {
		return
	}
	defer s.TxManager.ReleaseLock(ctx, lockKey, lockToken)

	if sweep.Attempts >= maxSweepAttempts {
		errMsg := fmt.Sprintf("sweep exceeded maximum retry limit (%d attempts)", maxSweepAttempts)
		s.markSweepFailed(ctx, sweep, errMsg)
		return
	}

	stopRenewal := make(chan struct{})
	defer close(stopRenewal)
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewal:
				return
			case <-ticker.C:
				renewed, err := s.TxManager.RenewLock(ctx, lockKey, lockToken, 5*time.Minute)
				if err != nil || !renewed {
					log.Printf("Failed to renew sweep lock %s: %v", sweep.ID, err)
				}
			}
		}
	}()

	if _, _, err := s.prepareSweepForExecution(ctx, sweep); err != nil {
		log.Printf("Sweep %s preflight failed: %v", sweep.ID, err)
		errMsg := err.Error()
		s.markSweepFailed(ctx, sweep, errMsg)
		return
	}

	// 3. Check Gas Price
	if !s.IsGasPriceOkay(ctx, sweep.Network) {
		log.Printf("Gas price high, skipping sweep for %s", sweep.ID)
		s.SweepRepository.UpdateStatus(ctx, sweep.ID, model.SweepStatusWaitingForGas, nil, nil)
		return
	}

	if err := s.SweepRepository.IncrementAttempts(ctx, sweep.ID); err != nil {
		log.Printf("Failed to increment sweep attempts for %s: %v", sweep.ID, err)
	}

	// 4. Sweep
	var txHash string
	var sweepErr error

	switch sweep.Network {
	case "BTC", "Bitcoin":
		txHash, sweepErr = s.sweepBTC(ctx, sweep)
	case "SOLANA", "Solana":
		if sweep.IsToken {
			txHash, sweepErr = s.sweepSolanaToken(ctx, sweep)
		} else {
			txHash, sweepErr = s.sweepSolana(ctx, sweep)
		}
	case "TON", "Ton":
		if sweep.IsToken {
			txHash, sweepErr = s.sweepTonJetton(ctx, sweep)
		} else {
			txHash, sweepErr = s.sweepTon(ctx, sweep)
		}
	case "TRON", "TRC20", "Tron":
		txHash, sweepErr = s.sweepTRON(ctx, sweep)
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "Ethereum", "BSC", "Polygon", "Arbitrum":
		// EVM chains
		if sweep.IsToken {
			txHash, sweepErr = s.SweepToken(ctx, sweep)
		} else {
			txHash, sweepErr = s.sweepEVMNative(ctx, sweep)
		}
	default:
		// Fallback for any other network
		if sweep.IsToken {
			txHash, sweepErr = s.SweepToken(ctx, sweep)
		} else {
			txHash, sweepErr = s.SweepNative(ctx, sweep)
		}
	}

	if sweepErr != nil {
		if errors.Is(sweepErr, ErrEnergyRentalPending) {
			log.Printf("Sweep %s waiting for energy rental", sweep.ID)
			s.SweepRepository.UpdateStatus(ctx, sweep.ID, model.SweepStatusWaitingForEnergyRental, nil, nil)
			return
		}
		if errors.Is(sweepErr, ErrPrefundInitiated) {
			if strings.TrimSpace(txHash) == "" {
				txHash = prefundTxHashFromError(sweepErr)
			}
			log.Printf("Sweep %s waiting for prefund: %s", sweep.ID, txHash)
			s.SweepRepository.UpdateStatus(ctx, sweep.ID, model.SweepStatusWaitingForPrefund, &txHash, nil)
			return
		}

		log.Printf("Sweep failed for %s: %v", sweep.ID, sweepErr)
		errMsg := sweepErr.Error()

		status := model.SweepStatusFailed
		if errors.Is(sweepErr, ErrSweepWaitingForGas) {
			status = model.SweepStatusWaitingForGas
		} else if strings.Contains(errMsg, "insufficient gas") {
			status = model.SweepStatusSkippedNoGas
		} else if strings.Contains(errMsg, "insufficient funds for prefund") {
			status = model.SweepStatusWaitingForGas
		}

		if status == model.SweepStatusFailed {
			s.markSweepFailed(ctx, sweep, errMsg)
			return
		}

		s.SweepRepository.UpdateStatus(ctx, sweep.ID, status, nil, &errMsg)
		return
	}

	log.Printf("Sweep success for %s: %s", sweep.ID, txHash)
	s.SweepRepository.UpdateStatus(ctx, sweep.ID, model.SweepStatusBroadcasting, &txHash, nil)
}

func (s *SweeperService) SetAutoSweep(ctx context.Context, enabled bool) error {
	val := "false"
	if enabled {
		val = "true"
	}
	return s.SettingsRepository.Set(ctx, "auto_sweep_enabled", val)
}

func (s *SweeperService) IsAutoSweepEnabled(ctx context.Context) (bool, error) {
	val, err := s.SettingsRepository.Get(ctx, "auto_sweep_enabled")
	if err != nil {
		return false, err
	}
	// Default to true if not set (or decide policy)
	// Actually user implicitly wants off by default until enabled? Or persistent?
	// Given "Don't assume... there should be a switch", let's assume default false if missing, or check logic in ProcessSweeps.
	// In ProcessSweeps we treated missing as true. Let's keep consistent.
	if val == "false" {
		return false, nil
	}
	return true, nil
}

func (s *SweeperService) RescanUnsweptFunds(ctx context.Context) (*SweepRescanResult, error) {
	// Check all addresses for leftover/unswept funds.
	// We iterate deposits that should be swept but aren't currently tracked in a successful sweep.
	return s.ensurePendingSweepsForFinalizedDeposits(ctx)
}

func isSweepEligibleDepositStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "CONFIRMED", "FINALIZED":
		return true
	default:
		return false
	}
}

func (s *SweeperService) ensurePendingSweepsForFinalizedDeposits(ctx context.Context) (*SweepRescanResult, error) {
	deposits, err := s.DepositRepository.FindSweepEligibleWithUnsweptDelta(ctx)
	if err != nil {
		return nil, err
	}

	result := &SweepRescanResult{
		MatchedDeposits: len(deposits),
	}

	// Deduplicate by deposit_address: pool addresses are reused across many deposits,
	// so one UpsertPoolBalanceSweep call per address is enough per cycle.
	seenAddresses := make(map[string]bool)

	for _, dep := range deposits {
		addrKey := dep.Network + ":" + dep.DepositAddress
		if seenAddresses[addrKey] {
			continue
		}
		seenAddresses[addrKey] = true
		result.MatchedAddresses++

		log.Printf("Rescan: Processing unswept deposit %s (%s %s)", dep.ID, dep.Coin, dep.DetectedAmount.Decimal)
		created, err := s.EnsureSweepExists(ctx, dep)
		if err != nil {
			log.Printf("Rescan: Failed to ensure sweep for %s: %v", dep.ID, err)
			continue
		}
		if created {
			result.QueuedSweeps++
		}
	}

	return result, nil
}

func (s *SweeperService) EnsureSweepExists(ctx context.Context, dep *model.CryptoDeposit) (bool, error) {
	if dep == nil || !isSweepEligibleDepositStatus(dep.Status) {
		return false, nil
	}

	// Pool addresses use live-balance sweeps instead of per-deposit receipt-based sweeps.
	if s.PoolRepository != nil && strings.ToUpper(strings.TrimSpace(dep.Network)) == "TRC20" {
		addr := strings.TrimSpace(dep.DepositAddress)
		if addr == "" {
			addr = strings.TrimSpace(dep.PaymentAddress)
		}
		isPool, poolErr := s.PoolRepository.IsPoolAddress(ctx, addr)
		if poolErr != nil {
			log.Printf("EnsureSweepExists: pool address check failed for %s: %v", addr, poolErr)
		} else if isPool {
			return s.UpsertPoolBalanceSweep(ctx, dep)
		}
	}

	receiptCount, err := s.DepositRepository.CountIncomingTransfers(ctx, dep.ID)
	if err != nil {
		return false, err
	}

	totalReceived, err := s.DepositRepository.SumIncomingTransfers(ctx, dep.ID)
	if err != nil {
		return false, err
	}
	if receiptCount == 0 && dep.DetectedAmount.Valid && dep.DetectedAmount.Decimal.GreaterThan(decimal.Zero) {
		totalReceived = dep.DetectedAmount.Decimal
	}
	if !totalReceived.GreaterThan(decimal.Zero) {
		return false, nil
	}

	existingFrontier, err := s.SweepRepository.FindBlockingDepositRoundByReceiptFrontier(ctx, dep.ID, receiptCount, totalReceived)
	if err != nil {
		return false, err
	}
	if existingFrontier != nil {
		if existingFrontier.Status == model.SweepStatusWaitingForPrefund && s.TxManager != nil {
			s.TxManager.RecordCounter(ctx, "sweep_prefund_replayed", map[string]string{
				"network": existingFrontier.Network,
			})
		}
		return false, nil
	}

	latestFrontier, err := s.SweepRepository.FindLatestRoundByReceiptFrontierAnyStatus(ctx, dep.ID, receiptCount, totalReceived)
	if err != nil {
		return false, err
	}
	if latestFrontier != nil && latestFrontier.Status == model.SweepStatusFailed && s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "failed_frontier_requeued", map[string]string{
			"network": dep.Network,
		})
	}

	coveredAmount, err := s.SweepRepository.SumCoveredAmountByDepositID(ctx, dep.ID)
	if err != nil {
		return false, err
	}

	unsweptDelta := totalReceived.Sub(coveredAmount)
	if !unsweptDelta.GreaterThan(decimal.Zero) {
		return false, nil
	}

	wallet, err := s.HdWalletRepository.FindByID(ctx, dep.HDWalletID)
	if err != nil {
		return false, fmt.Errorf("failed to find wallet config: %w", err)
	}
	if wallet == nil || !wallet.IsEnabled || strings.TrimSpace(wallet.HotWalletAddress) == "" {
		return false, fmt.Errorf("no active hot wallet configured for %s/%s", dep.Coin, dep.Network)
	}

	latest, err := s.SweepRepository.FindLatestRoundByDepositID(ctx, dep.ID)
	if err != nil {
		return false, err
	}
	nextSequence := 1
	if latest != nil && latest.Sequence >= nextSequence {
		nextSequence = latest.Sequence + 1
	}

	sweep := &model.Sweep{
		CryptoDepositID:     dep.ID,
		FromAddress:         dep.DepositAddress,
		ToHotWallet:         wallet.HotWalletAddress,
		Amount:              unsweptDelta,
		Coin:                dep.Coin,
		Network:             dep.Network,
		IsToken:             wallet.ContractAddress != nil && strings.TrimSpace(*wallet.ContractAddress) != "",
		Sequence:            nextSequence,
		SourceReceiptsCount: receiptCount,
		SourceTotalAmount:   totalReceived,
		Purpose:             model.SweepPurposeDepositFunds,
		Status:              model.SweepStatusPending,
	}

	if err := s.SweepRepository.Create(ctx, sweep); err != nil {
		// A unique constraint violation means a concurrent goroutine already created this sweep
		// (the SELECT-then-INSERT race). Treat as a no-op — the other goroutine owns it.
		if isUniqueViolation(err) {
			return false, nil
		}
		return false, err
	}

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "residual_sweep_round_created", map[string]string{
			"network": dep.Network,
		})
	}

	return true, nil
}

func (s *SweeperService) BackfillGasResidueSweeps(ctx context.Context) (int, error) {
	if s.SweepRepository == nil {
		return 0, nil
	}

	origins, err := s.SweepRepository.ListConfirmedDepositFundSweepsWithoutResidue(ctx)
	if err != nil {
		return 0, err
	}

	createdCount := 0
	for _, origin := range origins {
		created, ensureErr := s.EnsureGasResidueSweepForOrigin(ctx, origin)
		if ensureErr != nil {
			log.Printf("Failed to ensure gas residue sweep for origin %s: %v", origin.ID, ensureErr)
			continue
		}
		if created {
			createdCount++
		}
	}

	return createdCount, nil
}

func (s *SweeperService) EnsureGasResidueSweepForOrigin(ctx context.Context, origin *model.Sweep) (bool, error) {
	if origin == nil || origin.Status != model.SweepStatusConfirmed || !needsGasResidueSweep(origin) {
		return false, nil
	}

	existingResidue, err := s.SweepRepository.FindActiveOrConfirmedResidueByOriginSweepID(ctx, origin.ID)
	if err != nil {
		return false, err
	}
	if existingResidue != nil {
		return false, nil
	}

	deposit, err := s.DepositRepository.FindByID(ctx, origin.CryptoDepositID)
	if err != nil {
		return false, fmt.Errorf("failed to load deposit for residue sweep: %w", err)
	}
	if deposit == nil {
		return false, fmt.Errorf("deposit %s not found for residue sweep", origin.CryptoDepositID)
	}

	walletCfg, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil {
		return false, fmt.Errorf("failed to load wallet config for residue sweep: %w", err)
	}
	if walletCfg == nil || !walletCfg.IsEnabled || strings.TrimSpace(walletCfg.HotWalletAddress) == "" {
		return false, fmt.Errorf("missing hot wallet configuration for residue sweep")
	}

	recoverableAmount, coin, err := s.estimateGasResidueAmount(ctx, origin, walletCfg)
	if err != nil {
		return false, err
	}
	if !recoverableAmount.GreaterThan(decimal.Zero) {
		return false, nil
	}

	latest, err := s.SweepRepository.FindLatestRoundByDepositID(ctx, origin.CryptoDepositID)
	if err != nil {
		return false, err
	}
	nextSequence := 1
	if latest != nil && latest.Sequence >= nextSequence {
		nextSequence = latest.Sequence + 1
	}

	originID := origin.ID
	residueSweep := &model.Sweep{
		CryptoDepositID:     origin.CryptoDepositID,
		FromAddress:         origin.FromAddress,
		ToHotWallet:         walletCfg.HotWalletAddress,
		Amount:              recoverableAmount,
		Coin:                coin,
		Network:             origin.Network,
		IsToken:             false,
		Sequence:            nextSequence,
		SourceReceiptsCount: origin.SourceReceiptsCount,
		SourceTotalAmount:   origin.SourceTotalAmount,
		Purpose:             model.SweepPurposeGasResidue,
		OriginSweepID:       &originID,
		Status:              model.SweepStatusPending,
	}

	if err := s.SweepRepository.Create(ctx, residueSweep); err != nil {
		return false, err
	}

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "gas_residue_sweep_created", map[string]string{
			"network": origin.Network,
		})
	}

	return true, nil
}

func (s *SweeperService) unsweptNativeCustomerDelta(ctx context.Context, origin *model.Sweep) (decimal.Decimal, error) {
	if origin == nil || origin.IsToken || !origin.SourceTotalAmount.GreaterThan(decimal.Zero) {
		return decimal.Zero, nil
	}
	if s.SweepRepository == nil {
		return decimal.Zero, nil
	}

	coveredAmount, err := s.SweepRepository.SumCoveredAmountByDepositID(ctx, origin.CryptoDepositID)
	if err != nil {
		return decimal.Zero, err
	}
	if coveredAmount.GreaterThanOrEqual(origin.SourceTotalAmount) {
		return decimal.Zero, nil
	}
	return origin.SourceTotalAmount.Sub(coveredAmount), nil
}

func (s *SweeperService) estimateGasResidueAmount(ctx context.Context, origin *model.Sweep, walletCfg *model.HDWallet) (decimal.Decimal, string, error) {
	if origin == nil {
		return decimal.Zero, "", nil
	}

	unsweptCustomerDelta := decimal.Zero
	if !origin.IsToken {
		var err error
		unsweptCustomerDelta, err = s.unsweptNativeCustomerDelta(ctx, origin)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), err
		}
	}

	switch strings.ToUpper(strings.TrimSpace(origin.Network)) {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "ETHEREUM", "BSC", "POL", "ARB":
		amount, err := s.estimateEVMNativeRecoverableAmount(ctx, origin.Network, origin.FromAddress, walletCfg.HotWalletAddress, unsweptCustomerDelta)
		return amount, nativeCoinForNetwork(origin.Network), err
	case "TRC20", "TRON":
		balanceSun, err := s.tronAccountBalanceSun(ctx, origin.FromAddress)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("failed to get TRON balance: %w", err)
		}

		reserveSun := tronPrefundTransferFeeSun()
		if origin.IsToken {
			reserveSun = tronTokenSweepReserveSun() + tronPrefundTransferFeeSun()
		}

		unsweptDeltaSun := int64(0)
		if unsweptCustomerDelta.GreaterThan(decimal.Zero) {
			unsweptDeltaSun, err = exactTRXSunFromSweepAmount(unsweptCustomerDelta)
			if err != nil {
				return decimal.Zero, nativeCoinForNetwork(origin.Network), err
			}
		}

		if balanceSun <= reserveSun+unsweptDeltaSun {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), nil
		}

		recoverableSun := balanceSun - reserveSun - unsweptDeltaSun
		if recoverableSun <= 0 {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), nil
		}

		return decimal.NewFromInt(recoverableSun).Shift(-6), nativeCoinForNetwork(origin.Network), nil
	case "SOLANA":
		client := s.getSolanaRPCClient()
		fromPub, err := solana.PublicKeyFromBase58(origin.FromAddress)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("invalid solana residue source address: %w", err)
		}

		balance, err := client.GetBalance(ctx, fromPub, rpc.CommitmentFinalized)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("failed to get solana residue balance: %w", err)
		}

		fee := solanaFeeEstimateLamports()
		unsweptDeltaLamports := uint64(0)
		if unsweptCustomerDelta.GreaterThan(decimal.Zero) {
			unsweptDeltaLamports, err = exactUint64UnitsFromSweepAmount(unsweptCustomerDelta, 9, "SOL")
			if err != nil {
				return decimal.Zero, nativeCoinForNetwork(origin.Network), err
			}
		}
		if balance.Value <= fee+unsweptDeltaLamports {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), nil
		}

		return decimal.NewFromUint64(balance.Value - fee - unsweptDeltaLamports).Shift(-9), nativeCoinForNetwork(origin.Network), nil
	case "TON":
		api, err := s.getTonAPIClient(ctx)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), err
		}

		addr, err := address.ParseAddr(origin.FromAddress)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("invalid TON residue source address: %w", err)
		}

		master, err := api.CurrentMasterchainInfo(ctx)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("failed to get TON masterchain info: %w", err)
		}

		acc, err := api.GetAccount(ctx, master, addr)
		if err != nil {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), fmt.Errorf("failed to get TON residue balance: %w", err)
		}

		fee := tonFeeEstimateNano()
		balance := tonAccountBalanceNano(acc)
		unsweptDeltaNano := big.NewInt(0)
		if unsweptCustomerDelta.GreaterThan(decimal.Zero) {
			unsweptDeltaNano, err = exactNativeUnitsFromSweepAmount(unsweptCustomerDelta, 9, "TON")
			if err != nil {
				return decimal.Zero, nativeCoinForNetwork(origin.Network), err
			}
		}
		requiredFloor := new(big.Int).Add(new(big.Int).Set(fee), unsweptDeltaNano)
		if balance.Cmp(requiredFloor) <= 0 {
			return decimal.Zero, nativeCoinForNetwork(origin.Network), nil
		}

		return decimal.NewFromBigInt(new(big.Int).Sub(balance, requiredFloor), -9), nativeCoinForNetwork(origin.Network), nil
	default:
		return decimal.Zero, nativeCoinForNetwork(origin.Network), nil
	}
}

func (s *SweeperService) estimateEVMNativeRecoverableAmount(ctx context.Context, network, fromAddress, toAddress string, unsweptCustomerDelta decimal.Decimal) (decimal.Decimal, error) {
	client, err := s.getEVMClient(network)
	if err != nil {
		return decimal.Zero, err
	}

	fromAddr := common.HexToAddress(fromAddress)
	toAddr := common.HexToAddress(toAddress)
	callMsg := ethereum.CallMsg{
		From: fromAddr,
		To:   &toAddr,
	}

	gasLimit, err := client.EstimateGas(ctx, callMsg)
	if err != nil {
		gasLimit = 21000
	} else {
		gasLimit += gasLimit / 10
	}

	_, feeCap, _, err := s.currentEVMFeeCaps(ctx, client)
	if err != nil {
		return decimal.Zero, err
	}

	balance, err := client.BalanceAt(ctx, fromAddr, nil)
	if err != nil {
		return decimal.Zero, err
	}

	cost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	unsweptUnits := big.NewInt(0)
	if unsweptCustomerDelta.GreaterThan(decimal.Zero) {
		unsweptUnits, err = exactNativeUnitsFromSweepAmount(unsweptCustomerDelta, 18, nativeCoinForNetwork(network))
		if err != nil {
			return decimal.Zero, err
		}
	}
	recoverable := new(big.Int).Sub(balance, cost)
	recoverable.Sub(recoverable, unsweptUnits)
	if recoverable.Sign() <= 0 {
		return decimal.Zero, nil
	}

	return decimal.NewFromBigInt(recoverable, -18), nil
}

func (s *SweeperService) prepareSweepForExecution(ctx context.Context, sweep *model.Sweep) (*model.CryptoDeposit, *model.HDWallet, error) {
	if sweep == nil {
		return nil, nil, fmt.Errorf("sweep is required")
	}

	deposit, err := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reload deposit: %w", err)
	}
	if deposit == nil {
		return nil, nil, fmt.Errorf("deposit %s not found", sweep.CryptoDepositID)
	}

	wallet, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil {
		return deposit, nil, fmt.Errorf("failed to reload wallet config: %w", err)
	}
	if wallet == nil || !wallet.IsEnabled {
		s.emitAlert(ctx, fmt.Sprintf("Sweep %s for deposit %s blocked because wallet config is disabled or missing.", sweep.ID, deposit.ID))
		return deposit, wallet, fmt.Errorf("wallet config is disabled or missing")
	}
	if strings.TrimSpace(wallet.HotWalletAddress) == "" {
		s.emitAlert(ctx, fmt.Sprintf("Sweep %s for deposit %s blocked because hot wallet address is empty.", sweep.ID, deposit.ID))
		return deposit, wallet, fmt.Errorf("hot wallet address is empty")
	}
	if err := ValidateChainAddress(wallet.Network, wallet.HotWalletAddress, s.HDWallet.Params); err != nil {
		s.emitAlert(ctx, fmt.Sprintf("Sweep %s for deposit %s blocked because hot wallet address is invalid: %v", sweep.ID, deposit.ID, err))
		return deposit, wallet, fmt.Errorf("invalid hot wallet address: %w", err)
	}

	expectedFrom := strings.TrimSpace(deposit.DepositAddress)
	if expectedFrom == "" {
		expectedFrom = strings.TrimSpace(deposit.PaymentAddress)
	}
	if expectedFrom != "" && normalizeWatchedAddress(sweep.Network, sweep.FromAddress) != normalizeWatchedAddress(sweep.Network, expectedFrom) {
		if s.TxManager != nil {
			s.TxManager.RecordCounter(ctx, "sweep_source_mismatch", map[string]string{
				"network": sweep.Network,
			})
		}
		s.emitAlert(ctx, fmt.Sprintf("Sweep %s for deposit %s blocked because source address %s does not match expected deposit address %s.", sweep.ID, deposit.ID, sweep.FromAddress, expectedFrom))
		return deposit, wallet, fmt.Errorf("sweep source address mismatch")
	}

	if normalizeWatchedAddress(sweep.Network, sweep.ToHotWallet) != normalizeWatchedAddress(sweep.Network, wallet.HotWalletAddress) {
		if s.TxManager != nil {
			s.TxManager.RecordCounter(ctx, "sweep_destination_mismatch", map[string]string{
				"network": sweep.Network,
			})
		}
		if err := s.SweepRepository.UpdateDestination(ctx, sweep.ID, wallet.HotWalletAddress); err != nil {
			return deposit, wallet, fmt.Errorf("failed to refresh sweep destination: %w", err)
		}
		sweep.ToHotWallet = wallet.HotWalletAddress
		if s.TxManager != nil {
			s.TxManager.RecordCounter(ctx, "sweep_destination_refreshed", map[string]string{
				"network": sweep.Network,
			})
		}
	}

	return deposit, wallet, nil
}

func (s *SweeperService) IsGasPriceOkay(ctx context.Context, chainType string) bool {
	// 1. Get Gas Config
	gw, err := s.GasWalletRepository.GetByChainType(ctx, chainType)
	if err != nil || gw == nil {
		return true // Default open if no config
	}

	if !gw.IsEnabled {
		return true
	}

	switch repository.NormalizeGasWalletChainType(chainType) {
	case "EVM":
		client, err := s.getEVMClient(chainType)
		if err != nil {
			log.Printf("Failed to get client for %s: %v", chainType, err)
			return true
		}
		_, feeCap, _, err := s.currentEVMFeeCaps(ctx, client)
		if err != nil {
			log.Printf("Failed to estimate fee caps for %s: %v", chainType, err)
			return true
		}
		if err := validateEVMGasConstraints(gw, feeCap, 0); err != nil {
			return false
		}
	case "SOLANA":
		return true
	case "TON":
		return true
	case "BTC":
		feeRate, err := s.currentBTCFeeRate()
		if err == nil && gw.MaxGasPrice.GreaterThan(decimal.Zero) {
			return !decimal.NewFromInt(feeRate).GreaterThan(gw.MaxGasPrice)
		}
		return true
	case "TRON":
		return true
	}

	return true
}

func maxConfiguredGasPriceWei(gw *model.GasWallet) *big.Int {
	if gw == nil || !gw.MaxGasPrice.GreaterThan(decimal.Zero) {
		return nil
	}

	return gw.MaxGasPrice.Mul(decimal.NewFromInt(1_000_000_000)).BigInt()
}

func estimatedEVMGasCostNative(gasLimit uint64, feeCap *big.Int) decimal.Decimal {
	if gasLimit == 0 || feeCap == nil {
		return decimal.Zero
	}

	costWei := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	return decimal.NewFromBigInt(costWei, -18)
}

func validateEVMGasConstraints(gw *model.GasWallet, feeCap *big.Int, gasLimit uint64) error {
	if gw == nil || !gw.IsEnabled || feeCap == nil {
		return nil
	}

	maxGasPriceWei := maxConfiguredGasPriceWei(gw)
	if maxGasPriceWei != nil && feeCap.Cmp(maxGasPriceWei) > 0 {
		return fmt.Errorf(
			"%w: current fee cap %s gwei exceeds configured max %s gwei",
			ErrSweepWaitingForGas,
			decimal.NewFromBigInt(feeCap, -9).String(),
			gw.MaxGasPrice.String(),
		)
	}

	if gw.MaxGasPerSweep.GreaterThan(decimal.Zero) && gasLimit > 0 {
		gasCost := estimatedEVMGasCostNative(gasLimit, feeCap)
		if gasCost.GreaterThan(gw.MaxGasPerSweep) {
			return fmt.Errorf(
				"%w: estimated gas cost %s exceeds configured max per sweep %s",
				ErrSweepWaitingForGas,
				gasCost.String(),
				gw.MaxGasPerSweep.String(),
			)
		}
	}

	return nil
}

func estimateBTCSweepFeeSats(inputCount, outputCount int, feeRateSatVB int64) int64 {
	if inputCount <= 0 || outputCount <= 0 || feeRateSatVB <= 0 {
		return 0
	}

	estimatedSize := 10 + (inputCount * 180) + (outputCount * 34)
	return int64(estimatedSize) * feeRateSatVB
}

func estimatedBTCFeeAmount(feeSats int64) decimal.Decimal {
	return decimal.NewFromInt(feeSats).Shift(-8)
}

func validateBTCFeeConstraints(gw *model.GasWallet, feeRateSatVB int64, feeSats int64) error {
	if gw == nil || !gw.IsEnabled {
		return nil
	}

	if gw.MaxGasPrice.GreaterThan(decimal.Zero) && decimal.NewFromInt(feeRateSatVB).GreaterThan(gw.MaxGasPrice) {
		return fmt.Errorf(
			"%w: fee rate %d sat/vB exceeds configured max %s sat/vB",
			ErrSweepWaitingForGas,
			feeRateSatVB,
			gw.MaxGasPrice.String(),
		)
	}

	feeAmount := estimatedBTCFeeAmount(feeSats)
	if gw.MaxGasPerSweep.GreaterThan(decimal.Zero) && feeAmount.GreaterThan(gw.MaxGasPerSweep) {
		return fmt.Errorf(
			"%w: estimated BTC fee %s exceeds configured max per sweep %s",
			ErrSweepWaitingForGas,
			feeAmount.String(),
			gw.MaxGasPerSweep.String(),
		)
	}

	if gw.DailyGasLimit.GreaterThan(decimal.Zero) && gw.DailyGasUsed.Add(feeAmount).GreaterThan(gw.DailyGasLimit) {
		return fmt.Errorf(
			"%w: daily BTC fee budget exceeded (%s + %s > %s)",
			ErrSweepWaitingForGas,
			gw.DailyGasUsed.String(),
			feeAmount.String(),
			gw.DailyGasLimit.String(),
		)
	}

	return nil
}

func validateGasWalletOutlay(gw *model.GasWallet, outlay decimal.Decimal) error {
	if gw == nil || !gw.IsEnabled || !outlay.GreaterThan(decimal.Zero) {
		return nil
	}

	if gw.MaxGasPerSweep.GreaterThan(decimal.Zero) && outlay.GreaterThan(gw.MaxGasPerSweep) {
		return fmt.Errorf(
			"%w: estimated outlay %s exceeds configured max per sweep %s",
			ErrSweepWaitingForGas,
			outlay.String(),
			gw.MaxGasPerSweep.String(),
		)
	}

	if gw.DailyGasLimit.GreaterThan(decimal.Zero) && gw.DailyGasUsed.Add(outlay).GreaterThan(gw.DailyGasLimit) {
		return fmt.Errorf(
			"%w: daily gas budget exceeded (%s + %s > %s)",
			ErrSweepWaitingForGas,
			gw.DailyGasUsed.String(),
			outlay.String(),
			gw.DailyGasLimit.String(),
		)
	}

	return nil
}

func tronTokenSweepReserveSun() int64 {
	reserveSun := int64(5_000_000)
	if reserveEnv := os.Getenv("TRON_TOKEN_SWEEP_RESERVE_SUN"); reserveEnv != "" {
		if parsed, err := strconv.ParseInt(reserveEnv, 10, 64); err == nil && parsed > 0 {
			reserveSun = parsed
		}
	}
	return reserveSun
}

func tronPrefundTransferFeeSun() int64 {
	feeSun := int64(1_000_000)
	if feeEnv := os.Getenv("TRON_PREFUND_TRANSFER_FEE_SUN"); feeEnv != "" {
		if parsed, err := strconv.ParseInt(feeEnv, 10, 64); err == nil && parsed >= 0 {
			feeSun = parsed
		}
	}
	return feeSun
}

func tronTokenSweepMinBalanceSun() int64 {
	minBalanceSun := tronEnergyActivationAmountSun()
	if minBalanceEnv := os.Getenv("TRON_TOKEN_SWEEP_MIN_BALANCE_SUN"); minBalanceEnv != "" {
		if parsed, err := strconv.ParseInt(minBalanceEnv, 10, 64); err == nil && parsed >= 0 {
			minBalanceSun = parsed
		}
	}
	return minBalanceSun
}

func tronEnergyActivationAmountSun() int64 {
	amountSun := int64(100_000)
	if activationEnv := os.Getenv("TRON_ENERGY_ACTIVATION_AMOUNT_SUN"); activationEnv != "" {
		if parsed, err := strconv.ParseInt(activationEnv, 10, 64); err == nil && parsed > 0 {
			amountSun = parsed
		}
	}
	return amountSun
}

func tronEnergyOrderDurationHours() int {
	durationHours := 4
	if durationEnv := os.Getenv("TRON_ENERGY_ORDER_DURATION_HOURS"); durationEnv != "" {
		if parsed, err := strconv.Atoi(durationEnv); err == nil && parsed > 0 {
			durationHours = parsed
		}
	}
	return durationHours
}

func tronEnergyRentalTimeout() time.Duration {
	timeout := 65 * time.Minute
	if timeoutEnv := os.Getenv("TRON_ENERGY_ORDER_TIMEOUT_SECONDS"); timeoutEnv != "" {
		if parsed, err := strconv.Atoi(timeoutEnv); err == nil && parsed > 0 {
			timeout = time.Duration(parsed) * time.Second
		}
	}
	return timeout
}

func tronEnergyOrderTransferCount() int {
	count := 1
	if value := os.Getenv("TRON_ENERGY_ORDER_TIMES"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			count = parsed
		}
	}
	return count
}

func tronEnergyOrderEnergyAmount(transferCount int) int64 {
	energyPerTransfer := int64(65_000)
	if value := os.Getenv("TRON_ENERGY_AMOUNT_PER_TRANSFER"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			energyPerTransfer = parsed
		}
	}
	if transferCount <= 0 {
		transferCount = 1
	}
	return energyPerTransfer * int64(transferCount)
}

func estimateTronEnergyOrderCostSun(priceInfo *TronEnergyPriceInfo, transferCount, durationHours int) (int64, error) {
	if priceInfo == nil {
		return 0, fmt.Errorf("TRON energy price info is required")
	}
	if priceInfo.ActivatedPriceSun <= 0 {
		return 0, fmt.Errorf("TRON energy activated price is unavailable")
	}
	if transferCount <= 0 {
		return 0, fmt.Errorf("TRON energy transfer count must be positive")
	}
	if durationHours <= 0 {
		return 0, fmt.Errorf("TRON energy duration must be positive")
	}

	multiplier := 1.0
	foundTier := false
	for _, tier := range priceInfo.DurationTiers {
		if tier.DurationHours == durationHours {
			multiplier = tier.Multiplier
			foundTier = true
			break
		}
	}
	if !foundTier && len(priceInfo.DurationTiers) == 1 {
		multiplier = priceInfo.DurationTiers[0].Multiplier
		foundTier = true
	}
	if !foundTier {
		return 0, fmt.Errorf("TRON energy provider does not offer %d-hour duration", durationHours)
	}

	total := float64(priceInfo.ActivatedPriceSun) * float64(transferCount) * multiplier
	return int64(total + 0.5), nil
}

func isTronEnergyRentalActive(sweep *model.Sweep, now time.Time) bool {
	if sweep == nil || sweep.ProviderName == nil || sweep.ProviderStatus == nil {
		return false
	}
	if strings.TrimSpace(*sweep.ProviderName) == "" {
		return false
	}
	if strings.TrimSpace(*sweep.ProviderStatus) != string(TronEnergyRentalStatusActive) {
		return false
	}
	if sweep.EnergyRentalExpiresAt == nil {
		return true
	}
	return sweep.EnergyRentalExpiresAt.After(now)
}

func tronEnergyOrderNumber(sweep *model.Sweep) string {
	if sweep == nil {
		return ""
	}
	if sweep.ProviderOrderNo != nil && strings.TrimSpace(*sweep.ProviderOrderNo) != "" {
		return strings.TrimSpace(*sweep.ProviderOrderNo)
	}
	attempt := sweep.Attempts
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("sweep-%s-a%d", sweep.ID, attempt)
}

func (s *SweeperService) requiresTronEnergyRental(sweep *model.Sweep) bool {
	if sweep == nil || !isDepositFundSweep(sweep) || !sweep.IsToken {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(sweep.Network)) {
	case "TRON", "TRC20":
		return true
	default:
		return false
	}
}

func stringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	v := value
	return &v
}

func hasTronEnergyRentalExpired(sweep *model.Sweep, now time.Time) bool {
	if sweep == nil || sweep.EnergyRentalExpiresAt == nil {
		return false
	}
	return !sweep.EnergyRentalExpiresAt.After(now)
}

func tronEnergyCreateCooldown() time.Duration {
	return 10 * time.Minute
}

func hasSufficientTronEnergy(availableEnergy, requiredEnergy int64) bool {
	if requiredEnergy <= 0 {
		return true
	}
	if availableEnergy >= requiredEnergy {
		return true
	}

	// Tron resource APIs can differ by a tiny amount from provider-advertised energy
	// packages. Treat very small gaps as sufficient to avoid deadlocking a sweep on a
	// 1-unit mismatch like 130999 vs 131000.
	const energySlack = int64(1000)
	return availableEnergy+energySlack >= requiredEnergy
}

func (s *SweeperService) resetTronEnergyRentalForRetry(ctx context.Context, sweep *model.Sweep, errText string) error {
	pendingStatus := model.SweepStatusPending
	update := repository.SweepProviderStateUpdate{
		Status:             &pendingStatus,
		ProviderLastError:  &errText,
		ClearProviderState: true,
	}
	return s.SweepRepository.UpdateProviderState(ctx, sweep.ID, update)
}

func (s *SweeperService) ensureTronEnergyRental(ctx context.Context, sweep *model.Sweep) error {
	if !s.requiresTronEnergyRental(sweep) {
		return nil
	}
	if s.TronEnergyProvider == nil || !s.TronEnergyProvider.IsConfigured() {
		return fmt.Errorf("TRON energy rental provider is not configured")
	}

	now := time.Now().UTC()
	if isTronEnergyRentalActive(sweep, now) {
		return nil
	}

	checkingStatus := model.SweepStatusCheckingActivation
	_ = s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
		Status: &checkingStatus,
	})

	activated, err := s.tronAccountActivated(ctx, sweep.FromAddress)
	if err != nil {
		return fmt.Errorf("failed to check TRON address activation: %w", err)
	}
	if !activated {
		errText := "TRON address is not activated yet"
		_ = s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
			ProviderLastError:  &errText,
			ClearProviderState: true,
		})
		activationHash, prefundErr := s.PrefundGasAddress(ctx, sweep.FromAddress, big.NewInt(tronEnergyActivationAmountSun()), "TRON")
		if prefundErr != nil {
			return fmt.Errorf("failed to activate TRON address before energy rental: %w", prefundErr)
		}
		log.Printf("Activated TRON address %s for sweep %s via prefund %s before requesting energy rental", sweep.FromAddress, sweep.ID, activationHash)
		return prefundInitiatedWithHash(activationHash)
	}

	requiredEnergy := tronEnergyOrderEnergyAmount(tronEnergyOrderTransferCount())
	availableEnergy, energyErr := s.tronAccountAvailableEnergy(ctx, sweep.FromAddress)
	if energyErr != nil {
		log.Printf("Sweep %s: failed to check on-chain energy for %s: %v", sweep.ID, sweep.FromAddress, energyErr)
	} else if hasSufficientTronEnergy(availableEnergy, requiredEnergy) {
		log.Printf("Sweep %s: address %s already has sufficient on-chain energy (%d >= %d), skipping rental", sweep.ID, sweep.FromAddress, availableEnergy, requiredEnergy)
		return nil
	}

	if sweep.ProviderOrderID != nil && strings.TrimSpace(*sweep.ProviderOrderID) != "" &&
		sweep.ProviderStatus != nil &&
		strings.TrimSpace(*sweep.ProviderStatus) == string(TronEnergyRentalStatusPending) &&
		!hasTronEnergyRentalExpired(sweep, now) {
		return ErrEnergyRentalPending
	}

	orderNo := tronEnergyOrderNumber(sweep)
	if sweep.ProviderOrderNo != nil &&
		strings.TrimSpace(*sweep.ProviderOrderNo) == orderNo &&
		(sweep.ProviderOrderID == nil || strings.TrimSpace(*sweep.ProviderOrderID) == "") &&
		!hasTronEnergyRentalExpired(sweep, now) &&
		now.Sub(sweep.UpdatedAt) < tronEnergyCreateCooldown() {
		return ErrEnergyRentalPending
	}
	if existingOrder := recoverTronEnergyOrderByOrderNo(ctx, s.TronEnergyProvider, orderNo); existingOrder != nil && !hasTronEnergyRentalExpired(sweep, now) {
		orderStatus := string(existingOrder.NormalizedStatus)
		providerName := existingOrder.ProviderName
		waitingStatus := model.SweepStatusWaitingForEnergyRental
		expiresAt := now.Add(tronEnergyRentalTimeout())
		if existingOrder.NormalizedStatus == TronEnergyRentalStatusActive {
			waitingStatus = model.SweepStatusProcessingTransaction
		}
		if err := s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
			ProviderName:          &providerName,
			ProviderOrderID:       stringPtr(existingOrder.ProviderOrderID),
			ProviderOrderNo:       stringPtr(existingOrder.ProviderOrderNo),
			ProviderStatus:        &orderStatus,
			ProviderLastError:     nil,
			ProviderMetadataJSON:  stringPtr(existingOrder.MetadataJSON),
			EnergyRentalExpiresAt: &expiresAt,
			Status:                &waitingStatus,
		}); err != nil {
			return fmt.Errorf("failed to persist recovered TRON energy rental state: %w", err)
		}
		sweep.ProviderName = stringPtr(providerName)
		sweep.ProviderOrderID = stringPtr(existingOrder.ProviderOrderID)
		sweep.ProviderOrderNo = stringPtr(existingOrder.ProviderOrderNo)
		sweep.ProviderStatus = &orderStatus
		sweep.ProviderLastError = nil
		sweep.ProviderMetadataJSON = stringPtr(existingOrder.MetadataJSON)
		sweep.EnergyRentalExpiresAt = &expiresAt
		if existingOrder.NormalizedStatus == TronEnergyRentalStatusActive {
			return nil
		}
		return ErrEnergyRentalPending
	}

	transferCount := tronEnergyOrderTransferCount()
	durationHours := tronEnergyOrderDurationHours()
	requestingStatus := model.SweepStatusRequestingEnergy
	selectedProviderName := strings.TrimSpace(os.Getenv("TRON_ENERGY_PROVIDER"))
	if selectedProviderName == "" {
		selectedProviderName = "tron_energy"
	}
	creatingStatus := "creating"
	_ = s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
		ProviderName:      &selectedProviderName,
		ProviderOrderNo:   stringPtr(orderNo),
		ProviderStatus:    &creatingStatus,
		ProviderLastError: nil,
		Status:            &requestingStatus,
	})
	sweep.ProviderName = stringPtr(selectedProviderName)
	sweep.ProviderOrderNo = stringPtr(orderNo)
	sweep.ProviderStatus = &creatingStatus
	accountInfo, err := s.TronEnergyProvider.GetAccountInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to query TRON energy provider balance: %w", err)
	}
	priceInfo, err := s.TronEnergyProvider.GetPriceInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to query TRON energy provider pricing: %w", err)
	}
	requiredBalanceSun, err := estimateTronEnergyOrderCostSun(priceInfo, transferCount, durationHours)
	if err != nil {
		return fmt.Errorf("failed to estimate TRON energy rental cost: %w", err)
	}
	if accountInfo.AvailableBalanceSun < requiredBalanceSun {
		requiredTRX := decimal.NewFromInt(requiredBalanceSun).Shift(-6)
		availableTRX := decimal.NewFromInt(accountInfo.AvailableBalanceSun).Shift(-6)
		depositAddress := accountInfo.DepositAddress
		if strings.TrimSpace(depositAddress) == "" {
			return fmt.Errorf("TRON energy provider balance too low: need %s TRX, have %s TRX", requiredTRX.String(), availableTRX.String())
		}
		return fmt.Errorf("TRON energy provider balance too low: need %s TRX, have %s TRX. Top up provider account at %s", requiredTRX.String(), availableTRX.String(), depositAddress)
	}

	order, err := s.TronEnergyProvider.CreateEnergyOrder(ctx, TronEnergyOrderRequest{
		ReceiverAddress: sweep.FromAddress,
		OrderNo:         orderNo,
		TransferCount:   transferCount,
		DurationHours:   durationHours,
	})
	if err != nil {
		if isTronEnergyDuplicateOrderError(err) {
			existingOrder, lookupErr := s.TronEnergyProvider.GetOrderByOrderNo(ctx, orderNo)
			if lookupErr == nil && existingOrder != nil {
				order = existingOrder
				err = nil
			} else if lookupErr != nil {
				return fmt.Errorf("duplicate TRON energy order %s but failed to recover existing provider order: %w", orderNo, lookupErr)
			}
		}
	}
	if err != nil {
		if isTronEnergyReceiverNotActivatedError(err) {
			errText := err.Error()
			_ = s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
				ProviderLastError:  &errText,
				ClearProviderState: true,
			})
			activationHash, prefundErr := s.PrefundGasAddress(ctx, sweep.FromAddress, big.NewInt(tronEnergyActivationAmountSun()), "TRON")
			if prefundErr != nil {
				return fmt.Errorf("failed to activate TRON address before energy rental: %w", prefundErr)
			}
			log.Printf("Activated TRON address %s for sweep %s via prefund %s before energy rental retry", sweep.FromAddress, sweep.ID, activationHash)
			return prefundInitiatedWithHash(activationHash)
		}
		errText := err.Error()
		providerName := selectedProviderName
		failedStatus := string(TronEnergyRentalStatusFailed)
		_ = s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
			ProviderName:      &providerName,
			ProviderOrderNo:   stringPtr(orderNo),
			ProviderStatus:    &failedStatus,
			ProviderLastError: &errText,
		})
		return fmt.Errorf("failed to request TRON energy rental: %w", err)
	}

	orderStatus := string(order.NormalizedStatus)
	providerName := order.ProviderName
	expiresAt := now.Add(tronEnergyRentalTimeout())
	waitingStatus := model.SweepStatusWaitingForEnergyRental
	if err := s.SweepRepository.UpdateProviderState(ctx, sweep.ID, repository.SweepProviderStateUpdate{
		ProviderName:          &providerName,
		ProviderOrderID:       stringPtr(order.ProviderOrderID),
		ProviderOrderNo:       stringPtr(order.ProviderOrderNo),
		ProviderStatus:        &orderStatus,
		ProviderLastError:     nil,
		ProviderMetadataJSON:  stringPtr(order.MetadataJSON),
		EnergyRentalExpiresAt: &expiresAt,
		Status:                &waitingStatus,
	}); err != nil {
		return fmt.Errorf("failed to persist TRON energy rental state: %w", err)
	}

	sweep.ProviderName = stringPtr(providerName)
	sweep.ProviderOrderID = stringPtr(order.ProviderOrderID)
	sweep.ProviderOrderNo = stringPtr(order.ProviderOrderNo)
	sweep.ProviderStatus = &orderStatus
	sweep.ProviderLastError = nil
	sweep.ProviderMetadataJSON = stringPtr(order.MetadataJSON)
	sweep.EnergyRentalExpiresAt = &expiresAt
	return ErrEnergyRentalPending
}

func (s *SweeperService) recordGasOutlayPolicyBlocked(ctx context.Context, network string) {
	if s.TxManager == nil {
		return
	}
	s.TxManager.RecordCounter(ctx, "gas_outlay_policy_blocked", map[string]string{
		"network": network,
	})
}

func (s *SweeperService) currentEVMFeeCaps(ctx context.Context, client *ethclient.Client) (tipCap, feeCap *big.Int, head *types.Header, err error) {
	head, err = client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}

	if head.BaseFee != nil {
		tipCap, err = client.SuggestGasTipCap(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		feeCap = new(big.Int).Add(
			new(big.Int).Mul(head.BaseFee, big.NewInt(2)),
			tipCap,
		)
		return tipCap, feeCap, head, nil
	}

	feeCap, err = client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	return new(big.Int).Set(feeCap), feeCap, head, nil
}

func (s *SweeperService) enforceEVMGasConstraints(ctx context.Context, chainType string, feeCap *big.Int, gasLimit uint64) error {
	if s.GasWalletRepository == nil {
		return nil
	}

	gw, err := s.GasWalletRepository.GetByChainType(ctx, chainType)
	if err != nil {
		return fmt.Errorf("failed to load gas wallet config: %w", err)
	}

	return validateEVMGasConstraints(gw, feeCap, gasLimit)
}

func (s *SweeperService) SweepNative(ctx context.Context, sweep *model.Sweep) (string, error) {
	switch sweep.Network {
	case "SOLANA":
		return s.sweepSolana(ctx, sweep)
	case "TON":
		return s.sweepTon(ctx, sweep)
	case "BTC":
		return s.sweepBTC(ctx, sweep)
	case "TRC20", "TRON":
		return s.sweepTRON(ctx, sweep)
	default:
		return s.sweepEVMNative(ctx, sweep)
	}
}

func (s *SweeperService) getSolanaRPCClient() *rpc.Client {
	if s.SolanaService != nil && s.SolanaService.client != nil {
		return s.SolanaService.client
	}

	rpcURL := os.Getenv("SOLANA_RPC_URL")
	if rpcURL == "" {
		rpcURL = rpc.MainNetBeta_RPC
	}

	return rpc.New(rpcURL)
}

func (s *SweeperService) getTonAPIClient(ctx context.Context) (ton.APIClientWrapped, error) {
	if s.TonService != nil && s.TonService.api != nil {
		return s.TonService.api, nil
	}

	api, _, err := newTONAPIClient(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to ton: %w", err)
	}
	return api, nil
}

func (s *SweeperService) sweepEVMNative(ctx context.Context, sweep *model.Sweep) (string, error) {
	// 1. Get Private Key via HD Wallet
	privKey, err := s.getPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get private key: %w", err)
	}

	fromAddr := common.HexToAddress(sweep.FromAddress)
	toAddr := common.HexToAddress(sweep.ToHotWallet)

	client, err := s.getEVMClient(sweep.Network)
	if err != nil {
		return "", err
	}

	nonce, err := client.PendingNonceAt(ctx, fromAddr)
	if err != nil {
		return "", err
	}

	// Dynamic Gas Limit
	callMsg := ethereum.CallMsg{
		From:  fromAddr,
		To:    &toAddr,
		Value: nil, // Use nil/0 for estimation to avoid "insufficient funds" error during simulation
		Data:  nil,
	}

	gasLimit, err := client.EstimateGas(ctx, callMsg)
	if err != nil {
		log.Printf("Failed to estimate gas for native sweep, falling back to 21000: %v", err)
		gasLimit = 21000
	} else {
		// Add small buffer (10%) just in case
		gasLimit = gasLimit + (gasLimit / 10)
	}

	tipCap, feeCap, head, err := s.currentEVMFeeCaps(ctx, client)
	if err != nil {
		return "", err
	}
	if err := s.enforceEVMGasConstraints(ctx, sweep.Network, feeCap, gasLimit); err != nil {
		return "", err
	}

	balance, err := client.BalanceAt(ctx, fromAddr, nil)
	if err != nil {
		return "", err
	}

	cost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	var valueToSend *big.Int
	if isGasResidueSweep(sweep) {
		valueToSend = new(big.Int).Sub(balance, cost)
		if valueToSend.Sign() <= 0 {
			return "", fmt.Errorf("insufficient dust (balance %s < cost %s)", balance.String(), cost.String())
		}
	} else {
		queuedAmount, conversionErr := exactNativeUnitsFromSweepAmount(sweep.Amount, 18, nativeCoinForNetwork(sweep.Network))
		if conversionErr != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", conversionErr
		}
		if balance.Cmp(queuedAmount) < 0 {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", fmt.Errorf("queued sweep amount exceeds live balance")
		}
		valueToSend, err = subtractFeeFromQueuedUnits(queuedAmount, cost, nativeCoinForNetwork(sweep.Network))
		if err != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", err
		}
	}

	var tx *types.Transaction
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get chain ID: %w", err)
	}

	if head.BaseFee != nil {
		tx = types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       gasLimit,
			To:        &toAddr,
			Value:     valueToSend,
			Data:      nil,
		})
	} else {
		tx = types.NewTransaction(nonce, toAddr, valueToSend, gasLimit, feeCap, nil)
	}

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), privKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign native sweep tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return "", err
	}
	return signedTx.Hash().Hex(), nil
}

func (s *SweeperService) sweepSolana(ctx context.Context, sweep *model.Sweep) (string, error) {
	// 1. Get Private Key
	privKey, err := s.getSolanaPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get solana private key: %w", err)
	}

	// 2. Create Client
	client := s.getSolanaRPCClient()

	// 3. Build Transaction
	fromPub := privKey.Public().(ed25519.PublicKey)
	fromAccount := solana.PublicKeyFromBytes(fromPub)
	toAccount, err := solana.PublicKeyFromBase58(sweep.ToHotWallet)
	if err != nil {
		return "", fmt.Errorf("invalid to address: %w", err)
	}

	// Get recent blockhash
	recent, err := client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get blockhash: %w", err)
	}

	balance, err := client.GetBalance(ctx, fromAccount, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get balance: %w", err)
	}

	fee := solanaFeeEstimateLamports()
	var amount uint64
	if isGasResidueSweep(sweep) {
		if balance.Value < fee {
			return "", fmt.Errorf("insufficient balance for fee")
		}
		amount = balance.Value - fee
	} else {
		queuedAmount, conversionErr := exactUint64UnitsFromSweepAmount(sweep.Amount, 9, "SOL")
		if conversionErr != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", conversionErr
		}
		if balance.Value < queuedAmount {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", fmt.Errorf("queued sweep amount exceeds live balance")
		}
		amount, err = subtractFeeFromQueuedUint64(queuedAmount, fee, "SOL")
		if err != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", err
		}
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{
			system.NewTransferInstruction(
				amount,
				fromAccount,
				toAccount,
			).Build(),
		},
		recent.Value.Blockhash,
		solana.TransactionPayer(fromAccount),
	)
	if err != nil {
		return "", fmt.Errorf("failed to build tx: %w", err)
	}

	// 4. Sign & Send
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(fromAccount) {
			sk := solana.PrivateKey(privKey)
			return &sk
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to sign tx: %w", err)
	}

	sig, err := client.SendTransaction(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("failed to send tx: %w", err)
	}

	return sig.String(), nil
}

func (s *SweeperService) sweepSolanaToken(ctx context.Context, sweep *model.Sweep) (string, error) {
	privKey, err := s.getSolanaPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get solana private key: %w", err)
	}

	deposit, err := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	if err != nil || deposit == nil {
		return "", fmt.Errorf("failed to find deposit: %w", err)
	}

	walletCfg, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil || walletCfg == nil || walletCfg.ContractAddress == nil || *walletCfg.ContractAddress == "" {
		return "", fmt.Errorf("missing SPL mint configuration")
	}

	client := s.getSolanaRPCClient()

	fromPub := privKey.Public().(ed25519.PublicKey)
	owner := solana.PublicKeyFromBytes(fromPub)
	hotWallet, err := solana.PublicKeyFromBase58(sweep.ToHotWallet)
	if err != nil {
		return "", fmt.Errorf("invalid hot wallet address: %w", err)
	}
	mint, err := solana.PublicKeyFromBase58(*walletCfg.ContractAddress)
	if err != nil {
		return "", fmt.Errorf("invalid mint address: %w", err)
	}

	sourceATA, _, err := solana.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return "", fmt.Errorf("failed to derive source token account: %w", err)
	}
	destATA, _, err := solana.FindAssociatedTokenAddress(hotWallet, mint)
	if err != nil {
		return "", fmt.Errorf("failed to derive destination token account: %w", err)
	}

	tokenBalance, err := client.GetTokenAccountBalance(ctx, sourceATA, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get token balance: %w", err)
	}
	if tokenBalance == nil || tokenBalance.Value == nil || tokenBalance.Value.Amount == "" {
		return "", fmt.Errorf("source token account has no balance")
	}

	currentUnits, err := parseTokenBalanceUnits(tokenBalance.Value.Amount)
	if err != nil {
		return "", fmt.Errorf("invalid token balance: %w", err)
	}
	if currentUnits.Sign() == 0 {
		return "", fmt.Errorf("source token account has no balance")
	}

	queuedUnits, err := exactTokenUnitsFromSweepAmount(sweep.Amount, walletCfg.Decimals)
	if err != nil {
		return "", err
	}
	if currentUnits.Cmp(queuedUnits) < 0 {
		s.recordTokenSweepBalanceBelowQueuedAmount(ctx, sweep, currentUnits, queuedUnits)
		return "", fmt.Errorf("queued sweep amount exceeds live token balance")
	}
	if !queuedUnits.IsUint64() {
		return "", fmt.Errorf("queued sweep amount exceeds Solana token instruction limits")
	}

	instructions := make([]solana.Instruction, 0, 3)

	destInfo, err := client.GetAccountInfo(ctx, destATA)
	destMissing := err != nil || destInfo == nil || destInfo.Value == nil
	if destMissing {
		instructions = append(instructions, associatedtokenaccount.NewCreateInstruction(
			owner,
			hotWallet,
			mint,
		).Build())
	}

	instructions = append(instructions, token.NewTransferCheckedInstruction(
		queuedUnits.Uint64(),
		uint8(walletCfg.Decimals),
		sourceATA,
		mint,
		destATA,
		owner,
		nil,
	).Build())

	if shouldCloseTokenAccountAfterSweep(currentUnits, queuedUnits) {
		instructions = append(instructions, token.NewCloseAccountInstruction(
			sourceATA,
			hotWallet,
			owner,
			nil,
		).Build())
	}

	balance, err := client.GetBalance(ctx, owner, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get solana balance: %w", err)
	}

	requiredLamports := solanaFeeEstimateLamports()
	if destMissing {
		rent, rentErr := client.GetMinimumBalanceForRentExemption(ctx, 165, rpc.CommitmentFinalized)
		if rentErr != nil {
			return "", fmt.Errorf("failed to estimate token account rent: %w", rentErr)
		}
		requiredLamports += rent
	}

	if balance.Value < requiredLamports {
		deficit := requiredLamports - balance.Value
		prefundHash, err := s.PrefundGasAddress(ctx, owner.String(), new(big.Int).SetUint64(deficit), "SOLANA")
		if err != nil {
			return "", fmt.Errorf("prefund failed (SOLANA): %w", err)
		}
		return prefundHash, ErrPrefundInitiated
	}

	recent, err := client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get blockhash: %w", err)
	}

	tx, err := solana.NewTransaction(
		instructions,
		recent.Value.Blockhash,
		solana.TransactionPayer(owner),
	)
	if err != nil {
		return "", fmt.Errorf("failed to build token sweep tx: %w", err)
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(owner) {
			sk := solana.PrivateKey(privKey)
			return &sk
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to sign token sweep tx: %w", err)
	}

	sig, err := client.SendTransaction(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("failed to send token sweep tx: %w", err)
	}

	return sig.String(), nil
}

func (s *SweeperService) sweepTon(ctx context.Context, sweep *model.Sweep) (string, error) {
	// 1. Get Private Key
	privKey, err := s.getTonPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get ton private key: %w", err)
	}

	// 2. Connect
	api, err := s.getTonAPIClient(ctx)
	if err != nil {
		return "", err
	}

	// 3. Wallet
	// Standard V4R2 wallet
	w, err := wallet.FromPrivateKey(api, privKey, wallet.V4R2)
	if err != nil {
		return "", fmt.Errorf("failed to load wallet: %w", err)
	}

	// 4. Check Balance
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return "", err
	}

	balance, err := w.GetBalance(ctx, block)
	if err != nil {
		return "", err
	}

	fee := tonFeeEstimateNano()
	var amountToSend *big.Int
	if isGasResidueSweep(sweep) {
		if balance.Nano().Cmp(fee) <= 0 {
			return "", fmt.Errorf("insufficient ton balance for fee")
		}
		amountToSend = new(big.Int).Sub(balance.Nano(), fee)
	} else {
		queuedAmount, conversionErr := exactNativeUnitsFromSweepAmount(sweep.Amount, 9, "TON")
		if conversionErr != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", conversionErr
		}
		if balance.Nano().Cmp(queuedAmount) < 0 {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", fmt.Errorf("queued sweep amount exceeds live balance")
		}
		amountToSend, err = subtractFeeFromQueuedUnits(queuedAmount, fee, "TON")
		if err != nil {
			s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
			return "", err
		}
	}

	// 5. Transfer
	addr := address.MustParseAddr(sweep.ToHotWallet)

	log.Printf("Sweeping %s to %s", tlb.FromNanoTON(amountToSend).String(), addr.String())

	// Send message (no comment, non-bounceable if needed)
	transfer, err := w.BuildTransfer(addr, tlb.FromNanoTON(amountToSend), false, "")
	if err != nil {
		return "", err
	}

	tx, _, err := w.SendWaitTransaction(ctx, transfer)
	if err != nil {
		return "", fmt.Errorf("ton transfer failed: %w", err)
	}

	return hex.EncodeToString(tx.Hash), nil
}

func (s *SweeperService) sweepTonJetton(ctx context.Context, sweep *model.Sweep) (string, error) {
	privKey, err := s.getTonPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get ton private key: %w", err)
	}

	deposit, err := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	if err != nil || deposit == nil {
		return "", fmt.Errorf("failed to find deposit: %w", err)
	}

	walletCfg, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil || walletCfg == nil || walletCfg.ContractAddress == nil || *walletCfg.ContractAddress == "" {
		return "", fmt.Errorf("missing jetton master configuration")
	}

	api, err := s.getTonAPIClient(ctx)
	if err != nil {
		return "", err
	}

	w, err := wallet.FromPrivateKey(api, privKey, wallet.V4R2)
	if err != nil {
		return "", fmt.Errorf("failed to load ton wallet: %w", err)
	}

	masterAddr, err := address.ParseAddr(*walletCfg.ContractAddress)
	if err != nil {
		return "", fmt.Errorf("invalid jetton master address: %w", err)
	}
	hotAddr, err := address.ParseAddr(sweep.ToHotWallet)
	if err != nil {
		return "", fmt.Errorf("invalid hot wallet address: %w", err)
	}

	jettonClient := jetton.NewJettonMasterClient(api, masterAddr)
	jettonWallet, err := jettonClient.GetJettonWallet(ctx, w.WalletAddress())
	if err != nil {
		return "", fmt.Errorf("failed to resolve deposit jetton wallet: %w", err)
	}

	balance, err := jettonWallet.GetBalance(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get jetton balance: %w", err)
	}
	if balance == nil || balance.Sign() <= 0 {
		return "", fmt.Errorf("jetton wallet has no balance")
	}

	queuedUnits, err := exactTokenUnitsFromSweepAmount(sweep.Amount, walletCfg.Decimals)
	if err != nil {
		return "", err
	}
	if balance.Cmp(queuedUnits) < 0 {
		s.recordTokenSweepBalanceBelowQueuedAmount(ctx, sweep, balance, queuedUnits)
		return "", fmt.Errorf("queued sweep amount exceeds live token balance")
	}

	attachTON := os.Getenv("TON_JETTON_SWEEP_FEE")
	if attachTON == "" {
		attachTON = "0.05"
	}
	attachAmount := tlb.MustFromTON(attachTON)
	feeReserve := tlb.FromNanoTON(tonFeeEstimateNano())

	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return "", err
	}
	tonBalance, err := w.GetBalance(ctx, block)
	if err != nil {
		return "", err
	}

	requiredTON := new(big.Int).Add(attachAmount.Nano(), feeReserve.Nano())
	if tonBalance.Nano().Cmp(requiredTON) < 0 {
		deficit := new(big.Int).Sub(requiredTON, tonBalance.Nano())
		prefundHash, err := s.PrefundGasAddress(ctx, w.WalletAddress().String(), deficit, "TON")
		if err != nil {
			return "", fmt.Errorf("prefund failed (TON): %w", err)
		}
		return prefundHash, ErrPrefundInitiated
	}

	jettonAmount, err := tlb.FromNano(queuedUnits, walletCfg.Decimals)
	if err != nil {
		return "", fmt.Errorf("failed to encode jetton amount: %w", err)
	}
	payload, err := jetton.BuildTransferPayload(hotAddr, hotAddr, jettonAmount, tlb.MustFromTON("0"), nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build jetton transfer payload: %w", err)
	}

	msg := wallet.SimpleMessage(jettonWallet.Address(), attachAmount, payload)
	tx, _, err := w.SendWaitTransaction(ctx, msg)
	if err != nil {
		return "", fmt.Errorf("ton jetton sweep failed: %w", err)
	}

	return hex.EncodeToString(tx.Hash), nil
}

func (s *SweeperService) currentBTCFeeRate() (int64, error) {
	feeRate := int64(20)
	if s.BTCClient == nil {
		if s.MempoolClient == nil {
			return feeRate, fmt.Errorf("btc fee estimation unavailable")
		}
		rec, err := s.MempoolClient.GetRecommendedFees()
		if err != nil {
			return feeRate, err
		}
		return int64(rec.HalfHourFee), nil
	}

	res, err := s.BTCClient.EstimateSmartFee(3, &btcjson.EstimateModeConservative)
	if err != nil {
		return feeRate, err
	}
	if res.FeeRate == nil || *res.FeeRate <= 0 {
		return feeRate, nil
	}

	return int64(*res.FeeRate * 1e8 / 1000), nil
}

type btcSweepSigningInput struct {
	OutPoint wire.OutPoint
	TxOut    *wire.TxOut
	PrivKey  *btcec.PrivateKey
}

func signBTCSweepInput(tx *wire.MsgTx, sigHashes *txscript.TxSigHashes, inputIndex int, input btcSweepSigningInput) error {
	if tx == nil {
		return fmt.Errorf("btc tx is required")
	}
	if input.TxOut == nil {
		return fmt.Errorf("btc input prevout is required")
	}

	pkScript := input.TxOut.PkScript
	switch {
	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		witness, err := txscript.WitnessSignature(
			tx,
			sigHashes,
			inputIndex,
			input.TxOut.Value,
			pkScript,
			txscript.SigHashAll,
			input.PrivKey,
			true,
		)
		if err != nil {
			return fmt.Errorf("failed to sign native segwit input %d: %w", inputIndex, err)
		}
		tx.TxIn[inputIndex].Witness = witness
		tx.TxIn[inputIndex].SignatureScript = nil
		return nil
	case txscript.IsPayToPubKeyHash(pkScript):
		sigScript, err := txscript.SignatureScript(
			tx,
			inputIndex,
			pkScript,
			txscript.SigHashAll,
			input.PrivKey,
			true,
		)
		if err != nil {
			return fmt.Errorf("failed to sign legacy input %d: %w", inputIndex, err)
		}
		tx.TxIn[inputIndex].SignatureScript = sigScript
		tx.TxIn[inputIndex].Witness = nil
		return nil
	case txscript.IsPayToTaproot(pkScript):
		return fmt.Errorf("unsupported BTC input script type taproot at input %d", inputIndex)
	case txscript.IsPayToScriptHash(pkScript):
		return fmt.Errorf("unsupported BTC input script type p2sh at input %d", inputIndex)
	default:
		return fmt.Errorf("unsupported BTC input script at input %d", inputIndex)
	}
}

func btcVirtualSize(tx *wire.MsgTx) int64 {
	strippedSize := int64(tx.SerializeSizeStripped())
	totalSize := int64(tx.SerializeSize())
	witnessSize := totalSize - strippedSize
	weight := (strippedSize * blockchain.WitnessScaleFactor) + witnessSize
	return (weight + (blockchain.WitnessScaleFactor - 1)) / blockchain.WitnessScaleFactor
}

func estimateSignedBTCSweepFeeSats(inputs []btcSweepSigningInput, destScript, changeScript []byte, exactAmountSats int64, includeChange bool, feeRateSatVB int64) (int64, error) {
	if len(inputs) == 0 || len(destScript) == 0 || feeRateSatVB <= 0 {
		return 0, nil
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	fetcherOutputs := make(map[wire.OutPoint]*wire.TxOut, len(inputs))
	for _, input := range inputs {
		tx.AddTxIn(wire.NewTxIn(&input.OutPoint, nil, nil))
		txOutCopy := *input.TxOut
		fetcherOutputs[input.OutPoint] = &txOutCopy
	}

	tx.AddTxOut(wire.NewTxOut(exactAmountSats, destScript))
	if includeChange {
		tx.AddTxOut(wire.NewTxOut(1, changeScript))
	}

	fetcher := &SimpleFetcher{outputs: fetcherOutputs}
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, input := range inputs {
		if err := signBTCSweepInput(tx, sigHashes, i, input); err != nil {
			return 0, err
		}
	}

	return btcVirtualSize(tx) * feeRateSatVB, nil
}

const btcDustThresholdSats int64 = 546

func (s *SweeperService) sweepBTC(ctx context.Context, sweep *model.Sweep) (string, error) {
	stdKey, err := s.getPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", fmt.Errorf("failed to get private key: %w", err)
	}

	pKeyBytes := make([]byte, 32)
	stdKey.D.FillBytes(pKeyBytes)
	privKey, _ := btcec.PrivKeyFromBytes(pKeyBytes)

	sourceAddr, err := btcutil.DecodeAddress(sweep.FromAddress, s.HDWallet.Params)
	if err != nil {
		return "", err
	}
	sourceScript, err := txscript.PayToAddrScript(sourceAddr)
	if err != nil {
		return "", err
	}

	exactAmountSats, err := exactSatoshisFromSweepAmount(sweep.Amount)
	if err != nil {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", err
	}

	var utxos []mempool.UTXO
	var unspent []btcjson.ListUnspentResult

	if s.BTCClient == nil {
		u, err := s.MempoolClient.GetUTXOs(sweep.FromAddress)
		if err != nil {
			return "", fmt.Errorf("failed to list unspent (api): %w", err)
		}
		utxos = u
	} else {
		u, err := s.BTCClient.ListUnspentMinMaxAddresses(0, 999999, []btcutil.Address{sourceAddr})
		if err != nil {
			return "", fmt.Errorf("failed to list unspent (node): %w", err)
		}
		unspent = u
	}

	if (s.BTCClient == nil && len(utxos) == 0) || (s.BTCClient != nil && len(unspent) == 0) {
		return "", fmt.Errorf("no UTXOs found")
	}

	customerInputs := make([]btcSweepSigningInput, 0, len(utxos)+len(unspent))
	var totalCustomer int64
	if s.BTCClient == nil {
		for _, u := range utxos {
			hash, err := chainhash.NewHashFromStr(u.TxID)
			if err != nil {
				return "", err
			}
			customerInputs = append(customerInputs, btcSweepSigningInput{
				OutPoint: *wire.NewOutPoint(hash, u.Vout),
				TxOut:    wire.NewTxOut(u.Value, sourceScript),
				PrivKey:  privKey,
			})
			totalCustomer += u.Value
		}
	} else {
		for _, u := range unspent {
			hash, err := chainhash.NewHashFromStr(u.TxID)
			if err != nil {
				return "", err
			}
			amountSat, err := btcutil.NewAmount(u.Amount)
			if err != nil {
				return "", fmt.Errorf("invalid BTC UTXO amount for %s:%d: %w", u.TxID, u.Vout, err)
			}
			prevoutScript := sourceScript
			if strings.TrimSpace(u.Address) != "" {
				utxoAddr, decodeErr := btcutil.DecodeAddress(u.Address, s.HDWallet.Params)
				if decodeErr != nil {
					return "", fmt.Errorf("invalid BTC UTXO address for %s:%d: %w", u.TxID, u.Vout, decodeErr)
				}
				prevoutScript, err = txscript.PayToAddrScript(utxoAddr)
				if err != nil {
					return "", fmt.Errorf("failed to derive BTC prevout script for %s:%d: %w", u.TxID, u.Vout, err)
				}
			}
			customerInputs = append(customerInputs, btcSweepSigningInput{
				OutPoint: *wire.NewOutPoint(hash, u.Vout),
				TxOut:    wire.NewTxOut(int64(amountSat), prevoutScript),
				PrivKey:  privKey,
			})
			totalCustomer += int64(amountSat)
		}
	}
	if totalCustomer < exactAmountSats {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", fmt.Errorf("queued BTC sweep amount exceeds live balance")
	}

	feeRate, err := s.currentBTCFeeRate()
	if err != nil {
		log.Printf("Failed to estimate BTC fee rate, using fallback 20 sat/vB: %v", err)
		feeRate = 20
	}

	gw, err := s.loadGasWalletWithDailyReset(ctx, "BTC")
	if err != nil {
		return "", fmt.Errorf("failed to load BTC fee policy: %w", err)
	}

	customerChange := totalCustomer - exactAmountSats
	if customerChange > 0 && customerChange < btcDustThresholdSats {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", fmt.Errorf("btc customer change would be dust")
	}

	destAddr, err := btcutil.DecodeAddress(sweep.ToHotWallet, s.HDWallet.Params)
	if err != nil {
		return "", fmt.Errorf("invalid hot wallet address: %w", err)
	}
	destScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		return "", err
	}

	fee, err := estimateSignedBTCSweepFeeSats(customerInputs, destScript, sourceScript, exactAmountSats, customerChange > 0, feeRate)
	if err != nil {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", err
	}
	if fee <= 0 {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", fmt.Errorf("failed to estimate BTC sweep fee")
	}
	if err := validateBTCFeeConstraints(gw, feeRate, fee); err != nil {
		if s.TxManager != nil && errors.Is(err, ErrSweepWaitingForGas) {
			s.TxManager.RecordCounter(ctx, "btc_fee_policy_blocked", map[string]string{})
		}
		return "", err
	}

	hotOutputAmount, err := subtractFeeFromQueuedInt64(exactAmountSats, fee, "BTC")
	if err != nil {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", err
	}
	if hotOutputAmount < btcDustThresholdSats {
		s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
		return "", fmt.Errorf("btc hot-wallet output would be dust after fee")
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	allInputs := make([]btcSweepSigningInput, 0, len(customerInputs))
	for _, input := range customerInputs {
		tx.AddTxIn(wire.NewTxIn(&input.OutPoint, nil, nil))
		allInputs = append(allInputs, input)
	}

	tx.AddTxOut(wire.NewTxOut(hotOutputAmount, destScript))
	if customerChange > 0 {
		tx.AddTxOut(wire.NewTxOut(customerChange, sourceScript))
	}

	fetcherOutputs := make(map[wire.OutPoint]*wire.TxOut, len(allInputs))
	for _, input := range allInputs {
		txOutCopy := *input.TxOut
		fetcherOutputs[input.OutPoint] = &txOutCopy
	}
	fetcher := &SimpleFetcher{outputs: fetcherOutputs}
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	for i, input := range allInputs {
		if err := signBTCSweepInput(tx, sigHashes, i, input); err != nil {
			return "", err
		}
	}

	if s.BTCClient == nil {
		var buf bytes.Buffer
		tx.Serialize(&buf)
		hexTx := hex.EncodeToString(buf.Bytes())

		txid, err := s.MempoolClient.BroadcastTx(hexTx)
		if err != nil {
			return "", fmt.Errorf("api broadcast failed: %w", err)
		}
		if gw != nil {
			if err := s.GasWalletRepository.IncrementDailyGasUsed(ctx, "BTC", estimatedBTCFeeAmount(fee)); err != nil {
				log.Printf("Failed to increment BTC daily gas used: %v", err)
			}
		}
		return txid, nil
	} else {
		// Use client
		hash, err := s.BTCClient.SendRawTransaction(tx, true)
		if err != nil {
			return "", fmt.Errorf("broadcast failed: %w", err)
		}
		if gw != nil {
			if err := s.GasWalletRepository.IncrementDailyGasUsed(ctx, "BTC", estimatedBTCFeeAmount(fee)); err != nil {
				log.Printf("Failed to increment BTC daily gas used: %v", err)
			}
		}
		return hash.String(), nil
	}
}

// NewAPIFetcherWithScript builds fetcher from API UTXOs and the known script for the address.
func NewAPIFetcherWithScript(utxos []mempool.UTXO, script []byte) *SimpleFetcher {
	m := make(map[wire.OutPoint]*wire.TxOut)
	for _, u := range utxos {
		hash, _ := chainhash.NewHashFromStr(u.TxID)
		op := wire.NewOutPoint(hash, u.Vout)
		m[*op] = wire.NewTxOut(u.Value, script)
	}
	return &SimpleFetcher{outputs: m}
}

func (s *SweeperService) sweepTRON(ctx context.Context, sweep *model.Sweep) (string, error) {
	return s.sweepTRONWithSDK(ctx, sweep)
}

func (s *SweeperService) getSolanaPrivateKey(ctx context.Context, depositID string) (ed25519.PrivateKey, error) {
	deposit, err := s.DepositRepository.FindByID(ctx, depositID)
	if err != nil {
		return nil, fmt.Errorf("failed to find deposit: %w", err)
	}
	wallet, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil {
		return nil, fmt.Errorf("failed to find hd wallet: %w", err)
	}

	if wallet.EncryptedMasterSeed == nil || *wallet.EncryptedMasterSeed == "" {
		return nil, errors.New("wallet encrypted spend key is missing")
	}

	masterKeyHex, err := crypto_pkg.DecryptWithEnv(*wallet.EncryptedMasterSeed, "APP_SECRET")
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt master key: %w", err)
	}

	return s.HDWallet.DeriveSolanaPrivateKey(masterKeyHex, uint32(wallet.DerivationAccount), uint32(deposit.DerivationIndex))
}

func (s *SweeperService) getTonPrivateKey(ctx context.Context, depositID string) (ed25519.PrivateKey, error) {
	deposit, err := s.DepositRepository.FindByID(ctx, depositID)
	if err != nil {
		return nil, fmt.Errorf("failed to find deposit: %w", err)
	}
	walletCfg, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil {
		return nil, fmt.Errorf("failed to find hd wallet: %w", err)
	}

	if walletCfg.EncryptedMasterSeed == nil || *walletCfg.EncryptedMasterSeed == "" {
		return nil, errors.New("wallet encrypted spend key is missing")
	}

	masterKeyHex, err := crypto_pkg.DecryptWithEnv(*walletCfg.EncryptedMasterSeed, "APP_SECRET")
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt master key: %w", err)
	}

	return s.HDWallet.DeriveTonPrivateKey(masterKeyHex, uint32(walletCfg.DerivationAccount), uint32(deposit.DerivationIndex))
}

// getPrivateKey retrieves the private key for a deposit address
func (s *SweeperService) getPrivateKey(ctx context.Context, depositID string) (*ecdsa.PrivateKey, error) {
	// 1. Get Deposit
	deposit, err := s.DepositRepository.FindByID(ctx, depositID)
	if err != nil {
		return nil, fmt.Errorf("failed to find deposit: %w", err)
	}
	if deposit == nil {
		return nil, errors.New("deposit not found")
	}

	// 2. Get HD Wallet
	wallet, err := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
	if err != nil {
		return nil, fmt.Errorf("failed to find hd wallet: %w", err)
	}
	if wallet == nil {
		return nil, errors.New("hd wallet not found")
	}

	if wallet.EncryptedMasterSeed == nil || *wallet.EncryptedMasterSeed == "" {
		return nil, errors.New("wallet encrypted spend key is missing")
	}

	masterKeyHex, err := crypto_pkg.DecryptWithEnv(*wallet.EncryptedMasterSeed, "APP_SECRET")
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt master key: %w", err)
	}

	// 4. Derive Child Key
	return s.HDWallet.DerivePrivateKey(masterKeyHex, uint32(deposit.DerivationIndex))
}

func (s *SweeperService) decodeEd25519PrivateKeyHex(encKey string) (ed25519.PrivateKey, error) {
	keyHex, err := crypto_pkg.DecryptWithEnv(encKey, "APP_SECRET")
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt private key: %w", err)
	}

	keyBytes, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key hex: %w", err)
	}

	switch len(keyBytes) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(keyBytes), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(keyBytes), nil
	default:
		return nil, fmt.Errorf("unexpected ed25519 key length: %d", len(keyBytes))
	}
}

func (s *SweeperService) maybeAlertLowGasBalance(ctx context.Context, gw *model.GasWallet, chainType, address string, currentBalance decimal.Decimal) {
	if !currentBalance.LessThan(gw.MinBalance) {
		return
	}

	if gw.LastAlertBalance.Valid && gw.LastAlertBalance.Decimal.Equal(currentBalance) {
		return
	}

	s.emitAlert(ctx, fmt.Sprintf("⚠️ <b>LOW GAS ALERT</b>\n\nChain: <b>%s</b>\nAddress: <code>%s</code>\nCurrent Balance: <b>%s</b>\nMinimum Threshold: <b>%s</b>",
		chainType, address, currentBalance.String(), gw.MinBalance.String()))
	if err := s.GasWalletRepository.UpdateLastAlertBalance(ctx, gw.ID, currentBalance); err != nil {
		log.Printf("Failed to update gas wallet alert balance: %v", err)
	}
}

func resolveSweepTokenContractAddress(walletCfg *model.HDWallet) (common.Address, error) {
	if walletCfg == nil || walletCfg.ContractAddress == nil || strings.TrimSpace(*walletCfg.ContractAddress) == "" {
		return common.Address{}, fmt.Errorf("missing token contract configuration")
	}
	if !common.IsHexAddress(*walletCfg.ContractAddress) {
		return common.Address{}, fmt.Errorf("invalid token contract address")
	}
	return common.HexToAddress(*walletCfg.ContractAddress), nil
}

func (s *SweeperService) SweepToken(ctx context.Context, sweep *model.Sweep) (string, error) {
	// Token: Check Gas -> Prefund -> Sweep

	// 1. Check Gas Balance
	fromAddr := common.HexToAddress(sweep.FromAddress)
	client, err := s.getEVMClient(sweep.Network)
	if err != nil {
		return "", err
	}

	balance, err := client.BalanceAt(ctx, fromAddr, nil)
	if err != nil {
		return "", err
	}

	// 2. Sweep Token
	dep, _ := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	var walletCfg *model.HDWallet
	if dep != nil {
		walletCfg, _ = s.HdWalletRepository.FindByID(ctx, dep.HDWalletID)
	}

	if sweep.Network == "TRC20" || sweep.Network == "TRON" {
		// TRON tokens are handled by sweepTRON, but if we are here, something is wrong
		return "", fmt.Errorf("TRON token sweep routed to EVM handler")
	}

	tokenAddr, err := resolveSweepTokenContractAddress(walletCfg)
	if err != nil {
		return "", err
	}

	// Pack ERC20 transfer(address,uint256)
	methodID := []byte{0xa9, 0x05, 0x9c, 0xbb}
	toAddr := common.HexToAddress(sweep.ToHotWallet)

	// 2. Obtain Token Decimals and Scale Amount
	// High-precision conversion from human-readable decimal back to on-chain integer units.
	decimals := 18 // Default for most ERC20
	// We already have 'dep' and 'wallet' metadata if found above
	if walletCfg != nil {
		decimals = walletCfg.Decimals
	}

	amount, err := exactTokenUnitsFromSweepAmount(sweep.Amount, decimals)
	if err != nil {
		return "", fmt.Errorf("invalid EVM token sweep amount: %w", err)
	}

	paddedAddress := common.LeftPadBytes(toAddr.Bytes(), 32)
	paddedAmount := common.LeftPadBytes(amount.Bytes(), 32)

	var data []byte
	data = append(data, methodID...)
	data = append(data, paddedAddress...)
	data = append(data, paddedAmount...)

	privKey, err := s.getPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", err
	}

	// Estimate Gas
	callMsg := ethereum.CallMsg{
		From:  fromAddr,
		To:    &tokenAddr,
		Data:  data,
		Value: big.NewInt(0),
	}
	gasLimit, err := client.EstimateGas(ctx, callMsg)
	if err != nil {
		log.Printf("Failed to estimate gas for token sweep, fallback to 100000: %v", err)
		gasLimit = 100000
	} else {
		// Buffer
		gasLimit = gasLimit + (gasLimit / 5) // 20%
	}

	tipCap, feeCap, head, err := s.currentEVMFeeCaps(ctx, client)
	if err != nil {
		return "", err
	}
	if err := s.enforceEVMGasConstraints(ctx, sweep.Network, feeCap, gasLimit); err != nil {
		return "", err
	}

	requiredGas := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	if balance.Cmp(requiredGas) < 0 {
		deficit := new(big.Int).Sub(requiredGas, balance)
		log.Printf("Prefunding %s with %s wei", sweep.FromAddress, deficit.String())

		prefundHash, err := s.PrefundGas(ctx, fromAddr, deficit, sweep.Network)
		if err != nil {
			return "", fmt.Errorf("prefund failed: %w", err)
		}

		log.Printf("Prefund initiated: %s. Returning to poller.", prefundHash)
		return prefundHash, ErrPrefundInitiated
	}

	nonce, err := client.PendingNonceAt(ctx, fromAddr)
	if err != nil {
		return "", err
	}

	var tx *types.Transaction
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return "", err
	}

	if head.BaseFee != nil {
		tx = types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       gasLimit,
			To:        &tokenAddr,
			Value:     big.NewInt(0),
			Data:      data,
		})
	} else {
		tx = types.NewTransaction(nonce, tokenAddr, big.NewInt(0), gasLimit, feeCap, data)
	}

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), privKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign token sweep tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return "", err
	}
	return signedTx.Hash().Hex(), nil
}

func (s *SweeperService) PrefundGas(ctx context.Context, toAddr common.Address, amount *big.Int, chainType string) (string, error) {
	return s.PrefundGasAddress(ctx, toAddr.Hex(), amount, chainType)
}

func (s *SweeperService) PrefundGasAddress(ctx context.Context, toAddr string, amount *big.Int, chainType string) (string, error) {
	var (
		txHash string
		err    error
	)

	switch repository.NormalizeGasWalletChainType(chainType) {
	case "TRON":
		txHash, err = s.prefundGasTRON(ctx, toAddr, amount)
	case "SOLANA":
		txHash, err = s.prefundGasSolana(ctx, toAddr, amount)
	case "TON":
		txHash, err = s.prefundGasTON(ctx, toAddr, amount)
	default:
		txHash, err = s.prefundGasEVM(ctx, toAddr, amount, chainType)
	}

	if err == nil && strings.TrimSpace(txHash) != "" {
		rememberOperationalPrefund(ctx, chainType, toAddr, txHash)
	}

	return txHash, err
}

func (s *SweeperService) prefundGasEVM(ctx context.Context, toAddr string, amount *big.Int, chainType string) (string, error) {
	// 1. Get Gas Wallet
	gw, err := s.loadGasWalletWithDailyReset(ctx, chainType)
	if err != nil {
		return "", fmt.Errorf("failed to get gas wallet: %w", err)
	}
	if gw == nil {
		return "", fmt.Errorf("no gas wallet configured for %s", chainType)
	}

	// 3. Send Transaction from Gas Wallet
	gwKeyHex, err := crypto_pkg.DecryptWithEnv(gw.PrivateKeyEnc, "APP_SECRET")
	if err != nil {
		return "", fmt.Errorf("failed to decrypt gas wallet key: %v", err)
	}

	privKey, err := crypto.HexToECDSA(gwKeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid private key hex: %v", err)
	}

	gasWalletAddr := common.HexToAddress(gw.WalletAddress)

	client, err := s.getEVMClient(chainType)
	if err != nil {
		return "", err
	}

	// Check gas wallet balance
	gwBalance, err := client.BalanceAt(ctx, gasWalletAddr, nil)
	if err != nil {
		return "", fmt.Errorf("failed to check gas wallet balance: %w", err)
	}

	// Dynamic Fee Estimation (EIP-1559)
	// First estimate gas limit dynamically
	callMsg := ethereum.CallMsg{
		From:  gasWalletAddr,
		To:    &[]common.Address{common.HexToAddress(toAddr)}[0], // Only one optional To? no, To is *Address
		Value: amount,
		Data:  nil,
	}
	// Note: To needs to be correct pointer type.
	target := common.HexToAddress(toAddr)
	callMsg.To = &target

	gasLimit, err := client.EstimateGas(ctx, callMsg)
	if err != nil {
		log.Printf("Failed to estimate gas for prefund, use 21000 default: %v", err)
		gasLimit = 21000
	} else {
		gasLimit += (gasLimit / 10) // 10% buffer
	}

	tipCap, feeCap, head, err := s.currentEVMFeeCaps(ctx, client)
	if err != nil {
		return "", err
	}
	if err := s.enforceEVMGasConstraints(ctx, chainType, feeCap, gasLimit); err != nil {
		return "", err
	}

	cost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	totalReq := new(big.Int).Add(amount, cost)
	totalOutlayDec := decimal.NewFromBigInt(totalReq, -18)
	if err := validateGasWalletOutlay(gw, totalOutlayDec); err != nil {
		s.recordGasOutlayPolicyBlocked(ctx, chainType)
		return "", err
	}

	currentBalanceDec := decimal.NewFromBigInt(gwBalance, -18)
	s.maybeAlertLowGasBalance(ctx, gw, chainType, gw.WalletAddress, currentBalanceDec)

	if gwBalance.Cmp(totalReq) < 0 {
		return "", fmt.Errorf("insufficient funds for prefund in gas wallet (%s < %s)", currentBalanceDec.String(), decimal.NewFromBigInt(totalReq, -18).String())
	}

	nonce, err := client.PendingNonceAt(ctx, gasWalletAddr)
	if err != nil {
		return "", err
	}

	var tx *types.Transaction
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get chain ID: %w", err)
	}

	if head.BaseFee != nil {
		tx = types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       gasLimit,
			To:        &target,
			Value:     amount,
			Data:      nil,
		})
	} else {
		tx = types.NewTransaction(nonce, common.HexToAddress(toAddr), amount, gasLimit, feeCap, nil)
	}

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), privKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign gas prefund tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return "", err
	}

	// 4. Update Daily Usage
	err = s.GasWalletRepository.IncrementDailyGasUsed(ctx, chainType, totalOutlayDec)
	if err != nil {
		log.Printf("Failed to increment daily gas used: %v", err)
	}

	return signedTx.Hash().Hex(), nil
}

func (s *SweeperService) prefundGasTRON(ctx context.Context, toAddr string, amount *big.Int) (string, error) {
	if s.TronClient == nil {
		return "", fmt.Errorf("tron client not initialized")
	}

	gw, err := s.loadGasWalletWithDailyReset(ctx, "TRON")
	if err != nil {
		return "", fmt.Errorf("failed to get gas wallet: %w", err)
	}
	if gw == nil {
		return "", fmt.Errorf("no gas wallet configured for TRON")
	}

	gwKeyHex, err := crypto_pkg.DecryptWithEnv(gw.PrivateKeyEnc, "APP_SECRET")
	if err != nil {
		return "", fmt.Errorf("failed to decrypt gas wallet key: %v", err)
	}

	privKey, err := crypto.HexToECDSA(gwKeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid private key hex: %v", err)
	}

	gasWalletAddr := gw.WalletAddress
	balanceSun, err := s.tronAccountBalanceSun(ctx, gasWalletAddr)
	if err != nil {
		return "", fmt.Errorf("failed to check gas wallet balance: %w", err)
	}

	currentBalanceDec := decimal.NewFromInt(balanceSun).Shift(-6)
	s.maybeAlertLowGasBalance(ctx, gw, "TRON", gasWalletAddr, currentBalanceDec)

	amountSun := amount.Int64()
	totalOutlaySun := amountSun + tronPrefundTransferFeeSun()
	totalOutlayDec := decimal.NewFromInt(totalOutlaySun).Shift(-6)
	if err := validateGasWalletOutlay(gw, totalOutlayDec); err != nil {
		s.recordGasOutlayPolicyBlocked(ctx, "TRON")
		return "", err
	}
	if balanceSun < totalOutlaySun {
		return "", fmt.Errorf("insufficient funds for prefund in gas wallet (%s < %s)", currentBalanceDec.String(), totalOutlayDec.String())
	}

	txExt, err := tronCallWithRetry(ctx, "Transfer", s.TronClient, func(c *client.GrpcClient) (*tronapi.TransactionExtention, error) {
		return c.Transfer(gasWalletAddr, toAddr, amountSun)
	})
	if err != nil {
		return "", fmt.Errorf("failed to build TRON prefund transfer: %w", err)
	}

	signedTx, err := transaction.SignTransactionECDSA(txExt.Transaction, privKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign TRON prefund transfer: %w", err)
	}

	result, err := tronCallWithRetry(ctx, "Broadcast", s.TronClient, func(c *client.GrpcClient) (*tronapi.Return, error) {
		return c.Broadcast(signedTx)
	})
	if err != nil {
		return "", fmt.Errorf("TRON prefund broadcast failed: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("TRON prefund broadcast error: %s", string(result.Message))
	}

	if err := s.GasWalletRepository.IncrementDailyGasUsed(ctx, "TRON", totalOutlayDec); err != nil {
		log.Printf("Failed to increment daily gas used: %v", err)
	}

	return hex.EncodeToString(txExt.GetTxid()), nil
}

func (s *SweeperService) prefundGasSolana(ctx context.Context, toAddr string, amount *big.Int) (string, error) {
	gw, err := s.loadGasWalletWithDailyReset(ctx, "SOLANA")
	if err != nil {
		return "", fmt.Errorf("failed to get gas wallet: %w", err)
	}
	if gw == nil {
		return "", fmt.Errorf("no gas wallet configured for SOLANA")
	}

	privKey, err := s.decodeEd25519PrivateKeyHex(gw.PrivateKeyEnc)
	if err != nil {
		return "", fmt.Errorf("invalid solana gas wallet key: %w", err)
	}

	client := s.getSolanaRPCClient()

	fromPub := privKey.Public().(ed25519.PublicKey)
	fromAccount := solana.PublicKeyFromBytes(fromPub)
	toAccount, err := solana.PublicKeyFromBase58(toAddr)
	if err != nil {
		return "", fmt.Errorf("invalid destination address: %w", err)
	}

	balance, err := client.GetBalance(ctx, fromAccount, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to check gas wallet balance: %w", err)
	}

	currentBalanceDec := decimal.NewFromUint64(balance.Value).Shift(-9)
	s.maybeAlertLowGasBalance(ctx, gw, "SOLANA", fromAccount.String(), currentBalanceDec)

	feeLamports := solanaFeeEstimateLamports()

	totalRequired := new(big.Int).Add(amount, new(big.Int).SetUint64(feeLamports))
	totalOutlayDec := decimal.NewFromBigInt(totalRequired, -9)
	if err := validateGasWalletOutlay(gw, totalOutlayDec); err != nil {
		s.recordGasOutlayPolicyBlocked(ctx, "SOLANA")
		return "", err
	}
	if new(big.Int).SetUint64(balance.Value).Cmp(totalRequired) < 0 {
		return "", fmt.Errorf("insufficient funds for prefund in gas wallet (%s < %s)", currentBalanceDec.String(), decimal.NewFromBigInt(totalRequired, -9).String())
	}

	recent, err := client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return "", fmt.Errorf("failed to get blockhash: %w", err)
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{
			system.NewTransferInstruction(
				amount.Uint64(),
				fromAccount,
				toAccount,
			).Build(),
		},
		recent.Value.Blockhash,
		solana.TransactionPayer(fromAccount),
	)
	if err != nil {
		return "", fmt.Errorf("failed to build solana prefund tx: %w", err)
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(fromAccount) {
			sk := solana.PrivateKey(privKey)
			return &sk
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to sign solana prefund tx: %w", err)
	}

	sig, err := client.SendTransaction(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("failed to send solana prefund tx: %w", err)
	}

	if err := s.GasWalletRepository.IncrementDailyGasUsed(ctx, "SOLANA", totalOutlayDec); err != nil {
		log.Printf("Failed to increment daily gas used: %v", err)
	}

	return sig.String(), nil
}

func (s *SweeperService) prefundGasTON(ctx context.Context, toAddr string, amount *big.Int) (string, error) {
	gw, err := s.loadGasWalletWithDailyReset(ctx, "TON")
	if err != nil {
		return "", fmt.Errorf("failed to get gas wallet: %w", err)
	}
	if gw == nil {
		return "", fmt.Errorf("no gas wallet configured for TON")
	}

	privKey, err := s.decodeEd25519PrivateKeyHex(gw.PrivateKeyEnc)
	if err != nil {
		return "", fmt.Errorf("invalid ton gas wallet key: %w", err)
	}

	api, err := s.getTonAPIClient(ctx)
	if err != nil {
		return "", err
	}

	w, err := wallet.FromPrivateKey(api, privKey, wallet.V4R2)
	if err != nil {
		return "", fmt.Errorf("failed to load TON gas wallet: %w", err)
	}

	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return "", err
	}

	balance, err := w.GetBalance(ctx, block)
	if err != nil {
		return "", err
	}

	currentBalanceDec := decimal.NewFromBigInt(balance.Nano(), -9)
	s.maybeAlertLowGasBalance(ctx, gw, "TON", w.WalletAddress().String(), currentBalanceDec)

	feeReserve := tonFeeEstimateNano()
	totalRequired := new(big.Int).Add(amount, feeReserve)
	totalOutlayDec := decimal.NewFromBigInt(totalRequired, -9)
	if err := validateGasWalletOutlay(gw, totalOutlayDec); err != nil {
		s.recordGasOutlayPolicyBlocked(ctx, "TON")
		return "", err
	}
	if balance.Nano().Cmp(totalRequired) < 0 {
		return "", fmt.Errorf("insufficient funds for prefund in gas wallet (%s < %s)", currentBalanceDec.String(), decimal.NewFromBigInt(totalRequired, -9).String())
	}

	addr, err := address.ParseAddr(toAddr)
	if err != nil {
		return "", fmt.Errorf("invalid destination address: %w", err)
	}

	msg, err := w.BuildTransfer(addr, tlb.FromNanoTON(amount), false, "")
	if err != nil {
		return "", fmt.Errorf("failed to build TON prefund transfer: %w", err)
	}

	tx, _, err := w.SendWaitTransaction(ctx, msg)
	if err != nil {
		return "", fmt.Errorf("TON prefund failed: %w", err)
	}

	if err := s.GasWalletRepository.IncrementDailyGasUsed(ctx, "TON", totalOutlayDec); err != nil {
		log.Printf("Failed to increment daily gas used: %v", err)
	}

	return hex.EncodeToString(tx.Hash), nil
}

func (s *SweeperService) emitAlert(ctx context.Context, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	log.Printf("crypto_alert %s", message)
	if s != nil && s.TxManager != nil {
		_ = s.TxManager.EmitAlert(ctx, "sweeper:"+message, "Sweeper alert", message, "error", 6*time.Hour)
	}
}

// Helper for TRON/TRC20 logic
func (s *SweeperService) sweepTRONWithSDK(ctx context.Context, sweep *model.Sweep) (string, error) {
	// 1. Verify Tron client is initialized
	if s.TronClient == nil {
		return "", fmt.Errorf("tron client not initialized")
	}

	// 2. Get Key
	stdKey, err := s.getPrivateKey(ctx, sweep.CryptoDepositID)
	if err != nil {
		return "", err
	}

	// 3. Balance Check & Gas (TRX)
	// TRC20 transfer consumes Energy (burned TRX).
	// We need to check if FromAddress has TRX.
	balanceSun, err := s.tronAccountBalanceSun(ctx, sweep.FromAddress)
	if err != nil {
		return "", fmt.Errorf("failed to get account: %w", err)
	}

	// 4. Determine if native or token
	var tokenAddress string
	dep, _ := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	if dep != nil {
		wallet, _ := s.HdWalletRepository.FindByID(ctx, dep.HDWalletID)
		if wallet != nil && wallet.ContractAddress != nil && *wallet.ContractAddress != "" {
			tokenAddress = *wallet.ContractAddress
		}
	}

	// If no token address, it's a native TRX sweep
	if tokenAddress == "" {
		if sweep.IsToken {
			return "", fmt.Errorf("missing TRC20 contract configuration")
		}

		reserveSun := int64(100_000)
		if isGasResidueSweep(sweep) {
			reserveSun = tronPrefundTransferFeeSun()
		} else {
			if reserveEnv := os.Getenv("TRON_SWEEP_RESERVE_SUN"); reserveEnv != "" {
				if parsed, parseErr := strconv.ParseInt(reserveEnv, 10, 64); parseErr == nil && parsed >= 0 {
					reserveSun = parsed
				}
			}
		}

		var amountSun int64
		if isGasResidueSweep(sweep) {
			amountSun = balanceSun - reserveSun
			if amountSun <= 0 {
				return "", fmt.Errorf("insufficient TRX balance to sweep after reserve")
			}
		} else {
			queuedAmount, conversionErr := exactTRXSunFromSweepAmount(sweep.Amount)
			if conversionErr != nil {
				s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
				return "", conversionErr
			}
			if balanceSun < queuedAmount {
				s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
				return "", fmt.Errorf("queued sweep amount exceeds live balance")
			}
			amountSun, err = subtractFeeFromQueuedInt64(queuedAmount, reserveSun, "TRX")
			if err != nil {
				s.recordNativeExactSweepConstructionBlocked(ctx, sweep)
				return "", err
			}
		}

		txExt, err := tronCallWithRetry(ctx, "Transfer", s.TronClient, func(c *client.GrpcClient) (*tronapi.TransactionExtention, error) {
			return c.Transfer(sweep.FromAddress, sweep.ToHotWallet, amountSun)
		})
		if err != nil {
			return "", fmt.Errorf("failed to build TRX transfer: %w", err)
		}
		// Sign
		signedTx, err := transaction.SignTransactionECDSA(txExt.Transaction, stdKey)
		if err != nil {
			return "", fmt.Errorf("failed to sign TRON transaction: %w", err)
		}
		// Broadcast
		result, err := tronCallWithRetry(ctx, "Broadcast", s.TronClient, func(c *client.GrpcClient) (*tronapi.Return, error) {
			return c.Broadcast(signedTx)
		})
		if err != nil {
			return "", fmt.Errorf("TRON broadcast failed: %w", err)
		}
		if result.Code != 0 {
			return "", fmt.Errorf("TRON broadcast error: %s", string(result.Message))
		}
		return hex.EncodeToString(txExt.GetTxid()), nil
	}

	if err := s.ensureTronEnergyRental(ctx, sweep); err != nil {
		return "", err
	}

	requiredEnergy := tronEnergyOrderEnergyAmount(tronEnergyOrderTransferCount())
	availableEnergy, energyErr := s.tronAccountAvailableEnergy(ctx, sweep.FromAddress)
	if energyErr != nil {
		return "", fmt.Errorf("failed to verify TRON energy after rental: %w", energyErr)
	}
	if !hasSufficientTronEnergy(availableEnergy, requiredEnergy) {
		return "", ErrEnergyRentalPending
	}

	minBalanceSun := tronTokenSweepMinBalanceSun()
	if balanceSun < minBalanceSun {
		deficit := big.NewInt(minBalanceSun - balanceSun)
		prefundHash, err := s.PrefundGasAddress(ctx, sweep.FromAddress, deficit, "TRON")
		if err != nil {
			return "", fmt.Errorf("prefund failed (TRON): %w", err)
		}
		return prefundHash, ErrPrefundInitiated
	}
	reserveSun := tronTokenSweepReserveSun()

	// Obtain Token Decimals and Scale Amount
	decimals := 6 // Default for USDT (TRC20)
	deposit, _ := s.DepositRepository.FindByID(ctx, sweep.CryptoDepositID)
	if deposit != nil {
		wallet, _ := s.HdWalletRepository.FindByID(ctx, deposit.HDWalletID)
		if wallet != nil {
			decimals = wallet.Decimals
		}
	}

	// Amount in Sun (units)
	amountSun, err := exactTokenUnitsFromSweepAmount(sweep.Amount, decimals)
	if err != nil {
		return "", fmt.Errorf("invalid TRC20 sweep amount: %w", err)
	}

	// Create Transaction via SDK (TRC20Send)
	txExt, err := tronCallWithRetry(ctx, "TRC20Send", s.TronClient, func(c *client.GrpcClient) (*tronapi.TransactionExtention, error) {
		return c.TRC20Send(sweep.FromAddress, sweep.ToHotWallet, tokenAddress, amountSun, reserveSun)
	})
	if err != nil {
		return "", fmt.Errorf("failed to build TRC20 transfer: %w", err)
	}

	// Sign
	signedTx, err := transaction.SignTransactionECDSA(txExt.Transaction, stdKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign TRON transaction: %w", err)
	}

	// Broadcast
	result, err := tronCallWithRetry(ctx, "Broadcast", s.TronClient, func(c *client.GrpcClient) (*tronapi.Return, error) {
		return c.Broadcast(signedTx)
	})
	if err != nil {
		return "", fmt.Errorf("TRON broadcast failed: %w", err)
	}

	if result.Code != 0 {
		return "", fmt.Errorf("TRON broadcast error: %s", string(result.Message))
	}

	return hex.EncodeToString(txExt.GetTxid()), nil
}

// SimpleFetcher implements txscript.PrevOutputFetcher
type SimpleFetcher struct {
	outputs map[wire.OutPoint]*wire.TxOut
}

func NewSimpleFetcher(unspent []btcjson.ListUnspentResult, params *chaincfg.Params) *SimpleFetcher {
	m := make(map[wire.OutPoint]*wire.TxOut)
	for _, u := range unspent {
		hash, _ := chainhash.NewHashFromStr(u.TxID)
		op := wire.NewOutPoint(hash, u.Vout)

		// Create TxOut
		// We need scriptPubKey from address
		addr, _ := btcutil.DecodeAddress(u.Address, params)
		pkScript, _ := txscript.PayToAddrScript(addr)

		amt := int64(u.Amount * 1e8)
		m[*op] = wire.NewTxOut(amt, pkScript)
	}
	return &SimpleFetcher{outputs: m}
}

func (f *SimpleFetcher) FetchPrevOutput(op wire.OutPoint) *wire.TxOut {
	return f.outputs[op]
}

// CheckPrefundConfirmations monitors transactions that are waiting for gas prefunding
func (s *SweeperService) CheckPrefundConfirmations(ctx context.Context) {
	sweeps, err := s.SweepRepository.FindWaitingForPrefund(ctx)
	if err != nil {
		log.Printf("Failed to fetch waiting prefunds: %v", err)
		return
	}

	for _, sw := range sweeps {
		if sw.TxHash == nil || *sw.TxHash == "" {
			errMsg := "prefund transaction hash is missing"
			s.SweepRepository.UpdateStatus(ctx, sw.ID, model.SweepStatusWaitingForGas, nil, &errMsg)
			continue
		}

		status, checkErr := s.checkPrefundTxState(ctx, sw)
		if checkErr != nil {
			log.Printf("Error checking prefund tx %s: %v", *sw.TxHash, checkErr)
			continue
		}

		switch status.State {
		case sweepTxStateConfirmed:
			log.Printf("Prefund confirmed for sweep %s, continuing sweep flow", sw.ID)
			nextStatus := model.SweepStatusCheckingActivation
			if err := s.SweepRepository.UpdateStatus(ctx, sw.ID, nextStatus, nil, nil); err != nil {
				log.Printf("Failed to move prefunded sweep %s to activation check: %v", sw.ID, err)
				continue
			}
			sw.Status = nextStatus
			go s.processSingleSweep(context.Background(), sw)
		case sweepTxStateFailed:
			errMsg := "prefund transaction failed"
			if status.Reason != "" {
				errMsg = "prefund transaction failed: " + status.Reason
			}
			s.SweepRepository.UpdateStatus(ctx, sw.ID, model.SweepStatusWaitingForGas, nil, &errMsg)
		}
	}
}

func (s *SweeperService) CheckEnergyRentalProgress(ctx context.Context) {
	if s.TronEnergyProvider == nil || !s.TronEnergyProvider.IsConfigured() {
		return
	}

	sweeps, err := s.SweepRepository.FindWaitingForEnergyRental(ctx)
	if err != nil {
		log.Printf("Failed to fetch sweeps waiting for energy rental: %v", err)
		return
	}

	for _, sw := range sweeps {
		if sw.ProviderOrderID == nil || strings.TrimSpace(*sw.ProviderOrderID) == "" {
			errMsg := "energy rental order id is missing"
			if updateErr := s.SweepRepository.UpdateStatus(ctx, sw.ID, model.SweepStatusFailed, nil, &errMsg); updateErr != nil {
				log.Printf("Failed to mark sweep %s failed after missing energy rental order id: %v", sw.ID, updateErr)
			}
			continue
		}

		order, orderErr := s.TronEnergyProvider.GetOrder(ctx, strings.TrimSpace(*sw.ProviderOrderID))
		if orderErr != nil {
			if hasTronEnergyRentalExpired(sw, time.Now().UTC()) {
				errText := "TRON energy rental expired before activation"
				if sw.Attempts < 3 {
					if updateErr := s.resetTronEnergyRentalForRetry(ctx, sw, errText); updateErr != nil {
						log.Printf("Failed to reset expired TRON energy rental for sweep %s: %v", sw.ID, updateErr)
					}
				} else {
					failedStatus := model.SweepStatusFailed
					if updateErr := s.SweepRepository.UpdateProviderState(ctx, sw.ID, repository.SweepProviderStateUpdate{
						Status:            &failedStatus,
						ProviderLastError: &errText,
					}); updateErr != nil {
						log.Printf("Failed to mark expired TRON energy rental sweep %s failed: %v", sw.ID, updateErr)
					}
				}
				continue
			}
			errText := orderErr.Error()
			unknownStatus := string(TronEnergyRentalStatusUnknown)
			if updateErr := s.SweepRepository.UpdateProviderState(ctx, sw.ID, repository.SweepProviderStateUpdate{
				ProviderStatus:    &unknownStatus,
				ProviderLastError: &errText,
			}); updateErr != nil {
				log.Printf("Failed to store TRON energy rental poll error for sweep %s: %v", sw.ID, updateErr)
			}
			log.Printf("Error checking TRON energy rental order %s for sweep %s: %v", *sw.ProviderOrderID, sw.ID, orderErr)
			continue
		}

		orderStatus := string(order.NormalizedStatus)
		update := repository.SweepProviderStateUpdate{
			ProviderStatus:       &orderStatus,
			ProviderLastError:    nil,
			ProviderMetadataJSON: stringPtr(order.MetadataJSON),
		}

		switch order.NormalizedStatus {
		case TronEnergyRentalStatusActive:
			nextStatus := model.SweepStatusProcessingTransaction
			update.Status = &nextStatus
		case TronEnergyRentalStatusPending, TronEnergyRentalStatusUnknown:
			if hasTronEnergyRentalExpired(sw, time.Now().UTC()) {
				errText := "TRON energy rental expired before activation"
				if sw.Attempts < 3 {
					if updateErr := s.resetTronEnergyRentalForRetry(ctx, sw, errText); updateErr != nil {
						log.Printf("Failed to reset expired TRON energy rental for sweep %s: %v", sw.ID, updateErr)
					}
				} else {
					failedStatus := model.SweepStatusFailed
					update.Status = &failedStatus
					update.ProviderLastError = &errText
				}
				break
			}
		case TronEnergyRentalStatusFailed:
			if sw.Attempts < 3 {
				// Clear the failed order so the next sweep cycle creates a fresh one.
				errText := "TRON energy rental order failed, retrying with fresh order"
				if updateErr := s.resetTronEnergyRentalForRetry(ctx, sw, errText); updateErr != nil {
					log.Printf("Failed to reset TRON energy rental order for sweep %s: %v", sw.ID, updateErr)
				}
				log.Printf("TRON energy rental order failed for sweep %s (attempt %d), resetting to retry", sw.ID, sw.Attempts)
				continue
			} else {
				failedStatus := model.SweepStatusFailed
				errText := "TRON energy rental order failed after max retries"
				update.Status = &failedStatus
				update.ProviderLastError = &errText
				log.Printf("TRON energy rental order failed for sweep %s after %d attempts, marking failed", sw.ID, sw.Attempts)
			}
		}

		if err := s.SweepRepository.UpdateProviderState(ctx, sw.ID, update); err != nil {
			log.Printf("Failed to persist TRON energy rental progress for sweep %s: %v", sw.ID, err)
			continue
		}

		if order.NormalizedStatus == TronEnergyRentalStatusActive {
			sw.Status = model.SweepStatusProcessingTransaction
			sw.ProviderStatus = &orderStatus
			go s.processSingleSweep(context.Background(), sw)
		}
	}
}

// CheckSweepConfirmations monitors sweeps that are in BROADCASTING status
func (s *SweeperService) CheckSweepConfirmations(ctx context.Context) {
	sweeps, err := s.SweepRepository.FindBroadcasting(ctx)
	if err != nil {
		log.Printf("Failed to fetch broadcasting sweeps: %v", err)
		return
	}

	for _, sw := range sweeps {
		if sw.TxHash == nil || *sw.TxHash == "" {
			continue
		}

		status, checkErr := s.checkBroadcastTxState(ctx, sw)
		if checkErr != nil {
			log.Printf("Error checking sweep tx %s: %v", *sw.TxHash, checkErr)
			continue
		}

		switch status.State {
		case sweepTxStateConfirmed:
			log.Printf("Sweep %s confirmed: %s", sw.ID, *sw.TxHash)
			s.markSweepConfirmed(ctx, sw)
			if status.Receipt != nil {
				s.persistSweepReceipt(ctx, sw.ID, status.Receipt)
			}
		case sweepTxStateFailed:
			s.failSweepFromChain(ctx, sw, status.Reason)
		}
	}
}

func (s *SweeperService) RecoverStaleBroadcastingSweeps(ctx context.Context, olderThan time.Duration) {
	sweeps, err := s.SweepRepository.FindBroadcastingOlderThan(ctx, olderThan)
	if err != nil {
		log.Printf("Failed to fetch stale broadcasting sweeps: %v", err)
		return
	}

	for _, sw := range sweeps {
		if sw.TxHash == nil || *sw.TxHash == "" {
			s.failSweepFromChain(ctx, sw, "broadcasting sweep is missing tx hash")
			continue
		}

		status, checkErr := s.checkBroadcastTxState(ctx, sw)
		if checkErr != nil {
			log.Printf("Failed to reconcile stale sweep %s (%s): %v", sw.ID, *sw.TxHash, checkErr)
			continue
		}

		switch status.State {
		case sweepTxStateConfirmed:
			s.markSweepConfirmed(ctx, sw)
			if status.Receipt != nil {
				s.persistSweepReceipt(ctx, sw.ID, status.Receipt)
			}
		case sweepTxStateFailed:
			s.failSweepFromChain(ctx, sw, status.Reason)
		case sweepTxStateNotFound:
			s.failSweepAsNotFound(ctx, sw)
		}
	}
}

func (s *SweeperService) checkPrefundTxState(ctx context.Context, sw *model.Sweep) (sweepTxCheckResult, error) {
	if sw == nil || sw.TxHash == nil || *sw.TxHash == "" {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "prefund transaction hash is missing"}, nil
	}

	if isTONNetwork(sw.Network) {
		gw, err := s.GasWalletRepository.GetByChainType(ctx, "TON")
		if err != nil {
			return sweepTxCheckResult{}, fmt.Errorf("failed to load TON gas wallet: %w", err)
		}
		if gw == nil || strings.TrimSpace(gw.WalletAddress) == "" {
			return sweepTxCheckResult{}, fmt.Errorf("TON gas wallet is not configured")
		}
		return s.checkTONAddressTxState(ctx, gw.WalletAddress, *sw.TxHash)
	}

	return s.checkNetworkTxState(ctx, sw.Network, *sw.TxHash)
}

func (s *SweeperService) checkBroadcastTxState(ctx context.Context, sw *model.Sweep) (sweepTxCheckResult, error) {
	if sw == nil || sw.TxHash == nil || *sw.TxHash == "" {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "broadcast transaction hash is missing"}, nil
	}

	if isTONNetwork(sw.Network) {
		return s.checkTONAddressTxState(ctx, sw.FromAddress, *sw.TxHash)
	}

	return s.checkNetworkTxState(ctx, sw.Network, *sw.TxHash)
}

func (s *SweeperService) checkTONAddressTxState(ctx context.Context, sourceAddress, txHash string) (sweepTxCheckResult, error) {
	if s.TonService != nil {
		return s.TonService.CheckAddressTxState(ctx, sourceAddress, txHash)
	}

	api, err := s.getTonAPIClient(ctx)
	if err != nil {
		return sweepTxCheckResult{}, err
	}

	helper := &TonService{
		api:       api,
		txClient:  api,
		txManager: s.TxManager,
	}
	return helper.CheckAddressTxState(ctx, sourceAddress, txHash)
}

func (s *SweeperService) checkNetworkTxState(ctx context.Context, network, txHash string) (sweepTxCheckResult, error) {
	switch network {
	case "TRC20", "TRON", "Tron":
		return s.checkTronTxState(ctx, txHash)
	case "SOLANA", "Solana":
		return s.checkSolanaTxState(ctx, txHash)
	case "BTC", "Bitcoin":
		return s.checkBTCTxState(txHash)
	default:
		return s.checkEVMTxState(ctx, network, txHash)
	}
}

func (s *SweeperService) checkEVMTxState(ctx context.Context, network, txHash string) (sweepTxCheckResult, error) {
	client, err := s.getEVMClient(network)
	if err != nil {
		return sweepTxCheckResult{}, err
	}
	hash := common.HexToHash(txHash)
	_, isPending, err := client.TransactionByHash(ctx, hash)
	if err != nil {
		if err == ethereum.NotFound {
			return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
		}
		return sweepTxCheckResult{}, err
	}
	if isPending {
		return sweepTxCheckResult{State: sweepTxStatePending}, nil
	}

	receipt, err := client.TransactionReceipt(ctx, hash)
	if err != nil {
		if err == ethereum.NotFound {
			return sweepTxCheckResult{State: sweepTxStatePending}, nil
		}
		return sweepTxCheckResult{}, err
	}

	if receipt.Status == types.ReceiptStatusFailed {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "transaction reverted on-chain"}, nil
	}
	return sweepTxCheckResult{State: sweepTxStateConfirmed}, nil
}

func (s *SweeperService) checkBTCTxState(txHash string) (sweepTxCheckResult, error) {
	if s.MempoolClient != nil {
		tx, err := s.MempoolClient.GetTransaction(txHash)
		if err != nil {
			if strings.Contains(err.Error(), "status 404") {
				return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
			}
			return sweepTxCheckResult{}, err
		}
		if tx.Status.Confirmed {
			return sweepTxCheckResult{State: sweepTxStateConfirmed}, nil
		}
		return sweepTxCheckResult{State: sweepTxStatePending}, nil
	}
	if s.BTCClient == nil {
		return sweepTxCheckResult{}, fmt.Errorf("btc transaction lookup is unavailable")
	}

	hash, err := chainhash.NewHashFromStr(txHash)
	if err != nil {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "invalid BTC tx hash"}, nil
	}

	tx, err := s.BTCClient.GetRawTransactionVerbose(hash)
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "no such mempool") || strings.Contains(errLower, "not found") {
			return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
		}
		return sweepTxCheckResult{}, err
	}
	if tx.Confirmations > 0 {
		return sweepTxCheckResult{State: sweepTxStateConfirmed}, nil
	}
	return sweepTxCheckResult{State: sweepTxStatePending}, nil
}

func (s *SweeperService) checkSolanaTxState(ctx context.Context, txHash string) (sweepTxCheckResult, error) {
	sig, err := solana.SignatureFromBase58(txHash)
	if err != nil {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: "invalid solana signature"}, nil
	}

	client := s.getSolanaRPCClient()
	out, err := client.GetSignatureStatuses(ctx, true, sig)
	if err != nil {
		return sweepTxCheckResult{}, err
	}
	if out == nil || len(out.Value) == 0 || out.Value[0] == nil {
		return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
	}

	status := out.Value[0]
	if status.Err != nil {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: fmt.Sprintf("transaction failed: %v", status.Err)}, nil
	}
	if status.ConfirmationStatus == rpc.ConfirmationStatusFinalized || status.ConfirmationStatus == rpc.ConfirmationStatusConfirmed {
		return sweepTxCheckResult{State: sweepTxStateConfirmed}, nil
	}
	return sweepTxCheckResult{State: sweepTxStatePending}, nil
}

func (s *SweeperService) checkTronTxState(ctx context.Context, txHash string) (sweepTxCheckResult, error) {
	if s.TronClient == nil {
		return sweepTxCheckResult{}, fmt.Errorf("tron client not initialized")
	}

	tx, err := tronCallWithRetry(ctx, "GetTransactionByID", s.TronClient, func(c *client.GrpcClient) (*troncore.Transaction, error) {
		return c.GetTransactionByID(txHash)
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
		}
		return sweepTxCheckResult{}, err
	}
	if tx == nil {
		return sweepTxCheckResult{State: sweepTxStateNotFound}, nil
	}

	info, err := tronCallWithRetry(ctx, "GetTransactionInfoByID", s.TronClient, func(c *client.GrpcClient) (*troncore.TransactionInfo, error) {
		return c.GetTransactionInfoByID(txHash)
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return sweepTxCheckResult{State: sweepTxStatePending}, nil
		}
		return sweepTxCheckResult{}, err
	}
	if info == nil {
		return sweepTxCheckResult{State: sweepTxStatePending}, nil
	}

	if info.Result != 0 {
		return sweepTxCheckResult{State: sweepTxStateFailed, Reason: describeTronTxFailure(info)}, nil
	}

	var receipt *repository.SweepReceiptData
	if r := info.GetReceipt(); r != nil {
		receipt = &repository.SweepReceiptData{}
		if v := r.GetEnergyUsageTotal(); v > 0 {
			receipt.EnergyUsed = &v
		}
		if v := r.GetEnergyFee(); v > 0 {
			receipt.EnergyFeeSun = &v
		}
		if v := r.GetNetUsage(); v > 0 {
			receipt.NetUsage = &v
		}
		if v := r.GetNetFee(); v > 0 {
			receipt.NetFeeSun = &v
		}
	}
	return sweepTxCheckResult{State: sweepTxStateConfirmed, Receipt: receipt}, nil
}

func describeTronTxFailure(info *troncore.TransactionInfo) string {
	if info == nil {
		return "tron transaction failed on-chain"
	}

	parts := make([]string, 0, 4)

	if msg := strings.TrimSpace(string(info.GetResMessage())); msg != "" {
		parts = append(parts, msg)
	}
	if receipt := info.GetReceipt(); receipt != nil {
		if result := strings.TrimSpace(receipt.GetResult().String()); result != "" && result != "SUCCESS" {
			parts = append(parts, "receipt="+result)
		}
		if energyTotal := receipt.GetEnergyUsageTotal(); energyTotal > 0 {
			parts = append(parts, fmt.Sprintf("energy_used=%d", energyTotal))
		}
		if energyFee := receipt.GetEnergyFee(); energyFee > 0 {
			parts = append(parts, fmt.Sprintf("energy_fee_sun=%d", energyFee))
		}
		if netFee := receipt.GetNetFee(); netFee > 0 {
			parts = append(parts, fmt.Sprintf("net_fee_sun=%d", netFee))
		}
	}
	if result := strings.TrimSpace(info.GetResult().String()); result != "" && result != "SUCESS" && result != "SUCCESS" {
		parts = append(parts, "result="+result)
	}

	if len(parts) == 0 {
		return "tron transaction failed on-chain"
	}
	return "tron transaction failed on-chain: " + strings.Join(parts, ", ")
}

func (s *SweeperService) markSweepConfirmed(ctx context.Context, sw *model.Sweep) {
	if sw == nil {
		return
	}

	if err := s.SweepRepository.UpdateStatus(ctx, sw.ID, model.SweepStatusConfirmed, nil, nil); err != nil {
		log.Printf("Failed to mark sweep %s as confirmed: %v", sw.ID, err)
		return
	}

	sw.Status = model.SweepStatusConfirmed
	if needsGasResidueSweep(sw) {
		if _, err := s.EnsureGasResidueSweepForOrigin(ctx, sw); err != nil {
			log.Printf("Failed to enqueue gas residue sweep for %s: %v", sw.ID, err)
		}
	}

	// If this sweep drained a pool address, cover all remaining unswept deposits
	// at that address by creating synthetic CONFIRMED sweep records with the same tx_hash.
	if s.PoolRepository != nil && sw.Network == "TRC20" {
		s.coverRemainingPoolDeposits(ctx, sw)
	}
}

// UpsertPoolBalanceSweep fetches the live on-chain USDT balance of a pool address and
// creates or updates a single PENDING sweep for it. This replaces the per-deposit sweep
// model for pool addresses: instead of one sweep per deposit, there is one sweep per
// address whose amount reflects the total accumulated balance at the time of the last deposit.
func (s *SweeperService) UpsertPoolBalanceSweep(ctx context.Context, dep *model.CryptoDeposit) (bool, error) {
	if dep == nil {
		return false, nil
	}
	if s.TronClient == nil {
		return false, fmt.Errorf("tron client not initialized for pool balance sweep")
	}

	wallet, err := s.HdWalletRepository.FindByID(ctx, dep.HDWalletID)
	if err != nil {
		return false, fmt.Errorf("failed to load wallet config: %w", err)
	}
	if wallet == nil || !wallet.IsEnabled {
		return false, fmt.Errorf("wallet config unavailable for pool balance sweep")
	}
	if wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == "" {
		return false, fmt.Errorf("missing TRC20 contract address for pool balance sweep")
	}
	if strings.TrimSpace(wallet.HotWalletAddress) == "" {
		return false, fmt.Errorf("no hot wallet configured for pool balance sweep")
	}

	contractAddress := strings.TrimSpace(*wallet.ContractAddress)
	fromAddress := strings.TrimSpace(dep.DepositAddress)
	if fromAddress == "" {
		fromAddress = strings.TrimSpace(dep.PaymentAddress)
	}

	balanceUnits, err := tronCallWithRetry(ctx, "TRC20ContractBalance", s.TronClient, func(c *client.GrpcClient) (*big.Int, error) {
		return c.TRC20ContractBalance(fromAddress, contractAddress)
	})
	if err != nil {
		return false, fmt.Errorf("failed to fetch pool USDT balance for %s: %w", fromAddress, err)
	}
	if balanceUnits == nil || balanceUnits.Sign() <= 0 {
		return false, nil // nothing to sweep
	}

	decimals := wallet.Decimals
	if decimals <= 0 {
		decimals = 6
	}
	liveBalance := decimal.NewFromBigInt(balanceUnits, -int32(decimals))
	if !liveBalance.GreaterThan(decimal.Zero) {
		return false, nil
	}

	existing, err := s.SweepRepository.FindActiveSweepByFromAddress(ctx, fromAddress, dep.Network)
	if err != nil {
		return false, fmt.Errorf("failed to check existing pool sweep: %w", err)
	}

	if existing != nil {
		// Sweep is already in flight — don't modify it.
		switch existing.Status {
		case model.SweepStatusBroadcasting,
			model.SweepStatusProcessingTransaction,
			model.SweepStatusWaitingForPrefund,
			model.SweepStatusWaitingForEnergyRental,
			model.SweepStatusCheckingActivation:
			return false, nil
		}
		// Update the amount to the current live balance if it changed.
		if !existing.Amount.Equal(liveBalance) {
			if err := s.SweepRepository.UpdateAmount(ctx, existing.ID, liveBalance); err != nil {
				return false, fmt.Errorf("failed to update pool sweep amount: %w", err)
			}
			log.Printf("Pool sweep %s amount updated to %s for address %s", existing.ID, liveBalance, fromAddress)
		}
		return false, nil
	}

	// No active sweep — create a new one linked to this deposit.
	latest, err := s.SweepRepository.FindLatestRoundByDepositID(ctx, dep.ID)
	if err != nil {
		return false, err
	}
	nextSeq := 1
	if latest != nil && latest.Sequence >= nextSeq {
		nextSeq = latest.Sequence + 1
	}

	sweep := &model.Sweep{
		CryptoDepositID: dep.ID,
		FromAddress:     fromAddress,
		ToHotWallet:     wallet.HotWalletAddress,
		Amount:          liveBalance,
		Coin:            dep.Coin,
		Network:         dep.Network,
		IsToken:         true,
		Sequence:        nextSeq,
		Purpose:         model.SweepPurposeDepositFunds,
		Status:          model.SweepStatusPending,
	}
	if err := s.SweepRepository.Create(ctx, sweep); err != nil {
		return false, fmt.Errorf("failed to create pool balance sweep: %w", err)
	}

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "pool_balance_sweep_created", map[string]string{
			"network": dep.Network,
		})
	}

	log.Printf("Pool balance sweep created for address %s: %s %s", fromAddress, liveBalance, dep.Coin)
	return true, nil
}

// CreatePoolSweep creates a single PENDING sweep for a pool address with an explicit
// total amount that spans all accumulated deposits. This bypasses EnsureSweepExists
// (which would only use the primary deposit's own receipt amount) so that the sweeper
// sends the full accumulated balance on-chain in one transaction.
func (s *SweeperService) CreatePoolSweep(ctx context.Context, primaryDepositID string, fromAddress string, totalAmount decimal.Decimal) (bool, error) {
	// Check no active sweep already exists for this deposit.
	existing, err := s.SweepRepository.FindByDepositID(ctx, primaryDepositID)
	if err != nil {
		return false, fmt.Errorf("failed to check existing sweep: %w", err)
	}
	if existing != nil {
		switch existing.Status {
		case model.SweepStatusFailed:
			// Allow re-queueing over a failed sweep.
		default:
			return false, nil // active or confirmed, nothing to do
		}
	}

	dep, err := s.DepositRepository.FindByID(ctx, primaryDepositID)
	if err != nil || dep == nil {
		return false, fmt.Errorf("deposit not found: %w", err)
	}

	wallet, err := s.HdWalletRepository.FindByID(ctx, dep.HDWalletID)
	if err != nil || wallet == nil || !wallet.IsEnabled || strings.TrimSpace(wallet.HotWalletAddress) == "" {
		return false, fmt.Errorf("no active hot wallet configured for TRC20/USDT")
	}

	latest, err := s.SweepRepository.FindLatestRoundByDepositID(ctx, primaryDepositID)
	if err != nil {
		return false, err
	}
	nextSeq := 1
	if latest != nil && latest.Sequence >= nextSeq {
		nextSeq = latest.Sequence + 1
	}

	sweep := &model.Sweep{
		CryptoDepositID: primaryDepositID,
		FromAddress:     fromAddress,
		ToHotWallet:     wallet.HotWalletAddress,
		Amount:          totalAmount,
		Coin:            dep.Coin,
		Network:         dep.Network,
		IsToken:         wallet.ContractAddress != nil && strings.TrimSpace(*wallet.ContractAddress) != "",
		Sequence:        nextSeq,
		Purpose:         model.SweepPurposeDepositFunds,
		Status:          model.SweepStatusPending,
	}
	if err := s.SweepRepository.Create(ctx, sweep); err != nil {
		return false, fmt.Errorf("failed to create pool sweep: %w", err)
	}
	return true, nil
}

// coverRemainingPoolDeposits finds other unswept deposits at a pool address and creates
// CONFIRMED sweep records for them, capped to what the primary sweep actually moved.
// This prevents phantom accounting: deposits are only marked covered up to primary.Amount.
func (s *SweeperService) coverRemainingPoolDeposits(ctx context.Context, primary *model.Sweep) {
	isPool, err := s.PoolRepository.IsPoolAddress(ctx, primary.FromAddress)
	if err != nil || !isPool {
		return
	}

	remaining, err := s.PoolRepository.FindUnsweptDeposits(ctx, primary.FromAddress)
	if err != nil {
		log.Printf("Pool cover: failed to find unswept deposits for %s: %v", primary.FromAddress, err)
		return
	}

	// Budget = primary.Amount minus what was already applied to the primary deposit.
	// The primary deposit's own coverage is handled by the sweep record already created for it;
	// we only distribute the remaining budget across other deposits.
	// If primary.Amount < sum of all unswept deposits (e.g. a direct on-chain outflow reduced
	// the balance before the sweep), we cover deposits in order until budget is exhausted and
	// alert so the operator can investigate.
	budget := primary.Amount
	var totalOther decimal.Decimal
	for _, u := range remaining {
		if u.DepositID != primary.CryptoDepositID {
			totalOther = totalOther.Add(u.UnsweptAmount)
		}
	}
	if totalOther.GreaterThan(budget) {
		log.Printf("Pool cover WARNING: primary sweep amount %s is less than total unswept other deposits %s at %s — partial coverage only",
			budget, totalOther, primary.FromAddress)
		s.emitAlert(context.Background(), fmt.Sprintf(
			"Pool cover gap at %s: swept %s but other deposits total %s. Manual reconciliation needed.",
			primary.FromAddress, budget, totalOther,
		))
	}

	covered := decimal.Zero
	for _, u := range remaining {
		if u.DepositID == primary.CryptoDepositID {
			continue
		}
		coverAmount := u.UnsweptAmount
		if covered.Add(coverAmount).GreaterThan(budget) {
			coverAmount = budget.Sub(covered)
		}
		if !coverAmount.IsPositive() {
			log.Printf("Pool cover: budget exhausted, deposit %s left uncovered", u.DepositID)
			break
		}

		synth := &model.Sweep{
			CryptoDepositID: u.DepositID,
			FromAddress:     primary.FromAddress,
			ToHotWallet:     primary.ToHotWallet,
			Amount:          coverAmount,
			Coin:            primary.Coin,
			Network:         primary.Network,
			IsToken:         primary.IsToken,
			Status:          model.SweepStatusConfirmed,
			Purpose:         model.SweepPurposeDepositFunds,
		}
		if err := s.SweepRepository.Create(ctx, synth); err != nil {
			log.Printf("Pool cover: failed to create sweep record for deposit %s: %v", u.DepositID, err)
			continue
		}
		if primary.TxHash != nil && *primary.TxHash != "" {
			if err := s.SweepRepository.UpdateStatus(ctx, synth.ID, model.SweepStatusConfirmed, primary.TxHash, nil); err != nil {
				log.Printf("Pool cover: failed to set tx_hash for synthetic sweep %s: %v", synth.ID, err)
			}
		}
		covered = covered.Add(coverAmount)
		log.Printf("Pool cover: deposit %s covered %s by pool sweep tx %v", u.DepositID, coverAmount, primary.TxHash)
	}
}

func (s *SweeperService) persistSweepReceipt(ctx context.Context, sweepID string, r *repository.SweepReceiptData) {
	if r == nil {
		return
	}
	if err := s.SweepRepository.UpdateReceiptData(ctx, sweepID, *r); err != nil {
		log.Printf("Failed to persist receipt data for sweep %s: %v", sweepID, err)
		return
	}
	energyUsed := int64(0)
	energyFeeSun := int64(0)
	netFeeSun := int64(0)
	if r.EnergyUsed != nil {
		energyUsed = *r.EnergyUsed
	}
	if r.EnergyFeeSun != nil {
		energyFeeSun = *r.EnergyFeeSun
	}
	if r.NetFeeSun != nil {
		netFeeSun = *r.NetFeeSun
	}
	totalFeeSun := energyFeeSun + netFeeSun
	log.Printf("Sweep %s receipt: energy_used=%d, energy_fee=%d sun (%.4f TRX), net_fee=%d sun (%.4f TRX), total_fee=%d sun (%.4f TRX)",
		sweepID,
		energyUsed,
		energyFeeSun, float64(energyFeeSun)/1_000_000,
		netFeeSun, float64(netFeeSun)/1_000_000,
		totalFeeSun, float64(totalFeeSun)/1_000_000,
	)
}

func (s *SweeperService) markSweepFailed(ctx context.Context, sw *model.Sweep, reason string) {
	if sw == nil {
		return
	}

	errMsg := strings.TrimSpace(reason)
	if errMsg == "" {
		errMsg = "sweep failed"
	}

	if err := s.SweepRepository.UpdateStatus(ctx, sw.ID, model.SweepStatusFailed, nil, &errMsg); err != nil {
		log.Printf("Failed to mark sweep %s as failed: %v", sw.ID, err)
		return
	}

	sw.Status = model.SweepStatusFailed
	sw.ErrorMessage = &errMsg
	s.maybeAlertRepeatedSweepFailures(ctx, sw, errMsg)

	if normalizeSweepPurpose(sw) != model.SweepPurposeDepositFunds {
		return
	}

	retryAttempt, retryErr := s.createRetryAttempt(ctx, sw)
	if retryErr != nil {
		log.Printf("Failed to auto-queue retry attempt for sweep %s: %v", sw.ID, retryErr)
		return
	}
	if retryAttempt != nil && retryAttempt.ID != sw.ID {
		log.Printf("Auto-queued pending retry attempt %s after failure of sweep %s", retryAttempt.ID, sw.ID)
	}
}

func (s *SweeperService) maybeAlertRepeatedSweepFailures(ctx context.Context, sw *model.Sweep, latestReason string) {
	if sw == nil || s.TxManager == nil {
		return
	}

	count, err := s.SweepRepository.CountRecentFailuresByDepositID(ctx, sw.CryptoDepositID, time.Now().Add(-6*time.Hour))
	if err != nil {
		log.Printf("Failed to count recent sweep failures for deposit %s: %v", sw.CryptoDepositID, err)
		return
	}
	if count < 3 {
		return
	}

	message := fmt.Sprintf(
		"Deposit %s has accumulated %d failed sweep rounds within the last 6 hours.\nNetwork: %s\nPurpose: %s\nLatest reason: %s",
		sw.CryptoDepositID,
		count,
		sw.Network,
		normalizeSweepPurpose(sw),
		latestReason,
	)
	if err := s.TxManager.EmitAlert(ctx, "sweep-failure-burst:"+sw.CryptoDepositID, "Repeated sweep failures", message, "error", 6*time.Hour); err != nil {
		log.Printf("Failed to notify admins about repeated sweep failures for deposit %s: %v", sw.CryptoDepositID, err)
	}
}

func (s *SweeperService) failSweepFromChain(ctx context.Context, sw *model.Sweep, reason string) {
	if sw == nil {
		return
	}

	errMsg := "transaction failed on-chain"
	if strings.TrimSpace(reason) != "" {
		errMsg = reason
	}

	s.markSweepFailed(ctx, sw, errMsg)

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "sweep_onchain_failed", map[string]string{
			"network": sw.Network,
		})
	}
}

func (s *SweeperService) failSweepAsNotFound(ctx context.Context, sw *model.Sweep) {
	if sw == nil {
		return
	}

	errMsg := "broadcast transaction not found on-chain after stale timeout"
	s.markSweepFailed(ctx, sw, errMsg)

	if s.TxManager != nil {
		s.TxManager.RecordCounter(ctx, "sweep_broadcast_not_found", map[string]string{
			"network": sw.Network,
		})
		if isTONNetwork(sw.Network) {
			s.TxManager.RecordCounter(ctx, "ton_sweep_not_found", map[string]string{
				"network": sw.Network,
			})
		}
		message := fmt.Sprintf(
			"Sweep %s for deposit %s was left in BROADCASTING, but tx %s was not found on-chain after the stale timeout.\nNetwork: %s\nFrom: %s\nTo: %s",
			sw.ID,
			sw.CryptoDepositID,
			firstNonEmptyPtr(sw.TxHash),
			sw.Network,
			sw.FromAddress,
			sw.ToHotWallet,
		)
		if err := s.TxManager.EmitAlert(ctx, "sweep-broadcast-not-found:"+sw.ID, "Stale sweep broadcast not found", message, "error", 24*time.Hour); err != nil {
			log.Printf("Failed to notify admins about stale sweep %s: %v", sw.ID, err)
		}
	}
}
