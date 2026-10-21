#!/usr/bin/env node
// 一次性修复：把历史持仓的建仓成本补上买入费用。
//
// 背景：ApplyFill 的首建路径曾把 average_cost 直接写成成交价，
// 买入费用只从现金里扣掉、没有摊进成本。结果是
// 「初始资金 + 已实现 + 浮动」比「账户权益」多出恰好一笔笔佣金（每只约 5.03 元）。
// 代码已改为成本含费；本脚本负责把已有持仓的存量数据拉回同一口径。
//
// 做法是从成交明细重放，算出含费成本，再覆盖 average_cost——
// 而不是"给每只加 5 元"：加过仓的持仓，其成本里已经含了那一次的费用，
// 一律加 5 元会重复计费。
//
// 只改 average_cost，不改 last_price（那是市价）。
// 已平仓记录一律不动：那是对已发生事实的记载，回头改它等于篡改历史。
//
// 用法：
//   node scripts/repair-entry-cost.mjs           # 干跑，只打印
//   node scripts/repair-entry-cost.mjs --apply   # 写入（自动备份）

import { DatabaseSync } from 'node:sqlite';
import { copyFileSync, existsSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..');
const dataDir = process.env.A_STOCK_DATA_DIR || join(process.env.APPDATA || '', 'easy-stock');
const dbPath = process.env.A_STOCK_DB || join(dataDir, 'trading.db');
const apply = process.argv.includes('--apply');

if (!existsSync(dbPath)) {
	console.error(`找不到交易库：${dbPath}`);
	process.exit(1);
}

const round4 = (value) => Math.round(value * 1e4) / 1e4;
const round2 = (value) => Math.round(value * 100) / 100;

const db = new DatabaseSync(dbPath);

// 先确认持仓成本确实与重放值有差，避免"改了个已经正确的东西"。
const fills = db.prepare('SELECT content_json FROM trading_fills ORDER BY created_at').all();
const positions = db
	.prepare('SELECT symbol, name, quantity, average_cost FROM trading_positions')
	.all();

// 按标的重放：买入累加（含费），卖出按比例释放成本、不改均价，清仓归零后重置。
const replay = new Map();
for (const row of fills) {
	let fill;
	try {
		fill = JSON.parse(row.content_json);
	} catch {
		continue;
	}
	const state = replay.get(fill.symbol) || { quantity: 0, cost: 0 };
	const quantity = Number(fill.quantity) || 0;
	const price = Number(fill.price) || 0;
	const fee = Number(fill.fee) || 0;
	if (fill.side === 'buy') {
		state.quantity += quantity;
		state.cost += quantity * price + fee;
	} else if (state.quantity > 0) {
		state.cost *= (state.quantity - quantity) / state.quantity;
		state.quantity -= quantity;
	}
	if (state.quantity <= 0) {
		state.quantity = 0;
		state.cost = 0;
	}
	replay.set(fill.symbol, state);
}

const plan = [];
let totalGap = 0;
for (const position of positions) {
	const state = replay.get(position.symbol);
	if (!state || state.quantity <= 0) {
		console.warn(`跳过 ${position.symbol}：成交明细重放后数量为 0，与持仓 ${position.quantity} 股不一致`);
		continue;
	}
	const cost = round4(state.cost / state.quantity);
	const gap = round2((cost - position.average_cost) * position.quantity);
	if (Math.abs(cost - position.average_cost) < 1e-6) continue;
	totalGap += gap;
	plan.push({ ...position, nextCost: cost, gap });
}

console.log(`交易库：${dbPath}`);
console.log(`持仓 ${positions.length} 只，需修正 ${plan.length} 只，合计补计买入费 ${round2(totalGap)} 元\n`);
for (const item of plan) {
	console.log(
		`  ${item.symbol} ${item.name || ''} ${String(item.quantity).padStart(5)} 股  ` +
			`${item.average_cost.toFixed(4)} → ${item.nextCost.toFixed(4)}  (+${item.gap.toFixed(2)})`
	);
}

if (plan.length === 0) {
	console.log('\n无需修正。');
	db.close();
	process.exit(0);
}

if (!apply) {
	console.log('\n干跑结束。加 --apply 写入（会先备份）。');
	db.close();
	process.exit(0);
}

const backupDir = join(root, '.runtime', 'backup');
mkdirSync(backupDir, { recursive: true });
const stamp = new Date().toISOString().replace(/[:.]/g, '-');
const backupPath = join(backupDir, `trading-before-entry-cost-${stamp}.db`);
copyFileSync(dbPath, backupPath);
console.log(`\n已备份：${backupPath}`);

const update = db.prepare('UPDATE trading_positions SET average_cost=? WHERE symbol=?');
db.exec('BEGIN');
try {
	for (const item of plan) update.run(item.nextCost, item.symbol);
	db.exec('COMMIT');
} catch (error) {
	db.exec('ROLLBACK');
	console.error(`写入失败，已回滚：${error.message}`);
	db.close();
	process.exit(1);
}

const after = db
	.prepare('SELECT SUM(quantity*average_cost) AS cost FROM trading_positions')
	.get();
console.log(`写入完成。持仓成本合计 ${round2(after.cost)} 元`);
db.close();
