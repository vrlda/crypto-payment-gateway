package service

import (
	"context"
	"fmt"
	"strings"
)

type namedTronEnergyProvider struct {
	name     string
	provider TronEnergyProvider
}

func NamedTronEnergyProvider(name string, provider TronEnergyProvider) namedTronEnergyProvider {
	return namedTronEnergyProvider{name: strings.TrimSpace(name), provider: provider}
}

type FallbackTronEnergyProvider struct {
	providers []namedTronEnergyProvider
}

func NewFallbackTronEnergyProvider(providers ...namedTronEnergyProvider) *FallbackTronEnergyProvider {
	filtered := make([]namedTronEnergyProvider, 0, len(providers))
	for _, provider := range providers {
		if provider.provider == nil || !provider.provider.IsConfigured() {
			continue
		}
		filtered = append(filtered, provider)
	}
	return &FallbackTronEnergyProvider{providers: filtered}
}

func (p *FallbackTronEnergyProvider) IsConfigured() bool {
	return len(p.providers) > 0
}

func (p *FallbackTronEnergyProvider) CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
	var errs []string
	for _, candidate := range p.providers {
		order, err := candidate.provider.CreateEnergyOrder(ctx, req)
		if err == nil {
			return order, nil
		}
		if recovered := recoverTronEnergyOrderByOrderNo(ctx, candidate.provider, req.OrderNo); recovered != nil {
			return recovered, nil
		}
		if !canSafelyFallbackAfterCreateError(candidate.name, err) {
			return nil, err
		}
		if !isTronEnergyFallbackCandidateError(err) {
			return nil, err
		}
		errs = append(errs, fmt.Sprintf("%s: %v", candidate.name, err))
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("TRON energy provider is not configured")
	}
	return nil, fmt.Errorf("all TRON energy providers failed: %s", strings.Join(errs, " | "))
}

func (p *FallbackTronEnergyProvider) GetOrder(ctx context.Context, providerOrderID string) (*TronEnergyOrder, error) {
	var errs []string
	for _, candidate := range p.providers {
		order, err := candidate.provider.GetOrder(ctx, providerOrderID)
		if err == nil {
			return order, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", candidate.name, err))
	}
	return nil, fmt.Errorf("failed to fetch TRON energy order %s from configured providers: %s", providerOrderID, strings.Join(errs, " | "))
}

func (p *FallbackTronEnergyProvider) GetOrderByOrderNo(ctx context.Context, orderNo string) (*TronEnergyOrder, error) {
	var errs []string
	for _, candidate := range p.providers {
		order, err := candidate.provider.GetOrderByOrderNo(ctx, orderNo)
		if err == nil {
			return order, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", candidate.name, err))
	}
	return nil, fmt.Errorf("failed to fetch TRON energy order %s by orderNo from configured providers: %s", orderNo, strings.Join(errs, " | "))
}

func (p *FallbackTronEnergyProvider) GetAccountInfo(ctx context.Context) (*TronEnergyAccountInfo, error) {
	var errs []string
	for _, candidate := range p.providers {
		info, err := candidate.provider.GetAccountInfo(ctx)
		if err == nil {
			return info, nil
		}
		if !isTronEnergyFallbackCandidateError(err) {
			return nil, err
		}
		errs = append(errs, fmt.Sprintf("%s: %v", candidate.name, err))
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("TRON energy provider is not configured")
	}
	return nil, fmt.Errorf("failed to query TRON energy provider balance: %s", strings.Join(errs, " | "))
}

func (p *FallbackTronEnergyProvider) GetPriceInfo(ctx context.Context) (*TronEnergyPriceInfo, error) {
	var errs []string
	for _, candidate := range p.providers {
		info, err := candidate.provider.GetPriceInfo(ctx)
		if err == nil {
			return info, nil
		}
		if !isTronEnergyFallbackCandidateError(err) {
			return nil, err
		}
		errs = append(errs, fmt.Sprintf("%s: %v", candidate.name, err))
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("TRON energy provider is not configured")
	}
	return nil, fmt.Errorf("failed to query TRON energy provider pricing: %s", strings.Join(errs, " | "))
}

func recoverTronEnergyOrderByOrderNo(ctx context.Context, provider TronEnergyProvider, orderNo string) *TronEnergyOrder {
	if provider == nil || strings.TrimSpace(orderNo) == "" {
		return nil
	}
	order, err := provider.GetOrderByOrderNo(ctx, orderNo)
	if err != nil || order == nil {
		return nil
	}
	switch order.NormalizedStatus {
	case TronEnergyRentalStatusPending, TronEnergyRentalStatusActive:
		return order
	default:
		return nil
	}
}

func canSafelyFallbackAfterCreateError(providerName string, err error) bool {
	if err == nil {
		return true
	}
	if providerSupportsOrderNoRecovery(providerName) {
		return true
	}
	return isTronEnergyDefinitelyRejectedError(err)
}

func providerSupportsOrderNoRecovery(providerName string) bool {
	return strings.TrimSpace(providerName) != nettsProviderName
}

func isTronEnergyDefinitelyRejectedError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(errText, "receiver address is not activated") ||
		strings.Contains(errText, "接收地址未激活") ||
		strings.Contains(errText, "insufficient available balance") ||
		strings.Contains(errText, "insufficient_balance") ||
		strings.Contains(errText, "可用余额不足") ||
		strings.Contains(errText, "delegable energy limit") ||
		strings.Contains(errText, "capacity") ||
		strings.Contains(errText, "下单笔数超过平台当前可委托能量上限") ||
		strings.Contains(errText, "unsupported media type") ||
		strings.Contains(errText, "duplicate order")
}

func isTronEnergyFallbackCandidateError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(errText, "http 429") ||
		strings.Contains(errText, "too many requests") ||
		strings.Contains(errText, "rate limit") ||
		strings.Contains(errText, "ip_not_allowed") ||
		strings.Contains(errText, "unavailable") ||
		strings.Contains(errText, "capacity") ||
		strings.Contains(errText, "delegable energy limit") ||
		strings.Contains(errText, "稍后重试") ||
		strings.Contains(errText, "下单笔数超过平台当前可委托能量上限") ||
		strings.Contains(errText, "insufficient available balance") ||
		strings.Contains(errText, "insufficient_balance") ||
		strings.Contains(errText, "可用余额不足")
}
