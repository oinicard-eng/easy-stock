package trading

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"
)

// fakeChannel 是一个可控通道，用来制造真实券商才会出现的异常：
// 撤单被拒、撤单超时、回报超量。这些分支不靠"想象"覆盖，只能靠假通道逼出来。
type fakeChannel struct {
	submitResult BrokerResult
	cancelResult CancelResult
	cancelErr    error
	cancelCalls  int
}

func (f *fakeChannel) Name() string { return "fake" }

func (f *fakeChannel) Submit(context.Context, BrokerOrder) (BrokerResult, error) {
	return f.submitResult, nil
}

func (f *fakeChannel) Cancel(context.Context, CancelRequest) (CancelResult, error) {
	f.cancelCalls++
	return f.cancelResult, f.cancelErr
}

func newReportTestService(t *testing.T, store *Store, channel *fakeChannel) *Service {
	t.Helper()
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))
	if channel != nil {
		service.broker = channel
	}
	return service
}

func pendingBuy(t *testing.T, store *Store, symbol string, quantity int, limit float64) Order {
	t.Helper()
	stored, err := store.CreateOrders(context.Background(), []Order{testBuyOrder(symbol, quantity, limit)})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("期望落库 1 笔，实际 %d", len(stored))
	}
	return stored[0]
}

// 通道重投是最常见的真实行为：同一条回报必须只记一次账。
// 少了幂等，一次断线重连就会让持仓翻倍。
func TestApplyReportIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := newReportTestService(t, store, nil)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)

	report := BrokerReport{
		ReportID: "rpt-1", OrderID: order.ID, Status: OrderStatusFilled,
		FilledQuantity: 1000, Price: 10, Fee: 5, At: time.Now(),
	}
	if _, err := service.ApplyReport(ctx, report); err != nil {
		t.Fatalf("首次回报失败：%v", err)
	}
	if _, err := service.ApplyReport(ctx, report); err != nil {
		t.Fatalf("重复回报不应报错：%v", err)
	}

	positions, err := store.ListPositions(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	total := 0
	for _, position := range positions {
		total += position.Quantity
	}
	if total != 1000 {
		t.Fatalf("重复回报导致持仓翻倍：期望 1000 股，实际 %d 股", total)
	}

	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash != 0 {
		t.Fatalf("全额成交后冻结应归零，实际 %.2f", account.ReservedCash)
	}
}

// 部分成交是实盘常态：两条 400 股回报之后应剩 200 股仍在挂单。
func TestApplyReportAccumulatesPartialFills(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := newReportTestService(t, store, nil)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)

	for index, reportID := range []string{"rpt-a", "rpt-b"} {
		if _, err := service.ApplyReport(ctx, BrokerReport{
			ReportID: reportID, OrderID: order.ID, Status: OrderStatusPartial,
			FilledQuantity: 400, Price: 10, Fee: 2, At: time.Now().Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatalf("部分成交回报失败：%v", err)
		}
	}

	updated, err := store.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if updated.FilledQuantity != 800 {
		t.Fatalf("期望累计成交 800 股，实际 %d", updated.FilledQuantity)
	}
	if updated.Status != OrderStatusPartial {
		t.Fatalf("期望保持部分成交，实际 %s", updated.Status)
	}
	if updated.ReservedQuantity != 0 {
		t.Fatalf("买单不应冻结股数，实际 %d", updated.ReservedQuantity)
	}
	if updated.ReservedAmount <= 0 {
		t.Fatal("未成交部分应继续冻结资金")
	}
}

// 通道多报成交时只能截断：宁可少记，也不能记出一笔凭空多出来的持仓。
func TestApplyReportTruncatesOverfill(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := newReportTestService(t, store, nil)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)

	updated, err := service.ApplyReport(ctx, BrokerReport{
		ReportID: "rpt-over", OrderID: order.ID, Status: OrderStatusFilled,
		FilledQuantity: 5000, Price: 10, Fee: 5, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("超量回报不应报错，应截断：%v", err)
	}
	if updated.FilledQuantity != 1000 {
		t.Fatalf("超量回报应截断到 1000 股，实际 %d", updated.FilledQuantity)
	}
	events, err := store.ListEvents(ctx, DefaultAccountID, EventSeverityCritical, EventBrokerError, 10)
	if err != nil {
		t.Fatalf("读取事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("超量回报必须留下 1 条 critical 事件，实际 %d", len(events))
	}
}

// 已收口的订单再收到成交回报，说明两端有一端是错的：不改账，只报警。
func TestApplyReportRejectsFillOnClosedOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := newReportTestService(t, store, nil)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)

	if _, _, err := store.CloseOrder(ctx, order.ID, OrderStatusCancelled, "人工撤销"); err != nil {
		t.Fatalf("收口失败：%v", err)
	}

	if _, err := service.ApplyReport(ctx, BrokerReport{
		ReportID: "rpt-late", OrderID: order.ID, Status: OrderStatusFilled,
		FilledQuantity: 1000, Price: 10, Fee: 5, At: time.Now(),
	}); err == nil {
		t.Fatal("已收口订单收到成交回报必须报错")
	}

	positions, err := store.ListPositions(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取持仓失败：%v", err)
	}
	if len(positions) != 0 {
		t.Fatalf("矛盾回报不应改动账本，实际产生 %d 个持仓", len(positions))
	}
	events, err := store.ListEvents(ctx, DefaultAccountID, EventSeverityCritical, EventBrokerError, 10)
	if err != nil {
		t.Fatalf("读取事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("矛盾回报必须留下 1 条 critical 事件，实际 %d", len(events))
	}
}

// 通道拒绝撤单时，本地订单必须保持挂起：假装撤掉会让冻结凭空释放。
func TestCancelOrderKeepsPendingWhenChannelRejects(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	channel := &fakeChannel{cancelResult: CancelResult{Accepted: false, Reason: "已成交，无法撤单"}}
	service := newReportTestService(t, store, channel)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)
	reservedBefore := order.ReservedAmount

	if _, err := service.CancelOrder(ctx, order.ID); err == nil {
		t.Fatal("通道拒绝撤单时本地不应报成功")
	}
	current, err := store.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if current.Status != OrderStatusPending {
		t.Fatalf("撤单失败后订单应保持挂起，实际 %s", current.Status)
	}
	if current.ReservedAmount != reservedBefore {
		t.Fatalf("撤单失败后冻结不应变化：期望 %.2f，实际 %.2f", reservedBefore, current.ReservedAmount)
	}
	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash != reservedBefore {
		t.Fatalf("账户冻结应保持 %.2f，实际 %.2f", reservedBefore, account.ReservedCash)
	}
}

// 撤单成功后本地收口并释放冻结。
func TestCancelOrderReleasesReservationWhenChannelAccepts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	channel := &fakeChannel{cancelResult: CancelResult{Accepted: true}}
	service := newReportTestService(t, store, channel)
	order := pendingBuy(t, store, "600000.SH", 1000, 10)

	cancelled, err := service.CancelOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("撤单失败：%v", err)
	}
	if cancelled.Status != OrderStatusCancelled {
		t.Fatalf("期望已撤销，实际 %s", cancelled.Status)
	}
	if cancelled.ReservedAmount != 0 {
		t.Fatalf("撤销后订单冻结应归零，实际 %.2f", cancelled.ReservedAmount)
	}
	account, err := store.GetAccount(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取账户失败：%v", err)
	}
	if account.ReservedCash != 0 {
		t.Fatalf("撤销后账户冻结应归零，实际 %.2f", account.ReservedCash)
	}
}

// 日终作废时，撤单失败的委托不能被本地作废——否则第二天通道成交就变成超卖。
func TestExpireSkipsOrdersThatChannelCannotCancel(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	channel := &fakeChannel{cancelErr: errors.New("通道超时")}
	service := newReportTestService(t, store, channel)

	order := testBuyOrder("600000.SH", 1000, 10)
	order.CreatedAt = time.Now().UTC().AddDate(0, 0, -3)
	stored, err := store.CreateOrders(ctx, []Order{order})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	reserved := stored[0].ReservedAmount

	result, err := service.ExpireStaleOrders(ctx, DefaultAccountID, time.Now())
	if err != nil {
		t.Fatalf("日终清理失败：%v", err)
	}
	if result.Expired != 0 {
		t.Fatalf("撤单失败时不应作废任何委托，实际 %d 笔", result.Expired)
	}
	current, err := store.GetOrder(ctx, stored[0].ID)
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if current.Status != OrderStatusPending {
		t.Fatalf("撤单失败时订单应保持挂起，实际 %s", current.Status)
	}
	if current.ReservedAmount != reserved {
		t.Fatalf("冻结应保持不变：期望 %.2f，实际 %.2f", reserved, current.ReservedAmount)
	}
	if channel.cancelCalls < 3 {
		t.Fatalf("撤单应重试 3 次，实际调用 %d 次", channel.cancelCalls)
	}
}
