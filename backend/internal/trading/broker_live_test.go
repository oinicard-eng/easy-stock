package trading

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 实盘通道的验收标准：受理与成交分离、对账幂等、终态不丢已成交部分、
// 配置不完整时 fail-closed。

func liveTestOrder(id, symbol string) BrokerOrder {
	return BrokerOrder{
		Order: Order{
			ID: id, AccountID: DefaultAccountID, Symbol: symbol, Name: "浦发银行",
			Side: SideBuy, OrderType: OrderTypeLimit, Quantity: 100, LimitPrice: 10.1,
			StopLossPrice: 9.6, Status: OrderStatusPending,
		},
		Quote: QuoteView{Symbol: symbol, Name: "浦发银行", Price: 10, PreviousClose: 9.9, Turnover: 500_000_000},
	}
}

// fakeBrokerServer 是按契约实现的最小券商适配服务。
type fakeBrokerServer struct {
	mu sync.Mutex
	t  *testing.T

	acceptAccepted bool
	acceptOrderID  string
	acceptStatus   int

	queryStatus    string // pending / filled / partially_filled / rejected / cancelled
	queryFilled    int
	queryPrice     float64
	queryFee       float64
	queryMessage   string
	queries        int
	cancelAccepted bool
	seenToken      string
	seenOrder      submitPayload
}

type submitPayload struct {
	ClientOrderID string  `json:"client_order_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	OrderType     string  `json:"order_type"`
	Quantity      int     `json:"quantity"`
	LimitPrice    float64 `json:"limit_price"`
}

func (f *fakeBrokerServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seenToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var payload submitPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			f.t.Fatalf("解析委托请求失败：%v", err)
		}
		f.seenOrder = payload
		if f.acceptStatus != 0 {
			w.WriteHeader(f.acceptStatus)
			_, _ = w.Write([]byte(`{"message":"参数错误"}`))
			return
		}
		accepted := map[string]any{"accepted": f.acceptAccepted, "order_id": f.acceptOrderID, "message": ""}
		if !f.acceptAccepted {
			accepted["message"] = "资金不足"
		}
		_ = json.NewEncoder(w).Encode(accepted)
	})
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": f.queryStatus, "filled_quantity": f.queryFilled,
			"price": f.queryPrice, "fee": f.queryFee, "message": f.queryMessage,
		})
	})
	mux.HandleFunc("DELETE /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seenToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": f.cancelAccepted, "reason": "已成交无法撤单",
			"filled_quantity": f.queryFilled, "price": f.queryPrice,
		})
	})
	return mux
}

func newLiveBrokerForTest(t *testing.T, server *httptest.Server) *LiveBroker {
	t.Helper()
	broker, err := NewLiveBroker(LiveBrokerConfig{Endpoint: server.URL, Token: "test-token", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("构造实盘通道失败：%v", err)
	}
	return broker
}

func TestLiveBrokerConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		config LiveBrokerConfig
		want   string
	}{
		{"缺地址", LiveBrokerConfig{Token: "t"}, "地址"},
		{"缺令牌", LiveBrokerConfig{Endpoint: "https://gw"}, "令牌"},
		{"地址无协议", LiveBrokerConfig{Endpoint: "gw.internal", Token: "t"}, "http"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Validate()
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("应报 %s 缺失，实际 %v", testCase.want, err)
			}
		})
	}
	if _, err := NewLiveBroker(LiveBrokerConfig{Endpoint: "https://gw", Token: "t"}); err != nil {
		t.Fatalf("完整配置应通过：%v", err)
	}
}

func TestLiveBrokerSubmitAcceptAndReject(t *testing.T) {
	fake := &fakeBrokerServer{t: t, acceptAccepted: true, acceptOrderID: "BR-001"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	broker := newLiveBrokerForTest(t, server)

	// 受理成功：挂起 + 通道单号。
	result, err := broker.Submit(context.Background(), liveTestOrder("ord-1", "600000.SH"))
	if err != nil {
		t.Fatalf("受理失败：%v", err)
	}
	if result.Status != OrderStatusPending || result.ChannelOrderID != "BR-001" {
		t.Fatalf("受理结果错误：%+v", result)
	}
	if fake.seenToken != "test-token" {
		t.Fatalf("鉴权头未携带：%q", fake.seenToken)
	}
	if fake.seenOrder.ClientOrderID != "ord-1" || fake.seenOrder.LimitPrice != 10.1 {
		t.Fatalf("委托请求字段错误：%+v", fake.seenOrder)
	}

	// 受理被拒：rejected + 可展示原因。
	fake.mu.Lock()
	fake.acceptAccepted = false
	fake.mu.Unlock()
	result, err = broker.Submit(context.Background(), liveTestOrder("ord-2", "600000.SH"))
	if err != nil {
		t.Fatalf("拒单不应返回 error（那是网络故障的语义）：%v", err)
	}
	if result.Status != OrderStatusRejected || !strings.Contains(result.Reason, "资金不足") {
		t.Fatalf("拒单结果错误：%+v", result)
	}
}

func TestLiveBrokerSubmitHTTPErrorBecomesReject(t *testing.T) {
	server := httptest.NewServer((&fakeBrokerServer{t: t, acceptStatus: http.StatusBadRequest}).handler())
	defer server.Close()
	broker := newLiveBrokerForTest(t, server)
	result, err := broker.Submit(context.Background(), liveTestOrder("ord-1", "600000.SH"))
	if err != nil {
		t.Fatalf("券商 4xx 应转成 rejected 而不是 error：%v", err)
	}
	if result.Status != OrderStatusRejected || !strings.Contains(result.Reason, "参数错误") {
		t.Fatalf("拒单原因错误：%+v", result)
	}
}

func TestLiveBrokerQueryMapsStatuses(t *testing.T) {
	cases := []struct {
		raw  string
		want OrderStatus
	}{
		{"filled", OrderStatusFilled},
		{"partially_filled", OrderStatusPartial},
		{"pending", OrderStatusPending},
		{"rejected", OrderStatusRejected},
		{"cancelled", OrderStatusCancelled},
		{"what_is_this", OrderStatusPending}, // 未知状态按挂起，绝不猜成成交
	}
	for _, testCase := range cases {
		fake := &fakeBrokerServer{t: t, queryStatus: testCase.raw, queryFilled: 40, queryPrice: 10.05}
		server := httptest.NewServer(fake.handler())
		broker := newLiveBrokerForTest(t, server)
		result, err := broker.Query(context.Background(), "BR-001")
		server.Close()
		if err != nil {
			t.Fatalf("查询失败（%s）：%v", testCase.raw, err)
		}
		if result.Status != testCase.want {
			t.Fatalf("状态 %s 应映射为 %s，实际 %s", testCase.raw, testCase.want, result.Status)
		}
		if result.FilledQuantity != 40 || result.Price != 10.05 {
			t.Fatalf("回报字段丢失：%+v", result)
		}
	}
}

func TestLiveBrokerCancel(t *testing.T) {
	fake := &fakeBrokerServer{t: t, cancelAccepted: true, queryFilled: 30, queryPrice: 10.05}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	broker := newLiveBrokerForTest(t, server)
	result, err := broker.Cancel(context.Background(), CancelRequest{ChannelOrderID: "BR-9"})
	if err != nil {
		t.Fatalf("撤单失败：%v", err)
	}
	if !result.Accepted || result.FilledQty != 30 {
		t.Fatalf("撤单结果错误：%+v", result)
	}
	if fake.seenToken != "test-token" {
		t.Fatalf("鉴权头未携带：%q", fake.seenToken)
	}
}

// 全链路：Submit 受理 → 通道单号落库 → 对账拿到成交 → 持仓正确、重复对账不加仓。
func TestLiveChannelSubmitReconcileFillFlow(t *testing.T) {
	fake := &fakeBrokerServer{t: t, acceptAccepted: true, acceptOrderID: "BR-100"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	service := newTestService(t, `{"regime":"进攻","summary":"实盘链路","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":8,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`,
		map[string]foundation.Quote{"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9)})
	service.SetBroker(newLiveBrokerForTest(t, server))
	ctx := context.Background()

	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	executed, err := service.Execute(ctx, plan.ID)
	if err != nil {
		t.Fatalf("执行计划失败：%v", err)
	}
	if len(executed.Orders) != 1 || executed.Orders[0].ChannelOrderID != "BR-100" {
		t.Fatalf("通道单号应落库：%+v", executed.Orders)
	}
	if executed.Orders[0].Status != OrderStatusPending {
		t.Fatalf("受理后应保持挂起：%s", executed.Orders[0].Status)
	}
	if fake.seenOrder.ClientOrderID != executed.Orders[0].ID || fake.seenOrder.Quantity != executed.Orders[0].Quantity {
		t.Fatalf("委托请求与本地订单不一致：%+v", fake.seenOrder)
	}

	// 券商回报全部成交。
	fake.mu.Lock()
	fake.queryStatus = "filled"
	fake.queryFilled = executed.Orders[0].Quantity
	fake.queryPrice = 10.05
	fake.queryFee = 5
	fake.mu.Unlock()

	orders, err := service.MatchPending(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if len(orders) != 1 || orders[0].Status != OrderStatusFilled || orders[0].FilledQuantity != executed.Orders[0].Quantity {
		t.Fatalf("成交未入账：%+v", orders)
	}
	_, positions, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读持仓失败：%v", err)
	}
	found := false
	for _, position := range positions {
		if position.Symbol == "600000.SH" {
			found = true
			if position.Quantity != executed.Orders[0].Quantity {
				t.Fatalf("持仓数量错误：%+v", position)
			}
		}
	}
	if !found {
		t.Fatal("成交后应产生持仓")
	}

	// 重复对账：回报还是"全部成交"，本地已入账，绝不能再加一次仓。
	again, err := service.MatchPending(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("二次对账失败：%v", err)
	}
	for _, order := range again {
		if order.Symbol == "600000.SH" && order.FilledQuantity != executed.Orders[0].Quantity {
			t.Fatalf("重复对账改变了成交数量：%d", order.FilledQuantity)
		}
	}
}

// 券商拒撤（已成交）时，本地必须把已成交部分入账再按撤单收口，不能只改状态。
func TestLiveChannelCancelWithPartialFill(t *testing.T) {
	fake := &fakeBrokerServer{t: t, acceptAccepted: true, acceptOrderID: "BR-200", cancelAccepted: false}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	service := newTestService(t, `{"regime":"进攻","summary":"撤单链路","orders":[{"symbol":"600000.SH","name":"浦发银行","side":"buy","target_weight_percent":8,"order_type":"limit","limit_price":10.1,"stop_loss_price":9.6,"thesis":"测试","confidence":0.7}]}`,
		map[string]foundation.Quote{"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9)})
	service.SetBroker(newLiveBrokerForTest(t, server))
	ctx := context.Background()

	plan, err := service.RunPlanSync(ctx, PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	executed, err := service.Execute(ctx, plan.ID)
	if err != nil {
		t.Fatalf("执行计划失败：%v", err)
	}
	order := executed.Orders[0]

	// 先对账拿到部分成交 60 股，再让券商拒撤并回报累计 60 股。
	fake.mu.Lock()
	fake.queryStatus = "partially_filled"
	fake.queryFilled = 60
	fake.queryPrice = 10.05
	fake.mu.Unlock()
	if _, err := service.MatchPending(ctx, DefaultAccountID); err != nil {
		t.Fatalf("部分成交对账失败：%v", err)
	}
	reloaded, err := service.store.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("读订单失败：%v", err)
	}
	if reloaded.FilledQuantity != 60 {
		t.Fatalf("部分成交应入账 60 股，实际 %d", reloaded.FilledQuantity)
	}

	// 撤单：券商拒撤（已成交），但本地已入账部分应保留，订单按撤单收口。
	if _, err := service.CancelOrder(ctx, order.ID); err == nil {
		t.Fatal("券商拒撤时撤单应报错（本地与通道不一致）")
	}
	// 拒撤后订单应仍可通过对账收口：让回报变成 cancelled。
	fake.mu.Lock()
	fake.queryStatus = "cancelled"
	fake.queryFilled = 60
	fake.mu.Unlock()
	orders, err := service.MatchPending(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("终态对账失败：%v", err)
	}
	if len(orders) != 1 || orders[0].Status != OrderStatusCancelled || orders[0].FilledQuantity != 60 {
		t.Fatalf("终态收口错误：%+v", orders)
	}
	_, positions, err := service.AccountView(ctx, DefaultAccountID)
	if err != nil {
		t.Fatalf("读持仓失败：%v", err)
	}
	hasPosition := false
	for _, position := range positions {
		if position.Symbol == "600000.SH" && position.Quantity > 0 {
			hasPosition = true
		}
	}
	if !hasPosition {
		t.Fatal("撤单前已成交的 60 股必须留在持仓里")
	}
}
