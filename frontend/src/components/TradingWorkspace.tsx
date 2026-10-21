import {
	Activity,
	AlertTriangle,
	Banknote,
	BellRing,
	Brain,
	CalendarClock,
	CheckCircle2,
	ChevronDown,
	ChevronUp,
	Compass,
	FlaskConical,
	Gauge,
	GitCompareArrows,
	History,
	LoaderCircle,
	PieChart,
	Play,
	PowerCircle,
	RefreshCw,
	Scale,
	ShieldAlert,
	ShieldCheck,
	Sparkles,
	Target,
	Timer,
	TrendingUp,
	Wallet,
	XCircle,
} from 'lucide-react';
import { Fragment, useCallback, useEffect, useMemo, useState } from 'react';
import type { BackendConfig } from '../lib/backend';
import { PanelErrorBoundary } from './PanelErrorBoundary';
import {
	availableCash,
	cancelTradeOrder,
	configureTradingAutopilot,
	controlGateText,
	createTradePlan,
	executeTradePlan,
	fetchClosedTrades,
	fetchTradeDecisionByPlan,
	fetchTradePlan,
	fetchTradingAccount,
	fetchTradingAutopilot,
	fetchTradingChannel,
	fetchTradingAlerts,
	fetchTradingEvents,
	fetchTradingHealth,
	fetchTradingMemory,
	fetchTradingPerformance,
	fetchTradeCalibration,
	fetchTradingReviews,
	fetchTradingTimeline,
	testTradingAlert,
	fetchTradingUniverse,
	formatMoney,
	signedMoney,
	listTradeCycles,
	listTradePlans,
	matchPendingOrders,
	reconcileTradingAccount,
	rejectTradePlan,
	releaseTradingDrawdown,
	resetTradingBreaker,
	sandboxTradePlan,
	resetTradingAccount,
	runTradingCycle,
	runTradingProtective,
	runTradingReview,
	setPositionProtection,
	syncTradingMemory,
	setTradingControl,
	currentIntervalSeconds,
	scheduleCycles,
	tradeCycleStatusText,
	tradeCycleTriggerText,
	tradeOrderStatusText,
	tradePlanStatusText,
	tradeRegimeText,
	type TradeAccountView,
	type TradeAlertRecord,
	type TradeAlertsResult,
	type TradeAutopilotStatus,
	type TradeBoard,
	type TradeClosedTrade,
	type TradeCycle,
	type TradeChannelInfo,
	type TradeDecision,
	type TradeDrawdownState,
	type TradeEvent,
	type TradeHealth,
	type TradeLesson,
	type TradeTimeline,
	type TradeTimelineEntry,
	type TradeMarketView,
	type TradeMemory,
	type TradeOrder,
	type TradePerformance,
	type TradeCalibration,
	type TradePlan,
	type TradeCooldownView,
	type TradePlanRiskReview,
	type TradeSandboxResult,
	type TradePosition,
	type TradeResearch,
	type TradeReview,
} from '../lib/trading';
import {
	findTradingGroupName,
	findTradingModule,
	resolveTradingView,
	tradingNavGroups,
	type TradingView,
} from '../lib/trading-nav';
import './trading-workspace.css';

type Props = {
	config: BackendConfig | null;
	onOpenSettings?: () => void;
};

const profileOptions = [
	{ value: 'aggressive', label: '激进' },
	{ value: 'balanced', label: '均衡' },
	{ value: 'steady', label: '稳健' },
];

const horizonOptions = ['1 个交易日', '1-3 个交易日', '一周以内'];

// 左侧导航每个视图的图标，与行情总览同一套视觉语言。
const tradingModuleIcons = {
	overview: Gauge,
	market: Activity,
	planning: Sparkles,
	positions: Wallet,
	autopilot: PowerCircle,
	performance: GitCompareArrows,
	review: Compass,
	ops: ShieldCheck,
} satisfies Record<TradingView, typeof Gauge>;

function parseWatchlist(text: string): string[] {
	return text
		.split(/[\s,，;；、]+/)
		.map((item) => item.trim())
		.filter(Boolean);
}

function signedPercent(value: number): string {
	return `${value >= 0 ? '+' : ''}${value.toFixed(2)}%`;
}

function amountText(value: number): string {
	if (!Number.isFinite(value) || value <= 0) return '—';
	if (value >= 1e12) return `${(value / 1e12).toFixed(2)} 万亿`;
	if (value >= 1e8) return `${(value / 1e8).toFixed(0)} 亿`;
	return `${(value / 1e4).toFixed(0)} 万`;
}

function intervalText(seconds: number): string {
	if (!Number.isFinite(seconds) || seconds <= 0) return '—';
	if (seconds >= 60 && seconds % 60 === 0) return `${seconds / 60} 分钟/轮`;
	return `${seconds} 秒/轮`;
}

/** 市场环境：确定性统计结果，与生成计划时用的是同一套计算。 */
function MarketPanel({ market, onRefresh, busy }: { market: TradeMarketView; onRefresh: () => void; busy: boolean }) {
	const breadth = market.breadth;
	return (
		<div className={`trading-market regime-${market.regime}`}>
			<div className="trading-market-head">
				<div className="trading-market-title">
					<Activity size={17} />
					<div>
						<strong>
							市场：{tradeRegimeText[market.regime] ?? market.label}
						</strong>
						<span>
							权益仓位预算 {market.risk_budget_percent}% · 全市场 {breadth.total} 只有效行情
						</span>
					</div>
				</div>
				<button type="button" className="trading-link" onClick={onRefresh} disabled={busy}>
					{busy ? <LoaderCircle size={13} className="spin" /> : <RefreshCw size={13} />} 刷新
				</button>
			</div>
			<div className="trading-market-grid">
				<div>
					<span>上涨 / 下跌</span>
					<strong>
						{breadth.advancing} / {breadth.declining}
					</strong>
				</div>
				<div>
					<span>涨停 / 跌停</span>
					<strong>
						{breadth.limit_up} / {breadth.limit_down}
					</strong>
				</div>
				<div>
					<span>涨幅中位数</span>
					<strong className={breadth.median_change >= 0 ? 'up' : 'down'}>{signedPercent(breadth.median_change)}</strong>
				</div>
				<div>
					<span>成交额</span>
					<strong>{amountText(breadth.total_amount)}</strong>
				</div>
			</div>
			<details className="trading-details" open>
				<summary>
					<ChevronDown size={13} /> 判断依据与行业强度
				</summary>
				<ul className="trading-note-list">
					{(market.notes ?? []).map((note) => (
						<li key={note}>{note}</li>
					))}
				</ul>
				{market.industries && market.industries.length > 0 ? (
					<div className="trading-industry-chips">
						{market.industries.map((item) => (
							<span key={item.name} className={item.average_change >= 0 ? 'up' : 'down'}>
								{item.name} {signedPercent(item.average_change)}
								<em>
									{item.advancing}/{item.total}
								</em>
							</span>
						))}
					</div>
				) : null}
			</details>
		</div>
	);
}

/** 候选池：系统自动选出的标的，每只都带入选依据，也能看到排除了什么。 */
function UniversePanel({
	board,
	loading,
	onRefresh,
}: {
	board: TradeBoard | null;
	loading: boolean;
	onRefresh: () => void;
}) {
	if (!board) {
		return (
			<div className="trading-universe empty">
				{loading ? (
					<>
						<LoaderCircle size={16} className="spin" /> 正在扫描全市场……
					</>
				) : (
					<>
						<Target size={16} /> 候选池暂不可用（行情源未就绪）
					</>
				)}
			</div>
		);
	}
	// 后端在「无候选」时可能返回 null（历史接口行为），这里必须兜底，
	// 否则一次 .length 就把整页炸成白屏。
	const candidates = board.candidates ?? [];
	const total = board.exclusions?.reduce((sum, item) => sum + item.count, 0) ?? 0;
	return (
		<div className="trading-universe">
			<div className="trading-universe-head">
				<div>
					<strong>
						<Target size={16} /> 候选池 {candidates.length} 只
					</strong>
					<span>
						从全市场 {board.source_count} 只中确定性筛出，成交额门槛 {(board.threshold_amount / 1e4).toFixed(0)} 万元
						{board.degraded ? '（当日缩量，门槛已下调）' : ''}
						{board.capped ? '；因分散约束未选满' : ''}
					</span>
				</div>
				<button type="button" className="trading-link" onClick={onRefresh} disabled={loading}>
					{loading ? <LoaderCircle size={13} className="spin" /> : <RefreshCw size={13} />} 重扫
				</button>
			</div>
			{candidates.length === 0 ? (
				<p className="trading-universe-empty">
					本轮没有候选：{(board.exclusions ?? [])[0]?.reason ?? '全市场没有满足条件的标的'}
				</p>
			) : (
				<>
					<div className="trading-universe-table">
						<table>
							<thead>
								<tr>
									<th>#</th>
									<th>标的</th>
									<th>评分</th>
									<th>当日</th>
									<th>5 日</th>
									<th>成交额</th>
									<th>风险大类</th>
								</tr>
							</thead>
							<tbody>
								{candidates.map((candidate) => (
									<tr key={candidate.symbol}>
										<td>{candidate.rank}</td>
										<td>
											<strong>{candidate.name}</strong>
											<span>{candidate.symbol}</span>
										</td>
										<td>{candidate.score.toFixed(1)}</td>
										<td className={candidate.change_percent >= 0 ? 'up' : 'down'}>
											{signedPercent(candidate.change_percent)}
										</td>
										<td className={candidate.five_day_change_percent >= 0 ? 'up' : 'down'}>
											{signedPercent(candidate.five_day_change_percent)}
										</td>
										<td>{amountText(candidate.amount)}</td>
										<td>{candidate.sector || candidate.industry || '—'}</td>
									</tr>
								))}
							</tbody>
						</table>
					</div>
					<details className="trading-details">
						<summary>
							<ChevronDown size={13} /> 每只标的的入选依据
						</summary>
						<ul className="trading-reason-list">
							{candidates.map((candidate) => (
								<li key={candidate.symbol}>
									<strong>
										{candidate.symbol} {candidate.name}
									</strong>
									<span>{candidate.reasons.join('；')}</span>
								</li>
							))}
						</ul>
					</details>
				</>
			)}
			{board.exclusions && board.exclusions.length > 0 ? (
				<details className="trading-details">
					<summary>
						<ChevronDown size={13} /> 被排除的标的（合计 {total} 只）
					</summary>
					<ul className="trading-note-list">
						{board.exclusions.map((exclusion) => (
							<li key={exclusion.reason}>
								{exclusion.reason}：{exclusion.count} 只
								{exclusion.samples && exclusion.samples.length > 0 ? `（如 ${exclusion.samples.join('、')}）` : ''}
							</li>
						))}
					</ul>
				</details>
			) : null}
		</div>
	);
}

const verdictLevelText: Record<string, string> = { block: '拦截', adjust: '调整', warn: '提示' };
const verdictLevelRank: Record<string, number> = { block: 0, adjust: 1, warn: 2 };
const verdictScopeText: Record<string, string> = {
	account: '账户',
	market: '市场',
	order: '单笔',
	portfolio: '组合',
	position: '持仓',
};

const reviewRoleText: Record<string, string> = {
	PROPOSE: '提议',
	EVIDENCE: '取证',
	VETO: '否决',
	DECIDE: '裁决',
	EXECUTE: '执行',
};
const reviewLevelText: Record<string, string> = { pass: '通过', warn: '留意', block: '越界' };

/**
 * 计划级风险复核：从组合视角回答"这一整套方案安全吗"。
 *
 * 它与下面的裁决列表不是同一件事：裁决是逐笔的通过/拒绝（这笔为什么被拒），
 * 复核是整体额度的余量（六笔各合规、合起来是否压在同一个行业）。
 * 每条都带角色标签，让人一眼看出这条结论是模型提的、代码否的，还是账本执行的。
 */
function RiskReviewBlock({ review }: { review: TradePlanRiskReview }) {
	const blocked = review.items.filter((item) => item.level === 'block').length;
	const warned = review.items.filter((item) => item.level === 'warn').length;
	return (
		<div className={`trading-review ${review.verdict}`}>
			<div className="trading-review-head">
				<ShieldCheck size={15} />
				<strong>风险复核</strong>
				<span className={`trading-review-verdict ${review.verdict}`}>{reviewLevelText[review.verdict] ?? review.verdict}</span>
				<span className="trading-subtle">
					{review.items.length} 项检查 · 越界 {blocked} · 留意 {warned}
				</span>
			</div>
			<ul className="trading-review-items">
				{review.items.map((item, index) => (
					<li key={`${item.code}-${index}`} className={item.level}>
						<span className={`trading-role ${item.role.toLowerCase()}`}>{reviewRoleText[item.role] ?? item.role}</span>
						<code className="trading-verdict-code">{item.code}</code>
						<span className="trading-review-message">{item.message}</span>
					</li>
				))}
			</ul>
		</div>
	);
}

/**
 * 裁决面板：把"模型想做什么、守卫允许什么、最后下了什么单"摊开。
 * 它比一串返回消息更有用——消息告诉你有事发生，裁决告诉你为什么。
 */
/**
 * 冷静期条：这一轮有哪些标的/行业因为刚止损而不可买。
 *
 * 单独显形而不是只写在裁决里：被拦的建议在裁决列表里只有一行代码，
 * 而"今天这只票不能碰"是交易员做决定时最需要先知道的事实。
 */
function CooldownBar({ view }: { view?: TradeCooldownView }) {
	const symbols = Object.values(view?.symbols ?? {});
	const sectors = Object.values(view?.sectors ?? {});
	if (symbols.length === 0 && sectors.length === 0) return null;
	return (
		<div className="trading-cooldowns">
			<span className="trading-subtle">止损冷静期</span>
			{symbols.map((item) => (
				<span key={`s-${item.symbol}`} className="trading-chip" title={`${item.reason}（至 ${item.until.slice(0, 16).replace('T', ' ')}）`}>
					{item.symbol} 禁买至 {item.until.slice(11, 16)}
				</span>
			))}
			{sectors.map((item) => (
				<span key={`k-${item.sector}`} className="trading-chip danger" title={item.reason}>
					{item.sector} 当日禁买
				</span>
			))}
		</div>
	);
}

const sandboxKindText: Record<string, string> = {
	added: '放行',
	removed: '拒绝',
	resized: '缩量',
	kept: '不变',
	blocked: '拦截',
};

/**
 * 沙盘回放：拿这一份计划的输入，换一套风控阈值重新裁决。
 *
 * 它是唯一能回答"换个风格会怎样"的地方，而且不需要重跑一天行情。
 * 界面上刻意把两侧并排给出，因为单独看"激进会买 87%"没有意义，
 * 有意义的是"稳健买 62%、激进买 87%，差在下面这三笔"。
 */
function SandboxPanel({ config, plan }: { config: BackendConfig; plan: TradePlan }) {
	const [profile, setProfile] = useState(plan.trader_profile === 'aggressive' ? 'steady' : 'aggressive');
	const [result, setResult] = useState<TradeSandboxResult | null>(null);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState('');

	const run = async () => {
		setBusy(true);
		setError('');
		try {
			const data = await sandboxTradePlan(config, plan.id, profile);
			setResult(data);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '沙盘回放失败');
		} finally {
			setBusy(false);
		}
	};

	return (
		<div className="trading-sandbox">
			<div className="trading-review-head">
				<GitCompareArrows size={15} />
				<strong>沙盘回放</strong>
				<span className="trading-subtle">同一份行情与建议，只换风控阈值；不改动真实订单</span>
			</div>
			<div className="trading-sandbox-controls">
				<select value={profile} onChange={(event) => setProfile(event.target.value)} disabled={busy}>
					<option value="steady">稳健</option>
					<option value="balanced">均衡</option>
					<option value="aggressive">激进</option>
				</select>
				<button type="button" className="trading-ghost" onClick={() => void run()} disabled={busy}>
					{busy ? '回放中…' : `按「${profileLabel(profile)}」重算`}
				</button>
				{error ? <span className="trading-error-text">{error}</span> : null}
			</div>
			{result ? (
				<div className="trading-sandbox-result">
					<div className="trading-sandbox-compare">
						<div>
							<span className="trading-subtle">当时（{profileLabel(result.original.profile)}）</span>
							<strong>{result.original.accepted} 笔受理 · {result.original.blocked} 笔拦截</strong>
							<span className="trading-subtle">买入 {formatMoney(result.original.buy_notional)} · 仓位 {result.original.exposure_percent.toFixed(1)}%</span>
						</div>
						<div>
							<span className="trading-subtle">回放（{profileLabel(result.replayed.profile)}）</span>
							<strong>{result.replayed.accepted} 笔受理 · {result.replayed.blocked} 笔拦截</strong>
							<span className="trading-subtle">买入 {formatMoney(result.replayed.buy_notional)} · 仓位 {result.replayed.exposure_percent.toFixed(1)}%</span>
						</div>
					</div>
					{result.changes.length > 0 ? (
						<ul className="trading-review-items">
							{result.changes.map((change) => (
								<li key={`${change.symbol}-${change.kind}`} className={change.kind === 'removed' ? 'block' : change.kind === 'added' ? 'warn' : ''}>
									<span className="trading-role">{sandboxKindText[change.kind] ?? change.kind}</span>
									<strong>{change.symbol}</strong>
									<span className="trading-review-message">
										{change.original} → {change.replayed}
										{change.reason ? `（${change.reason}）` : ''}
									</span>
								</li>
							))}
						</ul>
					) : (
						<p className="trading-subtle">两种阈值下逐笔结论一致，只有额度余量不同。</p>
					)}
					{result.notes.map((note) => <p key={note} className="trading-subtle">{note}</p>)}
				</div>
			) : null}
		</div>
	);
}

function profileLabel(profile: string): string {
	if (profile === 'aggressive') return '激进';
	if (profile === 'steady') return '稳健';
	if (profile === 'balanced') return '均衡';
	return profile || '原风格';
}

function DecisionPanel({ plan }: { plan: TradePlan }) {
	const [expanded, setExpanded] = useState(false);
	const verdicts = plan.verdicts ?? [];
	const exposure = plan.exposure;
	if (verdicts.length === 0 && !exposure) return null;
	const sorted = [...verdicts].sort((left, right) => {
		const byLevel = (verdictLevelRank[left.level] ?? 9) - (verdictLevelRank[right.level] ?? 9);
		return byLevel !== 0 ? byLevel : left.code.localeCompare(right.code);
	});
	const shown = expanded ? sorted : sorted.slice(0, 8);
	const blocked = verdicts.filter((verdict) => verdict.level === 'block').length;
	const adjusted = verdicts.filter((verdict) => verdict.level === 'adjust').length;
	const budget = exposure?.budget_percent ?? 0;
	const after = exposure?.after_percent ?? 0;
	const ratio = budget > 0 ? Math.min(100, Math.max(0, (after / budget) * 100)) : 0;
	return (
		<div className="trading-decision">
			<div className="trading-decision-head">
				<Scale size={15} />
				<strong>裁决</strong>
				<span className="trading-subtle">
					拦截 {blocked} · 调整 {adjusted} · 共 {verdicts.length} 条依据
				</span>
				{plan.decision_id ? <code className="trading-decision-id">{plan.decision_id}</code> : null}
			</div>
			{exposure ? (
				<div className="trading-exposure">
					<div className="trading-exposure-row">
						<span>仓位</span>
						<strong>{exposure.before_percent.toFixed(1)}%</strong>
						<span className="trading-arrow">→</span>
						<strong>{after.toFixed(1)}%</strong>
						<span className="trading-subtle">上限 {budget.toFixed(0)}%</span>
						<span className="trading-subtle">
							买入 {formatMoney(exposure.buy_notional)}
							{exposure.sell_notional > 0 ? ` · 卖出 ${formatMoney(exposure.sell_notional)}` : ''}
							{` · 现金 ${exposure.cash_percent.toFixed(1)}%`}
						</span>
					</div>
					<div className="trading-exposure-bar" title={`买入后仓位占预算 ${ratio.toFixed(0)}%`}>
						<span style={{ width: `${ratio}%` }} />
					</div>
					{exposure.sectors && exposure.sectors.length > 0 ? (
						<div className="trading-sector-chips">
							{exposure.sectors.slice(0, 6).map((sector) => (
								<span key={sector.sector} className="trading-chip">
									{sector.sector} {sector.weight.toFixed(1)}% · {sector.names} 只
								</span>
							))}
						</div>
					) : null}
				</div>
			) : null}
			{plan.risk_review ? <RiskReviewBlock review={plan.risk_review} /> : null}
			<ul className="trading-verdicts">
				{shown.map((verdict, index) => (
					<li key={`${verdict.code}-${verdict.symbol ?? ''}-${index}`} className={verdict.level}>
						<span className={`trading-verdict-level ${verdict.level}`}>{verdictLevelText[verdict.level] ?? verdict.level}</span>
						<code className="trading-verdict-code">{verdict.code}</code>
						<span className="trading-verdict-scope">{verdictScopeText[verdict.scope] ?? verdict.scope}</span>
						{verdict.symbol ? <strong className="trading-verdict-symbol">{verdict.symbol}</strong> : null}
						<span className="trading-verdict-message">{verdict.message}</span>
						{verdict.limit ? (
							<span className="trading-verdict-metric">
								{(verdict.metric ?? 0).toFixed(2)} / 上限 {verdict.limit.toFixed(2)}
							</span>
						) : null}
					</li>
				))}
			</ul>
			{sorted.length > shown.length ? (
				<button type="button" className="trading-link" onClick={() => setExpanded(true)}>
					展开全部 {sorted.length} 条
				</button>
			) : null}
		</div>
	);
}

// 交易工作台是整个页面最重的模块：任何一个面板的渲染异常都不该把
// 整页炸成白屏（2026-10-21 UniversePanel 读 null.length 造成过一次）。
// 内层是真正的实现，外层边界兜住一切渲染异常，只替换自己。
export function TradingWorkspace(props: Props) {
	return (
		<PanelErrorBoundary title="交易工作台">
			<TradingWorkspaceInner {...props} />
		</PanelErrorBoundary>
	);
}

function TradingWorkspaceInner({ config, onOpenSettings }: Props) {
	const [account, setAccount] = useState<TradeAccountView | null>(null);
	const [plans, setPlans] = useState<TradePlan[]>([]);
	const [active, setActive] = useState<TradePlan | null>(null);
	const [watchlist, setWatchlist] = useState('');
	const [profile, setProfile] = useState('balanced');
	const [horizon, setHorizon] = useState(horizonOptions[1]);
	const [notes, setNotes] = useState('');
	const [loading, setLoading] = useState(false);
	const [generating, setGenerating] = useState(false);
	const [executing, setExecuting] = useState(false);
	const [matching, setMatching] = useState(false);
	const [controlBusy, setControlBusy] = useState(false);
	const [reconciling, setReconciling] = useState(false);
	const [board, setBoard] = useState<TradeBoard | null>(null);
	const [boardLoading, setBoardLoading] = useState(false);
	const [error, setError] = useState('');
	const [message, setMessage] = useState('');
	const [autopilot, setAutopilot] = useState<TradeAutopilotStatus | null>(null);
	const [cycles, setCycles] = useState<TradeCycle[]>([]);
	const [autopilotBusy, setAutopilotBusy] = useState(false);
	// 复盘与已实现平仓：它们是"系统学到了什么"的唯一证据来源，必须与账户同屏。
	const [reviews, setReviews] = useState<TradeReview[]>([]);
	const [closedTrades, setClosedTrades] = useState<TradeClosedTrade[]>([]);
	const [performance, setPerformance] = useState<TradePerformance | null>(null);
	// 置信度校准：它回答"模型说 0.9 的时候是不是真的更准"。
	// 置信度已经在参与仓位定价，没有校准曲线，这个字段就只是装饰。
	const [calibration, setCalibration] = useState<TradeCalibration | null>(null);
	const [reviewBusy, setReviewBusy] = useState(false);
	// 防守线编辑中：{symbol, stop, takeProfit}。空表示当前没有在编辑。
	const [protection, setProtection] = useState<{ symbol: string; stop: string; takeProfit: string } | null>(null);
	const [cycleBusy, setCycleBusy] = useState(false);
	// 运维面：健康快照 + 事件流 + 通道声明。
	// 它们回答的是"系统现在到底是不是在正常工作"，与盈亏是两件事。
	const [health, setHealth] = useState<TradeHealth | null>(null);
	const [events, setEvents] = useState<TradeEvent[]>([]);
	const [channel, setChannel] = useState<TradeChannelInfo | null>(null);
	// 告警投递记录：事件说"发生了什么"，告警说"这件事有没有送到人手上"。
	// 无人值守时后者才是关键——出事但没通知，和没出事，从外面看是一样的。
	const [alerts, setAlerts] = useState<TradeAlertsResult | null>(null);
	const [alertTesting, setAlertTesting] = useState(false);
	// 时间线回放：按天把循环、计划、裁决、委托、事件摊平成一条线。
	const [timelineDay, setTimelineDay] = useState(() => new Date().toISOString().slice(0, 10));
	const [timeline, setTimeline] = useState<TradeTimeline | null>(null);
	const [timelineLoading, setTimelineLoading] = useState(false);
	// 情景记忆：每笔平仓当时的市场状态、买入理由与结果。
	// 与 Lesson 的聚合统计不同，它保留单笔情境，是模型"回忆"的原料。
	const [memories, setMemories] = useState<TradeMemory[]>([]);
	const [memoryBusy, setMemoryBusy] = useState(false);
	// 左侧导航视图：hash 形如 #trading/<view>，刷新页面后停留在原视图。
	const [activeView, setActiveView] = useState<TradingView>(() => resolveTradingView(window.location.hash));

	useEffect(() => {
		const onHashChange = () => {
			if (window.location.hash.startsWith('#trading')) setActiveView(resolveTradingView(window.location.hash));
		};
		window.addEventListener('hashchange', onHashChange);
		return () => window.removeEventListener('hashchange', onHashChange);
	}, []);

	const selectTradingView = (view: TradingView) => {
		setActiveView(view);
		window.history.replaceState(null, '', `#trading/${view}`);
	};

	const activeTradingModule = findTradingModule(activeView);
	const gateTone = account?.control.kill_switch ? 'halt' : account?.control.pause_buying ? 'pause' : 'normal';
	const refreshTradingData = () => {
		void loadAccount();
		void loadBoard();
		void loadPlans();
	};

	const loadAccount = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			try {
				const view = await fetchTradingAccount(config, signal);
				if (signal?.aborted) return;
				setAccount(view);
				// 不自动把持仓填进关注池：持仓本来就会被纳入行情范围，
				// 自动填充会让"留空即自动选股"永远失效。
			} catch (cause) {
				if (!signal?.aborted) setError(cause instanceof Error ? cause.message : '读取模拟盘账户失败');
			}
		},
		[config],
	);

	const loadPlans = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			try {
				const list = await listTradePlans(config, 10, signal);
				if (signal?.aborted) return;
				setPlans(list);
				setActive((current) => {
					if (current) {
						const fresh = list.find((item) => item.id === current.id);
						if (fresh) return fresh;
					}
					return list[0] ?? null;
				});
			} catch (cause) {
				if (!signal?.aborted) setError(cause instanceof Error ? cause.message : '读取交易计划失败');
			}
		},
		[config],
	);

	// 候选池与市场认知：与生成计划时用的是同一套计算，
	// 所以这里看到的就是"系统凭什么选了这些票"。
	const loadBoard = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			setBoardLoading(true);
			try {
				const data = await fetchTradingUniverse(config, signal);
				if (!signal?.aborted) setBoard(data);
			} catch {
				// 候选池是辅助视图，拉取失败不应打断主流程，也不弹错误。
			} finally {
				if (!signal?.aborted) setBoardLoading(false);
			}
		},
		[config],
	);

	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		setLoading(true);
		Promise.all([loadAccount(controller.signal), loadPlans(controller.signal)]).finally(() => {
			if (!controller.signal.aborted) setLoading(false);
		});
		return () => controller.abort();
	}, [config, loadAccount, loadPlans]);

	// 自动循环状态与循环日志：它们变化比计划快（一轮可能只有几秒），
	// 用固定间隔轮询而不是靠操作触发，否则界面会永远停在"已关闭"。
	const loadAutopilot = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			try {
				const [status, list] = await Promise.all([
					fetchTradingAutopilot(config, signal),
					listTradeCycles(config, 20, signal),
				]);
				if (signal?.aborted) return;
				setAutopilot(status);
				setCycles(list);
			} catch {
				// 自动循环是辅助视图，拉取失败不阻断主流程。
			}
		},
		[config],
	);

	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		void loadAutopilot(controller.signal);
		const timer = window.setInterval(() => void loadAutopilot(controller.signal), 20000);
		return () => {
			controller.abort();
			window.clearInterval(timer);
		};
	}, [config, loadAutopilot]);

	const loadOps = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			try {
				const [snapshot, log, info, alertView] = await Promise.all([
					fetchTradingHealth(config, signal),
					fetchTradingEvents(config, 30, signal),
					fetchTradingChannel(config, signal).catch(() => null),
					// 告警记录单独降级：它依赖一张新表，读失败不该让整个运维面变空。
					fetchTradingAlerts(config, 40, signal).catch(() => null),
				]);
				if (signal?.aborted) return;
				setHealth(snapshot);
				setEvents(log);
				setChannel(info);
				if (alertView) setAlerts(alertView);
			} catch {
				// 运维信息拉取失败不清空上一次结果：一次网络抖动不该让健康条变成"未知"，
				// 反而掩盖掉真实的状态。
			}
		},
		[config],
	);

	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		void loadOps(controller.signal);
		// 健康检查轮询比账户慢：它由后端定时采集，前端拉太勤只会浪费请求。
		const timer = window.setInterval(() => void loadOps(controller.signal), 30000);
		return () => {
			controller.abort();
			window.clearInterval(timer);
		};
	}, [config, loadOps]);

	// 时间线按需加载：它不是实时面板，只在人想看某一天时才拉。
	const loadTimeline = useCallback(
		async (day: string, signal?: AbortSignal) => {
			if (!config) return;
			setTimelineLoading(true);
			try {
				const view = await fetchTradingTimeline(config, day, signal);
				if (signal?.aborted) return;
				setTimeline(view);
			} catch {
				// 回放失败保留上一次结果：换成空白面板会让人误以为"那天什么都没发生"。
			} finally {
				if (!signal?.aborted) setTimelineLoading(false);
			}
		},
		[config],
	);

	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		void loadTimeline(timelineDay, controller.signal);
		return () => controller.abort();
	}, [config, timelineDay, loadTimeline]);

	const handleTestAlert = useCallback(async () => {
		if (!config) return;
		setAlertTesting(true);
		try {
			const result = await testTradingAlert(config);
			setMessage(`测试告警已送达：${result.delivered.join('、') || '无出口'}`);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '测试告警发送失败');
		} finally {
			setAlertTesting(false);
		}
	}, [config]);

	const loadReview = useCallback(
		async (signal?: AbortSignal) => {
			if (!config) return;
			try {
				const [list, trades, perf, memory, calibrationReport] = await Promise.all([
					fetchTradingReviews(config, 10),
					fetchClosedTrades(config, 30),
					// 业绩归因失败不该拖累复盘：两者读的是同一份账本，
					// 但业绩多一层聚合，单独降级即可。
					fetchTradingPerformance(config, 30, signal).catch(() => null),
					// 记忆同理：它是辅助证据，缺了不影响复盘结论。
					fetchTradingMemory(config, { limit: 8 }, signal).catch(() => []),
					fetchTradeCalibration(config, signal).catch(() => null),
				]);
				if (signal?.aborted) return;
				setReviews(list);
				setClosedTrades(trades);
				setPerformance(perf);
				setMemories(memory);
				setCalibration(calibrationReport);
			} catch {
				// 复盘是辅助视图，拉取失败不阻断主流程。
			}
		},
		[config],
	);

	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		void loadReview(controller.signal);
		return () => controller.abort();
	}, [config, loadReview]);

	// 候选池单独加载：它要拉全市场快照，比账户与计划慢得多，不该拖住首屏。
	useEffect(() => {
		if (!config) return;
		const controller = new AbortController();
		void loadBoard(controller.signal);
		return () => controller.abort();
	}, [config, loadBoard]);

	// 生成中的计划需要轮询，直到进入待确认或失败状态。
	useEffect(() => {
		if (!config || !active || active.status !== 'running') return;
		const controller = new AbortController();
		const timer = window.setInterval(() => {
			fetchTradePlan(config, active.id, controller.signal)
				.then((plan) => {
					if (controller.signal.aborted) return;
					setActive(plan);
					if (plan.status !== 'running') {
						setGenerating(false);
						void loadPlans();
						void loadAccount();
					}
				})
				.catch(() => {
					/* 轮询失败保留上一次结果，下一轮继续 */
				});
		}, 2500);
		return () => {
			controller.abort();
			window.clearInterval(timer);
		};
	}, [config, active, loadAccount, loadPlans]);

	const handleGenerate = async () => {
		if (!config) return;
		// 留空即自动选股：这是"交易员"与"执行器"的分界，不该强制用户先想好买什么。
		const symbols = parseWatchlist(watchlist);
		setError('');
		setMessage('');
		setGenerating(true);
		try {
			const plan = await createTradePlan(config, {
				watchlist: symbols,
				auto_universe: symbols.length === 0,
				trader_profile: profile,
				horizon,
				notes: notes.trim(),
			});
			setActive(plan);
			await loadPlans();
		} catch (cause) {
			setGenerating(false);
			setError(cause instanceof Error ? cause.message : '生成交易计划失败');
		}
	};

	// 保存防守线：只写线，不代替用户判断"这条线合不合理"。
	// 设得比现价高，下一轮防守线会立刻触发——那是用户的决定，界面如实执行。
	const handleSaveProtection = async () => {
		if (!config || !protection) return;
		const stop = Number(protection.stop);
		const takeProfit = Number(protection.takeProfit);
		if (!Number.isFinite(stop) || !Number.isFinite(takeProfit) || stop < 0 || takeProfit < 0) {
			setError('止损价与止盈价必须是非负数字（填 0 表示不设）');
			return;
		}
		try {
			await setPositionProtection(config, protection.symbol, stop, takeProfit);
			setProtection(null);
			setMessage(`${protection.symbol} 防守线已更新`);
			await loadAccount();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '保存防守线失败');
		}
	};

	// 手动跑一次防守线：不调模型，几秒返回，随时可以点。
	const handleRunProtective = async () => {
		if (!config) return;
		setReviewBusy(true);
		setError('');
		try {
			const run = await runTradingProtective(config);
			if (run.triggered === 0) {
				setMessage(`防守线已检查 ${run.checked} 只持仓，未触发${run.notes?.length ? `：${run.notes.join('；')}` : ''}`);
			} else {
				setMessage(`防守线触发 ${run.triggered} 笔，成交 ${run.filled} 笔`);
			}
			await Promise.all([loadAccount(), loadReview(), loadPlans()]);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '防守线执行失败');
		} finally {
			setReviewBusy(false);
		}
	};

	const handleRunReview = async () => {
		if (!config) return;
		setReviewBusy(true);
		setError('');
		try {
			const review = await runTradingReview(config);
			setMessage(`已生成 ${review.trading_day} 复盘：平仓 ${review.facts.closed_trades} 笔，已实现 ${formatMoney(review.facts.realized_pnl)}`);
			await loadReview();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '生成复盘失败');
		} finally {
			setReviewBusy(false);
		}
	};

	// 补齐历史记忆。日常由每日复盘自动同步，这里是给"刚接上这套系统、
	// 账本里已有历史平仓"的场景手动补一次用的。
	const handleSyncMemory = async () => {
		if (!config) return;
		setMemoryBusy(true);
		setError('');
		try {
			const result = await syncTradingMemory(config);
			setMessage(result.added > 0 ? `已补齐 ${result.added} 条情景记忆，共 ${result.total} 条` : `没有需要补齐的记忆，当前共 ${result.total} 条`);
			await loadReview();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '补齐记忆失败');
		} finally {
			setMemoryBusy(false);
		}
	};

	const handleExecute = async () => {
		if (!config || !active) return;
		const actionable = (active.orders ?? []).filter((order) => order.status === 'pending');
		if (actionable.length === 0) {
			setError('当前计划没有可执行订单');
			return;
		}
		const summary = actionable
			.map((order) => `${order.side === 'buy' ? '买入' : '卖出'} ${order.name || order.symbol} ${order.quantity} 股`)
			.join('\n');
		if (!window.confirm(`确认按以下订单执行模拟盘交易？\n\n${summary}\n\n模拟盘不会触达券商账户。`)) return;
		setExecuting(true);
		setError('');
		try {
			const plan = await executeTradePlan(config, active.id);
			setActive(plan);
			setMessage('订单已送入模拟撮合');
			await Promise.all([loadAccount(), loadPlans()]);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '执行计划失败');
		} finally {
			setExecuting(false);
		}
	};

	const handleReject = async () => {
		if (!config || !active) return;
		if (!window.confirm('放弃这份计划？挂起订单会被撤销。')) return;
		try {
			const plan = await rejectTradePlan(config, active.id);
			setActive(plan);
			await loadPlans();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '放弃计划失败');
		}
	};

	const handleMatch = async () => {
		if (!config) return;
		setMatching(true);
		setError('');
		try {
			const orders = await matchPendingOrders(config);
			const filled = orders.filter((order) => order.status === 'filled' || order.status === 'partially_filled').length;
			setMessage(`撮合完成：${filled} 笔成交，${orders.length - filled} 笔仍挂起或被拒绝`);
			await Promise.all([loadAccount(), loadPlans()]);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '撮合失败');
		} finally {
			setMatching(false);
		}
	};

	const handleReset = async () => {
		if (!config) return;
		const input = window.prompt('重置模拟盘会清空持仓、订单与计划。请输入初始资金（元）：', '100000');
		if (input === null) return;
		const cash = Number(input);
		if (!Number.isFinite(cash) || cash < 10000) {
			setError('初始资金至少 10000 元');
			return;
		}
		try {
			const view = await resetTradingAccount(config, cash);
			setAccount(view);
			setActive(null);
			await loadPlans();
			setMessage('模拟盘已重置');
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '重置模拟盘失败');
		}
	};

	// 门控切换：kill_switch 是总闸，需要二次确认；它打开时后端会强制暂停买入。
	const handleControl = async (patch: { pause_buying?: boolean; kill_switch?: boolean }) => {
		if (!config || !account) return;
		const current = account.control;
		const next = {
			pause_buying: patch.pause_buying ?? current.pause_buying,
			kill_switch: patch.kill_switch ?? current.kill_switch,
		};
		if (next.kill_switch && !current.kill_switch) {
			if (!window.confirm('「全部停止」会阻止新计划生成，并让所有挂单无法成交（已挂单保留、可撤销）。确认开启？')) return;
		}
		setControlBusy(true);
		setError('');
		try {
			const saved = await setTradingControl(config, { ...next, reason: current.reason || '' });
			setAccount((previous) => (previous ? { ...previous, control: saved } : previous));
			setMessage(`账户门控已切换为「${controlGateText(saved)}」`);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '切换门控失败');
		} finally {
			setControlBusy(false);
		}
	};

	// 对账：冻结额是一笔看不见的账，漂移不会自己报错，只能主动查。
	// 默认只查不改——自动改账在排查阶段会把问题掩盖掉。
	const handleReconcile = async () => {
		if (!config) return;
		setReconciling(true);
		setError('');
		try {
			let report = await reconcileTradingAccount(config, false);
			if (!report.ok) {
				const detail = (report.issues || []).map((issue) => `· ${issue.message}`).join('\n');
				if (window.confirm(`对账发现不一致：\n\n${detail || '冻结额与挂单不匹配'}\n\n是否按挂单重算并修正账本？`)) {
					report = await reconcileTradingAccount(config, true);
				}
			}
			const head = report.ok
				? `对账通过：${report.open_buy_orders} 笔买入挂单，应收冻结 ¥${formatMoney(report.expected_reserved)}`
				: `对账未通过：冻结差额 ¥${formatMoney(report.drift)}（账本 ${formatMoney(report.reserved_cash)} / 应为 ${formatMoney(report.expected_reserved)}）`;
			setMessage(report.repaired ? `${head}（已按挂单重算修正）` : head);
			await loadAccount();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '对账失败');
		} finally {
			setReconciling(false);
		}
	};

	// 自动循环开关：打开即意味着"系统会自己发起计划"，
	// 所以必须二次确认，而不是点一下就生效。
	const handleAutopilot = async (patch: Parameters<typeof configureTradingAutopilot>[1]) => {
		if (!config) return;
		if (patch.enabled === true && !autopilot?.config.enabled) {
			const executeHint = patch.auto_execute ?? autopilot?.config.auto_execute;
			const mode = executeHint ? '生成后会立即送入模拟撮合' : '生成后停在待确认，需要你点执行';
			const cadence = autopilot ? currentIntervalSeconds(autopilot.config) : 900;
			const daily = autopilot ? scheduleCycles(autopilot.config.schedule) : 0;
			const plan = daily > 0 ? `全天约 ${daily} 轮（开盘与尾盘密、盘中稀）` : `每 ${intervalText(cadence)} 一轮`;
			if (!window.confirm(`开启自动交易循环？\n\n${plan}，${mode}。\n卖出与止损由代码每分钟检查，不占用模型调用。\n当日亏损超过阈值会自动熔断并停止开新仓。`)) return;
		}
		setAutopilotBusy(true);
		setError('');
		try {
			const status = await configureTradingAutopilot(config, patch);
			setAutopilot(status);
			setMessage(`自动循环${status.config.enabled ? '已开启' : '已关闭'}${status.config.auto_execute ? '（自动执行）' : '（需人工确认）'}`);
			await loadAutopilot();
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '修改自动循环失败');
		} finally {
			setAutopilotBusy(false);
		}
	};

	// 人工触发一轮：绕过开关与交易时段，但不绕过总闸与熔断。
	const handleRunCycle = async () => {
		if (!config) return;
		setCycleBusy(true);
		setError('');
		setMessage('');
		try {
			const cycle = await runTradingCycle(config);
			setMessage(
				`本轮${tradeCycleStatusText[cycle.status] ?? cycle.status}` +
					(cycle.reason ? `：${cycle.reason}` : `，建议 ${cycle.orders} 笔、成交 ${cycle.filled} 笔`),
			);
			await Promise.all([loadAutopilot(), loadPlans(), loadAccount()]);
			if (cycle.plan_id) {
				try {
					setActive(await fetchTradePlan(config, cycle.plan_id));
				} catch {
					/* 计划可能仍在生成中，忽略 */
				}
			}
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '执行一轮失败');
		} finally {
			setCycleBusy(false);
		}
	};

	// 解除熔断是唯一一处"人推翻机器保护"的入口，必须留下理由。
	const handleResetBreaker = async () => {
		if (!config) return;
		const reason = window.prompt('解除当日熔断的原因（会写入当日状态与日志）：', '');
		if (!reason || !reason.trim()) return;
		setAutopilotBusy(true);
		try {
			await resetTradingBreaker(config, reason.trim());
			setMessage('当日熔断已解除');
			await Promise.all([loadAutopilot(), loadAccount()]);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '解除熔断失败');
		} finally {
			setAutopilotBusy(false);
		}
	};

	// 回撤熔断与当日熔断是两条不同的防线，解除也必须是两个入口：
	// 合并成一个按钮，事件流里就分不清当时按下的是哪一条。
	const handleReleaseDrawdown = async () => {
		if (!config) return;
		const reason = window.prompt('解除回撤熔断的原因（解除后高点会重置为当前净值）：', '');
		if (!reason || !reason.trim()) return;
		setAutopilotBusy(true);
		try {
			await releaseTradingDrawdown(config, reason.trim());
			setMessage('回撤熔断已解除，净值高点已重置为当前值');
			await Promise.all([loadAutopilot(), loadAccount(), loadOps()]);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '解除回撤熔断失败');
		} finally {
			setAutopilotBusy(false);
		}
	};

	const openPlanById = async (planId: string) => {
		if (!config) return;
		// 计划详情只存在于「选股与计划」页：从日志或历史点开一条计划，
		// 必须同时切到那一页，否则点了没有任何可见反应。
		selectTradingView('planning');
		try {
			setActive(await fetchTradePlan(config, planId));
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : '读取计划失败');
		}
	};

	const unrealized = useMemo(() => {
		if (!account) return 0;
		return account.positions.reduce((total, position) => total + position.unrealized_pnl, 0);
	}, [account]);
	const returnPercent = useMemo(() => {
		if (!account || account.account.initial_cash <= 0) return 0;
		return ((account.account.equity - account.account.initial_cash) / account.account.initial_cash) * 100;
	}, [account]);

	// 后端在 running 阶段可能还没有订单，统一兜底成空数组，避免 .length 崩掉整页。
	const activeOrders = active?.orders ?? [];
	const actionableOrders = activeOrders.filter((order) => order.status === 'pending');
	const rejectedOrders = activeOrders.filter((order) => order.status === 'rejected');
	const pendingOrders = activeOrders.filter((order) => order.status === 'pending');

	// 左侧导航的实时标记：不点进去也知道哪里有东西等着看。
	const navBadges: Partial<Record<TradingView, string>> = {
		positions: account?.positions?.length ? String(account.positions.length) : '',
		planning: active?.status === 'draft' && actionableOrders.length > 0 ? `待确认 ${actionableOrders.length}` : '',
		autopilot: autopilot ? (autopilot.running ? '运行中' : '已停止') : '',
		ops: health && health.status !== 'healthy' ? '异常' : '',
	};

	// 「需要你处理」：把散落在各面板里、真正需要人决策的状态收拢到一处。
	// 不熟悉模块的人不应该靠逐个点开才能发现熔断、账本不平或告警没配。
	const attention: { tone: 'info' | 'warn' | 'danger'; text: string; view: TradingView; action: string }[] = [];
	if (autopilot?.today.breaker || autopilot?.breakers.some((item) => item.tripped)) {
		attention.push({ tone: 'danger', text: '风控熔断已触发，本轮不会开新仓', view: 'autopilot', action: '看运行控制' });
	}
	if (autopilot?.risk?.level === 'drawdown.stop') {
		attention.push({ tone: 'danger', text: `回撤 ${autopilot.risk.drawdown_percent.toFixed(2)}% 已到停止档`, view: 'autopilot', action: '释放回撤' });
	} else if (autopilot?.risk?.level === 'drawdown.warn') {
		attention.push({ tone: 'warn', text: `回撤 ${autopilot.risk.drawdown_percent.toFixed(2)}% 进入警戒档`, view: 'autopilot', action: '看运行控制' });
	}
	if (active?.status === 'draft' && actionableOrders.length > 0) {
		attention.push({ tone: 'info', text: `有 1 份待确认计划，${actionableOrders.length} 笔可执行`, view: 'planning', action: '去确认' });
	}
	if (pendingOrders.length > 0) {
		attention.push({ tone: 'info', text: `${pendingOrders.length} 笔挂单等待限价触及`, view: 'positions', action: '看挂单' });
	}
	if (account?.account.pnl && !account.account.pnl.aligned) {
		attention.push({ tone: 'warn', text: `盈亏口径未对齐，残差 ${signedMoney(account.account.pnl.residual)}`, view: 'overview', action: '看口径' });
	}
	if (health && !health.ledger.ok) {
		attention.push({ tone: 'danger', text: '账本不平：资金或冻结数量存在漂移', view: 'ops', action: '去对账' });
	}
	if (health?.alerts && !health.alerts.configured) {
		attention.push({ tone: 'warn', text: '未配置告警通道，出事没有人能收到通知', view: 'ops', action: '去配置' });
	}
	if (health?.alerts && health.alerts.failed_24h > 0) {
		attention.push({ tone: 'warn', text: `24 小时内 ${health.alerts.failed_24h} 条告警投递失败`, view: 'ops', action: '查通道' });
	}
	if (autopilot && !autopilot.running) {
		attention.push({ tone: 'info', text: '自动运行未开启，只能手动生成并确认计划', view: 'autopilot', action: '去开启' });
	}

	if (!config) {
		return <section className="trading-workspace"><p className="trading-empty">等待后端连接…</p></section>;
	}

	return (
		<section className="trading-workspace">
			<aside className="trading-nav" aria-label="AI交易员功能">
				<header><TrendingUp size={18} /><div><span>TRADING AGENT</span><strong>AI交易员</strong></div></header>
				{tradingNavGroups.map((group) => (
					<section key={group.id}>
						<h2>{group.name}</h2>
						<div>{group.modules.map((module) => {
							const Icon = tradingModuleIcons[module.id];
							const badge = navBadges[module.id];
							return (
								<button type="button" key={module.id} className={activeView === module.id ? 'active' : ''} onClick={() => selectTradingView(module.id)}>
									<Icon size={16} />
									<span><strong>{module.name}</strong><small>{module.description}</small></span>
									{badge ? <em>{badge}</em> : null}
								</button>
							);
						})}</div>
					</section>
				))}
				<footer className="trading-nav-foot">
					<i className={`trading-gate-dot ${gateTone}`} aria-hidden="true" />
					<span>{controlGateText(account?.control)}</span>
				</footer>
			</aside>

			<div className="trading-main">
				<header className="trading-hero">
					<div>
						<span>{findTradingGroupName(activeView)}</span>
						<h2>
							{activeTradingModule.name}
							{/* 模拟盘身份从每页一条大横幅降级为标题旁的徽标：身份信息一眼可见，
							    但不再在每个视图吃掉一行高度。完整说明放悬浮提示。 */}
							<span
								className="trading-paper-chip"
								title="这是模拟盘，不接券商、不会产生真实委托。模型只负责给出方向与权重，股数、费用、涨跌停与风控校验全部由本机代码计算；所有计划需你确认后才会进入模拟撮合。"
							>
								<FlaskConical size={12} /> 模拟盘
							</span>
						</h2>
						<p>{activeTradingModule.description}</p>
					</div>
					<div className="trading-hero-side">
						<button type="button" className="trading-ghost" onClick={refreshTradingData} disabled={loading || boardLoading}>
							{loading || boardLoading ? <LoaderCircle size={14} className="spin" /> : <RefreshCw size={14} />} 刷新数据
						</button>
					</div>
				</header>

				{(error || message) && (
					<div className={`trading-notice ${error ? 'error' : 'ok'}`}>
						{error ? <AlertTriangle size={16} /> : <CheckCircle2 size={16} />}
						<span>{error || message}</span>
						<button type="button" onClick={() => { setError(''); setMessage(''); }}>关闭</button>
					</div>
				)}

				{activeView === 'overview' && (
					<>
						{/* 四块状态条：自动运行、市场环境、健康、门控。
						    它们回答「系统现在在不在工作」，比盈亏更该被先看到。 */}
						<div className="trading-status-strip">
							<button type="button" className={`trading-status-tile ${autopilot?.running ? 'ok' : 'idle'}`} onClick={() => selectTradingView('autopilot')}>
								<i><CalendarClock size={17} /></i>
								<span className="trading-tile-body">
									<span>自动运行</span>
									<strong>{autopilot ? (autopilot.running ? '运行中' : '已停止') : '—'}</strong>
									<em>
										{autopilot
											? autopilot.running
												? autopilot.next_run_at
													? `下一轮 ${clockText(autopilot.next_run_at)}`
													: `今日已跑 ${autopilot.cycles_today} 轮`
												: '点此开启自动运行'
											: '等待读取'}
									</em>
								</span>
							</button>
							<button type="button" className={`trading-status-tile ${board?.market?.evaluated ? 'ok' : 'idle'}`} onClick={() => selectTradingView('market')}>
								<i><Activity size={17} /></i>
								<span className="trading-tile-body">
									<span>市场环境</span>
									<strong>{board?.market?.evaluated ? board.market.label : '未评估'}</strong>
									<em>{board?.market?.evaluated ? `允许仓位上限 ${board.market.risk_budget_percent}%` : '点此刷新市场判定'}</em>
								</span>
							</button>
							<button type="button" className={`trading-status-tile ${health ? (health.status === 'healthy' ? 'ok' : health.status === 'warning' ? 'warn' : 'bad') : 'idle'}`} onClick={() => selectTradingView('ops')}>
								<i><ShieldCheck size={17} /></i>
								<span className="trading-tile-body">
									<span>健康检查</span>
									<strong>{health ? healthLabel(health.status) : '—'}</strong>
									<em>{health ? `${health.positions} 只持仓 · ${health.open_orders} 笔挂单` : '等待读取'}</em>
								</span>
							</button>
							{/* 门控格不是跳转入口而是操作入口：状态和总闸放在同一格，
							    避免页面上出现两处写着「正常运行」的地方。 */}
							<div
								className={`trading-status-tile gate ${gateTone}`}
								title={account?.control.kill_switch
									? '不生成新计划，任何挂单都不会成交；已有挂单保留，可撤销。'
									: account?.control.pause_buying
										? '只停新增买入，卖出与减仓照常执行。'
										: '门控放行。全自动执行上线后，这里是唯一的人工总闸。'}
							>
								<i><PowerCircle size={17} /></i>
								<span className="trading-tile-body">
									<span>交易门控</span>
									<strong>{controlGateText(account?.control)}</strong>
									<div className="trading-gate-actions">
										<button
											type="button"
											className="trading-ghost"
											onClick={() => void handleControl({ pause_buying: !account?.control.pause_buying })}
											disabled={controlBusy || !account || account.control.kill_switch}
										>
											{account?.control.pause_buying && !account?.control.kill_switch ? '恢复买入' : '暂停买入'}
										</button>
										<button
											type="button"
											className={`trading-ghost ${account?.control.kill_switch ? 'danger active' : 'danger'}`}
											onClick={() => void handleControl({ kill_switch: !account?.control.kill_switch })}
											disabled={controlBusy || !account}
										>
											{account?.control.kill_switch ? '解除停止' : '全部停止'}
										</button>
										<button type="button" className="trading-link" onClick={() => void handleReconcile()} disabled={reconciling || !account}>
											{reconciling ? '对账中…' : '对账'}
										</button>
									</div>
								</span>
							</div>
						</div>

						<div className="trading-summary">
							<article>
								<i><Wallet size={16} /></i>
								<div>
									<span>总资产</span>
									<strong>{account ? `¥ ${formatMoney(account.account.equity)}` : '—'}</strong>
									<em className={returnPercent >= 0 ? 'up' : 'down'}>
										{account ? `${returnPercent >= 0 ? '+' : ''}${returnPercent.toFixed(2)}%` : '—'}
									</em>
								</div>
							</article>
							<article>
								<i><Banknote size={16} /></i>
								<div>
									<span>可用资金</span>
									<strong>{account ? `¥ ${formatMoney(availableCash(account.account))}` : '—'}</strong>
									<em>
										{account && account.account.reserved_cash > 0
											? `挂单冻结 ¥ ${formatMoney(account.account.reserved_cash)}`
											: `初始 ${account ? `¥ ${formatMoney(account.account.initial_cash)}` : '—'}`}
									</em>
								</div>
							</article>
							<article>
								<i><PieChart size={16} /></i>
								<div>
									<span>持仓市值</span>
									<strong>{account ? `¥ ${formatMoney(account.account.market_value)}` : '—'}</strong>
									<em>{account?.positions?.length ?? 0} 只标的</em>
								</div>
							</article>
							<article>
								<i><TrendingUp size={16} /></i>
								<div>
									<span>浮动盈亏</span>
									<strong className={unrealized >= 0 ? 'up' : 'down'}>{`${unrealized >= 0 ? '+' : ''}${formatMoney(unrealized)}`}</strong>
									<em>
										{account?.account.pnl
											? `已实现 ${account.account.pnl.realized >= 0 ? '+' : ''}${formatMoney(account.account.pnl.realized)}`
											: `账户：${account?.broker === 'paper' ? '模拟撮合' : account?.broker || '—'}`}
									</em>
								</div>
							</article>
						</div>

						{/* 待办放在资产之后：先知道自己有多少钱，再决定要不要处理这些事。 */}
						<section className="trading-attention">
							<header>
								<ShieldAlert size={16} />
								<h2>需要你处理</h2>
								<span className="trading-subtle">{attention.length ? `${attention.length} 项待办` : '暂无待办'}</span>
							</header>
							{attention.length === 0 ? (
								<p className="trading-subtle">当前没有需要你处理的事项：风控、账本与告警都在正常范围内。</p>
							) : (
								<ul>
									{attention.slice(0, 6).map((item) => (
										<li key={item.text} className={item.tone}>
											<i aria-hidden="true" />
											<span>{item.text}</span>
											<button type="button" className="trading-link" onClick={() => selectTradingView(item.view)}>{item.action}</button>
										</li>
									))}
								</ul>
							)}
						</section>

						{/* 两个数字口径不同，必须有一行把中间项摆出来。
						    少了它，"总资产 -630"和"浮动 +672"看上去就是矛盾的。 */}
						{account?.account.pnl && (
							<div className={`trading-pnl-check ${account.account.pnl.aligned ? 'ok' : 'warn'}`}>
								<span>
									总资产变动 {signedMoney(account.account.pnl.change)} = 已实现 {signedMoney(account.account.pnl.realized)}
									{' + '}
									浮动 {signedMoney(account.account.pnl.unrealized)}
									{account.account.pnl.aligned ? '' : ` + 残差 ${signedMoney(account.account.pnl.residual)}`}
								</span>
								<em>累计费用 {formatMoney(account.account.pnl.fees)}（已含在上面两项里，不再单独扣一次）</em>
								<b>
									{account.account.pnl.aligned
										? '对账通过'
										: `差额 ${signedMoney(account.account.pnl.residual)}（此类差额来自早期建仓成本未计买入费，该口径已修复）`}
								</b>
							</div>
						)}
					</>
				)}

				{activeView === 'market' && (board?.market?.evaluated ? (
					<MarketPanel market={board.market} onRefresh={() => void loadBoard()} busy={boardLoading} />
				) : (
					<p className="trading-empty">市场环境尚未加载，点击右上角「刷新数据」。</p>
				))}

				{activeView === 'planning' && (
					<>
						{/* 四步流程写在页面顶上：第一次用这个模块的人不必猜「先生成还是先选股」。 */}
						<div className="trading-steps">
							{([
								['选股范围', watchlist.trim() ? '使用你指定的关注池' : '留空则由系统按位置规则从全市场选'],
								['生成计划', '模型只给方向与目标权重'],
								['确认执行', '股数、费用、涨跌停由本机计算'],
								['撮合挂单', '限价未触及则挂起，可随时撤销'],
							] as const).map(([name, detail], index) => (
								<div key={name}><i>{index + 1}</i><span><strong>{name}</strong><small>{detail}</small></span></div>
							))}
						</div>
						{/* 候选池是「选股」这一步的结果：它必须在生成计划之前被看到，
						    否则用户不知道模型是在什么范围里选的。 */}
						<UniversePanel board={board} loading={boardLoading} onRefresh={() => void loadBoard()} />
						<div className="trading-grid">
						<aside className="trading-panel">
							<header>
								<Sparkles size={17} />
								<h2>生成交易计划</h2>
								<button type="button" className="trading-ghost" onClick={() => void loadAccount()} disabled={loading}>
									<RefreshCw size={14} /> 刷新账户
								</button>
							</header>
							<label>
								<span>关注池（可选，留空则由系统从全市场自动选股）</span>
								<textarea
									rows={3}
									value={watchlist}
									onChange={(event) => setWatchlist(event.target.value)}
									placeholder="留空 = 自动选股；也可以填 600000.SH, 000001.SZ 指定标的"
								/>
							</label>
							<div className="trading-form-row">
								<label>
									<span>交易者画像</span>
									<select value={profile} onChange={(event) => setProfile(event.target.value)}>
										{profileOptions.map((option) => (
											<option key={option.value} value={option.value}>{option.label}</option>
										))}
									</select>
								</label>
								<label>
									<span>计划周期</span>
									<select value={horizon} onChange={(event) => setHorizon(event.target.value)}>
										{horizonOptions.map((option) => (
											<option key={option} value={option}>{option}</option>
										))}
									</select>
								</label>
							</div>
							<label>
								<span>补充材料（可选，会作为事实交给模型）</span>
								<textarea
									rows={3}
									value={notes}
									onChange={(event) => setNotes(event.target.value)}
									placeholder="例如：今日情绪偏弱，只做低吸；某标的明日有解禁。"
								/>
							</label>
							<button type="button" className="trading-primary" onClick={() => void handleGenerate()} disabled={generating}>
								{generating ? <LoaderCircle size={16} className="spin" /> : <Sparkles size={16} />}
								{generating ? '模型生成中…' : '生成计划'}
							</button>
							<p className="trading-hint">
								模型只输出方向与目标权重；股数按账户总资产与限价换算，超出风控阈值的建议会被逐条拒绝并给出原因。
							</p>
							<button type="button" className="trading-link" onClick={() => void handleReset()}>重置模拟盘资金</button>
						</aside>

						<main className="trading-panel trading-plan">
							<header>
								<Wallet size={17} />
								<h2>计划详情</h2>
								{active?.snapshot.auto_universe ? (
									<span className="trading-tag">
										<Target size={12} /> 系统选股
									</span>
								) : null}
								{active && <span className={`trading-status ${active.status}`}>{tradePlanStatusText[active.status]}</span>}
								{active && <button type="button" className="trading-ghost" onClick={() => void loadPlans()}><RefreshCw size={14} /> 刷新</button>}
							</header>
							{!active ? (
								<p className="trading-empty">还没有计划。填写关注池后生成第一份模拟盘计划。</p>
							) : (
								<>
									<div className="trading-plan-head">
										<div>
											<strong>{active.regime || '未标注市场状态'}</strong>
											<span>{new Date(active.created_at).toLocaleString('zh-CN')} · {active.trader_profile || '未指定画像'} · {active.horizon || '未指定周期'}</span>
										</div>
										<div className="trading-plan-actions">
											<button type="button" className="trading-primary" onClick={() => void handleExecute()} disabled={executing || active.status !== 'draft' || actionableOrders.length === 0}>
												{executing ? <LoaderCircle size={15} className="spin" /> : <Play size={15} />}
												确认执行（{actionableOrders.length}）
											</button>
											<button type="button" className="trading-ghost danger" onClick={() => void handleReject()} disabled={active.status !== 'draft'}>
												<XCircle size={15} /> 放弃
											</button>
											<button type="button" className="trading-ghost" onClick={() => void handleMatch()} disabled={matching}>
												{matching ? <LoaderCircle size={14} className="spin" /> : <RefreshCw size={14} />} 撮合挂单
											</button>
										</div>
									</div>
									<p className="trading-plan-summary">{active.summary || active.error || '—'}</p>
									<ResearchPanel research={active.research} />
									<DecisionPanel plan={active} />
									<CooldownBar view={active.snapshot.cooldowns} />
									<SandboxPanel config={config} plan={active} />
									{active.risk_notes && active.risk_notes.length > 0 && (
										<ul className="trading-notes">
											{active.risk_notes.map((note) => <li key={note}>{note}</li>)}
										</ul>
									)}
									<div className="trading-table-wrap">
										<table className="trading-table">
											<thead>
												<tr>
													<th>标的</th><th>方向</th><th>数量</th><th>限价</th><th>止损</th><th>目标权重</th><th>状态</th><th>依据 / 拒绝原因</th>
												</tr>
											</thead>
											<tbody>
												{activeOrders.length === 0 && (
													<tr><td colSpan={8} className="trading-empty-cell">本次没有可执行建议</td></tr>
												)}
												{activeOrders.map((order) => (
													<tr key={order.id} className={order.status}>
														<td><strong>{order.symbol}</strong><span>{order.name}</span></td>
														<td className={order.side === 'buy' ? 'up' : 'down'}>{order.side === 'buy' ? '买入' : '卖出'}</td>
														<td>{order.quantity} 股</td>
														<td>{order.order_type === 'limit' ? order.limit_price.toFixed(2) : '市价'}</td>
														<td>{order.stop_loss_price ? order.stop_loss_price.toFixed(2) : '—'}</td>
														<td>{order.target_weight_percent ? `${order.target_weight_percent.toFixed(1)}%` : '—'}</td>
														<td><span className={`trading-order-status ${order.status}`}>{tradeOrderStatusText[order.status]}</span></td>
														<td className="trading-cell-note">{order.status === 'rejected' ? order.reject_reason : order.thesis || '—'}</td>
													</tr>
												))}
											</tbody>
										</table>
									</div>
									{rejectedOrders.length > 0 && (
										<p className="trading-hint warn">
											{rejectedOrders.length} 笔建议被风控拒绝，说明模型给出的方向未通过仓位、止损或标的白名单校验，不会进入撮合。
										</p>
									)}
								</>
							)}
						</main>
						</div>

						{/* 历史计划放在同页底部而不是独立入口：点一条记录，详情立刻在上方「计划详情」
						    里可见，不需要在两个页面之间来回跳。 */}
						{plans.length > 0 && (
							<section className="trading-panel">
								<header><History size={17} /><h2>历史计划</h2><span className="trading-subtle">最近 10 份 · 点击在上方查看详情</span></header>
								<ul className="trading-history">
									{plans.map((plan) => (
										<li key={plan.id}>
											<button type="button" className={active?.id === plan.id ? 'active' : ''} onClick={() => void openPlanById(plan.id)}>
												<strong>{plan.regime || '未标注'}</strong>
												<span>{new Date(plan.created_at).toLocaleString('zh-CN')}</span>
												<em className={`trading-status ${plan.status}`}>{tradePlanStatusText[plan.status]}</em>
												<small>{(plan.orders ?? []).length} 笔 · {plan.summary?.slice(0, 40) || plan.error || '—'}</small>
											</button>
										</li>
									))}
								</ul>
							</section>
						)}
					</>
				)}

				{activeView === 'positions' && (
					<>
						<section className="trading-panel">
							<header>
								<Wallet size={17} />
								<h2>模拟持仓</h2>
								<span className="trading-subtle">现价刷新自最近一次行情请求</span>
							</header>
							<div className="trading-table-wrap">
								<table className="trading-table">
									<thead>
										<tr><th>标的</th><th>数量</th><th>可卖 / 冻结</th><th>成本</th><th>现价</th><th>浮动盈亏</th><th>防守线</th><th>操作</th></tr>
									</thead>
									<tbody>
										{(!account || (account.positions ?? []).length === 0) && (
											<tr><td colSpan={8} className="trading-empty-cell">当前空仓</td></tr>
										)}
										{account?.positions.map((position) => {
											const sellable = position.available_quantity - (position.reserved_quantity ?? 0);
											const profitPercent = position.average_cost > 0 ? ((position.last_price / position.average_cost - 1) * 100) : 0;
											const editing = protection?.symbol === position.symbol;
											return (
												<Fragment key={position.symbol}>
													<tr>
														<td><strong>{position.symbol}</strong><span>{position.name}{position.sector ? ` · ${position.sector}` : ''}</span></td>
														<td>{position.quantity} 股</td>
														<td>
															{sellable} 股
															{(position.reserved_quantity ?? 0) > 0 && <span className="trading-subtle">（挂单 {position.reserved_quantity}）</span>}
														</td>
														<td>{position.average_cost.toFixed(3)}</td>
														<td>{position.last_price.toFixed(2)}</td>
														<td className={`trading-pnl-cell ${profitPercent >= 0 ? 'up' : 'down'}`}>
															<strong>{position.unrealized_pnl >= 0 ? '+' : ''}{formatMoney(position.unrealized_pnl)}</strong>
															<span>{profitPercent >= 0 ? '+' : ''}{profitPercent.toFixed(2)}%</span>
														</td>
														<td>
															<div className="trading-defense">
																{position.stop_loss_price ? <span className="trading-line stop" title="止损">止损 {position.stop_loss_price.toFixed(2)}</span> : <span className="trading-line none">未设止损</span>}
																{position.take_profit_price ? <span className="trading-line take" title="止盈">止盈 {position.take_profit_price.toFixed(2)}</span> : null}
															</div>
															{/* 失效条件与预期期限写在持仓上：它们是「这笔单什么时候算错」的承诺，
															    比止损更容易被遗忘，因为它不是一个价格。 */}
															{/* 入场置信度：复盘时回答「当时多有把握」，它比买入理由更客观——理由是叙述，置信度是当时的数字。 */}
															{/* 复核轮次：证明这笔仓位不是买入后没人再看 */}
															<div className="trading-defense-meta">
																{(position.reduce_count ?? 0) > 0 && <span>已止盈 {position.reduce_count} 级</span>}
																{(position.invalidation_rule?.price_below || position.invalidation) && (
																	<span title={position.invalidation ?? ''}>
																		失效 {position.invalidation_rule?.price_below ? `跌破 ${position.invalidation_rule.price_below.toFixed(2)}` : (position.invalidation ?? '').slice(0, 18)}
																	</span>
																)}
																{position.expected_days ? <span>预期 {position.expected_days} 日</span> : null}
																{position.entry_confidence ? <span title={position.entry_regime ? `建仓环境：${position.entry_regime}` : ''}>置信 {position.entry_confidence.toFixed(2)}</span> : null}
																{position.review_count ? <span>复核 {position.review_count} 轮</span> : null}
																{position.auto_reduce_at ? <span className="trading-chip">已自动减仓</span> : null}
															</div>
														</td>
														<td>
															<button type="button" className="trading-link" onClick={() => setProtection({
																symbol: position.symbol,
																stop: position.stop_loss_price ? String(position.stop_loss_price) : '',
																takeProfit: position.take_profit_price ? String(position.take_profit_price) : '',
															})}>
																{editing ? '编辑中' : '改防守线'}
															</button>
															<button type="button" className="trading-link" onClick={() => setWatchlist((current) => (current ? `${current}, ${position.symbol}` : position.symbol))}>
																加入关注池
															</button>
														</td>
													</tr>
													{editing && (
														<tr className="trading-protection-row">
															<td colSpan={8}>
																<div className="trading-protection-form">
																	<strong>{position.symbol} 防守线</strong>
																	<label>止损价
																		<input type="number" step="0.01" min="0" value={protection.stop}
																			onChange={(event) => setProtection({ ...protection, stop: event.target.value })} />
																	</label>
																	<label>止盈价
																		<input type="number" step="0.01" min="0" value={protection.takeProfit}
																			onChange={(event) => setProtection({ ...protection, takeProfit: event.target.value })} />
																	</label>
																	<button type="button" className="trading-primary" onClick={() => void handleSaveProtection()}>保存</button>
																	<button type="button" className="trading-link" onClick={() => setProtection(null)}>取消</button>
																	<span className="trading-subtle">填 0 表示不设该防线；止损由系统每轮自动执行，不等模型同意。</span>
																</div>
															</td>
														</tr>
													)}
												</Fragment>
											);
										})}
									</tbody>
								</table>
							</div>
						</section>

						{active && pendingOrders.length > 0 && (
							<section className="trading-panel">
								<header><RefreshCw size={17} /><h2>挂单</h2><span className="trading-subtle">限价未触及前保持挂起</span></header>
								<div className="trading-pending">
									{pendingOrders.map((order) => (
										<PendingOrder key={order.id} order={order} onCancel={async () => {
											if (!config) return;
											try {
												await cancelTradeOrder(config, order.id);
												setMessage('订单已撤销');
												await loadPlans();
											} catch (cause) {
												setError(cause instanceof Error ? cause.message : '撤销失败');
											}
										}} />
									))}
								</div>
							</section>
						)}
					</>
				)}

				{activeView === 'autopilot' && (
					<>
						<AutopilotPanel
							status={autopilot}
							busy={autopilotBusy}
							cycleBusy={cycleBusy}
							onConfigure={(patch) => void handleAutopilot(patch)}
							onRun={() => void handleRunCycle()}
							onResetBreaker={() => void handleResetBreaker()}
							onReleaseDrawdown={() => void handleReleaseDrawdown()}
						/>
						<CycleLog cycles={cycles} config={config} onOpenPlan={(planId) => void openPlanById(planId)} />
						{/* 决策时间线与周期日志同页：两者都是「看系统干了什么」，
						    区别只是时间线按日回放、日志看最近 20 轮。 */}
						<TimelinePanel
							day={timelineDay}
							timeline={timeline}
							loading={timelineLoading}
							onChangeDay={setTimelineDay}
							onRefresh={() => void loadTimeline(timelineDay)}
						/>
					</>
				)}

				{activeView === 'performance' && (
					<>
						<PerformancePanel performance={performance} onRefresh={() => void loadReview()} />
						<CalibrationPanel report={calibration} />
					</>
				)}

				{activeView === 'review' && (
					<>
						<ReviewPanel
							review={reviews[0] ?? null}
							history={reviews}
							trades={closedTrades}
							busy={reviewBusy}
							onRun={() => void handleRunReview()}
							onRunProtective={() => void handleRunProtective()}
						/>
						{/* 板块记忆与复盘同页：复盘回答「这轮哪里错了」，记忆回答「错完沉淀了什么」。 */}
						<MemoryPanel
							memories={memories}
							busy={memoryBusy}
							onSync={() => void handleSyncMemory()}
						/>
					</>
				)}

				{activeView === 'ops' && (
					<>
						<HealthBar health={health} channel={channel} onRefresh={() => void loadOps()} />
						<AlertPanel
							alerts={alerts}
							health={health}
							testing={alertTesting}
							onTest={() => void handleTestAlert()}
							onRefresh={() => void loadOps()}
						/>
						<OpsLog events={events} onRefresh={() => void loadOps()} />
					</>
				)}
			</div>
		</section>
	);
}

// PerformancePanel 是「这套系统到底赚不赚钱」的唯一入口。
//
// 它只呈现已实现结果：未平仓的浮盈不进胜率，否则每天都在改历史。
// 样本不足 30 笔时明确标注"还看不出来"，而不是把 3 笔 2 胜说成 67% 胜率。
function PerformancePanel({
	performance,
	onRefresh,
}: {
	performance: TradePerformance | null;
	onRefresh: () => void;
}) {
	const perf = performance;
	const daily = perf?.daily ?? [];
	const maxAbs = Math.max(1, ...daily.map((item) => Math.abs(item.pnl)));
	const sampleShort = (perf?.closed_trades ?? 0) < 30;
	const bySymbol = [...(perf?.by_symbol ?? [])].sort((left, right) => Math.abs(right.total_pnl) - Math.abs(left.total_pnl));
	const lessons = (perf?.lessons ?? []).slice(0, 6);
	const pnlTone = (value: number) => (value > 0 ? 'up' : value < 0 ? 'down' : '');
	const signed = (value: number) => `${value > 0 ? '+' : ''}${formatMoney(value)}`;

	return (
		<section className="trading-panel">
			<header>
				<Scale size={17} />
				<h2>业绩归因</h2>
				<span className="trading-subtle">
					{perf ? `${perf.from} ~ ${perf.to} · ${perf.closed_trades} 笔平仓` : '尚未加载'}
				</span>
				<button type="button" className="trading-ghost" onClick={onRefresh}>
					<RefreshCw size={14} />刷新
				</button>
			</header>
			{!perf ? (
				<p className="trading-hint">正在读取业绩…</p>
			) : perf.closed_trades === 0 ? (
				<p className="trading-hint">
					最近 {perf.days} 天没有已实现平仓。业绩只统计落袋的交易：未平仓的浮盈浮亏随行情跳动，把它算进胜率等于每天都在改历史。
				</p>
			) : (
				<>
					<div className="trading-summary trading-perf-summary">
						<article>
							<span>已实现盈亏</span>
							<strong className={pnlTone(perf.realized_pnl)}>{signed(perf.realized_pnl)}</strong>
							<em>
								盈利 {formatMoney(perf.gross_profit)} · 亏损 {formatMoney(-perf.gross_loss)}
							</em>
						</article>
						<article>
							<span>胜率</span>
							<strong>{perf.win_rate.toFixed(1)}%</strong>
							<em>
								{perf.wins} 胜 / {perf.losses} 负
							</em>
						</article>
						<article>
							<span>盈亏比</span>
							<strong>{perf.profit_factor > 0 ? perf.profit_factor.toFixed(2) : '—'}</strong>
							<em>{perf.profit_factor > 0 ? '总盈利 ÷ 总亏损' : '样本期内没有亏损笔'}</em>
						</article>
						<article>
							<span>单笔期望</span>
							<strong className={pnlTone(perf.expectancy)}>{signed(perf.expectancy)}</strong>
							<em>每笔平均已实现</em>
						</article>
					</div>

					{sampleShort && (
						<p className="trading-hint warn">
							样本只有 {perf.closed_trades} 笔，低于 30 笔，胜率与盈亏比还看不出来；这里只说明"到目前为止发生了什么"，不足以证明策略有效。
						</p>
					)}

					<div className="trading-perf-facts">
						<span>平均每笔盈利 {formatMoney(perf.avg_win)}</span>
						<span>平均每笔亏损 {formatMoney(perf.avg_loss)}</span>
						<span>平均持有 {perf.avg_hold_days.toFixed(1)} 天</span>
						<span>最好一笔 {signed(perf.best_trade)}</span>
						<span>最差一笔 {signed(perf.worst_trade)}</span>
					</div>

					<div className="trading-perf-exits">
						<span>止损 {perf.stop_loss_exits}</span>
						<span>硬止损 {perf.hard_stop_exits}</span>
						<span>移动止损 {perf.trail_stop_exits}</span>
						<span>止盈 {perf.take_profit_exits}</span>
						<span>模型离场 {perf.model_exits}</span>
					</div>

					{daily.length > 0 && (
						<div className="trading-perf-curve">
							<div className="trading-perf-curve-head">
								<strong>逐日已实现</strong>
								<span>累计 {signed(perf.realized_pnl)}</span>
							</div>
							<div className="trading-perf-bars">
								{daily.map((item) => {
									const height = Math.max(2, Math.round((Math.abs(item.pnl) / maxAbs) * 56));
									return (
										<div
											key={item.day}
											className={`trading-perf-bar ${item.pnl >= 0 ? 'up' : 'down'}`}
											title={`${item.day} · ${item.trades} 笔 · ${signed(item.pnl)} 元 · 累计 ${signed(item.cumulative)}`}
										>
											<i style={{ height: `${height}px` }} />
											<small>{item.day.slice(5)}</small>
										</div>
									);
								})}
							</div>
						</div>
					)}

					<div className="trading-perf-groups">
						<div>
							<strong>按标的</strong>
							<ul>
								{bySymbol.slice(0, 6).map((lesson) => (
									<li key={`${lesson.dimension}-${lesson.key}`}>
										<span>{lesson.key}</span>
										<em className={pnlTone(lesson.total_pnl)}>
											{lesson.sample} 笔 · {signed(lesson.total_pnl)}
										</em>
									</li>
								))}
								{bySymbol.length === 0 && <li className="trading-empty-cell">暂无平仓</li>}
							</ul>
						</div>
						<div>
							<strong>按离场原因与行业</strong>
							<ul>
								{lessons.map((lesson) => (
									<li key={`${lesson.dimension}-${lesson.key}`}>
										<span>
											{lesson.dimension}·{lesson.key}
										</span>
										<em className={pnlTone(lesson.total_pnl)}>
											{lesson.sample} 笔
											{lesson.win_rate != null ? ` · 胜率 ${lesson.win_rate.toFixed(0)}%` : ' · 样本不足'} · {signed(lesson.total_pnl)}
										</em>
									</li>
								))}
								{lessons.length === 0 && <li className="trading-empty-cell">暂无平仓</li>}
							</ul>
						</div>
					</div>
				</>
			)}
		</section>
	);
}

const triggerText: Record<string, string> = {
	stop_loss: '止损',
	hard_stop: '硬止损',
	trail_stop: '移动止损',
	model_exit: '模型离场',
	manual_exit: '人工离场',
};

function triggerName(trigger?: string): string {
	if (!trigger) return '未标注';
	if (trigger.startsWith('take_profit')) {
		const level = trigger.split('_').pop();
		return `止盈 ${level}`;
	}
	return triggerText[trigger] ?? trigger;
}

// CalibrationPanel 回答一个很具体的问题：模型说 0.9 的时候，是不是真的更准。
//
// 它存在的理由不是好看：置信度已经在参与仓位定价（置信度低就只给部分额度），
// 如果这个自评数字从来没被验证过，"按置信度定价"等于把未经验证的自信放大成资金暴露。
// 所以这里不隐藏难看的数字——偏差为负就是负，样本不足就直接说不足。
function CalibrationPanel({ report }: { report: TradeCalibration | null }) {
	if (!report) return null;
	const buckets = (report.buckets ?? []).filter((item) => item.samples + item.flats > 0);
	const shortSample = report.samples < 30;
	const gapTone = (gap: number) => (gap <= -0.15 ? 'down' : gap >= 0.15 ? 'up' : '');
	return (
		<section className="trading-panel">
			<header>
				<Gauge size={17} />
				<h2>置信度校准</h2>
				<span className="trading-subtle">
					{report.samples > 0 ? `${report.samples} 笔可校准 · Brier ${report.brier.toFixed(3)}` : '尚无可校准样本'}
				</span>
			</header>
			<p className={`trading-hint${shortSample ? ' warn' : ''}`}>{report.verdict}</p>
			{buckets.length > 0 && (
				<table className="trading-calibration">
					<thead>
						<tr>
							<th>自评区间</th>
							<th>笔数</th>
							<th>实际胜率</th>
							<th>平均自评</th>
							<th>偏差</th>
							<th>平均盈亏</th>
						</tr>
					</thead>
					<tbody>
						{buckets.map((item) => (
							<tr key={item.label}>
								<td>{item.label}</td>
								<td>
									{item.samples}
									{item.flats > 0 ? ` + ${item.flats} 平` : ''}
								</td>
								<td>{item.samples > 0 ? `${(item.win_rate * 100).toFixed(0)}%` : '—'}</td>
								<td>{(item.avg_confidence * 100).toFixed(0)}%</td>
								<td className={gapTone(item.gap)}>
									{item.samples > 0 ? `${item.gap > 0 ? '+' : ''}${(item.gap * 100).toFixed(0)}pp` : '—'}
								</td>
								<td className={item.avg_pnl_percent > 0 ? 'up' : item.avg_pnl_percent < 0 ? 'down' : ''}>
									{item.samples > 0 ? `${item.avg_pnl_percent > 0 ? '+' : ''}${item.avg_pnl_percent.toFixed(2)}%` : '—'}
								</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
			<p className="trading-hint">
				排除项：{report.flats} 笔打平、{report.missing} 笔未填置信度。打平不算失败（否则胜率被系统性压低），
				未填不当作零信心（保护性卖出的 0 是刻意的）。
			</p>
		</section>
	);
}

// ReviewPanel 展示"系统今天学到了什么"。
//
// 它刻意不美化数字：胜率不足样本时不显示百分比，亏损笔一定与盈利笔并列，
// 拦截原因按次数排在前面。复盘面板一旦开始挑选对自己有利的数字，就失去意义。
function ReviewPanel({
	review,
	history,
	trades,
	busy,
	onRun,
	onRunProtective,
}: {
	review: TradeReview | null;
	history: TradeReview[];
	trades: TradeClosedTrade[];
	busy: boolean;
	onRun: () => void;
	onRunProtective: () => void;
}) {
	const facts = review?.facts;
	const lessons: TradeLesson[] = (review?.cumulative && review.cumulative.length > 0 ? review.cumulative : review?.lessons) ?? [];
	return (
		<section className="trading-panel">
			<header>
				<Gauge size={17} />
				<h2>持仓防守线与复盘</h2>
				<span className="trading-subtle">
					{review ? `${review.trading_day} 已复盘 · 共 ${history.length} 天` : '数字全部来自账本，模型不参与计算'}
				</span>
				<div className="trading-auto-actions">
					<button type="button" className="trading-link" disabled={busy} onClick={onRunProtective}>
						{busy ? '执行中…' : '立即检查防守线'}
					</button>
					<button type="button" className="trading-link" disabled={busy} onClick={onRun}>
						{busy ? '计算中…' : '生成今日复盘'}
					</button>
				</div>
			</header>

			{!facts ? (
				<p className="trading-empty">
					还没有复盘记录。收盘后（15:05 起）系统会自动生成一次；也可以现在点「生成今日复盘」，它只用已落库的事实计算，不会调模型。
				</p>
			) : (
				<>
					<div className="trading-review-stats">
						<div><span>已实现盈亏</span><strong className={facts.realized_pnl >= 0 ? 'up' : 'down'}>{facts.realized_pnl >= 0 ? '+' : ''}{formatMoney(facts.realized_pnl)}</strong></div>
						<div><span>当日权益</span><strong>{formatMoney(facts.equity_start)} → {formatMoney(facts.equity_end)}</strong></div>
						<div><span>平仓 / 胜率</span><strong>{facts.closed_trades} 笔{facts.closed_trades >= 30 ? ` · ${facts.win_rate.toFixed(1)}%` : ' · 样本不足'}</strong></div>
						<div><span>盈亏比</span><strong>{facts.gross_loss > 0 ? facts.profit_factor.toFixed(2) : '无亏损'}</strong></div>
						<div><span>平均持有</span><strong>{facts.avg_hold_days.toFixed(1)} 天</strong></div>
						<div><span>止损 / 止盈</span><strong>{facts.stop_loss_exits + facts.hard_stop_exits + facts.trail_stop_exits} / {facts.take_profit_exits}</strong></div>
						<div><span>建议 / 通过 / 拦截</span><strong>{facts.proposed} / {facts.accepted} / {facts.blocked}</strong></div>
						<div><span>循环 / 失败</span><strong>{facts.cycles} / {facts.cycles_failed}</strong></div>
					</div>

					{(facts.block_reasons ?? []).length > 0 && (
						<div className="trading-sector-chips">
							<span className="trading-subtle">拦截原因：</span>
							{(facts.block_reasons ?? []).slice(0, 6).map((item) => (
								<span key={item.code} className="trading-chip">{item.code} × {item.count}</span>
							))}
						</div>
					)}

					<ul className="trading-replay-list trading-review-narrative">
						{(review?.narrative ?? []).map((line, index) => <li key={index}>{line}</li>)}
					</ul>

					<div className="trading-lessons">
						<h3>经验（累积，样本 ≥ 30 才给胜率）</h3>
						{lessons.length === 0 ? (
							<p className="trading-empty">还没有已实现平仓，暂时没有可用结论。</p>
						) : (
							<ul>
								{lessons.slice(0, 8).map((lesson) => (
									<li key={`${lesson.dimension}-${lesson.key}`} className={lesson.reliable ? 'reliable' : ''}>
										<span className="trading-chip">{lesson.dimension} · {lesson.key}</span>
										<span>{lesson.text}</span>
									</li>
								))}
							</ul>
						)}
					</div>

					{trades.length > 0 && (
						<div className="trading-table-wrap">
							<table className="trading-table trading-closed-table">
								<thead>
									<tr><th>平仓时间</th><th>标的</th><th>数量</th><th>成本 → 卖出</th><th>盈亏</th><th>原因</th></tr>
								</thead>
								<tbody>
									{trades.slice(0, 10).map((trade) => (
										<tr key={trade.id}>
											<td>{new Date(trade.closed_at).toLocaleString('zh-CN')}</td>
											<td><strong>{trade.symbol}</strong><span>{trade.name}</span></td>
											<td>{trade.quantity} 股</td>
											<td>{trade.entry_price.toFixed(3)} → {trade.exit_price.toFixed(2)}</td>
											<td className={trade.pnl >= 0 ? 'up' : 'down'}>
												{trade.pnl >= 0 ? '+' : ''}{formatMoney(trade.pnl)}
												<span className="trading-subtle">{trade.pnl_percent >= 0 ? '+' : ''}{trade.pnl_percent.toFixed(2)}%</span>
											</td>
											<td>{triggerName(trade.trigger)}{trade.source === 'protective' ? ' · 自动' : ''}</td>
										</tr>
									))}
								</tbody>
							</table>
						</div>
					)}
				</>
			)}
		</section>
	);
}

function PendingOrder({ order, onCancel }: { order: TradeOrder; onCancel: () => Promise<void> }) {
	return (
		<div className="trading-pending-item">
			<div>
				<strong>{order.symbol} {order.name}</strong>
				<span>
					{order.side === 'buy' ? '买入' : '卖出'} {order.quantity} 股 · 限价 {order.limit_price.toFixed(2)}
					{order.side === 'buy' && order.reserved_amount
						? ` · 已冻结 ¥ ${formatMoney(order.reserved_amount)}`
						: ''}
				</span>
			</div>
			<button type="button" className="trading-link" onClick={() => void onCancel()}>撤销</button>
		</div>
	);
}

type AutopilotPanelProps = {
	status: TradeAutopilotStatus | null;
	busy: boolean;
	cycleBusy: boolean;
	onConfigure: (patch: Parameters<typeof configureTradingAutopilot>[1]) => void;
	onRun: () => void;
	onResetBreaker: () => void;
	onReleaseDrawdown: () => void;
};

// 自动循环面板：把"开关、闸门、熔断"三件事放在同一屏。
// 它们是同一条链路上的连续环节，分散在三处会让人误以为"开关打开就等于会交易"。
function AutopilotPanel({ status, busy, cycleBusy, onConfigure, onRun, onResetBreaker, onReleaseDrawdown }: AutopilotPanelProps) {
	const config = status?.config;
	const today = status?.today;
	const tripped = Boolean(today?.breaker);
	const last = status?.last_cycle;
	const risk = status?.risk;
	const drawdownStopped = risk?.level === 'drawdown.stop';
	// 排班轮次与当前生效间隔都从 config 推导，不在前端另存一份：
	// 两处各写一套，界面显示的节奏迟早会和真实调度对不上。
	const scheduledCycles = scheduleCycles(config?.schedule);

	return (
		<section className="trading-autopilot">
			<header>
				<Gauge size={17} />
				<h2>自动交易循环</h2>
				<span className={`trading-auto-tag ${config?.enabled ? 'on' : 'off'}`}>
					{config?.enabled ? (status?.running ? '本轮执行中' : '运行中') : '已停止'}
				</span>
				<div className="trading-auto-actions">
					<button type="button" className="trading-ghost" onClick={onRun} disabled={cycleBusy}>
						{cycleBusy ? <LoaderCircle size={14} className="spin" /> : <Play size={14} />} 立即跑一轮
					</button>
					{tripped && (
						<button type="button" className="trading-ghost danger" onClick={onResetBreaker} disabled={busy}>
							解除熔断
						</button>
					)}
					{drawdownStopped && (
						<button type="button" className="trading-ghost danger" onClick={onReleaseDrawdown} disabled={busy}>
							解除回撤熔断
						</button>
					)}
				</div>
			</header>

			{!config ? (
				<p className="trading-empty">正在读取自动循环状态…</p>
			) : (
				<>
					<div className="trading-auto-grid">
						<button
							type="button"
							className={`trading-switch ${config.enabled ? 'on' : 'off'}`}
							onClick={() => onConfigure({ enabled: !config.enabled })}
							disabled={busy}
						>
							<span>自动循环</span>
							<strong>{config.enabled ? '已开启' : '已关闭'}</strong>
						</button>
						<button
							type="button"
							className={`trading-switch ${config.auto_execute ? 'on' : 'off'}`}
							onClick={() => onConfigure({ auto_execute: !config.auto_execute })}
							disabled={busy}
						>
							<span>自动执行</span>
							<strong>{config.auto_execute ? '生成即撮合' : '需人工确认'}</strong>
						</button>
						<label className="trading-auto-field">
							<span>兜底间隔</span>
							<select
								value={config.interval_seconds}
								onChange={(event) => onConfigure({ interval_seconds: Number(event.target.value) })}
								disabled={busy}
							>
								<option value={60}>1 分钟</option>
								<option value={180}>3 分钟</option>
								<option value={300}>5 分钟</option>
								<option value={600}>10 分钟</option>
								<option value={900}>15 分钟</option>
							</select>
						</label>
						<label className="trading-auto-field">
							<span>日亏熔断</span>
							<select
								value={config.max_daily_loss_percent}
								onChange={(event) => onConfigure({ max_daily_loss_percent: Number(event.target.value) })}
								disabled={busy}
							>
								<option value={2}>-2%</option>
								<option value={3}>-3%</option>
								<option value={5}>-5%</option>
							</select>
						</label>
					</div>

					<div className="trading-auto-stats">
						<div>
							<span>当日基准</span>
							<strong>{today && today.start_equity > 0 ? `¥ ${formatMoney(today.start_equity)}` : '待开轮'}</strong>
						</div>
						<div>
							<span>上一轮后当日盈亏</span>
							<strong className={(last?.day_change_percent ?? 0) >= 0 ? 'up' : 'down'}>
								{last ? `${(last.day_change_percent ?? 0) >= 0 ? '+' : ''}${(last.day_change_percent ?? 0).toFixed(2)}%` : '—'}
							</strong>
						</div>
						<div>
							<span>当日轮次</span>
							<strong>{status?.cycles_today ?? 0} / {config.max_cycles_per_day}</strong>
						</div>
						<div>
							<span>当前节奏</span>
							<strong>{config ? intervalText(currentIntervalSeconds(config)) : '—'}</strong>
						</div>
						<div>
							<span>全天计划</span>
							<strong>
								{scheduledCycles > 0 ? `${scheduledCycles} 轮（按排班）` : `${config.max_cycles_per_day} 轮（固定间隔）`}
							</strong>
						</div>
						<div>
							<span>开仓截止</span>
							<strong>{config.buy_cutoff}</strong>
						</div>
						<div>
							<span>下一轮</span>
							<strong>
								{status?.next_run_at && config.enabled
									? new Date(status.next_run_at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
									: '—'}
							</strong>
						</div>
					</div>

					{tripped && (
						<div className="trading-breaker-alert">
							<AlertTriangle size={16} />
							<div>
								<strong>当日已熔断，只允许卖出与减仓</strong>
								<span>{today?.breaker_detail || '触发原因未记录'}</span>
							</div>
						</div>
					)}

					{risk && risk.peak_equity > 0 ? (
						<div className={`trading-drawdown ${risk.level === 'drawdown.stop' ? 'stop' : risk.level === 'drawdown.warn' ? 'warn' : 'ok'}`}>
							<span>净值回撤</span>
							<strong>{risk.drawdown_percent.toFixed(2)}%</strong>
							<span className="trading-subtle">
								高点 {formatMoney(risk.peak_equity)} → 现值 {formatMoney(risk.equity)}
								{' · '}警戒 {risk.policy.warn_percent.toFixed(1)}% / 停止 {risk.policy.stop_percent.toFixed(1)}%
								{risk.level === 'drawdown.stop'
									? ' · 已触发停止线，买入暂停，需人工解除'
									: risk.level === 'drawdown.warn'
										? ` · 已触发警戒线，回到 ${risk.policy.release_percent.toFixed(1)}% 以内自动解除`
										: ''}
							</span>
						</div>
					) : null}

					<div className="trading-breakers">
						{(status?.breakers ?? []).map((breaker) => (
							<span
								key={breaker.code}
								className={`trading-breaker ${breaker.tripped ? 'tripped' : breaker.armed ? 'armed' : 'inert'}`}
								title={breaker.detail || ''}
							>
								{breaker.label}
								<em>{breaker.tripped ? '已触发' : breaker.armed ? '监控中' : '未接线'}</em>
							</span>
						))}
					</div>

					{last && (
						<p className="trading-auto-last">
							最近一轮 {new Date(last.started_at).toLocaleString('zh-CN')} · {tradeCycleStatusText[last.status] ?? last.status}
							{last.reason ? `：${last.reason}` : `，建议 ${last.orders} 笔、成交 ${last.filled} 笔`}
						</p>
					)}
					<p className="trading-hint">
						自动循环每轮先撮合既有挂单，再由模型判断并经过守卫裁决。日亏按当日基准权益计算；
						尾盘 {config.buy_cutoff} 后自动触发的一轮不开新仓，人工触发可越过时段限制，但越不过总闸与熔断。
					</p>
				</>
			)}
		</section>
	);
}

function CycleLog({ cycles, config, onOpenPlan }: { cycles: TradeCycle[]; config: BackendConfig; onOpenPlan: (planId: string) => void }) {
	const [expanded, setExpanded] = useState<string | null>(null);
	return (
		<section className="trading-panel">
			<header>
				<History size={17} />
				<h2>交易员日志</h2>
				<span className="trading-subtle">最近 20 轮 · 展开看市场、候选、模型、裁决、成交五段</span>
			</header>
			{cycles.length === 0 ? (
				<p className="trading-empty">还没有循环记录。打开自动循环或点「立即跑一轮」。</p>
			) : (
				<ul className="trading-cycles">
					{cycles.map((cycle) => (
						<li key={cycle.id} className={cycle.status}>
							<div className="trading-cycle-head">
								<Timer size={14} />
								<strong>{new Date(cycle.started_at).toLocaleString('zh-CN')}</strong>
								<span className={`trading-cycle-status ${cycle.status}`}>
									{tradeCycleStatusText[cycle.status] ?? cycle.status}
								</span>
								<span className="trading-subtle">
									{tradeCycleTriggerText[cycle.trigger] ?? cycle.trigger}
									{cycle.duration_ms ? ` · ${(cycle.duration_ms / 1000).toFixed(1)}s` : ''}
								</span>
								{cycle.plan_id && (
									<>
										<button
											type="button"
											className="trading-link"
											onClick={() => setExpanded((current) => (current === cycle.id ? null : cycle.id))}
										>
											{expanded === cycle.id ? '收起' : '回放'}
										</button>
										<button type="button" className="trading-link" onClick={() => onOpenPlan(cycle.plan_id!)}>
											查看计划
										</button>
									</>
								)}
							</div>
							<div className="trading-cycle-meta">
								<span>权益 {formatMoney(cycle.equity_before)} → {formatMoney(cycle.equity_after)}</span>
								<span className={(cycle.day_change_percent ?? 0) >= 0 ? 'up' : 'down'}>
									当日 {(cycle.day_change_percent ?? 0) >= 0 ? '+' : ''}{(cycle.day_change_percent ?? 0).toFixed(2)}%
								</span>
								<span>建议 {cycle.orders} 笔</span>
								<span>成交 {cycle.filled} 笔</span>
								<span>拦截 {cycle.blocked} 笔</span>
								{cycle.matched > 0 && <span>撮合在途 {cycle.matched} 笔</span>}
							</div>
							{cycle.reason && <p className="trading-cycle-reason">{cycle.reason}</p>}
							{(cycle.gates ?? []).length > 0 && (
								<div className="trading-cycle-gates">
									{(cycle.gates ?? []).map((gate) => (
										<span key={gate.code} className={gate.open ? 'open' : 'closed'} title={gate.detail || ''}>
											{gate.label}{gate.open ? '' : ' ✕'}
										</span>
									))}
								</div>
							)}
							{(cycle.notes ?? []).length > 0 && (
								<ul className="trading-notes">
									{(cycle.notes ?? []).map((note, index) => <li key={`${cycle.id}-${index}`}>{note}</li>)}
								</ul>
							)}
							{expanded === cycle.id && cycle.plan_id && <CycleReplay config={config} planId={cycle.plan_id} />}
						</li>
					))}
				</ul>
			)}
		</section>
	);
}

// CycleReplay 是「任意一笔成交三次点击回到所属 cycle」的落地：
// 展开一轮 → 看到模型原始建议与裁决 → 点标的看订单。
// 数据全部来自已落库的计划与裁决，不做二次推断。
function CycleReplay({ config, planId }: { config: BackendConfig; planId: string }) {
	const [plan, setPlan] = useState<TradePlan | null>(null);
	const [decision, setDecision] = useState<TradeDecision | null>(null);
	const [failed, setFailed] = useState(false);

	useEffect(() => {
		let alive = true;
		Promise.all([fetchTradePlan(config, planId), fetchTradeDecisionByPlan(config, planId)])
			.then(([loadedPlan, loadedDecision]) => {
				if (!alive) return;
				setPlan(loadedPlan);
				setDecision(loadedDecision);
			})
			.catch(() => {
				if (alive) setFailed(true);
			});
		return () => {
			alive = false;
		};
	}, [config, planId]);

	if (failed) return <p className="trading-cycle-reason">这一轮的计划读取失败（可能仍在生成）。</p>;
	if (!plan) return <p className="trading-cycle-reason">正在读取这一轮的记录…</p>;

	const market = plan.snapshot.market;
	const board = plan.snapshot.board;
	const proposal = decision?.proposal ?? [];
	const verdicts = decision?.verdicts ?? plan.verdicts ?? [];
	const blocked = verdicts.filter((verdict) => verdict.level === 'block');

	return (
		<div className="trading-replay">
			<div className="trading-replay-section">
				<h3>① 市场</h3>
				{market?.evaluated ? (
					<p>
						{tradeRegimeText[market.regime] ?? market.regime} · 风险预算 {market.risk_budget_percent?.toFixed(0)}% · 中位数{' '}
						{market.breadth?.median_change?.toFixed(2)}% · 涨停 {market.breadth?.limit_up} / 跌停{' '}
						{market.breadth?.limit_down}
					</p>
				) : (
					<p>本轮未取得全市场状态（人工指定标的或行情源不可用）。</p>
				)}
			</div>
			<div className="trading-replay-section">
				<h3>② 候选池</h3>
				{board && (board.candidates ?? []).length > 0 ? (
					<p>
						共 {(board.candidates ?? []).length} 只（源 {board.source_count} 只
						{board.degraded ? '，当日缩量已下调门槛' : ''}）：{' '}
						{(board.candidates ?? []).slice(0, 5).map((candidate) => `${candidate.name}(${candidate.score.toFixed(0)})`).join('、')}
						{(board.candidates ?? []).length > 5 ? ' …' : ''}
					</p>
				) : (
					<p>本轮未做全市场筛选（标的由人工指定）。</p>
				)}
			</div>
			<div className="trading-replay-section">
				<h3>③ 模型</h3>
				<p>
					{plan.regime || '未标注市场状态'}
					{decision?.model?.latency_ms ? ` · 耗时 ${(decision.model.latency_ms / 1000).toFixed(1)}s` : ''}
					{decision?.model?.total_tokens ? ` · ${decision.model.total_tokens} tokens` : ''}
				</p>
				<p>{plan.summary || plan.error || '—'}</p>
				{proposal.length > 0 && (
					<ul className="trading-replay-list">
						{proposal.map((order, index) => (
							<li key={`${order.symbol}-${index}`}>
								{order.side === 'buy' ? '买入' : '卖出'} {order.symbol}
								{order.target_weight_percent ? ` · 目标 ${order.target_weight_percent}%` : ''}
								{order.sell_fraction_percent ? ` · 减 ${order.sell_fraction_percent}%` : ''}
								{order.thesis ? ` · ${order.thesis}` : ''}
							</li>
						))}
					</ul>
				)}
			</div>
			<div className="trading-replay-section">
				<h3>④ 裁决</h3>
				<p>
					建议 {decision?.stats?.proposed ?? proposal.length} 笔 · 通过 {decision?.stats?.accepted ?? 0} 笔 · 拦截{' '}
					{decision?.stats?.blocked ?? blocked.length} 笔 · 调整 {decision?.stats?.adjusted ?? 0} 处
				</p>
				{blocked.length > 0 && (
					<ul className="trading-replay-list">
						{blocked.slice(0, 6).map((verdict, index) => (
							<li key={`${verdict.code}-${index}`}>
								<code>{verdict.code}</code> {verdict.message}
							</li>
						))}
					</ul>
				)}
			</div>
			<div className="trading-replay-section">
				<h3>⑤ 成交</h3>
				{(plan.orders ?? []).length === 0 ? (
					<p>本轮没有产生订单。</p>
				) : (
					<ul className="trading-replay-list">
						{(plan.orders ?? []).map((order) => (
							<li key={order.id}>
								{order.side === 'buy' ? '买入' : '卖出'} {order.symbol} {order.quantity} 股 ·{' '}
								{tradeOrderStatusText[order.status] ?? order.status}
								{order.status === 'rejected' && order.reject_reason ? `：${order.reject_reason}` : ''}
							</li>
						))}
					</ul>
				)}
			</div>
		</div>
	);
}

// HealthBar 是无人值守模式的第一行：一眼看出"系统还能不能自己跑"。
//
// 它刻意不显示盈亏——盈亏是结果，健康是前提。把两者混在一起时，
// 人会在账户赚着钱的时候忽略"循环已经三天没跑了"这种致命问题。
function HealthBar({
	health,
	channel,
	onRefresh,
}: {
	health: TradeHealth | null;
	channel: TradeChannelInfo | null;
	onRefresh: () => void;
}) {
	if (!health) {
		return (
			<div className="trading-health unknown">
				<LoaderCircle size={15} className="spin" />
				<strong>运行状态读取中</strong>
				<span className="trading-health-meta">等待后端返回健康快照</span>
			</div>
		);
	}
	const tone = health.status === 'critical' ? 'critical' : health.status === 'warning' ? 'warning' : 'healthy';
	const label = tone === 'critical' ? '需要立即处理' : tone === 'warning' ? '注意' : '运行正常';
	const beat = health.autopilot;
	return (
		<div className={`trading-health ${tone}`}>
			{tone === 'healthy' ? <CheckCircle2 size={15} /> : tone === 'warning' ? <AlertTriangle size={15} /> : <ShieldAlert size={15} />}
			<strong>{label}</strong>
			<span className="trading-health-meta">
				{channel ? (channel.live ? '实盘通道' : '模拟盘') : health.channel}
				{' · '}运行 {uptimeText(health.uptime_seconds)}
				{' · '}
				{beat.running
					? beat.last_tick_at
						? `心跳 ${new Date(beat.last_tick_at).toLocaleTimeString('zh-CN')}`
						: '尚未心跳'
					: '自动循环未启动'}
				{' · '}挂单 {health.open_orders}
				{' · '}账本 {health.ledger.ok ? '一致' : `漂移 ¥${formatMoney(health.ledger.drift)}`}
				{health.drawdown && health.drawdown.peak_equity > 0
					? ` · 回撤 ${health.drawdown.drawdown_percent.toFixed(2)}%${health.drawdown.level ? '（已暂停买入）' : ''}`
					: ''}
				{health.alerts?.configured
					? ` · 告警 ${health.alerts.channels.join('/')}${health.alerts.failed_24h > 0 ? `（24h 失败 ${health.alerts.failed_24h}）` : ''}`
					: ' · 告警未配置'}
				{beat.failure_streak > 0 ? ` · 连续失败 ${beat.failure_streak} 轮` : ''}
				{beat.degraded ? ' · 已降级' : ''}
			</span>
			<div className="trading-health-issues">
				{health.issues.length === 0 ? (
					<span className="trading-health-ok">没有发现问题</span>
				) : (
					health.issues.map((issue) => (
						<span key={issue.code} className={issue.severity} title={issue.code}>
							{issue.message}
						</span>
					))
				)}
			</div>
			<button type="button" className="trading-ghost" onClick={onRefresh}>
				<RefreshCw size={13} /> 重新自检
			</button>
		</div>
	);
}

// OpsLog 是运行事件流。它记录的是"系统自己做了什么决定"：
// 熔断、降级、自动修复、日终作废。这些事实不在交易记录里，
// 但恰恰是"为什么今天没买"的答案。
function OpsLog({ events, onRefresh }: { events: TradeEvent[]; onRefresh: () => void }) {
	return (
		<section className="trading-panel">
			<header>
				<Activity size={17} />
				<h2>运行事件</h2>
				<span className="trading-subtle">最近 30 条 · 熔断、降级、自愈、日终清理</span>
				<button type="button" className="trading-ghost" onClick={onRefresh}>
					<RefreshCw size={13} /> 刷新
				</button>
			</header>
			{events.length === 0 ? (
				<p className="trading-empty">还没有运行事件。熔断、连续失败自愈、日终委托作废都会记在这里。</p>
			) : (
				<ul className="trading-events">
					{events.map((event) => (
						<li key={event.id} className={event.severity}>
							<span className="trading-event-time">{new Date(event.created_at).toLocaleString('zh-CN')}</span>
							<span className={`trading-event-severity ${event.severity}`}>
								{event.severity === 'critical' ? '严重' : event.severity === 'warning' ? '提醒' : '记录'}
							</span>
							<span className="trading-event-message">
								{event.symbol ? `${event.symbol} ` : ''}
								{event.message}
							</span>
							<span className="trading-event-kind">{event.kind}</span>
						</li>
					))}
				</ul>
			)}
		</section>
	);
}

// TimelinePanel 是某一天的回放。
//
// 它存在的理由很具体：系统跑起来之后，"今天为什么没买""这笔单是哪一轮下的"
// 这类问题会散落在循环、计划、裁决、委托、事件五处，各有各的时间字段。
// 把它们按时间摊平成一条线，是排障时最先要建立的东西——时间感。
function TimelinePanel({
	day,
	timeline,
	loading,
	onChangeDay,
	onRefresh,
}: {
	day: string;
	timeline: TradeTimeline | null;
	loading: boolean;
	onChangeDay: (day: string) => void;
	onRefresh: () => void;
}) {
	const entries = timeline?.entries ?? [];
	const stats = timeline?.stats;
	return (
		<section className="trading-panel">
			<header>
				<CalendarClock size={17} />
				<h2>时间线回放</h2>
				<span className="trading-subtle">循环 · 计划 · 裁决 · 委托 · 事件，按时间摊平</span>
				<input
					type="date"
					className="trading-date-input"
					value={day}
					onChange={(event) => onChangeDay(event.target.value)}
					aria-label="回放日期"
				/>
				<button type="button" className="trading-ghost" onClick={onRefresh} disabled={loading}>
					{loading ? <LoaderCircle size={13} className="spin" /> : <RefreshCw size={13} />} 刷新
				</button>
			</header>

			{timeline?.notes && timeline.notes.length > 0 ? (
				<ul className="trading-timeline-notes">
					{timeline.notes.map((note) => (
						<li key={note}>
							<AlertTriangle size={12} /> {note}
						</li>
					))}
				</ul>
			) : null}

			{stats ? (
				<div className="trading-timeline-stats">
					<span>
						<b>{stats.cycles}</b> 轮循环{stats.failed_cycles > 0 ? `（失败 ${stats.failed_cycles}）` : ''}
					</span>
					<span>
						<b>{stats.plans}</b> 份计划
					</span>
					<span>
						<b>{stats.decisions}</b> 次裁决
					</span>
					<span>
						<b>{stats.orders}</b> 笔委托 / <b>{stats.filled}</b> 笔成交
					</span>
					<span>
						<b>{stats.blocked}</b> 笔被拦
					</span>
					<span>
						<b>{stats.events}</b> 条事件
					</span>
					{stats.model_calls > 0 ? (
						<span>
							模型 <b>{stats.model_calls}</b> 次 / <b>{stats.total_tokens.toLocaleString('zh-CN')}</b> tokens
						</span>
					) : null}
				</div>
			) : null}

			{entries.length === 0 ? (
				<p className="trading-empty">
					{loading ? '正在读取当天记录…' : '这一天没有任何记录。可能是非交易日，或当天自动循环没有开启。'}
				</p>
			) : (
				<ol className="trading-timeline">
					{entries.map((entry, index) => (
						<li
							key={`${entry.at}-${entry.category}-${index}`}
							className={`trading-timeline-item ${entry.severity ?? 'info'}`}
						>
							<span className="trading-timeline-time">{new Date(entry.at).toLocaleTimeString('zh-CN', { hour12: false })}</span>
							<span className={`trading-timeline-tag ${entry.category.replace('.', '-')}`}>
								{timelineCategoryText(entry.category)}
							</span>
							{entry.role ? (
								<span
									className={`trading-role ${entry.role.toLowerCase()}`}
									title={`权限链位置：${entry.role}`}
								>
									{reviewRoleText[entry.role] ?? entry.role}
								</span>
							) : null}
							<span className="trading-timeline-body">
								<strong>
									{entry.symbol ? `${entry.symbol} ` : ''}
									{entry.title}
								</strong>
								{entry.detail ? <em>{entry.detail}</em> : null}
							</span>
							{entry.amount && entry.amount > 0 ? (
								<span className="trading-timeline-amount">¥{formatMoney(entry.amount)}</span>
							) : null}
						</li>
					))}
				</ol>
			)}
		</section>
	);
}

function timelineCategoryText(category: string): string {
	const labels: Record<string, string> = {
		'cycle.start': '循环开始',
		'cycle.end': '循环结束',
		plan: '计划',
		decision: '裁决',
		order: '委托',
		fill: '成交',
		event: '事件',
		review: '复盘',
	};
	return labels[category] ?? category;
}

// AlertPanel 回答一个容易被忽略的问题：出事的时候，通知到底有没有送出去。
//
// 展出的不是"应该发什么"，而是"实际发了什么、成没成"。
// 没有投递结果，人只能靠"我没收到短信"来反推，而这种反推在两个方向上都不可靠。
function AlertPanel({
	alerts,
	health,
	testing,
	onTest,
	onRefresh,
}: {
	alerts: TradeAlertsResult | null;
	health: TradeHealth | null;
	testing: boolean;
	onTest: () => void;
	onRefresh: () => void;
}) {
	const records = alerts?.records ?? [];
	const channels = alerts?.channels ?? health?.alerts?.channels ?? [];
	const stats = alerts?.stats ?? {};
	const alertHealth = health?.alerts;
	return (
		<section className="trading-panel">
			<header>
				<BellRing size={17} />
				<h2>告警投递</h2>
				<span className="trading-subtle">熔断、降级、账本漂移、自愈是否真的送到了人手上</span>
				<button type="button" className="trading-ghost" onClick={onTest} disabled={testing || channels.length === 0}>
					{testing ? <LoaderCircle size={13} className="spin" /> : <BellRing size={13} />} 发送测试
				</button>
				<button type="button" className="trading-ghost" onClick={onRefresh}>
					<RefreshCw size={13} /> 刷新
				</button>
			</header>

			{channels.length === 0 ? (
				<p className="trading-empty">
					还没有配置告警出口。出问题时只会记在页面和数据库里，没有人会收到通知——
					这正是无人值守最容易失效的地方。设置环境变量 <code>A_STOCK_TRADING_ALERT_WEBHOOK</code>（企微/钉钉机器人地址）
					后重启服务即可，格式见交付手册。
				</p>
			) : (
				<>
					<div className="trading-alert-summary">
						<span className="trading-alert-channel">出口：{channels.join('、')}</span>
						{alertHealth ? (
							<>
								<span>24h 送达 <b>{alertHealth.sent_24h}</b></span>
								<span className={alertHealth.failed_24h > 0 ? 'warn' : ''}>
									失败 <b>{alertHealth.failed_24h}</b>
								</span>
								{alertHealth.dropped > 0 ? <span className="warn">队列丢弃 <b>{alertHealth.dropped}</b></span> : null}
							</>
						) : null}
						{stats.merged ? <span>已合并 <b>{stats.merged}</b></span> : null}
					</div>

					{records.length === 0 ? (
						<p className="trading-empty">还没有告警记录。没有记录不代表系统没问题，只代表最近没有触发告警条件。</p>
					) : (
						<ul className="trading-alerts">
							{records.map((record) => (
								<li key={record.id} className={`${record.severity} ${record.status}`}>
									<span className="trading-alert-time">{new Date(record.created_at).toLocaleString('zh-CN')}</span>
									<span className={`trading-alert-status ${record.status}`}>{alertStatusText(record.status)}</span>
									<span className="trading-alert-message">
										{record.symbol ? `${record.symbol} ` : ''}
										{record.message}
										{record.merged > 0 ? `（合并 ${record.merged} 条同类）` : ''}
									</span>
									<span className="trading-alert-kind">
										{record.channel ? `${record.channel}` : '未外发'}
										{record.error ? ` · ${record.error}` : ''}
									</span>
								</li>
							))}
						</ul>
					)}
				</>
			)}
		</section>
	);
}

function alertStatusText(status: TradeAlertRecord['status']): string {
	switch (status) {
		case 'sent':
			return '已送达';
		case 'failed':
			return '发送失败';
		default:
			return '已合并';
	}
}

// uptimeText 把秒数说成人话。长时间运行的秒数（如 18732）对人没有意义。
/** 健康状态的中文口径：warning 表示有降级项但不影响成交，critical 才需要立即处理。 */
function healthLabel(status: 'healthy' | 'warning' | 'critical'): string {
	return status === 'healthy' ? '正常' : status === 'warning' ? '注意' : '异常';
}

/** 只取时分：状态条里说「下一轮 14:36」，不需要日期。 */
function clockText(value?: string): string {
	if (!value) return '—';
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return '—';
	return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
}

function uptimeText(seconds: number): string {
	if (!Number.isFinite(seconds) || seconds <= 0) return '刚刚启动';
	if (seconds < 60) return `${Math.round(seconds)} 秒`;
	if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟`;
	if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时 ${Math.round((seconds % 3600) / 60)} 分`;
	return `${Math.floor(seconds / 86400)} 天 ${Math.floor((seconds % 86400) / 3600)} 小时`;
}


// ResearchPanel 是"它到底怎么想的"的唯一证据。
//
// 默认折叠：三步调研的原文能占满一屏，会把订单表挤下去。
// 但折叠时必须露出结论，否则"展开才能看到"等于没给。
function ResearchPanel({ research }: { research?: TradeResearch }) {
	const [open, setOpen] = useState(false);
	if (!research || !research.enabled) return null;
	const steps = research.steps ?? [];
	const seconds = Math.round(research.elapsed_ms / 100) / 10;

	return (
		<div className={`trading-research ${research.degraded ? 'degraded' : ''} ${open ? 'open' : ''}`}>
			<button type="button" className="trading-research-head" onClick={() => setOpen((value) => !value)}>
				<Compass size={15} />
				<strong>调研回放</strong>
				<span className="trading-research-meta">
					{steps.length === 0 ? '未发起查询' : `${steps.length} 步工具调用`}
					{' · '}{seconds}s
					{research.degraded ? ' · 中途降级' : ''}
				</span>
				{open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
			</button>
			{research.conclusion && (
				<p className="trading-research-conclusion">{research.conclusion}</p>
			)}
			{open && (
				<ol className="trading-research-steps">
					{steps.map((step) => (
						<li key={step.index} className={step.error ? 'failed' : ''}>
							<header>
								<span className="index">第 {step.index} 步</span>
								<code>{step.tool}</code>
								{step.args && Object.keys(step.args).length > 0 && (
									<span className="args">{JSON.stringify(step.args)}</span>
								)}
								<span className="latency">{step.latency_ms}ms</span>
								{typeof step.rows === 'number' && step.rows > 0 && <span className="rows">{step.rows} 行</span>}
							</header>
							{step.thought && <p className="thought">「{step.thought}」</p>}
							{step.error ? (
								<p className="error">工具报错：{step.error}</p>
							) : (
								<pre>{step.result}</pre>
							)}
						</li>
					))}
				</ol>
			)}
			{research.note && <p className="trading-research-note">{research.note}</p>}
		</div>
	);
}

const memoryOutcomeText: Record<string, string> = {
	win: '盈利',
	loss: '亏损',
	flat: '平盘',
};

// MemoryPanel 展示的是"这个系统记得什么"。
//
// 与「历史经验」的区别：经验是聚合统计（止损 8 次胜率 25%），
// 记忆是单笔情境（退潮期买入、理由是什么、峰值浮盈多少又回吐了多少）。
// 后者才是模型能真正"回忆"起来的原料，也是人复盘时最想看的东西。
function MemoryPanel({
	memories,
	busy,
	onSync,
}: {
	memories: TradeMemory[];
	busy: boolean;
	onSync: () => void;
}) {
	return (
		<section className="trading-panel">
			<header>
				<Brain size={17} />
				<h2>情景记忆</h2>
				<span className="trading-subtle">每笔平仓当时的市场状态、买入理由与结果</span>
				<button type="button" className="trading-ghost" onClick={onSync} disabled={busy}>
					{busy ? <LoaderCircle size={13} className="spin" /> : <RefreshCw size={13} />} 补齐历史
				</button>
			</header>
			{memories.length === 0 ? (
				<p className="trading-empty">
					还没有记忆。第一笔平仓之后，复盘会自动把当时的市场状态与买入理由存下来；
					模型在下一轮可以用 recall_trades 主动检索它们。
				</p>
			) : (
				<ul className="trading-memories">
					{memories.map((memory) => (
						<li key={memory.id} className={memory.outcome}>
							<div className="trading-memory-head">
								<strong>{memory.name || memory.symbol}</strong>
								<span className={`trading-memory-outcome ${memory.outcome}`}>
									{memoryOutcomeText[memory.outcome] ?? memory.outcome}
								</span>
								<span className="meta">{memory.trading_day} · {memory.regime_label}期 · 持有 {memory.hold_days} 天</span>
								<em className={memory.pnl_percent >= 0 ? 'up' : 'down'}>
									{memory.pnl_percent > 0 ? '+' : ''}{memory.pnl_percent.toFixed(2)}%
								</em>
							</div>
							{memory.thesis && <p className="trading-memory-thesis">当时理由：{memory.thesis}</p>}
							<p className="trading-memory-lesson">{memory.lesson}</p>
							{memory.give_back_percent >= 3 && (
								<p className="trading-memory-giveback">
									峰值浮盈 {memory.peak_gain_percent.toFixed(2)}%，回吐 {memory.give_back_percent.toFixed(2)} 个百分点
								</p>
							)}
						</li>
					))}
				</ul>
			)}
		</section>
	);
}
