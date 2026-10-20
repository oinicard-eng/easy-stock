package trading

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"easy-stock/backend/internal/trading/marketrule"
	"easy-stock/backend/internal/trading/policy"
)

// ErrCycleRunning 表示已有一轮自动循环在执行。
var ErrCycleRunning = errors.New("已有一轮自动循环正在执行")

// stopStreakLimit 是「连续止损」熔断的阈值。
// 它现在不会触发：止损成交要等 P3 把持仓防守线接上，在那之前 StopStreak 恒为 0。
// 保留这条规则而不是先删掉，是因为账本与循环的数据结构一旦定下，
// 事后补一个"连续亏损"字段要重跑全部历史记录。
const stopStreakLimit = 3

// Autopilot 是自动交易循环：按固定节拍发起一轮「认知 → 选股 → 裁决 → 执行」，
// 并在每轮之前跑一遍当日熔断检查。
//
// 它刻意只做编排，不碰规则：
//   - 买不买、买多少，由 policy 与裁决层决定；
//   - 能不能开新仓，由这里的闸门决定（开关、交易时段、熔断、尾盘）；
//   - 下单之后怎么成交，由 Broker 决定。
//
// 三层分开的理由是：任何一层出问题都能单独定位。混在一起时，
// "今天没买"会变成一个需要读三份日志才能回答的问题。
type Autopilot struct {
	service *Service
	store   *Store
	logger  *log.Logger
	now     func() time.Time

	mu      sync.Mutex
	running bool
	started bool
	stopCh  chan struct{}
	nextRun time.Time

	// lastCycleAt 只用于展示与限流判断，持久化的事实以 trading_cycles 为准。
	lastCycleAt time.Time

	// lastExpiry 记录上一次执行日终委托清理所用的收盘时点。
	// 它是内存值而非数据库标记：清理本身幂等，重启后重跑一次只会得到 0 笔，
	// 反而能补上进程停机期间跨过的收盘。
	lastExpiry time.Time

	// 以下字段支撑健康检查与自愈。它们都是内存值：
	// 进程重启后心跳归零是正确语义——"刚重启"本身就是要被看见的事实，
	// 把上一次进程的心跳持久化下来，反而会掩盖掉这段空白。
	lastTickAt    time.Time
	lastSuccessAt time.Time
	lastReconcile time.Time
	// watch 是事件通道的状态：触发记录、间隔、权益基线。
	// 全部放在内存里：重启后重新建立基线，比拿昨天的权益当基线安全得多。
	watch eventWatch
	// failureStreak 是连续失败轮数。成功一轮即清零：
	// 中间成功过一次，说明通道是通的，不该继续按"连续故障"处理。
	failureStreak int
	lastError     string
	// degradeUntilSuccess 记录降级后已连续成功的轮数，用于自动解除降级。
	degraded      bool
	recoverStreak int
}

// NewAutopilot 构造自动循环。它不自动启动：Start 由服务生命周期显式调用。
func NewAutopilot(service *Service, logger *log.Logger) *Autopilot {
	return &Autopilot{
		service: service,
		store:   service.Store(),
		logger:  logger,
		now:     time.Now,
	}
}

// Start 启动循环并做一次崩溃恢复。
//
// 恢复动作是必须的：上一进程在执行一轮时被杀掉，那条 running 记录会永远停在
// 那里。界面上分不清"正在跑"和"已经死了"，人就会等一个永远不会来的结果。
func (a *Autopilot) Start(ctx context.Context) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.stopCh = make(chan struct{})
	a.nextRun = a.now()
	a.mu.Unlock()

	if aborted, err := a.store.AbortRunningCycles(ctx, "进程重启，上一轮未完成"); err != nil {
		a.logf("event=autopilot_recover_failed err=%v", err)
	} else if aborted > 0 {
		a.logf("event=autopilot_recovered aborted=%d", aborted)
	}
	if plans, err := a.store.AbortRunningPlans(ctx, "进程重启，计划未完成"); err != nil {
		a.logf("event=autopilot_recover_plans_failed err=%v", err)
	} else if plans > 0 {
		a.logf("event=autopilot_recovered_plans aborted=%d", plans)
	}
	// 停机期间可能跨过了收盘：启动即补一次日终清理，否则昨日的挂单会继续锁着资金，
	// 页面上的"可用资金"从第一眼就是错的。
	a.settleExpired(ctx, DefaultAccountID, a.now())
	a.logf("event=autopilot_started")
	go a.loop()
}

// settleExpired 在跨过收盘时点后清理当日未成交委托，并释放被占用的资金与股数。
//
// 每天只做一次：cutoff 不变化就不重复查询。失败时不记录 cutoff，
// 让下一轮 tick 再试——这里失败意味着数据库不可写，重试比默默跳过更符合预期。
func (a *Autopilot) settleExpired(ctx context.Context, accountID string, now time.Time) {
	cutoff := LastSessionClose(now)
	a.mu.Lock()
	settled := !a.lastExpiry.IsZero() && !a.lastExpiry.Before(cutoff)
	a.mu.Unlock()
	if settled {
		return
	}
	if _, err := a.service.ExpireStaleOrders(ctx, accountID, now); err != nil {
		a.logf("event=autopilot_expire_failed err=%v", err)
		return
	}
	a.mu.Lock()
	a.lastExpiry = cutoff
	a.mu.Unlock()
}

// Stop 停止循环。已经在执行的那一轮会自然结束，不会被强行切断——
// 半路中断会留下冻结资金没有对应挂单的账，比多跑几秒更麻烦。
func (a *Autopilot) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return
	}
	a.started = false
	close(a.stopCh)
	a.logf("event=autopilot_stopped")
}

// loop 是调度节拍。它每 15 秒看一眼"该不该跑"，而不是直接用 3 分钟定时器：
// 交易时段可能在两次 tick 之间开启，用固定定时器会让开盘后的第一轮
// 最多晚 3 分钟才开始，而开盘后的三分钟恰恰是最贵的。
func (a *Autopilot) loop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.tick()
		}
	}
}

// 自愈相关常量。
const (
	// reconcileInterval 是自动对账的间隔。账本漂移不会自愈，
	// 但也不需要每 15 秒查一次：漂移只可能由"冻结/释放"路径产生，
	// 那些路径本身就在事务里，5 分钟足够在造成实际影响之前发现异常。
	reconcileInterval = 5 * time.Minute
	// degradeAfterFailures 是连续失败到这个次数就自动降级（暂停买入）。
	// 降级比"继续重试"安全：失败的原因可能是模型超时、行情中断，
	// 也可能是有 bug 的裁决逻辑，这三种情况下继续买入都不对。
	degradeAfterFailures = 3
	// recoverAfterSuccesses 是降级后连续成功到这个次数自动解除。
	// 要求连续成功而不要求人工确认，是因为这套系统的定位就是无人值守；
	// 但门槛要够高，避免一次偶然成功就把保护撤掉。
	recoverAfterSuccesses = 3
)

func (a *Autopilot) tick() {
	a.markTick()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	accountID := DefaultAccountID
	// 日终委托清理放在所有开关判断之前：它维护的是账本正确性，
	// 与"要不要开新仓"无关。自动驾驶关着的时候，昨天的挂单一样不能继续锁着钱。
	now := a.now()
	a.settleExpired(ctx, accountID, now)
	// 自愈对账同样放在开关之前：账本正确性与"今天要不要买"无关。
	// 自动驾驶关着的时候账记错了，一样是错的。
	a.healLedger(ctx, accountID, now)

	config, err := a.store.GetAutopilotConfig(ctx, accountID)
	if err != nil {
		a.logf("event=autopilot_config_failed err=%v", err)
		return
	}
	if !config.Enabled {
		return
	}
	// 收盘后生成当日复盘。它放在时段判断之前，因为 15:00 之后
	// IsTradingSession 已经是 false，放在后面就永远不会执行。
	a.maybeReview(ctx, accountID, now)
	// 非交易时段直接返回，且不留记录：一天 80 次"没开盘"会把日志冲掉，
	// 真正需要看的那几轮反而找不到了。
	if !marketrule.IsTradingSession(now) {
		return
	}
	if !a.due(now) {
		// 定时轮次还没到，但行情可能已经先到了。
		// 事件通道只处理"今天还没见过的、足够大的"异动，具体门槛见 event_trigger.go。
		if reason, fired := a.eventTriggered(ctx, accountID, now); fired {
			a.logf("event=autopilot_event_cycle reason=%s", reason)
			_ = a.store.AppendEvent(ctx, Event{
				AccountID: accountID,
				Severity:  EventSeverityWarning,
				Kind:      EventCycleEventTriggered,
				Message:   reason,
				CreatedAt: now.UTC(),
			})
			// 与定时轮次一样先推进下一次时间：事件触发失败不能原地重试，
			// 否则一次行情中断会变成每 15 秒一次的模型调用。
			a.setNext(now.Add(time.Duration(config.IntervalSeconds) * time.Second))
			if _, err := a.RunCycle(ctx, CycleTriggerEvent); err != nil {
				a.logf("event=autopilot_event_cycle_failed err=%v", err)
				a.recordFailure(ctx, accountID, err)
			} else {
				a.recordSuccess(ctx, accountID)
			}
		}
		return
	}
	// 先推进下一次时间再执行：一轮失败时不能原地重试，
	// 否则模型超时会变成每 15 秒一次的重试风暴。
	a.setNext(now.Add(time.Duration(config.IntervalSeconds) * time.Second))

	// 当日已熔断：只撮合既有挂单，不再调模型开新仓。
	// 熔断的意义就是"今天到此为止"，继续用模型碰运气等于没有熔断。
	if state, err := a.store.GetDayState(ctx, accountID, TradingDay(now)); err == nil && state.Tripped() {
		matched, matchErr := a.service.MatchPending(ctx, accountID)
		if matchErr != nil && !errors.Is(matchErr, ErrKillSwitch) {
			a.logf("event=autopilot_match_failed err=%v", matchErr)
		}
		_ = matched
		return
	}

	if _, err := a.RunCycle(ctx, CycleTriggerSchedule); err != nil {
		a.logf("event=autopilot_cycle_failed err=%v", err)
		a.recordFailure(ctx, accountID, err)
	} else {
		a.recordSuccess(ctx, accountID)
	}
}

// markTick 记录本轮开始时间。它只做一件事：让外面能看出"调度还活着"。
func (a *Autopilot) markTick() {
	a.mu.Lock()
	a.lastTickAt = a.now()
	a.mu.Unlock()
}

// Heartbeat 返回当前心跳快照，供健康检查与前端使用。
func (a *Autopilot) Heartbeat() Heartbeat {
	a.mu.Lock()
	defer a.mu.Unlock()
	beat := Heartbeat{
		Running:       a.started,
		LastError:     a.lastError,
		FailureStreak: a.failureStreak,
		Degraded:      a.degraded,
	}
	if !a.lastTickAt.IsZero() {
		at := a.lastTickAt
		beat.LastTickAt = &at
	}
	if !a.lastCycleAt.IsZero() {
		at := a.lastCycleAt
		beat.LastCycleAt = &at
	}
	if !a.lastSuccessAt.IsZero() {
		at := a.lastSuccessAt
		beat.LastSuccessAt = &at
	}
	if !a.nextRun.IsZero() && a.started {
		at := a.nextRun
		beat.SchedulerAt = &at
	}
	return beat
}

// recordFailure 累计连续失败，达到阈值时自动降级。
//
// 降级只做一件事：把 pause_buying 打开。止损、防守线、撮合都不受影响——
// 系统出问题时最需要保留的恰恰是"能卖出"，而不是"能买进"。
func (a *Autopilot) recordFailure(ctx context.Context, accountID string, cause error) {
	a.mu.Lock()
	a.failureStreak++
	streak := a.failureStreak
	a.lastError = cause.Error()
	alreadyDegraded := a.degraded
	a.recoverStreak = 0
	if streak >= degradeAfterFailures {
		a.degraded = true
	}
	a.mu.Unlock()

	if streak < degradeAfterFailures || alreadyDegraded {
		return
	}
	// 降级前再确认一次总闸没开：如果人已经全停了，再叠加一个"降级"只会让状态更难读。
	if control, err := a.store.GetControl(ctx, accountID); err == nil && control.KillSwitch {
		return
	}
	if _, err := a.service.SetControl(ctx, Control{
		AccountID:   accountID,
		PauseBuying: true,
		Reason:      fmt.Sprintf("自动降级：连续 %d 轮循环失败（%s）", streak, truncate(cause.Error(), 120)),
	}); err != nil {
		a.logf("event=autopilot_degrade_failed err=%v", err)
		return
	}
	a.logf("event=autopilot_degraded streak=%d err=%v", streak, cause)
	_ = a.store.AppendEvent(ctx, Event{
		AccountID: accountID,
		Severity:  EventSeverityCritical,
		Kind:      EventAutopilotDegraded,
		Message:   fmt.Sprintf("连续 %d 轮循环失败，已自动暂停买入（保护性卖出仍执行）", streak),
		Payload:   eventPayload(map[string]any{"streak": streak, "error": cause.Error()}),
		CreatedAt: time.Now().UTC(),
	})
}

// recordSuccess 清零失败计数，并在降级后连续成功足够多次时自动解除。
func (a *Autopilot) recordSuccess(ctx context.Context, accountID string) {
	a.mu.Lock()
	a.failureStreak = 0
	a.lastError = ""
	a.lastSuccessAt = a.now()
	if !a.degraded {
		a.mu.Unlock()
		return
	}
	a.recoverStreak++
	recoverStreak := a.recoverStreak
	a.mu.Unlock()

	if recoverStreak < recoverAfterSuccesses {
		return
	}
	control, err := a.store.GetControl(ctx, accountID)
	if err != nil {
		return
	}
	if control.KillSwitch {
		return
	}
	if _, err := a.service.SetControl(ctx, Control{
		AccountID:   accountID,
		PauseBuying: false,
		Reason:      fmt.Sprintf("自动解除降级：已连续 %d 轮成功", recoverStreak),
	}); err != nil {
		a.logf("event=autopilot_recover_failed err=%v", err)
		return
	}
	a.mu.Lock()
	a.degraded = false
	a.recoverStreak = 0
	a.mu.Unlock()
	a.logf("event=autopilot_recovered streak=%d", recoverStreak)
	_ = a.store.AppendEvent(ctx, Event{
		AccountID: accountID,
		Severity:  EventSeverityInfo,
		Kind:      EventAutopilotRecovered,
		Message:   fmt.Sprintf("连续 %d 轮成功，已自动解除买入暂停", recoverStreak),
		CreatedAt: time.Now().UTC(),
	})
}

// healLedger 定期对账并修复。
//
// 为什么是"自动修复"而不是"只告警"：冻结额与实际挂单不一致时，
// 可用资金会一直少算（或凭空多算），人不在场时这笔账只会越记越偏。
// 修复是幂等的重算，不是猜测——它按挂单重算应收冻结，而不是把数字凑平。
func (a *Autopilot) healLedger(ctx context.Context, accountID string, now time.Time) {
	a.mu.Lock()
	due := a.lastReconcile.IsZero() || now.Sub(a.lastReconcile) >= reconcileInterval
	a.mu.Unlock()
	if !due {
		return
	}
	a.mu.Lock()
	a.lastReconcile = now
	a.mu.Unlock()

	report, err := a.service.Reconcile(ctx, accountID, true)
	if err != nil {
		a.logf("event=autopilot_reconcile_failed err=%v", err)
		return
	}
	// 修复模式下的 OK 表示"修复后一致"，不能用来判断"修复前有没有问题"：
	// 真正说明发生过漂移的是 Repaired。用 OK 判断会漏掉每一次成功修复，
	// 而这恰恰是最该留痕的一类事实。
	if !report.Repaired && report.OK {
		return
	}
	a.logf("event=autopilot_drift drift=%.2f repaired=%t issues=%d", report.Drift, report.Repaired, len(report.Issues))
	severity := EventSeverityWarning
	kind := EventLedgerDriftHealed
	message := fmt.Sprintf("检测到账本冻结与挂单不一致（差额 %.2f 元），已按挂单重算并修复", report.DriftBefore)
	if !report.Repaired {
		severity = EventSeverityCritical
		kind = EventLedgerDrift
		message = fmt.Sprintf("检测到账本冻结与挂单不一致（差额 %.2f 元），且修复未成功", report.Drift)
	}
	_ = a.store.AppendEvent(ctx, Event{
		AccountID: accountID,
		Severity:  severity,
		Kind:      kind,
		Message:   message,
		Payload:   eventPayload(report),
		CreatedAt: time.Now().UTC(),
	})
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func (a *Autopilot) due(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !now.Before(a.nextRun)
}

func (a *Autopilot) setNext(at time.Time) {
	a.mu.Lock()
	a.nextRun = at
	a.mu.Unlock()
}

// RunCycle 执行一轮完整的交易循环。
//
// 闸门顺序不可交换，且每一道都留下 CycleGate 记录：
//  1. 自动开关（人工触发可越过，因为人工触发本身就是"我要跑一轮"的明确表达）
//  2. 交易时段（只有自动触发受此限制）
//  3. kill switch（任何触发方式都不能越过）
//  4. 当日熔断（任何触发方式都不能越过：它是资本保护，不是偏好）
//  5. 尾盘禁开仓（自动触发受此限制；人工可越过，但会记录原因）
//
// 为什么人工能越过时段与尾盘、却越不过熔断与总闸：
// 时段与尾盘是"什么时候做更划算"的问题，人能判断；熔断与总闸是
// "今天已经不能再亏"的事实，不因为人想再试一次而消失。
func (a *Autopilot) RunCycle(ctx context.Context, trigger CycleTrigger) (Cycle, error) {
	if !a.enter() {
		return Cycle{}, ErrCycleRunning
	}
	defer a.leave()

	now := a.now()
	accountID := DefaultAccountID
	config, err := a.store.GetAutopilotConfig(ctx, accountID)
	if err != nil {
		return Cycle{}, fmt.Errorf("读取自动循环配置失败：%w", err)
	}
	cycle := Cycle{
		ID:        newID("cyc"),
		AccountID: accountID,
		Trigger:   trigger,
		Status:    CycleStatusRunning,
		StartedAt: now.UTC(),
		Gates:     []CycleGate{},
	}
	finish := func(status CycleStatus, reason string) Cycle {
		end := a.now().UTC()
		cycle.Status = status
		cycle.Reason = reason
		cycle.EndedAt = &end
		cycle.DurationMS = end.Sub(cycle.StartedAt).Milliseconds()
		if cycle.Gates == nil {
			cycle.Gates = []CycleGate{}
		}
		if err := a.store.SaveCycle(ctx, cycle); err != nil {
			a.logf("event=autopilot_cycle_save_failed cycle=%s err=%v", cycle.ID, err)
		}
		a.logf("event=autopilot_cycle cycle=%s trigger=%s status=%s reason=%q orders=%d filled=%d day_pnl=%.2f%%",
			cycle.ID, cycle.Trigger, cycle.Status, cycle.Reason, cycle.Orders, cycle.Filled, cycle.DayChangePercent)
		return cycle
	}

	automatic := trigger != CycleTriggerManual

	// 1. 自动开关。
	if automatic && !config.Enabled {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateAutopilotEnabled, Label: "自动循环", Open: false, Detail: "开关关闭"})
		return finish(CycleStatusSkipped, "自动循环已关闭"), nil
	}
	cycle.Gates = append(cycle.Gates, CycleGate{Code: GateAutopilotEnabled, Label: "自动循环", Open: true})

	// 2. 交易时段。
	if automatic && !marketrule.IsTradingSession(now) {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateSession, Label: "交易时段", Open: false, Detail: "不在 9:30-11:30 / 13:00-15:00"})
		return finish(CycleStatusSkipped, "当前不在交易时段，不发起新一轮"), nil
	}
	cycle.Gates = append(cycle.Gates, CycleGate{Code: GateSession, Label: "交易时段", Open: true})

	// 3. 账户总闸。
	control, err := a.store.GetControl(ctx, accountID)
	if err != nil {
		return finish(CycleStatusFailed, fmt.Sprintf("读取账户总闸失败：%v", err)), err
	}
	if control.KillSwitch {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateKillSwitch, Label: "账户总闸", Open: false, Detail: "kill switch 已打开"})
		return finish(CycleStatusBlocked, "账户已全部停止（kill switch），本轮不发起计划"), nil
	}
	cycle.Gates = append(cycle.Gates, CycleGate{Code: GateKillSwitch, Label: "账户总闸", Open: true, Detail: control.Gate()})

	// 4. 当日轮次配额（防呆，不防风险）。
	dayStart := a.dayStart(now)
	used, err := a.store.CountCyclesSince(ctx, accountID, dayStart)
	if err != nil {
		a.logf("event=autopilot_quota_count_failed err=%v", err)
	}
	if automatic && used >= config.MaxCyclesPerDay {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateCycleQuota, Label: "当日轮次", Open: false,
			Detail: fmt.Sprintf("已用 %d/%d 轮", used, config.MaxCyclesPerDay)})
		return finish(CycleStatusSkipped, fmt.Sprintf("当日已发起 %d 轮，达到上限 %d", used, config.MaxCyclesPerDay)), nil
	}

	// 5. 账户与当日基准。
	account, _, err := a.service.AccountView(ctx, accountID)
	if err != nil {
		return finish(CycleStatusFailed, fmt.Sprintf("读取账户失败：%v", err)), err
	}
	cycle.EquityBefore = account.Equity
	state, err := a.store.EnsureDayState(ctx, accountID, TradingDay(now), account.Equity, now)
	if err != nil {
		return finish(CycleStatusFailed, fmt.Sprintf("写入当日基准失败：%v", err)), err
	}
	cycle.DayStartEquity = state.StartEquity
	cycle.DayChangePercent = percentChange(state.StartEquity, account.Equity)

	// 5.5 组合级回撤观察。与日亏熔断分开算：
	// 日亏每天零点归零，连续阴跌两周的账户永远不会触发它；回撤跨日累计。
	// 这里只观察、落库、按需按下暂停买入，真正的拦截仍由 control 与裁决层执行——
	// 同一个判断只留一条执行路径，否则"熔断了但计划还在买"会再次出现。
	if riskState, riskErr := a.service.ObserveEquity(ctx, accountID, account.Equity, now); riskErr != nil {
		cycle.Notes = append(cycle.Notes, fmt.Sprintf("回撤观察失败：%v", riskErr))
	} else {
		cycle.DrawdownPercent = riskState.DrawdownPercent
		if riskState.Paused() {
			cycle.Gates = append(cycle.Gates, CycleGate{Code: GateDrawdown, Label: "净值回撤", Open: false,
				Detail: fmt.Sprintf("%s：从高点 %.2f 回撤 %.2f%%", drawdownLevelLabel(riskState.Level),
					riskState.PeakEquity, riskState.DrawdownPercent)})
		} else {
			cycle.Gates = append(cycle.Gates, CycleGate{Code: GateDrawdown, Label: "净值回撤", Open: true,
				Detail: fmt.Sprintf("回撤 %.2f%%，警戒线 %.2f%%", riskState.DrawdownPercent, riskState.Policy.WarnPercent)})
		}
	}

	// 6. 熔断：用当日基准权益判断，而不是用"从最高点回撤"。
	// 日亏口径与人的直觉一致（今天亏了多少），也不会因为盘中新高而把亏损藏起来。
	state = a.checkBreaker(ctx, state, account.Equity, config, now)
	cycle.Breaker = state.Breaker

	var buyBlock *BuyBlock
	if state.Tripped() {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateBreaker, Label: "当日熔断", Open: false, Detail: state.BreakerDetail})
		buyBlock = &BuyBlock{Code: policy.CodeGateBreaker, Message: "当日已熔断，本轮只允许卖出与减仓：" + state.BreakerDetail}
	} else {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateBreaker, Label: "当日熔断", Open: true, Detail: "未触发"})
	}

	// 7. 尾盘禁开仓。时段闸门对任何触发方式都要记录，只是"是否生效"不同：
	// 人工触发时它仍然显示为已过点，但放行，界面必须能看出这个差别。
	cutoffReached := false
	if cutoff, err := parseClock(config.BuyCutoff); err == nil && clockMinutes(now) >= cutoff {
		cutoffReached = true
	}
	switch {
	case buyBlock != nil:
		// 熔断已经禁买，不再用时段重复解释一次。
	case cutoffReached && automatic:
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateBuyWindow, Label: "开仓时段", Open: false,
			Detail: fmt.Sprintf("已过 %s", config.BuyCutoff)})
		buyBlock = &BuyBlock{
			Code:    policy.CodeGateWindow,
			Message: fmt.Sprintf("已过 %s：尾盘买入无法当日验证，隔夜跳空风险不可控，本轮不开新仓", config.BuyCutoff),
		}
	case cutoffReached:
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateBuyWindow, Label: "开仓时段", Open: true,
			Detail: fmt.Sprintf("已过 %s，人工触发允许开仓", config.BuyCutoff)})
	default:
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateBuyWindow, Label: "开仓时段", Open: true,
			Detail: fmt.Sprintf("开仓截止 %s", config.BuyCutoff)})
	}

	// 8. 先撮合既有挂单：上一轮挂出去的买单可能本轮才够条件成交，
	// 把它放在生成新计划之前，快照里的可用资金才是撮合后的真实值。
	if matched, matchErr := a.service.MatchPending(ctx, accountID); matchErr != nil {
		if !errors.Is(matchErr, ErrKillSwitch) {
			cycle.Notes = append(cycle.Notes, fmt.Sprintf("撮合既有挂单失败：%v", matchErr))
		}
	} else {
		for _, order := range matched {
			if order.Status == OrderStatusFilled || order.Status == OrderStatusPartial {
				cycle.Matched++
			}
		}
	}

	// 8.4 波动率追踪：价格创新高后把止损跟上去。
	// 放在防守线之前：上移后的止损要么在本次检查里直接触发离场，
	// 要么成为新的底线；放在之后则会晚一轮生效，而这一轮里回撤已经发生了。
	if raised, trailErr := a.service.TrailStopsByATR(ctx, accountID); trailErr != nil {
		cycle.Notes = append(cycle.Notes, fmt.Sprintf("止损跟踪失败：%v", trailErr))
	} else if raised > 0 {
		cycle.Notes = append(cycle.Notes, fmt.Sprintf("按波动率上移止损 %d 笔", raised))
	}

	// 8.5 持仓防守线。它排在这里的理由：
	//   - 在撮合之后：拿到的持仓是刚成交过的真实持仓；
	//   - 在生成计划之前：模型看到的持仓已经是"该止损的已经卖掉"，
	//     不会对着一个已经被清掉的仓位再给一次卖出建议；
	//   - 即使本轮禁买（熔断/尾盘），它照常执行——止损不是开仓。
	protective, protectiveErr := a.service.RunProtective(ctx, accountID, cycle.ID)
	if protectiveErr != nil {
		cycle.Notes = append(cycle.Notes, fmt.Sprintf("防守线检查失败：%v", protectiveErr))
	}
	if protective.Triggered > 0 {
		cycle.Gates = append(cycle.Gates, CycleGate{Code: GateProtective, Label: "持仓防守线", Open: true,
			Detail: fmt.Sprintf("检查 %d 只，触发 %d 笔，成交 %d 笔", protective.Checked, protective.Triggered, protective.Filled)})
		cycle.Notes = append(cycle.Notes, fmt.Sprintf("防守线触发 %d 笔离场，成交 %d 笔", protective.Triggered, protective.Filled))
		cycle.DecisionID = firstNonEmpty(cycle.DecisionID, protective.DecisionID)
		// 止损成交会改变连败计数，必须立刻复核一次熔断：
		// 等下一轮（可能 3 分钟后）才熔断，这 3 分钟里还会再开新仓。
		if state.StopStreak > 0 || state.Tripped() {
			equityNow := cycle.EquityBefore
			if refreshed, _, err := a.service.AccountView(ctx, accountID); err == nil {
				equityNow = refreshed.Equity
			}
			state = a.checkBreaker(ctx, state, equityNow, config, now)
			cycle.Breaker = state.Breaker
			if state.Tripped() {
				buyBlock = &BuyBlock{Code: policy.CodeGateBreaker,
					Message: "当日已熔断，本轮只允许卖出与减仓：" + state.BreakerDetail}
			}
		}
	}

	// 9. 生成计划并裁决。
	request := PlanRequest{
		AccountID:     accountID,
		AutoUniverse:  true,
		TraderProfile: config.TraderProfile,
		Horizon:       config.Horizon,
		buyBlock:      buyBlock,
		// 只有定时轮次允许跳过"什么都没变"的一轮；人工触发必须真的问一次模型。
		skipOnNoChange: automatic,
	}
	plan, planErr := a.service.RunPlanSync(ctx, request, cycle.ID)
	cycle.PlanID = plan.ID
	cycle.DecisionID = plan.DecisionID
	cycle.Blocked = countOrders(plan.Orders, OrderStatusRejected) + droppedFromVerdicts(plan.Verdicts)
	cycle.Orders = countOrders(plan.Orders, OrderStatusPending, OrderStatusPartial)

	switch {
	case errors.Is(planErr, ErrPlanRunning):
		return finish(CycleStatusSkipped, "另有一轮正在生成计划，本轮让位"), nil
	case errors.Is(planErr, ErrNoMaterialChange):
		// 输入没变就不问模型。这是一条正常结论（省下一次调用），不是故障。
		return finish(CycleStatusSkipped, plan.ErrorMessage), nil
	case planErr != nil && plan.DecisionID == "":
		// 连裁决都没产生，说明在取数或模型阶段就断了：这是真失败。
		return finish(CycleStatusFailed, plan.ErrorMessage), planErr
	}
	// 有裁决但计划被判失败，通常是"模型建议全部被拦"或"本轮没有机会"。
	// 这不是故障，是一条结论，必须按 done 记录，否则失败率会虚高；
	// 但结论本身要写进日志，否则这张卡片只会显示"建议 0 笔"，看不出是熔断拦的。
	if plan.Status == PlanStatusFailed && strings.TrimSpace(plan.ErrorMessage) != "" {
		cycle.Notes = append(cycle.Notes, plan.ErrorMessage)
	}

	// 10. 自动执行。
	if config.AutoExecute && plan.Status == PlanStatusDraft && cycle.Orders > 0 {
		executed, execErr := a.service.Execute(ctx, plan.ID)
		if execErr != nil {
			cycle.Notes = append(cycle.Notes, fmt.Sprintf("自动执行失败：%v", execErr))
		} else {
			plan = executed
			cycle.Filled = countOrders(plan.Orders, OrderStatusFilled, OrderStatusPartial)
			cycle.Notes = append(cycle.Notes, fmt.Sprintf("自动执行：%d 笔挂单，成交 %d 笔", cycle.Orders, cycle.Filled))
		}
	} else if plan.Status == PlanStatusDraft && cycle.Orders > 0 {
		cycle.Notes = append(cycle.Notes, "自动执行已关闭，计划停在待确认，需人工扣扳机")
	}

	// 11. 收盘权益与熔断复核：本轮成交后可能刚好越过阈值，
	// 那就必须在同一轮里把熔断标出来，而不是等下一轮（下一轮可能已经亏更多）。
	if after, _, err := a.service.AccountView(ctx, accountID); err == nil {
		cycle.EquityAfter = after.Equity
		cycle.DayChangePercent = percentChange(state.StartEquity, after.Equity)
		state = a.checkBreaker(ctx, state, after.Equity, config, now)
		cycle.Breaker = state.Breaker
		// 成交后净值可能已经跌出新低：不在这里复核，回撤要到下一轮才被发现，
		// 而下一轮开始时可能已经又跌了一段。
		if riskState, riskErr := a.service.ObserveEquity(ctx, accountID, after.Equity, now); riskErr == nil {
			cycle.DrawdownPercent = riskState.DrawdownPercent
		} else {
			cycle.Notes = append(cycle.Notes, fmt.Sprintf("回撤复核失败：%v", riskErr))
		}
	}
	if cycle.EquityAfter == 0 {
		cycle.EquityAfter = cycle.EquityBefore
	}
	a.mu.Lock()
	a.lastCycleAt = now
	a.mu.Unlock()
	return finish(CycleStatusDone, ""), nil
}

func (a *Autopilot) enter() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return false
	}
	a.running = true
	return true
}

func (a *Autopilot) leave() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
}

// checkBreaker 判断当日是否触发熔断；触发时同时按下账户的 pause_buying。
//
// 两处都要写：DayState 是"今天的既成事实"（供循环与复盘读），
// Control 是"账户现在不许买"（供人工计划与撮合读）。只写一处，
// 就会出现"熔断了但手工生成的计划还在买"这种自相矛盾的状态。
func (a *Autopilot) checkBreaker(ctx context.Context, state DayState, equity float64, config AutopilotConfig, now time.Time) DayState {
	if state.Tripped() || state.StartEquity <= 0 {
		return state
	}
	loss := percentChange(state.StartEquity, equity)
	trip := func(code, detail string) DayState {
		at := now.UTC()
		state.Breaker = code
		state.BreakerDetail = detail
		state.BreakerAt = &at
		state.UpdatedAt = at
		if err := a.store.SaveDayState(ctx, state); err != nil {
			a.logf("event=autopilot_breaker_save_failed err=%v", err)
		}
		control, err := a.store.GetControl(ctx, state.AccountID)
		if err != nil {
			a.logf("event=autopilot_breaker_control_failed err=%v", err)
			return state
		}
		control.PauseBuying = true
		control.Reason = code + "：" + detail
		if _, err := a.store.SaveControl(ctx, control); err != nil {
			a.logf("event=autopilot_breaker_pause_failed err=%v", err)
		}
		a.logf("event=autopilot_breaker account=%s code=%s detail=%q", state.AccountID, code, detail)
		return state
	}
	if loss <= -config.MaxDailyLossPercent {
		return trip(BreakerDailyLoss, fmt.Sprintf("当日权益 %.2f → %.2f（%+.2f%%），触发 -%.1f%% 日亏熔断",
			state.StartEquity, equity, loss, config.MaxDailyLossPercent))
	}
	if state.StopStreak >= stopStreakLimit {
		return trip(BreakerStopStreak, fmt.Sprintf("连续 %d 笔止损，触发连败熔断", state.StopStreak))
	}
	return state
}

// RecordStopLoss 记录一次止损平仓。
//
// 保留它是因为「连续止损」的口径可能不止账户级一种（例如按策略、按行业），
// 而 DayState.StopStreak 已经落库；真正的写入路径是 Service.recordStopOutcome
// 走 Store.AdjustStopStreak（原子累加），这里只留作显式入口。
func (a *Autopilot) RecordStopLoss(ctx context.Context, accountID string) (DayState, error) {
	now := a.now()
	if _, err := a.store.AdjustStopStreak(ctx, a.service.accountID(accountID), TradingDay(now), 1); err != nil {
		return DayState{}, err
	}
	state, err := a.store.GetDayState(ctx, a.service.accountID(accountID), TradingDay(now))
	if err != nil {
		return DayState{}, err
	}
	if state.StopStreak >= stopStreakLimit && !state.Tripped() {
		at := now.UTC()
		state.Breaker = BreakerStopStreak
		state.BreakerDetail = fmt.Sprintf("连续 %d 笔止损", state.StopStreak)
		state.BreakerAt = &at
		state.UpdatedAt = at
		if err := a.store.SaveDayState(ctx, state); err != nil {
			return DayState{}, err
		}
	}
	return state, nil
}

// maybeReview 在收盘后生成当日复盘，每天只生成一次。
//
// 为什么需要幂等判断：tick 每 15 秒一次，收盘后到午夜之间有上千次机会，
// 没有判断就会把同一份复盘重算上千遍，而它每次都要扫几百条裁决与成交。
func (a *Autopilot) maybeReview(ctx context.Context, accountID string, now time.Time) {
	// 非交易日不生成复盘：假期里没有成交、没有行情，算出来的只是"今天什么都没发生"，
	// 既占存储也会把经验样本里的零值样本越堆越多。
	if !marketrule.IsTradingDay(now) {
		return
	}
	// 15:05 之后：给收盘集合竞价与数据源留 5 分钟。
	if clockMinutes(now) < 15*60+5 {
		return
	}
	day := TradingDay(now)
	if _, exists, err := a.store.GetReview(ctx, accountID, day); err != nil {
		a.logf("event=autopilot_review_check_failed err=%v", err)
		return
	} else if exists {
		return
	}
	review, err := a.service.GenerateReview(ctx, accountID, day)
	if err != nil {
		a.logf("event=autopilot_review_failed day=%s err=%v", day, err)
		return
	}
	a.logf("event=autopilot_review day=%s closed=%d pnl=%.2f", day, review.Facts.ClosedTrades, review.Facts.RealizedPnL)
}

// ResetBreaker 由人显式解除当日熔断，必须给理由。
//
// 为什么允许解除：熔断依赖的是行情快照与账户权益，一次错误价格（停牌复牌、
// 除权未调整）就能把它误触。没有解除手段，一个错误数据会把账户冻结一整天。
//
// 为什么必须给理由：这是唯一一处"人推翻机器保护"的入口，留下理由才能事后
// 判断这次解除是必要的还是冲动的。理由会写进当日状态与日志。
func (a *Autopilot) ResetBreaker(ctx context.Context, accountID, reason string) (DayState, error) {
	accountID = a.service.accountID(accountID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DayState{}, errors.New("解除熔断必须填写原因")
	}
	now := a.now()
	state, err := a.store.GetDayState(ctx, accountID, TradingDay(now))
	if err != nil {
		return DayState{}, err
	}
	if !state.Tripped() {
		return state, nil
	}
	trippedCode := state.Breaker
	state.Breaker = ""
	state.BreakerDetail = fmt.Sprintf("已于 %s 人工解除：%s", now.In(time.FixedZone("CST", 8*3600)).Format("15:04"), reason)
	state.BreakerAt = nil
	state.StopStreak = 0
	state.UpdatedAt = now.UTC()
	if err := a.store.SaveDayState(ctx, state); err != nil {
		return DayState{}, err
	}
	// 只解除熔断自己按下的暂停：如果人本来就手动暂停了买入，
	// 解除熔断不应该顺手把它也打开。
	if control, err := a.store.GetControl(ctx, accountID); err == nil {
		if strings.HasPrefix(control.Reason, trippedCode+":") || strings.HasPrefix(control.Reason, trippedCode+"：") {
			control.PauseBuying = false
			control.Reason = "熔断已人工解除：" + reason
			if _, err := a.store.SaveControl(ctx, control); err != nil {
				a.logf("event=autopilot_reset_control_failed err=%v", err)
			}
		}
	}
	a.logf("event=autopilot_breaker_reset account=%s code=%s reason=%q", accountID, trippedCode, reason)
	return state, nil
}

// Status 汇总自动循环的整体状态，供界面一次性渲染。
func (a *Autopilot) Status(ctx context.Context, accountID string) (AutopilotStatus, error) {
	accountID = a.service.accountID(accountID)
	config, err := a.store.GetAutopilotConfig(ctx, accountID)
	if err != nil {
		return AutopilotStatus{}, err
	}
	now := a.now()
	state, err := a.store.GetDayState(ctx, accountID, TradingDay(now))
	if err != nil {
		return AutopilotStatus{}, err
	}
	status := AutopilotStatus{
		Config: config,
		Today:  state,
	}
	a.mu.Lock()
	status.Running = a.running
	if config.Enabled && !a.nextRun.IsZero() {
		next := a.nextRun
		status.NextRunAt = &next
	}
	a.mu.Unlock()

	if cycles, err := a.store.ListCycles(ctx, accountID, 1); err == nil && len(cycles) > 0 {
		status.LastCycle = &cycles[0]
	}
	if used, err := a.store.CountCyclesSince(ctx, accountID, a.dayStart(now)); err == nil {
		status.CyclesToday = used
	}
	// 回撤状态是跨日事实，必须单独读出来放进状态：
	// 它不在 DayState 里，前端只看当日熔断会漏掉"从高点掉了 10%"这件事。
	risk, riskErr := a.store.GetRiskState(ctx, accountID)
	if riskErr != nil {
		a.logf("event=autopilot_risk_state_failed err=%v", riskErr)
	}
	status.Risk = risk
	status.Breakers = []BreakerView{
		{
			Code:    BreakerDailyLoss,
			Label:   fmt.Sprintf("日亏熔断（-%.1f%%）", config.MaxDailyLossPercent),
			Armed:   true,
			Tripped: state.Breaker == BreakerDailyLoss,
			Detail:  breakerDetail(state, BreakerDailyLoss),
		},
		{
			Code:    BreakerStopStreak,
			Label:   fmt.Sprintf("连败熔断（连续 %d 笔止损）", stopStreakLimit),
			Armed:   true,
			Tripped: state.Breaker == BreakerStopStreak,
			Detail:  fmt.Sprintf("当日已连续止损 %d 笔，达到 %d 笔即熔断", state.StopStreak, stopStreakLimit),
		},
		{
			Code:    "drawdown",
			Label:   fmt.Sprintf("回撤熔断（-%.1f%% 警戒 / -%.1f%% 停止）", risk.Policy.WarnPercent, risk.Policy.StopPercent),
			Armed:   risk.PeakEquity > 0,
			Tripped: risk.Tripped(),
			Detail: fmt.Sprintf("高点 %.2f、现值 %.2f，回撤 %.2f%%%s", risk.PeakEquity, risk.Equity,
				risk.DrawdownPercent, drawdownSuffix(risk)),
		},
	}
	return status, nil
}

func breakerDetail(state DayState, code string) string {
	if state.Breaker != code {
		return ""
	}
	return state.BreakerDetail
}

// Configure 保存自动循环配置。关闭时不清除当日熔断：
// 熔断是当日的事实，不能靠"先把开关关掉再打开"绕过去。
func (a *Autopilot) Configure(ctx context.Context, config AutopilotConfig) (AutopilotConfig, error) {
	config.AccountID = a.service.accountID(config.AccountID)
	saved, err := a.store.SaveAutopilotConfig(ctx, config)
	if err != nil {
		return AutopilotConfig{}, err
	}
	if saved.Enabled {
		// 打开后尽快跑第一轮，而不是等满一个间隔。
		a.setNext(a.now())
	}
	a.logf("event=autopilot_configured account=%s enabled=%t auto_execute=%t interval=%ds",
		saved.AccountID, saved.Enabled, saved.AutoExecute, saved.IntervalSeconds)
	return saved, nil
}

// Cycles 返回最近的循环记录。
func (a *Autopilot) Cycles(ctx context.Context, accountID string, limit int) ([]Cycle, error) {
	return a.store.ListCycles(ctx, a.service.accountID(accountID), limit)
}

// dayStart 返回北京时间当天 00:00 对应的时刻，用作轮次统计的起点。
func (a *Autopilot) dayStart(now time.Time) time.Time {
	zone := time.FixedZone("CST", 8*3600)
	cst := now.In(zone)
	return time.Date(cst.Year(), cst.Month(), cst.Day(), 0, 0, 0, 0, zone)
}

func (a *Autopilot) logf(format string, args ...any) {
	if a.logger != nil {
		a.logger.Printf(format, args...)
	}
}

func percentChange(from, to float64) float64 {
	if from <= 0 {
		return 0
	}
	return round2((to/from - 1) * 100)
}

func countOrders(orders []Order, statuses ...OrderStatus) int {
	wanted := make(map[OrderStatus]bool, len(statuses))
	for _, status := range statuses {
		wanted[status] = true
	}
	count := 0
	for _, order := range orders {
		if wanted[order.Status] {
			count++
		}
	}
	return count
}

// droppedFromVerdicts 从裁决流水里还原"被整批剔除的建议数"。
//
// 被剔除的买入根本不会变成订单，所以只数订单会把"熔断拦了 3 笔"记成"建议 0 笔"。
// 日志卡片上这两句话的差别，就是"系统今天没出手"与"系统想出手但被自己拦住"。
func droppedFromVerdicts(verdicts []policy.Verdict) int {
	dropped := 0
	for _, verdict := range verdicts {
		if verdict.Code == policy.CodeOrderBuyDropped {
			dropped += int(verdict.Metric)
		}
	}
	return dropped
}
