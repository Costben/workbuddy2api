// billing.go 积分轮询排程：周期性对全池账号做一次权威余额核查
// （upstream.UserResourceDetailed → pool.SetCreditsDetailed）。
//
// 为什么必须存在（本 patch 的另一半）：global 账号**不签到**——scheduler.CheckinAll
// 对 IsGlobal() 直接 skip（国际版无签到体系），于是 SetCreditsDetailed 这条唯一
// 的权威余额写入路径对 global 从不触发。结果是 global 账号的 entry.credits 只会被
// NoteModelCost 的扣减路径单调压低，最终长期停在 0：它既不代表真实余额，也无法
// 驱动任何基于余额的决策（SG 积分闸门正是依赖它）。
//
// 本排程把「余额核查」从签到里剥离成独立任务：CN 与 global 一视同仁（CN 账号多跑
// 一次无副作用——写的是同一个权威值），默认每 30 分钟一轮（Config.BillingInterval）。
//
// 间隔即闸门的最坏漏检窗口：SG 闸门只能拦住"已知余额不足"的号，拦不住两次核查之间
// 被打穿的号。故间隔与闸门阈值（pool.sg_gate.min_credits / resume_credits）是一对参数，
// 调一个要看另一个。
//
// 与签到的分工：签到 = 打卡 + 顺带查余额（CN only）；billing = 只查余额（全域）。
// 两者都写 SetCreditsDetailed，幂等，谁先到谁生效（同一上游事实来源）。
package scheduler

import (
	"context"
	"log"

	"workbuddy2api/internal/logfmt"
)

// billingAccountDelay 账号间限速：与活跃上报同口径（800ms），避免一轮核查对
// 上游形成秒级连发。测试可置 0。
var billingAccountDelay = activityAccountDelay

// RunBillingNow 立即对全池账号执行一次余额核查（定时入口 + 手动触发 + 测试）。
// 无 ctx 的外部入口走背景 ctx（语义同 RunActivityNow）。
func (s *Scheduler) RunBillingNow() {
	s.runBilling(context.Background())
}

// runBilling 余额核查遍历，随 ctx 取消立即退出。
//
// 遍历口径：
//   - disabled 账号跳过（凭证可能已死，核查只会白刷 WARN）；
//   - 无凭证（无 UID/无 token）跳过；
//   - **不跳过 cooling 账号**——这正是核查的价值所在：CoolHard（余额耗尽）的号
//     要靠它确认"钱回来了"从而提前解冻（ClearCreditCooldownIfRecovered）；
//   - 单账号失败只记 WARN，不影响其余账号（与 checkin/activity 同口径）。
//
// 每次核查做三件事：
//  1. SetCreditsDetailed：写权威余额 + 快过架子集（同时驱动 SG 闸门重算）；
//  2. ClearCreditCooldownIfRecovered：余额 > 0 且正处 CoolHard → 定向解冻
//     （只解"没钱"这一种冷却，不碰 429/6004，避免解冻即撞限流的循环）；
//  3. 逐号结果日志（成功一行带余额，失败一行带原因）。
func (s *Scheduler) runBilling(ctx context.Context) {
	statuses := s.cfg.Pool.List()
	var okN, failN, skipN, revivedN int
	first := true
	for _, st := range statuses {
		if st.Disabled {
			skipN++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			skipN++
			continue
		}
		if !first {
			if !sleepCtx(ctx, billingAccountDelay) {
				log.Printf("billing poll: ctx canceled, %d/%d accounts checked", okN+failN, len(statuses))
				return // 优雅停机：剩余账号下轮再查
			}
		}
		first = false
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			// 单号失败不阻断：global 账号的 billing 通道偶发抖动是已知形态，
			// 下一轮（默认 30m 后）自然重试，不在此重试放大风控面。
			log.Printf("billing %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			failN++
			continue
		}
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		if s.cfg.Pool.ClearCreditCooldownIfRecovered(st.UID, remain) {
			revivedN++
		}
		log.Printf("billing %s: credits=%d expiring=%d", logfmt.Label(st.UID, st.Nickname), remain, buckets.Expiring)
		okN++
	}
	log.Printf("billing done: total=%d ok=%d fail=%d skipped=%d revived=%d",
		len(statuses), okN, failN, skipN, revivedN)
}
