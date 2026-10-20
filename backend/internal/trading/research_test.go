package trading

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/appsettings"
	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/universe"
)

// universeBoardStub 是本轮候选池的桩：两只银行、一只算力，
// 用于验证候选依据输出与"相对行业中位数"的计算。
var universeBoardStub = universe.Board{Candidates: []universe.Candidate{
	{Symbol: "300308", Name: "中际旭创", Industry: "通信设备", Price: 128, ChangePercent: 3.23, Score: 88, Rank: 1,
		Reasons: []string{"5 日涨幅 +9.2%，趋势延续"}},
	{Symbol: "600000", Name: "浦发银行", Industry: "银行", Price: 10, ChangePercent: 2.04, Score: 71, Rank: 2,
		Reasons: []string{"成交额 3.2 亿，流动性充足"}},
	{Symbol: "600036", Name: "招商银行", Industry: "银行", Price: 35, ChangePercent: 1.45, Score: 68, Rank: 3,
		Reasons: []string{"行业内相对强度转正"}},
}}

// scriptedGateway 按脚本依次返回内容，用来驱动调研的多轮调用。
// 脚本用完后返回最后一条，避免测试因为次数没算准而挂在一个与断言无关的地方。
type scriptedGateway struct {
	replies []string
	calls   int
}

func (g *scriptedGateway) Prompt(context.Context, string) (agent.PromptResult, error) {
	if len(g.replies) == 0 {
		return agent.PromptResult{Content: `{"thought":"无脚本","action":"done","conclusion":""}`}, nil
	}
	index := g.calls
	if index >= len(g.replies) {
		index = len(g.replies) - 1
	}
	g.calls++
	return agent.PromptResult{Content: g.replies[index]}, nil
}

func (g *scriptedGateway) Status() agent.Status { return agent.Status{Available: true} }
func (g *scriptedGateway) ModelAPIKey() (string, error) {
	return "", nil
}
func (g *scriptedGateway) SyncLLM(appsettings.LLM, *string) error { return nil }
func (g *scriptedGateway) Start(context.Context) (agent.Process, error) {
	return nil, nil
}

// researchToolbox 构造一个带两行业、四只票的工具上下文。
func researchToolbox(t *testing.T) (*Store, *Toolbox) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 用真实行情源的带后缀写法：工具必须能和不带后缀的入参对上。
	quotes := []foundation.Quote{
		{Symbol: "600000.SH", Name: "浦发银行", Price: 10, PreviousClose: 9.8, High: 10.2, Low: 9.7, ChangePercent: 2.04, Amount: 3.2e8},
		{Symbol: "600036.SH", Name: "招商银行", Price: 35, PreviousClose: 34.5, High: 35.4, Low: 34.3, ChangePercent: 1.45, Amount: 5.1e8},
		{Symbol: "300308.SZ", Name: "中际旭创", Price: 128, PreviousClose: 124, High: 131, Low: 123, ChangePercent: 3.23, Amount: 2.4e9},
		{Symbol: "688775.SH", Name: "影石创新", Price: 88, PreviousClose: 80, High: 90, Low: 79, ChangePercent: 8.97, Amount: 1.8e9},
	}
	catalog := map[string]foundation.StockCatalogEntry{
		"600000": {BoardStock: foundation.BoardStock{Symbol: "600000", Name: "浦发银行", Price: 10, ChangePercent: 2.04, Amount: 3.2e8}, Industry: "银行"},
		"600036": {BoardStock: foundation.BoardStock{Symbol: "600036", Name: "招商银行", Price: 35, ChangePercent: 1.45, Amount: 5.1e8}, Industry: "银行"},
		"300308": {BoardStock: foundation.BoardStock{Symbol: "300308", Name: "中际旭创", Price: 128, ChangePercent: 3.23, Amount: 2.4e9}, Industry: "通信设备"},
		"688775": {BoardStock: foundation.BoardStock{Symbol: "688775.SH", Name: "影石创新", Price: 88, ChangePercent: 8.97, Amount: 1.8e9}, Industry: "电子设备制造"},
	}
	snapshot := Snapshot{
		Quotes:  quotes,
		Equity:  100000,
		Cash:    100000,
		Sectors: map[string]string{"300308.SZ": "科技", "688775.SH": "科技", "600000.SH": "金融"},
		Market:  testMarketView(),
	}
	// 候选池用来验证"相对行业中位数"与候选依据输出。
	snapshot.Board = &universeBoardStub
	return store, &Toolbox{
		store:     store,
		accountID: DefaultAccountID,
		snapshot:  snapshot,
		catalog:   catalog,
		now:       time.Now(),
	}
}

func TestToolboxRejectsUnknownTool(t *testing.T) {
	_, toolbox := researchToolbox(t)
	_, err := toolbox.Call(context.Background(), "drop_tables", nil)
	if err == nil {
		t.Fatal("未知工具必须报错，否则模型会以为它存在")
	}
	if !strings.Contains(err.Error(), "list_sector_flow") {
		t.Fatalf("错误信息应列出可用工具，实际：%v", err)
	}
}

func TestToolboxSymbolNormalization(t *testing.T) {
	_, toolbox := researchToolbox(t)
	for _, raw := range []string{"300308", "300308.SZ", "sz300308", " 300308 "} {
		outcome, err := toolbox.Call(context.Background(), "get_symbol_detail", map[string]any{"symbol": raw})
		if err != nil {
			t.Fatalf("%q 应能归一化到 300308，实际报错：%v", raw, err)
		}
		if !strings.Contains(outcome.Text, "中际旭创") {
			t.Fatalf("%q 查到的内容不对：%s", raw, outcome.Text)
		}
	}
}

func TestToolboxRejectsSymbolOutsideSnapshot(t *testing.T) {
	_, toolbox := researchToolbox(t)
	_, err := toolbox.Call(context.Background(), "get_symbol_detail", map[string]any{"symbol": "999999"})
	if err == nil {
		t.Fatal("快照外的标的不允许查询：否则模型会基于一个不存在的价格下单")
	}
}

func TestToolboxListsStocksByIndustry(t *testing.T) {
	_, toolbox := researchToolbox(t)
	outcome, err := toolbox.Call(context.Background(), "list_sector_stocks", map[string]any{"sector": "银行"})
	if err != nil {
		t.Fatalf("按行业查询失败：%v", err)
	}
	if outcome.Rows != 2 {
		t.Fatalf("银行应有 2 只，实际 %d：%s", outcome.Rows, outcome.Text)
	}
	// 按涨跌幅降序：浦发 2.04 在招行 1.45 之前。
	if strings.Index(outcome.Text, "600000") > strings.Index(outcome.Text, "600036") {
		t.Fatalf("应按涨跌幅降序，实际：%s", outcome.Text)
	}
}

func TestToolboxCompareComputesRelativeStrength(t *testing.T) {
	_, toolbox := researchToolbox(t)
	outcome, err := toolbox.Call(context.Background(), "compare_symbols", map[string]any{
		"symbols": []any{"600000", "600036"},
	})
	if err != nil {
		t.Fatalf("对比失败：%v", err)
	}
	if !strings.Contains(outcome.Text, "较行业中位") {
		t.Fatalf("对比结果缺少相对强弱列：%s", outcome.Text)
	}
}

func TestToolboxSectorFlowWithoutProvider(t *testing.T) {
	_, toolbox := researchToolbox(t)
	_, err := toolbox.Call(context.Background(), "list_sector_flow", map[string]any{"dimension": "concept"})
	if err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("没有资金流数据源时必须明确报未配置，实际：%v", err)
	}
}

func TestToolboxRejectsBadDimension(t *testing.T) {
	_, toolbox := researchToolbox(t)
	toolbox.sectorFlow = staticSectorFlow{}
	_, err := toolbox.Call(context.Background(), "list_sector_flow", map[string]any{"dimension": "行业"})
	if err == nil || !strings.Contains(err.Error(), "concept") {
		t.Fatalf("非法 dimension 应提示允许值，实际：%v", err)
	}
}

func TestResearchCollectsStepsThenConcludes(t *testing.T) {
	store, toolbox := researchToolbox(t)
	gateway := &scriptedGateway{replies: []string{
		`{"thought":"先看资金流向","action":"tool","tool":"list_sector_stocks","args":{"sector":"银行"}}`,
		`{"thought":"够了","action":"done","conclusion":"银行板块两只标的当日普涨"} `,
	}}
	service := NewService(store, gateway, nil, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(true)

	result := service.runResearch(context.Background(), ResearchInput{
		PlanID: "plan-test", AccountID: DefaultAccountID,
		Snapshot: toolbox.snapshot, Toolbox: toolbox, Now: time.Now(),
	})

	if !result.Enabled {
		t.Fatal("调研应处于启用状态")
	}
	if result.Degraded {
		t.Fatalf("正常脚本不应降级：%s", result.Note)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("应有 1 步工具调用，实际 %d", len(result.Steps))
	}
	if result.Steps[0].Tool != "list_sector_stocks" || result.Steps[0].Rows != 2 {
		t.Fatalf("第一步记录不对：%+v", result.Steps[0])
	}
	if !strings.Contains(result.Conclusion, "普涨") {
		t.Fatalf("结论未保留：%q", result.Conclusion)
	}
}

func TestResearchStopsAtStepLimit(t *testing.T) {
	store, toolbox := researchToolbox(t)
	// 模型一直要求查，验证代码侧的上限真的能收住它。
	gateway := &scriptedGateway{replies: []string{
		`{"thought":"一","action":"tool","tool":"list_sector_stocks","args":{"sector":"银行"}}`,
	}}
	service := NewService(store, gateway, nil, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(true)

	result := service.runResearch(context.Background(), ResearchInput{
		PlanID: "plan-limit", AccountID: DefaultAccountID,
		Snapshot: toolbox.snapshot, Toolbox: toolbox, Now: time.Now(),
	})
	if len(result.Steps) != maxResearchSteps {
		t.Fatalf("步数应被限制在 %d，实际 %d", maxResearchSteps, len(result.Steps))
	}
	if result.Conclusion == "" {
		t.Fatal("达到上限也要给出收尾说明，否则提示词里会出现空结论")
	}
}

func TestResearchDegradesOnBadOutput(t *testing.T) {
	store, toolbox := researchToolbox(t)
	gateway := &scriptedGateway{replies: []string{"我觉得银行板块不错，建议买入。"}}
	service := NewService(store, gateway, nil, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(true)

	result := service.runResearch(context.Background(), ResearchInput{
		PlanID: "plan-bad", AccountID: DefaultAccountID,
		Snapshot: toolbox.snapshot, Toolbox: toolbox, Now: time.Now(),
	})
	if !result.Degraded {
		t.Fatal("无法解析的输出必须标记降级，而不是静默继续")
	}
	if result.Note == "" {
		t.Fatal("降级必须留下原因")
	}
}

func TestResearchToolErrorIsRecordedAndFedBack(t *testing.T) {
	store, toolbox := researchToolbox(t)
	gateway := &scriptedGateway{replies: []string{
		`{"thought":"查个不存在的工具","action":"tool","tool":"get_news","args":{}}`,
		`{"thought":"改查详情","action":"tool","tool":"get_symbol_detail","args":{"symbol":"600000"}}`,
		`{"thought":"够了","action":"done","conclusion":"ok"}`,
	}}
	service := NewService(store, gateway, nil, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(true)

	result := service.runResearch(context.Background(), ResearchInput{
		PlanID: "plan-err", AccountID: DefaultAccountID,
		Snapshot: toolbox.snapshot, Toolbox: toolbox, Now: time.Now(),
	})
	if len(result.Steps) != 2 {
		t.Fatalf("工具报错也应记一步，实际 %d 步", len(result.Steps))
	}
	if result.Steps[0].Error == "" {
		t.Fatal("第一步应记录工具错误")
	}
	if result.Steps[1].Error != "" {
		t.Fatalf("第二步应成功，实际错误：%s", result.Steps[1].Error)
	}
}

func TestResearchStepsPersistAndReload(t *testing.T) {
	store, toolbox := researchToolbox(t)
	gateway := &scriptedGateway{replies: []string{
		`{"thought":"查银行","action":"tool","tool":"list_sector_stocks","args":{"sector":"银行"}}`,
		`{"thought":"够了","action":"done","conclusion":"ok"}`,
	}}
	service := NewService(store, gateway, nil, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(true)

	input := ResearchInput{PlanID: "plan-persist", AccountID: DefaultAccountID,
		Snapshot: toolbox.snapshot, Toolbox: toolbox, Now: time.Now()}
	result := service.runResearch(context.Background(), input)
	service.saveResearch(context.Background(), input, result)

	steps, err := service.ResearchSteps(context.Background(), "plan-persist")
	if err != nil {
		t.Fatalf("读取回放失败：%v", err)
	}
	if len(steps) != 1 || steps[0].Tool != "list_sector_stocks" {
		t.Fatalf("回放内容不对：%+v", steps)
	}

	// 重跑同一计划应整批替换，而不是留下两条时间线。
	input2 := input
	service.saveResearch(context.Background(), input2, result)
	again, _ := service.ResearchSteps(context.Background(), "plan-persist")
	if len(again) != 1 {
		t.Fatalf("重跑应替换而不是追加，实际 %d 步", len(again))
	}
}

func TestMemorySyncIsIdempotent(t *testing.T) {
	store, _ := researchToolbox(t)
	ctx := context.Background()
	seedClosedTrade(t, store, "close-1")

	added, err := store.SyncMemories(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if added != 1 {
		t.Fatalf("应新增 1 条，实际 %d", added)
	}
	again, err := store.SyncMemories(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("重复同步失败：%v", err)
	}
	if again != 0 {
		t.Fatalf("重复同步不应新增，实际 %d", again)
	}
}

func TestMemoryCapturesRegimeAndThesis(t *testing.T) {
	store, _ := researchToolbox(t)
	ctx := context.Background()
	seedClosedTrade(t, store, "close-2")

	if _, err := store.SyncMemories(ctx, DefaultAccountID); err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	memories, err := store.SearchMemories(ctx, MemoryQuery{AccountID: DefaultAccountID})
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("应有 1 条记忆，实际 %d", len(memories))
	}
	memory := memories[0]
	if memory.Regime != "range" {
		t.Fatalf("市场状态应取自计划快照，实际 %q", memory.Regime)
	}
	if !strings.Contains(memory.Thesis, "算力") {
		t.Fatalf("买入理由应保留原文，实际 %q", memory.Thesis)
	}
	// 成本 100、峰值 118、卖 104：峰值浮盈 18%，回吐 14 个百分点。
	if memory.PeakGainPercent != 18 || memory.GiveBackPercent != 14 {
		t.Fatalf("峰值回吐计算不对：peak=%.2f giveBack=%.2f", memory.PeakGainPercent, memory.GiveBackPercent)
	}
	if memory.Outcome != "win" {
		t.Fatalf("结果判档不对：%s", memory.Outcome)
	}
	if !strings.Contains(memory.Lesson, "回吐") {
		t.Fatalf("教训应指出峰值回吐，实际 %q", memory.Lesson)
	}
}

func TestMemorySearchFilters(t *testing.T) {
	store, _ := researchToolbox(t)
	ctx := context.Background()
	seedClosedTrade(t, store, "close-3")
	if _, err := store.SyncMemories(ctx, DefaultAccountID); err != nil {
		t.Fatalf("同步失败：%v", err)
	}

	match, err := store.SearchMemories(ctx, MemoryQuery{AccountID: DefaultAccountID, Regime: "range", Outcome: "win"})
	if err != nil || len(match) != 1 {
		t.Fatalf("按市场状态+结果应命中 1 条，实际 %d（err=%v）", len(match), err)
	}
	none, err := store.SearchMemories(ctx, MemoryQuery{AccountID: DefaultAccountID, Regime: "panic"})
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	if len(none) != 0 {
		t.Fatalf("不匹配的条件应返回空，实际 %d", len(none))
	}
	keyword, _ := store.SearchMemories(ctx, MemoryQuery{AccountID: DefaultAccountID, Keyword: "算力"})
	if len(keyword) != 1 {
		t.Fatalf("关键词应命中买入理由，实际 %d", len(keyword))
	}
}

// seedClosedTrade 造一笔"买入 100 → 峰值 118 → 卖 104"的平仓，
// 并补上产生它的计划与买单，用于验证记忆能从快照还原情境。
func seedClosedTrade(t *testing.T, store *Store, tradeID string) {
	t.Helper()
	ctx := context.Background()
	cycleID := "cycle-" + tradeID
	planID := "plan-" + tradeID

	opened := time.Now().Add(-72 * time.Hour)
	plan := Plan{
		ID: planID, AccountID: DefaultAccountID, Status: PlanStatusExecuted,
		Regime: "range", CycleID: cycleID,
		Orders: []Order{{
			ID: "order-" + tradeID, PlanID: planID, AccountID: DefaultAccountID,
			CycleID: cycleID, Symbol: "300308", Name: "中际旭创", Side: SideBuy,
			Thesis: "算力板块主力资金连续三日净流入，5 日强度居前", Invalidation: "板块资金转为净流出", Confidence: 0.62,
		}},
		Snapshot:  Snapshot{TakenAt: opened, Market: testMarketView()},
		CreatedAt: opened, UpdatedAt: opened,
	}
	if err := store.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("保存计划失败：%v", err)
	}
	if _, err := store.CreateOrders(ctx, plan.Orders); err != nil {
		t.Fatalf("保存订单失败：%v", err)
	}

	trade := ClosedTrade{
		ID: tradeID, AccountID: DefaultAccountID, Symbol: "300308", Name: "中际旭创", Sector: "科技",
		Quantity: 100, EntryPrice: 100, ExitPrice: 104, PnL: 396, PnLPercent: 4,
		Trigger: "trail_stop", Source: "protective", OrderID: "order-" + tradeID,
		CycleID: cycleID, OpenedAt: &opened, ClosedAt: time.Now(), HoldDays: 3,
		TradingDay: TradingDay(time.Now()), PeakPrice: 118,
	}
	payload, err := jsonMarshal(trade)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO trading_closed_trades
		(id,account_id,symbol,trading_day,trigger,content_json,closed_at) VALUES (?,?,?,?,?,?,?)`,
		trade.ID, trade.AccountID, trade.Symbol, trade.TradingDay, trade.Trigger, payload, nowText(trade.ClosedAt)); err != nil {
		t.Fatalf("写入平仓记录失败：%v", err)
	}
}

func jsonMarshal(value any) (string, error) {
	payload, err := json.Marshal(value)
	return string(payload), err
}

func testMarketView() cognition.MarketView {
	return cognition.MarketView{
		Evaluated: true, Regime: cognition.RegimeRange, Label: "震荡", RiskBudgetPercent: 40,
		Industries: []cognition.IndustryHeat{
			{Name: "银行", AverageChange: 1.7, Advancing: 2, Total: 2, Rank: 1, LeaderName: "浦发银行", LeaderSymbol: "600000"},
			{Name: "通信设备", AverageChange: 3.2, Advancing: 1, Total: 1, Rank: 2, LeaderName: "中际旭创", LeaderSymbol: "300308"},
		},
	}
}

// staticSectorFlow 是资金流数据的静态桩，只为验证参数校验路径。
type staticSectorFlow struct{}

func (staticSectorFlow) SectorFlowRanking(context.Context, string, int) ([]foundation.SectorFlowRank, foundation.SourceMeta, error) {
	return []foundation.SectorFlowRank{
		{Code: "BK0917", Name: "算力概念", ChangePercent: 2.1, MainNetInflow: 1.2e9, MainNetInflowRatio: 4.5},
	}, foundation.SourceMeta{Source: "test"}, nil
}

// 这个用例守的是一个真实踩过的坑：行情源给的是 688775.SH，模型参数写 688775，
// 早期实现按字符串相等比较，于是所有带后缀的标的都查不到行情。
// 模型连续三步撞在同一堵墙上，还据此得出了"科创板没有实时行情"的错误结论。
func TestToolboxMatchesSuffixedQuoteSymbols(t *testing.T) {
	_, toolbox := researchToolbox(t)
	for _, raw := range []string{"688775", "688775.SH", "sh688775", "SH688775"} {
		outcome, err := toolbox.Call(context.Background(), "get_symbol_detail", map[string]any{"symbol": raw})
		if err != nil {
			t.Fatalf("%q 应能匹配到 688775.SH 的行情，实际报错：%v", raw, err)
		}
		if !strings.Contains(outcome.Text, "影石创新") {
			t.Fatalf("%q 查到的内容不对：%s", raw, outcome.Text)
		}
	}

	// 横向对比同样要能跨格式匹配。
	outcome, err := toolbox.Call(context.Background(), "compare_symbols", map[string]any{
		"symbols": []any{"688775.SH", "300308"},
	})
	if err != nil {
		t.Fatalf("跨格式对比失败：%v", err)
	}
	if outcome.Rows != 2 {
		t.Fatalf("两只都应命中行情，实际 %d 行：%s", outcome.Rows, outcome.Text)
	}
}
