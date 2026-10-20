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
	// 按限价 10.2 计算 10% 仓位：10000/10.2=980 股，向下取整手为 900 股。
	if order.Quantity != 900 {
		t.Fatalf("按限价折算应为 900 股，实际 %d", order.Quantity)
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
	if len(ready.Orders) != 1 || ready.Orders[0].Quantity != 900 {
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
	if len(positions) != 1 || positions[0].Quantity != 900 {
		t.Fatalf("持仓异常：%+v", positions)
	}
	if positions[0].Available != 0 {
		t.Fatalf("T+1 当日买入不可卖，实际可卖 %d", positions[0].Available)
	}
	expectedCash := 100000 - 900*10 - executed.Orders[0].Fee
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
