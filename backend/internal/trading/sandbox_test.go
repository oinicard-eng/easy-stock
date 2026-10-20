package trading

import (
	"context"
	"testing"

	"easy-stock/backend/internal/foundation"
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
