package trading

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/appsettings"
	"easy-stock/backend/internal/foundation"
)

type fakeQuotes struct {
	quotes map[string]foundation.Quote
}

func (f fakeQuotes) Realtime(_ context.Context, symbols []string) ([]foundation.Quote, error) {
	out := []foundation.Quote{}
	for _, symbol := range symbols {
		if quote, ok := f.quotes[symbol]; ok {
			out = append(out, quote)
		}
	}
	return out, nil
}

type fakeGateway struct {
	content string
	err     error
}

func (f fakeGateway) Prompt(context.Context, string) (agent.PromptResult, error) {
	return agent.PromptResult{Content: f.content}, f.err
}
func (f fakeGateway) Status() agent.Status { return agent.Status{Available: true} }
func (f fakeGateway) ModelAPIKey() (string, error) {
	return "", nil
}
func (f fakeGateway) SyncLLM(appsettings.LLM, *string) error { return nil }
func (f fakeGateway) Start(context.Context) (agent.Process, error) {
	return nil, nil
}

func testQuote(symbol, name string, price, previousClose float64) foundation.Quote {
	return foundation.Quote{Symbol: symbol, Name: name, Price: price, PreviousClose: previousClose, High: price, Low: price}
}

func testSnapshot(quotes []foundation.Quote) Snapshot {
	return Snapshot{
		TakenAt: time.Now(),
		Equity:  100000,
		Cash:    100000,
		Quotes:  quotes,
	}
}

func TestBuildOrdersComputesSharesAndKeepsRiskBounded(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	draft := DraftPlan{
		Regime:  "进攻",
		Summary: "测试",
		Orders: []DraftOrder{{
			Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit",
			LimitPrice: 10.2, StopLossPrice: 9.5, Confidence: 62,
		}},
	}
	orders, issues := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 {
		t.Fatalf("期望 1 笔订单，实际 %d，问题：%+v", len(orders), issues)
	}
	order := orders[0]
	if order.Status != OrderStatusPending {
		t.Fatalf("期望订单通过校验，实际状态 %s：%s", order.Status, order.RejectReason)
	}
	// 置信度定价后的股数：目标 10% × 置信度 0.62 的系数 0.81 = 8.1%，
	// 8.1% × 10 万 / 限价 10.2 = 794 股，向下取整手为 700 股。
	// 这条断言同时覆盖了"置信度真的会影响成交股数"——只断言订单上的
	// Confidence 字段是不够的，那个字段可能是写了但没人用。
	if order.Quantity != 700 {
		t.Fatalf("置信度定价后应为 700 股，实际 %d", order.Quantity)
	}
	if order.TargetWeightPercent != 8.1 {
		t.Fatalf("定价后目标权重应为 8.1%%，实际 %.2f", order.TargetWeightPercent)
	}
	if order.Confidence != 0.62 {
		t.Fatalf("置信度应归一化到 0.62，实际 %v", order.Confidence)
	}
	if order.Symbol != "600000.SH" {
		t.Fatalf("标的应规范化为 600000.SH，实际 %s", order.Symbol)
	}
}

func TestBuildOrdersRejectsSymbolOutsideSnapshot(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "000001.SZ", Side: "buy", TargetWeightPercent: 5, OrderType: "limit", LimitPrice: 10, StopLossPrice: 9,
	}}}
	orders, issues := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("快照外标的必须被拒绝，实际 %+v", orders)
	}
	if len(issues) == 0 || !strings.Contains(issues[0].Message, "行情快照") {
		t.Fatalf("应给出快照范围问题，实际 %+v", issues)
	}
}

func TestBuildOrdersRejectsOverweightAndWideStop(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	rules := DefaultRiskRules()
	cases := []DraftOrder{
		{Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 35, OrderType: "limit", LimitPrice: 10, StopLossPrice: 9.5},
		{Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit", LimitPrice: 10, StopLossPrice: 8},
		{Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit", LimitPrice: 12, StopLossPrice: 11.5},
		{Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit", LimitPrice: 10},
	}
	for index, item := range cases {
		orders, _ := BuildOrders(DefaultAccountID, DraftPlan{Orders: []DraftOrder{item}}, snapshot, rules, time.Now())
		if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
			t.Fatalf("用例 %d 应被风控拒绝，实际 %+v", index, orders)
		}
	}
}

func TestBuildOrdersFallsBackToOneLotAfterPricing(t *testing.T) {
	// 30 元的票、10 万权益：5% = 5000 元够一手；
	// 但置信度 0.3（×0.65）叠加 ATR 5%（×0.55）后只剩 1.79%，
	// 1790 元 / 30.5 = 58 股，不足一手。此时应退到一手，而不是整笔作废。
	snapshot := testSnapshot([]foundation.Quote{testQuote("000001.SZ", "平安银行", 30, 30)})
	snapshot.ATRs = map[string]float64{"000001.SZ": 5}
	draft := DraftPlan{Regime: "range", Orders: []DraftOrder{{
		Symbol: "000001.SZ", Side: "buy", TargetWeightPercent: 5, OrderType: "limit",
		LimitPrice: 30.5, StopLossPrice: 28, Confidence: 0.3,
	}}}
	orders, verdicts := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusPending {
		t.Fatalf("定价压到不足一手时应按一手执行，实际 %+v / %+v", orders, verdicts)
	}
	if orders[0].Quantity != 100 {
		t.Fatalf("应买最小一手 100 股，实际 %d", orders[0].Quantity)
	}
	minLot := false
	for _, verdict := range verdicts {
		if verdict.Code == "sizing.min_lot" {
			minLot = true
		}
	}
	if !minLot {
		t.Fatalf("应留下「按最小一手执行」的裁决，实际 %+v", verdicts)
	}
}

func TestBuildOrdersBlocksTinyRequestEvenIfBelowOneLot(t *testing.T) {
	// 模型自己只要 0.5%（500 元 < 一手 3050 元）时不兜底：
	// 那是它的判断，系统替它放大六倍就是把仓位管理的责任接过来了。
	snapshot := testSnapshot([]foundation.Quote{testQuote("000001.SZ", "平安银行", 30, 30)})
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "000001.SZ", Side: "buy", TargetWeightPercent: 0.5, OrderType: "limit",
		LimitPrice: 30.5, StopLossPrice: 28, Confidence: 1,
	}}}
	orders, verdicts := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("模型自己只要不足一手时应拒绝，实际 %+v", orders)
	}
	if len(verdicts) == 0 || verdicts[0].Code != "sizing.below_lot" {
		t.Fatalf("应给出 below_lot 拦截，实际 %+v", verdicts)
	}
}

func TestBuildOrdersRejectsSellWithoutAvailableShares(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	snapshot.Positions = []Position{{Symbol: "600000.SH", Name: "浦发银行", Quantity: 1000, Available: 0, AverageCost: 9.8}}
	orders, issues := BuildOrders(DefaultAccountID, DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "sell", SellFractionPercent: 100, OrderType: "limit", LimitPrice: 10,
	}}}, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("T+1 不可卖必须被拒绝，实际 %+v", orders)
	}
	if len(issues) == 0 || !strings.Contains(issues[0].Message, "T+1") {
		t.Fatalf("应提示 T+1，实际 %+v", issues)
	}
}

func TestParseDraftPlanExtractsFencedJSON(t *testing.T) {
	content := "这是计划：\n```json\n{\"regime\":\"观望\",\"summary\":\"等待\",\"orders\":[]}\n```\n以上。"
	draft, err := ParseDraftPlan(content)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if draft.Regime != "观望" || len(draft.Orders) != 0 {
		t.Fatalf("解析结果异常：%+v", draft)
	}
}

func TestParseDraftPlanRejectsMissingOrders(t *testing.T) {
	if _, err := ParseDraftPlan(`{"regime":"观望","summary":"x"}`); err == nil {
		t.Fatal("缺少 orders 字段应报错")
	}
}

func newTestService(t *testing.T, content string, quotes map[string]foundation.Quote) *Service {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := log.New(io.Discard, "", 0)
	service := NewService(store, fakeGateway{content: content}, fakeQuotes{quotes: quotes}, nil, logger)
	// 计划流程用例不测调研：关掉后仍是"一次模型往返 → 一份计划"的原有语义。
	service.SetResearchEnabled(false)
	return service
}

func TestServiceGeneratesAndExecutesPaperPlan(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试计划","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	plan, err := service.StartPlan(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"})
	if err != nil {
		t.Fatalf("启动计划失败：%v", err)
	}
	ready := waitPlan(t, service, plan.ID)
	if ready.Status != PlanStatusDraft {
		t.Fatalf("期望待确认状态，实际 %s（%s）", ready.Status, ready.ErrorMessage)
	}
	// 10% 目标 × 置信度 0.6 的系数 0.8 = 8%，8% × 10 万 / 10.2 = 784 股 → 700 股。
	if len(ready.Orders) != 1 || ready.Orders[0].Quantity != 700 {
		t.Fatalf("订单数量异常：%+v", ready.Orders)
	}
	// 裁决必须落库，且与计划互相指认：这是"每次判断可观察"的最小要求。
	decisions, err := service.Decisions(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取裁决失败：%v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("期望 1 条裁决，实际 %d", len(decisions))
	}
	decision := decisions[0]
	if decision.PlanID != plan.ID || ready.DecisionID != decision.ID {
		t.Fatalf("计划与裁决未互相指认：plan=%s ready=%s decision=%s", plan.ID, ready.DecisionID, decision.ID)
	}
	if decision.Stats.Accepted != 1 || decision.Stats.Proposed != 1 {
		t.Fatalf("裁决统计异常：%+v", decision.Stats)
	}
	if ready.Exposure == nil || ready.Exposure.AfterPercent <= 0 {
		t.Fatalf("计划缺少仓位视图：%+v", ready.Exposure)
	}
	if len(decision.Narrative) == 0 {
		t.Fatal("裁决应生成可直接阅读的摘要")
	}

	executed, err := service.Execute(ctx, plan.ID)
	if err != nil {
		t.Fatalf("执行计划失败：%v", err)
	}
	if executed.Status != PlanStatusExecuted {
		t.Fatalf("期望已执行，实际 %s", executed.Status)
	}
	if executed.Orders[0].Status != OrderStatusFilled {
		t.Fatalf("限价买入应成交，实际 %s：%s", executed.Orders[0].Status, executed.Orders[0].RejectReason)
	}
	account, positions, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if len(positions) != 1 || positions[0].Quantity != 700 {
		t.Fatalf("持仓异常：%+v", positions)
	}
	if positions[0].Available != 0 {
		t.Fatalf("T+1 当日买入不可卖，实际可卖 %d", positions[0].Available)
	}
	// 建仓上下文跟着持仓走：市场状态未评估时退回模型写的 regime，
	// 置信度用订单上的归一化值。这两项是三个月后复盘时的唯一依据。
	if positions[0].EntryRegime != "进攻" {
		t.Fatalf("持仓应带上入场环境「进攻」，实际 %q", positions[0].EntryRegime)
	}
	if positions[0].EntryConfidence != 0.6 {
		t.Fatalf("持仓应带上入场置信度 0.6，实际 %.2f", positions[0].EntryConfidence)
	}
	// 成交价带滑点：买单按现价上滑，并被限价 10.20 封住。
	// 断言"在限价内、不高于现价 + 最冷档滑点"而不是断言一个精确数字，
	// 否则调整滑点参数时这条用例会变成纯粹的维护负担。
	// 测试行情没有成交额，按最冷档 0.3% 计，所以上界是 10.03。
	fillPrice := executed.Orders[0].FilledPrice
	if fillPrice < 10 || fillPrice > 10.04 {
		t.Fatalf("成交价应在现价与滑点之间，实际 %.4f", fillPrice)
	}
	expectedCash := 100000 - float64(executed.Orders[0].FilledQuantity)*fillPrice - executed.Orders[0].Fee
	if diff := account.Cash - expectedCash; diff > 0.01 || diff < -0.01 {
		t.Fatalf("现金应为 %.2f，实际 %.2f", expectedCash, account.Cash)
	}
}

func waitPlan(t *testing.T, service *Service, id string) Plan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		plan, err := service.GetPlan(context.Background(), id)
		if err == nil && plan.Status != PlanStatusRunning {
			return plan
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待计划生成超时")
	return Plan{}
}

func TestPaperBrokerKeepsOrderPendingWhenPriceAboveLimit(t *testing.T) {
	broker := NewPaperBroker()
	result, err := broker.Submit(context.Background(), BrokerOrder{
		Account: Account{Cash: 100000},
		Order:   Order{Symbol: "600000.SH", Side: SideBuy, OrderType: OrderTypeLimit, Quantity: 1000, LimitPrice: 9.5},
		Quote:   QuoteView{Symbol: "600000.SH", Price: 10, PreviousClose: 9.9},
	})
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if result.Status != OrderStatusPending {
		t.Fatalf("现价高于限价应保持挂单，实际 %s", result.Status)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// 被拦的建议必须保留"原本想买多少"。
//
// 这条规则在 2026-10-20 的实盘回放里被发现：时间线上出现 9 条"买入 0 股"，
// 既无法解读，也无法区分"模型想买 100 股但被行业上限拦住"和
// "资金真的只够买 0 股"。被拦订单不冻结资金，保留数量不会造成暴露虚高——
// 所有下游计算都只看 pending 状态。
func TestRejectedOrdersKeepIntendedQuantity(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	snapshot.Sectors = map[string]string{"600001.SH": "银行", "600000.SH": "银行"}
	// 已有银行持仓 40%（4 万 / 10 万），本次再买 10% 会突破 45% 的行业上限。
	snapshot.Positions = []Position{{
		Symbol: "600001.SH", Name: "某银行", Quantity: 4000, Available: 4000,
		AverageCost: 10, LastPrice: 10, MarketValue: 40000,
	}}
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10,
		OrderType: "limit", LimitPrice: 10, StopLossPrice: 9.5,
	}}}
	orders, verdicts := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("行业集中度超限应被拒绝，实际 %+v", orders)
	}
	if orders[0].Quantity <= 0 {
		t.Fatalf("被拒订单应保留原本拟买数量，实际 %d 股；拦截原因：%+v", orders[0].Quantity, verdicts)
	}
	if orders[0].ReservedAmount != 0 {
		t.Fatalf("被拒订单不得冻结资金，实际 %.2f", orders[0].ReservedAmount)
	}
	if len(verdicts) == 0 {
		t.Fatal("应留下拦截原因，否则事后无法判断拦得对不对")
	}
}
