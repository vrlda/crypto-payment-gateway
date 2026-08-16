package repository

import (
	"context"
	"fmt"
	"log"
	"time"

	"crypto_payment_gateway_core/internal/database"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// PoolAddress is a row in trc20_address_pool.
type PoolAddress struct {
	ID                string
	Address           string
	HDWalletID        string
	DerivationIndex   int
	Status            string
	AssignedDepositID *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// PoolAddressWithBalance augments PoolAddress with accumulated unswept USDT.
type PoolAddressWithBalance struct {
	PoolAddress
	AccumulatedUSDT decimal.Decimal
	DepositCount    int
}

// UnsweptPoolDeposit is a deposit at a pool address that has not been fully swept.
type UnsweptPoolDeposit struct {
	DepositID     string
	UnsweptAmount decimal.Decimal
}

type TRC20PoolRepository struct{}

func NewTRC20PoolRepository() *TRC20PoolRepository { return &TRC20PoolRepository{} }

// AcquireAddressTx picks the best available pool address inside an existing transaction.
// Returns nil, nil when the pool is empty (caller must derive a new address).
// Prefers addresses that already have accumulated balance.
func (r *TRC20PoolRepository) AcquireAddressTx(ctx context.Context, tx pgx.Tx) (*PoolAddress, error) {
	query := `
		SELECT id, address, hd_wallet_id, derivation_index, status, assigned_deposit_id, created_at, updated_at
		FROM trc20_address_pool
		WHERE status = 'available'
		ORDER BY (
			EXISTS (
				SELECT 1 FROM crypto_deposit_receipts cr
				JOIN crypto_deposits cd ON cd.id = cr.crypto_deposit_id
				WHERE cd.deposit_address = trc20_address_pool.address
			)
		) DESC, updated_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`
	var p PoolAddress
	err := tx.QueryRow(ctx, query).Scan(
		&p.ID, &p.Address, &p.HDWalletID, &p.DerivationIndex,
		&p.Status, &p.AssignedDepositID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trc20 pool acquire: %w", err)
	}
	return &p, nil
}

// InsertTx adds a freshly derived address to the pool inside an existing transaction.
func (r *TRC20PoolRepository) InsertTx(ctx context.Context, tx pgx.Tx, address, hdWalletID string, derivationIndex int) (*PoolAddress, error) {
	var p PoolAddress
	err := tx.QueryRow(ctx, `
		INSERT INTO trc20_address_pool (address, hd_wallet_id, derivation_index, status)
		VALUES ($1, $2, $3, 'available')
		RETURNING id, address, hd_wallet_id, derivation_index, status, assigned_deposit_id, created_at, updated_at
	`, address, hdWalletID, derivationIndex).Scan(
		&p.ID, &p.Address, &p.HDWalletID, &p.DerivationIndex,
		&p.Status, &p.AssignedDepositID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("trc20 pool insert: %w", err)
	}
	return &p, nil
}

// MarkInUseTx marks a pool address as in_use and links it to a deposit.
func (r *TRC20PoolRepository) MarkInUseTx(ctx context.Context, tx pgx.Tx, poolID, depositID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE trc20_address_pool
		SET status = 'in_use', assigned_deposit_id = $2, updated_at = NOW()
		WHERE id = $1
	`, poolID, depositID)
	return err
}

// ReleaseByDepositID returns a pool address to 'available' after its deposit reaches a terminal state.
// It also closes the deposit's watch window so the TRON scanner stops monitoring the address —
// if left open, the scanner would attribute the next customer's transaction to the old deposit
// instead of the new deposit that reuses the same pool address.
func (r *TRC20PoolRepository) ReleaseByDepositID(ctx context.Context, depositID string) error {
	// Close the scanner watch window on the released deposit so the scanner stops monitoring
	// this address and doesn't attribute the next customer's transaction to the old deposit.
	if _, err := database.DB.Exec(ctx, `
		UPDATE crypto_deposits
		SET watch_status = 'CLOSED', updated_at = NOW()
		WHERE id = $1 AND watch_status = 'ACTIVE'
	`, depositID); err != nil {
		log.Printf("ReleaseByDepositID: failed to close watch window for deposit %s: %v", depositID, err)
	}

	_, err := database.DB.Exec(ctx, `
		UPDATE trc20_address_pool
		SET status = 'available', assigned_deposit_id = NULL, updated_at = NOW()
		WHERE assigned_deposit_id = $1
	`, depositID)
	return err
}

// ListWithBalances returns all pool addresses with their accumulated unswept USDT totals.
func (r *TRC20PoolRepository) ListWithBalances(ctx context.Context) ([]PoolAddressWithBalance, error) {
	coveredIn := quotedSweepStatuses(coveredSweepStatuses())
	query := fmt.Sprintf(`
		WITH receipt_totals AS (
			SELECT d.deposit_address,
			       COALESCE(SUM(cr.amount), 0) AS total_received
			FROM crypto_deposits d
			LEFT JOIN crypto_deposit_receipts cr ON cr.crypto_deposit_id = d.id
			WHERE d.network = 'TRC20'
			GROUP BY d.deposit_address
		),
		covered_totals AS (
			SELECT d.deposit_address,
			       COALESCE(SUM(s.amount), 0) AS covered_amount
			FROM crypto_deposits d
			JOIN sweeps s ON s.crypto_deposit_id = d.id
			WHERE d.network = 'TRC20'
			  AND s.status IN (%s)
			GROUP BY d.deposit_address
		),
		deposit_counts AS (
			SELECT deposit_address, COUNT(*) AS cnt
			FROM crypto_deposits
			WHERE network = 'TRC20'
			GROUP BY deposit_address
		)
		SELECT p.id, p.address, p.hd_wallet_id, p.derivation_index, p.status,
		       p.assigned_deposit_id, p.created_at, p.updated_at,
		       GREATEST(COALESCE(rt.total_received, 0) - COALESCE(ct.covered_amount, 0), 0) AS accumulated_usdt,
		       COALESCE(dc.cnt, 0) AS deposit_count
		FROM trc20_address_pool p
		LEFT JOIN receipt_totals   rt ON rt.deposit_address = p.address
		LEFT JOIN covered_totals   ct ON ct.deposit_address = p.address
		LEFT JOIN deposit_counts   dc ON dc.deposit_address = p.address
		ORDER BY accumulated_usdt DESC, p.created_at ASC
	`, coveredIn)

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("trc20 pool list: %w", err)
	}
	defer rows.Close()

	var out []PoolAddressWithBalance
	for rows.Next() {
		var pb PoolAddressWithBalance
		if err := rows.Scan(
			&pb.ID, &pb.Address, &pb.HDWalletID, &pb.DerivationIndex, &pb.Status,
			&pb.AssignedDepositID, &pb.CreatedAt, &pb.UpdatedAt,
			&pb.AccumulatedUSDT, &pb.DepositCount,
		); err != nil {
			return nil, err
		}
		out = append(out, pb)
	}
	return out, rows.Err()
}

// FindByID returns a pool entry by its ID.
func (r *TRC20PoolRepository) FindByID(ctx context.Context, id string) (*PoolAddress, error) {
	var p PoolAddress
	err := database.DB.QueryRow(ctx, `
		SELECT id, address, hd_wallet_id, derivation_index, status, assigned_deposit_id, created_at, updated_at
		FROM trc20_address_pool WHERE id = $1
	`, id).Scan(
		&p.ID, &p.Address, &p.HDWalletID, &p.DerivationIndex,
		&p.Status, &p.AssignedDepositID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// FindByAddress returns a pool entry by TRC20 address (used after sweep confirmation).
func (r *TRC20PoolRepository) FindByAddress(ctx context.Context, address string) (*PoolAddress, error) {
	var p PoolAddress
	err := database.DB.QueryRow(ctx, `
		SELECT id, address, hd_wallet_id, derivation_index, status, assigned_deposit_id, created_at, updated_at
		FROM trc20_address_pool WHERE address = $1
	`, address).Scan(
		&p.ID, &p.Address, &p.HDWalletID, &p.DerivationIndex,
		&p.Status, &p.AssignedDepositID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// FindUnsweptDeposits returns all FINALIZED deposits at a pool address that have
// unswept balance, sorted oldest first.
func (r *TRC20PoolRepository) FindUnsweptDeposits(ctx context.Context, address string) ([]UnsweptPoolDeposit, error) {
	coveredIn := quotedSweepStatuses(coveredSweepStatuses())
	query := fmt.Sprintf(`
		SELECT d.id,
		       GREATEST(
		           COALESCE(SUM(cr.amount), 0) - COALESCE(SUM(s.amount) FILTER (WHERE s.status IN (%s)), 0),
		           0
		       ) AS unswept
		FROM crypto_deposits d
		LEFT JOIN crypto_deposit_receipts cr ON cr.crypto_deposit_id = d.id
		LEFT JOIN sweeps s ON s.crypto_deposit_id = d.id
		WHERE d.deposit_address = $1
		  AND d.network = 'TRC20'
		  AND d.status IN ('FINALIZED', 'CONFIRMED')
		GROUP BY d.id
		HAVING GREATEST(
		    COALESCE(SUM(cr.amount), 0) - COALESCE(SUM(s.amount) FILTER (WHERE s.status IN (%s)), 0),
		    0
		) > 0
		ORDER BY d.created_at ASC
	`, coveredIn, coveredIn)

	rows, err := database.DB.Query(ctx, query, address)
	if err != nil {
		return nil, fmt.Errorf("trc20 pool find unswept: %w", err)
	}
	defer rows.Close()

	var out []UnsweptPoolDeposit
	for rows.Next() {
		var u UnsweptPoolDeposit
		if err := rows.Scan(&u.DepositID, &u.UnsweptAmount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// IsPoolAddress returns true if the given address belongs to the TRC20 pool.
func (r *TRC20PoolRepository) IsPoolAddress(ctx context.Context, address string) (bool, error) {
	var exists bool
	err := database.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM trc20_address_pool WHERE address = $1)`, address,
	).Scan(&exists)
	return exists, err
}
