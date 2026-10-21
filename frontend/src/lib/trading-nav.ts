// AI 交易员的导航信息架构：左侧分组导航 + 右侧视图区，与行情总览同一套布局语言。
// 分组按使用流程划分：先看状态，再交易，然后看自动运行，最后复盘与运维。

export type TradingView =
	| 'overview'
	| 'market'
	| 'planning'
	| 'positions'
	| 'plans'
	| 'autopilot'
	| 'timeline'
	| 'performance'
	| 'review'
	| 'memory'
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
			{ id: 'overview', name: '账户总览', description: '资产、盈亏口径与健康状态' },
			{ id: 'market', name: '市场环境', description: '市场状态判定，规则与模型共用' },
		],
	},
	{
		id: 'trade',
		name: '交易执行',
		modules: [
			{ id: 'planning', name: '选股与计划', description: '选股池、生成计划与逐笔确认' },
			{ id: 'positions', name: '持仓与挂单', description: '防守线、冻结与复核痕迹' },
			{ id: 'plans', name: '历史计划', description: '最近计划回看与重新打开' },
		],
	},
	{
		id: 'run',
		name: '自动运行',
		modules: [
			{ id: 'autopilot', name: '运行控制', description: '启停、调度、熔断与回撤释放' },
			{ id: 'timeline', name: '决策时间线', description: '每轮决策留痕，按日回放' },
		],
	},
	{
		id: 'learning',
		name: '复盘学习',
		modules: [
			{ id: 'performance', name: '业绩与校准', description: '已实现结果与预测命中率' },
			{ id: 'review', name: '交易复盘', description: '周期复盘与保护性动作' },
			{ id: 'memory', name: '经验记忆', description: '跨轮次教训与同步状态' },
		],
	},
	{
		id: 'ops',
		name: '运维保障',
		modules: [
			{ id: 'ops', name: '健康与告警', description: '健康检查、告警通道与运维日志' },
		],
	},
];

export const defaultTradingView: TradingView = 'overview';

export function resolveTradingView(hash: string): TradingView {
	if (!hash.startsWith('#trading')) return defaultTradingView;
	const candidate = hash.replace(/^#trading\/?/, '').split(/[?&]/)[0] as TradingView;
	return tradingNavGroups.some((group) => group.modules.some((module) => module.id === candidate))
		? candidate
		: defaultTradingView;
}

export function findTradingModule(view: TradingView): TradingNavModule {
	return tradingNavGroups.flatMap((group) => group.modules).find((module) => module.id === view)
		|| tradingNavGroups[0].modules[0];
}

export function findTradingGroupName(view: TradingView): string {
	return tradingNavGroups.find((group) => group.modules.some((module) => module.id === view))?.name || '';
}
