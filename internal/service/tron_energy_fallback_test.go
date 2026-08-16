package service

import (
	"context"
	"errors"
	"testing"
)

type stubTronEnergyProvider struct {
	create       func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error)
	getByOrderNo func(ctx context.Context, orderNo string) (*TronEnergyOrder, error)
}

func (s stubTronEnergyProvider) IsConfigured() bool { return true }
func (s stubTronEnergyProvider) CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
	return s.create(ctx, req)
}
func (s stubTronEnergyProvider) GetOrder(context.Context, string) (*TronEnergyOrder, error) {
	return nil, errors.New("not implemented")
}
func (s stubTronEnergyProvider) GetOrderByOrderNo(context.Context, string) (*TronEnergyOrder, error) {
	if s.getByOrderNo != nil {
		return s.getByOrderNo(context.Background(), "")
	}
	return nil, errors.New("not implemented")
}
func (s stubTronEnergyProvider) GetAccountInfo(context.Context) (*TronEnergyAccountInfo, error) {
	return nil, errors.New("not implemented")
}
func (s stubTronEnergyProvider) GetPriceInfo(context.Context) (*TronEnergyPriceInfo, error) {
	return nil, errors.New("not implemented")
}

func TestFallbackTronEnergyProviderFallsBackOnRateLimit(t *testing.T) {
	t.Parallel()

	fallback := NewFallbackTronEnergyProvider(
		NamedTronEnergyProvider("primary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return nil, errors.New("provider returned HTTP 429: rate limited")
			},
		}),
		NamedTronEnergyProvider("secondary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return &TronEnergyOrder{ProviderName: "secondary", ProviderOrderID: "42"}, nil
			},
		}),
	)

	order, err := fallback.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{ReceiverAddress: "TAddr"})
	if err != nil {
		t.Fatalf("CreateEnergyOrder() error = %v", err)
	}
	if order.ProviderOrderID != "42" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "42")
	}
}

func TestFallbackTronEnergyProviderStopsOnNonFallbackError(t *testing.T) {
	t.Parallel()

	fallback := NewFallbackTronEnergyProvider(
		NamedTronEnergyProvider("primary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return nil, errors.New("provider error: receiver address is not activated")
			},
		}),
		NamedTronEnergyProvider("secondary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return &TronEnergyOrder{ProviderName: "secondary", ProviderOrderID: "42"}, nil
			},
		}),
	)

	_, err := fallback.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{ReceiverAddress: "TAddr"})
	if err == nil {
		t.Fatal("expected non-fallback error, got nil")
	}
	if !isTronEnergyReceiverNotActivatedError(err) {
		t.Fatalf("expected receiver-not-activated error, got %v", err)
	}
}

func TestFallbackTronEnergyProviderFallsBackOnCapacityExceeded(t *testing.T) {
	t.Parallel()

	fallback := NewFallbackTronEnergyProvider(
		NamedTronEnergyProvider("primary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return nil, errors.New("provider error: 下单笔数超过平台当前可委托能量上限，请减少笔数或稍后重试")
			},
		}),
		NamedTronEnergyProvider("secondary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return &TronEnergyOrder{ProviderName: "secondary", ProviderOrderID: "42"}, nil
			},
		}),
	)

	order, err := fallback.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{ReceiverAddress: "TAddr"})
	if err != nil {
		t.Fatalf("CreateEnergyOrder() error = %v", err)
	}
	if order.ProviderOrderID != "42" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "42")
	}
}

func TestFallbackTronEnergyProviderRecoversExistingOrderBeforeFallback(t *testing.T) {
	t.Parallel()

	fallback := NewFallbackTronEnergyProvider(
		NamedTronEnergyProvider("primary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				return nil, errors.New("provider returned HTTP 429: rate limited")
			},
			getByOrderNo: func(ctx context.Context, orderNo string) (*TronEnergyOrder, error) {
				return &TronEnergyOrder{
					ProviderName:     "primary",
					ProviderOrderID:  "739",
					ProviderOrderNo:  "sweep-1-a1",
					NormalizedStatus: TronEnergyRentalStatusPending,
				}, nil
			},
		}),
		NamedTronEnergyProvider("secondary", stubTronEnergyProvider{
			create: func(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
				t.Fatal("secondary provider should not be called after recovery")
				return nil, nil
			},
		}),
	)

	order, err := fallback.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{
		ReceiverAddress: "TAddr",
		OrderNo:         "sweep-1-a1",
	})
	if err != nil {
		t.Fatalf("CreateEnergyOrder() error = %v", err)
	}
	if order.ProviderOrderID != "739" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "739")
	}
}
