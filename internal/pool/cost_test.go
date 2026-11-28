package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// noteFreeTier 写两笔 credit=0 的足量观测，使账号进入 tier 0。
// issue #491 新语义：单笔 credit=0 只是「未知」（短请求的固定底价豁免），
// 至少两笔累计为 0 才判免费。
func noteFreeTier(p *Pool, uid, model string) {
	p.NoteModelCost(uid, model, 0, modelCostMinTokens)
	p.NoteModelCost(uid, model, 0, modelCostMinTokens)
}

// TestNoteModelCostAndPreferFree 成本账本的核心验收：同一模型上已实测免费的号
// 必须优先于已实测收费的号，且这个优先级要压过积分因子。
func TestNoteModelCostAndPreferFree(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// 探索关停前置（issue #136）：本测试锚定「无探索义务时的成本优先语义」，
	// 层序语义不变，显式关停新增的 costTier 探索维度（设计报告 §4 T9）。
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid"})
	// paid 积分远高于 free：若只看积分，paid 必胜；成本分层必须压过积分。
	p.SetCredits("free", 1)
	p.SetCredits("paid", 1_000_000)

	noteFreeTier(p, "free", "hy4-preview")                          // 两笔 0 → tier 0
	p.NoteModelCost("paid", "hy4-preview", 2.9, modelCostMinTokens) // 收费

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（免费号应优先于高积分收费号）", i, a)
		}
	}
}

// TestModelCostCheaperPaidWins 同为收费时，单价低的优先。
func TestModelCostCheaperPaidWins(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// 探索关停前置（issue #136）：本测试锚定「无探索义务时的成本优先语义」，
	// 层序语义不变，显式关停新增的 costTier 探索维度（设计报告 §4 T9）。
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cheap"})
	p.Add(&auth.Auth{UID: "pricey"})
	p.NoteModelCost("cheap", "hy4-preview", 0.3, modelCostMinTokens)
	p.NoteModelCost("pricey", "hy4-preview", 5.0, modelCostMinTokens)
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "cheap" {
			t.Fatalf("选中 %v, want cheap（单价低的优先）", a)
		}
	}
}

// TestModelCostUnknownBeatsKnownPaid 无观测的号优先于已知收费的号。
// 这是学习闭环的前提：新号的限免状态只能靠实测发现，若已知收费的号恒压过未知号，
// 那台免费的号永远轮不到，也就永远学不到"它其实是免费的"。
func TestModelCostUnknownBeatsKnownPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// 探索关停前置（issue #136）：本测试锚定「无探索义务时的成本优先语义」，
	// 层序语义不变，显式关停新增的 costTier 探索维度（设计报告 §4 T9）。
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "paid"})
	p.SetCredits("unknown", 1)
	p.SetCredits("paid", 1_000_000)
	p.NoteModelCost("paid", "hy4-preview", 2.9, modelCostMinTokens)

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "unknown" {
			t.Fatalf("选中 %v, want unknown（未知号需有机会被实测）", a)
		}
	}
}

// TestModelCostFreeBeatsUnknown 已确认免费的号压过未观测的号。
func TestModelCostFreeBeatsUnknown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// 探索关停前置（issue #136）：本测试锚定「无探索义务时的成本优先语义」，
	// 层序语义不变，显式关停新增的 costTier 探索维度（设计报告 §4 T9）。
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "knownfree"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.SetCredits("knownfree", 1)
	p.SetCredits("unknown", 1_000_000)
	noteFreeTier(p, "knownfree", "hy4-preview")

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "knownfree" {
			t.Fatalf("选中 %v, want knownfree（已确认免费 > 未知）", a)
		}
	}
}

// TestModelCostStaleIgnored 过期观测失效：防止"夜间免费"在白天仍被当作免费
// （否则会一直用那个已开始收费的号）。
func TestModelCostStaleIgnored(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	noteFreeTier(p, "u1", "hy4-preview")
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["hy4-preview"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL) // 手工做旧
	p.byUID["u1"].modelCost["hy4-preview"] = mc
	p.mu.Unlock()

	p.mu.RLock()
	_, ok := p.byUID["u1"].modelCostOf("hy4-preview", time.Now())
	p.mu.RUnlock()
	if ok {
		t.Error("过期的成本观测应失效（夜间免费白天不该仍算免费）")
	}
}

// TestModelCostPrunedOnPick 过期观测必须被**回收**（不只是被忽略）。
//
// 与 TestModelCostStaleIgnored 的区别：那个只断言 modelCostOf 读回 ok=false，
// 过期条目仍留在 map 里；本测试断言 pick 写锁路径真的把它删掉——否则 modelCost
// 与 modelCooldowns「map 不无限膨胀」的口径不一致（后者有 pruneExpiredModelCooldowns）。
func TestModelCostPrunedOnPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "stale-model", 12, modelCostMinTokens)

	p.mu.Lock()
	if len(p.byUID["u1"].modelCost) != 1 {
		p.mu.Unlock()
		t.Fatalf("前置条件不成立：modelCost len=%d want 1", len(p.byUID["u1"].modelCost))
	}
	mc := p.byUID["u1"].modelCost["stale-model"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL) // 手工做旧
	p.byUID["u1"].modelCost["stale-model"] = mc
	p.mu.Unlock()

	p.Pick("") // pick 写锁路径做惰性回收

	p.mu.RLock()
	_, still := p.byUID["u1"].modelCost["stale-model"]
	n := len(p.byUID["u1"].modelCost)
	p.mu.RUnlock()
	if still {
		t.Errorf("过期 modelCost 条目未被回收（map 只增不减），len=%d", n)
	}
}

// TestModelCostCumulativeNotEMA 累计量语义（issue #491）：不再 EMA，
// CostPerReq 精确等于 TotalCredit/TotalRequests；CostPer1k 由累计 token 现算，
// 只作诊断值。
func TestModelCostCumulativeNotEMA(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 1.0, 2000) // 0.5 credits/1k
	p.NoteModelCost("u1", "m", 3.0, 6000) // 也是 0.5 credits/1k
	mc, ok := modelCostSnapshot(p, "u1", "m")
	if !ok {
		t.Fatal("成本观测未写入")
	}
	if mc.TotalCredit != 4.0 || mc.TotalRequests != 2 || mc.TotalTokens != 8000 {
		t.Fatalf("累计量=%+v, want credit=4 requests=2 tokens=8000", mc)
	}
	if got := mc.costPerReq(); got != 2.0 {
		t.Errorf("CostPerReq=%v want 2.0（4 credit / 2 requests，不是 EMA）", got)
	}
	if got := mc.per1k(); got != 0.5 {
		t.Errorf("CostPer1k=%v want 0.5（4 credit / 8000 tokens ×1000）", got)
	}
	if mc.Samples != 2 {
		t.Errorf("Samples=%d want 2", mc.Samples)
	}
}

// TestModelCostShortRequestIgnored 观测门槛（issue #491）：tokens<2000 的请求
// 不更新账本（不创建条目、不改变累计量）——一次 max_tokens=5 探测不得污染排序。
func TestModelCostShortRequestIgnored(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 首笔就是短请求：不建条目（保留 tier 1 未知）。
	p.NoteModelCost("u1", "m", 5.0, modelCostMinTokens-1)
	if _, ok := modelCostSnapshot(p, "u1", "m"); ok {
		t.Fatal("短请求不应创建成本账本条目")
	}
	// 长请求建立基线。
	p.NoteModelCost("u1", "m", 2.0, modelCostMinTokens)
	before, _ := modelCostSnapshot(p, "u1", "m")
	// 再来一笔短请求（金额很大）：累计量与 LastSeen 都不得变化。
	p.NoteModelCost("u1", "m", 99.0, 5)
	after, _ := modelCostSnapshot(p, "u1", "m")
	if after.TotalCredit != before.TotalCredit ||
		after.TotalRequests != before.TotalRequests ||
		after.TotalTokens != before.TotalTokens {
		t.Errorf("短请求改变了累计量: before=%+v after=%+v", before, after)
	}
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Errorf("短请求不应刷新 LastSeen: before=%v after=%v", before.LastSeen, after.LastSeen)
	}
}

// TestModelCostZeroCreditTierRequiresTwo 单笔 credit=0 不判免费（未知，tier 1）；
// 两笔累计 0 才进 tier 0。行为断言：两笔 0 的号压过单笔 0 的号。
func TestModelCostZeroCreditTierRequiresTwo(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "one"})
	p.NoteModelCost("one", "m", 0, modelCostMinTokens)
	one, _ := modelCostSnapshot(p, "one", "m")
	if got := one.costTier(); got != 1 {
		t.Errorf("单笔 credit=0 costTier=%d want 1（未知，不是免费）", got)
	}

	p2 := New("")
	withNoPickGap(t)
	p2.SetCostExploreInterval(0)
	p2.SetRandomSource(func(n int64) int64 { return 0 })
	p2.Add(&auth.Auth{UID: "one"})
	p2.Add(&auth.Auth{UID: "two"})
	p2.NoteModelCost("one", "m", 0, modelCostMinTokens)
	noteFreeTier(p2, "two", "m")
	two, _ := modelCostSnapshot(p2, "two", "m")
	if got := two.costTier(); got != 0 {
		t.Fatalf("两笔 credit=0 costTier=%d want 0", got)
	}
	for i := 0; i < 20; i++ {
		if a := p2.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "two" {
			t.Fatalf("第 %d 次选中 %v，want two（两笔 0 = tier 0；单笔 0 = tier 1）", i, a)
		}
	}
}

// TestModelCostPerRequestOrdering 同样 credit、不同请求长度 → per-req 相同，
// 不应再出现旧 per1k 口径下 1300 倍的「长请求账号更便宜」假象。
//
// 只使用公开 API，便于在 0058 基线上做反向验证（旧实现按 per1k 排序会选中
// b-long，本测试会失败）。
func TestModelCostPerRequestOrdering(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "a-short"})
	p.Add(&auth.Auth{UID: "b-long"})
	p.NoteModelCost("a-short", "m", 0.14, 2000) // per1k=0.07
	p.NoteModelCost("b-long", "m", 0.14, 8000)  // per1k=0.0175（旧口径下的「便宜」）
	// 两号 per-req 都是 0.14：cost 并列后按权重/UID 稳定排序（a-short 在前），
	// 随机源 0 取第一个。旧实现会把 b-long 排在前面并选中它。
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "a-short" {
		t.Fatalf("选中 %v, want a-short（per-req 相同，不应按请求长度偏袒长请求号）", a)
	}
}

// TestModelCostPerRequestTotals 内部断言：per-req 相同、per1k 是诊断值且不同。
func TestModelCostPerRequestTotals(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a-short"})
	p.Add(&auth.Auth{UID: "b-long"})
	p.NoteModelCost("a-short", "m", 0.14, 2000)
	p.NoteModelCost("b-long", "m", 0.14, 8000)
	short, _ := modelCostSnapshot(p, "a-short", "m")
	long, _ := modelCostSnapshot(p, "b-long", "m")
	if short.costPerReq() != 0.14 || long.costPerReq() != 0.14 {
		t.Errorf("per-req short=%v long=%v，want 都是 0.14", short.costPerReq(), long.costPerReq())
	}
	if short.per1k() == long.per1k() {
		t.Errorf("per1k 诊断值应因请求长度不同而不同: short=%v long=%v", short.per1k(), long.per1k())
	}
}

// TestModelCostIgnoresInvalidInput 无法折算的输入不应写入账本（防污染）。
func TestModelCostIgnoresInvalidInput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 5.0, 0)                 // tokens=0
	p.NoteModelCost("", "m", 5.0, modelCostMinTokens)  // 空 uid
	p.NoteModelCost("u1", "", 5.0, modelCostMinTokens) // 空 model
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCost)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("非法输入不应写入成本账本，len=%d", n)
	}
}

// TestModelCostEmptyModelUnaffected 空模型名（PickExcluding 路径）不触发成本分层，
// 保持原三因子选号行为。
func TestModelCostEmptyModelUnaffected(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 5.0, modelCostMinTokens)
	if a := p.PickExcludingForRealm(nil, "", ""); a == nil {
		t.Error("空模型名不应被成本分层影响（应仍能选出账号）")
	}
}

// TestPickByUIDForModel 模型级可用性：绑定号在被限额的模型上返回 nil，
// 在其他模型上照常返回——handler 据此决定解绑还是沿用。
func TestPickByUIDForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 触发一次带解析时间的 6004 冷却：modelCooldowns[hy4-preview] 记录，
	// 该账号对 hy4-preview 冷却、对其他模型豁免（issue #31 语义）。
	reset := time.Now().Add(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "hy4-preview", "6004 model rate limit")

	// 先确认冷却真的落上了（否则后面两个断言是假阳性）。
	p.mu.RLock()
	_, recorded := p.byUID["u1"].modelCooldowns["hy4-preview"]
	p.mu.RUnlock()
	if !recorded {
		t.Fatalf("modelCooldowns[hy4-preview] 缺失（6004 模型级冷却未记录模型）")
	}

	if a := p.PickByUIDForModel("u1", "hy4-preview"); a != nil {
		t.Error("被限额的模型上，PickByUIDForModel 应返回 nil（让 handler 解绑轮换）")
	}
	if a := p.PickByUIDForModel("u1", "glm-5.3"); a == nil {
		t.Error("其他模型应照常可用（模型豁免生效）")
	}
}

// TestAvailableUIDsForModel 按模型过滤可用账号：被该模型限额的号不列入，
// 被其他模型限额的号照常列入。
func TestAvailableUIDsForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "hy4-preview", "6004")

	uids := p.AvailableUIDsForModel("hy4-preview")
	if len(uids) != 1 || uids[0] != "u2" {
		t.Errorf("AvailableUIDsForModel(hy4-preview)=%v want [u2]", uids)
	}
	all := p.AvailableUIDsForModel("glm-5.3")
	if len(all) != 2 {
		t.Errorf("AvailableUIDsForModel(glm-5.3)=%v want 两个账号（u1 对其他模型豁免）", all)
	}
}
