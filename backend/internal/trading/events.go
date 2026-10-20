package trading

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 事件等级。只有三级：无人值守时人不会盯着屏幕，等级必须少到"看一眼颜色就知道要不要处理"。
const (
	EventSeverityInfo     = "info"
	EventSeverityWarning  = "warning"
	EventSeverityCritical = "critical"
)

// 事件类型。用常量而不是自由文本，是为了让前端能稳定地按类型分组，
// 也让"同一类问题反复发生"可以被计数，而不是每次都是新的一句话。
const (
	EventAutopilotStarted   = "autopilot.started"
	EventAutopilotFailed    = "autopilot.failed"
	EventAutopilotDegraded  = "autopilot.degraded"
	EventAutopilotRecovered = "autopilot.recovered"
	EventBreakerArmed       = "breaker.armed"
	EventBreakerReset       = "breaker.reset"
	EventLedgerDrift        = "ledger.drift"
	EventLedgerDriftHealed  = "ledger.drift_healed"
	EventOrderExpired       = "order.expired"
	EventBrokerError        = "broker.error"
	EventBrokerRejected     = "broker.rejected"
	EventQuoteFailed        = "quote.failed"
	EventKillSwitch         = "control.kill_switch"
	// 回撤熔断的三个事件。它们是"慢刀子"的唯一留痕：
	// 日亏熔断次日自动失效，回撤不会，所以解除也必须有一条记录。
	EventDrawdownWarn    = "drawdown.warn"
	EventDrawdownStop    = "drawdown.stop"
	EventDrawdownRelease = "drawdown.release"
)

// Event 是一条运行事件。Payload 存结构化补充信息（标的、金额、订单号），
// Message 存"人能直接读懂的一句话"——两者都要，只有 payload 时人看不懂，
// 只有 message 时无法用于后续统计。
type Event struct {
	ID        int64           `json:"id"`
	AccountID string          `json:"account_id"`
	Severity  string          `json:"severity"`
	Kind      string          `json:"kind"`
	Message   string          `json:"message"`
	Symbol    string          `json:"symbol,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

const eventsSchema = `
	CREATE TABLE IF NOT EXISTS trading_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL DEFAULT '',
		severity TEXT NOT NULL,
		kind TEXT NOT NULL,
		message TEXT NOT NULL,
		symbol TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_trading_events_account ON trading_events(account_id, id DESC);
	CREATE INDEX IF NOT EXISTS idx_trading_events_kind ON trading_events(kind, id DESC);
`

func (s *Store) migrateEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, eventsSchema)
	return err
}

// AppendEvent 写入一条事件。
//
// 事件写失败不该中断主流程：熔断已经生效了，却因为"记日志失败"让整个循环报错，
// 是把可观测性放在了正确性之上。所以调用方一律忽略这里的 error，只把失败打回日志。
//
// 写库成功后同步触达 eventSink。钩子由告警模块挂上，它内部只入队不发送，
// 因此这里的同步调用不会把网络耗时带进交易路径。
func (s *Store) AppendEvent(ctx context.Context, event Event) error {
	if strings.TrimSpace(event.Kind) == "" {
		return fmt.Errorf("事件类型不能为空")
	}
	if event.Severity == "" {
		event.Severity = EventSeverityInfo
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	payload := ""
	if len(event.Payload) > 0 {
		payload = string(event.Payload)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trading_events
		(account_id,severity,kind,message,symbol,payload_json,created_at) VALUES (?,?,?,?,?,?,?)`,
		event.AccountID, event.Severity, event.Kind, event.Message, event.Symbol, payload, nowText(event.CreatedAt))
	if err != nil {
		return err
	}
	s.emitEvent(event)
	return nil
}

// SetEventSink 挂上事件外发钩子。传 nil 表示摘除。
//
// 钩子只能有一个：多个消费者会在同一条事件上重复告警，
// 而"同一件事发两遍"比"没人发"更难排查。
func (s *Store) SetEventSink(fn func(Event)) {
	if s == nil {
		return
	}
	if fn == nil {
		s.eventSink.Store(nil)
		return
	}
	s.eventSink.Store(&eventSinkHolder{fn: fn})
}

// emitEvent 调用已挂上的钩子。钩子内部必须自己保证不阻塞、不 panic：
// 这里是交易主路径，任何向外扩散的异常都会变成"记日志把交易搞挂了"。
func (s *Store) emitEvent(event Event) {
	if s == nil {
		return
	}
	holder := s.eventSink.Load()
	if holder == nil || holder.fn == nil {
		return
	}
	holder.fn(event)
}

// ListEvents 返回最近的事件，新的在前。kind 为空表示不过滤类型。
func (s *Store) ListEvents(ctx context.Context, accountID, severity, kind string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id,account_id,severity,kind,message,symbol,payload_json,created_at
		FROM trading_events WHERE 1=1`
	args := []any{}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND account_id=?`
		args = append(args, accountID)
	}
	if strings.TrimSpace(severity) != "" {
		query += ` AND severity=?`
		args = append(args, severity)
	}
	if strings.TrimSpace(kind) != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0, limit)
	for rows.Next() {
		var (
			event   Event
			payload string
			created string
		)
		if err := rows.Scan(&event.ID, &event.AccountID, &event.Severity, &event.Kind,
			&event.Message, &event.Symbol, &payload, &created); err != nil {
			return nil, err
		}
		if payload != "" {
			event.Payload = json.RawMessage(payload)
		}
		event.CreatedAt = parseTime(created)
		events = append(events, event)
	}
	return events, rows.Err()
}

// CountEventsSince 统计某类事件在时间点之后出现了多少次。
// 无人值守时"这条告警是不是新问题"靠它回答，而不是靠人往上翻日志。
func (s *Store) CountEventsSince(ctx context.Context, accountID, kind string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trading_events
		WHERE account_id=? AND kind=? AND created_at>=?`,
		accountID, kind, nowText(since)).Scan(&count)
	return count, err
}

// eventPayload 把任意结构编成 payload，失败时返回 nil 而不是报错：
// payload 是补充信息，编不出来也不能让这条事件消失。
func eventPayload(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}
