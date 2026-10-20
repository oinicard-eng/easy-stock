package trading

import (
	"context"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// fakeKLine 返回一段确定的日线，用于验证指标计算与排序处理。
type fakeKLine struct {
	bars    []foundation.KLine
	err     error
	reverse bool
	calls   int
}

func (f *fakeKLine) KLine(_ context.Context, symbol string, _ string, limit int) ([]foundation.KLine, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	bars := append([]foundation.KLine(nil), f.bars...)
	for index := range bars {
		bars[index].Symbol = symbol
	}
	if f.reverse {
		for left, right := 0, len(bars)-1; left < right; left, right = left+1, right-1 {
			bars[left], bars[right] = bars[right], bars[left]
		}
	}
	if limit > 0 && limit < len(bars) {
		bars = bars[len(bars)-limit:]
	}
	return bars, nil
}

// buildBars 生成 n 根单调上行的日线：收盘 10.00 起每根 +0.10，
// 振幅固定 0.40，量能从 1000 起每根 +10。
func buildBars(n int) []foundation.KLine {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]foundation.KLine, 0, n)
	for index := 0; index < n; index++ {
		closePrice := 10 + float64(index)*0.10
		bars = append(bars, foundation.KLine{
			Symbol: "600000.SH",
			Time:   base.AddDate(0, 0, index),
			Open:   closePrice - 0.05,
			High:   closePrice + 0.20,
			Low:    closePrice - 0.20,
			Close:  closePrice,
			Volume: 1000 + float64(index)*10,
		})
	}
	return bars
}

func TestGetKlineComputesIndicatorsAndSortsBars(t *testing.T) {
	_, toolbox := researchToolbox(t)
	provider := &fakeKLine{bars: buildBars(70), reverse: true}
	toolbox.klines = provider

	outcome, err := toolbox.Call(context.Background(), "get_kline", map[string]any{
		"symbol": "600000.SH",
		"days":   float64(70),
	})
	if err != nil {
		t.Fatalf("调用 get_kline 失败: %v", err)
	}
	for _, want := range []string{"MA5", "MA20", "ATR14", "20日区间", "量能", "连续"} {
		if !strings.Contains(outcome.Text, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, outcome.Text)
		}
	}

	// 乱序输入必须被排好。70 根收盘 10.00→16.90，MA5 = 最后五行均值。
	wantMA5 := (16.50 + 16.60 + 16.70 + 16.80 + 16.90) / 5
	data, ok := outcome.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data 类型异常: %T", outcome.Data)
	}
	ma5, ok := data["ma5"].(float64)
	if !ok || ma5 <= 0 {
		t.Fatalf("ma5 缺失: %+v", data)
	}
	if diff := ma5 - wantMA5; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ma5 = %.4f，期望 %.4f；说明输入没有按时间升序排列", ma5, wantMA5)
	}
	if rows, _ := data["bars"].(int); rows != 70 {
		t.Fatalf("bars = %v，期望 70", data["bars"])
	}
}

func TestGetKlineRequiresProviderAndEnoughBars(t *testing.T) {
	_, toolbox := researchToolbox(t)
	if _, err := toolbox.Call(context.Background(), "get_kline", map[string]any{"symbol": "600000"}); err == nil {
		t.Fatal("未配置日线源时应报错，而不是返回一条编造的曲线")
	}

	toolbox.klines = &fakeKLine{bars: buildBars(3)}
	if _, err := toolbox.Call(context.Background(), "get_kline", map[string]any{"symbol": "600000"}); err == nil {
		t.Fatal("日线不足 5 根时应报错，而不是拿 3 根算出均线")
	}
}

func TestGetKlineFallsBackWhenPrimaryFails(t *testing.T) {
	_, toolbox := researchToolbox(t)
	primary := &fakeKLine{err: context.DeadlineExceeded}
	fallback := &fakeKLine{bars: buildBars(40)}
	toolbox.klines = primary
	toolbox.klineAlt = fallback

	outcome, err := toolbox.Call(context.Background(), "get_kline", map[string]any{"symbol": "600000"})
	if err != nil {
		t.Fatalf("主源失败时应回落到备源: %v", err)
	}
	if primary.calls == 0 || fallback.calls == 0 {
		t.Fatalf("应各调用一次，实际主 %d 备 %d", primary.calls, fallback.calls)
	}
	if !strings.Contains(outcome.Text, "MA5") {
		t.Fatalf("备源结果缺少指标：\n%s", outcome.Text)
	}
}

func TestPlanPromptCarriesPendingOrdersAndRecentBlocks(t *testing.T) {
	rules := DefaultRiskRules()
	prompt := BuildPlanPrompt(PlanPromptInput{
		Account: Account{ID: DefaultAccountID, Cash: 100000, Equity: 100000},
		Rules:   rules,
		Now:     time.Now(),
		Pending: []Order{{
			Symbol: "600000.SH", Name: "浦发银行", Side: SideBuy,
			Quantity: 800, LimitPrice: 10.2, Status: OrderStatusPending,
		}},
		Blocks: []BlockedIntent{{
			Symbol: "300308", Name: "中际旭创", Side: SideBuy, Quant: 400,
			Price: 128, Weight: 12, Code: "sector.weight",
			Reason: "通信设备行业权重 52.0% 超过上限 45.0%", Repeat: 3,
		}},
	})

	for _, want := range []string{"当前挂单", "600000.SH", "待成交", "最近被拦下的建议", "sector.weight", "重复 3 次"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("提示词缺少 %q", want)
		}
	}
	// 被拦记录必须带原始意图，否则模型只知道"被拦"不知道该改什么。
	if !strings.Contains(prompt, "拟 买入 400 股") {
		t.Fatalf("被拦记录应保留原始数量与方向：\n%s", prompt)
	}
}

func TestRecentBlocksDedupesAndKeepsCount(t *testing.T) {
	store, _ := researchToolbox(t)
	service := &Service{store: store}
	now := time.Now().UTC()

	for index := 0; index < 3; index++ {
		order := Order{
			ID: newID("ord"), PlanID: "plan-x", AccountID: DefaultAccountID,
			Symbol: "300308", Name: "中际旭创", Side: SideBuy, Quantity: 400,
			LimitPrice: 128, TargetWeightPercent: 12, Status: OrderStatusRejected,
			RejectCode: "sector.weight", RejectReason: "行业权重超限",
			CreatedAt: now.Add(-time.Duration(index) * time.Minute), UpdatedAt: now,
		}
		if _, err := store.CreateOrders(context.Background(), []Order{order}); err != nil {
			t.Fatalf("写入被拒委托失败: %v", err)
		}
	}
	blocks := service.recentBlocksForPrompt(context.Background(), DefaultAccountID)
	if len(blocks) != 1 {
		t.Fatalf("同一标的同一规则应合并为 1 条，实际 %d 条", len(blocks))
	}
	if blocks[0].Repeat != 3 {
		t.Fatalf("重复次数应为 3，实际 %d", blocks[0].Repeat)
	}
	if blocks[0].Code != "sector.weight" {
		t.Fatalf("拦截代码丢失: %+v", blocks[0])
	}
}

func TestSectorFlowAcceptsChineseDimension(t *testing.T) {
	_, toolbox := researchToolbox(t)
	toolbox.sectorFlow = staticSectorFlow{}
	for _, raw := range []string{"行业", "industry", "概念", "concept"} {
		if _, err := toolbox.Call(context.Background(), "list_sector_flow", map[string]any{"dimension": raw}); err != nil {
			t.Fatalf("dimension=%q 应被接受，实际报错：%v", raw, err)
		}
	}
	if _, err := toolbox.Call(context.Background(), "list_sector_flow", map[string]any{"dimension": "板块"}); err == nil {
		t.Fatal("无法识别的维度应报错，而不是静默当成概念板块")
	}
}
