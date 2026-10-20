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
	MaxCyclesPerDay int       `json:"max_cycles_per_day"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// DefaultAutopilotConfig 返回安全的默认值：关闭、需人工确认、3 分钟一轮。
func DefaultAutopilotConfig() AutopilotConfig {
	return AutopilotConfig{
		AccountID:           DefaultAccountID,
		Enabled:             false,
		AutoExecute:         false,
		IntervalSeconds:     180,
		TraderProfile:       "balanced",
		Horizon:             "1-3 个交易日",
		MaxDailyLossPercent: 3,
		BuyCutoff:           "14:50",
		MaxCyclesPerDay:     40,
	}
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
	if c.MaxCyclesPerDay <= 0 || c.MaxCyclesPerDay > 240 {
		c.MaxCyclesPerDay = defaults.MaxCyclesPerDay
	}
	if strings.TrimSpace(c.AccountID) == "" {
		c.AccountID = defaults.AccountID
	}
	return c
}

// parseClock 解析 "HH:MM"。返回当天的分钟数。
func parseClock(value string) (int, error) {
	value = strings.TrimSpace(value)
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, fmt.Errorf("时间格式应为 HH:MM：%s", value)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("时间超出范围：%s", value)
	}
	return hour*60 + minute, nil
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
