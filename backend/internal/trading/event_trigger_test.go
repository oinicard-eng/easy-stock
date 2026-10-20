package trading

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 事件通道的验收标准：只在真的异动时触发；同一只票一天只触发一次；
// 两轮之间至少隔 6 分钟；没有昨收时不猜。

func eventTestAutopilot(t *testing.T) (*Autopilot, *Service, time.Time) {
	t.Helper()
	content := `{"regime":"进攻","summary":"事件测试","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":10,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.5,"thesis":"测试","confidence":0.7}]}`
	service := newTestService(t, content, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 10.6),
	})
	ctx := context.Background()
	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if _, err := service.Execute(ctx, plan.ID); err != nil {
		t.Fatalf("执行计划失败：%v", err)
	}
	autopilot := NewAutopilot(service, log.New(io.Discard, "", 0))
	return autopilot, service, time.Now()
}

func TestEventTriggerFiresOnPositionDrop(t *testing.T) {
	autopilot, _, now := eventTestAutopilot(t)
	reason, fired := autopilot.eventTriggered(context.Background(), DefaultAccountID, now)
	if !fired {
		t.Fatal("持仓日内跌 5.66%（相对昨收 10.6）应触发")
	}
	if !strings.Contains(reason, "日内跌") {
		t.Fatalf("触发原因应说明日内跌幅，实际：%s", reason)
	}
	// 同一只票当日已触发过：再次调用不该重复触发。
	if _, again := autopilot.eventTriggered(context.Background(), DefaultAccountID, now.Add(10*time.Minute)); again {
		t.Fatal("同一标的当日只应触发一次")
	}
}

func TestEventTriggerFiresOnSectorFlowReversal(t *testing.T) {
	// 价格还没跌，但持仓所在板块的主力资金已经转负：这类信号早于价格，
	// 等指数跌下去再反应，持仓已经跟着跌了一截。
	service := &Service{}
	service.SetSectorFlow(stubSectorFlow{rows: map[string]float64{"银行Ⅱ": -2.4, "白酒": 1.8}})
	autopilot := NewAutopilot(service, log.New(io.Discard, "", 0))
	account := Account{ID: DefaultAccountID, Equity: 100000}
	positions := []Position{{Symbol: "600000.SH", Name: "浦发银行", Quantity: 1000, LastPrice: 10, MarketValue: 10000, Sector: "银行"}}
	now := time.Now()

	reason, fired := autopilot.sectorFlowNegative(context.Background(), account, positions, now)
	if !fired {
		t.Fatal("板块主力净占比 -2.4% 应触发事件轮次")
	}
	if !strings.Contains(reason, "银行") || !strings.Contains(reason, "转负") {
		t.Fatalf("触发原因应说明板块与方向，实际：%s", reason)
	}
	// 同一板块当日只触发一次：资金流为负会持续，不设这个限制就是每 6 分钟一次。
	if _, again := autopilot.sectorFlowNegative(context.Background(), account, positions, now.Add(time.Minute)); again {
		t.Fatal("同一板块当日只应触发一次")
	}
}

func TestEventTriggerIgnoresMildSectorOutflow(t *testing.T) {
	// -0.4% 属于正常抖动，拿它叫一次模型是纯浪费。
	service := &Service{}
	service.SetSectorFlow(stubSectorFlow{rows: map[string]float64{"银行Ⅱ": -0.4}})
	autopilot := NewAutopilot(service, log.New(io.Discard, "", 0))
	account := Account{ID: DefaultAccountID, Equity: 100000}
	positions := []Position{{Symbol: "600000.SH", Quantity: 1000, LastPrice: 10, MarketValue: 10000, Sector: "银行"}}
	if _, fired := autopilot.sectorFlowNegative(context.Background(), account, positions, time.Now()); fired {
		t.Fatal("净占比 -0.4% 不应触发")
	}

	// 没有资金流数据源时直接跳过，不能因为"没有数据"就认为没有流出。
	silent := &Service{}
	quiet := NewAutopilot(silent, log.New(io.Discard, "", 0))
	if _, fired := quiet.sectorFlowNegative(context.Background(), account, positions, time.Now()); fired {
		t.Fatal("没有资金流数据源时不应触发")
	}
}

type stubSectorFlow struct {
	rows map[string]float64
}

func (s stubSectorFlow) SectorFlowRanking(context.Context, string, int) ([]foundation.SectorFlowRank, foundation.SourceMeta, error) {
	ranks := make([]foundation.SectorFlowRank, 0, len(s.rows))
	for name, ratio := range s.rows {
		ranks = append(ranks, foundation.SectorFlowRank{Name: name, MainNetInflowRatio: ratio})
	}
	return ranks, foundation.SourceMeta{Source: "stub"}, nil
}

func TestEventTriggerRespectsInterval(t *testing.T) {
	autopilot, _, now := eventTestAutopilot(t)
	if _, fired := autopilot.eventTriggered(context.Background(), DefaultAccountID, now); !fired {
		t.Fatal("首次应触发")
	}
	// 换一只票也无法绕过最小间隔：间隔是通道级的，不是标的级的。
	autopilot.mu.Lock()
	autopilot.watch.fired = map[string]string{}
	autopilot.mu.Unlock()
	if _, fired := autopilot.eventTriggered(context.Background(), DefaultAccountID, now.Add(2*time.Minute)); fired {
		t.Fatal("6 分钟内不应再次触发")
	}
}

func TestEventTriggerSkipsWithoutPreviousClose(t *testing.T) {
	autopilot, service, now := eventTestAutopilot(t)
	// 把行情换成没有昨收的：此时算不出"日内跌幅"。
	service.quotes = fakeQuotes{quotes: map[string]foundation.Quote{
		"600000.SH": {Symbol: "600000.SH", Name: "浦发银行", Price: 9},
	}}
	if _, fired := autopilot.eventTriggered(context.Background(), DefaultAccountID, now); fired {
		t.Fatal("没有昨收时不应猜一个日内跌幅出来")
	}
}
