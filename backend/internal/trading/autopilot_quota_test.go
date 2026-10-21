package trading

import "testing"

// 配额必须跟间隔匹配。60 秒间隔配 40 轮，一个上午就把全天额度用完，
// 下午再也不动——界面上像系统死了，实际是额度耗尽。这条规则就是防这个。
//
// 这里显式给空排班，测的是"全天统一间隔"这条分支：
// 有排班时额度按排班轮次算，那是另一条路径（见 schedule_test.go）。
func TestCyclesQuotaScalesWithInterval(t *testing.T) {
	cases := []struct {
		interval int
		want     int
	}{
		{60, 240}, // 240 分钟 ÷ 1 分钟 = 240（正好顶到上限）
		{180, 80}, // 240 ÷ 3 = 80
		{240, 60}, // 240 ÷ 4 = 60
		{30, 240}, // 240 ÷ 0.5 = 480，被上限截到 240
		{3600, 4}, // 240 ÷ 60 = 4
	}
	for _, c := range cases {
		got := AutopilotConfig{IntervalSeconds: c.interval, Schedule: []ScheduleTier{}}.Normalize().MaxCyclesPerDay
		if got != c.want {
			t.Fatalf("间隔 %d 秒时每日额度应为 %d，实际 %d", c.interval, c.want, got)
		}
	}
}

// 旧版硬编码的 40 是"从没改过"的意思，要跟着升级；
// 用户真正改过的值必须尊重，不能覆盖。
func TestCyclesQuotaUpgradesLegacyDefaultOnly(t *testing.T) {
	legacy := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: 40}.Normalize()
	if legacy.MaxCyclesPerDay == legacyMaxCyclesPerDay {
		t.Fatalf("旧默认值 40 应升级为与调度匹配的额度，实际仍为 %d", legacy.MaxCyclesPerDay)
	}
	custom := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: 25}.Normalize()
	if custom.MaxCyclesPerDay != 25 {
		t.Fatalf("用户显式设置的 25 轮必须保留，实际 %d", custom.MaxCyclesPerDay)
	}
}

// 默认排班要覆盖整个可开仓时段：从 9:30 到买入截止 14:50，
// 不能出现"某个时段没有任何一轮"的空洞——那段时间行情再剧烈系统也是瞎的。
func TestDefaultScheduleCoversBuyWindow(t *testing.T) {
	defaults := DefaultAutopilotConfig()
	cutoff, err := parseClock(defaults.BuyCutoff)
	if err != nil {
		t.Fatalf("买入截止时间无法解析：%v", err)
	}
	// 直接判断"是否落在某一行排班里"，不能用 IntervalAt 的返回值对比：
	// 默认 IntervalSeconds 与排班间隔同为 900，回落值和命中值无法区分。
	covered := func(minute int) bool {
		for _, tier := range defaults.Schedule {
			from, errFrom := parseClock(tier.From)
			to, errTo := parseClock(tier.To)
			if errFrom != nil || errTo != nil {
				continue
			}
			if minute >= from && minute < to {
				return true
			}
		}
		return false
	}
	for _, start := range []int{9*60 + 30, 13 * 60} {
		for minute := start; minute < cutoff; minute++ {
			if minute >= 11*60+30 && minute < 13*60 {
				continue // 午休
			}
			if !covered(minute) {
				t.Fatalf("%02d:%02d 没有任何排班覆盖，该时段不会发起任何一轮", minute/60, minute%60)
			}
		}
	}
}

// 配额必须留出事件触发的余量：排班只算定时轮，
// 异动触发是额外的，两者共用同一个额度池。
func TestDefaultQuotaLeavesRoomForEvents(t *testing.T) {
	defaults := DefaultAutopilotConfig()
	scheduled := cyclesForSchedule(defaults.Schedule, defaults.IntervalSeconds)
	if defaults.MaxCyclesPerDay <= scheduled {
		t.Fatalf("配额 %d 不大于排班轮次 %d，行情异动时将没有额度可用",
			defaults.MaxCyclesPerDay, scheduled)
	}
	if defaults.MaxCyclesPerDay > scheduled*3 {
		t.Fatalf("配额 %d 是排班轮次 %d 的三倍以上，事件触发的成本会失控",
			defaults.MaxCyclesPerDay, scheduled)
	}
}
