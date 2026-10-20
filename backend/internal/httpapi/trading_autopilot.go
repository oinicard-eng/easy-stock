package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"easy-stock/backend/internal/trading"
)

// tradingAutopilotStatus 返回自动循环的配置、当日状态与熔断器清单。
func (s *Server) tradingAutopilotStatus(w http.ResponseWriter, r *http.Request) {
	if s.autopilot == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	status, err := s.autopilot.Status(r.Context(), r.URL.Query().Get("account_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": status})
}

// tradingAutopilotConfigure 修改自动循环配置。
// 只允许改这里列出的字段：未知字段一律报错，避免"写错的键静默失效"。
func (s *Server) tradingAutopilotConfigure(w http.ResponseWriter, r *http.Request) {
	if s.autopilot == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		AccountID           string   `json:"account_id"`
		Enabled             *bool    `json:"enabled"`
		AutoExecute         *bool    `json:"auto_execute"`
		IntervalSeconds     *int     `json:"interval_seconds"`
		TraderProfile       string   `json:"trader_profile"`
		Horizon             string   `json:"horizon"`
		MaxDailyLossPercent *float64 `json:"max_daily_loss_percent"`
		BuyCutoff           string   `json:"buy_cutoff"`
		MaxCyclesPerDay     *int     `json:"max_cycles_per_day"`
	}
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// 以当前配置为底再覆盖：部分字段更新不该把其他字段打回默认值。
	current, err := s.autopilot.Status(r.Context(), body.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	config := current.Config
	if body.Enabled != nil {
		config.Enabled = *body.Enabled
	}
	if body.AutoExecute != nil {
		config.AutoExecute = *body.AutoExecute
	}
	if body.IntervalSeconds != nil {
		config.IntervalSeconds = *body.IntervalSeconds
	}
	if strings.TrimSpace(body.TraderProfile) != "" {
		config.TraderProfile = body.TraderProfile
	}
	if strings.TrimSpace(body.Horizon) != "" {
		config.Horizon = body.Horizon
	}
	if body.MaxDailyLossPercent != nil {
		config.MaxDailyLossPercent = *body.MaxDailyLossPercent
	}
	if strings.TrimSpace(body.BuyCutoff) != "" {
		config.BuyCutoff = body.BuyCutoff
	}
	if body.MaxCyclesPerDay != nil {
		config.MaxCyclesPerDay = *body.MaxCyclesPerDay
	}
	saved, err := s.autopilot.Configure(r.Context(), config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := s.autopilot.Status(r.Context(), saved.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": status})
}

// tradingCycleList 返回循环记录。它是「交易员日志」的列表接口。
func (s *Server) tradingCycleList(w http.ResponseWriter, r *http.Request) {
	if s.autopilot == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	cycles, err := s.autopilot.Cycles(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": cycles})
}

// tradingCycleRun 人工触发一轮循环。
//
// 它绕过自动开关与交易时段（人点这一下就是"现在跑一轮"的明确表达），
// 但不绕过 kill switch 与当日熔断——那两条是资本保护，不是偏好设置。
func (s *Server) tradingCycleRun(w http.ResponseWriter, r *http.Request) {
	if s.autopilot == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	cycle, err := s.autopilot.RunCycle(r.Context(), trading.CycleTriggerManual)
	if errors.Is(err, trading.ErrCycleRunning) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		// 循环本身可能已经记录了失败原因，这里仍返回 200 会让人以为成功。
		writeJSON(w, http.StatusBadGateway, map[string]any{"data": cycle, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": cycle})
}

// tradingBreakerReset 人工解除当日熔断，必须带原因。
func (s *Server) tradingBreakerReset(w http.ResponseWriter, r *http.Request) {
	if s.autopilot == nil {
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
	state, err := s.autopilot.ResetBreaker(r.Context(), body.AccountID, body.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": state})
}
