# AI 交易员编写方案（P2 施工版）

> 目标：在 `easy-stock` 内建成一个可运行的 AI 交易员闭环——自动看市场、自动选股、自动买卖、自动复盘、自动积累经验，全过程可回放、账本可自证。
>
> 本文是施工图：包结构、文件清单、数据表、接口、状态机、实施顺序、验收用例。设计依据见 `docs/trading-agent-architecture.md`，外部项目结论见 `docs/research/`。

## 一、范围与不可让步的约束

**P2 交付**：3 分钟一轮（事件可提前）+ 全市场筛选前 30 + 全自动执行（含双熔断）+ 决策日志可回放。

**四条硬约束**：

1. 模型不做取数、不做计算、不做风控判定；只做"在候选池内排序并给出理由"。
2. 账本只由代码写；每笔成交必须能回溯到 `cycle_id` 与 `decision_id`。
3. 任何一轮循环缺字段（市场快照/候选池/prompt hash/模型原文/裁决）即视为 `incomplete`，不得执行。
4. 默认只跑模拟盘；实盘通道未实现，调用即报错。

## 二、全市场筛选的现实约束（必须先解决）

现有 `RealtimeProvider.Realtime(ctx, symbols)` 一次请求几百只尚可，**5000 只 × 每 3 分钟绝不可行**。因此"全市场"分两级：

```text
每 30 分钟（或每日盘前）：全市场基础池刷新
  数据：全市场列表 + 日线（20 日成交额、5/20 日涨幅、换手、是否 ST/停牌/次新）
  产出：可交易基础池（约 800–1500 只），落库缓存
        ↓
每 3 分钟：只对「预筛前 60」拉实时行情
  预筛：基础池内按 量比/相对强度/板块强度/距 20 日线 打分取前 60
  实时：60 只（含持仓）分批拉取，超时降级
        ↓
候选池 30 只交给模型（持仓与关注池强制保留）
```

这条链路决定了 P2 的性能上限，也是必须先在代码里写死的东西。数据新鲜度不达标（行情时间戳超过 60 秒）→ 只允许卖，不允许买。

## 三、包结构

保持"域核心 + 能力子包"，避免再长出一个 3000 行的单体文件。

```text
backend/internal/trading/                 # 域核心（已有，扩展）
  types.go        领域模型 + 新增 Cycle/Signal/Verdict/Audit
  store.go        SQLite 账本 + 迁移（含 reserved_cash、decision_id）
  ledger.go       记账原语：Reserve/Release/ApplyFill/Reconcile
  broker.go       Broker 接口 + PaperBroker（已有）
  market.go       涨跌停/整手/费用（已有）
  risk.go         下单前校验（已有，改为调用 policy）
  service.go      对外 API 编排（已有，收敛）

backend/internal/trading/cognition/       # 市场认知（确定性）
  view.go         MarketView 结构与计算
  emotion.go      复用 marketemotion 快照 → 阶段/宽度
  trend.go        指数趋势、板块强度 Top5
  freshness.go    数据新鲜度判定

backend/internal/trading/universe/        # 选股
  base_pool.go    全市场基础池（30 分钟缓存）
  score.go        打分：量比/相对强度/板块/位置
  build.go        预筛 60 → 候选 30，记录入选/剔除理由

backend/internal/trading/decide/          # 模型决策
  prompt.go       提示词（市场事实 + 候选池 + 持仓 + 经验摘要）
  decode.go       JSON 提取（已有逻辑迁入）
  guard.go        输出守卫：schema/枚举/权重/标的格式/越权字段
  client.go       调用 Agent Service，记录 token/耗时/模型

backend/internal/trading/policy/          # 风控裁决（确定性）
  gate.go         S 级一票否决 + A 级仓位/次数 + B 级建议
  limits.go       硬限制：单票/总仓/日亏/回撤/流动性/白名单
  position.go     防守线、分级止盈、冷却期
  verdict.go      逐条裁决与理由

backend/internal/trading/loop/            # 循环编排
  cycle.go        状态机 + 单飞 + 幂等 + 崩溃恢复
  scheduler.go    盘前/盘中/盘后相位、3 分钟基线、事件触发、补跑
  auto.go         自动执行开关、pause_buying、kill_switch、熔断
  reconcile.go    每日账本对账

backend/internal/trading/review/          # 复盘与经验
  facts.go        代码算事实（盈亏/持仓时长/最大浮盈回撤/防守线）
  review.go       收盘复盘（AI 归类，禁止编数字）
  experience.go   经验库读写、维度枚举、样本下限、幂等写入

backend/internal/httpapi/trading.go       # 扩展：cycles 查询、开关、熔断、复盘
frontend/src/components/trading/          # 拆分：账户/计划/持仓/日志/设置
```

依赖方向单向：`loop → policy/universe/decide/cognition → trading(核心)`；核心不反向依赖任何子包。

## 四、数据模型

沿用 `trading.db`，新增与扩展如下（迁移用 `PRAGMA table_info` 判断，幂等）。

```sql
-- 账户：新增冻结资金
ALTER TABLE trading_accounts ADD COLUMN reserved_cash REAL NOT NULL DEFAULT 0;

-- 持仓：防守线、冷却、信念、状态
ALTER TABLE trading_positions ADD COLUMN highest_price REAL NOT NULL DEFAULT 0;
ALTER TABLE trading_positions ADD COLUMN risk_line REAL NOT NULL DEFAULT 0;
ALTER TABLE trading_positions ADD COLUMN thesis_type TEXT NOT NULL DEFAULT '';
ALTER TABLE trading_positions ADD COLUMN thesis_status TEXT NOT NULL DEFAULT 'ACTIVE'; -- ACTIVE|WEAKENING|BROKEN
ALTER TABLE trading_positions ADD COLUMN conviction INTEGER NOT NULL DEFAULT 50;
ALTER TABLE trading_positions ADD COLUMN state TEXT NOT NULL DEFAULT 'ACTIVE';          -- BUILDING|ACTIVE|COOLDOWN
ALTER TABLE trading_positions ADD COLUMN opened_at TEXT NOT NULL DEFAULT '';
ALTER TABLE trading_positions ADD COLUMN cooldown_until TEXT NOT NULL DEFAULT '';

-- 订单：决策关联与冻结
ALTER TABLE trading_orders ADD COLUMN cycle_id TEXT NOT NULL DEFAULT '';
ALTER TABLE trading_orders ADD COLUMN decision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE trading_orders ADD COLUMN reserved_cash REAL NOT NULL DEFAULT 0;

-- 成交：决策关联
ALTER TABLE trading_fills ADD COLUMN cycle_id TEXT NOT NULL DEFAULT '';
ALTER TABLE trading_fills ADD COLUMN decision_id TEXT NOT NULL DEFAULT '';

CREATE TABLE trading_cycles (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  phase TEXT NOT NULL,                -- observing|screening|deciding|judging|executing|settled|error|interrupted
  trigger TEXT NOT NULL,              -- schedule|event|manual|catch_up
  market_json TEXT NOT NULL,
  universe_json TEXT NOT NULL,
  prompt_hash TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL DEFAULT '',
  ai_raw TEXT NOT NULL DEFAULT '',
  signals_json TEXT NOT NULL,
  verdicts_json TEXT NOT NULL,
  orders_json TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_cycles_account_time ON trading_cycles(account_id, started_at DESC);

CREATE TABLE trading_state (
  account_id TEXT PRIMARY KEY,
  auto_trade_enabled INTEGER NOT NULL DEFAULT 0,
  pause_buying INTEGER NOT NULL DEFAULT 0,
  kill_switch INTEGER NOT NULL DEFAULT 0,
  halt_reason TEXT NOT NULL DEFAULT '',
  daily_loss_percent REAL NOT NULL DEFAULT 0,
  consecutive_stops INTEGER NOT NULL DEFAULT 0,
  last_cycle_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE TABLE trading_experience (
  id TEXT PRIMARY KEY,
  trade_date TEXT NOT NULL,
  thesis_type TEXT NOT NULL,          -- 枚举，禁止自由文本
  market_phase TEXT NOT NULL,
  board TEXT NOT NULL DEFAULT '',
  trades INTEGER NOT NULL,
  wins INTEGER NOT NULL,
  avg_pnl REAL NOT NULL,
  avg_holding_minutes INTEGER NOT NULL,
  mistake_code TEXT NOT NULL DEFAULT '',
  rule_text TEXT NOT NULL DEFAULT '',
  importance REAL NOT NULL DEFAULT 0,
  expires_at TEXT NOT NULL DEFAULT '',
  UNIQUE(trade_date, thesis_type, market_phase)
);

CREATE TABLE trading_reviews (
  trade_date TEXT PRIMARY KEY,
  facts_json TEXT NOT NULL,
  ai_json TEXT NOT NULL,
  reconcile_ok INTEGER NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE trading_audit (
  id TEXT PRIMARY KEY,
  at TEXT NOT NULL,
  actor TEXT NOT NULL,                -- user|system
  action TEXT NOT NULL,               -- enable_auto|disable_auto|pause_buying|resume|kill|param_change|reconcile_fail
  target TEXT NOT NULL DEFAULT '',
  detail_json TEXT NOT NULL DEFAULT '{}'
);
```

## 五、状态机

**Cycle**：`observing → screening → deciding → judging → executing → settled`，任一步失败 `error`；启动时把非终态标记 `interrupted`，不自动续跑。

**Order**：`pending → filled | partially_filled | cancelled | rejected`；同一 `cycle_id + symbol + side` 唯一，防重复下单。

**Position**：`BUILDING`（建仓 30 分钟内禁卖）→ `ACTIVE`；清仓后写 `COOLDOWN`（默认 30 分钟，可配置）到 `trading_state` 关联表，冷却期内禁止再买同一标的。

**门控优先级**：`kill_switch` > 交易时段 > 数据新鲜度 > `pause_buying`（只拦买）> S 级 > A 级 > 模型建议。

## 六、核心接口（签名先定，实现分批填）

```go
// cognition
type Observer interface {
    Observe(ctx context.Context, accountID string) (MarketView, error)
}

// universe
type Screener interface {
    Build(ctx context.Context, view MarketView, snapshot AccountSnapshot) (Universe, error)
}

// decide
type Decider interface {
    Decide(ctx context.Context, in DecisionInput) (Decision, error) // 含 raw/hash/usage
}

// policy
type Judge interface {
    Judge(ctx context.Context, in DecisionInput, signals []Signal) ([]Verdict, error)
}

// executor（已有 Broker，包一层）
type Executor interface {
    Execute(ctx context.Context, cycleID string, orders []Order) ([]Order, error)
}

// loop
type CycleRunner interface {
    Run(ctx context.Context, trigger string) (Cycle, error)
}
```

`DecisionInput` 只包含事实：`MarketView`、`Universe`、`Account`、`Positions`、`Rules`、`ExperienceDigest`、`Now`。模型拿不到任何可写句柄。

## 七、一轮循环的时序

```text
1  preflight
   kill_switch? 交易时段? 数据新鲜? 当日熔断? 账本已对账?  → 失败即退出并记录原因
2  observe    拉指数/情绪/宽度/涨跌停/板块 → MarketView（落库）
3  screen     基础池(缓存)→预筛60→实时行情→候选30（落库入选/剔除理由）
4  manage     先处理持仓：防守线、止盈、硬止损 → 生成强制卖出信号（不经过模型）
5  decide     模型在候选30+持仓中输出信号（JSON）→ guard 校验 → 落库 raw/hash/token
6  judge      S级否决/覆盖 → A级裁剪 → B级建议 → 逐条 Verdict
7  execute    reserve 资金 → broker 撮合 → applyFill → 释放/扣减 → 落库
8  settle     汇总本轮盈亏、更新防守线、写 audit
```

第 4 步先于第 5 步是刻意的：**该止损的不能等模型同意**。

## 八、实施顺序（每步都可独立验收）

### P2.1 账本加固（先做，1 个可验证版本）

- 迁移：`reserved_cash`、`decision_id`、`cycle_id`；
- `ledger.go`：`Reserve`（下单冻结）、`Release`（撤单/拒绝释放）、`ApplyFill`（冻结转实扣）、`Reconcile`（按 fills 重放含费用）；
- 门控拆分：`pause_buying` / `kill_switch` 写入 `trading_state`；
- 接口：`GET/PUT /api/v1/trading/state`、`POST /api/v1/trading/reconcile`；
- **验收**：两笔挂单共享 10 万资金，第二笔不得超额；撤单后资金释放；人为改账后 reconcile 报差异 >0.01 并置 `pause_buying`。

### P2.2 市场认知 + 候选池（确定性，不调模型）

- `cognition`：复用 `marketemotion` 与指数/板块数据；
- `universe`：基础池 30 分钟缓存 + 预筛 60 + 实时 60 → 候选 30；
- 落库 `MarketView` / `Universe`，含每只的入选与剔除理由；
- **验收**：给定固定行情 fixture，输出稳定可复现；单轮耗时 < 800ms（不含上游网络）；缺数据时返回 `degraded` 而非空池。

### P2.3 决策 + 守卫 + 裁决（接模型，但不自动下单）

- `decide`：提示词、调用、解析、`guard`；
- `policy`：S/A/B 分级 + 硬限制 + 逐条 Verdict；
- **验收**：模型输出池外标的/超权重/无止损 → 被 guard 或 judge 拒绝并留原因；模型超时 → 整轮 `error`，不下单；token 与耗时入库。

**已完成**（实现细节见 `docs/trading-agent-decision.md`）：

- `policy`：`Limits`（含行业上限 45% / 同行业 3 只）、`Verdict`（code / level / scope / metric / limit）、
  `Book`（组合暴露累加器）、`CheckStopLoss` / `CheckTradeRisk` / `CheckCashFloor`。
  `RiskRules` 改为 `policy.Limits` 的别名，提示词与校验共用同一份数字。
- `decide`：`Decide()` 按"总闸 → 市场 → 逐笔 → 组合"顺序执行，输出 `Decision`（Proposal / Verdicts /
  Stats / Exposure / Narrative / Model）。
- 持久化：`trading_decisions` 只增不改；`settleDecision` 用落库后的订单回填统计，
  账本拒绝的订单补 `ledger.reserve_failed`。
- 接口：`GET /api/v1/trading/decisions`（支持 `plan_id`）、`GET /api/v1/trading/decisions/{id}`。
- 前端：计划详情新增裁决面板（汇总条 / 仓位进度 / 行业分布 / 逐条裁决）。
- 验收记录：6 笔全通过（55.14% 仓位、6 个行业各 1 只）；暂停买入下 5 笔全部被剔除且未误报
  `order.empty`；`token` 与耗时写入 `decision.model`；账本 `drift=0`。

### P2.4 循环 + 自动执行 + 熔断

- `loop`：状态机、单飞、幂等、崩溃恢复、3 分钟调度 + 事件触发 + 盘前补跑；
- `auto`：开关、熔断（日亏 3%、连续 3 笔止损）、14:50 后禁开仓；
- **验收**：关闭开关后跑 3 轮不产生订单；熔断后当轮只卖不买；进程重启不重复下单；同一 cycle 重入不重复扣款。

**已完成**（实现细节见 `docs/trading-agent-loop.md`）：

- `cycle.go` / `cycle_store.go`：`Cycle`（trigger / status / gates / 权益前后 / 建议·成交·拦截数）、
  `DayState`（当日基准权益、连败计数、熔断事实）、`AutopilotConfig`；三张表 `trading_cycles` /
  `trading_day_state` / `trading_autopilot`，与计划、订单同库，便于联表复盘。
- `autopilot.go`：15 秒检查一次节拍（固定 3 分钟定时器会让开盘首轮最多晚 3 分钟）；
  闸门顺序「开关 → 时段 → 总闸 → 轮次配额 → 熔断 → 尾盘」，每道闸门写入 `Cycle.Gates`。
- 熔断：日亏按**当日基准权益**计算，触发同时写 `DayState`、`trading_control.pause_buying` 与本轮记录；
  当日基准只写一次（可刷新就等于永不触发）。`stop_streak` 标记为未装弹（P3 才有止损成交）。
- 恢复：启动时 `AbortRunningCycles` + `AbortRunningPlans`，把上次进程留下的 running 记录收尾。
- 单飞：`Service.beginRun` 供人工与自动共用，第二个调用返回 `ErrCycleRunning`（HTTP 409）。
- 自动执行：`auto_execute=true` 时计划生成后立即 `Execute`；关闭时停在 draft，等于「自动思考、人工扣扳机」。
- 接口：`GET/POST /api/v1/trading/autopilot`、`POST .../autopilot/breaker/reset`、
  `GET /api/v1/trading/cycles`、`POST /api/v1/trading/cycles/run`。
- 前端：自动循环面板（双开关、间隔、日亏阈值、当日基准/轮次/下一轮、熔断器清单）+ 交易员日志卡片
  （状态、时长、权益前后、建议/成交/拦截、闸门 chips、备注、跳转计划）。
- 验收记录：关闭开关连跑 4 轮零计划；人工触发 5 笔全部成交；阈值临时调至 0.02% 时真实权益 -0.04% 触发
  `daily_loss`，4 笔买入被整批剔除且日志写明原因；无理由解除熔断 HTTP 400；账本 `drift=0`。
- 单测 8 个：开关、人工越过开关但越不过总闸、熔断落库并暂停买入、解除需理由、计划与循环互指、
  尾盘闸门、单飞、崩溃恢复。

### P2.5 可观察 + 复盘骨架

- 前端「交易员日志」：cycle 卡片可展开看五段（市场/候选/模型原文/裁决/成交），支持按日期回放；
- `review`：收盘事实计算 + AI 归类（数字由代码给）+ 经验写入（枚举维度、样本下限 30、幂等）；
- **验收**：任意一笔成交可在 3 次点击内回到所属 cycle 与 decision；复盘数字与账本一致；样本 <30 的维度不显示胜率。

**可观察部分已完成**：

- 交易员日志卡片：状态 / 触发方式 / 耗时 / 权益前后 / 建议·成交·拦截·在途撮合 / 闸门 chips / 备注；
- 「回放」展开五段：① 市场（状态、预算、广度）② 候选池（数量、评分前五、缩量降级）③ 模型（状态、自述、原始建议列表、耗时与 token）
  ④ 裁决（建议/通过/拦截/调整 + 拦截条目与代码）⑤ 成交（订单状态与拒绝原因）；
- 数据全部取自已落库的 plan 与 decision，展开时才拉取，不在列表页预加载；
- 验收：任一轮 → 回放（1 次点击）→ 看到裁决与订单；任一轮 → 查看计划（1 次点击）→ 计划详情含裁决面板。

**待做**：`review`（收盘事实、AI 归类、经验写入）、按日期回放。

## 八·二、P3 + P4：持仓防守线与复盘（已交付）

两个阶段合并交付：防守线产生卖出成交，卖出成交才是复盘的原子事实。
详见 `docs/trading-agent-guard-review.md`。

### P3 持仓管理

- 账本第三条不变量：`position.reserved_quantity == Σ 未成交卖单剩余股数`。
  `Sellable() = available_quantity - reserved_quantity` 成为唯一可卖口径，
  风控/提示词/防守线共用，修复了"两笔卖单各卖一次"的超卖漏洞。
- `guard.go`：确定性防守线，顺序为 止损 → 硬止损 → 分级止盈 → 移动止损，
  命中即离场；不看市场状态、不看 `pause_buying`、不调模型，只看 `kill_switch`。
- 止盈成交后 `reduce_count+1` 且止损上移到成本价（把剩余仓位变成不会亏的交易）。
- `GuardConfig`：移动止损回撤 6%、启动门槛浮盈 4%、止盈阶梯 40/40。
- 连败熔断接上弹：保护性止损亏损 `stop_streak+1`（原子加减），盈利归零，3 笔触发熔断。
- 触发但 T+1 不可卖时记 `guard.no_sellable`，`Triggered` 与 `Created` 分开统计。
- 接口：`GET /positions`、`POST /positions/{symbol}/protection`、`POST /protective/run`。

### P4 复盘

- 卖出成交写 `ClosedTrade`（逐笔，不合并），`PnL` 与账本口径一致。
- `ReviewFacts` 全部由代码计算：胜率、盈亏比、平均盈亏、持有天数、
  按离场原因分类、裁决拦截分布、循环状态、模型成本。
- `Lesson.WinRate` 为指针：样本 < 30 时字段不出现，文案明说"样本不足"。
  维度只保留「离场原因」「行业」两类。
- `trading_reviews` 主键 `(account_id, trading_day)`，重算覆盖不新增；
  自动生成时机为收盘后 15:05 起，每天一次。
- 经验回流提示词：最近 30 个交易日、样本量前 8 条，附一句
  "经验不构成对本轮的指令，止损纪律不因历史统计而放宽"。
- 接口：`GET /closed-trades`、`GET /reviews`、`POST /reviews/run`、`GET /reviews/{day}`。

### 验收记录

- 7 只真实持仓写入防守线；把 301487.SZ 止损临时设到现价上方 →
  `triggered=1 created=0`，note 写"已触发止损，但可卖 0 股不足一手"，**没有**伪造成交。
- 真实复盘：建议 42 / 通过 27 / 拦截 15，拦截分布 `sizing.below_lot×3`、
  `order.buy_dropped×3`、`gate.breaker×2`、`sizing.scaled×1`；循环 4 轮全完成；
  权益 100000 → 99964.49（−0.04%）；当日 0 平仓，摘要明说"没有可归因的交易结果"。
- 新一轮真实计划：5 笔建议、3 笔通过（2 笔科创板一手 200 股买不起被拒），
  3 笔均带止损/止盈且 `source=model`；据此在提示词补第 11 条科创板手数约束。
- 对账 `drift=0`、`reserved_quantity=0`；新增 9 个单测全绿。

## 八·三、P5：交易日历、日终委托作废与业绩归因（已交付）

P1–P4 把"一轮"做完整了，P5 解决"跨日与长期"：委托的生命周期边界、
非交易日的调度、以及"这套系统到底赚不赚钱"的度量。

### 交易日历

- `marketrule.IsTradingDay` 接入 `foundation.IsAStockTradingDay`
  （周末 + 法定节假日表，与行情新鲜度、复盘共用同一份事实）；
- `IsTradingSession` 原来不判节假日，国庆春节会每 15 秒跑一轮循环、
  调一次模型——已修复；
- 复盘生成跳过非交易日：假期没有成交与行情，算出来的只是零值噪声。

### 日终委托作废（账本正确性修复）

- A 股委托当日有效、收盘即废。P1–P4 没有这一步：未成交的买单会
  把 `reserved_cash` **永久**锁住，第二天开仓被误判"资金不足"；
  卖单同理锁住股数。
- `OrderStatusExpired`（`expired`）与 `Store.ExpireStaleOrders(cutoff)`：
  事务内对 `cutoff`（`marketrule.LastSessionClose` = 最近一个已收盘
  交易日的 15:00）之前仍未收口的委托作废并释放冻结；状态更新用条件
  UPDATE 抢占，重启补跑与并发触发都不会二次释放。
- 调度：autopilot `tick` 在所有开关判断之前执行（账本卫生与是否开自动
  无关），`Start` 时补跑一次，覆盖进程停机期间跨过的收盘；失败不推进
  `lastExpiry`，下一轮 tick 重试。
- `cancelled` 与 `expired` 语义分开：前者是人主动撤单，后者是市场规则
  到期。复盘统计"为什么没成交"时两者的归因完全不同。

### 业绩归因

- `Performance`（`GET /trading/performance?days=30`）：基于
  `ClosedTrade` 计算胜率、盈亏比（总盈利÷总亏损）、单笔期望值、
  最好/最差单笔、平均持有天数、按离场原因分布、逐日已实现曲线
  （含累计）、按标的与按离场原因/行业分桶（复用复盘的
  `SummarizeTrades` / `BuildLessons`，保证两个页面口径一致）。
- 只统计已实现平仓；未实现浮盈不进胜率。样本 < 30 笔时前端明示
  "还看不出来"，不把 3 笔 2 胜说成 67%。
- 前端 `PerformancePanel`：四张核心卡片（已实现盈亏/胜率/盈亏比/
  期望值）+ 逐日柱状图（红涨绿跌）+ 按标的与原因分组明细。

### 前端

- 订单状态新增 `expired`（文案"收盘作废"）。

### 验收记录

- 单测：`LastSessionClose` 六个边界（盘中/收盘后/周一早/节假日两侧）
  全过；买单作废释放资金且幂等；当日单不被误作废；卖单作废释放股数
  且可卖恢复；业绩聚合 3 笔样本（胜率 66.67 / PF 3 / 期望 266.67 /
  累计曲线 1000→800）与空账本两条全过。
- `go test ./internal/trading ./internal/httpapi` 全绿；`tsc --noEmit` 通过。
- 真实接口 `GET /performance?days=30` 返回 0 平仓（当日建仓 T+1 未可卖），
  前端空态文案正确渲染；面板已上页面（历史计划与复盘之间）。

## 九、测试矩阵

| 层 | 用例 |
| --- | --- |
| 账本 | 冻结/释放/部分成交/撤单/并发两笔买单/费用精度/重放对账 |
| 风控 | 涨跌停边界、T+1、单票超限、总仓超限、日亏熔断、连续止损熔断、冷却期 |
| 守卫 | 池外标的、非法枚举、权重和 >100、缺止损、超长文本、fenced JSON |
| 调度 | 非交易时段、午休、14:50 边界、数据过期、盘前补跑、崩溃恢复 |
| 幂等 | 同 cycle 重复执行、同 symbol 重复下单、复盘重复写入 |
| 端到端 | 假行情 + 假模型：一轮完整 cycle 落库并可回放 |

单测覆盖核心包；端到端用假 provider，不依赖外网与真实模型。

## 十、资源与成本预算

- 行情：基础池 30 分钟一次（全市场日线/列表），实时 60 只 × 3 分钟；
- 模型：3 分钟一轮 ≈ 每日 30–40 次；单次输入约 4–6k token（市场+候选30+持仓），输出 ≤1k；
- 存储：每轮 cycle 约 20–40KB，按 240 轮/日、250 日 ≈ 1.5–2.4GB/年；需按月归档或只保留摘要 + 原始 JSON 压缩；
- 内存：循环单飞，无长驻协程池；调度器单 goroutine + 定时器。

## 十一、风险与回滚

| 风险 | 处置 |
| --- | --- |
| 行情接口降级 | 只卖不买；连续 3 轮降级自动 `pause_buying` |
| 模型输出异常 | guard 拒绝 + 整轮不下单；连续 3 轮失败转人工 |
| 账本不一致 | reconcile 失败即 `pause_buying` + 红色告警，需人工确认解除 |
| 重复下单 | 订单唯一键 + cycle 状态机；重启只恢复 `settled/error`，不续跑 |
| 参数误调 | 所有开关与阈值变更写 `trading_audit`，页面可查 |

回滚：新表与新增列均为增量，旧版本读旧列不受影响；功能开关 `auto_trade_enabled` 默认关闭，出问题直接关开关即可停止一切下单。

## 十二、明确不做（P2）

- 实盘通道、券商对接；
- 因子挖掘与多策略回测（P4 之后再评估）；
- 多 Agent 辩论（成本与延迟不匹配 3 分钟循环）；
- 训练/微调模型；
- 移动端。

## 十三、下一步

先做 **P2.1 账本加固**：它是自动交易的地基，也是唯一一处"做错了会真的算错钱"的地方。完成后用两笔挂单共享资金的场景验收，再往上叠认知与选股。

## 八·四、无人值守交付版（已交付，2026-10-20）

在 P5 之上补齐"没有人盯着时系统自己扛得住"的三层能力。运行手册见 `docs/trading-agent-delivery.md`。

### 1. 运行事件总线（events.go）

- 新表 `trading_events`：severity（info/warning/critical）、kind（稳定标识，如 `breaker.armed`、`ledger.drift_healed`）、message、payload。
- 关键节点全部落事件：熔断装弹/解除、降级/恢复、账本漂移/修复、日终作废、通道错误、矛盾回报。
- 接口 `GET /api/v1/trading/events?severity=&kind=&limit=`。
- 设计立场：事件写失败不中断主流程（可观测性不能凌驾于正确性），但丢的事件会留在进程日志里。

### 2. 健康自检（health.go）

- `GET /api/v1/trading/health`：healthy/warning/critical 三档 + issues 列表；critical 返回 HTTP 503，外部监控只看状态码。
- 采集只读库不拉行情：账本 drift、股数漂移、孤儿挂单、挂单积压、熔断、心跳、连续失败、降级状态、uptime。
- 判定阈值全部常量化（drift 容差 1 分、心跳超时 10 分钟、连续失败 3 轮、挂单 10 笔）。
- 刻意不显示盈亏：盈亏是结果，健康是前提，两者混排会让人在赚钱时忽略"循环已停"。

### 3. 自愈与降级（autopilot.go）

- 每 5 分钟自动对账（repair 模式），修复成功记 `ledger.drift_healed`，失败记 critical `ledger.drift`；对账报告新增 `drift_before` 保留修复前差额。
- 连续 3 轮失败自动降级：`pause_buying=true` + critical 事件；**保护性卖出不受影响**。连续 3 轮成功自动解除（无人值守时没人来点恢复，但门槛必须够高）。
- 失败中途成功一次即清零计数：通道是通的就不该继续按连续故障处理。
- 降级前检查 kill_switch：人已全停时不再叠加降级状态。

### 4. 通道契约与异步回报（report.go、broker.go）

- `CancelableChannel` 接口：撤单是独立能力，模拟盘不强制实现（本地撤销即全部撤销）。
- 委托回报 `POST /api/v1/trading/reports`，`BrokerReport` 以**增量**成交为准；`trading_broker_reports` 表 + UPSERT 抢占保证幂等：
  - 重复投递 → 幂等丢弃；
  - 处理中崩溃 → 2 分钟后可重新接管，不且回报；
  - 落账失败 → 释放抢占位允许重试。
- 超量回报按本地剩余截断 + critical 事件；已收口订单收到成交回报不改账只报警。
- 撤单退避重试（200ms/400ms/800ms）；通道明确拒绝（如已成交）不重试。撤单失败时本地订单保持挂起、冻结不变——假装撤掉会造成超卖。
- **日终作废改为先撤通道**：撤单失败的委托原地保留，宁可多冻一天，也不记一笔假的"已失效"。
- 撤单本地收口改为 `CloseOrder`：状态变更与冻结释放同事务，修掉旧路径"状态已改、释放失败"的中间态。
- `GET /api/v1/trading/channel` 返回通道能力（live / supports_cancel），live 的判断在后端，前端不会因配置拼错而误标实盘。

### 5. 前端

- 顶部 HealthBar：三色状态 + 心跳/挂单/账本/连败摘要 + 问题标签 + 重新自检。
- OpsLog 运行事件面板：时间、等级、内容、kind 四列，critical/warning 着色。

### 6. 守护脚本（scripts/trading-agent.mjs）

编译 → 启动 → 30 秒健康轮询 → 崩溃自动重启（指数退避至多 5 分钟）→ 连续 5 次不健康重启 → 日志轮转（5MB×3）。守护进程不碰账本不调交易接口。

### 7. 测试

新增 16 个单测：事件过滤与排序、健康三档判定（漂移/心跳/干净账本）、降级触发与自动恢复、成功清零、自愈修复留痕、回报幂等/部分成交累加/超量截断/矛盾回报拒账、撤单被拒保持挂起、撤单成功释放、日终作废跳过撤单失败的委托。`go test ./internal/trading ./internal/httpapi` 全绿，`tsc --noEmit`、`vite build` 通过，接口实测（503/404/400 语义、心跳落库）通过。

## 八·五、智能体升级：只读工具调研 + 情景记忆（已交付，2026-10-20）

此前系统的模型只做"一次成型"：拿到打包好的事实，输出一份 JSON，`DisableTools: true`。
这一版让它具备了两个真正的 agent 能力：**自己决定查什么**（受限工具循环）与**回忆过去**
（情景记忆），同时保持每一步可回放。

### 1. 受限只读工具集（tools.go）

六个纯读工具，数据与本轮快照严格同源：

| 工具 | 作用 |
|---|---|
| `list_sector_flow` | 板块资金流排行（东财真实数据，主力净流入 + 5日/10日累计） |
| `list_sector_heat` | 行业强度榜（来自本轮全市场快照） |
| `list_sector_stocks` | 行业成分股表现，看普涨还是龙头独涨 |
| `get_symbol_detail` | 单票完整快照 + 候选依据 + 持仓防守线 |
| `compare_symbols` | 多票对比，以同行业中位数为基准算相对强弱 |
| `recall_trades` | 检索情景记忆（见下） |

约束：白名单之外一律拒绝；参数严格校验（未知工具会列出可用工具名，模型可自行纠正）；
输出有行数与字符上限；`SectorFlowProvider` 未注入时明确报"数据源未配置"，不造假数据。

**符号统一走 6 位数字**（`symbolDigits`）：行情源是 `688775.SH`，模型参数可能写
`688775`。早期按字符串比较，导致带后缀标的全部"查不到行情"——模型连撞三步还推出
"科创板没有实时行情"的错误结论。这是本版抓到的最隐蔽的 bug，已加专门回归测试。

### 2. 多步调研循环（research.go）

模型输出 `{"thought","action":"tool","tool","args"}` 或 `{"action":"done","conclusion"}`，
**工具由 Go 执行**，结果回灌下一轮。最多 4 步（实测 3 步总是全花在收集上，第 4 步才
留得出收尾），剩余 ≤2 步时提示"只有缺了它判断会相反才继续查"。

- 每步落库 `trading_agent_steps`（thought/tool/args/结果/耗时/错误），整批替换防双时间线。
- 降级不阻断：模型超时、坏 JSON、工具报错都只降级为"直接出计划"——把可观测性做成
  单点故障是最常见的过度设计。
- 步数用完时用最后一步的想法收尾，比一句"未给出结论"有用。
- 实测（23:25 真实计划）：步 1 查概念资金流发现机器人 5日/10日净流出 74/397 亿；
  步 2 据此去验证板块内部是普涨还是龙头独涨；步 3 自主指出"候选池核心行业的资金方向
  还没核对"并补查行业维度。33 秒，无降级。这是"看结果→改主意"的真 agent 行为。

为什么不打开 Hermes 原生工具：原生工具会让调用脱离账本，同一提示词两次运行看到不同
外部世界，计划无法回放。纯文本 JSON 协议让"能自己查"和"每步可审计"不再二选一。

### 3. 情景记忆（memory.go）

`trading_memory` 表，复盘时从 `ClosedTrade` + 计划快照幂等补齐（`SyncMemories`）。

一条记忆保留：买入时的市场状态（取自计划而非平仓时刻——买入环境才决定该不该做）、
买入理由原文（Order.Thesis，保留原话不摘要）、失效条件、峰值浮盈与回吐百分点、
离场原因、结果判档（win/loss/flat，±0.5% 阈值）。`ClosedTrade` 新增 `PeakPrice`，
平仓是拿到峰值价的最后机会，不写就永久丢失。

`Lesson` 仍由代码按规则生成，只陈述事实不给建议——"下次别在退潮期追高"会被模型当成
本轮指令，绕过风控；事实描述（"退潮期买入，止损离场"）让它自己判断。

与 Lesson 的分工：Lesson 是聚合统计（"止损 8 次胜率 25%"），回答"平均而言"；
Memory 是单笔情境，回答"那一次发生了什么"。聚合会抹掉情境——模型读到胜率 25% 最
省力的解读是"少止损"，这是灾难性的；读到情境才可能推出"退潮期不追已涨过一轮的板块"。

模型可以用 `recall_trades` 主动检索记忆（按标的/板块/市场状态/离场原因/结果/关键词），
人也可以在页面直接翻（`GET /trading/memory`）。

### 4. 其他

- `Plan.Research` 随计划落库，前端计划详情有「调研回放」折叠面板（默认收起，露出结论）。
- 「情景记忆」面板展示最近记忆与"补齐历史"按钮。
- `Service.SetResearchEnabled`：单测用它关掉调研（计划流程用例不该为多一次模型往返买单）。
- 新增 17 个测试：工具校验、跨格式符号匹配、循环步数上限、报错回灌、落库替换、
  记忆幂等、情境还原（regime/thesis/回吐）、检索过滤。全绿。

### 5. 已知边界

- 调研每步一次模型往返，整轮约 30-45 秒；叠加计划生成，单轮约 60-75 秒。
  自动循环的单飞锁保证不并发，对 15 秒 tick 的调度无影响。
- 记忆在复盘时生成（T+0 收盘后），当天盘中买当天看不到自己的记忆——但 T+1 规则下
  当天也卖不掉，时效够用。
- 资金流依赖行情总览服务实现 `SectorFlowRanking`；未实现时工具明确报"未配置"。
