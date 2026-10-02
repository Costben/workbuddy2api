// gate_test.go SG 积分闸门（patch 0002）测试：滞回、realm 作用域、
// 免费池不受影响（核心验收）、未知余额保守进闸、持久化往返、兜底与粘性路径。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// sgPool 构造「2 个 global + 2 个 cn」的池并开启闸门（模型 sg-model，阈值 20/100）。
func sgPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	p.SetCostExploreInterval(0) // 关停探索维度，隔离本测试的关注点
	p.SetSGGate(true, "sg-model", 20, 100, "global")
	return p
}

// TestSGGateFreePoolUnaffected 核心验收：闸门只缩小付费模型的候选集，
// 免费模型的候选集**一个都不少**。
//
// 场景正是用户描述的："SG 池子变成 3 个的时候，免费池还是 5 个"。
// 这里用 2 个 global 号模拟：一个余额耗尽被摘、一个余额充足留用；
// 免费模型下两个都必须还在。
func TestSGGateFreePoolUnaffected(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g1", 5, 0)    // 低于 20 → 摘除 sg-model
	p.SetCreditsDetailed("g2", 5000, 0) // 充足 → 留用

	// 付费模型：只剩 g2（g1 被闸门摘掉）。
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealm(nil, "sg-model", "global")
		if a == nil || a.UID != "g2" {
			t.Fatalf("第 %d 次 sg-model 选中 %v，want g2（g1 余额不足应被摘）", i, a)
		}
	}

	// 免费模型：两个 global 号都必须可选（闸门不得外溢到其他模型）。
	seen := map[string]int{}
	for i := 0; i < 60; i++ {
		a := p.PickExcludingForRealm(nil, "free-model", "global")
		if a == nil {
			t.Fatal("free-model 选号返回 nil：免费池被闸门误伤")
		}
		seen[a.UID]++
	}
	if seen["g1"] == 0 || seen["g2"] == 0 {
		t.Fatalf("免费模型分布 %v，want g1/g2 都出现（免费池必须完整）", seen)
	}
}

// TestSGGateHysteresis 滞回：跌破 20 才进闸，须回到 100 以上才出闸；
// 20~99 之间维持现状，不抖动。
func TestSGGateHysteresis(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g1", 19, 0) // < 20 → 进闸
	if !sgGatedOf(p, "g1") {
		t.Fatal("余额 19 应进闸")
	}
	// 回升到 20~99：仍在闸内（滞回上沿是 100）。
	for _, c := range []int64{20, 50, 99} {
		p.SetCreditsDetailed("g1", c, 0)
		if !sgGatedOf(p, "g1") {
			t.Fatalf("余额 %d 仍在滞回窗口内，应保持进闸（恢复阈值 100）", c)
		}
	}
	// 回到 100：出闸。
	p.SetCreditsDetailed("g1", 100, 0)
	if sgGatedOf(p, "g1") {
		t.Fatal("余额 100 应出闸（>= resume_credits）")
	}
	// 再次跌破：重新进闸（且这次是从"已出闸"状态进入，走的是下沿 20）。
	p.SetCreditsDetailed("g1", 19, 0)
	if !sgGatedOf(p, "g1") {
		t.Fatal("出闸后再次跌破 20 应重新进闸")
	}
	// 出闸后停在 50：不应进闸（下沿是 20，不是 100）。
	p.SetCreditsDetailed("g1", 100, 0)
	p.SetCreditsDetailed("g1", 50, 0)
	if sgGatedOf(p, "g1") {
		t.Fatal("已出闸的号在 50 不应进闸（下沿 20）")
	}
}

// TestSGGateUnknownCreditsConservative 余额未知（从未权威核查）→ 保守进闸；
// 首次权威余额到达后按真实值重算。这是 global 账号（不签到、credits 曾是死数字）
// 能被正确对待的关键。
func TestSGGateUnknownCreditsConservative(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g2", 5000, 0) // g2 已有权威余额，留用
	// g1 从未核查 → creditsKnown=false → 保守进闸。
	if !sgGatedOf(p, "g1") {
		t.Fatal("余额未知的新号应保守进闸（不打付费模型）")
	}
	if a := p.PickExcludingForRealm(nil, "sg-model", "global"); a == nil || a.UID != "g2" {
		t.Fatalf("sg-model 选中 %v，want g2（g1 余额未知应被挡）", a)
	}
	// 权威核查到达且余额充足 → 立即解闸，无需重启或人工干预。
	p.SetCreditsDetailed("g1", 800, 0)
	if sgGatedOf(p, "g1") {
		t.Fatal("权威余额 800 到达后应立即出闸")
	}
	// 权威核查到达但余额仍低 → 保持进闸（这是"未知"到"真穷"的正常过渡）。
	p.SetCreditsDetailed("g1", 3, 0)
	if !sgGatedOf(p, "g1") {
		t.Fatal("权威余额 3 应保持进闸")
	}
}

// TestSGGateRealmScoped cn 账号不在闸门作用域内：余额再低也不被摘。
// （CN 有签到兜底，本闸门默认只管 global；这是"零回归"的边界。）
func TestSGGateRealmScoped(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("cn1", 0, 0)
	p.SetCreditsDetailed("cn2", 0, 0)
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealm(nil, "sg-model", "cn")
		if a == nil {
			t.Fatal("cn 账号余额 0 也被摘：闸门 realm 作用域失效")
		}
	}
	// 闸门位对 cn 账号恒 false（不残留）。
	if sgGatedOf(p, "cn1") {
		t.Fatal("cn 账号不应被置 sgGated")
	}
}

// TestSGGateDisabledNoop 闸门关闭（enabled=false）时完全回到引入前行为：
// 余额 0 的 global 号照常参与付费模型选号。
func TestSGGateDisabledNoop(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCostExploreInterval(0)
	p.SetSGGate(false, "sg-model", 20, 100, "global") // 关闭
	p.SetCreditsDetailed("g1", 0, 0)
	if sgGatedOf(p, "g1") {
		t.Fatal("闸门关闭时不应置 sgGated")
	}
	if a := p.PickExcludingForRealm(nil, "sg-model", "global"); a == nil || a.UID != "g1" {
		t.Fatalf("闸门关闭时 g1 应可选，got %v", a)
	}
}

// TestSGGatePersistRoundTrip 闸门位与 creditsKnown 跨重启保留：
// 否则重启会把刚摘掉的号放回付费模型，重演"打空积分连免费一起死"。
func TestSGGatePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})
	p.SetCostExploreInterval(0)
	p.SetSGGate(true, "sg-model", 20, 100, "global")
	p.SetCreditsDetailed("g1", 5, 0)   // 进闸
	p.SetCreditsDetailed("g2", 900, 0) // 出闸
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p2.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})
	// 注入配置后重算：g1 仍是 5（进闸），g2 仍是 900（出闸）。
	p2.SetCostExploreInterval(0)
	p2.SetSGGate(true, "sg-model", 20, 100, "global")
	if !sgGatedOf(p2, "g1") {
		t.Fatal("g1 的闸门位未跨重启保留（余额 5）")
	}
	if sgGatedOf(p2, "g2") {
		t.Fatal("g2 不应进闸（余额 900）")
	}
	// creditsKnown 也应保留（否则会被当成"未知"重新保守进闸一遍）。
	if !creditsKnownOf(p2, "g1") {
		t.Fatal("creditsKnown 未跨重启保留")
	}
}

// TestSGGateFallbackExcludesGated 全冷却兜底路径同样不得放行被摘除的号：
// 否则 normal 阶段被正确摘掉的号会在"全池冷却"时从兜底漏回付费模型。
//
// 对照设计：g1（余额不足，闸门摘除）+ g2（6004 模型级冷却）——
// normal 无候选，走兜底；付费模型下两者都不该被选中（g1 被闸门挡、g2 被模型冷却挡），
// 而免费模型下 g2 的模型冷却不适用，兜底应能选中它（闸门只管 sg-model）。
func TestSGGateFallbackExcludesGated(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g1", 1, 0)    // 进闸
	p.SetCreditsDetailed("g2", 5000, 0) // 留用
	// g2 对 sg-model 处于 6004 模型级冷却（账号整体仍健康）→ normal 无候选。
	p.CooldownSoftForModel("g2", time.Minute, time.Now().Add(time.Hour), "sg-model", "6004 model rate limit")
	if a := p.PickExcludingForRealm(nil, "sg-model", "global"); a != nil {
		t.Fatalf("兜底选中 %v，want nil（g1 被闸门摘、g2 对 sg-model 模型级冷却）", a)
	}
	// 对照：免费模型下 g2 的 sg-model 冷却不适用（g1 也未被闸门摘）→ 兜底有候选。
	// 注意此时两个号账号级都健康，走的是 normal 而非兜底，故断言"能选到号"即可。
	if a := p.PickExcludingForRealm(nil, "free-model", "global"); a == nil {
		t.Fatal("免费模型不应为空（g1/g2 账号级都健康，sg-model 冷却不外溢）")
	}
}

// TestSGGateFallbackPathExcludesGated 直接锚定兜底函数本身：当全池都进冷却、
// 只剩被闸门摘除的号时，付费模型的兜底必须返回 nil（而不是把闸门号放回去）。
func TestSGGateFallbackPathExcludesGated(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g1", 1, 0)    // 进闸
	p.SetCreditsDetailed("g2", 5000, 0) // 未进闸
	// 两个号都进账号级软冷却 → normal 无候选，强制走兜底。
	p.Cooldown("g1", CoolSoft, time.Minute, "429 rate limit")
	p.Cooldown("g2", CoolSoft, time.Minute, "429 rate limit")
	// 免费模型：兜底可选（两个号都未被闸门摘，冷却中允许兜底）。
	if a := p.PickExcludingForRealm(nil, "free-model", "global"); a == nil {
		t.Fatal("免费模型兜底应能选中冷却中的号")
	}
	// 付费模型：g1 被闸门摘，g2 是唯一合法兜底候选。
	// 把 g2 也摘掉（余额降到阈值下）后，兜底必须为空——证明闸门在兜底路径生效。
	p.SetCreditsDetailed("g2", 1, 0)
	if a := p.PickExcludingForRealm(nil, "sg-model", "global"); a != nil {
		t.Fatalf("付费模型兜底选中 %v，want nil（两号都被闸门摘）", a)
	}
}

// TestSGGateStickyAndAvailable 粘性命中与可用集合两条旁路同样尊重闸门：
// 否则被摘除的号会被绑定会话持续打付费模型。
func TestSGGateStickyAndAvailable(t *testing.T) {
	p := sgPool(t)
	p.SetCreditsDetailed("g1", 1, 0)    // 进闸
	p.SetCreditsDetailed("g2", 5000, 0) // 留用

	if a := p.PickByUIDForModel("g1", "sg-model"); a != nil {
		t.Fatalf("粘性命中放行了被摘除的 g1：%v", a)
	}
	if a := p.PickByUIDForModel("g2", "sg-model"); a == nil {
		t.Fatal("粘性命中误伤了未进闸的 g2")
	}
	// 免费模型下粘性命中不受影响。
	if a := p.PickByUIDForModel("g1", "free-model"); a == nil {
		t.Fatal("免费模型的粘性命中被闸门误伤")
	}
	// 可用集合：sg-model 下不含 g1，free-model 下含 g1。
	if uids := p.AvailableUIDsForModelRealm("sg-model", "global"); len(uids) != 1 || uids[0] != "g2" {
		t.Fatalf("AvailableUIDsForModelRealm(sg-model)=%v want [g2]", uids)
	}
	if uids := p.AvailableUIDsForModelRealm("free-model", "global"); len(uids) != 2 {
		t.Fatalf("AvailableUIDsForModelRealm(free-model)=%v want 2 个", uids)
	}
}

// TestSGGateThresholdChangeRecomputes 运维改阈值后立即重算全池闸门位
// （不必等下一轮 billing 轮询）：把 min 调高应立刻摘掉边缘号。
func TestSGGateThresholdChangeRecomputes(t *testing.T) {
	p := sgPool(t)
	// 先给一个充足的权威余额让 g1 脱离"未知→保守进闸"的初始态（滞回上沿 100）。
	p.SetCreditsDetailed("g1", 150, 0)
	if sgGatedOf(p, "g1") {
		t.Fatal("余额 150 应出闸")
	}
	p.SetCreditsDetailed("g1", 50, 0) // 阈值 20 下 50 >= 20 → 不进闸
	if sgGatedOf(p, "g1") {
		t.Fatal("余额 50 在阈值 20 下不应进闸")
	}
	// 阈值抬到 100：50 < 100 → 立刻进闸。
	p.SetSGGate(true, "sg-model", 100, 200, "global")
	if !sgGatedOf(p, "g1") {
		t.Fatal("阈值抬到 100 后余额 50 应立即进闸（重算未生效）")
	}
	// 阈值回落 20：50 >= 20 → 立刻出闸（恢复阈值同样回落到 100，50 < 100，
	// 故这里必须走"未进闸"分支判定——先证明重算确实按新下沿放行）。
	p.SetSGGate(true, "sg-model", 20, 20, "global")
	if sgGatedOf(p, "g1") {
		t.Fatal("阈值回落到 20/20 后余额 50 应立即出闸（重算未生效）")
	}
}

// TestClearCreditCooldownIfRecovered 余额核查后的定向解冻：
// 只解 CoolHard（"没钱"），不碰 CoolSoft（429）。
func TestClearCreditCooldownIfRecovered(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	// u1：余额耗尽硬冷却 → 核查发现余额恢复 → 解冻。
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if !p.ClearCreditCooldownIfRecovered("u1", 500) {
		t.Fatal("CoolHard 且余额 > 0 应解冻")
	}
	if st := statusByUID(t, p, "u1"); st.Cooling {
		t.Fatalf("u1 解冻后仍显示 cooling：%+v", st)
	}

	// u2：软冷却（429）→ 核查不应对它解冻（否则解冻即撞 429 循环）。
	p.CooldownSoftRate("u2", time.Minute, time.Time{}, "429 rate limit")
	if p.ClearCreditCooldownIfRecovered("u2", 500) {
		t.Fatal("CoolSoft 不应被 billing 核查解冻")
	}
	if st := statusByUID(t, p, "u2"); !st.Cooling {
		t.Fatalf("u2 的软冷却被误清：%+v", st)
	}

	// 余额仍为 0：不解冻（钱没回来）。
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if p.ClearCreditCooldownIfRecovered("u1", 0) {
		t.Fatal("余额 0 不应解冻")
	}
}

// --- helpers ---

func sgGatedOf(p *Pool, uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && e.sgGated
}

func creditsKnownOf(p *Pool, uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && e.creditsKnown
}
