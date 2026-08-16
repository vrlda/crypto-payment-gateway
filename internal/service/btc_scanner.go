package service

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"crypto_payment_gateway_core/pkg/mempool"
	"log"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/shopspring/decimal"
)

type btcDetectedPayment struct {
	deposit *model.CryptoDeposit
	amount  decimal.Decimal
}

type BTCScanner struct {
	Client        *rpcclient.Client
	MempoolClient *mempool.Client
	depositRepo   *repository.DepositRepository
	hdRepo        *repository.HdWalletRepository
	scanRepo      *repository.ScanningRepository
	txManager     *TransactionManager
	UseAPI        bool
}

func NewBTCScanner(host, user, pass string, depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *TransactionManager, useAPI bool) (*BTCScanner, error) {
	if useAPI {
		return &BTCScanner{
			MempoolClient: mempool.NewClient(false), // Mainnet default
			depositRepo:   depositRepo,
			hdRepo:        hdRepo,
			scanRepo:      repository.NewScanningRepository(),
			txManager:     txManager,
			UseAPI:        true,
		}, nil
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         host,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}

	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		return nil, err
	}

	return &BTCScanner{
		Client:      client,
		depositRepo: depositRepo,
		hdRepo:      hdRepo,
		scanRepo:    repository.NewScanningRepository(),
		txManager:   txManager,
		UseAPI:      false,
	}, nil
}

func (s *BTCScanner) Start(ctx context.Context) {
	log.Println("Starting BTC Scanner...")

	if s.UseAPI {
		s.startAPIPoller(ctx)
		return
	}

	if err := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, "BTC"); err != nil {
		log.Printf("Failed to rebuild BTC watched-address cache on startup: %v", err)
	}

	// 1. Get last scanned block from DB
	scanHeight, err := s.scanRepo.GetLastScannedBlock(ctx, "BTC")
	if err != nil || scanHeight == 0 {
		info, err := s.Client.GetBlockChainInfo()
		if err != nil {
			log.Printf("Failed to get BTC chain info: %v", err)
			return
		}
		scanHeight = int64(info.Blocks)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Client.Shutdown()
			return
		case <-ticker.C:
			count, err := s.Client.GetBlockCount()
			if err != nil {
				log.Printf("BTC GetBlockCount error: %v", err)
				continue
			}

			if database.Rdb != nil {
				monitoredKey, _ := monitoredCacheKeys("BTC")
				if count, err := database.Rdb.SCard(ctx, monitoredKey).Result(); err == nil && count == 0 {
					if rebuildErr := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, "BTC"); rebuildErr != nil {
						log.Printf("Failed to rebuild BTC watched-address cache: %v", rebuildErr)
					}
				}
			}

			// Scan range
			for i := scanHeight + 1; i <= count; i++ {
				hash, err := s.Client.GetBlockHash(i)
				if err != nil {
					break
				}

				block, err := s.Client.GetBlockVerboseTx(hash)
				if err != nil {
					log.Printf("Failed to get block %d: %v", i, err)
					break
				}

				for _, tx := range block.Tx {
					matches := make(map[string]*btcDetectedPayment)
					for _, vout := range tx.Vout {
						for _, addr := range vout.ScriptPubKey.Addresses {
							dep, err := resolveWatchedDepositByAddress(ctx, s.depositRepo, s.txManager, "BTC", addr)
							if err != nil || dep == nil {
								continue
							}
							accumulateDetectedBTCPayment(matches, dep, decimal.NewFromFloat(vout.Value))
						}
					}
					s.emitDetectedBTCPayments(ctx, tx.Txid, matches)
				}
				scanHeight = i
				s.scanRepo.UpdateLastScannedBlock(ctx, "BTC", scanHeight)
			}

			// 2. Continuous Confirmation Tracking
			s.checkConfirmations(ctx, int64(count))
		}
	}
}

func (s *BTCScanner) checkConfirmations(ctx context.Context, latestHeight int64) {
	deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, "BTC")
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

		resolution, confs, err := s.classifyDepositTxConfirmation(dep)
		if err != nil {
			log.Printf("Failed to reconcile BTC deposit %s tx %s: %v", dep.ID, *dep.TxHash, err)
			continue
		}
		if handleErr := s.txManager.HandleDepositTxResolution(ctx, "BTC", dep, resolution); handleErr != nil {
			log.Printf("Failed to handle BTC deposit resolution for %s: %v", dep.ID, handleErr)
			continue
		}
		if resolution != depositTxResolutionConfirmed {
			continue
		}

		if dep.Status == "DETECTED" && confs >= int64(dep.RequiredConfirmations) {
			log.Printf("✅ BTC DEPOSIT CONFIRMED for %s: %d confirmations reached (target: %d)", dep.ID, confs, dep.RequiredConfirmations)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "CONFIRMED", nil, nil); err != nil {
				log.Printf("Failed to persist BTC deposit confirmation for %s: %v", dep.ID, err)
			}
		} else if dep.Status == "CONFIRMED" && confs >= int64(finalizationThreshold) {
			log.Printf("💎 BTC DEPOSIT FINALIZED for %s: %d confirmations reached (finalization: %d)", dep.ID, confs, finalizationThreshold)
			if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "FINALIZED", nil, nil); err != nil {
				log.Printf("Failed to persist BTC deposit finalization for %s: %v", dep.ID, err)
			}
		} else if confs > 0 {
			if err := s.depositRepo.UpdateConfirmations(ctx, dep.ID, int(confs)); err != nil {
				log.Printf("Failed to persist BTC confirmations for %s: %v", dep.ID, err)
			}
		}
	}
}

func (s *BTCScanner) classifyDepositTxConfirmation(dep *model.CryptoDeposit) (depositTxResolution, int64, error) {
	if dep == nil || dep.TxHash == nil || *dep.TxHash == "" {
		return depositTxResolutionError, 0, nil
	}

	if s.UseAPI {
		tx, err := s.MempoolClient.GetTransaction(*dep.TxHash)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "404") || strings.Contains(strings.ToLower(err.Error()), "not found") {
				return depositTxResolutionNotFound, 0, nil
			}
			return depositTxResolutionError, 0, err
		}
		if !tx.Status.Confirmed {
			return depositTxResolutionPending, 0, nil
		}

		info, err := s.MempoolClient.GetTipHeight()
		if err == nil && tx.Status.BlockHeight > 0 {
			return depositTxResolutionConfirmed, int64(info - tx.Status.BlockHeight + 1), nil
		}
		return depositTxResolutionConfirmed, 1, nil
	}

	if s.Client == nil {
		return depositTxResolutionError, 0, nil
	}

	hash, err := chainhash.NewHashFromStr(*dep.TxHash)
	if err != nil {
		return depositTxResolutionError, 0, err
	}

	tx, err := s.Client.GetRawTransactionVerbose(hash)
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "no such mempool") || strings.Contains(errLower, "not found") {
			return depositTxResolutionNotFound, 0, nil
		}
		return depositTxResolutionError, 0, err
	}
	if tx.Confirmations > 0 {
		return depositTxResolutionConfirmed, int64(tx.Confirmations), nil
	}
	return depositTxResolutionPending, 0, nil
}

func (s *BTCScanner) startAPIPoller(ctx context.Context) {
	log.Println("BTC Scanner running in API Mode (Lite)")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	if err := rebuildWatchedAddressCache(ctx, s.depositRepo, s.txManager, "BTC"); err != nil {
		log.Printf("Failed to rebuild BTC watched-address cache on startup: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deposits, err := s.depositRepo.FindWatchedByNetwork(ctx, "BTC")
			if err != nil {
				continue
			}

			for _, dep := range deposits {
				address := dep.PaymentAddress
				if address == "" {
					address = dep.DepositAddress
				}

				txs, err := s.MempoolClient.GetAddressTxs(address)
				if err != nil {
					log.Printf("Failed to poll address %s: %v", address, err)
					continue
				}

				for _, tx := range txs {
					matches := make(map[string]*btcDetectedPayment)
					for _, vout := range tx.Vout {
						if vout.ScriptPubKeyAddress == address {
							accumulateDetectedBTCPayment(matches, dep, decimal.NewFromInt(vout.Value).Shift(-8))
						}
					}
					s.emitDetectedBTCPayments(ctx, tx.TxID, matches)
				}
				time.Sleep(200 * time.Millisecond)
			}

			s.checkConfirmations(ctx, 0)
		}
	}
}

func (s *BTCScanner) HandleDeposit(ctx context.Context, dep *model.CryptoDeposit, txHash string, amount float64) {
	decAmount := decimal.NewFromFloat(amount)
	log.Printf("💰 BTC DEPOSIT DETECTED for %s: %s received %s (tx: %s)", dep.ID, dep.DepositAddress, decAmount, txHash)
	if err := s.txManager.HandleDepositUpdate(ctx, dep.ID, "DETECTED", &txHash, &decAmount); err != nil {
		log.Printf("Failed to persist BTC deposit update for %s (tx: %s): %v", dep.ID, txHash, err)
	}
}

func accumulateDetectedBTCPayment(matches map[string]*btcDetectedPayment, dep *model.CryptoDeposit, amount decimal.Decimal) {
	if dep == nil {
		return
	}

	if existing, ok := matches[dep.ID]; ok {
		existing.amount = existing.amount.Add(amount)
		return
	}

	matches[dep.ID] = &btcDetectedPayment{
		deposit: dep,
		amount:  amount,
	}
}

func (s *BTCScanner) emitDetectedBTCPayments(ctx context.Context, txHash string, matches map[string]*btcDetectedPayment) {
	for _, match := range matches {
		if match == nil || match.deposit == nil {
			continue
		}

		amountFloat, _ := match.amount.Float64()
		s.HandleDeposit(ctx, match.deposit, txHash, amountFloat)
	}
}
