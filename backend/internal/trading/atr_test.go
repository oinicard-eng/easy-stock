package trading

import (
	"context"
	"testing"
	"time"
)

// ATR 校正的验收标准：三种错配（缺失、太近、太远）都要被修，
// 修完仍受账户级最大止损约束，且合理区间内不动手。

func TestATRAdjustStopBranches(t *testing.T) {
	const price = 10.0
	const atr = 0.20 // ATR 占现价 2%
	const maxStop = 10.0

	// 1. 未给止损：按 2.2 倍 ATR 兜底 → 10 - 0.44 = 9.56
	adjusted, note := atrAdjustStop(0, price, atr, maxStop)
	if adjusted != 9.56 || note == "" {
		t.Fatalf("缺失止损应按 2.2 ATR 兜底，实际 %.2f（%s）", adjusted, note)
	}
	if !atrDistanceOK(price, adjusted, atr) {
		t.Fatalf("兜底后的距离应落在合理区间：%.2f", adjusted)
	}

	// 2. 太近：0.1 元（0.5 倍 ATR），会被日常波动扫掉 → 放宽到 9.56
	adjusted, note = atrAdjustStop(9.9, price, atr, maxStop)
	if adjusted != 9.56 || note == "" {
		t.Fatalf("过近的止损应放宽，实际 %.2f（%s）", adjusted, note)
	}

	// 3. 太远：8.8 元（6 倍 ATR），单笔风险敞口过大 → 收紧到 9.30
	adjusted, note = atrAdjustStop(8.8, price, atr, maxStop)
	if adjusted != 9.30 || note == "" {
		t.Fatalf("过远的止损应收紧到 3.5 ATR，实际 %.2f（%s）", adjusted, note)
	}

	// 4. 合理区间内不动手：9.6 元约 2 倍 ATR
	if adjusted, note = atrAdjustStop(9.6, price, atr, maxStop); note != "" {
		t.Fatalf("合理距离不该被改动，实际改为 %.2f（%s）", adjusted, note)
	}

	// 5. 账户约束优先：ATR 很大时也不能突破最大止损比例。
	adjusted, _ = atrAdjustStop(0, price, 1.0, maxStop) // 2.2 ATR = 2.2 元，超出 10%
	if adjusted < 9.0 {
		t.Fatalf("校正结果不得低于最大止损线 9.00，实际 %.2f", adjusted)
	}
}

func TestATRTrailOnlyMovesUp(t *testing.T) {
	store := newCooldownStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAccount(ctx, DefaultAccountID, 100000); err != nil {
		t.Fatalf("建账户失败：%v", err)
	}
	// 直接落一笔持仓，止损 8.0，现价 10.0。
	order := Order{
		ID: newID("ord"), AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行",
		Side: SideBuy, OrderType: OrderTypeMarket, Quantity: 1000, LimitPrice: 10,
		Status: OrderStatusPending, StopLossPrice: 8, CreatedAt: time.Now().UTC(),
	}
	if _, err := store.CreateOrders(ctx, []Order{order}); err != nil {
		t.Fatalf("创建订单失败：%v", err)
	}
	filled, err := store.ApplyFill(ctx, order, 1000, 10, TradeFee(SideBuy, 10000))
	if err != nil {
		t.Fatalf("成交失败：%v", err)
	}
	if filled.Status != OrderStatusFilled {
		t.Fatalf("应全部成交：%s", filled.RejectReason)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_positions SET stop_loss_price=8,last_price=10 WHERE account_id=?`, DefaultAccountID); err != nil {
		t.Fatalf("准备持仓失败：%v", err)
	}
	if err := store.SetStopLoss(ctx, DefaultAccountID, "600000.SH", 9.2, time.Now().UTC()); err != nil {
		t.Fatalf("上移止损失败：%v", err)
	}
	position, err := store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.StopLossPrice != 9.2 {
		t.Fatalf("止损应上移到 9.2，实际 %.2f", position.StopLossPrice)
	}
	// 关键不变量：下移必须被 SQL 层拒绝，而不是靠调用方自觉。
	if err := store.SetStopLoss(ctx, DefaultAccountID, "600000.SH", 8.5, time.Now().UTC()); err != nil {
		t.Fatalf("下移调用不应报错：%v", err)
	}
	position, _ = store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if position.StopLossPrice != 9.2 {
		t.Fatalf("止损不得下移，实际变成 %.2f", position.StopLossPrice)
	}
}

func TestConfidenceFactorOnlyCutsLowValues(t *testing.T) {
	cases := []struct {
		value  float64
		factor float64
	}{
		{0, 1}, // 未填，不罚
		{0.3, 0.7},
		{0.5, 0.9},
		{0.62, 1}, // 表格里的默认区间，不该被打折
		{0.9, 1},
	}
	for _, item := range cases {
		factor, _ := confidenceFactor(item.value)
		if factor != item.factor {
			t.Fatalf("置信度 %.2f 的系数应为 %.2f，实际 %.2f", item.value, item.factor, factor)
		}
	}
}
