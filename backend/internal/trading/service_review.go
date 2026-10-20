package trading

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultReviewWindow 是累积经验默认回看的交易日数。
// 太短（5 天）样本不足以下结论，太长（250 天）会把半年前的策略环境算进今天。
const DefaultReviewWindow = 30

// GenerateReview 计算某个交易日的复盘并落库。
//
// 它是可以反复调用的：同样的账本事实应当得到同样的结论。重跑只会覆盖那一天的记录，
// 不会新增第二条——复盘不是流水，是"这天到底发生了什么"的定稿。
func (s *Service) GenerateReview(ctx context.Context, accountID, day string) (Review, error) {
	accountID = s.accountID(accountID)
	if strings.TrimSpace(day) == "" {
		day = TradingDay(time.Now())
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return Review{}, fmt.Errorf("交易日格式应为 YYYY-MM-DD：%s", day)
	}

	dayTrades, err := s.store.ListClosedTrades(ctx, accountID, day, day)
	if err != nil {
		return Review{}, err
	}
	windowStart := shiftDay(day, -DefaultReviewWindow)
	windowTrades, err := s.store.ListClosedTrades(ctx, accountID, windowStart, day)
	if err != nil {
		return Review{}, err
	}

	// 顺手补齐情景记忆。复盘是"事后总结"，记忆是"事后存档"，
	// 两者读的是同一份平仓事实，放在一起不需要多一次查询。
	// 同步失败只记日志：记忆缺一天不影响复盘结论，但它自己会攒下空白。
	if added, syncErr := s.store.SyncMemories(ctx, accountID); syncErr != nil {
		s.logf("event=trading_memory_sync_failed account=%s err=%v", accountID, syncErr)
	} else if added > 0 {
		s.logf("event=trading_memory_synced account=%s added=%d", accountID, added)
	}

	facts := ReviewFacts{}
	SummarizeTrades(&facts, dayTrades)

	// 裁决与循环按天筛。它们没有按天索引的查询方法，量也不大（一天几十条），
	// 在内存里筛比再加一套 SQL 更不容易出错。
	decisions, err := s.store.ListDecisions(ctx, accountID, 400)
	if err != nil {
		return Review{}, err
	}
	dayDecisions := make([]Decision, 0, len(decisions))
	for _, decision := range decisions {
		if TradingDay(decision.TakenAt) == day {
			dayDecisions = append(dayDecisions, decision)
		}
	}
	facts.Proposed, facts.Accepted, facts.Blocked, facts.BlockReasons = summarizeDecisions(dayDecisions)
	latency, tokens, count := int64(0), 0, 0
	for _, decision := range dayDecisions {
		if decision.Model.LatencyMS <= 0 {
			continue
		}
		latency += decision.Model.LatencyMS
		tokens += decision.Model.TotalTokens
		count++
	}
	if count > 0 {
		facts.ModelLatencyMSAvg = latency / int64(count)
		facts.ModelTokens = tokens
	}

	cycles, err := s.store.ListCycles(ctx, accountID, 400)
	if err != nil {
		return Review{}, err
	}
	regime := ""
	for _, cycle := range cycles {
		if TradingDay(cycle.StartedAt) != day {
			continue
		}
		facts.Cycles++
		switch cycle.Status {
		case CycleStatusDone:
			facts.CyclesDone++
		case CycleStatusFailed:
			facts.CyclesFailed++
		default:
			facts.CyclesSkipped++
		}
	}
	if len(dayDecisions) > 0 {
		latest := dayDecisions[0]
		regime = latest.Market.Label
	}

	plans, err := s.store.ListPlans(ctx, accountID, 200)
	if err != nil {
		return Review{}, err
	}
	for _, plan := range plans {
		if TradingDay(plan.CreatedAt) == day {
			facts.Plans++
		}
	}

	// 权益口径：起点用当日基准权益（与日亏熔断同源），终点优先用当日最后一轮
	// 循环的收盘权益——它才是"那天结束时"的快照，而不是现在这一刻的。
	state, err := s.store.GetDayState(ctx, accountID, day)
	if err != nil {
		return Review{}, err
	}
	facts.EquityStart = state.StartEquity
	facts.EquityEnd = state.StartEquity
	for _, cycle := range cycles {
		if TradingDay(cycle.StartedAt) != day || cycle.EquityAfter <= 0 {
			continue
		}
		facts.EquityEnd = cycle.EquityAfter
		break
	}
	if day == TradingDay(time.Now()) {
		if account, _, err := s.AccountView(ctx, accountID); err == nil {
			facts.EquityEnd = account.Equity
		}
	}
	facts.DayChangePercent = percentChange(facts.EquityStart, facts.EquityEnd)

	review := BuildReview(facts, dayTrades, windowTrades, regime)
	review.AccountID = accountID
	review.TradingDay = day
	if err := s.store.SaveReview(ctx, review); err != nil {
		return Review{}, err
	}
	s.logf("event=trading_review account=%s day=%s closed=%d pnl=%.2f lessons=%d",
		accountID, day, facts.ClosedTrades, facts.RealizedPnL, len(review.Lessons))
	return review, nil
}

// Reviews 返回最近的复盘记录。
func (s *Service) Reviews(ctx context.Context, accountID string, limit int) ([]Review, error) {
	return s.store.ListReviews(ctx, s.accountID(accountID), limit)
}

// Experience 返回可用于提示词的历史经验（累积窗口内的教训）。
//
// 只给「离场原因」与「行业」两个维度里样本最多的前几条：提示词不是数据库导出，
// 塞满 40 条统计会让模型把注意力花在读表格上，而不是看今天的行情。
func (s *Service) Experience(ctx context.Context, accountID string, limit int) []Lesson {
	accountID = s.accountID(accountID)
	today := TradingDay(time.Now())
	trades, err := s.store.ListClosedTrades(ctx, accountID, shiftDay(today, -DefaultReviewWindow), today)
	if err != nil {
		s.logf("event=trading_experience_failed account=%s err=%v", accountID, err)
		return nil
	}
	lessons := BuildLessons(trades)
	if limit > 0 && len(lessons) > limit {
		lessons = lessons[:limit]
	}
	return lessons
}

// shiftDay 在 YYYY-MM-DD 上做天数偏移。用时间解析而不是字符串拼接，
// 否则跨月、跨年时会算出 2026-13-01 这种日期。
func shiftDay(day string, delta int) string {
	parsed, err := time.ParseInLocation("2006-01-02", day, time.FixedZone("CST", 8*3600))
	if err != nil {
		return day
	}
	return parsed.AddDate(0, 0, delta).Format("2006-01-02")
}

// ExperienceLines 把经验转成提示词里的几行字。没有样本就明说没有，
// 而不是让模型以为"系统没有历史"和"历史一片大好"是一回事。
func ExperienceLines(lessons []Lesson) []string {
	if len(lessons) == 0 {
		return []string{"暂无已实现平仓记录，还没有可用的事后结论。本轮不要假装有经验。"}
	}
	sorted := append([]Lesson{}, lessons...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Sample > sorted[j].Sample })
	lines := make([]string, 0, len(sorted))
	for _, lesson := range sorted {
		lines = append(lines, lesson.Text)
	}
	return lines
}

// Review 按交易日读取复盘。
func (s *Service) Review(ctx context.Context, accountID, day string) (Review, bool, error) {
	accountID = s.accountID(accountID)
	if strings.TrimSpace(day) == "" {
		day = TradingDay(time.Now())
	}
	return s.store.GetReview(ctx, accountID, day)
}

// ClosedTrades 返回最近已实现平仓，新的在前。
// 存储层按日期区间查询，这里把区间放大到一年并在内存里截断：
// 加一个"按 limit 查"的 SQL 只为省一点内存，不值得多一条查询路径。
func (s *Service) ClosedTrades(ctx context.Context, accountID string, limit int) ([]ClosedTrade, error) {
	accountID = s.accountID(accountID)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	today := TradingDay(time.Now())
	trades, err := s.store.ListClosedTrades(ctx, accountID, shiftDay(today, -365), today)
	if err != nil {
		return nil, err
	}
	// 反转为新的在前。
	for left, right := 0, len(trades)-1; left < right; left, right = left+1, right-1 {
		trades[left], trades[right] = trades[right], trades[left]
	}
	if len(trades) > limit {
		trades = trades[:limit]
	}
	return trades, nil
}

// UpdateProtection 人工设置某只持仓的防守线。
func (s *Service) UpdateProtection(ctx context.Context, accountID, symbol string, stopLoss, takeProfit float64) (Position, error) {
	accountID = s.accountID(accountID)
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return Position{}, fmt.Errorf("缺少标的代码")
	}
	return s.store.UpdateProtection(ctx, accountID, symbol, stopLoss, takeProfit)
}
