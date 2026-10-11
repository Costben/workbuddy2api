package server

import (
	"context"
	"errors"
	"time"

	"workbuddy2api/internal/auth"
)

var errStickyBusy = errors.New("sticky account remains busy")

// acquireSticky waits within a fixed budget and preserves the binding on busy
// timeout. Account failure still permits the existing failover policy.
func (h *Handler) acquireSticky(ctx context.Context, uid, model, realm string) (*auth.Auth, error) {
	timer := time.NewTimer(h.cfg.StickyWait)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		acct, eligible := h.cfg.Pool.AcquireStickyForModelRealm(uid, model, realm)
		if acct != nil || !eligible {
			return acct, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errStickyBusy
		case <-ticker.C:
		}
	}
}
