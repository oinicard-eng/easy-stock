package trading

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// catalogFixture 是一个可控的全市场快照，用于测试自动选股。
type catalogFixture struct {
	entries []foundation.StockCatalogEntry
	err     error
}

func (f catalogFixture) StockCatalog(context.Context) ([]foundation.StockCatalogEntry, error) {
	return f.entries, f.err
}

// anyQuote 对任意代码返回一条可用行情，避免测试里手写 30 条报价。
// 价格固定为 10 元、昨收 9.9 元，涨跌幅 1.01%，不会触发涨跌停判定。
type anyQuote struct{}

func (anyQuote) Realtime(_ context.Context, symbols []string) ([]foundation.Quote, error) {
	out := make([]foundation.Quote, 0, len(symbols))
	for _, symbol := range symbols {
		out = append(out, foundation.Quote{
			Symbol:        symbol,
			Name:          "候选股",
			Price:         10,
			PreviousClose: 9.9,
			High:          10.1,
			Low:           9.85,
			ChangePercent: 1.01,
			Amount:        5e8,
		})
	}
	return out, nil
}

// buildCatalog 构造一个有足够样本量的市场：
// 先铺 600 只中性标的让广度指标成立，再混入若干"应当入选"和"必须排除"的标的。
func buildCatalog() []foundation.StockCatalogEntry {
	entries := make([]foundation.StockCatalogEntry, 0, 620)
	for index := 0; index < 600; index++ {
		entries = append(entries, foundation.StockCatalogEntry{
			BoardStock: foundation.BoardStock{
				Symbol:               fmt.Sprintf("60%04d.SH", 1000+index),
				Name:                 "普通股",
				Price:                10,
				ChangePercent:        0.8,
				FiveDayChangePercent: 2.0,
				Amount:               3e8,
			},
			Industry: "制造业",
		})
	}
	// 一只非常合适的标的：温和上涨、5 日趋势向上、成交额充足。
	entries = append(entries, foundation.StockCatalogEntry{
		BoardStock: foundation.BoardStock{
			Symbol:               "600519.SH",
			Name:                 "贵州茅台",
			Price:                10,
			ChangePercent:        3.2,
			FiveDayChangePercent: 8.6,
			Amount:               42e8,
		},
		Industry: "白酒",
	})
	// 必须被排除的标的。
	entries = append(entries,
		foundation.StockCatalogEntry{BoardStock: foundation.BoardStock{Symbol: "600001.SH", Name: "ST退风险", Price: 5.2, ChangePercent: 5, Amount: 4e8}, Industry: "制造业"},
		foundation.StockCatalogEntry{BoardStock: foundation.BoardStock{Symbol: "600002.SH", Name: "已涨停", Price: 11, ChangePercent: 10, Amount: 6e8}, Industry: "制造业"},
		foundation.StockCatalogEntry{BoardStock: foundation.BoardStock{Symbol: "600003.SH", Name: "停牌股", Price: 0, ChangePercent: 0, Amount: 0}, Industry: "制造业"},
		foundation.StockCatalogEntry{BoardStock: foundation.BoardStock{Symbol: "830001.BJ", Name: "北交股", Price: 15, ChangePercent: 3, Amount: 4e8}, Industry: "制造业"},
	)
	return entries
}

func TestServiceAutoSelectsUniverseWhenWatchlistEmpty(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	content := `{"regime":"进攻","summary":"自动选股测试","orders":[{"symbol":"600519.SH","name":"贵州茅台","side":"buy","target_weight_percent":5,"order_type":"limit","limit_price":10.05,"stop_loss_price":9.5,"thesis":"测试","confidence":0.6}]}`
	service := NewService(store, fakeGateway{content: content}, anyQuote{}, catalogFixture{entries: buildCatalog()}, log.New(io.Discard, "", 0))

	// 不传 watchlist：系统应当自行从全市场选股。
	plan, err := service.StartPlan(context.Background(), PlanRequest{TraderProfile: "balanced"})
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	finished := waitForPlan(t, store, plan.ID)
	if finished.Status != PlanStatusDraft {
		t.Fatalf("计划状态 = %s，错误信息 = %s", finished.Status, finished.ErrorMessage)
	}

	if !finished.Snapshot.AutoUniverse {
		t.Fatal("应标记为自动选股")
	}
	if finished.Snapshot.Board == nil {
		t.Fatal("自动选股时应带上候选池明细")
	}
	if len(finished.Snapshot.Universe) == 0 {
		t.Fatal("候选池不应为空")
	}
	if len(finished.Snapshot.Universe) > 30 {
		t.Fatalf("候选池 %d 只，超过上限 30 只", len(finished.Snapshot.Universe))
	}
	if !finished.Snapshot.Market.Evaluated {
		t.Fatal("自动选股必须经过市场认知")
	}

	// 候选池里不应出现被硬性规则排除的标的。
	banned := map[string]bool{"600001.SH": true, "600002.SH": true, "600003.SH": true, "830001.BJ": true}
	for _, symbol := range finished.Snapshot.Universe {
		if banned[symbol] {
			t.Fatalf("%s 不应进入候选池", symbol)
		}
	}
	// 优质标的应当入选，否则说明打分把好票筛掉了。
	found := false
	for _, symbol := range finished.Snapshot.Universe {
		if symbol == "600519.SH" {
			found = true
		}
	}
	if !found {
		t.Fatalf("600519.SH 应进入候选池，实际候选：%v", finished.Snapshot.Universe)
	}
	for _, candidate := range finished.Snapshot.Board.Candidates {
		if len(candidate.Reasons) == 0 {
			t.Fatalf("%s 缺少入选理由", candidate.Symbol)
		}
	}
}

// 行情源不可用时，自动选股必须失败并说明原因，而不是拿空池去问模型。
func TestServiceAutoUniverseFailsWithoutCatalog(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service := NewService(store, fakeGateway{}, anyQuote{}, nil, log.New(io.Discard, "", 0))
	if _, err := service.StartPlan(context.Background(), PlanRequest{}); err == nil {
		t.Fatal("未配置全市场行情源时应直接拒绝自动选股")
	}
}

// 全市场快照拉取失败时，自动选股不应继续。
func TestServiceAutoUniverseFailsWhenCatalogErrors(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service := NewService(store, fakeGateway{content: "{}"}, anyQuote{},
		catalogFixture{err: fmt.Errorf("上游超时")}, log.New(io.Discard, "", 0))
	plan, err := service.StartPlan(context.Background(), PlanRequest{})
	if err != nil {
		t.Fatalf("启动计划不应立即失败：%v", err)
	}
	finished := waitForPlan(t, store, plan.ID)
	if finished.Status != PlanStatusFailed {
		t.Fatalf("状态 = %s, want failed", finished.Status)
	}
	if finished.ErrorMessage == "" {
		t.Fatal("失败时应写明原因")
	}
}

// 显式指定标的时不做自动选股，且市场认知失败不应阻塞计划。
func TestServiceKeepsExplicitWatchlist(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	content := `{"regime":"进攻","summary":"显式标的测试","orders":[{"symbol":"600519.SH","name":"贵州茅台","side":"buy","target_weight_percent":5,"order_type":"limit","limit_price":10.05,"stop_loss_price":9.5,"thesis":"测试"}]}`
	service := NewService(store, fakeGateway{content: content}, anyQuote{},
		catalogFixture{err: fmt.Errorf("上游超时")}, log.New(io.Discard, "", 0))

	plan, err := service.StartPlan(context.Background(), PlanRequest{Watchlist: []string{"600519.SH"}})
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	finished := waitForPlan(t, store, plan.ID)
	if finished.Snapshot.AutoUniverse {
		t.Fatal("显式指定标的时不应标记自动选股")
	}
	if finished.Snapshot.Board != nil {
		t.Fatal("显式指定标的时不应生成候选池")
	}
	if finished.Status != PlanStatusDraft {
		t.Fatalf("状态 = %s，市场认知失败不应阻塞显式标的的计划：%s", finished.Status, finished.ErrorMessage)
	}
}

// 市场恐慌时不自动选股，因为空候选池本身就是"不该开仓"的结论。
func TestServiceAutoUniverseStopsWhenMarketPanics(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 构造跌停成片的市场。
	entries := make([]foundation.StockCatalogEntry, 0, 600)
	for index := 0; index < 600; index++ {
		entries = append(entries, foundation.StockCatalogEntry{
			BoardStock: foundation.BoardStock{
				Symbol:        fmt.Sprintf("60%04d.SH", index),
				Name:          "跌停股",
				Price:         9,
				ChangePercent: -10,
				Amount:        3e8,
			},
			Industry: "制造业",
		})
	}
	service := NewService(store, fakeGateway{content: "{}"}, anyQuote{},
		catalogFixture{entries: entries}, log.New(io.Discard, "", 0))

	plan, err := service.StartPlan(context.Background(), PlanRequest{})
	if err != nil {
		t.Fatalf("启动计划不应立即失败：%v", err)
	}
	finished := waitForPlan(t, store, plan.ID)
	if finished.Status != PlanStatusFailed {
		t.Fatalf("状态 = %s, want failed", finished.Status)
	}
	if finished.Snapshot.Market.Regime != "panic" {
		t.Fatalf("市场状态 = %s, want panic", finished.Snapshot.Market.Regime)
	}
}

// waitForPlan 轮询计划直到离开 running 状态。异步生成不能用固定 sleep，
// 那会让测试在慢机器上偶发失败。
func waitForPlan(t *testing.T, store *Store, planID string) Plan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		plan, err := store.GetPlan(context.Background(), planID)
		if err != nil {
			t.Fatalf("读取计划失败：%v", err)
		}
		if plan.Status != PlanStatusRunning {
			return plan
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("计划 %s 在 10 秒内未完成", planID)
	return Plan{}
}
