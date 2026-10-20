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

func TestCooldownBlocksSymbolAfterStopLoss(t *testing.T) {
	store := newCooldownStore(t)
	now := time.Now()
	insertClosedTrade(t, store, ClosedTrade{
		AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行", Sector: "银行",
		Trigger: TriggerStopLoss, PnLPercent: -6.2,
		TradingDay: TradingDay(now), ClosedAt: now.Add(-1 * time.Hour),
	})
	view, err := store.BuildCooldowns(context.Background(), DefaultAccountID, DefaultCooldownRule(), now)
	if err != nil {
		t.Fatalf("推导冷静期失败：%v", err)
	}
	cooldown, ok := view.Symbols["600000.SH"]
	if !ok {
		t.Fatalf("止损后应产生标的级冷静期：%+v", view)
	}
	if !cooldown.Until.After(now.Add(2 * time.Hour)) {
		t.Fatalf("4 小时窗口应覆盖当前时刻之后 2 小时以上，实际至 %s", cooldown.Until)
	}
	if verdict, passed := view.Block("600000.SH", "银行", now); passed {
		t.Fatalf("冷静期内应拦截买入，实际放行：%+v", verdict)
	} else if verdict.Code != policy.CodeRiskCooldownSymbol {
		t.Fatalf("拦截码应为 risk.cooldown_symbol，实际 %s", verdict.Code)
	}
	// 过了窗口自然解禁：冷静期不是"永久拉黑"。
	if _, passed := view.Block("600000.SH", "银行", now.Add(5*time.Hour)); !passed {
		t.Fatal("4 小时后应自动解禁")
	}
}

func TestCooldownIgnoresTakeProfit(t *testing.T) {
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
	if _, ok := view.Symbols["600000.SH"]; ok {
		t.Fatal("止盈离场后不该冷却：兑现利润不代表判断错了")
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
