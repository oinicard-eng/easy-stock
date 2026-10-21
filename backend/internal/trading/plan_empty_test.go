package trading

import (
	"context"
	"testing"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// 空仓与失败必须是两个状态：前者是模型判断"现在不该动手"，后者是"想说的事没做成"。
// 混成一个，用户会在计划列表里看到一串"未通过"，误以为系统在报错。
func TestPlanEmptyWhenModelSuggestsNothing(t *testing.T) {
	service := newTestService(t, `{"regime":"观望","summary":"满仓等待","orders":[]}`, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	plan, err := service.RunPlanSync(context.Background(), PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced",
	}, "cyc-empty")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if plan.Status != PlanStatusEmpty {
		t.Fatalf("模型未提建议应为主动空仓，实际 %s", plan.Status)
	}
	// 空仓不是错误，不该带错误文案。
	if plan.ErrorMessage != "" {
		t.Fatalf("主动空仓不应带错误信息，实际 %q", plan.ErrorMessage)
	}
	if len(plan.Orders) != 0 {
		t.Fatalf("空仓计划不该有订单，实际 %d 笔", len(plan.Orders))
	}
}

// 提了建议但全被风控拦下，是"没做成"，仍应记 failed 并保留原因。
func TestPlanFailedWhenAllSuggestionsBlocked(t *testing.T) {
	// 单票目标 50% 远超 balanced 的单票上限，必然被拦。
	content := `{"regime":"进攻","summary":"重仓一只","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":50,"order_type":"limit","limit_price":10,"stop_loss_price":9,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	plan, err := service.RunPlanSync(context.Background(), PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced",
	}, "cyc-blocked")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if plan.Status != PlanStatusFailed {
		t.Fatalf("建议全被拦应记失败，实际 %s", plan.Status)
	}
	if plan.ErrorMessage == "" {
		t.Fatal("失败计划必须留下原因")
	}
	blocked := false
	for _, verdict := range plan.Verdicts {
		if verdict.Code == policy.CodeRiskSingleName && verdict.Level == policy.LevelBlock {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("应出现单票超限的拦截裁决，实际 %+v", plan.Verdicts)
	}
}
