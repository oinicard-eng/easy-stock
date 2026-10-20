package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 本文件提供「持仓管理 + 复盘」的读取与触发入口。
//
// 这些接口的共同点：全部只读账本或只触发确定性计算，不接受"改数字"的请求。
// 唯一可写的是防守线（人工设置止损/止盈），因为那是人的决定，不是账本事实。

// tradingPositions 返回持仓，含可卖、冻结、防守线与浮盈。
func (s *Server) tradingPositions(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	_, positions, err := s.trading.AccountView(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": positions})
}

// tradingPositionProtection 设置某只持仓的止损/止盈。
func (s *Server) tradingPositionProtection(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	var body struct {
		AccountID       string  `json:"account_id"`
		StopLossPrice   float64 `json:"stop_loss_price"`
		TakeProfitPrice float64 `json:"take_profit_price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if body.StopLossPrice < 0 || body.TakeProfitPrice < 0 {
		writeError(w, http.StatusBadRequest, "止损价与止盈价不能为负")
		return
	}
	symbol := strings.TrimSpace(r.PathValue("symbol"))
	position, err := s.trading.UpdateProtection(r.Context(), body.AccountID, symbol, body.StopLossPrice, body.TakeProfitPrice)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": position})
}

// tradingProtectiveRun 手动跑一次持仓防守线检查。
// 它不调模型，几秒内返回，可以随时点。
func (s *Server) tradingProtectiveRun(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	run, err := s.trading.RunProtective(r.Context(), r.URL.Query().Get("account_id"), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": run})
}

// tradingClosedTrades 返回最近已实现平仓，供复盘与核查使用。
func (s *Server) tradingClosedTrades(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	trades, err := s.trading.ClosedTrades(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": trades})
}

// tradingPerformance 返回最近 N 天的业绩归因。全部由代码从平仓账本算出。
func (s *Server) tradingPerformance(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 365 {
			writeError(w, http.StatusBadRequest, "days must be between 1 and 365")
			return
		}
		days = parsed
	}
	performance, err := s.trading.Performance(r.Context(), r.URL.Query().Get("account_id"), days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": performance})
}

// tradingReviewList 返回最近的复盘记录，新的在前。
func (s *Server) tradingReviewList(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	limit := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	reviews, err := s.trading.Reviews(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": reviews})
}

// tradingReviewRun 生成（或重算）某个交易日的复盘。幂等。
func (s *Server) tradingReviewRun(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	var body struct {
		AccountID  string `json:"account_id"`
		TradingDay string `json:"trading_day"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体格式错误")
			return
		}
	}
	review, err := s.trading.GenerateReview(r.Context(), body.AccountID, body.TradingDay)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": review})
}

// tradingReviewGet 按交易日读取复盘。
func (s *Server) tradingReviewGet(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	day := strings.TrimSpace(r.PathValue("day"))
	review, ok, err := s.trading.Review(r.Context(), r.URL.Query().Get("account_id"), day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "该交易日没有复盘记录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": review})
}
