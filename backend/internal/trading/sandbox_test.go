package trading

import (
	"context"
	"strings"
	"testing"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// 沙盘的验收标准：同一份输入、只换阈值，差异必须逐笔可解释；
// 且它绝不改动真实订单。

func TestSandboxReplaysPlanWithAnotherProfile(t *testing.T) {
	// 22% 的单票权重：稳健（上限 20%）拒绝，激进（上限 25%）放行。
	content := `{"regime":"进攻","summary":"沙盘测试","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":22,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "steady"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if len(plan.Orders) != 1 || plan.Orders[0].Status != OrderStatusRejected {
		t.Fatalf("稳健风格下 22%% 单票应被拒：%+v", plan.Orders)
	}

	result, err := service.SandboxPlan(ctx, plan.ID, "aggressive")
	if err != nil {
		t.Fatalf("沙盘回放失败：%v", err)
	}
	if result.Original.Profile != "steady" || result.Replayed.Profile != "aggressive" {
		t.Fatalf("两侧风格标注错误：%+v / %+v", result.Original, result.Replayed)
	}
	if result.Original.Accepted != 0 || result.Replayed.Accepted != 1 {
		t.Fatalf("回放结论应为「稳健拒、激进放」：original=%+v replayed=%+v", result.Original, result.Replayed)
	}
	if len(result.Changes) != 1 || result.Changes[0].Kind != "added" {
		t.Fatalf("应记录一笔 added 差异：%+v", result.Changes)
	}
	// 差异必须自带原因：只说"从被拒变成可买"而不说被什么拒的，
	// 用户还得自己回去翻裁决。
	if strings.TrimSpace(result.Changes[0].Reason) == "" {
		t.Fatalf("差异缺少原因：%+v", result.Changes[0])
	}

	// 最关键的一条：沙盘不能改动真实计划。
	reloaded, err := service.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("读取计划失败：%v", err)
	}
	if reloaded.Orders[0].Status != OrderStatusRejected {
		t.Fatalf("沙盘不得改写真实订单状态，实际 %s", reloaded.Orders[0].Status)
	}
	if reloaded.Status != PlanStatusFailed {
		t.Fatalf("沙盘不得改写计划状态，实际 %s", reloaded.Status)
	}
}

func TestSandboxSameProfileIsIdentity(t *testing.T) {
	content := `{"regime":"进攻","summary":"同风格回放","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":8,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	result, err := service.SandboxPlan(ctx, plan.ID, "")
	if err != nil {
		t.Fatalf("沙盘回放失败：%v", err)
	}
	// 同一套阈值重放，逐笔动作必须完全一致：这是沙盘结论可信的前提。
	if len(result.Changes) != 0 {
		t.Fatalf("同风格回放不应产生差异：%+v", result.Changes)
	}
	if result.Original.Accepted != result.Replayed.Accepted {
		t.Fatalf("受理数应一致：%d vs %d", result.Original.Accepted, result.Replayed.Accepted)
	}
}

// 股数变化的原因必须写出来。resized 是最容易"看起来像系统算错"的一类差异：
// 同一份建议、同样的价格，回放后少了一百股，不解释就只能靠人猜。
func TestDiffSandboxExplainsResize(t *testing.T) {
	original := []Order{{
		Symbol: "600000.SH", Name: "浦发银行", Side: SideBuy, Status: OrderStatusPending,
		Quantity: 300, TargetWeightPercent: 4,
	}}
	replayed := []Order{{
		Symbol: "600000.SH", Name: "浦发银行", Side: SideBuy, Status: OrderStatusPending,
		Quantity: 200, TargetWeightPercent: 3.2,
	}}
	verdicts := []policy.Verdict{{
		Code: policy.CodeSizingConfidence, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
		Symbol: "600000.SH", Message: "置信度 0.60，目标权重 4.00% → 3.20%",
	}}
	changes := diffSandbox(original, replayed, verdicts)
	if len(changes) != 1 || changes[0].Kind != "resized" {
		t.Fatalf("应记录一笔 resized 差异：%+v", changes)
	}
	if !strings.Contains(changes[0].Reason, "sizing.confidence") {
		t.Fatalf("resized 差异应引用定价裁决，实际 %q", changes[0].Reason)
	}

	// 没有裁决时也要给出可读原因，不能留空。
	bare := diffSandbox(original, replayed, nil)
	if len(bare) != 1 || !strings.Contains(bare[0].Reason, "4.00%") {
		t.Fatalf("无裁决时应回落到权重变化说明，实际 %+v", bare)
	}
}

// 未知风格必须报错。静默当成"均衡"会给出一个看起来很确定的错误答案：
// 用户以为选了保守，其实看到的是均衡的结果。
func TestSandboxRejectsUnknownProfile(t *testing.T) {
	content := `{"regime":"进攻","summary":"未知风格","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":8,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if _, err := service.SandboxPlan(ctx, plan.ID, "conservative"); err == nil {
		t.Fatal("未知风格应报错，实际被静默接受")
	}
	// 中文名与大小写要能识别，否则前端换一种写法就报错，同样不可用。
	for _, profile := range []string{"激进", "稳健", "均衡", "AGGRESSIVE"} {
		if _, err := service.SandboxPlan(ctx, plan.ID, profile); err != nil {
			t.Fatalf("风格 %s 应被接受，实际 %v", profile, err)
		}
	}
}

// 仓位必须真的算出来：这是沙盘最直接的结论，留 0 等于没回答。
func TestSandboxComputesExposure(t *testing.T) {
	content := `{"regime":"进攻","summary":"仓位口径","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":8,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	result, err := service.SandboxPlan(ctx, plan.ID, "aggressive")
	if err != nil {
		t.Fatalf("沙盘回放失败：%v", err)
	}
	if result.Replayed.Exposure <= 0 {
		t.Fatalf("回放侧应算出买入后仓位，实际 %v", result.Replayed.Exposure)
	}
	// 空仓起步时，仓位应约等于买入金额/权益，允许四舍五入误差。
	expected := result.Replayed.BuyNotional / plan.Snapshot.Equity * 100
	if diff := result.Replayed.Exposure - expected; diff > 0.5 || diff < -0.5 {
		t.Fatalf("仓位口径不符：算得 %.2f%%，按买入金额应为 %.2f%%", result.Replayed.Exposure, expected)
	}
}

// 已成交的历史计划不能显示成"当时一笔没买"。
func TestSandboxCountsFilledAsAccepted(t *testing.T) {
	orders := []Order{
		{Symbol: "600000.SH", Side: SideBuy, Status: OrderStatusFilled, Quantity: 300, FilledQuantity: 300, LimitPrice: 10},
		{Symbol: "600519.SH", Side: SideBuy, Status: OrderStatusRejected, Quantity: 100, LimitPrice: 1300},
		{Symbol: "000001.SZ", Side: SideBuy, Status: OrderStatusExpired, Quantity: 500, LimitPrice: 11},
	}
	side := summarizeSandboxSide("steady", DefaultRiskRules(), orders, nil)
	if side.Accepted != 1 || side.Blocked != 1 {
		t.Fatalf("成交算放行、被拒算拦截、过期两边都不算：%+v", side)
	}
	if side.BuyNotional != 3000 {
		t.Fatalf("买入金额应按成交股数计算，实际 %v", side.BuyNotional)
	}
}
