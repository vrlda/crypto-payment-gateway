package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"crypto_payment_gateway_core/internal/database"

	"github.com/jackc/pgx/v5"
)

// PaymentTransaction is the optional merchant-payment record linked to a
// crypto deposit. The worker updates it when an upstream payment service has
// created one; it does not expose payment-management endpoints.
type PaymentTransaction struct {
	ID              string     `db:"id" json:"id"`
	MerchantID      string     `db:"merchant_id" json:"merchant_id"`
	WalletID        *string    `db:"wallet_id" json:"wallet_id"`
	AmountExpected  string     `db:"amount_expected" json:"amount_expected"`
	AmountBase      string     `db:"amount_base" json:"amount_base"`
	FeeClient       string     `db:"fee_client" json:"fee_client"`
	Currency        string     `db:"currency" json:"currency"`
	ChainType       string     `db:"chain_type" json:"chain_type"`
	Status          string     `db:"status" json:"status"`
	TxHash          *string    `db:"tx_hash" json:"tx_hash"`
	ExternalRefID   *string    `db:"external_ref_id" json:"external_ref_id"`
	CustomerEmail   *string    `db:"customer_email" json:"customer_email"`
	CommissionRate  float64    `db:"commission_rate" json:"commission_rate"`
	CommissionSplit int        `db:"commission_split_merchant_percent" json:"commission_split_merchant_percent"`
	ExpiresAt       *time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

type TransactionRepository struct{}

func NewTransactionRepository() *TransactionRepository {
	return &TransactionRepository{}
}

const transactionSelectFields = `
	id, merchant_id, wallet_id, amount_expected, COALESCE(amount_base, 0),
	COALESCE(fee_client, 0), currency, chain_type, status, tx_hash,
	external_ref_id, customer_email, COALESCE(commission_rate, 0),
	COALESCE(commission_split_merchant_percent, 0), expires_at, created_at`

func scanPaymentTransaction(scan func(dest ...any) error, tx *PaymentTransaction) error {
	return scan(
		&tx.ID, &tx.MerchantID, &tx.WalletID, &tx.AmountExpected, &tx.AmountBase,
		&tx.FeeClient, &tx.Currency, &tx.ChainType, &tx.Status, &tx.TxHash,
		&tx.ExternalRefID, &tx.CustomerEmail, &tx.CommissionRate, &tx.CommissionSplit,
		&tx.ExpiresAt, &tx.CreatedAt,
	)
}

func (r *TransactionRepository) Create(ctx context.Context, tx *PaymentTransaction) error {
	if tx.ExternalRefID != nil && *tx.ExternalRefID != "" {
		existing, err := r.FindByExternalRef(ctx, tx.MerchantID, *tx.ExternalRefID)
		if err != nil {
			return err
		}
		if existing != nil {
			tx.ID = existing.ID
			tx.Status = existing.Status
			tx.TxHash = existing.TxHash
			return nil
		}
	}

	return database.DB.QueryRow(ctx, `
		INSERT INTO transactions (
			merchant_id, amount_expected, amount_base, fee_client, currency,
			chain_type, status, external_ref_id, customer_email,
			commission_rate, commission_split_merchant_percent, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		tx.MerchantID, tx.AmountExpected, tx.AmountBase, tx.FeeClient,
		tx.Currency, tx.ChainType, tx.Status, tx.ExternalRefID, tx.CustomerEmail,
		tx.CommissionRate, tx.CommissionSplit, tx.ExpiresAt,
	).Scan(&tx.ID)
}

func (r *TransactionRepository) CreateTx(ctx context.Context, dbtx pgx.Tx, tx *PaymentTransaction) error {
	return dbtx.QueryRow(ctx, `
		INSERT INTO transactions (
			merchant_id, amount_expected, amount_base, fee_client, currency,
			chain_type, status, external_ref_id, customer_email,
			commission_rate, commission_split_merchant_percent, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		tx.MerchantID, tx.AmountExpected, tx.AmountBase, tx.FeeClient,
		tx.Currency, tx.ChainType, tx.Status, tx.ExternalRefID, tx.CustomerEmail,
		tx.CommissionRate, tx.CommissionSplit, tx.ExpiresAt,
	).Scan(&tx.ID)
}

func (r *TransactionRepository) FindByExternalRef(ctx context.Context, merchantID, externalRef string) (*PaymentTransaction, error) {
	var tx PaymentTransaction
	err := scanPaymentTransaction(func(dest ...any) error {
		return database.DB.QueryRow(ctx,
			`SELECT `+transactionSelectFields+` FROM transactions WHERE merchant_id=$1 AND external_ref_id=$2 LIMIT 1`,
			merchantID, externalRef,
		).Scan(dest...)
	}, &tx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to find transaction by external ref: %w", err)
	}
	return &tx, nil
}

func (r *TransactionRepository) FindByID(ctx context.Context, id string) (*PaymentTransaction, error) {
	var tx PaymentTransaction
	err := scanPaymentTransaction(func(dest ...any) error {
		return database.DB.QueryRow(ctx, `SELECT `+transactionSelectFields+` FROM transactions WHERE id=$1`, id).Scan(dest...)
	}, &tx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to find transaction: %w", err)
	}
	return &tx, nil
}

func (r *TransactionRepository) UpdatePaymentMethod(ctx context.Context, id, chainType, currency string) error {
	_, err := database.DB.Exec(ctx, "UPDATE transactions SET chain_type=$1, currency=$2 WHERE id=$3", chainType, currency, id)
	return err
}

func (r *TransactionRepository) UpdatePaymentMethodAndAmount(ctx context.Context, id, chainType, currency, amountExpected string) error {
	_, err := database.DB.Exec(ctx,
		"UPDATE transactions SET chain_type=$1, currency=$2, amount_expected=$3 WHERE id=$4",
		chainType, currency, amountExpected, id,
	)
	return err
}

func (r *TransactionRepository) UpdatePaymentMethodAndAmountTx(ctx context.Context, dbtx pgx.Tx, id, chainType, currency, amountExpected string) error {
	_, err := dbtx.Exec(ctx,
		"UPDATE transactions SET chain_type=$1, currency=$2, amount_expected=$3 WHERE id=$4",
		chainType, currency, amountExpected, id,
	)
	return err
}

func (r *TransactionRepository) UpdateStatus(ctx context.Context, id, status string, txHash *string) error {
	_, err := database.DB.Exec(ctx,
		"UPDATE transactions SET status=$1, tx_hash=COALESCE($2, tx_hash), updated_at=NOW() WHERE id=$3",
		status, txHash, id,
	)
	return err
}

func (r *TransactionRepository) ClearOrUpdateStatus(ctx context.Context, id, status string, txHash *string) error {
	_, err := database.DB.Exec(ctx, `
		UPDATE transactions
		SET status = $1, tx_hash = $2, updated_at = NOW()
		WHERE id = $3`, status, txHash, id)
	return err
}

func (r *TransactionRepository) ListExpiredPending(ctx context.Context, now time.Time) ([]PaymentTransaction, error) {
	rows, err := database.DB.Query(ctx, `
		SELECT `+transactionSelectFields+`
		FROM transactions
		WHERE status = 'PENDING' AND expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []PaymentTransaction
	for rows.Next() {
		var tx PaymentTransaction
		if err := scanPaymentTransaction(rows.Scan, &tx); err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}
