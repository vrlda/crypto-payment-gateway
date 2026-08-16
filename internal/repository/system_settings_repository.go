package repository

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

type SystemSettingsRepository struct{}

func NewSystemSettingsRepository() *SystemSettingsRepository {
	return &SystemSettingsRepository{}
}

func (r *SystemSettingsRepository) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := database.DB.QueryRow(ctx, "SELECT value FROM system_settings WHERE key = $1", key).Scan(&value)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

func (r *SystemSettingsRepository) Set(ctx context.Context, key, value string) error {
	_, err := database.DB.Exec(ctx, `
		INSERT INTO system_settings (key, value, updated_at) 
		VALUES ($1, $2, NOW()) 
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, key, value)
	return err
}
