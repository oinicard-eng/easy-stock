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
//   - 限价买入仅在现价不高于限价时成交，成交价为现价；
//   - 限价卖出仅在现价不低于限价时成交；
//   - 市价单按现价成交，但仍受涨跌停价校验，越界直接拒绝；
//   - 无有效行情时保持挂单，不做任何猜测性成交。
type PaperBroker struct{}

func NewPaperBroker() *PaperBroker { return &PaperBroker{} }

func (b *PaperBroker) Name() string { return "paper" }

func (b *PaperBroker) Submit(_ context.Context, request BrokerOrder) (BrokerResult, error) {
	order := request.Order
	quote := request.Quote
	if quote.Price <= 0 {
		return BrokerResult{Status: OrderStatusPending, Reason: "暂无有效行情，订单保持挂起"}, nil
	}
	percent := PriceLimitPercent(order.Symbol, order.Name)
	if quote.PreviousClose > 0 {
		upper := limitUpPrice(quote.PreviousClose, percent)
		lower := limitDownPrice(quote.PreviousClose, percent)
		if order.OrderType == OrderTypeLimit && (order.LimitPrice > upper || order.LimitPrice < lower) {
			return BrokerResult{Status: OrderStatusRejected, Reason: fmt.Sprintf("限价 %.2f 超出涨跌停区间 %.2f–%.2f", order.LimitPrice, lower, upper)}, nil
		}
	}

	quantity := order.Quantity
	// 撮合可花的钱是「账面现金减其他挂单冻结」，本单自身冻结不算在内。
	if order.Side == SideBuy {
		cash := request.Account.CashForOrder(order)
		unitCost := quote.Price * (1 + commissionRate + transferRate)
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
	case SideSell:
		if order.OrderType == OrderTypeLimit && quote.Price < order.LimitPrice {
			return BrokerResult{Status: OrderStatusPending, Reason: fmt.Sprintf("现价 %.2f 低于限价 %.2f，等待反弹", quote.Price, order.LimitPrice)}, nil
		}
	default:
		return BrokerResult{Status: OrderStatusRejected, Reason: "未知买卖方向"}, nil
	}

	price := quote.Price
	fee := TradeFee(order.Side, float64(quantity)*price)
	if order.Side == SideBuy {
		total := float64(quantity)*price + fee
		if total > request.Account.CashForOrder(order)+0.001 {
			return BrokerResult{Status: OrderStatusRejected, Reason: "可用资金不足以覆盖成交金额与费用"}, nil
		}
	}
	status := OrderStatusFilled
	if quantity < order.Quantity {
		status = OrderStatusPartial
	}
	return BrokerResult{Status: status, FilledQuantity: quantity, Price: price, Fee: fee}, nil
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
