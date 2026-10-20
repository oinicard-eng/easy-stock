package trading

// 第二批只读工具：把仓库里已经存在、但交易智能体看不到的数据接进来。
//
// 为什么这件事比调提示词更重要：原来 7 个工具全部是"当下价格结构"，
// 没有一条回答"为什么涨"。模型被要求写事实依据，手上只有成交额与涨幅，
// thesis 必然退化成"资金流入 + 涨幅居前"这种同义反复。
//
// 这里的五个工具遵守与 tools.go 相同的约束：只读、参数严格校验、
// 输出有上限、数据源缺失时明确报错，而不是返回一份编造的内容。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
)

// MarketIntelProvider 是大盘/两融/龙虎榜/公告的数据能力。
// 它刻意做成一个窄接口：交易侧只声明自己用到的四个方法，
// 这样行情总览服务多长几个方法不会影响交易，交易也不会绑死在它身上。
type MarketIntelProvider interface {
	Indexes(ctx context.Context, scope string) ([]foundation.MarketIndexSnapshot, foundation.SourceMeta, error)
	MarginSeries(ctx context.Context, limit int) ([]foundation.MarketMarginPoint, foundation.SourceMeta, error)
	BillboardDetail(ctx context.Context, symbol, tradeDate, reason string) (foundation.MarketBillboardDetail, foundation.SourceMeta, error)
	Announcements(ctx context.Context, query, symbol, category string, limit int) ([]foundation.MarketResearchItem, foundation.SourceMeta, error)
}

// NewsSearchProvider 是新闻能力：既要有全市场最新，也要能按标的检索。
// 只有全市场时，模型问"这只票为什么涨"会得到一堆与它无关的宏观新闻。
type NewsSearchProvider interface {
	LatestNews(ctx context.Context, limit int) ([]foundation.NewsItem, error)
	SearchStockNews(ctx context.Context, symbol, query string, limit int) ([]foundation.NewsItem, error)
}

const (
	// newsScanLimit 是全市场新闻的扫描深度。关键词过滤发生在本地，
	// 所以要扫得比返回条数多一个量级，否则"匹配到 0 条"会大量出现。
	newsScanLimit = 80
	// toolCallTimeout 是所有外部数据工具的调用上限。
	// 一个工具卡住会占掉整步调研预算，比查不到更糟——查不到模型还能换招。
	toolCallTimeout = 12 * time.Second
)

func marketIntelSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "search_news",
			Description: "检索新闻。给 symbol 返回与该标的相关的新闻，给 keyword 返回包含该关键词的市场新闻。只有标题与时间，没有正文，不能当作已核实事实。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Desc: "6 位代码，按标的检索"},
				{Name: "keyword", Type: "string", Desc: "关键词，如 机器人、算力、减持"},
				{Name: "limit", Type: "int", Desc: "返回条数，默认 8，最多 12"},
			},
		},
		{
			Name:        "get_announcements",
			Description: "个股公告标题与类型（业绩预告、减持、问询、股权激励等）。公告是事实性文件，比新闻可靠；这里只有标题，没有正文。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Required: true, Desc: "6 位代码"},
				{Name: "limit", Type: "int", Desc: "返回条数，默认 8，最多 15"},
			},
		},
		{
			Name:        "get_billboard",
			Description: "个股龙虎榜：上榜原因、买入/卖出前五席位、机构专用席位与净额。用于判断这波涨跌是谁在做。",
			Params: []ToolParam{
				{Name: "symbol", Type: "string", Required: true, Desc: "6 位代码"},
				{Name: "date", Type: "string", Desc: "交易日 YYYY-MM-DD，留空取最近一个有数据的交易日"},
			},
		},
		{
			Name:        "get_market_context",
			Description: "大盘环境：主要指数涨跌、市场状态与风险预算、涨跌家数与涨跌停数、中位涨幅、全市场成交额。用于判断现在该不该进攻。",
			Params:      []ToolParam{},
		},
		{
			Name:        "get_margin_balance",
			Description: "市场两融余额序列（融资余额、融券余额、融资净买入、环比变化）。注意：这是全市场口径，不是单只标的的融资余额，只能用于判断杠杆资金的整体进退。",
			Params: []ToolParam{
				{Name: "days", Type: "int", Desc: "回看交易日数，默认 10，最多 30"},
			},
		},
	}
}

// searchNews 检索新闻。
//
// 给 symbol 时走标的检索；只给 keyword 时在最近 newsScanLimit 条市场新闻里本地过滤。
// 后者必须如实说明"扫了多少条、匹配到几条"——模型看到"0 条"时，
// 如果不知道扫描深度，会推出"这件事没有新闻"的错误结论。
func (t *Toolbox) searchNews(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.news == nil {
		return ToolOutcome{}, fmt.Errorf("新闻数据源未配置，无法检索")
	}
	symbol := normalizeSymbolArg(stringArg(args, "symbol"))
	keyword := strings.TrimSpace(stringArg(args, "keyword"))
	limit := intArg(args, "limit", 8, 1, 12)
	if symbol == "" && keyword == "" {
		return ToolOutcome{}, fmt.Errorf("symbol 与 keyword 至少要给一个，否则无法确定查什么")
	}

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	var items []foundation.NewsItem
	scope := ""
	if symbol != "" {
		fetched, err := t.news.SearchStockNews(callCtx, symbol, keyword, limit)
		if err != nil {
			return ToolOutcome{}, fmt.Errorf("%s 新闻检索失败：%v", symbol, err)
		}
		items = fetched
		scope = fmt.Sprintf("%s 相关新闻", symbol)
	} else {
		fetched, err := t.news.LatestNews(callCtx, newsScanLimit)
		if err != nil {
			return ToolOutcome{}, fmt.Errorf("市场新闻拉取失败：%v", err)
		}
		items = filterNewsByKeyword(fetched, keyword)
		scope = fmt.Sprintf("最近 %d 条市场新闻中匹配「%s」", len(fetched), keyword)
	}

	items = dedupNews(items)
	if len(items) == 0 {
		return ToolOutcome{
			Text: fmt.Sprintf("%s：没有匹配的新闻。这不等于没有发生，只等于当前新闻源里没有。\n", scope),
			Data: items,
		}, nil
	}
	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s（%d 条）：\n", scope, len(items))
	for _, item := range items {
		stamp := "时间未知"
		if !item.PublishedAt.IsZero() {
			stamp = item.PublishedAt.In(cstZone()).Format("01-02 15:04")
		}
		fmt.Fprintf(&builder, "%s | %s\n", stamp, clipRunes(strings.TrimSpace(item.Title), 70))
	}
	builder.WriteString("以上只有标题，未核实正文；把它当线索而不是结论。\n")
	return ToolOutcome{Text: clipToolText(builder.String()), Data: items, Rows: len(items), Truncated: truncated}, nil
}

func filterNewsByKeyword(items []foundation.NewsItem, keyword string) []foundation.NewsItem {
	key := strings.ToLower(strings.TrimSpace(keyword))
	if key == "" {
		return items
	}
	matched := make([]foundation.NewsItem, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(item.Title + " " + item.Content + " " + strings.Join(item.Tags, " "))
		if strings.Contains(haystack, key) {
			matched = append(matched, item)
		}
	}
	return matched
}

// dedupNews 按标题去重并保持原顺序。
// 同一条新闻常被多个源转载，不去重会让"三条利空"看起来像"一条利好被淹没"。
func dedupNews(items []foundation.NewsItem) []foundation.NewsItem {
	seen := make(map[string]struct{}, len(items))
	kept := make([]foundation.NewsItem, 0, len(items))
	for _, item := range items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		key := strings.ToLower(title)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		kept = append(kept, item)
	}
	return kept
}

// getAnnouncements 返回公告标题与类型。
func (t *Toolbox) getAnnouncements(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.marketIntel == nil {
		return ToolOutcome{}, fmt.Errorf("公告数据源未配置，无法查询")
	}
	symbol := normalizeSymbolArg(stringArg(args, "symbol"))
	if symbol == "" {
		return ToolOutcome{}, fmt.Errorf("symbol 不能为空，且必须是 6 位代码")
	}
	limit := intArg(args, "limit", 8, 1, 15)

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	items, meta, err := t.marketIntel.Announcements(callCtx, "", symbol, "", limit)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("%s 公告查询失败：%v", symbol, err)
	}
	if len(items) == 0 {
		return ToolOutcome{
			Text: fmt.Sprintf("%s 近期没有查到公告（可能是数据源未覆盖该标的，或确实没有新公告）。\n", symbol),
			Data: items,
		}, nil
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s 公告 %d 条（来源 %s）：\n", symbol, len(items), firstNonEmpty(meta.Source, "未知"))
	for _, item := range items {
		stamp := "日期未知"
		if !item.PublishedAt.IsZero() {
			stamp = item.PublishedAt.In(cstZone()).Format("01-02")
		}
		category := firstNonEmpty(item.Category, item.Kind, "未分类")
		title := clipRunes(strings.TrimSpace(item.Title), 60)
		line := fmt.Sprintf("%s | %s | %s", stamp, category, title)
		if item.Rating != "" {
			line += fmt.Sprintf(" | 评级 %s", item.Rating)
		}
		fmt.Fprintf(&builder, "%s\n", line)
	}
	builder.WriteString("这里只有标题与类型，没有正文；涉及金额、比例、期限的结论必须另行核实。\n")
	return ToolOutcome{Text: clipToolText(builder.String()), Data: items, Rows: len(items)}, nil
}

// getBillboard 返回龙虎榜明细。
func (t *Toolbox) getBillboard(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.marketIntel == nil {
		return ToolOutcome{}, fmt.Errorf("龙虎榜数据源未配置，无法查询")
	}
	symbol := normalizeSymbolArg(stringArg(args, "symbol"))
	if symbol == "" {
		return ToolOutcome{}, fmt.Errorf("symbol 不能为空，且必须是 6 位代码")
	}
	date := strings.TrimSpace(stringArg(args, "date"))

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	detail, meta, err := t.marketIntel.BillboardDetail(callCtx, symbol, date, "")
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("%s 龙虎榜查询失败：%v", symbol, err)
	}
	if len(detail.BuySeats) == 0 && len(detail.SellSeats) == 0 {
		return ToolOutcome{
			Text: fmt.Sprintf("%s 没有龙虎榜记录：未触发上榜条件，或该交易日数据未覆盖。没上榜本身是中性信息，不代表没有资金在做。\n", symbol),
			Data: detail,
		}, nil
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s 龙虎榜（%s，来源 %s）\n", symbol, firstNonEmpty(detail.TradeDate, date, "最近交易日"), firstNonEmpty(meta.Source, "未知"))
	if detail.Reason != "" {
		fmt.Fprintf(&builder, "上榜原因：%s\n", detail.Reason)
	}

	institutionNet := 0.0
	writeSeats := func(label string, seats []foundation.MarketBillboardSeat, isBuy bool) {
		if len(seats) == 0 {
			return
		}
		fmt.Fprintf(&builder, "%s：\n", label)
		for index, seat := range seats {
			if index >= 5 {
				break
			}
			amount := seat.BuyAmount
			if !isBuy {
				amount = seat.SellAmount
			}
			flag := ""
			if seat.Institution {
				flag = "（机构）"
				institutionNet += seat.NetAmount
			}
			fmt.Fprintf(&builder, "  %d. %s%s %s 净额 %s\n", index+1, clipRunes(seat.Name, 18), flag, moneyText(amount), moneyText(seat.NetAmount))
		}
	}
	writeSeats("买入前五", detail.BuySeats, true)
	writeSeats("卖出前五", detail.SellSeats, false)
	if institutionNet != 0 {
		fmt.Fprintf(&builder, "机构席位净额合计 %s（正为净买入）\n", moneyText(institutionNet))
	}
	builder.WriteString("席位名称来自上游数据源，机构标记是分类而非身份确认。\n")
	return ToolOutcome{Text: clipToolText(builder.String()), Data: detail, Rows: len(detail.BuySeats) + len(detail.SellSeats)}, nil
}

// getMarketContext 汇总大盘环境。
//
// 它与提示词里的"市场环境"段落有重叠，但用途不同：提示词那段是本轮认知引擎的结论，
// 这个工具让模型可以在调研中主动再确认一次（例如怀疑快照过期时）。
func (t *Toolbox) getMarketContext(ctx context.Context, _ map[string]any) (ToolOutcome, error) {
	var builder strings.Builder

	if t.marketIntel != nil {
		callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
		indexes, meta, err := t.marketIntel.Indexes(callCtx, "")
		cancel()
		if err == nil && len(indexes) > 0 {
			fmt.Fprintf(&builder, "指数（来源 %s，抓取 %s）：\n",
				firstNonEmpty(meta.Source, "未知"), meta.FetchedAt.In(cstZone()).Format("15:04:05"))
			shown := 0
			for _, index := range indexes {
				if index.Price <= 0 {
					continue
				}
				if shown >= 6 {
					break
				}
				shown++
				fmt.Fprintf(&builder, "%s | %.2f | %+.2f%%\n", firstNonEmpty(index.Name, index.Code), index.Price, index.ChangePercent)
			}
		} else if err != nil {
			fmt.Fprintf(&builder, "指数数据获取失败：%v\n", err)
		}
	}

	market := t.snapshot.Market
	if !market.Evaluated {
		builder.WriteString("市场状态：本轮未评估（样本不足或认知引擎未运行），不要据此认为市场中性。\n")
	} else {
		fmt.Fprintf(&builder, "市场状态：%s，风险预算 %.0f%%\n", firstNonEmpty(market.Label, string(market.Regime)), market.RiskBudgetPercent)
		breadth := market.Breadth
		if breadth.Total > 0 {
			fmt.Fprintf(&builder, "涨跌家数：上涨 %d / 下跌 %d / 平盘 %d，涨停 %d / 跌停 %d\n",
				breadth.Advancing, breadth.Declining, breadth.Unchanged, breadth.LimitUp, breadth.LimitDown)
			fmt.Fprintf(&builder, "中位涨幅 %+.2f%%，成交额 %s\n", breadth.MedianChange, moneyText(breadth.TotalAmount))
			if breadth.AdvanceRatio > 0 {
				fmt.Fprintf(&builder, "上涨占比 %.0f%%\n", breadth.AdvanceRatio*100)
			}
		}
		for index, note := range market.Notes {
			if index >= 4 {
				break
			}
			fmt.Fprintf(&builder, "  说明：%s\n", note)
		}
	}
	if builder.Len() == 0 {
		return ToolOutcome{}, fmt.Errorf("大盘数据源未配置，且本轮没有市场认知结果，无法提供大盘环境")
	}
	return ToolOutcome{Text: clipToolText(builder.String()), Data: market, Rows: 1}, nil
}

// getMarginBalance 返回市场两融余额序列。
func (t *Toolbox) getMarginBalance(ctx context.Context, args map[string]any) (ToolOutcome, error) {
	if t.marketIntel == nil {
		return ToolOutcome{}, fmt.Errorf("两融数据源未配置，无法查询")
	}
	days := intArg(args, "days", 10, 3, 30)

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	points, meta, err := t.marketIntel.MarginSeries(callCtx, days)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("两融数据查询失败：%v", err)
	}
	if len(points) == 0 {
		return ToolOutcome{Text: "两融数据返回空（可能是数据源未覆盖或当日未更新）。\n", Data: points}, nil
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "市场两融（全市场口径，来源 %s，共 %d 个交易日）：\n", firstNonEmpty(meta.Source, "未知"), len(points))
	builder.WriteString("日期 | 两融余额 | 环比 | 融资净买入\n")
	for _, point := range points {
		fmt.Fprintf(&builder, "%s | %s | %s | %s\n",
			firstNonEmpty(point.TradeDate, "—"),
			moneyText(point.MarginBalance),
			moneyText(point.MarginBalanceChange),
			moneyText(point.FinancingNetBuyAmount))
	}
	if first, last := points[0], points[len(points)-1]; first.MarginBalance > 0 && len(points) > 1 {
		change := (last.MarginBalance/first.MarginBalance - 1) * 100
		fmt.Fprintf(&builder, "区间变化：%+.2f%%（%s → %s）\n", change, moneyText(first.MarginBalance), moneyText(last.MarginBalance))
	}
	builder.WriteString("这是杠杆资金的整体进退，不是单只标的的融资余额；不能用来判断个股。\n")
	return ToolOutcome{Text: clipToolText(builder.String()), Data: points, Rows: len(points)}, nil
}
