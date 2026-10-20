package trading

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"testing"
	"time"
)

func newTestAutopilot(t *testing.T, store *Store) *Autopilot {
	t.Helper()
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))
	return NewAutopilot(service, log.New(io.Discard, "", 0))
}

// 事件流必须既能按等级过滤，也要保证"新的在前"。
// 顺序错了，运维页面第一条就永远是那条早就处理过的旧告警。
func TestAppendAndListEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for index, item := range []struct {
		severity string
		kind     string
		message  string
	}{
		{EventSeverityInfo, EventAutopilotStarted, "启动"},
		{EventSeverityWarning, EventBreakerArmed, "熔断装弹"},
		{EventSeverityCritical, EventLedgerDrift, "账本漂移"},
	} {
		if err := store.AppendEvent(ctx, Event{
			AccountID: DefaultAccountID, Severity: item.severity, Kind: item.kind,
			Message: item.message, CreatedAt: time.Now().UTC().Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatalf("写入事件失败：%v", err)
		}
	}

	all, err := store.ListEvents(ctx, DefaultAccountID, "", "", 10)
	if err != nil {
		t.Fatalf("读取事件失败：%v", err)
	}
	if len(all) != 3 {
		t.Fatalf("期望 3 条事件，实际 %d", len(all))
	}
	if all[0].Kind != EventLedgerDrift {
		t.Fatalf("事件应按新到旧排序，首条实际为 %s", all[0].Kind)
	}

	critical, err := store.ListEvents(ctx, DefaultAccountID, EventSeverityCritical, "", 10)
	if err != nil {
		t.Fatalf("按等级过滤失败：%v", err)
	}
	if len(critical) != 1 || critical[0].Kind != EventLedgerDrift {
		t.Fatalf("按 critical 过滤结果不符合预期：%+v", critical)
	}

	count, err := store.CountEventsSince(ctx, DefaultAccountID, EventLedgerDrift, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("统计事件失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("期望统计到 1 条，实际 %d", count)
	}
}

// 账本漂移必须判成 critical：这类问题不会自愈，只会在下一次下注时放大。
func TestHealthFlagsLedgerDriftAsCritical(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 直接改账户冻结额，制造"账面冻结与挂单不符"。
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_accounts SET reserved_cash=500 WHERE id=?`, DefaultAccountID); err != nil {
		t.Fatalf("制造漂移失败：%v", err)
	}

	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))
	health, err := service.Health(ctx, DefaultAccountID, Heartbeat{Running: true, LastTickAt: ptrTime(time.Now())})
	if err != nil {
		t.Fatalf("健康检查失败：%v", err)
	}
	if health.Status != HealthCritical {
		t.Fatalf("账本漂移应判 critical，实际 %s（issues=%+v）", health.Status, health.Issues)
	}
	if health.Ledger.OK {
		t.Fatal("漂移时 Ledger.OK 必须为 false")
	}
	if health.Ledger.Drift != 500 {
		t.Fatalf("期望漂移 500，实际 %.2f", health.Ledger.Drift)
	}
}

// 干净账本 + 正常心跳必须是 healthy：否则健康检查会变成"永远报警"，
// 报警一旦永远亮着，就等于没有报警。
func TestHealthHealthyWhenLedgerClean(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))

	health, err := service.Health(ctx, DefaultAccountID, Heartbeat{Running: true, LastTickAt: ptrTime(time.Now())})
	if err != nil {
		t.Fatalf("健康检查失败：%v", err)
	}
	if health.Status != HealthHealthy {
		t.Fatalf("期望 healthy，实际 %s（issues=%+v）", health.Status, health.Issues)
	}
	if !health.Ledger.OK {
		t.Fatalf("空账户账本应一致：%+v", health.Ledger)
	}
}

// 调度心跳超时必须判 critical：进程活着但循环卡死，从端口是看不出来的。
func TestHealthCriticalWhenTickStale(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	service := NewService(store, nil, nil, nil, log.New(io.Discard, "", 0))

	stale := time.Now().Add(-30 * time.Minute)
	health, err := service.Health(ctx, DefaultAccountID, Heartbeat{Running: true, LastTickAt: &stale})
	if err != nil {
		t.Fatalf("健康检查失败：%v", err)
	}
	if health.Status != HealthCritical {
		t.Fatalf("心跳超时应判 critical，实际 %s", health.Status)
	}
}

// 连续失败 3 轮必须自动暂停买入，并把降级事实写进事件流。
// "今天没买"和"系统坏了所以没买"在页面上必须能区分开。
func TestAutopilotDegradesAfterRepeatedFailures(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	autopilot := newTestAutopilot(t, store)

	for i := 0; i < degradeAfterFailures; i++ {
		autopilot.recordFailure(ctx, DefaultAccountID, errors.New("模型超时"))
	}

	control, err := store.GetControl(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取门控失败：%v", err)
	}
	if !control.PauseBuying {
		t.Fatal("连续失败后应自动暂停买入")
	}
	if control.KillSwitch {
		t.Fatal("降级不应触发 kill switch：卖出必须保留")
	}

	beat := autopilot.Heartbeat()
	if !beat.Degraded {
		t.Fatal("心跳应标记为已降级")
	}
	if beat.FailureStreak != degradeAfterFailures {
		t.Fatalf("期望连续失败 %d 轮，实际 %d", degradeAfterFailures, beat.FailureStreak)
	}

	events, err := store.ListEvents(ctx, DefaultAccountID, EventSeverityCritical, EventAutopilotDegraded, 10)
	if err != nil {
		t.Fatalf("读取事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("降级应写入 1 条 critical 事件，实际 %d", len(events))
	}
}

// 降级后连续成功足够多次要自动解除：无人值守时没人会来点"恢复"。
func TestAutopilotRecoversAfterSuccesses(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	autopilot := newTestAutopilot(t, store)

	for i := 0; i < degradeAfterFailures; i++ {
		autopilot.recordFailure(ctx, DefaultAccountID, errors.New("行情中断"))
	}
	for i := 0; i < recoverAfterSuccesses; i++ {
		autopilot.recordSuccess(ctx, DefaultAccountID)
	}

	control, err := store.GetControl(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读取门控失败：%v", err)
	}
	if control.PauseBuying {
		t.Fatal("连续成功后应自动解除买入暂停")
	}
	if autopilot.Heartbeat().Degraded {
		t.Fatal("心跳应已清除降级标记")
	}
}

// 中途成功一次就清零：说明通道是通的，不该继续按"连续故障"计数。
func TestAutopilotSuccessResetsFailureStreak(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	autopilot := newTestAutopilot(t, store)

	autopilot.recordFailure(ctx, DefaultAccountID, errors.New("超时"))
	autopilot.recordFailure(ctx, DefaultAccountID, errors.New("超时"))
	autopilot.recordSuccess(ctx, DefaultAccountID)
	if streak := autopilot.Heartbeat().FailureStreak; streak != 0 {
		t.Fatalf("成功后连续失败计数应清零，实际 %d", streak)
	}

	autopilot.recordFailure(ctx, DefaultAccountID, errors.New("超时"))
	if autopilot.Heartbeat().Degraded {
		t.Fatal("清零后单次失败不应再次触发降级")
	}
}

// self-heal：漂移能被自动修复，并且留下一条"已修复"事件而不是"漂移"告警。
func TestHealLedgerRepairsDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	autopilot := newTestAutopilot(t, store)

	stored, err := store.CreateOrders(ctx, []Order{testBuyOrder("600000.SH", 1000, 10)})
	if err != nil {
		t.Fatalf("落库失败：%v", err)
	}
	expected := stored[0].ReservedAmount
	// 把冻结额改成明显错误的值，模拟"释放路径算错"。
	if _, err := store.db.ExecContext(ctx, `UPDATE trading_accounts SET reserved_cash=? WHERE id=?`, expected+321, DefaultAccountID); err != nil {
		t.Fatalf("制造漂移失败：%v", err)
	}

	autopilot.healLedger(ctx, DefaultAccountID, time.Now())

	report, err := store.ReconcileReservations(ctx, DefaultAccountID, false)
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if report.Drift > healthDriftTolerance || report.Drift < -healthDriftTolerance {
		t.Fatalf("漂移应已被修复，实际 %.2f", report.Drift)
	}

	events, err := store.ListEvents(ctx, DefaultAccountID, "", EventLedgerDriftHealed, 10)
	if err != nil {
		t.Fatalf("读取事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("修复应留下 1 条事件，实际 %d", len(events))
	}
	payload := map[string]any{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("事件 payload 应可解析：%v", err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
