package trading

import (
	"context"
	"database/sql"
	"time"
)

// trading_state：进程级小状态的持久化 KV。
//
// 变化检测指纹、事件通道的权益基线此前都放在内存里。它们共同的问题是
// **崩溃重启后的盲区**：重启后第一轮必然重新调一次模型、组合级异动
// 15 分钟内不判断。停机时间本身不是风险（窗口检查会兜住），但一次
// OOM 或更新重启，恰好发生在行情剧烈波动时，盲区就落在最不该有盲区的时刻。
//
// 放 KV 而不是各自建表：这些状态的共同特征是"丢了能自愈、只是多花一次
// 代价"，不值得为每个状态建一张带迁移的表。

const stateKVSchema = `
	CREATE TABLE IF NOT EXISTS trading_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
`

func (s *Store) migrateStateKV(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, stateKVSchema)
	return err
}

// GetState 读取一个键。不存在时返回空串与 false，不返回错误。
func (s *Store) GetState(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM trading_state WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetState 幂等写入一个键。
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trading_state(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, nowText(time.Now().UTC()))
	return err
}
