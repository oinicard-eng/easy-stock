package trading

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
	"easy-stock/backend/internal/trading/universe"
)

var (
	ErrPlanRunning   = errors.New("已有交易计划正在生成，请等待完成")
	ErrPlanNotDraft  = errors.New("只有待确认状态的计划可以执行")
	ErrNoTradingPlan = errors.New("未找到交易计划")
	// ErrKillSwitch 表示账户已全部停止。它优先于一切其他门控，
	// 包括「模型很有信心」和「用户点了执行」。
	ErrKillSwitch = errors.New("账户已触发全部停止（kill switch），不生成计划也不允许成交")
	// ErrPauseBuying 表示只暂停新增买入，卖出与减仓仍然放行。
	ErrPauseBuying = errors.New("账户已暂停买入，仅允许卖出与减仓")
)

// QuoteProvider 与 httpapi 的实时行情接口保持一致。
type QuoteProvider interface {
	Realtime(ctx context.Context, symbols []string) ([]foundation.Quote, error)
}

// StockCatalogProvider 提供全市场快照（含价格、涨跌幅、成交额、行业）。
// 认知层与选股层共用这一份数据，避免两套口径得出互相矛盾的结论。
type StockCatalogProvider interface {
	StockCatalog(ctx context.Context) ([]foundation.StockCatalogEntry, error)
}

type PlanRequest struct {
	AccountID     string   `json:"account_id,omitempty"`
	Watchlist     []string `json:"watchlist,omitempty"`
	TraderProfile string   `json:"trader_profile,omitempty"`
	Horizon       string   `json:"horizon,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	// AutoUniverse 为真时忽略 Watchlist，由系统从全市场自动选股。
	// Watchlist 为空时也会自动进入该模式。
	AutoUniverse bool `json:"auto_universe,omitempty"`
	// buyBlock 是本轮的环境禁买约束（尾盘、当日熔断）。
	// 刻意不导出：它只能由自动循环按确定性规则注入，
	// 若能从请求体传入，调用方就能自己"解除"熔断。
	buyBlock *BuyBlock
	// skipOnNoChange 只由定时轮次打开：输入与上次模型调用完全一致时跳过。
	// 人工触发的轮次必须真的问一次模型，否则按钮看起来像坏了。
	skipOnNoChange bool
}

type Service struct {
	store   *Store
	gateway agent.Gateway
	quotes  QuoteProvider
	catalog StockCatalogProvider
	broker  Broker
	rules   RiskRules
	logger  *log.Logger

	mu      sync.Mutex
	running bool

	// startedAt 用于健康检查里的 uptime。进程跑了多久是判断"刚重启过"的第一手信息：
	// 频繁重启本身就是一个需要被看见的故障，而不是"服务正常"。
	startedAt time.Time

	// sectorFlow 是可选能力：没配置资金流数据源时，对应工具会明确报错，
	// 而不是让模型以为"今天没有资金流进任何板块"。
	sectorFlow SectorFlowProvider
	// kline / klineAlt 是日线与备用日线来源。同样是可选的：
	// 没有它时 K 线工具报"数据源未配置"，其余功能不受影响。
	kline    KLineProvider
	klineAlt KLineProvider

	// catalogCache 保存最近一次全市场快照，供调研工具按行业下钻。
	// 只放内存不落库：几千条目录塞进每份计划会让库迅速膨胀，
	// 而且它每次都能源源不断地重新取到，没有持久化价值。
	catalogMu    sync.RWMutex
	catalogCache map[string]foundation.StockCatalogEntry

	// researchEnabled 控制生成计划前是否跑调研。
	// 默认开启；单测里显式关闭——那些用例验证的是计划与裁决的语义，
	// 不该因为多了一次模型往返而失败，也不该用假调研去测真流程。
	researchEnabled atomic.Bool

	// alerts 是告警投递器。为 nil 表示没有配置任何出口：
	// 事件照常落库、健康条照常变红，但这些信息不会离开这台机器。
	alerts *AlertManager

	// change 记录上一次模型调用时的输入指纹，用于跳过"什么都没变"的轮次。
	// 放在内存里：重启后必须重新问一次模型，停机期间发生了什么无法从指纹推出。
	changeMu sync.Mutex
	change   changeBaseline
}

func NewService(store *Store, gateway agent.Gateway, quotes QuoteProvider, catalog StockCatalogProvider, logger *log.Logger) *Service {
	service := &Service{
		store:     store,
		gateway:   gateway,
		quotes:    quotes,
		catalog:   catalog,
		broker:    NewPaperBroker(),
		rules:     DefaultRiskRules(),
		logger:    logger,
		startedAt: time.Now(),
	}
	// 默认开启调研：这是"智能体"与"一次性文本生成"的分界线。
	service.researchEnabled.Store(true)
	return service
}

// SetResearchEnabled 开关调研阶段。生产环境不应关闭；
// 只有明确想对比"有调研/无调研"两种表现的场景才需要它。
func (s *Service) SetResearchEnabled(enabled bool) {
	s.researchEnabled.Store(enabled)
}

func (s *Service) Store() *Store { return s.store }

// SetAlerts 注入告警管理器，同时把事件钩子挂到 Store 上。
//
// 钩子挂在 Store 而不是 Service，是因为事件写入有多个入口
// （自愈、作废、回报落账都直接调 store.AppendEvent），
// 只在一个入口挂钩子会漏掉其余入口，而漏掉的往往正是最该告警的那几条。
func (s *Service) SetAlerts(manager *AlertManager) {
	s.mu.Lock()
	s.alerts = manager
	s.mu.Unlock()
	if s.store == nil {
		return
	}
	if manager == nil {
		s.store.SetEventSink(nil)
		return
	}
	s.store.SetEventSink(manager.Submit)
}

// Alerts 返回告警管理器，未配置时为 nil。
func (s *Service) Alerts() *AlertManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts
}

// SetSectorFlow 注入板块资金流数据源。它是可选的：没有它系统照常交易，
// 只是模型查不到资金流——工具会明确说明"数据源未配置"，不会假装查过了。
func (s *Service) SetSectorFlow(provider SectorFlowProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sectorFlow = provider
}

// SetKLine 注入日线数据源（主源 + 备源）。备源允许为空。
// 与资金流同理，缺数据时工具会明说，不会拼一条假曲线出来。
func (s *Service) SetKLine(primary, fallback KLineProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kline = primary
	s.klineAlt = fallback
}

// rememberCatalog 缓存最近一次全市场快照，供调研工具按行业下钻使用。
func (s *Service) rememberCatalog(entries []foundation.StockCatalogEntry) {
	cache := make(map[string]foundation.StockCatalogEntry, len(entries))
	for _, entry := range entries {
		// 键用 6 位数字：目录里的代码带 .SH/.SZ 后缀，而工具参数可能不带，
		// 用展示格式当索引会让"查得到候选、查不到详情"这种诡异现象反复出现。
		if symbol := symbolDigits(entry.Symbol); symbol != "" {
			cache[symbol] = entry
		}
	}
	s.catalogMu.Lock()
	s.catalogCache = cache
	s.catalogMu.Unlock()
}

// buildToolbox 构造本轮的工具上下文。
// 工具与本轮快照同源，所以模型在工具里看到的价格和提示词里的表格一定一致。
func (s *Service) buildToolbox(accountID string, snapshot Snapshot, now time.Time) *Toolbox {
	s.catalogMu.RLock()
	cache := s.catalogCache
	s.catalogMu.RUnlock()
	s.mu.Lock()
	flow := s.sectorFlow
	primary, fallback := s.kline, s.klineAlt
	s.mu.Unlock()
	return &Toolbox{
		store:      s.store,
		accountID:  s.accountID(accountID),
		snapshot:   snapshot,
		catalog:    cache,
		sectorFlow: flow,
		klines:     primary,
		klineAlt:   fallback,
		now:        now,
	}
}

// openOrdersForPrompt 取当前未成交委托，供提示词列出"已经挂出去的单"。
// 读失败只记日志：少列一段挂单会让模型多提一笔重复建议（会被风控拦下），
// 而为此中断整轮计划，代价大得多。
func (s *Service) openOrdersForPrompt(ctx context.Context, accountID string) []Order {
	orders, err := s.store.ListPendingOrders(ctx, accountID)
	if err != nil {
		s.logf("event=trading_pending_orders_failed account=%s err=%v", accountID, err)
		return nil
	}
	return orders
}

// recentBlocksForPrompt 取最近 4 小时被拦下的建议，按「标的+方向+规则」去重后带上重复次数。
//
// 窗口取 4 小时而不是更长：半日前的拦截多半对应另一个行情环境，
// 拿它约束当下，会让模型对已经变化的市场过度克制。
// 去重是必需的：模型若连续 5 轮犯同一个错，列出 5 条一样的记录只会占掉注意力，
// 一条带「重复 5 次」的记录反而更能说明问题。
func (s *Service) recentBlocksForPrompt(ctx context.Context, accountID string) []BlockedIntent {
	const window = 4 * time.Hour
	const limit = 20
	orders, err := s.store.ListRejectedOrders(ctx, accountID, time.Now().UTC().Add(-window), limit)
	if err != nil {
		s.logf("event=trading_recent_blocks_failed account=%s err=%v", accountID, err)
		return nil
	}
	blocks := make([]BlockedIntent, 0, limit)
	index := map[string]int{}
	for _, order := range orders {
		key := order.Symbol + "|" + string(order.Side) + "|" + order.RejectCode
		if seat, seen := index[key]; seen {
			blocks[seat].Repeat++
			continue
		}
		index[key] = len(blocks)
		blocks = append(blocks, BlockedIntent{
			Symbol: order.Symbol, Name: order.Name, Side: order.Side,
			Weight: order.TargetWeightPercent, Quant: order.Quantity,
			Price: order.LimitPrice, Code: order.RejectCode, Reason: order.RejectReason,
			Blocked: order.CreatedAt, Repeat: 1,
		})
	}
	// 只留 6 条：这是"提醒"，不是台账。超过 6 条说明模型在同一处反复失败，
	// 那本身就该由降级或人工介入处理，而不是继续堆提示词。
	if len(blocks) > 6 {
		blocks = blocks[:6]
	}
	return blocks
}

func (s *Service) BrokerName() string {
	if s.broker == nil {
		return "paper"
	}
	return s.broker.Name()
}

// RulesForProfile 把交易者画像映射为确定性风控阈值。画像只影响阈值，不影响数据来源。
func RulesForProfile(profile string) RiskRules {
	rules := DefaultRiskRules()
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "aggressive", "激进":
		rules.MaxSingleWeightPercent = 25
		rules.MaxRiskPerTradePercent = 3
		rules.MaxStopLossPercent = 12
		rules.MinCashPercent = 3
	case "steady", "稳健":
		rules.MaxSingleWeightPercent = 15
		rules.MaxRiskPerTradePercent = 1.5
		rules.MaxStopLossPercent = 8
		rules.MinCashPercent = 10
	}
	return rules.Normalize()
}

// AccountView 返回账户与持仓的当前视图。行情刷新失败时保留上次价格并继续返回，
// 但不会伪装成最新价：调用方可通过 Age 自行判断时效。
func (s *Service) AccountView(ctx context.Context, accountID string) (Account, []Position, error) {
	accountID = s.accountID(accountID)
	account, err := s.store.EnsureAccount(ctx, accountID, DefaultInitialCash)
	if err != nil {
		return Account{}, nil, err
	}
	if err := s.store.RolloverAvailable(ctx, accountID, time.Now()); err != nil {
		s.logf("event=trading_rollover_failed account=%s err=%v", accountID, err)
	}
	positions, err := s.store.ListPositions(ctx, accountID)
	if err != nil {
		return Account{}, nil, err
	}
	if len(positions) > 0 && s.quotes != nil {
		symbols := make([]string, 0, len(positions))
		for _, position := range positions {
			symbols = append(symbols, position.Symbol)
		}
		quotesCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		quotes, quoteErr := s.quotes.Realtime(quotesCtx, symbols)
		cancel()
		if quoteErr != nil {
			s.logf("event=trading_quote_refresh_failed account=%s err=%v", accountID, quoteErr)
		} else {
			prices := map[string]float64{}
			for _, quote := range quotes {
				if quote.Price > 0 {
					prices[quote.Symbol] = quote.Price
				}
			}
			if err := s.store.UpdateLastPrices(ctx, accountID, prices); err != nil {
				s.logf("event=trading_price_persist_failed account=%s err=%v", accountID, err)
			}
			positions, err = s.store.ListPositions(ctx, accountID)
			if err != nil {
				return Account{}, nil, err
			}
		}
	}
	account.MarketValue = 0
	for _, position := range positions {
		account.MarketValue += position.MarketValue
	}
	account.MarketValue = round2(account.MarketValue)
	account.Equity = round2(account.Cash + account.MarketValue)
	return account, positions, nil
}

// StartPlan 立即返回一条 running 状态的计划记录，模型在后台生成。
// 同步调用会被 HTTP 超时切断，也会让前端无法感知长任务进度。
func (s *Service) StartPlan(ctx context.Context, request PlanRequest) (Plan, error) {
	if !s.beginRun() {
		return Plan{}, ErrPlanRunning
	}
	plan, watchlist, autoUniverse, err := s.preparePlan(ctx, request, "")
	if err != nil {
		s.clearRunning()
		return Plan{}, err
	}
	go s.generate(plan.ID, plan.AccountID, watchlist, autoUniverse, request)
	return plan, nil
}

// RunPlanSync 在调用方 goroutine 里完整跑一轮计划生成，并返回最终计划。
//
// 自动循环必须用同步版本：异步版把结果丢进数据库就返回，循环拿不到
// "这一轮到底下了几笔、成交几笔"，日志里也就只能写"已发起"。
// ctx 由调用方控制超时，进程退出时能被一起取消。
func (s *Service) RunPlanSync(ctx context.Context, request PlanRequest, cycleID string) (Plan, error) {
	if !s.beginRun() {
		return Plan{}, ErrPlanRunning
	}
	defer s.clearRunning()
	plan, watchlist, autoUniverse, err := s.preparePlan(ctx, request, cycleID)
	if err != nil {
		return Plan{}, err
	}
	return s.generatePlan(ctx, plan.ID, plan.AccountID, watchlist, autoUniverse, request,
		planRun{cycleID: cycleID, buyBlock: request.buyBlock, skipOnNoChange: request.skipOnNoChange})
}

// preparePlan 是两条生成路径（人工 / 自动）共用的准备工作。
// 抽出来不是为了少写几行，而是为了让"校验标的、取账户、判总闸、落占位计划"
// 只有一份实现——两份实现迟早会在某一处松掉一个检查。
func (s *Service) preparePlan(ctx context.Context, request PlanRequest, cycleID string) (Plan, []string, bool, error) {
	accountID := s.accountID(request.AccountID)
	watchlist, err := s.normalizeWatchlist(request.Watchlist)
	if err != nil {
		return Plan{}, nil, false, err
	}
	// 没有显式给出标的时自动选股：这才是"会自动选股的交易员"与"执行器"的分界。
	autoUniverse := request.AutoUniverse || len(watchlist) == 0
	if autoUniverse {
		if s.catalog == nil {
			return Plan{}, nil, false, errors.New("未配置全市场行情源，无法自动选股；请显式提供关注标的")
		}
		watchlist = nil
	}
	account, positions, err := s.AccountView(ctx, accountID)
	if err != nil {
		return Plan{}, nil, false, err
	}
	if account.Equity <= 0 {
		return Plan{}, nil, false, errors.New("账户总资产异常，无法生成计划")
	}
	control, err := s.store.GetControl(ctx, accountID)
	if err != nil {
		return Plan{}, nil, false, err
	}
	if control.KillSwitch {
		return Plan{}, nil, false, ErrKillSwitch
	}

	now := time.Now().UTC()
	plan := Plan{
		ID:            newID("plan"),
		AccountID:     accountID,
		CycleID:       cycleID,
		Status:        PlanStatusRunning,
		TraderProfile: strings.TrimSpace(request.TraderProfile),
		Horizon:       strings.TrimSpace(request.Horizon),
		Gate:          control.Gate(),
		Rules:         RulesForProfile(request.TraderProfile),
		// 显式给空切片：nil 会被序列化成 null，前端对 running 状态取 orders.length 会崩。
		Orders: []Order{},
		Snapshot: Snapshot{
			TakenAt:      now,
			Equity:       account.Equity,
			Cash:         account.Cash,
			ReservedCash: account.ReservedCash,
			Positions:    positions,
			Universe:     watchlist,
			AutoUniverse: autoUniverse,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreatePlan(ctx, plan); err != nil {
		return Plan{}, nil, false, err
	}
	return plan, watchlist, autoUniverse, nil
}

func (s *Service) generate(planID, accountID string, watchlist []string, autoUniverse bool, request PlanRequest) {
	defer s.clearRunning()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if _, err := s.generatePlan(ctx, planID, accountID, watchlist, autoUniverse, request, planRun{buyBlock: request.buyBlock}); err != nil {
		s.logf("event=trading_plan_generate_failed plan=%s err=%v", planID, err)
	}
}

// planRun 承载一次生成的环境约束：它属于哪一轮循环、本轮是否禁买。
type planRun struct {
	cycleID        string
	buyBlock       *BuyBlock
	skipOnNoChange bool
}

// generatePlan 是计划生成的全部流程：取快照 → 构建提示词 → 调模型 → 裁决 → 落库。
// 返回的 error 与 plan.ErrorMessage 一致，调用方据此决定这一轮循环是否算失败。
func (s *Service) generatePlan(ctx context.Context, planID, accountID string, watchlist []string, autoUniverse bool, request PlanRequest, run planRun) (Plan, error) {
	plan, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return Plan{}, fmt.Errorf("读取计划失败：%w", err)
	}
	fail := func(message string) (Plan, error) {
		plan.Status = PlanStatusFailed
		plan.ErrorMessage = message
		plan.UpdatedAt = time.Now().UTC()
		if plan.Orders == nil {
			plan.Orders = []Order{}
		}
		if err := s.store.SavePlan(ctx, plan); err != nil {
			s.logf("event=trading_plan_save_failed plan=%s err=%v", planID, err)
		}
		s.logf("event=trading_plan_failed plan=%s err=%s", planID, message)
		return plan, errors.New(message)
	}

	snapshot, err := s.buildSnapshot(ctx, accountID, watchlist, autoUniverse, plan.Snapshot.Positions)
	if err != nil {
		// 失败时保留已算出的市场认知与候选池，让"为什么没生成计划"可查，
		// 而不是只剩一句错误文字。
		plan.Snapshot.Market = snapshot.Market
		plan.Snapshot.Board = snapshot.Board
		plan.Snapshot.AutoUniverse = snapshot.AutoUniverse
		return fail(err.Error())
	}
	plan.Snapshot = snapshot
	if snapshot.Equity <= 0 {
		return fail("账户总资产异常，无法计算仓位")
	}

	// 阈值先算出来：即使这一轮因为"没有变化"被跳过，计划里也要留下它依据的额度，
	// 否则复盘时会看到一份没有上限的跳过记录。
	rules := RulesForProfile(request.TraderProfile)
	plan.Rules = rules

	// 反馈回灌所需的两次查询提前到这里：它们同时参与变化检测。
	// 如果留到提示词阶段才取，指纹就会漏掉"模型又被同一条规则拦了一次"，
	// 而那恰恰是最需要再问一次的情形。
	pending := s.openOrdersForPrompt(ctx, accountID)
	blocks := s.recentBlocksForPrompt(ctx, accountID)
	fingerprint := SnapshotFingerprint(snapshot, pending, blocks)
	if skip, note := s.shouldSkipNoChange(fingerprint, time.Now(), request.skipOnNoChange); skip {
		plan.Status = PlanStatusSkipped
		plan.ErrorMessage = note
		plan.UpdatedAt = time.Now().UTC()
		if plan.Orders == nil {
			plan.Orders = []Order{}
		}
		if err := s.store.SavePlan(ctx, plan); err != nil {
			s.logf("event=trading_plan_save_failed plan=%s err=%v", planID, err)
		}
		s.logf("event=trading_plan_skipped plan=%s reason=no_material_change cycle=%s", planID, run.cycleID)
		return plan, ErrNoMaterialChange
	}

	// 调研阶段：模型用只读工具自己查事实，然后才进计划提示词。
	//
	// 失败一律降级、不阻断：调研是增益，不是前置条件。把它做成硬前置的话，
	// 一次模型抖动就会让当天所有交易机会消失——这是把可观测性做成了单点故障。
	researchInput := ResearchInput{
		PlanID:    planID,
		CycleID:   run.cycleID,
		AccountID: accountID,
		Snapshot:  snapshot,
		Toolbox:   s.buildToolbox(accountID, snapshot, time.Now()),
		Now:       time.Now(),
	}
	research := s.runResearch(ctx, researchInput)
	plan.Research = &research
	s.saveResearch(ctx, researchInput, research)
	if research.Degraded {
		s.logf("event=trading_research_degraded plan=%s note=%s", planID, research.Note)
	}

	control, controlErr := s.store.GetControl(ctx, accountID)
	if controlErr != nil {
		return fail(fmt.Sprintf("读取账户总闸失败：%v", controlErr))
	}
	if control.KillSwitch {
		return fail(ErrKillSwitch.Error())
	}
	plan.Gate = control.Gate()
	prompt := BuildPlanPrompt(PlanPromptInput{
		Account: Account{
			ID: accountID, Cash: snapshot.Cash, ReservedCash: snapshot.ReservedCash,
			Equity: snapshot.Equity,
		},
		Positions: snapshot.Positions,
		Quotes:    snapshot.Quotes,
		Watchlist: snapshot.Universe,
		Rules:     rules,
		Profile:   request.TraderProfile,
		Horizon:   request.Horizon,
		Notes:     request.Notes,
		Now:       time.Now(),
		Market:    snapshot.Market,
		Board:     snapshot.Board,
		// 经验在这里取而不是存进快照：快照要随计划落库，塞进几十条统计会让
		// 每条计划记录多出几 KB，而这些数字下次还能重新算出来。
		Experience: s.Experience(ctx, accountID, 8),
		Research:   &research,
		Pending:    pending,
		Blocks:     blocks,
	})

	startedAt := time.Now()
	result, err := agent.PromptUsingOptions(ctx, s.gateway, prompt, agent.PromptOptions{
		Sandbox:              true,
		AutoApprove:          true,
		DisableTools:         true,
		FirstResponseTimeout: 100 * time.Second,
		IdleTimeout:          150 * time.Second,
		MaxAttempts:          2,
	})
	if err != nil {
		return fail(fmt.Sprintf("模型调用失败：%v", err))
	}
	modelRun := ModelRun{
		LatencyMS:        time.Since(startedAt).Milliseconds(),
		PromptChars:      len([]rune(prompt)),
		ContentChars:     len([]rune(result.Content)),
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		TotalTokens:      result.Usage.TotalTokens,
		Model:            firstNonEmpty(result.Usage.Model, result.ModelIdentity),
	}
	draft, err := ParseDraftPlan(result.Content)
	if err != nil {
		return fail(err.Error())
	}
	// 解析成功才算"这一轮真的问过模型"。在此之前记指纹，会把一次超时
	// 变成接下来 20 分钟的静音——失败应该被重试，而不是被记住。
	s.recordChange(fingerprint, planID, time.Now())
	// 模型建议只到这里为止：往下全部是确定性判断。
	// 决策与订单构建分成两步，是为了让"为什么没买"有结构化的答案，
	// 而不是散落在订单的拒绝原因里靠人拼。
	mode := "manual"
	if snapshot.AutoUniverse {
		mode = "auto"
	}
	decision, orders := Decide(DecideInput{
		AccountID: accountID,
		PlanID:    planID,
		CycleID:   run.cycleID,
		Mode:      mode,
		Draft:     draft,
		Snapshot:  snapshot,
		Rules:     rules,
		Control:   control,
		BuyBlock:  run.buyBlock,
		Model:     modelRun,
		Now:       time.Now().UTC(),
	})
	for index := range orders {
		orders[index].PlanID = planID
		orders[index].DecisionID = decision.ID
		orders[index].CycleID = run.cycleID
	}
	plan.Regime = draft.Regime
	plan.Summary = draft.Summary
	plan.RiskNotes = draft.RiskNotes
	plan.Issues = issuesFromVerdicts(decision.Verdicts)

	actionable := 0
	for _, order := range orders {
		if order.Status == OrderStatusPending {
			actionable++
		}
	}
	switch actionable {
	case 0:
		plan.Orders = orders
		plan.Status = PlanStatusFailed
		plan.ErrorMessage = zeroOrderReason(decision)
	default:
		// 落库时会再次校验可用资金并冻结；资金不够的订单会被就地标记为 rejected，
		// 因此必须用返回值覆盖计划里的订单，否则界面会显示"可执行"其实没进账本。
		stored, storeErr := s.store.CreateOrders(ctx, orders)
		if storeErr != nil {
			return fail(fmt.Sprintf("保存订单失败：%v", storeErr))
		}
		decision = settleDecision(decision, snapshot, orders, stored)
		plan.Orders = stored
		plan.Status = PlanStatusDraft
		frozen := 0.0
		for _, order := range stored {
			frozen += order.ReservedAmount
		}
		if frozen > 0 {
			plan.RiskNotes = append(plan.RiskNotes, fmt.Sprintf("本次已冻结 %.2f 元，成交前不可用于其他计划", round2(frozen)))
		}
	}
	plan.DecisionID = decision.ID
	plan.Verdicts = decision.Verdicts
	plan.Exposure = &decision.Exposure
	// 计划级风险复核。放在裁决落库之前生成，因为它读的正是这一轮最终订单；
	// 但它只读不写，不改变任何订单状态——报告与闸门必须是两件事，
	// 否则事后无法判断"这笔单是被守卫拦的，还是被报告拦的"。
	riskState, riskErr := s.store.GetRiskState(ctx, accountID)
	if riskErr != nil {
		s.logf("event=trading_risk_state_read_failed plan=%s err=%v", planID, riskErr)
	}
	review := BuildPlanRiskReview(RiskReviewInput{
		Snapshot: snapshot,
		Rules:    rules,
		Control:  control,
		Decision: decision,
		Orders:   plan.Orders,
		Risk:     riskState,
		Research: &research,
		Now:      time.Now().UTC(),
	})
	plan.RiskReview = &review
	if plan.Orders == nil {
		plan.Orders = []Order{}
	}
	// 裁决先于计划落库：即便后面保存计划失败，也已经留下"当时为什么这么判"的记录。
	if err := s.store.SaveDecision(ctx, decision); err != nil {
		s.logf("event=trading_decision_save_failed decision=%s err=%v", decision.ID, err)
	}
	plan.UpdatedAt = time.Now().UTC()
	if err := s.store.SavePlan(ctx, plan); err != nil {
		s.logf("event=trading_plan_save_failed plan=%s err=%v", planID, err)
		return plan, fmt.Errorf("保存计划失败：%w", err)
	}
	s.logf("event=trading_plan_ready plan=%s status=%s orders=%d actionable=%d decision=%s blocked=%d adjusted=%d cycle=%s",
		planID, plan.Status, len(plan.Orders), actionable, decision.ID, decision.Stats.Blocked, decision.Stats.Adjusted, run.cycleID)
	return plan, nil
}

// zeroOrderReason 把"一笔可执行的都没有"翻译成一句能定位原因的话。
// 三种情况的原因完全不同：模型没建议、门控/市场不让买、建议全被风控驳回。
func zeroOrderReason(decision Decision) string {
	if decision.Stats.Proposed == 0 {
		return "模型未给出可执行建议"
	}
	if decision.Dropped > 0 {
		for _, verdict := range decision.Verdicts {
			if verdict.Code == policy.CodeMarketUnknown || verdict.Code == policy.CodeMarketPanic ||
				verdict.Code == policy.CodeGateKillSwitch {
				return verdict.Message
			}
		}
		return fmt.Sprintf("门控剔除全部 %d 笔建议，本轮无可执行订单", decision.Dropped)
	}
	return "模型的全部建议均未通过风控校验"
}

// issuesFromVerdicts 把裁决流映射为旧版 Issues 字段，保证既有界面与调用方不断裂。
// block 对应 error，adjust/warn 对应 warning——Issue 是二元级别，
// 需要区分"调整"与"提示"的地方应直接读 verdicts。
func issuesFromVerdicts(verdicts []policy.Verdict) []Issue {
	issues := make([]Issue, 0, len(verdicts))
	for _, verdict := range verdicts {
		level := IssueWarning
		if verdict.Level == policy.LevelBlock {
			level = IssueError
		}
		issues = append(issues, Issue{Level: level, Field: verdict.Field, Message: verdict.Message, Code: verdict.Code})
	}
	return issues
}

// Execute 把待确认计划中的挂起订单送入执行通道（当前为模拟撮合）。
func (s *Service) Execute(ctx context.Context, planID string) (Plan, error) {
	plan, err := s.store.GetPlan(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoTradingPlan
	}
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanStatusDraft {
		return Plan{}, ErrPlanNotDraft
	}
	control, err := s.store.GetControl(ctx, plan.AccountID)
	if err != nil {
		return Plan{}, err
	}
	if control.KillSwitch {
		return Plan{}, ErrKillSwitch
	}
	account, positions, err := s.AccountView(ctx, plan.AccountID)
	if err != nil {
		return Plan{}, err
	}
	positionIndex := map[string]Position{}
	for _, position := range positions {
		positionIndex[position.Symbol] = position
	}

	orders, err := s.store.ListOrdersByPlan(ctx, planID)
	if err != nil {
		return Plan{}, err
	}
	for index := range orders {
		if orders[index].Status != OrderStatusPending && orders[index].Status != OrderStatusPartial {
			continue
		}
		// 暂停买入时挂单保持挂起而不是被拒绝：这是临时状态，恢复后仍应成交。
		if control.PauseBuying && orders[index].Side == SideBuy {
			continue
		}
		position := positionIndex[orders[index].Symbol]
		updated, matchErr := s.match(ctx, account, position, orders[index])
		if matchErr != nil {
			orders[index].Status = OrderStatusRejected
			orders[index].RejectReason = matchErr.Error()
			orders[index].UpdatedAt = time.Now().UTC()
			if err := s.store.UpdateOrder(ctx, orders[index]); err != nil {
				s.logf("event=trading_order_update_failed order=%s err=%v", orders[index].ID, err)
			}
			continue
		}
		orders[index] = updated
		// 成交后账户现金发生变化，后续订单必须按新的余额撮合。
		if updated.Status == OrderStatusFilled || updated.Status == OrderStatusPartial {
			if refreshed, positionsAfter, err := s.AccountView(ctx, plan.AccountID); err == nil {
				account = refreshed
				for _, item := range positionsAfter {
					positionIndex[item.Symbol] = item
				}
			}
		}
	}
	plan.Orders = orders
	now := time.Now().UTC()
	plan.Status = PlanStatusExecuted
	plan.ExecutedAt = &now
	plan.UpdatedAt = now
	if err := s.store.SavePlan(ctx, plan); err != nil {
		return Plan{}, err
	}
	s.logf("event=trading_plan_executed plan=%s orders=%d", plan.ID, len(orders))
	return plan, nil
}

// MatchPending 对账户内所有挂起订单重新撮合，供盘中事件驱动与手动刷新使用。
func (s *Service) MatchPending(ctx context.Context, accountID string) ([]Order, error) {
	accountID = s.accountID(accountID)
	control, err := s.store.GetControl(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if control.KillSwitch {
		return nil, ErrKillSwitch
	}
	account, positions, err := s.AccountView(ctx, accountID)
	if err != nil {
		return nil, err
	}
	positionIndex := map[string]Position{}
	for _, position := range positions {
		positionIndex[position.Symbol] = position
	}
	orders, err := s.store.ListPendingOrders(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for index := range orders {
		if control.PauseBuying && orders[index].Side == SideBuy {
			continue
		}
		updated, matchErr := s.match(ctx, account, positionIndex[orders[index].Symbol], orders[index])
		if matchErr != nil {
			orders[index].Status = OrderStatusRejected
			orders[index].RejectReason = matchErr.Error()
			orders[index].UpdatedAt = time.Now().UTC()
			_ = s.store.UpdateOrder(ctx, orders[index])
			continue
		}
		orders[index] = updated
		if updated.Status == OrderStatusFilled || updated.Status == OrderStatusPartial {
			if refreshed, positionsAfter, err := s.AccountView(ctx, accountID); err == nil {
				account = refreshed
				for _, item := range positionsAfter {
					positionIndex[item.Symbol] = item
				}
			}
		}
	}
	return orders, nil
}

func (s *Service) match(ctx context.Context, account Account, position Position, order Order) (Order, error) {
	if s.broker == nil {
		return order, ErrBrokerUnavailable
	}
	if s.quotes == nil {
		return order, errors.New("行情服务未配置")
	}
	quoteCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	quotes, err := s.quotes.Realtime(quoteCtx, []string{order.Symbol})
	if err != nil {
		return order, fmt.Errorf("获取行情失败：%w", err)
	}
	var quote foundation.Quote
	for _, item := range quotes {
		if item.Symbol == order.Symbol {
			quote = item
			break
		}
	}
	if quote.Symbol == "" {
		return order, fmt.Errorf("未获取到 %s 的有效行情", order.Symbol)
	}
	result, err := s.broker.Submit(context.WithoutCancel(quoteCtx), BrokerOrder{
		Account:  account,
		Position: position,
		Order:    order,
		Quote: QuoteView{
			Symbol:        quote.Symbol,
			Name:          quote.Name,
			Price:         quote.Price,
			PreviousClose: quote.PreviousClose,
		},
	})
	if err != nil {
		return order, err
	}
	switch result.Status {
	case OrderStatusFilled, OrderStatusPartial:
		remaining := order.Quantity - order.FilledQuantity
		quantity := result.FilledQuantity
		if quantity <= 0 || quantity > remaining {
			quantity = remaining
		}
		filled, err := s.store.ApplyFill(ctx, order, quantity, result.Price, result.Fee)
		if err != nil {
			return order, err
		}
		return filled, nil
	case OrderStatusRejected:
		order.Status = OrderStatusRejected
		order.RejectReason = result.Reason
	case OrderStatusPending:
		order.Status = OrderStatusPending
		order.RejectReason = result.Reason
	}
	order.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateOrder(ctx, order); err != nil {
		return order, err
	}
	return order, nil
}

func (s *Service) CancelOrder(ctx context.Context, orderID string) (Order, error) {
	order, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return Order{}, err
	}
	if !order.IsOpen() {
		return Order{}, errors.New("只有挂起或部分成交的订单可以撤销")
	}
	// 先撤通道，再改本地。
	//
	// 顺序反过来会制造一个很贵的假象：本地显示已撤销、资金已释放，
	// 而通道那边还挂着，收盘前照样成交，账本就会凭空多出一笔没有冻结支撑的持仓。
	// 接实盘时这条顺序不是可选项，所以现在就用纸面通道把它固定下来。
	if err := s.cancelWithRetry(ctx, order); err != nil {
		return Order{}, fmt.Errorf("撤单未成功，订单保持挂起：%w", err)
	}
	closed, changed, err := s.store.CloseOrder(ctx, orderID, OrderStatusCancelled, "人工撤销")
	if err != nil {
		return Order{}, err
	}
	if !changed {
		// 撤单期间已经成交：如实返回最新状态，不要谎报"已撤销"。
		return s.store.GetOrder(ctx, orderID)
	}
	s.logf("event=trading_order_cancelled order=%s released=%.2f", orderID, order.ReservedAmount)
	return closed, nil
}

func (s *Service) RejectPlan(ctx context.Context, planID string) (Plan, error) {
	plan, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanStatusDraft {
		return Plan{}, ErrPlanNotDraft
	}
	orders, err := s.store.ListOrdersByPlan(ctx, planID)
	if err != nil {
		return Plan{}, err
	}
	for index := range orders {
		if orders[index].Status == OrderStatusPending || orders[index].Status == OrderStatusPartial {
			orders[index].Status = OrderStatusCancelled
			orders[index].UpdatedAt = time.Now().UTC()
			if err := s.store.UpdateOrder(ctx, orders[index]); err != nil {
				return Plan{}, err
			}
			if _, err := s.store.ReleaseOrderReservation(ctx, orders[index].ID); err != nil {
				s.logf("event=trading_release_failed order=%s err=%v", orders[index].ID, err)
			}
			orders[index].ReservedAmount = 0
			orders[index].FrozenAt = nil
		}
	}
	plan.Orders = orders
	plan.Status = PlanStatusRejected
	plan.UpdatedAt = time.Now().UTC()
	if err := s.store.SavePlan(ctx, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (s *Service) GetPlan(ctx context.Context, planID string) (Plan, error) {
	plan, err := s.store.GetPlan(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoTradingPlan
	}
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanStatusRunning {
		if orders, err := s.store.ListOrdersByPlan(ctx, planID); err == nil && len(orders) > 0 {
			plan.Orders = orders
		}
	}
	return plan, nil
}

func (s *Service) ListPlans(ctx context.Context, accountID string, limit int) ([]Plan, error) {
	return s.store.ListPlans(ctx, s.accountID(accountID), limit)
}

// Decisions 返回最近的裁决流水，新的在前。
func (s *Service) Decisions(ctx context.Context, accountID string, limit int) ([]Decision, error) {
	return s.store.ListDecisions(ctx, s.accountID(accountID), limit)
}

// ErrNoDecision 表示没找到对应的裁决记录。
var ErrNoDecision = errors.New("未找到裁决记录")

// Decision 按 ID 读取裁决；ID 为空时按计划 ID 取最新一条。
// 计划与裁决是一对一关系，但计划可以被重新生成，所以按计划查要取最新。
func (s *Service) Decision(ctx context.Context, id, planID string) (Decision, error) {
	var (
		decision Decision
		err      error
	)
	switch {
	case strings.TrimSpace(id) != "":
		decision, err = s.store.GetDecision(ctx, strings.TrimSpace(id))
	case strings.TrimSpace(planID) != "":
		decision, err = s.store.GetDecisionByPlan(ctx, strings.TrimSpace(planID))
	default:
		return Decision{}, errors.New("需要提供裁决 ID 或计划 ID")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, ErrNoDecision
	}
	if err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func (s *Service) ResetAccount(ctx context.Context, accountID string, initialCash float64) (Account, []Position, error) {
	account, err := s.store.ResetAccount(ctx, s.accountID(accountID), initialCash)
	if err != nil {
		return Account{}, nil, err
	}
	return s.AccountView(ctx, account.ID)
}

// Control 返回账户总闸状态。从未设置过时是「正常运行」，不写入任何记录。
func (s *Service) Control(ctx context.Context, accountID string) (Control, error) {
	return s.store.GetControl(ctx, s.accountID(accountID))
}

// SetControl 修改总闸。kill_switch 打开会强制 pause_buying 为真。
func (s *Service) SetControl(ctx context.Context, control Control) (Control, error) {
	control.AccountID = s.accountID(control.AccountID)
	saved, err := s.store.SaveControl(ctx, control)
	if err != nil {
		return Control{}, err
	}
	s.logf("event=trading_control account=%s gate=%s reason=%q", saved.AccountID, saved.Gate(), saved.Reason)
	return saved, nil
}

// Reconcile 重算应收冻结额并与账本比对。repair=false 时只报告不改账。
//
// 它存在的理由：冻结是一笔"看不见的账"，一旦漂移，下一步就是超买或假性资金不足，
// 而这两种故障都不会自己报错。所以需要一个能主动发现它的动作。
func (s *Service) Reconcile(ctx context.Context, accountID string, repair bool) (ReconcileReport, error) {
	accountID = s.accountID(accountID)
	// 先刷新持仓现价，让报告里的市值与总资产是当期的，而不是上次撮合时的。
	if _, _, err := s.AccountView(ctx, accountID); err != nil {
		s.logf("event=trading_reconcile_price_refresh_failed account=%s err=%v", accountID, err)
	}
	report, err := s.store.ReconcileReservations(ctx, accountID, repair)
	if err != nil {
		return ReconcileReport{}, err
	}
	s.logf("event=trading_reconcile account=%s ok=%t drift=%.2f repaired=%t issues=%d",
		accountID, report.OK, report.Drift, report.Repaired, len(report.Issues))
	return report, nil
}

func (s *Service) accountID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultAccountID
	}
	return value
}

func (s *Service) normalizeWatchlist(symbols []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(symbols))
	for _, raw := range symbols {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		symbol, err := foundation.NormalizeSymbol(raw)
		if err != nil {
			return nil, fmt.Errorf("关注池包含无效代码：%s", raw)
		}
		if seen[symbol.Canonical] {
			continue
		}
		seen[symbol.Canonical] = true
		out = append(out, symbol.Canonical)
	}
	if len(out) > 30 {
		return nil, errors.New("关注池最多 30 个标的")
	}
	// 空列表不是错误：调用方不指定标的时由系统从全市场自动选股。
	return out, nil
}

// MarketStatus 返回当前市场认知与候选池，不含账户信息，供观察接口使用。
// 与计划生成共用同一套计算，观察到的就是实际决策所依据的。
// equity 为 0 时跳过"单手金额占比"约束，仅作纯选股预览。
func (s *Service) MarketStatus(ctx context.Context, limit int, equity float64) (cognition.MarketView, universe.Board, error) {
	if s.catalog == nil {
		return cognition.MarketView{}, universe.Board{}, errors.New("未配置全市场行情源")
	}
	entries, err := s.catalog.StockCatalog(ctx)
	if err != nil {
		return cognition.MarketView{}, universe.Board{}, fmt.Errorf("获取全市场快照失败：%w", err)
	}
	market := cognition.View(entries, time.Now())
	options := universe.DefaultOptions()
	if limit > 0 {
		options.Limit = limit
	}
	options.Equity = equity
	return market, universe.Build(entries, market, options), nil
}

func (s *Service) buildSnapshot(ctx context.Context, accountID string, watchlist []string, autoUniverse bool, knownPositions []Position) (Snapshot, error) {
	if s.quotes == nil {
		return Snapshot{}, errors.New("行情服务未配置")
	}
	account, positions, err := s.AccountView(ctx, accountID)
	if err != nil {
		return Snapshot{}, err
	}
	if len(positions) == 0 {
		positions = knownPositions
	}

	// 市场认知：自动选股时是必需输入，拿不到就不该猜；显式指定标的时只作背景，
	// 失败不阻塞——否则一次行情源抖动会让用户手动指定的计划也无法生成。
	var (
		market   cognition.MarketView
		board    universe.Board
		hasBoard bool
		catalog  []foundation.StockCatalogEntry
	)
	if s.catalog != nil {
		catalogCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		entries, catalogErr := s.catalog.StockCatalog(catalogCtx)
		cancel()
		switch {
		case catalogErr != nil && autoUniverse:
			return Snapshot{}, fmt.Errorf("获取全市场快照失败，无法自动选股：%w", catalogErr)
		case catalogErr != nil:
			s.logf("event=trading_catalog_failed account=%s err=%v", accountID, catalogErr)
		default:
			catalog = entries
			// 目录缓存供调研工具按行业下钻。放在这里而不是工具里重新拉：
			// 同一轮里"表格里的行业"和"工具里的行业"必须是同一份数据。
			s.rememberCatalog(entries)
			market = cognition.View(entries, time.Now())
			if autoUniverse {
				options := universe.DefaultOptions()
				// 候选池必须知道账户规模：10 万账户买不起 500 元股票的小仓位。
				options.Equity = account.Equity
				board = universe.Build(entries, market, options)
				hasBoard = true
				watchlist = board.Symbols()
				if len(watchlist) == 0 {
					// 空池也要把市场认知带出去：失败原因是"市场恐慌"还是"筛不出票"，
					// 对使用者是完全不同的信息，不能只留一句错误文字。
					return Snapshot{Market: market, Board: &board, AutoUniverse: true},
						errors.New(emptyUniverseReason(board))
				}
			}
		}
	} else if autoUniverse {
		return Snapshot{}, errors.New("未配置全市场行情源，无法自动选股")
	}

	symbolSet := map[string]bool{}
	symbols := make([]string, 0, len(watchlist)+len(positions))
	for _, symbol := range watchlist {
		if !symbolSet[symbol] {
			symbolSet[symbol] = true
			symbols = append(symbols, symbol)
		}
	}
	for _, position := range positions {
		if !symbolSet[position.Symbol] {
			symbolSet[position.Symbol] = true
			symbols = append(symbols, position.Symbol)
		}
	}
	quoteCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	quotes, err := s.quotes.Realtime(quoteCtx, symbols)
	if err != nil {
		return Snapshot{}, fmt.Errorf("获取行情失败：%w", err)
	}
	quoted := map[string]bool{}
	valid := make([]foundation.Quote, 0, len(quotes))
	for _, quote := range quotes {
		if quote.Price > 0 {
			quoted[quote.Symbol] = true
			valid = append(valid, quote)
		}
	}
	missing := []string{}
	for _, symbol := range symbols {
		if !quoted[symbol] {
			missing = append(missing, symbol)
		}
	}
	// 在途买入挂单要同时进入权重的计算口径：否则同一标的最多能被两份计划各买一次上限。
	openBuyValue, err := s.store.PendingBuyValue(ctx, accountID)
	if err != nil {
		s.logf("event=trading_open_buy_load_failed account=%s err=%v", accountID, err)
		openBuyValue = map[string]float64{}
	}
	pendingBuyValue := 0.0
	for _, value := range openBuyValue {
		pendingBuyValue += value
	}
	snapshot := Snapshot{
		TakenAt:         time.Now().UTC(),
		Equity:          account.Equity,
		Cash:            account.Cash,
		ReservedCash:    account.ReservedCash,
		OpenBuyValue:    openBuyValue,
		PendingBuyValue: round2(pendingBuyValue),
		Quotes:          valid,
		Positions:       positions,
		Universe:        symbols,
		MissingQuotes:   missing,
		Market:          market,
		AutoUniverse:    autoUniverse,
		Sectors:         sectorsFor(catalog, symbolSet),
	}
	if hasBoard {
		snapshot.Board = &board
	}
	return snapshot, nil
}

// sectorsFor 只保留本次涉及的标的的行业归并结果。
// 全市场映射有 5000 多条，随计划落库会让快照膨胀十倍，而决策只用得到这几十个。
func sectorsFor(catalog []foundation.StockCatalogEntry, symbols map[string]bool) map[string]string {
	if len(catalog) == 0 || len(symbols) == 0 {
		return nil
	}
	out := make(map[string]string, len(symbols))
	for _, entry := range catalog {
		if !symbols[entry.Symbol] || strings.TrimSpace(entry.Industry) == "" {
			continue
		}
		out[entry.Symbol] = universe.SectorOf(entry.Industry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emptyUniverseReason 把空候选池翻译成一句人能看懂的话。
// "没有候选"背后的原因差别很大：可能是市场恐慌，也可能是行情源只返回了几十只。
func emptyUniverseReason(board universe.Board) string {
	if len(board.Exclusions) == 0 {
		return "候选池为空：全市场没有满足条件的标的"
	}
	reason := board.Exclusions[0]
	if reason.Count > 0 && reason.Reason != "" {
		return fmt.Sprintf("候选池为空：%s（影响 %d 只）", reason.Reason, reason.Count)
	}
	return "候选池为空：" + reason.Reason
}

// beginRun 抢占"正在生成计划"的单飞席位。返回 false 表示已有一轮在跑。
// 单飞的粒度是进程级的：同一时刻只允许一次模型调用，
// 因为两次并发调用会各自基于同一份现金做决策，最后必然有一笔被账本拒绝。
func (s *Service) beginRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Service) clearRunning() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

func (s *Service) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}
