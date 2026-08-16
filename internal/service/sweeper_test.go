package service

import (
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"crypto_payment_gateway_core/internal/model"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/ethereum/go-ethereum/common"
	troncore "github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/shopspring/decimal"
	"github.com/xssnick/tonutils-go/tlb"
)

func TestSolanaFeeEstimateLamports(t *testing.T) {
	t.Parallel()

	fee := solanaFeeEstimateLamports()
	if fee == 0 {
		t.Fatal("solanaFeeEstimateLamports() returned 0")
	}
	if fee != 5000 {
		t.Fatalf("expected default fee 5000 lamports, got %d", fee)
	}
}

func TestMaxSweepAttempts(t *testing.T) {
	t.Parallel()

	if maxSweepAttempts <= 0 {
		t.Fatal("maxSweepAttempts must be positive")
	}
	if maxSweepAttempts > 100 {
		t.Fatalf("maxSweepAttempts seems too high: %d", maxSweepAttempts)
	}
}

func TestResolveTronAccountBalanceSun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		account *troncore.Account
		err     error
		want    int64
		wantErr bool
	}{
		{
			name: "account balance is returned",
			account: &troncore.Account{
				Balance: 123456,
			},
			want: 123456,
		},
		{
			name: "missing account is treated as zero balance",
			err:  fmt.Errorf("account not found"),
			want: 0,
		},
		{
			name: "nil account without error is treated as zero balance",
			want: 0,
		},
		{
			name:    "other errors still fail",
			err:     fmt.Errorf("rpc unavailable"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveTronAccountBalanceSun(tt.account, tt.err)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, got)
			}
		})
	}
}

func TestResolveTronAccountActivated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		account *troncore.Account
		err     error
		want    bool
		wantErr bool
	}{
		{
			name: "activated account with address bytes",
			account: &troncore.Account{
				Address: []byte{1, 2, 3},
			},
			want: true,
		},
		{
			name: "activated account with create time",
			account: &troncore.Account{
				CreateTime: 123,
			},
			want: true,
		},
		{
			name: "missing account is not activated",
			err:  fmt.Errorf("account not found"),
			want: false,
		},
		{
			name: "nil account is not activated",
			want: false,
		},
		{
			name:    "other errors fail",
			err:     fmt.Errorf("rpc unavailable"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveTronAccountActivated(tt.account, tt.err)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestValidateGasWalletOutlay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		wallet *model.GasWallet
		outlay string
		wantOK bool
	}{
		{
			name:   "nil wallet is allowed",
			wallet: nil,
			outlay: "1",
			wantOK: true,
		},
		{
			name: "disabled wallet skips policy",
			wallet: &model.GasWallet{
				IsEnabled:      false,
				MaxGasPerSweep: decimal.RequireFromString("0.5"),
				DailyGasLimit:  decimal.RequireFromString("1"),
				DailyGasUsed:   decimal.RequireFromString("0.9"),
			},
			outlay: "5",
			wantOK: true,
		},
		{
			name: "outlay within limits passes",
			wallet: &model.GasWallet{
				IsEnabled:      true,
				MaxGasPerSweep: decimal.RequireFromString("0.5"),
				DailyGasLimit:  decimal.RequireFromString("1"),
				DailyGasUsed:   decimal.RequireFromString("0.2"),
			},
			outlay: "0.3",
			wantOK: true,
		},
		{
			name: "max per sweep exceeded blocks",
			wallet: &model.GasWallet{
				IsEnabled:      true,
				MaxGasPerSweep: decimal.RequireFromString("0.5"),
			},
			outlay: "0.6",
			wantOK: false,
		},
		{
			name: "daily limit exceeded blocks",
			wallet: &model.GasWallet{
				IsEnabled:     true,
				DailyGasLimit: decimal.RequireFromString("1"),
				DailyGasUsed:  decimal.RequireFromString("0.8"),
			},
			outlay: "0.3",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateGasWalletOutlay(tt.wallet, decimal.RequireFromString(tt.outlay))
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatal("expected policy error, got nil")
			}
			if !errors.Is(err, ErrSweepWaitingForGas) {
				t.Fatalf("expected ErrSweepWaitingForGas, got %v", err)
			}
		})
	}
}

func TestNormalizeWatchedAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		network string
		address string
		want    string
	}{
		{
			name:    "evm addresses are normalized to lowercase",
			network: "ERC20",
			address: " 0xAbC123 ",
			want:    "0xabc123",
		},
		{
			name:    "btc addresses preserve case and trim spaces",
			network: "BTC",
			address: " 1BoatSLRHtKNngkdXEeobR76b53LETtpyT ",
			want:    "1BoatSLRHtKNngkdXEeobR76b53LETtpyT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeWatchedAddress(tt.network, tt.address); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestTronTokenSweepReserveSunDefaultsToFiveTRX(t *testing.T) {
	t.Setenv("TRON_TOKEN_SWEEP_RESERVE_SUN", "")

	if got := tronTokenSweepReserveSun(); got != 5_000_000 {
		t.Fatalf("tronTokenSweepReserveSun() = %d, want %d", got, int64(5_000_000))
	}
}

func TestTronTokenSweepMinBalanceSunDefaultsToActivationAmount(t *testing.T) {
	t.Setenv("TRON_TOKEN_SWEEP_MIN_BALANCE_SUN", "")
	t.Setenv("TRON_ENERGY_ACTIVATION_AMOUNT_SUN", "")

	if got := tronTokenSweepMinBalanceSun(); got != 100_000 {
		t.Fatalf("tronTokenSweepMinBalanceSun() = %d, want %d", got, int64(100_000))
	}
}

func TestTronTokenSweepMinBalanceSunAllowsOverride(t *testing.T) {
	t.Setenv("TRON_TOKEN_SWEEP_MIN_BALANCE_SUN", "250000")

	if got := tronTokenSweepMinBalanceSun(); got != 250_000 {
		t.Fatalf("tronTokenSweepMinBalanceSun() = %d, want %d", got, int64(250_000))
	}
}

func TestTronEnergyActivationAmountSunDefaultsToTenthTRX(t *testing.T) {
	t.Setenv("TRON_ENERGY_ACTIVATION_AMOUNT_SUN", "")

	if got := tronEnergyActivationAmountSun(); got != 100_000 {
		t.Fatalf("tronEnergyActivationAmountSun() = %d, want %d", got, int64(100_000))
	}
}

func TestEstimateTronEnergyOrderCostSun(t *testing.T) {
	t.Parallel()

	priceInfo := &TronEnergyPriceInfo{
		ActivatedPriceSun: 1_500_000,
		DurationTiers: []TronEnergyPriceTier{
			{DurationHours: 1, Multiplier: 1.0},
			{DurationHours: 4, Multiplier: 1.2},
		},
	}

	got, err := estimateTronEnergyOrderCostSun(priceInfo, 1, 4)
	if err != nil {
		t.Fatalf("estimateTronEnergyOrderCostSun() error = %v", err)
	}
	if got != 1_800_000 {
		t.Fatalf("estimateTronEnergyOrderCostSun() = %d, want %d", got, int64(1_800_000))
	}
}

func TestEstimateTronEnergyOrderCostSunRejectsUnsupportedDuration(t *testing.T) {
	t.Parallel()

	_, err := estimateTronEnergyOrderCostSun(&TronEnergyPriceInfo{
		ActivatedPriceSun: 1_500_000,
		DurationTiers: []TronEnergyPriceTier{
			{DurationHours: 1, Multiplier: 1.0},
			{DurationHours: 24, Multiplier: 2.0},
		},
	}, 1, 4)
	if err == nil {
		t.Fatal("expected unsupported duration error, got nil")
	}
}

func TestEstimateTronEnergyOrderCostSunFallsBackToSingleTier(t *testing.T) {
	t.Parallel()

	got, err := estimateTronEnergyOrderCostSun(&TronEnergyPriceInfo{
		ActivatedPriceSun: 2_405_000,
		DurationTiers: []TronEnergyPriceTier{
			{DurationHours: 1, Multiplier: 1.0},
		},
	}, 1, 4)
	if err != nil {
		t.Fatalf("estimateTronEnergyOrderCostSun() error = %v", err)
	}
	if got != 2_405_000 {
		t.Fatalf("estimateTronEnergyOrderCostSun() = %d, want %d", got, int64(2_405_000))
	}
}

func TestTronEnergyOrderNumberUsesAttemptScopedSuffix(t *testing.T) {
	t.Parallel()

	sweep := &model.Sweep{ID: "abc", Attempts: 2}
	if got := tronEnergyOrderNumber(sweep); got != "sweep-abc-a2" {
		t.Fatalf("tronEnergyOrderNumber() = %q, want %q", got, "sweep-abc-a2")
	}
}

func TestTronEnergyOrderNumberKeepsStoredProviderOrderNo(t *testing.T) {
	t.Parallel()

	orderNo := "stored-order-no"
	sweep := &model.Sweep{ID: "abc", Attempts: 4, ProviderOrderNo: &orderNo}
	if got := tronEnergyOrderNumber(sweep); got != orderNo {
		t.Fatalf("tronEnergyOrderNumber() = %q, want %q", got, orderNo)
	}
}

func TestResolveSweepTokenContractAddress(t *testing.T) {
	t.Parallel()

	validContract := "0x1111111111111111111111111111111111111111"

	tests := []struct {
		name    string
		wallet  *model.HDWallet
		wantErr bool
		want    common.Address
	}{
		{
			name:    "missing contract fails closed",
			wallet:  &model.HDWallet{},
			wantErr: true,
		},
		{
			name: "valid contract resolves",
			wallet: &model.HDWallet{
				ContractAddress: &validContract,
			},
			want: common.HexToAddress(validContract),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSweepTokenContractAddress(tt.wallet)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want.Hex(), got.Hex())
			}
		})
	}
}

func TestExactTokenUnitsFromSweepAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		amount   string
		decimals int
		want     string
		wantErr  bool
	}{
		{
			name:     "exact amount converts to units",
			amount:   "1.234567",
			decimals: 6,
			want:     "1234567",
		},
		{
			name:     "precision loss is rejected",
			amount:   "1.2345671",
			decimals: 6,
			wantErr:  true,
		},
		{
			name:     "zero amount is rejected",
			amount:   "0",
			decimals: 6,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := exactTokenUnitsFromSweepAmount(decimal.RequireFromString(tt.amount), tt.decimals)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got.String())
			}
		})
	}
}

func TestShouldCloseTokenAccountAfterSweep(t *testing.T) {
	t.Parallel()

	if !shouldCloseTokenAccountAfterSweep(big.NewInt(100), big.NewInt(100)) {
		t.Fatal("expected equal balances to close token account")
	}
	if shouldCloseTokenAccountAfterSweep(big.NewInt(101), big.NewInt(100)) {
		t.Fatal("expected larger live balance to keep token account open")
	}
}

func TestTonAccountBalanceNanoHandlesMissingState(t *testing.T) {
	t.Parallel()

	if got := tonAccountBalanceNano(nil); got.Sign() != 0 {
		t.Fatalf("expected nil account to return zero balance, got %s", got.String())
	}

	if got := tonAccountBalanceNano(&tlb.Account{}); got.Sign() != 0 {
		t.Fatalf("expected account without state to return zero balance, got %s", got.String())
	}

	acc := &tlb.Account{
		State: &tlb.AccountState{
			IsValid: true,
			AccountStorage: tlb.AccountStorage{
				Balance: tlb.MustFromTON("1.25"),
			},
		},
	}

	want := tlb.MustFromTON("1.25").Nano().String()
	if got := tonAccountBalanceNano(acc).String(); got != want {
		t.Fatalf("expected account balance %s, got %s", want, got)
	}
}

func TestQueuedJettonAmountEncodesExactly(t *testing.T) {
	t.Parallel()

	units, err := exactTokenUnitsFromSweepAmount(decimal.RequireFromString("12.345678"), 6)
	if err != nil {
		t.Fatalf("unexpected conversion error: %v", err)
	}

	amount, err := tlb.FromNano(units, 6)
	if err != nil {
		t.Fatalf("failed to encode jetton amount: %v", err)
	}
	if amount.Nano().Cmp(units) != 0 {
		t.Fatalf("encoded jetton amount = %s, want %s", amount.Nano().String(), units.String())
	}
}

func TestNeedsGasResidueSweep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		sweep *model.Sweep
		want  bool
	}{
		{
			name: "token sweep remains residue eligible",
			sweep: &model.Sweep{
				Network: "TRC20",
				Coin:    "USDT",
				IsToken: true,
				Purpose: model.SweepPurposeDepositFunds,
			},
			want: true,
		},
		{
			name: "native evm sweep becomes residue eligible",
			sweep: &model.Sweep{
				Network: "ERC20",
				Coin:    "ETH",
				IsToken: false,
				Purpose: model.SweepPurposeDepositFunds,
			},
			want: false,
		},
		{
			name: "btc native sweep does not create residue rounds",
			sweep: &model.Sweep{
				Network: "BTC",
				Coin:    "BTC",
				IsToken: false,
				Purpose: model.SweepPurposeDepositFunds,
			},
			want: false,
		},
		{
			name: "gas residue sweep itself is not residue eligible",
			sweep: &model.Sweep{
				Network: "TON",
				Coin:    "TON",
				Purpose: model.SweepPurposeGasResidue,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := needsGasResidueSweep(tt.sweep); got != tt.want {
				t.Fatalf("needsGasResidueSweep() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRetryableSweepStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status model.SweepStatus
		want   bool
	}{
		{name: "pending is retryable", status: model.SweepStatusPending, want: true},
		{name: "checking activation is retryable", status: model.SweepStatusCheckingActivation, want: true},
		{name: "requesting energy is retryable", status: model.SweepStatusRequestingEnergy, want: true},
		{name: "waiting for gas is retryable", status: model.SweepStatusWaitingForGas, want: true},
		{name: "skipped no gas is retryable", status: model.SweepStatusSkippedNoGas, want: true},
		{name: "failed is retryable", status: model.SweepStatusFailed, want: true},
		{name: "waiting for energy rental is not retryable", status: model.SweepStatusWaitingForEnergyRental, want: false},
		{name: "waiting for prefund is not retryable", status: model.SweepStatusWaitingForPrefund, want: false},
		{name: "processing transaction is not retryable", status: model.SweepStatusProcessingTransaction, want: false},
		{name: "broadcasting is not retryable", status: model.SweepStatusBroadcasting, want: false},
		{name: "confirmed is not retryable", status: model.SweepStatusConfirmed, want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isRetryableSweepStatus(tt.status); got != tt.want {
				t.Fatalf("isRetryableSweepStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestPrefundInitiatedWithHashWrapsSentinel(t *testing.T) {
	t.Parallel()

	err := prefundInitiatedWithHash("  abc123  ")
	if !errors.Is(err, ErrPrefundInitiated) {
		t.Fatalf("expected ErrPrefundInitiated wrapper, got %v", err)
	}
	if got := prefundTxHashFromError(err); got != "abc123" {
		t.Fatalf("prefundTxHashFromError() = %q, want %q", got, "abc123")
	}
}

func TestPrefundTxHashFromErrorHandlesNonWrappedError(t *testing.T) {
	t.Parallel()

	if got := prefundTxHashFromError(errors.New("nope")); got != "" {
		t.Fatalf("prefundTxHashFromError() = %q, want empty", got)
	}
}

func TestSignBTCSweepInputLegacyP2PKH(t *testing.T) {
	t.Parallel()

	privKey, _ := btcec.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24,
		25, 26, 27, 28, 29, 30, 31, 32,
	})
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("NewAddressPubKeyHash() error = %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript() error = %v", err)
	}

	prevHash := chainhash.Hash{}
	prevHash[0] = 1
	outPoint := wire.OutPoint{Hash: prevHash, Index: 0}
	input := btcSweepSigningInput{
		OutPoint: outPoint,
		TxOut:    wire.NewTxOut(100_000, pkScript),
		PrivKey:  privKey,
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&outPoint, nil, nil))
	tx.AddTxOut(wire.NewTxOut(90_000, pkScript))

	fetcher := &SimpleFetcher{outputs: map[wire.OutPoint]*wire.TxOut{
		outPoint: input.TxOut,
	}}
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	if err := signBTCSweepInput(tx, sigHashes, 0, input); err != nil {
		t.Fatalf("signBTCSweepInput() error = %v", err)
	}
	if len(tx.TxIn[0].Witness) != 0 {
		t.Fatalf("expected no witness for legacy input, got %d items", len(tx.TxIn[0].Witness))
	}
	if len(tx.TxIn[0].SignatureScript) == 0 {
		t.Fatal("expected signature script for legacy input")
	}
}

func TestSignBTCSweepInputNativeSegwit(t *testing.T) {
	t.Parallel()

	privKey, _ := btcec.PrivKeyFromBytes([]byte{
		32, 31, 30, 29, 28, 27, 26, 25,
		24, 23, 22, 21, 20, 19, 18, 17,
		16, 15, 14, 13, 12, 11, 10, 9,
		8, 7, 6, 5, 4, 3, 2, 1,
	})
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash() error = %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript() error = %v", err)
	}

	prevHash := chainhash.Hash{}
	prevHash[0] = 2
	outPoint := wire.OutPoint{Hash: prevHash, Index: 1}
	input := btcSweepSigningInput{
		OutPoint: outPoint,
		TxOut:    wire.NewTxOut(100_000, pkScript),
		PrivKey:  privKey,
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&outPoint, nil, nil))
	tx.AddTxOut(wire.NewTxOut(90_000, pkScript))

	fetcher := &SimpleFetcher{outputs: map[wire.OutPoint]*wire.TxOut{
		outPoint: input.TxOut,
	}}
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	if err := signBTCSweepInput(tx, sigHashes, 0, input); err != nil {
		t.Fatalf("signBTCSweepInput() error = %v", err)
	}
	if len(tx.TxIn[0].Witness) == 0 {
		t.Fatal("expected witness for native segwit input")
	}
	if len(tx.TxIn[0].SignatureScript) != 0 {
		t.Fatalf("expected empty signature script for native segwit input, got %x", tx.TxIn[0].SignatureScript)
	}
}

func TestSignBTCSweepInputRejectsUnsupportedP2SH(t *testing.T) {
	t.Parallel()

	privKey, _ := btcec.PrivKeyFromBytes([]byte{
		10, 11, 12, 13, 14, 15, 16, 17,
		18, 19, 20, 21, 22, 23, 24, 25,
		26, 27, 28, 29, 30, 31, 32, 33,
		34, 35, 36, 37, 38, 39, 40, 41,
	})
	redeemScript := []byte{txscript.OP_TRUE}
	scriptHash := btcutil.Hash160(redeemScript)
	addr, err := btcutil.NewAddressScriptHashFromHash(scriptHash, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("NewAddressScriptHashFromHash() error = %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript() error = %v", err)
	}

	prevHash := chainhash.Hash{}
	prevHash[0] = 3
	outPoint := wire.OutPoint{Hash: prevHash, Index: 0}
	input := btcSweepSigningInput{
		OutPoint: outPoint,
		TxOut:    wire.NewTxOut(100_000, pkScript),
		PrivKey:  privKey,
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&outPoint, nil, nil))
	tx.AddTxOut(wire.NewTxOut(90_000, pkScript))
	fetcher := &SimpleFetcher{outputs: map[wire.OutPoint]*wire.TxOut{
		outPoint: input.TxOut,
	}}
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	if err := signBTCSweepInput(tx, sigHashes, 0, input); err == nil {
		t.Fatal("expected unsupported script error, got nil")
	}
}

func TestEstimateSignedBTCSweepFeeSatsDifferentiatesScriptTypes(t *testing.T) {
	t.Parallel()

	legacyKey, _ := btcec.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24,
		25, 26, 27, 28, 29, 30, 31, 32,
	})
	legacyAddr, _ := btcutil.NewAddressPubKeyHash(btcutil.Hash160(legacyKey.PubKey().SerializeCompressed()), &chaincfg.MainNetParams)
	legacyScript, _ := txscript.PayToAddrScript(legacyAddr)
	legacyHash := chainhash.Hash{}
	legacyHash[0] = 4
	legacyInput := btcSweepSigningInput{
		OutPoint: wire.OutPoint{Hash: legacyHash, Index: 0},
		TxOut:    wire.NewTxOut(100_000, legacyScript),
		PrivKey:  legacyKey,
	}

	segwitKey, _ := btcec.PrivKeyFromBytes([]byte{
		32, 31, 30, 29, 28, 27, 26, 25,
		24, 23, 22, 21, 20, 19, 18, 17,
		16, 15, 14, 13, 12, 11, 10, 9,
		8, 7, 6, 5, 4, 3, 2, 1,
	})
	segwitAddr, _ := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(segwitKey.PubKey().SerializeCompressed()), &chaincfg.MainNetParams)
	segwitScript, _ := txscript.PayToAddrScript(segwitAddr)
	segwitHash := chainhash.Hash{}
	segwitHash[0] = 5
	segwitInput := btcSweepSigningInput{
		OutPoint: wire.OutPoint{Hash: segwitHash, Index: 0},
		TxOut:    wire.NewTxOut(100_000, segwitScript),
		PrivKey:  segwitKey,
	}

	legacyFee, err := estimateSignedBTCSweepFeeSats([]btcSweepSigningInput{legacyInput}, legacyScript, legacyScript, 90_000, false, 10)
	if err != nil {
		t.Fatalf("estimateSignedBTCSweepFeeSats(legacy) error = %v", err)
	}
	segwitFee, err := estimateSignedBTCSweepFeeSats([]btcSweepSigningInput{segwitInput}, segwitScript, segwitScript, 90_000, false, 10)
	if err != nil {
		t.Fatalf("estimateSignedBTCSweepFeeSats(segwit) error = %v", err)
	}
	if legacyFee <= 0 || segwitFee <= 0 {
		t.Fatalf("expected positive fees, got legacy=%d segwit=%d", legacyFee, segwitFee)
	}
	if segwitFee >= legacyFee {
		t.Fatalf("expected segwit fee < legacy fee, got legacy=%d segwit=%d", legacyFee, segwitFee)
	}
}

func TestFilterProcessableSweepsForMode(t *testing.T) {
	t.Parallel()

	freshPending := &model.Sweep{ID: "fresh", Status: model.SweepStatusPending, Attempts: 0}
	resumedPending := &model.Sweep{ID: "resumed", Status: model.SweepStatusPending, Attempts: 1}
	waitingForGas := &model.Sweep{ID: "gas", Status: model.SweepStatusWaitingForGas, Attempts: 2}

	autoDisabled := filterProcessableSweepsForMode(false, []*model.Sweep{
		freshPending,
		resumedPending,
		waitingForGas,
		nil,
	})
	if len(autoDisabled) != 2 {
		t.Fatalf("expected 2 sweeps with auto-sweep disabled, got %d", len(autoDisabled))
	}
	if autoDisabled[0].ID != "resumed" || autoDisabled[1].ID != "gas" {
		t.Fatalf("unexpected filtered sweeps with auto-sweep disabled: %#v", autoDisabled)
	}

	autoEnabled := filterProcessableSweepsForMode(true, []*model.Sweep{
		freshPending,
		resumedPending,
		waitingForGas,
		nil,
	})
	if len(autoEnabled) != 3 {
		t.Fatalf("expected 3 sweeps with auto-sweep enabled, got %d", len(autoEnabled))
	}
}

func TestIsTronEnergyRentalActive(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	providerName := trxForEnergyProviderName
	activeStatus := string(TronEnergyRentalStatusActive)
	pendingStatus := string(TronEnergyRentalStatusPending)
	futureExpiry := now.Add(30 * time.Minute)
	pastExpiry := now.Add(-30 * time.Minute)

	tests := []struct {
		name  string
		sweep *model.Sweep
		want  bool
	}{
		{
			name: "active provider with future expiry is active",
			sweep: &model.Sweep{
				ProviderName:          &providerName,
				ProviderStatus:        &activeStatus,
				EnergyRentalExpiresAt: &futureExpiry,
			},
			want: true,
		},
		{
			name: "pending provider is not active",
			sweep: &model.Sweep{
				ProviderName:          &providerName,
				ProviderStatus:        &pendingStatus,
				EnergyRentalExpiresAt: &futureExpiry,
			},
			want: false,
		},
		{
			name: "expired active rental is not active",
			sweep: &model.Sweep{
				ProviderName:          &providerName,
				ProviderStatus:        &activeStatus,
				EnergyRentalExpiresAt: &pastExpiry,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isTronEnergyRentalActive(tt.sweep, now); got != tt.want {
				t.Fatalf("isTronEnergyRentalActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasTronEnergyRentalExpired(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	if !hasTronEnergyRentalExpired(&model.Sweep{EnergyRentalExpiresAt: &past}, now) {
		t.Fatal("expected past expiry to be expired")
	}
	if hasTronEnergyRentalExpired(&model.Sweep{EnergyRentalExpiresAt: &future}, now) {
		t.Fatal("expected future expiry to remain active")
	}
	if hasTronEnergyRentalExpired(&model.Sweep{}, now) {
		t.Fatal("expected nil expiry to be non-expired")
	}
}

func TestExactNativeUnitsFromSweepAmount(t *testing.T) {
	t.Parallel()

	units, err := exactNativeUnitsFromSweepAmount(decimal.RequireFromString("0.015"), 9, "TON")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if units.String() != "15000000" {
		t.Fatalf("expected 15000000, got %s", units.String())
	}

	if _, err := exactNativeUnitsFromSweepAmount(decimal.RequireFromString("0.0000000011"), 9, "TON"); err == nil {
		t.Fatal("expected precision-loss error, got nil")
	}
}

func TestExactSatoshisFromSweepAmount(t *testing.T) {
	t.Parallel()

	sats, err := exactSatoshisFromSweepAmount(decimal.RequireFromString("0.00012345"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sats != 12345 {
		t.Fatalf("expected 12345 sats, got %d", sats)
	}
}

func TestSubtractFeeFromQueuedAmounts(t *testing.T) {
	t.Parallel()

	t.Run("big.Int helper subtracts fee", func(t *testing.T) {
		t.Parallel()

		got, err := subtractFeeFromQueuedUnits(big.NewInt(1000), big.NewInt(210), "ETH")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.String() != "790" {
			t.Fatalf("expected 790, got %s", got.String())
		}
	})

	t.Run("uint64 helper rejects underfunded queue", func(t *testing.T) {
		t.Parallel()

		if _, err := subtractFeeFromQueuedUint64(5000, 5000, "SOL"); err == nil {
			t.Fatal("expected insufficient queued amount error")
		}
	})

	t.Run("int64 helper subtracts fee", func(t *testing.T) {
		t.Parallel()

		got, err := subtractFeeFromQueuedInt64(12000, 1000, "BTC")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 11000 {
			t.Fatalf("expected 11000, got %d", got)
		}
	})
}

func TestExactNativeAmountWithFeeStaysAboveDust(t *testing.T) {
	t.Parallel()

	hotOutput, err := subtractFeeFromQueuedInt64(10_000, 900, "BTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hotOutput <= btcDustThresholdSats {
		t.Fatalf("expected dust-safe hot output, got %d", hotOutput)
	}
}
