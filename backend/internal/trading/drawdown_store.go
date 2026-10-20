package trading

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 回撤状态单独一张表，而不是塞进 trading_control。
//
// 两者的生命周期不同：control 是"现在许不许买"的开关，会被人和多个熔断器
// 反复改写；risk_state 是"账户走过的净值高点"这条历史事实，只增不减。
// 混在一行里，任何一次开关读写都可能把高点覆盖成 0，而那种丢失是不可逆的：
// 高点一没，回撤永远显示 0%，这条防线就静默失效了。

const riskStateSchema = `
	CREATE TABLE IF NOT EXISTS trading_risk_state (
		account_id TEXT PRIMARY KEY,
		peak_equity REAL NOT NULL DEFAULT 0,
		peak_at TEXT NOT NULL DEFAULT '',
		equity REAL NOT NULL DEFAULT 0,
		drawdown_percent REAL NOT NULL DEFAULT 0,
		level TEXT NOT NULL DEFAULT '',
		warn_percent REAL NOT NULL DEFAULT 8,
		stop_percent REAL NOT NULL DEFAULT 12,
		release_percent REAL NOT NULL DEFAULT 5,
		updated_at TEXT NOT NULL
	);
`

func (s *Store) migrateRiskState(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, riskStateSchema)
	return err
}

// GetRiskState 读取账户风险状态。没有记录时返回带默认阈值的零值，
// 而不是 sql.ErrNoRows：调用方每次都要处理"第一次观察"这一正常情况。
func (s *Store) GetRiskState(ctx context.Context, accountID string) (RiskState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT account_id,peak_equity,peak_at,equity,drawdown_percent,level,
		warn_percent,stop_percent,release_percent,updated_at FROM trading_risk_state WHERE account_id=?`, accountID)
	var (
		state              RiskState
		peakAt, updatedRaw string
	)
	err := row.Scan(&state.AccountID, &state.PeakEquity, &peakAt, &state.Equity, &state.DrawdownPercent,
		&state.Level, &state.Policy.WarnPercent, &state.Policy.StopPercent, &state.Policy.ReleasePercent, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return RiskState{AccountID: accountID, Policy: DefaultDrawdownPolicy()}, nil
	}
	if err != nil {
		return RiskState{}, err
	}
	state.PeakAt = parseTime(peakAt)
	state.UpdatedAt = parseTime(updatedRaw)
	state.Policy = state.Policy.Normalize()
	return state, nil
}

// SaveRiskState 幂等写入风险状态。
func (s *Store) SaveRiskState(ctx context.Context, state RiskState) error {
	if state.AccountID == "" {
		return fmt.Errorf("风险状态缺少账户")
	}
	state.Policy = state.Policy.Normalize()
	updated := state.UpdatedAt
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	peakAt := ""
	if !state.PeakAt.IsZero() {
		peakAt = nowText(state.PeakAt)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trading_risk_state(account_id,peak_equity,peak_at,equity,drawdown_percent,level,
			warn_percent,stop_percent,release_percent,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
			peak_equity=excluded.peak_equity, peak_at=excluded.peak_at, equity=excluded.equity,
			drawdown_percent=excluded.drawdown_percent, level=excluded.level,
			warn_percent=excluded.warn_percent, stop_percent=excluded.stop_percent,
			release_percent=excluded.release_percent, updated_at=excluded.updated_at`,
		state.AccountID, state.PeakEquity, peakAt, state.Equity, state.DrawdownPercent, state.Level,
		state.Policy.WarnPercent, state.Policy.StopPercent, state.Policy.ReleasePercent, nowText(updated))
	return err
}
