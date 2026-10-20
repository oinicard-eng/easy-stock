package trading

import (
	"context"
	"sort"
	"time"
)

// Performance 是一段时间内的业绩归因。它回答的问题只有一个：
// 这套系统到目前为止，是靠什么赚钱、因为什么亏钱。
//
// 与复盘的区别：复盘是"某一天发生了什么"，业绩是"这些天累计成了什么"。
// 两者共用同一份平仓账本，口径必须一致——如果复盘的胜率和业绩页的胜率
// 对不上，两个数字就都不可信了。
type Performance struct {
	AccountID string `json:"account_id"`
	Days      int    `json:"days"`
	From      string `json:"from"`
	To        string `json:"to"`

	ClosedTrades int     `json:"closed_trades"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	WinRate      float64 `json:"win_rate"`
	RealizedPnL  float64 `json:"realized_pnl"`
	GrossProfit  float64 `json:"gross_profit"`
	GrossLoss    float64 `json:"gross_loss"`
	ProfitFactor float64 `json:"profit_factor"`
	AvgWin       float64 `json:"avg_win"`
	AvgLoss      float64 `json:"avg_loss"`
	AvgHoldDays  float64 `json:"avg_hold_days"`
	// Expectancy 是每笔交易的期望值：正数代表这套流程在样本期内是正期望的。
	// 它比胜率更接近"能不能长期做下去"，因为胜率不反映盈亏幅度。
	Expectancy float64 `json:"expectancy"`
	BestTrade  float64 `json:"best_trade"`
	WorstTrade float64 `json:"worst_trade"`

	StopLossExits   int `json:"stop_loss_exits"`
	HardStopExits   int `json:"hard_stop_exits"`
	TrailStopExits  int `json:"trail_stop_exits"`
	TakeProfitExits int `json:"take_profit_exits"`
	ModelExits      int `json:"model_exits"`

	// Daily 是逐日已实现盈亏与累计曲线，用于看"是不是某一天把整月打回去了"。
	Daily []DailyPnL `json:"daily"`
	// Lessons 按离场原因与行业分桶，样本不足 30 笔的桶不给胜率。
	Lessons []Lesson `json:"lessons"`
	// BySymbol 是按标的的归因：它暴露"反复在同一只票上亏"这种问题。
	BySymbol []Lesson `json:"by_symbol"`
}

// DailyPnL 是某一天的已实现结果与累计结果。
type DailyPnL struct {
	Day        string  `json:"day"`
	Trades     int     `json:"trades"`
	PnL        float64 `json:"pnl"`
	Cumulative float64 `json:"cumulative"`
}

// Performance 计算最近 days 个自然日内的业绩归因。
//
// 只统计已实现平仓：未实现盈亏会随行情跳动，把它算进胜率等于每天都在改历史。
func (s *Service) Performance(ctx context.Context, accountID string, days int) (Performance, error) {
	accountID = s.accountID(accountID)
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	now := time.Now()
	to := TradingDay(now)
	from := now.AddDate(0, 0, -(days - 1))
	perf := Performance{AccountID: accountID, Days: days, From: TradingDay(from), To: to}

	trades, err := s.store.ListClosedTrades(ctx, accountID, perf.From, perf.To)
	if err != nil {
		return perf, err
	}
	if len(trades) == 0 {
		perf.Daily = []DailyPnL{}
		perf.Lessons = []Lesson{}
		perf.BySymbol = []Lesson{}
		return perf, nil
	}

	facts := ReviewFacts{}
	SummarizeTrades(&facts, trades)
	perf.ClosedTrades = facts.ClosedTrades
	perf.Wins = facts.Wins
	perf.Losses = facts.Losses
	perf.WinRate = facts.WinRate
	perf.RealizedPnL = facts.RealizedPnL
	perf.GrossProfit = facts.GrossProfit
	perf.GrossLoss = facts.GrossLoss
	perf.ProfitFactor = facts.ProfitFactor
	perf.AvgWin = facts.AvgWin
	perf.AvgLoss = facts.AvgLoss
	perf.AvgHoldDays = facts.AvgHoldDays
	perf.StopLossExits = facts.StopLossExits
	perf.HardStopExits = facts.HardStopExits
	perf.TrailStopExits = facts.TrailStopExits
	perf.TakeProfitExits = facts.TakeProfitExits
	perf.ModelExits = facts.ModelExits
	perf.Expectancy = round2(facts.RealizedPnL / float64(facts.ClosedTrades))

	perf.BestTrade = trades[0].PnL
	perf.WorstTrade = trades[0].PnL
	for _, trade := range trades {
		if trade.PnL > perf.BestTrade {
			perf.BestTrade = trade.PnL
		}
		if trade.PnL < perf.WorstTrade {
			perf.WorstTrade = trade.PnL
		}
	}

	perf.Daily = dailyPnL(trades)
	perf.Lessons = BuildLessons(trades)
	perf.BySymbol = lessonsBy(trades, "标的", func(trade ClosedTrade) string {
		label := trade.Symbol
		if trade.Name != "" && trade.Name != trade.Symbol {
			label = trade.Symbol + " " + trade.Name
		}
		return label
	})
	return perf, nil
}

// dailyPnL 把平仓记录按交易日聚合成曲线。没有交易的交易日不出现在结果里：
// 补零会让曲线看起来"一直在参与"，而实际上那几天系统什么都没做。
func dailyPnL(trades []ClosedTrade) []DailyPnL {
	buckets := map[string]*DailyPnL{}
	order := []string{}
	for _, trade := range trades {
		day := trade.TradingDay
		if day == "" {
			day = trade.ClosedAt.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
		}
		item, ok := buckets[day]
		if !ok {
			item = &DailyPnL{Day: day}
			buckets[day] = item
			order = append(order, day)
		}
		item.Trades++
		item.PnL = round2(item.PnL + trade.PnL)
	}
	sort.Strings(order)
	out := make([]DailyPnL, 0, len(order))
	cumulative := 0.0
	for _, day := range order {
		item := buckets[day]
		cumulative = round2(cumulative + item.PnL)
		item.Cumulative = cumulative
		out = append(out, *item)
	}
	return out
}
