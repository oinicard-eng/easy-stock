package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"easy-stock/backend/internal/trading"
)

type tradingAccountResponse struct {
	Account   trading.Account    `json:"account"`
	Positions []trading.Position `json:"positions"`
	Control   trading.Control    `json:"control"`
	Broker    string             `json:"broker"`
	Limits    trading.RiskRules  `json:"limits"`
}

// writeAccountView 统一账户视图的返回，避免三处 handler 各自拼装导致字段不一致。
func (s *Server) writeAccountView(w http.ResponseWriter, r *http.Request, accountID string, status int) {
	account, positions, err := s.trading.AccountView(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	control, err := s.trading.Control(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, status, map[string]any{"data": tradingAccountResponse{
		Account: account, Positions: positions, Control: control,
		Broker: s.trading.BrokerName(), Limits: trading.DefaultRiskRules(),
	}})
}

func (s *Server) tradingAccount(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	s.writeAccountView(w, r, r.URL.Query().Get("account_id"), http.StatusOK)
}

func (s *Server) tradingResetAccount(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		AccountID   string  `json:"account_id"`
		InitialCash float64 `json:"initial_cash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	account, _, err := s.trading.ResetAccount(r.Context(), body.AccountID, body.InitialCash)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeAccountView(w, r, account.ID, http.StatusOK)
}

// tradingControlGet 返回账户总闸（暂停买入 / 全部停止）。
func (s *Server) tradingControlGet(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	control, err := s.trading.Control(r.Context(), r.URL.Query().Get("account_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": control})
}

// tradingControlSet 修改总闸。打开 kill_switch 会同时暂停买入。
func (s *Server) tradingControlSet(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		AccountID   string `json:"account_id"`
		PauseBuying bool   `json:"pause_buying"`
		KillSwitch  bool   `json:"kill_switch"`
		Reason      string `json:"reason"`
	}
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	control, err := s.trading.SetControl(r.Context(), trading.Control{
		AccountID: body.AccountID, PauseBuying: body.PauseBuying,
		KillSwitch: body.KillSwitch, Reason: body.Reason,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": control})
}

// tradingReconcile 对账冻结额与挂单。默认只报告，repair=true 时才回写账本。
// tradingMarket 返回当前市场认知：状态、广度指标、行业强度与仓位预算。
// 这些数字与生成计划时用的是同一套计算，看到的即决策依据。
func (s *Server) tradingMarket(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	// 用账户实际总资产筛选候选，保证"看到的"与"能买的"一致。
	equity := 0.0
	if account, _, err := s.trading.AccountView(r.Context(), r.URL.Query().Get("account_id")); err == nil {
		equity = account.Equity
	}
	market, board, err := s.trading.MarketStatus(r.Context(), 0, equity)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"market":       market,
		"source_count": board.SourceCount,
	}})
}

// tradingUniverse 返回系统自动选出的候选池，含每只标的的入选理由与被排除项。
func (s *Server) tradingUniverse(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if limit <= 0 || limit > 50 {
		limit = 0
	}
	equity := 0.0
	if account, _, err := s.trading.AccountView(r.Context(), r.URL.Query().Get("account_id")); err == nil {
		equity = account.Equity
	}
	_, board, err := s.trading.MarketStatus(r.Context(), limit, equity)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": board})
}

func (s *Server) tradingReconcile(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	repair, _ := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get("repair")))
	report, err := s.trading.Reconcile(r.Context(), r.URL.Query().Get("account_id"), repair)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": report})
}

func (s *Server) tradingPlanCreate(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request trading.PlanRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	plan, err := s.trading.StartPlan(r.Context(), request)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, trading.ErrPlanRunning) || errors.Is(err, trading.ErrKillSwitch) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": plan})
}

func (s *Server) tradingPlanList(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	limit := 10
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 50 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 50")
			return
		}
		limit = parsed
	}
	plans, err := s.trading.ListPlans(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": plans})
}

func (s *Server) tradingPlanGet(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	plan, err := s.trading.GetPlan(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if errors.Is(err, trading.ErrNoTradingPlan) || errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "未找到交易计划")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": plan})
}

func (s *Server) tradingPlanExecute(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	plan, err := s.trading.Execute(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if errors.Is(err, trading.ErrKillSwitch) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, trading.ErrPlanNotDraft) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, trading.ErrNoTradingPlan) {
		writeError(w, http.StatusNotFound, "未找到交易计划")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": plan})
}

func (s *Server) tradingPlanReject(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	plan, err := s.trading.RejectPlan(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if errors.Is(err, trading.ErrPlanNotDraft) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": plan})
}

func (s *Server) tradingMatchPending(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	orders, err := s.trading.MatchPending(r.Context(), r.URL.Query().Get("account_id"))
	if errors.Is(err, trading.ErrKillSwitch) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": orders})
}

func (s *Server) tradingOrderCancel(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	order, err := s.trading.CancelOrder(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": order})
}

// tradingDecisionList 返回裁决流水。它是"每次判断可观察"的查询入口：
// 计划的 orders 只说明下了什么；裁决说明为什么是这些、为什么不是那些。
func (s *Server) tradingDecisionList(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
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
	// 带 plan_id 时按计划精确取一条，前端查看历史计划详情用这条路径，
	// 不需要先在列表里翻。
	if planID := strings.TrimSpace(r.URL.Query().Get("plan_id")); planID != "" {
		decision, err := s.trading.Decision(r.Context(), "", planID)
		if errors.Is(err, trading.ErrNoDecision) {
			writeError(w, http.StatusNotFound, "该计划没有裁决记录")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": decision})
		return
	}
	decisions, err := s.trading.Decisions(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": decisions})
}

// tradingDecisionGet 按裁决 ID 返回单条裁决。
func (s *Server) tradingDecisionGet(w http.ResponseWriter, r *http.Request) {
	if s.trading == nil {
		writeError(w, http.StatusServiceUnavailable, "交易服务不可用")
		return
	}
	decision, err := s.trading.Decision(r.Context(), strings.TrimSpace(r.PathValue("id")), "")
	if errors.Is(err, trading.ErrNoDecision) || errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "未找到裁决记录")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": decision})
}
