package trading

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CancelRequest 是撤单请求。ChannelOrderID 为空时通道可以按本地单号反查，
// 但真实券商几乎都要求通道侧编号，所以这里显式带上。
type CancelRequest struct {
	AccountID      string
	OrderID        string
	ChannelOrderID string
	Symbol         string
	Side           Side
	Quantity       int
	FilledQuantity int
}

// CancelResult 是撤单结果。Accepted=false 不代表失败：
// 券商可能回"已成交无法撤单"，此时必须把订单按成交收口，而不是当作撤销成功。
type CancelResult struct {
	Accepted    bool
	Reason      string
	FilledQty   int
	FilledPrice float64
}

// CancelableChannel 是支持撤单的通道。
//
// 单独定义而不是塞进 Broker：模拟盘不存在"撤单失败"这一状态，
// 强制它实现一个恒真的 Cancel 会让代码读起来像在假装有对手方。
type CancelableChannel interface {
	Cancel(ctx context.Context, request CancelRequest) (CancelResult, error)
}

// BrokerReport 是一条委托回报，由通道推送或轮询得到。
//
// FilledQuantity 是本次回报的**增量**成交数量，不是累计值。
// 真实券商的回报有两种口径，这里只接受增量：累计口径下重复投递一条回报
// 会让持仓凭空多出一倍，而增量口径下重复投递只会被幂等表挡掉。
type BrokerReport struct {
	ReportID       string
	OrderID        string
	ChannelOrderID string
	Status         OrderStatus
	FilledQuantity int
	Price          float64
	Fee            float64
	Reason         string
	At             time.Time
}

// ErrReportUnknownOrder 表示回报指向一笔本地不存在的委托。
// 这不是可以忽略的情况：要么通道串号，要么本地账本丢了订单，两者都要人介入。
var ErrReportUnknownOrder = errors.New("回报对应的委托不存在")

const reportsSchema = `
	CREATE TABLE IF NOT EXISTS trading_broker_reports (
		report_id TEXT PRIMARY KEY,
		order_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'applying',
		content_json TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_broker_reports_order ON trading_broker_reports(order_id);
`

func (s *Store) migrateReports(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, reportsSchema)
	return err
}

// reportClaim 描述一次回报抢占的结果。
type reportClaim int

const (
	// reportClaimFresh 表示这条回报此前从未见过，可以处理。
	reportClaimFresh reportClaim = iota
	// reportClaimDuplicate 表示已经处理过，直接丢弃——重复回报是最常见的通道行为。
	reportClaimDuplicate
)

// claimReport 用一条 UPSERT 抢占回报。
//
// 为什么需要 status 列而不是简单 INSERT OR IGNORE：
// 抢到之后、落账之前进程可能崩掉，那条回报会永远停在"处理中"，
// 重试被当成重复而丢弃就真的丢单了。所以"处理中"超过 staleAfter
// 允许被重新接管，而"已完成"才是永久幂等。
func (s *Store) claimReport(ctx context.Context, report BrokerReport, staleAfter time.Duration) (reportClaim, error) {
	now := time.Now().UTC()
	at := report.At
	if at.IsZero() {
		at = now
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return reportClaimDuplicate, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO trading_broker_reports
		(report_id,order_id,status,content_json,created_at,updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(report_id) DO UPDATE SET status='applying', updated_at=excluded.updated_at
		WHERE trading_broker_reports.status='applying' AND trading_broker_reports.updated_at < ?`,
		report.ReportID, report.OrderID, "applying", string(payload), nowText(now), nowText(at),
		nowText(now.Add(-staleAfter)))
	if err != nil {
		return reportClaimDuplicate, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return reportClaimDuplicate, err
	}
	if affected == 0 {
		return reportClaimDuplicate, nil
	}
	return reportClaimFresh, nil
}

// markReportApplied 把回报标记为已落账。此后同 id 的回报永久幂等。
func (s *Store) markReportApplied(ctx context.Context, reportID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE trading_broker_reports SET status='applied', updated_at=? WHERE report_id=?`,
		nowText(time.Now().UTC()), reportID)
	return err
}

// releaseReportClaim 在落账失败时把回报退回可重试状态。
// 留着'处理中'会让这条回报在 staleAfter 之前无法重试，等于人为制造一段丢单窗口。
func (s *Store) releaseReportClaim(ctx context.Context, reportID string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM trading_broker_reports WHERE report_id=? AND status='applying'`, reportID)
}

// CloseOrder 把订单收口为非成交终态（撤销/拒绝/过期）并释放全部冻结。
//
// 条件更新保证只有仍挂着的订单会被改：如果期间已经成交，这次收口会改到 0 行，
// 调用方据此知道"这笔已经不该再撤了"。
func (s *Store) CloseOrder(ctx context.Context, orderID string, status OrderStatus, reason string) (Order, bool, error) {
	order, err := s.GetOrder(ctx, orderID)
	if err != nil {
		return Order{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE trading_orders SET status=?,updated_at=? WHERE id=? AND status IN (?,?)`,
		string(status), nowText(now), orderID, string(OrderStatusPending), string(OrderStatusPartial))
	if err != nil {
		return Order{}, false, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return order, false, nil
	}
	// 释放冻结与收口必须在同一事务：分开做时若进程在中间退出，
	// 就会留下一笔"已撤销但仍占着钱"的订单，只能靠对账去猜。
	if order.ReservedAmount > 0 {
		if err := adjustReservedTx(ctx, tx, order.AccountID, -order.ReservedAmount, now); err != nil {
			return Order{}, false, err
		}
	}
	if order.ReservedQuantity > 0 {
		if err := s.adjustReservedQuantityTx(ctx, tx, order.AccountID, order.Symbol, -order.ReservedQuantity, now); err != nil {
			return Order{}, false, err
		}
	}
	order.Status = status
	if reason != "" {
		order.RejectReason = reason
	}
	order.ReservedAmount = 0
	order.ReservedQuantity = 0
	order.FrozenAt = nil
	order.UpdatedAt = now
	payload, err := json.Marshal(order)
	if err != nil {
		return Order{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trading_orders SET content_json=? WHERE id=?`, string(payload), orderID); err != nil {
		return Order{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Order{}, false, err
	}
	return order, true, nil
}

// ApplyReport 把一条通道回报落进账本。
//
// 三条原则：
//  1. 幂等优先：同一条 ReportID 重复投递只记一次账；
//  2. 成交数量以"本地剩余"为上限：通道多报的股数一律截断并告警，
//     宁可少记也不能让持仓凭空变大；
//  3. 回报与本地状态冲突时不猜：已收口的订单再收到成交，只写事件不改账，
//     因为此时任何一端都可能是错的，唯一安全的动作是让人来看。
func (s *Service) ApplyReport(ctx context.Context, report BrokerReport) (Order, error) {
	if report.ReportID == "" {
		return Order{}, errors.New("回报缺少 ReportID，无法保证幂等")
	}
	if report.OrderID == "" {
		return Order{}, errors.New("回报缺少本地订单号")
	}
	claim, err := s.store.claimReport(ctx, report, 2*time.Minute)
	if err != nil {
		return Order{}, err
	}
	if claim == reportClaimDuplicate {
		// 重复回报不是错误：通道重投是常态，返回当前订单即可。
		return s.store.GetOrder(ctx, report.OrderID)
	}

	order, err := s.store.GetOrder(ctx, report.OrderID)
	if err != nil {
		s.store.releaseReportClaim(ctx, report.ReportID)
		return Order{}, fmt.Errorf("%w：%s", ErrReportUnknownOrder, report.OrderID)
	}
	if report.ChannelOrderID != "" && order.ChannelOrderID != report.ChannelOrderID {
		order.ChannelOrderID = report.ChannelOrderID
	}

	filled := report.FilledQuantity
	if filled > 0 {
		if !order.IsOpen() {
			// 已收口订单收到成交回报：账本说卖完了，通道说又成交了。
			// 不改本地账，只把矛盾暴露出来。
			s.store.releaseReportClaim(ctx, report.ReportID)
			_ = s.store.AppendEvent(ctx, Event{
				AccountID: order.AccountID,
				Severity:  EventSeverityCritical,
				Kind:      EventBrokerError,
				Symbol:    order.Symbol,
				Message:   fmt.Sprintf("委托 %s 已处于 %s，却又收到 %d 股成交回报，账本未改动，需人工核对", order.ID, order.Status, filled),
				Payload:   eventPayload(report),
			})
			return order, fmt.Errorf("订单 %s 已收口（%s），拒绝应用成交回报", order.ID, order.Status)
		}
		remaining := order.Quantity - order.FilledQuantity
		if filled > remaining {
			_ = s.store.AppendEvent(ctx, Event{
				AccountID: order.AccountID,
				Severity:  EventSeverityCritical,
				Kind:      EventBrokerError,
				Symbol:    order.Symbol,
				Message:   fmt.Sprintf("回报成交 %d 股超过本地剩余 %d 股，已按剩余截断", filled, remaining),
				Payload:   eventPayload(report),
			})
			filled = remaining
		}
		if filled > 0 {
			updated, fillErr := s.store.ApplyFill(ctx, order, filled, report.Price, report.Fee)
			if fillErr != nil {
				s.store.releaseReportClaim(ctx, report.ReportID)
				return Order{}, fmt.Errorf("落账成交失败：%w", fillErr)
			}
			order = updated
		}
	}

	// 成交之外还要处理"订单级终态"：撤单成功、废单、部分成交后撤单。
	// 只在订单仍挂着时才收口，避免把刚成交的单覆盖成已撤销。
	if order.IsOpen() {
		switch report.Status {
		case OrderStatusRejected, OrderStatusCancelled, OrderStatusExpired:
			closed, changed, closeErr := s.store.CloseOrder(ctx, order.ID, report.Status, report.Reason)
			if closeErr != nil {
				s.store.releaseReportClaim(ctx, report.ReportID)
				return Order{}, closeErr
			}
			if changed {
				order = closed
			}
		}
	}
	if err := s.store.markReportApplied(ctx, report.ReportID); err != nil {
		s.logf("event=trading_report_mark_failed report=%s err=%v", report.ReportID, err)
	}
	s.logf("event=trading_report_applied order=%s report=%s status=%s filled=%d",
		order.ID, report.ReportID, report.Status, report.FilledQuantity)
	return order, nil
}

// cancelWithRetry 向通道撤单，失败按指数退避重试。
//
// 为什么必须重试：撤单失败时本地订单仍是挂起态，资金和股数继续被冻结。
// 更糟的是日终场景——本地作废了、通道还挂着，第二天通道成交就会变成超卖。
// 所以撤单不成功就不能改本地状态，宁可报错也不能假装撤掉了。
func (s *Service) cancelWithRetry(ctx context.Context, order Order) error {
	channel, ok := s.broker.(CancelableChannel)
	if !ok {
		// 模拟盘没有独立对手方，本地撤销即全部撤销。
		return nil
	}
	var lastErr error
	delay := 200 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		result, err := channel.Cancel(ctx, CancelRequest{
			AccountID:      order.AccountID,
			OrderID:        order.ID,
			ChannelOrderID: order.ChannelOrderID,
			Symbol:         order.Symbol,
			Side:           order.Side,
			Quantity:       order.Quantity,
			FilledQuantity: order.FilledQuantity,
		})
		if err == nil && result.Accepted {
			return nil
		}
		if err == nil {
			// 通道明确拒绝撤单：通常意味着已经成交，重试没有意义。
			lastErr = fmt.Errorf("通道拒绝撤单：%s", fallback(result.Reason, "未说明原因"))
			break
		}
		lastErr = err
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
	}
	_ = s.store.AppendEvent(ctx, Event{
		AccountID: order.AccountID,
		Severity:  EventSeverityWarning,
		Kind:      EventBrokerError,
		Symbol:    order.Symbol,
		Message:   fmt.Sprintf("委托 %s 撤单失败：%v（本地订单保持挂起，冻结未释放）", order.ID, lastErr),
		Payload:   eventPayload(map[string]any{"order_id": order.ID, "error": lastErr.Error()}),
	})
	return lastErr
}
