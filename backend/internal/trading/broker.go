package trading

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrBrokerUnavailable 表示真实券商通道尚未实现。P1 只有模拟盘。
var ErrBrokerUnavailable = errors.New("真实券商通道尚未接入，当前仅支持模拟盘")

// BrokerOrder 是下发到执行通道的订单快照。行情由服务层注入，通道不自行取数，
// 这样模拟盘与未来实盘共享同一套校验与记账逻辑。
type BrokerOrder struct {
	Account  Account
	Position Position
	Order    Order
	Quote    QuoteView
}

// QuoteView 是撮合所需的最小行情字段。
type QuoteView struct {
	Symbol        string
	Name          string
	Price         float64
	PreviousClose float64
	// Turnover 是当日累计成交额（元）。滑点档位与流动性上限都按它算：
	// 没有这个数字，撮合就只能假设"无论多大的单都有足够对手盘"，
	// 而那个假设正是回测收益虚高的主要来源。0 表示数据源没给。
	Turnover float64
}

type BrokerResult struct {
	Status         OrderStatus
	FilledQuantity int
	Price          float64
	Fee            float64
	Reason         string
	// ChannelOrderID 是券商侧委托号。异步通道受理成功时返回，
	// 服务层写回订单后，后续对账与撤单都靠它定位券商侧的委托。
	// 为空表示本笔委托没有券商侧形态（模拟盘）。
	ChannelOrderID string
}

type Broker interface {
	Name() string
	Submit(ctx context.Context, request BrokerOrder) (BrokerResult, error)
}

// PaperBroker 按最新价做保守撮合：
//   - 限价买入仅在现价不高于限价时成交，成交价为现价（加滑点，不高于限价）；
//   - 限价卖出仅在现价不低于限价时成交（减滑点，不低于限价）；
//   - 滑点按当日成交额分档：越冷的票进得越贵，成交额缺失按最冷档；
//   - 单笔不超过当日成交额的 0.5%，超出只成交能吃掉的部分；
//   - 市价单按现价成交，但仍受涨跌停价校验，越界直接拒绝；
//   - 涨停的买单、跌停的卖单不成交：真实市场里那一刻没有对手盘，
//     用"现价成交"会让回测里的收益凭空多出一截；
//   - 无有效行情时保持挂单，不做任何猜测性成交。
//
// 与第一版（永远按现价全额成交）的区别，正是"回测很漂亮、实盘做不到"的三个来源：
// 成交价更好、成交量大、涨跌停照样成交。
type PaperBroker struct {
	// SlippageBPS 是滑点的手动覆盖值（万分之几）。0 表示按成交额自动分档。
	// 留这个开关是为了压力测试（比如"滑点翻倍会怎样"），正常跑不该设它。
	SlippageBPS float64
}

func NewPaperBroker() *PaperBroker { return &PaperBroker{} }

func (b *PaperBroker) Name() string { return "paper" }

const (
	// 滑点按当日成交额分档。流动性越差，一单打进去推动价格越多。
	// 这不是精确的市场冲击模型，但它把"所有票都能按现价轻松成交"这个
	// 最离谱的假设改成了"票越冷，进得越贵"。
	slippageIlliquid = 0.0030 // 成交额 < 5000 万
	slippageNormal   = 0.0015 // 5000 万 – 3 亿
	slippageDeep     = 0.0005 // > 3 亿
	turnoverNormal   = 50_000_000
	turnoverDeep     = 300_000_000
	// liquidityShareCap 是单笔可吃掉的当日成交额上限。
	// 0.5% 意味着 1 亿成交额的票，单笔最多 50 万：再大就必须分几天做，
	// 否则"一次买满"在真实市场里会把价格推走。
	liquidityShareCap = 0.005
)

func (b *PaperBroker) Submit(_ context.Context, request BrokerOrder) (BrokerResult, error) {
	order := request.Order
	quote := request.Quote
	if quote.Price <= 0 {
		return BrokerResult{Status: OrderStatusPending, Reason: "暂无有效行情，订单保持挂起"}, nil
	}
	percent := PriceLimitPercent(order.Symbol, order.Name)
	var upper, lower float64
	if quote.PreviousClose > 0 {
		upper = limitUpPrice(quote.PreviousClose, percent)
		lower = limitDownPrice(quote.PreviousClose, percent)
		if order.OrderType == OrderTypeLimit && (order.LimitPrice > upper || order.LimitPrice < lower) {
			return BrokerResult{Status: OrderStatusRejected, Reason: fmt.Sprintf("限价 %.2f 超出涨跌停区间 %.2f–%.2f", order.LimitPrice, lower, upper)}, nil
		}
	}

	quantity := order.Quantity
	slip := b.slippageFor(quote.Turnover)
	// 撮合可花的钱是「账面现金减其他挂单冻结」，本单自身冻结不算在内。
	if order.Side == SideBuy {
		cash := request.Account.CashForOrder(order)
		unitCost := quote.Price * (1 + commissionRate + transferRate + slip)
		maxShares := int(cash / unitCost)
		maxShares = roundSharesDown(maxShares, MinBoardLot(order.Symbol))
		if maxShares < quantity {
			quantity = maxShares
		}
		if quantity <= 0 {
			return BrokerResult{Status: OrderStatusRejected, Reason: "可用资金不足以买入最小申报数量"}, nil
		}
	} else {
		if quantity > request.Position.Available {
			quantity = request.Position.Available
		}
		if quantity <= 0 {
			return BrokerResult{Status: OrderStatusRejected, Reason: "可卖数量为 0（T+1 或已无持仓）"}, nil
		}
	}

	switch order.Side {
	case SideBuy:
		// 市价单也带保护限价（由风控层写入），越界同样不成交。
		if quote.Price > order.LimitPrice && order.LimitPrice > 0 {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("现价 %.2f 高于限价 %.2f，等待回落", quote.Price, order.LimitPrice)}, nil
		}
		// 一字涨停没有卖盘。此时"按涨停价成交"是最典型的回测幻觉：
		// 真实情况下排在后面的单根本吃不到量。
		if upper > 0 && quote.Price >= upper {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("%s 处于涨停价 %.2f，买盘排队中，本轮不成交", order.Symbol, upper)}, nil
		}
	case SideSell:
		if order.OrderType == OrderTypeLimit && quote.Price < order.LimitPrice {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("现价 %.2f 低于限价 %.2f，等待反弹", quote.Price, order.LimitPrice)}, nil
		}
		// 跌停时同样没有买盘。保护性卖出会一直挂到收盘，
		// 系统必须能看见"止损触发了但卖不出去"，而不是假装卖掉了。
		if lower > 0 && quote.Price <= lower {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("%s 处于跌停价 %.2f，卖盘排队中，本轮不成交", order.Symbol, lower)}, nil
		}
	default:
		return BrokerResult{Status: OrderStatusRejected, Reason: "未知买卖方向"}, nil
	}

	price := b.fillPrice(order.Side, quote.Price, order.LimitPrice, slip)
	fee := TradeFee(order.Side, float64(quantity)*price)
	if order.Side == SideBuy {
		total := float64(quantity)*price + fee
		if total > request.Account.CashForOrder(order)+0.001 {
			// 滑点可能让"刚好够"的单变成不够，此时按资金反算一个能成交的数量，
			// 而不是整笔拒绝——整笔拒绝会让一笔 90% 能成的单彻底失败。
			quantity = roundSharesDown(int(request.Account.CashForOrder(order)/(price*(1+commissionRate+transferRate))), MinBoardLot(order.Symbol))
			if quantity <= 0 {
				return BrokerResult{Status: OrderStatusRejected, Reason: "可用资金不足以覆盖成交金额与费用"}, nil
			}
			fee = TradeFee(order.Side, float64(quantity)*price)
		}
	}
	// 流动性上限：单笔不超过当日成交额的 0.5%。
	//
	// 这条比滑点更硬：滑点只是"买贵一点"，而超出盘口深度的那部分根本成交不了。
	// 不做截断的话，一笔 500 万的单打进日成交 2000 万的票会一次全成，
	// 回测里看不见任何冲击成本，实盘第一天就会发现买不完。
	if quote.Turnover > 0 {
		capNotional := quote.Turnover * liquidityShareCap
		if float64(quantity)*price > capNotional {
			capped := roundSharesDown(int(capNotional/price), MinBoardLot(order.Symbol))
			if capped <= 0 {
				return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf(
					"%s 当日成交额 %.0f 万元，单笔上限不足一手，本轮不成交", order.Symbol, quote.Turnover/10000)}, nil
			}
			quantity = capped
			fee = TradeFee(order.Side, float64(quantity)*price)
		}
	}
	status := OrderStatusFilled
	if quantity < order.Quantity {
		status = OrderStatusPartial
	}
	return BrokerResult{Status: status, FilledQuantity: quantity, Price: price, Fee: fee}, nil
}

// slippageFor 按当日成交额返回滑点比例。
//
// 成交额缺失（0）时按最低流动性档处理：宁可让模拟盘显得更难做，
// 也不能因为"数据源没给这个字段"就让所有成交都变便宜。
// 未知不等于好，这条规则在撮合里比在任何地方都重要。
func (b *PaperBroker) slippageFor(turnover float64) float64 {
	if b.SlippageBPS > 0 && b.SlippageBPS <= 100 {
		return b.SlippageBPS / 10000
	}
	switch {
	case turnover >= turnoverDeep:
		return slippageDeep
	case turnover >= turnoverNormal:
		return slippageNormal
	default:
		return slippageIlliquid
	}
}

// fillPrice 在限价约束内应用滑点：买入往上滑、卖出往下滑。
//
// 滑点方向永远对自己不利，这是刻意的——反方向的滑点等于内幕信息，
// 只存在于"想让回测好看"的实现里。
func (b *PaperBroker) fillPrice(side Side, market, limit, slip float64) float64 {
	switch side {
	case SideBuy:
		price := market * (1 + slip)
		if limit > 0 && price > limit {
			price = limit
		}
		return round2(price)
	case SideSell:
		price := market * (1 - slip)
		if limit > 0 && price < limit {
			price = limit
		}
		return round2(price)
	default:
		return round2(market)
	}
}

// UnimplementedBroker 是实盘通道的显式占位，任何调用都会失败而不是静默降级。
type UnimplementedBroker struct{ Channel string }

func (b UnimplementedBroker) Name() string {
	if strings.TrimSpace(b.Channel) == "" {
		return "real"
	}
	return b.Channel
}

func (b UnimplementedBroker) Submit(context.Context, BrokerOrder) (BrokerResult, error) {
	return BrokerResult{}, fmt.Errorf("%w（通道：%s）", ErrBrokerUnavailable, b.Name())
}

// ChannelInfo 是执行通道的能力声明，供前端与运维判断当前处于哪种模式。
//
// Live 不是"配置成实盘"的意思，而是"钱是真的"：它决定页面要不要提示
// "本页操作会真实下单"。把这个判断放在后端，前端就不会因为配置项拼错而误导人。
type ChannelInfo struct {
	Name string `json:"name"`
	Live bool   `json:"live"`
	// SupportsCancel 表示通道支持独立撤单。模拟盘为 false：本地撤销即全部撤销，
	// 不存在"通道还挂着"这一状态。
	SupportsCancel bool   `json:"supports_cancel"`
	Note           string `json:"note"`
}

// ChannelInfo 返回当前通道的能力。
func (s *Service) ChannelInfo() ChannelInfo {
	name := s.BrokerName()
	_, cancelable := s.broker.(CancelableChannel)
	info := ChannelInfo{Name: name, Live: name != "paper", SupportsCancel: cancelable}
	if !info.Live {
		info.Note = "模拟盘：撮合按最新价保守成交，不产生真实委托"
	}
	return info
}
