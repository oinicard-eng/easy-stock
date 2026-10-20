package httpapi

// 情景记忆与调研回放的读取入口。
//
// 两者都是"事后可查"，不接受任何写入数字的请求：
// 记忆由平仓账本推导，调研步骤由生成过程写死，人工只能触发补齐，不能编辑内容。
// 允许编辑等于允许篡改"当时到底看到了什么"，那会让复盘失去证据价值。

import (
	"net/http"
	"strconv"
	"strings"

	"easy-stock/backend/internal/trading"
)

// tradingMemory 返回情景记忆，支持按标的/板块/市场状态/离场原因/结果过滤。
func (s *Server) tradingMemory(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	query := r.URL.Query()
	limit := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	memories, err := s.trading.SearchMemories(r.Context(), trading.MemoryQuery{
		AccountID: strings.TrimSpace(query.Get("account_id")),
		Symbol:    strings.TrimSpace(query.Get("symbol")),
		Sector:    strings.TrimSpace(query.Get("sector")),
		Regime:    strings.ToLower(strings.TrimSpace(query.Get("regime"))),
		Trigger:   strings.ToLower(strings.TrimSpace(query.Get("trigger"))),
		Outcome:   strings.ToLower(strings.TrimSpace(query.Get("outcome"))),
		Keyword:   strings.TrimSpace(query.Get("keyword")),
		Limit:     limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": memories})
}

// tradingMemorySync 手动补齐记忆（补历史用），返回新增条数。
func (s *Server) tradingMemorySync(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	added, err := s.trading.SyncMemories(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, _ := s.trading.MemoryStats(r.Context(), accountID)
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"added": added, "total": total}})
}

// tradingPlanResearch 返回某次计划生成前的调研步骤。
//
// 单独开接口而不是塞进计划详情：步骤可达数千字符，而计划列表页并不需要它。
func (s *Server) tradingPlanResearch(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	planID := strings.TrimSpace(r.PathValue("id"))
	if planID == "" {
		writeError(w, http.StatusBadRequest, "缺少计划 ID")
		return
	}
	steps, err := s.trading.ResearchSteps(r.Context(), planID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": steps})
}
