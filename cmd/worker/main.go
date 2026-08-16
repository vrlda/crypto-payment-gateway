package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/repository"
	"crypto_payment_gateway_core/internal/service"
	"crypto_payment_gateway_core/pkg/mempool"

	"github.com/btcsuite/btcd/rpcclient"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/joho/godotenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type tronProviderSpec struct {
	Name   string
	Target string
	APIKey string
}

func main() {
	loadEnvironment()

	if err := database.InitDB(); err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	defer database.DB.Close()

	if err := database.InitRedis(); err != nil {
		log.Fatalf("failed to initialize Redis: %v", err)
	}
	defer database.Rdb.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	txRepo := repository.NewTransactionRepository()
	merchantRepo := repository.NewMerchantRepository()
	sweepRepo := repository.NewSweepRepository()
	gasRepo := repository.NewGasWalletRepository()
	hdRepo := repository.NewHdWalletRepository()
	depositRepo := repository.NewDepositRepository()
	settingsRepo := repository.NewSystemSettingsRepository()
	poolRepo := repository.NewTRC20PoolRepository()

	opsService := service.NewOperationsService(hdRepo)
	webhookService := service.NewWebhookService(merchantRepo)
	txManager := service.NewTransactionManager(
		webhookService,
		depositRepo,
		merchantRepo,
		txRepo,
		sweepRepo,
		hdRepo,
		opsService,
		poolRepo,
	)
	hdWallet := service.NewHDWalletService(false)

	evmChains := map[string]string{
		"ERC20":    os.Getenv("ETH_RPC_URL"),
		"BEP20":    os.Getenv("BSC_RPC_URL"),
		"POLYGON":  os.Getenv("POLYGON_RPC_URL"),
		"ARBITRUM": os.Getenv("ARBITRUM_RPC_URL"),
		"EVM":      os.Getenv("EVM_RPC_URL"),
	}
	evmClients := configureEVMClients(evmChains)
	tronClient := configureTronClients()

	solanaService := service.NewSolanaService(os.Getenv("SOLANA_RPC_URL"), depositRepo, hdRepo, txManager, false)
	go solanaService.StartPoller(ctx)

	tonService, err := service.NewTonService(ctx, depositRepo, hdRepo, txManager, false)
	if err != nil {
		log.Printf("TON service disabled: %v", err)
	} else {
		go tonService.StartPoller(ctx)
	}

	tronScanner := service.NewTronScanner(depositRepo, hdRepo, txManager, tronClient, os.Getenv("TRON_PRO_API_KEY"))
	go tronScanner.Start(ctx)

	for chain, rpcURL := range evmChains {
		if chain == "EVM" || strings.TrimSpace(rpcURL) == "" {
			continue
		}
		scanner, scannerErr := service.NewEVMScanner(chain, rpcURL, depositRepo, hdRepo, txManager)
		if scannerErr != nil {
			log.Printf("%s scanner disabled: %v", chain, scannerErr)
			continue
		}
		go scanner.Start(ctx)
	}

	mempoolClient := mempool.NewClient(false)
	btcClient := startBitcoinScanner(ctx, depositRepo, hdRepo, txManager, mempoolClient)
	tronEnergyProvider := configureTronEnergyProvider()

	sweeperService := service.NewSweeperService(
		evmClients,
		btcClient,
		mempoolClient,
		solanaService,
		tonService,
		tronClient,
		txManager,
		sweepRepo,
		gasRepo,
		hdRepo,
		depositRepo,
		hdWallet,
		settingsRepo,
		tronEnergyProvider,
	)
	sweeperService.PoolRepository = poolRepo
	txManager.SetSweepEnsurer(sweeperService)
	go sweeperService.StartPoller(ctx)

	go runStartupMaintenance(ctx, sweeperService, opsService)
	go runEvery(ctx, 5*time.Minute, func(runCtx context.Context) {
		webhookService.ProcessRetries(runCtx)
	})
	go runEvery(ctx, time.Minute, func(runCtx context.Context) {
		if err := txManager.ExpirePendingTransactions(runCtx); err != nil {
			log.Printf("failed to expire pending transactions: %v", err)
		}
	})

	log.Println("crypto-core worker started")
	<-ctx.Done()
	log.Println("crypto-core worker stopping")

	for chain, rpcClient := range evmClients {
		log.Printf("closing %s RPC client", chain)
		rpcClient.Close()
	}
}

func loadEnvironment() {
	for _, path := range []string{".env", "../.env", "/app/.env"} {
		if err := godotenv.Load(path); err == nil {
			log.Printf("loaded environment from %s", path)
			return
		}
	}
	log.Println("no .env file found; using process environment")
}

func configureEVMClients(chains map[string]string) map[string]*ethclient.Client {
	clients := make(map[string]*ethclient.Client)
	for chain, rpcURL := range chains {
		if strings.TrimSpace(rpcURL) == "" {
			continue
		}
		client, err := ethclient.Dial(rpcURL)
		if err != nil {
			log.Printf("failed to connect to %s RPC: %v", chain, err)
			continue
		}
		clients[chain] = client
		log.Printf("connected to %s RPC", chain)
	}
	return clients
}

func configureTronClients() *client.GrpcClient {
	providerSpecs := parseTronProviderSpecs(os.Getenv("TRON_GRPC_PROVIDERS"))
	if len(providerSpecs) == 0 {
		target := strings.TrimSpace(os.Getenv("TRON_GRPC_URL"))
		if target == "" {
			target = "grpc.trongrid.io:50051"
		}
		providerSpecs = []tronProviderSpec{{
			Name:   "trongrid",
			Target: target,
			APIKey: strings.TrimSpace(os.Getenv("TRON_PRO_API_KEY")),
		}}
	}

	var primary *client.GrpcClient
	grpcProviders := make([]service.TronGRPCProvider, 0, len(providerSpecs))
	for _, spec := range providerSpecs {
		grpcClient := client.NewGrpcClient(spec.Target)
		if err := grpcClient.Start(grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
			log.Printf("failed to start TRON gRPC provider %s: %v", spec.Name, err)
			continue
		}
		if spec.APIKey != "" {
			_ = grpcClient.SetAPIKey(spec.APIKey)
		}
		if primary == nil {
			primary = grpcClient
		}
		grpcProviders = append(grpcProviders, service.TronGRPCProvider{Name: spec.Name, Client: grpcClient})
	}
	service.ConfigureTronGRPCProviders(grpcProviders)

	httpSpecs := parseTronProviderSpecs(os.Getenv("TRON_HTTP_PROVIDERS"))
	if len(httpSpecs) == 0 {
		httpSpecs = []tronProviderSpec{{
			Name:   "trongrid",
			Target: "https://api.trongrid.io",
			APIKey: strings.TrimSpace(os.Getenv("TRON_PRO_API_KEY")),
		}}
	}
	httpProviders := make([]service.TronHTTPProvider, 0, len(httpSpecs))
	for _, spec := range httpSpecs {
		if strings.TrimSpace(spec.Target) == "" {
			continue
		}
		httpProviders = append(httpProviders, service.TronHTTPProvider{
			Name:    spec.Name,
			BaseURL: spec.Target,
			APIKey:  spec.APIKey,
		})
	}
	service.ConfigureTronHTTPProviders(httpProviders)
	log.Printf("TRON providers configured: gRPC=%d HTTP=%d", len(grpcProviders), len(httpProviders))
	return primary
}

func configureTronEnergyProvider() service.TronEnergyProvider {
	client := &http.Client{Timeout: 15 * time.Second}
	trxForEnergy := service.NewTRXForEnergyProvider(
		os.Getenv("TRON_ENERGY_API_KEY"),
		os.Getenv("TRON_ENERGY_API_BASE_URL"),
		1,
		client,
	)
	netts := service.NewNettsProvider(
		os.Getenv("NETTS_API_KEY"),
		os.Getenv("NETTS_API_BASE_URL"),
		os.Getenv("NETTS_REAL_IP"),
		client,
	)
	tronRental := service.NewTronRentalProvider(
		os.Getenv("TRONRENTAL_API_KEY"),
		os.Getenv("TRONRENTAL_API_BASE_URL"),
		client,
	)

	var fallback *service.FallbackTronEnergyProvider
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRON_ENERGY_PROVIDER"))) {
	case "netts":
		fallback = service.NewFallbackTronEnergyProvider(
			service.NamedTronEnergyProvider("netts", netts),
			service.NamedTronEnergyProvider("trx_for_energy", trxForEnergy),
			service.NamedTronEnergyProvider("tronrental", tronRental),
		)
	case "tronrental":
		fallback = service.NewFallbackTronEnergyProvider(
			service.NamedTronEnergyProvider("tronrental", tronRental),
			service.NamedTronEnergyProvider("trx_for_energy", trxForEnergy),
			service.NamedTronEnergyProvider("netts", netts),
		)
	default:
		fallback = service.NewFallbackTronEnergyProvider(
			service.NamedTronEnergyProvider("trx_for_energy", trxForEnergy),
			service.NamedTronEnergyProvider("netts", netts),
			service.NamedTronEnergyProvider("tronrental", tronRental),
		)
	}
	if !fallback.IsConfigured() {
		log.Println("warning: no TRON energy provider is configured; TRC20 sweeps will wait for a provider")
	}
	return fallback
}

func startBitcoinScanner(ctx context.Context, depositRepo *repository.DepositRepository, hdRepo *repository.HdWalletRepository, txManager *service.TransactionManager, mempoolClient *mempool.Client) *rpcclient.Client {
	btcHost := strings.TrimSpace(os.Getenv("BTC_HOST"))
	if btcHost != "" {
		scanner, err := service.NewBTCScanner(
			btcHost,
			os.Getenv("BTC_USER"),
			os.Getenv("BTC_PASS"),
			depositRepo,
			hdRepo,
			txManager,
			false,
		)
		if err == nil {
			go scanner.Start(ctx)
			log.Println("BTC scanner started in node mode")
			return scanner.Client
		}
		log.Printf("BTC node unavailable; falling back to API mode: %v", err)
	}

	scanner, err := service.NewBTCScanner("", "", "", depositRepo, hdRepo, txManager, true)
	if err != nil {
		log.Printf("BTC scanner disabled: %v", err)
		return nil
	}
	scanner.MempoolClient = mempoolClient
	go scanner.Start(ctx)
	log.Println("BTC scanner started in API mode")
	return nil
}

func runStartupMaintenance(ctx context.Context, sweeper *service.SweeperService, ops *service.OperationsService) {
	if result, err := sweeper.RescanUnsweptFunds(ctx); err != nil {
		log.Printf("initial unswept-funds rescan failed: %v", err)
	} else {
		log.Printf("initial unswept-funds rescan complete: matched=%d queued=%d", result.MatchedDeposits, result.QueuedSweeps)
	}

	if created, err := sweeper.BackfillGasResidueSweeps(ctx); err != nil {
		log.Printf("initial gas-residue backfill failed: %v", err)
	} else {
		log.Printf("initial gas-residue backfill complete: created=%d", created)
	}

	if err := sweeper.RefreshGasWalletBalances(ctx); err != nil {
		log.Printf("initial gas-wallet refresh failed: %v", err)
	}
	if err := ops.AuditWalletConfiguration(ctx); err != nil {
		log.Printf("initial wallet audit failed: %v", err)
	}

	go runEvery(ctx, time.Hour, func(runCtx context.Context) {
		if err := sweeper.RefreshGasWalletBalances(runCtx); err != nil {
			log.Printf("hourly gas-wallet refresh failed: %v", err)
		}
	})
	go runEvery(ctx, time.Hour, func(runCtx context.Context) {
		sweeper.RecoverStaleBroadcastingSweeps(runCtx, 2*time.Hour)
	})
	go runEvery(ctx, time.Hour, func(runCtx context.Context) {
		if err := ops.AuditWalletConfiguration(runCtx); err != nil {
			log.Printf("hourly wallet audit failed: %v", err)
		}
	})
}

func runEvery(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}

func parseTronProviderSpecs(raw string) []tronProviderSpec {
	parts := strings.Split(raw, ",")
	providers := make([]tronProviderSpec, 0, len(parts))
	for index, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), "|")
		if fields[0] == "" {
			continue
		}
		spec := tronProviderSpec{Name: fmt.Sprintf("tron-%d", index+1), Target: strings.TrimSpace(fields[0])}
		if len(fields) >= 2 {
			spec.Name = strings.TrimSpace(fields[0])
			spec.Target = strings.TrimSpace(fields[1])
		}
		if len(fields) >= 3 {
			spec.APIKey = strings.TrimSpace(fields[2])
		}
		providers = append(providers, spec)
	}
	return providers
}
