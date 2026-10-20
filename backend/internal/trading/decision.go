package trading

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
)

// Decision 是一次完整的裁决记录：模型提了什么、守卫允许了什么、最后下了什么单。
//
// 为什么要单独落库而不是只存在计划里：
//  1. 计划会被人工放弃、会被执行、会失败，它的状态一直在变；裁决是当时那一刻的
//     事实，不能被后续状态覆盖，否则"事后看起来当时就该买"这种错觉就出现了。
//  2. 复盘要按代码聚合："这周 risk.sector_weight 拦了多少笔、拦掉的那些后来涨了没有"。
//     聚合必须建在稳定结构上，而不是中文提示语上。
type Decision struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	PlanID    string    `json:"plan_id,omitempty"`
	CycleID   string    `json:"cycle_id,omitempty"`
	TakenAt   time.Time `json:"taken_at"`
	// Mode 是 candidate_pool 的来源：auto（系统全市场选股）或 manual（人工指定标的）。
	Mode string `json:"mode"`
	// Gate 是裁决时账户总闸的状态，区分"模型判断错了"与"当时本来就不该开仓"。
	Gate string `json:"gate"`
	// Market 是裁决时的市场认知，用于把结果归因到具体的市场环境。
	Market cognition.MarketView `json:"market"`
	Rules  RiskRules            `json:"rules"`
	// Proposal 保留模型的原始建议。被守卫拦掉的建议也要留下来：
	// 只有知道模型想做什么，才能评价守卫拦得对不对。
	Proposal []DraftOrder `json:"proposal,omitempty"`
	// ModelSummary / ModelRegime 是模型自己的判断，与 Market 并列存放，
	// 二者不一致本身就是一条值得复盘的信息。
	ModelRegime  string   `json:"model_regime,omitempty"`
	ModelSummary string   `json:"model_summary,omitempty"`
	RiskNotes    []string `json:"risk_notes,omitempty"`
	// Verdicts 是完整裁决流水，含通过之外的拦截、调整与提示。
	Verdicts []policy.Verdict `json:"verdicts"`
	Stats    policy.Stats     `json:"stats"`
	// Model 记录这次判断的代价。没有它就无法回答"自动交易一天要花多少钱"，
	// 也无法判断某轮结论是不是模型超时重试后的结果。
	Model ModelRun `json:"model,omitempty"`
	// Dropped 是因门控或市场状态被整批剔除的建议数。它与 Stats.Blocked 分开记，
	// 因为"没让买"和"买了但被拒"在复盘里是两种完全不同的事实。
	Dropped  int      `json:"dropped,omitempty"`
	Exposure Exposure `json:"exposure"`
	// Narrative 是给人看的裁决摘要，按顺序一行一个论断。
	// 它不是日志：每一条都能对回某个 Verdicts 里的 Code。
	Narrative []string `json:"narrative,omitempty"`
}

// Exposure 是决策前后的仓位视图。它是"为什么只买了这么点"的直接答案。
type Exposure struct {
	BeforePercent float64 `json:"before_percent"`
	AfterPercent  float64 `json:"after_percent"`
	// PendingPercent 是未成交买入挂单占用的仓位，已计入 Before。
	PendingPercent float64 `json:"pending_percent"`
	CashPercent    float64 `json:"cash_percent"`
	// BudgetPercent 是本次实际生效的仓位上限（市场预算与风控上限取较小值）。
	BudgetPercent float64               `json:"budget_percent"`
	BuyNotional   float64               `json:"buy_notional"`
	SellNotional  float64               `json:"sell_notional"`
	Sectors       []policy.SectorWeight `json:"sectors,omitempty"`
}

// ModelRun 是一次模型调用的代价与产出规模。
type ModelRun struct {
	LatencyMS        int64  `json:"latency_ms,omitempty"`
	PromptChars      int    `json:"prompt_chars,omitempty"`
	ContentChars     int    `json:"content_chars,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens,omitempty"`
	Model            string `json:"model,omitempty"`
}

// BuyBlock 是本轮「不准开新仓」的确定性理由，由调用方（自动循环）填入。
//
// 它与 Control 的区别必须分清：
//   - Control 是账户级持久开关，落库、跨进程、由人或熔断器按下；
//   - BuyBlock 是本轮的环境约束（尾盘时段、当日已熔断），不改变账户状态，
//     下一轮是否禁买要重新判断。
//
// 把两者混成一个字段，就会出现"昨天尾盘禁买，今天开盘还在禁买"这种静默失效。
type BuyBlock struct {
	Code    string
	Message string
}

// DecideInput 是一次裁决的全部输入。所有字段都由调用方从确定性来源填入，
// 模型只贡献 Draft。
type DecideInput struct {
	AccountID string
	PlanID    string
	CycleID   string
	Mode      string
	Draft     DraftPlan
	Snapshot  Snapshot
	Rules     RiskRules
	Control   Control
	// BuyBlock 非空时本轮禁止开新仓，卖出与减仓照常。
	BuyBlock *BuyBlock
	Model    ModelRun
	Now      time.Time
}

// Decide 执行"模型建议 → 守卫校验 → 裁决"。
//
// 顺序不可交换：
//  1. 账户总闸（kill switch / 暂停买入）——它否决的是整轮动作，不是某一笔；
//  2. 市场状态——数据不足或恐慌时不接飞刀，这条必须在选股之后、下单之前；
//  3. 逐笔风控与组合守卫（BuildOrders）——单笔合法但组合越界，同样不允许。
func Decide(in DecideInput) (Decision, []Order) {
	rules := in.Rules.Normalize()
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	decision := Decision{
		ID:           newID("dec"),
		AccountID:    in.AccountID,
		PlanID:       in.PlanID,
		CycleID:      in.CycleID,
		TakenAt:      now,
		Mode:         strings.TrimSpace(in.Mode),
		Gate:         in.Control.Gate(),
		Market:       in.Snapshot.Market,
		Rules:        rules,
		Proposal:     in.Draft.Orders,
		ModelRegime:  strings.TrimSpace(in.Draft.Regime),
		ModelSummary: strings.TrimSpace(in.Draft.Summary),
		RiskNotes:    in.Draft.RiskNotes,
		Model:        in.Model,
	}
	verdicts := make([]policy.Verdict, 0, len(in.Draft.Orders)+4)
	// 空切片而不是 nil：nil 会被 JSON 序列化成 null，前端取 orders.length 会直接崩。
	// 这条规则在 StartPlan 里已经踩过一次，这里同样适用。
	noOrders := []Order{}

	// 1. 账户总闸。
	killSwitch := in.Control.KillSwitch
	if killSwitch {
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeGateKillSwitch, Level: policy.LevelBlock, Scope: policy.ScopeAccount,
			Field: "control", Message: "账户已全部停止（kill switch），本轮不生成任何订单",
		})
	} else if in.Control.PauseBuying {
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeGatePauseBuying, Level: policy.LevelWarn, Scope: policy.ScopeAccount,
			Field: "control", Message: "账户暂停买入，本轮只允许卖出与减仓",
		})
	}

	// 2. 市场状态。Evaluated=false 表示本轮压根没取到全市场数据（人工指定标的时允许），
	// 此时不额外限制；Evaluated=true 且状态为 unknown 才是真正的"看不清"，必须禁止开仓。
	marketBlocked := false
	if !killSwitch && in.Snapshot.Market.Evaluated {
		switch in.Snapshot.Market.Regime {
		case cognition.RegimeUnknown:
			marketBlocked = true
			verdicts = append(verdicts, policy.Verdict{
				Code: policy.CodeMarketUnknown, Level: policy.LevelBlock, Scope: policy.ScopeMarket,
				Field: "market", Message: fmt.Sprintf("全市场样本不足（%s），无法判断市场状态，本轮禁止开新仓",
					in.Snapshot.Market.Label),
			})
		case cognition.RegimePanic:
			marketBlocked = true
			verdicts = append(verdicts, policy.Verdict{
				Code: policy.CodeMarketPanic, Level: policy.LevelBlock, Scope: policy.ScopeMarket,
				Field: "market", Metric: in.Snapshot.Market.Breadth.MedianChange,
				Message: fmt.Sprintf("市场处于恐慌（中位数 %+.2f%%、跌停 %d 只），本轮禁止开新仓",
					in.Snapshot.Market.Breadth.MedianChange, in.Snapshot.Market.Breadth.LimitDown),
			})
		}
	}

	draft := in.Draft
	dropped := 0
	// 本轮禁买可能有多个来源同时成立：门控、市场状态、时段、当日熔断。
	// 每一条都单独留痕——复盘要回答的是"这次没买，是因为市场差，还是因为今天已经亏够了"。
	buyBlocked := in.Control.PauseBuying || marketBlocked
	if in.BuyBlock != nil && !killSwitch {
		buyBlocked = true
		code := strings.TrimSpace(in.BuyBlock.Code)
		if code == "" {
			code = policy.CodeGateWindow
		}
		verdicts = append(verdicts, policy.Verdict{
			Code: code, Level: policy.LevelBlock, Scope: policy.ScopeAccount,
			Field: "window", Message: in.BuyBlock.Message,
		})
	}
	switch {
	case killSwitch:
		dropped = len(in.Draft.Orders)
		draft.Orders = nil
	case buyBlocked:
		draft, dropped = dropBuys(draft)
	}
	if dropped > 0 {
		cause := "门控生效"
		tail := "仅保留卖出与减仓"
		switch {
		case killSwitch:
			cause, tail = "账户全部停止", "本轮不产生任何订单"
		case in.BuyBlock != nil:
			cause, tail = "时段或熔断约束", "仅保留卖出与减仓"
		case marketBlocked && !in.Control.PauseBuying:
			cause, tail = "市场状态不允许开仓", "仅保留卖出与减仓"
		}
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeOrderBuyDropped, Level: policy.LevelAdjust, Scope: policy.ScopeAccount,
			Field: "orders", Metric: float64(dropped),
			Message: fmt.Sprintf("%s，已剔除 %d 笔建议，%s", cause, dropped, tail),
		})
	}
	if killSwitch {
		return finishDecision(decision, verdicts, noOrders, in, dropped), nil
	}
	// 买入被整批剔除后没有剩余建议时，不要再走一遍订单构建：
	// 那会给出一条"模型没有给出任何交易建议"，而事实是模型给了、被门控挡了。
	if len(draft.Orders) == 0 && dropped > 0 {
		return finishDecision(decision, verdicts, noOrders, in, dropped), nil
	}

	// 3. 逐笔风控 + 组合守卫。
	orders, buildVerdicts := BuildOrders(in.AccountID, draft, in.Snapshot, rules, now)
	verdicts = append(verdicts, buildVerdicts...)
	return finishDecision(decision, verdicts, orders, in, dropped), orders
}

// dropBuys 剔除买入建议，保留卖出与减仓。
func dropBuys(draft DraftPlan) (DraftPlan, int) {
	kept := make([]DraftOrder, 0, len(draft.Orders))
	dropped := 0
	for _, order := range draft.Orders {
		if Side(strings.ToLower(strings.TrimSpace(order.Side))) == SideBuy {
			dropped++
			continue
		}
		kept = append(kept, order)
	}
	draft.Orders = kept
	return draft, dropped
}

// finishDecision 补齐统计、仓位与摘要。所有派生数字都在这里算一次，
// 避免每个调用点各算一遍、各算错一次。
func finishDecision(decision Decision, verdicts []policy.Verdict, orders []Order, in DecideInput, dropped int) Decision {
	decision.Verdicts = verdicts
	stats := policy.Summarize(verdicts)
	stats.Proposed = len(decision.Proposal)
	// Blocked 以被拒订单数为准，而不是数拦截条数：一笔订单可能同时触发
	// 单票与行业两条拦截，用它来回答"拦了几笔"才不会重复计数。
	rejected, accepted := 0, 0
	for _, order := range orders {
		switch order.Status {
		case OrderStatusPending:
			accepted++
		case OrderStatusRejected:
			rejected++
		}
	}
	stats.Accepted = accepted
	stats.Ordered = accepted
	stats.Blocked = rejected + dropped
	decision.Stats = stats
	decision.Dropped = dropped
	decision.Exposure = measureExposure(in.Snapshot, orders, in.Rules.Normalize())
	decision.Narrative = explain(decision, in.Snapshot)
	return decision
}

// settleDecision 用落库后的订单状态回填裁决统计。
//
// 为什么需要这一步：裁决阶段认为"可执行"的订单，可能在冻结资金时被账本拒绝，
// 那才是最终事实。计划里写着 6 笔可执行、账户里只有 4 笔真实挂单，
// 是最容易让人不再信任系统的偏差。
//
// before 是裁决输出，after 是落库后的返回值：两者状态从 pending 变成 rejected，
// 才是"账本拒绝"；裁决阶段已经驳回的订单本来就有原因，不能混为一谈。
func settleDecision(decision Decision, snapshot Snapshot, before, after []Order) Decision {
	pendingBefore := map[string]bool{}
	for _, order := range before {
		if order.Status == OrderStatusPending {
			pendingBefore[order.ID] = true
		}
	}
	accepted, rejected, ledgerRejected := 0, 0, 0
	for _, order := range after {
		switch order.Status {
		case OrderStatusPending, OrderStatusPartial:
			accepted++
		case OrderStatusRejected:
			rejected++
			if pendingBefore[order.ID] {
				ledgerRejected++
				decision.Verdicts = append(decision.Verdicts, policy.Verdict{
					Code: policy.CodeLedgerReserveFailed, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Symbol: order.Symbol, Message: order.RejectReason,
				})
			}
		}
	}
	decision.Stats.Accepted = accepted
	decision.Stats.Ordered = accepted
	decision.Stats.Blocked = rejected + decision.Dropped
	// 摘要整体重建而不是追加：追加会让上半段在计划详情里出现两遍，
	// 而这两遍的内容还略有差异（统计已更新、解释文案没变），读起来像 bug。
	decision.Narrative = nil
	if ledgerRejected > 0 {
		decision.Narrative = append(decision.Narrative,
			fmt.Sprintf("账本拒绝 %d 笔：裁决通过但冻结资金时可用余额不足", ledgerRejected))
	}
	decision.Narrative = append(decision.Narrative, explain(decision, snapshot)...)
	return decision
}

func measureExposure(snapshot Snapshot, orders []Order, rules RiskRules) Exposure {
	equity := snapshot.Equity
	exposure := Exposure{}
	if equity > 0 {
		value := 0.0
		for _, position := range snapshot.Positions {
			value += position.MarketValue
		}
		exposure.BeforePercent = round2(value / equity * 100)
		exposure.PendingPercent = round2(snapshot.PendingBuyValue / equity * 100)
	}
	for _, order := range orders {
		if order.Status != OrderStatusPending {
			continue
		}
		notional := float64(order.Quantity) * order.LimitPrice
		if order.Side == SideBuy {
			exposure.BuyNotional += notional
		} else {
			exposure.SellNotional += notional
		}
	}
	exposure.BuyNotional = round2(exposure.BuyNotional)
	exposure.SellNotional = round2(exposure.SellNotional)
	if equity > 0 {
		// 买入后仓位必须把在途挂单算进去：只算持仓会得出"0% → 25%"，
		// 而账户里其实还有 55% 的挂单在途，真实暴露已经 80%。
		exposure.AfterPercent = round2(exposure.BeforePercent + exposure.PendingPercent + exposure.BuyNotional/equity*100)
		exposure.CashPercent = round2((snapshot.AvailableCash() + exposure.SellNotional - exposure.BuyNotional) / equity * 100)
	}
	exposure.BudgetPercent = rules.MaxTotalExposurePercent
	if snapshot.Market.Evaluated {
		exposure.BudgetPercent = snapshot.Market.MaxExposurePercent(rules.MaxTotalExposurePercent)
	}
	exposure.Sectors = sectorExposure(snapshot, orders, equity)
	return exposure
}

// sectorExposure 汇总决策后的行业暴露，用于回答"这六只票是不是同一个风险"。
func sectorExposure(snapshot Snapshot, orders []Order, equity float64) []policy.SectorWeight {
	if equity <= 0 {
		return nil
	}
	weights := map[string]float64{}
	names := map[string]map[string]bool{}
	add := func(symbol, sector string, weight float64) {
		if sector == "" || weight <= 0 {
			return
		}
		weights[sector] += weight
		if names[sector] == nil {
			names[sector] = map[string]bool{}
		}
		names[sector][symbol] = true
	}
	for _, position := range snapshot.Positions {
		add(position.Symbol, snapshot.Sectors[position.Symbol], position.WeightPercent(equity))
	}
	// 在途买入挂单也占行业额度：不把它们算进来，"行业已 45%"要等成交之后才看得见，
	// 而那时已经晚了。
	for symbol, value := range snapshot.OpenBuyValue {
		add(symbol, snapshot.Sectors[symbol], value/equity*100)
	}
	for _, order := range orders {
		if order.Status != OrderStatusPending || order.Side != SideBuy {
			continue
		}
		add(order.Symbol, order.Sector, float64(order.Quantity)*order.LimitPrice/equity*100)
	}
	out := make([]policy.SectorWeight, 0, len(weights))
	for sector, weight := range weights {
		out = append(out, policy.SectorWeight{Sector: sector, Weight: round2(weight), Names: len(names[sector])})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight == out[j].Weight {
			return out[i].Sector < out[j].Sector
		}
		return out[i].Weight > out[j].Weight
	})
	return out
}

// explain 生成人能读的裁决摘要。每条论断都必须能对回某个 Verdict 或订单状态，
// 不加"看起来不错"这类无法验证的话。
func explain(decision Decision, snapshot Snapshot) []string {
	lines := []string{}
	if snapshot.Market.Evaluated {
		lines = append(lines, fmt.Sprintf("市场判断「%s」，风险预算 %.0f%%，总仓位上限取 %.2f%%",
			snapshot.Market.Label, snapshot.Market.RiskBudgetPercent, decision.Exposure.BudgetPercent))
	} else {
		lines = append(lines, fmt.Sprintf("本轮未取得全市场状态，仓位上限按风控值 %.2f%%", decision.Exposure.BudgetPercent))
	}
	lines = append(lines, fmt.Sprintf("模型建议 %d 笔，通过 %d 笔，拦截 %d 笔，调整 %d 处",
		decision.Stats.Proposed, decision.Stats.Accepted, decision.Stats.Blocked, decision.Stats.Adjusted))
	if decision.Exposure.BuyNotional > 0 || decision.Exposure.SellNotional > 0 {
		pending := ""
		if decision.Exposure.PendingPercent > 0 {
			pending = fmt.Sprintf("，其中在途挂单 %.2f%%", decision.Exposure.PendingPercent)
		}
		lines = append(lines, fmt.Sprintf("仓位 %.2f%% → %.2f%%%s（买入 %.2f 元、卖出 %.2f 元），决策后现金 %.2f%%",
			decision.Exposure.BeforePercent, decision.Exposure.AfterPercent, pending,
			decision.Exposure.BuyNotional, decision.Exposure.SellNotional, decision.Exposure.CashPercent))
	}
	if len(decision.Exposure.Sectors) > 0 {
		parts := make([]string, 0, 3)
		for index, sector := range decision.Exposure.Sectors {
			if index >= 3 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %.2f%%（%d 只）", sector.Sector, sector.Weight, sector.Names))
		}
		lines = append(lines, "行业分布："+strings.Join(parts, "、"))
	}
	// 拦截原因按代码聚合：同一原因拦了三笔，说三遍没有意义。
	counts := map[string]int{}
	order := []string{}
	for _, verdict := range decision.Verdicts {
		if verdict.Level != policy.LevelBlock {
			continue
		}
		if counts[verdict.Code] == 0 {
			order = append(order, verdict.Code)
		}
		counts[verdict.Code]++
	}
	if len(order) > 0 {
		parts := make([]string, 0, len(order))
		for _, code := range order {
			parts = append(parts, fmt.Sprintf("%s ×%d", code, counts[code]))
		}
		lines = append(lines, "拦截原因："+strings.Join(parts, "、"))
	}
	if strings.TrimSpace(decision.ModelSummary) != "" {
		lines = append(lines, "模型自述："+truncateText(decision.ModelSummary, 120))
	}
	if decision.Model.LatencyMS > 0 {
		cost := fmt.Sprintf("模型耗时 %.1fs", float64(decision.Model.LatencyMS)/1000)
		if decision.Model.TotalTokens > 0 {
			cost += fmt.Sprintf("、%d tokens", decision.Model.TotalTokens)
		}
		if decision.Model.Model != "" {
			cost += "（" + decision.Model.Model + "）"
		}
		lines = append(lines, cost)
	}
	return lines
}

func truncateText(text string, limit int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
