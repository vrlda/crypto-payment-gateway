package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type SweepStatus string

const (
	SweepStatusPending                SweepStatus = "PENDING"
	SweepStatusCheckingActivation     SweepStatus = "CHECKING_ACTIVATION"
	SweepStatusBroadcasting           SweepStatus = "BROADCASTING"
	SweepStatusConfirmed              SweepStatus = "CONFIRMED"
	SweepStatusFailed                 SweepStatus = "FAILED"
	SweepStatusProcessingTransaction  SweepStatus = "PROCESSING_TRANSACTION"
	SweepStatusRequestingEnergy       SweepStatus = "REQUESTING_ENERGY"
	SweepStatusWaitingForGas          SweepStatus = "WAITING_FOR_GAS"
	SweepStatusWaitingForPrefund      SweepStatus = "WAITING_FOR_PREFUND"
	SweepStatusWaitingForEnergyRental SweepStatus = "WAITING_FOR_ENERGY_RENTAL"
	SweepStatusSkippedNoGas           SweepStatus = "SKIPPED_NO_GAS"
)

type SweepPurpose string

const (
	// Deposit-fund sweeps move customer funds and count toward deposit coverage.
	SweepPurposeDepositFunds SweepPurpose = "DEPOSIT_FUNDS"
	// Gas-residue sweeps recover operational prefunds and never count toward deposit coverage.
	SweepPurposeGasResidue SweepPurpose = "GAS_RESIDUE"
)

type Sweep struct {
	ID                    string          `db:"id" json:"id"`
	CryptoDepositID       string          `db:"crypto_deposit_id" json:"crypto_deposit_id"`
	FromAddress           string          `db:"from_address" json:"from_address"`
	ToHotWallet           string          `db:"to_hot_wallet" json:"to_hot_wallet"`
	Amount                decimal.Decimal `db:"amount" json:"amount"`
	Coin                  string          `db:"coin" json:"coin"`
	Network               string          `db:"network" json:"network"`
	IsToken               bool            `db:"is_token" json:"is_token"`
	Sequence              int             `db:"sequence" json:"sequence"`
	SourceReceiptsCount   int             `db:"source_receipts_count" json:"source_receipts_count"`
	SourceTotalAmount     decimal.Decimal `db:"source_total_amount" json:"source_total_amount"`
	Purpose               SweepPurpose    `db:"purpose" json:"purpose"`
	OriginSweepID         *string         `db:"origin_sweep_id" json:"origin_sweep_id,omitempty"`
	TxHash                *string         `db:"tx_hash" json:"tx_hash"`
	ProviderName          *string         `db:"provider_name" json:"provider_name,omitempty"`
	ProviderOrderID       *string         `db:"provider_order_id" json:"provider_order_id,omitempty"`
	ProviderOrderNo       *string         `db:"provider_order_no" json:"provider_order_no,omitempty"`
	ProviderStatus        *string         `db:"provider_status" json:"provider_status,omitempty"`
	ProviderLastError     *string         `db:"provider_last_error" json:"provider_last_error,omitempty"`
	ProviderMetadataJSON  *string         `db:"provider_metadata_json" json:"provider_metadata_json,omitempty"`
	EnergyRentalExpiresAt *time.Time      `db:"energy_rental_expires_at" json:"energy_rental_expires_at,omitempty"`
	EnergyUsed            *int64          `db:"energy_used" json:"energy_used,omitempty"`
	EnergyFeeSun          *int64          `db:"energy_fee_sun" json:"energy_fee_sun,omitempty"`
	NetUsage              *int64          `db:"net_usage" json:"net_usage,omitempty"`
	NetFeeSun             *int64          `db:"net_fee_sun" json:"net_fee_sun,omitempty"`
	Fee                   decimal.Decimal `db:"fee" json:"fee"`
	Status                SweepStatus     `db:"status" json:"status"`
	ErrorMessage          *string         `db:"error_message" json:"error_message"`
	Attempts              int             `db:"attempts" json:"attempts"`
	CreatedAt             time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt             time.Time       `db:"updated_at" json:"updated_at"`
}

type GasWallet struct {
	ID                string              `db:"id" json:"id"`
	ChainType         string              `db:"chain_type" json:"chain_type"`
	WalletAddress     string              `db:"wallet_address" json:"wallet_address"`
	PrivateKeyEnc     string              `db:"private_key_enc" json:"private_key,omitempty"`
	IsEnabled         bool                `db:"is_enabled" json:"is_enabled"`
	MinBalance        decimal.Decimal     `db:"min_balance" json:"min_balance"`
	MaxGasPrice       decimal.Decimal     `db:"max_gas_price" json:"max_gas_price"`
	DailyGasLimit     decimal.Decimal     `db:"daily_gas_limit" json:"daily_gas_limit"`
	MaxGasPerSweep    decimal.Decimal     `db:"max_gas_per_sweep" json:"max_gas_per_sweep"`
	DailyGasUsed      decimal.Decimal     `db:"daily_gas_used" json:"daily_gas_used"`
	DailyResetAt      time.Time           `db:"daily_reset_at" json:"daily_reset_at"`
	CurrentBalance    decimal.NullDecimal `db:"current_balance" json:"current_balance,omitempty"`
	BalanceCheckedAt  *time.Time          `db:"balance_checked_at" json:"balance_checked_at,omitempty"`
	BalanceCheckError *string             `db:"balance_check_error" json:"balance_check_error,omitempty"`
	IsBelowMinBalance bool                `db:"is_below_min_balance" json:"is_below_min_balance"`
	LastAlertBalance  decimal.NullDecimal `db:"last_alert_balance" json:"last_alert_balance,omitempty"`
	CreatedAt         time.Time           `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time           `db:"updated_at" json:"updated_at"`
}
