package repository

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"errors"

	"github.com/jackc/pgx/v5"
)

type ScanningRepository struct{}

func NewScanningRepository() *ScanningRepository {
	return &ScanningRepository{}
}

func (r *ScanningRepository) GetLastScannedBlock(ctx context.Context, chain string) (int64, error) {
	var height int64
	query := `SELECT last_scanned_block FROM scanning_state WHERE chain_type = $1`
	err := database.DB.QueryRow(ctx, query, chain).Scan(&height)
	if err != nil {
		return 0, err
	}
	return height, nil
}

func (r *ScanningRepository) GetLastScannedBlockOrClone(ctx context.Context, chain string) (int64, bool, error) {
	height, err := r.GetLastScannedBlock(ctx, chain)
	if err == nil {
		return height, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}

	cloneSource := legacyScanningStateSource(chain)
	if cloneSource == "" {
		return 0, false, err
	}

	tag, cloneErr := database.DB.Exec(ctx, `
		INSERT INTO scanning_state (chain_type, last_scanned_block, updated_at)
		SELECT $1, last_scanned_block, NOW()
		FROM scanning_state
		WHERE chain_type = $2
		ON CONFLICT (chain_type) DO NOTHING
	`, chain, cloneSource)
	if cloneErr != nil {
		return 0, false, cloneErr
	}

	height, err = r.GetLastScannedBlock(ctx, chain)
	if err != nil {
		return 0, false, err
	}

	return height, tag.RowsAffected() > 0, nil
}

func (r *ScanningRepository) UpdateLastScannedBlock(ctx context.Context, chain string, height int64) error {
	query := `
		INSERT INTO scanning_state (chain_type, last_scanned_block, updated_at)
		VALUES ($2, $1, NOW())
		ON CONFLICT (chain_type) DO UPDATE
		SET last_scanned_block = EXCLUDED.last_scanned_block,
		    updated_at = NOW()
	`
	_, err := database.DB.Exec(ctx, query, height, chain)
	return err
}

func legacyScanningStateSource(chain string) string {
	switch chain {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM":
		return "EVM"
	default:
		return ""
	}
}
