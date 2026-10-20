// 交易智能体无人值守守护进程。
//
// 用法：
//   node scripts/trading-agent.mjs                # 前台守护（Ctrl+C 停止）
//   node scripts/trading-agent.mjs --no-build     # 跳过编译，直接守护现有二进制
//   node scripts/trading-agent.mjs --web          # 同时托管 frontend/dist，单端口访问
//
// 它做五件事，且只做这五件：
//   1. 编译后端（可用 --no-build 跳过）；
//   2. 启动并轮询健康接口，critical 视为不健康；
//   3. 进程退出或健康持续失败时自动重启，重启间隔指数退避，防止崩溃风暴；
//   4. 日志超过 5MB 轮转，只保留最近 3 份；
//   5. --web 时额外托管前端构建产物（只读静态文件）。
//
// 它刻意不做的事：不碰账本、不调交易接口。守护进程的权限应当越小越好，
// 一个能"顺手修账"的守护脚本本身就是新的故障源。
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const binary = path.join(repoRoot, 'backend', 'bin', 'easy-stock-server.exe');
const logDir = path.join(repoRoot, '.runtime');
const logFile = path.join(logDir, 'trading-agent.log');
const addr = process.env.A_STOCK_ADDR || '127.0.0.1:20081';
const healthURL = `http://${addr}/api/v1/trading/health`;
const distDir = path.join(repoRoot, 'frontend', 'dist');
const webAddr = process.env.A_STOCK_WEB_ADDR || '127.0.0.1:20073';

const noBuild = process.argv.includes('--no-build');
const serveWeb = process.argv.includes('--web');
const maxLogBytes = 5 * 1024 * 1024;
const maxRotations = 3;
const healthIntervalMS = 30_000;
const maxRestartDelayMS = 5 * 60_000;

if (!noBuild) {
	console.log('[agent] building backend...');
	const build = spawnSync('go', ['build', '-o', binary, './cmd/server'], {
		cwd: path.join(repoRoot, 'backend'),
		stdio: 'inherit',
		shell: process.platform === 'win32',
	});
	if (build.status !== 0) {
		console.error('[agent] build failed, aborting');
		process.exit(1);
	}
}

fs.mkdirSync(logDir, { recursive: true });

function rotateLogIfNeeded() {
	try {
		const stat = fs.statSync(logFile);
		if (stat.size < maxLogBytes) return;
		const stamp = new Date().toISOString().replace(/[:.]/g, '-');
		fs.renameSync(logFile, `${logFile}.${stamp}`);
		const rotations = fs
			.readdirSync(logDir)
			.filter((name) => name.startsWith('trading-agent.log.'))
			.sort()
			.reverse();
		for (const name of rotations.slice(maxRotations)) {
			fs.rmSync(path.join(logDir, name), { force: true });
		}
	} catch {
		// 轮转失败不影响运行：最坏情况是日志变大。
	}
}

function appendLog(line) {
	rotateLogIfNeeded();
	fs.appendFileSync(logFile, `${new Date().toISOString()} ${line}\n`);
}

let child = null;
let stopping = false;
let consecutiveFailures = 0;
let restartDelay = 5_000;

function startBackend() {
	child = spawn(binary, [], {
		cwd: repoRoot,
		env: { ...process.env, A_STOCK_ADDR: addr },
		stdio: ['ignore', fs.openSync(logFile, 'a'), fs.openSync(logFile, 'a')],
		windowsHide: true,
	});
	appendLog(`event=start pid=${child.pid} addr=${addr}`);
	child.on('exit', (code, signal) => {
		appendLog(`event=exit pid=${child?.pid} code=${code} signal=${signal}`);
		child = null;
		if (!stopping) scheduleRestart('进程退出');
	});
}

async function checkHealth() {
	try {
		const controller = new AbortController();
		const timer = setTimeout(() => controller.abort(), 8000);
		const response = await fetch(healthURL, { signal: controller.signal });
		clearTimeout(timer);
		if (response.status === 200) {
			consecutiveFailures = 0;
			restartDelay = 5_000;
			return;
		}
		// 503 = critical。连续多次 critical 说明系统进入坏状态，
		// 但进程还活着——重启不能修账，先记录，连续失败才重启。
		consecutiveFailures += 1;
		const body = await response.json().catch(() => null);
		const issues = body?.data?.issues?.map((issue) => issue.message).join('；') ?? '';
		appendLog(`event=unhealthy status=${response.status} failures=${consecutiveFailures} issues=${issues}`);
		if (consecutiveFailures >= 5) {
			consecutiveFailures = 0;
			appendLog('event=restart_for_unhealthy 连续 5 次不健康，重启进程');
			child?.kill();
		}
	} catch (error) {
		consecutiveFailures += 1;
		appendLog(`event=health_unreachable failures=${consecutiveFailures} err=${error.message}`);
		// 端口都不响应通常意味着进程已挂：exit 回调会触发重启，这里只计数。
	}
}

function scheduleRestart(reason) {
	if (stopping) return;
	appendLog(`event=restart_scheduled delay=${restartDelay} reason=${reason}`);
	setTimeout(() => {
		if (stopping) return;
		startBackend();
		restartDelay = Math.min(restartDelay * 2, maxRestartDelayMS);
	}, restartDelay);
}

console.log(`[agent] supervising ${binary} at ${addr}`);
console.log(`[agent] log: ${logFile}`);
startBackend();
const healthTimer = setInterval(() => void checkHealth(), healthIntervalMS);

// 静态托管是只读的：它只把 dist 里的文件发出去，不写盘、不代理、不改请求。
// 之所以不让它反向代理 API，是因为前端默认直接打到 20081，
// 多一层代理会多一个"到底哪层出错"的排查面。
function startWebServer() {
	const indexFile = path.join(distDir, 'index.html');
	if (!fs.existsSync(indexFile)) {
		console.error(`[agent] 未找到前端构建产物：${indexFile}`);
		console.error('[agent] 先执行 cd frontend && npm run build，或去掉 --web');
		return null;
	}
	const mime = {
		'.html': 'text/html; charset=utf-8',
		'.js': 'text/javascript; charset=utf-8',
		'.css': 'text/css; charset=utf-8',
		'.json': 'application/json; charset=utf-8',
		'.svg': 'image/svg+xml',
		'.png': 'image/png',
		'.jpg': 'image/jpeg',
		'.jpeg': 'image/jpeg',
		'.gif': 'image/gif',
		'.ico': 'image/x-icon',
		'.woff': 'font/woff',
		'.woff2': 'font/woff2',
		'.ttf': 'font/ttf',
		'.map': 'application/json; charset=utf-8',
	};
	const server = http.createServer((req, res) => {
		let urlPath;
		try {
			urlPath = decodeURIComponent((req.url || '/').split('?')[0]);
		} catch {
			res.writeHead(400).end('bad request');
			return;
		}
		let filePath = path.join(distDir, urlPath === '/' ? 'index.html' : urlPath);
		// 目录穿越防护：拼接后的路径必须仍在 dist 之内。
		if (!path.resolve(filePath).startsWith(path.resolve(distDir))) {
			res.writeHead(403).end('forbidden');
			return;
		}
		if (!fs.existsSync(filePath) || fs.statSync(filePath).isDirectory()) {
			// 单页应用：未知路径一律回 index.html，由前端路由处理。
			filePath = indexFile;
		}
		const ext = path.extname(filePath).toLowerCase();
		const immutable = urlPath.startsWith('/assets/');
		res.writeHead(200, {
			'Content-Type': mime[ext] || 'application/octet-stream',
			// assets 文件名带内容哈希，可以长期缓存；index.html 必须每次回源，
			// 否则升级后用户会一直拿到旧版本的入口。
			'Cache-Control': immutable ? 'public, max-age=31536000, immutable' : 'no-cache',
		});
		fs.createReadStream(filePath).pipe(res);
	});
	server.on('error', (error) => {
		console.error(`[agent] 前端托管失败：${error.message}`);
	});
	server.listen(Number(webAddr.split(':')[1] || 20073), webAddr.split(':')[0], () => {
		console.log(`[agent] web: http://${webAddr}/#trading`);
		appendLog(`event=web_start addr=${webAddr} dist=${distDir}`);
	});
	return server;
}

const webServer = serveWeb ? startWebServer() : null;

process.on('SIGINT', () => {
	stopping = true;
	clearInterval(healthTimer);
	appendLog('event=stop 收到停止信号');
	webServer?.close();
	child?.kill();
	process.exit(0);
});
