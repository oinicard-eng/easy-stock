package trading

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
)

// 权限链词汇。借自多智能体编排（PROPOSE → EVIDENCE → VETO → DECIDE → EXECUTE），
// 但这里不是五个模型，而是五个"谁有权做这件事"的槽位：
//
//	PROPOSE  模型提出建议（唯一的 LLM 环节）
//	EVIDENCE 模型用只读工具查到的事实（调研记录）
//	VETO     代码拥有的否决权（守卫拦截、总闸、回撤）
//	DECIDE   代码在允许范围内决定数量与权重
//	EXECUTE  账本把决定变成挂单、成交与冻结
//
// 把它们写进报告，是为了让"这条结论是谁下的"不再需要靠人猜。
const (
	RolePropose  = "PROPOSE"
	RoleEvidence = "EVIDENCE"
	RoleVeto     = "VETO"
	RoleDecide   = "DECIDE"
	RoleExecute  = "EXECUTE"
)

// 复核条目的结论档位。pass 是新增的一档：多智能体报告里通常只记问题，
// 但"检查过且没问题"同样是一条结论——没有它，人无法区分"没检查"和"检查了没事"。
const (
	ReviewPass  = "pass"
	ReviewWarn  = "warn"
	ReviewBlock = "block"
)

// RiskReviewItem 是一条复核结论。Role 说明谁有权下这个结论，
// Code 与裁决代码同源，便于和 decisions 表对齐。
type RiskReviewItem struct {
	Role    string  `json:"role"`
	Code    string  `json:"code"`
	Level   string  `json:"level"`
	Message string  `json:"message"`
	Metric  float64 `json:"metric,omitempty"`
	Limit   float64 `json:"limit,omitempty"`
}

// PlanRiskReview 是"计划级"的风险复核报告。
//
// 它与逐笔裁决的区别：裁决回答"这笔单为什么被拒"，复核回答
// "这一整套方案整体上安全吗"。后者不能只看单笔——六笔各 15% 全部合规，
// 合起来却压在同一个行业、同一种风格上，只有组合视角能看出来。
//
// 它是一份报告，不是第二道闸门：不改订单、不改状态、不产生冻结。
// 一旦它开始否决，就会与守卫形成两套口径，复盘时无法判断哪一套生效了。
type PlanRiskReview struct {
	// Verdict 是整份方案的结论：任一 block 即 block，任一 warn 即 warn。
	Verdict    string           `json:"verdict"`
	ReviewedAt time.Time        `json:"reviewed_at"`
	Items      []RiskReviewItem `json:"items"`
	// Summary 是给人读的几行数字，每条都能对回上面某个 Item。
	Summary []string     `json:"summary"`
	Stats   policy.Stats `json:"stats"`
}

// RiskReviewInput 是复核的全部输入。全部来自确定性来源，不含模型文本。
type RiskReviewInput struct {
	Snapshot Snapshot
	Rules    RiskRules
	Control  Control
	Decision Decision
	Orders   []Order
	Risk     RiskState
	Research *ResearchResult
	Now      time.Time
}

// BuildPlanRiskReview 生成计划级风险复核。
func BuildPlanRiskReview(in RiskReviewInput) PlanRiskReview {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rules := in.Rules.Normalize()
	review := PlanRiskReview{ReviewedAt: now, Items: []RiskReviewItem{}, Stats: in.Decision.Stats}
	add := func(item RiskReviewItem) { review.Items = append(review.Items, item) }

	// PROPOSE：模型提了什么。它本身不是风险，但后面所有比例都要以它为分母。
	add(RiskReviewItem{
		Role: RolePropose, Code: "review.proposal", Level: ReviewPass,
		Message: fmt.Sprintf("模型建议 %d 笔，市场判断「%s」", len(in.Decision.Proposal), fallback(in.Decision.ModelRegime, "未标注")),
		Metric:  float64(len(in.Decision.Proposal)),
	})

	// EVIDENCE：调研是否真的发生过。降级的调研不是错误，但必须让人知道
	// "这次判断是在没有查到日线/资金流的情况下做出的"。
	if in.Research != nil {
		level := ReviewPass
		okSteps := 0
		for _, step := range in.Research.Steps {
			if step.Error == "" {
				okSteps++
			}
		}
		message := fmt.Sprintf("调研 %d 步（成功 %d 步）", len(in.Research.Steps), okSteps)
		if in.Research.Conclusion != "" {
			message += "：" + truncateText(in.Research.Conclusion, 60)
		}
		if !in.Research.Enabled {
			level = ReviewWarn
			message = "本轮未完成调研：仅依据行情快照判断"
		}
		if in.Research.Degraded {
			level = ReviewWarn
			message = "调研降级：" + fallback(in.Research.Note, "未能完成工具调研，本次判断仅基于快照")
		}
		add(RiskReviewItem{Role: RoleEvidence, Code: "review.research", Level: level,
			Message: message, Metric: float64(okSteps)})
	}

	// VETO 1：账户总闸。它是唯一能整轮否决的开关。
	switch {
	case in.Control.KillSwitch:
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeGateKillSwitch, Level: ReviewBlock,
			Message: "账户已全部停止（kill switch），本轮不产生任何订单"})
	case in.Control.PauseBuying:
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeGatePauseBuying, Level: ReviewWarn,
			Message: "账户暂停买入：" + fallback(in.Control.Reason, "仅允许卖出与减仓")})
	default:
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeGatePauseBuying, Level: ReviewPass, Message: "账户总闸正常，允许开仓"})
	}

	// VETO 2：组合回撤。与日亏并列，因为它们抓的是两种不同的亏损形态。
	if in.Risk.Level != DrawdownLevelNone {
		level := ReviewWarn
		if in.Risk.Level == DrawdownLevelStop {
			level = ReviewBlock
		}
		add(RiskReviewItem{Role: RoleVeto, Code: "risk.drawdown", Level: level,
			Message: fmt.Sprintf("净值回撤 %.2f%%（高点 %.2f → 现值 %.2f），%s",
				in.Risk.DrawdownPercent, in.Risk.PeakEquity, in.Risk.Equity, drawdownLevelLabel(in.Risk.Level)),
			Metric: in.Risk.DrawdownPercent, Limit: drawdownThreshold(in.Risk)})
	} else {
		add(RiskReviewItem{Role: RoleVeto, Code: "risk.drawdown", Level: ReviewPass,
			Message: fmt.Sprintf("净值回撤 %.2f%%，距警戒线 %.2f%% 有余量",
				in.Risk.DrawdownPercent, in.Risk.Policy.WarnPercent),
			Metric: in.Risk.DrawdownPercent, Limit: in.Risk.Policy.WarnPercent})
	}

	// VETO 3：市场状态。看不清或恐慌时不允许开新仓。
	if in.Snapshot.Market.Evaluated {
		switch in.Snapshot.Market.Regime {
		case cognition.RegimeUnknown, cognition.RegimePanic:
			add(RiskReviewItem{Role: RoleVeto, Code: "risk.market", Level: ReviewBlock,
				Message: fmt.Sprintf("市场状态「%s」，本轮禁止开新仓", in.Snapshot.Market.Label)})
		default:
			add(RiskReviewItem{Role: RoleVeto, Code: "risk.market", Level: ReviewPass,
				Message: fmt.Sprintf("市场状态「%s」，风险预算 %.0f%%", in.Snapshot.Market.Label, in.Snapshot.Market.RiskBudgetPercent)})
		}
	}

	// 下面四条都要基于"本轮真实下单"，而不是模型建议：
	// 被守卫缩量或拒绝的建议不会占用任何额度，用它们算比例会得出"超限"的假结论。
	buys := pendingBuys(in.Orders)
	equity := in.Snapshot.Equity
	maxWeight, maxWeightSymbol := 0.0, ""
	maxRisk, maxRiskSymbol := 0.0, ""
	missingStop, missingStopSymbols := 0, []string{}
	for _, order := range buys {
		weight := order.TargetWeightPercent
		if weight <= 0 && equity > 0 {
			weight = float64(order.Quantity) * order.LimitPrice / equity * 100
		}
		if weight > maxWeight {
			maxWeight, maxWeightSymbol = weight, order.Symbol
		}
		if order.StopLossPrice <= 0 || order.LimitPrice <= 0 {
			missingStop++
			missingStopSymbols = append(missingStopSymbols, order.Symbol)
			continue
		}
		if equity > 0 {
			risk := float64(order.Quantity) * (order.LimitPrice - order.StopLossPrice) / equity * 100
			if risk > maxRisk {
				maxRisk, maxRiskSymbol = round2(risk), order.Symbol
			}
		}
	}
	if len(buys) > 0 {
		// VETO 4：止损覆盖。没有止损的买入等于把风险敞口交给行情决定。
		level := ReviewPass
		message := fmt.Sprintf("%d 笔买入全部带止损", len(buys))
		if missingStop > 0 {
			level = ReviewBlock
			message = fmt.Sprintf("%d 笔买入缺少有效止损（%s）", missingStop, strings.Join(missingStopSymbols, "、"))
		}
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeRiskStopLossMissing, Level: level,
			Message: message, Metric: float64(missingStop), Limit: 0})

		// VETO 5：单票上限。
		level = ReviewPass
		message = fmt.Sprintf("最大单票 %s %.2f%%（上限 %.2f%%）", maxWeightSymbol, maxWeight, rules.MaxSingleWeightPercent)
		if maxWeight > rules.MaxSingleWeightPercent {
			level = ReviewBlock
		} else if maxWeight > rules.MaxSingleWeightPercent*0.8 {
			level = ReviewWarn
			message = fmt.Sprintf("最大单票 %s %.2f%%，已用掉上限 %.2f%% 的 %.0f%%",
				maxWeightSymbol, maxWeight, rules.MaxSingleWeightPercent,
				maxWeight/rules.MaxSingleWeightPercent*100)
		}
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeRiskSingleName, Level: level,
			Message: message, Metric: round2(maxWeight), Limit: rules.MaxSingleWeightPercent})

		// DECIDE 1：单笔风险预算。
		level = ReviewPass
		message = fmt.Sprintf("最大单笔风险 %s %.2f%%（上限 %.2f%%）", maxRiskSymbol, maxRisk, rules.MaxRiskPerTradePercent)
		if maxRisk > rules.MaxRiskPerTradePercent {
			level = ReviewBlock
		} else if maxRisk > rules.MaxRiskPerTradePercent*0.8 {
			level = ReviewWarn
		}
		add(RiskReviewItem{Role: RoleDecide, Code: policy.CodeRiskTradeRisk,
			Level: level, Message: message, Metric: maxRisk, Limit: rules.MaxRiskPerTradePercent})
	}

	// VETO 6：行业集中度。六只票同属一个行业时，单票合规不代表组合合规。
	if len(in.Decision.Exposure.Sectors) > 0 {
		top := in.Decision.Exposure.Sectors[0]
		level := ReviewPass
		message := fmt.Sprintf("最重行业 %s %.2f%% / %d 只（上限 %.2f%% / %d 只）",
			top.Sector, top.Weight, top.Names, rules.MaxSectorWeightPercent, rules.MaxSectorNames)
		if top.Weight > rules.MaxSectorWeightPercent || top.Names > rules.MaxSectorNames {
			level = ReviewBlock
		} else if top.Weight > rules.MaxSectorWeightPercent*0.8 {
			level = ReviewWarn
		}
		add(RiskReviewItem{Role: RoleVeto, Code: policy.CodeRiskSectorWeight, Level: level,
			Message: message, Metric: top.Weight, Limit: rules.MaxSectorWeightPercent})
	}

	// DECIDE 2：总仓位与预算。
	budget := in.Decision.Exposure.BudgetPercent
	after := in.Decision.Exposure.AfterPercent
	level := ReviewPass
	message := fmt.Sprintf("买入后仓位 %.2f%%（预算上限 %.2f%%）", after, budget)
	switch {
	case budget > 0 && after > budget:
		level = ReviewBlock
	case budget > 0 && after > budget*0.9:
		level = ReviewWarn
	}
	add(RiskReviewItem{Role: RoleDecide, Code: policy.CodeRiskExposure, Level: level,
		Message: message, Metric: after, Limit: budget})

	// DECIDE 3：现金底线。
	level = ReviewPass
	message = fmt.Sprintf("决策后现金 %.2f%%（底线 %.2f%%）", in.Decision.Exposure.CashPercent, rules.MinCashPercent)
	if in.Decision.Exposure.CashPercent < rules.MinCashPercent {
		level = ReviewWarn
	}
	add(RiskReviewItem{Role: RoleDecide, Code: policy.CodeRiskCashFloor, Level: level,
		Message: message, Metric: in.Decision.Exposure.CashPercent, Limit: rules.MinCashPercent})

	// EXECUTE：最终落到账本上的结果。这是唯一的既成事实，
	// 前面的所有比例都只是预期。
	add(RiskReviewItem{Role: RoleExecute, Code: "review.execution", Level: ReviewPass,
		Message: fmt.Sprintf("通过 %d 笔、拦截 %d 笔、调整 %d 处，冻结资金 %.2f 元",
			in.Decision.Stats.Accepted, in.Decision.Stats.Blocked, in.Decision.Stats.Adjusted, in.Decision.Exposure.BuyNotional),
		Metric: float64(in.Decision.Stats.Accepted)})

	review.Verdict = ReviewPass
	for _, item := range review.Items {
		if item.Level == ReviewBlock {
			review.Verdict = ReviewBlock
			break
		}
		if item.Level == ReviewWarn {
			review.Verdict = ReviewWarn
		}
	}
	review.Summary = summarizeRiskReview(review, in)
	return review
}

// summarizeRiskReview 把条目压成几行数字。排序固定：先结论，再额度，最后执行。
func summarizeRiskReview(review PlanRiskReview, in RiskReviewInput) []string {
	lines := make([]string, 0, 6)
	verdict := "通过"
	switch review.Verdict {
	case ReviewBlock:
		verdict = "存在否决项"
	case ReviewWarn:
		verdict = "存在需要留意的项"
	}
	lines = append(lines, fmt.Sprintf("复核结论：%s（%d 项检查，%d 项拦截）",
		verdict, len(review.Items), countReviewLevel(review.Items, ReviewBlock)))
	if in.Risk.PeakEquity > 0 {
		lines = append(lines, fmt.Sprintf("净值高点 %.2f、现值 %.2f，回撤 %.2f%%",
			in.Risk.PeakEquity, in.Risk.Equity, in.Risk.DrawdownPercent))
	}
	// 复用裁决摘要里的数字口径，避免复核与裁决各说一套。
	for _, item := range review.Items {
		if item.Code == policy.CodeRiskExposure || item.Code == policy.CodeRiskCashFloor ||
			item.Code == policy.CodeRiskSectorWeight {
			lines = append(lines, item.Message)
		}
	}
	if review.Stats.Blocked > 0 {
		lines = append(lines, fmt.Sprintf("拦截 %d 笔（明细见裁决流水）", review.Stats.Blocked))
	}
	return lines
}

func countReviewLevel(items []RiskReviewItem, level string) int {
	count := 0
	for _, item := range items {
		if item.Level == level {
			count++
		}
	}
	return count
}

// pendingBuys 返回仍可执行的买单，按标的排序保证报告可复现。
func pendingBuys(orders []Order) []Order {
	out := make([]Order, 0, len(orders))
	for _, order := range orders {
		if order.Status == OrderStatusPending && order.Side == SideBuy {
			out = append(out, order)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func drawdownThreshold(state RiskState) float64 {
	if state.Level == DrawdownLevelStop {
		return state.Policy.StopPercent
	}
	return state.Policy.WarnPercent
}
