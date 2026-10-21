package trading

import "testing"

// 配额必须跟间隔匹配。60 秒间隔配 40 轮，一个上午就把全天额度用完，
// 下午再也不动——界面上像系统死了，实际是额度耗尽。这条规则就是防这个。
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
		got := AutopilotConfig{IntervalSeconds: c.interval, MaxCyclesPerDay: 0}.Normalize().MaxCyclesPerDay
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
		t.Fatalf("旧默认值 40 应升级为与间隔匹配的额度，实际仍为 %d", legacy.MaxCyclesPerDay)
	}
	custom := AutopilotConfig{IntervalSeconds: 60, MaxCyclesPerDay: 25}.Normalize()
	if custom.MaxCyclesPerDay != 25 {
		t.Fatalf("用户显式设置的 25 轮必须保留，实际 %d", custom.MaxCyclesPerDay)
	}
}

// 默认配置自身也要自洽：不该出现"默认值跑不满一个交易日"的情况。
func TestDefaultConfigCoversTradingDay(t *testing.T) {
	defaults := DefaultAutopilotConfig()
	coverage := defaults.MaxCyclesPerDay * defaults.IntervalSeconds
	if coverage < tradingMinutesPerDay*60 {
		t.Fatalf("默认配额只能覆盖 %d 秒，不足一个交易日（%d 秒）",
			coverage, tradingMinutesPerDay*60)
	}
}
