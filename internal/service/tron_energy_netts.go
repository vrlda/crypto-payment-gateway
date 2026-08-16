package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const nettsProviderName = "netts"

var nettsCountPricePattern = regexp.MustCompile(`(\d+)-([0-9]+(?:\.[0-9]+)?)\s*TRX`)

type nettsUserInfoResponse struct {
	Status   string `json:"status"`
	Code     int    `json:"code"`
	Message  string `json:"message"`
	UserInfo struct {
		DepositAddress string `json:"deposit_address"`
	} `json:"user_info"`
	Stats struct {
		Balance float64 `json:"balance"`
	} `json:"stats"`
}

type nettsOrder1HRequest struct {
	Amount         int64  `json:"amount"`
	ReceiveAddress string `json:"receiveAddress"`
}

type nettsOrder1HResponse struct {
	Detail struct {
		Data struct {
			OrderID         string      `json:"orderId"`
			Hash            string      `json:"hash"`
			Energy          int64       `json:"energy"`
			PaidTRX         interface{} `json:"paidTRX"`
			DelegateAddress string      `json:"delegateAddress"`
			Status          string      `json:"status"`
		} `json:"data"`
		Error string `json:"error"`
	} `json:"detail"`
}

type nettsOrderCheckResponse struct {
	Success bool   `json:"success"`
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	Order   struct {
		ID            string   `json:"id"`
		Cost          float64  `json:"cost"`
		TargetAddress string   `json:"target_address"`
		Energy        int64    `json:"energy"`
		Duration      string   `json:"duration"`
		TxHashes      []string `json:"tx_hashes"`
		Timestamp     string   `json:"timestamp"`
		Comments      string   `json:"comments"`
	} `json:"order"`
	Activation *struct {
		Cost   float64 `json:"cost"`
		TxHash string  `json:"tx_hash"`
	} `json:"activation"`
}

type NettsProvider struct {
	apiKey     string
	baseURL    string
	realIP     string
	httpClient *http.Client
}

func NewNettsProvider(apiKey, baseURL, realIP string, httpClient *http.Client) *NettsProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://netts.io"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &NettsProvider{
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    baseURL,
		realIP:     strings.TrimSpace(realIP),
		httpClient: httpClient,
	}
}

func (p *NettsProvider) IsConfigured() bool {
	return strings.TrimSpace(p.apiKey) != "" && strings.TrimSpace(p.realIP) != ""
}

func (p *NettsProvider) providerRequest(ctx context.Context, method, path string, headers map[string]string, body []byte) ([]byte, int, error) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}

func (p *NettsProvider) authHeaders() map[string]string {
	return map[string]string{
		"X-API-KEY":    p.apiKey,
		"X-Real-IP":    p.realIP,
		"Content-Type": "application/json",
	}
}

func (p *NettsProvider) CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
	payload, err := json.Marshal(nettsOrder1HRequest{
		Amount:         tronEnergyOrderEnergyAmount(req.TransferCount),
		ReceiveAddress: strings.TrimSpace(req.ReceiverAddress),
	})
	if err != nil {
		return nil, err
	}

	raw, status, err := p.providerRequest(ctx, http.MethodPost, "/apiv2/order1h", p.authHeaders(), payload)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nettsProviderError(status, raw)
	}

	var resp nettsOrder1HResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode Netts order1h response: %w", err)
	}
	if msg := strings.TrimSpace(resp.Detail.Error); msg != "" {
		return nil, fmt.Errorf("provider error: %s", msg)
	}

	orderID := strings.TrimSpace(resp.Detail.Data.OrderID)
	if orderID == "" {
		return nil, fmt.Errorf("Netts order response missing orderId")
	}

	orderStatus := strings.TrimSpace(resp.Detail.Data.Status)
	return &TronEnergyOrder{
		ProviderName:     nettsProviderName,
		ProviderOrderID:  orderID,
		ProviderOrderNo:  orderID,
		ProviderStatus:   orderStatus,
		NormalizedStatus: normalizeNettsOrderStatus(orderStatus),
		MetadataJSON:     string(raw),
	}, nil
}

func (p *NettsProvider) GetOrder(ctx context.Context, providerOrderID string) (*TronEnergyOrder, error) {
	headers := map[string]string{
		"X-API-KEY": p.apiKey,
	}
	raw, status, err := p.providerRequest(ctx, http.MethodGet, "/apiv2/order_check?order_id="+strings.TrimSpace(providerOrderID), headers, nil)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nettsProviderError(status, raw)
	}

	var resp nettsOrderCheckResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode Netts order_check response: %w", err)
	}
	if !resp.Success && strings.TrimSpace(resp.Msg) != "" {
		return nil, fmt.Errorf("provider error: %s", resp.Msg)
	}
	if strings.TrimSpace(resp.Order.ID) == "" {
		return nil, fmt.Errorf("Netts order response missing id")
	}

	statusText := "processing"
	if len(resp.Order.TxHashes) > 0 {
		statusText = "confirmed"
	}
	return &TronEnergyOrder{
		ProviderName:     nettsProviderName,
		ProviderOrderID:  strings.TrimSpace(resp.Order.ID),
		ProviderOrderNo:  strings.TrimSpace(resp.Order.ID),
		ProviderStatus:   statusText,
		NormalizedStatus: normalizeNettsOrderStatus(statusText),
		MetadataJSON:     string(raw),
	}, nil
}

func (p *NettsProvider) GetOrderByOrderNo(ctx context.Context, orderNo string) (*TronEnergyOrder, error) {
	return nil, fmt.Errorf("Netts does not support provider order lookup by client orderNo")
}

func (p *NettsProvider) GetAccountInfo(ctx context.Context) (*TronEnergyAccountInfo, error) {
	headers := map[string]string{
		"X-API-KEY": p.apiKey,
		"X-Real-IP": p.realIP,
	}
	raw, status, err := p.providerRequest(ctx, http.MethodGet, "/apiv2/userinfo", headers, nil)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nettsProviderError(status, raw)
	}

	var resp nettsUserInfoResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode Netts userinfo response: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(resp.Status), "error") && strings.TrimSpace(resp.Message) != "" {
		return nil, fmt.Errorf("provider error: %s", strings.TrimSpace(resp.Message))
	}

	balanceSun, err := decimal.NewFromString(strconv.FormatFloat(resp.Stats.Balance, 'f', -1, 64))
	if err != nil {
		return nil, fmt.Errorf("invalid Netts balance: %w", err)
	}
	balanceSun = balanceSun.Shift(6)

	return &TronEnergyAccountInfo{
		AvailableBalanceSun: balanceSun.IntPart(),
		DepositAddress:      strings.TrimSpace(resp.UserInfo.DepositAddress),
	}, nil
}

func (p *NettsProvider) GetPriceInfo(ctx context.Context) (*TronEnergyPriceInfo, error) {
	headers := map[string]string{
		"X-API-KEY": p.apiKey,
		"X-Real-IP": p.realIP,
		"X-Format":  "count",
	}
	raw, status, err := p.providerRequest(ctx, http.MethodGet, "/apiv2/prices", headers, nil)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nettsProviderError(status, raw)
	}

	countPricing := parseNettsCountPricing(string(raw))
	firstOrderCost, ok := countPricing[1]
	if !ok || !firstOrderCost.GreaterThan(decimal.Zero) {
		return nil, fmt.Errorf("Netts pricing response missing 1-order price")
	}

	return &TronEnergyPriceInfo{
		ActivatedPriceSun: firstOrderCost.Shift(6).IntPart(),
		ActivatedEnergy:   65_000,
		DurationTiers: []TronEnergyPriceTier{
			{DurationHours: 1, Multiplier: 1.0},
		},
	}, nil
}

func parseNettsCountPricing(raw string) map[int]decimal.Decimal {
	matches := nettsCountPricePattern.FindAllStringSubmatch(raw, -1)
	out := make(map[int]decimal.Decimal, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		count, err := strconv.Atoi(strings.TrimSpace(match[1]))
		if err != nil || count <= 0 {
			continue
		}
		cost, err := decimal.NewFromString(strings.TrimSpace(match[2]))
		if err != nil {
			continue
		}
		out[count] = cost
	}
	return out
}

func nettsProviderError(status int, payload []byte) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return fmt.Errorf("provider returned HTTP %d", status)
	}

	var generic map[string]any
	if err := json.Unmarshal(payload, &generic); err == nil {
		for _, key := range []string{"msg", "message", "detail"} {
			if value := strings.TrimSpace(stringValue(generic[key])); value != "" {
				return fmt.Errorf("provider error: %s", value)
			}
		}
	}
	return fmt.Errorf("provider returned HTTP %d: %s", status, trimmed)
}

func normalizeNettsOrderStatus(status string) TronEnergyRentalStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "confirmed", "success", "done":
		return TronEnergyRentalStatusActive
	case "pending", "processing", "queued":
		return TronEnergyRentalStatusPending
	case "failed", "error", "cancelled":
		return TronEnergyRentalStatusFailed
	default:
		return TronEnergyRentalStatusUnknown
	}
}
