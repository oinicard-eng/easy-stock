package trading

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFormatPriorResearchKeepsFactsAndAge(t *testing.T) {
	plannedAt := time.Date(2026, 10, 21, 9, 30, 0, 0, cstZone())
	now := plannedAt.Add(9 * time.Minute)
	result := ResearchResult{
		Enabled:    true,
		Conclusion: "算力板块主力净流入 12 亿，居概念板块第一",
		Steps: []ResearchStep{
			{Tool: "list_sector_flow", Args: map[string]any{"dimension": "concept"}, Result: "板块资金流（concept）：\n名称 | 代码 | 涨跌% |\n算力概念 | BK0917 | 2.10 |"},
			{Tool: "get_announcements", Args: map[string]any{"symbol": "600519"}, Result: "600519 公告 2 条：\n10-20 | 减持 | 股东减持计划"},
			{Tool: "get_kline", Error: "日线获取失败", Result: ""},
		},
	}
	text := formatPriorResearch(result, plannedAt, now)

	if !strings.Contains(text, "9 分钟前") {
		t.Fatalf("必须标注新鲜度，否则模型会把半小时前的资金流当现价依据：%s", text)
	}
	if !strings.Contains(text, "list_sector_flow") || !strings.Contains(text, "算力概念") {
		t.Fatalf("应保留工具名与事实：%s", text)
	}
	if strings.Contains(text, "日线获取失败") {
		t.Fatalf("失败的步骤不应进入复用段：%s", text)
	}
	if !strings.Contains(text, "当时结论") {
		t.Fatalf("应带上当时的结论：%s", text)
	}
}

func TestFirstMeaningfulLineSkipsHeaders(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"表头在前", "代码 | 名称 | 现价 |\n600519 | 贵州茅台 | 1680.00", "600519 | 贵州茅台 | 1680.00"},
		{"标题行跳过", "600519 公告 2 条：\n10-20 | 减持", "10-20 | 减持"},
		{"只有标题时视为无事实", "板块资金流：\n", ""},
		{"全空", "\n\n", ""},
	}
	for _, item := range cases {
		if got := firstMeaningfulLine(item.text); got != item.want {
			t.Fatalf("%s：期望 %q，实际 %q", item.name, item.want, got)
		}
	}
}

func TestPriorResearchWindowExpires(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	write := func(id string, createdAt time.Time) {
		plan := Plan{
			ID: id, AccountID: DefaultAccountID, Status: PlanStatusDraft,
			CreatedAt: createdAt, UpdatedAt: createdAt,
			Research: &ResearchResult{
				Enabled:    true,
				Conclusion: "机器人板块 5 日净流出 74 亿",
				Steps: []ResearchStep{
					{Tool: "list_sector_flow", Result: "板块资金流（concept）：\n机器人 | BK0868 | -1.20 |"},
				},
			},
		}
		if err := store.CreatePlan(ctx, plan); err != nil {
			t.Fatalf("写入计划失败：%v", err)
		}
	}

	service := &Service{store: store}

	write("plan-fresh", now.Add(-3*time.Minute))
	if text := service.PriorResearchForPrompt(ctx, DefaultAccountID, now); !strings.Contains(text, "机器人板块 5 日净流出") {
		t.Fatalf("3 分钟前的调研应被复用，实际：%q", text)
	}

	// 超窗口：复用半小时前的观察，比不复用更危险。
	store2 := newTestStore(t)
	service2 := &Service{store: store2}
	old := Plan{
		ID: "plan-old", AccountID: DefaultAccountID, Status: PlanStatusDraft,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		Research: &ResearchResult{Enabled: true, Conclusion: "两小时前的结论"},
	}
	if err := store2.CreatePlan(ctx, old); err != nil {
		t.Fatalf("写入旧计划失败：%v", err)
	}
	if text := service2.PriorResearchForPrompt(ctx, DefaultAccountID, now); text != "" {
		t.Fatalf("超出窗口的调研不应复用，实际：%q", text)
	}
}

func TestPriorResearchSkipsDisabledResearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plan := Plan{
		ID: "plan-noresearch", AccountID: DefaultAccountID, Status: PlanStatusDraft,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
		Research: &ResearchResult{Enabled: false, Note: "模型不可用"},
	}
	if err := store.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("写入计划失败：%v", err)
	}
	service := &Service{store: store}
	if text := service.PriorResearchForPrompt(ctx, DefaultAccountID, now); text != "" {
		t.Fatalf("未真正发起调研的轮次不应复用，实际：%q", text)
	}
}
