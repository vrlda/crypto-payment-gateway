package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"crypto_payment_gateway_core/internal/database"
	"crypto_payment_gateway_core/internal/repository"
	"crypto_payment_gateway_core/pkg/crypto"
)

type WebhookService struct {
	client       *http.Client
	merchantRepo *repository.MerchantRepository
}

func NewWebhookService(merchantRepo *repository.MerchantRepository) *WebhookService {
	return &WebhookService{
		client:       newSafeWebhookHTTPClient(),
		merchantRepo: merchantRepo,
	}
}

type WebhookPayload struct {
	Event     string      `json:"event"`
	Data      interface{} `json:"data"`
	Timestamp int64       `json:"timestamp"`
}

var (
	ErrWebhookURLNotConfigured    = errors.New("webhook url not configured")
	ErrWebhookSecretNotConfigured = errors.New("webhook signing secret not configured")
	ErrWebhookSecretInvalid       = errors.New("webhook signing secret is invalid")
	ErrWebhookEndpointUnreachable = errors.New("webhook endpoint is unreachable")
	ErrWebhookEndpointRejected    = errors.New("webhook endpoint rejected the request")
	ErrWebhookURLUnsafe           = errors.New("webhook url points to an unsafe destination")
)

func newSafeWebhookHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		safeIP, err := validateWebhookHostAndGetIP(ctx, host)
		if err != nil {
			return nil, err
		}
		safeAddress := net.JoinHostPort(safeIP, port)
		return dialer.DialContext(ctx, network, safeAddress)
	}

	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func isUnsafeWebhookIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}

	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}

func validateWebhookHostAndGetIP(ctx context.Context, host string) (string, error) {
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		if isUnsafeWebhookIP(parsedIP) {
			return "", ErrWebhookURLUnsafe
		}
		return parsedIP.String(), nil
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("webhook host did not resolve")
	}

	for _, ip := range ips {
		if !isUnsafeWebhookIP(ip.IP) {
			return ip.IP.String(), nil
		}
	}

	return "", ErrWebhookURLUnsafe
}

func ValidateWebhookURL(ctx context.Context, rawURL string) error {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid webhook url: %w", err)
	}
	if parsedURL.Scheme != "https" {
		return errors.New("webhook url must use https")
	}
	if parsedURL.User != nil {
		return errors.New("webhook url must not include credentials")
	}
	host := parsedURL.Hostname()
	if host == "" {
		return errors.New("webhook url host is required")
	}

	_, err = validateWebhookHostAndGetIP(ctx, host)
	return err
}

// Dispatch sends a signed webhook to the merchant
func (s *WebhookService) Dispatch(ctx context.Context, merchantId string, payload WebhookPayload) error {
	// 1. Get Merchant Webhook Config
	url, secretEnc, err := s.merchantRepo.GetWebhookConfig(ctx, merchantId)
	if err != nil {
		return fmt.Errorf("failed to get merchant config: %w", err)
	}

	if url == "" {
		return ErrWebhookURLNotConfigured
	}
	if secretEnc == "" {
		return ErrWebhookSecretNotConfigured
	}
	if err := ValidateWebhookURL(ctx, url); err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookURLUnsafe, err)
	}

	// 2. Decrypt Secret
	secret, err := crypto.Decrypt(secretEnc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookSecretInvalid, err)
	}

	// 3. Prepare Payload
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// 4. Sign Payload (HMAC-SHA256)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))

	// 5. Send Request
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", signature)
	req.Header.Set("User-Agent", "CryptoGateway/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		s.logFailure(ctx, merchantId, url, string(body), 0, err.Error(), 1)
		return fmt.Errorf("%w: %v", ErrWebhookEndpointUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		s.logFailure(ctx, merchantId, url, string(body), resp.StatusCode, string(respBody), 1)
		return fmt.Errorf("%w: status %d, body: %s", ErrWebhookEndpointRejected, resp.StatusCode, string(respBody))
	}

	// Success
	_, _ = database.DB.Exec(ctx, `
		INSERT INTO webhook_logs (merchant_id, url, payload, response_code, response_body, status, attempt_count, created_at) 
		VALUES ($1, $2, $3, $4, 'OK', 'SUCCESS', 1, NOW())
	`, merchantId, url, string(body), resp.StatusCode)
	return nil
}

func (s *WebhookService) logFailure(ctx context.Context, merchantId, url, payload string, code int, respBody string, attempt int) {
	nextRetry := time.Now().Add(time.Duration(attempt*attempt) * 10 * time.Minute) // Simple exponential backoff
	query := `
		INSERT INTO webhook_logs (merchant_id, url, payload, response_code, response_body, status, attempt_count, next_retry_at, created_at) 
		VALUES ($1, $2, $3, $4, $5, 'FAILED', $6, $7, NOW())`
	_, _ = database.DB.Exec(ctx, query, merchantId, url, payload, code, respBody, attempt, nextRetry)
}

// ProcessRetries scans for failed webhooks and retries them
func (s *WebhookService) ProcessRetries(ctx context.Context) {
	query := `
		SELECT id, merchant_id, url, payload, attempt_count 
		FROM webhook_logs 
		WHERE status = 'FAILED' AND attempt_count < 5 AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		LIMIT 50`

	rows, err := database.DB.Query(ctx, query)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, merchantId, url, payload string
		var attempt int
		if err := rows.Scan(&id, &merchantId, &url, &payload, &attempt); err != nil {
			continue
		}

		// Re-dispatch logic
		go func(wid, mid, targetUrl, data string, count int) {
			newCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			// 1. Get secret
			_, secretEnc, err := s.merchantRepo.GetWebhookConfig(newCtx, mid)
			if err != nil {
				nextRetry := time.Now().Add(time.Duration((count+1)*(count+1)) * 10 * time.Minute)
				_, _ = database.DB.Exec(context.Background(), `
					UPDATE webhook_logs 
					SET attempt_count = attempt_count + 1, next_retry_at = $1, response_body = $2 
					WHERE id = $3`, nextRetry, "CONFIG_FETCH_FAILED", wid)
				return
			}
			secret, _ := crypto.Decrypt(secretEnc)

			// 2. Sign
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(data))
			signature := hex.EncodeToString(mac.Sum(nil))

			// 3. Send
			req, _ := http.NewRequestWithContext(newCtx, "POST", targetUrl, bytes.NewBuffer([]byte(data)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Signature", signature)
			req.Header.Set("User-Agent", "CryptoGateway/1.0-Retry")

			resp, err := s.client.Do(req)
			if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
				// Update with failed attempt
				nextRetry := time.Now().Add(time.Duration((count+1)*(count+1)) * 10 * time.Minute)
				_, _ = database.DB.Exec(context.Background(), `
					UPDATE webhook_logs 
					SET attempt_count = attempt_count + 1, next_retry_at = $1, response_body = $2 
					WHERE id = $3`, nextRetry, "RETRY_FAILED", wid)
				return
			}
			defer resp.Body.Close()

			// Success
			_, _ = database.DB.Exec(context.Background(), `UPDATE webhook_logs SET status = 'SUCCESS', attempt_count = attempt_count + 1 WHERE id = $1`, wid)
		}(id, merchantId, url, payload, attempt)
	}
}
