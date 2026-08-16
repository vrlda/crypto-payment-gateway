package service

import "testing"

func TestPaymentWebhookEventNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		isLate bool
		want   []string
	}{
		{
			name:   "finalized emits canonical and compatibility event",
			status: "FINALIZED",
			want:   []string{"payment.finalized", "payment.completed"},
		},
		{
			name:   "late finalized keeps late event and compatibility alias",
			status: "FINALIZED",
			isLate: true,
			want:   []string{"payment.late_finalized", "payment.completed"},
		},
		{
			name:   "confirmed emits single event",
			status: "CONFIRMED",
			want:   []string{"payment.confirmed"},
		},
		{
			name:   "late detected keeps single late event",
			status: "DETECTED",
			isLate: true,
			want:   []string{"payment.late_detected"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := paymentWebhookEventNames(tt.status, tt.isLate)
			if len(got) != len(tt.want) {
				t.Fatalf("paymentWebhookEventNames() len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("paymentWebhookEventNames()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
