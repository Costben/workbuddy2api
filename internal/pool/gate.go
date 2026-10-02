// SG 积分闸门：global 域免费（0 倍率）与 -sg 付费模型共用同一个账号池，
// 而 -sg 打空积分会让账号整体撞 402 → applyErrorPolicy 走 CooldownUntilTomorrow4AM
// （**账号级**硬冷却）→ 免费模型也一起不可用。这是「一次 -sg 把免费池打没」的根因。
//
// 本文件只做一件事：给**单个模型**加一道基于权威余额的软闸门——余额低于下限时，
// 把该账号从该模型的候选集里摘掉，其余模型（免费池）完全不受影响。
//
// 与既有维度的正交性：
//   - 不改 healthy/healthyForModel（账号级 + 6004 模型级冷却），另开一层 sgAllowed 谓词；
//   - 不写 until/coolKind（那会连带影响免费模型），只置 entry.sgGated 这一个模型级状态位；
//   - 与 modelCooldowns 并列但语义不同：那条是"上游说这个模型现在不行"（外部事实），
//     本条是"我们自己判断这个号不该打这个模型"（内部策略），互不覆盖。
//
// 滞回（hysteresis）：入门阈值 sgMinCredits、恢复阈值 sgResumeCredits 分开，
// 避免余额在阈值附近抖动导致账号反复进出闸门（参考既有 softStreak 退避的设计取向）。
package pool

import (
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
)

// 闸门三个默认值（SetSGGate 注入；0 值 = 关闭）。
const (
	// defaultSGMinCredits 入门阈值：余额低于此值即摘除该账号的闸门模型请求。
	// 取 50（运维口径）：闸门只能拦住"已知余额不足"的号，拦不住两次核查之间被打穿的
	// 号——核查间隔就是最坏漏检窗口（默认 30m）。阈值 50 是给这个窗口留的余量：
	// 实测 -sg 单次约 0.0014~0.0057 积分（1k token），50 足以扛住一个间隔内的消耗。
	defaultSGMinCredits = 50
	// defaultSGResumeCredits 恢复阈值：余额回到此值以上才恢复闸门模型的请求。
	// 取 120 = 2.4 倍入门阈值（运维口径 2026-10-02 定）：滞回窗口够宽，
	// 避免余额在 50 附近反复横跳；又不至于把刚回血的号长期晾在闸门外。
	defaultSGResumeCredits = 120
	// defaultSGGateRealm 闸门生效的 realm。global 账号不签到（scheduler.CheckinAll
	// 对 global 直接 skip），credits 曾是终身冻结的死数字，正需要本闸门兜底；
	// CN 账号有签到兜底，默认不吃本闸门（零回归）。"*" = 全 realm 生效。
	defaultSGGateRealm = "global"
)

// SetSGGate 注入 SG 积分闸门参数（main 从 config 解析后调用）。
//   - model 为空 或 min <= 0 或 enabled=false → 闸门关闭（完全回到引入前行为）。
//   - resume < min 时钳到 min（滞回窗口退化但不出错）。
//   - realm 为空 → 回落默认 "global"；"*" 表示全 realm。
func (p *Pool) SetSGGate(enabled bool, model string, minCredits, resumeCredits int64, realm string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !enabled || model == "" || minCredits <= 0 {
		p.sgGateModel = ""
		// 关闭闸门时清掉全池闸门位：留着 true 不影响可用性（sgAllowed 先看启用位），
		// 但会让 /status 出现"闸门已关却显示 sg_gated=true"的误导快照。
		for _, e := range p.byUID {
			e.sgGated = false
		}
		return
	}
	if resumeCredits < minCredits {
		resumeCredits = minCredits
	}
	if realm == "" {
		realm = defaultSGGateRealm
	}
	p.sgGateModel = model
	p.sgMinCredits = minCredits
	p.sgResumeCredits = resumeCredits
	p.sgGateRealm = realm
	// 参数变更后立即按新阈值重算全池闸门位：运维调阈值不必等下一次 billing 轮询
	// 才生效（尤其是把阈值调高让号立刻出闸的场景）。
	p.refreshSGGateAllLocked()
}

// sgGateActiveLocked 报告闸门是否启用（配置齐全）。调用方需已持 p.mu。
func (p *Pool) sgGateActiveLocked() bool { return p.sgGateModel != "" && p.sgMinCredits > 0 }

// sgRealmMatchLocked 报告账号是否落在闸门生效的 realm 内。调用方需已持 p.mu。
func (p *Pool) sgRealmMatchLocked(e *entry) bool {
	if p.sgGateRealm == "" || p.sgGateRealm == "*" {
		return true
	}
	return e.a.Realm() == p.sgGateRealm
}

// sgAllowed 报告账号对 reqModel 是否通过 SG 闸门。
//
// 恒真路径（零回归）：闸门未启用 / 请求模型为空 / 请求模型≠闸门模型 / 账号不在闸门 realm。
// 只有"闸门模型 + 目标 realm"这一条窄路径才读 sgGated 位。
//
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读）。
func (p *Pool) sgAllowed(e *entry, reqModel string) bool {
	if !p.sgGateActiveLocked() || reqModel == "" || reqModel != p.sgGateModel {
		return true
	}
	if !p.sgRealmMatchLocked(e) {
		return true
	}
	return !e.sgGated
}

// applySGGateLocked 按当前 credits/creditsKnown 重算单个账号的 sgGated 位（滞回）。
// 状态迁移打日志（进闸/出闸各一行），运维可据此对账「这个号什么时候被摘的、为什么」。
// 调用方必须已持有 p.mu。
//
// 判定口径：
//   - 闸门未启用 / 账号不在闸门 realm → 恒 false（位清零，不残留）。
//   - 余额未知（creditsKnown=false）→ 保守置 true（未知不等于有钱；新号在首次
//     billing 轮询前不打 -sg，避免"首次 -sg 就打空"的原始故障）。首轮轮询后即可解闸。
//   - 已进闸：余额 >= sgResumeCredits 才出闸（滞回上沿）。
//   - 未进闸：余额 <  sgMinCredits   才进闸（滞回下沿）。
func (p *Pool) applySGGateLocked(e *entry) {
	if !p.sgGateActiveLocked() || !p.sgRealmMatchLocked(e) {
		e.sgGated = false
		return
	}
	was := e.sgGated
	var now bool
	switch {
	case !e.creditsKnown:
		now = true // 余额未知：保守进闸
	case e.sgGated:
		now = e.credits < p.sgResumeCredits // 已进闸：须回到恢复阈值才放行
	default:
		now = e.credits < p.sgMinCredits // 未进闸：跌破入门阈值才摘除
	}
	if now == was {
		return
	}
	e.sgGated = now
	p.dirty.Store(true)
	if now {
		reason := "credits below minimum"
		if !e.creditsKnown {
			reason = "credits unknown"
		}
		log.Printf("WARN: [pool] sg gate ON acct=%s model=%s credits=%d known=%v (%s) — 停止向该账号请求此模型",
			logfmt.Label(e.a.UID, e.a.Nickname), p.sgGateModel, e.credits, e.creditsKnown, reason)
	} else {
		log.Printf("[pool] sg gate OFF acct=%s model=%s credits=%d (>=%d) — 恢复该模型的请求",
			logfmt.Label(e.a.UID, e.a.Nickname), p.sgGateModel, e.credits, p.sgResumeCredits)
	}
}

// refreshSGGateAllLocked 重算全池闸门位（SetSGGate 参数变更时调用）。调用方必须已持有 p.mu。
func (p *Pool) refreshSGGateAllLocked() {
	for _, e := range p.byUID {
		p.applySGGateLocked(e)
	}
}

// SGGateStatus 透出闸门配置（/status 用，运维据此确认阈值与目标模型）。
func (p *Pool) SGGateStatus() (model string, minCredits, resumeCredits int64, realm string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sgGateModel, p.sgMinCredits, p.sgResumeCredits, p.sgGateRealm
}

// ClearCreditCooldownIfRecovered 余额核查后的定向解冻：仅当账号正处 CoolHard
// （余额耗尽硬冷却）且权威余额 > 0 时，清除冷却域。
//
// 为什么不直接用 ReenableIfCredits：那个是**签到**语义（签到成功 ⇒ 余额恢复 ⇒
// 无条件清全部冷却域），CN 账号每天两次签到兜底，粗粒度无妨。而 billing 轮询是
// **高频旁路核查**，若也清全部冷却域，会把正在生效的 429 软冷却/6004 模型级冷却
// 一并抹掉 → 账号立刻回到 429 雷区，形成「轮询一次解冻一次、再撞一次」的循环。
// 本方法只解 CoolHard（"没钱"这一种），其余冷却各自到期，语义精确。
// 返回 true 表示本次确实解冻。调用方传权威余额（不写 credits，写余额走 SetCreditsDetailed）。
func (p *Pool) ClearCreditCooldownIfRecovered(uid string, credits int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.disabled {
		return false
	}
	if e.coolKind != CoolHard || e.until.IsZero() || !time.Now().Before(e.until) {
		return false // 不在 CoolHard 有效期内：无事可做
	}
	if credits <= 0 {
		return false // 余额仍为零：维持冷却
	}
	p.reviveCoolingLocked(e, credits) // 与签到解冻共用原语（清冷却域，不动熔断器）
	log.Printf("[pool] credit cooldown cleared acct=%s credits=%d — billing 核查确认余额恢复",
		logfmt.Label(e.a.UID, e.a.Nickname), credits)
	return true
}
