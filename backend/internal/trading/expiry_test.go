package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/trading/marketrule"
)

func mustCST(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.FixedZone("CST", 8*3600))
	if err != nil {
		t.Fatalf("解析时间失败：%v", err)
	}
	return parsed
}

// 委托的生命周期边界必须落在"最近一个已收盘交易日"，否则周一早上清不掉周五的单。
func TestLastSessionCloseBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string
		want string
	}{
		{"盘中回退到上一交易日", "2026-10-20 10:00", "2026-10-19 15:00"},
		{"收盘后为当日", "2026-10-20 16:00", "2026-10-20 15:00"},
		{"周一开盘前回到周五", "2026-10-19 09:10", "2026-10-16 15:00"},
		{"节假日回到节前最后交易日", "2026-10-03 12:00", "2026-09-30 15:00"},
		{"节后首日开盘前仍是节前", "2026-10-08 09:10", "2026-09-30 15:00"},
		{"节后首日收盘后为当日", "2026-10-08 15:30", "2026-10-08 15:00"},
	}
	for _, item := range cases {
		got := marketrule.LastSessionClose(mustCST(t, item.now))
		want := mustCST(t, item.want)
		if !got.Equal(want) {
			t.Fatalf("%s：期望 %s，实际 %s", item.name, want.Format(time.RFC3339), got.Format(time.RFC3339))
		}
	}
}

// 节假日不得被当成交易时段：否则循环会在停市日反复调模型。
func TestTradingSessionSkipsExchangeHoliday(t *testing.T) {
	holiday := mustCST(t, "2026-10-02 10:00")
	if marketrule.IsTradingSession(holiday) {
		t.Fatal("国庆假期不应判定为交易时段")
	}
	if !marketrule.IsTradingSession(mustCST(t, "2026-10-20 10:00")) {
		t.Fatal("普通交易日盘中应判定为交易时段")
	}
}

// 昨日未成交的买单必须被作废并释放冻结，否则这笔钱会被永久锁住。
func TestExpireStaleOrdersReleasesBuyReservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	order := testBuyOrder("600000.SH", 1000, 10)
	order.CreatedAt = time.Now().UTC().AddDate(0, 0, -3)
	stored, err := store.CreateOrders(ctx, []Order{order})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	if stored[0].Status != OrderStatusPending || stored[0].ReservedAmount <= 0 {
		t.Fatalf("前置条件不成立：%s 冻结 %.2f", stored[0].Status, stored[0].ReservedAmount)
	}
	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash <= 0 {
		t.Fatalf("前置条件不成立：冻结额应大于 0，实际 %.2f", account.ReservedCash)
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	result, err := store.ExpireStaleOrders(ctx, DefaultAccountID, cutoff)
	if err != nil {
		t.Fatalf("作废失败：%v", err)
	}
	if result.Expired != 1 {
		t.Fatalf("应作废 1 笔，实际 %d", result.Expired)
	}
	expired, err := store.GetOrder(ctx, stored[0].ID)
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if expired.Status != OrderStatusExpired {
		t.Fatalf("订单状态应为 expired，实际 %s", expired.Status)
	}
	if expired.ReservedAmount != 0 || expired.ReservedQuantity != 0 {
		t.Fatalf("作废后不应残留冻结，实际 %.2f / %d", expired.ReservedAmount, expired.ReservedQuantity)
	}
	account, err = store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash != 0 {
		t.Fatalf("冻结额应清零，实际 %.2f", account.ReservedCash)
	}
	if account.AvailableCash() != DefaultInitialCash {
		t.Fatalf("可用资金应回到初始值，实际 %.2f", account.AvailableCash())
	}

	// 幂等：同一批委托不会二次释放。
	again, err := store.ExpireStaleOrders(ctx, DefaultAccountID, cutoff)
	if err != nil {
		t.Fatalf("重复作废失败：%v", err)
	}
	if again.Expired != 0 || again.ReleasedCash != 0 {
		t.Fatalf("重复调用应无动作，实际 %d 笔 / %.2f 元", again.Expired, again.ReleasedCash)
	}
}

// 当日挂单在盘中不能被作废：它还应该有机会成交。
func TestExpireStaleOrdersKeepsTodayOrders(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	order := testBuyOrder("600000.SH", 1000, 10)
	stored, err := store.CreateOrders(ctx, []Order{order})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	cutoff := time.Now().UTC().Add(-time.Hour)
	result, err := store.ExpireStaleOrders(ctx, DefaultAccountID, cutoff)
	if err != nil {
		t.Fatalf("作废失败：%v", err)
	}
	if result.Expired != 0 {
		t.Fatalf("当日委托不应被作废，实际作废 %d 笔", result.Expired)
	}
	pending, err := store.GetOrder(ctx, stored[0].ID)
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if pending.Status != OrderStatusPending {
		t.Fatalf("订单应保持挂起，实际 %s", pending.Status)
	}
}

// 卖单作废必须同时释放股数冻结：只放资金不放股数，持仓会永远卖不出去。
func TestExpireStaleOrdersReleasesSellReservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	buy := testBuyOrder("600000.SH", 1000, 10)
	stored, err := store.CreateOrders(ctx, []Order{buy})
	if err != nil {
		t.Fatalf("买入落库失败：%v", err)
	}
	if _, err := store.ApplyFill(ctx, stored[0], 1000, 10, TradeFee(SideBuy, 10000)); err != nil {
		t.Fatalf("买入成交失败：%v", err)
	}
	// 模拟 T+1 已过：买入当日的可卖数量为 0。
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_positions
		SET available_quantity=quantity WHERE account_id=? AND symbol=?`, DefaultAccountID, "600000.SH"); err != nil {
		t.Fatalf("调整可卖数量失败：%v", err)
	}

	sell := Order{
		ID: newID("ord"), PlanID: newID("plan"), AccountID: DefaultAccountID,
		Symbol: "600000.SH", Name: "600000.SH", Side: SideSell, OrderType: OrderTypeLimit,
		Quantity: 500, LimitPrice: 11, Status: OrderStatusPending,
		Trigger:   "model_exit",
		CreatedAt: time.Now().UTC().AddDate(0, 0, -2),
		UpdatedAt: time.Now().UTC().AddDate(0, 0, -2),
	}
	sellStored, err := store.CreateOrders(ctx, []Order{sell})
	if err != nil {
		t.Fatalf("卖单落库失败：%v", err)
	}
	if sellStored[0].ReservedQuantity != 500 {
		t.Fatalf("卖单应冻结 500 股，实际 %d", sellStored[0].ReservedQuantity)
	}
	position, err := store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.Sellable() != 500 {
		t.Fatalf("冻结后可卖应为 500 股，实际 %d", position.Sellable())
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	result, err := store.ExpireStaleOrders(ctx, DefaultAccountID, cutoff)
	if err != nil {
		t.Fatalf("作废失败：%v", err)
	}
	if result.Expired != 1 || result.ReleasedQty != 500 {
		t.Fatalf("应作废 1 笔并释放 500 股，实际 %d 笔 / %d 股", result.Expired, result.ReleasedQty)
	}
	position, err = store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.ReservedQuantity != 0 {
		t.Fatalf("作废后冻结股数应清零，实际 %d", position.ReservedQuantity)
	}
	if position.Sellable() != 1000 {
		t.Fatalf("作废后可卖应恢复为 1000 股，实际 %d", position.Sellable())
	}
}
