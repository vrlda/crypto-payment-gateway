package mempool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	BaseURLMainnet = "https://mempool.space/api"
	BaseURLTestnet = "https://mempool.space/testnet/api"
)

type Client struct {
	BaseURL     string
	HTTP        *http.Client
	MinInterval time.Duration
	MaxRetries  int

	mu          sync.Mutex
	lastRequest time.Time
}

type UTXO struct {
	TxID   string `json:"txid"`
	Vout   uint32 `json:"vout"`
	Status struct {
		Confirmed   bool   `json:"confirmed"`
		BlockHeight uint64 `json:"block_height"`
		BlockHash   string `json:"block_hash"`
		BlockTime   int64  `json:"block_time"`
	} `json:"status"`
	Value int64 `json:"value"` // Satoshis
}

type Transaction struct {
	TxID   string `json:"txid"`
	Status struct {
		Confirmed   bool   `json:"confirmed"`
		BlockHeight uint64 `json:"block_height"`
	} `json:"status"`
	Vin []struct {
		TxID      string   `json:"txid"`
		Vout      uint32   `json:"vout"`
		Prevout   *TPSout  `json:"prevout"`
		ScriptSig string   `json:"scriptsig"`
		Sequence  uint32   `json:"sequence"`
		Witness   []string `json:"witness"`
	} `json:"vin"`
	Vout []TPSout `json:"vout"`
}

type TPSout struct {
	ScriptPubKey        string `json:"scriptpubkey"`
	ScriptPubKeyAsm     string `json:"scriptpubkey_asm"`
	ScriptPubKeyType    string `json:"scriptpubkey_type"`
	ScriptPubKeyAddress string `json:"scriptpubkey_address"`
	Value               int64  `json:"value"`
}

func NewClient(isTestnet bool) *Client {
	url := BaseURLMainnet
	if isTestnet {
		url = BaseURLTestnet
	}
	return &Client{
		BaseURL:     url,
		HTTP:        &http.Client{Timeout: 10 * time.Second},
		MinInterval: 1200 * time.Millisecond,
		MaxRetries:  6,
	}
}

func (c *Client) pace() {
	if c == nil || c.MinInterval <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastRequest.IsZero() {
		wait := c.MinInterval - time.Since(c.lastRequest)
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	c.lastRequest = time.Now()
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		wait := time.Until(t)
		if wait > 0 {
			return wait
		}
	}
	return 0
}

func (c *Client) retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
			return retryAfter
		}
	}

	switch attempt {
	case 0:
		return 2 * time.Second
	case 1:
		return 5 * time.Second
	case 2:
		return 10 * time.Second
	case 3:
		return 20 * time.Second
	default:
		return 30 * time.Second
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c == nil || c.HTTP == nil {
		return nil, fmt.Errorf("mempool client is not configured")
	}

	maxAttempts := c.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		currentReq := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			currentReq.Body = body
		}

		c.pace()

		resp, err := c.HTTP.Do(currentReq)
		if err != nil {
			if attempt == maxAttempts-1 {
				return nil, err
			}
			time.Sleep(c.retryDelay(attempt, nil))
			continue
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		resp.Body.Close()
		if attempt == maxAttempts-1 {
			return nil, fmt.Errorf("api returned status %d", http.StatusTooManyRequests)
		}

		time.Sleep(c.retryDelay(attempt, resp))
	}

	return nil, fmt.Errorf("request retries exhausted")
}

func (c *Client) getJSON(path string, target any) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("api returned status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

func (c *Client) postText(path, body string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("broadcast failed: %s", string(respBody))
	}

	return string(respBody), nil
}

// GetAddressTxs fetches transactions for an address
func (c *Client) GetAddressTxs(address string) ([]Transaction, error) {
	var txs []Transaction
	if err := c.getJSON(fmt.Sprintf("/address/%s/txs", address), &txs); err != nil {
		return nil, err
	}
	return txs, nil
}

// GetUTXOs fetches unspent outputs
func (c *Client) GetUTXOs(address string) ([]UTXO, error) {
	var utxos []UTXO
	if err := c.getJSON(fmt.Sprintf("/address/%s/utxo", address), &utxos); err != nil {
		return nil, err
	}
	return utxos, nil
}

// GetTransaction fetches details for a specific transaction
func (c *Client) GetTransaction(txid string) (*Transaction, error) {
	var tx Transaction
	if err := c.getJSON(fmt.Sprintf("/tx/%s", txid), &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// GetTipHeight fetches the current block tip height
func (c *Client) GetTipHeight() (uint64, error) {
	var height uint64
	if err := c.getJSON("/blocks/tip/height", &height); err != nil {
		return 0, err
	}
	return height, nil
}

// BroadcastTx sends a raw hex transaction
func (c *Client) BroadcastTx(hexTx string) (string, error) {
	return c.postText("/tx", hexTx)
}

type RecommendedFees struct {
	FastestFee  int `json:"fastestFee"`
	HalfHourFee int `json:"halfHourFee"`
	HourFee     int `json:"hourFee"`
	EconomyFee  int `json:"economyFee"`
	MinimumFee  int `json:"minimumFee"`
}

func (c *Client) GetRecommendedFees() (*RecommendedFees, error) {
	var fees RecommendedFees
	if err := c.getJSON("/v1/fees/recommended", &fees); err != nil {
		return nil, err
	}
	return &fees, nil
}
