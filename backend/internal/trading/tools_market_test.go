package trading

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
)

type stubMarketIntel struct {
	indexes   []foundation.MarketIndexSnapshot
	margin    []foundation.MarketMarginPoint
	billboard foundation.MarketBillboardDetail
	announce  []foundation.MarketResearchItem
	err       error
}

func (s stubMarketIntel) Indexes(context.Context, string) ([]foundation.MarketIndexSnapshot, foundation.SourceMeta, error) {
	if s.err != nil {
		return nil, foundation.SourceMeta{}, s.err
	}
	return s.indexes, foundation.SourceMeta{Source: "stub", FetchedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, cstZone())}, nil
}

func (s stubMarketIntel) MarginSeries(context.Context, int) ([]foundation.MarketMarginPoint, foundation.SourceMeta, error) {
	if s.err != nil {
		return nil, foundation.SourceMeta{}, s.err
	}
	return s.margin, foundation.SourceMeta{Source: "stub"}, nil
}

func (s stubMarketIntel) BillboardDetail(context.Context, string, string, string) (foundation.MarketBillboardDetail, foundation.SourceMeta, error) {
	if s.err != nil {
		return foundation.MarketBillboardDetail{}, foundation.SourceMeta{}, s.err
	}
	return s.billboard, foundation.SourceMeta{Source: "stub"}, nil
}

func (s stubMarketIntel) Announcements(context.Context, string, string, string, int) ([]foundation.MarketResearchItem, foundation.SourceMeta, error) {
	if s.err != nil {
		return nil, foundation.SourceMeta{}, s.err
	}
	return s.announce, foundation.SourceMeta{Source: "stub"}, nil
}

type stubNews struct {
	latest []foundation.NewsItem
	stock  []foundation.NewsItem
	err    error
}

func (s stubNews) LatestNews(context.Context, int) ([]foundation.NewsItem, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.latest, nil
}

func (s stubNews) SearchStockNews(context.Context, string, string, int) ([]foundation.NewsItem, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.stock, nil
}

func TestToolboxSpecsIncludeMarketIntel(t *testing.T) {
	toolbox := &Toolbox{}
	specs := toolbox.Specs()
	if len(specs) != 12 {
		t.Fatalf("工具数量应为 12，实际 %d：%s", len(specs), toolNames(specs))
	}
	for _, name := range []string{"search_news", "get_announcements", "get_billboard", "get_market_context", "get_margin_balance"} {
		found := false
		for _, spec := range specs {
			if spec.Name == name {
				found = true
				if strings.TrimSpace(spec.Description) == "" {
					t.Fatalf("%s 缺少说明，模型无法判断该不该调用", name)
				}
			}
		}
		if !found {
			t.Fatalf("工具说明里缺少 %s", name)
		}
	}
}

func TestToolboxMarketToolsReportMissingDataSource(t *testing.T) {
	toolbox := &Toolbox{}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"search_news", map[string]any{"keyword": "机器人"}, "新闻数据源未配置"},
		{"get_announcements", map[string]any{"symbol": "600519"}, "公告数据源未配置"},
		{"get_billboard", map[string]any{"symbol": "600519"}, "龙虎榜数据源未配置"},
		{"get_margin_balance", map[string]any{}, "两融数据源未配置"},
	}
	for _, item := range cases {
		if _, err := toolbox.Call(context.Background(), item.name, item.args); err == nil || !strings.Contains(err.Error(), item.want) {
			t.Fatalf("%s 在数据源缺失时应报 %q，实际 %v", item.name, item.want, err)
		}
	}
}

func TestSearchNewsFiltersDedupsAndSaysHowMuchItScanned(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 30, 0, 0, cstZone())
	toolbox := &Toolbox{news: stubNews{latest: []foundation.NewsItem{
		{Title: "机器人板块获政策支持", PublishedAt: now},
		{Title: "机器人板块获政策支持", PublishedAt: now}, // 转载重复
		{Title: "白酒消费回暖", PublishedAt: now},
	}}}

	outcome, err := toolbox.Call(context.Background(), "search_news", map[string]any{"keyword": "机器人"})
	if err != nil {
		t.Fatalf("关键词检索不应失败：%v", err)
	}
	if outcome.Rows != 1 {
		t.Fatalf("去重后应只剩 1 条，实际 %d：%s", outcome.Rows, outcome.Text)
	}
	if !strings.Contains(outcome.Text, "最近 3 条市场新闻") {
		t.Fatalf("必须说明扫描深度，否则「0 条」会被误读成「没发生」：%s", outcome.Text)
	}
	if !strings.Contains(outcome.Text, "未核实正文") {
		t.Fatalf("必须提示只有标题：%s", outcome.Text)
	}
}

func TestSearchNewsRequiresSomeScope(t *testing.T) {
	toolbox := &Toolbox{news: stubNews{}}
	if _, err := toolbox.Call(context.Background(), "search_news", map[string]any{}); err == nil {
		t.Fatal("既不给 symbol 也不给 keyword 时必须报错")
	}
}

func TestGetAnnouncementsRendersCategoryAndTitle(t *testing.T) {
	toolbox := &Toolbox{marketIntel: stubMarketIntel{announce: []foundation.MarketResearchItem{
		{Title: "关于股东减持计划的预披露公告", Category: "减持", PublishedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, cstZone())},
	}}}
	outcome, err := toolbox.Call(context.Background(), "get_announcements", map[string]any{"symbol": "600519.SH"})
	if err != nil {
		t.Fatalf("公告查询不应失败：%v", err)
	}
	for _, want := range []string{"减持", "预披露", "没有正文"} {
		if !strings.Contains(outcome.Text, want) {
			t.Fatalf("公告文本缺少 %q：%s", want, outcome.Text)
		}
	}
}

func TestGetBillboardSaysNoRecordInsteadOfEmpty(t *testing.T) {
	toolbox := &Toolbox{marketIntel: stubMarketIntel{}}
	outcome, err := toolbox.Call(context.Background(), "get_billboard", map[string]any{"symbol": "600519"})
	if err != nil {
		t.Fatalf("没有上榜记录不是错误：%v", err)
	}
	if !strings.Contains(outcome.Text, "没有龙虎榜记录") {
		t.Fatalf("应明确说明没有记录：%s", outcome.Text)
	}
}

func TestGetBillboardAggregatesInstitutionNet(t *testing.T) {
	toolbox := &Toolbox{marketIntel: stubMarketIntel{billboard: foundation.MarketBillboardDetail{
		TradeDate: "2026-10-20", Symbol: "600519", Reason: "日涨幅偏离值达 7%",
		BuySeats: []foundation.MarketBillboardSeat{
			{Name: "机构专用", BuyAmount: 12000000, NetAmount: 12000000, Institution: true},
			{Name: "某游资营业部", BuyAmount: 5000000, NetAmount: 3000000},
		},
		SellSeats: []foundation.MarketBillboardSeat{
			{Name: "机构专用", SellAmount: 4000000, NetAmount: -4000000, Institution: true},
		},
	}}}
	outcome, err := toolbox.Call(context.Background(), "get_billboard", map[string]any{"symbol": "600519"})
	if err != nil {
		t.Fatalf("龙虎榜查询不应失败：%v", err)
	}
	if !strings.Contains(outcome.Text, "日涨幅偏离值达 7%") {
		t.Fatalf("应带上榜原因：%s", outcome.Text)
	}
	if !strings.Contains(outcome.Text, "机构席位净额合计") {
		t.Fatalf("应汇总机构净额：%s", outcome.Text)
	}
	if !strings.Contains(outcome.Text, "机构）") {
		t.Fatalf("机构席位应被标记：%s", outcome.Text)
	}
}

func TestGetMarketContextUsesSnapshotBreadth(t *testing.T) {
	market := testMarketView()
	market.Breadth = cognition.Breadth{
		Total: 5200, Advancing: 2800, Declining: 1900, Unchanged: 500,
		LimitUp: 45, LimitDown: 8, MedianChange: 0.35, TotalAmount: 9.2e11, AdvanceRatio: 0.6,
	}
	toolbox := &Toolbox{snapshot: Snapshot{Market: market}, marketIntel: stubMarketIntel{indexes: []foundation.MarketIndexSnapshot{
		{Name: "上证指数", Price: 3120.45, ChangePercent: 0.42},
		{Name: "创业板指", Price: 2010.10, ChangePercent: -0.88},
	}}}
	outcome, err := toolbox.Call(context.Background(), "get_market_context", nil)
	if err != nil {
		t.Fatalf("大盘查询不应失败：%v", err)
	}
	for _, want := range []string{"上证指数", "创业板指", "涨跌家数", "中位涨幅"} {
		if !strings.Contains(outcome.Text, want) {
			t.Fatalf("大盘文本缺少 %q：%s", want, outcome.Text)
		}
	}
}

func TestGetMarketContextFlagsUnevaluatedMarket(t *testing.T) {
	toolbox := &Toolbox{snapshot: Snapshot{}, marketIntel: stubMarketIntel{err: errors.New("上游超时")}}
	outcome, err := toolbox.Call(context.Background(), "get_market_context", nil)
	if err != nil {
		t.Fatalf("指数失败但快照未评估时仍应给出说明：%v", err)
	}
	if !strings.Contains(outcome.Text, "未评估") {
		t.Fatalf("必须提示本轮未评估：%s", outcome.Text)
	}
}

func TestGetMarginBalanceComputesRangeChange(t *testing.T) {
	toolbox := &Toolbox{marketIntel: stubMarketIntel{margin: []foundation.MarketMarginPoint{
		{TradeDate: "2026-10-17", MarginBalance: 1.5e12, MarginBalanceChange: 1e10, FinancingNetBuyAmount: 8e9},
		{TradeDate: "2026-10-20", MarginBalance: 1.53e12, MarginBalanceChange: 3e10, FinancingNetBuyAmount: 2.5e10},
	}}}
	outcome, err := toolbox.Call(context.Background(), "get_margin_balance", map[string]any{"days": 5})
	if err != nil {
		t.Fatalf("两融查询不应失败：%v", err)
	}
	if !strings.Contains(outcome.Text, "全市场口径") {
		t.Fatalf("必须说明这是全市场口径，否则会被当成个股融资余额：%s", outcome.Text)
	}
	if !strings.Contains(outcome.Text, "区间变化") {
		t.Fatalf("应给出区间变化：%s", outcome.Text)
	}
}
