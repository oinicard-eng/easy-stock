package trading

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 循环与当日状态两张表必须和计划、订单在同一个库里：
// 复盘要回答"这一轮为什么没买"，就必须把 cycle、plan、decision、orders 放在
// 一个能联表查询的地方。拆到两个库，等于每次复盘都要人工对时间戳。
const cycleSchema = `
	CREATE TABLE IF NOT EXISTS trading_cycles (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		trigger TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT NOT NULL DEFAULT '',
		content_json TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_trading_cycles_account ON trading_cycles(account_id, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_trading_cycles_status ON trading_cycles(status);

	CREATE TABLE IF NOT EXISTS trading_day_state (
		account_id TEXT NOT NULL,
		trading_day TEXT NOT NULL,
		start_equity REAL NOT NULL DEFAULT 0,
		stop_streak INTEGER NOT NULL DEFAULT 0,
		breaker TEXT NOT NULL DEFAULT '',
		breaker_detail TEXT NOT NULL DEFAULT '',
		breaker_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY (account_id, trading_day)
	);

	CREATE TABLE IF NOT EXISTS trading_autopilot (
		account_id TEXT PRIMARY KEY,
		content_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
`

// SaveCycle 写入或更新一轮循环。循环记录是"事实日志"，只允许状态向前推进，
// 但重跑同一轮（幂等恢复）需要能覆盖，所以用 upsert 而不是只插不改。
func (s *Store) SaveCycle(ctx context.Context, cycle Cycle) error {
	if strings.TrimSpace(cycle.ID) == "" {
		return errors.New("循环 ID 不能为空")
	}
	payload, err := json.Marshal(cycle)
	if err != nil {
		return err
	}
	endedAt := ""
	if cycle.EndedAt != nil {
		endedAt = nowText(*cycle.EndedAt)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO trading_cycles(id,account_id,trigger,status,started_at,ended_at,content_json)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, ended_at=excluded.ended_at, content_json=excluded.content_json`,
		cycle.ID, cycle.AccountID, string(cycle.Trigger), string(cycle.Status),
		nowText(cycle.StartedAt), endedAt, string(payload))
	return err
}

// GetCycle 按 ID 读取一轮循环。
func (s *Store) GetCycle(ctx context.Context, id string) (Cycle, error) {
	row := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_cycles WHERE id=?`, strings.TrimSpace(id))
	var payload string
	if err := row.Scan(&payload); err != nil {
		return Cycle{}, err
	}
	return decodeCycle(payload)
}

// ListCycles 返回最近的循环，新的在前。
func (s *Store) ListCycles(ctx context.Context, accountID string, limit int) ([]Cycle, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT content_json FROM trading_cycles WHERE account_id=? ORDER BY started_at DESC LIMIT ?`,
		accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Cycle, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		cycle, err := decodeCycle(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, cycle)
	}
	return out, rows.Err()
}

func decodeCycle(payload string) (Cycle, error) {
	var cycle Cycle
	if err := json.Unmarshal([]byte(payload), &cycle); err != nil {
		return Cycle{}, fmt.Errorf("解析循环记录失败：%w", err)
	}
	return cycle, nil
}

// CountCyclesSince 统计某个时间点之后真正发起过的轮次（跳过的不算）。
// 它是"当日轮次配额"的依据：配额约束的是实际调模型的次数，不是调度次数。
func (s *Store) CountCyclesSince(ctx context.Context, accountID string, since time.Time) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM trading_cycles
		WHERE account_id=? AND started_at>=? AND status IN ('running','done','blocked','failed','aborted')`,
		accountID, nowText(since))
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// AbortRunningCycles 把上次进程留下的"执行中"循环标成 aborted。
//
// 崩溃恢复不能靠"看起来还在跑"来判断：进程已经死了，那一轮永远不会结束。
// 如果不清理，界面上会永远挂着一个 running 的循环，人也就再也分不清
// "系统正在工作"和"系统已经卡死"。
func (s *Store) AbortRunningCycles(ctx context.Context, note string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_cycles WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return 0, err
		}
		payloads = append(payloads, payload)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	aborted := 0
	for _, payload := range payloads {
		cycle, err := decodeCycle(payload)
		if err != nil {
			continue
		}
		cycle.Status = CycleStatusAborted
		cycle.EndedAt = &now
		cycle.Reason = note
		if cycle.DurationMS == 0 {
			cycle.DurationMS = now.Sub(cycle.StartedAt).Milliseconds()
		}
		cycle.Notes = append(cycle.Notes, note)
		if err := s.SaveCycle(ctx, cycle); err != nil {
			return aborted, err
		}
		aborted++
	}
	return aborted, nil
}

// AbortRunningPlans 把上次进程留下的"生成中"计划标为失败。
//
// 与循环恢复是一对：循环被标成 aborted，但它挂着的计划仍停在 running，
// 界面会一直转圈，而且这笔计划永远不会再有人来收尾。
// 恢复时必须两条一起处理，否则等于只修了一半。
func (s *Store) AbortRunningPlans(ctx context.Context, note string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_plans WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return 0, err
		}
		payloads = append(payloads, payload)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	aborted := 0
	for _, payload := range payloads {
		var plan Plan
		if err := json.Unmarshal([]byte(payload), &plan); err != nil {
			continue
		}
		plan.Status = PlanStatusFailed
		plan.ErrorMessage = note
		plan.UpdatedAt = now
		if plan.Orders == nil {
			plan.Orders = []Order{}
		}
		if err := s.SavePlan(ctx, plan); err != nil {
			return aborted, err
		}
		aborted++
	}
	return aborted, nil
}

// GetDayState 读取当日状态。没有记录时返回零值且不报错：
// "今天还没开始"是正常状态，不是错误。
func (s *Store) GetDayState(ctx context.Context, accountID, day string) (DayState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT account_id,trading_day,start_equity,stop_streak,breaker,breaker_detail,breaker_at,updated_at
		FROM trading_day_state WHERE account_id=? AND trading_day=?`, accountID, day)
	var (
		state     DayState
		breakerAt string
		updatedAt string
	)
	if err := row.Scan(&state.AccountID, &state.TradingDay, &state.StartEquity, &state.StopStreak,
		&state.Breaker, &state.BreakerDetail, &breakerAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DayState{AccountID: accountID, TradingDay: day}, nil
		}
		return DayState{}, err
	}
	if breakerAt != "" {
		parsed := parseTime(breakerAt)
		state.BreakerAt = &parsed
	}
	state.UpdatedAt = parseTime(updatedAt)
	return state, nil
}

// EnsureDayState 在当日还没有基准权益时写入一个。
//
// 基准只在当日第一次触达账户时确定，之后任何一轮都不再修改：
// 若允许每轮刷新基准，日亏永远从零开始算，熔断就变成永远不触发。
func (s *Store) EnsureDayState(ctx context.Context, accountID, day string, equity float64, now time.Time) (DayState, error) {
	state, err := s.GetDayState(ctx, accountID, day)
	if err != nil {
		return DayState{}, err
	}
	if state.StartEquity > 0 {
		return state, nil
	}
	if equity <= 0 {
		return state, nil
	}
	state.AccountID = accountID
	state.TradingDay = day
	state.StartEquity = round2(equity)
	state.UpdatedAt = now.UTC()
	if err := s.SaveDayState(ctx, state); err != nil {
		return DayState{}, err
	}
	return state, nil
}

// AdjustStopStreak 调整当日的连续止损计数。
//
// delta 为正数表示又打了一次止损，负数表示归零（传 -1<<30 这种哨兵值而不是
// "设为 0"的另一个方法，是为了让调用方只依赖一个原子操作：先读再写会在
// 两笔同时成交时丢计数，而连败熔断恰恰要在"连续"上准确）。
func (s *Store) AdjustStopStreak(ctx context.Context, accountID, day string, delta int) (DayState, error) {
	if delta == 0 {
		return s.GetDayState(ctx, accountID, day)
	}
	now := nowText(time.Now().UTC())
	var expr string
	if delta > 0 {
		expr = fmt.Sprintf("stop_streak+%d", delta)
	} else {
		expr = "0"
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO trading_day_state(account_id,trading_day,start_equity,stop_streak,breaker,breaker_detail,breaker_at,updated_at)
		VALUES(?,?,0,%d,'','','',?)
		ON CONFLICT(account_id,trading_day) DO UPDATE SET stop_streak=%s, updated_at=excluded.updated_at`,
		maxInt(delta, 0), expr), accountID, day, now); err != nil {
		return DayState{}, err
	}
	return s.GetDayState(ctx, accountID, day)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SaveDayState 写入当日状态（基准权益、熔断事实）。
func (s *Store) SaveDayState(ctx context.Context, state DayState) error {
	breakerAt := ""
	if state.BreakerAt != nil {
		breakerAt = nowText(*state.BreakerAt)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trading_day_state(account_id,trading_day,start_equity,stop_streak,breaker,breaker_detail,breaker_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id,trading_day) DO UPDATE SET
			start_equity=excluded.start_equity,
			stop_streak=excluded.stop_streak,
			breaker=excluded.breaker,
			breaker_detail=excluded.breaker_detail,
			breaker_at=excluded.breaker_at,
			updated_at=excluded.updated_at`,
		state.AccountID, state.TradingDay, state.StartEquity, state.StopStreak,
		state.Breaker, state.BreakerDetail, breakerAt, nowText(state.UpdatedAt))
	return err
}

// GetAutopilotConfig 读取自动循环配置。从未保存过时返回默认值（关闭）。
func (s *Store) GetAutopilotConfig(ctx context.Context, accountID string) (AutopilotConfig, error) {
	row := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_autopilot WHERE account_id=?`, accountID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			config := DefaultAutopilotConfig()
			config.AccountID = accountID
			return config, nil
		}
		return AutopilotConfig{}, err
	}
	config := DefaultAutopilotConfig()
	if err := json.Unmarshal([]byte(payload), &config); err != nil {
		return AutopilotConfig{}, fmt.Errorf("解析自动循环配置失败：%w", err)
	}
	config.AccountID = accountID
	return config.Normalize(), nil
}

// SaveAutopilotConfig 写入自动循环配置。
func (s *Store) SaveAutopilotConfig(ctx context.Context, config AutopilotConfig) (AutopilotConfig, error) {
	config = config.Normalize()
	config.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(config)
	if err != nil {
		return AutopilotConfig{}, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO trading_autopilot(account_id,content_json,updated_at) VALUES(?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET content_json=excluded.content_json, updated_at=excluded.updated_at`,
		config.AccountID, string(payload), nowText(config.UpdatedAt))
	if err != nil {
		return AutopilotConfig{}, err
	}
	return config, nil
}
