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
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 10.5),
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
		t.Fatal("持仓日内跌 4.76%（相对昨收 10.5）应触发")
	}
	if !strings.Contains(reason, "日内跌") {
		t.Fatalf("触发原因应说明日内跌幅，实际：%s", reason)
	}
	// 同一只票当日已触发过：再次调用不该重复触发。
	if _, again := autopilot.eventTriggered(context.Background(), DefaultAccountID, now.Add(10*time.Minute)); again {
		t.Fatal("同一标的当日只应触发一次")
	}
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
