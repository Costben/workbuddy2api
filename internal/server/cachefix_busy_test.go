package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func TestCacheFixBusyBindingAndRecovery(t *testing.T) {
	for _, recover := range []bool{false, true} {
		p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "token-busy", ExpiresAt: 9999999999}, &auth.Auth{UID: "other", AccessToken: "token-other", ExpiresAt: 9999999999})
		p.SetMaxInFlight(1)
		p.Acquire("busy")
		sess := session.New(session.Config{Available: p.AvailableUIDs, EligibleForModel: func(m string) []string { return p.StickyEligibleUIDsForModelRealm(m, "cn") }})
		sess.Bind("session-fixture", "busy")
		calls := 0
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			calls++
			if authz != "Bearer token-busy" {
				t.Error("switched account while busy")
			}
			return 200, sseOK, true
		})
		h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, StickyWait: 80 * time.Millisecond})
		if recover {
			go func() { time.Sleep(10 * time.Millisecond); p.Release("busy") }()
		}
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-flash","messages":[],"metadata":{"session_id":"session-fixture"}}`))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if recover {
			if rr.Code != 200 || calls != 1 {
				t.Fatalf("recovery code=%d calls=%d", rr.Code, calls)
			}
			if !p.Acquire("busy") {
				t.Fatal("request lease leaked")
			}
			p.Release("busy")
		} else {
			if rr.Code != 429 || calls != 0 || rr.Header().Get("Retry-After") != "1" {
				t.Fatalf("busy code=%d calls=%d", rr.Code, calls)
			}
			p.Release("busy")
		}
		uid, _ := sess.ResolveForModel("session-fixture", "deepseek-flash")
		if uid != "busy" {
			t.Fatalf("binding=%s want busy", uid)
		}
	}
}

func TestCacheFixCanceledWaitDoesNotAcquire(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "token", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, StickyWait: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	acct, err := h.acquireSticky(ctx, "busy", "deepseek-flash", "cn")
	if acct != nil || err != context.Canceled {
		t.Fatalf("acct=%v err=%v", acct, err)
	}
	if !p.Acquire("busy") {
		t.Fatal("canceled wait acquired a lease")
	}
	p.Release("busy")
}
