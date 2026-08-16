package repository

import (
	"context"
	"errors"

	"crypto_payment_gateway_core/internal/database"

	"github.com/jackc/pgx/v5"
)

// Merchant contains only the payment-routing fields required by the crypto
// worker. Authentication, dashboard, payout, and account-management fields
// intentionally do not belong in this repository.
type Merchant struct {
	ID                     string
	AcceptAnyPaymentAmount bool
}

type MerchantRepository struct{}

func NewMerchantRepository() *MerchantRepository {
	return &MerchantRepository{}
}

func (r *MerchantRepository) FindByID(ctx context.Context, id string) (*Merchant, error) {
	const query = `
		SELECT id, COALESCE(accept_any_payment_amount, FALSE)
		FROM merchants
		WHERE id = $1`

	var merchant Merchant
	err := database.DB.QueryRow(ctx, query, id).Scan(
		&merchant.ID,
		&merchant.AcceptAnyPaymentAmount,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &merchant, nil
}

func (r *MerchantRepository) GetWebhookConfig(ctx context.Context, merchantID string) (string, string, error) {
	var url, secretEnc string
	err := database.DB.QueryRow(ctx, `
		SELECT COALESCE(webhook_url, ''), COALESCE(webhook_secret_enc, '')
		FROM merchants
		WHERE id = $1`, merchantID).Scan(&url, &secretEnc)
	if err != nil {
		return "", "", err
	}
	return url, secretEnc, nil
}
