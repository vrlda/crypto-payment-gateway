package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

const defaultPriceAPIBaseURL = "https://api.coingecko.com/api/v3"

var priceCoinIDs = map[string]string{
	"BTC":   "bitcoin",
	"ETH":   "ethereum",
	"LTC":   "litecoin",
	"BNB":   "binancecoin",
	"ARB":   "arbitrum",
	"MATIC": "polygon-ecosystem-token",
	"POL":   "polygon-ecosystem-token",
	"SOL":   "solana",
	"TON":   "the-open-network",
	"TRX":   "tron",
	"USDC":  "usd-coin",
	"USDT":  "tether",
}

var pricePrecisions = map[string]int32{
	"BTC":   8,
	"ETH":   8,
	"LTC":   8,
	"BNB":   8,
	"ARB":   8,
	"MATIC": 8,
	"POL":   8,
	"SOL":   9,
	"TON":   9,
	"TRX":   6,
	"USDC":  6,
	"USDT":  6,
}

type cachedPrice struct {
	price     decimal.Decimal
	expiresAt time.Time
}

type PriceService struct {
	baseURL string
	client  *http.Client

	mu    sync.RWMutex
	cache map[string]cachedPrice
}

func NewPriceService() *PriceService {
	baseURL := os.Getenv("PRICE_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultPriceAPIBaseURL
	}

	return &PriceService{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		cache: make(map[string]cachedPrice),
	}
}

func newPriceServiceForTests(baseURL string, client *http.Client) *PriceService {
	return &PriceService{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
		cache:   make(map[string]cachedPrice),
	}
}

func NewPriceServiceForTests(baseURL string, client *http.Client) *PriceService {
	return newPriceServiceForTests(baseURL, client)
}

func (s *PriceService) ConvertUSDToCoin(ctx context.Context, usdAmount decimal.Decimal, coin string) (decimal.Decimal, error) {
	if !usdAmount.IsPositive() {
		return decimal.Zero, fmt.Errorf("amount must be positive")
	}

	symbol := strings.ToUpper(strings.TrimSpace(coin))
	price, err := s.GetUSDPrice(ctx, symbol)
	if err != nil {
		return decimal.Zero, err
	}
	if !price.IsPositive() {
		return decimal.Zero, fmt.Errorf("invalid %s price", symbol)
	}

	precision := int32(8)
	if p, ok := pricePrecisions[symbol]; ok {
		precision = p
	}

	return usdAmount.Div(price).RoundCeil(precision), nil
}

func (s *PriceService) ConvertCoinToUSD(ctx context.Context, coinAmount decimal.Decimal, coin string) (decimal.Decimal, error) {
	if !coinAmount.IsPositive() {
		return decimal.Zero, fmt.Errorf("amount must be positive")
	}

	symbol := strings.ToUpper(strings.TrimSpace(coin))
	price, err := s.GetUSDPrice(ctx, symbol)
	if err != nil {
		return decimal.Zero, err
	}
	if !price.IsPositive() {
		return decimal.Zero, fmt.Errorf("invalid %s price", symbol)
	}

	return coinAmount.Mul(price), nil
}

func (s *PriceService) GetUSDPrice(ctx context.Context, coin string) (decimal.Decimal, error) {
	symbol := strings.ToUpper(strings.TrimSpace(coin))
	if symbol == "USD" || symbol == "USDT" || symbol == "USDC" {
		return decimal.NewFromInt(1), nil
	}

	if cached, ok := s.getCachedPrice(symbol); ok {
		return cached, nil
	}

	coinID, ok := priceCoinIDs[symbol]
	if !ok {
		return decimal.Zero, fmt.Errorf("unsupported coin for price conversion: %s", symbol)
	}

	endpoint, err := url.Parse(s.baseURL + "/simple/price")
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid price API base URL: %w", err)
	}

	query := endpoint.Query()
	query.Set("ids", coinID)
	query.Set("vs_currencies", "usd")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("failed to build price request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("failed to fetch %s price: %w", symbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("price provider returned %s for %s", resp.Status, symbol)
	}

	var payload map[string]map[string]json.Number
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("failed to decode %s price response: %w", symbol, err)
	}

	priceNumber, ok := payload[coinID]["usd"]
	if !ok {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("price response missing usd quote for %s", symbol)
	}

	price, err := decimal.NewFromString(priceNumber.String())
	if err != nil {
		if cached, ok := s.getAnyCachedPrice(symbol); ok {
			return cached, nil
		}
		return decimal.Zero, fmt.Errorf("invalid %s usd quote: %w", symbol, err)
	}

	s.setCachedPrice(symbol, price)
	return price, nil
}

func (s *PriceService) getCachedPrice(symbol string) (decimal.Decimal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cached, ok := s.cache[symbol]
	if !ok || time.Now().After(cached.expiresAt) {
		return decimal.Zero, false
	}
	return cached.price, true
}

func (s *PriceService) getAnyCachedPrice(symbol string) (decimal.Decimal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cached, ok := s.cache[symbol]
	if !ok {
		return decimal.Zero, false
	}
	return cached.price, true
}

func (s *PriceService) setCachedPrice(symbol string, price decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cache[symbol] = cachedPrice{
		price:     price,
		expiresAt: time.Now().Add(1 * time.Minute),
	}
}
