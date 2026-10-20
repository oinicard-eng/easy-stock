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
}

type BrokerResult struct {
	Status         OrderStatus
	FilledQuantity int
	Price          float64
	Fee            float64
	Reason         string
}

type Broker interface {
	Name() string
	Submit(ctx context.Context, request BrokerOrder) (BrokerResult, error)
}

// PaperBroker 按最新价做保守撮合：
//   - 限价买入仅在现价不高于限价时成交，成交价为现价（加滑点，不高于限价）；
//   - 限价卖出仅在现价不低于限价时成交（减滑点，不低于限价）；
//   - 市价单按现价成交，但仍受涨跌停价校验，越界直接拒绝；
//   - 涨停的买单、跌停的卖单不成交：真实市场里那一刻没有对手盘，
//     用"现价成交"会让回测里的收益凭空多出一截；
//   - 无有效行情时保持挂单，不做任何猜测性成交。
//
// 与第一版（永远按现价全额成交）的区别，正是"回测很漂亮、实盘做不到"的三个来源：
// 成交价更好、成交量大、涨跌停照样成交。
type PaperBroker struct {
	// SlippageBPS 是成交价相对现价的滑点（万分之几）。
	// 默认 8bp：A 股竞价撮合的小单滑点大约在这个量级，
	// 给 0 会让所有回测收益都偏高一点点，而这一点点会累积成系统性乐观。
	SlippageBPS float64
}

func NewPaperBroker() *PaperBroker { return &PaperBroker{SlippageBPS: defaultSlippageBPS} }

func (b *PaperBroker) Name() string { return "paper" }

const (
	defaultSlippageBPS = 8.0
	// 部分成交的门槛。金额越大越难一次吃完：
	// 30 万以内按全额，100 万以内按 80%，再往上按 50%。
	// 这些不是精确的市场冲击模型，只是让"大单一定全成"这个假设不再成立。
	partialFillThresholdSmall = 300_000
	partialFillThresholdLarge = 1_000_000
	partialFillRatioMedium    = 0.8
	partialFillRatioLarge     = 0.5
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
	// 撮合可花的钱是「账面现金减其他挂单冻结」，本单自身冻结不算在内。
	if order.Side == SideBuy {
		cash := request.Account.CashForOrder(order)
		unitCost := quote.Price * (1 + commissionRate + transferRate + b.slippage())
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

	price := b.fillPrice(order.Side, quote.Price, order.LimitPrice)
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
	// 部分成交：金额越大越难一次吃完。同一订单多次撮合的比例是确定的
	// （见 fillRatio），否则同一份回放在两次运行里会得到不同结果。
	if ratio := b.fillRatio(order, float64(quantity)*price); ratio < 1 {
		partial := roundSharesDown(int(float64(quantity)*ratio), MinBoardLot(order.Symbol))
		if partial <= 0 {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("盘口深度不足，%d 股无法成交", quantity)}, nil
		}
		quantity = partial
	}
	status := OrderStatusFilled
	if quantity < order.Quantity {
		status = OrderStatusPartial
	}
	return BrokerResult{Status: status, FilledQuantity: quantity, Price: price, Fee: fee}, nil
}

// slippage 返回滑点比例，非法配置按默认值处理。
func (b *PaperBroker) slippage() float64 {
	if b.SlippageBPS < 0 || b.SlippageBPS > 100 {
		return defaultSlippageBPS / 10000
	}
	return b.SlippageBPS / 10000
}

// fillPrice 在限价约束内应用滑点：买入往上滑、卖出往下滑。
//
// 滑点方向永远对自己不利，这是刻意的——反方向的滑点等于内幕信息，
// 只存在于"想让回测好看"的实现里。
func (b *PaperBroker) fillPrice(side Side, market, limit float64) float64 {
	slip := b.slippage()
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

// fillRatio 返回本次可成交比例（0–1）。
//
// 用订单 ID 的哈希做确定性抖动：同一笔单在"资金反算"或"重放"时得到同一个比例，
// 不同订单之间又不会全部卡在同一个整数比例上。用随机数会让回放不可复现，
// 而"这次为什么只成交了 60%"必须能被重现，否则无法判断是模型问题还是撮合问题。
func (b *PaperBroker) fillRatio(order Order, notional float64) float64 {
	base := 1.0
	switch {
	case notional > partialFillThresholdLarge:
		base = partialFillRatioLarge
	case notional > partialFillThresholdSmall:
		base = partialFillRatioMedium
	}
	if base >= 1 {
		return 1
	}
	hash := fnv32(order.ID)
	// 在 [base, 1) 之间取一个确定值。
	span := 1 - base
	return base + float64(hash%1000)/1000*span
}

func fnv32(value string) uint32 {
	hash := uint32(2166136261)
	for i := 0; i < len(value); i++ {
		hash ^= uint32(value[i])
		hash *= 16777619
	}
	return hash
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
