import { describe, expect, it } from 'vitest';
import { defaultTradingView, findTradingGroupName, findTradingModule, resolveTradingView, tradingNavGroups } from './trading-nav';

describe('trading navigation', () => {
	it('uses the approved product name and groups by workflow', () => {
		expect(tradingNavGroups.map((group) => group.name)).toEqual(['今日状态', '交易执行', '自动运行', '复盘学习', '运维保障']);
		const names = tradingNavGroups.flatMap((group) => group.modules.map((module) => module.name));
		expect(names).toContain('账户总览');
		expect(names).toContain('选股与计划');
		expect(names).toContain('持仓与挂单');
		expect(names).toContain('健康与告警');
		// 交易模块对用户的统一名称是「AI交易员」。
		expect(JSON.stringify(tradingNavGroups)).not.toContain('交易智能体');
	});

	it('covers every view exactly once with unique ids', () => {
		const ids = tradingNavGroups.flatMap((group) => group.modules.map((module) => module.id));
		expect(new Set(ids).size).toBe(ids.length);
		// 8 个视图：plans 并入 planning、timeline 并入 autopilot、memory 并入 review。
		expect(ids).toEqual(['overview', 'market', 'planning', 'positions', 'autopilot', 'performance', 'review', 'ops']);
	});

	it('resolves supported trading hashes and falls back to overview', () => {
		expect(resolveTradingView('#trading')).toBe('overview');
		expect(resolveTradingView('#trading/')).toBe('overview');
		expect(resolveTradingView('#trading/positions')).toBe('positions');
		expect(resolveTradingView('#trading/planning')).toBe('planning');
		expect(resolveTradingView('#trading/autopilot')).toBe('autopilot');
		expect(resolveTradingView('#trading/unknown')).toBe(defaultTradingView);
		// 其他工作区的 hash 不应解析出交易视图。
		expect(resolveTradingView('#market/pulse')).toBe(defaultTradingView);
	});

	it('maps legacy view ids to their merged homes', () => {
		expect(resolveTradingView('#trading/plans')).toBe('planning');
		expect(resolveTradingView('#trading/timeline')).toBe('autopilot');
		expect(resolveTradingView('#trading/memory')).toBe('review');
	});

	it('finds modules and group names', () => {
		expect(findTradingModule('positions').name).toBe('持仓与挂单');
		expect(findTradingGroupName('ops')).toBe('运维保障');
		expect(findTradingGroupName('overview')).toBe('今日状态');
	});
});
