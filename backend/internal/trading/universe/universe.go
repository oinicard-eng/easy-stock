// Package universe 从全市场快照中筛出交给模型的候选池。
//
// 三条原则：
//
//  1. 先排除再打分。流动性不足、涨停买不进、ST 这类标的无论涨得多好都不该进候选，
//     打分会掩盖它们的缺陷。
//  2. 每一分都能解释。候选不是一串代码，而是"为什么是它"：模型读到的是附理由的候选，
//     人也能核对系统有没有乱来。
//  3. 明确记录排除了什么。只给结果不给原因，用户会觉得"我的股票怎么没被选中"，
//     进而不再信任系统；Exclusions 就是回答这个问题的地方。
package universe

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/marketrule"
)

// Candidate 是一个进入候选池的标的，含全部入选依据。
type Candidate struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Industry string `json:"industry,omitempty"`
	// Sector 是细分行业归并后的大类，说明分散依据。
	Sector               string  `json:"sector,omitempty"`
	Price                float64 `json:"price"`
	ChangePercent        float64 `json:"change_percent"`
	FiveDayChangePercent float64 `json:"five_day_change_percent"`
	// Amount 是当日成交额，单位元。
	Amount float64 `json:"amount"`
	// Score 是 0~100 的合成分，只用于排序，不代表预期收益。
	Score float64 `json:"score"`
	Rank  int     `json:"rank"`
	// Reasons 是入选依据，每条都带具体数字，直接进提示词。
	Reasons []string `json:"reasons"`
}

// Exclusion 记录一类被排除的标的及数量。
type Exclusion struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	// Samples 保留少量样本（代码+名称），便于人工核对规则是否过严。
	Samples []string `json:"samples,omitempty"`
}

// Board 是一次候选池构建的完整结果。
type Board struct {
	TakenAt time.Time `json:"taken_at"`
	// SourceCount 是全市场快照规模，用于判断这次"看得全不全"。
	SourceCount int `json:"source_count"`
	// ThresholdAmount 是本次实际生效的成交额门槛（元），可能因缩量而降级。
	ThresholdAmount float64 `json:"threshold_amount"`
	// Degraded 表示门槛被降低过，说明当日市场流动性低于常态。
	Degraded bool `json:"degraded"`
	// Capped 表示因分散约束未能选满 Limit。宁可少选，也不放松分散。
	Capped     bool                 `json:"capped"`
	Candidates []Candidate          `json:"candidates"`
	Exclusions []Exclusion          `json:"exclusions"`
	Market     cognition.MarketView `json:"market"`
}

// Symbols 返回候选池的代码列表，保持排序。
func (b Board) Symbols() []string {
	symbols := make([]string, 0, len(b.Candidates))
	for _, candidate := range b.Candidates {
		symbols = append(symbols, candidate.Symbol)
	}
	return symbols
}

// Options 控制筛选强度。
type Options struct {
	// Limit 是候选池目标数量，默认 30。
	Limit int
	// MinAmount 是成交额门槛（元）。缩量时按 amountFallbacks 逐级放宽。
	MinAmount float64
	// MinPrice 是低价股门槛（元），用于剔除面值与退市风险标的。
	MinPrice float64
	// ExcludeBoards 是要整体排除的板块名称，取值 "BJ"（北交所）。
	ExcludeBoards []string
	// Equity 是账户总资产。大于 0 时剔除"买一手就占掉大半个仓位"的高价股：
	// 10 万资金买 500 元的股票，一手就是 5 万，所谓"小仓位试探"根本做不到。
	// 为 0 时跳过该约束（仅用于纯选股预览）。
	Equity float64
	// MaxLotRatio 是单手金额占总资产的上限，默认 0.25。
	MaxLotRatio float64
	// MaxPerIndustry 是同一行业最多入选数，默认 3。
	MaxPerIndustry int
	// MaxPerBoardRatio 是同一板块最多占比，默认 0.45。
	MaxPerBoardRatio float64
}

// DefaultOptions 是经过取舍的默认参数。
func DefaultOptions() Options {
	return Options{
		Limit:            30,
		MinAmount:        5000 * 1e4,
		MinPrice:         2,
		ExcludeBoards:    []string{"BJ"},
		MaxLotRatio:      0.25,
		MaxPerIndustry:   3,
		MaxPerBoardRatio: 0.45,
	}
}

// amountFallbacks 是成交额门槛的降级序列。
// 固定门槛在缩量行情（节前、极端缩量）会把全市场筛空，此时"没有候选"
// 并不代表"没有机会"，只代表门槛不合适。逐级放宽比直接失败更有用。
var amountFallbacks = []float64{5000 * 1e4, 3000 * 1e4, 1500 * 1e4, 500 * 1e4}

// 打分权重，合计 100。
const (
	weightMomentum         = 30
	weightTrend            = 25
	weightLiquidity        = 20
	weightIndustry         = 15
	weightRelativeStrength = 10
)

// Build 构建候选池。市场状态为 panic 或 unknown 时返回空池——
// 这不是筛选问题，而是当前不该开新仓。
func Build(entries []foundation.StockCatalogEntry, market cognition.MarketView, options Options) Board {
	if options.Limit <= 0 {
		options.Limit = DefaultOptions().Limit
	}
	if options.MinPrice <= 0 {
		options.MinPrice = DefaultOptions().MinPrice
	}
	if len(options.ExcludeBoards) == 0 {
		options.ExcludeBoards = DefaultOptions().ExcludeBoards
	}
	if options.MaxLotRatio <= 0 {
		options.MaxLotRatio = DefaultOptions().MaxLotRatio
	}
	if options.MaxPerIndustry <= 0 {
		options.MaxPerIndustry = DefaultOptions().MaxPerIndustry
	}
	if options.MaxPerBoardRatio <= 0 {
		options.MaxPerBoardRatio = DefaultOptions().MaxPerBoardRatio
	}

	board := Board{
		TakenAt: time.Now().UTC(),
		SourceCount: len(entries),
		Market:      market,
		// 无候选也要是空数组而不是 null：前端会对 candidates.length 取值，
		// JSON null 会在浏览器里变成一整页白屏（2026-10-21 实测事故）。
		Candidates: []Candidate{},
	}
	if !market.Regime.Valid() {
		board.Exclusions = []Exclusion{{Reason: "市场状态为「" + market.Label + "」，本轮不开新仓", Count: len(entries)}}
		return board
	}

	excludedBoards := map[string]bool{}
	for _, name := range options.ExcludeBoards {
		excludedBoards[strings.ToUpper(strings.TrimSpace(name))] = true
	}

	industryRank := map[string]cognition.IndustryHeat{}
	for _, heat := range market.Industries {
		industryRank[heat.Name] = heat
	}
	// industryTotal 用快照里出现过的全部行业数估计，用于把名次折算成分位。
	industryTotal := 0
	seenIndustry := map[string]bool{}
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Industry); name != "" && !seenIndustry[name] {
			seenIndustry[name] = true
			industryTotal++
		}
	}

	// 第一轮：硬性排除。这些标的无论涨幅多好都不该进候选。
	passed := make([]foundation.StockCatalogEntry, 0, len(entries))
	counter := newExclusionCounter()
	for _, entry := range entries {
		switch {
		case entry.Price <= 0:
			counter.add("停牌或无有效价格", entry)
			continue
		case isRiskWarned(entry.Name):
			counter.add("ST 或退市风险警示", entry)
			continue
		case entry.Price < options.MinPrice:
			counter.add("股价低于 "+trimFloat(options.MinPrice)+" 元", entry)
			continue
		case options.Equity > 0 && options.MaxLotRatio > 0 &&
			float64(marketrule.MinBoardLot(entry.Symbol))*entry.Price > options.Equity*options.MaxLotRatio:
			// 买一手就吃掉四分之一仓位的高价股，对小资金账户等于没有仓位管理可言。
			counter.add(fmt.Sprintf("单手金额超过总资产的 %.0f%%，无法小仓位试探", options.MaxLotRatio*100), entry)
			continue
		case excludedBoards[boardOf(entry.Symbol)]:
			counter.add("板块不在可选范围（北交所）", entry)
			continue
		}
		percent := marketrule.PriceLimitPercent(entry.Symbol, entry.Name)
		change := entry.ChangePercent
		if math.Abs(change-percent) <= 0.35 {
			counter.add("已涨停或接近涨停，无法从容买入", entry)
			continue
		}
		if math.Abs(change+percent) <= 0.35 {
			counter.add("已跌停或接近跌停，不接下落的刀", entry)
			continue
		}
		passed = append(passed, entry)
	}

	// 第二轮：成交额门槛，必要时逐级降级。
	threshold := options.MinAmount
	if threshold <= 0 {
		threshold = amountFallbacks[0]
	}
	var liquid []foundation.StockCatalogEntry
	for _, candidate := range amountFallbacks {
		if candidate > threshold && threshold > 0 {
			continue
		}
		liquid = liquid[:0]
		for _, entry := range passed {
			if entry.Amount >= candidate {
				liquid = append(liquid, entry)
			}
		}
		threshold = candidate
		if len(liquid) >= options.Limit {
			break
		}
	}
	if threshold < amountFallbacks[0] {
		board.Degraded = true
	}
	board.ThresholdAmount = threshold
	for _, entry := range passed {
		if entry.Amount < threshold {
			counter.add("成交额低于 "+trimFloatWan(threshold)+" 万元，流动性不足", entry)
		}
	}
	if len(liquid) == 0 {
		board.Exclusions = counter.list()
		return board
	}

	// 第三轮：打分。成交额用对数刻度，因为其分布极度右偏（几千万到几百亿）。
	scores := make([]float64, len(liquid))
	maxLogAmount := 0.0
	minLogAmount := math.Inf(1)
	for _, entry := range liquid {
		value := math.Log10(math.Max(entry.Amount, 1))
		if value > maxLogAmount {
			maxLogAmount = value
		}
		if value < minLogAmount {
			minLogAmount = value
		}
	}
	for index, entry := range liquid {
		heat := industryRank[entry.Industry]
		scores[index] = scoreOf(entry, heat, industryTotal, minLogAmount, maxLogAmount)
	}

	order := make([]int, len(liquid))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool {
		if scores[order[i]] == scores[order[j]] {
			return liquid[order[i]].Amount > liquid[order[j]].Amount
		}
		return scores[order[i]] > scores[order[j]]
	})

	// 第四轮：分散约束。按分数从高到低选，但同行业与同板块设上限。
	//
	// 这一步是必要的，不是锦上添花。实测某日按纯分数排序，15 只候选里 11 只是科创板，
	// 且集中在半导体与机器人——看似分散在 15 个标的上，实际是同一个风险敞口，
	// 板块回调时会一起跌。真实交易员的分散不是数代码个数，而是数独立风险来源。
	//
	// 代价是可能选入分数略低的标的。这个取舍是值得的：组合亏损的主因从来不是
	// "少赚了几个点"，而是"以为分散了、其实全押在一个方向上"。
	maxPerBoard := options.Limit
	if options.MaxPerBoardRatio > 0 && options.MaxPerBoardRatio < 1 {
		maxPerBoard = int(math.Ceil(float64(options.Limit) * options.MaxPerBoardRatio))
		if maxPerBoard < 1 {
			maxPerBoard = 1
		}
	}
	industryCount := map[string]int{}
	boardCount := map[string]int{}
	selected := make([]int, 0, options.Limit)
	for _, index := range order {
		entry := liquid[index]
		// 按大类计数，而不是按数据源给的细分行业名：后者会让
		// 「半导体 3 只 + 电子元件 3 只 + 消费电子 3 只」通过约束，
		// 实际全押在同一个电子周期上。
		sector := sectorOf(entry.Industry)
		if sector != "" && options.MaxPerIndustry > 0 && industryCount[sector] >= options.MaxPerIndustry {
			counter.add("同类风险敞口已达上限（"+sector+"）", entry)
			continue
		}
		boardName := boardOf(entry.Symbol)
		if boardCount[boardName] >= maxPerBoard {
			counter.add("同板块候选已达上限（避免单一风险）", entry)
			continue
		}
		if sector != "" {
			industryCount[sector]++
		}
		boardCount[boardName]++
		selected = append(selected, index)
		if len(selected) >= options.Limit {
			break
		}
	}
	if len(selected) < options.Limit {
		board.Capped = true
	}
	order = selected

	candidates := make([]Candidate, 0, len(order))
	for rank, index := range order {
		entry := liquid[index]
		heat, hasHeat := industryRank[entry.Industry]
		candidates = append(candidates, Candidate{
			Symbol:               entry.Symbol,
			Name:                 entry.Name,
			Industry:             entry.Industry,
			Sector:               sectorOf(entry.Industry),
			Price:                entry.Price,
			ChangePercent:        entry.ChangePercent,
			FiveDayChangePercent: entry.FiveDayChangePercent,
			Amount:               entry.Amount,
			Score:                round1(scores[index]),
			Rank:                 rank + 1,
			Reasons:              reasonsOf(entry, heat, hasHeat, industryTotal),
		})
	}
	board.Candidates = candidates
	board.Exclusions = counter.list()
	return board
}

// scoreOf 计算 0~100 的合成分。每个维度都做了"过热惩罚"，
// 因为线性追涨会把仓位集中到已经涨完的标的上。
func scoreOf(entry foundation.StockCatalogEntry, heat cognition.IndustryHeat, industryTotal int, minLog, maxLog float64) float64 {
	momentum := momentumScore(entry.ChangePercent, marketrule.PriceLimitPercent(entry.Symbol, entry.Name))
	trend := trendScore(entry.FiveDayChangePercent)
	liquidity := liquidityScore(math.Log10(math.Max(entry.Amount, 1)), minLog, maxLog)
	industry := industryScore(heat, industryTotal)
	relative := relativeScore(entry.ChangePercent, heat)

	total := momentum*weightMomentum + trend*weightTrend + liquidity*weightLiquidity +
		industry*weightIndustry + relative*weightRelativeStrength
	return total / 100
}

// momentumScore 刻意做成倒 U 型：当日涨幅 3%~6% 为最优区间，之后逐级扣分。
// 涨停板买不进，接近涨停时买入成本也最差。
//
// 扣分必须平滑。曾经写成"超过涨停幅度的 60% 直接归零"，结果是主板涨 6.01%
// 的股票得分 15、涨 0.8% 的得分 48 —— 涨得多反而被当成利空，与直觉相反。
// 现在按"距离涨停还有多远"线性递减，6% 为满分、涨停位为 30 分。
func momentumScore(change, limitPercent float64) float64 {
	switch {
	case change < -5:
		return 5
	case change < 0:
		return 20 + (change+5)*4 // -5% → 20，0% → 40
	case change <= 6:
		return 40 + change*10 // 0% → 40，6% → 100
	case change <= limitPercent:
		span := limitPercent - 6
		if span <= 0 {
			return 30
		}
		return 100 - (change-6)/span*70 // 6% → 100，涨停位 → 30
	default:
		return 30
	}
}

// trendScore 偏好"已经启动但未走完"的中期趋势。
// 5 日涨幅超过 25% 视为短期透支，开始扣分。
func trendScore(fiveDay float64) float64 {
	switch {
	case fiveDay < -10:
		return 5
	case fiveDay < 0:
		return 30 + fiveDay // -10% → 20，0% → 30
	case fiveDay <= 15:
		return 55 + fiveDay*3 // 0% → 55，15% → 100
	case fiveDay <= 25:
		return 100 - (fiveDay-15)*3 // 15% → 100，25% → 70
	default:
		return math.Max(20, 70-(fiveDay-25)*2)
	}
}

// liquidityScore 把对数成交额线性映射到 0~100。
// 用对数是因为成交额从几千万到几百亿跨越三个数量级，
// 线性映射会让所有标的挤在低分区，失去区分度。
func liquidityScore(logAmount, minLog, maxLog float64) float64 {
	if maxLog <= minLog {
		return 60
	}
	ratio := (logAmount - minLog) / (maxLog - minLog)
	return 40 + ratio*60
}

// industryScore 用行业中平均涨幅的分位给分：第 1 名 100，末位 20。
func industryScore(heat cognition.IndustryHeat, industryTotal int) float64 {
	if heat.Rank <= 0 || industryTotal <= 1 {
		return 50
	}
	ratio := 1 - float64(heat.Rank-1)/float64(industryTotal-1)
	return 20 + ratio*80
}

// relativeScore 衡量"是否跑赢自己的行业"，识别板块内龙头。
func relativeScore(change float64, heat cognition.IndustryHeat) float64 {
	if heat.Name == "" {
		return 50
	}
	diff := change - heat.AverageChange
	switch {
	case diff >= 5:
		return 100
	case diff >= 0:
		return 55 + diff*9
	case diff >= -5:
		return 55 + diff*7
	default:
		return 10
	}
}

func reasonsOf(entry foundation.StockCatalogEntry, heat cognition.IndustryHeat, hasHeat bool, industryTotal int) []string {
	reasons := make([]string, 0, 4)
	percent := marketrule.PriceLimitPercent(entry.Symbol, entry.Name)

	switch {
	case entry.ChangePercent >= 0 && entry.ChangePercent <= 6:
		reasons = append(reasons, "当日 "+signed(entry.ChangePercent)+"，处于启动区间（未过热）")
	case entry.ChangePercent > 6:
		reasons = append(reasons, "当日 "+signed(entry.ChangePercent)+"，已明显拉升，涨停幅度 "+trimFloat(percent)+"%，追高风险上升")
	default:
		reasons = append(reasons, "当日 "+signed(entry.ChangePercent)+"，逆势或回调")
	}

	switch {
	case entry.FiveDayChangePercent > 25:
		reasons = append(reasons, "5 日 "+signed(entry.FiveDayChangePercent)+"，短期涨幅透支")
	case entry.FiveDayChangePercent > 0:
		reasons = append(reasons, "5 日 "+signed(entry.FiveDayChangePercent)+"，中期趋势向上")
	default:
		reasons = append(reasons, "5 日 "+signed(entry.FiveDayChangePercent)+"，中期趋势偏弱")
	}

	reasons = append(reasons, "成交额 "+trimFloatYi(entry.Amount)+"，现价 "+trimFloat(entry.Price)+" 元")
	if hasHeat {
		reasons = append(reasons, "行业「"+heat.Name+"」平均 "+signed(heat.AverageChange)+
			"，行业强度第 "+trimInt(heat.Rank)+"/"+trimInt(industryTotal)+" 名")
	}
	return reasons
}

// sectorOf 把细分行业归入大类，用于分散约束。
//
// 为什么需要这一步：数据源给的行业名很细，实测同一天的前 12 只候选分属
// 「半导体、电子元件、消费电子设备、电子设备制造、计算机软件」五个不同行业，
// 按"同行业最多 3 只"逐个计数时约束形同虚设——它们其实是同一个电子周期，
// 一起涨也一起跌。分散要数的是独立风险来源，不是行业名的个数。
//
// 归并刻意粗放：宁可把两个弱相关的行业并成一类（少选几只），
// 也不要把同一个风险拆成两类（以为分散了、其实没有）。
func sectorOf(industry string) string {
	name := strings.TrimSpace(industry)
	if name == "" {
		return ""
	}
	// 顺序有意义：先匹配更具体的类别，避免"汽车电子"被归进"电子"这类歧义。
	groups := []struct {
		sector   string
		keywords []string
	}{
		{"电子", []string{"半导体", "芯片", "集成电路", "电子元件", "电子设备", "消费电子", "光学", "光电子", "面板", "PCB", "印制电路", "被动元件", "分立器件", "电子器件", "电子制造"}},
		{"计算机", []string{"计算机", "软件", "互联网", "信息安全", "云计算", "IT服务", "通信设备", "通信服务", "数据中心"}},
		{"医药", []string{"医药", "制药", "生物", "医疗", "中药", "疫苗", "CXO", "化学制剂", "原料药"}},
		{"电力设备", []string{"电池", "光伏", "风电", "储能", "电网", "电源", "电气", "新能源", "电机"}},
		{"机械", []string{"机械", "通用设备", "专用设备", "自动化", "机器人", "机床", "仪器", "工程机械"}},
		{"汽车", []string{"汽车", "整车", "零部件", "轮胎", "乘用车", "商用车"}},
		{"食品农业", []string{"食品", "饮料", "白酒", "啤酒", "乳品", "调味", "农产品", "养殖", "种植", "饲料", "农牧", "林业", "渔业"}},
		{"消费", []string{"家电", "家居", "纺织", "服装", "零售", "商贸", "旅游", "酒店", "餐饮", "美容", "教育", "家具", "家用轻工", "文娱用品"}},
		{"金融", []string{"银行", "证券", "保险", "多元金融", "信托"}},
		{"周期", []string{"煤炭", "钢铁", "有色", "金属", "化工", "化学", "建材", "水泥", "石油", "燃气", "玻璃", "造纸"}},
		{"地产建筑", []string{"房地产", "建筑", "工程", "装修", "装饰"}},
		{"传媒", []string{"传媒", "游戏", "影视", "出版", "广告", "文化"}},
		{"公用事业", []string{"电力", "水务", "环保", "公用", "供热"}},
		{"军工", []string{"国防", "军工", "航天", "航空", "兵器", "船舶"}},
		{"交运", []string{"交通", "物流", "航运", "港口", "铁路", "快递", "机场"}},
	}
	for _, group := range groups {
		for _, keyword := range group.keywords {
			if strings.Contains(name, keyword) {
				return group.sector
			}
		}
	}
	// 未能归类时退回原行业名：宁可少分散，也不要把无关行业强行合并。
	return name
}

// 名称里带 ST 的一律排除：涨跌停仅 5%，流动性差且退市风险不可用技术面衡量。
func isRiskWarned(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.Contains(upper, "ST") || strings.Contains(name, "退")
}

// boardOf 返回标的所属板块：SH / SZ / BJ。
func boardOf(symbol string) string {
	upper := strings.ToUpper(strings.TrimSpace(symbol))
	if index := strings.LastIndex(upper, "."); index >= 0 && index+1 < len(upper) {
		return upper[index+1:]
	}
	// 无后缀时按代码前缀推断。
	code := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(upper, "SH"), "SZ"), "BJ")
	switch {
	case strings.HasPrefix(code, "4"), strings.HasPrefix(code, "8"), strings.HasPrefix(code, "92"):
		return "BJ"
	case strings.HasPrefix(code, "6"), strings.HasPrefix(code, "9"):
		return "SH"
	default:
		return "SZ"
	}
}

// exclusionCounter 按原因聚合被排除的标的，并保留少量样本。
type exclusionCounter struct {
	order   []string
	counts  map[string]int
	samples map[string][]string
}

const maxSamplesPerReason = 3

func newExclusionCounter() *exclusionCounter {
	return &exclusionCounter{counts: map[string]int{}, samples: map[string][]string{}}
}

func (c *exclusionCounter) add(reason string, entry foundation.StockCatalogEntry) {
	if _, ok := c.counts[reason]; !ok {
		c.order = append(c.order, reason)
	}
	c.counts[reason]++
	if len(c.samples[reason]) < maxSamplesPerReason {
		c.samples[reason] = append(c.samples[reason], entry.Symbol+" "+entry.Name)
	}
}

func (c *exclusionCounter) list() []Exclusion {
	items := make([]Exclusion, 0, len(c.order))
	for _, reason := range c.order {
		items = append(items, Exclusion{Reason: reason, Count: c.counts[reason], Samples: c.samples[reason]})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Count > items[j].Count })
	return items
}

func signed(value float64) string {
	if value >= 0 {
		return "+" + trimFloat(value) + "%"
	}
	return trimFloat(value) + "%"
}

func trimFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func trimFloatWan(value float64) string {
	return trimFloat(value / 1e4)
}

func trimFloatYi(value float64) string {
	if value >= 1e8 {
		return trimFloat(round1(value/1e8)) + " 亿"
	}
	return trimFloat(round1(value/1e4)) + " 万"
}

func trimInt(value int) string {
	return strconv.Itoa(value)
}

func round1(value float64) float64 { return math.Round(value*10) / 10 }

// SectorOf 把细分行业名归入大类，供账户层的行业集中度守卫复用。
//
// 导出的理由：选股与风控必须用同一套归并口径。若选股按 16 大类分散、
// 风控按细分行业判断集中度，两者会各自"看起来合理"，合起来却是漏洞。
func SectorOf(industry string) string { return sectorOf(industry) }
