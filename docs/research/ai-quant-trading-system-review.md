# 外部项目评估：ai-quant-trading-system（v9.0.6）

评估对象：<https://github.com/51ijjkbghvhj/ai-quant-trading-system>
评估时间：2026-10-20
评估目的：判断能否用于 easy-stock 交易智能体（`backend/internal/trading`）

## 一、结论

**不能作为运行时组件接入，但值得借鉴四个机制。**

不可接入的硬原因：

1. **没有真实下单能力**。`ai-trader/v2/ai_executor.py` 全文 9 行，函数体只有 print 与 `return True`，注释写明 "Placeholder for trade execution. In a real system, this would connect to a broker API."。仓库里没有任何券商 SDK（xtquant / easytrader / futu / ib）引用，所谓"全自动交易"是写 `portfolio.json` 的模拟撮合。
2. **账本是 JSON 文件，不是数据库**。持仓、现金、成交都堆在 `user_data/portfolio.json`（仓库里提交了 79KB 实例），并发写与部分失败都没有事务保护。我们的账本已经是 SQLite 单事务提交。
3. **单文件巨型事件总线**。`ai-trader/v3/event_bus.py` 单文件 121KB（约 2900 行），混了行情抓取、LLM 调用、风控、撮合、写盘、日志、线程调度。无法单独复用其中一层。
4. **无测试、无依赖清单、无 CI**。仓库 49 个文件，没有任何测试文件（`snapshot_test.json` 是数据快照，`static/test.js` 是前端脚本），没有 `requirements.txt`（README 却让人 `pip install -r requirements.txt`），没有 `.github/workflows`。
5. **工程健康度低**。3 stars / 1 fork / 1 watcher；建库至今 6 次提交，其中 5 次改 README，全部代码来自单次提交 `release: v9.0.6-SAFE`；仓库里提交了 875KB 运行日志（重复两份）、`trading.db`（0 字节）、`quotes_cache.json`、`portfolio.lock`。

可以借鉴的原因：它的**认知/执行分层思路与我们的架构同源**，且几个机制的设计比我们当前 P1 更完整（防守线、信念值生命周期、经验回灌）。MIT 协议允许借鉴，注明来源即可。

## 二、架构对照

| 能力 | 他们的实现 | 我们的现状 | 判断 |
| --- | --- | --- | --- |
| 全市场数据 | 腾讯/新浪/东财，`event_bus.py:289-500` | 已有 `foundation` + 多家 provider | 我们更完整，不借鉴 |
| 市场情绪 | `market_structure.py` 板块聚合→阶段判定 | `marketemotion` + `themeindex` | 我们更完整，不借鉴 |
| 候选池约束 | 全市场池 + 持仓/关注池，`get_full_market_pool` | 白名单严格限定在行情快照内 | 我们更严格，保留 |
| LLM 决策 | 单次 prompt 输出信号 JSON | 单次 prompt 输出计划 JSON | 同构，保留 |
| JSON 提取 | 栈平衡匹配（`ai_model.py:83`） | 我们等价（`decode.go`） | 能力相同，无需移植 |
| 硬风控 | S/A/B 三级 + 一票否决（`risk_engine.py`） | 单一确定性校验层（`risk.go`） | **可借鉴分级与止盈覆盖** |
| 防守线 | 保本/移动止盈，线只升不降 | 只有固定止损价 | **值得移植，P1 缺口** |
| 持仓生命周期 | 建仓/持有/冷却 + 信念值 + Thesis 状态 | 只有订单级 thesis 文本 | **值得移植，P1 缺口** |
| 仓位计算 | Score × Mood × Winrate（`event_bus.py:1248`） | 模型给权重，代码折算股数 | **值得借鉴"胜率缩放"** |
| 经验层 | SQLite 记录 mistakes/thesis_stats，次日注入 | 无 | **值得借鉴，对应 P4** |
| 执行 | 模拟撮合，无券商 | 模拟撮合 + Broker 接口预留 | 我们更规范 |
| 面板 | 单 HTML 79KB + Flask | React 工作台 | 我们更完整 |

## 三、值得移植的四个机制

### 1. 动态防守线（优先级最高）

他们的算法（`event_bus.py:2010-2028`）：

```python
trail_pct = 0.03                      # 默认回撤
if 20cm: trail_pct = 0.06             # 创业板/科创板放宽
if 高潮: 0.08 / 0.05
elif 退潮/冰点: 0.03 / 0.015

if profit_ratio > 0.03: new_line = highest * (1 - trail_pct)
else:                   new_line = cost * 0.95
b["risk_line"] = max(现有线, new_line)   # 线只升不降
```

一旦现价 ≤ `risk_line`，**风控信号优先于 AI 结论**直接生成卖出（`event_bus.py:2035-2065`）。

价值：我们的止损是下单时写死的静态价，盈利后不会上移，回吐是必然的。需要在我们这边补：

- `positions` 表增加 `highest_price`、`risk_line`；
- 每次刷新行情时重算，只升不降；
- 触发时产出"风控强制卖出"信号，优先于模型计划，但仍走人工确认（P1 不自动执行）。

### 2. 止盈分级覆盖（A 级）

`risk_engine.py:51-54`：+10% 卖 1/3、+15% 再卖 1/3、+20% 清仓；浮亏触及 -5%/-7%/-8%（主板/创业板/科创板）时，**即使模型建议持有也强制改为卖出**。

价值：我们的 `risk.go` 只在买入时校验止损价，持仓后没有任何强制退出规则。这两条可以直接用 Go 实现，且不依赖模型。

### 3. 持仓生命周期与冷却期

`position_lifecycle.py:177-240`：

- 状态机 `BUILDING → ACTIVE → COOLDOWN`，建仓保护期内不允许卖出；
- `conviction`（0-100）随信号增减，低于阈值强制卖、高于阈值禁止卖；
- 清仓后进入冷静期，期间不允许重新买入；
- `thesis` 有 `ACTIVE / WEAKENING / BROKEN` 三态，BROKEN 直接触发卖出。

价值：能治我们当前的"止损后立刻反手再买"和"模型反复改主意"。我们的 `trading_orders` 已存 `thesis`/`invalidation` 文本，缺的是状态与冷却。

### 4. 经验层回灌

`experience_layer.py:172-210`：从 SQLite 取最近的高权重错误、Thesis 类型胜率，拼进次日提示词。

价值：这是 P4 的核心。我们的优势是已有 `plan`、`order`、`fill` 完整链路，可以直接统计"按画像/regime/thesis 的胜率"，不必照抄他们的表结构。

## 四、不要照搬的部分

| 部分 | 问题 |
| --- | --- |
| `event_bus.py` 单文件架构 | 2900 行混职责，无法测试、无法替换单层 |
| JSON 文件当账本 | 无事务、无并发保护，崩溃即丢账 |
| 全局单例 `_position_manager` | 状态跨请求泄漏，测试不可隔离 |
| 把风控写进 AI 循环里 | 风控与模型调用耦合，模型失败时风控也失效 |
| 35% 单票仓位 | `calc_dynamic_budget` 对 score≥90 给 35%，但 `risk_engine.py:353` 单票硬顶 25%，两处不一致，高分信号必然被自己的风控拒绝 |
| 仓库内提交运行数据 | 日志/持仓/缓存入库，说明没有发布流程 |

## 五、建议的落地顺序

| 阶段 | 内容 | 预估改动 | 收益 |
| --- | --- | --- | --- |
| P1.5 | 防守线 + 分级止盈 + 强制退出信号（Go，确定性） | `trading/positions` 加两列、`risk.go` 加持仓期规则、`service.go` 每次刷新重算 | 直接减少利润回吐，不依赖模型 |
| P2 | 冷却期 + Thesis 三态 + 信念值 | 加 `conviction`/`state`/`cooldown_until`，计划生成前过滤 | 抑制反复交易 |
| P4 | 经验层（胜率与错误回灌提示词） | 基于现有 fill 统计，加一段 prompt | 提升计划质量 |
| 不实施 | 引入其 Python 运行时、面板、事件总线 | — | 收益为负 |

## 六、引用清单

- `ai-trader/v2/ai_executor.py:1-9`（执行占位）
- `ai-trader/v3/event_bus.py:216`（候选池）、`1291`（模拟执行）、`2010-2065`（防守线）
- `ai-trader/v2/risk_engine.py:44-70`（S/A/B 参数）、`330-420`（仓位与止盈）
- `ai-trader/v3/position_lifecycle.py:177-240`（可买卖判定与信念值）
- `ai-trader/v3/experience_layer.py:172-210`（经验回灌）
- `ai-trader/v2/ai_model.py:83-142`（JSON 栈平衡提取）

License：MIT（`LICENSE`，Copyright 2026 炸洋芋）。借鉴算法时在文件头注明来源与思路出处。
