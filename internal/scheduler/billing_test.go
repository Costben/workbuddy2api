// billing_test.go 积分轮询排程（patch 0002）测试：
// global 账号余额刷新、CoolHard 定向解冻、失败不阻断、ctx 取消快速退出。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestRunBillingRefreshesGlobalCredits 核心验收：global 账号（不签到，credits 曾是
// 终身冻结的死数字）经 billing 轮询后拿到权威余额，并驱动 SG 闸门出闸。
func TestRunBillingRefreshesGlobalCredits(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 0
	t.Cleanup(func() { billingAccountDelay = old })

	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.SetSGGate(true, "sg-model", 20, 100, "global")
	// 轮询前：余额未知 → 保守进闸（打不了付费模型）。
	if a := p.PickByUIDForModel("g1", "sg-model"); a != nil {
		t.Fatal("轮询前余额未知的 global 号不应可选付费模型")
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBillingNow()

	st, _ := p.Status("g1")
	if !st.CreditsKnown {
		t.Errorf("billing 轮询后 credits_known 应为 true: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500（权威余额未写入）", st.Credits)
	}
	if st.SGGated {
		t.Errorf("余额 500 >= resume 100，应已出闸: %+v", st)
	}
	if a := p.PickByUIDForModel("g1", "sg-model"); a == nil {
		t.Error("余额充足后应恢复付费模型可用")
	}
}

// TestRunBillingRevivesHardCreditCooldown billing 核查发现余额恢复 → 定向解冻 CoolHard。
func TestRunBillingRevivesHardCreditCooldown(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 0
	t.Cleanup(func() { billingAccountDelay = old })

	f := &fakeUpstream{resourceRemain: 300}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolHard, 12*time.Hour, "余额不足")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBillingNow()

	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("余额恢复后 CoolHard 应被定向解冻: %+v", st)
	}
}

// TestRunBillingSoftCooldownUntouched billing 核查**不得**解冻软冷却（429）：
// 否则会形成「轮询一次解冻一次、再撞一次 429」的循环。
func TestRunBillingSoftCooldownUntouched(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 0
	t.Cleanup(func() { billingAccountDelay = old })

	f := &fakeUpstream{resourceRemain: 300}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBillingNow()

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("软冷却不应被 billing 核查解冻: %+v", st)
	}
}

// TestRunBillingSingleFailureDoesNotAbort 单号失败不阻断其余账号。
func TestRunBillingSingleFailureDoesNotAbort(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 0
	t.Cleanup(func() { billingAccountDelay = old })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			http.Error(w, "not found", 404)
			return
		}
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "boom", 500) // 第一个号失败
			return
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":0}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBillingNow()

	// 两个号都发起了请求（u1 失败不阻断 u2），且 u2 的余额落库。
	if calls.Load() != 2 {
		t.Errorf("upstream calls=%d want 2（单号失败不应阻断遍历）", calls.Load())
	}
	if st, _ := p.Status("u2"); st.Credits != 700 {
		t.Errorf("u2 credits=%d want 700", st.Credits)
	}
}

// TestRunBillingSkipsDisabled 禁用账号跳过（凭证可能已死，核查只会白刷 WARN）。
func TestRunBillingSkipsDisabled(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 0
	t.Cleanup(func() { billingAccountDelay = old })

	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "12153 session dead")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBillingNow()

	if st, _ := p.Status("u1"); st.Credits != 0 {
		t.Errorf("禁用账号不应被核查（credits=%d）", st.Credits)
	}
}

// TestRunBillingCtxCancel billing 遍历随 ctx 取消立即退出（不睡满剩余账号的限速）。
func TestRunBillingCtxCancel(t *testing.T) {
	old := billingAccountDelay
	billingAccountDelay = 30 * time.Second // 人为放大：取消后不应等它睡满
	t.Cleanup(func() { billingAccountDelay = old })

	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runBilling(ctx); close(done) }()

	time.Sleep(100 * time.Millisecond) // 让第一个账号查完，进入账号间限速
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 runBilling 未及时退出（限速睡眠未被打断）")
	}
}

// TestBillingScheduledByDefault 默认排程含 billing，且是**固定 30m 间隔**
// （槽位落在 :00/:30，而非整点小时表）。
func TestBillingScheduledByDefault(t *testing.T) {
	s := New(Config{CheckinHours: []int{9, 21}})
	if s.cfg.BillingInterval != 30*time.Minute {
		t.Fatalf("BillingInterval=%v want 30m（缺省应被 New 填充）", s.cfg.BillingInterval)
	}
	if s.cfg.BillingDisabled {
		t.Fatal("billing 缺省应启用")
	}
	// 00:30 起算：最近槽位是 01:00（cat 也在这一时刻，两者同刻合批）。
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 0, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskBilling) {
		t.Errorf("kinds=%v 01:00 应含 billing（30m 间隔的整点槽位）", kinds)
	}
	// 00:45 起算：最近的 billing 槽位是 01:00，cat 在 01:00，仍是同刻。
	// 关键在于 01:30 这个**非整点**槽位也要被排到——这是固定间隔与整点小时表的差别。
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 1, 5, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 1, 30, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v（30m 间隔的半整点槽位）", at, want)
	}
	if !hasKind(kinds, taskBilling) {
		t.Errorf("kinds=%v 01:30 应含 billing", kinds)
	}
}

// TestBillingIntervalConfigurable 间隔可配（如 15m），槽位随之变密；
// 低于下限时钳到下限（避免对上游高频连发）。
func TestBillingIntervalConfigurable(t *testing.T) {
	s := New(Config{BillingInterval: 15 * time.Minute})
	if got := s.billingMinutes(); len(got) != 96 {
		t.Fatalf("15m 间隔应铺 96 个槽位，got %d", len(got))
	}
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 1, 5, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 1, 15, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskBilling) {
		t.Errorf("kinds=%v 应含 billing", kinds)
	}

	// 配到下限之下（1m）→ 钳到下限 10m。
	s2 := New(Config{BillingInterval: time.Minute})
	if s2.cfg.BillingInterval != defaultBillingMinInterval {
		t.Fatalf("1m 应被钳到 %v，got %v", defaultBillingMinInterval, s2.cfg.BillingInterval)
	}
	if got := s2.billingMinutes(); len(got) != 144 {
		t.Fatalf("10m 间隔应铺 144 个槽位，got %d", len(got))
	}
}

// TestBillingDisabledNoSlots 关闭 billing 后不产生任何槽位，
// 且 dueSlots 不再枚举它（否则补跑路径会把关掉的任务跑起来）。
func TestBillingDisabledNoSlots(t *testing.T) {
	s := New(Config{BillingDisabled: true})
	if got := s.billingMinutes(); got != nil {
		t.Fatalf("billing 关闭时不应有槽位，got %v", got)
	}
	for _, d := range s.dueSlots(time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local),
		time.Date(2026, 9, 11, 23, 59, 0, 0, time.Local)) {
		if hasKind(d.kinds, taskBilling) {
			t.Fatalf("billing 已关闭，补跑槽位不应含它: %v", d)
		}
	}
}

// TestBillingDueSlotsCatchUp 睡过一段时间后，跨过的 billing 槽位按时间顺序补跑
// （30m 间隔下睡眠 2 小时应补 4 个槽位）。
func TestBillingDueSlotsCatchUp(t *testing.T) {
	s := New(Config{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
	got := s.dueSlots(
		time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local),
		time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local))
	want := []time.Time{
		time.Date(2026, 9, 11, 10, 30, 0, 0, time.Local),
		time.Date(2026, 9, 11, 11, 0, 0, 0, time.Local),
		time.Date(2026, 9, 11, 11, 30, 0, 0, time.Local),
		time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local),
	}
	if len(got) != len(want) {
		t.Fatalf("补跑槽位数=%d want %d: %v", len(got), len(want), got)
	}
	for i, d := range got {
		if !d.planned.Equal(want[i]) {
			t.Errorf("槽位[%d]=%v want %v", i, d.planned, want[i])
		}
		if !hasKind(d.kinds, taskBilling) {
			t.Errorf("槽位[%d] 应含 billing: %v", i, d.kinds)
		}
	}
}
