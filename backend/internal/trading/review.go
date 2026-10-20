package trading

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/policy"
)

// 本文件是「复盘」。它只做一件事：把已经发生的事实摊平成数字，再从中提炼经验。
//
// 三条原则：
//  1. 数字全部由代码算，模型不参与。让模型算胜率，等于让它编一个看起来合理的数。
//  2. 事实来源只有账本：已实现平仓、成交、裁决、循环。未实现盈亏会随行情跳动，
//     拿它做归因，今天的结论明天就反过来。
//  3. 样本不足就明说。10 笔交易的 60% 胜率不是"胜率 60%"，是"还看不出来"。

// reviewSampleFloor 是给出胜率所需的最小样本数。
// 低于它只报平均盈亏，不给胜率——一个百分比在小样本上几乎总是误导。
const reviewSampleFloor = 30

// ReviewFacts 是某个交易日的既成事实。字段名都刻意用绝对数而不是比率，
// 比率可以在展示层算，事实不能在展示层猜。
type ReviewFacts struct {
	EquityStart float64 `json:"equity_start"`
	EquityEnd   float64 `json:"equity_end"`
	// DayChangePercent 是当日权益变化。它与已实现盈亏不同：
	// 前者含浮盈波动，后者只算落袋。两个都留着，才能分清"赚了但没卖"与"卖了就涨"。
	DayChangePercent float64 `json:"day_change_percent"`

	ClosedTrades int     `json:"closed_trades"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	WinRate      float64 `json:"win_rate"`
	RealizedPnL  float64 `json:"realized_pnl"`
	GrossProfit  float64 `json:"gross_profit"`
	GrossLoss    float64 `json:"gross_loss"`
	// ProfitFactor 是总盈利 / 总亏损。没有亏损时为 0，展示层据此显示"无亏损"。
	ProfitFactor float64 `json:"profit_factor"`
	AvgWin       float64 `json:"avg_win"`
	AvgLoss      float64 `json:"avg_loss"`
	AvgHoldDays  float64 `json:"avg_hold_days"`

	// 按离场原因分类。这是复盘最有用的一个维度：
	// 亏损集中在 stop_loss 说明入场或止损位有问题，集中在 model_exit 说明模型判断有问题。
	StopLossExits   int `json:"stop_loss_exits"`
	HardStopExits   int `json:"hard_stop_exits"`
	TrailStopExits  int `json:"trail_stop_exits"`
	TakeProfitExits int `json:"take_profit_exits"`
	ModelExits      int `json:"model_exits"`

	Cycles        int `json:"cycles"`
	CyclesDone    int `json:"cycles_done"`
	CyclesFailed  int `json:"cycles_failed"`
	CyclesSkipped int `json:"cycles_skipped"`
	Plans         int `json:"plans"`
	// Proposed / Accepted / Blocked 是裁决层面的事实：
	// "系统想买多少"与"实际允许买多少"的差距，全在这三个数里。
	Proposed int `json:"proposed"`
	Accepted int `json:"accepted"`
	Blocked  int `json:"blocked"`
	// BlockReasons 按裁决代码统计拦截次数，新的在前由展示层决定顺序。
	BlockReasons      []CodeCount `json:"block_reasons,omitempty"`
	ModelLatencyMSAvg int64       `json:"model_latency_ms_avg"`
	ModelTokens       int         `json:"model_tokens"`

	TopGainers []TradeRef `json:"top_gainers,omitempty"`
	TopLosers  []TradeRef `json:"top_losers,omitempty"`
}

// CodeCount 是某个裁决代码出现的次数。
type CodeCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// TradeRef 是复盘里引用一笔交易的最小信息。
type TradeRef struct {
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	PnL        float64 `json:"pnl"`
	PnLPercent float64 `json:"pnl_percent"`
	Trigger    string  `json:"trigger,omitempty"`
}

// Lesson 是一条从事实里提炼出的经验。
//
// WinRate 用指针的原因：样本不足时必须"不显示"，而不是显示 0%。
// 0% 与"还不知道"在复盘里是两条完全相反的信息。
type Lesson struct {
	Dimension string   `json:"dimension"`
	Key       string   `json:"key"`
	Sample    int      `json:"sample"`
	Wins      int      `json:"wins"`
	WinRate   *float64 `json:"win_rate,omitempty"`
	AvgPnL    float64  `json:"avg_pnl"`
	TotalPnL  float64  `json:"total_pnl"`
	// Reliable 表示样本达到下限，胜率可以当结论看。
	Reliable bool   `json:"reliable"`
	Text     string `json:"text"`
}

// Review 是一个交易日的复盘结果。
type Review struct {
	AccountID   string      `json:"account_id"`
	TradingDay  string      `json:"trading_day"`
	GeneratedAt time.Time   `json:"generated_at"`
	Facts       ReviewFacts `json:"facts"`
	// Lessons 是当日样本提炼出的经验；Cumulative 是最近窗口的累积经验。
	// 两者都给：当日看"今天错在哪"，累积看"这套系统一贯错在哪"。
	Lessons    []Lesson `json:"lessons"`
	Cumulative []Lesson `json:"cumulative,omitempty"`
	// Narrative 是给人读的摘要，每一行都能对回 Facts 里的数字。
	Narrative []string `json:"narrative"`
}

// BuildReview 从账本事实生成复盘中。trades 是当日已实现平仓，window 是最近若干交易日的。
func BuildReview(facts ReviewFacts, dayTrades, windowTrades []ClosedTrade, regime string) Review {
	review := Review{
		TradingDay:  "",
		GeneratedAt: time.Now().UTC(),
		Facts:       facts,
		Lessons:     BuildLessons(dayTrades),
		Cumulative:  BuildLessons(windowTrades),
	}
	review.Narrative = explainReview(facts, dayTrades, regime)
	return review
}

// BuildLessons 从平仓记录提炼经验。维度固定为「离场原因」与「行业」两类：
// 这两个维度的样本能在几十笔内积累到有意义的量，而"按日期""按价格档位"分下去
// 每组只剩两三笔，除了制造噪声没有别的用途。
func BuildLessons(trades []ClosedTrade) []Lesson {
	if len(trades) == 0 {
		return []Lesson{}
	}
	lessons := make([]Lesson, 0, 8)
	lessons = append(lessons, lessonsBy(trades, "离场原因", func(trade ClosedTrade) string {
		return firstNonEmpty(trade.Trigger, "未标注")
	})...)
	lessons = append(lessons, lessonsBy(trades, "行业", func(trade ClosedTrade) string {
		return firstNonEmpty(trade.Sector, "未知行业")
	})...)
	// 按样本量降序：样本越多越接近结论，界面上应该先看到它们。
	sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].Sample > lessons[j].Sample })
	return lessons
}

func lessonsBy(trades []ClosedTrade, dimension string, keyOf func(ClosedTrade) string) []Lesson {
	type bucket struct {
		sample int
		wins   int
		total  float64
	}
	buckets := map[string]*bucket{}
	for _, trade := range trades {
		key := strings.TrimSpace(keyOf(trade))
		if key == "" {
			key = "未标注"
		}
		item, ok := buckets[key]
		if !ok {
			item = &bucket{}
			buckets[key] = item
		}
		item.sample++
		if trade.Win() {
			item.wins++
		}
		item.total += trade.PnL
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lessons := make([]Lesson, 0, len(keys))
	for _, key := range keys {
		item := buckets[key]
		avg := round2(item.total / float64(item.sample))
		lesson := Lesson{
			Dimension: dimension,
			Key:       key,
			Sample:    item.sample,
			Wins:      item.wins,
			AvgPnL:    avg,
			TotalPnL:  round2(item.total),
			Reliable:  item.sample >= reviewSampleFloor,
		}
		if lesson.Reliable {
			rate := round2(float64(item.wins) / float64(item.sample) * 100)
			lesson.WinRate = &rate
			lesson.Text = fmt.Sprintf("%s「%s」%d 笔：胜率 %.1f%%，平均每笔 %+.2f 元，合计 %+.2f 元",
				dimension, key, item.sample, rate, avg, lesson.TotalPnL)
		} else {
			lesson.Text = fmt.Sprintf("%s「%s」%d 笔：平均每笔 %+.2f 元，合计 %+.2f 元（样本不足 %d 笔，胜率不予显示）",
				dimension, key, item.sample, avg, lesson.TotalPnL, reviewSampleFloor)
		}
		lessons = append(lessons, lesson)
	}
	return lessons
}

// SummarizeTrades 把一批平仓汇总成事实字段。facts 由调用方先填好非交易类字段。
func SummarizeTrades(facts *ReviewFacts, trades []ClosedTrade) {
	facts.ClosedTrades = len(trades)
	for _, trade := range trades {
		if trade.Win() {
			facts.Wins++
			facts.GrossProfit += trade.PnL
		} else {
			facts.Losses++
			facts.GrossLoss += -trade.PnL
		}
		facts.RealizedPnL += trade.PnL
		facts.AvgHoldDays += float64(trade.HoldDays)
		switch {
		case trade.Trigger == TriggerStopLoss:
			facts.StopLossExits++
		case trade.Trigger == TriggerHardStop:
			facts.HardStopExits++
		case trade.Trigger == TriggerTrailStop:
			facts.TrailStopExits++
		case strings.HasPrefix(trade.Trigger, TriggerTakeProfit):
			facts.TakeProfitExits++
		case trade.Trigger == TriggerModelExit || trade.Trigger == TriggerManualExit:
			facts.ModelExits++
		}
	}
	facts.RealizedPnL = round2(facts.RealizedPnL)
	facts.GrossProfit = round2(facts.GrossProfit)
	facts.GrossLoss = round2(facts.GrossLoss)
	if facts.ClosedTrades > 0 {
		facts.WinRate = round2(float64(facts.Wins) / float64(facts.ClosedTrades) * 100)
		facts.AvgHoldDays = round2(facts.AvgHoldDays / float64(facts.ClosedTrades))
		facts.AvgWin = round2(facts.GrossProfit / float64(maxInt(facts.Wins, 1)))
		facts.AvgLoss = round2(facts.GrossLoss / float64(maxInt(facts.Losses, 1)))
	}
	if facts.GrossLoss > 0 {
		facts.ProfitFactor = round2(facts.GrossProfit / facts.GrossLoss)
	}
	sorted := append([]ClosedTrade{}, trades...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].PnL > sorted[j].PnL })
	for _, trade := range sorted {
		ref := TradeRef{Symbol: trade.Symbol, Name: trade.Name, PnL: trade.PnL, PnLPercent: trade.PnLPercent, Trigger: trade.Trigger}
		if len(facts.TopGainers) < 3 && trade.PnL > 0 {
			facts.TopGainers = append(facts.TopGainers, ref)
		}
	}
	for index := len(sorted) - 1; index >= 0; index-- {
		trade := sorted[index]
		if len(facts.TopLosers) >= 3 {
			break
		}
		if trade.PnL < 0 {
			facts.TopLosers = append(facts.TopLosers, TradeRef{
				Symbol: trade.Symbol, Name: trade.Name, PnL: trade.PnL, PnLPercent: trade.PnLPercent, Trigger: trade.Trigger})
		}
	}
}

// explainReview 生成可读摘要。它不复述每个数字，只讲数字之间的关系：
// "胜率 50% 但亏钱"这种结构性问题，比单看两个数更容易被忽略。
func explainReview(facts ReviewFacts, trades []ClosedTrade, regime string) []string {
	lines := []string{}
	lines = append(lines, fmt.Sprintf("当日权益 %.2f → %.2f（%+.2f%%），已实现盈亏 %+.2f 元",
		facts.EquityStart, facts.EquityEnd, facts.DayChangePercent, facts.RealizedPnL))
	if facts.ClosedTrades == 0 {
		lines = append(lines, "当日没有平仓，本日复盘只有行情与执行层面的事实，没有可归因的交易结果")
	} else {
		lines = append(lines, fmt.Sprintf("平仓 %d 笔：%d 胜 %d 负，胜率 %.1f%%，平均持有 %.1f 天",
			facts.ClosedTrades, facts.Wins, facts.Losses, facts.WinRate, facts.AvgHoldDays))
		if facts.WinRate >= 50 && facts.RealizedPnL < 0 {
			lines = append(lines, fmt.Sprintf("胜率过半但合计亏损：平均盈利 %.2f 元、平均亏损 %.2f 元，亏损笔的幅度大于盈利笔，属于典型的「小赚大亏」结构",
				facts.AvgWin, facts.AvgLoss))
		}
		if facts.StopLossExits+facts.HardStopExits > facts.TakeProfitExits {
			lines = append(lines, fmt.Sprintf("止损离场 %d 笔、止盈离场 %d 笔，被动离场多于主动兑现",
				facts.StopLossExits+facts.HardStopExits, facts.TakeProfitExits))
		}
		if facts.HardStopExits > 0 {
			lines = append(lines, fmt.Sprintf("有 %d 笔由账户硬止损强制离场，说明买入时没有给出有效止损线，入场环节需要修",
				facts.HardStopExits))
		}
	}
	if facts.Proposed > 0 {
		lines = append(lines, fmt.Sprintf("裁决层：模型建议 %d 笔，通过 %d 笔，拦截 %d 笔",
			facts.Proposed, facts.Accepted, facts.Blocked))
	}
	if facts.Cycles > 0 {
		lines = append(lines, fmt.Sprintf("循环 %d 轮：完成 %d、跳过 %d、失败 %d",
			facts.Cycles, facts.CyclesDone, facts.CyclesSkipped, facts.CyclesFailed))
	}
	if strings.TrimSpace(regime) != "" {
		lines = append(lines, "当日市场状态："+regime)
	}
	return lines
}

// SaveReview 幂等写入复盘。同一天重复生成会覆盖内容而不是新增一条：
// 复盘是"这天的事实"，重跑一次应该得到同一份，而不是两条略有差异的记录。
func (s *Store) SaveReview(ctx context.Context, review Review) error {
	payload, err := json.Marshal(review)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO trading_reviews(account_id,trading_day,content_json,generated_at)
		VALUES(?,?,?,?)
		ON CONFLICT(account_id,trading_day) DO UPDATE SET
			content_json=excluded.content_json, generated_at=excluded.generated_at`,
		review.AccountID, review.TradingDay, payload, nowText(review.GeneratedAt))
	return err
}

// GetReview 读取某一天的复盘；不存在时返回 (零值, false, nil)。
func (s *Store) GetReview(ctx context.Context, accountID, day string) (Review, bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_reviews WHERE account_id=? AND trading_day=?`,
		accountID, day).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Review{}, false, nil
		}
		return Review{}, false, err
	}
	var review Review
	if err := json.Unmarshal([]byte(payload), &review); err != nil {
		return Review{}, false, err
	}
	return review, true, nil
}

// ListReviews 返回最近若干天的复盘，新的在前。
func (s *Store) ListReviews(ctx context.Context, accountID string, limit int) ([]Review, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_reviews
		WHERE account_id=? ORDER BY trading_day DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var review Review
		if err := json.Unmarshal([]byte(payload), &review); err == nil {
			reviews = append(reviews, review)
		}
	}
	return reviews, rows.Err()
}

// ListClosedTrades 返回 [from, to] 闭区间内的平仓记录，按平仓时间升序。
func (s *Store) ListClosedTrades(ctx context.Context, accountID, fromDay, toDay string) ([]ClosedTrade, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_closed_trades
		WHERE account_id=? AND trading_day>=? AND trading_day<=? ORDER BY closed_at`, accountID, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	trades := []ClosedTrade{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var trade ClosedTrade
		if err := json.Unmarshal([]byte(payload), &trade); err == nil {
			trades = append(trades, trade)
		}
	}
	return trades, rows.Err()
}

// RecentBlockReasons 汇总一段时间内裁决的拦截代码次数。
// 它回答的是"系统最常在哪个环节拦住模型"——那是流程里最需要修的地方。
func summarizeDecisions(decisions []Decision) (proposed, accepted, blocked int, reasons []CodeCount) {
	counts := map[string]int{}
	for _, decision := range decisions {
		proposed += decision.Stats.Proposed
		accepted += decision.Stats.Accepted
		blocked += decision.Stats.Blocked
		for _, verdict := range decision.Verdicts {
			if verdict.Level != policy.LevelBlock && verdict.Level != policy.LevelAdjust {
				continue
			}
			counts[verdict.Code]++
		}
	}
	reasons = make([]CodeCount, 0, len(counts))
	for code, count := range counts {
		reasons = append(reasons, CodeCount{Code: code, Count: count})
	}
	sort.SliceStable(reasons, func(i, j int) bool { return reasons[i].Count > reasons[j].Count })
	return proposed, accepted, blocked, reasons
}
