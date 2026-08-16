package service

import (
	"crypto_payment_gateway_core/internal/model"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/shopspring/decimal"
)

func TestDecodeERC20TransferLog(t *testing.T) {
	t.Parallel()

	entry := &types.Log{
		Topics: []common.Hash{
			erc20TransferEventSig,
			common.HexToHash("0x0000000000000000000000001111111111111111111111111111111111111111"),
			common.HexToHash("0x0000000000000000000000002222222222222222222222222222222222222222"),
		},
		Data: common.LeftPadBytes(big.NewInt(12_500_000).Bytes(), 32),
	}

	recipient, amount, ok := decodeERC20TransferLog(entry, 6)
	if !ok {
		t.Fatal("expected ERC20 transfer log to decode")
	}
	if recipient != "0x2222222222222222222222222222222222222222" {
		t.Fatalf("unexpected recipient: %s", recipient)
	}

	want := decimal.RequireFromString("12.5")
	if !amount.Equal(want) {
		t.Fatalf("expected %s, got %s", want, amount)
	}
}

func TestEVMRPCBlockDecodesUnknownTransactionTypes(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"transactions": [
			{
				"hash": "0xfceb83eb75d5cbae4a226ee4e968fc325241c77bfd37bf9359c09c1d5f41a111",
				"type": "0x6a",
				"to": "0x1111111111111111111111111111111111111111",
				"value": "0x2386f26fc10000"
			}
		]
	}`)

	var block evmRPCBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("failed to decode raw block payload: %v", err)
	}
	if len(block.Transactions) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(block.Transactions))
	}
	tx := block.Transactions[0]
	if tx.To == nil || tx.To.Hex() != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected recipient: %v", tx.To)
	}
	if tx.Value == nil || tx.Value.ToInt().Cmp(big.NewInt(0x2386f26fc10000)) != 0 {
		t.Fatalf("unexpected value: %v", tx.Value)
	}
}

func TestEVMDepositAcceptsNativeTransfer(t *testing.T) {
	t.Parallel()

	contractAddress := "0x1111111111111111111111111111111111111111"

	tests := []struct {
		name   string
		dep    *model.CryptoDeposit
		wallet *model.HDWallet
		want   bool
	}{
		{
			name: "native invoice accepts matching native coin",
			dep: &model.CryptoDeposit{
				Coin:    "ETH",
				Network: "ERC20",
			},
			wallet: &model.HDWallet{},
			want:   true,
		},
		{
			name: "token invoice rejects native transfer",
			dep: &model.CryptoDeposit{
				Coin:    "USDT",
				Network: "ERC20",
			},
			wallet: &model.HDWallet{
				ContractAddress: &contractAddress,
			},
			want: false,
		},
		{
			name: "native invoice rejects wrong coin",
			dep: &model.CryptoDeposit{
				Coin:    "USDT",
				Network: "ERC20",
			},
			wallet: &model.HDWallet{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := evmDepositAcceptsNativeTransfer(tt.dep, tt.wallet); got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}
