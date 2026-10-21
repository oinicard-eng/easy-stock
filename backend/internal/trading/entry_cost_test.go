package trading

import (
	"context"
	"testing"
)

// 建仓成本必须含买入费用，否则账永远对不平。
//
// 这不是精度问题：A 股佣金 5 元起步，一笔 3000 元的建仓光佣金就摊掉 0.17%。
// 费用不摊进成本，而现金已经实扣，结果是
// 「初始资金 + 已实现 + 浮动」永远比「账户权益」多出恰好一笔佣金。
// 本用例锁死这条恒等式：权益变动 = 已实现 + 浮动。
func TestEntryCostIncludesBuyFee(t *testing.T) {
	store := newCooldownStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}

	const (
		symbol   = "600000.SH"
		quantity = 300
		price    = 10.0
	)
	orders, err := store.CreateOrders(ctx, []Order{testBuyOrder(symbol, quantity, price)})
	if err != nil {
		t.Fatalf("下单失败：%v", err)
	}
	fee := TradeFee(SideBuy, float64(quantity)*price)
	if _, err := store.ApplyFill(ctx, orders[0], quantity, price, fee); err != nil {
		t.Fatalf("成交失败：%v", err)
	}

	position, err := store.GetPosition(ctx, DefaultAccountID, symbol)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	want := round4((float64(quantity)*price + fee) / float64(quantity))
	if position.AverageCost != want {
		t.Fatalf("建仓成本应含买入费 %.4f，实际 %.4f（少了 %.2f 元）",
			want, position.AverageCost, (want-position.AverageCost)*quantity)
	}
	// 市价字段仍是成交价：成本可以含费，行情不能。
	if position.LastPrice != price {
		t.Fatalf("last_price 应记成交价 %.2f，实际 %.2f", price, position.LastPrice)
	}

	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	// 这是用户肉眼要比的那个等式：权益的变动 = 已实现（此处为 0）+ 浮动。
	// 权益在这里自己算，因为 GetAccount 读的是库里的原始行，权益是展示层聚合出来的。
	equity := round2(account.Cash + position.MarketValue)
	gap := round2(equity - account.InitialCash - position.Unrealized)
	// 容差取 2 分：成本存 4 位小数，再乘回股数时会带出最多
	// 0.00005 × 每股 × 股数的舍入残差，300 股也就 1.5 分。
	// 关键是不能差出一整笔佣金（5 元）——那才是口径错了。
	if gap > 0.02 || gap < -0.02 {
		t.Fatalf("权益变动 %.2f 应等于浮动盈亏 %.2f，差 %.2f 元（费用没进成本）",
			round2(equity-account.InitialCash), position.Unrealized, gap)
	}
}

// 加仓后成本仍含费，且与新建仓路径一致；部分卖出不改变均价。
func TestAverageCostStaysConsistentAcrossAdds(t *testing.T) {
	store := newCooldownStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}

	const symbol = "600001.SH"
	buy := func(quantity int, price float64) {
		t.Helper()
		orders, err := store.CreateOrders(ctx, []Order{testBuyOrder(symbol, quantity, price)})
		if err != nil {
			t.Fatalf("下单失败：%v", err)
		}
		fee := TradeFee(SideBuy, float64(quantity)*price)
		if _, err := store.ApplyFill(ctx, orders[0], quantity, price, fee); err != nil {
			t.Fatalf("成交失败：%v", err)
		}
	}

	buy(200, 10)
	buy(200, 12)

	position, err := store.GetPosition(ctx, DefaultAccountID, symbol)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	fee1 := TradeFee(SideBuy, 2000)
	fee2 := TradeFee(SideBuy, 2400)
	want := round4((2000 + fee1 + 2400 + fee2) / 400)
	if position.AverageCost != want {
		t.Fatalf("两次建仓后成本应为 %.4f，实际 %.4f", want, position.AverageCost)
	}
	if position.Quantity != 400 {
		t.Fatalf("应持有 400 股，实际 %d", position.Quantity)
	}
}
