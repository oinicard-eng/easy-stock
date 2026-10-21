package trading

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// 冷静期的验收标准：止损之后不能马上回去买同一只票；止盈之后不该被冷却；
// 同一行业当日两次止损后，整个行业当天停手。

func newCooldownStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// insertClosedTrade 直接写一条平仓记录。绕开记账流程是刻意的：
// 这里要验证的是"冷静期如何解读既有事实"，而不是"记账是否正确"。
func insertClosedTrade(t *testing.T, store *Store, trade ClosedTrade) {
	t.Helper()
	if trade.ID == "" {
		trade.ID = newID("trd")
	}
	payload, err := json.Marshal(trade)
	if err != nil {
		t.Fatalf("序列化平仓记录失败：%v", err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO trading_closed_trades
		(id,account_id,symbol,trading_day,trigger,content_json,closed_at) VALUES(?,?,?,?,?,?,?)`,
		trade.ID, trade.AccountID, trade.Symbol, trade.TradingDay, trade.Trigger, string(payload), nowText(trade.ClosedAt)); err != nil {
		t.Fatalf("写入平仓记录失败：%v", err)
	}
}

// cooldownCST 构造一个北京时间时点。冷静期窗口按交易时段算，
// 测试必须用固定时点：用 time.Now() 的话，测试在盘中、午休、夜里跑
// 会得到三种不同的解禁时刻，断言无从谈起。
func cooldownCST(hour, min int, dayOffset int) time.Time {
	local := time.Now().In(cstZone()).AddDate(0, 0, dayOffset)
	// 落到周三附近的平日本身，避开周末。
	for wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday; {
		local = local.AddDate(0, 0, 1)
		wd = local.Weekday()
	}
	y, m, d := local.Date()
	return time.Date(y, m, d, hour, min, 0, 0, cstZone())
}

func TestCooldownBlocksSymbolAfterStopLoss(t *testing.T) {
	store := newCooldownStore(t)
	// 周三 10:00 止损，10:30 判定。4 小时交易时间 = 当日剩余 3.5 小时
	// （10:00–11:30 + 13:00–15:00）+ 次日开盘后 0.5 小时 → 周四 10:00 解禁。
	closedAt := cooldownCST(10, 0, 0)
	now := cooldownCST(10, 30, 0)
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行", Sector: "银行",
		Trigger: TriggerStopLoss, PnLPercent: -6.2,
		TradingDay: TradingDay(closedAt), ClosedAt: closedAt,
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	cooldown, ok := view.Symbols["600000.SH"]
	if !ok {
		t.Fatalf("止损后应产生标的级冷静期：%+v", view)
	}
	wantUntil := cooldownCST(10, 0, 1)
	if !cooldown.Until.Equal(wantUntil) {
		t.Fatalf("解禁时刻应为次日 %s（4 小时交易时间），实际 %s", wantUntil, cooldown.Until)
	}
	if verdict, passed := view.Block("600000.SH", "银行", now); passed {
		t.Fatalf("冷静期内应拦截买入，实际放行：%+v", verdict)
	} else if verdict.Code != policy.CodeRiskCooldownSymbol {
		t.Fatalf("拦截码应为 risk.cooldown_symbol，实际 %s", verdict.Code)
	}
	// 过了窗口自然解禁：冷静期不是"永久拉黑"。
	if _, passed := view.Block("600000.SH", "银行", wantUntil.Add(time.Minute)); !passed {
		t.Fatal("解禁时刻之后应自动放行")
	}
}

func TestCooldownWindowSkipsLunchBreakAndWeekend(t *testing.T) {
	// 找到下一个星期几的辅助：直接按日期构造（不走 cooldownCST 的跳周末逻辑，
	// 否则它永远造不出周六）。同时刻取自真实日历的该日，保证是星期几正确。
	nextWeekday := func(hour, min int, want time.Weekday) time.Time {
		base := time.Now().In(cstZone())
		for offset := 0; offset < 8; offset++ {
			day := base.AddDate(0, 0, offset)
			if day.Weekday() == want {
				y, m, d := day.Date()
				return time.Date(y, m, d, hour, min, 0, 0, cstZone())
			}
		}
		return time.Time{}
	}

	// 周五 14:30 止损：当日只剩 0.5 小时交易时间，剩余 3.5 小时
	// 顺延到下周一：开盘 2 小时（上午）+ 午后 1.5 小时 → 周一 14:30 解禁。
	friday := nextWeekday(14, 30, time.Friday)
	until := addTradingHours(friday, 4)
	monday := friday.AddDate(0, 0, 3) // 周五 +3 天即下周一
	want := time.Date(monday.Year(), monday.Month(), monday.Day(), 14, 30, 0, 0, cstZone())
	if !until.Equal(want) {
		t.Fatalf("周五尾盘止损应顺延到下周一 14:30 解禁（跳过周末与午休），实际 %s", until)
	}

	// 午休起点止损：11:20 止损 → 上午剩 10 分钟 + 下午 2 小时 + 次日 1 小时 50 分 → 次日 11:20。
	lunch := nextWeekday(11, 20, time.Tuesday)
	until = addTradingHours(lunch, 4)
	// 次日若是周末要顺延：直接取 lunch 之后最近的次日同时刻（跳过周末）。
	nextDay := lunch.AddDate(0, 0, 1)
	for wd := nextDay.Weekday(); wd == time.Saturday || wd == time.Sunday; {
		nextDay = nextDay.AddDate(0, 0, 1)
		wd = nextDay.Weekday()
	}
	if !until.Equal(nextDay) {
		t.Fatalf("午休前止损应跳过午休累计，实际 %s，期望 %s", until, nextDay)
	}

	// 周末止损：从下周一开盘起算 4 小时交易时间 = 上午 2 小时 + 午后 2 小时 → 周一 15:00（收盘）。
	saturday := nextWeekday(15, 30, time.Saturday)
	until = addTradingHours(saturday, 4)
	wantMonday := saturday.AddDate(0, 0, 2) // 周六 +2 天即下周一
	want = time.Date(wantMonday.Year(), wantMonday.Month(), wantMonday.Day(), 15, 0, 0, 0, cstZone())
	if !until.Equal(want) {
		t.Fatalf("周末止损应从下周一开盘起算，实际 %s，期望 %s", until, want)
	}
}

// 止盈离场不产生"止损冷静期"——兑现利润不代表判断错了，不该按止损对待。
// 但"当日卖出即当日不再买回"这条与离场原因无关：一天之内在同一只票上
// 卖出又买回，本身就是判断在两小时内翻转，与赚没赚到钱无关。
func TestCooldownAfterTakeProfitBansSameDayRebuyOnly(t *testing.T) {
	store := newCooldownStore(t)
	now := time.Now()
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Sector: "银行",
		Trigger: TriggerTakeProfit, PnLPercent: 8,
		TradingDay: TradingDay(now), ClosedAt: now.Add(-30 * time.Minute),
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	cooldown, ok := view.Symbols["600000.SH"]
	if !ok {
		t.Fatal("止盈当日卖出后，当日仍应禁止买回同一标的")
	}
	if cooldown.Trigger != TriggerTakeProfit {
		t.Fatalf("冷却原因应为止盈本身，实际为 %q", cooldown.Trigger)
	}
	// 解禁时刻必须是次日开盘，而不是"4 小时交易时间"——后者正是止损冷静期的口径。
	if want := nextTradingDayOpen(now); !cooldown.Until.Equal(want) {
		t.Fatalf("解禁时刻应为次日开盘 %s，实际 %s", want.Format(time.RFC3339), cooldown.Until.Format(time.RFC3339))
	}
}

// 埃夫特那次实测的回归：model_exit 清仓后，当日不得再买回。
// 原实现只认止损类离场，这条完全绕过去了。
func TestCooldownBansSameDayRebuyAfterModelExit(t *testing.T) {
	store := newCooldownStore(t)
	closedAt := cooldownCST(10, 0, 0)
	now := cooldownCST(10, 20, 0)
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "688165.SH", Name: "埃夫特", Sector: "机械",
		Trigger: TriggerModelExit, PnLPercent: -1.43,
		TradingDay: TradingDay(closedAt), ClosedAt: closedAt,
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	verdict, ok := view.Block("688165.SH", "机械", now)
	if ok {
		t.Fatal("模型主动离场后 20 分钟又允许买回同一标的，日内反复进出没有被拦住")
	}
	if verdict.Code != policy.CodeRiskCooldownSymbol {
		t.Fatalf("拦截码应为 %s，实际 %s", policy.CodeRiskCooldownSymbol, verdict.Code)
	}

	// 次日开盘解禁：给模型一整晚重新判断，而不是 4 小时后立刻能买回。
	next := nextTradingDayOpen(now).Add(time.Minute)
	if _, passed := view.Block("688165.SH", "机械", next); !passed {
		t.Fatal("次日开盘后应已解禁")
	}
}

// 当日禁买与止损冷静期同时命中时，取较晚的解禁时刻。
// 周五上午止损：止损窗口跨周末到周一，比"次日开盘"更晚，必须保留前者。
func TestCooldownKeepsLaterOfSameDayBanAndStopWindow(t *testing.T) {
	store := newCooldownStore(t)
	closedAt := cooldownCST(10, 0, 0)
	now := cooldownCST(10, 30, 0)
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Sector: "银行",
		Trigger: TriggerStopLoss, PnLPercent: -6.2,
		TradingDay: TradingDay(closedAt), ClosedAt: closedAt,
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	cooldown, ok := view.Symbols["600000.SH"]
	if !ok {
		t.Fatal("止损后应有禁买约束")
	}
	stopUntil := addTradingHours(closedAt, DefaultCooldownRule().SymbolHours)
	if cooldown.Until.Before(stopUntil) {
		t.Fatalf("解禁时刻取早了：止损窗口到 %s，实际登记 %s——冷静期被当日禁买规则覆盖掉了一部分",
			stopUntil.Format(time.RFC3339), cooldown.Until.Format(time.RFC3339))
	}
}

// AllowSameDayRebuy 保留为可配置项：确认它确实能关掉，
// 而不是写死了一条无法回滚的规则。
func TestCooldownSameDayRebuyCanBeAllowed(t *testing.T) {
	store := newCooldownStore(t)
	closedAt := cooldownCST(10, 0, 0)
	now := cooldownCST(10, 20, 0)
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "688165.SH", Sector: "机械",
		Trigger: TriggerModelExit, PnLPercent: -1.43,
		TradingDay: TradingDay(closedAt), ClosedAt: closedAt,
	})
	rule := DefaultCooldownRule()
	rule.AllowSameDayRebuy = true
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, rule, now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	if _, ok := view.Symbols["688165.SH"]; ok {
		t.Fatal("显式允许当日买回时不该产生约束")
	}
}

func TestCooldownBlocksSectorAfterTwoLosses(t *testing.T) {
	store := newCooldownStore(t)
	now := time.Now()
	day := TradingDay(now)
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Sector: "银行",
		Trigger: TriggerStopLoss, PnLPercent: -5, TradingDay: day, ClosedAt: now.Add(-2 * time.Hour),
	})
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "601398.SH", Sector: "银行",
		Trigger: TriggerHardStop, PnLPercent: -9, TradingDay: day, ClosedAt: now.Add(-40 * time.Minute),
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	if _, ok := view.Sectors["银行"]; !ok {
		t.Fatalf("同板块两次止损应产生行业级冷静期：%+v", view)
	}
	// 行业级拦截要能拦住该行业内的另一只票。
	verdict, passed := view.Block("601288.SH", "银行", now)
	if passed {
		t.Fatal("行业冷静期内应拦截同行业其他标的")
	}
	if verdict.Code != policy.CodeRiskCooldownSector {
		t.Fatalf("拦截码应为 risk.cooldown_sector，实际 %s", verdict.Code)
	}
	// 不同行业不受影响。
	if _, passed := view.Block("600519.SH", "白酒", now); !passed {
		t.Fatal("行业冷静期不该波及其他行业")
	}
}

func TestBuildOrdersBlocksBuyDuringCooldown(t *testing.T) {
	now := time.Now()
	snapshot := Snapshot{
		Equity:  100000,
		Cash:    100000,
		Quotes:  []foundation.Quote{testQuote("600000.SH", "浦发银行", 10, 9.9)},
		Sectors: map[string]string{"600000.SH": "银行"},
		Cooldowns: CooldownView{Symbols: map[string]Cooldown{
			"600000.SH": {Symbol: "600000.SH", Until: now.Add(2 * time.Hour), Reason: "刚止损"},
		}},
	}
	draft := DraftPlan{Orders: []DraftOrder{{
		Symbol: "600000.SH", Side: "buy", TargetWeightPercent: 10,
		OrderType: "limit", LimitPrice: 10, StopLossPrice: 9.5,
	}}}
	orders, verdicts := BuildOrders(DefaultAccountID, draft, snapshot, DefaultRiskRules(), now)
	if len(orders) != 1 || orders[0].Status != OrderStatusRejected {
		t.Fatalf("冷静期内买入应被拒：%+v", orders)
	}
	if orders[0].RejectCode != policy.CodeRiskCooldownSymbol {
		t.Fatalf("拒绝码应为冷静期，实际 %s（%s）", orders[0].RejectCode, orders[0].RejectReason)
	}
	// 被拒订单仍要保留拟买数量：复盘要回答"模型原本想买多少"。
	if orders[0].Quantity != 1000 {
		t.Fatalf("被拒订单应保留拟买数量 1000 股，实际 %d", orders[0].Quantity)
	}
	_ = verdicts
}

func TestCooldownSurvivesWeekendLookback(t *testing.T) {
	// 回归用例：周五 10:00 止损，4 小时交易时间 → 周一 10:00 解禁。
	// 周一一早（9:30）判定时，这条记录的自然时间已过去约 71 小时。
	// 回看窗口若固定为 48 小时，查询会漏掉它，冷静期在周一只剩最后半小时时
	// 凭空消失——保护还在有效期内却被静默解除。
	store := newCooldownStore(t)
	constraints := func(hour, min int, want time.Weekday) time.Time {
		base := time.Now().In(cstZone())
		for offset := 0; offset < 8; offset++ {
			day := base.AddDate(0, 0, offset)
			if day.Weekday() == want {
				y, m, d := day.Date()
				return time.Date(y, m, d, hour, min, 0, 0, cstZone())
			}
		}
		return time.Time{}
	}
	friday := constraints(10, 0, time.Friday)
	monday := friday.AddDate(0, 0, 3)
	now := time.Date(monday.Year(), monday.Month(), monday.Day(), 9, 30, 0, 0, cstZone())
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Sector: "银行",
		Trigger: TriggerStopLoss, PnLPercent: -6,
		TradingDay: TradingDay(friday), ClosedAt: friday,
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	cooldown, ok := view.Symbols["600000.SH"]
	if !ok {
		t.Fatal("跨周末的止损记录必须在周一早盘仍然生效")
	}
	wantUntil := time.Date(monday.Year(), monday.Month(), monday.Day(), 10, 0, 0, 0, cstZone())
	if !cooldown.Until.Equal(wantUntil) {
		t.Fatalf("解禁时刻应为周一 10:00，实际 %s", cooldown.Until)
	}
	if _, passed := view.Block("600000.SH", "银行", now); passed {
		t.Fatal("周一 9:30 应仍在冷静期内")
	}
}
