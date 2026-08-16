package service

import (
	"context"
	"errors"
	"testing"
)

func TestValidateWebhookURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr error
	}{
		{
			name:   "public ip is allowed",
			rawURL: "https://8.8.8.8/webhook",
		},
		{
			name:    "loopback ip is rejected",
			rawURL:  "https://127.0.0.1/webhook",
			wantErr: ErrWebhookURLUnsafe,
		},
		{
			name:    "private ip is rejected",
			rawURL:  "https://10.0.0.5/webhook",
			wantErr: ErrWebhookURLUnsafe,
		},
		{
			name:    "plaintext http is rejected",
			rawURL:  "http://8.8.8.8/webhook",
			wantErr: errors.New("webhook url must use https"),
		},
		{
			name:    "invalid scheme is rejected",
			rawURL:  "ftp://8.8.8.8/webhook",
			wantErr: errors.New("webhook url must use https"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWebhookURL(context.Background(), tt.rawURL)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("ValidateWebhookURL(%q) returned unexpected error: %v", tt.rawURL, err)
			}
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("ValidateWebhookURL(%q) returned nil error, want %v", tt.rawURL, tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) && err.Error() != tt.wantErr.Error() {
					t.Fatalf("ValidateWebhookURL(%q) error = %v, want %v", tt.rawURL, err, tt.wantErr)
				}
			}
		})
	}
}
