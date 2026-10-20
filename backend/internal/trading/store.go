package trading

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"easy-stock/backend/internal/sqliteutil"
	"easy-stock/backend/internal/trading/policy"
)

// DefaultAccountID 是模拟盘主账户。P1 只暴露一个账户，但表结构按多账户设计，
// 后续接入实盘时可用 broker 维度区分。
const DefaultAccountID = "paper-main"

const DefaultInitialCash = 100000.0

type Store struct {
	db *sql.DB
	// eventSink 是事件外发钩子，由告警模块在启动时挂上。
	//
	// 用 atomic.Pointer 而不是互斥锁：它在服务启动时写一次，之后被所有
	// goroutine 并发读，而事件写入发生在交易主路径上，不该为读一个函数指针付锁开销。
	eventSink atomic.Pointer[eventSinkHolder]
}

// eventSinkHolder 包一层是因为 atomic.Pointer 不能安全地存 nil 函数值。
type eventSinkHolder struct {
	fn func(Event)
}

func OpenStore(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = ":memory:"
	}
	dataSource := path
	if path == ":memory:" {
		dataSource = fmt.Sprintf("file:easy-stock-trading-%d?mode=memory&cache=shared", time.Now().UnixNano())
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建交易数据目录失败: %w", err)
	}
	db, err := sqliteutil.Open(dataSource)
	if err != nil {
		return nil, fmt.Errorf("打开交易数据库失败: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS trading_accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			initial_cash REAL NOT NULL,
			cash REAL NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS trading_positions (
			account_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			quantity INTEGER NOT NULL,
			available_quantity INTEGER NOT NULL,
			average_cost REAL NOT NULL,
			last_price REAL NOT NULL DEFAULT 0,
			last_buy_date TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY (account_id, symbol)
		);
		CREATE TABLE IF NOT EXISTS trading_plans (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			status TEXT NOT NULL,
			content_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_trading_plans_account ON trading_plans(account_id, created_at DESC);
		CREATE TABLE IF NOT EXISTS trading_orders (
			id TEXT PRIMARY KEY,
			plan_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			status TEXT NOT NULL,
			content_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_trading_orders_plan ON trading_orders(plan_id);
		CREATE INDEX IF NOT EXISTS idx_trading_orders_status ON trading_orders(status, created_at);
		CREATE TABLE IF NOT EXISTS trading_fills (
			id TEXT PRIMARY KEY,
			order_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			content_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_trading_fills_order ON trading_fills(order_id);
		CREATE TABLE IF NOT EXISTS trading_control (
			account_id TEXT PRIMARY KEY,
			pause_buying INTEGER NOT NULL DEFAULT 0,
			kill_switch INTEGER NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// P2.1 起账本引入挂单冻结。旧库用 ALTER 补齐，保证已有账户不会因为升级而丢数据。
	for _, column := range []struct{ table, name, define string }{
		{"trading_accounts", "reserved_cash", "REAL NOT NULL DEFAULT 0"},
		{"trading_orders", "decision_id", "TEXT NOT NULL DEFAULT ''"},
		{"trading_orders", "cycle_id", "TEXT NOT NULL DEFAULT ''"},
		// P3：卖出冻结与持仓防守线。可卖股数也必须冻结，否则两笔卖单会各卖一次。
		{"trading_positions", "reserved_quantity", "INTEGER NOT NULL DEFAULT 0"},
		{"trading_positions", "stop_loss_price", "REAL NOT NULL DEFAULT 0"},
		{"trading_positions", "take_profit_price", "REAL NOT NULL DEFAULT 0"},
		{"trading_positions", "peak_price", "REAL NOT NULL DEFAULT 0"},
		{"trading_positions", "reduce_count", "INTEGER NOT NULL DEFAULT 0"},
		{"trading_positions", "sector", "TEXT NOT NULL DEFAULT ''"},
		{"trading_positions", "opened_at", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, column.table, column.name, column.define); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_trading_orders_decision ON trading_orders(decision_id);
		CREATE INDEX IF NOT EXISTS idx_trading_orders_cycle ON trading_orders(cycle_id);
		CREATE TABLE IF NOT EXISTS trading_closed_trades (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			trading_day TEXT NOT NULL,
			trigger TEXT NOT NULL DEFAULT '',
			content_json TEXT NOT NULL,
			closed_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_closed_trades_account ON trading_closed_trades(account_id, closed_at DESC);
		CREATE INDEX IF NOT EXISTS idx_closed_trades_day ON trading_closed_trades(account_id, trading_day);
		CREATE TABLE IF NOT EXISTS trading_reviews (
			account_id TEXT NOT NULL,
			trading_day TEXT NOT NULL,
			content_json TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			PRIMARY KEY (account_id, trading_day)
		);
	`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, decisionsSchema)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, cycleSchema); err != nil {
		return err
	}
	if err := s.migrateEvents(ctx); err != nil {
		return err
	}
	if err := s.migrateMemory(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentSteps(ctx); err != nil {
		return err
	}
	if err := s.migrateAlerts(ctx); err != nil {
		return err
	}
	return s.migrateReports(ctx)
}

// ensureColumn 是幂等的加列操作。SQLite 的 ALTER TABLE 不支持 IF NOT EXISTS，
// 所以先查 PRAGMA 再决定是否改动。
func (s *Store) ensureColumn(ctx context.Context, table, name, define string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		var (
			cid       int
			column    string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &column, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		if column == name {
			exists = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, define))
	return err
}

func nowText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// EnsureAccount 在首次运行时创建模拟盘账户。已存在时原样返回，不重置资金。
func (s *Store) EnsureAccount(ctx context.Context, id string, initialCash float64) (Account, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultAccountID
	}
	account, err := s.GetAccount(ctx, id)
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}
	if initialCash <= 0 {
		initialCash = DefaultInitialCash
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO trading_accounts(id,name,initial_cash,cash,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		id, "模拟盘", initialCash, initialCash, nowText(now), nowText(now))
	if err != nil {
		return Account{}, fmt.Errorf("创建模拟盘账户失败: %w", err)
	}
	return s.GetAccount(ctx, id)
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,initial_cash,cash,reserved_cash,created_at,updated_at FROM trading_accounts WHERE id=?`, id)
	var (
		account              Account
		created, updatedText string
	)
	if err := row.Scan(&account.ID, &account.Name, &account.InitialCash, &account.Cash, &account.ReservedCash, &created, &updatedText); err != nil {
		return Account{}, err
	}
	account.CreatedAt = parseTime(created)
	account.UpdatedAt = parseTime(updatedText)
	return account, nil
}

func (s *Store) ResetAccount(ctx context.Context, id string, initialCash float64) (Account, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultAccountID
	}
	if initialCash <= 0 {
		initialCash = DefaultInitialCash
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowText(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_positions WHERE account_id=?`, id); err != nil {
		return Account{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_fills WHERE account_id=?`, id); err != nil {
		return Account{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_orders WHERE account_id=?`, id); err != nil {
		return Account{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_plans WHERE account_id=?`, id); err != nil {
		return Account{}, err
	}
	// 重置账户必须同时清掉总闸，否则「重置后仍然不能交易」会成为一个
	// 难以排查的状态。
	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_control WHERE account_id=?`, id); err != nil {
		return Account{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trading_accounts(id,name,initial_cash,cash,reserved_cash,created_at,updated_at)
		VALUES(?,?,?,?,0,?,?)
		ON CONFLICT(id) DO UPDATE SET initial_cash=excluded.initial_cash, cash=excluded.cash,
			reserved_cash=0, updated_at=excluded.updated_at`,
		id, "模拟盘", initialCash, initialCash, now, now); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.GetAccount(ctx, id)
}

// positionColumns 是持仓表读取的唯一字段清单。三处读取（列表、单条、事务内）
// 必须共用它：各写一份的结果是新增一列时漏改一处，那处读出来的持仓就永远没有防守线。
const positionColumns = `account_id,symbol,name,quantity,available_quantity,average_cost,last_price,
	reserved_quantity,stop_loss_price,take_profit_price,peak_price,reduce_count,sector,opened_at,last_buy_date,updated_at`

// scanPosition 按 positionColumns 的顺序读一行。opened_at 为空串表示建仓时间未知
// （旧数据），此时返回 nil 而不是零值时间——零值时间会被算成"持有 2000 天"。
func scanPosition(scanner interface{ Scan(...any) error }, position *Position) error {
	var updated, lastBuyDB, openedDB string
	if err := scanner.Scan(&position.AccountID, &position.Symbol, &position.Name, &position.Quantity,
		&position.Available, &position.AverageCost, &position.LastPrice,
		&position.ReservedQuantity, &position.StopLossPrice, &position.TakeProfitPrice,
		&position.PeakPrice, &position.ReduceCount, &position.Sector, &openedDB,
		&lastBuyDB, &updated); err != nil {
		return err
	}
	position.LastBuyDate = lastBuyDB
	position.UpdatedAt = parseTime(updated)
	if strings.TrimSpace(openedDB) != "" {
		opened := parseTime(openedDB)
		if !opened.IsZero() {
			position.OpenedAt = &opened
		}
	}
	position.MarketValue = round2(float64(position.Quantity) * position.LastPrice)
	position.Unrealized = round2(float64(position.Quantity) * (position.LastPrice - position.AverageCost))
	return nil
}

func (s *Store) ListPositions(ctx context.Context, accountID string) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+positionColumns+`
		FROM trading_positions WHERE account_id=? AND quantity>0 ORDER BY symbol`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := []Position{}
	for rows.Next() {
		var position Position
		if err := scanPosition(rows, &position); err != nil {
			return nil, err
		}
		positions = append(positions, position)
	}
	return positions, rows.Err()
}

func (s *Store) GetPosition(ctx context.Context, accountID, symbol string) (Position, error) {
	var position Position
	err := scanPosition(s.db.QueryRowContext(ctx, `SELECT `+positionColumns+`
		FROM trading_positions WHERE account_id=? AND symbol=?`, accountID, symbol), &position)
	if err != nil {
		return Position{}, err
	}
	return position, nil
}

// UpdateLastPrices 只更新持仓的现价与市值，不改变数量与成本。
func (s *Store) UpdateLastPrices(ctx context.Context, accountID string, prices map[string]float64) error {
	if len(prices) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowText(time.Now().UTC())
	for symbol, price := range prices {
		if price <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE trading_positions SET last_price=?, peak_price=CASE WHEN ?>peak_price THEN ? ELSE peak_price END, updated_at=?
			WHERE account_id=? AND symbol=?`,
			price, price, price, now, accountID, symbol); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RolloverAvailable 处理 T+1：昨日及更早买入的股份今日可卖。
func (s *Store) RolloverAvailable(ctx context.Context, accountID string, now time.Time) error {
	today := now.Format("2006-01-02")
	_, err := s.db.ExecContext(ctx, `UPDATE trading_positions SET available_quantity=quantity, updated_at=?
		WHERE account_id=? AND last_buy_date<>? AND available_quantity<quantity`, nowText(now), accountID, today)
	return err
}

func (s *Store) CreatePlan(ctx context.Context, plan Plan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO trading_plans(id,account_id,status,content_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		plan.ID, plan.AccountID, string(plan.Status), string(payload), nowText(plan.CreatedAt), nowText(plan.UpdatedAt))
	return err
}

func (s *Store) SavePlan(ctx context.Context, plan Plan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE trading_plans SET status=?,content_json=?,updated_at=? WHERE id=?`,
		string(plan.Status), string(payload), nowText(plan.UpdatedAt), plan.ID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetPlan(ctx context.Context, id string) (Plan, error) {
	var (
		payload string
		plan    Plan
	)
	err := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_plans WHERE id=?`, id).Scan(&payload)
	if err != nil {
		return Plan{}, err
	}
	if err := json.Unmarshal([]byte(payload), &plan); err != nil {
		return Plan{}, fmt.Errorf("解析交易计划失败: %w", err)
	}
	normalizePlan(&plan)
	return plan, nil
}

// normalizePlan 把旧版落库数据里缺失的切片字段归一化为空切片。
// 这些字段没有 omitempty，nil 会被序列化成 null；前端对 null 调 .length
// 会崩掉整页，而不是降级显示"0 笔"。
func normalizePlan(plan *Plan) {
	if plan.Orders == nil {
		plan.Orders = []Order{}
	}
	if plan.Verdicts == nil {
		plan.Verdicts = []policy.Verdict{}
	}
	if plan.Issues == nil {
		plan.Issues = []Issue{}
	}
	if plan.Snapshot.Positions == nil {
		plan.Snapshot.Positions = []Position{}
	}
}

func (s *Store) ListPlans(ctx context.Context, accountID string, limit int) ([]Plan, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_plans WHERE account_id=? ORDER BY created_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []Plan{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var plan Plan
		if err := json.Unmarshal([]byte(payload), &plan); err != nil {
			continue
		}
		normalizePlan(&plan)
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// CreateOrders 落库一批订单，并为其中的买入挂单冻结资金。
//
// 冻结与插入必须在同一事务内完成，否则会出现「订单已存在但资金没冻结」的
// 半状态，之后任何一次风控都会按错误的可用资金计算仓位。
//
// 可用资金不足以覆盖某笔买入时，该笔订单被就地标记为 rejected 并写入原因，
// 而不是静默缩减数量——缩减会改变模型给的相对权重，且用户看不到。
// 返回值为落库后的真实状态，调用方必须以此为准。
func (s *Store) CreateOrders(ctx context.Context, orders []Order) ([]Order, error) {
	if len(orders) == 0 {
		return orders, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	result := make([]Order, len(orders))
	copy(result, orders)
	for index := range result {
		order := result[index]
		if order.IsOpen() && order.Side == SideBuy {
			amount, err := reserveAmountForOrder(order)
			if err != nil {
				order.Status = OrderStatusRejected
				order.RejectReason = err.Error()
				order.UpdatedAt = time.Now().UTC()
			} else if err := s.reserveTx(ctx, tx, order.AccountID, &order, amount); err != nil {
				order.Status = OrderStatusRejected
				order.RejectReason = err.Error()
				order.UpdatedAt = time.Now().UTC()
			}
		}
		if order.IsOpen() && order.Side == SideSell {
			// 卖单冻结股数。资金可以共享到成交那一刻再算，股数不行：
			// 两笔各卖 100% 的单子若都只做"当前可卖"校验，两笔都能通过。
			remaining := order.Quantity - order.FilledQuantity
			if err := s.reserveSellQuantityTx(ctx, tx, order.AccountID, order.Symbol, remaining, &order); err != nil {
				order.Status = OrderStatusRejected
				order.RejectReason = err.Error()
				// 保留原始数量：这条记录的价值在于"当时想卖多少"，置零等于丢掉它。
				order.UpdatedAt = time.Now().UTC()
			}
		}
		if err := insertOrderTx(ctx, tx, order); err != nil {
			return nil, err
		}
		result[index] = order
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func insertOrderTx(ctx context.Context, tx *sql.Tx, order Order) error {
	payload, err := json.Marshal(order)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trading_orders(id,plan_id,account_id,symbol,side,status,decision_id,cycle_id,content_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, order.ID, order.PlanID, order.AccountID, order.Symbol, string(order.Side),
		string(order.Status), order.DecisionID, order.CycleID, string(payload), nowText(order.CreatedAt), nowText(order.UpdatedAt))
	return err
}

func (s *Store) UpdateOrder(ctx context.Context, order Order) error {
	payload, err := json.Marshal(order)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE trading_orders SET status=?,decision_id=?,cycle_id=?,content_json=?,updated_at=? WHERE id=?`,
		string(order.Status), order.DecisionID, order.CycleID, string(payload), nowText(order.UpdatedAt), order.ID)
	return err
}

func (s *Store) GetOrder(ctx context.Context, id string) (Order, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT content_json FROM trading_orders WHERE id=?`, id).Scan(&payload); err != nil {
		return Order{}, err
	}
	var order Order
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return Order{}, err
	}
	return order, nil
}

func (s *Store) ListOrdersByPlan(ctx context.Context, planID string) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders WHERE plan_id=? ORDER BY created_at`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err == nil {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}

func (s *Store) ListPendingOrders(ctx context.Context, accountID string) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders
		WHERE account_id=? AND status IN (?,?) ORDER BY created_at`, accountID, string(OrderStatusPending), string(OrderStatusPartial))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err == nil {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}

// ListRejectedOrders 返回 since 之后被拒的委托，新的在前。
//
// 它存在的唯一理由是给下一轮模型看："你上次想买这些，被这些理由拦了"。
// 没有这条查询，模型每一轮都在重新犯错——实测同一批标的会被反复提交、
// 反复在同一个规则上被拦，白烧 token 也白占计划名额。
func (s *Store) ListRejectedOrders(ctx context.Context, accountID string, since time.Time, limit int) ([]Order, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM trading_orders
		WHERE account_id=? AND status=? AND created_at>=?
		ORDER BY created_at DESC LIMIT ?`,
		accountID, string(OrderStatusRejected), nowText(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var order Order
		if err := json.Unmarshal([]byte(payload), &order); err == nil {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}

// ApplyFill 在单个事务内完成：写入成交、更新订单、更新持仓与现金。
// 数量与价格由调用方（模拟撮合）确定，模型无法直接写入账本。
func (s *Store) ApplyFill(ctx context.Context, order Order, quantity int, price, fee float64) (Order, error) {
	if quantity <= 0 || price <= 0 {
		return Order{}, errors.New("成交数量与价格必须为正")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	notional := round2(float64(quantity) * price)
	// 成交会消耗冻结：先把「成交后剩余部分」应冻结的金额算出来，差额退回可用。
	// 这样冻结额始终等于剩余未成交股数所需的资金，对账时可以直接重算比对。
	if err := s.settleReservationTx(ctx, tx, &order, quantity, now); err != nil {
		return Order{}, err
	}
	// 卖单释放的是股数，与资金分开结算：两条不变量各自独立。
	if err := s.settleSellReservationTx(ctx, tx, &order, quantity, now); err != nil {
		return Order{}, err
	}
	position, positionErr := s.getPositionTx(ctx, tx, order.AccountID, order.Symbol)
	if positionErr != nil && !errors.Is(positionErr, sql.ErrNoRows) {
		return Order{}, positionErr
	}

	switch order.Side {
	case SideBuy:
		if err := s.applyBuy(ctx, tx, order, position, positionErr == nil, quantity, price, fee, notional, now); err != nil {
			return Order{}, err
		}
	case SideSell:
		if positionErr != nil {
			return Order{}, errors.New("卖出时未找到持仓")
		}
		if position.Available < quantity {
			return Order{}, fmt.Errorf("可卖数量不足：需要 %d，实际 %d", quantity, position.Available)
		}
		if err := s.applySell(ctx, tx, order, position, quantity, price, fee, notional, now); err != nil {
			return Order{}, err
		}
		// 已实现盈亏必须在这里算：applySell 之后持仓均价已经变了（或持仓已被删除），
		// 再回头算就找不到成本了。
		realized := round2(float64(quantity)*(price-position.AverageCost) - fee)
		order.RealizedPnL = round2(order.RealizedPnL + realized)
		if err := s.insertClosedTradeTx(ctx, tx, order, position, quantity, price, fee, realized, now); err != nil {
			return Order{}, err
		}
		// 分级止盈成交后要记住"减过一次"，并把止损上移到成本价：
		// 止盈的目的不是多赚一点，是把这笔交易变成不会亏的交易。
		if order.Source == OrderSourceProtective && strings.HasPrefix(order.Trigger, TriggerTakeProfit) {
			newStop := order.StopLossPrice
			if position.AverageCost > 0 && position.AverageCost > newStop {
				newStop = position.AverageCost
			}
			if _, err := tx.ExecContext(ctx, `UPDATE trading_positions
				SET reduce_count=reduce_count+1, stop_loss_price=?, updated_at=?
				WHERE account_id=? AND symbol=?`, round2(newStop), nowText(now), order.AccountID, order.Symbol); err != nil {
				return Order{}, err
			}
		}
	default:
		return Order{}, errors.New("未知买卖方向")
	}

	order.FilledQuantity += quantity
	order.FilledPrice = price
	order.Fee = round2(order.Fee + fee)
	order.Status = OrderStatusFilled
	if order.FilledQuantity < order.Quantity {
		order.Status = OrderStatusPartial
	}
	order.UpdatedAt = now
	payload, err := json.Marshal(order)
	if err != nil {
		return Order{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trading_orders SET status=?,content_json=?,updated_at=? WHERE id=?`,
		string(order.Status), string(payload), nowText(now), order.ID); err != nil {
		return Order{}, err
	}
	fill := Fill{
		ID: newID("fill"), OrderID: order.ID, AccountID: order.AccountID, Symbol: order.Symbol,
		Side: order.Side, Quantity: quantity, Price: price, Fee: fee, CreatedAt: now,
	}
	fillPayload, err := json.Marshal(fill)
	if err != nil {
		return Order{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trading_fills(id,order_id,account_id,content_json,created_at) VALUES(?,?,?,?,?)`,
		fill.ID, fill.OrderID, fill.AccountID, string(fillPayload), nowText(now)); err != nil {
		return Order{}, err
	}
	if err := tx.Commit(); err != nil {
		return Order{}, err
	}
	return order, nil
}

func (s *Store) applyBuy(ctx context.Context, tx *sql.Tx, order Order, position Position, exists bool, quantity int, price, fee, notional float64, now time.Time) error {
	total := round2(notional + fee)
	result, err := tx.ExecContext(ctx, `UPDATE trading_accounts SET cash=cash-?,updated_at=? WHERE id=? AND cash>=?`,
		total, nowText(now), order.AccountID, total)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("可用资金不足：需要 %.2f 元", total)
	}
	today := now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	// 防守线随建仓写入。加仓时以新单的建议为准（模型刚重新判断过），
	// 旧单没有给出则保留原值——被空值覆盖等于把保护悄悄取消。
	stop := order.StopLossPrice
	takeProfit := order.TakeProfitPrice
	if !exists {
		_, err = tx.ExecContext(ctx, `INSERT INTO trading_positions(account_id,symbol,name,quantity,available_quantity,average_cost,last_price,
			reserved_quantity,stop_loss_price,take_profit_price,peak_price,reduce_count,sector,opened_at,last_buy_date,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			order.AccountID, order.Symbol, order.Name, quantity, 0, price, price,
			0, round2(stop), round2(takeProfit), price, 0, order.Sector, nowText(now), today, nowText(now))
		return err
	}
	newQuantity := position.Quantity + quantity
	newCost := (position.AverageCost*float64(position.Quantity) + notional + fee) / float64(newQuantity)
	newCost = round4(newCost)
	if stop <= 0 {
		stop = position.StopLossPrice
	}
	if takeProfit <= 0 {
		takeProfit = position.TakeProfitPrice
	}
	peak := position.PeakPrice
	if price > peak {
		peak = price
	}
	_, err = tx.ExecContext(ctx, `UPDATE trading_positions SET quantity=?,average_cost=?,last_price=?,last_buy_date=?,
		stop_loss_price=?,take_profit_price=?,peak_price=?,sector=?,updated_at=?
		WHERE account_id=? AND symbol=?`,
		newQuantity, newCost, price, today, round2(stop), round2(takeProfit), round2(peak),
		firstNonEmpty(order.Sector, position.Sector), nowText(now), order.AccountID, order.Symbol)
	return err
}

// insertClosedTradeTx 写入一条已实现平仓记录。
// 持仓天数按建仓时间到成交时间算；旧数据没有 opened_at 时记 0 天并保留 nil，
// 不猜一个"大概持有了几天"——那会污染按持仓周期做的归因。
func (s *Store) insertClosedTradeTx(ctx context.Context, tx *sql.Tx, order Order, position Position, quantity int, price, fee, realized float64, now time.Time) error {
	pnlPercent := 0.0
	if position.AverageCost > 0 {
		pnlPercent = round2((price/position.AverageCost - 1) * 100)
	}
	holdDays := 0
	if position.OpenedAt != nil && !position.OpenedAt.IsZero() {
		holdDays = int(now.Sub(*position.OpenedAt).Hours() / 24)
		if holdDays < 0 {
			holdDays = 0
		}
	}
	// 峰值价与持仓表保持同一口径：取历史峰值和本次成交价中较高者。
	// 平仓后持仓行会被删除，这是最后一次能拿到它的机会。
	peak := position.PeakPrice
	if price > peak {
		peak = price
	}
	trade := ClosedTrade{
		ID:         newID("close"),
		AccountID:  order.AccountID,
		Symbol:     order.Symbol,
		Name:       firstNonEmpty(order.Name, position.Name),
		Sector:     firstNonEmpty(order.Sector, position.Sector),
		Quantity:   quantity,
		EntryPrice: position.AverageCost,
		ExitPrice:  price,
		Fee:        fee,
		PnL:        realized,
		PnLPercent: pnlPercent,
		Trigger:    order.Trigger,
		Source:     order.Source,
		OrderID:    order.ID,
		DecisionID: order.DecisionID,
		CycleID:    order.CycleID,
		OpenedAt:   position.OpenedAt,
		ClosedAt:   now,
		HoldDays:   holdDays,
		TradingDay: TradingDay(now),
		PeakPrice:  round2(peak),
	}
	payload, err := json.Marshal(trade)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trading_closed_trades(id,account_id,symbol,trading_day,trigger,content_json,closed_at)
		VALUES(?,?,?,?,?,?,?)`,
		trade.ID, trade.AccountID, trade.Symbol, trade.TradingDay, trade.Trigger, string(payload), nowText(now))
	return err
}

func (s *Store) applySell(ctx context.Context, tx *sql.Tx, order Order, position Position, quantity int, price, fee, notional float64, now time.Time) error {
	proceeds := round2(notional - fee)
	if _, err := tx.ExecContext(ctx, `UPDATE trading_accounts SET cash=cash+?,updated_at=? WHERE id=?`, proceeds, nowText(now), order.AccountID); err != nil {
		return err
	}
	remaining := position.Quantity - quantity
	if remaining <= 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM trading_positions WHERE account_id=? AND symbol=?`, order.AccountID, order.Symbol)
		return err
	}
	available := position.Available - quantity
	if available < 0 {
		available = 0
	}
	_, err := tx.ExecContext(ctx, `UPDATE trading_positions SET quantity=?,available_quantity=?,last_price=?,updated_at=?
		WHERE account_id=? AND symbol=?`, remaining, available, price, nowText(now), order.AccountID, order.Symbol)
	return err
}

func (s *Store) getPositionTx(ctx context.Context, tx *sql.Tx, accountID, symbol string) (Position, error) {
	var position Position
	err := scanPosition(tx.QueryRowContext(ctx, `SELECT `+positionColumns+`
		FROM trading_positions WHERE account_id=? AND symbol=?`, accountID, symbol), &position)
	if err != nil {
		return Position{}, err
	}
	return position, nil
}

// UpdateProtection 人工调整某只持仓的防守线。
// 它不校验价格方向：止损设得比现价高会被下一轮防守线立刻触发，
// 那是用户自己的选择，系统要做的是执行，而不是替他判断"这样不划算"。
func (s *Store) UpdateProtection(ctx context.Context, accountID, symbol string, stopLoss, takeProfit float64) (Position, error) {
	_, err := s.db.ExecContext(ctx, `UPDATE trading_positions SET stop_loss_price=?, take_profit_price=?, updated_at=?
		WHERE account_id=? AND symbol=?`, round2(stopLoss), round2(takeProfit), nowText(time.Now().UTC()), accountID, symbol)
	if err != nil {
		return Position{}, err
	}
	return s.GetPosition(ctx, accountID, symbol)
}
