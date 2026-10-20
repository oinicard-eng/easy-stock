package trading

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 回撤熔断的验收重点不是"能不能算百分比"，而是三件事：
// 高点会不会被低估、暂停会不会覆盖别人的开关、解除后会不会立刻又触发。

func TestDrawdownWarnPausesBuying(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	now := time.Now().UTC()

	// 第一次观察没有任何历史：高点必须取"权益与初始资金中的较大者"，
	// 否则从 10 万亏到 9.2 万的账户回撤会显示 0%。
	state, err := service.ObserveEquity(ctx, DefaultAccountID, 100000, now)
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if state.PeakEquity != 100000 || state.Level != DrawdownLevelNone {
		t.Fatalf("初始状态异常：%+v", state)
	}

	state, err = service.ObserveEquity(ctx, DefaultAccountID, 91000, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if state.Level != DrawdownLevelWarn {
		t.Fatalf("9%% 回撤应触发警戒线，实际 %s（%.2f%%）", state.Level, state.DrawdownPercent)
	}
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if !control.PauseBuying {
		t.Fatal("警戒线应暂停买入")
	}
	if !hasDrawdownReason(control.Reason) {
		t.Fatalf("暂停理由应带回撤前缀，实际 %q", control.Reason)
	}
	if control.KillSwitch {
		t.Fatal("回撤不应触发 kill switch：那会连止损卖出一起拦掉")
	}
}

func TestDrawdownPeakNeverLowersOnFall(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 120000, now); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 98000, now.Add(time.Minute)); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	state, err := service.store.GetRiskState(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取风险状态失败：%v", err)
	}
	if state.PeakEquity != 120000 {
		t.Fatalf("下跌时高点不能被下调，实际 %.2f", state.PeakEquity)
	}
	if state.DrawdownPercent != 18.33 {
		t.Fatalf("回撤应为 18.33%%，实际 %.2f%%", state.DrawdownPercent)
	}
}

func TestDrawdownStopNeedsManualReleaseAndResetsPeak(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 100000, now); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	state, err := service.ObserveEquity(ctx, DefaultAccountID, 87000, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if !state.Tripped() {
		t.Fatalf("13%% 回撤应触发停止线，实际 %s", state.Level)
	}
	// 反弹 2 个百分点不足以自动解除：能弹回来的账户也常常会再跌回去。
	state, err = service.ObserveEquity(ctx, DefaultAccountID, 89000, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if !state.Tripped() {
		t.Fatalf("停止线不应因小幅反弹自动解除，实际 %s", state.Level)
	}

	if _, err := service.ReleaseDrawdown(ctx, DefaultAccountID, ""); err == nil {
		t.Fatal("解除回撤熔断必须填写原因")
	}
	released, err := service.ReleaseDrawdown(ctx, DefaultAccountID, "误触，行情已企稳")
	if err != nil {
		t.Fatalf("解除失败：%v", err)
	}
	if released.Level != DrawdownLevelNone || released.PeakEquity != released.Equity {
		t.Fatalf("解除后应回到 none 且高点重置为现值：%+v", released)
	}
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if control.PauseBuying {
		t.Fatal("解除后应恢复买入")
	}
	// 解除后立刻再观察一次，不能又弹回停止线——那样解除按钮就没有意义。
	state, err = service.ObserveEquity(ctx, DefaultAccountID, released.Equity, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if state.Level != DrawdownLevelNone {
		t.Fatalf("解除后应保持 none，实际 %s（%.2f%%）", state.Level, state.DrawdownPercent)
	}
}

func TestDrawdownRecoversOnlyBelowReleaseLine(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 100000, now); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 91000, now.Add(time.Minute)); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	// 6.5% 落在 5% 解除线与 8% 警戒线之间：必须维持警戒，不能来回抖动。
	state, err := service.ObserveEquity(ctx, DefaultAccountID, 93500, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if state.Level != DrawdownLevelWarn {
		t.Fatalf("6.5%% 应维持警戒（滞回），实际 %s", state.Level)
	}
	state, err = service.ObserveEquity(ctx, DefaultAccountID, 96000, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if state.Level != DrawdownLevelNone {
		t.Fatalf("回到 4%% 应自动解除，实际 %s", state.Level)
	}
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if control.PauseBuying {
		t.Fatal("回撤恢复后应解除暂停买入")
	}
}

// 这一条是最要紧的安全性断言：回撤恢复不能把日亏熔断或人工 kill switch 顺手解掉。
func TestDrawdownDoesNotClearForeignPause(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	if _, err := service.SetControl(ctx, Control{
		AccountID: DefaultAccountID, PauseBuying: true, Reason: "daily_loss：当日亏损 4.2%",
	}); err != nil {
		t.Fatalf("设置总闸失败：%v", err)
	}
	now := time.Now().UTC()
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 105000, now); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 95000, now.Add(time.Minute)); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if !control.PauseBuying || !strings.HasPrefix(control.Reason, "daily_loss") {
		t.Fatalf("不应覆盖别人的暂停理由：%+v", control)
	}
	// 回撤恢复后同样不能清掉别人的暂停。
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 105000, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	control, err = service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if !control.PauseBuying || control.Reason != "daily_loss：当日亏损 4.2%" {
		t.Fatalf("回撤恢复不应解除别人的暂停：%+v", control)
	}
}

func TestRiskStatePersistsAcrossReads(t *testing.T) {
	service := newTestService(t, "", nil)
	ctx := context.Background()
	if _, err := service.ObserveEquity(ctx, DefaultAccountID, 100000, time.Now().UTC()); err != nil {
		t.Fatalf("观察权益失败：%v", err)
	}
	state, control, err := service.RiskView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取风险视图失败：%v", err)
	}
	if state.PeakEquity != 100000 || state.Policy.WarnPercent != 8 {
		t.Fatalf("风险状态未持久化：%+v", state)
	}
	if control.Gate() == "" {
		t.Fatalf("总闸状态异常：%+v", control)
	}
}

func TestDrawdownPolicyNormalizeFixesInvertedThresholds(t *testing.T) {
	policy := DrawdownPolicy{WarnPercent: 12, StopPercent: 5, ReleasePercent: 20}.Normalize()
	if !(policy.WarnPercent < policy.StopPercent && policy.ReleasePercent < policy.WarnPercent) {
		t.Fatalf("阈值未修正：%+v", policy)
	}
	level, dd := policy.Assess(DrawdownLevelNone, 100000, 100000)
	if level != DrawdownLevelNone || dd != 0 {
		t.Fatalf("零回撤判定异常：%s %.2f", level, dd)
	}
	if level, _ := policy.Assess(DrawdownLevelNone, 0, 100000); level != DrawdownLevelNone {
		t.Fatalf("无高点时不应触发：%s", level)
	}
}
