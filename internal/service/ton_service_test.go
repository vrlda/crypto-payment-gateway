package service

import (
	"context"
	"crypto_payment_gateway_core/internal/model"
	"errors"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
)

type fakeTonTxClient struct {
	account           *tlb.Account
	accountErrs       []error
	masterErrs        []error
	listErrsByLT      map[uint64][]error
	pages             map[uint64][]*tlb.Transaction
	listCalls         int
	getAccountCalls   int
	getMasterInfoCall int
}

func (f *fakeTonTxClient) CurrentMasterchainInfo(ctx context.Context) (*ton.BlockIDExt, error) {
	f.getMasterInfoCall++
	if len(f.masterErrs) > 0 {
		err := f.masterErrs[0]
		f.masterErrs = f.masterErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return &ton.BlockIDExt{}, nil
}

func (f *fakeTonTxClient) GetAccount(ctx context.Context, block *ton.BlockIDExt, addr *address.Address) (*tlb.Account, error) {
	f.getAccountCalls++
	if len(f.accountErrs) > 0 {
		err := f.accountErrs[0]
		f.accountErrs = f.accountErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return f.account, nil
}

func (f *fakeTonTxClient) ListTransactions(ctx context.Context, addr *address.Address, limit uint32, lt uint64, txHash []byte) ([]*tlb.Transaction, error) {
	f.listCalls++
	if len(f.listErrsByLT[lt]) > 0 {
		err := f.listErrsByLT[lt][0]
		f.listErrsByLT[lt] = f.listErrsByLT[lt][1:]
		if err != nil {
			return nil, err
		}
	}
	if f.pages == nil {
		return nil, nil
	}
	return f.pages[lt], nil
}

func tonTestAddress() string {
	return "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N"
}

func tonSuccessComputePhase() tlb.ComputePhase {
	return tlb.ComputePhase{
		Phase: tlb.ComputePhaseVM{
			Success: true,
		},
	}
}

func tonFailedComputePhase() tlb.ComputePhase {
	return tlb.ComputePhase{
		Phase: tlb.ComputePhaseVM{
			Success: false,
		},
	}
}

func tonSuccessActionPhase() *tlb.ActionPhase {
	return &tlb.ActionPhase{
		Success:    true,
		Valid:      true,
		ResultCode: 0,
	}
}

func tonTx(hashByte byte, lt, prevLT uint64, desc any) *tlb.Transaction {
	return &tlb.Transaction{
		Hash:        []byte{hashByte},
		LT:          lt,
		PrevTxLT:    prevLT,
		PrevTxHash:  []byte{hashByte - 1},
		Description: desc,
	}
}

func TestTonTxStateFromTransaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tx   *tlb.Transaction
		want sweepTxState
	}{
		{
			name: "ordinary success is confirmed",
			tx: tonTx(0xaa, 10, 0, tlb.TransactionDescriptionOrdinary{
				ComputePhase: tonSuccessComputePhase(),
				ActionPhase:  tonSuccessActionPhase(),
			}),
			want: sweepTxStateConfirmed,
		},
		{
			name: "aborted transaction fails",
			tx: tonTx(0xab, 10, 0, tlb.TransactionDescriptionOrdinary{
				Aborted:      true,
				ComputePhase: tonSuccessComputePhase(),
			}),
			want: sweepTxStateFailed,
		},
		{
			name: "compute failure fails",
			tx: tonTx(0xac, 10, 0, tlb.TransactionDescriptionOrdinary{
				ComputePhase: tonFailedComputePhase(),
			}),
			want: sweepTxStateFailed,
		},
		{
			name: "unsupported description is pending",
			tx:   tonTx(0xad, 10, 0, struct{}{}),
			want: sweepTxStatePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tonTxStateFromTransaction(tt.tx); got.State != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got.State)
			}
		})
	}
}

func TestTonServiceCheckAddressTxStatePaginatesAndStopsOnMatch(t *testing.T) {
	t.Parallel()

	client := &fakeTonTxClient{
		account: &tlb.Account{
			LastTxLT:   300,
			LastTxHash: []byte{0xff},
		},
		pages: map[uint64][]*tlb.Transaction{
			300: {
				tonTx(0x91, 290, 280, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
				tonTx(0x92, 300, 299, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
			},
			280: {
				tonTx(0xbb, 280, 0, tlb.TransactionDescriptionOrdinary{
					ComputePhase: tonSuccessComputePhase(),
					ActionPhase:  tonSuccessActionPhase(),
				}),
			},
		},
	}

	svc := &TonService{
		txClient: client,
	}

	status, err := svc.CheckAddressTxState(context.Background(), tonTestAddress(), "bb")
	if err != nil {
		t.Fatalf("CheckAddressTxState returned error: %v", err)
	}
	if status.State != sweepTxStateConfirmed {
		t.Fatalf("expected confirmed, got %s", status.State)
	}
	if client.listCalls != 2 {
		t.Fatalf("expected 2 history pages, got %d", client.listCalls)
	}
}

func TestTonServiceCheckAddressTxStateReturnsNotFound(t *testing.T) {
	t.Parallel()

	client := &fakeTonTxClient{
		account: &tlb.Account{
			LastTxLT:   100,
			LastTxHash: []byte{0xee},
		},
		pages: map[uint64][]*tlb.Transaction{
			100: {
				tonTx(0xaa, 100, 0, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
			},
		},
	}

	svc := &TonService{txClient: client}

	status, err := svc.CheckAddressTxState(context.Background(), tonTestAddress(), "ff")
	if err != nil {
		t.Fatalf("CheckAddressTxState returned error: %v", err)
	}
	if status.State != sweepTxStateNotFound {
		t.Fatalf("expected not found, got %s", status.State)
	}
}

func TestTonServiceWalkTransactionsRetriesTransientLiteServerErrors(t *testing.T) {
	t.Parallel()

	client := &fakeTonTxClient{
		account: &tlb.Account{
			LastTxLT:   100,
			LastTxHash: []byte{0xee},
		},
		accountErrs: []error{
			errors.New("lite server error, code 651: cannot load block"),
			nil,
		},
		listErrsByLT: map[uint64][]error{
			100: {
				errors.New("lite server error, code 500: failed to get account state"),
				nil,
			},
		},
		pages: map[uint64][]*tlb.Transaction{
			100: {
				tonTx(0xaa, 100, 0, tlb.TransactionDescriptionOrdinary{
					ComputePhase: tonSuccessComputePhase(),
					ActionPhase:  tonSuccessActionPhase(),
				}),
			},
		},
	}

	svc := &TonService{txClient: client}
	addr := address.MustParseAddr(tonTestAddress())

	seen := 0
	err := svc.walkTransactions(context.Background(), addr, func(tx *tlb.Transaction) (bool, error) {
		seen++
		return false, nil
	})
	if err != nil {
		t.Fatalf("walkTransactions returned error after transient failures: %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected one transaction after retries, saw %d", seen)
	}
	if client.getAccountCalls < 2 {
		t.Fatalf("expected GetAccount to retry, got %d calls", client.getAccountCalls)
	}
	if client.listCalls < 2 {
		t.Fatalf("expected ListTransactions to retry, got %d calls", client.listCalls)
	}
}

func TestTonServiceWalkTransactionsStopsWhenHandlerStops(t *testing.T) {
	t.Parallel()

	client := &fakeTonTxClient{
		account: &tlb.Account{
			LastTxLT:   50,
			LastTxHash: []byte{0xdd},
		},
		pages: map[uint64][]*tlb.Transaction{
			50: {
				tonTx(0x10, 40, 30, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
				tonTx(0x11, 50, 49, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
			},
			30: {
				tonTx(0x12, 30, 0, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
			},
		},
	}

	svc := &TonService{txClient: client}
	addr := address.MustParseAddr(tonTestAddress())

	seen := 0
	err := svc.walkTransactions(context.Background(), addr, func(tx *tlb.Transaction) (bool, error) {
		seen++
		return tx.LT == 50, nil
	})
	if err != nil {
		t.Fatalf("walkTransactions returned error: %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected to stop after first matching tx, saw %d txs", seen)
	}
	if client.listCalls != 1 {
		t.Fatalf("expected one page fetch, got %d", client.listCalls)
	}
}

func TestTonServiceCheckDepositTxConfirmation(t *testing.T) {
	t.Parallel()

	client := &fakeTonTxClient{
		account: &tlb.Account{
			LastTxLT:   100,
			LastTxHash: []byte{0xcc},
		},
		pages: map[uint64][]*tlb.Transaction{
			100: {
				tonTx(0xaa, 100, 0, tlb.TransactionDescriptionOrdinary{
					ComputePhase: tonSuccessComputePhase(),
					ActionPhase:  tonSuccessActionPhase(),
				}),
			},
		},
	}

	svc := &TonService{txClient: client}
	txHash := "aa"
	dep := &model.CryptoDeposit{
		DepositAddress: tonTestAddress(),
		TxHash:         &txHash,
	}

	confirmed, err := svc.CheckDepositTxConfirmation(context.Background(), dep)
	if err != nil {
		t.Fatalf("CheckDepositTxConfirmation returned error: %v", err)
	}
	if !confirmed {
		t.Fatal("expected deposit tx confirmation to be true")
	}
}

func TestTonServiceCheckDepositTxResolution(t *testing.T) {
	t.Parallel()

	t.Run("maps confirmed transaction", func(t *testing.T) {
		t.Parallel()

		client := &fakeTonTxClient{
			account: &tlb.Account{
				LastTxLT:   100,
				LastTxHash: []byte{0xcc},
			},
			pages: map[uint64][]*tlb.Transaction{
				100: {
					tonTx(0xaa, 100, 0, tlb.TransactionDescriptionOrdinary{
						ComputePhase: tonSuccessComputePhase(),
						ActionPhase:  tonSuccessActionPhase(),
					}),
				},
			},
		}

		svc := &TonService{txClient: client}
		txHash := "aa"
		dep := &model.CryptoDeposit{
			DepositAddress: tonTestAddress(),
			TxHash:         &txHash,
		}

		resolution, err := svc.CheckDepositTxResolution(context.Background(), dep)
		if err != nil {
			t.Fatalf("CheckDepositTxResolution returned error: %v", err)
		}
		if resolution != depositTxResolutionConfirmed {
			t.Fatalf("expected confirmed resolution, got %s", resolution)
		}
	})

	t.Run("maps missing transaction", func(t *testing.T) {
		t.Parallel()

		client := &fakeTonTxClient{
			account: &tlb.Account{
				LastTxLT:   100,
				LastTxHash: []byte{0xee},
			},
			pages: map[uint64][]*tlb.Transaction{
				100: {
					tonTx(0xaa, 100, 0, tlb.TransactionDescriptionOrdinary{ComputePhase: tonSuccessComputePhase()}),
				},
			},
		}

		svc := &TonService{txClient: client}
		txHash := "ff"
		dep := &model.CryptoDeposit{
			DepositAddress: tonTestAddress(),
			TxHash:         &txHash,
		}

		resolution, err := svc.CheckDepositTxResolution(context.Background(), dep)
		if err != nil {
			t.Fatalf("CheckDepositTxResolution returned error: %v", err)
		}
		if resolution != depositTxResolutionNotFound {
			t.Fatalf("expected not-found resolution, got %s", resolution)
		}
	})
}

func TestTonBounceReason(t *testing.T) {
	t.Parallel()

	reason := tonBounceReason(&tlb.BouncePhase{Phase: tlb.BouncePhaseNoFunds{}})
	if reason == "" {
		t.Fatal("expected bounce reason to be populated")
	}
}

func TestTonComputePhaseFailureReason(t *testing.T) {
	t.Parallel()

	failed, reason := tonComputePhaseFailureReason(tlb.ComputePhase{
		Phase: tlb.ComputePhaseSkipped{
			Reason: tlb.ComputeSkipReason{Type: tlb.ComputeSkipReasonNoGas},
		},
	})
	if !failed {
		t.Fatal("expected compute phase to fail")
	}
	if reason == "" {
		t.Fatal("expected compute phase failure reason")
	}
}

func TestTonActionPhaseFailureReason(t *testing.T) {
	t.Parallel()

	failed, reason := tonActionPhaseFailureReason(&tlb.ActionPhase{
		Success:    false,
		Valid:      true,
		ResultCode: 32,
	})
	if !failed {
		t.Fatal("expected action phase to fail")
	}
	if reason == "" {
		t.Fatal("expected action phase failure reason")
	}
}
