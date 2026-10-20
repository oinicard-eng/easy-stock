# 交易智能体的裁决层（P2.3）

## 这一层解决什么问题

在 P2.2 之前，模型输出之后只有一步：`BuildOrders` 把建议变成订单，不合格的就地标记
`rejected` 并写一句中文原因。这能防住超买，但回答不了三个问题：

1. **今天为什么没买？** 原因散落在每笔订单的 `reject_reason` 里，要人去拼。
2. **这周拦了多少笔、拦得对不对？** 没有稳定标识，只能正则匹配中文。
3. **组合真的分散吗？** 单票上限 20%，六只票同属电子，逐笔检查全部通过。

P2.3 把"模型建议 → 守卫校验 → 裁决"拆成三层，并把裁决本身作为一等数据落库。

```
模型 DraftPlan ──▶ 守卫（policy + BuildOrders）──▶ Decision（含 Verdicts / Exposure / Narrative）
                                                        │
                                                        ├─▶ Orders（进账本）
                                                        └─▶ trading_decisions（历史流水，只增不改）
```

## 三个不变式

**一、拦截与调整分开。** `block` 表示"不允许"，`adjust` 表示"允许但已缩量"。把缩量成交
算成失败，复盘统计就再也解释不清组合的真实行为。

**二、每条裁决有稳定 Code。** 一旦发布不再改名——它写在历史决策的 JSON 里，改名等于让
过去的数据失去解释力。需要新语义就加新代码。

**三、顺序不可交换。** 账户总闸 → 市场状态 → 逐笔风控 → 组合守卫。
kill switch 否决的是整轮动作，不是某一笔；恐慌时不该接飞刀，这条必须在算股数之前生效，
否则会出现"股数算好了再被丢掉"的无谓计算与被占用的组合额度。

## 守卫清单

| Code | 级别 | 作用域 | 语义 |
| --- | --- | --- | --- |
| `gate.kill_switch` | block | account | 账户全部停止，本轮不产生任何订单 |
| `gate.pause_buying` | warn | account | 暂停买入，卖出与减仓放行 |
| `market.unknown` | block | market | 全市场样本不足（< 500 只），看不清就不开仓 |
| `market.panic` | block | market | 恐慌状态，禁止开新仓 |
| `order.count_cap` | adjust | order | 建议笔数超过上限，截断 |
| `order.empty` | block | order | 模型没有给出任何建议 |
| `order.buy_dropped` | adjust | account | 门控/市场状态导致买入建议被整批剔除 |
| `order.invalid_symbol` | block | order | 代码无效 |
| `order.duplicate_symbol` | block | order | 同一标的一次计划只允许一笔 |
| `order.invalid_side` | block | order | 买卖方向无效 |
| `order.no_quote` | block | order | 不在行情快照内或没有有效价格 |
| `order.limit_price_missing` | block | order | 限价单缺少限价 |
| `order.price_out_of_limit` | block | order | 限价/现价超出涨跌停区间 |
| `risk.single_name` | block | order | 已持仓 + 在途 + 本次目标超过单票上限 |
| `risk.sector_weight` | block | portfolio | 所属行业买入后合计超过行业上限 |
| `risk.sector_name_count` | block | portfolio | 所属行业持有只数达到上限 |
| `risk.stop_loss_missing` | block | order | 买入缺少止损价 |
| `risk.stop_loss_invalid` | block | order | 止损价不低于买入价 |
| `risk.stop_loss_wide` | block | order | 止损距离超过上限 |
| `risk.trade_risk` | block | order | 止损触发亏损超过单笔风险上限 |
| `risk.exposure` | block | account | 账户总资产异常，无法计算仓位 |
| `risk.cash_floor` | warn | portfolio | 买入后现金占比低于底线 |
| `sizing.below_lot` | block | order | 目标金额/卖出比例不足一手 |
| `sizing.reduced_by_cash` | adjust/block | order | 可用资金不足，缩量或拒绝 |
| `sizing.scaled` | adjust | portfolio | 总仓位超上限，等比缩减全部买入 |
| `sizing.dropped_on_scale` | — | order | 等比缩减后不足一手（写在订单原因上） |
| `position.not_held` | block | position | 没有持仓不能卖出 |
| `position.unavailable` | block | position | T+1 可卖数量不足 |
| `ledger.reserve_failed` | block | order | 裁决通过但落库冻结时余额不足 |

`Verdict.Metric` 与 `Verdict.Limit` 记录当时依据的数字，复盘时可以只靠它们重算"当时该不该拦"，
不必翻日志。

## 组合账：为什么逐笔检查不够

`policy.Book` 是组合暴露的累加器。已持仓、在途挂单、本轮已通过的买入都记在同一本账上：

- 单票：`Book.SymbolWeight(symbol) + 本次权重 ≤ MaxSingleWeightPercent`
- 行业：`Book.SectorWeight(sector) + 本次权重 ≤ MaxSectorWeightPercent`（默认 45%）
- 只数：同一行业最多 `MaxSectorNames` 只（默认 3）

行业归并复用选股层的 `universe.SectorOf`——两处用不同口径是典型漏洞：选股按 16 大类分散、
风控按细分行业判集中度，各自都"看起来合理"，合起来却不设防。

`TryBuy` 的检查与提交是两步：失败不写账。否则被拒绝的订单会吃掉后续订单的行业名额。

行业未知（人工指定标的且行情源没有行业字段）时跳过行业判断，而不是猜一个——
宁可少拦，也不要把无关行业强行合并。

## 决策前后的仓位

`Decision.Exposure` 是"为什么只买了这么点"的直接答案：

- `before_percent`：已持仓 + 在途挂单的仓位
- `after_percent`：加上本轮通过的买入
- `budget_percent`：本次实际生效的上限（市场预算与风控上限取较小值）
- `cash_percent`：决策后现金占比
- `sectors`：决策后的行业分布与只数

## 持久化

`trading_decisions` 表只增不改。裁决记录的是"当时为什么这么下单"，事后修改它等于篡改历史；
需要补充结论时写新的记录（P4 复盘），而不是回头改这条。

计划里保留一份 `verdicts` 与 `exposure` 副本，方便前端一次请求拿到全部信息；
统计与复盘一律读 `trading_decisions`，避免两处口径。

`settleDecision` 会用落库后的订单状态回填统计：裁决认为可执行的订单可能被账本拒绝
（可用资金不足），那才是最终事实。计划写着 6 笔可执行、账户里只有 4 笔真实挂单，
是最容易让人不再信任系统的偏差。

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/trading/decisions?limit=20` | 最近的裁决，新的在前 |
| GET | `/api/v1/trading/decisions?plan_id=...` | 按计划取那次裁决 |
| GET | `/api/v1/trading/decisions/{id}` | 按裁决 ID 取 |

计划详情（`GET /api/v1/trading/plans/{id}`）已含 `decision_id`、`verdicts`、`exposure`。

## 前端

计划详情里新增"裁决"面板：先一行汇总（拦截 / 调整 / 共几条依据）、仓位进度条与行业分布，
再逐条列出裁决（级别、代码、作用域、标的、消息、metric/limit）。默认展示 8 条，可展开全部。
拦截排在调整之前，调整排在提示之前——先看被否掉的部分。

## 已知边界

1. **可卖数量未冻结**：多份计划可以同时对同一持仓挂卖单，成交时由账本按实际持仓兜底，
   但计划阶段可能高估可卖数量。属 P3 范围（`reserved_quantity`）。
2. **行业上限只管买入**：卖出不受行业约束，减仓本就该无条件放行。
3. **`market.unknown` 只在真正评估过之后才生效**：`Evaluated=false` 表示本轮没取全市场数据
   （人工指定标的时允许），此时不限制交易——用禁买掩盖数据缺失会让人误以为系统在保护他。
4. **裁决不判断"该不该买这只票"**：那是模型与选股层的职责。守卫只管"能不能买、能买多少"。
