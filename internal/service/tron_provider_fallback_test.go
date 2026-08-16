package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
)

func TestTronHTTPGetWithFallbackOnRateLimit(t *testing.T) {
	oldProviders := configuredTronHTTPProviders("")
	t.Cleanup(func() {
		ConfigureTronHTTPProviders(oldProviders)
	})

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`rate limited`))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fallback.Close()

	ConfigureTronHTTPProviders([]TronHTTPProvider{
		{Name: "primary", BaseURL: primary.URL},
		{Name: "fallback", BaseURL: fallback.URL},
	})

	result, err := tronHTTPGetWithFallback(context.Background(), "test_http", "", "/v1/accounts/test")
	if err != nil {
		t.Fatalf("tronHTTPGetWithFallback() error = %v", err)
	}
	if result.Provider.Name != "fallback" {
		t.Fatalf("expected fallback provider, got %q", result.Provider.Name)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", result.StatusCode)
	}
}

func TestTronCallWithRetryFallsBackProvider(t *testing.T) {
	oldProviders := configuredTronGRPCProviders(nil)
	t.Cleanup(func() {
		ConfigureTronGRPCProviders(oldProviders)
	})

	primary := &client.GrpcClient{}
	fallback := &client.GrpcClient{}
	ConfigureTronGRPCProviders([]TronGRPCProvider{
		{Name: "primary", Client: primary},
		{Name: "fallback", Client: fallback},
	})

	got, err := tronCallWithRetry(context.Background(), "test_grpc", nil, func(c *client.GrpcClient) (string, error) {
		if c == primary {
			return "", fmt.Errorf("rpc error: code = Unavailable desc = unexpected HTTP status code received from server: 429 (Too Many Requests)")
		}
		if c == fallback {
			return "ok", nil
		}
		return "", fmt.Errorf("unexpected provider")
	})
	if err != nil {
		t.Fatalf("tronCallWithRetry() error = %v", err)
	}
	if got != "ok" {
		t.Fatalf("expected ok, got %q", got)
	}
}

func TestIsTronProviderFallbackError(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		if !isTronProviderFallbackError(fmt.Errorf("429 too many requests")) {
			t.Fatal("expected rate limit error to fallback")
		}
	})

	t.Run("network unavailable", func(t *testing.T) {
		if !isTronProviderFallbackError(fmt.Errorf("rpc error: code = Unavailable desc = transport is closing")) {
			t.Fatal("expected unavailable error to fallback")
		}
	})

	t.Run("business error", func(t *testing.T) {
		if isTronProviderFallbackError(fmt.Errorf("provider error: Receiver address is not activated")) {
			t.Fatal("did not expect activation error to fallback")
		}
	})
}
