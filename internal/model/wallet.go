package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type HDWallet struct {
	ID                        string          `db:"id" json:"id"`
	Coin                      string          `db:"coin" json:"coin"`
	Network                   string          `db:"network" json:"network"`
	ChainType                 string          `db:"-" json:"chain_type,omitempty"` // Derived compatibility field
	MasterPublicKey           string          `db:"master_public_key" json:"xpub_key"`
	EncryptedMasterSeed       *string         `db:"encrypted_master_seed" json:"-"`
	HotWalletAddress          string          `db:"hot_wallet_address" json:"hot_wallet_address"`
	DerivationAccount         int             `db:"derivation_account" json:"derivation_account"`
	CurrentDerivationIndex    int             `db:"current_derivation_index" json:"current_derivation_index"`
	RequiredConfirmations     int             `db:"required_confirmations" json:"required_confirmations"`
	FinalizationConfirmations int             `db:"finalization_confirmations" json:"finalization_confirmations"`
	AmountTolerancePercent    decimal.Decimal `db:"amount_tolerance_percent" json:"amount_tolerance_percent"`
	Decimals                  int             `db:"decimals" json:"decimals"`
	IsEnabled                 bool            `db:"is_enabled" json:"is_enabled"`
	ContractAddress           *string         `db:"contract_address" json:"contract_address,omitempty"`
	DisplayName               *string         `db:"display_name" json:"display_name,omitempty"`
	IconURL                   *string         `db:"icon_url" json:"icon_url,omitempty"`
	CreatedAt                 time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt                 time.Time       `db:"updated_at" json:"updated_at"`
}

type CryptoDeposit struct {
	ID                    string              `db:"id" json:"id"`
	HDWalletID            string              `db:"hd_wallet_id" json:"hd_wallet_id"`
	UserID                *string             `db:"user_id" json:"user_id"`
	DepositAddress        string              `db:"deposit_address" json:"deposit_address"`
	PaymentAddress        string              `db:"payment_address" json:"payment_address"`
	DerivationIndex       int                 `db:"derivation_index" json:"derivation_index"`
	Coin                  string              `db:"coin" json:"coin"`
	Network               string              `db:"network" json:"network"`
	AmountExpected        decimal.NullDecimal `db:"amount_expected" json:"amount_expected"`
	AmountBase            decimal.NullDecimal `db:"amount_base" json:"amount_base"`
	FeeClient             decimal.NullDecimal `db:"fee_client" json:"fee_client"`
	DetectedAmount        decimal.NullDecimal `db:"detected_amount" json:"detected_amount"`
	TxHash                *string             `db:"tx_hash" json:"tx_hash"`
	Status                string              `db:"status" json:"status"` // PENDING, DETECTED, CONFIRMED, FINALIZED, FAILED
	PaymentID             *string             `db:"payment_id" json:"payment_id"`
	Confirmations         int                 `db:"confirmations" json:"confirmations"`
	RequiredConfirmations int                 `db:"required_confirmations" json:"required_confirmations"`
	IsLate                bool                `db:"is_late" json:"is_late"`
	WatchStatus           string              `db:"watch_status" json:"watch_status"`
	WatchExpiresAt        *time.Time          `db:"watch_expires_at" json:"watch_expires_at,omitempty"`
	LastInboundAt         *time.Time          `db:"last_inbound_at" json:"last_inbound_at,omitempty"`
	CommissionRate        decimal.NullDecimal `db:"commission_rate" json:"commission_rate"`
	CommissionSplit       int                 `db:"commission_split_merchant_percent" json:"commission_split_merchant_percent"`
	CreatedAt             time.Time           `db:"created_at" json:"created_at"`
	UpdatedAt             time.Time           `db:"updated_at" json:"updated_at"`
}

const (
	DepositWatchStatusActive = "ACTIVE"
	DepositWatchStatusClosed = "CLOSED"
)

type MerchantNetworkConfig struct {
	ID                    string    `db:"id" json:"id"`
	MerchantID            string    `db:"merchant_id" json:"merchant_id"`
	HDWalletID            string    `db:"hd_wallet_id" json:"hd_wallet_id"`
	RequiredConfirmations int       `db:"required_confirmations" json:"required_confirmations"`
	CreatedAt             time.Time `db:"created_at" json:"created_at"`
	UpdatedAt             time.Time `db:"updated_at" json:"updated_at"`
}
