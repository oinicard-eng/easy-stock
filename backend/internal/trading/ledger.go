package trading

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是账本的「资金冻结」层，P2 起所有风控都以它给出的可用资金为准。
//
// 为什么需要冻结：挂单在成交前不扣现金，若风控按账面现金计算仓位，
// 两个计划可以各自把同一笔钱规划一遍。交易日结束时两笔同时触发，
// 第二笔必然失败——这不是策略问题，是账本不认识「这笔钱已经许给别人了」。
//
// 三条不变量（任何改动都必须保持）：
//  1. account.reserved_cash == 所有未成交买入单的 ReservedAmount 之和；
//  2. account.cash - account.reserved_cash >= 0；
//  3. 释放额按「剩余股数重算」得出，不做比例推算，避免四舍五入漂移。
var ErrInsufficientCash = errors.New("可用资金不足")

// ErrInsufficientPosition 表示可卖股数不足（T+1 未解禁，或已被其他卖单占用）。
var ErrInsufficientPosition = errors.New("可卖股数不足")

// expectedReserve 是某笔买入订单在剩余 remaining 股时应当冻结的金额。
// 它是冻结、释放、对账三处共用的唯一算法，避免三个地方各算一套。
func expectedReserve(order Order, remaining int) float64 {
	if remaining <= 0 {
		return 0
	}
	price := order.LimitPrice
	if price <= 0 {
		return 0
	}
	notional := float64(remaining) * price
	return round2(notional + TradeFee(SideBuy, notional))
}

// reserveAmountForOrder 在订单落库前计算初始冻结额。
// 没有有效价格就不允许冻结——冻结不了一分钱的买单，也就无从校验风险。
func reserveAmountForOrder(order Order) (float64, error) {
	if order.Quantity <= 0 {
		return 0, errors.New("订单数量必须为正")
	}
	if order.LimitPrice <= 0 {
		return 0, errors.New("缺少有效限价，无法冻结资金")
	}
	amount := expectedReserve(order, order.Quantity-order.FilledQuantity)
	if amount <= 0 {
		return 0, errors.New("冻结金额计算结果无效")
	}
	return amount, nil
}

// reserveTx 在事务内冻结资金。条件更新保证并发下不会把余额冻成负数。
func (s *Store) reserveTx(ctx context.Context, tx *sql.Tx, accountID string, order *Order, amount float64) error {
	result, err := tx.ExecContext(ctx, `UPDATE trading_accounts
		SET reserved_cash=reserved_cash+?, updated_at=?
		WHERE id=? AND cash-reserved_cash>=?`,
		amount, nowText(time.Now().UTC()), accountID, amount)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		account, accountErr := s.getAccountTx(ctx, tx, accountID)
		if accountErr != nil {
			return accountErr
		}
		return fmt.Errorf("%w：需要冻结 %.2f 元，可用 %.2f 元（另有 %.2f 元已被挂单占用）",
			ErrInsufficientCash, amount, account.AvailableCash(), account.ReservedCash)
	}
	// 初始冻结额必须覆盖而不是累加：调用方只可能在订单落库时调用一次，
	// 累加会让"计划阶段已估算过冻结额"的订单被记成双倍，撤销时释放不掉。
	order.ReservedAmount = round2(amount)
	frozenAt := time.Now().UTC()
	order.FrozenAt = &frozenAt
	return nil
}

// settleReservationTx 在成交事务内调整冻结额：按剩余未成交股数重算目标冻结，
// 与当前冻结额的差额退回可用资金。
func (s *Store) settleReservationTx(ctx context.Context, tx *sql.Tx, order *Order, filledQuantity int, now time.Time) error {
	if order.Side != SideBuy || order.ReservedAmount <= 0 {
		return nil
	}
	remaining := order.Quantity - order.FilledQuantity - filledQuantity
	target := expectedReserve(*order, remaining)
	delta := round2(order.ReservedAmount - target)
	order.ReservedAmount = target
	if target <= 0 {
		order.FrozenAt = nil
	}
	if delta == 0 {
		return nil
	}
	return adjustReservedTx(ctx, tx, order.AccountID, -delta, now)
}

// adjustReservedTx 把冻结额增减 delta（正数增加，负数释放），并守住下界 0。
//
// 每次变更都四舍五入到分：钱只有两位小数，逐笔累加浮点残差会在几百次挂单/撤销后
// 留下 1e-12 量级的"存在但不该存在"的冻结额，对账时又要靠容差掩盖它。
func adjustReservedTx(ctx context.Context, tx *sql.Tx, accountID string, delta float64, now time.Time) error {
	delta = round2(delta)
	if delta == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE trading_accounts
		SET reserved_cash = CASE WHEN ROUND(reserved_cash+?,2) < 0 THEN 0 ELSE ROUND(reserved_cash+?,2) END,
			updated_at=?
		WHERE id=?`, delta, delta, nowText(now), accountID)
	return err
}

// ReleaseOrderReservation 释放某笔订单的全部剩余冻结，用于撤销与拒单。
// 返回实际释放的金额，0 表示该订单本来就没有冻结。
//
// 资金与股数一起释放：卖单只有股数冻结、买单只有资金冻结，
// 拆成两个入口会让调用方迟早漏掉一个，而漏掉的那次就是"撤销了卖单但股数还锁着"。
func (s *Store) ReleaseOrderReservation(ctx context.Context, orderID string) (float64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	order, err := getOrderTx(ctx, tx, orderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	released := order.ReservedAmount
	now := time.Now().UTC()
	if released > 0 {
		if err := adjustReservedTx(ctx, tx, order.AccountID, -released, now); err != nil {
			return 0, err
		}
	}
	if order.ReservedQuantity > 0 {
		if err := s.adjustReservedQuantityTx(ctx, tx, order.AccountID, order.Symbol, -order.ReservedQuantity, now); err != nil {
			return 0, err
		}
	}
	order.ReservedAmount = 0
	order.ReservedQuantity = 0
	order.FrozenAt = nil
	order.UpdatedAt = now
	payload, err := json.Marshal(order)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trading_orders SET status=?,content_json=?,updated_at=? WHERE id=?`,
		string(order.Status), string(payload), nowText(order.UpdatedAt), order.ID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return round2(released), nil
}

// reserveSellQuantityTx 在事务内冻结可卖股数。
// 条件更新（available_quantity-reserved_quantity>=qty）保证两笔卖单不会各卖一次。
func (s *Store) reserveSellQuantityTx(ctx context.Context, tx *sql.Tx, accountID, symbol string, quantity int, order *Order) error {
	if quantity <= 0 {
		return errors.New("卖出数量必须为正")
	}
	result, err := tx.ExecContext(ctx, `UPDATE trading_positions
		SET reserved_quantity=reserved_quantity+?, updated_at=?
		WHERE account_id=? AND symbol=? AND available_quantity-reserved_quantity>=?`,
		quantity, nowText(time.Now().UTC()), accountID, symbol, quantity)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		position, positionErr := s.getPositionTx(ctx, tx, accountID, symbol)
		if positionErr != nil {
			return fmt.Errorf("冻结可卖股数失败：%w", positionErr)
		}
		return fmt.Errorf("%w：需要 %d 股，可卖 %d 股（已挂卖单 %d 股）",
			ErrInsufficientPosition, quantity, position.Sellable(), position.ReservedQuantity)
	}
	order.ReservedQuantity = quantity
	frozenAt := time.Now().UTC()
	order.FrozenAt = &frozenAt
	return nil
}

// adjustReservedQuantityTx 增减某标的的冻结股数，并守住下界 0。
func (s *Store) adjustReservedQuantityTx(ctx context.Context, tx *sql.Tx, accountID, symbol string, delta int, now time.Time) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE trading_positions
		SET reserved_quantity = CASE WHEN reserved_quantity+? < 0 THEN 0 ELSE reserved_quantity+? END,
			updated_at=?
		WHERE account_id=? AND symbol=?`, delta, delta, nowText(now), accountID, symbol)
	return err
}

// settleSellReservationTx 在成交时释放已成交部分的股数冻结。
// 剩余未成交部分继续冻结，保证未成交的卖单仍然占着它的股数。
func (s *Store) settleSellReservationTx(ctx context.Context, tx *sql.Tx, order *Order, filledQuantity int, now time.Time) error {
	if order.Side != SideSell || order.ReservedQuantity <= 0 {
		return nil
	}
	remaining := order.Quantity - order.FilledQuantity - filledQuantity
	if remaining < 0 {
		remaining = 0
	}
	release := order.ReservedQuantity - remaining
	if release > 0 {
		if err := s.adjustReservedQuantityTx(ctx, tx, order.AccountID, order.Symbol, -release, now); err != nil {
			return err
		}
	}
	order.ReservedQuantity = remaining
	if remaining == 0 {
		order.FrozenAt = nil
	}
	return nil
}

// ReconcileReservations 按挂单重算应收冻结额，与账本记账值比对。
//
// repair=true 时按重算结果回写（订单级与账户级同时修正）。默认只报告不改，
// 因为自动改账在排查阶段会掩盖问题。
func (s *Store) ReconcileReservations(ctx context.Context, accountID string, repair bool) (ReconcileReport, error) {
	account, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return ReconcileReport{}, err
	}
	positions, err := s.ListPositions(ctx, accountID)
	if err != nil {
		return ReconcileReport{}, err
	}
	orders, err := s.listOrdersForReconcile(ctx, accountID)
	if err != nil {
		return ReconcileReport{}, err
	}

	report := ReconcileReport{
		AccountID:    accountID,
		TakenAt:      time.Now().UTC(),
		Cash:         account.Cash,
		ReservedCash: account.ReservedCash,
		MarketValue:  round2(account.MarketValue),
	}
	marketValue := 0.0
	for _, position := range positions {
		marketValue += position.MarketValue
	}
	report.MarketValue = round2(marketValue)
	report.Equity = round2(account.Cash + marketValue)

	expected := 0.0
	expectedSellQty := map[string]int{}
	for _, order := range orders {
		if order.Side == SideBuy && order.IsOpen() {
			remaining := order.Quantity - order.FilledQuantity
			expected += expectedReserve(order, remaining)
			report.OpenBuyOrders++
			continue
		}
		if order.Side == SideSell && order.IsOpen() {
			remaining := order.Quantity - order.FilledQuantity
			if remaining > 0 {
				expectedSellQty[order.Symbol] += remaining
			}
		}
		// 已收口（成交/撤销/拒绝）却还挂着冻结，属于泄漏，必须报告。
		if order.ReservedAmount > 0 {
			report.OrphanOrders = append(report.OrphanOrders, order.ID)
		}
	}
	report.ExpectedReserved = round2(expected)
	report.Drift = round2(account.ReservedCash - report.ExpectedReserved)

	// 股数冻结逐标的比对。它和资金是两条独立的不变量：
	// 资金对了不代表股数对了（卖单不动钱），只对资金对账会漏掉超卖。
	for _, position := range positions {
		report.ReservedQuantity += position.ReservedQuantity
	}
	for _, position := range positions {
		want := expectedSellQty[position.Symbol]
		if want == position.ReservedQuantity {
			continue
		}
		if report.ReservedQuantityDrift == nil {
			report.ReservedQuantityDrift = map[string]string{}
		}
		report.ReservedQuantityDrift[position.Symbol] = fmt.Sprintf("账本 %d 股，按挂单重算 %d 股", position.ReservedQuantity, want)
		report.Issues = append(report.Issues, Issue{Level: IssueError, Field: "reserved_quantity",
			Message: fmt.Sprintf("%s 冻结股数与挂单不一致：账本 %d 股，按挂单重算 %d 股",
				position.Symbol, position.ReservedQuantity, want)})
	}
	// 有挂卖单但持仓已不存在，属于孤儿卖单：成交时必然失败，必须暴露出来。
	for symbol, want := range expectedSellQty {
		found := false
		for _, position := range positions {
			if position.Symbol == symbol {
				found = true
				break
			}
		}
		if !found && want > 0 {
			report.Issues = append(report.Issues, Issue{Level: IssueError, Field: "reserved_quantity",
				Message: fmt.Sprintf("%s 有 %d 股挂卖单但已无持仓", symbol, want)})
		}
	}

	if account.Cash-account.ReservedCash < -0.01 {
		report.Issues = append(report.Issues, Issue{Level: IssueError, Field: "cash",
			Message: fmt.Sprintf("可用资金为负：冻结 %.2f 元超过账面现金 %.2f 元", account.ReservedCash, account.Cash)})
	}
	if absFloat(report.Drift) > 0.01 {
		report.Issues = append(report.Issues, Issue{Level: IssueError, Field: "reserved_cash",
			Message: fmt.Sprintf("冻结额与挂单不一致：账本记账 %.2f 元，按挂单重算 %.2f 元，差额 %.2f 元",
				report.ReservedCash, report.ExpectedReserved, report.Drift)})
	}
	if len(report.OrphanOrders) > 0 {
		report.Issues = append(report.Issues, Issue{Level: IssueWarning, Field: "orders",
			Message: fmt.Sprintf("%d 笔已结束订单仍占用冻结资金", len(report.OrphanOrders))})
	}

	// 修复条件用精确比较而不是容差：账本里哪怕只剩 1e-12 的冻结额，也是一笔
	// 不该存在的占用，早改掉比让它在容差里长期存在要好。
	if repair && (account.ReservedCash != report.ExpectedReserved || len(report.OrphanOrders) > 0 || len(report.ReservedQuantityDrift) > 0) {
		if err := s.repairReservations(ctx, accountID, orders, positions); err != nil {
			return report, err
		}
		report.Repaired = true
		report.DriftBefore = report.Drift
		report.ReservedCash = report.ExpectedReserved
		report.Drift = 0
		report.ReservedQuantityDrift = nil
		// 修复后当前状态已一致，但原始问题必须保留在报告里备查，只降级为提示。
		for index := range report.Issues {
			report.Issues[index].Level = IssueWarning
			report.Issues[index].Message = "（已修复）" + report.Issues[index].Message
		}
		report.OK = true
		return report, nil
	}
	report.OK = len(report.Issues) == 0
	return report, nil
}

func (s *Store) repairReservations(ctx context.Context, accountID string, orders []Order, positions []Position) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	expected := 0.0
	expectedSellQty := map[string]int{}
	for _, order := range orders {
		target := 0.0
		if order.Side == SideBuy && order.IsOpen() {
			target = expectedReserve(order, order.Quantity-order.FilledQuantity)
		}
		if order.Side == SideSell && order.IsOpen() {
			if remaining := order.Quantity - order.FilledQuantity; remaining > 0 {
				expectedSellQty[order.Symbol] += remaining
			}
		}
		if absFloat(order.ReservedAmount-target) > 0.01 {
			order.ReservedAmount = target
			if target <= 0 {
				order.FrozenAt = nil
			}
			payload, err := json.Marshal(order)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE trading_orders SET content_json=? WHERE id=?`, string(payload), order.ID); err != nil {
				return err
			}
		}
		expected += target
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trading_accounts SET reserved_cash=?,updated_at=? WHERE id=?`,
		round2(expected), nowText(now), accountID); err != nil {
		return err
	}
	for _, position := range positions {
		want := expectedSellQty[position.Symbol]
		if want == position.ReservedQuantity {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE trading_positions SET reserved_quantity=?,updated_at=?
			WHERE account_id=? AND symbol=?`, want, nowText(now), accountID, position.Symbol); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) listOrdersForReconcile(ctx context.Context, accountID string) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders
		WHERE account_id=? ORDER BY created_at DESC LIMIT 1000`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err == nil {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}

func (s *Store) getAccountTx(ctx context.Context, tx *sql.Tx, id string) (Account, error) {
	var (
		account              Account
		created, updatedText string
	)
	err := tx.QueryRowContext(ctx, `SELECT id,name,initial_cash,cash,reserved_cash,created_at,updated_at FROM trading_accounts WHERE id=?`, id).
		Scan(&account.ID, &account.Name, &account.InitialCash, &account.Cash, &account.ReservedCash, &created, &updatedText)
	if err != nil {
		return Account{}, err
	}
	account.CreatedAt = parseTime(created)
	account.UpdatedAt = parseTime(updatedText)
	return account, nil
}

func getOrderTx(ctx context.Context, tx *sql.Tx, id string) (Order, error) {
	var payload string
	if err := tx.QueryRowContext(ctx, `SELECT content_json FROM trading_orders WHERE id=?`, id).Scan(&payload); err != nil {
		return Order{}, err
	}
	var order Order
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return Order{}, err
	}
	return order, nil
}

// PendingBuyValue 汇总每个标的「未成交买入挂单」的剩余金额。
//
// 用途：单票权重上限必须把挂单算进去。只看已持仓会漏掉一种情况——
// 两份计划各买同一只 15%，合计 30%，每一份单独看都合规，
// 但成交后就突破了 20% 上限。资金冻结解决了"钱被重复规划"，
// 这个解决"仓位被重复规划"，两者缺一不可。
func (s *Store) PendingBuyValue(ctx context.Context, accountID string) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders
		WHERE account_id=? AND side=? AND status IN (?,?) ORDER BY created_at`,
		accountID, string(SideBuy), string(OrderStatusPending), string(OrderStatusPartial))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]float64{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		remaining := order.Quantity - order.FilledQuantity
		if remaining <= 0 || order.LimitPrice <= 0 {
			continue
		}
		values[order.Symbol] = round2(values[order.Symbol] + float64(remaining)*order.LimitPrice)
	}
	return values, rows.Err()
}

// GetControl 返回账户总闸。从未设置过时返回零值（全部放行），不写库。
func (s *Store) GetControl(ctx context.Context, accountID string) (Control, error) {
	control := Control{AccountID: accountID}
	var (
		pause, kill  int
		reason, when string
	)
	err := s.db.QueryRowContext(ctx, `SELECT pause_buying,kill_switch,reason,updated_at FROM trading_control WHERE account_id=?`, accountID).
		Scan(&pause, &kill, &reason, &when)
	if errors.Is(err, sql.ErrNoRows) {
		return control, nil
	}
	if err != nil {
		return Control{}, err
	}
	control.PauseBuying = pause != 0
	control.KillSwitch = kill != 0
	control.Reason = reason
	control.UpdatedAt = parseTime(when)
	return control, nil
}

// SaveControl 落库总闸。kill_switch 打开时强制 pause_buying 为真：
// 两种状态若出现「已全部停止但仍可买入」，前端与门控必然有一处漏判。
func (s *Store) SaveControl(ctx context.Context, control Control) (Control, error) {
	accountID := strings.TrimSpace(control.AccountID)
	if accountID == "" {
		accountID = DefaultAccountID
	}
	if control.KillSwitch {
		control.PauseBuying = true
	}
	control.AccountID = accountID
	control.Reason = strings.TrimSpace(control.Reason)
	control.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO trading_control(account_id,pause_buying,kill_switch,reason,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET pause_buying=excluded.pause_buying,
			kill_switch=excluded.kill_switch, reason=excluded.reason, updated_at=excluded.updated_at`,
		accountID, boolToInt(control.PauseBuying), boolToInt(control.KillSwitch), control.Reason, nowText(control.UpdatedAt))
	if err != nil {
		return Control{}, err
	}
	return control, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
