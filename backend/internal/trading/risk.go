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
	// Invalidation 接受两种 JSON 形态：结构化对象，或一句中文。
	// 兼容字符串不是留后门，而是因为模型经常无视格式要求直接写人话，
	// 而一份"因为格式不对被整份拒绝"的计划，比一份条件被降级解析的计划更糟。
	Invalidation InvalidationSpec `json:"invalidation,omitempty"`
	// ExpectedDays 是预期兑现天数（交易日），时间止损据此判定。
	ExpectedDays int     `json:"expected_days,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
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
	// 入场环境以系统判定的市场状态为准，缺失时退回模型自己写的 regime。
	// 两条都存不下来的话，持仓就永远不知道自己是在什么环境下建的。
	entryRegime := strings.TrimSpace(string(snapshot.Market.Regime))
	if !snapshot.Market.Evaluated || entryRegime == "" {
		entryRegime = strings.TrimSpace(draft.Regime)
	}
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
			Invalidation:        describeInvalidation(&item.Invalidation, item.Invalidation.Note),
			InvalidationRule:    rulePointer(item.Invalidation),
			ExpectedDays:        normalizeExpectedDays(item.ExpectedDays),
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
			// 定价 = 置信度 × 波动率。两个系数都来自可核对的量：
			// 置信度是模型自己给的，ATR 是行情给的。
			// 两个系数都只减不增——系统的职责是把"它自己也没把握的、
			// 或者波动大到一止损就伤账户的"建议缩小，不是替模型加杠杆。
			targetWeight := item.TargetWeightPercent
			factor := 1.0
			notes := make([]string, 0, 2)
			atrApplied := false
			if f, note := confidenceFactor(order.Confidence); f < 1 {
				factor *= f
				notes = append(notes, note)
			}
			if f, note := atrSizingFactor(snapshot.ATRs[order.Symbol]); f < 1 {
				factor *= f
				atrApplied = true
				notes = append(notes, note)
			}
			if factor < 1 {
				targetWeight = round2(item.TargetWeightPercent * factor)
				code := policy.CodeSizingConfidence
				if atrApplied {
					code = policy.CodeSizingATR
				}
				verdicts = append(verdicts, policy.Verdict{
					Code: code, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
					Field: field, Symbol: order.Symbol, Metric: item.TargetWeightPercent, Limit: targetWeight,
					Message: fmt.Sprintf("%s %s，目标权重 %.2f%% → %.2f%%（系数 %.2f）",
						order.Symbol, strings.Join(notes, "、"), item.TargetWeightPercent, targetWeight, factor),
				})
			}
			amount := equity * targetWeight / 100
			quantity := roundSharesDown(int(amount/entry), lot)
			// 立刻回写到订单上，不能等到最后"通过"时才赋值。
			// 后续任何一条拦截（资金、单笔风险、行业集中度）都要能带上
			// "原本打算买多少"，否则留下的就是一条"买入 0 股"的无意义记录。
			order.Quantity = quantity
			if quantity < lot {
				oneLot := float64(lot) * entry
				originalAmount := equity * item.TargetWeightPercent / 100
				// 定价（置信度 × 波动率）把"原本买得起一手"的建议压到不足一手时，
				// 退到一手而不是直接作废。
				//
				// 这不是放宽风控：一手是账户能执行的最小单位，定价的目的是
				// "别买太多"，不是"别买"。两个系数相乘很容易把小账户上的
				// 5% 打成 2.2%，而 2.2% 在 30 元的票上就是 74 股——按原来的写法，
				// 整笔建议消失，计划变成"模型提了、系统什么都没做"。
				//
				// 界限很清楚：只有"定价造成的不足一手"才兜底。模型自己只要了
				// 不到一手的量（originalAmount < oneLot）时不兜底，那是它的判断，
				// 系统不该替它把仓位放大几倍。
				if factor < 1 && originalAmount >= oneLot && oneLot <= equity*rules.MaxSingleWeightPercent/100 {
					quantity = lot
					order.Quantity = lot
					verdicts = append(verdicts, policy.Verdict{
						Code: policy.CodeSizingMinLot, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
						Field: field, Symbol: order.Symbol, Metric: amount, Limit: round2(oneLot / equity * 100),
						Message: fmt.Sprintf("%s 定价后目标金额 %.0f 元不足一手，按最小一手 %d 股执行（约占 %.2f%%）",
							order.Symbol, amount, lot, oneLot/equity*100),
					})
				} else {
					block(&order, policy.Verdict{
						Code: policy.CodeSizingBelowLot, Level: policy.LevelBlock, Scope: policy.ScopeOrder,
						Field: field, Symbol: order.Symbol, Metric: amount,
						Message: fmt.Sprintf("%s 目标金额 %.0f 元不足以买入一手（%d 股）", order.Symbol, amount, lot),
					})
					continue
				}
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
			// 冷静期：刚在这只票（或这个行业）上止损，今天不回到同一个坑里。
			// 位置在数量算出之后：被拦的建议必须带着"原本想买多少"，
			// 拦在算数量之前会留下一条 0 股的记录，复盘时看不出模型的原意。
			if verdict, ok := snapshot.Cooldowns.Block(order.Symbol, order.Sector, now); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			if verdict, ok := rules.CheckTradeRisk(order.Symbol, float64(quantity)*(entry-order.StopLossPrice), equity); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			// 组合守卫放在最后：前面都过了才占用组合额度，
			// 否则被拒的订单会白白吃掉行业名额与仓位预算。
			if verdict, ok := book.TryBuy(order.Symbol, order.Sector, targetWeight); !ok {
				verdict.Field = field
				block(&order, verdict)
				continue
			}
			order.Source = OrderSourceModel
			order.Quantity = quantity
			order.EntryRegime = entryRegime
			// 订单上写实际生效的权重，而不是模型原话：界面显示"目标 10%"
			// 而实际只买了 7%，是最容易让人以为系统算错的一种偏差。
			order.TargetWeightPercent = targetWeight
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

// rulePointer 只在条件真的可执行时才返回指针。
// 返回空结构会让守卫误以为"有失效条件"，每条持仓都进一次判定分支。
func rulePointer(spec InvalidationSpec) *InvalidationSpec {
	if spec.Empty() {
		return nil
	}
	cloned := spec
	return &cloned
}

// normalizeExpectedDays 把预期天数拉回合理区间。0 表示"模型没给"，
// 由调用方按计划周期推断；给 0 与给 365 一样不可执行。
func normalizeExpectedDays(value int) int {
	if value <= 0 || value > 120 {
		return 0
	}
	return value
}

// confidenceFactor 把模型自评的置信度换算成仓位系数：0.5 + 0.5 × confidence。
//
// 为什么是线性而不是分档：分档只有"罚/不罚"两态，0.54 与 0.56 会得到 0.9 与 1.0
// 的断崖，而置信度本身是连续量。线性之后 0.4 给 0.7 额度、0.7 给 0.85、1.0 给满，
// 既不把低置信一棍打死，也不让 0.9 和 0.55 拿同样的钱。
//
// 0 不罚：归一化后 0 表示"模型没填"，而不是"极度不确定"。
// 把它当极度不确定罚一刀，会让所有省略该字段的旧格式建议被打折，
// 而复盘时找不到任何解释。
func confidenceFactor(value float64) (float64, string) {
	if value <= 0 {
		return 1, ""
	}
	if value > 1 {
		value = 1
	}
	return round2(0.5 + 0.5*value), fmt.Sprintf("置信度 %.2f", value)
}

// atrSizingFactor 把波动率换算成仓位系数：clamp(2.5 / ATR%, 0.55, 1)。
//
// 2.5% 是 A 股中等波动票的日 ATR，作为"给满额"的基准。ATR 到 5% 时系数 0.5
// 被夹到 0.55——同样 10% 止损，高波动票更容易被日常波动打掉，
// 仓位必须相应缩小，否则"止损很紧、仓位很重"会让一次正常回调变成实质亏损。
//
// 上界故意设成 1 而不是 >1：低波动票不加仓。加仓等于系统替模型放大杠杆，
// 而且会把已经通过单票上限校验的权重推到上限之外。
func atrSizingFactor(atrPercent float64) (float64, string) {
	if atrPercent <= atrSizingBaseline {
		return 1, ""
	}
	factor := clampFloat(atrSizingBaseline/atrPercent, atrSizingFloor, 1)
	return round2(factor), fmt.Sprintf("ATR 占现价 %.2f%%（波动偏大）", atrPercent)
}

const (
	// atrSizingBaseline 是"不因波动缩减仓位"的 ATR 基准（百分比）。
	atrSizingBaseline = 2.5
	// atrSizingFloor 是波动率定价的下限系数。低于它就不再缩：
	// 再缩下去一笔正常建议只买一两手，等于用规则否掉了模型的判断。
	atrSizingFloor = 0.55
)

// ApplyHorizonDefaults 在裁决之前补齐模型没给的预期天数与失效条件兜底。
//
// 为什么要在裁决前做而不是在下单时：这两项会写进订单、写进持仓、写进裁决记录。
// 到了守卫那一层再补，裁决里就看不到"这个数字是谁定的"，复盘会把系统默认值
// 当成模型自己的判断。
func ApplyHorizonDefaults(draft *DraftPlan, horizon string) {
	days := expectedDaysForHorizon(horizon)
	for index := range draft.Orders {
		order := &draft.Orders[index]
		if Side(strings.ToLower(strings.TrimSpace(order.Side))) != SideBuy {
			continue
		}
		if order.ExpectedDays <= 0 {
			order.ExpectedDays = days
		}
		// 失效条件与止损价重合时，把失效条件下的“论点失效”抬到止损线上方一点，
		// 否则价格先到失效条件再触发止损，减仓与清仓会在同一秒各执行一次。
		if rule := &order.Invalidation; rule.PriceBelow > 0 && order.StopLossPrice > 0 && rule.PriceBelow <= order.StopLossPrice {
			rule.PriceBelow = round2(order.StopLossPrice * 1.005)
		}
	}
}

// expectedDaysForHorizon 把计划周期映射成默认兑现天数。
// 只做关键词匹配，匹配不到给 10 天：比 0（等于关闭时间止损）安全，
// 比 3 天（等于对波段仓位施加日内纪律）更不容易误伤。
func expectedDaysForHorizon(horizon string) int {
	value := strings.ToLower(strings.TrimSpace(horizon))
	switch {
	case strings.Contains(value, "日内") || strings.Contains(value, "超短") || strings.Contains(value, "intraday"):
		return 2
	case strings.Contains(value, "短") || strings.Contains(value, "swing"):
		return 5
	case strings.Contains(value, "中") || strings.Contains(value, "mid"):
		return 20
	case strings.Contains(value, "长") || strings.Contains(value, "long"):
		return 40
	default:
		return 10
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
