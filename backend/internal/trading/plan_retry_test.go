package trading

// 模型偶发不守格式时的重试兜底。
//
// 实测背景：某天 8 轮自动循环里有 1 轮失败，原因是"模型输出中没有 JSON 对象"
// ——那一轮的调研恰好步数用完、结论停在半句话上，模型顺着半截结论继续想，
// 整条回复没有出现 {。这类失败与策略无关，纯粹是格式抖动，但代价是整轮作废、
// 这一轮的机会直接消失。所以解析失败要重试一次，而不是立刻把计划标为失败。

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/appsettings"
	"easy-stock/backend/internal/foundation"
)

// sequenceGateway 按调用序号返回不同内容。值类型的 fakeGateway 每次返回
// 同一个串，而重试的意义正在于第二次拿到不同的回复。
type sequenceGateway struct {
	mu       sync.Mutex
	contents []string
	calls    int
}

func (g *sequenceGateway) Prompt(context.Context, string) (agent.PromptResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	index := g.calls
	if index >= len(g.contents) {
		index = len(g.contents) - 1
	}
	g.calls++
	return agent.PromptResult{Content: g.contents[index]}, nil
}

func (g *sequenceGateway) Status() agent.Status                         { return agent.Status{Available: true} }
func (g *sequenceGateway) ModelAPIKey() (string, error)                 { return "", nil }
func (g *sequenceGateway) SyncLLM(appsettings.LLM, *string) error       { return nil }
func (g *sequenceGateway) Start(context.Context) (agent.Process, error) { return nil, nil }

func (g *sequenceGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestPlanParseFailureRetriesOnce(t *testing.T) {
	// 第一次：只有思考文字，没有 JSON 对象（复现实测里的失败形态）。
	// 第二次：合法计划。重试应当把这一轮救回来。
	valid := `{"regime":"观望","summary":"重试后给出空计划","orders":[]}`
	gateway := &sequenceGateway{contents: []string{"我需要再确认一下板块资金，先看通信链是否延续……", valid}}
	store := newCooldownStore(t)
	service := NewService(store, gateway, fakeQuotes{quotes: map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	}}, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(false)

	plan, err := service.RunPlanSync(context.Background(), PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced",
	}, "cyc-retry")
	if err != nil {
		t.Fatalf("解析失败后应重试成功，实际失败：%v", err)
	}
	// 空计划本身是合法结论（"模型未给出可执行建议"），关键是不能残留
	// 解析错误——那说明救回来的只是 err，不是这一轮。
	if strings.Contains(plan.ErrorMessage, "JSON") {
		t.Fatalf("重试成功后不该残留解析错误：%s", plan.ErrorMessage)
	}
	if got := gateway.count(); got != 2 {
		t.Fatalf("应恰好调用模型两次（首发 + 一次重试），实际 %d 次", got)
	}
}

func TestPlanParseFailureFailsAfterSingleRetry(t *testing.T) {
	// 两次都不给 JSON：不能无限重试，必须如实失败。
	gateway := &sequenceGateway{contents: []string{"还是只有文字", "依然没有结构"}}
	store := newCooldownStore(t)
	service := NewService(store, gateway, fakeQuotes{quotes: map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	}}, nil, log.New(io.Discard, "", 0))
	service.SetResearchEnabled(false)

	if _, err := service.RunPlanSync(context.Background(), PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced",
	}, "cyc-retry-fail"); err == nil {
		t.Fatal("两次都无法解析时应返回错误，不能假装成功")
	}
	if got := gateway.count(); got != 2 {
		t.Fatalf("重试只做一次，实际调用 %d 次", got)
	}
}
