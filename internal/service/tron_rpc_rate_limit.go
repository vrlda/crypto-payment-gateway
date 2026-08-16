package service

import (
	"context"
	"sync"
	"time"
)

var (
	tronRPCLimiterMu      sync.Mutex
	tronRPCLastRequestAt  time.Time
	tronRPCMinInterval    = 1200 * time.Millisecond
	tronRPCRetryLimit     = 4
	tronRPCRetryBaseDelay = 1500 * time.Millisecond
)

func tronWaitForRateLimitSlot(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	tronRPCLimiterMu.Lock()
	defer tronRPCLimiterMu.Unlock()

	if tronRPCMinInterval > 0 && !tronRPCLastRequestAt.IsZero() {
		wait := tronRPCMinInterval - time.Since(tronRPCLastRequestAt)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	tronRPCLastRequestAt = time.Now()
	return nil
}
