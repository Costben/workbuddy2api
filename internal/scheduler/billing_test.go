package scheduler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// billingBody 拼一个符合上游信封（{code,msg,data}）的余额查询响应。
// 信封外层不能省：doJSON 只取 data 字段，直接给 {"Response":...} 会被当成空 data，
// 表现为 "resource parse: unexpected end of JSON input"。
func billingBody(accounts string) string {
	return `{"code":0,"msg":"","data":{"Response":{"Data":{"Accounts":[` + accounts + `]}}}}`
}

// billingFake 起一个只应答余额查询的假上游，返回调用计数。
// 任何路径都回 200：global 的 billing 路径 404 时会再打一次 /v2 fallback，
// 那会让「一次刷新 = 一次调用」的断言失真。
func billingFake(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// billingScheduler 组装一个只挂 fake 上游的调度器。
func billingScheduler(srv *httptest.Server, p *pool.Pool, cfg Config) *Scheduler {
	up := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true,
	}
	cfg.Pool = p
	cfg.Upstream = up
	return New(cfg)
}

func globalAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Nickname: uid, AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
}

// TestBillingRefreshWritesCreditsAndExpiring 周期刷新必须把「余额总量」与「快过期
// 子集」一起写进账本——选号权重两项都用，只写总量等于快过期加成永远为 0。
func TestBillingRefreshWritesCreditsAndExpiring(t *testing.T) {
	// 一个 1 天后到期的套餐（窗口 7 天 → 计入 Expiring）+ 一个无到期套餐（Stable）。
	// 上游时间字段是 UTC+8 墙钟（与 upstream.softRateResetLoc 同口径）。
	cst := time.FixedZone("UTC+8", 8*60*60)
	soon := time.Now().In(cst).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	body := billingBody(`{"CapacityRemain":55,"CycleEndTime":"` + soon + `"},` +
		`{"CapacityRemain":248}`)
	srv, calls := billingFake(t, body)

	p := pool.New("")
	p.Add(globalAuth("g1"))
	s := billingScheduler(srv, p, Config{ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.refreshGlobalCredits(context.Background())

	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls=%d want 1（一账号一次只读查询）", n)
	}
	st := p.List()
	if len(st) != 1 {
		t.Fatalf("accounts=%d want 1", len(st))
	}
	if st[0].Credits != 303 {
		t.Errorf("credits=%d want 303", st[0].Credits)
	}
	if st[0].CreditsExpiring != 55 {
		t.Errorf("credits_expiring=%d want 55", st[0].CreditsExpiring)
	}
}

// TestBillingRefreshSkipsCNAccounts CN 账号的余额由签到轮回填，周期刷新不得再打
// 一次上游（双份请求只增加风控暴露，不增加信息）。
func TestBillingRefreshSkipsCNAccounts(t *testing.T) {
	srv, calls := billingFake(t, billingBody(`{"CapacityRemain":100}`))

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := billingScheduler(srv, p, Config{ExpiringSoonWindow: time.Hour})

	s.refreshGlobalCredits(context.Background())

	if n := calls.Load(); n != 0 {
		t.Fatalf("upstream calls=%d want 0（CN 账号不在周期刷新范围内）", n)
	}
	if st := p.List(); st[0].Credits != 0 {
		t.Errorf("credits=%d want 0（CN 账本不应被这条循环改写）", st[0].Credits)
	}
}

// TestBillingRefreshDoesNotClearCooldowns 只写账本，不解冻：解冻会清 429 软冷却与
// 6004 模型级冷却，周期刷新若每 30 分钟清一次，等于把上游刚给的限流信号抹掉。
func TestBillingRefreshDoesNotClearCooldowns(t *testing.T) {
	srv, _ := billingFake(t, billingBody(`{"CapacityRemain":500}`))

	p := pool.New("")
	p.Add(globalAuth("g1"))
	p.Cooldown("g1", pool.CoolSoft, time.Hour, "429 rate limited")
	s := billingScheduler(srv, p, Config{ExpiringSoonWindow: time.Hour})

	s.refreshGlobalCredits(context.Background())

	st := p.List()[0]
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500（余额照写）", st.Credits)
	}
	if !st.Cooling {
		t.Error("cooling=false want true（周期刷新不得解冻，解冻归签到轮）")
	}
}

// TestBillingRefreshSurvivesUpstreamError 单个账号查询失败不影响其余账号，也不写账本。
func TestBillingRefreshSurvivesUpstreamError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, billingBody(`{"CapacityRemain":120}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(globalAuth("g1"))
	p.Add(globalAuth("g2"))
	s := billingScheduler(srv, p, Config{ExpiringSoonWindow: time.Hour})

	s.refreshGlobalCredits(context.Background())

	// 一个失败、一个成功：成功的那个必须已写入，失败的那个保持原值。
	st := p.List()
	var got int64
	for _, one := range st {
		got += one.Credits
	}
	if got != 120 {
		t.Errorf("sum(credits)=%d want 120（一个失败一个成功）", got)
	}
}

// TestRunBillingDisabledReturnsImmediately billing_enabled=false（或间隔非正）时
// 循环必须立刻退出，一次上游都不打——零值 Config 保持旧行为。
func TestRunBillingDisabledReturnsImmediately(t *testing.T) {
	srv, calls := billingFake(t, billingBody(`{"CapacityRemain":1}`))

	for _, cfg := range []Config{{BillingDisabled: true, BillingInterval: time.Minute},
		{BillingInterval: 0}} {
		p := pool.New("")
		p.Add(globalAuth("g1"))
		s := billingScheduler(srv, p, cfg)

		done := make(chan struct{})
		go func() { s.RunBilling(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunBilling 未在禁用时立刻返回")
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}
}

// TestRunBillingCancelsDuringFirstDelay ctx 取消时首刷延迟必须可打断（否则停机要
// 等满 2 分钟）。
func TestRunBillingCancelsDuringFirstDelay(t *testing.T) {
	srv, calls := billingFake(t, billingBody(`{"CapacityRemain":1}`))
	p := pool.New("")
	p.Add(globalAuth("g1"))
	s := billingScheduler(srv, p, Config{BillingInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunBilling(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunBilling 未响应 ctx 取消")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("upstream calls=%d want 0（首刷延迟内被取消）", n)
	}
}
