package trading

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// 健康状态的三个档位。无人值守时只有这三档有意义：
// healthy = 什么都不用做；warning = 需要人看一眼但系统还在正常工作；
// critical = 系统已经不能正常工作了，必须立刻处理。
const (
	HealthHealthy  = "healthy"
	HealthWarning  = "warning"
	HealthCritical = "critical"
)

// HealthIssue 是一条健康问题。Code 供程序判断，Message 供人阅读。
type HealthIssue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Heartbeat 是自动循环的心跳。它回答"循环还活着吗、最近一次成功是什么时候"。
//
// 只看进程存活是不够的：进程活着但每一轮都失败，账户其实已经停摆，
// 而端口还在响应、页面还能打开，从外面完全看不出来。
type Heartbeat struct {
	Running       bool       `json:"running"`
	SchedulerAt   *time.Time `json:"scheduler_at,omitempty"`
	LastTickAt    *time.Time `json:"last_tick_at,omitempty"`
	LastCycleAt   *time.Time `json:"last_cycle_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// LastError 是最近一次失败的原文。健康页要能看到"到底为什么失败"，
	// 而不是只有一个"失败"的结论。
	LastError     string `json:"last_error,omitempty"`
	FailureStreak int    `json:"failure_streak"`
	// Degraded 表示因连续失败被自动降级（暂停买入）。降级是系统自己做的决定，
	// 必须显式暴露出来，否则人会把"今天没买"误读成"模型没找到机会"。
	Degraded bool `json:"degraded"`
}

// Health 是一次健康自检的结果。它只读库、不拉行情：
// 健康检查本身必须是廉价的，否则它自己会成为新的故障点。
type Health struct {
	Status        string       `json:"status"`
	CheckedAt     time.Time    `json:"checked_at"`
	UptimeSeconds float64      `json:"uptime_seconds"`
	Channel       string       `json:"channel"`
	AccountID     string       `json:"account_id"`
	Autopilot     Heartbeat    `json:"autopilot"`
	Positions     int          `json:"positions"`
	OpenOrders    int          `json:"open_orders"`
	ReservedCash  float64      `json:"reserved_cash"`
	Ledger        LedgerHealth `json:"ledger"`
	Alerts        AlertHealth  `json:"alerts"`
	Breaker       string       `json:"breaker,omitempty"`
	// Drawdown 是组合级回撤状态。它不属于"今日熔断"：日亏次日归零，
	// 回撤会一直压着买入，直到净值回到高点附近或人工解除。
	Drawdown RiskState     `json:"drawdown"`
	Issues   []HealthIssue `json:"issues"`
}

// AlertHealth 是告警通道自身的状态。
//
// 它回答一个容易被忽略的问题：告警系统还通不通。
// 一个从不报错的告警系统，和一个从没收到过告警的系统，外表完全一样；
// 不把投递结果算进健康检查，就只能等到真出事那天才发现通知一直没发出去。
type AlertHealth struct {
	// Configured 表示是否配置了至少一个出口（webhook / 文件）。
	Configured bool     `json:"configured"`
	Channels   []string `json:"channels"`
	// Sent24h / Failed24h 是最近 24 小时的投递结果。
	Sent24h   int   `json:"sent_24h"`
	Failed24h int   `json:"failed_24h"`
	Dropped   int64 `json:"dropped"`
}

// LedgerHealth 是账本一致性摘要。任何一项非零都意味着"钱或股数对不上"，
// 这类问题不会自愈，只会在下一次下注时放大。
type LedgerHealth struct {
	OK                    bool    `json:"ok"`
	Drift                 float64 `json:"drift"`
	ReservedQuantityDrift int     `json:"reserved_quantity_drift"`
	OrphanOrders          int     `json:"orphan_orders"`
}

// 健康判定阈值。都写成常量，是为了让"为什么判成 critical"可以被追溯，
// 而不是散落在几个 if 里的魔法数字。
const (
	// healthTickStaleAfter 超过这个时间没有 tick，说明调度线程已经卡死或退出。
	healthTickStaleAfter = 10 * time.Minute
	// healthFailureStreakCritical 连续失败到这个次数就判 critical：
	// 单次失败可能是行情抖动，连续三次意味着不是抖动。
	healthFailureStreakCritical = 3
	// healthOpenOrderWarning 挂单积压到这个数量提示一次：
	// 挂单不会自己消失，堆积往往意味着价格长期不达或撮合通道有问题。
	healthOpenOrderWarning = 10
	// healthDriftTolerance 是账本漂移的容差，一分钱以内视为舍入噪声。
	healthDriftTolerance = 0.005
	// healthStartupGrace 是启动宽限期：这段时间内不报"循环没有心跳"。
	// tick 间隔 15 秒，进程刚起来必然还没跑过一轮。不设宽限期的话，
	// 每次部署都会收到一条假告警，而假告警多了真告警就没人看了。
	healthStartupGrace = 90 * time.Second
)

// Health 汇总当前运行状态。
//
// 它刻意不调用 AccountView：健康检查可能被监控每秒打一次，
// 每次都去拉一遍持仓行情既浪费额度，也会在行情服务故障时连带把健康检查拖成超时。
func (s *Service) Health(ctx context.Context, accountID string, beat Heartbeat) (Health, error) {
	accountID = s.accountID(accountID)
	now := time.Now()
	health := Health{
		CheckedAt: now,
		Channel:   s.BrokerName(),
		AccountID: accountID,
		Autopilot: beat,
		Issues:    []HealthIssue{},
	}
	if !s.startedAt.IsZero() {
		health.UptimeSeconds = math.Round(now.Sub(s.startedAt).Seconds()*10) / 10
	}

	positions, err := s.store.ListPositions(ctx, accountID)
	if err != nil {
		return health, err
	}
	health.Positions = len(positions)

	account, err := s.store.EnsureAccount(ctx, accountID, DefaultInitialCash)
	if err != nil {
		return health, err
	}
	health.ReservedCash = account.ReservedCash

	openOrders, err := s.store.ListPendingOrders(ctx, accountID)
	if err == nil {
		health.OpenOrders = len(openOrders)
	}

	// 只读对账：健康检查绝不能顺手改账。修复是自愈循环的职责，
	// 两者混在一起时，人就再也分不清"账本来是对的"和"检查时被改对了"。
	if report, reconcileErr := s.store.ReconcileReservations(ctx, accountID, false); reconcileErr == nil {
		drift := math.Abs(report.Drift)
		quantityDrift := 0
		for _, detail := range report.ReservedQuantityDrift {
			if strings.TrimSpace(detail) != "" {
				quantityDrift++
			}
		}
		health.Ledger = LedgerHealth{
			OK:                    drift <= healthDriftTolerance && quantityDrift == 0 && len(report.OrphanOrders) == 0,
			Drift:                 round2(report.Drift),
			ReservedQuantityDrift: quantityDrift,
			OrphanOrders:          len(report.OrphanOrders),
		}
	} else {
		health.Issues = append(health.Issues, HealthIssue{
			Code:     "ledger.check_failed",
			Severity: EventSeverityCritical,
			Message:  fmt.Sprintf("账本对账执行失败：%v", reconcileErr),
		})
	}

	if state, stateErr := s.store.GetDayState(ctx, accountID, TradingDay(now)); stateErr == nil && state.Tripped() {
		health.Breaker = state.Breaker
	}
	if risk, riskErr := s.store.GetRiskState(ctx, accountID); riskErr == nil {
		health.Drawdown = risk
	} else {
		health.Issues = append(health.Issues, HealthIssue{
			Code:     "drawdown.read_failed",
			Severity: EventSeverityWarning,
			Message:  fmt.Sprintf("读取回撤状态失败：%v", riskErr),
		})
	}

	s.collectAlertHealth(ctx, &health, accountID, now)
	s.assessHealth(&health, now)
	return health, nil
}

// collectAlertHealth 汇总告警通道状态。
//
// 计数只查库、不读内存：进程重启后内存计数归零，但"昨天发了 400 条全失败"
// 这个事实仍然成立，健康检查不该因为重启就把它忘掉。
func (s *Service) collectAlertHealth(ctx context.Context, health *Health, accountID string, now time.Time) {
	manager := s.Alerts()
	if manager == nil {
		return
	}
	channels := manager.Channels()
	health.Alerts.Channels = channels
	health.Alerts.Configured = len(channels) > 0
	health.Alerts.Dropped = manager.Stats()["dropped"]

	since := now.Add(-24 * time.Hour)
	if sent, err := s.store.CountAlertsSince(ctx, accountID, AlertStatusSent, since); err == nil {
		health.Alerts.Sent24h = sent
	}
	if failed, err := s.store.CountAlertsSince(ctx, accountID, AlertStatusFailed, since); err == nil {
		health.Alerts.Failed24h = failed
	}
}

// assessHealth 把采集到的数字翻译成结论。判定顺序是"先致命后警告"：
// 只要有一项 critical，整体就是 critical，不会因为其他项健康而被稀释。
func (s *Service) assessHealth(health *Health, now time.Time) {
	add := func(code, severity, format string, args ...any) {
		health.Issues = append(health.Issues, HealthIssue{
			Code:     code,
			Severity: severity,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	if !health.Ledger.OK {
		add("ledger.drift", EventSeverityCritical,
			"账本对不上：资金冻结漂移 %.2f 元、股数漂移 %d 个标的、孤儿挂单 %d 笔",
			health.Ledger.Drift, health.Ledger.ReservedQuantityDrift, health.Ledger.OrphanOrders)
	}

	if health.Autopilot.FailureStreak >= healthFailureStreakCritical {
		add("autopilot.failure_streak", EventSeverityCritical,
			"自动循环连续失败 %d 轮，最近一次：%s",
			health.Autopilot.FailureStreak, fallback(health.Autopilot.LastError, "无错误原文"))
	}

	if health.Autopilot.Running {
		if health.Autopilot.LastTickAt == nil {
			if health.UptimeSeconds > healthStartupGrace.Seconds() {
				add("autopilot.no_tick", EventSeverityWarning, "自动循环已启动但还没有完成第一次检查")
			}
		} else if idle := now.Sub(*health.Autopilot.LastTickAt); idle > healthTickStaleAfter {
			add("autopilot.stale", EventSeverityCritical,
				"自动循环已 %s 没有心跳，调度可能已停止", humanDuration(idle))
		}
	}

	if health.Autopilot.Degraded {
		add("autopilot.degraded", EventSeverityWarning, "已因连续失败自动降级：暂停买入，保护性卖出仍在执行")
	}

	if health.OpenOrders >= healthOpenOrderWarning {
		add("orders.backlog", EventSeverityWarning,
			"有 %d 笔挂单未成交，资金与股数被持续冻结", health.OpenOrders)
	}

	if health.Breaker != "" {
		add("breaker."+health.Breaker, EventSeverityWarning, "当日已熔断（%s），买入被禁止", breakerLabel(health.Breaker))
	}

	// 回撤熔断：状态是"账户在保护中"，不是"系统故障"，所以只报警告。
	// 但它必须出现在健康条里——否则账户悄悄停止买入三天，人只会以为模型没找到机会。
	if health.Drawdown.Paused() {
		add("drawdown."+strings.TrimPrefix(health.Drawdown.Level, "drawdown."), EventSeverityWarning,
			"净值回撤 %.2f%%（高点 %.2f → 现值 %.2f）已触发%s，买入暂停%s",
			health.Drawdown.DrawdownPercent, health.Drawdown.PeakEquity, health.Drawdown.Equity,
			drawdownLevelLabel(health.Drawdown.Level), drawdownReleaseHint(health.Drawdown))
	}

	// 告警投递全失败：系统可能一切正常，但"出事时没人会知道"。
	// 这不算 critical——交易本身没坏；但它必须出现在 warning 里，
	// 否则这套无人值守在真正需要它的那一天会安静地失效。
	if health.Alerts.Failed24h > 0 && health.Alerts.Sent24h == 0 {
		add("alerts.delivery_failed", EventSeverityWarning,
			"近 24 小时告警投递失败 %d 次、成功 0 次，通知通道可能已失效", health.Alerts.Failed24h)
	}
	if health.Alerts.Dropped > 0 {
		add("alerts.dropped", EventSeverityWarning,
			"有 %d 条告警因队列拥塞被丢弃，投递速度跟不上事件产生速度", health.Alerts.Dropped)
	}

	health.Status = HealthHealthy
	for _, issue := range health.Issues {
		if issue.Severity == EventSeverityCritical {
			health.Status = HealthCritical
			return
		}
		if issue.Severity == EventSeverityWarning {
			health.Status = HealthWarning
		}
	}
}

func breakerLabel(code string) string {
	switch code {
	case BreakerDailyLoss:
		return "当日亏损熔断"
	case BreakerStopStreak:
		return "连续止损熔断"
	default:
		return code
	}
}

// drawdownReleaseHint 说明这一档怎么才能解除。两条路径完全不同：
// 警戒线会随净值回升自动解除，停止线必须人工确认——不写清楚，
// 人就会一直等一个不会自己消失的保护。
func drawdownReleaseHint(state RiskState) string {
	if state.Level == DrawdownLevelStop {
		return "，需人工确认后解除"
	}
	return fmt.Sprintf("，回撤回到 %.2f%% 以内自动解除", state.Policy.ReleasePercent)
}

func fallback(value, other string) string {
	if strings.TrimSpace(value) == "" {
		return other
	}
	return value
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}
