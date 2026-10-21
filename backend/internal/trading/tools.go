package trading

// 受限只读工具集：让模型在出计划之前可以自己查数据。
//
// 为什么是"受限"而不是把 Hermes 的全部工具打开：
//  1. 交易计划必须是可回放的。开放的 shell/MCP 工具会引入外部状态，
//     同一个提示词两次运行可能看到不同的世界，回测与追责都失去基准。
//  2. 工具必须是只读的。一个能写文件、能发请求的模型，在交易链路上
//     等价于一个没有审计的运营权限。
//  3. 每个工具的输出都要落库。模型"看了什么才做出这个决定"是复盘的第一手材料，
//     只把结论写进计划等于把推理过程扔掉了。
//
// 所以这里定义的是白名单：十二个纯读工具，参数严格校验，输出有行数上限，
// 数据源全部来自本轮快照与现有 provider，不引入任何新的外部依赖。
// 其中七个只看价格结构（资金流、行业强度、成分、日线、详情、对比、记忆），
// 五个回答"为什么涨"（新闻、公告、龙虎榜、大盘、两融，见 tools_market.go）。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/universe"
)

// SectorFlowProvider 是板块资金流能力。没有注入时对应工具会明确报"数据源未配置"，
// 而不是返回一份编造的榜单。
type SectorFlowProvider interface {
	SectorFlowRanking(ctx context.Context, dimension string, limit int) ([]foundation.SectorFlowRank, foundation.SourceMeta, error)
}

// KLineProvider 是日线数据能力。没有注入时 K 线工具会明确报"数据源未配置"，
// 而不是拿当日快照拼一条假曲线。
type KLineProvider interface {
	KLine(ctx context.Context, symbol string, period string, limit int) ([]foundation.KLine, error)
}

// ToolParam 描述一个工具参数，用于生成提示词里的调用说明。
type ToolParam struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Desc     string `json:"desc"`
}

// ToolSpec 是给模型看的工具说明书。
type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Params      []ToolParam `json:"params"`
}

// ToolOutcome 是一次工具调用的结果。
type ToolOutcome struct {
	// Text 是给模型读的紧凑文本，带表头，行数受上限约束。
	Text string `json:"text"`
	// Data 是原始结构，落库用于回放。
	Data any `json:"data,omitempty"`
	// Rows 是实际返回行数，用于回放时判断模型看到的信息量。
	Rows int `json:"rows"`
	// Truncated 表示结果被截断。模型需要知道"我没看全"，否则会把 top20 当成全部。
	Truncated bool `json:"truncated,omitempty"`
}

// Toolbox 是一次计划生成的工具上下文。它持有本轮快照，所以工具看到的
// 数据与提示词里的行情表严格同源——不会出现"表里 10.2 元、工具里 10.5 元"。
type Toolbox struct {
	store      *Store
	accountID  string
	snapshot   Snapshot
	catalog    map[string]foundation.StockCatalogEntry
	sectorFlow SectorFlowProvider
	// klines 是日线来源。K 线让模型能看到"位置"（均线、区间分位、波动率），
	// 而当日快照只能看到"今天涨了多少"——后者不足以判断一笔交易的风险回报。
	klines   KLineProvider
	klineAlt KLineProvider
	// marketIntel / news 回答的是"为什么涨"：公告、龙虎榜、大盘、两融、新闻。
	// 只有价格结构时，模型写出的理由只能是价格的同义反复。
	marketIntel MarketIntelProvider
	// emotion 是市场情绪周期（人气阶段）。它回答"现在市场冷不冷"，
	// 与 marketIntel 的指数涨跌是两个维度：指数可能微涨而人气已在退潮。
	emotion EmotionProvider
	news    NewsSearchProvider
	now     time.Time
}

const (
	// toolMaxRows 是单个工具返回的最大行数。
	// 再多模型也读不完，只会稀释注意力——这一点与提示词里 quotes 上限同理。
	toolMaxRows = 20
	// toolMaxTextChars 是单个工具结果的字符上限，防止某个宽表把上下文挤爆。
	toolMaxTextChars = 2600
)

// Specs 返回全部工具说明。顺序稳定，保证提示词可复现。
func (t *Toolbox) Specs() []ToolSpec {
	return append([]ToolSpec{
		{
			Name:        "list_sector_flow",
			Description: "板块资金流排行（主力净流入、净占比、5日/10日累计）。用于判断资金正在流入还是撤离哪个方向。",
			Params: []ToolParam{
				{Name: "dimension", Type: "string", Desc: "concept 或 industry，默认 concept"},
				{Name: "limit", Type: "int", Desc: "返回条数，默认 10，最多 20"},
			},
		},
		{
			Name:        "list_sector_heat",
			Description: "行业强度榜（平均涨幅、上涨家数、龙头股、名次）。数据来自本轮全市场快照。",
			Params: []ToolParam{
				{Name: "limit", Type: "int", Desc: "返回条数，默认 10，最多 20"},
			},
		},
		{
			Name:        "list_sector_stocks",
			Description: "某个行业或板块大类的成分股表现。可按涨跌幅排序，用于看板块内部是普涨还是只有龙头在涨。",
			Params: []ToolParam{
				{Name: "sector", Type: "string", Required: true, Desc: "行业名或板块大类名"},
				{Name: "limit", Type: "int", Desc: "返回条数，默认 10，最多 20"},
			},
		},
		{
			Name:        "get_kline",
			Description: "个股日线与技术位置：MA5/10/20/60、ATR14 及其占现价比例、20 日区间分位、近 5 日与 20 日均量比、连续上涨/下跌天数。用于判断趋势位置与波动大小。days 传 250 可看一年分位、传 750 可看三年分位，适合回答『这个价位在历史上算高还是低』。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Required: true, Desc: "6 位代码，如 600519"},
				{Name: "days", Type: "int", Desc: "回看交易日数，默认 60，最多 750"},
			},
		},
		{
			Name:        "get_symbol_detail",
			Description: "单只标的的完整快照：价格区间、成交额、5日涨幅、所属行业、是否在候选池及入选理由、是否持仓及防守线。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Required: true, Desc: "6 位代码，如 600519"},
			},
		},
		{
			Name:        "compare_symbols",
			Description: "多只标的横向对比，并给出各自相对所属行业中位数的强弱。用于在几个候选之间做取舍。",
			Params: []ToolParam{
				{Name: "symbols", Type: "string[]", Required: true, Desc: "2~6 个代码"},
			},
		},
		{
			Name:        "recall_trades",
			Description: "检索历史平仓记忆：当时市场状态、买入理由、峰值浮盈、离场原因与结果。用于回答『这类情况以前发生过什么』。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Desc: "按代码过滤"},
				{Name: "sector", Type: "string", Desc: "按板块大类过滤"},
				{Name: "regime", Type: "string", Desc: "panic/ebb/range/uptrend/euphoria"},
				{Name: "trigger", Type: "string", Desc: "stop_loss/take_profit/trail_stop/model_exit"},
				{Name: "outcome", Type: "string", Desc: "win/loss/flat"},
				{Name: "keyword", Type: "string", Desc: "在买入理由与教训里搜关键词"},
				{Name: "limit", Type: "int", Desc: "返回条数，默认 6，最多 12"},
			},
		},
	}, marketIntelSpecs()...)
}

// Call 执行一个工具。未知工具名返回错误，模型看到错误后可以自行改正。
func (t *Toolbox) Call(ctx context.Context, name string, args map[string]any) (ToolOutcome, error) {
	switch name {
	case "list_sector_flow":
		return t.listSectorFlow(ctx, args)
	case "list_sector_heat":
		return t.listSectorHeat(args)
	case "list_sector_stocks":
		return t.listSectorStocks(args)
	case "get_kline":
		return t.getKline(ctx, args)
	case "get_symbol_detail":
		return t.getSymbolDetail(args)
	case "compare_symbols":
		return t.compareSymbols(args)
	case "recall_trades":
		return t.recallTrades(ctx, args)
	case "search_news":
		return t.searchNews(ctx, args)
	case "get_announcements":
		return t.getAnnouncements(ctx, args)
	case "get_billboard":
		return t.getBillboard(ctx, args)
	case "get_market_context":
		return t.getMarketContext(ctx, args)
	case "get_margin_balance":
		return t.getMarginBalance(ctx, args)
	default:
		return ToolOutcome{}, fmt.Errorf("没有名为 %q 的工具，可用工具：%s", name, toolNames(t.Specs()))
	}
}

func toolNames(specs []ToolSpec) string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return strings.Join(names, ", ")
}

// ── 工具实现 ──────────────────────────────────────────────

func (t *Toolbox) listSectorFlow(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.sectorFlow == nil {
		return ToolOutcome{}, fmt.Errorf("板块资金流数据源未配置，无法查询")
	}
	dimension := strings.ToLower(strings.TrimSpace(stringArg(args, "dimension")))
	// 中文别名必须接受。实测模型会写 dimension:"行业"，
	// 严格拒绝的代价是白耗一步调研预算——一步就是一次模型往返。
	switch dimension {
	case "", "concept", "概念", "概念板块":
		dimension = "concept"
	case "industry", "行业", "行业板块":
		dimension = "industry"
	}
	if dimension != "concept" && dimension != "industry" {
		return ToolOutcome{}, fmt.Errorf("dimension 只能是 concept 或 industry，收到 %q", dimension)
	}
	limit := intArg(args, "limit", 10, 1, toolMaxRows)

	callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	rows, meta, err := t.sectorFlow.SectorFlowRanking(callCtx, dimension, limit)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("板块资金流查询失败：%w", err)
	}
	if len(rows) == 0 {
		return ToolOutcome{Text: "板块资金流返回空列表（可能是非交易时段或上游无数据）。", Data: rows}, nil
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "板块资金流（%s，来源 %s，抓取 %s）：\n", dimension, firstNonEmpty(meta.Source, "未知"), meta.FetchedAt.In(cstZone()).Format("15:04:05"))
	builder.WriteString("名称 | 代码 | 涨跌% | 主力净流入 | 净占比% | 5日主力 | 10日主力\n")
	for _, row := range rows {
		fmt.Fprintf(&builder, "%s | %s | %.2f | %s | %.2f | %s | %s\n",
			row.Name, row.Code, row.ChangePercent,
			moneyText(row.MainNetInflow), row.MainNetInflowRatio,
			moneyText(row.FiveDayMainNet), moneyText(row.TenDayMainNet))
	}
	if meta.Stale {
		builder.WriteString("注意：该数据被上游标记为延迟。\n")
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: rows, Rows: len(rows)}, nil
}

func (t *Toolbox) listSectorHeat(args map[string]any) (ToolOutcome, error) {
	limit := intArg(args, "limit", 10, 1, toolMaxRows)
	industries := t.snapshot.Market.Industries
	if len(industries) == 0 {
		return ToolOutcome{Text: "本轮快照没有行业强度数据（市场认知未评估或样本不足）。"}, nil
	}
	sorted := make([]cognition.IndustryHeat, 0, len(industries))
	for _, item := range industries {
		sorted = append(sorted, item)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		// Rank 为 0 表示无排名，沉底。
		left, right := sorted[i].Rank, sorted[j].Rank
		if left == 0 {
			left = math.MaxInt32
		}
		if right == 0 {
			right = math.MaxInt32
		}
		return left < right
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}

	var builder strings.Builder
	builder.WriteString("行业强度榜（按名次）：\n")
	builder.WriteString("名次 | 行业 | 平均涨幅% | 上涨/总数 | 龙头\n")
	for _, item := range sorted {
		fmt.Fprintf(&builder, "%d | %s | %.2f | %d/%d | %s\n",
			item.Rank, item.Name, item.AverageChange, item.Advancing, item.Total,
			firstNonEmpty(item.LeaderName, "—"))
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: sorted, Rows: len(sorted)}, nil
}

func (t *Toolbox) listSectorStocks(args map[string]any) (ToolOutcome, error) {
	sector := strings.TrimSpace(stringArg(args, "sector"))
	if sector == "" {
		return ToolOutcome{}, fmt.Errorf("sector 不能为空")
	}
	limit := intArg(args, "limit", 10, 1, toolMaxRows)
	if len(t.catalog) == 0 {
		return ToolOutcome{}, fmt.Errorf("本轮没有可用全市场快照，无法按行业查询")
	}

	type row struct {
		Symbol        string
		Name          string
		Industry      string
		Price         float64
		ChangePercent float64
		FiveDay       float64
		Amount        float64
	}
	rows := make([]row, 0, 64)
	for _, entry := range t.catalog {
		if !strings.EqualFold(strings.TrimSpace(entry.Industry), sector) && !strings.EqualFold(t.sectorsOf(entry.Symbol), sector) {
			continue
		}
		if entry.Price <= 0 {
			continue
		}
		rows = append(rows, row{
			Symbol: entry.Symbol, Name: entry.Name, Industry: entry.Industry,
			Price: entry.Price, ChangePercent: entry.ChangePercent,
			FiveDay: entry.FiveDayChangePercent, Amount: entry.Amount,
		})
	}
	if len(rows) == 0 {
		return ToolOutcome{}, fmt.Errorf("没有找到行业或板块 %q 的成分股（可先用 list_sector_heat 查准确名称）", sector)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ChangePercent > rows[j].ChangePercent })
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}

	var builder strings.Builder
	suffix := ""
	if truncated {
		suffix = "，已截断"
	}
	fmt.Fprintf(&builder, "%s 成分股（按涨跌幅降序，共匹配 %d 只%s）：\n", sector, len(rows), suffix)
	builder.WriteString("代码 | 名称 | 现价 | 涨跌% | 5日% | 成交额\n")
	for _, item := range rows {
		fmt.Fprintf(&builder, "%s | %s | %.2f | %.2f | %.2f | %s\n",
			item.Symbol, item.Name, item.Price, item.ChangePercent, item.FiveDay, moneyText(item.Amount))
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: rows, Rows: len(rows), Truncated: truncated}, nil
}

// getKline 返回日线技术位置。
//
// 它回答的是提示词表格回答不了的问题：这只票在趋势的哪个位置、波动有多大。
// 同样是 8% 的权重，放在 ATR 1.2% 的银行股和 ATR 5.8% 的概念股上是两种风险。
// 没有波动率数据，模型只能拿"今天涨了几个点"代替风险判断——这正是
// "看起来有依据、其实只有一根 K 线"的根源。
func (t *Toolbox) getKline(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.klines == nil {
		return ToolOutcome{}, fmt.Errorf("日线数据源未配置，无法查询技术位置")
	}
	symbol := normalizeSymbolArg(stringArg(args, "symbol"))
	if symbol == "" {
		return ToolOutcome{}, fmt.Errorf("symbol 不能为空，且必须是 6 位代码")
	}
	// 上限 750（约三年）。原先卡在 120 是因为新浪源只能给这么多，
	// 而 120 根算不出超过一年的分位——"现在是不是三年高位"这类问题
	// 以前问不了。QuantMind 能给 4000 根，东财次之，新浪仍只给约 120；
	// 实际拿到多少由返回文本里的"日线 N 根"如实体现，不做补造。
	days := intArg(args, "days", 60, 20, 750)

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bars, err := t.klines.KLine(callCtx, symbol, "day", days)
	if (err != nil || len(bars) == 0) && t.klineAlt != nil {
		// 只有主源失败才动备源。备源也失败时保留主源错误：
		// 主源的错误信息更能说明"为什么今天拿不到"，换成备源的超时反而误导排查。
		if altBars, altErr := t.klineAlt.KLine(callCtx, symbol, "day", days); altErr == nil && len(altBars) > 0 {
			bars, err = altBars, nil
		}
	}
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("%s 日线获取失败：%v", symbol, err)
	}
	bars = cleanKLineBars(bars)
	if len(bars) < 5 {
		return ToolOutcome{}, fmt.Errorf("%s 可用日线只有 %d 根，不足以判断趋势（至少需要 5 根）", symbol, len(bars))
	}

	closes := make([]float64, len(bars))
	volumes := make([]float64, len(bars))
	for index, bar := range bars {
		closes[index] = bar.Close
		volumes[index] = bar.Volume
	}
	last := bars[len(bars)-1]
	name := last.Symbol
	if entry, ok := t.catalogEntry(symbol); ok && strings.TrimSpace(entry.Name) != "" {
		name = entry.Name
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s 日线 %d 根（%s 至 %s）\n", symbol, name, len(bars),
		bars[0].Time.In(cstZone()).Format("2006-01-02"), last.Time.In(cstZone()).Format("2006-01-02"))
	fmt.Fprintf(&builder, "收盘 %.2f | 当日 %+.2f%%\n", last.Close, last.ChangePercent)

	parts := make([]string, 0, 4)
	for _, period := range []int{5, 10, 20, 60} {
		if value := smaOf(closes, period); value > 0 {
			parts = append(parts, fmt.Sprintf("MA%d %.2f(%+.1f%%)", period, value, (last.Close/value-1)*100))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(&builder, "均线：%s\n", strings.Join(parts, " | "))
	}
	if align := maAlignment(closes); align != "" {
		fmt.Fprintf(&builder, "排列：%s\n", align)
	}

	atr := atrOf(bars, 14)
	if atr > 0 {
		fmt.Fprintf(&builder, "ATR14 %.2f，占现价 %.2f%%\n", atr, atr/last.Close*100)
	}

	high, low := rangeOf(bars, 20)
	if high > 0 && high > low {
		fmt.Fprintf(&builder, "20日区间 %.2f–%.2f，当前分位 %.0f%%\n", low, high, (last.Close-low)/(high-low)*100)
	}

	if volumeRatio := volumeRatioOf(volumes, 5, 20); volumeRatio > 0 {
		fmt.Fprintf(&builder, "量能：近5日均量 / 20日均量 = %.2f\n", volumeRatio)
	}
	if streak := closeStreak(closes); streak != 0 {
		if streak > 0 {
			fmt.Fprintf(&builder, "连续 %d 日收涨\n", streak)
		} else {
			fmt.Fprintf(&builder, "连续 %d 日收跌\n", -streak)
		}
	}

	data := map[string]any{
		"symbol": symbol, "bars": len(bars), "close": last.Close,
		"ma5": smaOf(closes, 5), "ma10": smaOf(closes, 10),
		"ma20": smaOf(closes, 20), "ma60": smaOf(closes, 60),
		"atr14": round2(atr), "high20": high, "low20": low,
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: data, Rows: len(bars)}, nil
}

// cleanKLineBars 过滤脏数据并按时间升序排好。
// 指标计算对顺序极其敏感：倒序输入算出来的 MA 会变成"未来函数"，
// 而且不会报错，只会给出一个看起来合理的错误数字。
func cleanKLineBars(bars []foundation.KLine) []foundation.KLine {
	cleaned := make([]foundation.KLine, 0, len(bars))
	for _, bar := range bars {
		if bar.Close <= 0 || bar.Time.IsZero() {
			continue
		}
		if bar.High > 0 && bar.Low > 0 && bar.High < bar.Low {
			continue
		}
		cleaned = append(cleaned, bar)
	}
	sort.SliceStable(cleaned, func(i, j int) bool { return cleaned[i].Time.Before(cleaned[j].Time) })
	return cleaned
}

func smaOf(values []float64, period int) float64 {
	if period <= 0 || len(values) < period {
		return 0
	}
	sum := 0.0
	for _, value := range values[len(values)-period:] {
		sum += value
	}
	return sum / float64(period)
}

// atrOf 用简单平均真实波幅，而非 Wilder 平滑。
// 两者在 60 根以内差异很小，而简单平均有一个好处：结果只取决于窗口内的数据，
// 同样的输入永远得到同样的输出——回放时这点比"更专业"更重要。
func atrOf(bars []foundation.KLine, period int) float64 {
	if period <= 0 || len(bars) < period+1 {
		return 0
	}
	sum := 0.0
	start := len(bars) - period
	for index := start; index < len(bars); index++ {
		previous := bars[index-1].Close
		high, low := bars[index].High, bars[index].Low
		if high <= 0 || low <= 0 {
			high, low = bars[index].Close, bars[index].Close
		}
		trueRange := math.Max(high-low, math.Max(math.Abs(high-previous), math.Abs(low-previous)))
		sum += trueRange
	}
	return sum / float64(period)
}

func rangeOf(bars []foundation.KLine, period int) (high, low float64) {
	if period <= 0 || len(bars) == 0 {
		return 0, 0
	}
	start := len(bars) - period
	if start < 0 {
		start = 0
	}
	high, low = bars[start].High, bars[start].Low
	for _, bar := range bars[start:] {
		if bar.High > high {
			high = bar.High
		}
		if bar.Low > 0 && (low <= 0 || bar.Low < low) {
			low = bar.Low
		}
	}
	return high, low
}

func volumeRatioOf(volumes []float64, short, long int) float64 {
	if short <= 0 || long <= 0 || len(volumes) < long {
		return 0
	}
	recent := smaOf(volumes, short)
	base := smaOf(volumes, long)
	if base <= 0 {
		return 0
	}
	return recent / base
}

// closeStreak 返回末尾连续同向的根数：正数为连涨，负数为连跌。
func closeStreak(closes []float64) int {
	if len(closes) < 2 {
		return 0
	}
	streak := 0
	for index := len(closes) - 1; index > 0; index-- {
		change := closes[index] - closes[index-1]
		switch {
		case change > 0 && streak >= 0:
			streak++
		case change < 0 && streak <= 0:
			streak--
		default:
			return streak
		}
	}
	return streak
}

// maAlignment 描述均线排列。模型看四个数字也能自己比较，
// 但明确写出来能显著减少"看错排列"这类低级错误。
func maAlignment(closes []float64) string {
	ma5, ma10 := smaOf(closes, 5), smaOf(closes, 10)
	ma20 := smaOf(closes, 20)
	if ma5 <= 0 || ma10 <= 0 || ma20 <= 0 {
		return ""
	}
	ma60 := smaOf(closes, 60)
	switch {
	case ma5 > ma10 && ma10 > ma20 && (ma60 <= 0 || ma20 > ma60):
		return "多头（5>10>20>60）"
	case ma5 < ma10 && ma10 < ma20 && (ma60 <= 0 || ma20 < ma60):
		return "空头（5<10<20<60）"
	case ma20 > 0 && ma60 > 0 && ma20 > ma60:
		return "短期纠缠，中期仍在上方"
	case ma20 > 0 && ma60 > 0 && ma20 < ma60:
		return "短期纠缠，中期仍在下方"
	default:
		return "均线纠缠，无明确方向"
	}
}

func (t *Toolbox) getSymbolDetail(args map[string]any) (ToolOutcome, error) {
	symbol := normalizeSymbolArg(stringArg(args, "symbol"))
	if symbol == "" {
		return ToolOutcome{}, fmt.Errorf("symbol 不能为空，且必须是 6 位代码")
	}
	quote, ok := t.quoteOf(symbol)
	if !ok {
		return ToolOutcome{}, fmt.Errorf("本轮快照里没有 %s 的行情，不能使用该标的（可能是停牌、退市或行情源未覆盖）", symbol)
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s\n", quote.Symbol, quote.Name)
	fmt.Fprintf(&builder, "现价 %.2f | 昨收 %.2f | 涨跌 %+.2f (%.2f%%) | 今开 %.2f | 高 %.2f | 低 %.2f\n",
		quote.Price, quote.PreviousClose, quote.Change, quote.ChangePercent, quote.Open, quote.High, quote.Low)
	if quote.High > quote.Low && quote.PreviousClose > 0 {
		fmt.Fprintf(&builder, "振幅 %.2f%% | 成交额 %s\n", (quote.High-quote.Low)/quote.PreviousClose*100, moneyText(quote.Amount))
	}

	if entry, ok := t.catalogEntry(symbol); ok {
		fmt.Fprintf(&builder, "行业 %s | 板块大类 %s | 5日涨幅 %.2f%%\n",
			firstNonEmpty(entry.Industry, "未知"), firstNonEmpty(t.sectorsOf(symbol), "未知"), entry.FiveDayChangePercent)
		if len(entry.Concepts) > 0 {
			limit := len(entry.Concepts)
			if limit > 6 {
				limit = 6
			}
			fmt.Fprintf(&builder, "概念：%s\n", strings.Join(entry.Concepts[:limit], "、"))
		}
	}

	if candidate, ok := t.candidateOf(symbol); ok {
		fmt.Fprintf(&builder, "候选池：第 %d 名，综合分 %.1f\n", candidate.Rank, candidate.Score)
		for i, reason := range candidate.Reasons {
			if i >= 4 {
				break
			}
			fmt.Fprintf(&builder, "  入选依据：%s\n", reason)
		}
	} else {
		builder.WriteString("候选池：本轮未进入自动选股候选池\n")
	}

	if position, ok := t.positionOf(symbol); ok {
		fmt.Fprintf(&builder, "持仓 %d 股，成本 %.2f，浮盈 %.2f%%，可卖 %d 股\n",
			position.Quantity, position.AverageCost, unrealizedPercent(position, quote.Price), position.Sellable())
		if position.StopLossPrice > 0 || position.TakeProfitPrice > 0 {
			fmt.Fprintf(&builder, "防守线：止损 %.2f，止盈 %.2f，峰值 %.2f\n",
				position.StopLossPrice, position.TakeProfitPrice, position.PeakPrice)
		}
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: quote, Rows: 1}, nil
}

func (t *Toolbox) compareSymbols(args map[string]any) (ToolOutcome, error) {
	raw := stringsArg(args, "symbols")
	if len(raw) < 2 {
		return ToolOutcome{}, fmt.Errorf("symbols 至少需要 2 个代码")
	}
	if len(raw) > 6 {
		raw = raw[:6]
	}
	type row struct {
		Symbol        string
		Name          string
		Industry      string
		Price         float64
		ChangePercent float64
		FiveDay       float64
		Amount        float64
		RelativeToMed float64
		Missing       bool
	}
	rows := make([]row, 0, len(raw))
	missing := make([]string, 0)
	for _, item := range raw {
		symbol := normalizeSymbolArg(item)
		quote, ok := t.quoteOf(symbol)
		if !ok {
			missing = append(missing, symbol)
			continue
		}
		entry, _ := t.catalogEntry(symbol)
		rows = append(rows, row{
			Symbol: symbol, Name: quote.Name, Industry: entry.Industry,
			Price: quote.Price, ChangePercent: quote.ChangePercent,
			FiveDay: entry.FiveDayChangePercent, Amount: quote.Amount,
		})
	}
	if len(rows) == 0 {
		return ToolOutcome{}, fmt.Errorf("这些代码都没有行情：%s", strings.Join(missing, ", "))
	}

	// 相对强弱用"候选池内同行业个股当日涨幅中位数"作为基准。
	// 用中位数而不是均值，是为了避免一只涨停板把整个行业的中枢拉高。
	medians := t.industryMedianChange()
	for i := range rows {
		median, ok := medians[rows[i].Industry]
		if !ok {
			continue
		}
		rows[i].RelativeToMed = round2(rows[i].ChangePercent - median)
	}

	var builder strings.Builder
	builder.WriteString("横向对比（相对强弱以同行业中位数为基准）：\n")
	builder.WriteString("代码 | 名称 | 行业 | 现价 | 涨跌% | 较行业中位 | 5日% | 成交额\n")
	for _, item := range rows {
		fmt.Fprintf(&builder, "%s | %s | %s | %.2f | %.2f | %+.2f | %.2f | %s\n",
			item.Symbol, item.Name, firstNonEmpty(item.Industry, "未知"), item.Price,
			item.ChangePercent, item.RelativeToMed, item.FiveDay, moneyText(item.Amount))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&builder, "无行情：%s\n", strings.Join(missing, ", "))
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: rows, Rows: len(rows)}, nil
}

func (t *Toolbox) recallTrades(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	limit := intArg(args, "limit", 6, 1, 12)
	query := MemoryQuery{
		AccountID: t.accountID,
		Symbol:    normalizeSymbolArg(stringArg(args, "symbol")),
		Sector:    stringArg(args, "sector"),
		Regime:    strings.ToLower(stringArg(args, "regime")),
		Trigger:   strings.ToLower(stringArg(args, "trigger")),
		Outcome:   strings.ToLower(stringArg(args, "outcome")),
		Keyword:   stringArg(args, "keyword"),
		Limit:     limit,
	}
	memories, err := t.store.SearchMemories(ctx, query)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("记忆检索失败：%w", err)
	}
	if len(memories) == 0 {
		return ToolOutcome{Text: "没有匹配的历史记忆。这不代表这类情况没发生过，只代表账本里还没有可对齐的平仓记录。", Data: memories}, nil
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "历史记忆（%d 条，新的在前）：\n", len(memories))
	for _, memory := range memories {
		fmt.Fprintf(&builder, "- %s %s｜%s期｜%s\n", memory.TradingDay, firstNonEmpty(memory.Name, memory.Symbol), memory.RegimeLabel, memory.Lesson)
		if memory.Thesis != "" {
			fmt.Fprintf(&builder, "  当时理由：%s\n", clipRunes(memory.Thesis, 80))
		}
		if memory.PeakGainPercent > 0 {
			fmt.Fprintf(&builder, "  峰值浮盈 %.2f%%，回吐 %.2f 个百分点\n", memory.PeakGainPercent, memory.GiveBackPercent)
		}
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: memories, Rows: len(memories)}, nil
}

// ── 辅助 ──────────────────────────────────────────────────

func (t *Toolbox) quoteOf(symbol string) (foundation.Quote, bool) {
	// 比较一律走 6 位数字：行情源的代码是 688775.SH 这种带后缀格式，
	// 而工具参数可能写成 688775、sh688775 或 688775.SH。
	// 早期版本直接字符串相等比较，导致带后缀的标的全部查不到行情——
	// 模型因此连续三步都撞在"取不到数据"上，还据此推出了"科创板没有行情"的错误结论。
	key := symbolDigits(symbol)
	if key == "" {
		return foundation.Quote{}, false
	}
	for _, quote := range t.snapshot.Quotes {
		if symbolDigits(quote.Symbol) == key {
			return quote, true
		}
	}
	return foundation.Quote{}, false
}

func (t *Toolbox) candidateOf(symbol string) (universe.Candidate, bool) {
	if t.snapshot.Board == nil {
		return universe.Candidate{}, false
	}
	key := symbolDigits(symbol)
	for _, candidate := range t.snapshot.Board.Candidates {
		if symbolDigits(candidate.Symbol) == key {
			return candidate, true
		}
	}
	return universe.Candidate{}, false
}

func (t *Toolbox) positionOf(symbol string) (Position, bool) {
	key := symbolDigits(symbol)
	for _, position := range t.snapshot.Positions {
		if symbolDigits(position.Symbol) == key {
			return position, true
		}
	}
	return Position{}, false
}

func (t *Toolbox) sectorsOf(symbol string) string {
	if len(t.snapshot.Sectors) == 0 {
		return ""
	}
	key := symbolDigits(symbol)
	for code, sector := range t.snapshot.Sectors {
		if symbolDigits(code) == key {
			return sector
		}
	}
	return ""
}

// industryMedianChange 计算候选池内每个行业当日涨幅的中位数。
func (t *Toolbox) industryMedianChange() map[string]float64 {
	grouped := map[string][]float64{}
	if t.snapshot.Board != nil {
		for _, candidate := range t.snapshot.Board.Candidates {
			if candidate.Industry == "" {
				continue
			}
			grouped[candidate.Industry] = append(grouped[candidate.Industry], candidate.ChangePercent)
		}
	}
	medians := make(map[string]float64, len(grouped))
	for industry, values := range grouped {
		if len(values) == 0 {
			continue
		}
		sort.Float64s(values)
		mid := len(values) / 2
		if len(values)%2 == 1 {
			medians[industry] = values[mid]
		} else {
			medians[industry] = (values[mid-1] + values[mid]) / 2
		}
	}
	return medians
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		// JSON 数字解码后是 float64，整数值的代码要避免变成 600519.000000。
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func intArg(args map[string]any, key string, def, min, max int) int {
	if args == nil {
		return def
	}
	value, ok := args[key]
	if !ok || value == nil {
		return def
	}
	parsed := 0
	switch typed := value.(type) {
	case float64:
		parsed = int(typed)
	case int:
		parsed = typed
	case string:
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed); err != nil {
			return def
		}
	default:
		return def
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func stringsArg(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	value, ok := args[key]
	if !ok || value == nil {
		return nil
	}
	items := make([]string, 0, 6)
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprintf("%v", item)); text != "" {
				items = append(items, text)
			}
		}
	case []string:
		for _, item := range typed {
			if text := strings.TrimSpace(item); text != "" {
				items = append(items, text)
			}
		}
	case string:
		for _, item := range strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
			if text := strings.TrimSpace(item); text != "" {
				items = append(items, text)
			}
		}
	}
	return items
}

// catalogEntry 按 6 位数字查目录，兼容目录里带后缀的写法。
func (t *Toolbox) catalogEntry(symbol string) (foundation.StockCatalogEntry, bool) {
	key := symbolDigits(symbol)
	if key == "" {
		return foundation.StockCatalogEntry{}, false
	}
	if entry, ok := t.catalog[key]; ok {
		return entry, true
	}
	// 兼容尚未按 digits 重建索引的调用方（例如测试里直接塞入的 map）。
	for code, entry := range t.catalog {
		if symbolDigits(code) == key {
			return entry, true
		}
	}
	return foundation.StockCatalogEntry{}, false
}

// symbolDigits 从任意写法里取出 6 位数字代码。取不到返回空串。
//
// 所有内部查找都用它做键：A 股代码是 6 位数字，前缀后缀都是展示格式。
// 把"展示格式"当成身份去比较，是这类系统最常见的一类隐性 bug。
func symbolDigits(raw string) string {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}
	if index := strings.IndexAny(value, ". "); index > 0 {
		value = value[:index]
	}
	for _, prefix := range []string{"SH", "SZ", "BJ"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimSpace(value)
	if len(value) != 6 {
		return ""
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return ""
		}
	}
	return value
}

// normalizeSymbolArg 把 600519.SH / sh600519 / 600519 统一成 6 位代码。
// 模型经常混用这几种写法，直接拒绝会让它反复试错浪费一轮调用。
func normalizeSymbolArg(raw string) string {
	return symbolDigits(raw)
}

func moneyText(value float64) string {
	abs := math.Abs(value)
	switch {
	case abs >= 1e8:
		return fmt.Sprintf("%.2f亿", value/1e8)
	case abs >= 1e4:
		return fmt.Sprintf("%.0f万", value/1e4)
	default:
		return fmt.Sprintf("%.0f", value)
	}
}

func clipToolText(text string) string {
	runes := []rune(text)
	if len(runes) <= toolMaxTextChars {
		return text
	}
	return string(runes[:toolMaxTextChars]) + "\n（结果过长已截断）"
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func unrealizedPercent(position Position, price float64) float64 {
	if position.AverageCost <= 0 || price <= 0 {
		return 0
	}
	return round2((price/position.AverageCost - 1) * 100)
}

func cstZone() *time.Location {
	return time.FixedZone("CST", 8*3600)
}
