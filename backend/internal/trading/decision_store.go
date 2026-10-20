package trading

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 裁决流水与计划分表存放，理由见 Decision 的注释：
// 计划的状态会变，裁决是当时的事实，二者的生命周期不同。
const decisionsSchema = `
	CREATE TABLE IF NOT EXISTS trading_decisions (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		plan_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		content_json TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_trading_decisions_account ON trading_decisions(account_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_trading_decisions_plan ON trading_decisions(plan_id);
`

// SaveDecision 写入一条裁决。裁决一经写入不再修改：它记录的是"当时为什么这么下单"，
// 事后修改它就等于篡改历史。需要补充结论时写新的记录（复盘），而不是回头改这条。
func (s *Store) SaveDecision(ctx context.Context, decision Decision) error {
	if decision.ID == "" {
		return errors.New("裁决缺少 ID")
	}
	payload, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	takenAt := decision.TakenAt
	if takenAt.IsZero() {
		takenAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO trading_decisions(id,account_id,plan_id,created_at,content_json)
		VALUES(?,?,?,?,?)`, decision.ID, decision.AccountID, decision.PlanID, nowText(takenAt), string(payload))
	return err
}

func (s *Store) GetDecision(ctx context.Context, id string) (Decision, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_decisions WHERE id=?`, id).Scan(&payload)
	if err != nil {
		return Decision{}, err
	}
	var decision Decision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func (s *Store) GetDecisionByPlan(ctx context.Context, planID string) (Decision, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT content_json FROM trading_decisions WHERE plan_id=? ORDER BY created_at DESC LIMIT 1`, planID).Scan(&payload)
	if err != nil {
		return Decision{}, err
	}
	var decision Decision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

// ListDecisions 返回最近的裁决列表，新的在前。
func (s *Store) ListDecisions(ctx context.Context, accountID string, limit int) ([]Decision, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT content_json FROM trading_decisions WHERE account_id=? ORDER BY created_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := []Decision{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var decision Decision
		if err := json.Unmarshal([]byte(payload), &decision); err != nil {
			continue
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// DecisionCount 返回账户累计裁决轮次。复盘样本量下限要用它判断，
// 而不是靠"感觉最近跑了不少次"。
func (s *Store) DecisionCount(ctx context.Context, accountID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM trading_decisions WHERE account_id=?`, accountID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return count, err
}
