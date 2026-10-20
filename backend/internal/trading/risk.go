package trading

import (
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// DraftOrder 是模型输出的原始建议。它不含股数与金额——股数必须由
// BuildOrders 依据注入的行情与账户状态计算，避免模型凭空报数量。
type DraftOrder struct {
	Symbol              string  `json:"symbol"`
	Name                string  `json:"name,omitempty"`
	Side                string  `json:"side"`
	TargetWeightPercent float64 `json:"target_weight_percent,omitempty"`
	SellFractionPercent float64 `json:"sell_fraction_percent,omitempty"`
	OrderType           string  `json:"order_type,omitempty"`
	LimitPrice          float64 `json:"limit_price,omitempty"`
	StopLossPrice       float64 `json:"stop_loss_price,omitempty"`
	TakeProfitPrice     float64 `json:"take_profit_price,omitempty"`
	Thesis              string  `json:"thesis,omitempty"`
	Invalidation        string  `json:"invalidation,omitempty"`
	Confidence          float64 `json:"confidence,omitempty"`
}

type DraftPlan struct {
	Regime    string       `json:"regime"`
	Summary   string       `json:"summary"`
	RiskNotes []string     `json:"risk_notes,omitempty"`
	Orders    []DraftOrder `json:"orders"`
}

// BuildOrders 把模型建议转换为可执行订单，并做确定性风控校验。
//
// 两步分离是本函数的核心：先判断"允不允许"，再计算"多少股"。
// 混在一起做会让"被拒绝"变成"股数算不出来"，而这两件事对复盘的意义完全不同。
//
// 校验失败的订单仍然保留在结果中（状态为 rejected 并带原因），
// 这样用户能看到模型原意与被拒理由，而不是被静默丢弃。
func BuildOrders(accountID string, draft DraftPlan, snapshot Snapshot, rules RiskRules, now time.Time) ([]Order, []policy.Verdict) {
	rules = rules.Normalize()
	verdicts := make([]policy.Verdict, 0, len(draft.Orders)+4)
	orders := make([]Order, 0, len(draft.Orders))
	if len(draft.Orders) == 0 {
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeOrderEmpty, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
			Field: "orders", Message: "模型没有给出任何交易建议",
		})
		return orders, verdicts
	}
	accepted := draft.Orders
	if len(accepted) > rules.MaxOrders {
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeOrderCountCap, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
			Field: "orders", Metric: float64(len(accepted)), Limit: float64(rules.MaxOrders),
			Message: fmt.Sprintf("模型给出 %d 笔建议，超过单次上限 %d 笔，仅保留前 %d 笔",
				len(accepted), rules.MaxOrders, rules.MaxOrders),
		})
		accepted = accepted[:rules.MaxOrders]
	}

	quotes := map[string]foundation.Quote{}
	for _, quote := range snapshot.Quotes {
		quotes[quote.Symbol] = quote
	}
	positions := map[string]Position{}
	for _, position := range snapshot.Positions {
		positions[position.Symbol] = position
	}
	equity := snapshot.Equity
	if equity <= 0 {
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeRiskTradeRisk, Level: policy.LevelBlock, Scope: policy.ScopeAccount,
			Field: "equity", Message: "账户总资产异常，无法计算仓位",
		})
		return orders, verdicts
	}

	// 组合账不是每笔独立判断，而是逐笔累加。已持仓、在途挂单、本次已通过的买入
	// 都在同一本账上，否则"六笔各 15%"会全部通过，合计却压在同一个行业里。
	book := policy.NewBook(equity, rules)
	for _, position := range snapshot.Positions {
		book.Seed(position.Symbol, snapshot.Sectors[position.Symbol], position.WeightPercent(equity))
	}
	for symbol, value := range snapshot.OpenBuyValue {
		if value <= 0 {
			continue
		}
		book.Seed(symbol, snapshot.Sectors[symbol], round2(value/equity*100))
	}

	seen := map[string]bool{}
	// 可花的钱必须扣掉已被挂单冻结的部分，否则多笔挂单会共享同一笔现金。
	cashLeft := snapshot.AvailableCash()
	buyNotional := 0.0
	sellProceeds := 0.0

	block := func(order *Order, verdict policy.Verdict) {
		order.Status = OrderStatusRejected
		order.RejectReason = verdict.Message
		order.RejectCode = verdict.Code
		// 刻意保留 Quantity：被拦的建议也要能回答"模型原本想买多少"。
		// 置零会留下一条"买入 0 股"的记录，既看不懂，也让复盘无法比较
		// "模型想买 100 股被拦"和"模型只想买 100 股但资金不够"。
		// 下游所有计算都只看 pending 状态，因此保留数量不会造成冻结或暴露虚高。
		order.ReservedAmount = 0
		order.Sector = ""
		orders = append(orders, *order)
		verdicts = append(verdicts, verdict)
	}

	for index, item := range draft.Orders {
		field := fmt.Sprintf("orders[%d]", index)
		if index >= len(accepted) {
			break
		}
		order := Order{
			ID:                  newID("ord"),
			AccountID:           accountID,
			Side:                Side(strings.ToLower(strings.TrimSpace(item.Side))),
			Status:              OrderStatusPending,
			Thesis:              strings.TrimSpace(item.Thesis),
			Invalidation:        strings.TrimSpace(item.Invalidation),
			Confidence:          normalizeConfidence(item.Confidence),
			OrderType:           OrderType(strings.ToLower(strings.TrimSpace(item.OrderType))),
			CreatedAt:           now,
			UpdatedAt:           now,
			LimitPrice:          round2(item.LimitPrice),
			StopLossPrice:       round2(item.StopLossPrice),
			TakeProfitPrice:     round2(item.TakeProfitPrice),
			TargetWeightPercent: round2(item.TargetWeightPercent),
		}

		symbol, err := foundation.NormalizeSymbol(item.Symbol)
		if err != nil {
			order.Symbol = strings.TrimSpace(item.Symbol)
			block(&order, policy.Verdict{
				Code: policy.CodeOrderInvalidSymbol, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
				Field: field, Symbol: order.Symbol, Message: fmt.Sprintf("标的代码无效：%s", item.Symbol),
			})
			continue
		}
		order.Symbol = symbol.Canonical
		if seen[order.Symbol] {
			block(&order, policy.Verdict{
				Code: policy.CodeOrderDuplicateSymbol, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
				Field: field, Symbol: order.Symbol,
				Message: fmt.Sprintf("%s 重复出现，同一标的每次计划只允许一笔", order.Symbol),
			})
			continue
		}
		seen[order.Symbol] = true

		quote, ok := quotes[order.Symbol]
		if !ok || quote.Price <= 0 {
			order.Name = strings.TrimSpace(item.Name)
			block(&order, policy.Verdict{
				Code: policy.CodeOrderNoQuote, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
				Field: field, Symbol: order.Symbol,
				Message: fmt.Sprintf("%s 不在本次行情快照内或暂无有效价格，拒绝按未知价格下单", order.Symbol),
			})
			continue
		}
		order.Name = firstNonEmpty(order.Name, quote.Name, item.Name)
		order.Sector = snapshot.Sectors[order.Symbol]

		if !order.Side.Valid() {
			block(&order, policy.Verdict{
				Code: policy.CodeOrderInvalidSide, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
				Field: field, Symbol: order.Symbol,
				Message: fmt.Sprintf("%s 买卖方向无效：%q", order.Symbol, item.Side),
			})
			continue
		}
		if order.OrderType != OrderTypeLimit && order.OrderType != OrderTypeMarket {
			order.OrderType = OrderTypeLimit
		}

		percent := PriceLimitPercent(order.Symbol, order.Name)
		upper := limitUpPrice(quote.PreviousClose, percent)
		lower := limitDownPrice(quote.PreviousClose, percent)
		// 市价单在真实市场并不存在「按现价必然成交」这回事。落库时写入保护限价，
		// 一让冻结金额可计算，二让模拟撮合不会在极端跳空时凭空成交。
		if order.OrderType == OrderTypeMarket {
			protect := round2(quote.Price * 1.02)
			if order.Side == SideSell {
				protect = round2(quote.Price * 0.98)
			}
			if quote.PreviousClose > 0 {
				if order.Side == SideBuy && protect > upper {
					protect = upper
				}
				if order.Side == SideSell && protect < lower {
					protect = lower
				}
			}
			order.LimitPrice = protect
		}
		entry := quote.Price
		if order.OrderType == OrderTypeLimit {
			if order.LimitPrice <= 0 {
				block(&order, policy.Verdict{
					Code: policy.CodeOrderLimitPriceMiss, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol,
					Message: fmt.Sprintf("%s 限价单缺少有效限价", order.Symbol),
				})
				continue
			}
			if quote.PreviousClose > 0 && (order.LimitPrice > upper || order.LimitPrice < lower) {
				block(&order, policy.Verdict{
					Code: policy.CodeOrderPriceOutOfLimit, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: order.LimitPrice,
					Message: fmt.Sprintf("%s 限价 %.2f 超出涨跌停区间 %.2f–%.2f", order.Symbol, order.LimitPrice, lower, upper),
				})
				continue
			}
			entry = order.LimitPrice
		} else if quote.PreviousClose > 0 && (quote.Price > upper || quote.Price < lower) {
			block(&order, policy.Verdict{
				Code: policy.CodeOrderPriceOutOfLimit, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
				Field: field, Symbol: order.Symbol, Metric: quote.Price,
				Message: fmt.Sprintf("%s 现价 %.2f 超出涨跌停区间 %.2f–%.2f", order.Symbol, quote.Price, lower, upper),
			})
			continue
		}

		lot := MinBoardLot(order.Symbol)
		if lot < rules.BoardLot {
			lot = rules.BoardLot
		}

		if order.Side == SideBuy {
			if item.TargetWeightPercent <= 0 || item.TargetWeightPercent > 100 {
				block(&order, policy.Verdict{
					Code: policy.CodeRiskSingleName, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: item.TargetWeightPercent,
					Message: fmt.Sprintf("%s 目标仓位 %.2f%% 无效", order.Symbol, item.TargetWeightPercent),
				})
				continue
			}
			if item.TargetWeightPercent > rules.MaxSingleWeightPercent {
				block(&order, policy.Verdict{
					Code: policy.CodeRiskSingleName, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: item.TargetWeightPercent, Limit: rules.MaxSingleWeightPercent,
					Message: fmt.Sprintf("%s 目标仓位 %.2f%% 超过单票上限 %.2f%%", order.Symbol, item.TargetWeightPercent, rules.MaxSingleWeightPercent),
				})
				continue
			}
			if verdict, ok := rules.CheckStopLoss(order.Symbol, entry, order.StopLossPrice); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			amount := equity * item.TargetWeightPercent / 100
			quantity := roundSharesDown(int(amount/entry), lot)
			// 立刻回写到订单上，不能等到最后"通过"时才赋值。
			// 后续任何一条拦截（资金、单笔风险、行业集中度）都要能带上
			// "原本打算买多少"，否则留下的就是一条"买入 0 股"的无意义记录。
			order.Quantity = quantity
			if quantity < lot {
				block(&order, policy.Verdict{
					Code: policy.CodeSizingBelowLot, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: amount,
					Message: fmt.Sprintf("%s 目标金额 %.0f 元不足以买入一手（%d 股）", order.Symbol, amount, lot),
				})
				continue
			}
			notional := float64(quantity) * entry
			fee := TradeFee(SideBuy, notional)
			if notional+fee > cashLeft {
				original := quantity
				// 按可用资金逐手缩减，直到金额与费用都能被覆盖。
				quantity = roundSharesDown(int(cashLeft/entry), lot)
				for quantity >= lot {
					notional = float64(quantity) * entry
					fee = TradeFee(SideBuy, notional)
					if notional+fee <= cashLeft {
						break
					}
					quantity -= lot
				}
				if quantity < lot {
					// 拦下来时恢复原始拟买数量：这条记录要回答的是
					// "模型想买多少、为什么买不成"，而不是"缩减后剩 0 股"。
					order.Quantity = original
					block(&order, policy.Verdict{
						Code: policy.CodeSizingReducedByCash, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
						Field: field, Symbol: order.Symbol, Metric: cashLeft,
						Message: fmt.Sprintf("%s 可用资金 %.2f 元不足以完成该笔买入", order.Symbol, cashLeft),
					})
					continue
				}
				verdicts = append(verdicts, policy.Verdict{
					Code: policy.CodeSizingReducedByCash, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: cashLeft,
					Message: fmt.Sprintf("%s 按可用资金缩减为 %d 股", order.Symbol, quantity),
				})
			}
			if verdict, ok := rules.CheckTradeRisk(order.Symbol, float64(quantity)*(entry-order.StopLossPrice), equity); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			// 组合守卫放在最后：前面都过了才占用组合额度，
			// 否则被拒的订单会白白吃掉行业名额与仓位预算。
			if verdict, ok := book.TryBuy(order.Symbol, order.Sector, item.TargetWeightPercent); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			order.Source = OrderSourceModel
			order.Quantity = quantity
			cashLeft = round2(cashLeft - notional - fee)
			buyNotional += notional
		} else {
			position, ok := positions[order.Symbol]
			if !ok || position.Quantity <= 0 {
				block(&order, policy.Verdict{
					Code: policy.CodePositionNotHeld, Level: policy.LevelBlock, Scope: policy.ScopePosition,
					Field: field, Symbol: order.Symbol,
					Message: fmt.Sprintf("%s 没有持仓，不能卖出", order.Symbol),
				})
				continue
			}
			// 可卖 = 已解禁 - 已挂卖单占用。用 Available 判断会让两笔卖单都通过，
			// 到成交时第二笔才发现不够，而那时它已经占了一个挂单位置。
			sellable := position.Sellable()
			fraction := item.SellFractionPercent
			if fraction <= 0 || fraction > 100 {
				fraction = 100
			}
			quantity := roundSharesDown(int(float64(sellable)*fraction/100), lot)
			// 与买入同理：先回写，再让后面的任何拦截都带上拟卖数量。
			order.Quantity = quantity
			if sellable < lot {
				block(&order, policy.Verdict{
					Code: policy.CodePositionUnavailable, Level: policy.LevelBlock, Scope: policy.ScopePosition,
					Field: field, Symbol: order.Symbol, Metric: float64(sellable),
					Message: fmt.Sprintf("%s 可卖数量 %d 股不足一手（已解禁 %d 股 − 已挂卖单 %d 股；T+1 当日买入不可卖）",
						order.Symbol, sellable, position.Available, position.ReservedQuantity),
				})
				continue
			}
			if quantity < lot {
				block(&order, policy.Verdict{
					Code: policy.CodeSizingBelowLot, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: fraction,
					Message: fmt.Sprintf("%s 卖出比例 %.0f%% 折算不足一手", order.Symbol, fraction),
				})
				continue
			}
			if quantity > sellable {
				quantity = roundSharesDown(sellable, lot)
			}
			// 模型主动离场：留下原因，复盘才能把"看错"与"被打止损"分开。
			order.Source = OrderSourceModel
			order.Trigger = TriggerModelExit
			order.Quantity = quantity
			sellProceeds += float64(quantity) * entry
		}
		orders = append(orders, order)
	}

	// 组合层面检查：买入后的总仓位与现金底线。
	//
	// 这里的处理方式对自动交易很关键。只给警告等于把"模型是否守规矩"当成前提，
	// 而模型并不总是守：它会给出合计 120% 的目标权重。若放任到下单阶段，
	// 资金不足会让靠前的几笔占满额度、后面的被逐笔缩减，相对权重被扭曲成"先到先得"。
	// 因此超出上限时按统一比例缩放全部买入，保持模型给出的相对权重不变。
	exposureLimit := rules.MaxTotalExposurePercent
	if snapshot.Market.Evaluated {
		// 市场预算只能收紧，不能放宽风控阈值。
		exposureLimit = snapshot.Market.MaxExposurePercent(rules.MaxTotalExposurePercent)
	}
	if buyNotional > 0 {
		currentValue := snapshot.PendingBuyValue
		for _, position := range snapshot.Positions {
			currentValue += position.MarketValue
		}
		projected := (currentValue + buyNotional) / equity * 100
		if projected > exposureLimit {
			scaled, scaledNotional := scaleBuysToExposure(orders, equity, currentValue, exposureLimit, buyNotional)
			orders = scaled
			buyNotional = scaledNotional
			message := fmt.Sprintf("买入后总仓位约 %.2f%% 超过上限 %.2f%%，已按比例缩减各笔买入以保持相对权重",
				projected, exposureLimit)
			if snapshot.Market.Evaluated {
				message += fmt.Sprintf("（上限来自市场判断「%s」：预算 %.2f%%、风控上限 %.2f%%，取较小值）",
					snapshot.Market.Label, snapshot.Market.RiskBudgetPercent, rules.MaxTotalExposurePercent)
			}
			verdicts = append(verdicts, policy.Verdict{
				Code: policy.CodeSizingScaled, Level: policy.LevelAdjust, Scope: policy.ScopePortfolio,
				Field: "exposure", Metric: projected, Limit: exposureLimit, Message: message,
			})
		}
		projectedCash := (snapshot.AvailableCash() + sellProceeds - buyNotional) / equity * 100
		if verdict, ok := rules.CheckCashFloor(round2(projectedCash)); !ok {
			verdicts = append(verdicts, verdict)
		}
	}

	return orders, verdicts
}

// scaleBuysToExposure 按相同的比例缩减全部待成交买单，使买入后总仓位落在上限内。
// 等比缩减而不是逐笔裁剪：前者保持模型给出的相对权重，后者会退化成"先到先得"。
// 缩减后不足一手的订单标记为 rejected 并说明原因，不做向上取整——
// 向上取整会让总仓位重新突破上限。
func scaleBuysToExposure(orders []Order, equity, currentValue, exposureLimit, buyNotional float64) ([]Order, float64) {
	allowance := exposureLimit/100*equity - currentValue
	if allowance < 0 {
		allowance = 0
	}
	ratio := 0.0
	if buyNotional > 0 {
		ratio = allowance / buyNotional
	}
	if ratio > 1 {
		ratio = 1
	}
	scaledNotional := 0.0
	for index := range orders {
		order := &orders[index]
		if order.Side != SideBuy || order.Status != OrderStatusPending {
			continue
		}
		lot := MinBoardLot(order.Symbol)
		entry := order.LimitPrice
		if entry <= 0 {
			entry = 1
		}
		quantity := roundSharesDown(int(float64(order.Quantity)*ratio), lot)
		if quantity < lot {
			order.Status = OrderStatusRejected
			order.RejectReason = fmt.Sprintf("按总仓位上限 %.2f%% 等比缩减后不足一手，本笔取消", exposureLimit)
			order.Quantity = 0
			order.ReservedAmount = 0
			order.Fee = 0
			continue
		}
		notional := float64(quantity) * entry
		order.Quantity = quantity
		order.TargetWeightPercent = round2(notional / equity * 100)
		order.Fee = TradeFee(SideBuy, notional)
		scaledNotional += notional
	}
	return orders, scaledNotional
}

func normalizeConfidence(value float64) float64 {
	if value <= 0 {
		return 0
	}
	if value > 1 {
		value = value / 100
	}
	if value > 1 {
		value = 1
	}
	return round2(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
