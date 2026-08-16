package service

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
)

var erc20TransferEventSig = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

type evmRPCBlock struct {
	Transactions []evmRPCTransaction `json:"transactions"`
}

type evmRPCTransaction struct {
	Hash  common.Hash     `json:"hash"`
	To    *common.Address `json:"to"`
	Value *hexutil.Big    `json:"value"`
}

type EVMScanner struct {
	ChainType   string
	RPCUrl      string
	Client      *ethclient.Client
	depositRepo *repository.DepositRepository
	hdRepo      *repository.HdWalletRepository
	scanRepo    *repository.ScanningRepository
	txManager   *TransactionManager
}

func NewEVMScanner(chainType, rpcUrl string, depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *TransactionManager) (*EVMScanner, error) {
	client, err := ethclient.Dial(rpcUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s node: %w", chainType, err)
	}

	return &EVMScanner{
		ChainType:   chainType,
		RPCUrl:      rpcUrl,
		Client:      client,
		depositRepo: depositRepo,
		hdRepo:      hdRepo,
		scanRepo:    repository.NewScanningRepository(),
		txManager:   txManager,
	}, nil
}

func (s *EVMScanner) Start(ctx context.Context) {
	log.Printf("Starting scanner for %s", s.ChainType)

	if err := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, s.ChainType); err != nil {
		log.Printf("Failed to rebuild %s watched-address cache on startup: %v", s.ChainType, err)
	}

	// Get last scanned block from DB
	scanHeight, clonedLegacyState, err := s.scanRepo.GetLastScannedBlockOrClone(ctx, s.ChainType)
	if clonedLegacyState {
		log.Printf("Cloned legacy EVM scan state for %s at block %d", s.ChainType, scanHeight)
		if s.txManager != nil {
			s.txManager.RecordCounter(ctx, "evm_scan_state_cloned", map[string]string{
				"network": s.ChainType,
			})
		}
	}
	initialized := err == nil && scanHeight > 0
	if err != nil {
		log.Printf("Failed to load last scanned block for %s: %v", s.ChainType, err)
	}

	if !initialized {
		currentBlock, err := s.Client.BlockNumber(ctx)
		if err != nil {
			log.Printf("Failed to get latest block for %s during startup, will retry: %v", s.ChainType, err)
		} else {
			scanHeight = int64(currentBlock) - 10 // Start slightly behind
			if scanHeight < 0 {
				scanHeight = 0
			}
			initialized = true
		}
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping scanner for %s", s.ChainType)
			return
		case <-ticker.C:
			if !initialized {
				latest, err := s.Client.BlockNumber(ctx)
				if err != nil {
					log.Printf("Error fetching initial block number for %s: %v", s.ChainType, err)
					continue
				}
				scanHeight = int64(latest) - 10
				if scanHeight < 0 {
					scanHeight = 0
				}
				initialized = true
			}

			latest, err := s.Client.BlockNumber(ctx)
			if err != nil {
				log.Printf("Error fetching block number: %v", err)
				continue
			}

			// Process blocks range
			for i := scanHeight + 1; i <= int64(latest); i++ {
				if err := s.ProcessBlock(ctx, i); err != nil {
					log.Printf("Failed to process block %d: %v", i, err)
					break
				}
				scanHeight = i
				s.scanRepo.UpdateLastScannedBlock(ctx, s.ChainType, scanHeight)
			}

			// 2. Production Check: Confirmation tracking
			s.checkConfirmations(ctx, int64(latest))
		}
	}
}

func (s *EVMScanner) checkConfirmations(ctx context.Context, latestBlock int64) {
	deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, s.ChainType)
	if err != nil {
		return
	}

	for _, dep := range deposits {
		if !shouldTrackDepositTx(dep) {
			continue
		}

		finalizationThreshold := dep.RequiredConfirmations
		wallet, _ := s.hdRepo.FindByID(ctx, dep.HDWalletID)
		if wallet != nil && wallet.FinalizationConfirmations > 0 {
			finalizationThreshold = wallet.FinalizationConfirmations
		}
		if finalizationThreshold < dep.RequiredConfirmations {
			finalizationThreshold = dep.RequiredConfirmations
		}

		resolution, confs, err := s.classifyDepositTxConfirmation(ctx, dep, latestBlock)
		if err != nil {
			log.Printf("Failed to reconcile %s deposit %s tx %s: %v", s.ChainType, dep.ID, *dep.TxHash, err)
			continue
		}
		if handleErr := s.txManager.HandleDepositTxResolution(ctx, s.ChainType, dep, resolution); handleErr != nil {
			log.Printf("Failed to handle %s deposit resolution for %s: %v", s.ChainType, dep.ID, handleErr)
			continue
		}
		if resolution != depositTxResolutionConfirmed {
			continue
		}
		if dep.Status == "DETECTED" && confs >= int64(dep.RequiredConfirmations) {
			log.Printf("✅ DEPOSIT CONFIRMED for %s: %d confirmations reached (target: %d)", dep.ID, confs, dep.RequiredConfirmations)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "CONFIRMED", nil, nil); err != nil {
				log.Printf("Failed to persist %s deposit confirmation for %s: %v", s.ChainType, dep.ID, err)
			}
		} else if dep.Status == "CONFIRMED" && confs >= int64(finalizationThreshold) {
			log.Printf("💎 DEPOSIT FINALIZED for %s: %d confirmations reached (finalization: %d)", dep.ID, confs, finalizationThreshold)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "FINALIZED", nil, nil); err != nil {
				log.Printf("Failed to persist %s deposit finalization for %s: %v", s.ChainType, dep.ID, err)
			}
		} else if confs > 0 {
			if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, int(confs)); err != nil {
				log.Printf("Failed to persist %s confirmations for %s: %v", s.ChainType, dep.ID, err)
			}
		}
	}
}

func (s *EVMScanner) classifyDepositTxConfirmation(ctx context.Context, dep *model.CryptoDeposit, latestBlock int64) (depositTxResolution, int64, error) {
	if dep == nil || dep.TxHash == nil || *dep.TxHash == "" {
		return depositTxResolutionError, 0, nil
	}

	receipt, err := s.Client.TransactionReceipt(ctx, common.HexToHash(*dep.TxHash))
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			return depositTxResolutionNotFound, 0, nil
		}
		return depositTxResolutionError, 0, err
	}
	if receipt == nil || receipt.BlockNumber == nil {
		return depositTxResolutionPending, 0, nil
	}

	confs := latestBlock - receipt.BlockNumber.Int64() + 1
	if confs < 0 {
		confs = 0
	}

	return depositTxResolutionConfirmed, confs, nil
}

func (s *EVMScanner) ProcessBlock(ctx context.Context, blockHeight int64) error {
	if database.Rdb != nil {
		monitoredKey, _ := monitoredCacheKeys(s.ChainType)
		if count, err := database.Rdb.SCard(ctx, monitoredKey).Result(); err == nil && count == 0 {
			if rebuildErr := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, s.ChainType); rebuildErr != nil {
				log.Printf("Failed to rebuild %s watched-address cache: %v", s.ChainType, rebuildErr)
			}
		}
	}

	block, err := s.fetchRPCBlock(ctx, blockHeight)
	if err != nil {
		return err
	}
	return s.processBlockWithoutReceipts(ctx, blockHeight, block)
}

func (s *EVMScanner) processBlockWithoutReceipts(ctx context.Context, blockHeight int64, block *evmRPCBlock) error {
	if block == nil {
		return fmt.Errorf("block is required")
	}

	logs, err := s.fetchTransferLogsForBlock(ctx, blockHeight)
	if err != nil {
		return err
	}
	s.handleTokenTransferLogs(ctx, logs)

	for _, tx := range block.Transactions {
		if tx.To == nil || tx.Value == nil || tx.Value.ToInt().Sign() <= 0 {
			continue
		}

		dep, err := resolveWatchedDepositByAddress(ctx, s.depositRepo, s.txManager, s.ChainType, tx.To.Hex())
		if err != nil || dep == nil {
			continue
		}

		wallet, walletErr := s.hdRepo.FindByID(ctx, dep.HDWalletID)
		if !evmDepositAcceptsNativeTransfer(dep, wallet) {
			if walletErr != nil {
				log.Printf("Failed to load wallet config for native-transfer guard on deposit %s: %v", dep.ID, walletErr)
			}
			if s.txManager != nil {
				s.txManager.RecordCounter(ctx, "evm_native_transfer_to_token_invoice_ignored", map[string]string{
					"coin":    dep.Coin,
					"network": dep.Network,
				})
			}
			continue
		}

		receipt, err := s.Client.TransactionReceipt(ctx, tx.Hash)
		if err != nil {
			return err
		}
		if receipt == nil || receipt.Status != types.ReceiptStatusSuccessful {
			continue
		}

		s.handleDetectedTransfer(ctx, dep, tx.Hash.Hex(), tx.Value.ToInt(), nil)
	}

	return nil
}

func (s *EVMScanner) fetchRPCBlock(ctx context.Context, blockHeight int64) (*evmRPCBlock, error) {
	var block evmRPCBlock
	if err := s.Client.Client().CallContext(ctx, &block, "eth_getBlockByNumber", hexutil.EncodeBig(big.NewInt(blockHeight)), true); err != nil {
		return nil, err
	}
	return &block, nil
}

func (s *EVMScanner) fetchTransferLogsForBlock(ctx context.Context, blockHeight int64) ([]*types.Log, error) {
	entries, err := s.Client.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: big.NewInt(blockHeight),
		ToBlock:   big.NewInt(blockHeight),
		Topics:    [][]common.Hash{{erc20TransferEventSig}},
	})
	if err != nil {
		return nil, err
	}

	logs := make([]*types.Log, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		logs = append(logs, &entry)
	}
	return logs, nil
}

func (s *EVMScanner) handleTokenTransferLogs(ctx context.Context, logs []*types.Log) {
	type aggregatedTransfer struct {
		deposit *model.CryptoDeposit
		txHash  string
		amount  decimal.Decimal
	}

	aggregated := make(map[string]*aggregatedTransfer)

	for _, entry := range logs {
		if entry == nil || len(entry.Topics) < 3 || entry.Topics[0] != erc20TransferEventSig {
			continue
		}
		if len(entry.Data) == 0 {
			continue
		}

		recipient := common.HexToAddress(entry.Topics[2].Hex()).Hex()
		dep, err := resolveWatchedDepositByAddress(ctx, s.depositRepo, s.txManager, s.ChainType, recipient)
		if err != nil || dep == nil {
			continue
		}

		wallet, err := s.hdRepo.FindByID(ctx, dep.HDWalletID)
		if err != nil || wallet == nil || wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == "" {
			continue
		}
		if !strings.EqualFold(entry.Address.Hex(), *wallet.ContractAddress) {
			log.Printf(
				"⚠️ Ignoring token transfer to monitored address %s from wrong contract %s (expected %s)",
				recipient,
				entry.Address.Hex(),
				*wallet.ContractAddress,
			)
			continue
		}

		_, amount, ok := decodeERC20TransferLog(entry, wallet.Decimals)
		if !ok {
			continue
		}
		key := dep.ID + ":" + entry.TxHash.Hex()
		if existing, ok := aggregated[key]; ok {
			existing.amount = existing.amount.Add(amount)
			continue
		}

		aggregated[key] = &aggregatedTransfer{
			deposit: dep,
			txHash:  entry.TxHash.Hex(),
			amount:  amount,
		}
	}

	for _, transfer := range aggregated {
		if transfer == nil || transfer.deposit == nil {
			continue
		}
		s.HandleDeposit(ctx, transfer.deposit, transfer.txHash, transfer.amount)
	}
}

func decodeERC20TransferLog(entry *types.Log, decimals int) (string, decimal.Decimal, bool) {
	if entry == nil || len(entry.Topics) < 3 || entry.Topics[0] != erc20TransferEventSig || len(entry.Data) == 0 {
		return "", decimal.Zero, false
	}

	recipient := strings.ToLower(common.HexToAddress(entry.Topics[2].Hex()).Hex())
	amountRaw := new(big.Int).SetBytes(entry.Data)
	amount := decimal.NewFromBigInt(amountRaw, int32(-decimals))
	return recipient, amount, true
}

func evmNativeCoinForNetwork(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "ERC20", "ARBITRUM":
		return "ETH"
	case "BEP20":
		return "BNB"
	case "POLYGON":
		return "MATIC"
	default:
		return ""
	}
}

func evmDepositAcceptsNativeTransfer(dep *model.CryptoDeposit, wallet *model.HDWallet) bool {
	if dep == nil {
		return false
	}
	if wallet != nil && wallet.ContractAddress != nil && strings.TrimSpace(*wallet.ContractAddress) != "" {
		return false
	}
	expectedNative := evmNativeCoinForNetwork(dep.Network)
	return expectedNative != "" && strings.EqualFold(strings.TrimSpace(dep.Coin), expectedNative)
}

func (s *EVMScanner) handleDetectedTransfer(ctx context.Context, dep *model.CryptoDeposit, txHash string, amountRaw *big.Int, expectedContract *common.Address) {
	if dep == nil {
		return
	}

	wallet, err := s.hdRepo.FindByID(ctx, dep.HDWalletID)
	decimals := 18
	if err == nil && wallet != nil && wallet.Decimals > 0 {
		decimals = wallet.Decimals
	}

	if expectedContract != nil {
		if wallet == nil || wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == "" {
			log.Printf("⚠️ Ignoring token transfer for wallet without contract address: deposit=%s", dep.ID)
			return
		}
		if !strings.EqualFold(expectedContract.Hex(), *wallet.ContractAddress) {
			log.Printf("⚠️ Ignoring token transfer to %s from wrong contract %s (expected %s)", dep.DepositAddress, expectedContract.Hex(), *wallet.ContractAddress)
			return
		}
	}

	amount := decimal.NewFromBigInt(amountRaw, int32(-decimals))
	s.HandleDeposit(ctx, dep, txHash, amount)
}

// HandleDeposit is called when a monitored address receives funds
func (s *EVMScanner) HandleDeposit(ctx context.Context, dep *model.CryptoDeposit, txHash string, amount decimal.Decimal) error {
	targetAddress := dep.PaymentAddress
	if targetAddress == "" {
		targetAddress = dep.DepositAddress
	}
	if isOperationalPrefundTx(ctx, s.txManager, dep.Network, targetAddress, txHash) {
		log.Printf("Ignoring operational prefund for %s on %s: %s", dep.ID, dep.Network, txHash)
		return nil
	}

	log.Printf("💰 DEPOSIT DETECTED for %s: %s received %s (tx: %s)", dep.ID, dep.DepositAddress, amount, txHash)
	return s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txHash, &amount)
}
