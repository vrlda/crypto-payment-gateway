package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTRXForEnergyProviderCreateEnergyOrder(t *testing.T) {
	t.Parallel()

	var capturedContentType string
	var capturedAPIKey string
	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/times" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		capturedContentType = r.Header.Get("Content-Type")
		capturedAPIKey = r.Header.Get("x-api-key")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if err := json.Unmarshal(body, &capturedBody); err != nil {
			t.Fatalf("request body is not json: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","order":{"id":11,"status":0,"times":1,"receiverAddress":"TReceiver","orderNo":"SWEEP-1","orderTime":3600,"timeStamp":1759505787190},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})

	order, err := provider.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{
		ReceiverAddress: "TReceiver",
		OrderNo:         "SWEEP-1",
		TransferCount:   1,
		DurationHours:   1,
	})
	if err != nil {
		t.Fatalf("CreateEnergyOrder() error = %v", err)
	}

	if capturedAPIKey != "test-key" {
		t.Fatalf("x-api-key = %q, want %q", capturedAPIKey, "test-key")
	}
	if capturedContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", capturedContentType)
	}
	if got := capturedBody["receiverAddress"]; got != "TReceiver" {
		t.Fatalf("receiverAddress = %#v, want %q", got, "TReceiver")
	}
	if got := capturedBody["orderNo"]; got != "SWEEP-1" {
		t.Fatalf("orderNo = %#v, want %q", got, "SWEEP-1")
	}
	if got := capturedBody["energyType"]; got != "activated" {
		t.Fatalf("energyType = %#v, want %q", got, "activated")
	}
	if got := capturedBody["durationHours"]; got != float64(1) {
		t.Fatalf("durationHours = %#v, want %v", got, 1)
	}
	if got := capturedBody["times"]; got != float64(1) {
		t.Fatalf("times = %#v, want %v", got, 1)
	}
	if order.ProviderOrderID != "11" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "11")
	}
	if order.ProviderOrderNo != "SWEEP-1" {
		t.Fatalf("ProviderOrderNo = %q, want %q", order.ProviderOrderNo, "SWEEP-1")
	}
	if order.NormalizedStatus != TronEnergyRentalStatusPending {
		t.Fatalf("NormalizedStatus = %q, want %q", order.NormalizedStatus, TronEnergyRentalStatusPending)
	}
}

func TestTRXForEnergyProviderCreateEnergyOrderHandlesProviderErrorString(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ERROR: unauthorized"))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("bad-key", server.URL, 1, &http.Client{Timeout: time.Second})
	_, err := provider.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{
		ReceiverAddress: "TReceiver",
		OrderNo:         "SWEEP-1",
		TransferCount:   1,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error = %v, want provider error string", err)
	}
}

func TestTRXForEnergyProviderGetOrderNormalizesNegativeStatusToFailed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/11" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","order":{"id":11,"status":-1,"times":1,"receiverAddress":"TReceiver","orderNo":"SWEEP-1","orderTime":3600,"timeStamp":1759505787190},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})

	order, err := provider.GetOrder(context.Background(), "11")
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	if order.NormalizedStatus != TronEnergyRentalStatusFailed {
		t.Fatalf("NormalizedStatus = %q, want %q", order.NormalizedStatus, TronEnergyRentalStatusFailed)
	}
}

func TestTRXForEnergyProviderAcceptsStringTimestamp(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","order":{"id":22,"status":1,"times":1,"receiverAddress":"TReceiver","orderNo":"SWEEP-2","orderTime":3600,"timeStamp":"1759505787190"},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})

	order, err := provider.GetOrder(context.Background(), "22")
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	if order.ProviderOrderID != "22" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "22")
	}
	if order.NormalizedStatus != TronEnergyRentalStatusActive {
		t.Fatalf("NormalizedStatus = %q, want %q", order.NormalizedStatus, TronEnergyRentalStatusActive)
	}
}

func TestTRXForEnergyProviderReturnsHTTPStatusErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})

	_, err := provider.GetOrder(context.Background(), "22")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("error = %v, want HTTP 429 detail", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v, want response body detail", err)
	}
}

func TestTRXForEnergyProviderReturnsEnvelopeErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"ERROR","order":{"id":0,"status":-1},"reason":"INVALID_REQUEST","message":"bad payload"}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})

	_, err := provider.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{
		ReceiverAddress: "TReceiver",
		OrderNo:         "SWEEP-1",
		TransferCount:   1,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad payload") {
		t.Fatalf("error = %v, want envelope message", err)
	}
}

func TestTRXForEnergyProviderGetAccountInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/myInfo" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","account":{"avaliableBalance":1000000,"addressForDeposit":"TA2KiBYtPcQ2SK2cZjs6WNf4Q9g57xMV6r"},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})
	info, err := provider.GetAccountInfo(context.Background())
	if err != nil {
		t.Fatalf("GetAccountInfo() error = %v", err)
	}
	if info.AvailableBalanceSun != 1_000_000 {
		t.Fatalf("AvailableBalanceSun = %d, want %d", info.AvailableBalanceSun, int64(1_000_000))
	}
	if info.DepositAddress != "TA2KiBYtPcQ2SK2cZjs6WNf4Q9g57xMV6r" {
		t.Fatalf("DepositAddress = %q", info.DepositAddress)
	}
}

func TestTRXForEnergyProviderGetPriceInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/getPrice" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","price":{"ENERGY_ACTIVATED":1500000,"USDT_TRANSFER_ENERGY_AMOUNT":64285,"orderDurationTiers":[{"durationHours":1,"multiplier":1.0},{"durationHours":4,"multiplier":1.2}]},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})
	info, err := provider.GetPriceInfo(context.Background())
	if err != nil {
		t.Fatalf("GetPriceInfo() error = %v", err)
	}
	if info.ActivatedPriceSun != 1_500_000 {
		t.Fatalf("ActivatedPriceSun = %d, want %d", info.ActivatedPriceSun, int64(1_500_000))
	}
	if len(info.DurationTiers) != 2 {
		t.Fatalf("DurationTiers len = %d, want 2", len(info.DurationTiers))
	}
}

func TestTRXForEnergyProviderGetOrderByOrderNo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/getByOrderNo" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("orderNo"); got != "SWEEP-1" {
			t.Fatalf("orderNo query = %q, want %q", got, "SWEEP-1")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"SUCCESS","order":{"id":99,"status":0,"times":1,"receiverAddress":"TReceiver","orderNo":"SWEEP-1","orderTime":3600,"timeStamp":1759505787190},"reason":null,"message":null}`))
	}))
	defer server.Close()

	provider := NewTRXForEnergyProvider("test-key", server.URL, 1, &http.Client{Timeout: time.Second})
	order, err := provider.GetOrderByOrderNo(context.Background(), "SWEEP-1")
	if err != nil {
		t.Fatalf("GetOrderByOrderNo() error = %v", err)
	}
	if order.ProviderOrderID != "99" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "99")
	}
}

func TestIsTronEnergyReceiverNotActivatedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "english provider error", err: context.DeadlineExceeded, want: false},
		{name: "english activated error", err: io.EOF, want: false},
		{name: "custom english", err: http.ErrNotSupported, want: false},
		{name: "translated english", err: &tronEnergyTestError{msg: "provider error: Receiver address is not activated"}, want: true},
		{name: "translated chinese", err: &tronEnergyTestError{msg: "provider error: 接收地址未激活"}, want: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTronEnergyReceiverNotActivatedError(tt.err); got != tt.want {
				t.Fatalf("isTronEnergyReceiverNotActivatedError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsTronEnergyDuplicateOrderError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "english duplicate", err: &tronEnergyTestError{msg: "provider error: Duplicate orderNo SWEEP-1"}, want: true},
		{name: "reason code duplicate", err: &tronEnergyTestError{msg: "provider error: DUPLICATE_ORDER_NO"}, want: true},
		{name: "chinese duplicate", err: &tronEnergyTestError{msg: "provider error: 重复下单 OrderNo SWEEP-1"}, want: true},
		{name: "other error", err: &tronEnergyTestError{msg: "provider error: INSUFFICIENT_BALANCE"}, want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTronEnergyDuplicateOrderError(tt.err); got != tt.want {
				t.Fatalf("isTronEnergyDuplicateOrderError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type tronEnergyTestError struct{ msg string }

func (e *tronEnergyTestError) Error() string { return e.msg }
