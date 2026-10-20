package trading

import (
	"context"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
)

// 复核报告的验收标准：它必须能用确定性数字指出"整体哪里越界"，
// 且绝不改动订单——后者是它和闸门的根本区别。

func reviewTestSnapshot() Snapshot {
	return Snapshot{
		Equity: 100000,
		Cash:   50000,
		Positions: []Position{
			{Symbol: "600000.SH", Quantity: 1000, Available: 1000, AverageCost: 10, LastPrice: 10, MarketValue: 10000},
		},
		Market: cognition.MarketView{
			Evaluated: true, Regime: cognition.RegimeRange, Label: "震荡", RiskBudgetPercent: 60,
		},
	}
}

func reviewTestDecision() Decision {
	return Decision{
		Stats:    policy.Stats{Proposed: 1, Accepted: 1, Ordered: 1},
		Proposal: []DraftOrder{{Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 20}},
		Exposure: Exposure{
			BeforePercent: 10, AfterPercent: 30, BudgetPercent: 60, CashPercent: 70,
			Sectors: []policy.SectorWeight{{Sector: "银行", Weight: 30, Names: 1}},
		},
	}
}

func TestPlanRiskReviewFlagsSingleNameLimit(t *testing.T) {
	decision := reviewTestDecision()
	orders := []Order{{
		ID: "ord-1", Symbol: "600000.SH", Side: SideBuy, Status: OrderStatusPending,
		Quantity: 2500, LimitPrice: 10, StopLossPrice: 9.5, TargetWeightPercent: 25,
	}}
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: reviewTestSnapshot(),
		Rules:    DefaultRiskRules(),
		Decision: decision,
		Orders:   orders,
		Risk:     RiskState{Policy: DefaultDrawdownPolicy()},
		Now:      time.Now().UTC(),
	})
	if review.Verdict != ReviewBlock {
		t.Fatalf("25%% 单票应被判为 block，实际 %s", review.Verdict)
	}
	item := findReviewItem(review, policy.CodeRiskSingleName)
	if item == nil || item.Level != ReviewBlock || item.Role != RoleVeto {
		t.Fatalf("单票结论异常：%+v", item)
	}
	if item.Metric != 25 || item.Limit != 20 {
		t.Fatalf("单票指标应带出 25/20：%+v", item)
	}
	// 报告是只读的：订单状态与数量不能被它改掉。
	if orders[0].Status != OrderStatusPending || orders[0].Quantity != 2500 {
		t.Fatalf("复核不得修改订单：%+v", orders[0])
	}
}

func TestPlanRiskReviewPassesCleanPlan(t *testing.T) {
	decision := reviewTestDecision()
	decision.Exposure.Sectors = []policy.SectorWeight{{Sector: "银行", Weight: 18, Names: 1}}
	decision.Exposure.AfterPercent = 28
	orders := []Order{{
		ID: "ord-1", Symbol: "600000.SH", Side: SideBuy, Status: OrderStatusPending,
		Quantity: 1000, LimitPrice: 10, StopLossPrice: 9.7, TargetWeightPercent: 10,
	}}
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: reviewTestSnapshot(),
		Rules:    DefaultRiskRules(),
		Decision: decision,
		Orders:   orders,
		Risk:     RiskState{PeakEquity: 100000, Equity: 100000, Policy: DefaultDrawdownPolicy()},
		Now:      time.Now().UTC(),
	})
	if review.Verdict != ReviewPass {
		t.Fatalf("干净方案应通过，实际 %s：%+v", review.Verdict, review.Items)
	}
	if countReviewLevel(review.Items, ReviewBlock) != 0 {
		t.Fatalf("不应有拦截项：%+v", review.Items)
	}
	// 五个角色的槽位必须都在：报告的价值之一就是让"谁下的结论"可见。
	roles := map[string]bool{}
	for _, item := range review.Items {
		roles[item.Role] = true
	}
	for _, role := range []string{RolePropose, RoleVeto, RoleDecide, RoleExecute} {
		if !roles[role] {
			t.Fatalf("缺少 %s 角色的结论：%+v", role, review.Items)
		}
	}
	if len(review.Summary) == 0 || !strings.Contains(review.Summary[0], "复核结论") {
		t.Fatalf("摘要缺少结论行：%+v", review.Summary)
	}
}

func TestPlanRiskReviewBlocksMissingStopLoss(t *testing.T) {
	decision := reviewTestDecision()
	orders := []Order{{
		ID: "ord-1", Symbol: "600000.SH", Side: SideBuy, Status: OrderStatusPending,
		Quantity: 1000, LimitPrice: 10, TargetWeightPercent: 10,
	}}
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: reviewTestSnapshot(),
		Rules:    DefaultRiskRules(),
		Decision: decision,
		Orders:   orders,
		Risk:     RiskState{Policy: DefaultDrawdownPolicy()},
		Now:      time.Now().UTC(),
	})
	item := findReviewItem(review, policy.CodeRiskStopLossMissing)
	if item == nil || item.Level != ReviewBlock {
		t.Fatalf("缺止损的买入必须被指出：%+v", item)
	}
	if review.Verdict != ReviewBlock {
		t.Fatalf("整体结论应为 block，实际 %s", review.Verdict)
	}
}

// 被守卫拒掉的单不占额度，用它们算比例会得出"复核超限"的假结论。
func TestPlanRiskReviewIgnoresRejectedOrders(t *testing.T) {
	decision := reviewTestDecision()
	decision.Exposure.Sectors = []policy.SectorWeight{{Sector: "银行", Weight: 12, Names: 1}}
	orders := []Order{{
		ID: "ord-1", Symbol: "600000.SH", Side: SideBuy, Status: OrderStatusRejected,
		Quantity: 2500, LimitPrice: 10, StopLossPrice: 9.5, TargetWeightPercent: 25,
		RejectReason: "超过单票上限",
	}}
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: reviewTestSnapshot(),
		Rules:    DefaultRiskRules(),
		Decision: decision,
		Orders:   orders,
		Risk:     RiskState{Policy: DefaultDrawdownPolicy()},
		Now:      time.Now().UTC(),
	})
	if item := findReviewItem(review, policy.CodeRiskSingleName); item != nil {
		t.Fatalf("被拒订单不应参与单票复核：%+v", item)
	}
	if review.Verdict != ReviewPass {
		t.Fatalf("被拒订单不应让复核判为不通过：%+v", review.Items)
	}
}

func TestPlanRiskReviewReflectsDrawdownLevel(t *testing.T) {
	decision := reviewTestDecision()
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: reviewTestSnapshot(),
		Rules:    DefaultRiskRules(),
		Decision: decision,
		Risk: RiskState{
			PeakEquity: 100000, Equity: 87000, DrawdownPercent: 13,
			Level: DrawdownLevelStop, Policy: DefaultDrawdownPolicy(),
		},
		Now: time.Now().UTC(),
	})
	item := findReviewItem(review, "risk.drawdown")
	if item == nil || item.Level != ReviewBlock {
		t.Fatalf("停止线必须出现在复核里：%+v", item)
	}
	if !strings.Contains(item.Message, "回撤停止线") {
		t.Fatalf("回撤说明缺失：%s", item.Message)
	}
}

func findReviewItem(review PlanRiskReview, code string) *RiskReviewItem {
	for index := range review.Items {
		if review.Items[index].Code == code {
			return &review.Items[index]
		}
	}
	return nil
}

// 端到端断言：计划里必须真的带着复核报告，时间线上必须能看到权限链角色。
// 前者证明报告接进了生成流程，后者证明"谁下的结论"在回放里可见。
func TestGeneratedPlanCarriesRiskReviewAndTimelineRoles(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if plan.RiskReview == nil {
		t.Fatal("计划缺少风险复核报告")
	}
	if plan.RiskReview.Verdict == "" || len(plan.RiskReview.Items) == 0 {
		t.Fatalf("复核报告内容不完整：%+v", plan.RiskReview)
	}
	if findReviewItem(*plan.RiskReview, "review.execution") == nil {
		t.Fatalf("复核报告缺少执行结论：%+v", plan.RiskReview.Items)
	}

	timeline, err := service.Timeline(ctx, DefaultAccountID, time.Now())
	if err != nil {
		t.Fatalf("读取时间线失败：%v", err)
	}
	roles := map[string]bool{}
	for _, entry := range timeline.Entries {
		roles[entry.Role] = true
	}
	for _, role := range []string{RolePropose, RoleDecide, RoleExecute} {
		if !roles[role] {
			t.Fatalf("时间线缺少 %s 角色：%+v", role, timeline.Entries)
		}
	}
}
