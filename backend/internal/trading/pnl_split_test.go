package trading

import (
	"context"
	"testing"

	"easy-stock/backend/internal/foundation"
)

// 用户会在账户卡片上把"总资产"和"浮动盈亏"并排比较。
// 这两个数字口径不同，所以本用例锁死中间项：
//
//	变动 = 已实现 + 浮动 + 残差
//
// 残差必须为 0。不为 0 就说明有一笔钱既没进已实现、也没进浮动，
// 那就是账错了，而不是"看起来不对"。
func TestPnLSplitAddsUp(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10.5, 10.4),
	})
	ctx := context.Background()
	if _, err := service.store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}

	orders, err := service.store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 300, 10)})
	if err != nil {
		t.Fatalf("下单失败：%v", err)
	}
	fee := TradeFee(SideBuy, 3000)
	if _, err := service.store.ApplyFill(ctx, orders[0], 300, 10, fee); err != nil {
		t.Fatalf("成交失败：%v", err)
	}

	account, positions, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.PnL == nil {
		t.Fatal("账户应带有盈亏拆解")
	}
	split := *account.PnL

	if split.Realized != 0 {
		t.Fatalf("没有平仓，已实现应为 0，实际 %.2f", split.Realized)
	}
	// 浮动应当是"毛利 - 买入费"：300 × (10.5 - 10) - 5.03。
	wantUnrealized := round2(positions[0].Unrealized)
	if split.Unrealized != wantUnrealized {
		t.Fatalf("浮动应为 %.2f，实际 %.2f", wantUnrealized, split.Unrealized)
	}
	if sum := round2(split.Realized + split.Unrealized + split.Residual); sum != split.Change {
		t.Fatalf("三项之和 %.2f 不等于变动 %.2f", sum, split.Change)
	}
	if !split.Aligned {
		t.Fatalf("口径统一时残差应约等于 0，实际 %.2f", split.Residual)
	}
	if split.Fees != fee {
		t.Fatalf("费用合计应为 %.2f，实际 %.2f", fee, split.Fees)
	}
}

// 残差不为 0 时必须如实报出来，而不是被抹平成 0——
// 这个字段的全部价值就在于它敢说"这里有一笔我解释不了的钱"。
func TestPnLSplitReportsResidual(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	if _, err := service.store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}

	orders, err := service.store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 300, 10)})
	if err != nil {
		t.Fatalf("下单失败：%v", err)
	}
	fee := TradeFee(SideBuy, 3000)
	if _, err := service.store.ApplyFill(ctx, orders[0], 300, 10, fee); err != nil {
		t.Fatalf("成交失败：%v", err)
	}
	// 人为制造 20 元无主现金：模拟"有笔账没进任何统计"的情况。
	if _, err := service.store.db.ExecContext(ctx,
		`UPDATE trading_accounts SET cash=cash+20 WHERE id=?`, DefaultAccountID); err != nil {
		t.Fatalf("注入现金失败：%v", err)
	}

	account, _, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.PnL == nil {
		t.Fatal("账户应带有盈亏拆解")
	}
	// 容差 2 分：成本存 4 位小数，乘回股数会带出舍入残差，与那 20 元无关。
	if gap := account.PnL.Residual - 20; gap > 0.02 || gap < -0.02 {
		t.Fatalf("残差应如实约等于 20，实际 %.2f", account.PnL.Residual)
	}
	if account.PnL.Aligned {
		t.Fatal("残差 20 元不该被判定为已对齐")
	}
}
