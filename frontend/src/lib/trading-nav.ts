// AI 交易员的导航信息架构：左侧分组导航 + 右侧视图区，与行情总览同一套布局语言。
//
// 分组依据是「使用者一天的操作流」，不是功能的技术归属：
// 开盘先看状态（钱和系统在不在工作）→ 交易（选股、确认、持仓）→
// 自动运行交给系统盯 → 收盘后复盘 → 出问题时去运维。
// 每组内的页面数量刻意少：一个视图就是一件事，相关内容在页内分区，不摊成多个入口。

export type TradingView =
	| 'overview'
	| 'market'
	| 'planning'
	| 'positions'
	| 'autopilot'
	| 'performance'
	| 'review'
	| 'ops';

export type TradingNavModule = {
	id: TradingView;
	name: string;
	description: string;
};

export type TradingNavGroup = {
	id: string;
	name: string;
	modules: TradingNavModule[];
};

export const tradingNavGroups: TradingNavGroup[] = [
	{
		id: 'status',
		name: '今日状态',
		modules: [
			{ id: 'overview', name: '账户总览', description: '资产、盈亏与待办' },
			{ id: 'market', name: '市场环境', description: '市场判定与仓位上限' },
		],
	},
	{
		id: 'trade',
		name: '交易执行',
		modules: [
			{ id: 'planning', name: '选股与计划', description: '生成、确认与历史计划' },
			{ id: 'positions', name: '持仓与挂单', description: '防守线与在途委托' },
		],
	},
	{
		id: 'run',
		name: '自动运行',
		modules: [
			{ id: 'autopilot', name: '自动运行', description: '启停、周期日志与时间线' },
		],
	},
	{
		id: 'learning',
		name: '复盘学习',
		modules: [
			{ id: 'performance', name: '业绩与校准', description: '已实现结果与命中率' },
			{ id: 'review', name: '复盘与记忆', description: '周期复盘与板块教训' },
		],
	},
	{
		id: 'ops',
		name: '运维保障',
		modules: [
			{ id: 'ops', name: '健康与告警', description: '自检、告警通道与日志' },
		],
	},
];

export const defaultTradingView: TradingView = 'overview';

// 旧版视图 ID 的兼容映射：历史计划并入选股与计划、时间线并入自动运行、记忆并入复盘。
const legacyViewAlias: Record<string, TradingView> = {
	plans: 'planning',
	timeline: 'autopilot',
	memory: 'review',
};

export function resolveTradingView(hash: string): TradingView {
	if (!hash.startsWith('#trading')) return defaultTradingView;
	const candidate = hash.replace(/^#trading\/?/, '').split(/[?&]/)[0];
	const aliased = legacyViewAlias[candidate] ?? (candidate as TradingView);
	return tradingNavGroups.some((group) => group.modules.some((module) => module.id === aliased))
		? aliased
		: defaultTradingView;
}

export function findTradingModule(view: TradingView): TradingNavModule {
	return tradingNavGroups.flatMap((group) => group.modules).find((module) => module.id === view)
		|| tradingNavGroups[0].modules[0];
}

export function findTradingGroupName(view: TradingView): string {
	return tradingNavGroups.find((group) => group.modules.some((module) => module.id === view))?.name || '';
}
