package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const trxForEnergyProviderName = "trx_for_energy"

type trxForEnergyFlexibleInt64 int64

func (v *trxForEnergyFlexibleInt64) UnmarshalJSON(data []byte) error {
	var asInt int64
	if err := json.Unmarshal(data, &asInt); err == nil {
		*v = trxForEnergyFlexibleInt64(asInt)
		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			*v = 0
			return nil
		}
		parsed, err := strconv.ParseInt(asString, 10, 64)
		if err != nil {
			return err
		}
		*v = trxForEnergyFlexibleInt64(parsed)
		return nil
	}

	return fmt.Errorf("unsupported int64 payload: %s", string(data))
}

type trxForEnergyOrderResponse struct {
	ID              int64                     `json:"id"`
	OrderTime       int64                     `json:"orderTime"`
	OrderType       int                       `json:"orderType"`
	ReceiverAddress string                    `json:"receiverAddress"`
	ResourceType    int                       `json:"resourceType"`
	Source          string                    `json:"source"`
	Status          int                       `json:"status"`
	TimeStamp       trxForEnergyFlexibleInt64 `json:"timeStamp"`
	Times           int                       `json:"times"`
	OrderNo         string                    `json:"orderNo"`
	FailReason      string                    `json:"failReason"`
	UpdatedAt       string                    `json:"updatedAt"`
}

type trxForEnergyEnvelope struct {
	Result  string                     `json:"result"`
	Order   *trxForEnergyOrderResponse `json:"order"`
	Reason  string                     `json:"reason"`
	Message string                     `json:"message"`
}

type trxForEnergyAccountResponse struct {
	ID               int64  `json:"id"`
	Balance          int64  `json:"balance"`
	AvailableBalance int64  `json:"avaliableBalance"`
	DepositAddress   string `json:"addressForDeposit"`
}

type trxForEnergyMyInfoEnvelope struct {
	Result  string                       `json:"result"`
	Account *trxForEnergyAccountResponse `json:"account"`
	Reason  string                       `json:"reason"`
	Message string                       `json:"message"`
}

type trxForEnergyDurationTierResponse struct {
	DurationHours int     `json:"durationHours"`
	Multiplier    float64 `json:"multiplier"`
}

type trxForEnergyPriceResponse struct {
	ActivatedPriceSun int64                              `json:"ENERGY_ACTIVATED"`
	ActivatedEnergy   int64                              `json:"USDT_TRANSFER_ENERGY_AMOUNT"`
	DurationTiers     []trxForEnergyDurationTierResponse `json:"orderDurationTiers"`
}

type trxForEnergyPriceEnvelope struct {
	Result  string                     `json:"result"`
	Price   *trxForEnergyPriceResponse `json:"price"`
	Reason  string                     `json:"reason"`
	Message string                     `json:"message"`
}

type TRXForEnergyProvider struct {
	apiKey       string
	baseURL      string
	defaultTimes int
	httpClient   *http.Client
}

func NewTRXForEnergyProvider(apiKey, baseURL string, defaultTimes int, httpClient *http.Client) *TRXForEnergyProvider {
	if defaultTimes <= 0 {
		defaultTimes = 1
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://trxforenergy.com/test/api"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &TRXForEnergyProvider{
		apiKey:       strings.TrimSpace(apiKey),
		baseURL:      baseURL,
		defaultTimes: defaultTimes,
		httpClient:   httpClient,
	}
}

func (p *TRXForEnergyProvider) IsConfigured() bool {
	return strings.TrimSpace(p.apiKey) != ""
}

func (p *TRXForEnergyProvider) CreateEnergyOrder(ctx context.Context, req TronEnergyOrderRequest) (*TronEnergyOrder, error) {
	transferCount := req.TransferCount
	if transferCount <= 0 {
		transferCount = p.defaultTimes
	}
	durationHours := req.DurationHours
	if durationHours <= 0 {
		durationHours = 4
	}

	requestBody := map[string]any{
		"times":           transferCount,
		"receiverAddress": strings.TrimSpace(req.ReceiverAddress),
		"energyType":      "activated",
		"durationHours":   durationHours,
	}
	if strings.TrimSpace(req.OrderNo) != "" {
		requestBody["orderNo"] = strings.TrimSpace(req.OrderNo)
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/orders/times", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return p.decodeOrderResponse(resp.StatusCode, resp.Body)
}

func (p *TRXForEnergyProvider) GetOrder(ctx context.Context, providerOrderID string) (*TronEnergyOrder, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/orders/"+providerOrderID, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return p.decodeOrderResponse(resp.StatusCode, resp.Body)
}

func (p *TRXForEnergyProvider) GetOrderByOrderNo(ctx context.Context, orderNo string) (*TronEnergyOrder, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/orders/getByOrderNo?orderNo="+url.QueryEscape(strings.TrimSpace(orderNo)), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return p.decodeOrderResponse(resp.StatusCode, resp.Body)
}

func (p *TRXForEnergyProvider) GetAccountInfo(ctx context.Context) (*TronEnergyAccountInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/myInfo", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(payload))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, trimmed)
	}

	var envelope trxForEnergyMyInfoEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode TRX For Energy myInfo response: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Result), "ERROR") {
		if msg := strings.TrimSpace(envelope.Message); msg != "" {
			return nil, fmt.Errorf("provider error: %s", msg)
		}
		if reason := strings.TrimSpace(envelope.Reason); reason != "" {
			return nil, fmt.Errorf("provider error: %s", reason)
		}
		return nil, fmt.Errorf("provider error: result=ERROR")
	}
	if envelope.Account == nil {
		return nil, fmt.Errorf("provider response missing account payload")
	}

	return &TronEnergyAccountInfo{
		AvailableBalanceSun: envelope.Account.AvailableBalance,
		DepositAddress:      strings.TrimSpace(envelope.Account.DepositAddress),
	}, nil
}

func (p *TRXForEnergyProvider) GetPriceInfo(ctx context.Context) (*TronEnergyPriceInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/getPrice", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(payload))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, trimmed)
	}

	var envelope trxForEnergyPriceEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode TRX For Energy getPrice response: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Result), "ERROR") {
		if msg := strings.TrimSpace(envelope.Message); msg != "" {
			return nil, fmt.Errorf("provider error: %s", msg)
		}
		if reason := strings.TrimSpace(envelope.Reason); reason != "" {
			return nil, fmt.Errorf("provider error: %s", reason)
		}
		return nil, fmt.Errorf("provider error: result=ERROR")
	}
	if envelope.Price == nil {
		return nil, fmt.Errorf("provider response missing price payload")
	}

	tiers := make([]TronEnergyPriceTier, 0, len(envelope.Price.DurationTiers))
	for _, tier := range envelope.Price.DurationTiers {
		tiers = append(tiers, TronEnergyPriceTier{
			DurationHours: tier.DurationHours,
			Multiplier:    tier.Multiplier,
		})
	}

	return &TronEnergyPriceInfo{
		ActivatedPriceSun: envelope.Price.ActivatedPriceSun,
		ActivatedEnergy:   envelope.Price.ActivatedEnergy,
		DurationTiers:     tiers,
	}, nil
}

func (p *TRXForEnergyProvider) decodeOrderResponse(statusCode int, body io.Reader) (*TronEnergyOrder, error) {
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("provider returned HTTP %d with empty body", statusCode)
		}
		return nil, fmt.Errorf("empty provider response")
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider returned HTTP %d: %s", statusCode, trimmed)
	}
	if strings.HasPrefix(trimmed, "ERROR:") {
		return nil, fmt.Errorf("provider error: %s", strings.TrimSpace(strings.TrimPrefix(trimmed, "ERROR:")))
	}

	var envelope trxForEnergyEnvelope
	if err := json.Unmarshal(payload, &envelope); err == nil && (envelope.Order != nil || strings.TrimSpace(envelope.Result) != "") {
		if strings.EqualFold(strings.TrimSpace(envelope.Result), "ERROR") {
			if msg := strings.TrimSpace(envelope.Message); msg != "" {
				return nil, fmt.Errorf("provider error: %s", msg)
			}
			if reason := strings.TrimSpace(envelope.Reason); reason != "" {
				return nil, fmt.Errorf("provider error: %s", reason)
			}
			return nil, fmt.Errorf("provider error: result=ERROR")
		}
		if envelope.Order == nil {
			return nil, fmt.Errorf("provider response missing order payload")
		}
		return p.normalizeOrderPayload(payload, envelope.Order), nil
	}

	var raw trxForEnergyOrderResponse
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("decode TRX For Energy response: %w", err)
	}

	return p.normalizeOrderPayload(payload, &raw), nil
}

func (p *TRXForEnergyProvider) normalizeOrderPayload(payload []byte, raw *trxForEnergyOrderResponse) *TronEnergyOrder {
	return &TronEnergyOrder{
		ProviderName:     trxForEnergyProviderName,
		ProviderOrderID:  strconv.FormatInt(raw.ID, 10),
		ProviderOrderNo:  raw.OrderNo,
		ProviderStatus:   strconv.Itoa(raw.Status),
		NormalizedStatus: normalizeTRXForEnergyStatus(raw.Status),
		MetadataJSON:     string(payload),
	}
}

func normalizeTRXForEnergyStatus(status int) TronEnergyRentalStatus {
	switch {
	case status < 0:
		return TronEnergyRentalStatusFailed
	case status == 0:
		return TronEnergyRentalStatusPending
	case status > 0:
		return TronEnergyRentalStatusActive
	default:
		return TronEnergyRentalStatusUnknown
	}
}

func isTronEnergyReceiverNotActivatedError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(errText, "接收地址未激活") ||
		strings.Contains(errText, "receiver address is not activated") ||
		strings.Contains(errText, "address is not activated")
}

func isTronEnergyDuplicateOrderError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(errText, "duplicate orderno") ||
		strings.Contains(errText, "duplicate_order_no") ||
		strings.Contains(errText, "重复下单")
}
