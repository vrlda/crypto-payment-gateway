package repository

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

type GasWalletRepository struct{}

func NewGasWalletRepository() *GasWalletRepository {
	return &GasWalletRepository{}
}

func NormalizeGasWalletChainType(chainType string) string {
	switch strings.ToUpper(strings.TrimSpace(chainType)) {
	case "BTC", "BITCOIN":
		return "BTC"
	case "TRON", "TRC20":
		return "TRON"
	case "SOLANA":
		return "SOLANA"
	case "TON":
		return "TON"
	case "EVM", "ERC20", "BEP20", "POLYGON", "ARBITRUM", "ETHEREUM", "BSC":
		return "EVM"
	default:
		return strings.TrimSpace(chainType)
	}
}

func gasWalletChainTypeAliases(chainType string) []string {
	switch NormalizeGasWalletChainType(chainType) {
	case "BTC":
		return []string{"BTC", "Bitcoin"}
	case "TRON":
		return []string{"TRON", "TRC20", "Tron"}
	case "SOLANA":
		return []string{"SOLANA", "Solana"}
	case "TON":
		return []string{"TON", "Ton"}
	case "EVM":
		return []string{"EVM", "ERC20", "BEP20", "POLYGON", "ARBITRUM", "Ethereum", "BSC", "Polygon", "Arbitrum"}
	default:
		return []string{strings.TrimSpace(chainType)}
	}
}

func (r *GasWalletRepository) GetByChainType(ctx context.Context, chainType string) (*model.GasWallet, error) {
	query := `SELECT id, chain_type, wallet_address, private_key_enc, is_enabled, 
	                 min_balance, max_gas_price, daily_gas_limit, max_gas_per_sweep, 
	                 daily_gas_used, daily_reset_at, current_balance, balance_checked_at,
	                 balance_check_error, is_below_min_balance, last_alert_balance
	          FROM gas_wallets
	          WHERE chain_type = ANY($1)
	          ORDER BY
	              CASE WHEN chain_type = $2 THEN 0 ELSE 1 END,
	              updated_at DESC
	          LIMIT 1`

	canonicalChainType := NormalizeGasWalletChainType(chainType)
	row := database.DB.QueryRow(ctx, query, gasWalletChainTypeAliases(chainType), canonicalChainType)

	var gw model.GasWallet

	err := row.Scan(
		&gw.ID, &gw.ChainType, &gw.WalletAddress, &gw.PrivateKeyEnc, &gw.IsEnabled,
		&gw.MinBalance, &gw.MaxGasPrice, &gw.DailyGasLimit, &gw.MaxGasPerSweep,
		&gw.DailyGasUsed, &gw.DailyResetAt, &gw.CurrentBalance, &gw.BalanceCheckedAt,
		&gw.BalanceCheckError, &gw.IsBelowMinBalance, &gw.LastAlertBalance,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get gas wallet: %w", err)
	}
	gw.ChainType = NormalizeGasWalletChainType(gw.ChainType)
	return &gw, nil
}

func (r *GasWalletRepository) IncrementDailyGasUsed(ctx context.Context, chainType string, amount decimal.Decimal) error {
	gw, err := r.GetByChainType(ctx, chainType)
	if err != nil || gw == nil {
		return err
	}

	query := `UPDATE gas_wallets SET daily_gas_used = daily_gas_used + $1 WHERE id = $2`
	_, err = database.DB.Exec(ctx, query, amount, gw.ID)
	return err
}

func (r *GasWalletRepository) UpdateDailyReset(ctx context.Context, chainType string) error {
	gw, err := r.GetByChainType(ctx, chainType)
	if err != nil || gw == nil {
		return err
	}

	query := `UPDATE gas_wallets SET daily_gas_used = 0, daily_reset_at = NOW() WHERE id = $1`
	_, err = database.DB.Exec(ctx, query, gw.ID)
	return err
}
func (r *GasWalletRepository) UpdateLastAlertBalance(ctx context.Context, id string, balance decimal.Decimal) error {
	query := `UPDATE gas_wallets SET last_alert_balance = $1, updated_at = NOW() WHERE id = $2`
	_, err := database.DB.Exec(ctx, query, balance, id)
	return err
}

func (r *GasWalletRepository) UpdateBalanceSnapshot(ctx context.Context, id string, balance decimal.Decimal, checkedAt time.Time, isBelowMinBalance bool) error {
	query := `UPDATE gas_wallets
	          SET current_balance = $1,
	              balance_checked_at = $2,
	              balance_check_error = NULL,
	              is_below_min_balance = $3,
	              updated_at = NOW()
	          WHERE id = $4`
	_, err := database.DB.Exec(ctx, query, balance, checkedAt, isBelowMinBalance, id)
	return err
}

func (r *GasWalletRepository) UpdateBalanceCheckError(ctx context.Context, id string, checkedAt time.Time, errorMessage string) error {
	query := `UPDATE gas_wallets
	          SET current_balance = NULL,
	              balance_checked_at = $1,
	              balance_check_error = $2,
	              is_below_min_balance = FALSE,
	              updated_at = NOW()
	          WHERE id = $3`
	_, err := database.DB.Exec(ctx, query, checkedAt, errorMessage, id)
	return err
}

func (r *GasWalletRepository) ClearBalanceSnapshot(ctx context.Context, id string) error {
	query := `UPDATE gas_wallets
	          SET current_balance = NULL,
	              balance_checked_at = NULL,
	              balance_check_error = NULL,
	              is_below_min_balance = FALSE,
	              updated_at = NOW()
	          WHERE id = $1`
	_, err := database.DB.Exec(ctx, query, id)
	return err
}

func (r *GasWalletRepository) ListAll(ctx context.Context) ([]model.GasWallet, error) {
	query := `SELECT id, chain_type, wallet_address, private_key_enc, is_enabled, 
	                 min_balance, max_gas_price, daily_gas_limit, max_gas_per_sweep, 
	                 daily_gas_used, daily_reset_at, current_balance, balance_checked_at,
	                 balance_check_error, is_below_min_balance, last_alert_balance,
	                 created_at, updated_at
	          FROM gas_wallets ORDER BY chain_type ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var wallets []model.GasWallet
	for rows.Next() {
		var gw model.GasWallet
		err := rows.Scan(
			&gw.ID, &gw.ChainType, &gw.WalletAddress, &gw.PrivateKeyEnc, &gw.IsEnabled,
			&gw.MinBalance, &gw.MaxGasPrice, &gw.DailyGasLimit, &gw.MaxGasPerSweep,
			&gw.DailyGasUsed, &gw.DailyResetAt, &gw.CurrentBalance, &gw.BalanceCheckedAt,
			&gw.BalanceCheckError, &gw.IsBelowMinBalance, &gw.LastAlertBalance,
			&gw.CreatedAt, &gw.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		wallets = append(wallets, gw)
	}
	return wallets, nil
}

func (r *GasWalletRepository) Create(ctx context.Context, gw *model.GasWallet) error {
	query := `
		INSERT INTO gas_wallets (
			chain_type, wallet_address, private_key_enc, is_enabled, 
			min_balance, max_gas_price, daily_gas_limit, max_gas_per_sweep
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`

	return database.DB.QueryRow(ctx, query,
		gw.ChainType, gw.WalletAddress, gw.PrivateKeyEnc, gw.IsEnabled,
		gw.MinBalance, gw.MaxGasPrice, gw.DailyGasLimit, gw.MaxGasPerSweep,
	).Scan(&gw.ID, &gw.CreatedAt, &gw.UpdatedAt)
}

func (r *GasWalletRepository) Update(ctx context.Context, gw *model.GasWallet) error {
	query := `
		UPDATE gas_wallets SET 
			chain_type = $1,
			wallet_address = $2, 
			is_enabled = $3, 
			min_balance = $4, 
			max_gas_price = $5, 
			daily_gas_limit = $6, 
			max_gas_per_sweep = $7`

	params := []interface{}{
		NormalizeGasWalletChainType(gw.ChainType), gw.WalletAddress, gw.IsEnabled,
		gw.MinBalance, gw.MaxGasPrice, gw.DailyGasLimit, gw.MaxGasPerSweep,
	}

	argID := 8
	if gw.PrivateKeyEnc != "" {
		query += `, private_key_enc = $8`
		params = append(params, gw.PrivateKeyEnc)
		argID = 9
	}

	query += fmt.Sprintf(`, updated_at = NOW() WHERE id = $%d`, argID)
	params = append(params, gw.ID)

	_, err := database.DB.Exec(ctx, query, params...)
	return err
}

func (r *GasWalletRepository) Delete(ctx context.Context, id string) error {
	_, err := database.DB.Exec(ctx, "DELETE FROM gas_wallets WHERE id = $1", id)
	return err
}
