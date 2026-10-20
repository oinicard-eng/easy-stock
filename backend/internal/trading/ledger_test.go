package trading

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.EnsureAccount(context.Background(), DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("创建账户失败：%v", err)
	}
	return store
}

func testBuyOrder(symbol string, quantity int, limitPrice float64) Order {
	now := time.Now().UTC()
	return Order{
		ID: newID("ord"), PlanID: newID("plan"), AccountID: DefaultAccountID,
		Symbol: symbol, Name: symbol, Side: SideBuy, OrderType: OrderTypeLimit,
		Quantity: quantity, LimitPrice: limitPrice, Status: OrderStatusPending,
		CreatedAt: now, UpdatedAt: now,
	}
}

// 这是 P2.1 的核心验收：两笔挂单不能共享同一笔现金。
// 若冻结缺失，第二笔会以 6 万成交并让账户现金变成负数。
func TestCreateOrdersBlocksSecondPendingBuyFromSameCash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := testBuyOrder("600000.SH", 6000, 10) // 6 万 + 费用
	second := testBuyOrder("000001.SZ", 6000, 10)
	stored, err := store.CreateOrders(ctx, []Order{first, second})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	if stored[0].Status != OrderStatusPending || stored[0].ReservedAmount <= 0 {
		t.Fatalf("第一笔应挂起并冻结，实际 %s，冻结 %.2f", stored[0].Status, stored[0].ReservedAmount)
	}
	if stored[1].Status != OrderStatusRejected {
		t.Fatalf("第二笔资金不足必须被拒绝，实际 %s", stored[1].Status)
	}
	if !containsText(stored[1].RejectReason, "可用资金不足") {
		t.Fatalf("拒绝原因应说明资金不足，实际 %q", stored[1].RejectReason)
	}

	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash <= 0 {
		t.Fatalf("冻结额应大于 0，实际 %.2f", account.ReservedCash)
	}
	if account.AvailableCash() < 0 {
		t.Fatalf("可用资金不得为负，实际 %.2f", account.AvailableCash())
	}
	if diff := account.Cash - DefaultInitialCash; math.Abs(diff) > 0.01 {
		t.Fatalf("冻结不应改变账面现金，实际 %.2f", account.Cash)
	}
}

// 风控必须以可用资金为口径，否则挂单占用会被重复计算。
func TestBuildOrdersUsesAvailableCash(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	// 账面 10 万，但 9 万已被挂单冻结，只剩 1 万可用。
	snapshot.ReservedCash = 90000
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 20, OrderType: "limit",
		LimitPrice: 10, StopLossPrice: 9.5,
	}}}
	orders, issues := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 {
		t.Fatalf("期望 1 笔订单，实际 %d", len(orders))
	}
	order := orders[0]
	if order.Status == OrderStatusRejected {
		t.Fatalf("应按可用资金缩减而不是拒绝：%s", order.RejectReason)
	}
	// 2 万目标受 1 万可用资金限制，最多 1000 股（含费用后 900 股）。
	if order.Quantity > 1000 {
		t.Fatalf("可用资金 1 万不应买入 %d 股", order.Quantity)
	}
	shrunk := false
	for _, issue := range issues {
		if containsText(issue.Message, "按可用资金缩减") {
			shrunk = true
		}
	}
	if !shrunk {
		t.Fatalf("缩减应有可见提示，实际问题：%+v", issues)
	}
}

func TestApplyFillReleasesReservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	stored, err := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	order := stored[0]
	reservedBefore := order.ReservedAmount
	if reservedBefore <= 0 {
		t.Fatal("下单后应产生冻结")
	}

	filled, err := store.ApplyFill(ctx, order, 1000, 10, TradeFee(SideBuy, 10000))
	if err != nil {
		t.Fatalf("成交失败：%v", err)
	}
	if filled.Status != OrderStatusFilled {
		t.Fatalf("期望全部成交，实际 %s", filled.Status)
	}
	if filled.ReservedAmount != 0 {
		t.Fatalf("全部成交后冻结应清零，实际 %.2f", filled.ReservedAmount)
	}
	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if math.Abs(account.ReservedCash) > 0.01 {
		t.Fatalf("成交后账户冻结应为 0，实际 %.2f", account.ReservedCash)
	}
	if diff := account.Cash - (DefaultInitialCash - 10000 - TradeFee(SideBuy, 10000)); math.Abs(diff) > 0.01 {
		t.Fatalf("现金扣减异常，实际 %.2f", account.Cash)
	}
}

func TestPartialFillKeepsResidualReservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	stored, _ := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	order := stored[0]

	filled, err := store.ApplyFill(ctx, order, 400, 10, TradeFee(SideBuy, 4000))
	if err != nil {
		t.Fatalf("部分成交失败：%v", err)
	}
	if filled.Status != OrderStatusPartial {
		t.Fatalf("期望部分成交，实际 %s", filled.Status)
	}
	expected := expectedReserve(filled, 600)
	if diff := filled.ReservedAmount - expected; math.Abs(diff) > 0.01 {
		t.Fatalf("剩余冻结应为 600 股对应 %.2f，实际 %.2f", expected, filled.ReservedAmount)
	}
	account, _ := store.GetAccount(ctx, DefaultAccountID)
	if diff := account.ReservedCash - expected; math.Abs(diff) > 0.01 {
		t.Fatalf("账户冻结应与订单一致 %.2f，实际 %.2f", expected, account.ReservedCash)
	}
}

func TestReleaseReservationOnCancel(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	stored, _ := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	order := stored[0]

	released, err := store.ReleaseOrderReservation(ctx, order.ID)
	if err != nil {
		t.Fatalf("释放失败：%v", err)
	}
	if math.Abs(released-order.ReservedAmount) > 0.01 {
		t.Fatalf("应释放 %.2f，实际 %.2f", order.ReservedAmount, released)
	}
	account, _ := store.GetAccount(ctx, DefaultAccountID)
	if math.Abs(account.ReservedCash) > 0.01 {
		t.Fatalf("释放后账户冻结应为 0，实际 %.2f", account.ReservedCash)
	}
	if math.Abs(account.AvailableCash()-account.Cash) > 0.01 {
		t.Fatalf("释放后可用资金应等于账面现金")
	}
}

func TestReconcileDetectsAndRepairsDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	stored, _ := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	order := stored[0]

	// 人为制造漂移：账本冻结额比挂单实际需要多 5000。
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_accounts SET reserved_cash=reserved_cash+5000 WHERE id=?`, DefaultAccountID); err != nil {
		t.Fatalf("注入漂移失败：%v", err)
	}
	report, err := store.ReconcileReservations(ctx, DefaultAccountID, false)
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if report.OK {
		t.Fatal("存在漂移时对账不应返回 OK")
	}
	if diff := report.Drift - 5000; math.Abs(diff) > 0.01 {
		t.Fatalf("漂移应为 5000，实际 %.2f", report.Drift)
	}

	repaired, err := store.ReconcileReservations(ctx, DefaultAccountID, true)
	if err != nil {
		t.Fatalf("修复失败：%v", err)
	}
	if !repaired.Repaired || !repaired.OK {
		t.Fatalf("修复后应对账通过，实际 repaired=%t ok=%t issues=%+v", repaired.Repaired, repaired.OK, repaired.Issues)
	}
	account, _ := store.GetAccount(ctx, DefaultAccountID)
	if diff := account.ReservedCash - order.ReservedAmount; math.Abs(diff) > 0.01 {
		t.Fatalf("修复后冻结应等于挂单金额 %.2f，实际 %.2f", order.ReservedAmount, account.ReservedCash)
	}
}

func TestKillSwitchBlocksPlanAndExecute(t *testing.T) {
	content := `{"regime":"进攻","summary":"测试","orders":[{"symbol":"600000.SH","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.2,"stop_loss_price":9.5}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()

	plan, err := service.StartPlan(ctx, PlanRequest{Watchlist: []string{"600000.SH"}})
	if err != nil {
		t.Fatalf("启动计划失败：%v", err)
	}
	ready := waitPlan(t, service, plan.ID)
	if ready.Status != PlanStatusDraft {
		t.Fatalf("期望待确认，实际 %s（%s）", ready.Status, ready.ErrorMessage)
	}

	if _, err := service.SetControl(ctx, Control{AccountID: DefaultAccountID, KillSwitch: true, Reason: "测试熔断"}); err != nil {
		t.Fatalf("设置总闸失败：%v", err)
	}
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if !control.PauseBuying {
		t.Fatal("kill_switch 打开时应强制 pause_buying 为真")
	}

	if _, err := service.Execute(ctx, ready.ID); err == nil {
		t.Fatal("全部停止时执行计划必须失败")
	}
	if _, err := service.StartPlan(ctx, PlanRequest{Watchlist: []string{"600000.SH"}}); err == nil {
		t.Fatal("全部停止时不应再生成新计划")
	}
}

// 暂停买入不能连止损一起停：卖出必须仍然放行。
func TestPauseBuyingKeepsBuysPendingButAllowsSell(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quotes := fakeQuotes{quotes: map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	}}
	service := NewService(store, fakeGateway{}, quotes, nil, nil)

	// 先建立可卖持仓：买入成交后手工放开 T+1 限制。
	stored, err := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	if err != nil {
		t.Fatalf("建仓失败：%v", err)
	}
	if _, err := store.ApplyFill(ctx, stored[0], 1000, 10, TradeFee(SideBuy, 10000)); err != nil {
		t.Fatalf("成交失败：%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_positions SET available_quantity=quantity WHERE account_id=?`, DefaultAccountID); err != nil {
		t.Fatalf("放开 T+1 失败：%v", err)
	}

	now := time.Now().UTC()
	sell := Order{
		ID: newID("ord"), PlanID: newID("plan"), AccountID: DefaultAccountID,
		Symbol: "600000.SH", Side: SideSell, OrderType: OrderTypeLimit,
		Quantity: 1000, LimitPrice: 9.5, Status: OrderStatusPending, CreatedAt: now, UpdatedAt: now,
	}
	buy := testBuyOrder("600000.SH", 100, 10)
	if _, err := store.CreateOrders(ctx, []Order{sell, buy}); err != nil {
		t.Fatalf("落库订单失败：%v", err)
	}
	if _, err := service.SetControl(ctx, Control{AccountID: DefaultAccountID, PauseBuying: true, Reason: "测试"}); err != nil {
		t.Fatalf("设置暂停买入失败：%v", err)
	}

	orders, err := service.MatchPending(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("撮合失败：%v", err)
	}
	var sellStatus, buyStatus OrderStatus
	for _, order := range orders {
		if order.Side == SideSell {
			sellStatus = order.Status
		} else {
			buyStatus = order.Status
		}
	}
	if sellStatus != OrderStatusFilled {
		t.Fatalf("暂停买入时卖出应照常成交，实际 %s", sellStatus)
	}
	if buyStatus != OrderStatusPending {
		t.Fatalf("暂停买入时买入应保持挂起（可恢复），实际 %s", buyStatus)
	}
}

// 在途挂单必须计入单票权重：否则两份计划各买 15% 都能通过，成交后合计 30% 突破上限。
func TestBuildOrdersCountsOpenOrdersInSymbolWeight(t *testing.T) {
	snapshot := testSnapshot([]foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)})
	// 已有 15% 的在途买入挂单（1.5 万 / 10 万总资产）。
	snapshot.OpenBuyValue = map[string]float64{"600000.SH": 15000}
	snapshot.PendingBuyValue = 15000
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10, OrderType: "limit",
		LimitPrice: 10, StopLossPrice: 9.5,
	}}}
	orders, issues := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), time.Now())
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("在途挂单 15%% + 本次 10%% 应超过单票上限并被拒绝，实际 %+v", orders)
	}
	found := false
	for _, issue := range issues {
		if containsText(issue.Message, "在途挂单") {
			found = true
		}
	}
	if !found {
		t.Fatalf("拒绝原因应说明在途挂单占用，实际问题：%+v", issues)
	}
}

func TestPendingBuyValueAggregatesOpenOrders(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.CreateOrders(ctx, []Order{
		testBuyOrder("600000.SH", 600, 10),
		testBuyOrder("000001.SZ", 300, 10),
	}); err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	values, err := store.PendingBuyValue(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("汇总失败：%v", err)
	}
	if diff := values["600000.SH"] - 6000; math.Abs(diff) > 0.01 {
		t.Fatalf("600000.SH 挂单金额应为 6000，实际 %.2f", values["600000.SH"])
	}
	if diff := values["000001.SZ"] - 3000; math.Abs(diff) > 0.01 {
		t.Fatalf("000001.SZ 挂单金额应为 3000，实际 %.2f", values["000001.SZ"])
	}
}

func containsText(value, fragment string) bool {
	return strings.Contains(value, fragment)
}
