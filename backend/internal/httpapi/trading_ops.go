package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"easy-stock/backend/internal/trading"
)

// tradingHealth 返回无人值守所需的健康快照。
//
// 状态码只区分两档：healthy/warning 都返回 200，critical 返回 503。
// 这样外部监控只要看状态码就能判断"要不要叫醒人"，
// 不需要解析 body 里的业务字段——监控脚本解析业务字段是最容易写错的一环。
func (s *Server) tradingHealth(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	beat := trading.Heartbeat{}
	if s.autopilot != nil {
		beat = s.autopilot.Heartbeat()
	}
	health, err := s.trading.Health(r.Context(), r.URL.Query().Get("account_id"), beat)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if health.Status == trading.HealthCritical {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"data": health})
}

// tradingEvents 返回最近的运行事件，新的在前。供页面与排障使用。
func (s *Server) tradingEvents(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	query := r.URL.Query()
	limit := 100
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	events, err := s.trading.Store().ListEvents(r.Context(),
		query.Get("account_id"), query.Get("severity"), query.Get("kind"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": events})
}

// tradingChannel 返回当前执行通道的能力声明。
//
// 为什么单独暴露：实盘与模拟盘的差别不只是"钱是不是真的"，
// 而是"撤单可能失败、回报可能乱序"。前端需要知道这些差异，
// 才能把"订单没成交"和"通道没回话"显示成两种不同的状态。
func (s *Server) tradingChannel(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": s.trading.ChannelInfo()})
}

// tradingReportSubmit 接收一条通道委托回报。
//
// 这是接实盘时唯一的成交入口：券商回调或轮询器把回报推到这里，
// 由账本统一落账。模拟盘不用它——模拟撮合是同步的，不存在回报。
// 幂等由 ReportID 保证，重复投递是安全的。
func (s *Server) tradingReportSubmit(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		ReportID       string  `json:"report_id"`
		OrderID        string  `json:"order_id"`
		ChannelOrderID string  `json:"channel_order_id"`
		Status         string  `json:"status"`
		FilledQuantity int     `json:"filled_quantity"`
		Price          float64 `json:"price"`
		Fee            float64 `json:"fee"`
		Reason         string  `json:"reason"`
		At             string  `json:"at"`
	}
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	report := trading.BrokerReport{
		ReportID:       strings.TrimSpace(body.ReportID),
		OrderID:        strings.TrimSpace(body.OrderID),
		ChannelOrderID: strings.TrimSpace(body.ChannelOrderID),
		Status:         trading.OrderStatus(strings.TrimSpace(body.Status)),
		FilledQuantity: body.FilledQuantity,
		Price:          body.Price,
		Fee:            body.Fee,
		Reason:         body.Reason,
	}
	if raw := strings.TrimSpace(body.At); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			report.At = parsed
		}
	}
	order, err := s.trading.ApplyReport(r.Context(), report)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, trading.ErrReportUnknownOrder) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": order})
}
