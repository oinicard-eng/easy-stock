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
	// 当日成交额 1 亿 → 单笔上限 50 万；1000 股 × 1000 元 = 100 万，只能成一半。
	request := brokerRequest(SideBuy, 1000, 1100, 1000, 1000)
	request.Account.Cash = 2_000_000
	request.Quote.Turnover = 100_000_000

	first, err := broker.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	if first.Status != OrderStatusPartial {
		t.Fatalf("超过成交额 0.5%% 的单应部分成交，实际 %s（成交 %d 股）", first.Status, first.FilledQuantity)
	}
	if first.FilledQuantity != 400 {
		// 50 万上限 ÷ 含滑点价 1000.30 ≈ 499 股，按 100 股整手向下取整得 400 股。
		t.Fatalf("50 万上限应成交 400 股（整手取整），实际 %d 股", first.FilledQuantity)
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
	// 成交额更大的票，同样金额能全额成交：上限是随流动性变的，不是固定金额。
	deep := request
	deep.Quote.Turnover = 300_000_000
	result, err := broker.Submit(context.Background(), deep)
	if err != nil {
		t.Fatalf("撮合另一笔失败：%v", err)
	}
	if result.Status != OrderStatusFilled || result.FilledQuantity != 1000 {
		t.Fatalf("成交额 3 亿时上限 150 万，应全额成交 1000 股，实际 %s / %d 股",
			result.Status, result.FilledQuantity)
	}
}

func TestPaperBrokerSlippageFollowsTurnover(t *testing.T) {
	// 成交额越冷，滑点越大；缺失时按最冷档，不能因为"没数据"就变便宜。
	broker := NewPaperBroker()
	price := func(turnover float64) float64 {
		request := brokerRequest(SideBuy, 100, 10.5, 10, 10)
		request.Quote.Turnover = turnover
		result, err := broker.Submit(context.Background(), request)
		if err != nil || result.Status != OrderStatusFilled {
			t.Fatalf("成交额 %.0f 的撮合失败：%v / %s", turnover, err, result.Reason)
		}
		return result.Price
	}
	illiquid, normal, deep, unknown := price(0), price(100_000_000), price(500_000_000), price(0)
	if !(illiquid > normal && normal > deep) {
		t.Fatalf("滑点应随成交额递减：冷门 %.2f，正常 %.2f，深票 %.2f", illiquid, normal, deep)
	}
	if illiquid != unknown {
		t.Fatalf("成交额缺失应按最冷档，%.2f != %.2f", unknown, illiquid)
	}
	// 冷门档 0.3%：10 元票买入价应在 10.03 附近，而不是分毫不动。
	if illiquid < 10.02 {
		t.Fatalf("冷门票买入滑点偏小：%.3f", illiquid)
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
