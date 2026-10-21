package trading

import (
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
	"easy-stock/backend/internal/trading/universe"
)

// 位置事实的计算口径。这几个数字都是"能被外部核对"的：
// 现价 11、MA20 10、20 日高 11.2、前收 10.5，偏离与涨幅都能手算。
func TestEntryPositionOfComputesFromProfile(t *testing.T) {
	profile := atrProfile{
		Symbol: "600000.SH", Price: 11, MA20: 10, High20: 11.2,
		ConsecutiveUp: 3, PrevClose: 10.5, Bars: 42,
	}
	position := entryPositionOf(quoteOf(nil, "600000.SH"), profile)
	if !position.Measured {
		t.Fatal("有完整画像时应判定为已测量")
	}
	if position.ChangePercent != 4.76 {
		t.Fatalf("当日涨幅应回退到日线计算为 4.76%%，实际 %.2f", position.ChangePercent)
	}
	if position.FromMA20Percent != 10 {
		t.Fatalf("距 MA20 应为 10%%，实际 %.2f", position.FromMA20Percent)
	}
	if position.FromHigh20Percent != -1.79 {
		t.Fatalf("距 20 日高应为 -1.79%%，实际 %.2f", position.FromHigh20Percent)
	}
	if position.ConsecutiveUp != 3 {
		t.Fatalf("连涨天数应为 3，实际 %d", position.ConsecutiveUp)
	}
}

// 快照自带涨跌幅优先于日线推算：新浪/东财的 ChangePercent 已含复权处理，
// 比用两根不复权收盘现算更准。
func TestEntryPositionPrefersQuoteChangePercent(t *testing.T) {
	profile := atrProfile{Symbol: "600000.SH", Price: 11, MA20: 10, High20: 11, PrevClose: 10}
	position := entryPositionOf(testQuote("600000.SH", "浦发银行", 11, 10.2), profile)
	if position.ChangePercent != 7.84 {
		t.Fatalf("应优先用快照涨跌幅 7.84%%，实际 %.2f", position.ChangePercent)
	}
}

// 三条规则各自独立命中，且码不同。合并成一个码会让复盘分不清该改哪条阈值。
func TestCheckEntryPositionBlocksThreeShapes(t *testing.T) {
	rules := DefaultRiskRules()
	cases := []struct {
		name     string
		position EntryPosition
		wantCode string
	}{
		{"当日涨幅", EntryPosition{Measured: true, ChangePercent: 6.5}, policy.CodeRiskEntryChase},
		{"偏离均线", EntryPosition{Measured: true, ChangePercent: 2, FromMA20Percent: 12.4}, policy.CodeRiskEntryExtended},
		{"连涨过久", EntryPosition{Measured: true, ChangePercent: 1, FromMA20Percent: 5, ConsecutiveUp: 5}, policy.CodeRiskEntryStreak},
	}
	for _, item := range cases {
		verdict, blocked := CheckEntryPosition(Order{Symbol: "600000.SH"}, item.position, rules)
		if !blocked {
			t.Fatalf("%s：应拦截", item.name)
		}
		if verdict.Code != item.wantCode {
			t.Fatalf("%s：拦截码应为 %s，实际 %s", item.name, item.wantCode, verdict.Code)
		}
		if verdict.Level != policy.LevelBlock {
			t.Fatalf("%s：级别应为 block，实际 %s", item.name, verdict.Level)
		}
	}
}

// 边界值必须落在"放行"一侧：阈值写的是"≥ 才拦"，等于阈值的那一档
// 在统计表里还没转负，拦掉就是误伤。
func TestCheckEntryPositionAllowsAtBoundary(t *testing.T) {
	rules := DefaultRiskRules()
	position := EntryPosition{
		Measured: true, ChangePercent: 5.99, FromMA20Percent: 11.99, ConsecutiveUp: 4,
	}
	if verdict, blocked := CheckEntryPosition(Order{Symbol: "600000.SH"}, position, rules); blocked {
		t.Fatalf("边界内不应拦截，实际被 %s 拦下", verdict.Code)
	}
}

// 位置算不出来时是 warn，不是 block：数据源抖动不该让账户停止开仓，
// 但必须留痕，否则"这段买入没有位置约束"在复盘里查不出来。
func TestCheckEntryPositionUnknownIsWarn(t *testing.T) {
	verdict, blocked := CheckEntryPosition(Order{Symbol: "600000.SH"}, EntryPosition{}, DefaultRiskRules())
	if blocked {
		t.Fatal("位置未知不应拦截")
	}
	if verdict.Code != policy.CodeRiskEntryUnmeasured || verdict.Level != policy.LevelWarn {
		t.Fatalf("应为 warn 级 %s，实际 %s/%s", policy.CodeRiskEntryUnmeasured, verdict.Level, verdict.Code)
	}
}

// 端到端：位置超限的买单必须变成 rejected，且带得回拦截码。
// 这是"规则真的生效"的唯一证据——单测只测函数，这里测的是订单流。
func TestBuildOrdersBlocksChaseBuy(t *testing.T) {
	now := time.Now()
	snapshot := Snapshot{
		Equity: 100000,
		Cash:   100000,
		Quotes: []foundation.Quote{testQuote("600000.SH", "浦发银行", 10.6, 10)},
		EntryPositions: map[string]EntryPosition{
			"600000.SH": {Symbol: "600000.SH", Measured: true, ChangePercent: 6, FromMA20Percent: 3},
		},
	}
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10,
		OrderType: "limit", LimitPrice: 10.6, StopLossPrice: 10,
	}}}
	orders, verdicts := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), now)
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("应被拦下，实际 %+v", orders)
	}
	if orders[0].RejectCode != policy.CodeRiskEntryChase {
		t.Fatalf("拦截码应为 %s，实际 %s", policy.CodeRiskEntryChase, orders[0].RejectCode)
	}
	found := false
	for _, verdict := range verdicts {
		if verdict.Code == policy.CodeRiskEntryChase {
			found = true
		}
	}
	if !found {
		t.Fatal("裁决里应有 risk.entry_chase 记录")
	}
}

// 快照不带位置表时整道门跳过。老沙盘快照没有这份数据，
// 按"未知"去 warn 会把历史重放刷满噪音。
func TestBuildOrdersSkipsGateWithoutPositionTable(t *testing.T) {
	now := time.Now()
	snapshot := Snapshot{
		Equity: 100000,
		Cash:   100000,
		Quotes: []foundation.Quote{testQuote("600000.SH", "浦发银行", 10.6, 10)},
	}
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10,
		OrderType: "limit", LimitPrice: 10.6, StopLossPrice: 10,
	}}}
	orders, _ := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), now)
	if len(orders) != 1 || orders[0].Status != OrderStatusPending {
		t.Fatalf("无位置表时不应拦截，实际 %+v", orders)
	}
}

// 提示词必须同时做两件事：把位置摆出来，把超限的标出来。
// 只在裁决层拦、不在提示词里说，模型会连续给出会被拦的建议，
// 界面上就成了"模型很积极、系统总在否决"。
func TestPromptMarksPositionAndConstraint(t *testing.T) {
	board := &universe.Board{
		SourceCount: 5000, ThresholdAmount: 8e7,
		Candidates: []universe.Candidate{
			{Symbol: "300308", Name: "中际旭创", Price: 128, Score: 88, Reasons: []string{"5 日涨幅 +9.2%"}},
			{Symbol: "600000", Name: "浦发银行", Price: 10, Score: 71, Reasons: []string{"成交额 3.2 亿"}},
		},
	}
	prompt := BuildPlanPrompt(PlanPromptInput{
		Account: Account{ID: DefaultAccountID, Cash: 100000, Equity: 100000},
		Quotes:  []foundation.Quote{testQuote("300308.SZ", "中际旭创", 128, 121), testQuote("600000.SH", "浦发银行", 10, 9.9)},
		Rules:   DefaultRiskRules(),
		Now:     testSessionTime(),
		Board:   board,
		EntryPositions: map[string]EntryPosition{
			"300308.SZ": {Symbol: "300308.SZ", Measured: true, ChangePercent: 5.79, FromMA20Percent: 16, ConsecutiveUp: 6},
			"600000.SH": {Symbol: "600000.SH", Measured: true, ChangePercent: 1.01, FromMA20Percent: 2, ConsecutiveUp: 1},
		},
		ExposureCeiling: 55,
	})
	if !strings.Contains(prompt, "【位置超限】") {
		t.Fatal("超限标的应被标出")
	}
	if !strings.Contains(prompt, "连续上涨少于 5 天") {
		t.Fatal("提示词应写明位置硬约束的阈值")
	}
	if !strings.Contains(prompt, "距 MA20 +16.00%") {
		t.Fatalf("应写出具体位置数字，实际片段：%s", snippetAround(prompt, "300308"))
	}
}

// snippetAround 取关键词附近的一小段，用于失败时看清实际输出。
func snippetAround(text, keyword string) string {
	index := strings.Index(text, keyword)
	if index < 0 {
		return "（未找到关键词）"
	}
	end := index + 160
	if end > len(text) {
		end = len(text)
	}
	return text[index:end]
}

// 选股层与下单层的位置口径必须完全一致。
//
// 两处各写了一遍判断（universe.PositionBlockReason 与 CheckEntryPosition），
// 这是刻意的：universe 是纯函数、不能反过来依赖 trading，而把判定塞进
// 一个共享包又会让"什么算追高"散落在第三处。代价是可能漂移，
// 所以用这条对照测试把两边钉住：改一处不改另一处，这里就红。
//
// 漂移的后果不是小事——选股层放进来、下单层再拦掉，等于白算一次；
// 选股层拦掉、下单层放行，则是"池子里没有它、模型也点不到它"，
// 而日志里两条数字会同时看起来正常。
func TestEntryLimitsMatchPolicy(t *testing.T) {
	rules := DefaultRiskRules().Normalize()
	limits := universe.EntryLimits{
		MaxChangePercent: rules.MaxEntryChangePercent,
		MaxMA20Percent:   rules.MaxEntryMA20Percent,
		MaxUpDays:        rules.MaxEntryUpDays,
	}
	cases := []EntryPosition{
		{ChangePercent: 0, FromMA20Percent: 0, ConsecutiveUp: 0, Measured: true},
		{ChangePercent: 5.99, FromMA20Percent: 11.99, ConsecutiveUp: 4, Measured: true},
		{ChangePercent: 6, FromMA20Percent: 0, ConsecutiveUp: 0, Measured: true},
		{ChangePercent: 9.8, FromMA20Percent: 2, ConsecutiveUp: 1, Measured: true},
		{ChangePercent: 1, FromMA20Percent: 12, ConsecutiveUp: 1, Measured: true},
		{ChangePercent: 1, FromMA20Percent: 40, ConsecutiveUp: 1, Measured: true},
		{ChangePercent: 1, FromMA20Percent: 2, ConsecutiveUp: 5, Measured: true},
		{ChangePercent: 1, FromMA20Percent: 2, ConsecutiveUp: 9, Measured: true},
		{Measured: false},
	}
	for _, position := range cases {
		position.Symbol = "600000.SH"
		_, blocked := CheckEntryPosition(Order{Symbol: position.Symbol}, position, rules)
		poolReason := universe.PositionBlockReason(universe.Position{
			ChangePercent:   position.ChangePercent,
			FromMA20Percent: position.FromMA20Percent,
			ConsecutiveUp:   position.ConsecutiveUp,
			Measured:        position.Measured,
		}, limits)
		if blocked != (poolReason != "") {
			t.Fatalf("%+v：下单层 blocked=%t，选股层 reason=%q，两层口径不一致",
				position, blocked, poolReason)
		}
	}
}
