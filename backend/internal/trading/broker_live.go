package trading

// LiveBroker 是实盘通道的 REST 参考适配器。
//
// 它的存在回答一个问题："接实盘还差什么"。答案不再是"整个执行层"，
// 而只是"一个说这家券商方言的适配器"——委托、查单、撤单三个动作的
// HTTP 契约在这里已经定死并用假券商服务器测过。
//
// 契约（任何厂商接入时按此实现服务端，或写一层翻译）：
//
//	POST {endpoint}/orders
//	  请求头：Authorization: Bearer {token}、X-Client-Order-Id: {本地单号}
//	  请求体：{"client_order_id":"...","symbol":"600000.SH","side":"buy",
//	           "order_type":"limit","quantity":100,"limit_price":10.10}
//	  2xx 响应：{"accepted":true,"order_id":"券商单号","message":""}
//	  accepted=false 或 4xx/5xx：受理失败，message 必须是可直接展示的中文原因
//
//	GET {endpoint}/orders/{client_order_id}
//	  2xx 响应：{"status":"pending|filled|partially_filled|rejected|cancelled",
//	           "filled_quantity":累计成交股数,"price":成交均价,"fee":费用,"message":""}
//
//	DELETE {endpoint}/orders/{client_order_id}
//	  2xx 响应：{"accepted":true,"reason":"","filled_quantity":0,"price":0}
//	  accepted=false 表示券商拒撤（通常已成交），reason 必须说明。
//
// 两条纪律：
//  1. 幂等。同一 client_order_id 重复提交必须返回同一笔委托，不得重复下单——
//     网络超时后的重试是常态，不是异常。
//  2. 受理与成交分离。Submit 成功只代表"券商收了"，成交一律走 Query 对账，
//     绝不在 Submit 里自作主张报成交。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LiveBrokerConfig 是实盘通道的连接配置。
type LiveBrokerConfig struct {
	// Endpoint 是券商适配服务的根地址，如 https://broker-gw.internal。
	Endpoint string
	// Token 是鉴权令牌，以 Bearer 方式放在请求头。
	Token string
	// Timeout 是单次 HTTP 调用的超时。0 取默认 10 秒。
	Timeout time.Duration
}

// Validate 检查配置完整性。地址与令牌缺一不可：
// 半套配置比没有配置更危险——它会让人以为实盘已经接好。
func (c LiveBrokerConfig) Validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("实盘通道缺少券商服务地址（A_STOCK_TRADING_BROKER_ENDPOINT）")
	}
	if !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("实盘通道地址必须以 http:// 或 https:// 开头，实际 %q", c.Endpoint)
	}
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("实盘通道缺少鉴权令牌（A_STOCK_TRADING_BROKER_TOKEN）")
	}
	return nil
}

// LiveBroker 按上述契约与券商适配服务通信。
type LiveBroker struct {
	config LiveBrokerConfig
	client *http.Client
}

// NewLiveBroker 构造实盘通道。配置不完整直接报错，而不是带着半套配置启动。
func NewLiveBroker(config LiveBrokerConfig) (*LiveBroker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &LiveBroker{
		config: config,
		client: &http.Client{Timeout: timeout},
	}, nil
}

func (b *LiveBroker) Name() string { return "live" }

// QueryChannel 表示"受理与成交分离"的通道：委托状态要靠轮询拿回来。
// 模拟盘同步撮合，不实现它；实盘必须实现。
type QueryChannel interface {
	// Query 返回通道单号对应委托的当前状态。filled_quantity 是累计值，
	// 增量由服务层用「累计 − 已记」算出，重复轮询是安全的。
	Query(ctx context.Context, channelOrderID string) (BrokerResult, error)
}

func (b *LiveBroker) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化委托请求失败：%w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.config.Endpoint, "/")+path, reader)
	if err != nil {
		return nil, fmt.Errorf("构造券商请求失败：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.config.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("券商服务不可达：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取券商响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if message == "" {
			message = resp.Status
		}
		return raw, fmt.Errorf("券商返回 %s：%s", resp.Status, message)
	}
	return raw, nil
}

// Submit 把委托推给券商。受理成功返回挂起态与通道单号；
// 券商明确拒绝返回 rejected；网络或服务端故障返回 error（订单保持挂起，等待重试）。
func (b *LiveBroker) Submit(ctx context.Context, request BrokerOrder) (BrokerResult, error) {
	order := request.Order
	side := strings.ToLower(string(order.Side))
	payload := map[string]any{
		"client_order_id": order.ID,
		"symbol":          order.Symbol,
		"side":            side,
		"order_type":      strings.ToLower(string(order.OrderType)),
		"quantity":        order.Quantity,
		"limit_price":     order.LimitPrice,
	}
	raw, err := b.do(ctx, http.MethodPost, "/orders", payload)
	if err != nil {
		// 券商 4xx 属于确定性拒绝（参数错、权限不足），转成 rejected 落库；
		// 网络/5xx 是暂时性故障，保持 error 让订单留在挂起态。
		if strings.Contains(err.Error(), "券商返回 4") {
			return BrokerResult{Status: OrderStatusRejected, Reason: err.Error()}, nil
		}
		return BrokerResult{}, err
	}
	var accepted struct {
		Accepted bool   `json:"accepted"`
		OrderID  string `json:"order_id"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		return BrokerResult{}, fmt.Errorf("解析券商受理响应失败：%w", err)
	}
	if !accepted.Accepted {
		reason := firstNonEmpty(strings.TrimSpace(accepted.Message), "券商未说明拒绝原因")
		return BrokerResult{Status: OrderStatusRejected, Reason: reason}, nil
	}
	if strings.TrimSpace(accepted.OrderID) == "" {
		return BrokerResult{}, fmt.Errorf("券商受理响应缺少 order_id，无法对账，拒绝继续")
	}
	return BrokerResult{
		Status:         OrderStatusPending,
		ChannelOrderID: accepted.OrderID,
		Reason:         "券商已受理，等待成交回报",
	}, nil
}

// Query 轮询委托状态。status 映射失败时按挂起处理并携带原始值：
// 未知状态不能当作成交，也不能当作拒绝——钱的事，不知道就是不知道。
func (b *LiveBroker) Query(ctx context.Context, channelOrderID string) (BrokerResult, error) {
	if strings.TrimSpace(channelOrderID) == "" {
		return BrokerResult{}, fmt.Errorf("通道单号为空，无法查询")
	}
	raw, err := b.do(ctx, http.MethodGet, "/orders/"+channelOrderID, nil)
	if err != nil {
		return BrokerResult{}, err
	}
	var report struct {
		Status         string  `json:"status"`
		FilledQuantity int     `json:"filled_quantity"`
		Price          float64 `json:"price"`
		Fee            float64 `json:"fee"`
		Message        string  `json:"message"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return BrokerResult{}, fmt.Errorf("解析券商回报失败：%w", err)
	}
	result := BrokerResult{
		FilledQuantity: report.FilledQuantity,
		Price:          report.Price,
		Fee:            report.Fee,
		Reason:         strings.TrimSpace(report.Message),
		ChannelOrderID: channelOrderID,
	}
	switch strings.ToLower(strings.TrimSpace(report.Status)) {
	case "filled":
		result.Status = OrderStatusFilled
	case "partially_filled":
		result.Status = OrderStatusPartial
	case "pending":
		result.Status = OrderStatusPending
	case "rejected":
		result.Status = OrderStatusRejected
	case "cancelled":
		result.Status = OrderStatusCancelled
	default:
		result.Status = OrderStatusPending
		if result.Reason == "" {
			result.Reason = "券商回报了未知状态 " + report.Status
		}
	}
	return result, nil
}

// Cancel 向券商撤单。拒撤时把成交信息带回来，由服务层决定按成交收口。
func (b *LiveBroker) Cancel(ctx context.Context, request CancelRequest) (CancelResult, error) {
	if strings.TrimSpace(request.ChannelOrderID) == "" {
		return CancelResult{}, fmt.Errorf("订单没有通道单号，无法向券商撤单")
	}
	raw, err := b.do(ctx, http.MethodDelete, "/orders/"+request.ChannelOrderID, nil)
	if err != nil {
		return CancelResult{}, err
	}
	var result struct {
		Accepted       bool    `json:"accepted"`
		Reason         string  `json:"reason"`
		FilledQuantity int     `json:"filled_quantity"`
		Price          float64 `json:"price"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return CancelResult{}, fmt.Errorf("解析券商撤单响应失败：%w", err)
	}
	return CancelResult{
		Accepted:    result.Accepted,
		Reason:      strings.TrimSpace(result.Reason),
		FilledQty:   result.FilledQuantity,
		FilledPrice: result.Price,
	}, nil
}
