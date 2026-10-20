package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 建仓上下文必须跟着持仓走：入场环境、入场置信度、复核次数。
// 没有这三个字段，持仓只能回答"成本多少"，回答不了"当初在什么环境下、
// 多有把握时买的"，而后者才是复盘时判断"该不该继续持有"的依据。

func buyOrder(id, regime string, confidence float64) Order {
	return Order{
		ID: id, AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行",
		Side: SideBuy, OrderType: OrderTypeLimit, Quantity: 100, LimitPrice: 10,
		StopLossPrice: 9.5, Confidence: confidence, EntryRegime: regime,
		Status: OrderStatusPending, Source: OrderSourceModel,
	}
}

func TestPositionKeepsEntryContext(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.ApplyFill(ctx, buyOrder("ord-1", "range", 0.62), 100, 10, 0); err != nil {
		t.Fatalf("成交入账失败：%v", err)
	}
	positions, err := store.ListPositions(ctx, DefaultAccountID)
	if err != nil || len(positions) != 1 {
		t.Fatalf("读取持仓失败：%v / %+v", err, positions)
	}
	position := positions[0]
	if position.EntryRegime != "range" {
		t.Fatalf("入场环境应为 range，实际 %q", position.EntryRegime)
	}
	if position.EntryConfidence != 0.62 {
		t.Fatalf("入场置信度应为 0.62，实际 %.2f", position.EntryConfidence)
	}

	// 复核计数按轮累加，而不是按"触发次数"累加：
	// 论点被看过 40 次都没失效，和一次都没看过，是完全不同的两件事。
	for i := 0; i < 2; i++ {
		if err := store.BumpReviewCounts(ctx, DefaultAccountID); err != nil {
			t.Fatalf("累加复核次数失败：%v", err)
		}
	}
	positions, _ = store.ListPositions(ctx, DefaultAccountID)
	if positions[0].ReviewCount != 2 {
		t.Fatalf("复核次数应为 2，实际 %d", positions[0].ReviewCount)
	}

	// 加仓不覆盖入场上下文：它描述的是这笔仓位最初的论点从哪来。
	// 被第二次买入的 regime 覆盖之后，复盘时看到的就永远是最后一次加仓的环境。
	if _, err := store.ApplyFill(ctx, buyOrder("ord-2", "euphoria", 0.9), 100, 10, 0); err != nil {
		t.Fatalf("加仓成交失败：%v", err)
	}
	positions, _ = store.ListPositions(ctx, DefaultAccountID)
	if positions[0].EntryRegime != "range" || positions[0].EntryConfidence != 0.62 {
		t.Fatalf("加仓不应改写入场上下文，实际 %q / %.2f",
			positions[0].EntryRegime, positions[0].EntryConfidence)
	}
}

func TestRunProtectiveCountsReviews(t *testing.T) {
	// 复核计数必须在"真的看过"时累加：防守线每轮取到行情、逐只判过论点，
	// 才 +1。没有这条链，review_count 就只是一个永远为 0 的装饰字段。
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 10),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 9.5, 12, 10)

	if _, err := service.RunProtective(ctx, DefaultAccountID, "cyc-review"); err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	position, err := service.store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.ReviewCount != 1 {
		t.Fatalf("复核一轮后计数应为 1，实际 %d", position.ReviewCount)
	}
	// 再跑一轮继续累加：它计的是轮次，不是"是否触发"。
	if _, err := service.RunProtective(ctx, DefaultAccountID, "cyc-review"); err != nil {
		t.Fatalf("第二轮防守线执行失败：%v", err)
	}
	position, _ = service.store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if position.ReviewCount != 2 {
		t.Fatalf("两轮后计数应为 2，实际 %d", position.ReviewCount)
	}
}

func TestBuildOrdersStampsEntryRegime(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	draft := DraftPlan{Regime: "range", Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit",
		LimitPrice: 10.2, StopLossPrice: 9.5, Confidence: 0.6,
	}}}
	orders, _ := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 {
		t.Fatalf("期望 1 笔订单，实际 %d", len(orders))
	}
	// 市场状态未评估时退回模型自己写的 regime，而不是留空：
	// 留空等于这条持仓永远说不出自己在什么环境下建的。
	if orders[0].EntryRegime != "range" {
		t.Fatalf("订单应带上入场环境 range，实际 %q", orders[0].EntryRegime)
	}
}
