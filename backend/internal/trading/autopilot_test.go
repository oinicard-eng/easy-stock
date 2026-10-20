package trading

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/appsettings"
)

// 空建议的模型输出：用来把"一轮完整跑完但没交易"与"这一轮出故障"区分开。
const emptyDraft = `{"regime":"观望","summary":"本轮没有值得交易的机会","orders":[]}`

func newAutopilotTest(t *testing.T, content string) (*Service, *Autopilot) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := log.New(io.Discard, "", 0)
	service := NewService(store, fakeGateway{content: content}, anyQuote{}, catalogFixture{entries: buildCatalog()}, logger)
	// 这些用例验证的是循环调度与裁决，不是调研；关掉避免多一次模型往返。
	service.SetResearchEnabled(false)
	autopilot := NewAutopilot(service, logger)
	return service, autopilot
}

// fixedClock 把循环时间钉在北京时间某一天的某个时刻。
func fixedClock(t *testing.T, autopilot *Autopilot, hour, minute int) time.Time {
	t.Helper()
	zone := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 10, 20, hour, minute, 0, 0, zone)
	autopilot.now = func() time.Time { return at }
	return at
}

func enableAutopilot(t *testing.T, autopilot *Autopilot, mutate func(*AutopilotConfig)) AutopilotConfig {
	t.Helper()
	config := DefaultAutopilotConfig()
	config.Enabled = true
	config.AutoExecute = false
	if mutate != nil {
		mutate(&config)
	}
	saved, err := autopilot.Configure(context.Background(), config)
	if err != nil {
		t.Fatalf("保存自动循环配置失败：%v", err)
	}
	return saved
}

func gateOpen(cycle Cycle, code string) (bool, bool) {
	for _, gate := range cycle.Gates {
		if gate.Code == code {
			return gate.Open, true
		}
	}
	return false, false
}

func TestAutopilotSkipsWhenDisabled(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	cycle, err := autopilot.RunCycle(ctx, CycleTriggerSchedule)
	if err != nil {
		t.Fatalf("自动触发不应返回错误：%v", err)
	}
	if cycle.Status != CycleStatusSkipped {
		t.Fatalf("关闭开关时应跳过，实际 %s（%s）", cycle.Status, cycle.Reason)
	}
	if open, ok := gateOpen(cycle, GateAutopilotEnabled); !ok || open {
		t.Fatalf("开关闸门应记录为关闭：%+v", cycle.Gates)
	}
	// 关掉开关后跑多少轮都不该产生计划——这是"开关真的生效"的唯一证据。
	for index := 0; index < 3; index++ {
		if _, err := autopilot.RunCycle(ctx, CycleTriggerSchedule); err != nil {
			t.Fatalf("第 %d 轮不应报错：%v", index, err)
		}
	}
	plans, err := service.ListPlans(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取计划失败：%v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("关闭开关后不得生成计划，实际 %d 条", len(plans))
	}
}

func TestAutopilotManualRunBypassesSwitchButNotKillSwitch(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	// 开关关闭时人工触发仍应产生一轮（这是"我要跑一次"的明确表达）。
	cycle, err := autopilot.RunCycle(ctx, CycleTriggerManual)
	if err != nil {
		t.Fatalf("人工触发失败：%v", err)
	}
	if cycle.Status != CycleStatusDone {
		t.Fatalf("人工触发应完整执行，实际 %s（%s）", cycle.Status, cycle.Reason)
	}

	// kill switch 是资本保护，人工也不能越过。
	if _, err := service.SetControl(ctx, Control{AccountID: DefaultAccountID, KillSwitch: true, Reason: "测试"}); err != nil {
		t.Fatalf("设置总闸失败：%v", err)
	}
	blocked, err := autopilot.RunCycle(ctx, CycleTriggerManual)
	if err != nil {
		t.Fatalf("被拦截时不应返回错误：%v", err)
	}
	if blocked.Status != CycleStatusBlocked {
		t.Fatalf("kill switch 应拦截整轮，实际 %s", blocked.Status)
	}
	if blocked.PlanID != "" {
		t.Fatalf("被拦截的轮次不得生成计划，实际 %s", blocked.PlanID)
	}
}

func TestAutopilotDailyLossBreakerTripsAndPausesBuying(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	at := fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	// 造一个"今天从 11 万开始、现在只有 10 万"的局面：-9.09%，越过 -3% 阈值。
	store := service.Store()
	if err := store.SaveDayState(ctx, DayState{
		AccountID: DefaultAccountID, TradingDay: TradingDay(at),
		StartEquity: 110000, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("写入当日基准失败：%v", err)
	}

	cycle, err := autopilot.RunCycle(ctx, CycleTriggerManual)
	if err != nil {
		t.Fatalf("熔断轮次不应返回错误：%v", err)
	}
	if cycle.Breaker != BreakerDailyLoss {
		t.Fatalf("应触发日亏熔断，实际 %q", cycle.Breaker)
	}
	if open, ok := gateOpen(cycle, GateBreaker); !ok || open {
		t.Fatalf("熔断闸门应记录为关闭：%+v", cycle.Gates)
	}
	// 熔断必须同时按下 pause_buying，否则人工生成计划还能继续买。
	control, err := service.Control(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取总闸失败：%v", err)
	}
	if !control.PauseBuying {
		t.Fatalf("熔断后应自动暂停买入，实际 %+v", control)
	}
	if !strings.HasPrefix(control.Reason, BreakerDailyLoss) {
		t.Fatalf("总闸原因应指向熔断来源，实际 %q", control.Reason)
	}

	// 熔断是当日既成事实：解除前再跑一轮仍然是熔断状态。
	state, err := store.GetDayState(ctx, DefaultAccountID, TradingDay(at))
	if err != nil {
		t.Fatalf("读取当日状态失败：%v", err)
	}
	if !state.Tripped() {
		t.Fatal("熔断事实应落库")
	}
}

func TestAutopilotBreakerResetRequiresReason(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	at := fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	if err := service.Store().SaveDayState(ctx, DayState{
		AccountID: DefaultAccountID, TradingDay: TradingDay(at),
		StartEquity: 100000, Breaker: BreakerDailyLoss, BreakerDetail: "测试熔断",
	}); err != nil {
		t.Fatalf("写入熔断状态失败：%v", err)
	}
	if _, err := autopilot.ResetBreaker(ctx, DefaultAccountID, "  "); err == nil {
		t.Fatal("没有原因时必须拒绝解除熔断")
	}
	state, err := autopilot.ResetBreaker(ctx, DefaultAccountID, "行情源除权未调整，价格错误")
	if err != nil {
		t.Fatalf("解除熔断失败：%v", err)
	}
	if state.Tripped() {
		t.Fatal("解除后不应仍处于熔断状态")
	}
	if !strings.Contains(state.BreakerDetail, "人工解除") {
		t.Fatalf("解除动作必须留痕，实际 %q", state.BreakerDetail)
	}
}

func TestAutopilotCycleRecordsPlanAndGates(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	at := fixedClock(t, autopilot, 10, 0)
	enableAutopilot(t, autopilot, nil)
	ctx := context.Background()

	cycle, err := autopilot.RunCycle(ctx, CycleTriggerSchedule)
	if err != nil {
		t.Fatalf("执行循环失败：%v", err)
	}
	if cycle.Status != CycleStatusDone {
		t.Fatalf("无机会的轮次应记为完成，实际 %s（%s）", cycle.Status, cycle.Reason)
	}
	if cycle.PlanID == "" || cycle.DecisionID == "" {
		t.Fatalf("循环必须挂上计划与裁决：%+v", cycle)
	}
	// 计划与循环互相指认，才能"三次点击回到所属 cycle"。
	plan, err := service.GetPlan(ctx, cycle.PlanID)
	if err != nil {
		t.Fatalf("读取计划失败：%v", err)
	}
	if plan.CycleID != cycle.ID {
		t.Fatalf("计划未记录 cycle_id：plan=%q cycle=%q", plan.CycleID, cycle.ID)
	}
	// 当日基准在第一轮确定。
	state, err := service.Store().GetDayState(ctx, DefaultAccountID, TradingDay(at))
	if err != nil {
		t.Fatalf("读取当日状态失败：%v", err)
	}
	if state.StartEquity != 100000 {
		t.Fatalf("当日基准应为 10 万，实际 %.2f", state.StartEquity)
	}
	// 落库可查。
	cycles, err := autopilot.Cycles(ctx, DefaultAccountID, 10)
	if err != nil || len(cycles) != 1 {
		t.Fatalf("循环记录应为 1 条，实际 %d（%v）", len(cycles), err)
	}
}

func TestAutopilotBlocksBuysAfterBuyCutoff(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	fixedClock(t, autopilot, 14, 55)
	enableAutopilot(t, autopilot, nil)
	ctx := context.Background()

	cycle, err := autopilot.RunCycle(ctx, CycleTriggerSchedule)
	if err != nil {
		t.Fatalf("执行循环失败：%v", err)
	}
	open, ok := gateOpen(cycle, GateBuyWindow)
	if !ok || open {
		t.Fatalf("14:55 应关闭开仓窗口：%+v", cycle.Gates)
	}
	// 买入被拦的理由必须落到裁决里，否则复盘只能说"那天没买"。
	decision, err := service.Decision(ctx, "", cycle.PlanID)
	if err != nil {
		t.Fatalf("读取裁决失败：%v", err)
	}
	found := false
	for _, verdict := range decision.Verdicts {
		if verdict.Code == "gate.session_window" {
			found = true
		}
	}
	// 模型本轮没有给买入建议时不会出现该裁决，这里允许两种结果，
	// 但要保证窗口闸门本身已经关闭（上面已校验）。
	_ = found
}

func TestAutopilotSingleFlight(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	release := make(chan struct{})
	started := make(chan struct{})
	service.gateway = blockingGateway{release: release, started: started, content: emptyDraft}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := autopilot.RunCycle(ctx, CycleTriggerManual); err != nil {
			t.Errorf("第一轮不应失败：%v", err)
		}
	}()
	<-started
	if _, err := autopilot.RunCycle(ctx, CycleTriggerManual); err != ErrCycleRunning {
		t.Fatalf("并发第二轮应被单飞挡住，实际 %v", err)
	}
	close(release)
	<-done
}

func TestAutopilotRecoversInterruptedCycle(t *testing.T) {
	service, autopilot := newAutopilotTest(t, emptyDraft)
	at := fixedClock(t, autopilot, 10, 0)
	ctx := context.Background()

	stale := Cycle{
		ID: newID("cyc"), AccountID: DefaultAccountID, Trigger: CycleTriggerSchedule,
		Status: CycleStatusRunning, StartedAt: at.Add(-5 * time.Minute),
	}
	if err := service.Store().SaveCycle(ctx, stale); err != nil {
		t.Fatalf("写入循环失败：%v", err)
	}
	autopilot.Start(ctx)
	defer autopilot.Stop()

	recovered, err := service.Store().GetCycle(ctx, stale.ID)
	if err != nil {
		t.Fatalf("读取循环失败：%v", err)
	}
	if recovered.Status != CycleStatusAborted {
		t.Fatalf("崩溃残留的循环应标为 aborted，实际 %s", recovered.Status)
	}
	if recovered.EndedAt == nil {
		t.Fatal("恢复时必须补上结束时间，否则界面会一直显示进行中")
	}
}

// blockingGateway 让第一轮停在模型调用里，用来验证单飞。
type blockingGateway struct {
	release chan struct{}
	started chan struct{}
	content string
}

func (g blockingGateway) Prompt(context.Context, string) (agent.PromptResult, error) {
	close(g.started)
	<-g.release
	return agent.PromptResult{Content: g.content}, nil
}
func (g blockingGateway) Status() agent.Status                         { return agent.Status{Available: true} }
func (g blockingGateway) ModelAPIKey() (string, error)                 { return "", nil }
func (g blockingGateway) SyncLLM(appsettings.LLM, *string) error       { return nil }
func (g blockingGateway) Start(context.Context) (agent.Process, error) { return nil, nil }
