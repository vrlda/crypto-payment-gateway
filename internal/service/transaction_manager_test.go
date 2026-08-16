package service

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestMinimumAcceptedAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expected  string
		tolerance string
		want      string
	}{
		{
			name:      "no tolerance keeps exact amount",
			expected:  "0.5",
			tolerance: "0",
			want:      "0.5",
		},
		{
			name:      "small tolerance lowers threshold",
			expected:  "100",
			tolerance: "2.5",
			want:      "97.5",
		},
		{
			name:      "negative tolerance is clamped",
			expected:  "10",
			tolerance: "-5",
			want:      "10",
		},
		{
			name:      "over one hundred tolerance is clamped",
			expected:  "10",
			tolerance: "150",
			want:      "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := minimumAcceptedAmount(
				decimal.RequireFromString(tt.expected),
				decimal.RequireFromString(tt.tolerance),
			)

			want := decimal.RequireFromString(tt.want)
			if !got.Equal(want) {
				t.Fatalf("expected %s, got %s", want, got)
			}
		})
	}
}

func TestDetectedMeetsThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		detected  string
		threshold string
		want      bool
	}{
		{
			name:      "exact match passes",
			detected:  "5",
			threshold: "5",
			want:      true,
		},
		{
			name:      "greater amount passes",
			detected:  "20",
			threshold: "10",
			want:      true,
		},
		{
			name:      "smaller amount fails",
			detected:  "5",
			threshold: "10",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := detectedMeetsThreshold(
				decimal.RequireFromString(tt.detected),
				decimal.RequireFromString(tt.threshold),
			)

			if got != tt.want {
				t.Fatalf("detectedMeetsThreshold() = %v, want %v", got, tt.want)
			}
		})
	}
}
