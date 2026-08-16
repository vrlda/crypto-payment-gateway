package service

import (
	"strings"
	"testing"
)

func TestIsTronRateLimitError(t *testing.T) {
	t.Parallel()

	if !isTronRateLimitError(assertErr("rpc error: code = Unavailable desc = unexpected HTTP status code received from server: 429 (Too Many Requests)")) {
		t.Fatal("expected 429 transport error to be treated as a rate limit")
	}
	if !isTronRateLimitError(assertErr("Too Many Requests")) {
		t.Fatal("expected explicit rate-limit error to be treated as a rate limit")
	}
	if isTronRateLimitError(assertErr("connection reset by peer")) {
		t.Fatal("did not expect unrelated transport error to be treated as a rate limit")
	}
}

func TestCompressLiveAuditErrorsGroupsRepeatedDetails(t *testing.T) {
	t.Parallel()

	input := []string{
		"dep-1 (USDT/TRC20): rpc error: code = Unavailable desc = unexpected HTTP status code received from server: 429 (Too Many Requests)",
		"dep-2 (USDT/TRC20): rpc error: code = Unavailable desc = unexpected HTTP status code received from server: 429 (Too Many Requests)",
		"dep-3 (USDT/TRC20): rpc error: code = Unavailable desc = unexpected HTTP status code received from server: 429 (Too Many Requests)",
		"dep-4 (BTC/BTC): invalid address",
	}

	got := compressLiveAuditErrors(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 compressed errors, got %d: %#v", len(got), got)
	}
	if want := "3 deposits hit the same audit error [dep-1, dep-2, dep-3]"; !strings.HasPrefix(got[0], want) {
		t.Fatalf("unexpected grouped error: %q", got[0])
	}
	if got[1] != "dep-4: invalid address" {
		t.Fatalf("unexpected single error: %q", got[1])
	}
}

func TestTronTriggerConstantContractBalanceParam(t *testing.T) {
	t.Parallel()

	got, err := tronTriggerConstantContractBalanceParam("TPv9ZzDtwCfz2T2YtaSoKotkbZXnx5vUyq")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const want = "00000000000000000000004198fda5e19c7fa27c222092361ac12c99020cfe16"
	if got != want {
		t.Fatalf("param = %q, want %q", got, want)
	}
}

func assertErr(message string) error {
	return testError(message)
}

type testError string

func (e testError) Error() string {
	return string(e)
}
