package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
)

type TronGRPCProvider struct {
	Name   string
	Client *client.GrpcClient
}

type TronHTTPProvider struct {
	Name    string
	BaseURL string
	APIKey  string
}

type tronHTTPResult struct {
	Body       []byte
	StatusCode int
	Provider   TronHTTPProvider
}

var (
	tronProviderConfigMu sync.RWMutex
	tronGRPCProviders    []TronGRPCProvider
	tronHTTPProviders    []TronHTTPProvider
)

func ConfigureTronGRPCProviders(providers []TronGRPCProvider) {
	tronProviderConfigMu.Lock()
	defer tronProviderConfigMu.Unlock()
	tronGRPCProviders = dedupeTronGRPCProviders(providers)
}

func ConfigureTronHTTPProviders(providers []TronHTTPProvider) {
	tronProviderConfigMu.Lock()
	defer tronProviderConfigMu.Unlock()
	tronHTTPProviders = dedupeTronHTTPProviders(providers)
}

func dedupeTronGRPCProviders(providers []TronGRPCProvider) []TronGRPCProvider {
	out := make([]TronGRPCProvider, 0, len(providers))
	seen := make(map[*client.GrpcClient]struct{}, len(providers))
	for _, provider := range providers {
		if provider.Client == nil {
			continue
		}
		if _, exists := seen[provider.Client]; exists {
			continue
		}
		seen[provider.Client] = struct{}{}
		out = append(out, provider)
	}
	return out
}

func dedupeTronHTTPProviders(providers []TronHTTPProvider) []TronHTTPProvider {
	out := make([]TronHTTPProvider, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		baseURL := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		if baseURL == "" {
			continue
		}
		key := strings.ToLower(baseURL) + "|" + strings.TrimSpace(provider.APIKey)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		provider.BaseURL = baseURL
		out = append(out, provider)
	}
	return out
}

func configuredTronGRPCProviders(primary *client.GrpcClient) []TronGRPCProvider {
	tronProviderConfigMu.RLock()
	defer tronProviderConfigMu.RUnlock()

	providers := make([]TronGRPCProvider, 0, len(tronGRPCProviders)+1)
	if primary != nil {
		providers = append(providers, TronGRPCProvider{Name: "primary", Client: primary})
	}
	providers = append(providers, tronGRPCProviders...)
	return dedupeTronGRPCProviders(providers)
}

func configuredTronHTTPProviders(defaultAPIKey string) []TronHTTPProvider {
	tronProviderConfigMu.RLock()
	defer tronProviderConfigMu.RUnlock()

	if len(tronHTTPProviders) > 0 {
		providers := make([]TronHTTPProvider, 0, len(tronHTTPProviders))
		providers = append(providers, tronHTTPProviders...)
		return dedupeTronHTTPProviders(providers)
	}

	return []TronHTTPProvider{{
		Name:    "trongrid",
		BaseURL: "https://api.trongrid.io",
		APIKey:  strings.TrimSpace(defaultAPIKey),
	}}
}

func tronCallWithRetry[T any](ctx context.Context, operation string, primary *client.GrpcClient, fn func(*client.GrpcClient) (T, error)) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}

	providers := configuredTronGRPCProviders(primary)
	if len(providers) == 0 {
		return zero, fmt.Errorf("tron client not initialized")
	}

	attempts := tronRPCRetryLimit
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		for idx, provider := range providers {
			if err := tronWaitForRateLimitSlot(ctx); err != nil {
				return zero, err
			}

			value, err := fn(provider.Client)
			if err == nil {
				return value, nil
			}

			lastErr = err
			if !isTronProviderFallbackError(err) {
				return zero, err
			}

			if idx < len(providers)-1 {
				log.Printf("TRON provider fallback during %s via %s: %v", operation, provider.Name, err)
				continue
			}
		}

		if attempt == attempts-1 {
			break
		}

		delay := tronRPCRetryBaseDelay * time.Duration(attempt+1)
		if isTronRateLimitError(lastErr) {
			log.Printf("TRON RPC rate limited during %s; retrying in %s: %v", operation, delay, lastErr)
		} else {
			log.Printf("TRON provider unavailable during %s; retrying in %s: %v", operation, delay, lastErr)
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("tron operation failed: %s", operation)
	}
	return zero, lastErr
}

func tronHTTPGetWithFallback(ctx context.Context, operation, defaultAPIKey, path string) (tronHTTPResult, error) {
	var zero tronHTTPResult
	providers := configuredTronHTTPProviders(defaultAPIKey)
	if len(providers) == 0 {
		return zero, fmt.Errorf("no tron http provider configured")
	}

	var lastErr error
	for idx, provider := range providers {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.BaseURL+path, nil)
		if err != nil {
			return zero, err
		}
		if provider.APIKey != "" {
			req.Header.Set("TRON-PRO-API-KEY", provider.APIKey)
		}

		resp, err := tronGridHTTPClient.Do(req)
		if err != nil {
			lastErr = err
			if isTronProviderFallbackError(err) && idx < len(providers)-1 {
				log.Printf("TRON HTTP fallback during %s via %s: %v", operation, provider.Name, err)
				continue
			}
			return zero, err
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if isTronProviderFallbackError(readErr) && idx < len(providers)-1 {
				log.Printf("TRON HTTP fallback during %s via %s body read: %v", operation, provider.Name, readErr)
				continue
			}
			return zero, readErr
		}

		result := tronHTTPResult{Body: body, StatusCode: resp.StatusCode, Provider: provider}
		if tronHTTPStatusShouldFallback(resp.StatusCode) && idx < len(providers)-1 {
			lastErr = fmt.Errorf("provider %s returned status %d", provider.Name, resp.StatusCode)
			log.Printf("TRON HTTP fallback during %s via %s: status=%d", operation, provider.Name, resp.StatusCode)
			continue
		}
		return result, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("tron http operation failed: %s", operation)
	}
	return zero, lastErr
}

func tronHTTPStatusShouldFallback(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func isTronProviderFallbackError(err error) bool {
	if err == nil {
		return false
	}
	if isTronRateLimitError(err) {
		return true
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unavailable") ||
		strings.Contains(text, "deadline exceeded") ||
		strings.Contains(text, "timeout") ||
		strings.Contains(text, "eof") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "refused") ||
		strings.Contains(text, "transport is closing") ||
		strings.Contains(text, "temporary failure")
}
