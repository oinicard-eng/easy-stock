package trading

// 情景记忆：一笔交易发生的时候，市场是什么样子、当时的理由是什么、结果如何。
//
// 与 Lesson 的分工：
//   - Lesson 是聚合统计（"止损离场 8 次，胜率 25%"），回答"平均而言"；
//   - Memory 是单笔情境（"3 月 12 日退潮期买入中际旭创，理由是算力板块资金
//     持续流入，两天后 -6% 止损"），回答"那一次到底发生了什么"。
//
// 为什么两者都要：聚合会抹掉情境。模型读到"止损胜率 25%"，最省力的解读是
// "少止损"——这是灾难性的。读到具体情境，才可能推出"退潮期不该追已经涨过
// 一轮的板块"这种可用结论。所以记忆必须保留当时的市场状态与买入理由原文，
// 而不是只留一个结果数字。
//
// 记忆在复盘时生成，不在平仓瞬间生成：平仓那一刻拿不到"买入时的市场状态"，
// 而计划快照里完整存着。事后补写反而更准确，且天然幂等。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/trading/cognition"
)

// Memory 是一笔已平仓交易的完整情境。
type Memory struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	TradeID   string `json:"trade_id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name,omitempty"`
	Sector    string `json:"sector,omitempty"`
	// TradingDay 是平仓日，用于按时间回看。
	TradingDay string `json:"trading_day"`
	// Regime 是买入那一轮的市场状态，取自计划快照而非平仓时刻。
	// 买入时的环境才决定这笔交易该不该做，平仓时的环境只影响怎么卖。
	Regime      string `json:"regime"`
	RegimeLabel string `json:"regime_label"`
	// Thesis 是模型当时的买入理由原文。保留原文而不是摘要：
	// "因为算力板块主力净流入连续三天扩大"和"因为板块强"是完全不同的信息量。
	Thesis       string  `json:"thesis,omitempty"`
	Invalidation string  `json:"invalidation,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
	ExitTrigger  string  `json:"exit_trigger,omitempty"`
	ExitSource   string  `json:"exit_source,omitempty"`
	EntryPrice   float64 `json:"entry_price"`
	ExitPrice    float64 `json:"exit_price"`
	PnL          float64 `json:"pnl"`
	PnLPercent   float64 `json:"pnl_percent"`
	HoldDays     int     `json:"hold_days"`
	// PeakGainPercent 是持仓期间最高价相对成本的涨幅。
	PeakGainPercent float64 `json:"peak_gain_percent"`
	// GiveBackPercent 是从峰值回吐的百分点。正数表示"曾经赚得更多"。
	GiveBackPercent float64 `json:"give_back_percent"`
	// Outcome 只有 win / loss / flat 三档，用固定阈值判定。
	Outcome string `json:"outcome"`
	// Lesson 由代码按规则生成，只描述事实，不给建议。
	// 给建议会让模型把历史结论当成指令，从而绕过本轮的风控约束。
	Lesson    string    `json:"lesson"`
	CreatedAt time.Time `json:"created_at"`
}

const memorySchema = `
	CREATE TABLE IF NOT EXISTS trading_memory (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		trade_id TEXT NOT NULL,
		symbol TEXT NOT NULL,
		sector TEXT NOT NULL DEFAULT '',
		regime TEXT NOT NULL DEFAULT '',
		trigger TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		trading_day TEXT NOT NULL,
		content_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(account_id, trade_id)
	);
	CREATE INDEX IF NOT EXISTS idx_memory_account_day ON trading_memory(account_id, trading_day DESC);
	CREATE INDEX IF NOT EXISTS idx_memory_symbol ON trading_memory(account_id, symbol);
`

func (s *Store) migrateMemory(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, memorySchema)
	return err
}

// MemoryQuery 是记忆检索条件。所有字段都是可选的，零值表示不限制。
//
// 刻意不做向量检索：记忆量级是几百到几千条，结构化过滤加关键词足够，
// 而且"按退潮期 + 止损 + 算力板块"这种组合条件比语义相似更贴近交易问题。
type MemoryQuery struct {
	AccountID string
	Symbol    string
	Sector    string
	Regime    string
	Trigger   string
	Outcome   string
	// Keyword 在买入理由与教训里做包含匹配。
	Keyword string
	Limit   int
}

// SyncMemories 把还没有记忆的平仓记录补齐，返回新增条数。
//
// 幂等：已存在的 trade_id 直接跳过。复盘每天跑一次，重复调用只会得到 0。
// 每次先做一次便宜的差集计数：没有待补的平仓记录时直接返回，
// 这样自动驾驶每轮调用它也不会有任何代价。
func (s *Store) SyncMemories(ctx context.Context, accountID string) (int, error) {
	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trading_closed_trades t
		 WHERE t.account_id=? AND NOT EXISTS (
		     SELECT 1 FROM trading_memory m WHERE m.account_id=t.account_id AND m.trade_id=t.id)`,
		accountID).Scan(&pending); err != nil {
		return 0, err
	}
	if pending == 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_closed_trades WHERE account_id=?`, accountID)
	if err != nil {
		return 0, err
	}
	trades := make([]ClosedTrade, 0, 64)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return 0, err
		}
		var trade ClosedTrade
		if err := json.Unmarshal([]byte(payload), &trade); err != nil {
			continue
		}
		trades = append(trades, trade)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	existing := map[string]bool{}
	known, err := s.db.QueryContext(ctx, `SELECT trade_id FROM trading_memory WHERE account_id=?`, accountID)
	if err != nil {
		return 0, err
	}
	for known.Next() {
		var id string
		if err := known.Scan(&id); err == nil {
			existing[id] = true
		}
	}
	known.Close()

	inserted := 0
	now := time.Now().UTC()
	for _, trade := range trades {
		if existing[trade.ID] {
			continue
		}
		memory := s.buildMemory(ctx, accountID, trade, now)
		payload, err := json.Marshal(memory)
		if err != nil {
			continue
		}
		result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO trading_memory
			(id,account_id,trade_id,symbol,sector,regime,trigger,outcome,trading_day,content_json,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			memory.ID, accountID, memory.TradeID, memory.Symbol, memory.Sector,
			memory.Regime, memory.ExitTrigger, memory.Outcome, memory.TradingDay,
			string(payload), nowText(memory.CreatedAt))
		if err != nil {
			return inserted, err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			inserted++
		}
	}
	return inserted, nil
}

// buildMemory 组装一条记忆。取不到计划快照时退化为只有结果的记忆，
// 并把 Regime 标成 unknown —— 不猜一个"大概是震荡"。
func (s *Store) buildMemory(ctx context.Context, accountID string, trade ClosedTrade, now time.Time) Memory {
	regime := ""
	thesis := ""
	invalidation := ""
	confidence := 0.0
	if plan, ok := s.planForTrade(ctx, trade); ok {
		regime = plan.Regime
		if regime == "" {
			regime = string(plan.Snapshot.Market.Regime)
		}
		for _, order := range plan.Orders {
			if order.Symbol == trade.Symbol && order.Side == SideBuy {
				thesis = firstNonEmpty(thesis, order.Thesis)
				invalidation = firstNonEmpty(invalidation, order.Invalidation)
				if confidence == 0 {
					confidence = order.Confidence
				}
			}
		}
	}
	if regime == "" {
		regime = string(cognition.RegimeUnknown)
	}

	peakGain := 0.0
	if trade.EntryPrice > 0 && trade.PeakPrice > 0 {
		peakGain = round2((trade.PeakPrice/trade.EntryPrice - 1) * 100)
	}
	giveBack := 0.0
	if peakGain > 0 {
		giveBack = round2(peakGain - trade.PnLPercent)
		if giveBack < 0 {
			giveBack = 0
		}
	}

	outcome := "flat"
	switch {
	case trade.PnLPercent > 0.5:
		outcome = "win"
	case trade.PnLPercent < -0.5:
		outcome = "loss"
	}

	memory := Memory{
		ID:              newID("mem"),
		AccountID:       accountID,
		TradeID:         trade.ID,
		Symbol:          trade.Symbol,
		Name:            trade.Name,
		Sector:          trade.Sector,
		TradingDay:      trade.TradingDay,
		Regime:          regime,
		RegimeLabel:     cognition.Regime(regime).Label(),
		Thesis:          thesis,
		Invalidation:    invalidation,
		Confidence:      confidence,
		ExitTrigger:     trade.Trigger,
		ExitSource:      trade.Source,
		EntryPrice:      trade.EntryPrice,
		ExitPrice:       trade.ExitPrice,
		PnL:             trade.PnL,
		PnLPercent:      trade.PnLPercent,
		HoldDays:        trade.HoldDays,
		PeakGainPercent: peakGain,
		GiveBackPercent: giveBack,
		Outcome:         outcome,
		CreatedAt:       now,
	}
	memory.Lesson = memoryLesson(memory, trade.OpenedAt)
	return memory
}

// planForTrade 找到产生这笔买入的计划。
// 优先按 cycle_id 精确匹配；早期数据没有 cycle_id 时，退回"该标的最近一次买入计划"。
func (s *Store) planForTrade(ctx context.Context, trade ClosedTrade) (Plan, bool) {
	query := `SELECT plan_id FROM trading_orders WHERE account_id=? AND symbol=? AND side='buy'`
	args := []any{trade.AccountID, trade.Symbol}
	if trade.CycleID != "" {
		query += ` AND cycle_id=?`
		args = append(args, trade.CycleID)
	}
	query += ` ORDER BY created_at DESC LIMIT 1`

	var planID string
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&planID); err != nil {
		if err != sql.ErrNoRows {
			return Plan{}, false
		}
		return Plan{}, false
	}
	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return Plan{}, false
	}
	return plan, true
}

// memoryLesson 用固定规则生成一句教训。只陈述事实，不写"应该"。
//
// 这条约束很重要：一旦写成"下次不要在退潮期追高"，模型会把它当作本轮指令，
// 而本轮的退潮可能正是唯一的机会。事实描述让它自己判断，建议会替它判断。
func memoryLesson(memory Memory, openedAt *time.Time) string {
	parts := make([]string, 0, 3)
	switch memory.ExitTrigger {
	case "stop_loss", "hard_stop":
		parts = append(parts, fmt.Sprintf("按止损纪律离场，亏 %.2f%%", memory.PnLPercent))
	case "take_profit":
		parts = append(parts, fmt.Sprintf("到达止盈位离场，赚 %.2f%%", memory.PnLPercent))
	case "trail_stop":
		parts = append(parts, fmt.Sprintf("移动止损保住 %.2f%% 利润", memory.PnLPercent))
	case "model_exit":
		parts = append(parts, fmt.Sprintf("模型主动离场，%.2f%%", memory.PnLPercent))
	default:
		parts = append(parts, fmt.Sprintf("离场结果 %.2f%%", memory.PnLPercent))
	}

	// 刚建仓就止损，说明问题在买入时点或止损距离，不在离场执行。
	if memory.Outcome == "loss" && (memory.ExitTrigger == "stop_loss" || memory.ExitTrigger == "hard_stop") && memory.HoldDays <= 1 {
		parts = append(parts, "建仓后一个交易日内即触发止损")
	}
	// 峰值回吐是止盈设置问题的最直接证据：赚过又还回去，和从没赚过是两件事。
	if memory.GiveBackPercent >= 3 && memory.PeakGainPercent > 0 {
		parts = append(parts, fmt.Sprintf("持仓期间最高浮盈 %.2f%%，回吐 %.2f 个百分点", memory.PeakGainPercent, memory.GiveBackPercent))
	}
	if memory.Regime != "" && memory.Regime != string(cognition.RegimeUnknown) {
		parts = append(parts, fmt.Sprintf("买入时市场处于%s期", memory.RegimeLabel))
	}
	return strings.Join(parts, "；")
}

// SearchMemories 按条件召回记忆，新的在前。
func (s *Store) SearchMemories(ctx context.Context, query MemoryQuery) ([]Memory, error) {
	conditions := []string{"account_id=?"}
	args := []any{query.AccountID}
	if value := strings.TrimSpace(query.Symbol); value != "" {
		conditions = append(conditions, "symbol=?")
		args = append(args, strings.ToUpper(value))
	}
	if value := strings.TrimSpace(query.Sector); value != "" {
		conditions = append(conditions, "sector=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(query.Regime); value != "" {
		conditions = append(conditions, "regime=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(query.Trigger); value != "" {
		conditions = append(conditions, "trigger=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(query.Outcome); value != "" {
		conditions = append(conditions, "outcome=?")
		args = append(args, value)
	}
	limit := query.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	sqlText := `SELECT content_json FROM trading_memory WHERE ` + strings.Join(conditions, " AND ") +
		` ORDER BY trading_day DESC, created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	memories := make([]Memory, 0, limit)
	keyword := strings.ToLower(strings.TrimSpace(query.Keyword))
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var memory Memory
		if err := json.Unmarshal([]byte(payload), &memory); err != nil {
			continue
		}
		// 关键词在内存里过滤：只有几到几十条，没必要为它加全文索引。
		if keyword != "" && !strings.Contains(strings.ToLower(memory.Thesis+" "+memory.Lesson+" "+memory.Invalidation), keyword) {
			continue
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

// CountMemories 返回记忆条数，用于健康检查与界面展示。
func (s *Store) CountMemories(ctx context.Context, accountID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trading_memory WHERE account_id=?`, accountID).Scan(&count)
	return count, err
}

// SearchMemories 是 HTTP 层与工具共用的检索入口。
func (s *Service) SearchMemories(ctx context.Context, query MemoryQuery) ([]Memory, error) {
	query.AccountID = s.accountID(query.AccountID)
	return s.store.SearchMemories(ctx, query)
}

// SyncMemories 手动补齐记忆。自动复盘每日会调一次，这里是给"补历史"用的。
func (s *Service) SyncMemories(ctx context.Context, accountID string) (int, error) {
	return s.store.SyncMemories(ctx, s.accountID(accountID))
}

// MemoryStats 返回记忆条数，供界面显示"这个系统记得多少"。
func (s *Service) MemoryStats(ctx context.Context, accountID string) (int, error) {
	return s.store.CountMemories(ctx, s.accountID(accountID))
}
