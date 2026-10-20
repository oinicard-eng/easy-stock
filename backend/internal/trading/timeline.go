package trading

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 时间线回放：把某一天发生的事按时间顺序摊平成一条线。
//
// 为什么需要它：系统跑起来之后，"今天为什么没买""这笔单是哪一轮下的"
// 这类问题会散落在 cycles、plans、decisions、orders、events 五张表里，
// 每张表都有自己的时间字段。人工对时间戳既慢又容易漏，
// 而复盘时最贵的恰恰是"重新建立当时的时间感"。
//
// 时间线不做任何判断、不做任何修复：它只按时间排序事实。
// 一旦它开始"解释"，就会和健康检查、复盘报告出现第二套口径。

// 时间线条目的类别。用常量是为了让前端能稳定地分组与配色。
const (
	TimelineCycleStart = "cycle.start"
	TimelineCycleEnd   = "cycle.end"
	TimelinePlan       = "plan"
	TimelineDecision   = "decision"
	TimelineOrder      = "order"
	TimelineFill       = "fill"
	TimelineEvent      = "event"
	TimelineReview     = "review"
)

// TimelineEntry 是时间线上的一个节点。
type TimelineEntry struct {
	At       time.Time `json:"at"`
	Category string    `json:"category"`
	Severity string    `json:"severity,omitempty"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	// RefID 指向原始记录，前端可据此跳转到详情。
	RefID  string  `json:"ref_id,omitempty"`
	Symbol string  `json:"symbol,omitempty"`
	Amount float64 `json:"amount,omitempty"`
}

// TimelineStats 是这一天的一句话摘要。
// 它存在的意义是让人不必读完整条线就能判断"这一天值不值得细看"。
type TimelineStats struct {
	Cycles     int `json:"cycles"`
	FailedCycles int `json:"failed_cycles"`
	Plans      int `json:"plans"`
	Decisions  int `json:"decisions"`
	Orders     int `json:"orders"`
	Filled     int `json:"filled"`
	Blocked    int `json:"blocked"`
	Events     int `json:"events"`
	Reviews    int `json:"reviews"`
	// ModelCalls / TotalTokens 回答"这一天思考花了多少钱"。
	// 没有它，自动交易的成本会变成一个只能靠账单反推的黑箱。
	ModelCalls  int `json:"model_calls"`
	TotalTokens int `json:"total_tokens"`
}

// Timeline 是某一天的回放结果。
type Timeline struct {
	Day       string          `json:"day"`
	AccountID string          `json:"account_id"`
	From      time.Time       `json:"from"`
	To        time.Time       `json:"to"`
	Stats     TimelineStats   `json:"stats"`
	Entries   []TimelineEntry `json:"entries"`
	// Notes 记录回放过程中的降级：某张表读失败时，时间线仍然返回，
	// 但必须让人知道"这条线不完整"，而不是让人以为那天真的什么都没发生。
	Notes []string `json:"notes,omitempty"`
}

// Timeline 汇总某一天的全部事件。
//
// day 按北京时间解释（与交易日口径一致），边界取当天 00:00 到次日 00:00。
func (s *Store) Timeline(ctx context.Context, accountID string, day time.Time) (Timeline, error) {
	cst := time.FixedZone("CST", 8*3600)
	local := day.In(cst)
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cst)
	to := from.AddDate(0, 0, 1)

	timeline := Timeline{
		Day:       from.Format("2006-01-02"),
		AccountID: accountID,
		From:      from,
		To:        to,
		Entries:   []TimelineEntry{},
	}
	// SQL 用 ±1 秒的宽松边界粗筛，再在内存里精确过滤。
	// 这样既能用上 created_at 索引，又不会被 RFC3339Nano 省略末尾零带来的
	// 字典序边界问题影响——"00:00:00Z" 在字符串上比 "00:00:00.5Z" 更大。
	queryFrom := nowText(from.Add(-time.Second))
	queryTo := nowText(to.Add(time.Second))
	inRange := func(at time.Time) bool {
		return !at.Before(from) && at.Before(to)
	}

	collect := func(table string, rows func() error) {
		if err := rows(); err != nil {
			timeline.Notes = append(timeline.Notes,
				fmt.Sprintf("%s 读取失败，本条线不完整：%v", table, err))
		}
	}

	collect("自动循环", func() error { return s.collectCycles(ctx, &timeline, accountID, queryFrom, queryTo, inRange) })
	collect("计划", func() error { return s.collectPlans(ctx, &timeline, accountID, queryFrom, queryTo, inRange) })
	collect("裁决", func() error { return s.collectDecisions(ctx, &timeline, accountID, queryFrom, queryTo, inRange) })
	collect("委托", func() error { return s.collectOrders(ctx, &timeline, accountID, queryFrom, queryTo, inRange) })
	collect("运行事件", func() error { return s.collectEvents(ctx, &timeline, accountID, queryFrom, queryTo, inRange) })
	collect("复盘", func() error { return s.collectReview(ctx, &timeline, accountID, timeline.Day) })

	sort.SliceStable(timeline.Entries, func(i, j int) bool {
		if timeline.Entries[i].At.Equal(timeline.Entries[j].At) {
			// 同一时刻按类别权重排：循环先于计划、计划先于裁决、裁决先于委托。
			// 否则"先有鸡还是先有蛋"的展示顺序会随查询计划变化。
			return timelineCategoryRank(timeline.Entries[i].Category) < timelineCategoryRank(timeline.Entries[j].Category)
		}
		return timeline.Entries[i].At.Before(timeline.Entries[j].At)
	})
	return timeline, nil
}

// timelineCategoryRank 给同类时刻的条目定展示优先级。
func timelineCategoryRank(category string) int {
	switch category {
	case TimelineCycleStart:
		return 0
	case TimelinePlan:
		return 1
	case TimelineDecision:
		return 2
	case TimelineOrder:
		return 3
	case TimelineFill:
		return 4
	case TimelineCycleEnd:
		return 5
	case TimelineEvent:
		return 6
	case TimelineReview:
		return 7
	}
	return 8
}

func (s *Store) collectCycles(ctx context.Context, timeline *Timeline, accountID, from, to string, inRange func(time.Time) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json, started_at, ended_at FROM trading_cycles
		WHERE account_id=? AND started_at>=? AND started_at<? ORDER BY started_at`, accountID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			content string
			started string
			ended   string
		)
		if err := rows.Scan(&content, &started, &ended); err != nil {
			return err
		}
		var cycle Cycle
		if err := json.Unmarshal([]byte(content), &cycle); err != nil {
			continue
		}
		if inRange(cycle.StartedAt) {
			timeline.Entries = append(timeline.Entries, TimelineEntry{
				At:       cycle.StartedAt,
				Category: TimelineCycleStart,
				Title:    "循环开始（" + cycleTriggerLabel(cycle.Trigger) + "）",
				Detail:   cycle.ID,
				RefID:    cycle.ID,
			})
			timeline.Stats.Cycles++
		}
		if cycle.EndedAt == nil || !inRange(*cycle.EndedAt) {
			continue
		}
		if cycle.Status == CycleStatusFailed {
			timeline.Stats.FailedCycles++
		}
		timeline.Stats.Blocked += cycle.Blocked
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			At:       *cycle.EndedAt,
			Category: TimelineCycleEnd,
			Severity: cycleStatusSeverity(cycle.Status),
			Title:    "循环结束：" + cycleStatusLabel(cycle.Status),
			Detail:   cycleEndDetail(cycle),
			RefID:    cycle.ID,
		})
	}
	return rows.Err()
}

func (s *Store) collectPlans(ctx context.Context, timeline *Timeline, accountID, from, to string, inRange func(time.Time) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_plans
		WHERE account_id=? AND created_at>=? AND created_at<? ORDER BY created_at`, accountID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		var plan Plan
		if err := json.Unmarshal([]byte(content), &plan); err != nil {
			continue
		}
		if !inRange(plan.CreatedAt) {
			continue
		}
		timeline.Stats.Plans++
		entry := TimelineEntry{
			At:       plan.CreatedAt,
			Category: TimelinePlan,
			Severity: planStatusSeverity(plan.Status),
			Title:    orderCountTitle("生成计划", len(plan.Orders), plan.Status),
			Detail:   truncate(strings.TrimSpace(plan.Summary), 180),
			RefID:    plan.ID,
		}
		if plan.ErrorMessage != "" {
			entry.Detail = truncate(plan.ErrorMessage, 180)
		}
		timeline.Entries = append(timeline.Entries, entry)

		if plan.ExecutedAt != nil && inRange(*plan.ExecutedAt) {
			timeline.Entries = append(timeline.Entries, TimelineEntry{
				At:       *plan.ExecutedAt,
				Category: TimelineOrder,
				Title:    fmt.Sprintf("执行计划：下发 %d 笔委托", len(plan.Orders)),
				RefID:    plan.ID,
			})
		}
	}
	return rows.Err()
}

func (s *Store) collectDecisions(ctx context.Context, timeline *Timeline, accountID, from, to string, inRange func(time.Time) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_decisions
		WHERE account_id=? AND created_at>=? AND created_at<? ORDER BY created_at`, accountID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		var decision Decision
		if err := json.Unmarshal([]byte(content), &decision); err != nil {
			continue
		}
		if !inRange(decision.TakenAt) {
			continue
		}
		timeline.Stats.Decisions++
		timeline.Stats.Blocked += decision.Stats.Blocked
		timeline.Stats.ModelCalls++
		timeline.Stats.TotalTokens += decision.Model.TotalTokens

		title := fmt.Sprintf("裁决：通过 %d 笔 / 拦截 %d 笔", decision.Stats.Accepted, decision.Stats.Blocked)
		detail := ""
		if len(decision.Narrative) > 0 {
			detail = truncate(strings.Join(decision.Narrative, "；"), 200)
		}
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			At:       decision.TakenAt,
			Category: TimelineDecision,
			Severity: decisionSeverity(decision),
			Title:    title,
			Detail:   detail,
			RefID:    decision.ID,
		})
	}
	return rows.Err()
}

func (s *Store) collectOrders(ctx context.Context, timeline *Timeline, accountID, from, to string, inRange func(time.Time) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders
		WHERE account_id=? AND created_at>=? AND created_at<? ORDER BY created_at`, accountID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		var order Order
		if err := json.Unmarshal([]byte(content), &order); err != nil {
			continue
		}
		if !inRange(order.CreatedAt) {
			continue
		}
		timeline.Stats.Orders++
		// 被拒的委托要标出来，并且保留原始数量：
		// "模型想买 100 股、被行业集中度拦住"是一个有用的结论，
		// 而"买入 0 股"只是噪声。
		var title string
		if order.Quantity <= 0 {
			// 存量数据的数量在旧逻辑下已被置零，这里如实说明，
			// 而不是显示一条读不懂的"买入 0 股"。
			title = fmt.Sprintf("%s %s（数量未记录）@%.2f",
				sideLabel(order.Side), displayName(order.Symbol, order.Name), order.LimitPrice)
		} else {
			title = fmt.Sprintf("%s %s %d 股 @%.2f",
				sideLabel(order.Side), displayName(order.Symbol, order.Name), order.Quantity, order.LimitPrice)
		}
		severity := ""
		if order.Status == OrderStatusRejected {
			title += "（被拒）"
			severity = EventSeverityWarning
		}
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			At:       order.CreatedAt,
			Category: TimelineOrder,
			Severity: severity,
			Title:    title,
			Detail:   orderDetail(order),
			RefID:    order.ID,
			Symbol:   order.Symbol,
			Amount:   order.LimitPrice * float64(order.Quantity),
		})

		// 成交时间没有独立字段，用 updated_at 近似：订单状态变成已成交时，
		// updated_at 就是那次状态跃迁的时间。它不总是精确到毫秒，
		// 但足以回答"这笔单是上午成交还是下午成交"。
		if order.Status != OrderStatusFilled && order.Status != OrderStatusPartial {
			continue
		}
		if order.FilledQuantity <= 0 || !inRange(order.UpdatedAt) || order.UpdatedAt.Equal(order.CreatedAt) {
			continue
		}
		detail := fmt.Sprintf("成交 %d 股 @%.2f", order.FilledQuantity, order.FilledPrice)
		if order.Fee != 0 {
			detail += fmt.Sprintf("，费用 %.2f", order.Fee)
		}
		if order.RealizedPnL != 0 {
			detail += fmt.Sprintf("，已实现盈亏 %+.2f", order.RealizedPnL)
		}
		timeline.Stats.Filled++
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			At:       order.UpdatedAt,
			Category: TimelineFill,
			Title:    fmt.Sprintf("成交 %s %s", sideLabel(order.Side), displayName(order.Symbol, order.Name)),
			Detail:   detail,
			RefID:    order.ID,
			Symbol:   order.Symbol,
			Amount:   order.FilledPrice * float64(order.FilledQuantity),
		})
	}
	return rows.Err()
}

func (s *Store) collectEvents(ctx context.Context, timeline *Timeline, accountID, from, to string, inRange func(time.Time) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT severity,kind,message,symbol,created_at FROM trading_events
		WHERE account_id=? AND created_at>=? AND created_at<? ORDER BY created_at`, accountID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			severity string
			kind     string
			message  string
			symbol   string
			created  string
		)
		if err := rows.Scan(&severity, &kind, &message, &symbol, &created); err != nil {
			return err
		}
		at := parseTime(created)
		if !inRange(at) {
			continue
		}
		timeline.Stats.Events++
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			At:       at,
			Category: TimelineEvent,
			Severity: severity,
			Title:    kind,
			Detail:   message,
			Symbol:   symbol,
		})
	}
	return rows.Err()
}

func (s *Store) collectReview(ctx context.Context, timeline *Timeline, accountID, day string) error {
	var content, generated string
	err := s.db.QueryRowContext(ctx, `SELECT content_json, generated_at FROM trading_reviews
		WHERE account_id=? AND trading_day=?`, accountID, day).Scan(&content, &generated)
	if err != nil {
		// 没有复盘是最常见的情况（当天还没收盘、或样本不足），不是错误。
		if strings.Contains(err.Error(), "no rows") {
			return nil
		}
		return err
	}
	var review Review
	if err := json.Unmarshal([]byte(content), &review); err != nil {
		return nil
	}
	at := parseTime(generated)
	if at.IsZero() {
		at = timeline.To
	}
	timeline.Stats.Reviews++
	timeline.Entries = append(timeline.Entries, TimelineEntry{
		At:       at,
		Category: TimelineReview,
		Title:    "生成当日复盘",
		Detail:   truncate(strings.Join(review.Narrative, "；"), 200),
	})
	return nil
}

func cycleTriggerLabel(trigger CycleTrigger) string {
	switch trigger {
	case CycleTriggerSchedule:
		return "定时"
	case CycleTriggerManual:
		return "人工触发"
	}
	return string(trigger)
}

func cycleStatusLabel(status CycleStatus) string {
	switch status {
	case CycleStatusDone:
		return "完成"
	case CycleStatusSkipped:
		return "跳过"
	case CycleStatusBlocked:
		return "被闸门拦住"
	case CycleStatusFailed:
		return "失败"
	case CycleStatusRunning:
		return "仍在运行"
	case "aborted":
		return "被中断"
	}
	return string(status)
}

func cycleStatusSeverity(status CycleStatus) string {
	switch status {
	case CycleStatusFailed:
		return EventSeverityWarning
	case CycleStatusBlocked:
		return EventSeverityWarning
	}
	return EventSeverityInfo
}

func cycleEndDetail(cycle Cycle) string {
	parts := make([]string, 0, 3)
	if cycle.Reason != "" {
		parts = append(parts, cycle.Reason)
	}
	if cycle.Orders > 0 || cycle.Filled > 0 {
		parts = append(parts, fmt.Sprintf("下单 %d 笔、成交 %d 笔", cycle.Orders, cycle.Filled))
	}
	if cycle.Blocked > 0 {
		parts = append(parts, fmt.Sprintf("拦截 %d 笔", cycle.Blocked))
	}
	if cycle.Breaker != "" {
		parts = append(parts, "熔断："+breakerLabel(cycle.Breaker))
	}
	if cycle.DurationMS > 0 {
		parts = append(parts, fmt.Sprintf("耗时 %.1fs", float64(cycle.DurationMS)/1000))
	}
	return strings.Join(parts, "，")
}

func planStatusSeverity(status PlanStatus) string {
	switch status {
	case PlanStatusFailed:
		return EventSeverityWarning
	}
	return EventSeverityInfo
}

func orderCountTitle(prefix string, count int, status PlanStatus) string {
	label := string(status)
	switch status {
	case PlanStatusDraft:
		label = "待确认"
	case PlanStatusExecuted:
		label = "已执行"
	case PlanStatusRejected:
		label = "已放弃"
	case PlanStatusFailed:
		label = "失败"
	case PlanStatusRunning:
		label = "生成中"
	}
	if count == 0 {
		return prefix + "（" + label + "，无建议）"
	}
	return fmt.Sprintf("%s（%s，%d 笔建议）", prefix, label, count)
}

func decisionSeverity(decision Decision) string {
	if decision.Stats.Blocked > 0 && decision.Stats.Accepted == 0 {
		return EventSeverityWarning
	}
	return EventSeverityInfo
}

func sideLabel(side Side) string {
	switch side {
	case SideBuy:
		return "买入"
	case SideSell:
		return "卖出"
	}
	return string(side)
}

func displayName(symbol, name string) string {
	if strings.TrimSpace(name) == "" {
		return symbol
	}
	return name
}

func orderDetail(order Order) string {
	parts := make([]string, 0, 4)
	if order.Source != "" {
		source := order.Source
		switch source {
		case "model":
			source = "模型建议"
		case "protective":
			source = "防守线触发"
		case "manual":
			source = "人工"
		}
		parts = append(parts, "来源："+source)
	}
	if order.Trigger != "" {
		parts = append(parts, "触发："+order.Trigger)
	}
	if order.StopLossPrice > 0 {
		parts = append(parts, fmt.Sprintf("止损 %.2f", order.StopLossPrice))
	}
	if order.Thesis != "" {
		parts = append(parts, truncate(order.Thesis, 120))
	}
	if order.RejectReason != "" {
		parts = append(parts, "拒绝原因："+order.RejectReason)
	}
	return strings.Join(parts, "，")
}

// Timeline 是 Service 层入口，默认账户。
func (s *Service) Timeline(ctx context.Context, accountID string, day time.Time) (Timeline, error) {
	return s.store.Timeline(ctx, s.accountID(accountID), day)
}
