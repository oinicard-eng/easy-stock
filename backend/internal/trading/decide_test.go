package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
)

func decideTestSnapshot() Snapshot {
	return Snapshot{
		TakenAt:      time.Now(),
		Equity:       100000,
		Cash:         100000,
		ReservedCash: 0,
		Quotes: []foundation.Quote{
			testQuote("600000.SH", "浦发银行", 10, 9.9),
			testQuote("600519.SH", "贵州茅台", 100, 99),
		},
		Positions: []Position{
			{Symbol: "600519.SH", Name: "贵州茅台", Quantity: 100, Available: 100, AverageCost: 95, LastPrice: 100, MarketValue: 10000},
		},
		Universe: []string{"600000.SH"},
		Sectors:  map[string]string{"600000.SH": "金融", "600519.SH": "食品农业"},
	}
}

func buyDraft(symbol string, weight float64) DraftOrder {
	return DraftOrder{
		Symbol: symbol, Side: "buy", TargetWeightPercent: weight, OrderType: "limit",
		LimitPrice: 10, StopLossPrice: 9.6, Thesis: "测试",
	}
}

func hasVerdictCode(verdicts []policy.Verdict, code string) bool {
	for _, verdict := range verdicts {
		if verdict.Code == code {
			return true
		}
	}
	return false
}

// 全部停止时不允许任何订单，连卖出也不放行：这是它与"暂停买入"的关键区别。
func TestDecideKillSwitchDropsEverything(t *testing.T) {
	snapshot := decideTestSnapshot()
	draft := DraftPlan{Orders: []DraftOrder{
		buyDraft("600000.SH", 10),
		{Symbol: "600519.SH", Side: "sell", SellFractionPercent: 100, OrderType: "limit", LimitPrice: 100},
	}}
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Mode: "manual", Draft: draft, Snapshot: snapshot,
		Rules: DefaultRiskRules(), Control: Control{KillSwitch: true}, Now: time.Now(),
	})
	if len(orders) != 0 {
		t.Fatalf("全部停止时不应产生订单，实际 %+v", orders)
	}
	if !hasVerdictCode(decision.Verdicts, policy.CodeGateKillSwitch) {
		t.Fatalf("应记录 gate.kill_switch，实际 %+v", decision.Verdicts)
	}
	if decision.Stats.Proposed != 2 || decision.Stats.Accepted != 0 || decision.Dropped != 2 {
		t.Fatalf("统计异常：%+v", decision.Stats)
	}
}

// 暂停买入只拦买入：保护性卖出必须放行，否则用户想跑都跑不掉。
func TestDecidePauseBuyingKeepsSells(t *testing.T) {
	snapshot := decideTestSnapshot()
	draft := DraftPlan{Orders: []DraftOrder{
		buyDraft("600000.SH", 10),
		{Symbol: "600519.SH", Side: "sell", SellFractionPercent: 100, OrderType: "limit", LimitPrice: 100},
	}}
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Draft: draft, Snapshot: snapshot,
		Rules: DefaultRiskRules(), Control: Control{PauseBuying: true}, Now: time.Now(),
	})
	if len(orders) != 1 || orders[0].Side != SideSell {
		t.Fatalf("暂停买入时应只保留卖出单，实际 %+v", orders)
	}
	if orders[0].Status != OrderStatusPending {
		t.Fatalf("卖出单应可执行，实际 %s：%s", orders[0].Status, orders[0].RejectReason)
	}
	if !hasVerdictCode(decision.Verdicts, policy.CodeOrderBuyDropped) {
		t.Fatalf("应记录被剔除的买入建议，实际 %+v", decision.Verdicts)
	}
	if decision.Dropped != 1 || decision.Stats.Accepted != 1 {
		t.Fatalf("统计异常：%+v", decision.Stats)
	}
}

// 恐慌时禁止开新仓：这不是风控阈值问题，而是"今天不该下注"的判断。
func TestDecidePanicBlocksNewPositions(t *testing.T) {
	snapshot := decideTestSnapshot()
	snapshot.Market = cognition.MarketView{
		Evaluated: true, Regime: cognition.RegimePanic, Label: "恐慌", RiskBudgetPercent: 20,
		Breadth: cognition.Breadth{MedianChange: -4.2, LimitDown: 120},
	}
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Draft: DraftPlan{Orders: []DraftOrder{buyDraft("600000.SH", 10)}},
		Snapshot: snapshot, Rules: DefaultRiskRules(), Now: time.Now(),
	})
	if len(orders) != 0 {
		t.Fatalf("恐慌时不应产生买入订单，实际 %+v", orders)
	}
	if !hasVerdictCode(decision.Verdicts, policy.CodeMarketPanic) {
		t.Fatalf("应记录 market.panic，实际 %+v", decision.Verdicts)
	}
	if decision.Exposure.BudgetPercent != 20 {
		t.Fatalf("仓位预算应取市场值 20，实际 %.2f", decision.Exposure.BudgetPercent)
	}
}

// 未评估市场数据时不应禁止开仓：那是数据缺失，不是看清了风险。
func TestDecideAllowsBuyingWithoutMarketEvaluation(t *testing.T) {
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Draft: DraftPlan{Orders: []DraftOrder{buyDraft("600000.SH", 10)}},
		Snapshot: decideTestSnapshot(), Rules: DefaultRiskRules(), Now: time.Now(),
	})
	if len(orders) != 1 || orders[0].Status != OrderStatusPending {
		t.Fatalf("未评估市场状态时应允许按风控下单，实际 %+v（%+v）", orders, decision.Verdicts)
	}
}

// 单票都合法、合计压在同一个行业里，必须被组合守卫拦下。
func TestDecideBlocksSectorConcentration(t *testing.T) {
	snapshot := decideTestSnapshot()
	snapshot.Quotes = nil
	symbols := []string{"600001.SH", "600002.SH", "600003.SH", "600004.SH"}
	snapshot.Sectors = map[string]string{}
	draft := DraftPlan{}
	for _, symbol := range symbols {
		snapshot.Quotes = append(snapshot.Quotes, testQuote(symbol, symbol, 10, 9.9))
		snapshot.Sectors[symbol] = "电子"
		draft.Orders = append(draft.Orders, buyDraft(symbol, 5))
	}
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Draft: draft, Snapshot: snapshot,
		Rules: DefaultRiskRules(), Now: time.Now(),
	})
	accepted := 0
	for _, order := range orders {
		if order.Status == OrderStatusPending {
			accepted++
		}
	}
	if accepted != 3 {
		t.Fatalf("同行业最多 3 只，实际通过 %d 只（%+v）", accepted, decision.Verdicts)
	}
	if !hasVerdictCode(decision.Verdicts, policy.CodeRiskSectorNameCount) {
		t.Fatalf("应记录行业只数超限，实际 %+v", decision.Verdicts)
	}
	if decision.Exposure.Sectors[0].Sector != "电子" || decision.Exposure.Sectors[0].Names != 3 {
		t.Fatalf("行业暴露统计异常：%+v", decision.Exposure.Sectors)
	}
}

// 全部建议都是买入且被门控剔除时，不能报"模型没有给出建议"——模型给了，是门控挡了。
func TestDecidePauseBuyingAllBuysDoesNotReportEmpty(t *testing.T) {
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID,
		Draft:     DraftPlan{Orders: []DraftOrder{buyDraft("600000.SH", 10), buyDraft("600519.SH", 5)}},
		Snapshot:  decideTestSnapshot(), Rules: DefaultRiskRules(),
		Control: Control{PauseBuying: true}, Now: time.Now(),
	})
	if len(orders) != 0 {
		t.Fatalf("暂停买入不应产生订单，实际 %+v", orders)
	}
	if hasVerdictCode(decision.Verdicts, policy.CodeOrderEmpty) {
		t.Fatalf("不应出现 order.empty：%+v", decision.Verdicts)
	}
	if !hasVerdictCode(decision.Verdicts, policy.CodeOrderBuyDropped) || decision.Dropped != 2 {
		t.Fatalf("应记录 2 笔被剔除的买入：%+v dropped=%d", decision.Verdicts, decision.Dropped)
	}
	if decision.Stats.Blocked != 2 || decision.Stats.Accepted != 0 {
		t.Fatalf("统计异常：%+v", decision.Stats)
	}
}

// 买入后仓位必须含在途挂单：否则账户里 55% 挂单 + 25% 新买入会显示成"0% → 25%"。
func TestDecisionExposureIncludesPendingOrders(t *testing.T) {
	snapshot := decideTestSnapshot()
	snapshot.OpenBuyValue = map[string]float64{"600000.SH": 20000}
	snapshot.PendingBuyValue = 20000
	snapshot.ReservedCash = 20000
	snapshot.Cash = 100000
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600519.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit",
		LimitPrice: 100, StopLossPrice: 95,
	}}}
	decision, orders := Decide(DecideInput{
		AccountID: DefaultAccountID, Draft: draft, Snapshot: snapshot,
		Rules: DefaultRiskRules(), Now: time.Now(),
	})
	if len(orders) != 1 || orders[0].Status != OrderStatusPending {
		t.Fatalf("该笔买入应通过：%+v（%+v）", orders, decision.Verdicts)
	}
	exposure := decision.Exposure
	if exposure.BeforePercent != 10 || exposure.PendingPercent != 20 {
		t.Fatalf("持仓/在途应分别为 10%%/20%%，实际 %.2f/%.2f", exposure.BeforePercent, exposure.PendingPercent)
	}
	if exposure.AfterPercent != 40 {
		t.Fatalf("买入后仓位应含在途：期望 40%%，实际 %.2f", exposure.AfterPercent)
	}
	sectors := map[string]float64{}
	for _, sector := range exposure.Sectors {
		sectors[sector.Sector] = sector.Weight
	}
	if sectors["金融"] != 20 {
		t.Fatalf("在途挂单应计入行业暴露，实际 %+v", exposure.Sectors)
	}
}

func TestDecisionStoreRoundTrip(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	decision, _ := Decide(DecideInput{
		AccountID: DefaultAccountID, PlanID: "plan-1",
		Draft:    DraftPlan{Orders: []DraftOrder{buyDraft("600000.SH", 10)}},
		Snapshot: decideTestSnapshot(), Rules: DefaultRiskRules(), Now: time.Now(),
	})
	if err := store.SaveDecision(ctx, decision); err != nil {
		t.Fatalf("保存裁决失败：%v", err)
	}
	loaded, err := store.GetDecisionByPlan(ctx, "plan-1")
	if err != nil {
		t.Fatalf("按计划读取裁决失败：%v", err)
	}
	if loaded.ID != decision.ID || len(loaded.Verdicts) != len(decision.Verdicts) {
		t.Fatalf("裁决往返后不一致：%+v", loaded)
	}
	list, err := store.ListDecisions(ctx, DefaultAccountID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("裁决列表异常：%v len=%d", err, len(list))
	}
	count, err := store.DecisionCount(ctx, DefaultAccountID)
	if err != nil || count != 1 {
		t.Fatalf("裁决计数异常：%v count=%d", err, count)
	}
}
