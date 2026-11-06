package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 门槛只压指定模型：低分号退出 sg 的候选集，免费模型不受影响。
func TestModelCreditGateScopesToOneModel(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "rich"})
	p.SetCredits("low", 95)
	p.SetCredits("rich", 105)
	p.SetModelCreditGate(map[string]int64{"deepseek-v4.1-flash-sg": 100})

	for i := 0; i < 20; i++ {
		got := p.Pick("deepseek-v4.1-flash-sg")
		if got == nil {
			t.Fatal("有 ≥ 门槛的号时 sg 不应返回 nil")
		}
		if got.UID != "rich" {
			t.Fatalf("sg 选中了低分号 %s（credits=%d）", got.UID, p.creditOf(got.UID))
		}
	}
	// 免费模型：低分号必须还能被选中（门槛不能溢出到其它模型）。
	sawLow := false
	for i := 0; i < 60; i++ {
		got := p.Pick("deepseek-v4.1-flash")
		if got == nil {
			t.Fatal("免费模型不应返回 nil")
		}
		if got.UID == "low" {
			sawLow = true
			break
		}
	}
	if !sawLow {
		t.Fatal("免费模型 60 次都没选到低分号：门槛错误地作用到了别的模型")
	}
}

// 全部号都低于门槛时必须返回 nil（交给上层 503），不能像 account 级保底那样回退。
func TestModelCreditGateNoFallback(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 10)
	p.SetCredits("b", 99)
	p.SetModelCreditGate(map[string]int64{"sg": 100})
	if got := p.Pick("sg"); got != nil {
		t.Fatalf("全部低于门槛时应返回 nil，got %s", got.UID)
	}
	// 同一批号跑别的模型照常可用。
	if got := p.Pick("free"); got == nil {
		t.Fatal("其它模型不应受影响")
	}
}

// credits == 0 在模型级门槛里算「不足」（与账号级保底「未知放行」故意不同）。
func TestModelCreditGateTreatsZeroAsInsufficient(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "fresh"})
	// 不加 SetCredits：credits 保持 0，模拟 billing 首刷之前的新号。
	p.SetModelCreditGate(map[string]int64{"sg": 100})
	if got := p.Pick("sg"); got != nil {
		t.Fatalf("余额未同步的号不应接门槛模型，got %s", got.UID)
	}
	if got := p.Pick("free"); got == nil {
		t.Fatal("余额未同步的号应照常服务其它模型")
	}
}

// 全冷却兜底也要过门槛：否则正常候选被门槛清空后，低分号会从兜底原路返回。
func TestModelCreditGateAppliesToFallback(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "rich"})
	p.SetCredits("low", 0)
	p.SetCredits("rich", 500)
	p.SetModelCreditGate(map[string]int64{"sg": 100})
	p.Cooldown("rich", CoolSoft, time.Hour, "test")

	if got := p.Pick("sg"); got != nil && got.UID == "low" {
		t.Fatalf("兜底不应把低分号放回来，got %s（credits=%d）", got.UID, p.creditOf(got.UID))
	}
	// 免费模型走兜底仍应拿到号。
	if got := p.Pick("free"); got == nil {
		t.Fatal("免费模型的兜底不应受影响")
	}
}

// 粘性绑定必须服从门槛：否则会话粘住以后，SG 流量持续压在烧空的号上。
func TestModelCreditGateReleasesSticky(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetCredits("sticky", 50)
	p.SetModelCreditGate(map[string]int64{"sg": 100})

	if got := p.PickByUIDForModel("sticky", "sg"); got != nil {
		t.Fatalf("低于门槛的粘性号不应命中 sg，got %s", got.UID)
	}
	if got := p.PickByUIDForModel("sticky", "free"); got == nil {
		t.Fatal("低于门槛的粘性号应照常服务其它模型")
	}

	p.SetCredits("sticky", 150)
	if got := p.PickByUIDForModel("sticky", "sg"); got == nil {
		t.Fatal("达到门槛后粘性应恢复")
	}
}

// 无效条目（空模型名 / 非正门槛）等价于关闭，不应产生任何过滤。
func TestModelCreditGateDropsInvalidEntries(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 1)
	p.SetModelCreditGate(map[string]int64{"": 100, "sg": 0, "other": -5})
	if got := p.Pick("sg"); got == nil {
		t.Fatal("门槛为 0 时应等价于关闭")
	}
	if got := p.PickByUIDForModel("poor", "other"); got == nil {
		t.Fatal("门槛为负时应等价于关闭")
	}
}

// 与账号级保底共存：账号级压的是全池，模型级只压一个模型。
func TestModelCreditGateCoexistsWithCreditFloor(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "mid"})
	p.SetCredits("mid", 150)
	p.SetCreditFloor(200) // 账号级：150 < 200，但只有它一个号 → 回退仍可用
	p.SetModelCreditGate(map[string]int64{"sg": 400})

	if got := p.Pick("sg"); got != nil {
		t.Fatalf("模型级门槛 400 应挡住 150 分的号，got %s", got.UID)
	}
	if got := p.Pick("free"); got == nil {
		t.Fatal("账号级保底在无其它候选时应回退，不应返回 nil")
	}
}
