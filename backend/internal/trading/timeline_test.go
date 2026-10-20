package trading

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/trading/policy"
)

// 时间线必须把五张表按时间摊平成一条线。
// 这是排障时第一个要看的东西：不建立时间感，"那天为什么没买"就只能靠猜。
func TestTimelineMergesSourcesInTimeOrder(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	cst := time.FixedZone("CST", 8*3600)
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, cst)
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 10, 20, hour, minute, 0, 0, cst)
	}

	ended := at(9, 35)
	if err := store.SaveCycle(ctx, Cycle{
		ID:         "cyc-1",
		AccountID:  DefaultAccountID,
		Trigger:    CycleTriggerSchedule,
		Status:     CycleStatusDone,
		StartedAt:  at(9, 30),
		EndedAt:    &ended,
		DurationMS: 300000,
		Orders:     2,
		Filled:     1,
	}); err != nil {
		t.Fatalf("写入循环失败: %v", err)
	}
	if err := store.CreatePlan(ctx, Plan{
		ID:        "plan-1",
		AccountID: DefaultAccountID,
		Status:    PlanStatusDraft,
		Summary:   "买入两只机器人产业链标的",
		CreatedAt: at(9, 31),
		UpdatedAt: at(9, 31),
	}); err != nil {
		t.Fatalf("写入计划失败: %v", err)
	}
	if _, err := store.EnsureAccount(ctx, DefaultAccountID, DefaultInitialCash); err != nil {
		t.Fatalf("建立账户失败: %v", err)
	}
	created, err := store.CreateOrders(ctx, []Order{{
		ID: "ord-1", PlanID: "plan-1", AccountID: DefaultAccountID,
		Symbol: "688165.SH", Name: "埃夫特", Side: SideBuy,
		OrderType: OrderTypeLimit,
		Quantity:  800, LimitPrice: 14.67,
		Status:    OrderStatusPending,
		CreatedAt: at(9, 32), UpdatedAt: at(9, 32),
	}})
	if err != nil {
		t.Fatalf("写入委托失败: %v", err)
	}
	// 标记成交：时间线要用 updated_at 近似成交时刻。
	filled := created[0]
	filled.Status = OrderStatusFilled
	filled.FilledQuantity = 800
	filled.FilledPrice = 14.67
	filled.UpdatedAt = at(9, 33)
	if err := store.UpdateOrder(ctx, filled); err != nil {
		t.Fatalf("更新委托失败: %v", err)
	}
	if err := store.SaveDecision(ctx, Decision{
		ID:        "dec-1",
		AccountID: DefaultAccountID,
		PlanID:    "plan-1",
		TakenAt:   at(9, 32),
		Stats:     policy.Stats{Proposed: 2, Accepted: 1, Blocked: 1},
		Narrative: []string{"板块资金流三日净流入", "行业集中度未超限"},
		Model:     ModelRun{TotalTokens: 1200},
	}); err != nil {
		t.Fatalf("写入裁决失败: %v", err)
	}
	if err := store.AppendEvent(ctx, Event{
		AccountID: DefaultAccountID,
		Severity:  EventSeverityWarning,
		Kind:      EventBreakerArmed,
		Message:   "当日亏损熔断",
		CreatedAt: at(14, 0),
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
	// 前一天的记录不应出现在今天的时间线上。
	if err := store.AppendEvent(ctx, Event{
		AccountID: DefaultAccountID,
		Severity:  EventSeverityInfo,
		Kind:      "yesterday",
		Message:   "昨天的",
		CreatedAt: at(12, 0).AddDate(0, 0, -1),
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}

	timeline, err := store.Timeline(ctx, DefaultAccountID, day)
	if err != nil {
		t.Fatalf("生成时间线失败: %v", err)
	}
	if timeline.Day != "2026-10-20" {
		t.Fatalf("日期应为 2026-10-20，实际 %s", timeline.Day)
	}
	if len(timeline.Notes) != 0 {
		t.Fatalf("不应有降级说明：%v", timeline.Notes)
	}
	if len(timeline.Entries) == 0 {
		t.Fatal("时间线不应为空")
	}
	// 严格递增：任何一条比前一条更早，说明归并顺序错了。
	for i := 1; i < len(timeline.Entries); i++ {
		if timeline.Entries[i].At.Before(timeline.Entries[i-1].At) {
			t.Fatalf("第 %d 条时间早于前一条：%s < %s",
				i, timeline.Entries[i].At, timeline.Entries[i-1].At)
		}
	}
	// 昨天的记录不能混进来。
	for _, entry := range timeline.Entries {
		if entry.Title == "yesterday" {
			t.Fatal("前一天的事件不应出现在当天时间线")
		}
	}
	if timeline.Stats.Cycles != 1 || timeline.Stats.Plans != 1 || timeline.Stats.Decisions != 1 {
		t.Fatalf("统计不正确：%+v", timeline.Stats)
	}
	if timeline.Stats.ModelCalls != 1 || timeline.Stats.TotalTokens != 1200 {
		t.Fatalf("模型调用统计不正确：%+v", timeline.Stats)
	}
	if timeline.Stats.Filled != 1 {
		t.Fatalf("成交数应为 1，实际 %d", timeline.Stats.Filled)
	}
	if timeline.Stats.Events != 1 {
		t.Fatalf("事件数应为 1，实际 %d", timeline.Stats.Events)
	}
}

// 同一时刻的多条记录要有稳定顺序：循环先于计划、计划先于裁决、裁决先于委托。
// 顺序不稳定时，人每次刷新看到的因果链都不一样。
func TestTimelineOrdersSameInstantByCategory(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	cst := time.FixedZone("CST", 8*3600)
	instant := time.Date(2026, 10, 20, 9, 30, 0, 0, cst)
	if err := store.SaveCycle(ctx, Cycle{
		ID: "cyc-x", AccountID: DefaultAccountID, Trigger: CycleTriggerManual,
		Status: CycleStatusRunning, StartedAt: instant,
	}); err != nil {
		t.Fatalf("写入循环失败: %v", err)
	}
	if err := store.CreatePlan(ctx, Plan{
		ID: "plan-x", AccountID: DefaultAccountID, Status: PlanStatusDraft,
		CreatedAt: instant, UpdatedAt: instant,
	}); err != nil {
		t.Fatalf("写入计划失败: %v", err)
	}

	timeline, err := store.Timeline(ctx, DefaultAccountID, instant)
	if err != nil {
		t.Fatalf("生成时间线失败: %v", err)
	}
	var cycleIndex, planIndex = -1, -1
	for i, entry := range timeline.Entries {
		switch entry.Category {
		case TimelineCycleStart:
			cycleIndex = i
		case TimelinePlan:
			planIndex = i
		}
	}
	if cycleIndex < 0 || planIndex < 0 {
		t.Fatal("循环与计划都应出现在时间线里")
	}
	if cycleIndex > planIndex {
		t.Fatalf("同一时刻循环应排在计划之前，实际 cycle=%d plan=%d", cycleIndex, planIndex)
	}
}

// 空白天必须返回空列表而不是报错，并且 entries 是空数组而非 null——
// 前端拿到 null 再去 .length 就是一次白屏。
func TestTimelineEmptyDayReturnsEmptyArray(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer store.Close()

	timeline, err := store.Timeline(context.Background(), DefaultAccountID,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("空白天不应报错: %v", err)
	}
	if timeline.Entries == nil {
		t.Fatal("entries 必须初始化为空切片，不能是 nil")
	}
	if len(timeline.Entries) != 0 {
		t.Fatalf("空白天不应有记录，实际 %d 条", len(timeline.Entries))
	}
}
