package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestConvertUSDToCoin(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ids"); got != "bitcoin" {
			t.Fatalf("expected bitcoin id, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bitcoin":{"usd":50000}}`))
	}))
	defer server.Close()

	service := newPriceServiceForTests(server.URL, server.Client())

	amount, err := service.ConvertUSDToCoin(context.Background(), decimal.RequireFromString("0.5"), "BTC")
	if err != nil {
		t.Fatalf("ConvertUSDToCoin returned error: %v", err)
	}

	want := decimal.RequireFromString("0.00001")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertUSDToStablecoin(t *testing.T) {
	t.Parallel()

	service := newPriceServiceForTests("https://example.invalid", http.DefaultClient)

	amount, err := service.ConvertUSDToCoin(context.Background(), decimal.RequireFromString("0.5"), "USDT")
	if err != nil {
		t.Fatalf("ConvertUSDToCoin returned error: %v", err)
	}

	want := decimal.RequireFromString("0.5")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertCoinToUSD(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ids"); got != "bitcoin" {
			t.Fatalf("expected bitcoin id, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bitcoin":{"usd":50000}}`))
	}))
	defer server.Close()

	service := newPriceServiceForTests(server.URL, server.Client())

	amount, err := service.ConvertCoinToUSD(context.Background(), decimal.RequireFromString("0.002"), "BTC")
	if err != nil {
		t.Fatalf("ConvertCoinToUSD returned error: %v", err)
	}

	want := decimal.RequireFromString("100")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertUSDToCoinSupportsSolana(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ids"); got != "solana" {
			t.Fatalf("expected solana id, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"solana":{"usd":125}}`))
	}))
	defer server.Close()

	service := newPriceServiceForTests(server.URL, server.Client())

	amount, err := service.ConvertUSDToCoin(context.Background(), decimal.RequireFromString("25"), "SOL")
	if err != nil {
		t.Fatalf("ConvertUSDToCoin returned error: %v", err)
	}

	want := decimal.RequireFromString("0.2")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertCoinToUSDSupportsMaticAlias(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ids"); got != "polygon-ecosystem-token" {
			t.Fatalf("expected polygon-ecosystem-token id, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"polygon-ecosystem-token":{"usd":2.5}}`))
	}))
	defer server.Close()

	service := newPriceServiceForTests(server.URL, server.Client())

	amount, err := service.ConvertCoinToUSD(context.Background(), decimal.RequireFromString("4"), "MATIC")
	if err != nil {
		t.Fatalf("ConvertCoinToUSD returned error: %v", err)
	}

	want := decimal.RequireFromString("10")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertStablecoinToUSD(t *testing.T) {
	t.Parallel()

	service := newPriceServiceForTests("https://example.invalid", http.DefaultClient)

	amount, err := service.ConvertCoinToUSD(context.Background(), decimal.RequireFromString("12.5"), "USDT")
	if err != nil {
		t.Fatalf("ConvertCoinToUSD returned error: %v", err)
	}

	want := decimal.RequireFromString("12.5")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestConvertUSDToCoinRejectsUnsupportedAsset(t *testing.T) {
	t.Parallel()

	service := newPriceServiceForTests("https://example.invalid", http.DefaultClient)

	if _, err := service.ConvertUSDToCoin(context.Background(), decimal.RequireFromString("1"), "DOGE"); err == nil {
		t.Fatal("expected unsupported coin error")
	}
}

func TestGetUSDPriceFallsBackToStaleCache(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	service := newPriceServiceForTests(server.URL, server.Client())
	service.setCachedPrice("BTC", decimal.RequireFromString("50000"))
	service.mu.Lock()
	cached := service.cache["BTC"]
	cached.expiresAt = time.Now().Add(-1 * time.Minute)
	service.cache["BTC"] = cached
	service.mu.Unlock()

	price, err := service.GetUSDPrice(context.Background(), "BTC")
	if err != nil {
		t.Fatalf("GetUSDPrice returned error: %v", err)
	}

	want := decimal.RequireFromString("50000")
	if !price.Equal(want) {
		t.Fatalf("expected %s, got %s", want, price)
	}
}
