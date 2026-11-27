package scheduler

// 0058 保活闸门的回归测试。
//
// 覆盖 review 要求的行为：
//   - 远期账号（AT 还有 ~59 天）不刷新；
//   - 已过期（ExpiresAt < now）仍然刷新；
//   - 429 失败不推进 session_dead 计数（连续多次 429 也不禁用）；
//   - 非 429 的 12153 session dead 仍按连续计数禁用；
//   - 最大刷新年龄：AT 还很久但 auth 文件 15 天没写过 → 刷新（RT 至少 14 天轮换一次）；
//   - expiresAt=0 的存量号：24h 内不刷、超过 24h 才刷（否则等于每轮全刷，闸门失效）。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

const testDay = 24 * time.Hour

// newRefreshRecorder 起一个只处理 refresh 的假上游，返回服务与刷新调用计数。
// status/body 由用例指定：200 成功、429 限流、401+12153 session dead。
func newRefreshRecorder(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/token/refresh") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// writeAuthFile 落一份最小 auth 文件，并把 mtime 设为 at（模拟「上次刷新时刻」）。
func writeAuthFile(t *testing.T, uid string, expiresAt int64, at time.Time) (*auth.Auth, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-"+uid+".json")
	doc := fmt.Sprintf(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d},"account":{"uid":%q}}`, expiresAt, uid)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	a := &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiresAt, FilePath: path}
	return a, path
}

// newKeepaliveScheduler 用一个假上游 + 账号池构建 scheduler（与 refresh_race_test 同构）。
func newKeepaliveScheduler(t *testing.T, srv *httptest.Server, accounts ...*auth.Auth) *Scheduler {
	t.Helper()
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

// TestKeepaliveSkipsFarFutureAccount 远期账号（AT 还有 59 天、刚写过文件）不刷新：
// 这是 429 缓解的主体收益，量级从「每轮全池」降到「临近到期的几个」。
func TestKeepaliveSkipsFarFutureAccount(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusOK, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`)
	a, _ := writeAuthFile(t, "far", time.Now().Add(59*testDay).Unix(), time.Now())
	s := newKeepaliveScheduler(t, srv, a)

	s.RunKeepaliveNow()

	if got := calls.Load(); got != 0 {
		t.Fatalf("远期账号不应发起刷新，实际 %d 次", got)
	}
}

// TestKeepaliveRefreshesExpiredAccount 已过期（ExpiresAt < now）仍然要刷：
// 闸门只能拦「还很久」的号，不能把到期号一起拦掉。
func TestKeepaliveRefreshesExpiredAccount(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusOK, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`)
	a, _ := writeAuthFile(t, "expired", time.Now().Add(-time.Hour).Unix(), time.Now())
	s := newKeepaliveScheduler(t, srv, a)

	s.RunKeepaliveNow()

	if got := calls.Load(); got != 1 {
		t.Fatalf("已过期账号应刷新一次，实际 %d 次", got)
	}
	if got := a.ExpiresAtValue(); got <= time.Now().Unix() {
		t.Fatalf("刷新成功后 ExpiresAt 应前移，实际 %d", got)
	}
}

// TestKeepaliveRateLimitDoesNotCountSessionDead 429 是上游限流、不是账号故障：
// 连续多轮 429 不得推进 session_dead 连续计数（否则 3 轮就把健康号误禁用）。
func TestKeepaliveRateLimitDoesNotCountSessionDead(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusTooManyRequests, `{"code":6004,"msg":"too many requests"}`)
	a, _ := writeAuthFile(t, "rate-limited", time.Now().Add(time.Hour).Unix(), time.Now())
	s := newKeepaliveScheduler(t, srv, a)

	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		s.RunKeepaliveNow()
	}

	st, ok := s.cfg.Pool.Status("rate-limited")
	if !ok {
		t.Fatal("账号不在池里")
	}
	if st.Disabled {
		t.Fatalf("429 不该累计 session_dead（%d 轮后账号被禁用）", pool.SessionDeadThreshold())
	}
	if got, want := calls.Load(), int32(pool.SessionDeadThreshold()); got != want {
		t.Fatalf("每轮都应尝试刷新：实际 %d 次，期望 %d 次", got, want)
	}
}

// TestKeepaliveSessionDeadStillCounts 非 429 的 12153 仍走连续计数：
// 429 的早退分支不能把真正需要禁用的死会话一起跳过。
func TestKeepaliveSessionDeadStillCounts(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`)
	a, _ := writeAuthFile(t, "session-dead", time.Now().Add(time.Hour).Unix(), time.Now())
	s := newKeepaliveScheduler(t, srv, a)

	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		s.RunKeepaliveNow()
	}

	st, ok := s.cfg.Pool.Status("session-dead")
	if !ok {
		t.Fatal("账号不在池里")
	}
	if !st.Disabled {
		t.Fatalf("连续 %d 次 12153 应禁用账号，实际仍启用", pool.SessionDeadThreshold())
	}
	if got, want := calls.Load(), int32(pool.SessionDeadThreshold()); got != want {
		t.Fatalf("达到阈值前每轮都应尝试刷新：实际 %d 次，期望 %d 次", got, want)
	}
}

// TestKeepaliveRefreshesStaleAccountByMaxAge AT 还剩 59 天，但 auth 文件 15 天没写过 →
// 必须刷新：refresh 会同时轮换 refresh-token，旧「每天全刷」顺带完成的轮换不能丢。
func TestKeepaliveRefreshesStaleAccountByMaxAge(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusOK, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`)
	a, _ := writeAuthFile(t, "stale", time.Now().Add(59*testDay).Unix(), time.Now().Add(-15*testDay))
	s := newKeepaliveScheduler(t, srv, a)

	s.RunKeepaliveNow()

	if got := calls.Load(); got != 1 {
		t.Fatalf("超过最大刷新年龄（14 天）应刷新一次，实际 %d 次", got)
	}
}

// TestKeepaliveUnknownExpiryUsesDailyGap expiresAt=0 的存量号（管理端导入会写 0）：
// NeedsRefresh 对它们恒真，直接沿用等于每轮全刷；按「上次刷新 + 24h」兜底。
func TestKeepaliveUnknownExpiryUsesDailyGap(t *testing.T) {
	srv, calls := newRefreshRecorder(t, http.StatusOK, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`)
	fresh, _ := writeAuthFile(t, "unknown-fresh", 0, time.Now())
	stale, _ := writeAuthFile(t, "unknown-stale", 0, time.Now().Add(-25*time.Hour))
	s := newKeepaliveScheduler(t, srv, fresh, stale)

	s.RunKeepaliveNow()

	if got := calls.Load(); got != 1 {
		t.Fatalf("只有超过 24h 的未知到期号应刷新，实际 %d 次", got)
	}
	if got := fresh.ExpiresAtValue(); got != 0 {
		t.Fatalf("刚写过的未知到期号不应被刷新，ExpiresAt 被改成 %d", got)
	}
	if got := stale.ExpiresAtValue(); got <= time.Now().Unix() {
		t.Fatalf("超过 24h 的未知到期号刷新后应写入真实 ExpiresAt，实际 %d", got)
	}
}
