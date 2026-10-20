package cognition

import (
	"math"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
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

// flatMarket 构造一个指定涨跌幅、指定数量的市场，用于测试阈值边界。
func flatMarket(change float64, count int) []foundation.StockCatalogEntry {
	entries := make([]foundation.StockCatalogEntry, 0, count)
	for index := 0; index < count; index++ {
		entries = append(entries, entry(
			"60000"+string(rune('0'+index%10))+".SH", "测试", "测试行业",
			10, change, change*3, 2e8))
	}
	return entries
}

func TestClassifyMapsBreadthToRegime(t *testing.T) {
	cases := []struct {
		name     string
		breadth  Breadth
		want     Regime
		wantBudg float64
	}{
		{
			name:     "普涨为上行",
			breadth:  Breadth{Total: 4000, Advancing: 3000, Declining: 900, MedianChange: 1.2, AdvanceRatio: 0.77, LimitUp: 40},
			want:     RegimeUptrend,
			wantBudg: 85,
		},
		{
			name:     "涨停成片且中位数大涨为亢奋",
			breadth:  Breadth{Total: 4000, Advancing: 3400, Declining: 500, MedianChange: 2.4, AdvanceRatio: 0.87, LimitUp: 120},
			want:     RegimeEuphoria,
			wantBudg: 65,
		},
		{
			name:     "多数下跌为退潮",
			breadth:  Breadth{Total: 4000, Advancing: 900, Declining: 3000, MedianChange: -1.1, AdvanceRatio: 0.23, LimitUp: 8},
			want:     RegimeEbb,
			wantBudg: 40,
		},
		{
			name:     "跌停成片为恐慌",
			breadth:  Breadth{Total: 4000, Advancing: 400, Declining: 3400, MedianChange: -4.2, AdvanceRatio: 0.11, LimitDown: 42},
			want:     RegimePanic,
			wantBudg: 20,
		},
		{
			name:     "涨跌互现为震荡",
			breadth:  Breadth{Total: 4000, Advancing: 1900, Declining: 1900, MedianChange: 0.05, AdvanceRatio: 0.5, LimitUp: 30},
			want:     RegimeRange,
			wantBudg: 55,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, budget := Classify(testCase.breadth)
			if got != testCase.want {
				t.Fatalf("regime = %s, want %s", got, testCase.want)
			}
			if budget != testCase.wantBudg {
				t.Fatalf("budget = %.0f, want %.0f", budget, testCase.wantBudg)
			}
		})
	}
}

// 亢奋时的仓位预算必须低于上行：越热越重仓是最常见的新手错误。
func TestEuphoriaBudgetIsLowerThanUptrend(t *testing.T) {
	uptrend, uptrendBudget := Classify(Breadth{Total: 4000, Advancing: 3000, Declining: 900, MedianChange: 1.2, AdvanceRatio: 0.77})
	euphoria, euphoriaBudget := Classify(Breadth{Total: 4000, Advancing: 3400, Declining: 500, MedianChange: 2.4, AdvanceRatio: 0.87, LimitUp: 120})
	if uptrend != RegimeUptrend || euphoria != RegimeEuphoria {
		t.Fatalf("前置状态不符：uptrend=%s euphoria=%s", uptrend, euphoria)
	}
	if euphoriaBudget >= uptrendBudget {
		t.Fatalf("亢奋预算 %.0f 不应不低于上行预算 %.0f", euphoriaBudget, uptrendBudget)
	}
}

// 样本不足时必须返回 unknown 且预算为 0：宁可不动，不可据残缺数据下注。
func TestClassifyRefusesThinSample(t *testing.T) {
	regime, budget := Classify(Breadth{Total: 120, Advancing: 100, Declining: 20, MedianChange: 2, AdvanceRatio: 0.83})
	if regime != RegimeUnknown {
		t.Fatalf("regime = %s, want unknown", regime)
	}
	if budget != 0 {
		t.Fatalf("budget = %.0f, want 0", budget)
	}
}

func TestRegimeValidBlocksPanicAndUnknown(t *testing.T) {
	blocked := []Regime{RegimeUnknown, RegimePanic}
	for _, regime := range blocked {
		if regime.Valid() {
			t.Fatalf("%s 应视为不可开仓", regime)
		}
	}
	allowed := []Regime{RegimeEbb, RegimeRange, RegimeUptrend, RegimeEuphoria}
	for _, regime := range allowed {
		if !regime.Valid() {
			t.Fatalf("%s 应视为可开仓", regime)
		}
	}
}

// ST 股涨停幅度只有 5%，涨 5% 即涨停；若按主板 10% 判断会漏计，
// 导致恐慌/亢奋判定失真。
func TestObserveCountsSTLimitUpAtFivePercent(t *testing.T) {
	entries := []foundation.StockCatalogEntry{
		entry("600001.SH", "ST测试", "测试行业", 5.25, 5.0, 8, 1e8),
		entry("600002.SH", "正常股", "测试行业", 11, 5.0, 3, 1e8),
	}
	observation := Observe(entries)
	if observation.Breadth.LimitUp != 1 {
		t.Fatalf("涨停家数 = %d, want 1（只有 ST 那只）", observation.Breadth.LimitUp)
	}
}

// 创业板 20% 涨停：涨 10% 不应被算作涨停，涨 20% 才应算。
func TestObserveRespectsChiNextLimit(t *testing.T) {
	entries := []foundation.StockCatalogEntry{
		entry("300001.SZ", "创业板甲", "测试行业", 12, 10.0, 5, 1e8),
		entry("300002.SZ", "创业板乙", "测试行业", 12, 20.0, 5, 1e8),
	}
	observation := Observe(entries)
	if observation.Breadth.LimitUp != 1 {
		t.Fatalf("涨停家数 = %d, want 1（只有 20%% 那只）", observation.Breadth.LimitUp)
	}
}

// 停牌标的没有价格，不应进入任何统计，否则会把广度指标稀释。
func TestObserveSkipsSuspended(t *testing.T) {
	entries := []foundation.StockCatalogEntry{
		entry("600001.SH", "正常股", "测试行业", 10, 2, 3, 1e8),
		entry("600002.SH", "停牌股", "测试行业", 0, 0, 0, 0),
	}
	observation := Observe(entries)
	if observation.Breadth.Total != 1 {
		t.Fatalf("Total = %d, want 1", observation.Breadth.Total)
	}
	if observation.Suspended != 1 {
		t.Fatalf("Suspended = %d, want 1", observation.Suspended)
	}
}

// 中位数必须不被极端值带偏：这是选择中位数而非均值的原因。
func TestObserveMedianResistsOutlier(t *testing.T) {
	values := []float64{-9.9, 0.2, 0.3, 0.4, 0.5, 9.9}
	got := median(values)
	if math.Abs(got-0.35) > 1e-9 {
		t.Fatalf("median = %v, want 0.35", got)
	}
}

func TestObserveIndustryHeatSortedByAverageChange(t *testing.T) {
	entries := make([]foundation.StockCatalogEntry, 0, 12)
	for index := 0; index < 6; index++ {
		entries = append(entries, entry("60001"+string(rune('0'+index))+".SH", "强势股", "强势行业", 10, 5, 10, 2e8))
	}
	for index := 0; index < 6; index++ {
		entries = append(entries, entry("60002"+string(rune('0'+index))+".SH", "弱势股", "弱势行业", 10, -3, -6, 2e8))
	}
	observation := Observe(entries)
	if len(observation.Industries) != 2 {
		t.Fatalf("行业数 = %d, want 2", len(observation.Industries))
	}
	if observation.Industries[0].Name != "强势行业" || observation.Industries[0].Rank != 1 {
		t.Fatalf("首位行业 = %s rank=%d, want 强势行业 rank=1",
			observation.Industries[0].Name, observation.Industries[0].Rank)
	}
	if observation.Industries[0].LeaderSymbol == "" {
		t.Fatal("行业应带龙头标的")
	}
}

// 成分股太少的行业不应进入榜单，否则两三个股票的小行业会长期霸榜。
func TestObserveDropsTinyIndustries(t *testing.T) {
	entries := []foundation.StockCatalogEntry{
		entry("600001.SH", "甲", "小行业", 10, 9, 20, 1e8),
		entry("600002.SH", "乙", "小行业", 10, 9, 20, 1e8),
	}
	for index := 0; index < 6; index++ {
		entries = append(entries, entry("60003"+string(rune('0'+index))+".SH", "丙", "大行业", 10, 1, 2, 1e8))
	}
	observation := Observe(entries)
	for _, heat := range observation.Industries {
		if heat.Name == "小行业" {
			t.Fatal("成分股不足的行业不应进入榜单")
		}
	}
}

func TestViewProducesReadableNotes(t *testing.T) {
	entries := flatMarket(1.2, 600)
	view := View(entries, time.Now())
	if view.Regime != RegimeUptrend {
		t.Fatalf("regime = %s, want uptrend", view.Regime)
	}
	if len(view.Notes) < 3 {
		t.Fatalf("说明条数 = %d, 至少应有 3 条", len(view.Notes))
	}
	if view.Label != "上行" {
		t.Fatalf("label = %s, want 上行", view.Label)
	}
}

// 市场预算与风控上限取较小值：市场不好时以市场为准。
func TestMaxExposureTakesStricterLimit(t *testing.T) {
	view := MarketView{RiskBudgetPercent: 40}
	if got := view.MaxExposurePercent(95); got != 40 {
		t.Fatalf("got %.0f, want 40", got)
	}
	if got := view.MaxExposurePercent(30); got != 30 {
		t.Fatalf("got %.0f, want 30", got)
	}
	blocked := MarketView{RiskBudgetPercent: 0}
	if got := blocked.MaxExposurePercent(95); got != 0 {
		t.Fatalf("got %.0f, want 0", got)
	}
}
