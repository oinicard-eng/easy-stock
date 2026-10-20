import { requestJSON, type BackendConfig } from './backend';

export type TradeSide = 'buy' | 'sell';
export type TradeOrderType = 'limit' | 'market';
export type TradePlanStatus = 'running' | 'draft' | 'executed' | 'rejected' | 'failed' | 'skipped';
export type TradeOrderStatus = 'pending' | 'filled' | 'partially_filled' | 'cancelled' | 'rejected' | 'expired';

export type TradeAccount = {
	id: string;
	name: string;
	initial_cash: number;
	/** 账面现金，包含已被挂单冻结的部分 */
	cash: number;
	/** 未成交买入挂单占用的资金，不能用于新开仓 */
	reserved_cash: number;
	market_value: number;
	equity: number;
	created_at: string;
	updated_at: string;
};

/** 可用资金 = cash - reserved_cash，是唯一可动用的钱 */
export function availableCash(account: TradeAccount): number {
	return Math.round((account.cash - (account.reserved_cash || 0)) * 100) / 100;
}

/** 失效条件的可执行形态。字段为空表示模型只写了文字，没有可判定条件。 */
export type TradeInvalidationSpec = {
	price_below?: number;
	price_above?: number;
	sector_flow?: string;
	deadline_days?: number;
	note?: string;
};

/** 止损后的禁买约束：标的级 4 小时、行业级当日。 */
export type TradeCooldown = {
	symbol?: string;
	sector?: string;
	until: string;
	reason: string;
	trigger?: string;
};

export type TradeCooldownView = {
	symbols?: Record<string, TradeCooldown>;
	sectors?: Record<string, TradeCooldown>;
};

export type TradePosition = {
	account_id: string;
	symbol: string;
	name: string;
	quantity: number;
	available_quantity: number;
	/** 未成交卖单占用的股数；可卖 = available_quantity - reserved_quantity */
	reserved_quantity?: number;
	average_cost: number;
	last_price: number;
	market_value: number;
	unrealized_pnl: number;
	/** 持仓防守线，由买入建议写入、止盈后自动上移到成本价 */
	stop_loss_price?: number;
	take_profit_price?: number;
	/** 持仓期间最高价，移动止损据此计算回撤 */
	peak_price?: number;
	/** 已执行的止盈级数 */
	reduce_count?: number;
	sector?: string;
	opened_at?: string;
	last_buy_date?: string;
	/** 买入理由原文：持仓上留一份，计划过期后仍能回答「当初为什么买」 */
	thesis?: string;
	/** 失效条件原文 */
	invalidation?: string;
	/** 失效条件的可执行形态，守卫据此判定 */
	invalidation_rule?: TradeInvalidationSpec;
	/** 预期兑现天数（交易日），时间止损的阈值来源 */
	expected_days?: number;
	/** 失效条件已触发过一次自动减仓的时刻 */
	auto_reduce_at?: string;
};

export type TradeOrder = {
	id: string;
	plan_id: string;
	account_id: string;
	decision_id?: string;
	cycle_id?: string;
	symbol: string;
	name: string;
	/** 行业归并后的大类；为空表示当时无行业信息，不参与行业集中度判断 */
	sector?: string;
	side: TradeSide;
	order_type: TradeOrderType;
	quantity: number;
	limit_price: number;
	target_weight_percent?: number;
	stop_loss_price?: number;
	take_profit_price?: number;
	status: TradeOrderStatus;
	filled_quantity: number;
	filled_price: number;
	fee: number;
	/** 该订单当前仍冻结的资金额 */
	reserved_amount?: number;
	/** 卖单冻结的股数 */
	reserved_quantity?: number;
	/** 订单来源：model 模型建议 / protective 防守线自动 / manual 人工 */
	source?: string;
	/** 离场原因：stop_loss / take_profit_1 / trail_stop / hard_stop / model_exit */
	trigger?: string;
	/** 成交后的已实现盈亏，由账本写入 */
	realized_pnl?: number;
	reject_reason?: string;
	thesis?: string;
	invalidation?: string;
	invalidation_rule?: TradeInvalidationSpec;
	expected_days?: number;
	confidence?: number;
	created_at: string;
	updated_at: string;
};

/** 账户总闸。kill_switch 打开时 pause_buying 必为真。 */
export type TradeControl = {
	account_id: string;
	pause_buying: boolean;
	kill_switch: boolean;
	reason?: string;
	updated_at?: string;
};

export type TradeControlRequest = {
	account_id?: string;
	pause_buying: boolean;
	kill_switch: boolean;
	reason?: string;
};

/** 冻结额对账报告 */
export type TradeReconcileReport = {
	account_id: string;
	taken_at: string;
	ok: boolean;
	cash: number;
	reserved_cash: number;
	expected_reserved: number;
	drift: number;
	market_value: number;
	equity: number;
	open_buy_orders: number;
	orphan_orders?: string[];
	issues?: TradeIssue[];
	repaired: boolean;
};

export type TradeIssue = {
	level: 'error' | 'warning';
	field: string;
	message: string;
	/** 稳定裁决代码，如 risk.single_name */
	code?: string;
};

/** 裁决级别：block 拒绝、adjust 已调整、warn 提示。 */
export type TradeVerdictLevel = 'block' | 'adjust' | 'warn';
export type TradeVerdictScope = 'account' | 'market' | 'order' | 'portfolio' | 'position';

/** 一条裁决记录。metric/limit 是当时依据的数字，复盘时可重算。 */
export type TradeVerdict = {
	code: string;
	level: TradeVerdictLevel;
	scope: TradeVerdictScope;
	symbol?: string;
	field?: string;
	message: string;
	metric?: number;
	limit?: number;
};

/** 决策前后的仓位视图，回答"为什么只买了这么点"。 */
export type TradeExposure = {
	before_percent: number;
	after_percent: number;
	/** 未成交买入挂单占用的仓位，已计入 before */
	pending_percent: number;
	cash_percent: number;
	/** 本次实际生效的仓位上限（市场预算与风控上限取较小值） */
	budget_percent: number;
	buy_notional: number;
	sell_notional: number;
	sectors?: { sector: string; weight: number; names: number }[];
};

export type TradeDecisionStats = {
	proposed: number;
	accepted: number;
	blocked: number;
	adjusted: number;
	warned: number;
	ordered: number;
};

/**
 * 一次裁决的完整记录。计划只说明下了什么单，裁决说明为什么是这些、为什么不是那些。
 */
export type TradeDecision = {
	id: string;
	account_id: string;
	plan_id?: string;
	cycle_id?: string;
	taken_at: string;
	/** auto 表示候选池由系统全市场选出 */
	mode: string;
	gate: string;
	market?: TradeMarketView;
	rules: TradePlan['rules'];
	/** 模型原始建议，含被拦掉的部分 */
	proposal?: {
		symbol: string;
		name?: string;
		side: TradeSide;
		target_weight_percent?: number;
		sell_fraction_percent?: number;
		order_type?: string;
		limit_price?: number;
		stop_loss_price?: number;
		thesis?: string;
		confidence?: number;
	}[];
	model_regime?: string;
	model_summary?: string;
	risk_notes?: string[];
	verdicts?: TradeVerdict[];
	stats: TradeDecisionStats;
	/** 本次模型调用的耗时与 token，用于判断自动交易的运行成本 */
	model?: {
		latency_ms?: number;
		prompt_chars?: number;
		content_chars?: number;
		prompt_tokens?: number;
		completion_tokens?: number;
		total_tokens?: number;
		model?: string;
	};
	/** 因门控或市场状态被整批剔除的建议数 */
	dropped?: number;
	exposure: TradeExposure;
	narrative?: string[];
};

export type TradeQuote = {
	symbol: string;
	name: string;
	price: number;
	previous_close: number;
	change_percent: number;
	high: number;
	low: number;
	amount: number;
};

/** 市场状态。deterministic 统计得出，不是模型观点。 */
export type TradeMarketBreadth = {
	total: number;
	tradable: number;
	advancing: number;
	declining: number;
	unchanged: number;
	limit_up: number;
	limit_down: number;
	median_change: number;
	average_change: number;
	total_amount: number;
	advance_ratio: number;
};

export type TradeIndustryHeat = {
	name: string;
	average_change: number;
	advancing: number;
	total: number;
	rank: number;
	leader_symbol?: string;
	leader_name?: string;
};

export type TradeMarketView = {
	/** false 表示本轮没有做市场评估，此时不限制交易 */
	evaluated: boolean;
	regime: string;
	label: string;
	/** 本轮允许的最大总仓位，与风控上限取较小值 */
	risk_budget_percent: number;
	breadth: TradeMarketBreadth;
	industries?: TradeIndustryHeat[];
	notes?: string[];
	taken_at?: string;
};

/** 候选标的，含每一分的来源 */
export type TradeCandidate = {
	symbol: string;
	name: string;
	industry?: string;
	/** 细分行业归并后的大类，用于说明分散依据 */
	sector?: string;
	price: number;
	change_percent: number;
	five_day_change_percent: number;
	amount: number;
	score: number;
	rank: number;
	reasons: string[];
};

export type TradeExclusion = {
	reason: string;
	count: number;
	samples?: string[];
};

export type TradeBoard = {
	taken_at: string;
	source_count: number;
	threshold_amount: number;
	/** 当日缩量，成交额门槛已下调 */
	degraded: boolean;
	/** 因分散约束未选满目标数量 */
	capped: boolean;
	candidates: TradeCandidate[];
	exclusions?: TradeExclusion[];
	market?: TradeMarketView;
};

export const tradeRegimeText: Record<string, string> = {
	unknown: '数据不足',
	panic: '恐慌',
	ebb: '退潮',
	range: '震荡',
	uptrend: '上行',
	euphoria: '亢奋',
};

export type TradePlan = {
	id: string;
	account_id: string;
	status: TradePlanStatus;
	trader_profile: string;
	horizon: string;
	regime: string;
	gate?: string;
	summary: string;
	risk_notes?: string[];
	/** 本次裁决 ID；裁决流水里可查到完整依据 */
	decision_id?: string;
	/** 裁决流水副本，随计划一起返回 */
	verdicts?: TradeVerdict[];
	/** 决策前后的仓位与预算 */
	exposure?: TradeExposure;
	/** 计划级风险复核：组合视角的额度余量，不是闸门 */
	risk_review?: TradePlanRiskReview;
	orders: TradeOrder[];
	issues?: TradeIssue[];
	snapshot: {
		taken_at: string;
		equity: number;
		cash: number;
		reserved_cash?: number;
		quotes: TradeQuote[];
		positions: TradePosition[];
		universe: string[];
		missing_quotes?: string[];
		/** 市场环境判断，本轮是否评估过看 evaluated */
		market?: TradeMarketView;
		/** 自动选股的候选池明细 */
		board?: TradeBoard;
		/** 候选池由系统自动生成，而非人工指定 */
		auto_universe?: boolean;
		/** 本次计划时生效的止损冷静期约束 */
		cooldowns?: TradeCooldownView;
	};
	rules: {
		max_orders: number;
		max_single_weight_percent: number;
		max_total_exposure_percent: number;
		min_cash_percent: number;
		max_risk_per_trade_percent: number;
		max_stop_loss_percent: number;
		board_lot: number;
		/** 单一行业仓位上限 */
		max_sector_weight_percent?: number;
		/** 单一行业最多持有只数 */
		max_sector_names?: number;
	};
	/** 生成计划前的调研记录：查了什么、看到什么、得出什么事实结论 */
	research?: TradeResearch;
	error?: string;
	created_at: string;
	updated_at: string;
	executed_at?: string;
};

export type TradeAccountView = {
	account: TradeAccount;
	positions: TradePosition[];
	control: TradeControl;
	broker: string;
	limits: TradePlan['rules'];
};

export type TradePlanRequest = {
	/** 留空表示由系统从全市场自动选股 */
	watchlist?: string[];
	/** 显式要求自动选股，即使传了 watchlist 也会被忽略 */
	auto_universe?: boolean;
	trader_profile?: string;
	horizon?: string;
	notes?: string;
};

export async function fetchTradingMarket(config: BackendConfig, signal?: AbortSignal): Promise<TradeMarketView> {
	const payload = await requestJSON<{ data: { market: TradeMarketView; source_count: number } }>(
		config,
		'/api/v1/trading/market',
		{ signal },
	);
	return payload.data.market;
}

export async function fetchTradingUniverse(config: BackendConfig, signal?: AbortSignal): Promise<TradeBoard> {
	const payload = await requestJSON<{ data: TradeBoard }>(config, '/api/v1/trading/universe', { signal });
	return payload.data;
}

export async function fetchTradingAccount(config: BackendConfig, signal?: AbortSignal): Promise<TradeAccountView> {
	const payload = await requestJSON<{ data: TradeAccountView }>(config, '/api/v1/trading/account', { signal });
	return payload.data;
}

export async function resetTradingAccount(config: BackendConfig, initialCash: number): Promise<TradeAccountView> {
	const payload = await requestJSON<{ data: TradeAccountView }>(config, '/api/v1/trading/account/reset', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ initial_cash: initialCash }),
	});
	return payload.data;
}

export async function createTradePlan(config: BackendConfig, request: TradePlanRequest, signal?: AbortSignal): Promise<TradePlan> {
	const payload = await requestJSON<{ data: TradePlan }>(config, '/api/v1/trading/plans', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(request),
		signal,
	});
	return payload.data;
}

export async function fetchTradePlan(config: BackendConfig, id: string, signal?: AbortSignal): Promise<TradePlan> {
	const payload = await requestJSON<{ data: TradePlan }>(config, `/api/v1/trading/plans/${encodeURIComponent(id)}`, { signal });
	return payload.data;
}

export async function listTradePlans(config: BackendConfig, limit = 10, signal?: AbortSignal): Promise<TradePlan[]> {
	const payload = await requestJSON<{ data: TradePlan[] }>(config, `/api/v1/trading/plans?limit=${limit}`, { signal });
	return payload.data || [];
}

/** 裁决流水：每次"为什么这么下单"的结构化记录，新的在前。 */
export async function listTradeDecisions(config: BackendConfig, limit = 20, signal?: AbortSignal): Promise<TradeDecision[]> {
	const payload = await requestJSON<{ data: TradeDecision[] }>(
		config,
		`/api/v1/trading/decisions?limit=${encodeURIComponent(String(limit))}`,
		{ signal },
	);
	return payload.data || [];
}

/** 按计划取那次裁决；没有记录时返回 null，而不是抛错——历史计划可能早于本功能。 */
export async function fetchTradeDecisionByPlan(
	config: BackendConfig,
	planId: string,
	signal?: AbortSignal,
): Promise<TradeDecision | null> {
	try {
		const payload = await requestJSON<{ data: TradeDecision }>(
			config,
			`/api/v1/trading/decisions?plan_id=${encodeURIComponent(planId)}`,
			{ signal },
		);
		return payload.data ?? null;
	} catch {
		return null;
	}
}

/** 沙盘回放：同一份输入、只换风控阈值，逐笔列出差异。纯读，不改真实订单。 */
export type TradeSandboxSide = {
	profile: string;
	rules: TradePlan['rules'];
	accepted: number;
	blocked: number;
	adjusted: number;
	buy_notional: number;
	exposure_percent: number;
};

export type TradeSandboxChange = {
	symbol: string;
	name?: string;
	/** added 新规则放行 / removed 新规则拒绝 / resized 股数变化 */
	kind: 'added' | 'removed' | 'resized' | 'blocked' | 'kept';
	original: string;
	replayed: string;
	reason?: string;
};

export type TradeSandboxResult = {
	plan_id: string;
	planned_at: string;
	original: TradeSandboxSide;
	replayed: TradeSandboxSide;
	changes: TradeSandboxChange[];
	notes: string[];
};

export async function sandboxTradePlan(
	config: BackendConfig,
	id: string,
	profile: string,
): Promise<TradeSandboxResult> {
	const payload = await requestJSON<{ data: TradeSandboxResult }>(
		config,
		`/api/v1/trading/plans/${encodeURIComponent(id)}/sandbox`,
		{
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ profile }),
		},
	);
	return payload.data;
}

export async function executeTradePlan(config: BackendConfig, id: string): Promise<TradePlan> {
	const payload = await requestJSON<{ data: TradePlan }>(config, `/api/v1/trading/plans/${encodeURIComponent(id)}/execute`, { method: 'POST' });
	return payload.data;
}

export async function rejectTradePlan(config: BackendConfig, id: string): Promise<TradePlan> {
	const payload = await requestJSON<{ data: TradePlan }>(config, `/api/v1/trading/plans/${encodeURIComponent(id)}/reject`, { method: 'POST' });
	return payload.data;
}

export async function matchPendingOrders(config: BackendConfig): Promise<TradeOrder[]> {
	const payload = await requestJSON<{ data: TradeOrder[] }>(config, '/api/v1/trading/match', { method: 'POST' });
	return payload.data || [];
}

export async function cancelTradeOrder(config: BackendConfig, id: string): Promise<TradeOrder> {
	const payload = await requestJSON<{ data: TradeOrder }>(config, `/api/v1/trading/orders/${encodeURIComponent(id)}/cancel`, { method: 'POST' });
	return payload.data;
}

export async function fetchTradingControl(config: BackendConfig, signal?: AbortSignal): Promise<TradeControl> {
	const payload = await requestJSON<{ data: TradeControl }>(config, '/api/v1/trading/control', { signal });
	return payload.data;
}

export async function setTradingControl(config: BackendConfig, request: TradeControlRequest): Promise<TradeControl> {
	const payload = await requestJSON<{ data: TradeControl }>(config, '/api/v1/trading/control', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(request),
	});
	return payload.data;
}

export async function reconcileTradingAccount(config: BackendConfig, repair = false): Promise<TradeReconcileReport> {
	const payload = await requestJSON<{ data: TradeReconcileReport }>(
		config,
		`/api/v1/trading/reconcile${repair ? '?repair=true' : ''}`,
		{ method: 'POST' },
	);
	return payload.data;
}

/** 门控文案，与后端 Control.Gate() 保持一致 */
export function controlGateText(control: TradeControl | null | undefined): string {
	if (!control) return '未知';
	if (control.kill_switch) return '全部停止';
	if (control.pause_buying) return '暂停买入';
	return '正常运行';
}

/** 自动循环配置。默认关闭，打开后才会按间隔发起新一轮。 */
export type TradeAutopilotConfig = {
	account_id: string;
	enabled: boolean;
	/** 打开后计划生成即自动撮合；关闭时停在待确认，等人工扣扳机 */
	auto_execute: boolean;
	interval_seconds: number;
	trader_profile: string;
	horizon: string;
	max_daily_loss_percent: number;
	buy_cutoff: string;
	max_cycles_per_day: number;
	updated_at?: string;
};

export type TradeCycleGate = {
	code: string;
	label: string;
	open: boolean;
	detail?: string;
};

/** 一轮自动循环。它是「交易员日志」的主键：下面挂着计划、裁决与订单。 */
export type TradeCycle = {
	id: string;
	account_id: string;
	trigger: string;
	status: string;
	reason?: string;
	started_at: string;
	ended_at?: string;
	duration_ms?: number;
	plan_id?: string;
	decision_id?: string;
	equity_before: number;
	equity_after: number;
	day_start_equity: number;
	day_change_percent: number;
	orders: number;
	filled: number;
	blocked: number;
	matched: number;
	breaker?: string;
	gates?: TradeCycleGate[];
	notes?: string[];
};

/** 组合级回撤状态。高点只增不减，解除时才重置为现值。 */
export type TradeDrawdownState = {
	account_id: string;
	peak_equity: number;
	peak_at?: string;
	equity: number;
	drawdown_percent: number;
	level?: '' | 'drawdown.warn' | 'drawdown.stop';
	policy: { warn_percent: number; stop_percent: number; release_percent: number };
	updated_at?: string;
};

/** 复核条目：role 说明谁下的结论，level 是 pass / warn / block。 */
export type TradeRiskReviewItem = {
	role: string;
	code: string;
	level: 'pass' | 'warn' | 'block';
	message: string;
	metric?: number;
	limit?: number;
};

export type TradePlanRiskReview = {
	verdict: 'pass' | 'warn' | 'block';
	reviewed_at: string;
	items: TradeRiskReviewItem[];
	summary: string[];
	stats?: { proposed: number; accepted: number; blocked: number; adjusted: number };
};

export type TradeBreakerView = {
	code: string;
	label: string;
	/** 规则是否真的会生效；false 表示链路未接通，不能当成保护已就位 */
	armed: boolean;
	tripped: boolean;
	detail?: string;
};

export type TradeDayState = {
	account_id: string;
	trading_day: string;
	start_equity: number;
	stop_streak: number;
	breaker?: string;
	breaker_detail?: string;
	breaker_at?: string;
};

export type TradeAutopilotStatus = {
	config: TradeAutopilotConfig;
	running: boolean;
	last_cycle?: TradeCycle;
	today: TradeDayState;
	breakers: TradeBreakerView[];
	/** 组合级回撤状态。跨日存在，不能从 today 推导。 */
	risk?: TradeDrawdownState;
	next_run_at?: string;
	cycles_today: number;
};

export const tradeCycleStatusText: Record<string, string> = {
	running: '执行中',
	done: '已完成',
	skipped: '已跳过',
	blocked: '被拦截',
	failed: '失败',
	aborted: '中断',
};

export const tradeCycleTriggerText: Record<string, string> = {
	schedule: '调度',
	event: '事件',
	manual: '人工',
	resume: '重启补跑',
};

export async function fetchTradingAutopilot(config: BackendConfig, signal?: AbortSignal): Promise<TradeAutopilotStatus> {
	const payload = await requestJSON<{ data: TradeAutopilotStatus }>(config, '/api/v1/trading/autopilot', { signal });
	return payload.data;
}

export async function configureTradingAutopilot(
	config: BackendConfig,
	request: Partial<Omit<TradeAutopilotConfig, 'account_id' | 'updated_at'>>,
): Promise<TradeAutopilotStatus> {
	const payload = await requestJSON<{ data: TradeAutopilotStatus }>(config, '/api/v1/trading/autopilot', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(request),
	});
	return payload.data;
}

export async function runTradingCycle(config: BackendConfig): Promise<TradeCycle> {
	const payload = await requestJSON<{ data: TradeCycle }>(config, '/api/v1/trading/cycles/run', { method: 'POST' });
	return payload.data;
}

export async function listTradeCycles(config: BackendConfig, limit = 20, signal?: AbortSignal): Promise<TradeCycle[]> {
	const payload = await requestJSON<{ data: TradeCycle[] }>(
		config,
		`/api/v1/trading/cycles?limit=${encodeURIComponent(String(limit))}`,
		{ signal },
	);
	return payload.data || [];
}

/** 人工解除当日熔断。必须给原因：这是唯一一处人能推翻机器保护的入口。 */
export async function resetTradingBreaker(config: BackendConfig, reason: string): Promise<TradeDayState> {
	const payload = await requestJSON<{ data: TradeDayState }>(config, '/api/v1/trading/autopilot/breaker/reset', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ reason }),
	});
	return payload.data;
}

export const tradePlanStatusText: Record<TradePlanStatus, string> = {
	running: '生成中',
	draft: '待确认',
	executed: '已执行',
	rejected: '已放弃',
	failed: '未通过',
	skipped: '无变化跳过',
};

export const tradeOrderStatusText: Record<TradeOrderStatus, string> = {
	pending: '挂单中',
	filled: '已成交',
	partially_filled: '部分成交',
	cancelled: '已撤销',
	rejected: '已拒绝',
	expired: '收盘作废',
};

export function formatMoney(value: number): string {
	return value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}


/** 一笔已实现平仓。复盘唯一可信的原子事实。 */
export type TradeClosedTrade = {
	id: string;
	account_id: string;
	symbol: string;
	name: string;
	sector?: string;
	quantity: number;
	entry_price: number;
	exit_price: number;
	fee: number;
	pnl: number;
	pnl_percent: number;
	trigger?: string;
	source?: string;
	order_id: string;
	decision_id?: string;
	cycle_id?: string;
	opened_at?: string;
	closed_at: string;
	hold_days: number;
	trading_day: string;
};

/** 一条从事实里提炼出的经验。样本不足时没有 win_rate 字段。 */
export type TradeLesson = {
	dimension: string;
	key: string;
	sample: number;
	wins: number;
	win_rate?: number;
	avg_pnl: number;
	total_pnl: number;
	reliable: boolean;
	text: string;
};

export type TradeCodeCount = {
	code: string;
	count: number;
};

export type TradeReviewFacts = {
	equity_start: number;
	equity_end: number;
	day_change_percent: number;
	closed_trades: number;
	wins: number;
	losses: number;
	win_rate: number;
	realized_pnl: number;
	gross_profit: number;
	gross_loss: number;
	profit_factor: number;
	avg_win: number;
	avg_loss: number;
	avg_hold_days: number;
	stop_loss_exits: number;
	hard_stop_exits: number;
	trail_stop_exits: number;
	take_profit_exits: number;
	model_exits: number;
	cycles: number;
	cycles_done: number;
	cycles_failed: number;
	cycles_skipped: number;
	plans: number;
	proposed: number;
	accepted: number;
	blocked: number;
	block_reasons?: TradeCodeCount[];
	model_latency_ms_avg: number;
	model_tokens: number;
	top_gainers?: { symbol: string; name: string; pnl: number; pnl_percent: number; trigger?: string }[];
	top_losers?: { symbol: string; name: string; pnl: number; pnl_percent: number; trigger?: string }[];
};

export type TradeReview = {
	account_id: string;
	trading_day: string;
	generated_at: string;
	facts: TradeReviewFacts;
	lessons: TradeLesson[];
	cumulative?: TradeLesson[];
	narrative: string[];
};

/** 持仓防守线一次的运行结果 */
export type TradeProtectiveRun = {
	checked: number;
	triggered: number;
	created: number;
	filled: number;
	decision_id?: string;
	orders: TradeOrder[];
	notes?: string[];
};

export async function setPositionProtection(
	config: BackendConfig,
	symbol: string,
	stopLossPrice: number,
	takeProfitPrice: number,
): Promise<TradePosition> {
	const payload = await requestJSON<{ data: TradePosition }>(
		config,
		`/api/v1/trading/positions/${encodeURIComponent(symbol)}/protection`,
		{ method: 'POST', body: JSON.stringify({ stop_loss_price: stopLossPrice, take_profit_price: takeProfitPrice }) },
	);
	return payload.data;
}

export async function runTradingProtective(config: BackendConfig): Promise<TradeProtectiveRun> {
	const payload = await requestJSON<{ data: TradeProtectiveRun }>(config, '/api/v1/trading/protective/run', { method: 'POST' });
	return payload.data;
}

export async function fetchClosedTrades(config: BackendConfig, limit = 50): Promise<TradeClosedTrade[]> {
	const payload = await requestJSON<{ data: TradeClosedTrade[] }>(config, `/api/v1/trading/closed-trades?limit=${limit}`);
	return payload.data ?? [];
}

/** 业绩归因：全部由后端从平仓账本算出，前端不做二次计算。 */
export type TradeDailyPnL = {
	day: string;
	trades: number;
	pnl: number;
	cumulative: number;
};

export type TradePerformance = {
	account_id: string;
	days: number;
	from: string;
	to: string;
	closed_trades: number;
	wins: number;
	losses: number;
	win_rate: number;
	realized_pnl: number;
	gross_profit: number;
	gross_loss: number;
	profit_factor: number;
	avg_win: number;
	avg_loss: number;
	avg_hold_days: number;
	expectancy: number;
	best_trade: number;
	worst_trade: number;
	stop_loss_exits: number;
	hard_stop_exits: number;
	trail_stop_exits: number;
	take_profit_exits: number;
	model_exits: number;
	daily: TradeDailyPnL[];
	lessons: TradeLesson[];
	by_symbol: TradeLesson[];
};

export async function fetchTradingPerformance(config: BackendConfig, days = 30, signal?: AbortSignal): Promise<TradePerformance> {
	const payload = await requestJSON<{ data: TradePerformance }>(config, `/api/v1/trading/performance?days=${days}`, { signal });
	return payload.data;
}

// TradeCalibrationBucket 是一个置信度区间的校准结果。
// gap = 真实胜率 − 平均自评：负值表示这个区间"说得很满、做得一般"。
export type TradeCalibrationBucket = {
	label: string;
	lower: number;
	upper: number;
	samples: number;
	wins: number;
	losses: number;
	flats: number;
	win_rate: number;
	avg_confidence: number;
	avg_pnl_percent: number;
	gap: number;
};

export type TradeCalibration = {
	account_id: string;
	samples: number;
	flats: number;
	missing: number;
	brier: number;
	buckets: TradeCalibrationBucket[];
	verdict: string;
	generated_at: string;
};

// fetchTradeCalibration 读置信度校准报告。
// 它是"给自己照镜子"的只读接口，不参与任何交易决策。
export async function fetchTradeCalibration(config: BackendConfig, signal?: AbortSignal): Promise<TradeCalibration> {
	const payload = await requestJSON<{ data: TradeCalibration }>(config, `/api/v1/trading/calibration`, { signal });
	return payload.data;
}

export async function fetchTradingReviews(config: BackendConfig, limit = 30): Promise<TradeReview[]> {
	const payload = await requestJSON<{ data: TradeReview[] }>(config, `/api/v1/trading/reviews?limit=${limit}`);
	return payload.data ?? [];
}

export async function runTradingReview(config: BackendConfig, tradingDay?: string): Promise<TradeReview> {
	const payload = await requestJSON<{ data: TradeReview }>(config, '/api/v1/trading/reviews/run', {
		method: 'POST',
		body: JSON.stringify(tradingDay ? { trading_day: tradingDay } : {}),
	});
	return payload.data;
}

/** 运行健康快照。status 三档：healthy / warning / critical。 */
export type TradeHealthIssue = {
	code: string;
	severity: 'info' | 'warning' | 'critical';
	message: string;
};

export type TradeHeartbeat = {
	running: boolean;
	scheduler_at?: string | null;
	last_tick_at?: string | null;
	last_cycle_at?: string | null;
	last_success_at?: string | null;
	last_error?: string;
	failure_streak: number;
	degraded: boolean;
};

export type TradeHealth = {
	status: 'healthy' | 'warning' | 'critical';
	checked_at: string;
	uptime_seconds: number;
	channel: string;
	account_id: string;
	autopilot: TradeHeartbeat;
	positions: number;
	open_orders: number;
	reserved_cash: number;
	ledger: {
		ok: boolean;
		drift: number;
		reserved_quantity_drift: number;
		orphan_orders: number;
	};
	breaker?: string;
	/** 组合级回撤状态：高点、现值、回撤与所处档位 */
	drawdown: TradeDrawdownState;
	issues: TradeHealthIssue[];
	/** 告警通道自身状态。没人收到通知和一直没出事，从外面看是一样的。 */
	alerts: TradeAlertHealth;
};

/** 告警通道状态：配了几个出口、最近 24 小时送出去多少、失败多少。 */
export type TradeAlertHealth = {
	configured: boolean;
	channels: string[];
	sent_24h: number;
	failed_24h: number;
	dropped: number;
};

/** 运行事件。kind 是稳定标识，message 给人看。 */
export type TradeEvent = {
	id: number;
	account_id: string;
	severity: 'info' | 'warning' | 'critical';
	kind: string;
	message: string;
	symbol?: string;
	payload?: unknown;
	created_at: string;
};

export type TradeChannelInfo = {
	name: string;
	live: boolean;
	supports_cancel: boolean;
	note: string;
};

/** 组合级风险状态：净值高点、回撤与总闸。与 health 分开：一个看账户，一个看系统。 */
export async function fetchTradingRisk(
	config: BackendConfig,
	signal?: AbortSignal,
): Promise<{ drawdown: TradeDrawdownState; control: TradeControl }> {
	const payload = await requestJSON<{ data: { drawdown: TradeDrawdownState; control: TradeControl } }>(
		config,
		'/api/v1/trading/risk',
		{ signal },
	);
	return payload.data;
}

/** 人工解除回撤熔断。必须给理由：这是唯一一处人推翻机器保护的入口。 */
export async function releaseTradingDrawdown(config: BackendConfig, reason: string): Promise<TradeDrawdownState> {
	const payload = await requestJSON<{ data: TradeDrawdownState }>(config, '/api/v1/trading/risk/drawdown/release', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ reason }),
	});
	return payload.data;
}

export async function fetchTradingHealth(config: BackendConfig, signal?: AbortSignal): Promise<TradeHealth> {
	const payload = await requestJSON<{ data: TradeHealth }>(config, '/api/v1/trading/health', { signal });
	return payload.data;
}

export async function fetchTradingEvents(config: BackendConfig, limit = 30, signal?: AbortSignal): Promise<TradeEvent[]> {
	const payload = await requestJSON<{ data: TradeEvent[] }>(config, `/api/v1/trading/events?limit=${limit}`, { signal });
	return payload.data ?? [];
}

export async function fetchTradingChannel(config: BackendConfig, signal?: AbortSignal): Promise<TradeChannelInfo> {
	const payload = await requestJSON<{ data: TradeChannelInfo }>(config, '/api/v1/trading/channel', { signal });
	return payload.data;
}


/** 调研中的一步。tool 为空表示这一步没有调用工具（例如模型直接收尾）。 */
export type TradeResearchStep = {
	index: number;
	thought: string;
	tool: string;
	args?: Record<string, unknown>;
	result?: string;
	rows?: number;
	latency_ms: number;
	error?: string;
	at: string;
};

/** 一轮调研的完整记录。degraded 为真时记录不完整，界面要明确提示。 */
export type TradeResearch = {
	enabled: boolean;
	degraded?: boolean;
	note?: string;
	conclusion?: string;
	steps: TradeResearchStep[];
	elapsed_ms: number;
};

/** 一条情景记忆：某笔交易发生时的市场状态、买入理由与结果。 */
export type TradeMemory = {
	id: string;
	trade_id: string;
	symbol: string;
	name?: string;
	sector?: string;
	trading_day: string;
	regime: string;
	regime_label: string;
	thesis?: string;
	invalidation?: string;
	confidence?: number;
	exit_trigger?: string;
	exit_source?: string;
	entry_price: number;
	exit_price: number;
	pnl: number;
	pnl_percent: number;
	hold_days: number;
	peak_gain_percent: number;
	give_back_percent: number;
	outcome: 'win' | 'loss' | 'flat' | string;
	lesson: string;
	created_at: string;
};

export type TradeMemoryQuery = {
	symbol?: string;
	sector?: string;
	regime?: string;
	trigger?: string;
	outcome?: string;
	keyword?: string;
	limit?: number;
};

function memoryQueryString(query: TradeMemoryQuery): string {
	const params = new URLSearchParams();
	for (const [key, value] of Object.entries(query)) {
		if (value === undefined || value === null || value === '') continue;
		params.set(key, String(value));
	}
	const text = params.toString();
	return text ? `?${text}` : '';
}

/** 读取某次计划的调研回放。计划详情里已带一份，这个接口用于单独刷新。 */
export async function fetchPlanResearch(config: BackendConfig, planId: string, signal?: AbortSignal): Promise<TradeResearchStep[]> {
	const payload = await requestJSON<{ data: TradeResearchStep[] }>(config, `/api/v1/trading/plans/${planId}/research`, { signal });
	return payload.data ?? [];
}

export async function fetchTradingMemory(config: BackendConfig, query: TradeMemoryQuery = {}, signal?: AbortSignal): Promise<TradeMemory[]> {
	const payload = await requestJSON<{ data: TradeMemory[] }>(config, `/api/v1/trading/memory${memoryQueryString(query)}`, { signal });
	return payload.data ?? [];
}

/** 补齐历史记忆。返回新增条数与当前总数。 */
export async function syncTradingMemory(config: BackendConfig): Promise<{ added: number; total: number }> {
	const payload = await requestJSON<{ data: { added: number; total: number } }>(config, '/api/v1/trading/memory/sync', { method: 'POST' });
	return payload.data;
}

/** 一条告警投递记录。status 说明这条告警到底有没有送到人手上。 */
export type TradeAlertRecord = {
	id: number;
	account_id: string;
	severity: 'info' | 'warning' | 'critical';
	kind: string;
	symbol?: string;
	message: string;
	channel?: string;
	status: 'sent' | 'failed' | 'suppressed';
	error?: string;
	merged: number;
	created_at: string;
};

export type TradeAlertsResult = {
	records: TradeAlertRecord[];
	channels: string[];
	stats: Record<string, number>;
};

export async function fetchTradingAlerts(config: BackendConfig, limit = 50, signal?: AbortSignal): Promise<TradeAlertsResult> {
	const payload = await requestJSON<{ data: TradeAlertsResult }>(config, `/api/v1/trading/alerts?limit=${limit}`, { signal });
	return {
		records: payload.data?.records ?? [],
		channels: payload.data?.channels ?? [],
		stats: payload.data?.stats ?? {},
	};
}

/** 主动发一条测试告警，验证出口配置是否真的能用。 */
export async function testTradingAlert(config: BackendConfig): Promise<{ delivered: string[]; error?: string }> {
	const payload = await requestJSON<{ data: { delivered: string[]; error?: string } }>(config, '/api/v1/trading/alerts/test', { method: 'POST' });
	return payload.data;
}

/** 时间线上的一个节点。category 决定分组与配色，severity 决定是否需要高亮。 */
export type TradeTimelineEntry = {
	at: string;
	category: string;
	severity?: 'info' | 'warning' | 'critical';
	/** 权限链位置：PROPOSE / EVIDENCE / VETO / DECIDE / EXECUTE */
	role?: string;
	title: string;
	detail?: string;
	ref_id?: string;
	symbol?: string;
	amount?: number;
};

export type TradeTimelineStats = {
	cycles: number;
	failed_cycles: number;
	plans: number;
	decisions: number;
	orders: number;
	filled: number;
	blocked: number;
	events: number;
	reviews: number;
	model_calls: number;
	total_tokens: number;
};

export type TradeTimeline = {
	day: string;
	account_id: string;
	from: string;
	to: string;
	stats: TradeTimelineStats;
	entries: TradeTimelineEntry[];
	/** 某张表读失败时的降级说明。非空表示这条线不完整。 */
	notes?: string[];
};

export async function fetchTradingTimeline(config: BackendConfig, day: string, signal?: AbortSignal): Promise<TradeTimeline> {
	const payload = await requestJSON<{ data: TradeTimeline }>(config, `/api/v1/trading/timeline?day=${encodeURIComponent(day)}`, { signal });
	return {
		day: payload.data?.day ?? day,
		account_id: payload.data?.account_id ?? '',
		from: payload.data?.from ?? '',
		to: payload.data?.to ?? '',
		stats: payload.data?.stats ?? {
			cycles: 0, failed_cycles: 0, plans: 0, decisions: 0,
			orders: 0, filled: 0, blocked: 0, events: 0, reviews: 0,
			model_calls: 0, total_tokens: 0,
		},
		entries: payload.data?.entries ?? [],
		notes: payload.data?.notes,
	};
}
