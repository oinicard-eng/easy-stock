package trading

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/policy"
)

// 本文件是「持仓防守线」。它与模型无关：触发条件是确定的数字比较，
// 结论也是一定的——到了止损价就卖，不讨论、不等下一轮、不给模型再想一次的机会。
//
// 为什么必须独立于模型：
//  1. 模型可能超时、可能返回空建议、可能"再看看"。止损不能依赖一次成功的 API 调用。
//  2. 止损是承诺。买入时写下 -7%，到 -7% 不执行，那条止损线就只是装饰。
//  3. 复盘需要干净的因果：亏在止损上，说明入场点或止损位设错了；
//     亏在"没止损"上，说明系统有病。两者混在一起永远修不好。
//
// 但保护性卖出仍然走同一本账、同一套冻结、同一条裁决流水：
// 它不是特权通道，只是决策来源不同（Source=protective）。

// GuardConfig 是防守线的数值参数。
type GuardConfig struct {
	// TrailPercent 是移动止损的回撤幅度：从持仓期最高价回撤这么多就离场。
	TrailPercent float64
	// TrailArmPercent 是移动止损的启动门槛：浮盈没到这个数之前不启用。
	// 没有它，刚买入的一次正常波动就会被当成"从高点回撤"清仓。
	TrailArmPercent float64
	// TakeProfitLadder 是分级止盈的比例（占可卖股数），例如 [40, 40]。
	// 剩余部分交给移动止损，让盈利单有机会跑得更远。
	TakeProfitLadder []float64
	// MaxOrders 是单轮最多生成多少笔保护性卖单，防止异常行情下一次性刷出几十笔。
	MaxOrders int
}

// DefaultGuardConfig 返回一套保守参数。
func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		TrailPercent:     6,
		TrailArmPercent:  4,
		TakeProfitLadder: []float64{40, 40},
		MaxOrders:        8,
	}
}

func (c GuardConfig) normalize() GuardConfig {
	defaults := DefaultGuardConfig()
	if c.TrailPercent <= 0 || c.TrailPercent > 30 {
		c.TrailPercent = defaults.TrailPercent
	}
	if c.TrailArmPercent < 0 || c.TrailArmPercent > 30 {
		c.TrailArmPercent = defaults.TrailArmPercent
	}
	if len(c.TakeProfitLadder) == 0 {
		c.TakeProfitLadder = defaults.TakeProfitLadder
	}
	if c.MaxOrders <= 0 || c.MaxOrders > 50 {
		c.MaxOrders = defaults.MaxOrders
	}
	return c
}

// GuardResult 是一次防守线检查的产出。
type GuardResult struct {
	Orders   []Order
	Verdicts []policy.Verdict
	// Held 是被检查过的持仓数，用于区分"没有持仓"与"有持仓但都没触发"。
	Held int
}

// BuildProtectiveOrders 对每个持仓做一次防守线判断，返回应当立即执行的卖单。
//
// 判断顺序不可交换，且一旦命中就跳到下一个标的：
// 止损优先于止盈（同一时刻既破止损又达止盈只可能是数据异常，此时按最坏情况处理），
// 硬止损优先于移动止损（硬止损是买入时就承诺的，不能被"还没回撤够"覆盖）。
func BuildProtectiveOrders(snapshot Snapshot, rules RiskRules, config GuardConfig, now time.Time) GuardResult {
	rules = rules.Normalize()
	config = config.normalize()
	result := GuardResult{Orders: []Order{}, Verdicts: []policy.Verdict{}}

	quotes := map[string]float64{}
	for _, quote := range snapshot.Quotes {
		if quote.Price > 0 {
			quotes[quote.Symbol] = quote.Price
		}
	}

	positions := make([]Position, 0, len(snapshot.Positions))
	for _, position := range snapshot.Positions {
		if position.Quantity > 0 {
			positions = append(positions, position)
		}
	}
	// 按浮亏从大到小检查：一轮只能发有限笔单时，先处理亏得最多的那些。
	sort.SliceStable(positions, func(i, j int) bool {
		return positions[i].ProfitPercent() < positions[j].ProfitPercent()
	})
	result.Held = len(positions)

	for _, position := range positions {
		if len(result.Orders) >= config.MaxOrders {
			result.Verdicts = append(result.Verdicts, policy.Verdict{
				Code: policy.CodeOrderCountCap, Level: policy.LevelAdjust, Scope: policy.ScopePortfolio,
				Field: "orders", Metric: float64(len(result.Orders)), Limit: float64(config.MaxOrders),
				Message: fmt.Sprintf("保护性卖出已达单轮上限 %d 笔，其余持仓留到下一轮处理", config.MaxOrders),
			})
			break
		}
		price := quotes[position.Symbol]
		if price <= 0 {
			// 没有有效行情就不做判断。用成本价代替现价会让止损永远不触发，
			// 用 0 代替则会让每只票都"跌破止损"——两种都是编造事实。
			continue
		}
		heldDays := holdingDays(position.OpenedAt, now)
		trigger, fraction, verdict := evaluatePosition(position, price, heldDays, rules, config)
		if trigger == TriggerNoTrigger {
			continue
		}
		if position.Sellable() < MinBoardLot(position.Symbol) {
			result.Verdicts = append(result.Verdicts, policy.Verdict{
				Code: policy.CodeGuardNoSellable, Level: policy.LevelWarn, Scope: policy.ScopePosition,
				Symbol: position.Symbol, Metric: float64(position.Sellable()),
				Message: fmt.Sprintf("%s 已触发%s，但可卖 %d 股不足一手（已解禁 %d 股、已挂卖单占用 %d 股），本轮无法执行",
					position.Symbol, triggerLabel(trigger), position.Sellable(), position.Available, position.ReservedQuantity),
			})
			continue
		}

		lot := MinBoardLot(position.Symbol)
		quantity := roundSharesDown(int(float64(position.Sellable())*fraction/100), lot)
		if quantity < lot {
			quantity = roundSharesDown(position.Sellable(), lot)
		}
		if quantity <= 0 {
			continue
		}
		order := Order{
			ID:              newID("ord"),
			AccountID:       position.AccountID,
			Symbol:          position.Symbol,
			Name:            position.Name,
			Sector:          position.Sector,
			Side:            SideSell,
			OrderType:       OrderTypeMarket,
			Quantity:        quantity,
			LimitPrice:      protectiveSellPrice(position.Symbol, position.Name, price),
			Source:          OrderSourceProtective,
			Trigger:         trigger,
			StopLossPrice:   position.StopLossPrice,
			TakeProfitPrice: position.TakeProfitPrice,
			// 保护性卖出没有"模型信心"这一说，留 0 而不是编一个 1.0。
			Confidence: 0,
			Thesis:     verdict.Message,
			Status:     OrderStatusPending,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		result.Orders = append(result.Orders, order)
		result.Verdicts = append(result.Verdicts, verdict)
	}
	return result
}

// evaluatePosition 判断单个持仓该不该离场，返回触发原因与卖出比例（占可卖股数，0–100）。
//
// 判断顺序即优先级，不可交换：止损（承诺）→ 硬止损（兜底）→ 失效条件（论点弱）
// → 止盈（计划内兑现）→ 移动止损（保住利润）→ 时间止损（期限到了）。
// 前两条是清仓，中间两条是减仓，最后两条按情形定力度——力度差异写在这里，
// 因为"止损"和"失效"如果都用清仓执行，模型写下的失效条件就变成了第二个止损。
func evaluatePosition(position Position, price float64, heldDays int, rules RiskRules, config GuardConfig) (string, float64, policy.Verdict) {
	profit := 0.0
	if position.AverageCost > 0 {
		profit = (price/position.AverageCost - 1) * 100
	}

	// 1. 显式止损线：买入时写下的承诺。
	if position.StopLossPrice > 0 && price <= position.StopLossPrice {
		return TriggerStopLoss, 100, policy.Verdict{
			Code: policy.CodeGuardStopLoss, Level: policy.LevelBlock, Scope: policy.ScopePosition,
			Symbol: position.Symbol, Metric: round2(profit), Limit: round2((position.StopLossPrice/position.AverageCost - 1) * 100),
			Message: fmt.Sprintf("%s 现价 %.2f 触及止损线 %.2f（浮亏 %+.2f%%，成本 %.2f），清仓",
				position.Symbol, price, position.StopLossPrice, profit, position.AverageCost),
		}
	}

	// 2. 硬止损兜底：模型没给止损线（或给了不合理的线）时，账户层必须有自己的底线。
	hardStop := -rules.MaxStopLossPercent
	if hardStop < -0.01 && profit <= hardStop {
		return TriggerHardStop, 100, policy.Verdict{
			Code: policy.CodeGuardHardStop, Level: policy.LevelBlock, Scope: policy.ScopePosition,
			Symbol: position.Symbol, Metric: round2(profit), Limit: hardStop,
			Message: fmt.Sprintf("%s 浮亏 %+.2f%% 超过账户硬止损 %.1f%%（该持仓未设有效止损线），清仓",
				position.Symbol, profit, rules.MaxStopLossPercent),
		}
	}

	// 3. 失效条件：买入时写下的"什么情况下这笔判断不成立"。
	// 只减仓不清仓——这是它与止损的根本区别；同一持仓只自动执行一次，
	// 否则一次误判会在连续几轮里把仓位砍到零。
	if position.InvalidationRule != nil && position.AutoReduceAt == nil {
		if hit, reason := position.InvalidationRule.Triggered(price, heldDays); hit {
			return TriggerInvalidation, invalidationReducePercent, policy.Verdict{
				Code: policy.CodeGuardInvalidation, Level: policy.LevelAdjust, Scope: policy.ScopePosition,
				Symbol: position.Symbol, Metric: round2(profit),
				Message: fmt.Sprintf("%s 失效条件成立：%s（浮盈 %+.2f%%），减仓 %d%% 而非清仓",
					position.Symbol, reason, profit, invalidationReducePercent),
			}
		}
	}

	// 4. 分级止盈：先兑现一部分，剩余交给移动止损。
	if position.TakeProfitPrice > 0 && price >= position.TakeProfitPrice {
		level := position.ReduceCount
		if level < len(config.TakeProfitLadder) {
			fraction := config.TakeProfitLadder[level]
			if fraction <= 0 || fraction > 100 {
				fraction = 50
			}
			return TakeProfitTrigger(level), fraction, policy.Verdict{
				Code: policy.CodeGuardTakeProfit, Level: policy.LevelAdjust, Scope: policy.ScopePosition,
				Symbol: position.Symbol, Metric: round2(profit), Limit: position.TakeProfitPrice,
				Message: fmt.Sprintf("%s 现价 %.2f 达到止盈线 %.2f（浮盈 %+.2f%%），第 %d 级止盈卖出 %.0f%%",
					position.Symbol, price, position.TakeProfitPrice, profit, level+1, fraction),
			}
		}
	}

	// 5. 移动止损：已经在盈利，但从最高点回撤了足够多。
	if position.PeakPrice > 0 && profit >= config.TrailArmPercent {
		drawdown := (price/position.PeakPrice - 1) * 100
		if drawdown <= -config.TrailPercent {
			return TriggerTrailStop, 100, policy.Verdict{
				Code: policy.CodeGuardTrailStop, Level: policy.LevelBlock, Scope: policy.ScopePosition,
				Symbol: position.Symbol, Metric: round2(drawdown), Limit: -config.TrailPercent,
				Message: fmt.Sprintf("%s 从最高 %.2f 回撤 %.2f%% 至 %.2f（浮盈仍 %+.2f%%），移动止损离场",
					position.Symbol, position.PeakPrice, drawdown, price, profit),
			}
		}
	}

	// 6. 时间止损：超过预期兑现期限，价格既没跌到止损、也没涨到止盈。
	// 这是"钱被占着不干活"的唯一解——不设它，一个横盘三个月的仓位会永远占着额度。
	if position.ExpectedDays > 0 && position.AutoReduceAt == nil {
		if limit := deadlineNaturalDays(position.ExpectedDays); limit > 0 && heldDays > limit {
			switch {
			case profit < 0:
				return TriggerTimeStop, 100, policy.Verdict{
					Code: policy.CodeGuardTimeStop, Level: policy.LevelBlock, Scope: policy.ScopePosition,
					Symbol: position.Symbol, Metric: round2(profit),
					Message: fmt.Sprintf("%s 持有 %d 天超过预期 %d 个交易日且浮亏 %+.2f%%，时间止损离场",
						position.Symbol, heldDays, position.ExpectedDays, profit),
				}
			case profit < timeStopMinProfitPercent:
				return TriggerTimeStop, 50, policy.Verdict{
					Code: policy.CodeGuardTimeStop, Level: policy.LevelAdjust, Scope: policy.ScopePosition,
					Symbol: position.Symbol, Metric: round2(profit),
					Message: fmt.Sprintf("%s 持有 %d 天超过预期 %d 个交易日，浮盈仅 %+.2f%%，减仓 50%% 释放额度",
						position.Symbol, heldDays, position.ExpectedDays, profit),
				}
			}
		}
	}
	return TriggerNoTrigger, 0, policy.Verdict{}
}

const (
	// invalidationReducePercent 是失效条件触发时的减仓比例。
	// 取 50% 而不是 100%：失效条件写错的概率远高于止损线写错，
	// 一次误判只该减半，剩下的交给人或止损。
	invalidationReducePercent = 50
	// timeStopMinProfitPercent 是"超期但还算及格"的浮盈线。
	// 低于它说明判断既没错到该止损、也没对到值得继续占额度。
	timeStopMinProfitPercent = 2.0
)

// TakeProfitTrigger 返回第 level 级（从 0 开始）止盈的触发标识。
// 分级用同一个 Trigger 前缀 + 级别，复盘按前缀聚合，按级别看细节。
func TakeProfitTrigger(level int) string {
	return fmt.Sprintf("%s_%d", TriggerTakeProfit, level+1)
}

// protectiveSellPrice 给市价卖单一个保护限价：允许滑价 2%，但不越过跌停。
// 保护性卖出宁可少卖几个点，也不能因为"限价太高"挂在场上等到收盘。
func protectiveSellPrice(symbol, name string, price float64) float64 {
	protect := round2(price * 0.98)
	percent := PriceLimitPercent(symbol, name)
	if previous := price; previous > 0 {
		lower := limitDownPrice(previous, percent)
		if protect < lower {
			protect = lower
		}
	}
	if protect <= 0 {
		protect = price
	}
	return protect
}

func triggerLabel(trigger string) string {
	switch {
	case trigger == TriggerStopLoss:
		return "止损"
	case trigger == TriggerHardStop:
		return "硬止损"
	case trigger == TriggerTrailStop:
		return "移动止损"
	case trigger == TriggerInvalidation:
		return "失效条件"
	case trigger == TriggerTimeStop:
		return "时间止损"
	case strings.HasPrefix(trigger, TriggerTakeProfit):
		return "止盈"
	default:
		return trigger
	}
}

// ProtectiveDecision 把保护性卖出包成一条标准裁决记录，
// 让它与模型裁决共用同一张表、同一套查询与同一个回放界面。
func ProtectiveDecision(snapshot Snapshot, rules RiskRules, control Control, result GuardResult, cycleID string, now time.Time) Decision {
	decision := Decision{
		ID:        newID("dec"),
		AccountID: "",
		CycleID:   cycleID,
		TakenAt:   now.UTC(),
		Mode:      "protective",
		Gate:      control.Gate(),
		Market:    snapshot.Market,
		Rules:     rules.Normalize(),
		Verdicts:  result.Verdicts,
		Narrative: []string{},
	}
	if len(snapshot.Positions) > 0 {
		decision.AccountID = snapshot.Positions[0].AccountID
	}
	// 保护性卖出没有"模型建议"这一层：Proposal 留空，
	// 订单本身就是裁决结果。硬塞一份 Proposal 会让界面显示两份内容一样的清单。
	decision.Stats = policy.Summarize(result.Verdicts)
	decision.Stats.Proposed = len(result.Orders)
	decision.Stats.Accepted = len(result.Orders)
	decision.Stats.Ordered = len(result.Orders)
	decision.Exposure = measureExposure(snapshot, result.Orders, rules.Normalize())

	decision.Narrative = append(decision.Narrative,
		fmt.Sprintf("本轮由持仓防守线发起，检查 %d 只持仓，触发 %d 笔离场", result.Held, len(result.Orders)))
	for _, order := range result.Orders {
		decision.Narrative = append(decision.Narrative, fmt.Sprintf("%s %s：%s",
			order.Symbol, triggerLabel(order.Trigger), truncateText(order.Thesis, 80)))
	}
	if len(result.Orders) == 0 {
		decision.Narrative = append(decision.Narrative, "所有持仓均在防守线内，无需动作")
	}
	return decision
}
