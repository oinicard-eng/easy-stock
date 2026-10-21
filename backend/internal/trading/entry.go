package trading

// 入场位置的门。
//
// 这道门补的是一个存在很久的空缺：在它之前，14 条确定性规则全部落在
// "买多少"和"怎么止损"上，没有任何一条回答"在这个位置买，合理吗"。
// 于是模型可以在一个当日已涨 5.6%、离 20 日均线很远的价位上开仓，
// 系统的全部反应只是把仓位算小一点、把止损按 ATR 放宽一点——
// 它保证了这笔亏损可承受，但没有人问过这笔该不该做。
//
// 阈值不是拍的。用局域网 QuantMind 的 150 只票、750 根前复权日线
// （14.4 万个交易日截面）按"次日开盘买入"的口径统计过——
// 这一点很关键：我们的委托不会在当日收盘成交，用收盘价算前瞻收益
// 会系统性高估，因为涨幅大的票次日往往高开，那部分溢价我们吃不到。
// 所以下表用的是「5 日后收盘 / 次日开盘 - 1」。
//
//	当日涨幅      样本     未来1日   未来5日   次日跳空   5日胜率   开盘入场5日
//	[-2,0)      48664    +0.014   +0.185   -0.023    47.6%     +0.208
//	[ 0,2)      51933    +0.023   +0.254   -0.050    47.9%     +0.304
//	[ 2,4)      13557    +0.071   +0.397   -0.122    47.7%     +0.519
//	[ 4,6)       4199    +0.025   +0.288   -0.128    45.2%     +0.416
//	[ 6,8)       1288    -0.157   -0.281   -0.203    42.9%     -0.078
//	[ 8,+∞)      2576    +1.591   +0.991   +2.067    43.3%     -1.076
//
//	距 MA20       样本     未来5日   次日跳空   5日胜率   开盘入场5日
//	[ 0, 5)     47821    +0.330   -0.033    48.0%     +0.363
//	[ 5,10)     13736    +0.277   -0.093    45.0%     +0.370
//	[10,15)      4170    -0.137   -0.091    41.5%     -0.046
//	[15,20)      1665    -0.768   +0.151    38.5%     -0.919
//	[20,+∞)      1934    -1.485   +0.546    34.6%     -2.031
//
//	连涨天数      样本     未来5日   次日跳空   5日胜率   开盘入场5日
//	0            75788    +0.191   -0.077    47.7%     +0.268
//	2           16893    +0.410   -0.042    48.0%     +0.452
//	4            3651    +0.394   +0.100    47.7%     +0.294
//	5+           3249    +0.079   +0.522    44.4%     -0.443
//
// 三条结论都与直觉相反的地方要写清楚，否则后来人会按常识改回去：
//
//  1. 涨得最猛的那一档（≥8%，基本是涨停）从收盘价看是正收益（+0.99%），
//     但扣掉 2.07% 的次日跳空之后是 -1.08%。**跳空吃掉的部分才是我们的成本**，
//     所以不能因为"涨停还会涨"就放行。
//  2. 连涨 4 天不是问题，5 天才转负。把阈值设成 4 会误伤大量正常趋势票。
//  3. 站在 20 日新高本身几乎不影响收益（开盘入场 5 日 -0.01%），
//     真正有害的是"离均线太远"。所以规则盯偏离度，不盯新高。
//
// 未标定的部分如实说明：这份样本只有 3 年（QuantMind 单次最多 750 根），
// 没有覆盖 2015 与 2018 两轮极端行情；胜率口径是"5 日后为正"，
// 不含交易成本。等本地回放引擎建起来后应当用十年数据重标一次。

import (
	"fmt"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// entryMAPeriod 是均线窗口。20 日是 A 股最常用的短期位置基准，
// 再短会被单日噪音主导，再长（60 日）对持仓周期 3-10 天的计划没有区分度。
const entryMAPeriod = 20

// EntryPosition 是一只标的在裁决时刻的位置事实。
//
// 它随快照落库，因此三个月后还能回答"当时是在什么位置买的"。
// Measured 为 false 表示日线拿不到——这时所有数值都为 0，
// 调用方必须据此区分"位置很好"和"位置不知道"。
type EntryPosition struct {
	Symbol string `json:"symbol"`
	// ChangePercent 是当日涨幅（%），来自行情快照。
	ChangePercent float64 `json:"change_percent"`
	// FromMA20Percent 是现价相对 20 日均线的偏离（%）。
	FromMA20Percent float64 `json:"from_ma20_percent"`
	// FromHigh20Percent 是现价距 20 日收盘最高价的回落幅度（%），
	// 0 表示正处在 20 日新高。
	//
	// 它只用于展示与复盘，不参与拦截：实测站在新高本身对未来收益
	// 几乎没有影响（开盘入场 5 日 -0.01%），真正有害的是离均线太远。
	FromHigh20Percent float64 `json:"from_high20_percent"`
	// ConsecutiveUp 是截至今日的连续上涨天数。
	ConsecutiveUp int `json:"consecutive_up"`
	// Bars 是参与计算的日线根数，用于判断这份位置事实够不够硬。
	Bars int `json:"bars"`
	// Measured 表示位置是否真的算出来了。
	Measured bool `json:"measured"`
}

// entryPositionOf 用行情快照与日线画像组装位置事实。
//
// 两项输入分工明确：当日涨幅只有快照有（盘中实时），
// 均线与连涨只有日线有（历史）。任何一项缺失都不猜，只标 Measured=false。
func entryPositionOf(quote foundation.Quote, profile atrProfile) EntryPosition {
	position := EntryPosition{Symbol: firstNonEmpty(quote.Symbol, profile.Symbol), Bars: profile.Bars}
	if profile.MA20 <= 0 || profile.Price <= 0 {
		return position
	}
	// 当日涨幅有三个来源，按可靠性依次降级：
	// 快照自带的涨跌幅 → 快照现价与昨收 → 日线最后两根收盘。
	// 之所以要后两级：候选池有几十只票，行情快照里不一定每只都有；
	// 而"缺涨幅就当 0%"会把一只涨了 9% 的票判成平盘，这道门就形同虚设。
	change := 0.0
	switch {
	case quote.ChangePercent != 0:
		change = quote.ChangePercent
	case quote.Price > 0 && quote.PreviousClose > 0:
		change = (quote.Price/quote.PreviousClose - 1) * 100
	case profile.PrevClose > 0:
		change = (profile.Price/profile.PrevClose - 1) * 100
	}
	position.ChangePercent = round2(change)
	position.FromMA20Percent = round2((profile.Price/profile.MA20 - 1) * 100)
	if profile.High20 > 0 {
		position.FromHigh20Percent = round2((profile.Price/profile.High20 - 1) * 100)
	}
	position.ConsecutiveUp = profile.ConsecutiveUp
	position.Measured = true
	return position
}

// quoteOf 在快照里找一只票的行情，找不到时返回只带代码的空行情。
// 空行情让 entryPositionOf 退化到用日线最后两根算涨幅，而不是把涨幅当 0%。
func quoteOf(quotes []foundation.Quote, symbol string) foundation.Quote {
	for _, quote := range quotes {
		if quote.Symbol == symbol {
			return quote
		}
	}
	return foundation.Quote{Symbol: symbol}
}

// CheckEntryPosition 判断这笔买入是否处在一个已经被证明负期望的位置。
//
// 三条规则各管一种形态，命中任一条即拦截：
//   - entry_chase：当日涨幅 ≥ 阈值。追的是别人的止盈盘。
//   - entry_extended：现价高出 MA20 ≥ 阈值。连涨小阳线每天只涨 1.5%，
//     当日涨幅看着温和，累计偏离已经很远，只看涨幅会漏掉这种温水追高。
//   - entry_streak：连涨 ≥ 阈值天。趋势末端的最后一棒。
//
// 未测到位置时返回 warn 而不是 block：数据源抖动不该让账户停止开仓，
// 但也不能静默通过——留一条 warn，复盘时能看出这段买入没有位置约束。
//
// 返回的 bool 为 true 表示应当拦截。
func CheckEntryPosition(order Order, position EntryPosition, rules RiskRules) (policy.Verdict, bool) {
	rules = rules.Normalize()
	base := policy.Verdict{
		Scope:  policy.ScopeOrder,
		Symbol: order.Symbol,
	}
	if !position.Measured {
		base.Code = policy.CodeRiskEntryUnmeasured
		base.Level = policy.LevelWarn
		base.Field = "symbol"
		base.Message = fmt.Sprintf("%s 未取到日线，入场位置无法判定，本笔按无位置约束放行", order.Symbol)
		return base, false
	}
	switch {
	case position.ChangePercent >= rules.MaxEntryChangePercent:
		base.Code = policy.CodeRiskEntryChase
		base.Level = policy.LevelBlock
		base.Field = "symbol"
		base.Metric = position.ChangePercent
		base.Limit = rules.MaxEntryChangePercent
		base.Message = fmt.Sprintf("%s 当日已涨 %.2f%%（上限 %.1f%%），此位置买入是接别人的止盈盘",
			order.Symbol, position.ChangePercent, rules.MaxEntryChangePercent)
		return base, true
	case position.FromMA20Percent >= rules.MaxEntryMA20Percent:
		base.Code = policy.CodeRiskEntryExtended
		base.Level = policy.LevelBlock
		base.Field = "symbol"
		base.Metric = position.FromMA20Percent
		base.Limit = rules.MaxEntryMA20Percent
		base.Message = fmt.Sprintf("%s 现价高出 20 日均线 %.2f%%（上限 %.1f%%），等回踩再买",
			order.Symbol, position.FromMA20Percent, rules.MaxEntryMA20Percent)
		return base, true
	case position.ConsecutiveUp >= rules.MaxEntryUpDays:
		base.Code = policy.CodeRiskEntryStreak
		base.Level = policy.LevelBlock
		base.Field = "symbol"
		base.Metric = float64(position.ConsecutiveUp)
		base.Limit = float64(rules.MaxEntryUpDays)
		base.Message = fmt.Sprintf("%s 已连续上涨 %d 天（上限 %d 天），这一棒不该由我们接",
			order.Symbol, position.ConsecutiveUp, rules.MaxEntryUpDays)
		return base, true
	}
	return policy.Verdict{}, false
}

// countEntryBlocked 数出候选池里有多少只票在本轮因位置不可买。
// 它只用于日志与自证：光说"位置已接入"没有用，要能看出这一轮到底挡掉了几只。
func countEntryBlocked(snapshot Snapshot, rules RiskRules) int {
	blocked := 0
	for _, symbol := range snapshot.Universe {
		position, ok := snapshot.EntryPositions[normalizeSymbolOrEmpty(symbol)]
		if !ok {
			continue
		}
		if _, hit := CheckEntryPosition(Order{Symbol: symbol}, position, rules); hit {
			blocked++
		}
	}
	return blocked
}

// EntryPositionLine 把位置事实渲染成提示词里的一行。
// 写给人看也写给模型看：数字自己会说话，不需要加"注意风险"这种评语。
func EntryPositionLine(position EntryPosition) string {
	if !position.Measured {
		return "位置未知（未取到日线）"
	}
	high := "持平 20 日高"
	if position.FromHigh20Percent < 0 {
		high = fmt.Sprintf("距 20 日高 %.2f%%", position.FromHigh20Percent)
	}
	return fmt.Sprintf("当日 %+.2f%%，距 MA20 %+.2f%%，%s，连涨 %d 天",
		position.ChangePercent, position.FromMA20Percent, high, position.ConsecutiveUp)
}
