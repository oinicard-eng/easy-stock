# 外部项目评估：QuantBot v1.8（本地源码）

评估对象：`D:\quantbot-main\quantbot-1.8\quantbot-main`
评估时间：2026-10-20
评估目的：判断能否用于 easy-stock 交易智能体（AI 交易员）

## 一、结论

**这是目前看到的、最接近"AI 交易员"目标形态的参考实现，但许可证不允许我们复制任何代码。**

LICENSE 是 **PolyForm Noncommercial License 1.0.0**：仅允许非商业用途，且衍生作品受同样限制。easy-stock 存在商业版工作树（见 `AGENTS.md`），把它的代码搬进来会直接违反许可证。因此：

- **可以**：读它的架构、参数、状态机、失败处理方式，在自己的代码里独立实现；
- **不可以**：复制粘贴、改写后搬运、把它的文件放进本仓库。

这条边界必须先说清楚，否则后面所有"复用"都是法律风险。

## 二、事实画像

| 维度 | 事实 |
| --- | --- |
| 技术栈 | Go 1.25 + Wails v2 + React 18；GORM/SQLite + DuckDB；Python 侧 TDX/ML 服务 |
| 规模 | 296 个 Go 文件、34 个测试文件、`internal/` 约 16MB |
| 交易通道 | `SimulatedBroker` / `QMTBroker`（迅投 MiniQMT，经本地 Python 桥）/ `PtradeBroker`（恒生 Ptrade 客户端内嵌 Python，经 HTTP 桥） |
| 账本 | `portfolio.Engine.Buy/Sell(..., decisionID)`，成交记录带 `decision_id`、`reserved_cash`、手续费拆项 |
| 风控 | `policy.Engine` 硬限制 + `risk.Engine` 指标（VaR/CVaR/Sharpe/ATR/回撤）+ 执行端买入门控 |
| 调度 | `scheduler.AutoScheduler` 按盘前/盘中/盘后相位运行，含"错过盘前补跑" |
| 决策 | CIO：主张 → 强制取证 → 对抗评审 → 多空辩论 → Kelly 缩放 → 集成投票；最终 `min(规则, 主张, 质疑, Kelly)` |
| 复盘 | 每日复盘、Agent 任务日志按阶段查询、透明度数据、研究时间线 |
| 审计 | `audit.Service` 异步批量写、保留期清理、筛股/规划/回测/登录等事件 |
| 回测 | 三因子成本模型（佣金+印花税+Almgren-Chriss 平方根冲击），样本池胜率校准 |

结论：这是一套完整闭环，不是演示项目。README 的"5 个 Agent"不是聊天窗口，是有明确职责与决策边界的流水线。

## 三、与我们当前架构的对照

| 能力 | QuantBot | 我们（P1 + 设计） | 差距判断 |
| --- | --- | --- | --- |
| 交易通道 | 模拟 + QMT + Ptrade 实盘 | 模拟 + Broker 接口（实盘未实现） | 我们缺实盘，P5 补 |
| 账本 | 带 `decision_id`、`reserved_cash`、费用拆项 | 有成交/持仓/现金，无决策关联、无挂单占用 | **必须补** |
| 决策链 | 多阶段 LLM + 规则门控 + 回退 | 单次 LLM + 确定性风控 | 我们 P2 先做单次 + 可选质疑 |
| 硬风控 | `policy.Engine` 统一硬限制，拦买不拦卖 | `risk.go` 下单前校验 | **补"拦买不拦卖"与硬门控** |
| 输出校验 | `OutputGuard`（schema/禁输出/决策边界） | 仅 JSON 结构校验 | **补** |
| 持仓风控 | ATR/移动止损/利润锁定/板块暴露/集中度 | 只有静态止损价 | **P3 必须补** |
| 调度 | 盘前/盘中/盘后 + 补跑 + 相位判断 | 无 | **P2 必须补** |
| 可观察 | 决策证据链、Agent 任务日志、透明度接口 | 计划详情 | **P2 补 decision 维度** |
| 复盘 | 每日复盘 + 记忆日志 | 无 | P4 |
| 回测 | 完整引擎 + 成本模型 | 无 | 不在 P2 范围 |
| 许可 | 非商业 | 开源 + 商业双版 | **禁止复制代码** |

## 四、值得独立实现的七个设计

### 1. `decision_id` 贯穿全链路

它的 `TradeRecord` 带 `decision_id`，订单、成交、持仓变动、复盘都能回溯到同一次决策。我们的 `Order` 只有 `plan_id`，成交表没有决策关联，复盘时无法回答"这笔亏损是哪个判断造成的"。

落地：`trading_orders` 增加 `decision_id`，`trading_fills` 冗余同一 ID，复盘按 decision 聚合盈亏。

### 2. 挂单占用资金（ReservedCash）

它的 `PortfolioSnapshot` 区分 `cash` / `reserved_cash` / `available_cash`。我们目前只在成交时扣现金，多笔挂单会重复占用同一笔钱，只能靠撮合时逐个报错。

落地：下单即冻结 `quantity × price + fee`，撤单/拒绝释放，成交时从冻结转实际扣减。

### 3. "拦买不拦卖"的共享门控

它的 `policy.Engine.pauseBuilding` 是一个**只拦买入、放行卖出**的开关，覆盖所有买入入口（CIO 建仓、自动池、手动下单、审批补确认），且独立于"紧急停止"。

落地：我们的自动交易开关要拆成两个：`pause_buying`（熔断/退潮时只停买）与 `kill_switch`（全停）。

### 4. 决策只降不升

它的公式是 `生效仓位 = min(规则, 主张, 质疑, Kelly)`——LLM 只能把仓位改得更保守，永远不能放大越过规则线。并且 LLM 未配置/失败/超时会**完全回退规则决策**，不让单点故障阻断交易。

落地：我们的 `BuildOrders` 已经是"模型给权重、代码裁剪"，需要补的是：模型建议权重再乘以一个 `risk_scale`（情绪/回撤/连亏），且该系数只能 ≤1。

### 5. OutputGuard：校验模型输出的边界

它不只校验 JSON 结构，还校验"禁止输出项"与"决策边界"（例如模型是否越权给出超出其职责的指令）。我们的 `ParseDraftPlan` 只保证能解析。

落地：在 `decode.go` 后加一层 `Guard`：orders 数量上限、权重总和、禁止字段、side/order_type 枚举、symbol 必须形如代码——不通过则整份计划作废并落库原因。

### 6. 盘前补跑

它的调度器在系统晚启动时会补偿执行错过的盘前任务，直到 15:00 前。我们如果 9:35 才开机，不该直接跳过当天。

落地：调度器启动时检查当日是否已有 `premarket` cycle，没有则补跑一次，并标记 `catch_up=true`。

### 7. 审计与保留期

它的 `audit.Service` 异步批量写入、按保留天数清理，记录筛股/规划/回测/交易等事件。我们的 `runtimelog` 只有运行日志，没有业务审计。

落地：P2 的 cycle 表本身就是审计；再加一张 `trading_audit` 记录开关变更、参数修改、熔断触发、人工干预。

## 五、不建议照搬的部分

| 部分 | 原因 |
| --- | --- |
| 5 Agent 串行 + 4 阶段 LLM 辩论 | 单次决策 4–6 次模型调用，延迟分钟级、成本翻数倍。我们 3 分钟一轮的循环承受不了。P2 只做"主张 + 可选质疑"两次调用，辩论留作 P4 复盘工具 |
| 17 种策略 + 多因子自适应权重 | 需要长周期历史数据、因子库与回测校准，属于量化研究范畴，不是 P2 能吞下的 |
| DuckDB + 双数据库 | 我们已有 SQLite 账本，再加一套分析库会增加部署与备份复杂度 |
| Wails 桌面壳 | 我们已有 Electron，无收益 |
| QMT/Ptrade 桥接实现 | P5 才需要，且许可证禁止复制，必须自己对接券商文档 |
| 30% 单票上限 | 对我们的画像体系偏激进，保持 15/20/25% 分档更稳 |

## 六、对 P2 范围的修订

原 P2：循环调度、市场认知、候选池、AI 决策、风控裁决、自动执行、决策日志、熔断。

加入 QuantBot 验证过的五项后，P2 调整为：

1. **调度**：盘前/盘中/盘后相位 + 启动补跑 + 3 分钟基线 + 事件触发；
2. **认知**：情绪阶段/宽度/涨跌停/板块强度/指数趋势，落库并注入提示词；
3. **选股**：全市场硬过滤 → 打分 → 前 30，记录入选与剔除理由；
4. **决策**：单次 LLM 主张 + 可选一次质疑评审（默认关闭），`min` 合成，失败回退规则；
5. **裁决**：PolicyEngine 式硬门控（单票/总仓/日亏/回撤/流动性/白名单）+ S 级一票否决；
6. **执行**：`reserved_cash` 冻结、`decision_id` 贯穿、模拟撮合、T+1；
7. **观察**：cycle 全字段落库 + 决策证据链 + 审计事件 + 前端"交易员日志"可回放；
8. **熔断**：单日亏损 3%、连续 3 笔止损、`pause_buying` 与 `kill_switch` 分离。

验收不变：一轮 cycle 可完整回放；关开关不产生订单；熔断后不再开仓；账本与决策可互相追溯。

## 七、引用清单

- 许可：`LICENSE`（PolyForm Noncommercial 1.0.0）
- 账本：`internal/portfolio/engine.go:130-160`（TradeRecord）、`693`（Buy）、`959`（Sell）
- 通道：`internal/broker/types.go:91-109`（Broker 接口）、`app_broker.go:1-80`（模式选择与成交回调）
- 硬门控：`internal/policy/engine.go:27-30`（pauseBuilding）、`51-55`（HardLimits）、`180-194`（单票）
- 决策：`internal/cio/proposal.go:323-331`（min 合成）、`cio.go:1455-1462`（仓位缩放）
- 输出校验：`internal/scheduler/output_guard.go:1-50`
- 调度：`internal/scheduler/auto_scheduler.go:19-128`（相位与补跑）
- 成本：`internal/backtest/cost_model.go:1-50`
- 审计：`internal/audit/service.go:35-108`
