package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 模拟撮合的验收标准：滑点方向永远对自己不利；涨跌停不成交；
// 部分成交比例对同一订单是确定的（否则回放不可复现）。

func brokerRequest(side Side, quantity int, limit, price, previousClose float64) BrokerOrder {
	return BrokerOrder{
		Account: Account{ID: DefaultAccountID, Cash: 1_000_000, ReservedCash: 0},
		Position: Position{
			AccountID: DefaultAccountID, Symbol: "600000.SH", Quantity: 100000,
			Available: 100000, AverageCost: 10,
		},
		Order: Order{
			ID: "ord-fixed", AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行",
			Side: side, OrderType: OrderTypeLimit, Quantity: quantity, LimitPrice: limit,
			Status: OrderStatusPending,
		},
		Quote: QuoteView{Symbol: "600000.SH", Name: "浦发银行", Price: price, PreviousClose: previousClose},
	}
}

func TestPaperBrokerAppliesAdverseSlippage(t *testing.T) {
	broker := NewPaperBroker()
	ctx := context.Background()

	buy := brokerRequest(SideBuy, 1000, 10.20, 10.00, 10.00)
	result, err := broker.Submit(ctx, buy)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if result.Status != OrderStatusFilled {
		t.Fatalf("限价内应成交：%s", result.Reason)
	}
	if result.Price <= 10.00 {
		t.Fatalf("买入成交价应高于现价（滑点不利），实际 %.3f", result.Price)
	}
	if result.Price > 10.20 {
		t.Fatalf("成交价不得突破限价，实际 %.3f", result.Price)
	}

	sell := brokerRequest(SideSell, 1000, 9.80, 10.00, 10.00)
	result, err = broker.Submit(ctx, sell)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if result.Price >= 10.00 {
		t.Fatalf("卖出成交价应低于现价（滑点不利），实际 %.3f", result.Price)
	}
	if result.Price < 9.80 {
		t.Fatalf("成交价不得低于限价，实际 %.3f", result.Price)
	}
}

func TestPaperBrokerDoesNotFillAtLimitUp(t *testing.T) {
	broker := NewPaperBroker()
	// 现价已经是涨停价（10% 板，昨收 10 → 涨停 11）。
	request := brokerRequest(SideBuy, 1000, 11, 11, 10)
	result, err := broker.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if result.Status == OrderStatusFilled || result.Status == OrderStatusPartial {
		t.Fatalf("涨停时买单不应成交，实际 %s", result.Status)
	}
	if result.Status != OrderStatusPending {
		t.Fatalf("涨停时应保持挂单等待，实际 %s（%s）", result.Status, result.Reason)
	}
}

func TestPaperBrokerPartialFillIsDeterministic(t *testing.T) {
	broker := NewPaperBroker()
	// 1000 股 × 1000 元 = 100 万，落在"大额"档，必然部分成交。
	request := brokerRequest(SideBuy, 1000, 1100, 1000, 1000)
	request.Account.Cash = 2_000_000

	first, err := broker.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if first.Status != OrderStatusPartial {
		t.Fatalf("大额单应部分成交，实际 %s（成交 %d 股）", first.Status, first.FilledQuantity)
	}
	// 同一订单重复撮合必须得到同一个数量：回放的可复现性依赖这一点。
	for i := 0; i < 3; i++ {
		again, err := broker.Submit(context.Background(), request)
		if err != nil {
			t.Fatalf("重复撮合失败：%v", err)
		}
		if again.FilledQuantity != first.FilledQuantity {
			t.Fatalf("同一订单的部分成交数量应确定，第一次 %d，第 %d 次 %d",
				first.FilledQuantity, i+2, again.FilledQuantity)
		}
	}
	// 不同订单不应全部卡在同一个比例上（否则"确定性"会变成"一律 50%"）。
	other := request
	other.Order.ID = "ord-other"
	second, err := broker.Submit(context.Background(), other)
	if err != nil {
		t.Fatalf("撮合另一笔失败：%v", err)
	}
	if second.FilledQuantity == first.FilledQuantity && first.FilledQuantity == 500 {
		t.Fatalf("不同订单的比例不应完全一致：均为 %d 股", first.FilledQuantity)
	}
}

func TestSnapshotFingerprintNoticesCooldown(t *testing.T) {
	// 冷静期必须进入变化检测指纹：止损后的一轮是"最该重新问一次"的那一轮，
	// 指纹不变会让它被当成"什么都没变"直接跳过。
	snapshot := Snapshot{
		Equity: 100000, Cash: 50000,
		Quotes: []foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)},
	}
	before := SnapshotFingerprint(snapshot, nil, nil)
	snapshot.Cooldowns = CooldownView{Symbols: map[string]Cooldown{
		"600000.SH": {Symbol: "600000.SH", Until: time.Now().UTC()},
	}}
	after := SnapshotFingerprint(snapshot, nil, nil)
	if before == after {
		t.Fatal("冷静期变化应改变指纹，否则止损后的那一轮会被跳过")
	}
}
