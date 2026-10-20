// Package cognition 把一份全市场快照翻译成「当前市场处于什么状态」。
//
// 设计上只做确定性计算，不调用模型。理由：市场状态是后续所有决策的前提，
// 它必须可复现、可回测、可解释。如果让模型判断"今天是牛市还是熊市"，
// 同一个输入两次调用会得到两个答案，那么后面所有的仓位预算都失去基准。
//
// 样本口径刻意与选股层共用同一份 StockCatalog 快照：若认知层用指数接口、
// 选股层用目录接口，两者时间与成分不同，会出现"大盘判定退潮、候选池全是涨停板"
// 这种自相矛盾的局面，而模型无从判断该信哪一个。
package cognition

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/marketrule"
)

// Regime 是市场状态。取值固定且有限，便于落库统计与后续归因。
type Regime string

const (
	// RegimeUnknown 表示快照不足，无法判断。此时不允许开新仓，是安全默认值。
	RegimeUnknown Regime = "unknown"
	// RegimePanic 恐慌：跌停成片或中位数深跌，无差别抛售。
	RegimePanic Regime = "panic"
	// RegimeEbb 退潮：多数下跌，赚钱效应消失。
	RegimeEbb Regime = "ebb"
	// RegimeRange 震荡：涨跌互现，无明确方向。
	RegimeRange Regime = "range"
	// RegimeUptrend 上行：普涨，赚钱效应扩散但未过热。
	RegimeUptrend Regime = "uptrend"
	// RegimeEuphoria 亢奋：涨停成片、中位数大涨，情绪过热。
	RegimeEuphoria Regime = "euphoria"
)

var regimeLabels = map[Regime]string{
	RegimeUnknown:  "数据不足",
	RegimePanic:    "恐慌",
	RegimeEbb:      "退潮",
	RegimeRange:    "震荡",
	RegimeUptrend:  "上行",
	RegimeEuphoria: "亢奋",
}

// Label 返回中文名。
func (r Regime) Label() string {
	if label, ok := regimeLabels[r]; ok {
		return label
	}
	return string(r)
}

// Valid 表示该状态是否允许开新仓。unknown 与 panic 一律不开仓。
func (r Regime) Valid() bool {
	return r != RegimeUnknown && r != RegimePanic
}

// Breadth 是市场广度指标，全部由同一份快照算出，口径自洽。
type Breadth struct {
	// Total 是有有效价格（当日有交易）的标的数，停牌不计入。
	Total int `json:"total"`
	// Tradable 是有成交额、可用于选股的标的数。
	Tradable      int     `json:"tradable"`
	Advancing     int     `json:"advancing"`
	Declining     int     `json:"declining"`
	Unchanged     int     `json:"unchanged"`
	LimitUp       int     `json:"limit_up"`
	LimitDown     int     `json:"limit_down"`
	MedianChange  float64 `json:"median_change"`
	AverageChange float64 `json:"average_change"`
	// TotalAmount 是全市场当日成交额合计，单位元。
	TotalAmount float64 `json:"total_amount"`
	// AdvanceRatio 是上涨家数占涨跌总家数的比例，0~1。
	AdvanceRatio float64 `json:"advance_ratio"`
}

// IndustryHeat 是单个行业的当日强度。
type IndustryHeat struct {
	Name          string  `json:"name"`
	AverageChange float64 `json:"average_change"`
	Advancing     int     `json:"advancing"`
	Total         int     `json:"total"`
	// Rank 按平均涨幅降序排列，1 为最强。
	Rank int `json:"rank"`
	// LeaderSymbol 与 LeaderName 是行业内涨幅最高的标的。
	LeaderSymbol string `json:"leader_symbol,omitempty"`
	LeaderName   string `json:"leader_name,omitempty"`
}

// MarketView 是一次市场认知的完整结论，会随计划一起持久化。
type MarketView struct {
	// Evaluated 表示是否真的做过评估。零值（未评估）与"评估后样本不足"
	// 是两种不同情况：未评估不应限制交易，样本不足必须禁止开仓。
	Evaluated bool   `json:"evaluated"`
	Regime    Regime `json:"regime"`
	Label     string `json:"label"`
	// RiskBudgetPercent 是本轮允许的最大总仓位。
	// 它不是风控上限，而是"当前市场值不值得下注"的判断；
	// 最终仓位取它与 RiskRules.MaxTotalExposurePercent 的较小值。
	RiskBudgetPercent float64 `json:"risk_budget_percent"`
	Breadth           Breadth `json:"breadth"`
	// Industries 是行业内强度榜，只保留最强与最弱的若干项，避免提示词过长。
	Industries []IndustryHeat `json:"industries"`
	// Notes 是中文说明，直接进提示词，让模型读到"为什么"而不只是"是什么"。
	Notes   []string  `json:"notes"`
	TakenAt time.Time `json:"taken_at"`
}

// Observation 是认知层的中间结果，保留原始样本量用于诊断。
type Observation struct {
	Breadth    Breadth
	Industries []IndustryHeat
	// Suspended 是无有效价格的标的数，用于提示数据是否完整。
	Suspended int
	// Skipped 是因缺少涨跌幅数据而无法参与统计的标的数。
	Skipped int
}

// limitTolerance 是判定涨跌停的容差（百分点）。
// 交易所按前收盘价四舍五入到分算涨跌停价，因此实际涨幅与名义幅度总有微小偏差：
// 前收 3.01 元的主板股票涨停价为 3.31 元，涨幅 9.97%；前收 3.00 元则为 10.00%。
// 实测偏差不超过 0.05 个百分点，取 0.15 已足够，且不会把 9.7% 误判为涨停。
const limitTolerance = 0.15

// minSampleSize 是判定市场状态所需的最少可交易标的数。
// 低于此数说明数据源异常或处于非交易时段，不应据此下结论。
const minSampleSize = 500

// industryTopN 与 industryBottomN 是行业榜保留的强度。
const (
	industryTopN    = 10
	industryBottomN = 3
)

// minIndustrySize 是进入行业榜的最少成分股数，避免只有两三只股票的小行业霸榜。
const minIndustrySize = 5

// Observe 计算广度与行业强度。这一层不做状态判定，便于单独测试。
func Observe(entries []foundation.StockCatalogEntry) Observation {
	observation := Observation{}
	changes := make([]float64, 0, len(entries))
	totalAmount := 0.0
	industryBuckets := map[string][]foundation.StockCatalogEntry{}

	for _, entry := range entries {
		if entry.Price <= 0 {
			observation.Suspended++
			continue
		}
		observation.Breadth.Total++
		change := entry.ChangePercent
		if math.IsNaN(change) || math.IsInf(change, 0) {
			observation.Skipped++
			continue
		}
		changes = append(changes, change)
		switch {
		case change > 0.005:
			observation.Breadth.Advancing++
		case change < -0.005:
			observation.Breadth.Declining++
		default:
			observation.Breadth.Unchanged++
		}
		percent := marketrule.PriceLimitPercent(entry.Symbol, entry.Name)
		if math.Abs(change-percent) <= limitTolerance {
			observation.Breadth.LimitUp++
		}
		if math.Abs(change+percent) <= limitTolerance {
			observation.Breadth.LimitDown++
		}
		if entry.Amount > 0 {
			observation.Breadth.Tradable++
			totalAmount += entry.Amount
		}
		if industry := strings.TrimSpace(entry.Industry); industry != "" {
			industryBuckets[industry] = append(industryBuckets[industry], entry)
		}
	}

	observation.Breadth.TotalAmount = totalAmount
	observation.Breadth.MedianChange = median(changes)
	observation.Breadth.AverageChange = average(changes)
	denominator := observation.Breadth.Advancing + observation.Breadth.Declining
	if denominator > 0 {
		observation.Breadth.AdvanceRatio = float64(observation.Breadth.Advancing) / float64(denominator)
	}
	observation.Industries = buildIndustryHeat(industryBuckets)
	return observation
}

// Classify 把广度指标翻译成市场状态与仓位预算。
//
// 阈值全部来自可解释的经验规则，而非拟合参数：
// 判定顺序从极端到常态，因为"跌停成片"与"涨停成片"同时出现时，
// 应当按更危险的一侧处理。
func Classify(breadth Breadth) (Regime, float64) {
	if breadth.Total < minSampleSize {
		return RegimeUnknown, 0
	}
	switch {
	case breadth.LimitDown >= 30 || breadth.MedianChange <= -3.0 || breadth.AdvanceRatio <= 0.15:
		return RegimePanic, 20
	case breadth.AdvanceRatio <= 0.35 || (breadth.LimitUp < 15 && breadth.MedianChange < -0.5):
		return RegimeEbb, 40
	case breadth.LimitUp >= 80 && breadth.AdvanceRatio >= 0.70 && breadth.MedianChange >= 1.5:
		// 亢奋的预算刻意低于上行：此时买入成本最高、回撤最快，
		// 线性地"越热越重仓"是最常见的新手错误。
		return RegimeEuphoria, 65
	case breadth.AdvanceRatio >= 0.60 && breadth.MedianChange >= 0.5:
		return RegimeUptrend, 85
	default:
		return RegimeRange, 55
	}
}

// View 合成完整结论，含人话解释。
func View(entries []foundation.StockCatalogEntry, now time.Time) MarketView {
	observation := Observe(entries)
	regime, budget := Classify(observation.Breadth)
	view := MarketView{
		Evaluated:         true,
		Regime:            regime,
		Label:             regime.Label(),
		RiskBudgetPercent: budget,
		Breadth:           observation.Breadth,
		Industries:        observation.Industries,
		TakenAt:           now,
	}
	view.Notes = buildNotes(observation, regime, budget)
	return view
}

// MaxExposurePercent 返回本轮实际可用的总仓位上限：
// 市场预算与风控上限取较小值，市场不好时以市场为准。
func (v MarketView) MaxExposurePercent(ruleLimit float64) float64 {
	if v.RiskBudgetPercent <= 0 {
		return 0
	}
	if ruleLimit > 0 && ruleLimit < v.RiskBudgetPercent {
		return ruleLimit
	}
	return v.RiskBudgetPercent
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func buildIndustryHeat(buckets map[string][]foundation.StockCatalogEntry) []IndustryHeat {
	items := make([]IndustryHeat, 0, len(buckets))
	for name, members := range buckets {
		if len(members) < minIndustrySize {
			continue
		}
		heat := IndustryHeat{Name: name, Total: len(members)}
		sum := 0.0
		leaderChange := math.Inf(-1)
		for _, member := range members {
			sum += member.ChangePercent
			if member.ChangePercent > 0 {
				heat.Advancing++
			}
			if member.ChangePercent > leaderChange {
				leaderChange = member.ChangePercent
				heat.LeaderSymbol = member.Symbol
				heat.LeaderName = member.Name
			}
		}
		heat.AverageChange = sum / float64(len(members))
		items = append(items, heat)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].AverageChange == items[j].AverageChange {
			return items[i].Name < items[j].Name
		}
		return items[i].AverageChange > items[j].AverageChange
	})
	for index := range items {
		items[index].Rank = index + 1
	}
	if len(items) <= industryTopN+industryBottomN {
		return items
	}
	trimmed := make([]IndustryHeat, 0, industryTopN+industryBottomN)
	trimmed = append(trimmed, items[:industryTopN]...)
	trimmed = append(trimmed, items[len(items)-industryBottomN:]...)
	return trimmed
}

func buildNotes(observation Observation, regime Regime, budget float64) []string {
	breadth := observation.Breadth
	notes := make([]string, 0, 5)
	if breadth.Total == 0 {
		return append(notes, "行情快照为空，无法判断市场状态。")
	}
	if breadth.Total < minSampleSize {
		return append(notes, fmt.Sprintf(
			"仅取得 %d 只有效行情，样本不足 %d 只，本次不判断市场状态、不允许开新仓。",
			breadth.Total, minSampleSize))
	}

	notes = append(notes, fmt.Sprintf(
		"全市场 %d 只有效行情（另有 %d 只停牌无价格），上涨 %d 只、下跌 %d 只、平盘 %d 只，上涨占比 %.1f%%。",
		breadth.Total, observation.Suspended, breadth.Advancing, breadth.Declining, breadth.Unchanged, breadth.AdvanceRatio*100))
	notes = append(notes, fmt.Sprintf(
		"涨停 %d 家、跌停 %d 家，涨幅中位数 %+.2f%%、平均 %+.2f%%，成交额合计 %.0f 亿元。",
		breadth.LimitUp, breadth.LimitDown, breadth.MedianChange, breadth.AverageChange, breadth.TotalAmount/1e8))

	if len(observation.Industries) > 0 {
		leaders := observation.Industries
		if len(leaders) > industryTopN {
			leaders = leaders[:industryTopN]
		}
		parts := make([]string, 0, len(leaders))
		for _, item := range leaders {
			parts = append(parts, fmt.Sprintf("%s %+.2f%%（%d/%d 上涨）", item.Name, item.AverageChange, item.Advancing, item.Total))
		}
		notes = append(notes, "行业强度前列："+strings.Join(parts, "、")+"。")
		if len(observation.Industries) > industryBottomN && len(observation.Industries) > industryTopN {
			weak := observation.Industries[len(observation.Industries)-industryBottomN:]
			weakParts := make([]string, 0, len(weak))
			for _, item := range weak {
				weakParts = append(weakParts, fmt.Sprintf("%s %+.2f%%", item.Name, item.AverageChange))
			}
			notes = append(notes, "行业强度末位："+strings.Join(weakParts, "、")+"。")
		}
	}

	notes = append(notes, fmt.Sprintf(
		"当前判定为「%s」，本轮权益仓位上限 %.0f%%。%s", regime.Label(), budget, regimeAdvice(regime)))
	return notes
}

func regimeAdvice(regime Regime) string {
	switch regime {
	case RegimePanic:
		return "市场处于无差别抛售，任何买入都是逆势，只允许减仓与止损，不开新仓。"
	case RegimeEbb:
		return "赚钱效应消退，多数标的下注期望为负，只做最有把握的少数机会并收紧止损。"
	case RegimeRange:
		return "方向不明，适合轻仓试探与区间操作，不做趋势假设。"
	case RegimeUptrend:
		return "赚钱效应扩散，可正常建仓，但仍需逐笔设置止损，不因普涨而放宽标准。"
	case RegimeEuphoria:
		return "情绪过热，买入成本最高、回撤最快，仓位应低于上行阶段，且不追涨停板。"
	default:
		return "数据不足，不判断方向。"
	}
}
