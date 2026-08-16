package mempool

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetUTXOsRetriesOnRateLimit(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		if count <= 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	client := &Client{
		BaseURL:     server.URL,
		HTTP:        server.Client(),
		MinInterval: 0,
		MaxRetries:  3,
	}

	utxos, err := client.GetUTXOs("bc1test")
	if err != nil {
		t.Fatalf("GetUTXOs returned error: %v", err)
	}
	if len(utxos) != 0 {
		t.Fatalf("expected no utxos, got %d", len(utxos))
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("expected 3 requests, got %d", got)
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	got := parseRetryAfter("2")
	if got != 2*time.Second {
		t.Fatalf("expected 2s, got %v", got)
	}
}
