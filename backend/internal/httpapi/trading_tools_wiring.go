package httpapi

// 交易侧工具的数据接线。
//
// 为什么要适配层而不是直接让 trading 依赖 MarketOverviewProvider：
// 行情总览接口有 10 个方法，交易只用 4 个。直接依赖会让"给行情总览加个方法"
// 变成"可能影响交易编译"的事；反过来，交易侧想换数据源时也得实现一堆用不到的方法。
// 窄接口 + 一个只会转发的适配器，两边各自演化。

import (
	"context"
	"fmt"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading"
)

type tradingMarketIntelAdapter struct {
	inner MarketOverviewProvider
}

func (a tradingMarketIntelAdapter) Indexes(ctx context.Context, scope string) ([]foundation.MarketIndexSnapshot, foundation.SourceMeta, error) {
	return a.inner.MarketIndexes(ctx, scope)
}

func (a tradingMarketIntelAdapter) MarginSeries(ctx context.Context, limit int) ([]foundation.MarketMarginPoint, foundation.SourceMeta, error) {
	return a.inner.MarketMarginSeries(ctx, limit)
}

func (a tradingMarketIntelAdapter) BillboardDetail(ctx context.Context, symbol, tradeDate, reason string) (foundation.MarketBillboardDetail, foundation.SourceMeta, error) {
	return a.inner.MarketBillboardDetail(ctx, symbol, tradeDate, reason)
}

func (a tradingMarketIntelAdapter) Announcements(ctx context.Context, query, symbol, category string, limit int) ([]foundation.MarketResearchItem, foundation.SourceMeta, error) {
	return a.inner.MarketAnnouncements(ctx, query, symbol, category, limit)
}

// tradingNewsAdapter 把"全市场新闻"与"个股新闻检索"两个各自独立的源合成一个。
// 它们在仓库里本来就是两个接口，合成之后交易侧只需要一个注入点。
type tradingNewsAdapter struct {
	latest NewsProvider
	search StockNewsSearchProvider
}

func (a tradingNewsAdapter) LatestNews(ctx context.Context, limit int) ([]foundation.NewsItem, error) {
	if a.latest == nil {
		return nil, fmt.Errorf("全市场新闻源未配置")
	}
	return a.latest.LatestNews(ctx, limit)
}

func (a tradingNewsAdapter) SearchStockNews(ctx context.Context, symbol, query string, limit int) ([]foundation.NewsItem, error) {
	if a.search == nil {
		return nil, fmt.Errorf("个股新闻检索源未配置")
	}
	return a.search.SearchStockNews(ctx, symbol, query, limit)
}

// newTradingNewsAdapter 在两个源都没配置时返回 nil，
// 让交易侧保持"没有新闻能力"的状态，而不是拿到一个每次调用都报错的对象——
// 后者会让提示词里出现一个永远不会成功的工具，白耗模型的调研步数。
func newTradingNewsAdapter(latest NewsProvider, search StockNewsSearchProvider) trading.NewsSearchProvider {
	if latest == nil && search == nil {
		return nil
	}
	return tradingNewsAdapter{latest: latest, search: search}
}
