package trading

// 止损后的冷静期。
//
// 要解决的问题很具体：止损卖出之后，下一轮提示词里这只票又出现在候选池，
// 现价刚好比成本低一点，模型会给出"回调到位、可以再进"的建议。守卫能拦的只有
// 单票上限与行业上限，都拦不住"同一个坑赔两次"——因为第二次买的时候，仓位是空的。
//
// 两条规则，都由已发生的事实推导，不依赖模型自述：
//  1. 标的级：任一持仓因止损类原因清仓后的 4 小时内，不得再买入同一标的。
//  2. 板块级：同一行业当日累计 2 次止损后，当日不得再买入该行业任何标的。
//
// 为什么是"推导"而不是"另建一张表"：平仓记录已经完整记下了时间、标的、行业与原因。
// 再存一份就等于有两个真相来源，某一天它们对不上时，没人知道该信哪一个。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/policy"
)

// CooldownRule 是冷静期的参数。
type CooldownRule struct {
	// SymbolHours 是同一标的止损后的禁买时长。
	// 4 小时覆盖一个完整交易日的剩余时段：上午止损的票，当天不会再被买回。
	SymbolHours float64
	// SectorLossesPerDay 是同一行业当日止损次数上限。达到即当日禁买该行业。
	SectorLossesPerDay int
}

// DefaultCooldownRule 返回默认参数。
func DefaultCooldownRule() CooldownRule {
	return CooldownRule{SymbolHours: 4, SectorLossesPerDay: 2}
}

func (r CooldownRule) normalize() CooldownRule {
	defaults := DefaultCooldownRule()
	if r.SymbolHours <= 0 || r.SymbolHours > 72 {
		r.SymbolHours = defaults.SymbolHours
	}
	if r.SectorLossesPerDay <= 0 || r.SectorLossesPerDay > 10 {
		r.SectorLossesPerDay = defaults.SectorLossesPerDay
	}
	return r
}

// Cooldown 是一条仍然生效的禁买约束。
type Cooldown struct {
	Symbol string `json:"symbol,omitempty"`
	Sector string `json:"sector,omitempty"`
	// Until 是解禁时刻。板块级约束到当日收盘，因此 Until 为当日 15:00。
	Until time.Time `json:"until"`
	// Reason 直接进入拦截提示语，必须写清"哪一笔、什么时候、什么原因"。
	Reason string `json:"reason"`
	// Trigger 是引发冷静期的止损类型，复盘据此区分"止损后追高"与"硬止损后抄底"。
	Trigger string `json:"trigger,omitempty"`
}

// CooldownView 是随快照落库的冷静期清单。
//
// 落进快照而不是只留在内存里：被拦的那笔建议要在三个月后还能回答
// "当时为什么不让我买"——而那时平仓记录可能已经被归档清理了。
type CooldownView struct {
	Symbols map[string]Cooldown `json:"symbols,omitempty"`
	Sectors map[string]Cooldown `json:"sectors,omitempty"`
}

// Empty 表示没有任何生效中的约束。
func (v CooldownView) Empty() bool { return len(v.Symbols) == 0 && len(v.Sectors) == 0 }

// stopTrigger 判断一个离场原因是否属于"止损类"。
// 只收 stop_loss / hard_stop / trail_stop 三种：止盈与模型主动离场后的再买入，
// 是正常策略行为，不该被冷却；把它们也算进来，冷静期会变成"卖出即禁买"。
func stopTrigger(trigger string) bool {
	switch trigger {
	case TriggerStopLoss, TriggerHardStop, TriggerTrailStop:
		return true
	default:
		return false
	}
}

// BuildCooldowns 从已平仓记录推导当前生效的冷静期约束。
//
// now 是判定时刻；返回的 map 只含仍然生效的条目。
func (s *Store) BuildCooldowns(ctx context.Context, accountID string, rule CooldownRule, now time.Time) (CooldownView, error) {
	rule = rule.normalize()
	view := CooldownView{Symbols: map[string]Cooldown{}, Sectors: map[string]Cooldown{}}
	// 只看最近两天的记录：标的级窗口 4 小时、板块级是当日，
	// 更早的止损与当前决策无关，而全表扫描会随交易历史线性变慢。
	since := now.Add(-48 * time.Hour)
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_closed_trades
		WHERE account_id=? AND closed_at>=? ORDER BY closed_at DESC`, accountID, nowText(since))
	if err != nil {
		return view, err
	}
	defer rows.Close()

	day := TradingDay(now)
	sectorLosses := map[string]int{}
	sectorLast := map[string]ClosedTrade{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return view, err
		}
		var trade ClosedTrade
		if err := json.Unmarshal([]byte(payload), &trade); err != nil {
			continue
		}
		if !stopTrigger(trade.Trigger) {
			continue
		}
		// 标的级：只取最近一次，窗口内即禁买。
		until := trade.ClosedAt.Add(time.Duration(rule.SymbolHours * float64(time.Hour)))
		if now.Before(until) {
			if existing, ok := view.Symbols[trade.Symbol]; !ok || existing.Until.Before(until) {
				view.Symbols[trade.Symbol] = Cooldown{
					Symbol:  trade.Symbol,
					Until:   until,
					Trigger: trade.Trigger,
					Reason: fmt.Sprintf("%s 于 %s 因%s离场（%.2f%%），%s 前不再买入同一标的",
						trade.Symbol, trade.ClosedAt.In(cstZone()).Format("01-02 15:04"),
						triggerLabel(trade.Trigger), trade.PnLPercent, until.In(cstZone()).Format("01-02 15:04")),
				}
			}
		}
		// 板块级：只统计当日的止损次数。
		if trade.TradingDay != day || strings.TrimSpace(trade.Sector) == "" {
			continue
		}
		sectorLosses[trade.Sector]++
		if _, ok := sectorLast[trade.Sector]; !ok {
			sectorLast[trade.Sector] = trade
		}
	}
	if err := rows.Err(); err != nil {
		return view, err
	}

	closeAt := sessionCloseOf(now)
	for sector, count := range sectorLosses {
		if count < rule.SectorLossesPerDay {
			continue
		}
		last := sectorLast[sector]
		view.Sectors[sector] = Cooldown{
			Sector:  sector,
			Until:   closeAt,
			Trigger: last.Trigger,
			Reason: fmt.Sprintf("「%s」今日已 %d 笔止损（最近一笔 %s %.2f%%），当日不再买入该行业任何标的",
				sector, count, triggerLabel(last.Trigger), last.PnLPercent),
		}
	}
	return view, nil
}

// sessionCloseOf 返回当天 15:00（北京时间）。收盘后才止损的记录不再产生板块约束，
// 因此这里用"当日最近一个收盘时刻"而不是"下一个"。
func sessionCloseOf(now time.Time) time.Time {
	local := now.In(cstZone())
	return time.Date(local.Year(), local.Month(), local.Day(), 15, 0, 0, 0, cstZone())
}

// Cooldowns 是服务层入口，供计划生成时把约束塞进快照。
func (s *Service) Cooldowns(ctx context.Context, accountID string) (CooldownView, error) {
	view, err := s.store.BuildCooldowns(ctx, s.accountID(accountID), DefaultCooldownRule(), time.Now())
	if err != nil {
		// 冷静期是"额外的保护"，读取失败时返回空视图而不是让整轮计划失败：
		// 因为它拦的是"再买一次"，而不是"该不该卖"。为它阻断整轮会把一个
		// 增益保护变成单点故障。但失败本身要留痕，否则没人知道保护没生效。
		s.logf("event=trading_cooldown_read_failed err=%v", err)
		return CooldownView{}, nil
	}
	return view, nil
}

// Block 判断某个标的在给定时刻是否处于冷静期。
//
// 先查标的、再查行业：标的级是更精确的原因，优先说清"你刚在这只票上止损"，
// 而不是"这个行业今天亏了两笔"——前者更可执行（换票即可），后者更像连坐。
func (v CooldownView) Block(symbol, sector string, now time.Time) (policy.Verdict, bool) {
	if cooldown, ok := v.Symbols[symbol]; ok && now.Before(cooldown.Until) {
		return policy.Verdict{
			Code: policy.CodeRiskCooldownSymbol, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
			Symbol: symbol, Metric: cooldown.Until.Sub(now).Minutes(), Message: cooldown.Reason,
		}, false
	}
	if sector != "" {
		if cooldown, ok := v.Sectors[sector]; ok && now.Before(cooldown.Until) {
			return policy.Verdict{
				Code: policy.CodeRiskCooldownSector, Level: policy.LevelBlock, Scope: policy.ScopePortfolio,
				Symbol: symbol, Metric: cooldown.Until.Sub(now).Minutes(), Message: cooldown.Reason,
			}, false
		}
	}
	return policy.Verdict{}, true
}

// Describe 生成一段给人看的冷静期摘要，用于提示词与界面。
// 只列仍然生效的条目，过期的不该继续占用模型的注意力。
func (v CooldownView) Describe(now time.Time) []string {
	lines := make([]string, 0, len(v.Symbols)+len(v.Sectors))
	for _, cooldown := range v.Symbols {
		if now.Before(cooldown.Until) {
			lines = append(lines, fmt.Sprintf("%s：%s", cooldown.Symbol, cooldown.Reason))
		}
	}
	for _, cooldown := range v.Sectors {
		if now.Before(cooldown.Until) {
			lines = append(lines, fmt.Sprintf("%s：%s", cooldown.Sector, cooldown.Reason))
		}
	}
	sort.Strings(lines)
	return lines
}
