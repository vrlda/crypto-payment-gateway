package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"crypto_payment_gateway_core/internal/repository"

	"github.com/redis/go-redis/v9"
)

const (
	opsCounterTotalKey    = "ops:counters:total"
	opsCounterDailyPrefix = "ops:counters:daily:"
	opsCounterRetention   = 45 * 24 * time.Hour
	opsAlertPrefix        = "ops:alert:"
)

// OperationsService contains crypto-worker observability and wallet audits.
// It intentionally has no dashboard, admin, Telegram, or notification UI
// dependencies. Alerts are deduplicated in Redis and emitted to the worker log.
type OperationsService struct {
	rdb    *redis.Client
	hdRepo *repository.HdWalletRepository
}

func NewOperationsService(hdRepo *repository.HdWalletRepository) *OperationsService {
	return &OperationsService{rdb: database.Rdb, hdRepo: hdRepo}
}

func (s *OperationsService) IncrementCounter(ctx context.Context, name string, labels map[string]string) {
	if s == nil || name == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if strings.EqualFold(strings.TrimSpace(os.Getenv("OPS_LOG_COUNTERS")), "true") {
		log.Printf("ops_counter name=%s labels=%s", name, formatOpsLabels(labels))
	}
	if s.rdb == nil {
		return
	}

	if _, err := s.rdb.HIncrBy(ctx, opsCounterTotalKey, name, 1).Result(); err != nil {
		log.Printf("failed to increment total counter %s: %v", name, err)
	}

	dailyKey := opsCounterDailyPrefix + time.Now().UTC().Format("2006-01-02")
	if _, err := s.rdb.HIncrBy(ctx, dailyKey, name, 1).Result(); err != nil {
		log.Printf("failed to increment daily counter %s: %v", dailyKey, err)
		return
	}
	if _, err := s.rdb.Expire(ctx, dailyKey, opsCounterRetention).Result(); err != nil {
		log.Printf("failed to set counter retention for %s: %v", dailyKey, err)
	}
}

func (s *OperationsService) EmitAlert(ctx context.Context, dedupeKey, title, message, notificationType string, ttl time.Duration) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if dedupeKey != "" {
		allowed, err := s.shouldSendAlert(ctx, dedupeKey, ttl)
		if err != nil {
			log.Printf("failed to deduplicate crypto alert %s: %v", dedupeKey, err)
		} else if !allowed {
			return nil
		}
	}
	log.Printf("crypto_alert type=%s title=%q message=%q", notificationType, title, message)
	return nil
}

func (s *OperationsService) AuditWalletConfiguration(ctx context.Context) error {
	if s == nil || s.hdRepo == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	wallets, err := s.hdRepo.ListAll(ctx)
	if err != nil {
		return err
	}

	byRoot := make(map[string][]model.HDWallet)
	for _, wallet := range wallets {
		if strings.TrimSpace(wallet.MasterPublicKey) == "" {
			continue
		}
		byRoot[wallet.MasterPublicKey] = append(byRoot[wallet.MasterPublicKey], wallet)
	}

	for root, group := range byRoot {
		if len(group) < 2 || !walletRootReuseShouldAlert(group) {
			continue
		}

		families := walletRootReuseFamilies(group)
		title := "Cross-family HD root reuse detected"
		if len(families) == 1 && walletGroupHasDuplicateSelectors(group) {
			title = "Duplicate wallet rows share the same HD root"
		}
		_ = s.EmitAlert(ctx, "duplicate-root:"+shortHash(root), title, describeWalletGroup(group), "warning", 24*time.Hour)
	}

	for _, wallet := range wallets {
		if issue := walletSweepabilityIssue(&wallet); issue != "" {
			_ = s.EmitAlert(ctx, "unsweepable-wallet:"+wallet.ID, "Enabled wallet is not sweepable", fmt.Sprintf("%s/%s: %s", wallet.Coin, wallet.Network, issue), "error", 12*time.Hour)
		}
	}
	return nil
}

func (s *OperationsService) shouldSendAlert(ctx context.Context, dedupeKey string, ttl time.Duration) (bool, error) {
	if s.rdb == nil {
		return true, nil
	}
	return s.rdb.SetNX(ctx, opsAlertPrefix+shortHash(dedupeKey), time.Now().UTC().Format(time.RFC3339), ttl).Result()
}

func walletNeedsContractAddress(coin, network string) bool {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "BTC":
		return false
	case "TRC20":
		return strings.ToUpper(strings.TrimSpace(coin)) != "TRX"
	case "SOLANA":
		return strings.ToUpper(strings.TrimSpace(coin)) != "SOL"
	case "TON":
		return strings.ToUpper(strings.TrimSpace(coin)) != "TON"
	case "ERC20":
		return strings.ToUpper(strings.TrimSpace(coin)) != "ETH"
	case "BEP20":
		return strings.ToUpper(strings.TrimSpace(coin)) != "BNB"
	case "POLYGON":
		return strings.ToUpper(strings.TrimSpace(coin)) != "MATIC"
	case "ARBITRUM":
		return strings.ToUpper(strings.TrimSpace(coin)) != "ETH"
	default:
		return false
	}
}

func walletRequiresSpendKey(network string) bool {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "BTC", "TRC20", "SOLANA", "TON", "ERC20", "BEP20", "POLYGON", "ARBITRUM":
		return true
	default:
		return false
	}
}

func walletSweepabilityIssue(wallet *model.HDWallet) string {
	if wallet == nil || !wallet.IsEnabled {
		return ""
	}
	if strings.TrimSpace(wallet.MasterPublicKey) == "" {
		return "master public key is missing"
	}
	if strings.TrimSpace(wallet.HotWalletAddress) == "" {
		return "hot wallet address is missing"
	}
	if walletNeedsContractAddress(wallet.Coin, wallet.Network) && (wallet.ContractAddress == nil || strings.TrimSpace(*wallet.ContractAddress) == "") {
		return "contract address is missing"
	}
	if walletRequiresSpendKey(wallet.Network) && (wallet.EncryptedMasterSeed == nil || strings.TrimSpace(*wallet.EncryptedMasterSeed) == "") {
		return "encrypted spend key is missing"
	}
	return ""
}

func walletRootReuseFamilies(wallets []model.HDWallet) []string {
	familySet := make(map[string]struct{})
	for _, wallet := range wallets {
		familySet[walletAddressSpaceFamily(wallet.Network)] = struct{}{}
	}

	families := make([]string, 0, len(familySet))
	for family := range familySet {
		families = append(families, family)
	}
	sort.Strings(families)
	return families
}

func walletRootReuseShouldAlert(wallets []model.HDWallet) bool {
	return len(walletRootReuseFamilies(wallets)) > 1 || walletGroupHasDuplicateSelectors(wallets)
}

func walletGroupHasDuplicateSelectors(wallets []model.HDWallet) bool {
	selectors := make(map[string]struct{}, len(wallets))
	for _, wallet := range wallets {
		key := walletSelectorKey(wallet)
		if _, exists := selectors[key]; exists {
			return true
		}
		selectors[key] = struct{}{}
	}
	return false
}

func walletSelectorKey(wallet model.HDWallet) string {
	return strings.ToUpper(strings.TrimSpace(wallet.Coin)) + ":" + normalizeWalletAuditNetwork(wallet.Network)
}

func walletAddressSpaceFamily(network string) string {
	switch normalizeWalletAuditNetwork(network) {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM":
		return "EVM"
	case "TRC20":
		return "TRON"
	case "BTC":
		return "BTC"
	case "SOLANA":
		return "SOLANA"
	case "TON":
		return "TON"
	default:
		return normalizeWalletAuditNetwork(network)
	}
}

func normalizeWalletAuditNetwork(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "BTC", "BITCOIN":
		return "BTC"
	case "ETHEREUM", "ERC20":
		return "ERC20"
	case "BSC", "BEP20":
		return "BEP20"
	case "POLYGON", "POL":
		return "POLYGON"
	case "ARBITRUM", "ARB":
		return "ARBITRUM"
	case "TRON", "TRC20":
		return "TRC20"
	case "SOLANA":
		return "SOLANA"
	case "TON":
		return "TON"
	default:
		return strings.ToUpper(strings.TrimSpace(network))
	}
}

func describeWalletGroup(wallets []model.HDWallet) string {
	parts := make([]string, 0, len(wallets))
	for _, wallet := range wallets {
		parts = append(parts, fmt.Sprintf("%s/%s (%s)", wallet.Coin, wallet.Network, shortID(wallet.ID)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func formatOpsLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, labels[key]))
	}
	return strings.Join(parts, ",")
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
