package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNettsProviderCreateEnergyOrder(t *testing.T) {
	t.Parallel()

	var capturedHeaders http.Header
	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apiv2/order1h" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		capturedHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if err := json.Unmarshal(body, &capturedBody); err != nil {
			t.Fatalf("request body is not json: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detail":{"data":{"orderId":"739","status":"processing"}}}`))
	}))
	defer server.Close()

	provider := NewNettsProvider("test-key", server.URL, "77.239.99.245", &http.Client{Timeout: time.Second})

	order, err := provider.CreateEnergyOrder(context.Background(), TronEnergyOrderRequest{
		ReceiverAddress: "TReceiver",
		TransferCount:   1,
		DurationHours:   1,
	})
	if err != nil {
		t.Fatalf("CreateEnergyOrder() error = %v", err)
	}

	if capturedHeaders.Get("X-API-KEY") != "test-key" {
		t.Fatalf("X-API-KEY = %q, want %q", capturedHeaders.Get("X-API-KEY"), "test-key")
	}
	if capturedHeaders.Get("X-Real-IP") != "77.239.99.245" {
		t.Fatalf("X-Real-IP = %q, want %q", capturedHeaders.Get("X-Real-IP"), "77.239.99.245")
	}
	if capturedHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", capturedHeaders.Get("Content-Type"))
	}
	if got := capturedBody["amount"]; got != float64(65000) {
		t.Fatalf("amount = %#v, want %v", got, 65000)
	}
	if got := capturedBody["receiveAddress"]; got != "TReceiver" {
		t.Fatalf("receiveAddress = %#v, want %q", got, "TReceiver")
	}
	if order.ProviderName != nettsProviderName {
		t.Fatalf("ProviderName = %q, want %q", order.ProviderName, nettsProviderName)
	}
	if order.ProviderOrderID != "739" {
		t.Fatalf("ProviderOrderID = %q, want %q", order.ProviderOrderID, "739")
	}
	if order.NormalizedStatus != TronEnergyRentalStatusPending {
		t.Fatalf("NormalizedStatus = %q, want %q", order.NormalizedStatus, TronEnergyRentalStatusPending)
	}
}

func TestNettsProviderGetAccountInfoAndPriceInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apiv2/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","user_info":{"deposit_address":"TDeposit"},"stats":{"balance":24.795}}`))
		case "/apiv2/prices":
			if got := r.Header.Get("X-Format"); got != "count" {
				t.Fatalf("X-Format = %q, want %q", got, "count")
			}
			_, _ = w.Write([]byte("1-2.405 TRX\n10-24.05 TRX"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := NewNettsProvider("test-key", server.URL, "77.239.99.245", &http.Client{Timeout: time.Second})

	account, err := provider.GetAccountInfo(context.Background())
	if err != nil {
		t.Fatalf("GetAccountInfo() error = %v", err)
	}
	if account.DepositAddress != "TDeposit" {
		t.Fatalf("DepositAddress = %q, want %q", account.DepositAddress, "TDeposit")
	}
	if account.AvailableBalanceSun != 24_795_000 {
		t.Fatalf("AvailableBalanceSun = %d, want %d", account.AvailableBalanceSun, int64(24_795_000))
	}

	priceInfo, err := provider.GetPriceInfo(context.Background())
	if err != nil {
		t.Fatalf("GetPriceInfo() error = %v", err)
	}
	if priceInfo.ActivatedPriceSun != 2_405_000 {
		t.Fatalf("ActivatedPriceSun = %d, want %d", priceInfo.ActivatedPriceSun, int64(2_405_000))
	}
	if priceInfo.ActivatedEnergy != 65_000 {
		t.Fatalf("ActivatedEnergy = %d, want %d", priceInfo.ActivatedEnergy, int64(65_000))
	}
	if len(priceInfo.DurationTiers) != 1 || priceInfo.DurationTiers[0].DurationHours != 1 {
		t.Fatalf("DurationTiers = %#v, want one 1-hour tier", priceInfo.DurationTiers)
	}
}

func TestNettsProviderGetOrderNormalizesConfirmed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apiv2/order_check" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("order_id"); got != "739" {
			t.Fatalf("order_id = %q, want %q", got, "739")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"order":{"id":"739","tx_hashes":["abc123"]}}`))
	}))
	defer server.Close()

	provider := NewNettsProvider("test-key", server.URL, "77.239.99.245", &http.Client{Timeout: time.Second})

	order, err := provider.GetOrder(context.Background(), "739")
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	if order.NormalizedStatus != TronEnergyRentalStatusActive {
		t.Fatalf("NormalizedStatus = %q, want %q", order.NormalizedStatus, TronEnergyRentalStatusActive)
	}
}
