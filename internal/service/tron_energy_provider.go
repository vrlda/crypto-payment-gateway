package service

import "context"

type TronEnergyRentalStatus string

const (
	TronEnergyRentalStatusPending TronEnergyRentalStatus = "pending"
	TronEnergyRentalStatusActive  TronEnergyRentalStatus = "active"
	TronEnergyRentalStatusFailed  TronEnergyRentalStatus = "failed"
	TronEnergyRentalStatusUnknown TronEnergyRentalStatus = "unknown"
)

type TronEnergyOrderRequest struct {
	ReceiverAddress string
	OrderNo         string
	TransferCount   int
	DurationHours   int
}

type TronEnergyOrder struct {
	ProviderName     string
	ProviderOrderID  string
	ProviderOrderNo  string
	ProviderStatus   string
	NormalizedStatus TronEnergyRentalStatus
	MetadataJSON     string
}

type TronEnergyAccountInfo struct {
	AvailableBalanceSun int64
	DepositAddress      string
}

type TronEnergyPriceTier struct {
	DurationHours int
	Multiplier    float64
}

type TronEnergyPriceInfo struct {
	ActivatedPriceSun int64
	ActivatedEnergy   int64
	DurationTiers     []TronEnergyPriceTier
}

type TronEnergyProvider interface {
	IsConfigured() bool
	CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error)
	GetOrder(ctx context.Context, providerOrderID string) (*TronEnergyOrder, error)
	GetOrderByOrderNo(ctx context.Context, orderNo string) (*TronEnergyOrder, error)
	GetAccountInfo(ctx context.Context) (*TronEnergyAccountInfo, error)
	GetPriceInfo(ctx context.Context) (*TronEnergyPriceInfo, error)
}
