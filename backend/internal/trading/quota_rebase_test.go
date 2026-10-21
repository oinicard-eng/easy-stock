package trading

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

func newQuotaAutopilot(t *testing.T) *Autopilot {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := NewService(store, fakeGateway{}, fakeQuotes{}, nil, log.New(io.Discard, "", 0))
	autopilot := NewAutopilot(service, log.New(io.Discard, "", 0))
	autopilot.now = func() time.Time { return cstAt(14, 0) }
	return autopilot
}

// seedCycles 直接写 N 条今日循环记录，模拟"今天已经跑过这么多轮"。
func seedCycles(t *testing.T, autopilot *Autopilot, count int) {
	t.Helper()
	ctx := context.Background()
	now := autopilot.now()
	for index := 0; index < count; index++ {
		cycle := Cycle{
			ID:        newID("cyc"),
			AccountID: DefaultAccountID,
			Trigger:   CycleTriggerSchedule,
			Status:    CycleStatusDone,
			StartedAt: now.Add(-time.Duration(count-index) * time.Minute).UTC(),
		}
		if err := autopilot.store.SaveCycle(ctx, cycle); err != nil {
			t.Fatalf("写入循环记录失败：%v", err)
		}
	}
}

// 配额从 240 调到 27 时，当天已经跑掉的轮次不能把剩下的时间锁死。
// 这条规则的意义：改配额只影响"以后还能跑几轮"，不是对当天历史的追认。
func TestQuotaRebaseWhenQuotaLowered(t *testing.T) {
	autopilot := newQuotaAutopilot(t)
	ctx := context.Background()
	seedCycles(t, autopilot, 52)

	if got := autopilot.usedCyclesForQuota(ctx, DefaultAccountID, autopilot.now()); got != 52 {
		t.Fatalf("未改配额前应读到 52 轮，实际 %d", got)
	}

	// 先存一份旧配置，再把它降下来——模拟用户把额度从 240 调到 27。
	if _, err := autopilot.Configure(ctx, AutopilotConfig{Enabled: true, IntervalSeconds: 900, MaxCyclesPerDay: 240}); err != nil {
		t.Fatalf("保存旧配置失败：%v", err)
	}
	saved, err := autopilot.Configure(ctx, AutopilotConfig{Enabled: true, IntervalSeconds: 900, MaxCyclesPerDay: 27})
	if err != nil {
		t.Fatalf("保存新配置失败：%v", err)
	}
	if saved.MaxCyclesPerDay != 27 {
		t.Fatalf("配额应为 27，实际 %d", saved.MaxCyclesPerDay)
	}
	// 基线之后从零算起，所以还能跑满 27 轮，而不是"已超 25 轮、直接停摆"。
	if got := autopilot.usedCyclesForQuota(ctx, DefaultAccountID, autopilot.now()); got != 0 {
		t.Fatalf("改配额后应从 0 起算，实际 %d", got)
	}
}

// 基线写坏时按"无基线"处理：宁可少跑几轮，也不能让一条脏数据把额度放大。
func TestQuotaRebaseIgnoresCorruptOrInconsistent(t *testing.T) {
	autopilot := newQuotaAutopilot(t)
	ctx := context.Background()
	now := autopilot.now()
	seedCycles(t, autopilot, 10)
	key := quotaRebaseKey(DefaultAccountID, TradingDay(now))

	cases := []struct {
		value string
		note  string
	}{
		{"abc", "不是数字"},
		{"-5", "负数"},
		{"0", "零"},
		{"99", "比当天实际轮次还大（数据不一致）"},
	}
	for _, c := range cases {
		if err := autopilot.store.SetState(ctx, key, c.value); err != nil {
			t.Fatalf("写入状态失败：%v", err)
		}
		if got := autopilot.usedCyclesForQuota(ctx, DefaultAccountID, now); got != 10 {
			t.Fatalf("基线为 %q（%s）时应回落到实际轮次 10，实际 %d", c.value, c.note, got)
		}
	}
}

// 配额没变时不该重置基线：否则反复保存同一个配置就能无限刷额度。
func TestQuotaRebaseOnlyOnQuotaChange(t *testing.T) {
	autopilot := newQuotaAutopilot(t)
	ctx := context.Background()
	if _, err := autopilot.Configure(ctx, AutopilotConfig{Enabled: true, IntervalSeconds: 900, MaxCyclesPerDay: 30}); err != nil {
		t.Fatalf("首次保存失败：%v", err)
	}
	seedCycles(t, autopilot, 8)

	// 只改别的字段，配额不变。
	if _, err := autopilot.Configure(ctx, AutopilotConfig{Enabled: true, IntervalSeconds: 600, MaxCyclesPerDay: 30}); err != nil {
		t.Fatalf("再次保存失败：%v", err)
	}
	if got := autopilot.usedCyclesForQuota(ctx, DefaultAccountID, autopilot.now()); got != 8 {
		t.Fatalf("配额未变时不应重置基线，期望仍读到 8 轮，实际 %d", got)
	}
}
