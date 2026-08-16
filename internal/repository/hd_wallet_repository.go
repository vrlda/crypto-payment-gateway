package repository

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const hdWalletSelectFields = `id, coin, network, master_public_key, encrypted_master_seed,
	hot_wallet_address, derivation_account, current_derivation_index,
	required_confirmations, finalization_confirmations, amount_tolerance_percent, decimals,
	is_enabled, contract_address, display_name, icon_url, created_at, updated_at`

type HdWalletRepository struct{}

func NewHdWalletRepository() *HdWalletRepository {
	return &HdWalletRepository{}
}

func normalizeHDWalletNetwork(network string) string {
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

func scanHDWallet(scan func(dest ...any) error, w *model.HDWallet) error {
	if err := scan(
		&w.ID, &w.Coin, &w.Network, &w.MasterPublicKey, &w.EncryptedMasterSeed,
		&w.HotWalletAddress, &w.DerivationAccount, &w.CurrentDerivationIndex,
		&w.RequiredConfirmations, &w.FinalizationConfirmations, &w.AmountTolerancePercent, &w.Decimals,
		&w.IsEnabled, &w.ContractAddress, &w.DisplayName, &w.IconURL, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return err
	}

	w.Coin = strings.ToUpper(strings.TrimSpace(w.Coin))
	w.Network = normalizeHDWalletNetwork(w.Network)
	return nil
}

func addressSpaceKeyForWallet(wallet *model.HDWallet) (string, error) {
	if wallet == nil {
		return "", errors.New("wallet is required")
	}
	if wallet.MasterPublicKey == "" {
		return "", errors.New("wallet master public key is required")
	}
	return wallet.MasterPublicKey, nil
}

func (r *HdWalletRepository) FindByID(ctx context.Context, id string) (*model.HDWallet, error) {
	query := `SELECT ` + hdWalletSelectFields + ` FROM hd_wallets WHERE id=$1`

	var w model.HDWallet
	err := scanHDWallet(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, id).Scan(dest...)
	}, &w)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (r *HdWalletRepository) FindByCoinAndNetwork(ctx context.Context, coin, network string) (*model.HDWallet, error) {
	query := `SELECT ` + hdWalletSelectFields + ` FROM hd_wallets WHERE UPPER(coin)=UPPER($1) AND UPPER(network)=UPPER($2) AND is_enabled=true LIMIT 1`

	var w model.HDWallet
	err := scanHDWallet(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, strings.ToUpper(strings.TrimSpace(coin)), normalizeHDWalletNetwork(network)).Scan(dest...)
	}, &w)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (r *HdWalletRepository) FindByCoinAndNetworkAnyStatus(ctx context.Context, coin, network string) (*model.HDWallet, error) {
	query := `SELECT ` + hdWalletSelectFields + ` FROM hd_wallets WHERE UPPER(coin)=UPPER($1) AND UPPER(network)=UPPER($2) LIMIT 1`

	var w model.HDWallet
	err := scanHDWallet(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, strings.ToUpper(strings.TrimSpace(coin)), normalizeHDWalletNetwork(network)).Scan(dest...)
	}, &w)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (r *HdWalletRepository) GetNextAddressIndex(ctx context.Context, wallet *model.HDWallet) (uint32, error) {
	addressSpaceKey, err := addressSpaceKeyForWallet(wallet)
	if err != nil {
		return 0, err
	}

	tx, err := database.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO hd_derivation_counters (address_space_key, current_index, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (address_space_key) DO NOTHING
	`, addressSpaceKey, wallet.CurrentDerivationIndex)
	if err != nil {
		return 0, err
	}

	var nextIndex int
	err = tx.QueryRow(ctx, `
		UPDATE hd_derivation_counters
		SET current_index = current_index + 1, updated_at = NOW()
		WHERE address_space_key = $1
		RETURNING current_index
	`, addressSpaceKey).Scan(&nextIndex)
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE hd_wallets
		SET current_derivation_index = GREATEST(current_derivation_index, $1), updated_at = NOW()
		WHERE id = $2
	`, nextIndex, wallet.ID); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	return uint32(nextIndex), nil
}

func (r *HdWalletRepository) GetNextIndex(ctx context.Context, id string) (uint32, error) {
	wallet, err := r.FindByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if wallet == nil {
		return 0, errors.New("wallet not found")
	}
	return r.GetNextAddressIndex(ctx, wallet)
}

func (r *HdWalletRepository) ListAll(ctx context.Context) ([]model.HDWallet, error) {
	query := `SELECT ` + hdWalletSelectFields + ` FROM hd_wallets ORDER BY coin ASC, network ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var wallets []model.HDWallet
	for rows.Next() {
		var w model.HDWallet
		if err := scanHDWallet(rows.Scan, &w); err != nil {
			return nil, err
		}
		wallets = append(wallets, w)
	}
	return wallets, nil
}

func (r *HdWalletRepository) ListEnabled(ctx context.Context) ([]model.HDWallet, error) {
	query := `SELECT ` + hdWalletSelectFields + ` FROM hd_wallets WHERE is_enabled=true ORDER BY coin ASC, network ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var wallets []model.HDWallet
	for rows.Next() {
		var w model.HDWallet
		if err := scanHDWallet(rows.Scan, &w); err != nil {
			return nil, err
		}
		wallets = append(wallets, w)
	}
	return wallets, nil
}

func (r *HdWalletRepository) UpdateMasterKey(ctx context.Context, id, xpub, xprivEnc string) error {
	_, err := database.DB.Exec(ctx, `
		UPDATE hd_wallets
		SET master_public_key = $1, encrypted_master_seed = $2, updated_at = NOW()
		WHERE id = $3
	`, xpub, xprivEnc, id)
	return err
}

func (r *HdWalletRepository) UpsertConfig(ctx context.Context, w *model.HDWallet) (string, error) {
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	w.Coin = strings.ToUpper(strings.TrimSpace(w.Coin))
	w.Network = normalizeHDWalletNetwork(w.Network)

	query := `INSERT INTO hd_wallets
		(id, coin, network, master_public_key, encrypted_master_seed, hot_wallet_address, derivation_account,
		 required_confirmations, finalization_confirmations, amount_tolerance_percent, decimals, is_enabled,
		 contract_address, display_name, icon_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (coin, network) DO UPDATE SET
			master_public_key = CASE WHEN EXCLUDED.master_public_key != '' THEN EXCLUDED.master_public_key ELSE hd_wallets.master_public_key END,
			encrypted_master_seed = COALESCE(EXCLUDED.encrypted_master_seed, hd_wallets.encrypted_master_seed),
			hot_wallet_address = CASE WHEN EXCLUDED.hot_wallet_address != '' THEN EXCLUDED.hot_wallet_address ELSE hd_wallets.hot_wallet_address END,
			derivation_account = CASE
				WHEN EXCLUDED.derivation_account > 0 THEN EXCLUDED.derivation_account
				ELSE hd_wallets.derivation_account
			END,
			required_confirmations = EXCLUDED.required_confirmations,
			finalization_confirmations = EXCLUDED.finalization_confirmations,
			amount_tolerance_percent = CASE
				WHEN EXCLUDED.amount_tolerance_percent > 0 THEN EXCLUDED.amount_tolerance_percent
				ELSE hd_wallets.amount_tolerance_percent
			END,
			decimals = EXCLUDED.decimals,
			is_enabled = EXCLUDED.is_enabled,
			contract_address = COALESCE(EXCLUDED.contract_address, hd_wallets.contract_address),
			display_name = COALESCE(EXCLUDED.display_name, hd_wallets.display_name),
			icon_url = COALESCE(EXCLUDED.icon_url, hd_wallets.icon_url),
			updated_at = NOW()
		RETURNING id`

	var id string
	err := database.DB.QueryRow(ctx, query,
		w.ID, w.Coin, w.Network, w.MasterPublicKey, w.EncryptedMasterSeed, w.HotWalletAddress, w.DerivationAccount,
		w.RequiredConfirmations, w.FinalizationConfirmations, w.AmountTolerancePercent, w.Decimals, w.IsEnabled,
		w.ContractAddress, w.DisplayName, w.IconURL,
	).Scan(&id)
	return id, err
}

func (r *HdWalletRepository) UpdateByID(ctx context.Context, w *model.HDWallet) error {
	if w == nil || w.ID == "" {
		return errors.New("wallet id is required")
	}

	tag, err := database.DB.Exec(ctx, `
		UPDATE hd_wallets
		SET master_public_key = CASE WHEN $1 != '' THEN $1 ELSE master_public_key END,
			encrypted_master_seed = COALESCE($2, encrypted_master_seed),
			hot_wallet_address = $3,
			required_confirmations = $4,
			finalization_confirmations = $5,
			amount_tolerance_percent = $6,
			decimals = $7,
			is_enabled = $8,
			contract_address = $9,
			display_name = $10,
			icon_url = $11,
			updated_at = NOW()
		WHERE id = $12
	`, w.MasterPublicKey, w.EncryptedMasterSeed, w.HotWalletAddress, w.RequiredConfirmations,
		w.FinalizationConfirmations, w.AmountTolerancePercent, w.Decimals, w.IsEnabled, w.ContractAddress,
		w.DisplayName, w.IconURL, w.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("wallet %s not found", w.ID)
	}
	return nil
}
