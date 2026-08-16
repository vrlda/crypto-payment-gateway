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

const sweepSelectFields = `id, crypto_deposit_id, from_address, to_hot_wallet, amount,
		coin, network, is_token, sequence, source_receipts_count, source_total_amount,
		COALESCE(purpose, 'DEPOSIT_FUNDS') AS purpose, origin_sweep_id,
		tx_hash, provider_name, provider_order_id, provider_order_no, provider_status, provider_last_error,
		provider_metadata_json, energy_rental_expires_at,
		energy_used, energy_fee_sun, net_usage, net_fee_sun,
		fee, status, error_message, attempts, created_at, updated_at`

func qualifiedSweepSelectFields(alias string) string {
	if strings.TrimSpace(alias) == "" {
		return sweepSelectFields
	}
	return fmt.Sprintf(`%s.id, %s.crypto_deposit_id, %s.from_address, %s.to_hot_wallet, %s.amount,
		%s.coin, %s.network, %s.is_token, %s.sequence, %s.source_receipts_count, %s.source_total_amount,
		COALESCE(%s.purpose, 'DEPOSIT_FUNDS') AS purpose, %s.origin_sweep_id,
		%s.tx_hash, %s.provider_name, %s.provider_order_id, %s.provider_order_no, %s.provider_status, %s.provider_last_error,
		%s.provider_metadata_json, %s.energy_rental_expires_at,
		%s.energy_used, %s.energy_fee_sun, %s.net_usage, %s.net_fee_sun,
		%s.fee, %s.status, %s.error_message, %s.attempts, %s.created_at, %s.updated_at`,
		alias, alias, alias, alias, alias,
		alias, alias, alias, alias, alias, alias,
		alias, alias,
		alias, alias, alias, alias, alias, alias,
		alias, alias,
		alias, alias, alias, alias,
		alias, alias, alias, alias, alias, alias,
	)
}

func activeSweepStatuses() []model.SweepStatus {
	return []model.SweepStatus{
		model.SweepStatusPending,
		model.SweepStatusCheckingActivation,
		model.SweepStatusProcessingTransaction,
		model.SweepStatusRequestingEnergy,
		model.SweepStatusWaitingForGas,
		model.SweepStatusWaitingForPrefund,
		model.SweepStatusWaitingForEnergyRental,
		model.SweepStatusBroadcasting,
	}
}

func coveredSweepStatuses() []model.SweepStatus {
	return []model.SweepStatus{
		model.SweepStatusPending,
		model.SweepStatusCheckingActivation,
		model.SweepStatusProcessingTransaction,
		model.SweepStatusRequestingEnergy,
		model.SweepStatusWaitingForGas,
		model.SweepStatusWaitingForPrefund,
		model.SweepStatusWaitingForEnergyRental,
		model.SweepStatusBroadcasting,
		model.SweepStatusConfirmed,
	}
}

func blockingSweepStatuses() []model.SweepStatus {
	return coveredSweepStatuses()
}

func quotedSweepStatuses(statuses []model.SweepStatus) string {
	quoted := make([]string, 0, len(statuses))
	for _, status := range statuses {
		quoted = append(quoted, fmt.Sprintf("'%s'", status))
	}
	return strings.Join(quoted, ", ")
}

type SweepRepository struct{}

func NewSweepRepository() *SweepRepository {
	return &SweepRepository{}
}

func scanSweep(scan func(dest ...any) error, s *model.Sweep) error {
	return scan(
		&s.ID, &s.CryptoDepositID, &s.FromAddress, &s.ToHotWallet, &s.Amount,
		&s.Coin, &s.Network, &s.IsToken, &s.Sequence, &s.SourceReceiptsCount, &s.SourceTotalAmount,
		&s.Purpose, &s.OriginSweepID, &s.TxHash, &s.ProviderName, &s.ProviderOrderID, &s.ProviderOrderNo,
		&s.ProviderStatus, &s.ProviderLastError, &s.ProviderMetadataJSON, &s.EnergyRentalExpiresAt,
		&s.EnergyUsed, &s.EnergyFeeSun, &s.NetUsage, &s.NetFeeSun,
		&s.Fee, &s.Status, &s.ErrorMessage, &s.Attempts,
		&s.CreatedAt, &s.UpdatedAt,
	)
}

func (r *SweepRepository) Create(ctx context.Context, sweep *model.Sweep) error {
	query := `INSERT INTO sweeps (
		crypto_deposit_id, from_address, to_hot_wallet, amount,
		coin, network, is_token, sequence, source_receipts_count, source_total_amount,
		purpose, origin_sweep_id,
		status, fee, attempts, created_at, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, COALESCE($15, 0), NOW(), NOW())
	  RETURNING id`

	purpose := sweep.Purpose
	if purpose == "" {
		purpose = model.SweepPurposeDepositFunds
	}

	return database.DB.QueryRow(ctx, query,
		sweep.CryptoDepositID, sweep.FromAddress, sweep.ToHotWallet, sweep.Amount,
		sweep.Coin, sweep.Network, sweep.IsToken, sweep.Sequence, sweep.SourceReceiptsCount, sweep.SourceTotalAmount,
		purpose, sweep.OriginSweepID, sweep.Status, sweep.Fee, sweep.Attempts,
	).Scan(&sweep.ID)
}

func (r *SweepRepository) FindByDepositID(ctx context.Context, depositID string) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE crypto_deposit_id = $1 ORDER BY sequence DESC LIMIT 1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, depositID).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *SweepRepository) FindByID(ctx context.Context, id string) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE id = $1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, id).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *SweepRepository) FindLatestRoundByDepositID(ctx context.Context, depositID string) (*model.Sweep, error) {
	return r.FindByDepositID(ctx, depositID)
}

func (r *SweepRepository) FindBlockingDepositRoundByReceiptFrontier(ctx context.Context, depositID string, receiptCount int, totalAmount decimal.Decimal) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps
	          WHERE crypto_deposit_id = $1
	            AND source_receipts_count = $2
	            AND source_total_amount = $3
	            AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
	            AND status IN (` + quotedSweepStatuses(blockingSweepStatuses()) + `)
	          ORDER BY sequence DESC
	          LIMIT 1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, depositID, receiptCount, totalAmount).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *SweepRepository) FindLatestRoundByReceiptFrontierAnyStatus(ctx context.Context, depositID string, receiptCount int, totalAmount decimal.Decimal) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps
	          WHERE crypto_deposit_id = $1
	            AND source_receipts_count = $2
	            AND source_total_amount = $3
	            AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
	          ORDER BY sequence DESC
	          LIMIT 1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, depositID, receiptCount, totalAmount).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *SweepRepository) SumCoveredAmountByDepositID(ctx context.Context, depositID string) (decimal.Decimal, error) {
	var total decimal.Decimal
	err := database.DB.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM sweeps
		WHERE crypto_deposit_id = $1
		  AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
		  AND status IN (`+quotedSweepStatuses(coveredSweepStatuses())+`)
	`, depositID).Scan(&total)
	return total, err
}

func (r *SweepRepository) FindActiveOrConfirmedResidueByOriginSweepID(ctx context.Context, originSweepID string) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps
	          WHERE origin_sweep_id = $1
	            AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'GAS_RESIDUE'
	            AND status IN (` + quotedSweepStatuses(coveredSweepStatuses()) + `)
	          ORDER BY sequence DESC
	          LIMIT 1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, originSweepID).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *SweepRepository) ListConfirmedDepositFundSweepsWithoutResidue(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + qualifiedSweepSelectFields("s") + `
	          FROM sweeps s
	          LEFT JOIN sweeps residue
	            ON residue.origin_sweep_id = s.id
	           AND COALESCE(residue.purpose, 'DEPOSIT_FUNDS') = 'GAS_RESIDUE'
	           AND residue.status IN (` + quotedSweepStatuses(coveredSweepStatuses()) + `)
	          WHERE COALESCE(s.purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
	            AND s.status = 'CONFIRMED'
	            AND residue.id IS NULL
	          ORDER BY s.updated_at ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

func (r *SweepRepository) ListActiveDepositFundSweepsByDepositID(ctx context.Context, depositID string) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps
	          WHERE crypto_deposit_id = $1
	            AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
	            AND status IN (` + quotedSweepStatuses(activeSweepStatuses()) + `)
	          ORDER BY created_at ASC`

	rows, err := database.DB.Query(ctx, query, depositID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

func (r *SweepRepository) HasConfirmedDepositFundSweepByDepositID(ctx context.Context, depositID string) (bool, error) {
	var exists bool
	err := database.DB.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM sweeps
			WHERE crypto_deposit_id = $1
			  AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
			  AND status = 'CONFIRMED'
		)
	`, depositID).Scan(&exists)
	return exists, err
}

func (r *SweepRepository) CountRecentFailuresByDepositID(ctx context.Context, depositID string, since time.Time) (int, error) {
	var count int
	err := database.DB.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM sweeps
		WHERE crypto_deposit_id = $1
		  AND status = 'FAILED'
		  AND updated_at >= $2
	`, depositID, since).Scan(&count)
	return count, err
}

func (r *SweepRepository) UpdateStatus(ctx context.Context, id string, status model.SweepStatus, txHash *string, errMsg *string) error {
	query := `UPDATE sweeps SET status = $1, tx_hash = COALESCE($2, tx_hash), error_message = $3, updated_at = NOW() WHERE id = $4`
	_, err := database.DB.Exec(ctx, query, status, txHash, errMsg, id)
	return err
}

// SweepReceiptData holds on-chain resource consumption for a confirmed TRON sweep.
type SweepReceiptData struct {
	EnergyUsed   *int64
	EnergyFeeSun *int64
	NetUsage     *int64
	NetFeeSun    *int64
}

func (r *SweepRepository) UpdateReceiptData(ctx context.Context, id string, d SweepReceiptData) error {
	_, err := database.DB.Exec(ctx, `
		UPDATE sweeps
		SET energy_used    = $2,
		    energy_fee_sun = $3,
		    net_usage      = $4,
		    net_fee_sun    = $5,
		    updated_at     = NOW()
		WHERE id = $1
	`, id, d.EnergyUsed, d.EnergyFeeSun, d.NetUsage, d.NetFeeSun)
	return err
}

func (r *SweepRepository) UpdateDestination(ctx context.Context, id, toHotWallet string) error {
	_, err := database.DB.Exec(ctx, `
		UPDATE sweeps
		SET to_hot_wallet = $1,
		    updated_at = NOW()
		WHERE id = $2
	`, toHotWallet, id)
	return err
}

func (r *SweepRepository) IncrementAttempts(ctx context.Context, id string) error {
	_, err := database.DB.Exec(ctx, `UPDATE sweeps SET attempts = attempts + 1, updated_at = NOW() WHERE id = $1`, id)
	return err
}

func (r *SweepRepository) FindPending(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + `
	          FROM sweeps
	          WHERE status IN ('PENDING', 'CHECKING_ACTIVATION', 'REQUESTING_ENERGY', 'PROCESSING_TRANSACTION', 'WAITING_FOR_GAS')
	          ORDER BY
	              CASE WHEN status = 'PENDING' THEN 0 ELSE 1 END,
	              updated_at ASC
	          LIMIT 20`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

func (r *SweepRepository) FindAllUnswept(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + `
	          FROM sweeps
	          WHERE status IN ('PENDING', 'CHECKING_ACTIVATION', 'REQUESTING_ENERGY', 'PROCESSING_TRANSACTION', 'WAITING_FOR_GAS', 'WAITING_FOR_ENERGY_RENTAL', 'SKIPPED_NO_GAS', 'FAILED')
	          ORDER BY created_at ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

func (r *SweepRepository) FindWaitingForPrefund(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE status = 'WAITING_FOR_PREFUND'`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

func (r *SweepRepository) FindWaitingForEnergyRental(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE status = 'WAITING_FOR_ENERGY_RENTAL' ORDER BY updated_at ASC`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

type SweepProviderStateUpdate struct {
	ProviderName          *string
	ProviderOrderID       *string
	ProviderOrderNo       *string
	ProviderStatus        *string
	ProviderLastError     *string
	ProviderMetadataJSON  *string
	EnergyRentalExpiresAt *time.Time
	Status                *model.SweepStatus
	ClearProviderState    bool
}

func (r *SweepRepository) UpdateProviderState(ctx context.Context, id string, update SweepProviderStateUpdate) error {
	query := `UPDATE sweeps
		          SET provider_name = CASE WHEN $9 THEN NULL ELSE COALESCE($1, provider_name) END,
		              provider_order_id = CASE WHEN $9 THEN NULL ELSE COALESCE($2, provider_order_id) END,
		              provider_order_no = CASE WHEN $9 THEN NULL ELSE COALESCE($3, provider_order_no) END,
		              provider_status = CASE WHEN $9 THEN NULL ELSE COALESCE($4, provider_status) END,
		              provider_last_error = $5,
		              provider_metadata_json = CASE WHEN $9 THEN NULL ELSE COALESCE($6, provider_metadata_json) END,
		              energy_rental_expires_at = CASE WHEN $9 THEN NULL ELSE COALESCE($7, energy_rental_expires_at) END,
		              status = COALESCE($8, status),
		              updated_at = NOW()
		          WHERE id = $10`

	_, err := database.DB.Exec(
		ctx,
		query,
		update.ProviderName,
		update.ProviderOrderID,
		update.ProviderOrderNo,
		update.ProviderStatus,
		update.ProviderLastError,
		update.ProviderMetadataJSON,
		update.EnergyRentalExpiresAt,
		update.Status,
		update.ClearProviderState,
		id,
	)
	return err
}

func (r *SweepRepository) List(ctx context.Context, limit, offset int) ([]*model.Sweep, int, error) {
	var total int
	err := database.DB.QueryRow(ctx, "SELECT COUNT(*) FROM sweeps").Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + sweepSelectFields + ` FROM sweeps ORDER BY created_at DESC LIMIT $1 OFFSET $2`

	rows, err := database.DB.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, 0, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, total, nil
}

func (r *SweepRepository) FindBroadcasting(ctx context.Context) ([]*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE status = 'BROADCASTING'`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}

// FindActiveSweepByFromAddress returns the most recent non-confirmed, non-failed DEPOSIT_FUNDS sweep
// for a given from_address and network. Used to find the existing pool balance sweep to upsert.
func (r *SweepRepository) FindActiveSweepByFromAddress(ctx context.Context, fromAddress, network string) (*model.Sweep, error) {
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps
	          WHERE from_address = $1
	            AND network = $2
	            AND status NOT IN ('CONFIRMED', 'FAILED')
	            AND COALESCE(purpose, 'DEPOSIT_FUNDS') = 'DEPOSIT_FUNDS'
	          ORDER BY created_at DESC
	          LIMIT 1`

	var s model.Sweep
	err := scanSweep(func(dest ...any) error {
		return database.DB.QueryRow(ctx, query, fromAddress, network).Scan(dest...)
	}, &s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// UpdateAmount updates only the amount field of a sweep record.
func (r *SweepRepository) UpdateAmount(ctx context.Context, sweepID string, amount decimal.Decimal) error {
	_, err := database.DB.Exec(ctx,
		`UPDATE sweeps SET amount = $2, updated_at = NOW() WHERE id = $1`,
		sweepID, amount,
	)
	return err
}

func (r *SweepRepository) FindBroadcastingOlderThan(ctx context.Context, olderThan time.Duration) ([]*model.Sweep, error) {
	cutoff := time.Now().Add(-olderThan)
	query := `SELECT ` + sweepSelectFields + ` FROM sweeps WHERE status = 'BROADCASTING' AND updated_at < $1 ORDER BY updated_at ASC`

	rows, err := database.DB.Query(ctx, query, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sweeps []*model.Sweep
	for rows.Next() {
		var s model.Sweep
		if err := scanSweep(rows.Scan, &s); err != nil {
			return nil, err
		}
		sweeps = append(sweeps, &s)
	}
	return sweeps, nil
}
