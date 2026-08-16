package service

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func normalizeWatchedAddress(network, address string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "ETHEREUM", "BSC", "POL", "ARB":
		return strings.ToLower(strings.TrimSpace(address))
	default:
		return strings.TrimSpace(address)
	}
}

func monitoredCacheKeys(network string) (string, string) {
	normalizedNetwork := strings.TrimSpace(network)
	return fmt.Sprintf("monitored_addresses:%s", normalizedNetwork), fmt.Sprintf("monitored_mapping:%s", normalizedNetwork)
}

func operationalPrefundCacheKey(network, address, txHash string) string {
	return fmt.Sprintf(
		"operational_prefund:%s:%s:%s",
		normalizeDepositTxWatchNetwork(network),
		normalizeWatchedAddress(network, address),
		normalizeDepositTxWatchHash(network, txHash),
	)
}

func rememberOperationalPrefund(ctx context.Context, network, address, txHash string) {
	if database.Rdb == nil || strings.TrimSpace(address) == "" || strings.TrimSpace(txHash) == "" {
		return
	}
	_ = database.Rdb.Set(ctx, operationalPrefundCacheKey(network, address, txHash), "1", 48*time.Hour).Err()
}

func isOperationalPrefundTx(ctx context.Context, txManager *TransactionManager, network, address, txHash string) bool {
	if database.Rdb == nil || strings.TrimSpace(address) == "" || strings.TrimSpace(txHash) == "" {
		return false
	}

	exists, err := database.Rdb.Exists(ctx, operationalPrefundCacheKey(network, address, txHash)).Result()
	if err != nil || exists == 0 {
		return false
	}

	if txManager != nil {
		txManager.RecordCounter(ctx, "scanner_operational_prefund_ignored", map[string]string{
			"network": normalizeDepositTxWatchNetwork(network),
		})
	}

	return true
}

func cacheWatchedDeposit(ctx context.Context, network string, dep *model.CryptoDeposit) {
	if dep == nil || database.Rdb == nil {
		return
	}

	setKey, mappingKey := monitoredCacheKeys(network)
	addresses := []string{dep.PaymentAddress}
	if dep.DepositAddress != "" && dep.DepositAddress != dep.PaymentAddress {
		addresses = append(addresses, dep.DepositAddress)
	}

	pipe := database.Rdb.TxPipeline()
	for _, address := range addresses {
		normalized := normalizeWatchedAddress(network, address)
		if normalized == "" {
			continue
		}
		pipe.SAdd(ctx, setKey, normalized)
		pipe.HSet(ctx, mappingKey, normalized, dep.ID)
	}
	_, _ = pipe.Exec(ctx)
}

func removeWatchedDepositAddress(ctx context.Context, network, address string) {
	if database.Rdb == nil {
		return
	}

	setKey, mappingKey := monitoredCacheKeys(network)
	normalized := normalizeWatchedAddress(network, address)
	if normalized == "" {
		return
	}

	pipe := database.Rdb.TxPipeline()
	pipe.SRem(ctx, setKey, normalized)
	pipe.HDel(ctx, mappingKey, normalized)
	_, _ = pipe.Exec(ctx)
}

func rebuildWatchedAddressCache(ctx context.Context, depRepo *repository.DepositRepository, txManager *TransactionManager, network string) error {
	if depRepo == nil || database.Rdb == nil {
		return nil
	}

	deposits, err := depRepo.FindWatchedByNetwork(ctx, network)
	if err != nil {
		return err
	}

	setKey, mappingKey := monitoredCacheKeys(network)
	pipe := database.Rdb.TxPipeline()
	pipe.Del(ctx, setKey, mappingKey)
	for _, dep := range deposits {
		if dep == nil {
			continue
		}
		addresses := []string{dep.PaymentAddress}
		if dep.DepositAddress != "" && dep.DepositAddress != dep.PaymentAddress {
			addresses = append(addresses, dep.DepositAddress)
		}
		for _, address := range addresses {
			normalized := normalizeWatchedAddress(network, address)
			if normalized == "" {
				continue
			}
			pipe.SAdd(ctx, setKey, normalized)
			pipe.HSet(ctx, mappingKey, normalized, dep.ID)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	if txManager != nil {
		txManager.RecordCounter(ctx, "scanner_cache_empty_rebuild", map[string]string{
			"network": network,
		})
	}

	return nil
}

func resolveWatchedDepositByAddress(ctx context.Context, depRepo *repository.DepositRepository, txManager *TransactionManager, network, address string) (*model.CryptoDeposit, error) {
	if depRepo == nil {
		return nil, nil
	}

	normalized := normalizeWatchedAddress(network, address)
	if normalized == "" {
		return nil, nil
	}

	if database.Rdb != nil {
		_, mappingKey := monitoredCacheKeys(network)
		depositID, err := database.Rdb.HGet(ctx, mappingKey, normalized).Result()
		if err == nil && depositID != "" {
			dep, depErr := depRepo.FindByID(ctx, depositID)
			if depErr == nil && dep != nil && dep.WatchStatus == model.DepositWatchStatusActive {
				return dep, nil
			}
			if dep == nil || dep.WatchStatus != model.DepositWatchStatusActive {
				removeWatchedDepositAddress(ctx, network, address)
			}
		} else if err != nil && !errors.Is(err, redis.Nil) {
			// Fall back to DB if Redis is unavailable or transiently failing.
		}
	}

	dep, err := depRepo.FindWatchedByNetworkAndAddress(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if dep == nil {
		if txManager != nil {
			txManager.RecordCounter(ctx, "scanner_cache_miss_not_found", map[string]string{
				"network": network,
			})
		}
		return nil, nil
	}

	cacheWatchedDeposit(ctx, network, dep)
	if txManager != nil {
		txManager.RecordCounter(ctx, "scanner_cache_db_fallback", map[string]string{
			"network": network,
		})
	}
	return dep, nil
}
