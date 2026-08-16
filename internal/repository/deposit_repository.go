package repository

import (
	"context"
	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/model"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

const depositSelectFields = `id, hd_wallet_id, user_id, deposit_address,
		COALESCE(payment_address, deposit_address) AS payment_address,
		derivation_index, coin, network, amount_expected, amount_base, fee_client,
		detected_amount, tx_hash, status, payment_id, confirmations, required_confirmations,
		COALESCE(is_late, FALSE), COALESCE(watch_status, 'ACTIVE'),
		watch_expires_at, last_inbound_at, COALESCE(commission_rate, 0),
		COALESCE(commission_split_merchant_percent, 100), created_at, updated_at`

type DepositRepository struct{}

func NewDepositRepository() *DepositRepository {
	return &DepositRepository{}
}

type FundedDeposit struct {
	model.CryptoDeposit
	TotalReceived decimal.Decimal `json:"total_received"`
	CoveredAmount decimal.Decimal `json:"covered_amount"`
	UnsweptAmount decimal.Decimal `json:"unswept_amount"`
}

func scanDeposit(scan func(dest ...any) error, d *model.CryptoDeposit) error {
	return scan(
		&d.ID, &d.HDWalletID, &d.UserID, &d.DepositAddress, &d.PaymentAddress, &d.DerivationIndex,
		&d.Coin, &d.Network, &d.AmountExpected, &d.AmountBase, &d.FeeClient, &d.DetectedAmount, &d.TxHash,
		&d.Status, &d.PaymentID, &d.Confirmations, &d.RequiredConfirmations, &d.IsLate, &d.WatchStatus,
		&d.WatchExpiresAt, &d.LastInboundAt, &d.CommissionRate, &d.CommissionSplit, &d.CreatedAt, &d.UpdatedAt,
	)
}

func isCaseInsensitiveAddressNetwork(network string) bool {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "ERC20", "BEP20", "POLYGON", "ARBITRUM", "ETHEREUM", "BSC", "POL", "ARB":
		return true
	default:
		return false
	}
}

func (r *DepositRepository) FindByID(ctx context.Context, id string) (*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits WHERE id=$1`

	var d model.CryptoDeposit
	err := scanDeposit(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, id).Scan(dest...)
	}, &d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (r *DepositRepository) FindPendingByNetwork(ctx context.Context, network string) ([]*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
		          WHERE network=$1 AND status IN ('PENDING', 'DETECTED', 'CONFIRMED')`

	rows, err := database.DB.Query(ctx, query, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, err
		}
		deposits = append(deposits, &d)
	}
	return deposits, nil
}

func (r *DepositRepository) FindWatchedByNetwork(ctx context.Context, network string) ([]*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
	          WHERE network=$1 AND watch_status='ACTIVE'`

	rows, err := database.DB.Query(ctx, query, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, err
		}
		deposits = append(deposits, &d)
	}
	return deposits, nil
}

func (r *DepositRepository) FindWatchedByNetworkAndAddress(ctx context.Context, network, address string) (*model.CryptoDeposit, error) {
	var query string
	if isCaseInsensitiveAddressNetwork(network) {
		query = `SELECT ` + depositSelectFields + ` FROM crypto_deposits
		          WHERE network=$1
		            AND watch_status='ACTIVE'
		            AND (LOWER(payment_address)=LOWER($2) OR LOWER(deposit_address)=LOWER($2))
		          ORDER BY CASE WHEN LOWER(payment_address)=LOWER($2) THEN 0 ELSE 1 END, created_at DESC
		          LIMIT 1`
	} else {
		query = `SELECT ` + depositSelectFields + ` FROM crypto_deposits
		          WHERE network=$1
		            AND watch_status='ACTIVE'
		            AND (payment_address=$2 OR deposit_address=$2)
		          ORDER BY CASE WHEN payment_address=$2 THEN 0 ELSE 1 END, created_at DESC
		          LIMIT 1`
	}

	var d model.CryptoDeposit
	err := scanDeposit(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, network, address).Scan(dest...)
	}, &d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

// FindByPaymentID returns the deposit associated with a payment transaction.
func (r *DepositRepository) FindByPaymentID(ctx context.Context, paymentID string) (*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
	          WHERE payment_id=$1
	          ORDER BY
	            CASE
	              WHEN status IN ('FINALIZED', 'CONFIRMED', 'DETECTED') THEN 0
	              WHEN tx_hash IS NOT NULL THEN 1
	              WHEN detected_amount IS NOT NULL AND detected_amount > 0 THEN 2
	              ELSE 3
	            END,
	            created_at DESC
	          LIMIT 1`

	var d model.CryptoDeposit
	err := scanDeposit(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, paymentID).Scan(dest...)
	}, &d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (r *DepositRepository) ListByPaymentID(ctx context.Context, paymentID string) ([]*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
	          WHERE payment_id=$1
	          ORDER BY created_at DESC`

	rows, err := database.DB.Query(ctx, query, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, err
		}
		deposits = append(deposits, &d)
	}

	return deposits, nil
}

func (r *DepositRepository) DetachPendingSiblingsFromPayment(ctx context.Context, paymentID, keepDepositID string) (int64, error) {
	tag, err := database.DB.Exec(ctx, `
		UPDATE crypto_deposits
		SET payment_id = NULL, updated_at = NOW()
		WHERE payment_id = $1
		  AND id <> $2
		  AND status = 'PENDING'
		  AND confirmations = 0
		  AND COALESCE(tx_hash, '') = ''
		  AND COALESCE(detected_amount, 0) = 0
	`, paymentID, keepDepositID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *DepositRepository) UpdateStatus(ctx context.Context, id string, status string, txHash *string, amount *decimal.Decimal) error {
	query := `UPDATE crypto_deposits SET status=$1, updated_at=NOW()`
	args := []any{status}
	argIdx := 2

	if txHash != nil {
		query += fmt.Sprintf(", tx_hash=$%d", argIdx)
		args = append(args, *txHash)
		argIdx++
	}

	if amount != nil {
		query += fmt.Sprintf(", detected_amount=$%d", argIdx)
		args = append(args, *amount)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id=$%d", argIdx)
	args = append(args, id)

	if status == "DETECTED" {
		query += " AND status NOT IN ('CONFIRMED', 'FINALIZED')"
	} else if status == "CONFIRMED" {
		query += " AND status != 'FINALIZED'"
	}

	_, err := database.DB.Exec(ctx, query, args...)
	return err
}

func (r *DepositRepository) UpdateExpectedAmount(ctx context.Context, id string, amount decimal.Decimal) error {
	_, err := database.DB.Exec(ctx, `
		UPDATE crypto_deposits
		SET amount_expected = $1,
		    updated_at = NOW()
		WHERE id = $2
	`, amount, id)
	return err
}

func (r *DepositRepository) UpdateExpectedAmountTx(ctx context.Context, dbtx pgx.Tx, id string, amount decimal.Decimal) error {
	_, err := dbtx.Exec(ctx, `
		UPDATE crypto_deposits
		SET amount_expected = $1,
		    updated_at = NOW()
		WHERE id = $2
	`, amount, id)
	return err
}

func (r *DepositRepository) Create(ctx context.Context, d *model.CryptoDeposit) error {
	query := `INSERT INTO crypto_deposits
		(hd_wallet_id, user_id, deposit_address, payment_address, derivation_index, coin, network, amount_expected, amount_base, fee_client, status, payment_id, required_confirmations, is_late, commission_rate, commission_split_merchant_percent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`

	return database.DB.QueryRow(ctx, query,
		d.HDWalletID, d.UserID, d.DepositAddress, d.PaymentAddress, d.DerivationIndex,
		d.Coin, d.Network, d.AmountExpected, d.AmountBase, d.FeeClient, d.Status, d.PaymentID,
		d.RequiredConfirmations, d.IsLate, d.CommissionRate, d.CommissionSplit,
	).Scan(&d.ID)
}

func (r *DepositRepository) CreateTx(ctx context.Context, dbtx pgx.Tx, d *model.CryptoDeposit) error {
	query := `INSERT INTO crypto_deposits
		(hd_wallet_id, user_id, deposit_address, payment_address, derivation_index, coin, network, amount_expected, amount_base, fee_client, status, payment_id, required_confirmations, is_late, commission_rate, commission_split_merchant_percent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`

	return dbtx.QueryRow(ctx, query,
		d.HDWalletID, d.UserID, d.DepositAddress, d.PaymentAddress, d.DerivationIndex,
		d.Coin, d.Network, d.AmountExpected, d.AmountBase, d.FeeClient, d.Status, d.PaymentID,
		d.RequiredConfirmations, d.IsLate, d.CommissionRate, d.CommissionSplit,
	).Scan(&d.ID)
}

func (r *DepositRepository) UpdateConfirmations(ctx context.Context, id string, confs int) error {
	_, err := database.DB.Exec(ctx, `UPDATE crypto_deposits SET confirmations=$1, updated_at=NOW() WHERE id=$2`, confs, id)
	return err
}

func (r *DepositRepository) MarkLate(ctx context.Context, id string, isLate bool) error {
	_, err := database.DB.Exec(ctx, `UPDATE crypto_deposits SET is_late=$1, updated_at=NOW() WHERE id=$2`, isLate, id)
	return err
}

func (r *DepositRepository) TouchInboundActivity(ctx context.Context, depositID, txHash string, observedAt time.Time) error {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	_, err := database.DB.Exec(ctx, `
		UPDATE crypto_deposits
		SET watch_status = $1,
		    last_inbound_at = $2::timestamptz,
		    watch_expires_at = $2::timestamptz + INTERVAL '30 days',
		    updated_at = NOW()
		WHERE id = $3
	`, model.DepositWatchStatusActive, observedAt.UTC(), depositID)
	return err
}

func (r *DepositRepository) RecordIncomingTransfer(ctx context.Context, depositID, txHash string, amount decimal.Decimal) (bool, error) {
	query := `
		INSERT INTO crypto_deposit_receipts (crypto_deposit_id, tx_hash, amount)
		VALUES ($1, $2, $3)
		ON CONFLICT (crypto_deposit_id, tx_hash) DO NOTHING
	`

	tag, err := database.DB.Exec(ctx, query, depositID, txHash, amount)
	if err != nil {
		return false, err
	}

	return tag.RowsAffected() > 0, nil
}

func (r *DepositRepository) GetIncomingTransferAmount(ctx context.Context, depositID, txHash string) (decimal.Decimal, bool, error) {
	var amount decimal.Decimal
	err := database.DB.QueryRow(ctx, `
		SELECT amount
		FROM crypto_deposit_receipts
		WHERE crypto_deposit_id = $1
		  AND tx_hash = $2
	`, depositID, txHash).Scan(&amount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return decimal.Zero, false, nil
		}
		return decimal.Zero, false, err
	}
	return amount, true, nil
}

func (r *DepositRepository) UpsertIncomingTransferAmountMax(ctx context.Context, depositID, txHash string, amount decimal.Decimal) (bool, error) {
	tag, err := database.DB.Exec(ctx, `
		INSERT INTO crypto_deposit_receipts (crypto_deposit_id, tx_hash, amount)
		VALUES ($1, $2, $3)
		ON CONFLICT (crypto_deposit_id, tx_hash) DO UPDATE
		SET amount = EXCLUDED.amount
		WHERE crypto_deposit_receipts.amount < EXCLUDED.amount
	`, depositID, txHash, amount)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *DepositRepository) HasIncomingTransfer(ctx context.Context, depositID, txHash string) (bool, error) {
	var exists bool
	err := database.DB.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM crypto_deposit_receipts
			WHERE crypto_deposit_id = $1
			  AND tx_hash = $2
		)
	`, depositID, txHash).Scan(&exists)
	return exists, err
}

// TxHashAlreadyClaimed returns true if txHash appears in crypto_deposit_receipts
// for ANY deposit. Used by pool-address scanning to prevent re-attributing old
// transactions to new deposits at the same reused address.
func (r *DepositRepository) TxHashAlreadyClaimed(ctx context.Context, txHash string) (bool, error) {
	var exists bool
	err := database.DB.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM crypto_deposit_receipts WHERE tx_hash = $1
		)
	`, txHash).Scan(&exists)
	return exists, err
}

func (r *DepositRepository) DeleteIncomingTransfer(ctx context.Context, depositID, txHash string) (bool, error) {
	tag, err := database.DB.Exec(ctx, `
		DELETE FROM crypto_deposit_receipts
		WHERE crypto_deposit_id = $1
		  AND tx_hash = $2
	`, depositID, txHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *DepositRepository) SumIncomingTransfers(ctx context.Context, depositID string) (decimal.Decimal, error) {
	var total decimal.Decimal
	query := `SELECT COALESCE(SUM(amount), 0) FROM crypto_deposit_receipts WHERE crypto_deposit_id = $1`
	err := database.DB.QueryRow(ctx, query, depositID).Scan(&total)
	if err != nil {
		return decimal.Zero, err
	}
	return total, nil
}

func (r *DepositRepository) CountIncomingTransfers(ctx context.Context, depositID string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM crypto_deposit_receipts WHERE crypto_deposit_id = $1`
	err := database.DB.QueryRow(ctx, query, depositID).Scan(&count)
	return count, err
}

func (r *DepositRepository) ResetOnchainState(ctx context.Context, depositID string, detectedTotal decimal.Decimal, clearTxHash bool) error {
	query := `
		UPDATE crypto_deposits
		SET status = 'PENDING',
		    confirmations = 0,
		    detected_amount = $1,
		    watch_status = $2,
		    updated_at = NOW()
	`
	args := []any{detectedTotal, model.DepositWatchStatusActive}
	if clearTxHash {
		query += `, tx_hash = NULL`
	}
	query += ` WHERE id = $3`
	args = append(args, depositID)

	_, err := database.DB.Exec(ctx, query, args...)
	return err
}

func (r *DepositRepository) FindSweepEligibleWithUnsweptDelta(ctx context.Context) ([]*model.CryptoDeposit, error) {
	query := `
		WITH receipt_totals AS (
			SELECT
				d.id AS deposit_id,
				COALESCE(SUM(r.amount), COALESCE(d.detected_amount, 0), 0) AS total_received
			FROM crypto_deposits d
			LEFT JOIN crypto_deposit_receipts r ON r.crypto_deposit_id = d.id
			GROUP BY d.id, d.detected_amount
		),
		covered_sweeps AS (
			SELECT
				crypto_deposit_id,
				COALESCE(SUM(amount), 0) AS covered_amount
			FROM sweeps
			WHERE COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
			  AND status IN (` + quotedSweepStatuses(coveredSweepStatuses()) + `)
			GROUP BY crypto_deposit_id
		)
		SELECT ` + depositSelectFields + `
		FROM crypto_deposits d
		JOIN receipt_totals rt ON rt.deposit_id = d.id
		LEFT JOIN covered_sweeps cs ON cs.crypto_deposit_id = d.id
		WHERE d.status IN ('CONFIRMED', 'FINALIZED')
		  AND rt.total_received > COALESCE(cs.covered_amount, 0)
	`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, err
		}
		deposits = append(deposits, &d)
	}
	return deposits, nil
}

func (r *DepositRepository) CloseExpiredWatchWindows(ctx context.Context, now time.Time) (int64, error) {
	tag, err := database.DB.Exec(ctx, `
		WITH receipt_totals AS (
			SELECT
				d.id AS deposit_id,
				COALESCE(SUM(r.amount), COALESCE(d.detected_amount, 0), 0) AS total_received
			FROM crypto_deposits d
			LEFT JOIN crypto_deposit_receipts r ON r.crypto_deposit_id = d.id
			GROUP BY d.id, d.detected_amount
		),
		covered_sweeps AS (
			SELECT
				crypto_deposit_id,
				COALESCE(SUM(amount), 0) AS covered_amount
			FROM sweeps
			WHERE COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
			  AND status IN (`+quotedSweepStatuses(coveredSweepStatuses())+`)
			GROUP BY crypto_deposit_id
		),
		active_rounds AS (
			SELECT DISTINCT crypto_deposit_id
			FROM sweeps
			WHERE status IN (`+quotedSweepStatuses(activeSweepStatuses())+`)
		)
		UPDATE crypto_deposits d
		SET watch_status = $2,
		    updated_at = NOW()
		FROM receipt_totals rt
		LEFT JOIN covered_sweeps cs ON cs.crypto_deposit_id = rt.deposit_id
		LEFT JOIN active_rounds ar ON ar.crypto_deposit_id = rt.deposit_id
		WHERE d.id = rt.deposit_id
		  AND d.watch_status = $1
		  AND d.status = 'FINALIZED'
		  AND d.watch_expires_at IS NOT NULL
		  AND d.watch_expires_at < $3
		  AND COALESCE(cs.covered_amount, 0) >= rt.total_received
		  AND ar.crypto_deposit_id IS NULL
	`, model.DepositWatchStatusActive, model.DepositWatchStatusClosed, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *DepositRepository) ListFunded(ctx context.Context, limit, offset int) ([]*FundedDeposit, int, error) {
	countQuery := `
		WITH receipt_totals AS (
			SELECT
				d.id AS deposit_id,
				COALESCE(SUM(r.amount), COALESCE(d.detected_amount, 0), 0) AS total_received
			FROM crypto_deposits d
			LEFT JOIN crypto_deposit_receipts r ON r.crypto_deposit_id = d.id
			GROUP BY d.id, d.detected_amount
		),
		covered_sweeps AS (
			SELECT
				crypto_deposit_id,
				COALESCE(SUM(amount), 0) AS covered_amount
			FROM sweeps
			WHERE COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
			  AND status IN (` + quotedSweepStatuses(coveredSweepStatuses()) + `)
			GROUP BY crypto_deposit_id
		)
		SELECT COUNT(*)
		FROM crypto_deposits d
		JOIN receipt_totals rt ON rt.deposit_id = d.id
		LEFT JOIN covered_sweeps cs ON cs.crypto_deposit_id = d.id
		WHERE GREATEST(rt.total_received - COALESCE(cs.covered_amount, 0), 0) > 0
	`

	var total int
	if err := database.DB.QueryRow(ctx, countQuery).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		WITH receipt_totals AS (
			SELECT
				d.id AS deposit_id,
				COALESCE(SUM(r.amount), COALESCE(d.detected_amount, 0), 0) AS total_received
			FROM crypto_deposits d
			LEFT JOIN crypto_deposit_receipts r ON r.crypto_deposit_id = d.id
			GROUP BY d.id, d.detected_amount
		),
		covered_sweeps AS (
			SELECT
				crypto_deposit_id,
				COALESCE(SUM(amount), 0) AS covered_amount
			FROM sweeps
			WHERE COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
			  AND status IN (` + quotedSweepStatuses(coveredSweepStatuses()) + `)
			GROUP BY crypto_deposit_id
		)
		SELECT ` + depositSelectFields + `,
		       rt.total_received,
		       COALESCE(cs.covered_amount, 0) AS covered_amount,
		       GREATEST(rt.total_received - COALESCE(cs.covered_amount, 0), 0) AS unswept_amount
		FROM crypto_deposits d
		JOIN receipt_totals rt ON rt.deposit_id = d.id
		LEFT JOIN covered_sweeps cs ON cs.crypto_deposit_id = d.id
		WHERE GREATEST(rt.total_received - COALESCE(cs.covered_amount, 0), 0) > 0
		ORDER BY unswept_amount DESC, COALESCE(d.last_inbound_at, d.updated_at, d.created_at) DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := database.DB.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var funded []*FundedDeposit
	for rows.Next() {
		var row FundedDeposit
		err := rows.Scan(
			&row.ID, &row.HDWalletID, &row.UserID, &row.DepositAddress, &row.PaymentAddress, &row.DerivationIndex,
			&row.Coin, &row.Network, &row.AmountExpected, &row.AmountBase, &row.FeeClient, &row.DetectedAmount, &row.TxHash,
			&row.Status, &row.PaymentID, &row.Confirmations, &row.RequiredConfirmations, &row.IsLate, &row.WatchStatus,
			&row.WatchExpiresAt, &row.LastInboundAt, &row.CommissionRate, &row.CommissionSplit, &row.CreatedAt, &row.UpdatedAt,
			&row.TotalReceived, &row.CoveredAmount, &row.UnsweptAmount,
		)
		if err != nil {
			return nil, 0, err
		}
		funded = append(funded, &row)
	}

	return funded, total, nil
}

func (r *DepositRepository) List(ctx context.Context, limit, offset int) ([]*model.CryptoDeposit, int, error) {
	var total int
	err := database.DB.QueryRow(ctx, "SELECT COUNT(*) FROM crypto_deposits").Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
	          ORDER BY created_at DESC LIMIT $1 OFFSET $2`

	rows, err := database.DB.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, 0, err
		}
		deposits = append(deposits, &d)
	}
	return deposits, total, nil
}

func (r *DepositRepository) ListAll(ctx context.Context) ([]*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits ORDER BY created_at DESC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deposits []*model.CryptoDeposit
	for rows.Next() {
		var d model.CryptoDeposit
		if err := scanDeposit(rows.Scan, &d); err != nil {
			return nil, err
		}
		deposits = append(deposits, &d)
	}

	return deposits, nil
}

// FindLatestFinalizedByAddress returns the most recent FINALIZED or CONFIRMED deposit at a given address.
// Used to resolve HDWalletID and coin/network info for pool address sweeps.
func (r *DepositRepository) FindLatestFinalizedByAddress(ctx context.Context, address, network string) (*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits
	          WHERE deposit_address = $1 AND network = $2 AND status IN ('FINALIZED', 'CONFIRMED')
	          ORDER BY created_at DESC LIMIT 1`

	var d model.CryptoDeposit
	err := scanDeposit(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, address, network).Scan(dest...)
	}, &d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (r *DepositRepository) FindByTxHash(ctx context.Context, txHash string) (*model.CryptoDeposit, error) {
	query := `SELECT ` + depositSelectFields + ` FROM crypto_deposits WHERE tx_hash=$1`

	var d model.CryptoDeposit
	err := scanDeposit(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, txHash).Scan(dest...)
	}, &d)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}
