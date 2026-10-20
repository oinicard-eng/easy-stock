package trading

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 告警投递记录。它和 trading_events 的区别是：
// events 记"发生了什么"，alerts 记"这件事有没有被送出去"。
//
// 分开存的原因是两者的可靠性要求相反：事件写失败可以忽略，
// 告警投递失败必须留下证据——否则人会以为"没收到通知就是没出事"。
const alertsSchema = `
	CREATE TABLE IF NOT EXISTS trading_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL DEFAULT '',
		severity TEXT NOT NULL,
		kind TEXT NOT NULL,
		symbol TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL,
		channel TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		merged INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_trading_alerts_created ON trading_alerts(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_trading_alerts_kind ON trading_alerts(kind, created_at DESC);
`

func (s *Store) migrateAlerts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, alertsSchema)
	return err
}

// Record 是一条告警投递记录。
type Record struct {
	ID        int64     `json:"id"`
	AccountID string    `json:"account_id"`
	Severity  string    `json:"severity"`
	Kind      string    `json:"kind"`
	Symbol    string    `json:"symbol,omitempty"`
	Message   string    `json:"message"`
	Channel   string    `json:"channel,omitempty"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Merged    int       `json:"merged"`
	CreatedAt time.Time `json:"created_at"`
}

// AppendAlert 写入一条投递记录。
func (s *Store) AppendAlert(ctx context.Context, record Record) error {
	if strings.TrimSpace(record.Kind) == "" {
		return fmt.Errorf("告警类型不能为空")
	}
	if record.Status == "" {
		record.Status = AlertStatusSent
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trading_alerts
		(account_id,severity,kind,symbol,message,channel,status,error,merged,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		record.AccountID, record.Severity, record.Kind, record.Symbol, record.Message,
		record.Channel, record.Status, record.Error, record.Merged, nowText(record.CreatedAt))
	return err
}

// ListAlerts 返回最近的告警记录，新的在前。status 为空表示不过滤。
func (s *Store) ListAlerts(ctx context.Context, accountID, status string, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id,account_id,severity,kind,symbol,message,channel,status,error,merged,created_at
		FROM trading_alerts WHERE 1=1`
	args := []any{}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND account_id=?`
		args = append(args, accountID)
	}
	if strings.TrimSpace(status) != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]Record, 0, limit)
	for rows.Next() {
		var (
			record  Record
			created string
		)
		if err := rows.Scan(&record.ID, &record.AccountID, &record.Severity, &record.Kind,
			&record.Symbol, &record.Message, &record.Channel, &record.Status, &record.Error,
			&record.Merged, &created); err != nil {
			return nil, err
		}
		record.CreatedAt = parseTime(created)
		records = append(records, record)
	}
	return records, rows.Err()
}

// CountAlertsSince 统计某个时间之后真正送达的告警数，用于健康检查判断
// "告警通道是不是一直在失败"。
func (s *Store) CountAlertsSince(ctx context.Context, accountID, status string, since time.Time) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM trading_alerts WHERE account_id=? AND created_at>=?`
	args := []any{accountID, nowText(since)}
	if strings.TrimSpace(status) != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}
