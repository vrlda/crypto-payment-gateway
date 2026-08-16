package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const tronRentalProviderName = "tronrental"

type tronRentalPriceResponse struct {
	EnergyTRX    map[string]string `json:"energy_trx"`
	EnergyVolume int64             `json:"energy_volume"`
}

type tronRentalBuyEnergyRequest struct {
	Address       string `json:"address"`
	EnergyAmount  int64  `json:"energy_amount"`
	DurationHours int    `json:"duration_hours"`
}

type tronRentalOrderResponse struct {
	OrderID       json.Number `json:"order_id"`
	Address       string      `json:"address"`
	EnergyAmount  int64       `json:"energy_amount"`
	DurationHours int         `json:"duration_hours"`
	PriceTRX      string      `json:"price_trx"`
	Status        string      `json:"status"`
}

type tronRentalDepositResponse struct {
	DepositAddress string `json:"deposit_address"`
}

type tronRentalBalanceResponse struct {
	BalanceTRX string      `json:"balance_trx"`
	Balance    string      `json:"balance"`
	Available  string      `json:"available_balance"`
	AmountTRX  string      `json:"amount_trx"`
	Raw        interface{} `json:"-"`
}

type TronRentalProvider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewTronRentalProvider(apiKey, baseURL string, httpClient *http.Client) *TronRentalProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.tronrental.com/v1"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &TronRentalProvider{
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

func (p *TronRentalProvider) IsConfigured() bool {
	return strings.TrimSpace(p.apiKey) != ""
}

func (p *TronRentalProvider) providerRequest(ctx context.Context, method, path string, body []byte, authRequired bool) ([]byte, error) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	if authRequired {
		req.Header.Set("X-API-Key", p.apiKey)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		trimmed := strings.TrimSpace(string(payload))
		if trimmed == "" {
			return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, trimmed)
	}
	return payload, nil
}

func (p *TronRentalProvider) CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
	durationHours := req.DurationHours
	if durationHours <= 0 {
		durationHours = tronEnergyOrderDurationHours()
	}
	payload, err := json.Marshal(tronRentalBuyEnergyRequest{
		Address:       strings.TrimSpace(req.ReceiverAddress),
		EnergyAmount:  tronEnergyOrderEnergyAmount(req.TransferCount),
		DurationHours: durationHours,
	})
	if err != nil {
		return nil, err
	}

	raw, err := p.providerRequest(ctx, http.MethodPost, "/energy/buy", payload, true)
	if err != nil {
		return nil, err
	}

	var order tronRentalOrderResponse
	if err := json.Unmarshal(raw, &order); err != nil {
		return nil, fmt.Errorf("decode TronRental buy energy response: %w", err)
	}

	return &TronEnergyOrder{
		ProviderName:     tronRentalProviderName,
		ProviderOrderID:  order.OrderID.String(),
		ProviderOrderNo:  strings.TrimSpace(req.OrderNo),
		ProviderStatus:   strings.TrimSpace(order.Status),
		NormalizedStatus: normalizeTronRentalOrderStatus(order.Status),
		MetadataJSON:     string(raw),
	}, nil
}

func (p *TronRentalProvider) GetOrder(ctx context.Context, providerOrderID string) (*TronEnergyOrder, error) {
	raw, err := p.providerRequest(ctx, http.MethodGet, "/orders/"+strings.TrimSpace(providerOrderID), nil, true)
	if err != nil {
		return nil, err
	}
	return p.decodeOrderResponse(raw)
}

func (p *TronRentalProvider) GetOrderByOrderNo(ctx context.Context, orderNo string) (*TronEnergyOrder, error) {
	raw, err := p.providerRequest(ctx, http.MethodGet, "/orders", nil, true)
	if err != nil {
		return nil, err
	}

	var orders []map[string]any
	if err := json.Unmarshal(raw, &orders); err != nil {
		return nil, fmt.Errorf("decode TronRental orders response: %w", err)
	}
	target := strings.TrimSpace(orderNo)
	for _, order := range orders {
		if strings.TrimSpace(stringValue(order["client_order_no"])) == target ||
			strings.TrimSpace(stringValue(order["order_no"])) == target ||
			strings.TrimSpace(stringValue(order["metadata_order_no"])) == target {
			single, err := json.Marshal(order)
			if err != nil {
				return nil, err
			}
			return p.decodeOrderResponse(single)
		}
	}
	return nil, fmt.Errorf("provider order not found for orderNo %s", target)
}

func (p *TronRentalProvider) GetAccountInfo(ctx context.Context) (*TronEnergyAccountInfo, error) {
	balanceRaw, err := p.providerRequest(ctx, http.MethodGet, "/account/balance", nil, true)
	if err != nil {
		return nil, err
	}
	depositRaw, err := p.providerRequest(ctx, http.MethodGet, "/account/deposit", nil, true)
	if err != nil {
		return nil, err
	}

	var deposit tronRentalDepositResponse
	if err := json.Unmarshal(depositRaw, &deposit); err != nil {
		return nil, fmt.Errorf("decode TronRental deposit response: %w", err)
	}

	balanceSun, err := tronRentalBalanceSun(balanceRaw)
	if err != nil {
		return nil, err
	}

	return &TronEnergyAccountInfo{
		AvailableBalanceSun: balanceSun,
		DepositAddress:      strings.TrimSpace(deposit.DepositAddress),
	}, nil
}

func (p *TronRentalProvider) GetPriceInfo(ctx context.Context) (*TronEnergyPriceInfo, error) {
	raw, err := p.providerRequest(ctx, http.MethodGet, "/prices", nil, false)
	if err != nil {
		return nil, err
	}

	var prices tronRentalPriceResponse
	if err := json.Unmarshal(raw, &prices); err != nil {
		return nil, fmt.Errorf("decode TronRental price response: %w", err)
	}

	priceTRX, ok := prices.EnergyTRX["1h"]
	if !ok {
		return nil, fmt.Errorf("TronRental price response missing 1h energy price")
	}
	priceDec, err := decimal.NewFromString(strings.TrimSpace(priceTRX))
	if err != nil {
		return nil, fmt.Errorf("invalid TronRental 1h energy price: %w", err)
	}

	tiers := make([]TronEnergyPriceTier, 0, len(prices.EnergyTRX))
	for period, trxString := range prices.EnergyTRX {
		hours, ok := tronRentalPeriodToHours(period)
		if !ok {
			continue
		}
		trxValue, err := decimal.NewFromString(strings.TrimSpace(trxString))
		if err != nil || !trxValue.GreaterThan(decimal.Zero) {
			continue
		}
		tiers = append(tiers, TronEnergyPriceTier{
			DurationHours: hours,
			Multiplier:    trxValue.Div(priceDec).InexactFloat64(),
		})
	}

	return &TronEnergyPriceInfo{
		ActivatedPriceSun: priceDec.Shift(6).IntPart(),
		ActivatedEnergy:   prices.EnergyVolume,
		DurationTiers:     tiers,
	}, nil
}

func (p *TronRentalProvider) decodeOrderResponse(raw []byte) (*TronEnergyOrder, error) {
	var orderMap map[string]any
	if err := json.Unmarshal(raw, &orderMap); err != nil {
		return nil, fmt.Errorf("decode TronRental order response: %w", err)
	}

	orderID := strings.TrimSpace(stringValue(orderMap["order_id"]))
	if orderID == "" {
		orderID = strings.TrimSpace(stringValue(orderMap["id"]))
	}
	status := strings.TrimSpace(stringValue(orderMap["status"]))
	if status == "" {
		status = strings.TrimSpace(stringValue(orderMap["state"]))
	}
	orderNo := strings.TrimSpace(stringValue(orderMap["client_order_no"]))
	if orderNo == "" {
		orderNo = strings.TrimSpace(stringValue(orderMap["order_no"]))
	}

	return &TronEnergyOrder{
		ProviderName:     tronRentalProviderName,
		ProviderOrderID:  orderID,
		ProviderOrderNo:  orderNo,
		ProviderStatus:   status,
		NormalizedStatus: normalizeTronRentalOrderStatus(status),
		MetadataJSON:     string(raw),
	}, nil
}

func tronRentalBalanceSun(raw []byte) (int64, error) {
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		return 0, fmt.Errorf("decode TronRental balance response: %w", err)
	}

	candidates := []string{
		stringValue(rawMap["available_balance"]),
		stringValue(rawMap["balance_trx"]),
		stringValue(rawMap["balance"]),
		stringValue(rawMap["amount_trx"]),
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		dec, err := decimal.NewFromString(candidate)
		if err != nil {
			continue
		}
		return dec.Shift(6).IntPart(), nil
	}
	return 0, fmt.Errorf("TronRental balance response did not contain a parseable TRX balance")
}

func tronRentalPeriodToHours(period string) (int, bool) {
	switch strings.TrimSpace(strings.ToLower(period)) {
	case "1h":
		return 1, true
	case "1d":
		return 24, true
	case "3d":
		return 72, true
	case "30d":
		return 720, true
	default:
		return 0, false
	}
}

func normalizeTronRentalOrderStatus(status string) TronEnergyRentalStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "queued", "processing":
		return TronEnergyRentalStatusPending
	case "completed", "confirmed", "active", "success", "delegated":
		return TronEnergyRentalStatusActive
	case "failed", "cancelled", "expired", "error":
		return TronEnergyRentalStatusFailed
	default:
		return TronEnergyRentalStatusUnknown
	}
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}
