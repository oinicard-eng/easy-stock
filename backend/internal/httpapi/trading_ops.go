package httpapi

import (
	"context"
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

// tradingTimeline 回放某一天的时间线。
//
// 它把循环、计划、裁决、委托、事件、复盘按时间摊平成一条线，
// 用于回答"那天到底发生了什么"——这是排障时最先要建立的东西。
func (s *Server) tradingTimeline(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	query := r.URL.Query()
	day := time.Now()
	if raw := strings.TrimSpace(query.Get("day")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.FixedZone("CST", 8*3600))
		if err != nil {
			writeError(w, http.StatusBadRequest, "day 必须是 YYYY-MM-DD")
			return
		}
		day = parsed
	}
	timeline, err := s.trading.Timeline(r.Context(), query.Get("account_id"), day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": timeline})
}

// tradingAlerts 返回告警投递记录，新的在前。
//
// 它回答的问题和 events 不同：events 说"发生了什么"，
// alerts 说"这件事有没有送到人手上"。无人值守时后者才是关键——
// 出事但没通知，和没出事，从外面看是一样的。
func (s *Server) tradingAlerts(w http.ResponseWriter, r *http.Request) {
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
	records, err := s.trading.Store().ListAlerts(r.Context(),
		query.Get("account_id"), query.Get("status"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	manager := s.trading.Alerts()
	stats := map[string]int64{}
	channels := []string{}
	if manager != nil {
		stats = manager.Stats()
		channels = manager.Channels()
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"records":  records,
		"channels": channels,
		"stats":    stats,
	}})
}

// tradingAlertTest 主动发一条测试告警。
//
// 没有这个入口，人配完 webhook 只能等真出事才知道配错了，
// 而"通知配错了"恰恰是最该被立刻发现的问题。
func (s *Server) tradingAlertTest(w http.ResponseWriter, r *http.Request) {
	var manager *trading.AlertManager
	if s.trading != nil {
		manager = s.trading.Alerts()
	}
	if manager == nil {
		writeError(w, http.StatusServiceUnavailable, "告警未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sent, err := manager.Test(ctx)
	result := map[string]any{"delivered": sent}
	if err != nil {
		result["error"] = err.Error()
		writeJSON(w, http.StatusBadGateway, map[string]any{"data": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": result})
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

// tradingRisk 返回组合级风险状态：净值高点、回撤、总闸。
//
// 它与 health 的区别：health 是"系统还能不能自己跑"，risk 是"账户现在处在
// 什么保护档位"。监控要看前者，交易员看后者。
func (s *Server) tradingRisk(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	state, control, err := s.trading.RiskView(r.Context(), r.URL.Query().Get("account_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"drawdown": state,
		"control":  control,
	}})
}

// tradingDrawdownRelease 人工解除回撤熔断，必须带原因。
//
// 与 breaker/reset 分开而不是合并成一个"解除"按钮：两者解除的是不同事实，
// 一个是"今天亏够了"，一个是"距高点掉太多"。合并后事件流里只会出现
// 一句"已解除"，事后无法判断当时按下的是哪一条防线。
func (s *Server) tradingDrawdownRelease(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		AccountID string `json:"account_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	state, err := s.trading.ReleaseDrawdown(r.Context(), body.AccountID, body.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": state})
}
