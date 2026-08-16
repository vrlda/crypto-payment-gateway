package service

import (
	"testing"

	"github.com/gagliardetto/solana-go/rpc"
)

func TestSolanaSignatureConfirmationStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        *rpc.SignatureStatusesResult
		wantConfirmed bool
		wantFinalized bool
	}{
		{
			name:          "nil status",
			status:        nil,
			wantConfirmed: false,
			wantFinalized: false,
		},
		{
			name: "processed only",
			status: &rpc.SignatureStatusesResult{
				ConfirmationStatus: rpc.ConfirmationStatusProcessed,
			},
			wantConfirmed: false,
			wantFinalized: false,
		},
		{
			name: "confirmed",
			status: &rpc.SignatureStatusesResult{
				ConfirmationStatus: rpc.ConfirmationStatusConfirmed,
			},
			wantConfirmed: true,
			wantFinalized: false,
		},
		{
			name: "finalized",
			status: &rpc.SignatureStatusesResult{
				ConfirmationStatus: rpc.ConfirmationStatusFinalized,
			},
			wantConfirmed: true,
			wantFinalized: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := solanaSignatureIsConfirmed(tt.status); got != tt.wantConfirmed {
				t.Fatalf("solanaSignatureIsConfirmed() = %v, want %v", got, tt.wantConfirmed)
			}
			if got := solanaSignatureIsFinalized(tt.status); got != tt.wantFinalized {
				t.Fatalf("solanaSignatureIsFinalized() = %v, want %v", got, tt.wantFinalized)
			}
		})
	}
}
