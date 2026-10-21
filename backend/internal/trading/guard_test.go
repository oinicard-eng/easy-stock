package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// seedPosition 造一笔已成交且已解禁的持仓，并把防守线一起写进去。
// 直接改库是为了绕开 T+1 的日期推进：测试不该依赖跑测试那天是不是交易日。
func seedPosition(t *testing.T, store *Store, symbol string, quantity int, cost, stop, takeProfit, peak float64) Position {
	t.Helper()
	ctx := context.Background()
	if _, err := store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}
	orders, err := store.CreateOrders(ctx, []Order{testBuyOrder(symbol, quantity, cost)})
	if err != nil {
		t.Fatalf("建仓下单失败：%v", err)
	}
	if orders[0].Status != OrderStatusPending {
		t.Fatalf("建仓订单应挂起：%s", orders[0].RejectReason)
	}
	fee := TradeFee(SideBuy, float64(quantity)*cost)
	if _, err := store.ApplyFill(ctx, orders[0], quantity, cost, fee); err != nil {
		t.Fatalf("建仓成交失败：%v", err)
	}
	peakPrice := peak
	if peakPrice <= 0 {
		peakPrice = cost
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_positions
		SET available_quantity=quantity, reserved_quantity=0, stop_loss_price=?, take_profit_price=?, peak_price=?
		WHERE account_id=? AND symbol=?`, round2(stop), round2(takeProfit), round2(peakPrice), DefaultAccountID, symbol); err != nil {
		t.Fatalf("写入防守线失败：%v", err)
	}
	position, err := store.GetPosition(ctx, DefaultAccountID, symbol)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	return position
}

// P3 的核心验收：两笔卖单不能各卖同一批股数。
func TestSellQuantityReservationBlocksOversell(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedPosition(t, store, "600000.SH", 1000, 10, 9, 12, 10)

	now := time.Now().UTC()
	makeSell := func(id string) Order {
		return Order{
			ID: id, PlanID: newID("plan"), AccountID: DefaultAccountID,
			Symbol: "600000.SH", Name: "浦发银行", Side: SideSell, OrderType: OrderTypeLimit,
			Quantity: 1000, LimitPrice: 10, Status: OrderStatusPending,
			Source: OrderSourceModel, Trigger: TriggerModelExit,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	first, err := store.CreateOrders(ctx, []Order{makeSell("ord-sell-1")})
	if err != nil {
		t.Fatalf("第一笔卖单失败：%v", err)
	}
	if first[0].Status != OrderStatusPending || first[0].ReservedQuantity != 1000 {
		t.Fatalf("第一笔卖单应冻结 1000 股：%+v", first[0])
	}
	position, err := store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.Sellable() != 0 {
		t.Fatalf("冻结后应无股可卖，实际 %d", position.Sellable())
	}

	second, err := store.CreateOrders(ctx, []Order{makeSell("ord-sell-2")})
	if err != nil {
		t.Fatalf("第二笔卖单不应报错，而应就地拒绝：%v", err)
	}
	if second[0].Status != OrderStatusRejected {
		t.Fatalf("第二笔卖单必须被拒（否则就是超卖），实际 %s", second[0].Status)
	}
	if !containsText(second[0].RejectReason, "可卖") {
		t.Fatalf("拒绝原因应说明可卖股数不足：%s", second[0].RejectReason)
	}

	// 撤销第一笔后股数必须回到可卖状态。先改状态再释放，
	// 与 Service.CancelOrder 的顺序一致——否则对账会把"已释放但仍挂起"记成漂移。
	order, err := store.GetOrder(ctx, "ord-sell-1")
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	order.Status = OrderStatusCancelled
	order.UpdatedAt = time.Now().UTC()
	if err := store.UpdateOrder(ctx, order); err != nil {
		t.Fatalf("更新订单失败：%v", err)
	}
	if _, err := store.ReleaseOrderReservation(ctx, "ord-sell-1"); err != nil {
		t.Fatalf("释放失败：%v", err)
	}
	position, err = store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.Sellable() != 1000 || position.ReservedQuantity != 0 {
		t.Fatalf("撤销后应恢复 1000 股可卖，实际可卖 %d、冻结 %d", position.Sellable(), position.ReservedQuantity)
	}

	report, err := store.ReconcileReservations(ctx, DefaultAccountID, false)
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if len(report.ReservedQuantityDrift) > 0 {
		t.Fatalf("对账应无股数漂移：%+v", report.ReservedQuantityDrift)
	}
}

// 止损必须自动执行：位置越过止损线，无需模型同意。
func TestProtectiveStopLossFillsAndRecordsClosedTrade(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 9.4, 10),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 9.5, 12, 10)

	run, err := service.RunProtective(ctx, DefaultAccountID, "cyc-test")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	if run.Triggered != 1 || run.Filled != 1 {
		t.Fatalf("应触发并成交 1 笔，实际 triggered=%d filled=%d notes=%v", run.Triggered, run.Filled, run.Notes)
	}
	if run.Orders[0].Trigger != TriggerStopLoss {
		t.Fatalf("触发原因应为 stop_loss，实际 %s", run.Orders[0].Trigger)
	}
	_, positions, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if len(positions) != 0 {
		t.Fatalf("清仓后不应还有持仓：%+v", positions)
	}

	trades, err := service.ClosedTrades(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取平仓记录失败：%v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("应写入 1 条已实现平仓，实际 %d", len(trades))
	}
	if trades[0].PnL >= 0 {
		t.Fatalf("止损平仓应为亏损，实际 %.2f", trades[0].PnL)
	}
	if trades[0].Trigger != TriggerStopLoss || trades[0].Source != OrderSourceProtective {
		t.Fatalf("平仓记录应保留离场原因：%+v", trades[0])
	}

	// 连败计数必须被记上：它是连败熔断的唯一数据来源。
	state, err := service.store.GetDayState(ctx, DefaultAccountID, TradingDay(time.Now()))
	if err != nil {
		t.Fatalf("读取当日状态失败：%v", err)
	}
	if state.StopStreak != 1 {
		t.Fatalf("止损后连败计数应为 1，实际 %d", state.StopStreak)
	}

	// 没有新的触发时不应重复下单。
	again, err := service.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("二次检查失败：%v", err)
	}
	if again.Triggered != 0 {
		t.Fatalf("已清仓不应再触发，实际 %d 笔", again.Triggered)
	}
}

// 分级止盈：第一次到价卖 40%，并把止损上移到成本价。
func TestProtectiveTakeProfitIsTieredAndMovesStop(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 11.2, 11),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 9.5, 11, 11.2)

	run, err := service.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	if run.Triggered != 1 || run.Filled != 1 {
		t.Fatalf("应触发并成交 1 笔：%+v", run)
	}
	if run.Orders[0].Trigger != TakeProfitTrigger(0) {
		t.Fatalf("触发原因应为 take_profit_1，实际 %s", run.Orders[0].Trigger)
	}
	if run.Orders[0].Quantity != 400 {
		t.Fatalf("第一级止盈应卖 40%%（400 股），实际 %d", run.Orders[0].Quantity)
	}
	position, err := service.store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.Quantity != 600 || position.ReduceCount != 1 {
		t.Fatalf("应剩 600 股且记一次减仓，实际 %d 股、%d 次", position.Quantity, position.ReduceCount)
	}
	// 止损上移到成本价，并按分向上取整——取整只能更保守，不能把止损压到成本下方。
	if position.StopLossPrice < position.AverageCost {
		t.Fatalf("止损 %.2f 不该低于成本 %.4f", position.StopLossPrice, position.AverageCost)
	}
	if position.StopLossPrice-position.AverageCost >= 0.01 {
		t.Fatalf("止损 %.2f 应贴近成本 %.4f，不该高出一分以上", position.StopLossPrice, position.AverageCost)
	}

	trades, err := service.ClosedTrades(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取平仓记录失败：%v", err)
	}
	if len(trades) != 1 || trades[0].PnL <= 0 {
		t.Fatalf("止盈平仓应盈利：%+v", trades)
	}
}

// 移动止损：盈利中被回撤打掉，且不能把"刚买入的小波动"当成回撤。
func TestProtectiveTrailStopNeedsArmedProfit(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 11.2, 12),
	})
	ctx := context.Background()
	// 成本 10、最高 12、现价 11.2：浮盈 12%，回撤 6.7%，超过 6% 阈值。
	seedPosition(t, service.store, "600000.SH", 1000, 10, 0, 0, 12)

	run, err := service.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	if run.Triggered != 1 || run.Orders[0].Trigger != TriggerTrailStop {
		t.Fatalf("应触发移动止损：%+v", run.Orders)
	}
	if run.Orders[0].Quantity != 1000 {
		t.Fatalf("移动止损应清仓，实际卖 %d 股", run.Orders[0].Quantity)
	}

	// 浮盈只有 2%（低于 4% 的启动门槛）时，同样幅度的回撤不应触发。
	service2 := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10.2, 10.4),
	})
	seedPosition(t, service2.store, "600000.SH", 1000, 10, 0, 0, 10.4)
	run2, err := service2.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	if run2.Triggered != 0 {
		t.Fatalf("浮盈未达启动门槛不应触发移动止损：%+v", run2.Orders)
	}
}

// 硬止损：模型没给止损线时，账户层必须有兜底。
func TestProtectiveHardStopWhenNoStopLine(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 8.8, 9),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 0, 0, 10)

	run, err := service.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	if run.Triggered != 1 || run.Orders[0].Trigger != TriggerHardStop {
		t.Fatalf("浮亏 12%% 应触发硬止损：%+v", run.Orders)
	}
}

// T+1 不可卖时必须留下"看见了但执行不了"的记录，而不是静默跳过。
func TestProtectiveReportsUnsellablePosition(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 9.0, 9.6),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 9.5, 12, 10)
	if _, err := service.store.db.ExecContext(ctx, `UPDATE trading_positions SET available_quantity=0 WHERE symbol=?`, "600000.SH"); err != nil {
		t.Fatalf("改可卖数量失败：%v", err)
	}

	run, err := service.RunProtective(ctx, DefaultAccountID, "")
	if err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}
	// 触发计数必须为 1（防守线确实被触发了），但订单数为 0。
	// 把这种情况记成"没触发"，等于把 T+1 的约束藏起来。
	if run.Triggered != 1 || run.Created != 0 {
		t.Fatalf("应记为触发 1 次但生成 0 笔订单，实际 triggered=%d created=%d", run.Triggered, run.Created)
	}
	if len(run.Notes) == 0 || !containsText(run.Notes[0], "可卖") {
		t.Fatalf("应说明触发但无法执行：%v", run.Notes)
	}
}

// 复盘：样本不足时不给胜率，达到下限才给。
func TestBuildLessonsRespectsSampleFloor(t *testing.T) {
	few := make([]ClosedTrade, 0, 10)
	for index := 0; index < 10; index++ {
		few = append(few, ClosedTrade{Symbol: "600000.SH", Sector: "银行", Trigger: TriggerStopLoss, PnL: -100, PnLPercent: -5})
	}
	lessons := BuildLessons(few)
	if len(lessons) == 0 {
		t.Fatal("应产生经验条目")
	}
	for _, lesson := range lessons {
		if lesson.Reliable || lesson.WinRate != nil {
			t.Fatalf("样本 %d 不足 30，不应给出胜率：%+v", lesson.Sample, lesson)
		}
		if !containsText(lesson.Text, "样本不足") {
			t.Fatalf("样本不足应在文案里说明：%s", lesson.Text)
		}
	}

	many := make([]ClosedTrade, 0, reviewSampleFloor)
	for index := 0; index < reviewSampleFloor; index++ {
		pnl := -100.0
		if index%2 == 0 {
			pnl = 150
		}
		many = append(many, ClosedTrade{Symbol: "600000.SH", Sector: "银行", Trigger: TriggerStopLoss, PnL: pnl, PnLPercent: 1})
	}
	for _, lesson := range BuildLessons(many) {
		if lesson.Sample >= reviewSampleFloor && (lesson.WinRate == nil || !lesson.Reliable) {
			t.Fatalf("样本达到下限必须给出胜率：%+v", lesson)
		}
	}
}

// 复盘数字必须来自账本，且重算不产生第二条记录。
func TestGenerateReviewIsIdempotentAndUsesLedgerFacts(t *testing.T) {
	service := newTestService(t, "", map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 11.2, 11),
	})
	ctx := context.Background()
	seedPosition(t, service.store, "600000.SH", 1000, 10, 9.5, 11, 11.2)
	if _, err := service.RunProtective(ctx, DefaultAccountID, ""); err != nil {
		t.Fatalf("防守线执行失败：%v", err)
	}

	day := TradingDay(time.Now())
	first, err := service.GenerateReview(ctx, DefaultAccountID, day)
	if err != nil {
		t.Fatalf("生成复盘失败：%v", err)
	}
	if first.Facts.ClosedTrades != 1 || first.Facts.Wins != 1 {
		t.Fatalf("复盘事实应与账本一致：%+v", first.Facts)
	}
	if first.Facts.TakeProfitExits != 1 {
		t.Fatalf("应记 1 笔止盈离场：%+v", first.Facts)
	}
	if len(first.Narrative) == 0 || len(first.Lessons) == 0 {
		t.Fatalf("复盘应有摘要与经验：%+v", first)
	}

	second, err := service.GenerateReview(ctx, DefaultAccountID, day)
	if err != nil {
		t.Fatalf("重算复盘失败：%v", err)
	}
	if second.Facts.RealizedPnL != first.Facts.RealizedPnL {
		t.Fatalf("重算结果应一致：%.2f vs %.2f", first.Facts.RealizedPnL, second.Facts.RealizedPnL)
	}
	reviews, err := service.Reviews(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取复盘失败：%v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("同一天重算不应新增记录，实际 %d 条", len(reviews))
	}
}

// 对账必须能发现"挂卖单但持仓已不存在"这种孤儿状态。
func TestReconcileDetectsOrphanSellReservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedPosition(t, store, "600000.SH", 1000, 10, 9, 12, 10)
	orders, err := store.CreateOrders(ctx, []Order{{
		ID: newID("ord"), PlanID: newID("plan"), AccountID: DefaultAccountID,
		Symbol: "600000.SH", Name: "浦发银行", Side: SideSell, OrderType: OrderTypeLimit,
		Quantity: 1000, LimitPrice: 10, Status: OrderStatusPending,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatalf("卖出下单失败：%v", err)
	}
	if orders[0].Status != OrderStatusPending {
		t.Fatalf("卖单应挂起：%s", orders[0].RejectReason)
	}
	// 人为制造漂移：账本冻结被改掉，但挂单还在。
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_positions SET reserved_quantity=0 WHERE symbol=?`, "600000.SH"); err != nil {
		t.Fatalf("制造漂移失败：%v", err)
	}
	report, err := store.ReconcileReservations(ctx, DefaultAccountID, false)
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if len(report.ReservedQuantityDrift) == 0 {
		t.Fatal("应发现股数冻结漂移")
	}
	if _, err := store.ReconcileReservations(ctx, DefaultAccountID, true); err != nil {
		t.Fatalf("修复失败：%v", err)
	}
	position, err := store.GetPosition(ctx, DefaultAccountID, "600000.SH")
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if position.ReservedQuantity != 1000 {
		t.Fatalf("修复后应恢复 1000 股冻结，实际 %d", position.ReservedQuantity)
	}
}
