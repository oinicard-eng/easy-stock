package universe

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
)

// entry 构造一条测试用快照记录。
func entry(symbol, name, industry string, price, change, fiveDay, amount float64) foundation.StockCatalogEntry {
	return foundation.StockCatalogEntry{
		BoardStock: foundation.BoardStock{
			Symbol:               symbol,
			Name:                 name,
			Price:                price,
			ChangePercent:        change,
			FiveDayChangePercent: fiveDay,
			Amount:               amount,
		},
		Industry: industry,
	}
}

// marketOf 用真实认知逻辑构造市场视图，避免测试里手写一个与生产不一致的状态。
func marketOf(entries []foundation.StockCatalogEntry, now time.Time) cognition.MarketView {
	return cognition.View(entries, now)
}

// noise 构造一批用于填充样本量的普通标的，使市场状态判定有足够样本，
// 同时把涨跌停统计压到不会触发 panic 的水平。
//
// 行业与板块必须分散：候选池带同行业/同板块上限（这是刻意设计的约束），
// 若测试数据全是同一行业同一板块，会被约束正确地筛到只剩几只，
// 从而测不到 Limit 之类的参数。
func noise(count int, industryPrefix string) []foundation.StockCatalogEntry {
	items := make([]foundation.StockCatalogEntry, 0, count)
	for index := 0; index < count; index++ {
		prefix, suffix := "60", "SH"
		if index%2 == 1 {
			prefix, suffix = "00", "SZ"
		}
		items = append(items, entry(
			fmt.Sprintf("%s%04d.%s", prefix, 1000+index, suffix),
			"噪声股", fmt.Sprintf("%s%02d", industryPrefix, index%20), 12, 0.8, 2.0, 3e8))
	}
	return items
}

func pad(value int) string {
	digits := []byte{'0', '0', '0', '0', '0', '0'}
	for position := len(digits) - 1; position >= 0 && value > 0; position-- {
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits)
}

func TestBuildExcludesByHardRules(t *testing.T) {
	now := time.Now()
	entries := noise(600, "综合")
	entries = append(entries,
		entry("600001.SH", "ST风险", "综合", 5.2, 5.0, 8, 4e8),
		entry("600002.SH", "退市提醒", "综合", 1.2, 1.0, 2, 3e8),
		entry("600003.SH", "已涨停股", "综合", 11, 10.0, 12, 6e8),
		entry("600004.SH", "已跌停股", "综合", 9, -10.0, -15, 5e8),
		entry("600005.SH", "停牌股", "综合", 0, 0, 0, 0),
		entry("600006.SH", "低价股", "综合", 1.85, 2.0, 3, 4e8),
		entry("830001.BJ", "北交股", "综合", 15, 3.0, 5, 4e8),
	)

	board := Build(entries, marketOf(entries, now), DefaultOptions())

	rejected := map[string]string{}
	for _, item := range board.Exclusions {
		rejected[item.Reason] = item.Samples[0]
	}
	expectations := []struct {
		reason string
		sample string
	}{
		{"ST 或退市风险警示", "600001.SH"},
		{"ST 或退市风险警示", "600002.SH"},
		{"已涨停或接近涨停", "600003.SH"},
		{"已跌停或接近跌停", "600004.SH"},
		{"停牌或无有效价格", "600005.SH"},
		{"股价低于", "600006.SH"},
		{"北交所", "830001.BJ"},
	}
	for _, expectation := range expectations {
		found := false
		for _, item := range board.Exclusions {
			if !strings.Contains(item.Reason, expectation.reason) {
				continue
			}
			for _, sample := range item.Samples {
				if strings.HasPrefix(sample, expectation.sample) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s 应被「%s」排除，实际排除项：%v", expectation.sample, expectation.reason, board.Exclusions)
		}
	}

	for _, candidate := range board.Candidates {
		switch candidate.Symbol {
		case "600001.SH", "600002.SH", "600003.SH", "600004.SH", "600005.SH", "600006.SH", "830001.BJ":
			t.Errorf("%s 不应出现在候选池", candidate.Symbol)
		}
	}
}

func TestBuildRespectsLimit(t *testing.T) {
	entries := noise(800, "综合")
	options := DefaultOptions()
	options.Limit = 12
	board := Build(entries, marketOf(entries, time.Now()), options)
	if len(board.Candidates) != 12 {
		t.Fatalf("候选数 = %d, want 12", len(board.Candidates))
	}
	for index, candidate := range board.Candidates {
		if candidate.Rank != index+1 {
			t.Fatalf("第 %d 位 rank = %d", index, candidate.Rank)
		}
	}
}

// 恐慌与数据不足时一律返回空池：这不是筛选问题，而是不该开新仓。
func TestBuildReturnsEmptyWhenMarketBlocked(t *testing.T) {
	entries := noise(600, "综合")
	blocked := cognition.MarketView{Regime: cognition.RegimePanic, Label: "恐慌", RiskBudgetPercent: 20}
	board := Build(entries, blocked, DefaultOptions())
	if len(board.Candidates) != 0 {
		t.Fatalf("恐慌状态下候选数 = %d, want 0", len(board.Candidates))
	}
	if len(board.Exclusions) == 0 {
		t.Fatal("应说明未开仓的原因")
	}

	unknown := cognition.MarketView{Regime: cognition.RegimeUnknown, Label: "数据不足"}
	board = Build(entries, unknown, DefaultOptions())
	if len(board.Candidates) != 0 {
		t.Fatalf("数据不足时候选数 = %d, want 0", len(board.Candidates))
	}
}

// 缩量行情下应逐级放宽成交额门槛，而不是直接给空池。
func TestBuildDegradesThresholdWhenMarketIsThin(t *testing.T) {
	entries := make([]foundation.StockCatalogEntry, 0, 700)
	for index := 0; index < 700; index++ {
		// 全部标的成交额只有 2000 万，低于默认 5000 万门槛。
		prefix, suffix := "60", "SH"
		if index%2 == 1 {
			prefix, suffix = "00", "SZ"
		}
		entries = append(entries, entry(fmt.Sprintf("%s%04d.%s", prefix, 1000+index, suffix),
			"缩量股", fmt.Sprintf("缩量行业%02d", index%20), 12, 0.6, 1.5, 2e7))
	}
	board := Build(entries, marketOf(entries, time.Now()), DefaultOptions())
	if len(board.Candidates) == 0 {
		t.Fatal("缩量时应降级门槛而不是返回空池")
	}
	if !board.Degraded {
		t.Fatal("应标记门槛已降级")
	}
	if board.ThresholdAmount >= 5000*1e4 {
		t.Fatalf("门槛 = %.0f, 应低于 5000 万", board.ThresholdAmount)
	}
}

// 启动区间的标的应排在过热标的之前：涨停附近买入成本最差，也最容易接盘。
//
// 两类标的各铺 12 个互不相同的行业、板块交替，否则会被"同行业最多 3 只"
// 的分散约束正确地截断，测不到打分本身。
func TestBuildRanksLaunchAboveOverheated(t *testing.T) {
	now := time.Now()
	entries := noise(600, "背景行业")
	for index := 0; index < 12; index++ {
		entries = append(entries, entry(
			fmt.Sprintf("603%03d.SH", index), "温和股", fmt.Sprintf("温和行业%02d", index),
			12, 3.0, 6, 3e8))
	}
	for index := 0; index < 12; index++ {
		entries = append(entries, entry(
			fmt.Sprintf("001%03d.SZ", index), "过热股", fmt.Sprintf("过热行业%02d", index),
			12, 8.5, 6, 3e8))
	}

	options := DefaultOptions()
	options.Limit = 60
	board := Build(entries, marketOf(entries, now), options)
	if len(board.Candidates) == 0 {
		t.Fatal("候选池不应为空")
	}
	mildSum, mildCount := 0.0, 0
	hotSum, hotCount := 0.0, 0
	for _, candidate := range board.Candidates {
		if strings.HasPrefix(candidate.Name, "温和股") {
			mildSum += candidate.Score
			mildCount++
		}
		if strings.HasPrefix(candidate.Name, "过热股") {
			hotSum += candidate.Score
			hotCount++
		}
	}
	if mildCount == 0 || hotCount == 0 {
		t.Fatalf("两类标的应都进入候选池：温和 %d 只、过热 %d 只", mildCount, hotCount)
	}
	mildAverage := mildSum / float64(mildCount)
	hotAverage := hotSum / float64(hotCount)
	if mildAverage <= hotAverage {
		t.Fatalf("启动区间平均分 %.2f 应高于过热区间 %.2f", mildAverage, hotAverage)
	}
}

// 分散约束必须真的生效：把大批同行业标的放进来，入选数不应超过上限。
func TestBuildCapsSameIndustry(t *testing.T) {
	entries := noise(600, "背景行业")
	for index := 0; index < 60; index++ {
		entries = append(entries, entry(
			fmt.Sprintf("605%03d.SH", index), "同行业股", "单一行业",
			12, 3.0, 6, 5e8))
	}
	options := DefaultOptions()
	options.Limit = 30
	board := Build(entries, marketOf(entries, time.Now()), options)

	count := 0
	for _, candidate := range board.Candidates {
		if candidate.Industry == "单一行业" {
			count++
		}
	}
	if count > options.MaxPerIndustry {
		t.Fatalf("单一行业入选 %d 只，超过上限 %d 只", count, options.MaxPerIndustry)
	}
	if count == 0 {
		t.Fatal("单一行业的优质标的应至少入选若干只")
	}
	found := false
	for _, exclusion := range board.Exclusions {
		if strings.Contains(exclusion.Reason, "同类风险敞口") {
			found = true
		}
	}
	if !found {
		t.Fatal("应记录被行业上限挡掉的标的")
	}
}

// 同板块也不能过度集中：否则"15 只候选里 11 只科创板"这种情况会重现。
func TestBuildCapsSameBoard(t *testing.T) {
	entries := noise(600, "背景行业")
	for index := 0; index < 80; index++ {
		entries = append(entries, entry(
			fmt.Sprintf("688%03d.SH", index), "科创股", fmt.Sprintf("科创行业%02d", index%20),
			12, 3.0, 6, 5e8))
	}
	options := DefaultOptions()
	options.Limit = 30
	board := Build(entries, marketOf(entries, time.Now()), options)

	count := 0
	for _, candidate := range board.Candidates {
		if strings.HasSuffix(candidate.Symbol, ".SH") && strings.HasPrefix(candidate.Symbol, "688") {
			count++
		}
	}
	limit := int(float64(options.Limit)*options.MaxPerBoardRatio) + 1
	if count > limit {
		t.Fatalf("科创板入选 %d 只，超过板块上限约 %d 只", count, limit)
	}
}

// 小资金买不起的高价股应被剔除：10 万账户买 500 元的股票，一手就是半个仓位。
func TestBuildDropsUnaffordableHighPricedStocks(t *testing.T) {
	entries := noise(600, "背景行业")
	entries = append(entries, entry("688999.SH", "高价股", "高价行业", 520, 3.0, 6, 20e8))
	entries = append(entries, entry("688998.SH", "低价股", "低价行业", 12, 3.0, 6, 20e8))

	options := DefaultOptions()
	options.Equity = 100000
	board := Build(entries, marketOf(entries, time.Now()), options)

	for _, candidate := range board.Candidates {
		if candidate.Symbol == "688999.SH" {
			t.Fatal("520 元的股票一手要 5.2 万，超过 10 万账户的 25%，应被剔除")
		}
	}
	found := false
	for _, exclusion := range board.Exclusions {
		if strings.Contains(exclusion.Reason, "单手金额") {
			found = true
		}
	}
	if !found {
		t.Fatal("应记录因单手金额过高而剔除的标的")
	}
}

func TestBuildCandidatesCarryReasons(t *testing.T) {
	entries := noise(600, "综合")
	board := Build(entries, marketOf(entries, time.Now()), DefaultOptions())
	if len(board.Candidates) == 0 {
		t.Fatal("候选池不应为空")
	}
	for _, candidate := range board.Candidates {
		if len(candidate.Reasons) < 3 {
			t.Fatalf("%s 的入选理由只有 %d 条，至少应有 3 条", candidate.Symbol, len(candidate.Reasons))
		}
		for _, reason := range candidate.Reasons {
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("%s 存在空理由", candidate.Symbol)
			}
		}
	}
}

func TestBuildScoresAreBounded(t *testing.T) {
	entries := noise(600, "综合")
	board := Build(entries, marketOf(entries, time.Now()), DefaultOptions())
	for _, candidate := range board.Candidates {
		if candidate.Score < 0 || candidate.Score > 100 {
			t.Fatalf("%s 分数 %.2f 越界，应在 0~100", candidate.Symbol, candidate.Score)
		}
	}
}

// 成交额相差三个数量级时，成交额高者应获得更高流动性分，但不能因此
// 把全部名额都给它——总分仍由动量与趋势主导。
func TestLiquidityScoreSeparatesOrdersOfMagnitude(t *testing.T) {
	low := liquidityScore(7.0, 7.0, 10.0)   // 约 1000 万
	high := liquidityScore(10.0, 7.0, 10.0) // 约 100 亿
	if high <= low {
		t.Fatalf("高成交额得分 %.1f 应高于低成交额 %.1f", high, low)
	}
	if low < 40 || high > 100 {
		t.Fatalf("得分应落在 40~100，实际 %.1f / %.1f", low, high)
	}
}

func TestBoardOfRecognizesBoards(t *testing.T) {
	cases := map[string]string{
		"600000.SH": "SH",
		"000001.SZ": "SZ",
		"830799.BJ": "BJ",
		"300750.SZ": "SZ",
		"688981.SH": "SH",
	}
	for symbol, want := range cases {
		if got := boardOf(symbol); got != want {
			t.Fatalf("boardOf(%s) = %s, want %s", symbol, got, want)
		}
	}
}
