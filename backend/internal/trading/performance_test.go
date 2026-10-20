package trading

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"
)

// closedTradeFixture 直接写平仓表，绕过撮合：业绩统计只关心已实现结果。
func closedTradeFixture(t *testing.T, store *Store, symbol string, day string, pnl float64, trigger string) {
	t.Helper()
	closedAt, err := time.ParseInLocation("2006-01-02 15:00", day+" 15:00", time.FixedZone("CST", 8*3600))
	if err != nil {
		t.Fatalf("解析平仓时间失败：%v", err)
	}
	trade := ClosedTrade{
		ID: newID("closed"), AccountID: DefaultAccountID, Symbol: symbol, Name: symbol,
		Sector: "测试行业", Quantity: 100, EntryPrice: 10, ExitPrice: 10 + pnl/100,
		PnL: pnl, Trigger: trigger, ClosedAt: closedAt.UTC(), HoldDays: 1, TradingDay: day,
	}
	payload, err := json.Marshal(trade)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`INSERT INTO trading_closed_trades(id,account_id,symbol,trading_day,trigger,content_json,closed_at) VALUES(?,?,?,?,?,?,?)`,
		trade.ID, trade.AccountID, trade.Symbol, trade.TradingDay, trade.Trigger, string(payload), day+"T07:00:00Z"); err != nil {
		t.Fatalf("写入平仓记录失败：%v", err)
	}
}

func TestPerformanceAggregatesClosedTrades(t *testing.T) {
	store := newTestStore(t)
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))
	ctx := context.Background()

	today := TradingDay(time.Now())
	yesterday := TradingDay(time.Now().AddDate(0, 0, -1))
	closedTradeFixture(t, store, "600000.SH", yesterday, 1000, TriggerTakeProfit)
	closedTradeFixture(t, store, "600000.SH", today, -400, TriggerStopLoss)
	closedTradeFixture(t, store, "000001.SZ", today, 200, TriggerModelExit)

	perf, err := service.Performance(ctx, DefaultAccountID, 30)
	if err != nil {
		t.Fatalf("计算业绩失败：%v", err)
	}
	if perf.ClosedTrades != 3 || perf.Wins != 2 || perf.Losses != 1 {
		t.Fatalf("笔数统计错误：%d 笔 / %d 胜 / %d 负", perf.ClosedTrades, perf.Wins, perf.Losses)
	}
	if diff := perf.WinRate - 66.67; diff > 0.01 || diff < -0.01 {
		t.Fatalf("胜率应为 66.67，实际 %.2f", perf.WinRate)
	}
	if diff := perf.RealizedPnL - 800; diff > 0.01 || diff < -0.01 {
		t.Fatalf("已实现盈亏应为 800，实际 %.2f", perf.RealizedPnL)
	}
	if diff := perf.ProfitFactor - 3; diff > 0.01 || diff < -0.01 {
		t.Fatalf("盈亏比应为 3，实际 %.2f", perf.ProfitFactor)
	}
	if diff := perf.Expectancy - 266.67; diff > 0.01 || diff < -0.01 {
		t.Fatalf("期望值应为 266.67，实际 %.2f", perf.Expectancy)
	}
	if perf.BestTrade != 1000 || perf.WorstTrade != -400 {
		t.Fatalf("最好/最差单笔错误：%.2f / %.2f", perf.BestTrade, perf.WorstTrade)
	}
	if len(perf.Daily) != 2 {
		t.Fatalf("应有两天的曲线，实际 %d", len(perf.Daily))
	}
	if perf.Daily[0].Cumulative != 1000 || perf.Daily[1].Cumulative != 800 {
		t.Fatalf("累计曲线错误：%.2f → %.2f", perf.Daily[0].Cumulative, perf.Daily[1].Cumulative)
	}
	if len(perf.BySymbol) == 0 {
		t.Fatal("应按标的给出归因")
	}
	// 同一只票一胜一负，合计 +600；样本不足 30 笔，胜率必须不显示。
	for _, lesson := range perf.BySymbol {
		if lesson.Key == "600000.SH" && lesson.WinRate != nil {
			t.Fatal("样本不足时应隐藏胜率")
		}
	}
}

// 没有平仓时不能编造数字：胜率、盈亏比、期望值都必须是 0 且曲线为空。
func TestPerformanceEmptyLedger(t *testing.T) {
	store := newTestStore(t)
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))
	perf, err := service.Performance(context.Background(), DefaultAccountID, 30)
	if err != nil {
		t.Fatalf("计算业绩失败：%v", err)
	}
	if perf.ClosedTrades != 0 || perf.RealizedPnL != 0 || perf.Expectancy != 0 {
		t.Fatalf("空账本不应有数字：%+v", perf)
	}
	if perf.Daily == nil || len(perf.Daily) != 0 {
		t.Fatalf("空账本的曲线应为空数组，实际 %#v", perf.Daily)
	}
}
