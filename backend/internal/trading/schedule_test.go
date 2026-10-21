package trading

import (
	"testing"
	"time"
)

// cstAt 构造一个北京时间的时刻，测试不依赖机器时区。
func cstAt(hour, minute int) time.Time {
	return time.Date(2026, 10, 21, hour, minute, 0, 0, time.FixedZone("CST", 8*3600))
}

func TestIntervalAtFollowsSchedule(t *testing.T) {
	config := DefaultAutopilotConfig().Normalize()
	cases := []struct {
		hour, minute int
		want         int
		note         string
	}{
		{9, 30, 900, "开盘第一分钟落在开盘档"},
		{9, 59, 900, "开盘档右边界内"},
		{10, 0, 900, "10:00 起是盘中档"},
		{11, 29, 900, "上午收盘前"},
		{13, 0, 900, "午后开盘"},
		{14, 19, 900, "尾盘档之前"},
		{14, 20, 600, "14:20 起加密到 10 分钟"},
		{14, 59, 600, "收盘前"},
	}
	for _, c := range cases {
		if got := config.IntervalAt(cstAt(c.hour, c.minute)); got != c.want {
			t.Errorf("%02d:%02d 应返回 %ds（%s），实际 %ds", c.hour, c.minute, c.want, c.note, got)
		}
	}
}

// 排班之外的时间必须回落，而不是返回 0——返回 0 会让 setNext 把下一轮设在"现在"，
// 变成死循环式的连续调用，恰好是这套排班要避免的事。
func TestIntervalAtFallsBackOutsideSchedule(t *testing.T) {
	config := DefaultAutopilotConfig().Normalize()
	for _, c := range [][2]int{{8, 30}, {12, 0}, {15, 30}, {20, 0}} {
		got := config.IntervalAt(cstAt(c[0], c[1]))
		if got != config.IntervalSeconds {
			t.Errorf("%02d:%02d 应回落到 %ds，实际 %ds", c[0], c[1], config.IntervalSeconds, got)
		}
		if got <= 0 {
			t.Fatalf("%02d:%02d 返回了非正间隔 %d，会让调度原地空转", c[0], c[1], got)
		}
	}
}

// 坏行只跳过自己：一个手误的时段不该拖累其它三行，更不该让整轮调度失效。
func TestIntervalAtSkipsBrokenTier(t *testing.T) {
	config := AutopilotConfig{
		IntervalSeconds: 1800,
		Schedule: []ScheduleTier{
			{From: "9:3O", To: "10:00", IntervalSeconds: 120},  // 字母 O 写错
			{From: "10:00", To: "10:00", IntervalSeconds: 120}, // 空区间
			{From: "11:00", To: "11:30", IntervalSeconds: 0},   // 间隔为 0
			{From: "14:00", To: "15:00", IntervalSeconds: 300},
		},
	}
	if got := config.IntervalAt(cstAt(9, 45)); got != 1800 {
		t.Errorf("坏行不应命中，应回落到 1800，实际 %d", got)
	}
	if got := config.IntervalAt(cstAt(14, 30)); got != 300 {
		t.Errorf("坏行之后的有效行仍应生效，期望 300，实际 %d", got)
	}
}

// 空排班＝用户明确要"全天统一间隔"，必须尊重；nil＝字段缺失的老配置，补默认。
// 这两者在 JSON 里可区分（null vs []），不能合并处理。
func TestNormalizeDistinguishesNilAndEmptySchedule(t *testing.T) {
	legacy := AutopilotConfig{IntervalSeconds: 900}
	if got := legacy.Normalize(); len(got.Schedule) == 0 {
		t.Fatal("字段缺失的老配置应补上默认排班")
	}

	explicit := AutopilotConfig{IntervalSeconds: 900, Schedule: []ScheduleTier{}}
	if got := explicit.Normalize(); len(got.Schedule) != 0 {
		t.Fatalf("显式空排班应保留，实际变成 %d 行", len(got.Schedule))
	}
	if got := explicit.Normalize().IntervalAt(cstAt(9, 45)); got != 900 {
		t.Errorf("空排班时应全天用 900s，实际 %d", got)
	}
}

// 排班把一天的轮次从 240 压到 17 左右，配额必须跟着降，
// 否则配额虽然放宽了，成本却由调度决定——两边不一致会让"配额"失去防呆意义。
func TestNormalizeQuotaMatchesSchedule(t *testing.T) {
	config := DefaultAutopilotConfig().Normalize()
	// 默认排班：30/15 + 90/15 + 80/15 + 40/10 = 2 + 6 + 5 + 4 = 17 轮，加 10 轮事件余量。
	if config.MaxCyclesPerDay != 27 {
		t.Fatalf("默认排班配额应为 27（17 轮排班 + 10 轮事件余量），实际 %d", config.MaxCyclesPerDay)
	}

	// 旧版 40 轮必须被升级：它是"从没改过配置"的痕迹，留着就还是会在盘中停摆。
	old := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: legacyMaxCyclesPerDay}
	if got := old.Normalize().MaxCyclesPerDay; got != 27 {
		t.Fatalf("旧配额 40 应按排班升级为 27，实际 %d", got)
	}

	// 用户显式设的值不能被改：这条边界写错过一次，会把调低的额度悄悄改大。
	explicit := AutopilotConfig{IntervalSeconds: 900, MaxCyclesPerDay: 12}
	if got := explicit.Normalize().MaxCyclesPerDay; got != 12 {
		t.Fatalf("用户显式设置的 12 轮应保留，实际 %d", got)
	}
}

// 语义校验：这个排班存在的理由就是"少问模型"，
// 把一天的轮次算出来直接和成本挂钩，避免以后有人把间隔悄悄改回 60 秒。
func TestDefaultScheduleKeepsDailyCyclesLow(t *testing.T) {
	cycles := cyclesForSchedule(DefaultSchedule(), 900)
	if cycles > 40 {
		t.Fatalf("默认排班一天 %d 轮过多，按实测单价约 %.1f 元/天，超出可接受范围", cycles, float64(cycles)*0.12)
	}
	if cycles < 8 {
		t.Fatalf("默认排班一天只有 %d 轮，盘中几乎不再观察，失去自动交易的意义", cycles)
	}
}

// 存量配置的迁移：老库里没有排班字段，配额是"按单一间隔覆盖全天"算出来的。
// 排班上线后必须一起重算，否则界面显示"0/240"会让人以为还能跑 240 轮，
// 而实际调度只有十几轮——额度没超支，但它掩盖了真实的成本节奏。
func TestNormalizeMigratesLegacyQuotaWhenScheduleBackfilled(t *testing.T) {
	// 这就是今天库里的真实状态：60 秒间隔、240 轮配额、没有排班字段。
	legacy := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: 240}
	got := legacy.Normalize()
	if got.MaxCyclesPerDay != 27 {
		t.Fatalf("老配置补排班后配额应重算为 27，实际 %d", got.MaxCyclesPerDay)
	}

	// 手改过的配额不能被连带覆盖：它不是自动推导值。
	handTuned := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: 12}
	if got := handTuned.Normalize().MaxCyclesPerDay; got != 12 {
		t.Fatalf("手改的 12 轮应保留，实际 %d", got)
	}

	// 已经显式配了排班的配置，配额不被重算。
	explicit := AutopilotConfig{
		IntervalSeconds: 60,
		MaxCyclesPerDay: 240,
		Schedule:        []ScheduleTier{{From: "09:30", To: "15:00", IntervalSeconds: 60}},
	}
	if got := explicit.Normalize().MaxCyclesPerDay; got != 240 {
		t.Fatalf("显式配了排班时配额不该被改，实际 %d", got)
	}
}
