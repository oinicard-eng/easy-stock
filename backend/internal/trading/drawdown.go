package trading

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 组合级回撤熔断：从净值高点的回撤，而不是"今天亏了多少"。
//
// 两种口径不能互相替代：
//   - 日亏熔断（Autopilot.checkBreaker）回答"今天是不是亏够了"，它每天零点归零，
//     所以连续阴跌两周、每天跌 5% 的账户永远不会触发它；
//   - 回撤熔断回答"距离我最好的时候掉了多少"，它跨日累计，能抓住慢刀子。
//
// 三个关键设计：
//
//  1. 高点只认"已确认权益"——每轮循环结束时的权益与首次观察时的初始资金，
//     不用盘中 tick 的瞬时值。一根插针就能把峰值抬高 3%，之后正常波动会被
//     误判成回撤，而误报多了这条防线就会被关掉。
//  2. 两档都只暂停买入，不按 kill switch。kill switch 会连止损卖出一起拦，
//     而回撤最深的时刻恰恰是最需要减仓的时刻。
//  3. 恢复有滞回：回到 ReleasePercent 以下才解除，避免在警戒线附近
//     7.9% ↔ 8.1% 反复开关，把人训练成忽略告警。

const (
	// DrawdownLevelNone 表示没触及任何档位。
	DrawdownLevelNone = ""
	// DrawdownLevelWarn 表示触及警戒线：暂停买入，卖出与防守线照常。
	DrawdownLevelWarn = "drawdown.warn"
	// DrawdownLevelStop 表示触及停止线：暂停买入且必须人工解除。
	DrawdownLevelStop = "drawdown.stop"
)

// DrawdownPolicy 是回撤熔断的两道阈值与恢复线。
//
// 默认 8% / 12% / 5% 来自一条经验约束：单票 30%、总仓位 80% 的账户，
// 一个 -10% 的板块级回撤会带来约 -8% 的净值回撤——警戒线必须落在
// "还能靠选股修复"与"已经伤到本金"之间。
type DrawdownPolicy struct {
	WarnPercent float64 `json:"warn_percent"`
	StopPercent float64 `json:"stop_percent"`
	// ReleasePercent 是自动解除线，必须严格低于 WarnPercent，否则没有滞回。
	ReleasePercent float64 `json:"release_percent"`
}

// DefaultDrawdownPolicy 返回基准阈值。
func DefaultDrawdownPolicy() DrawdownPolicy {
	return DrawdownPolicy{WarnPercent: 8, StopPercent: 12, ReleasePercent: 5}
}

// Normalize 修正非法配置：停止线必须高于警戒线，警戒线必须高于解除线。
// 阈值写反的后果是"永远不触发"或"一触发就锁死"，两者都比报错更难发现。
func (p DrawdownPolicy) Normalize() DrawdownPolicy {
	if p.WarnPercent <= 0 || p.WarnPercent >= 100 {
		p.WarnPercent = 8
	}
	if p.StopPercent <= p.WarnPercent || p.StopPercent >= 100 {
		p.StopPercent = p.WarnPercent + 4
	}
	if p.ReleasePercent < 0 || p.ReleasePercent >= p.WarnPercent {
		p.ReleasePercent = p.WarnPercent / 2
	}
	return p
}

// RiskState 是账户级风险状态：净值高点、当前回撤、所处档位。
//
// 它必须落库。进程重启后"我上周从高点回撤了 11%"这个事实仍然成立，
// 只放在内存里的话，一次部署就会把回撤归零、把已经该停的账户重新放开买入。
type RiskState struct {
	AccountID  string    `json:"account_id"`
	PeakEquity float64   `json:"peak_equity"`
	PeakAt     time.Time `json:"peak_at,omitempty"`
	// Equity 是最近一次观察到的权益，供展示"高点 / 现值 / 回撤"三件套。
	Equity          float64        `json:"equity"`
	DrawdownPercent float64        `json:"drawdown_percent"`
	Level           string         `json:"level,omitempty"`
	Policy          DrawdownPolicy `json:"policy"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Tripped 表示已触及停止线。
func (r RiskState) Tripped() bool { return r.Level == DrawdownLevelStop }

// Paused 表示当前档位要求暂停买入（警戒线或停止线都算）。
func (r RiskState) Paused() bool {
	return r.Level == DrawdownLevelWarn || r.Level == DrawdownLevelStop
}

// Assess 计算回撤档位。
//
// prev 是上一次的档位，用于实现滞回：
//   - 跌破停损线 → stop（只能人工解除，不因反弹自动恢复）；
//   - 跌破警戒线 → warn；
//   - 在 warn 与 release 之间 → 维持原档位（不因为小幅反弹来回切换）；
//   - 回到 release 以下 → none。
func (p DrawdownPolicy) Assess(prev string, peak, equity float64) (string, float64) {
	p = p.Normalize()
	if peak <= 0 || equity <= 0 {
		return DrawdownLevelNone, 0
	}
	dd := (peak - equity) / peak * 100
	if dd < 0 {
		dd = 0
	}
	dd = round2(dd)
	switch {
	case dd >= p.StopPercent:
		return DrawdownLevelStop, dd
	case prev == DrawdownLevelStop:
		// 停止线不能靠反弹自动解除，也不能降级成警戒线：能弹回来 4% 的账户，
		// 也常常会再跌回去。解除必须由人确认，理由写进事件——
		// 这是唯一一处"人推翻机器保护"。
		return DrawdownLevelStop, dd
	case dd >= p.WarnPercent:
		return DrawdownLevelWarn, dd
	case prev == DrawdownLevelWarn && dd > p.ReleasePercent:
		return DrawdownLevelWarn, dd
	}
	return DrawdownLevelNone, dd
}

// drawdownReasonPrefix 用于区分"这条暂停买入是不是我按的"。
// 只有自己按下的开关才允许自动解除，否则回撤恢复会把人工 kill switch
// 或日亏熔断顺手一起解掉——那是最危险的一类 bug。
func drawdownReasonPrefix(level string) string {
	if level == "" {
		return "drawdown."
	}
	return level + "："
}

func hasDrawdownReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), "drawdown.")
}

// ObserveEquity 记录权益高点、评估回撤，并按需按下或解除暂停买入。
//
// 调用点有两处：每轮循环开始（用当前权益）与结束（用成交后的权益）。
// 只在一处调用会漏掉两种情况——开盘就跳空低开（还没成交就已经回撤），
// 以及盘中成交后净值跌破新低（要等下一轮才被发现）。
func (s *Service) ObserveEquity(ctx context.Context, accountID string, equity float64, now time.Time) (RiskState, error) {
	accountID = s.accountID(accountID)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	state, err := s.store.GetRiskState(ctx, accountID)
	if err != nil {
		return RiskState{}, err
	}
	policy := state.Policy.Normalize()
	previous := state.Level

	// 首次观察时把初始资金也当作高点：账户从 10 万亏到 9.2 万，
	// 如果只以 9.2 万为起点，回撤会显示 0%，第一次就该报的警永远不报。
	peak := state.PeakEquity
	if peak <= 0 {
		if account, err := s.store.GetAccount(ctx, accountID); err == nil && account.InitialCash > peak {
			peak = account.InitialCash
		}
	}
	peakAt := state.PeakAt
	if equity > peak {
		peak = equity
		peakAt = now
	}
	level, dd := policy.Assess(previous, peak, equity)

	state = RiskState{
		AccountID:       accountID,
		PeakEquity:      round2(peak),
		PeakAt:          peakAt,
		Equity:          round2(equity),
		DrawdownPercent: dd,
		Level:           level,
		Policy:          policy,
		UpdatedAt:       now.UTC(),
	}
	if err := s.store.SaveRiskState(ctx, state); err != nil {
		return state, err
	}
	if level == previous {
		return state, nil
	}
	s.applyDrawdownLevel(ctx, state, previous, now)
	return state, nil
}

// applyDrawdownLevel 把档位变化翻译成账户开关与事件。
//
// 事件而不是直接发告警：告警出口、冷却、合并、投递记账都已由 AlertManager
// 统一处理，这里再发一次会得到两份记录，而两份记录的冷却窗口互不相干。
func (s *Service) applyDrawdownLevel(ctx context.Context, state RiskState, previous string, now time.Time) {
	control, err := s.store.GetControl(ctx, state.AccountID)
	if err != nil {
		s.logf("event=drawdown_control_read_failed account=%s err=%v", state.AccountID, err)
		return
	}
	switch state.Level {
	case DrawdownLevelWarn, DrawdownLevelStop:
		severity := EventSeverityWarning
		kind := EventDrawdownWarn
		label := "净值回撤警戒"
		if state.Level == DrawdownLevelStop {
			severity = EventSeverityCritical
			kind = EventDrawdownStop
			label = "净值回撤止损"
		}
		// 别人按下的暂停不覆盖理由：日亏熔断、连续止损、人工 kill switch
		// 各有各的责任人，混成一句"回撤"会让复盘找不到真正的原因。
		if !control.PauseBuying || hasDrawdownReason(control.Reason) {
			control.PauseBuying = true
			control.Reason = fmt.Sprintf("%s：净值从 %.2f 回撤到 %.2f（-%.2f%%），超过 %.2f%% 阈值，暂停买入",
				state.Level, state.PeakEquity, state.Equity, state.DrawdownPercent, thresholdOf(state.Policy, state.Level))
			control.UpdatedAt = now.UTC()
			if _, err := s.store.SaveControl(ctx, control); err != nil {
				s.logf("event=drawdown_control_save_failed account=%s err=%v", state.AccountID, err)
			}
		}
		_ = s.store.AppendEvent(ctx, Event{
			AccountID: state.AccountID,
			Severity:  severity,
			Kind:      kind,
			Message: fmt.Sprintf("%s：净值高点 %.2f → 现值 %.2f（-%.2f%%），%s",
				label, state.PeakEquity, state.Equity, state.DrawdownPercent,
				pauseConsequence(state.Level)),
			CreatedAt: now.UTC(),
		})
	case DrawdownLevelNone:
		if previous == DrawdownLevelNone {
			return
		}
		// 只有自己按下的暂停才解除；停止线转 none 只可能是人工解除后重算，
		// 这里同样只清自己写的那条理由。
		if control.PauseBuying && hasDrawdownReason(control.Reason) {
			control.PauseBuying = false
			control.Reason = fmt.Sprintf("drawdown.release：回撤收敛到 %.2f%%，自动解除暂停买入", state.DrawdownPercent)
			control.UpdatedAt = now.UTC()
			if _, err := s.store.SaveControl(ctx, control); err != nil {
				s.logf("event=drawdown_release_save_failed account=%s err=%v", state.AccountID, err)
			}
		}
		_ = s.store.AppendEvent(ctx, Event{
			AccountID: state.AccountID,
			Severity:  EventSeverityInfo,
			Kind:      EventDrawdownRelease,
			Message: fmt.Sprintf("回撤解除：现值 %.2f，距高点 %.2f 仅 -%.2f%%，恢复买入",
				state.Equity, state.PeakEquity, state.DrawdownPercent),
			CreatedAt: now.UTC(),
		})
	}
}

func thresholdOf(policy DrawdownPolicy, level string) float64 {
	if level == DrawdownLevelStop {
		return policy.StopPercent
	}
	return policy.WarnPercent
}

func pauseConsequence(level string) string {
	if level == DrawdownLevelStop {
		return "已暂停买入，需人工确认后解除"
	}
	return "已暂停买入，卖出与防守线照常执行"
}

// drawdownLevelLabel 给档位一个中文短名，供闸门与界面使用。
func drawdownLevelLabel(level string) string {
	switch level {
	case DrawdownLevelStop:
		return "回撤停止线"
	case DrawdownLevelWarn:
		return "回撤警戒线"
	}
	return "回撤正常"
}

func drawdownSuffix(state RiskState) string {
	switch state.Level {
	case DrawdownLevelStop:
		return "（已触发停止线，买入暂停，需人工解除）"
	case DrawdownLevelWarn:
		return "（已触发警戒线，买入暂停）"
	}
	return fmt.Sprintf("（警戒线 %.2f%%）", state.Policy.WarnPercent)
}

// RiskView 返回账户当前的回撤状态与总闸。它只读，不更新高点：
// 查询接口如果顺带写库，用监控每分钟打一次就会把峰值刷成"最近一次查询时的权益"。
func (s *Service) RiskView(ctx context.Context, accountID string) (RiskState, Control, error) {
	accountID = s.accountID(accountID)
	state, err := s.store.GetRiskState(ctx, accountID)
	if err != nil {
		return RiskState{}, Control{}, err
	}
	control, err := s.store.GetControl(ctx, accountID)
	if err != nil {
		return RiskState{}, Control{}, err
	}
	return state, control, nil
}

// ReleaseDrawdown 由人显式解除回撤熔断。
//
// 与 ResetBreaker 的区别：那个解除的是"今天亏够了"，第二天自然失效；
// 这个解除的是"距离高点掉了太多"，不解除会一直压着，直到净值创新高。
// 因此它必须给理由，并且解除后仍保留高点——高点不会因为人工放行而降低。
func (s *Service) ReleaseDrawdown(ctx context.Context, accountID, reason string) (RiskState, error) {
	accountID = s.accountID(accountID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return RiskState{}, errors.New("解除回撤熔断必须填写原因")
	}
	state, err := s.store.GetRiskState(ctx, accountID)
	if err != nil {
		return RiskState{}, err
	}
	if !state.Paused() {
		return state, nil
	}
	now := time.Now().UTC()
	control, err := s.store.GetControl(ctx, accountID)
	if err != nil {
		return RiskState{}, err
	}
	if control.PauseBuying && hasDrawdownReason(control.Reason) {
		control.PauseBuying = false
		control.Reason = "drawdown.release：" + reason
		control.UpdatedAt = now
		if _, err := s.store.SaveControl(ctx, control); err != nil {
			return state, err
		}
	}
	previous := state.Level
	releaseDrawdown := state.DrawdownPercent
	state.Level = DrawdownLevelNone
	// 解除即把高点重置为现值。不重置的话，下一轮观察会用"旧高点 vs 现值"
	// 立刻重新触发同一个档位，解除按钮等于没有——人按十次也解不开。
	// 重置的语义是"人已经接受了这段亏损，从当前净值重新计量"，
	// 它不会抹掉任何成交记录，平仓盈亏仍在账本里。
	if state.Equity > 0 {
		state.PeakEquity = state.Equity
		state.PeakAt = now
	}
	state.DrawdownPercent = 0
	state.UpdatedAt = now
	if err := s.store.SaveRiskState(ctx, state); err != nil {
		return state, err
	}
	_ = s.store.AppendEvent(ctx, Event{
		AccountID: accountID,
		Severity:  EventSeverityInfo,
		Kind:      EventDrawdownRelease,
		Message: fmt.Sprintf("人工解除回撤熔断（%s）：解除时回撤 -%.2f%%，高点已重置为现值 %.2f",
			reason, releaseDrawdown, state.PeakEquity),
		CreatedAt: now,
	})
	s.logf("event=drawdown_released account=%s from=%s drawdown=%.2f reason=%q",
		accountID, previous, releaseDrawdown, reason)
	return state, nil
}
