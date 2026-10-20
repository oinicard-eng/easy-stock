package trading

import (
	"context"
	"fmt"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// ProtectiveRun 是一次防守线检查的结果摘要，供循环日志与接口使用。
type ProtectiveRun struct {
	Checked    int      `json:"checked"`
	Triggered  int      `json:"triggered"`
	Created    int      `json:"created"`
	Filled     int      `json:"filled"`
	DecisionID string   `json:"decision_id,omitempty"`
	Orders     []Order  `json:"orders"`
	Notes      []string `json:"notes,omitempty"`
}

// RunProtective 对账户持仓跑一遍防守线，把触发的卖出立即送入撮合。
//
// 它不做的事：
//   - 不看市场状态（止损在恐慌里更该执行，而不是因为"市场不好"而豁免）；
//   - 不看门控 pause_buying（那只停买入），但 kill_switch 会拦住（全停就是全停）；
//   - 不调模型（模型超时的时候止损仍然要能执行）。
func (s *Service) RunProtective(ctx context.Context, accountID, cycleID string) (ProtectiveRun, error) {
	accountID = s.accountID(accountID)
	run := ProtectiveRun{Orders: []Order{}}

	control, err := s.store.GetControl(ctx, accountID)
	if err != nil {
		return run, err
	}
	if control.KillSwitch {
		run.Notes = append(run.Notes, "账户已全部停止（kill switch），防守线不执行")
		return run, nil
	}

	account, positions, err := s.AccountView(ctx, accountID)
	if err != nil {
		return run, err
	}
	if len(positions) == 0 {
		return run, nil
	}

	quotes, err := s.positionQuotes(ctx, positions)
	if err != nil {
		// 取不到行情就不判断。防守线必须建立在真实价格上，
		// 用成本价或 0 代替会产生"全部触发"或"永不触发"两种错误结论。
		return run, fmt.Errorf("获取持仓行情失败，跳过防守线检查：%w", err)
	}

	snapshot := Snapshot{
		TakenAt:      time.Now().UTC(),
		Equity:       account.Equity,
		Cash:         account.Cash,
		ReservedCash: account.ReservedCash,
		Quotes:       quotes,
		Positions:    positions,
	}
	rules := DefaultRiskRules()
	result := BuildProtectiveOrders(snapshot, rules, DefaultGuardConfig(), time.Now())
	// 复核计数在这一步累加：此刻价格已经取到、论点刚被逐只检查过，
	// 才是真正"复核了一次"。取不到行情时上面已经返回，不会虚增这个数。
	if bumpErr := s.store.BumpReviewCounts(ctx, accountID); bumpErr != nil {
		s.logf("event=trading_review_count_failed account=%s err=%v", accountID, bumpErr)
	}
	// 失效条件成立单独留痕。写在早退之前：即使这一轮没能生成卖单
	// （例如 T+1 不可卖），"判断已经被证伪"这件事也必须留下。
	// 只靠订单的 trigger 字段记录的话，事件流上看不到这类事实。
	for _, order := range result.Orders {
		if order.Trigger != TriggerInvalidation {
			continue
		}
		_ = s.store.AppendEvent(ctx, Event{
			AccountID: accountID,
			Severity:  EventSeverityWarning,
			Kind:      EventThesisInvalidated,
			Symbol:    order.Symbol,
			Message: fmt.Sprintf("%s 失效条件成立，减仓 %d 股（当初的论点：%s）",
				order.Symbol, order.Quantity, clipRunes(order.Thesis, 60)),
			Payload: eventPayload(map[string]any{
				"quantity": order.Quantity,
				"rule":     order.Invalidation,
				"thesis":   order.Thesis,
			}),
			CreatedAt: time.Now().UTC(),
		})
	}
	run.Checked = result.Held
	// Triggered 统计"防守线被触发的次数"，包含触发了但卖不掉的那种。
	// 只数订单会把"止损到了却因 T+1 卖不了"记成"没有触发"，
	// 而这恰恰是最需要被看见的一类事实。
	unsellable := 0
	for _, verdict := range result.Verdicts {
		if verdict.Code == policy.CodeGuardNoSellable {
			unsellable++
		}
	}
	run.Triggered = len(result.Orders) + unsellable
	run.Created = len(result.Orders)
	if len(result.Orders) == 0 {
		for _, verdict := range result.Verdicts {
			run.Notes = append(run.Notes, verdict.Message)
		}
		return run, nil
	}

	decision := ProtectiveDecision(snapshot, rules, control, result, cycleID, time.Now())
	decision.AccountID = accountID
	for index := range result.Orders {
		result.Orders[index].DecisionID = decision.ID
		result.Orders[index].CycleID = cycleID
	}

	// 裁决先落库：即便后面下单失败，"当时为什么要卖"也已经留下了。
	if err := s.store.SaveDecision(ctx, decision); err != nil {
		s.logf("event=trading_protective_decision_save_failed decision=%s err=%v", decision.ID, err)
	}
	run.DecisionID = decision.ID

	stored, err := s.store.CreateOrders(ctx, result.Orders)
	if err != nil {
		return run, fmt.Errorf("保护性卖单落库失败：%w", err)
	}
	run.Orders = stored

	// 立即撮合。保护性卖单不该在场上等下一轮：
	// 等一轮意味着要多承受一次完整的行情波动，而触发止损的前提正是"波动已经不利"。
	for index := range stored {
		order := stored[index]
		if order.Status != OrderStatusPending && order.Status != OrderStatusPartial {
			if order.RejectReason != "" {
				run.Notes = append(run.Notes, fmt.Sprintf("%s 卖单被拒：%s", order.Symbol, order.RejectReason))
			}
			continue
		}
		position := Position{}
		for _, item := range positions {
			if item.Symbol == order.Symbol {
				position = item
				break
			}
		}
		updated, matchErr := s.match(ctx, account, position, order)
		if matchErr != nil {
			run.Notes = append(run.Notes, fmt.Sprintf("%s 撮合失败：%v", order.Symbol, matchErr))
			continue
		}
		stored[index] = updated
		if updated.Status == OrderStatusFilled || updated.Status == OrderStatusPartial {
			run.Filled++
			if err := s.recordStopOutcome(ctx, accountID, updated); err != nil {
				s.logf("event=trading_stop_streak_failed order=%s err=%v", updated.ID, err)
			}
			if refreshed, after, err := s.AccountView(ctx, accountID); err == nil {
				account = refreshed
				positions = after
			}
		}
	}
	run.Orders = stored
	s.logf("event=trading_protective account=%s checked=%d triggered=%d filled=%d decision=%s",
		accountID, run.Checked, run.Triggered, run.Filled, decision.ID)
	return run, nil
}

// positionQuotes 取持仓标的的最新行情。批量一次请求，逐个标的发会慢到不可用。
func (s *Service) positionQuotes(ctx context.Context, positions []Position) ([]foundation.Quote, error) {
	if s.quotes == nil {
		return nil, fmt.Errorf("行情服务未配置")
	}
	symbols := make([]string, 0, len(positions))
	for _, position := range positions {
		symbols = append(symbols, position.Symbol)
	}
	quoteCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	quotes, err := s.quotes.Realtime(quoteCtx, symbols)
	if err != nil {
		return nil, err
	}
	valid := make([]foundation.Quote, 0, len(quotes))
	for _, quote := range quotes {
		if quote.Price > 0 {
			valid = append(valid, quote)
		}
	}
	return valid, nil
}

// recordStopOutcome 维护当日连续止损计数。
//
// 只统计保护性离场：模型主动卖出亏了钱，说明模型判断错，但不是"止损被反复打穿"，
// 用同一根计数会把两种截然不同的问题混成一个熔断。盈利离场则清零——
// 连续亏损的意思是"最近一直在输"，中间赢一次就不该继续算连败。
func (s *Service) recordStopOutcome(ctx context.Context, accountID string, order Order) error {
	if order.Source != OrderSourceProtective {
		return nil
	}
	isStop := order.Trigger == TriggerStopLoss || order.Trigger == TriggerHardStop || order.Trigger == TriggerTrailStop
	delta := 0
	switch {
	case isStop && order.RealizedPnL < 0:
		delta = 1
	case order.RealizedPnL > 0:
		delta = -1 << 30 // 直接归零，见 Store.AdjustStopStreak
	}
	if delta == 0 {
		return nil
	}
	_, err := s.store.AdjustStopStreak(ctx, accountID, TradingDay(time.Now()), delta)
	return err
}
