package trading

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

func afterHours(day int) time.Time {
	return time.Date(2026, 10, day, 21, 35, 0, 0, time.FixedZone("CST", 8*3600))
}

func draftPlanWithBuy(t *testing.T, service *Service) Plan {
	t.Helper()
	plan, err := service.StartPlan(context.Background(), PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced",
	})
	if err != nil {
		t.Fatalf("启动计划失败：%v", err)
	}
	ready := waitPlan(t, service, plan.ID)
	if ready.Status != PlanStatusDraft {
		t.Fatalf("期望待确认状态，实际 %s（%s）", ready.Status, ready.ErrorMessage)
	}
	if len(ready.Orders) == 0 {
		t.Fatal("用例需要一个挂起买单")
	}
	return ready
}

// 盘后点"执行"必须被拒绝。
//
// 这不是保守，而是纠正一个真实事故：2026-10-20 晚上 21:35 有 9 笔委托在
// 收盘后被"成交"，撮合用的是当日收盘价。第二天开盘普遍低开 4% 左右，
// 模型看到浮亏就清仓，合计亏 1,157.67 元——而这笔亏损在现实里不会发生，
// 因为 A 股收市后根本没有撮合。
func TestExecuteRejectsOutsideTradingSession(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试计划","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ready := draftPlanWithBuy(t, service)

	service.now = func() time.Time { return afterHours(20) }
	if _, err := service.Execute(context.Background(), ready.ID); !errors.Is(err, ErrMarketClosed) {
		t.Fatalf("盘后执行必须返回 ErrMarketClosed，实际 %v", err)
	}
	// 被拒绝之后：计划仍是草稿，订单仍是挂起，不能出现"半执行"状态。
	after, err := service.GetPlan(context.Background(), ready.ID)
	if err != nil {
		t.Fatalf("读取计划失败：%v", err)
	}
	if after.Status != PlanStatusDraft {
		t.Fatalf("盘后被拒的计划应保持草稿，实际 %s", after.Status)
	}
	for _, order := range after.Orders {
		if order.Status == OrderStatusFilled || order.FilledQuantity > 0 {
			t.Fatalf("盘后不应产生成交：%+v", order)
		}
	}
}

// 第二层防线：MatchPending 不经过 Execute 的时段检查，
// 手动刷新与事件通道都会直接调它，所以它自己必须挡住休市撮合。
func TestMatchPendingKeepsOrdersAfterHours(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试计划","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	draftPlanWithBuy(t, service)

	service.now = func() time.Time { return afterHours(20) }
	orders, err := service.MatchPending(context.Background(), DefaultAccountID)
	if err != nil {
		t.Fatalf("休市撮合不应报错，应原样保留挂单：%v", err)
	}
	for _, order := range orders {
		if order.Status == OrderStatusFilled || order.FilledQuantity > 0 {
			t.Fatalf("休市期间不得成交：%+v", order)
		}
	}
}

// 归因标签：交易员的每一次模型调用都必须带上 trading 模块名，
// 否则用量面板上看到的是"其他模块花了多少钱"，无法回答"这个交易员一天烧多少"。
func TestTradingCallsTaggedForUsage(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试计划","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var modules []string
	service := NewService(store, fakeGateway{content: content, modules: &modules},
		fakeQuotes{quotes: map[string]foundation.Quote{
			"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
		}}, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(false)
	service.now = func() time.Time { return testSessionTime() }

	plan, err := service.StartPlan(context.Background(), PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"})
	if err != nil {
		t.Fatalf("启动计划失败：%v", err)
	}
	waitPlan(t, service, plan.ID)

	if len(modules) == 0 {
		t.Fatal("应至少调用一次模型")
	}
	for _, module := range modules {
		if module != UsageModuleTrading {
			t.Fatalf("模型调用的用量模块应为 %q，实际 %q", UsageModuleTrading, module)
		}
	}
}

// 盘后生成的计划，提示词必须说清楚"看到的是收盘价、现在不能成交"。
// 不说的话，模型会把收盘价当现价算浮亏、按昨收价挂限价，第二天开盘全错。
func TestPlanPromptDeclaresSessionState(t *testing.T) {
	input := PlanPromptInput{
		Account: Account{ID: DefaultAccountID, Cash: 100000, Equity: 100000},
		Rules:   DefaultRiskRules(),
		Now:     afterHours(20),
	}
	prompt := BuildPlanPrompt(input)
	if !strings.Contains(prompt, "非连续竞价时段") {
		t.Fatal("盘后提示词必须标注时段，否则模型会把收盘价当现价")
	}

	input.Now = testSessionTime()
	if strings.Contains(BuildPlanPrompt(input), "非连续竞价时段") {
		t.Fatal("盘中提示词不该出现盘后说明，那会让模型以为现在不能交易")
	}
}
