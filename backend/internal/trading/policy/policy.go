// Package policy 是交易决策的确定性约束层。
//
// 它只回答一个问题：这笔买入，在给定的账户与组合状态下，允许还是不允许。
// 不取数、不调模型、不写库——因为可被单元测试穷举的规则，才是真正生效的规则。
//
// 两条设计约束：
//
//  1. 每条裁决必须带稳定的 Code。复盘时要能统计"这周被 risk.sector_weight
//     拦了多少笔"，而不是去正则匹配中文提示语。
//  2. 拦截（block）与调整（adjust）必须分开。前者是"不允许"，后者是
//     "允许但已缩量"。混为一谈会让复盘统计失真：缩量成交的单子被算成失败，
//     组合的实际行为就再也解释不清楚了。
package policy

import (
	"fmt"
	"math"
	"strings"
)

// Level 是裁决的严重级别。只有 block 会改变动作方向。
type Level string

const (
	// LevelBlock 表示该动作被拒绝。
	LevelBlock Level = "block"
	// LevelAdjust 表示动作被允许，但数量或价格已被守卫改写。
	LevelAdjust Level = "adjust"
	// LevelWarn 表示动作未被改变，只是值得注意。
	LevelWarn Level = "warn"
)

func (l Level) Valid() bool {
	return l == LevelBlock || l == LevelAdjust || l == LevelWarn
}

// Scope 说明这条裁决作用在哪一层，便于前端分组与复盘归因。
type Scope string

const (
	// ScopeAccount 账户级：总闸、账户状态。
	ScopeAccount Scope = "account"
	// ScopeMarket 市场级：状态判断与仓位预算。
	ScopeMarket Scope = "market"
	// ScopeOrder 单笔级：代码、价格、止损、单票上限。
	ScopeOrder Scope = "order"
	// ScopePortfolio 组合级：行业集中、总仓位、现金底线。
	ScopePortfolio Scope = "portfolio"
	// ScopePosition 持仓级：T+1、可卖数量。
	ScopePosition Scope = "position"
)

// 裁决代码。一旦发布就不再改名：它会出现在历史决策的 JSON 里，
// 改名等于让过去的数据失去解释力。需要新增语义时加新代码。
const (
	CodeGateKillSwitch  = "gate.kill_switch"
	CodeGatePauseBuying = "gate.pause_buying"
	// CodeGateBreaker 表示当日的自动熔断已触发（日亏超限）。
	// 它与 pause_buying 是两件事：后者是人按的开关，前者是系统按事实按下的。
	// 混成一条会让复盘分不清"这次是人不让买"还是"这笔亏太多"。
	CodeGateBreaker = "gate.breaker"
	// CodeGateWindow 表示当前时段不允许开新仓（尾盘禁开仓）。
	CodeGateWindow = "gate.session_window"

	CodeMarketUnknown = "market.unknown"
	CodeMarketPanic   = "market.panic"

	CodeOrderCountCap = "order.count_cap"
	CodeOrderEmpty    = "order.empty"
	// CodeOrderBuyDropped 表示门控或市场状态导致买入建议被整批剔除。
	// 它与 CodeGatePauseBuying 的区别：后者是门控本身，前者是门控造成的后果。
	CodeOrderBuyDropped      = "order.buy_dropped"
	CodeOrderInvalidSymbol   = "order.invalid_symbol"
	CodeOrderDuplicateSymbol = "order.duplicate_symbol"
	CodeOrderInvalidSide     = "order.invalid_side"
	CodeOrderNoQuote         = "order.no_quote"
	CodeOrderPriceOutOfLimit = "order.price_out_of_limit"
	CodeOrderLimitPriceMiss  = "order.limit_price_missing"

	CodeRiskSingleName      = "risk.single_name"
	CodeRiskSectorWeight    = "risk.sector_weight"
	CodeRiskSectorNameCount = "risk.sector_name_count"
	CodeRiskStopLossMissing = "risk.stop_loss_missing"
	CodeRiskStopLossInvalid = "risk.stop_loss_invalid"
	CodeRiskStopLossWide    = "risk.stop_loss_wide"
	CodeRiskTradeRisk       = "risk.trade_risk"
	CodeRiskExposure        = "risk.exposure"
	CodeRiskCashFloor       = "risk.cash_floor"

	CodeSizingBelowLot       = "sizing.below_lot"
	CodeSizingReducedByCash  = "sizing.reduced_by_cash"
	CodeSizingScaled         = "sizing.scaled"
	CodeSizingDroppedOnScale = "sizing.dropped_on_scale"

	CodePositionNotHeld     = "position.not_held"
	CodePositionUnavailable = "position.unavailable"

	// CodeLedgerReserveFailed 表示订单在落库冻结时被账本拒绝（可用资金不足）。
	// 它发生在裁决之后，但仍是裁决的一部分：计划说"可执行"而账本说"没钱"，
	// 这种矛盾必须留下痕迹，而不是只写在订单的 reject_reason 里。
	CodeLedgerReserveFailed = "ledger.reserve_failed"

	// 保护性离场。这四个代码不由模型产生，而是持仓防守线被触发时的确定性裁决。
	// 单独成组是因为它们的复盘含义完全不同：止损是被动认赔，
	// 止盈是计划内兑现，把它们与"模型想卖"混在一起统计会让胜率失去意义。
	CodeGuardStopLoss   = "guard.stop_loss"
	CodeGuardTrailStop  = "guard.trail_stop"
	CodeGuardHardStop   = "guard.hard_stop"
	CodeGuardTakeProfit = "guard.take_profit"
	// CodeGuardNoSellable 表示防守线已触发但可卖股数为 0（T+1 或已在途卖单）。
	// 必须显式记录：不写的话，"止损没执行"会看起来像"系统没发现"。
	CodeGuardNoSellable = "guard.no_sellable"
)

// Verdict 是一条裁决记录。Metric 与 Limit 是这条裁决所依据的数字，
// 复盘时可以只靠它们重算当时是否该拦，不必翻日志。
type Verdict struct {
	Code    string  `json:"code"`
	Level   Level   `json:"level"`
	Scope   Scope   `json:"scope"`
	Symbol  string  `json:"symbol,omitempty"`
	Field   string  `json:"field,omitempty"`
	Message string  `json:"message"`
	Metric  float64 `json:"metric,omitempty"`
	Limit   float64 `json:"limit,omitempty"`
}

// Blocked 表示这条裁决是否真的否决了动作。
func (v Verdict) Blocked() bool { return v.Level == LevelBlock }

// Limits 是全部数值上限。模型只能被提示词约束，真正的边界在这里。
//
// 行业上限（MaxSectorWeightPercent / MaxSectorNames）是 P2.3 新增的：
// 单票 20% 不等于分散，六只票同属电子一个周期，组合风险与单票满仓没有区别。
type Limits struct {
	MaxOrders               int     `json:"max_orders"`
	MaxSingleWeightPercent  float64 `json:"max_single_weight_percent"`
	MaxTotalExposurePercent float64 `json:"max_total_exposure_percent"`
	MinCashPercent          float64 `json:"min_cash_percent"`
	MaxRiskPerTradePercent  float64 `json:"max_risk_per_trade_percent"`
	MaxStopLossPercent      float64 `json:"max_stop_loss_percent"`
	BoardLot                int     `json:"board_lot"`
	MaxSectorWeightPercent  float64 `json:"max_sector_weight_percent"`
	MaxSectorNames          int     `json:"max_sector_names"`
}

// Default 是基准阈值。
func Default() Limits {
	return Limits{
		MaxOrders:               8,
		MaxSingleWeightPercent:  20,
		MaxTotalExposurePercent: 95,
		MinCashPercent:          5,
		MaxRiskPerTradePercent:  2,
		MaxStopLossPercent:      10,
		BoardLot:                100,
		MaxSectorWeightPercent:  45,
		MaxSectorNames:          3,
	}
}

// Normalize 把越界或缺失的阈值拉回合法区间。
// 它不报错而是静默修正：风控阈值坏了必须继续保护账户，而不是让下单流程失败。
func (l Limits) Normalize() Limits {
	defaults := Default()
	if l.MaxOrders <= 0 || l.MaxOrders > 20 {
		l.MaxOrders = defaults.MaxOrders
	}
	if l.MaxSingleWeightPercent <= 0 || l.MaxSingleWeightPercent > 100 {
		l.MaxSingleWeightPercent = defaults.MaxSingleWeightPercent
	}
	if l.MaxTotalExposurePercent <= 0 || l.MaxTotalExposurePercent > 100 {
		l.MaxTotalExposurePercent = defaults.MaxTotalExposurePercent
	}
	if l.MinCashPercent < 0 || l.MinCashPercent > 100 {
		l.MinCashPercent = defaults.MinCashPercent
	}
	if l.MaxRiskPerTradePercent <= 0 || l.MaxRiskPerTradePercent > 20 {
		l.MaxRiskPerTradePercent = defaults.MaxRiskPerTradePercent
	}
	if l.MaxStopLossPercent <= 0 || l.MaxStopLossPercent > 50 {
		l.MaxStopLossPercent = defaults.MaxStopLossPercent
	}
	if l.BoardLot <= 0 {
		l.BoardLot = defaults.BoardLot
	}
	if l.MaxSectorWeightPercent <= 0 || l.MaxSectorWeightPercent > 100 {
		l.MaxSectorWeightPercent = defaults.MaxSectorWeightPercent
	}
	if l.MaxSectorNames <= 0 {
		l.MaxSectorNames = defaults.MaxSectorNames
	}
	return l
}

// Book 汇总组合层面的暴露，让每一笔买入都对着同一个预算判断。
//
// 为什么需要它：逐笔检查单票上限，六笔各 15% 全部通过，合计 90% 压在同一个行业里。
// 组合风险不是单笔风险的加总，必须有一个能累积状态的账。
type Book struct {
	equity      float64
	limits      Limits
	symbolW     map[string]float64
	sectorW     map[string]float64
	sectorNames map[string]map[string]bool
}

// NewBook 以当前总资产与已生效的暴露初始化组合账。
func NewBook(equity float64, limits Limits) *Book {
	return &Book{
		equity:      equity,
		limits:      limits.Normalize(),
		symbolW:     map[string]float64{},
		sectorW:     map[string]float64{},
		sectorNames: map[string]map[string]bool{},
	}
}

// Seed 计入一笔已存在的暴露（已持仓或在途挂单）。sector 为空时只计单票，
// 不计行业——宁可少拦，也不要把未知行业猜成某个具体行业。
func (b *Book) Seed(symbol, sector string, weight float64) {
	if weight <= 0 {
		return
	}
	b.symbolW[symbol] += weight
	if sector = strings.TrimSpace(sector); sector != "" {
		b.sectorW[sector] += weight
		if b.sectorNames[sector] == nil {
			b.sectorNames[sector] = map[string]bool{}
		}
		b.sectorNames[sector][symbol] = true
	}
}

// SymbolWeight 返回该标的已计入的暴露（含本次已通过的买入）。
func (b *Book) SymbolWeight(symbol string) float64 { return round2(b.symbolW[symbol]) }

// SectorWeight 返回该行业已计入的暴露。
func (b *Book) SectorWeight(sector string) float64 {
	return round2(b.sectorW[strings.TrimSpace(sector)])
}

// TryBuy 判断一笔买入能否计入组合，通过则累加暴露。
// 失败时返回 block 裁决且不改变账本——检查与提交必须是两个动作，
// 否则被拒绝的订单会污染后续订单的判断。
func (b *Book) TryBuy(symbol, sector string, weight float64) (Verdict, bool) {
	if weight <= 0 {
		return Verdict{}, true
	}
	sector = strings.TrimSpace(sector)
	combined := round2(b.symbolW[symbol] + weight)
	if b.limits.MaxSingleWeightPercent > 0 && combined > b.limits.MaxSingleWeightPercent {
		return Verdict{
			Code: CodeRiskSingleName, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: fmt.Sprintf("%s 已持仓与在途挂单合计 %.2f%%，再加 %.2f%% 将达 %.2f%%，超过单票上限 %.2f%%",
				symbol, b.symbolW[symbol], weight, combined, b.limits.MaxSingleWeightPercent),
			Metric: combined, Limit: b.limits.MaxSingleWeightPercent,
		}, false
	}
	if sector != "" {
		sectorTotal := round2(b.sectorW[sector] + weight)
		if b.limits.MaxSectorWeightPercent > 0 && sectorTotal > b.limits.MaxSectorWeightPercent {
			return Verdict{
				Code: CodeRiskSectorWeight, Level: LevelBlock, Scope: ScopePortfolio, Symbol: symbol,
				Message: fmt.Sprintf("%s 所属「%s」买入后合计 %.2f%%，超过行业上限 %.2f%%（集中度不是分散）",
					symbol, sector, sectorTotal, b.limits.MaxSectorWeightPercent),
				Metric: sectorTotal, Limit: b.limits.MaxSectorWeightPercent,
			}, false
		}
		names := b.sectorNames[sector]
		if names != nil && !names[symbol] && len(names) >= b.limits.MaxSectorNames {
			return Verdict{
				Code: CodeRiskSectorNameCount, Level: LevelBlock, Scope: ScopePortfolio, Symbol: symbol,
				Message: fmt.Sprintf("「%s」已持有 %d 只（%s），达到行业只数上限 %d",
					sector, len(names), strings.Join(sortedKeys(names), "、"), b.limits.MaxSectorNames),
				Metric: float64(len(names)), Limit: float64(b.limits.MaxSectorNames),
			}, false
		}
	}
	b.symbolW[symbol] = combined
	if sector != "" {
		b.sectorW[sector] = round2(b.sectorW[sector] + weight)
		if b.sectorNames[sector] == nil {
			b.sectorNames[sector] = map[string]bool{}
		}
		b.sectorNames[sector][symbol] = true
	}
	return Verdict{}, true
}

// Exposure 返回当前已计入的总暴露百分比。
func (b *Book) Exposure() float64 {
	if b.equity <= 0 {
		return 0
	}
	total := 0.0
	for _, weight := range b.symbolW {
		total += weight
	}
	return round2(total)
}

// Sectors 返回按暴露降序的行业分布，用于裁决摘要。
func (b *Book) Sectors() []SectorWeight {
	out := make([]SectorWeight, 0, len(b.sectorW))
	for name, weight := range b.sectorW {
		out = append(out, SectorWeight{Sector: name, Weight: round2(weight), Names: len(b.sectorNames[name])})
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Weight > out[i].Weight {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// SectorWeight 是一个行业的暴露与只数。
type SectorWeight struct {
	Sector string  `json:"sector"`
	Weight float64 `json:"weight"`
	Names  int     `json:"names"`
}

// CheckStopLoss 校验止损的合法性与距离。买入没有止损就不该存在：
// 没有失效条件的买入建议无法复盘，也无法自动退出。
func (l Limits) CheckStopLoss(symbol string, entry, stop float64) (Verdict, bool) {
	l = l.Normalize()
	switch {
	case stop <= 0:
		return Verdict{
			Code: CodeRiskStopLossMissing, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: fmt.Sprintf("%s 买入缺少止损价", symbol),
		}, false
	case stop >= entry:
		return Verdict{
			Code: CodeRiskStopLossInvalid, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: fmt.Sprintf("%s 止损价 %.2f 必须低于买入价 %.2f", symbol, stop, entry),
			Metric:  stop, Limit: entry,
		}, false
	}
	distance := round2((entry - stop) / entry * 100)
	if distance > l.MaxStopLossPercent {
		return Verdict{
			Code: CodeRiskStopLossWide, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: fmt.Sprintf("%s 止损距离 %.2f%% 超过上限 %.2f%%", symbol, distance, l.MaxStopLossPercent),
			Metric:  distance, Limit: l.MaxStopLossPercent,
		}, false
	}
	return Verdict{}, true
}

// CheckTradeRisk 校验单笔最大亏损占账户比例。
// riskPercent 的依据是"止损真的触发"时的亏损，而不是仓位大小。
func (l Limits) CheckTradeRisk(symbol string, lossAmount, equity float64) (Verdict, bool) {
	l = l.Normalize()
	if equity <= 0 {
		return Verdict{
			Code: CodeRiskTradeRisk, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: "账户总资产异常，无法评估单笔风险",
		}, false
	}
	percent := round2(lossAmount / equity * 100)
	if percent > l.MaxRiskPerTradePercent {
		return Verdict{
			Code: CodeRiskTradeRisk, Level: LevelBlock, Scope: ScopeOrder, Symbol: symbol,
			Message: fmt.Sprintf("%s 止损触发将亏损 %.2f%%，超过单笔风险上限 %.2f%%",
				symbol, percent, l.MaxRiskPerTradePercent),
			Metric: percent, Limit: l.MaxRiskPerTradePercent,
		}, false
	}
	return Verdict{}, true
}

// CheckCashFloor 校验买入后的现金占比。它是组合级的最后一道：
// 前几道都通过、钱也够，但买完只剩 1% 现金，一次跳空就没有补仓与止损的余地。
func (l Limits) CheckCashFloor(projectedPercent float64) (Verdict, bool) {
	l = l.Normalize()
	if projectedPercent < l.MinCashPercent {
		return Verdict{
			Code: CodeRiskCashFloor, Level: LevelWarn, Scope: ScopePortfolio,
			Message: fmt.Sprintf("买入后现金占比约 %.2f%%，低于底线 %.2f%%", projectedPercent, l.MinCashPercent),
			Metric:  projectedPercent, Limit: l.MinCashPercent,
		}, false
	}
	return Verdict{}, true
}

// Stats 是裁决统计，给前端一句话摘要与复盘聚合共用。
type Stats struct {
	Proposed int `json:"proposed"`
	Accepted int `json:"accepted"`
	Blocked  int `json:"blocked"`
	Adjusted int `json:"adjusted"`
	Warned   int `json:"warned"`
	Ordered  int `json:"ordered"`
}

// Summarize 从订单状态与裁决流计算统计。
func Summarize(verdicts []Verdict) Stats {
	stats := Stats{}
	for _, verdict := range verdicts {
		switch verdict.Level {
		case LevelBlock:
			stats.Blocked++
		case LevelAdjust:
			stats.Adjusted++
		case LevelWarn:
			stats.Warned++
		}
	}
	return stats
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func round2(value float64) float64 { return math.Round(value*100) / 100 }
