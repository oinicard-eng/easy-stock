package trading

import (
	"fmt"
	"strings"
	"time"
)

// CycleStatus 是一轮自动循环的终态。它刻意分得比"成功/失败"更细：
// 跳过（没到交易时段）与拦截（熔断、门控）都不是失败，
// 把它们算进失败率，会让人以为系统在出故障。
type CycleStatus string

const (
	// CycleStatusRunning 表示这一轮正在执行。进程崩溃后它会被恢复流程改成 aborted。
	CycleStatusRunning CycleStatus = "running"
	// CycleStatusDone 表示这一轮完整跑完（可能没有产生订单，那也是一种正常结论）。
	CycleStatusDone CycleStatus = "done"
	// CycleStatusSkipped 表示不满足开轮条件（非交易时段、开关关闭、当日轮次用尽）。
	CycleStatusSkipped CycleStatus = "skipped"
	// CycleStatusBlocked 表示被闸门拦住（kill switch、当日熔断），本轮不产生新仓。
	CycleStatusBlocked CycleStatus = "blocked"
	// CycleStatusFailed 表示执行出错（行情源失败、模型失败、落库失败）。
	CycleStatusFailed CycleStatus = "failed"
	// CycleStatusAborted 表示进程在轮次执行中退出。它不是失败，是"未完成"。
	CycleStatusAborted CycleStatus = "aborted"
)

// CycleTrigger 说明这一轮是谁发起的。调度与人工触发的复盘价值完全不同：
// 人工触发往往发生在人觉得"该做点什么"的时候，本身就是一条情绪记录。
type CycleTrigger string

const (
	CycleTriggerSchedule CycleTrigger = "schedule"
	CycleTriggerEvent    CycleTrigger = "event"
	CycleTriggerManual   CycleTrigger = "manual"
	// CycleTriggerResume 表示进程重启后补跑的那一轮。盘前补跑也用它。
	CycleTriggerResume CycleTrigger = "resume"
)

// CycleGate 是一轮循环中某道闸门的状态。
// 它存在的理由：一轮被跳过，界面必须能一眼看出是"没开盘"还是"已经熔断"。
type CycleGate struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Open   bool   `json:"open"`
	Detail string `json:"detail,omitempty"`
}

// 闸门代码。与裁决代码一样，一旦发布不再改名。
const (
	GateAutopilotEnabled = "autopilot.enabled"
	GateSession          = "session"
	GateKillSwitch       = "gate.kill_switch"
	GateBreaker          = "gate.breaker"
	GateBuyWindow        = "gate.session_window"
	GateCycleQuota       = "cycle.quota"
	// GateProtective 表示本轮跑过持仓防守线。它总是"开"的：
	// 防守线不受门控限制，记录它是为了让日志能看出"这轮检查过、没触发"。
	GateProtective = "guard.positions"
	GateReview     = "review.daily"
	// GateDrawdown 是组合级回撤闸门。它与 gate.breaker 并列而不是合并：
	// 日亏熔断看当天，回撤看历史高点，两者同时存在时才拦得住"慢刀子"。
	GateDrawdown = "drawdown.level"
)

// Cycle 是一轮自动交易的完整记录。它是「交易员日志」的主键：
// 每个 cycle 下挂着一个计划、一条裁决、若干订单。
type Cycle struct {
	ID        string       `json:"id"`
	AccountID string       `json:"account_id"`
	Trigger   CycleTrigger `json:"trigger"`
	Status    CycleStatus  `json:"status"`
	// Reason 说明这一轮为什么跳过/被拦/失败。成功时为空。
	Reason     string     `json:"reason,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	PlanID     string     `json:"plan_id,omitempty"`
	DecisionID string     `json:"decision_id,omitempty"`
	// EquityBefore / EquityAfter 是本轮前后的总资产，用来把盈亏归到具体某一轮。
	EquityBefore float64 `json:"equity_before"`
	EquityAfter  float64 `json:"equity_after"`
	// DayStartEquity 是当日基准权益。日亏熔断按它计算，所以必须随每轮记下来，
	// 否则事后无法复算"当时到底亏了多少才触发的"。
	DayStartEquity   float64 `json:"day_start_equity"`
	DayChangePercent float64 `json:"day_change_percent"`
	// DrawdownPercent 是本轮观察到的组合回撤（相对净值高点）。
	// 它与 DayChangePercent 并列记录：一个是"今天"，一个是"从最好时起"。
	DrawdownPercent float64 `json:"drawdown_percent,omitempty"`
	// Orders 是裁决通过并落库的订单数；Filled 是本轮结束时已成交的笔数。
	Orders  int `json:"orders"`
	Filled  int `json:"filled"`
	Blocked int `json:"blocked"`
	// Matched 是本轮撮合掉的既有挂单数（事件驱动部分）。
	Matched int `json:"matched"`
	// Breaker 是本轮结束后当日熔断的状态，空串表示未触发。
	Breaker string      `json:"breaker,omitempty"`
	Gates   []CycleGate `json:"gates,omitempty"`
	Notes   []string    `json:"notes,omitempty"`
}

// Running 表示这一轮还没结束。
func (c Cycle) Running() bool { return c.Status == CycleStatusRunning }

// AutopilotConfig 是自动交易循环的配置。它必须落库：
// 进程重启后仍然要记得"我本来是开着的"，否则一次重启就静默变成手动模式。
type AutopilotConfig struct {
	AccountID string `json:"account_id"`
	// Enabled 打开后循环才会按间隔发起新一轮。默认关闭：
	// 自动交易不该是部署完成就自动开始的行为。
	Enabled bool `json:"enabled"`
	// AutoExecute 决定生成的计划是否立即撮合。
	// 关闭时计划停在 draft，等于"自动思考、人工扣扳机"，用于上线前观察。
	AutoExecute     bool   `json:"auto_execute"`
	IntervalSeconds int    `json:"interval_seconds"`
	TraderProfile   string `json:"trader_profile"`
	Horizon         string `json:"horizon"`
	// MaxDailyLossPercent 是日亏熔断阈值（相对当日基准权益）。
	MaxDailyLossPercent float64 `json:"max_daily_loss_percent"`
	// BuyCutoff 是尾盘禁开仓时点，"14:50" 形式。
	BuyCutoff string `json:"buy_cutoff"`
	// MaxCyclesPerDay 是当天最多发起多少轮。它是防呆而不是风控：
	// 没有它，一个写错的间隔会让循环变成刷单机，一晚上把手续费刷掉。
	MaxCyclesPerDay int `json:"max_cycles_per_day"`
	// Schedule 按时段覆盖 IntervalSeconds；为空表示全天统一用 IntervalSeconds。
	// 为什么需要分时段：A 股 T+1 意味着当天买入当天不能卖，
	// 每分钟重新决策一次在物理上拿不到任何额外信息——买了也卖不掉。
	// 而每一轮都是一次完整的多步模型调用（实测约 0.12 元），
	// 240 轮就是 17 元/天，一年吃掉本金的 4%。
	Schedule  []ScheduleTier `json:"schedule,omitempty"`
	UpdatedAt time.Time      `json:"updated_at,omitempty"`
}

// ScheduleTier 描述一个时段内的调度间隔。区间左闭右开，按交易时钟（北京时间）判断。
type ScheduleTier struct {
	From            string `json:"from"`             // "09:30"
	To              string `json:"to"`               // "10:00"
	IntervalSeconds int    `json:"interval_seconds"` // 该时段内每多少秒一轮
}

// IntervalAt 返回 now 所处的调度间隔。
// 没有任何时段命中时回落到 IntervalSeconds——排班表是加速器，不是唯一来源，
// 一张写错的表不能把循环停掉。
func (c AutopilotConfig) IntervalAt(now time.Time) int {
	clock := clockMinutes(now)
	for _, tier := range c.Schedule {
		from, errFrom := parseClock(tier.From)
		to, errTo := parseClock(tier.To)
		if errFrom != nil || errTo != nil || tier.IntervalSeconds <= 0 || to <= from {
			// 坏行只跳过这一行：它不该拖累其它时段，更不该让整轮调度失效。
			continue
		}
		if clock >= from && clock < to {
			return tier.IntervalSeconds
		}
	}
	return c.IntervalSeconds
}

// DefaultSchedule 返回默认排班，按"这个时刻能得到多少新信息"分配预算，而非平均分配：
//
//	09:30-10:00 900s  开盘半小时方向最乱，看两轮确认趋势，不急着建仓
//	10:00-11:30 900s  趋势基本成型，每 15 分钟看一次
//	13:00-14:20 900s  午后延续或反转，每 15 分钟看一次
//	14:20-15:00 600s  尾盘是最优建仓窗口：当日信息最全、隔夜暴露最短，加密到 10 分钟
//
// 合计约 17 轮/天。卖出与风控不在这里：止损、止盈、时间止损、失效条件、
// 回撤熔断全部由代码每分钟执行，一次模型调用都不需要。
func DefaultSchedule() []ScheduleTier {
	return []ScheduleTier{
		{From: "09:30", To: "10:00", IntervalSeconds: 900},
		{From: "10:00", To: "11:30", IntervalSeconds: 900},
		{From: "13:00", To: "14:20", IntervalSeconds: 900},
		{From: "14:20", To: "15:00", IntervalSeconds: 600},
	}
}

// DefaultAutopilotConfig 返回安全的默认值：关闭、需人工确认、按排班调度。
func DefaultAutopilotConfig() AutopilotConfig {
	return AutopilotConfig{
		AccountID:       DefaultAccountID,
		Enabled:         false,
		AutoExecute:     false,
		IntervalSeconds: 900,
		TraderProfile:   "balanced",
		Horizon:         "1-3 个交易日",
		// MaxDailyLossPercent 是日亏熔断阈值（相对当日基准权益）。
		MaxDailyLossPercent: 3,
		BuyCutoff:           "14:50",
		Schedule:            DefaultSchedule(),
		// 配额覆盖"排班轮次 + 事件触发余量"。排班 17 轮，
		// 留一半给异动触发，合计 26；事件通道另有自己的门槛与冷却。
		MaxCyclesPerDay: cyclesForSchedule(DefaultSchedule(), 900) + 10,
	}
}

// legacyMaxCyclesPerDay 是旧版固定的每日轮次上限。
// 保留这个常量只为了识别"从没改过配额的账户"，把它升级到与间隔匹配的值。
const legacyMaxCyclesPerDay = 40

// tradingMinutesPerDay 是 A 股连续竞价的总时长（9:30-11:30 + 13:00-15:00）。
const tradingMinutesPerDay = 240

// cyclesForInterval 按交易时长反推当天能跑多少轮。
// 上限 240 与 Normalize 的校验保持一致，防止把间隔设成 30 秒后额度失控。
func cyclesForInterval(intervalSeconds int) int {
	if intervalSeconds <= 0 {
		intervalSeconds = DefaultAutopilotConfig().IntervalSeconds
	}
	cycles := tradingMinutesPerDay * 60 / intervalSeconds
	if cycles < 1 {
		cycles = 1
	}
	if cycles > 240 {
		cycles = 240
	}
	return cycles
}

// cyclesForSchedule 按排班表算出一天的轮次总量。
// 排班为空或全部无效时回落到单一间隔口径，避免算出 0 轮把循环锁死。
func cyclesForSchedule(schedule []ScheduleTier, fallbackInterval int) int {
	total := 0
	for _, tier := range schedule {
		from, errFrom := parseClock(tier.From)
		to, errTo := parseClock(tier.To)
		if errFrom != nil || errTo != nil || tier.IntervalSeconds <= 0 || to <= from {
			continue
		}
		total += (to - from) * 60 / tier.IntervalSeconds
	}
	if total <= 0 {
		return cyclesForInterval(fallbackInterval)
	}
	if total > 240 {
		return 240
	}
	return total
}

// Normalize 把越界或缺失的配置改回可用值。非法值一律回落到默认，
// 而不是报错拒绝——否则一个手误的 interval 会让整条自动链路停摆。
func (c AutopilotConfig) Normalize() AutopilotConfig {
	defaults := DefaultAutopilotConfig()
	if c.IntervalSeconds < 30 {
		c.IntervalSeconds = defaults.IntervalSeconds
	}
	if c.IntervalSeconds > 3600 {
		c.IntervalSeconds = 3600
	}
	switch strings.ToLower(strings.TrimSpace(c.TraderProfile)) {
	case "aggressive", "激进":
		c.TraderProfile = "aggressive"
	case "steady", "稳健":
		c.TraderProfile = "steady"
	default:
		c.TraderProfile = defaults.TraderProfile
	}
	if strings.TrimSpace(c.Horizon) == "" {
		c.Horizon = defaults.Horizon
	}
	if c.MaxDailyLossPercent <= 0 || c.MaxDailyLossPercent > 20 {
		c.MaxDailyLossPercent = defaults.MaxDailyLossPercent
	}
	if _, err := parseClock(c.BuyCutoff); err != nil {
		c.BuyCutoff = defaults.BuyCutoff
	}
	// 排班表逐行校验，坏行丢弃而不是整表作废：
	// 一个手误的 "9:3O" 不该让另外三行一起失效。
	// nil 表示"字段缺失"（老配置），补默认排班；
	// 空数组表示用户明确选择"全天统一间隔"，尊重它。
	scheduleWasMissing := c.Schedule == nil
	if scheduleWasMissing {
		c.Schedule = defaults.Schedule
	} else {
		valid := make([]ScheduleTier, 0, len(c.Schedule))
		for _, tier := range c.Schedule {
			from, errFrom := parseClock(tier.From)
			to, errTo := parseClock(tier.To)
			if errFrom != nil || errTo != nil || to <= from {
				continue
			}
			if tier.IntervalSeconds < 30 {
				tier.IntervalSeconds = 30
			}
			if tier.IntervalSeconds > 3600 {
				tier.IntervalSeconds = 3600
			}
			valid = append(valid, tier)
		}
		c.Schedule = valid
	}
	// 配额必须与调度匹配，否则会出现"跑到一半就停摆"：
	// 60 秒间隔配 40 轮，一个上午就把全天额度用完，下午再也不会动，
	// 界面上看起来像系统死了，实际是配额用尽。
	// 有排班时按排班轮次 + 事件触发余量算，没有排班时按单一间隔覆盖全天算。
	quotaFor := func() int {
		if len(c.Schedule) > 0 {
			return cyclesForSchedule(c.Schedule, c.IntervalSeconds) + 10
		}
		return cyclesForInterval(c.IntervalSeconds)
	}
	switch {
	case c.MaxCyclesPerDay <= 0 || c.MaxCyclesPerDay > 240:
		c.MaxCyclesPerDay = quotaFor()
	case c.MaxCyclesPerDay == legacyMaxCyclesPerDay:
		// 40 是旧版硬编码默认值，不是用户的选择：老配置存的就是它，
		// 不跟着升级就会继续在盘中停摆。显式改过的其它值一律尊重。
		c.MaxCyclesPerDay = quotaFor()
	case scheduleWasMissing && c.MaxCyclesPerDay == cyclesForInterval(c.IntervalSeconds):
		// 配额恰好等于"按单一间隔覆盖全天"的自动推导值，说明它也是自动算出来的。
		// 排班把一天压到十几轮，这个配额继续留着会让界面显示"0/240"，
		// 用户会以为还有很多额度——额度本身不超支，但它会掩盖真实的调度节奏。
		// 只有恰好相等才重算：手改过的值（比如 12）不会被这一步覆盖。
		c.MaxCyclesPerDay = quotaFor()
	}
	if c.MaxCyclesPerDay > 240 {
		c.MaxCyclesPerDay = 240
	}
	if strings.TrimSpace(c.AccountID) == "" {
		c.AccountID = defaults.AccountID
	}
	return c
}

// parseClock 解析 "HH:MM"。返回当天的分钟数。
//
// 用 time.Parse 而不是 fmt.Sscanf：Sscanf 的 %d 只读到数字就停，
// 不管后面还剩什么，"9:3O"（字母 O）会被解析成 9:03 并当作合法值。
// 配置里一个错字被静默吃成另一个时刻，比直接报错难查得多。
func parseClock(value string) (int, error) {
	value = strings.TrimSpace(value)
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("时间格式应为 HH:MM：%s", value)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

// DayState 是当日账户状态：基准权益、连续亏损计数、熔断事实。
//
// 为什么按「交易日」而不是按自然日：A 股有夜盘外的隔夜跳空，
// 用自然日会让周五收盘到周一开盘的跳空算进周一的日亏，熔断在开盘第一分钟就误触发。
type DayState struct {
	AccountID  string `json:"account_id"`
	TradingDay string `json:"trading_day"`
	// StartEquity 是当日基准权益。当日第一次触达账户时记录，之后不再改。
	StartEquity float64 `json:"start_equity"`
	// StopStreak 是连续止损次数。P3 接入止损链路后才有数据，当前恒为 0。
	StopStreak int `json:"stop_streak"`
	// Breaker 非空表示当日已熔断，取值见 BreakerDailyLoss / BreakerStopStreak。
	Breaker       string     `json:"breaker,omitempty"`
	BreakerDetail string     `json:"breaker_detail,omitempty"`
	BreakerAt     *time.Time `json:"breaker_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const (
	BreakerDailyLoss  = "daily_loss"
	BreakerStopStreak = "stop_streak"
)

// Tripped 表示当日已熔断。熔断是当日的既成事实，不随手动解除门控而消失。
func (d DayState) Tripped() bool { return strings.TrimSpace(d.Breaker) != "" }

// BreakerView 是给界面看的熔断器状态。
//
// Armed 字段是刻意的诚实标记：连续止损熔断依赖 P3 的止损成交记录，
// 在链路接通之前它是"已安装未装弹"，不能显示成"已保护"。
type BreakerView struct {
	Code    string `json:"code"`
	Label   string `json:"label"`
	Armed   bool   `json:"armed"`
	Tripped bool   `json:"tripped"`
	Detail  string `json:"detail,omitempty"`
}

// AutopilotStatus 是循环的整体状态，一次请求给全界面需要的所有信息。
type AutopilotStatus struct {
	Config AutopilotConfig `json:"config"`
	// Running 表示此刻正有一轮在执行。
	Running   bool          `json:"running"`
	LastCycle *Cycle        `json:"last_cycle,omitempty"`
	Today     DayState      `json:"today"`
	Breakers  []BreakerView `json:"breakers"`
	// Risk 是组合级风险状态（净值高点与回撤）。它跨日存在，
	// 不能由 Today 推导，所以单独给一份。
	Risk RiskState `json:"risk"`
	// NextRunAt 是下一次调度时间；关闭时为空。
	NextRunAt   *time.Time `json:"next_run_at,omitempty"`
	CyclesToday int        `json:"cycles_today"`
}

// TradingDay 返回给定时刻所属的交易日（按北京时间）。
// 周末与节假日没有专门处理：节假日的行情时间戳会让循环自然空转，
// 而在这里硬编节假日表，每年都要维护一次，且错一天就会全盘停摆。
func TradingDay(now time.Time) string {
	cst := now.In(time.FixedZone("CST", 8*3600))
	return cst.Format("2006-01-02")
}

// clockMinutes 返回北京时间当天的分钟数。
func clockMinutes(now time.Time) int {
	cst := now.In(time.FixedZone("CST", 8*3600))
	return cst.Hour()*60 + cst.Minute()
}
