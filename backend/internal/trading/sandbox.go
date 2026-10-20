package trading

// 策略回放沙盘：拿一份历史计划，用另一套风控阈值重新裁决一次。
//
// 它要回答的问题只有一句：如果当时用"激进"而不是"稳健"，结果会差多少。
// 这个问题不能靠改配置重跑一天来回答——行情已经变了，得到的不是同一个问题。
//
// 三条设计约束：
//  1. 纯读。不写订单、不冻结资金、不产生新的裁决记录。
//     沙盘一旦会写库，它就会和真实账本混在一起，之后再没人能分清哪笔是真的。
//  2. 只换阈值，不换输入。行情快照、持仓、现金、模型建议全部沿用当时那一份。
//     这是"控制变量"：结果差异只能来自规则差异，否则无法归因。
//  3. 差异必须逐笔列出。只给一个"总仓位 62% → 71%"没有用，
//     人要看的是"哪一笔被放行了、哪一笔被砍小了"。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/policy"
)

// SandboxResult 是一次沙盘回放的完整产出。
type SandboxResult struct {
	PlanID    string    `json:"plan_id"`
	PlannedAt time.Time `json:"planned_at"`
	// Original 是当时的实际结果（读自历史计划与裁决，不是重新算的）。
	Original SandboxSide `json:"original"`
	// Replayed 是用指定风格重新裁决的结果。
	Replayed SandboxSide `json:"replayed"`
	// Changes 是逐标的差异。空数组表示两种风格结论一致。
	Changes []SandboxChange `json:"changes"`
	Notes   []string        `json:"notes"`
}

// SandboxSide 是一侧的汇总。
type SandboxSide struct {
	Profile string    `json:"profile"`
	Rules   RiskRules `json:"rules"`
	// Accepted / Blocked / Adjusted 是裁决统计，口径与决策页一致。
	Accepted int `json:"accepted"`
	Blocked  int `json:"blocked"`
	Adjusted int `json:"adjusted"`
	// BuyNotional 是通过的买单金额合计，用于回答"这套规则实际会买多少钱"。
	BuyNotional float64 `json:"buy_notional"`
	Exposure    float64 `json:"exposure_percent"`
}

// SandboxChange 是单个标的的差异。
type SandboxChange struct {
	Symbol string `json:"symbol"`
	Name   string `json:"name,omitempty"`
	// Kind 取 added / removed / resized / blocked / kept。
	// added：原规则拒绝、新规则放行；removed：反过来；resized：都放行但股数不同。
	Kind     string `json:"kind"`
	Original string `json:"original"`
	Replayed string `json:"replayed"`
	// Reason 是新旧结论不同时的直接原因（取新规则的裁决消息）。
	Reason string `json:"reason,omitempty"`
}

// SandboxPlan 用指定交易风格重新裁决一份历史计划。
func (s *Service) SandboxPlan(ctx context.Context, planID, profile string) (SandboxResult, error) {
	plan, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return SandboxResult{}, fmt.Errorf("读取计划失败：%w", err)
	}
	if plan.DecisionID == "" {
		return SandboxResult{}, fmt.Errorf("计划 %s 还没有裁决记录，无法回放", planID)
	}
	decision, err := s.store.GetDecision(ctx, plan.DecisionID)
	if err != nil {
		return SandboxResult{}, fmt.Errorf("读取裁决失败：%w", err)
	}
	if len(decision.Proposal) == 0 {
		return SandboxResult{}, fmt.Errorf("计划 %s 的裁决没有模型建议（可能是保护性离场），无从回放", planID)
	}
	if len(plan.Snapshot.Quotes) == 0 {
		return SandboxResult{}, fmt.Errorf("计划 %s 的快照没有行情，无法重算股数与仓位", planID)
	}

	profile = strings.TrimSpace(profile)
	if profile == "" {
		profile = plan.TraderProfile
	}
	// 不认识的风格必须报错。静默回落到"均衡"会给出一个看起来很确定的错误答案：
	// 用户以为自己换成了保守，看到的却是均衡的结果，于是得出"换风格没用"。
	if canonical, ok := canonicalProfile(profile); ok {
		profile = canonical
	} else {
		return SandboxResult{}, fmt.Errorf("未知的交易风格「%s」，可选：steady / balanced / aggressive", profile)
	}
	rules := RulesForProfile(profile)
	result := SandboxResult{
		PlanID:    plan.ID,
		PlannedAt: plan.CreatedAt,
		Changes:   []SandboxChange{},
		Notes:     []string{},
	}

	// 原侧直接读历史订单，不重新计算：重新算会把"当时账本的拒绝（资金不够）"
	// 一起抹掉，而那恰恰是原规则最真实的一部分。
	result.Original = summarizeSandboxSide(plan.TraderProfile, plan.Rules, plan.Orders, plan.Verdicts)
	if result.Original.Rules.MaxOrders == 0 {
		result.Original.Rules = RulesForProfile(plan.TraderProfile)
	}
	// 仓位必须真的算出来：它是沙盘最直接的结论（"激进会把仓位推到 84%"），
	// 留 0 会让人以为两边都没买。
	result.Original.Exposure = sandboxExposure(plan.Snapshot, plan.Orders)

	// 重放：同一份快照、同一份模型建议，只换阈值。
	// 时间用当时的裁决时间：冷静期、失效条件都以那一刻为准，
	// 用 now 会让"当时在冷静期内"的单在回放里变成可买。
	now := decision.TakenAt
	if now.IsZero() {
		now = plan.CreatedAt
	}
	draft := DraftPlan{
		Regime:    decision.ModelRegime,
		Summary:   decision.ModelSummary,
		RiskNotes: decision.RiskNotes,
		Orders:    decision.Proposal,
	}
	replayedOrders, replayedVerdicts := BuildOrders(plan.AccountID, draft, plan.Snapshot, rules, now)
	result.Replayed = summarizeSandboxSide(profile, rules, replayedOrders, replayedVerdicts)
	result.Replayed.Exposure = sandboxExposure(plan.Snapshot, replayedOrders)

	result.Changes = diffSandbox(plan.Orders, replayedOrders, replayedVerdicts)
	if len(result.Changes) == 0 {
		result.Notes = append(result.Notes, "两种规则下逐笔结论一致：差异只体现在额度余量，不体现在动作上")
	}
	// 只在真正可比时提示市场状态：市场恐慌/未知时，两套阈值都会被同一道闸门拦掉，
	// 用户会误以为"换风格没用"，实际是市场层先否了。
	if plan.Snapshot.Market.Evaluated {
		switch plan.Snapshot.Market.Regime {
		case "panic", "unknown":
			result.Notes = append(result.Notes, fmt.Sprintf(
				"当时市场状态为「%s」，市场层本身就会禁止开新仓；换风格改变不了这一层结论", plan.Snapshot.Market.Label))
		}
	}
	if !plan.Snapshot.Cooldowns.Empty() {
		result.Notes = append(result.Notes, fmt.Sprintf("快照内含 %d 条止损冷静期约束，回放同样生效",
			len(plan.Snapshot.Cooldowns.Symbols)+len(plan.Snapshot.Cooldowns.Sectors)))
	}
	return result, nil
}

func summarizeSandboxSide(profile string, rules RiskRules, orders []Order, verdicts []policy.Verdict) SandboxSide {
	side := SandboxSide{Profile: profile, Rules: rules.Normalize()}
	side.Adjusted = policy.Summarize(verdicts).Adjusted
	for _, order := range orders {
		switch order.Status {
		case OrderStatusRejected:
			side.Blocked++
		case OrderStatusCancelled, OrderStatusExpired:
			// 撤单/过期既不是"放行后买入"，也不是"被规则拦下"。
			// 混进任何一边都会让沙盘的两侧口径不一致。
		default:
			// pending / partially_filled / filled 都算放行：
			// 已成交的历史计划如果只认挂在单，会显示"当时一笔没买"，
			// 而实际账本上已经买进去了——那是最容易误导人的一种错。
			side.Accepted++
			if order.Side == SideBuy {
				quantity := order.Quantity
				if order.Status == OrderStatusFilled && order.FilledQuantity > 0 {
					quantity = order.FilledQuantity
				}
				side.BuyNotional += float64(quantity) * order.LimitPrice
			}
		}
	}
	side.BuyNotional = round2(side.BuyNotional)
	return side
}

// sandboxExposure 算"这些单全部成交后的总仓位"。
//
// 用 equity - cash 取持仓市值，而不是累加 positions：老计划的快照可能只存了
// 一部分字段，而 equity 与 cash 在每份快照里都必然有值且自洽。
func sandboxExposure(snapshot Snapshot, orders []Order) float64 {
	if snapshot.Equity <= 0 {
		return 0
	}
	value := snapshot.Equity - snapshot.Cash
	if value < 0 {
		value = 0
	}
	for _, order := range orders {
		if order.Status == OrderStatusRejected || order.Status == OrderStatusCancelled || order.Status == OrderStatusExpired {
			continue
		}
		quantity := order.Quantity
		if order.Status == OrderStatusFilled && order.FilledQuantity > 0 {
			quantity = order.FilledQuantity
		}
		notional := float64(quantity) * order.LimitPrice
		if order.Side == SideBuy {
			value += notional
		} else {
			value -= notional
		}
	}
	if value < 0 {
		value = 0
	}
	return round2(value / snapshot.Equity * 100)
}

// canonicalProfile 把风格名归一到三个合法值。
func canonicalProfile(profile string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "aggressive", "激进":
		return "aggressive", true
	case "steady", "稳健":
		return "steady", true
	case "balanced", "均衡":
		return "balanced", true
	default:
		return "", false
	}
}

// diffSandbox 逐标的比较两侧的最终状态。
//
// 以"股数与状态"为比较维度，而不是比较裁决消息：消息里含数字，
// 阈值一变所有消息都会变，用它比较会得到"每一笔都不同"的假结论。
func diffSandbox(original, replayed []Order, replayedVerdicts []policy.Verdict) []SandboxChange {
	index := func(orders []Order) map[string]Order {
		out := make(map[string]Order, len(orders))
		for _, order := range orders {
			out[order.Symbol] = order
		}
		return out
	}
	before := index(original)
	after := index(replayed)
	blocks := map[string]string{}
	adjusts := map[string]string{}
	for _, verdict := range replayedVerdicts {
		if verdict.Symbol == "" {
			continue
		}
		target := adjusts
		if verdict.Level == policy.LevelBlock {
			target = blocks
		} else if verdict.Level != policy.LevelAdjust {
			continue
		}
		if _, ok := target[verdict.Symbol]; !ok {
			target[verdict.Symbol] = verdict.Code + "：" + verdict.Message
		}
	}
	symbols := make([]string, 0, len(before)+len(after))
	seen := map[string]bool{}
	for _, order := range append(append([]Order{}, original...), replayed...) {
		if order.Symbol == "" || seen[order.Symbol] {
			continue
		}
		seen[order.Symbol] = true
		symbols = append(symbols, order.Symbol)
	}
	changes := make([]SandboxChange, 0, len(symbols))
	for _, symbol := range symbols {
		oldOrder, hadOld := before[symbol]
		newOrder, hasNew := after[symbol]
		kind := ""
		switch {
		case !hadOld && hasNew:
			kind = "added"
		case hadOld && !hasNew:
			kind = "removed"
		case hadOld && hasNew:
			oldActive := sandboxAllowed(oldOrder)
			newActive := sandboxAllowed(newOrder)
			switch {
			case oldActive && !newActive:
				kind = "removed"
			case !oldActive && newActive:
				kind = "added"
			case oldActive && newActive && oldOrder.Quantity != newOrder.Quantity:
				kind = "resized"
			default:
				kind = "kept"
			}
		}
		if kind == "kept" {
			continue
		}
		name := firstNonEmpty(newOrder.Name, oldOrder.Name)
		// 差异必须自带原因，否则用户只能自己猜"为什么少了 100 股"。
		// resized 优先用调整类裁决（定价/资金缩减），removed 优先用拦截裁决；
		// 两边都取不到时，至少把权重变化写出来。
		var reason string
		if kind == "resized" {
			reason = firstNonEmpty(adjusts[symbol], blocks[symbol],
				fmt.Sprintf("重算后目标权重 %.2f%% → %.2f%%", oldOrder.TargetWeightPercent, newOrder.TargetWeightPercent))
		} else {
			reason = firstNonEmpty(blocks[symbol], adjusts[symbol])
		}
		changes = append(changes, SandboxChange{
			Symbol:   symbol,
			Name:     name,
			Kind:     kind,
			Original: describeSandboxOrder(oldOrder, hadOld),
			Replayed: describeSandboxOrder(newOrder, hasNew),
			Reason:   reason,
		})
	}
	return changes
}

// sandboxAllowed 判断一笔订单在沙盘口径下算不算"放行"。
func sandboxAllowed(order Order) bool {
	switch order.Status {
	case OrderStatusRejected, OrderStatusCancelled, OrderStatusExpired:
		return false
	default:
		return true
	}
}

func describeSandboxOrder(order Order, exists bool) string {
	if !exists {
		return "未提交该笔建议"
	}
	if order.Status == OrderStatusRejected {
		code := firstNonEmpty(order.RejectCode, "被拒")
		return fmt.Sprintf("被拒（%s）", code)
	}
	return fmt.Sprintf("%s %d 股 @%.2f（目标 %.2f%%）", sideLabel(order.Side), order.Quantity, order.LimitPrice, order.TargetWeightPercent)
}
