package trading

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ExpireResult 是一次日终清理的结果。
type ExpireResult struct {
	AccountID    string    `json:"account_id"`
	Cutoff       time.Time `json:"cutoff"`
	Expired      int       `json:"expired"`
	ReleasedCash float64   `json:"released_cash"`
	ReleasedQty  int       `json:"released_quantity"`
	Orders       []string  `json:"orders,omitempty"`
	Symbols      []string  `json:"symbols,omitempty"`
}

// ExpireStaleOrders 把 cutoff 之前创建、仍未收口的委托作废并释放冻结。
//
// 为什么必须有这一步：A 股的委托当日有效，收盘即废。模拟盘如果不主动作废，
// 未成交的买单会把 reserved_cash 永久锁住——第二天、第三天再用同样的资金去开仓
// 就会被判"可用资金不足"，而这个结论完全错误：钱在账上，只是账本以为它还挂着。
// 卖单同理，冻结的股数会让持仓永远卖不出去。
//
// 只处理 pending / partially_filled：已成交、已撤销、已拒绝的委托不重复释放。
// 状态更新用条件 UPDATE 抢占，保证进程重启补跑或并发触发时不会二次释放冻结。
func (s *Store) ExpireStaleOrders(ctx context.Context, accountID string, cutoff time.Time) (ExpireResult, error) {
	return s.expireStaleOrders(ctx, accountID, cutoff, nil)
}

// expireStaleOrders 是带排除列表的实现。skip 里的订单保持原状态：
// 通道撤单失败的委托不能本地作废，否则本地认为已经结束、通道仍会成交。
func (s *Store) expireStaleOrders(ctx context.Context, accountID string, cutoff time.Time, skip map[string]bool) (ExpireResult, error) {
	result := ExpireResult{AccountID: accountID, Cutoff: cutoff}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id,content_json FROM trading_orders
		WHERE account_id=? AND status IN (?,?)`,
		accountID, string(OrderStatusPending), string(OrderStatusPartial))
	if err != nil {
		return result, err
	}
	candidates := []Order{}
	for rows.Next() {
		var (
			id      string
			payload string
		)
		if err := rows.Scan(&id, &payload); err != nil {
			_ = rows.Close()
			return result, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		order.ID = id
		candidates = append(candidates, order)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	_ = rows.Close()

	now := time.Now().UTC()
	for _, order := range candidates {
		if !order.CreatedAt.Before(cutoff) {
			continue
		}
		if skip[order.ID] {
			continue
		}
		// 抢占：只有仍处于挂起态的订单才会被改，避免重复释放。
		updated, err := tx.ExecContext(ctx, `UPDATE trading_orders SET status=?,updated_at=?
			WHERE id=? AND status IN (?,?)`,
			string(OrderStatusExpired), nowText(now), order.ID,
			string(OrderStatusPending), string(OrderStatusPartial))
		if err != nil {
			return result, err
		}
		if affected, _ := updated.RowsAffected(); affected == 0 {
			continue
		}
		releasedCash := order.ReservedAmount
		if releasedCash > 0 {
			if err := adjustReservedTx(ctx, tx, order.AccountID, -releasedCash, now); err != nil {
				return result, err
			}
			result.ReleasedCash = round2(result.ReleasedCash + releasedCash)
		}
		if order.ReservedQuantity > 0 {
			if err := s.adjustReservedQuantityTx(ctx, tx, order.AccountID, order.Symbol, -order.ReservedQuantity, now); err != nil {
				return result, err
			}
			result.ReleasedQty += order.ReservedQuantity
			result.Symbols = appendUnique(result.Symbols, order.Symbol)
		}
		order.Status = OrderStatusExpired
		order.ReservedAmount = 0
		order.ReservedQuantity = 0
		order.FrozenAt = nil
		order.UpdatedAt = now
		if order.RejectReason == "" {
			order.RejectReason = "当日委托未成交，收盘后自动失效"
		}
		payload, err := json.Marshal(order)
		if err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE trading_orders SET content_json=? WHERE id=?`, string(payload), order.ID); err != nil {
			return result, err
		}
		result.Expired++
		result.Orders = append(result.Orders, order.ID)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// ExpireStaleOrders 按最近收盘时点清理过期委托。
// 幂等：同一批委托只会被作废一次，重复调用返回 0 笔。
//
// 作废之前先向通道撤单：本地作废而通道仍挂着的委托，第二天会在通道成交，
// 而账本早已把这笔当成失效——那笔成交就变成了没有冻结支撑的凭空持仓。
// 撤单失败的委托原地保留，宁可多冻一天，也不能记出一笔假的"已失效"。
func (s *Service) ExpireStaleOrders(ctx context.Context, accountID string, now time.Time) (ExpireResult, error) {
	accountID = s.accountID(accountID)
	cutoff := LastSessionClose(now)
	candidates, err := s.store.ListOpenOrdersBefore(ctx, accountID, cutoff)
	if err != nil {
		return ExpireResult{AccountID: accountID, Cutoff: cutoff}, err
	}
	skip := map[string]bool{}
	for _, order := range candidates {
		if cancelErr := s.cancelWithRetry(ctx, order); cancelErr != nil {
			skip[order.ID] = true
		}
	}
	result, err := s.store.expireStaleOrders(ctx, accountID, cutoff, skip)
	if err != nil {
		return result, err
	}
	if result.Expired > 0 {
		s.logf("event=trading_orders_expired count=%d released_cash=%.2f released_qty=%d cutoff=%s",
			result.Expired, result.ReleasedCash, result.ReleasedQty, cutoff.Format(time.RFC3339))
		_ = s.store.AppendEvent(ctx, Event{
			AccountID: accountID,
			Severity:  EventSeverityInfo,
			Kind:      EventOrderExpired,
			Message: fmt.Sprintf("日终作废 %d 笔未成交委托，释放资金 %.2f 元、股数 %d 股",
				result.Expired, result.ReleasedCash, result.ReleasedQty),
			Payload: eventPayload(result),
		})
	}
	if len(skip) > 0 {
		s.logf("event=trading_orders_expire_skipped count=%d", len(skip))
	}
	return result, nil
}

func appendUnique(list []string, value string) []string {
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}

// ListOpenOrdersBefore 返回 cutoff 之前创建、仍挂着的委托。
// 日终清理要先拿到这份清单，才能逐笔去通道撤单——撤不掉的不能被本地作废。
func (s *Store) ListOpenOrdersBefore(ctx context.Context, accountID string, cutoff time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,content_json FROM trading_orders
		WHERE account_id=? AND status IN (?,?)`,
		accountID, string(OrderStatusPending), string(OrderStatusPartial))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var (
			id      string
			payload string
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		order.ID = id
		if order.CreatedAt.Before(cutoff) {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}
