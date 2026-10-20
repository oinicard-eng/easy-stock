# 交易智能体交付手册

> 版本：P5 + 无人值守交付版（2026-10-20）
> 本文是运行手册：怎么跑、怎么判断它是否正常、出事怎么办、接实盘还差什么。

## 一、交付能力总览

| 能力 | 状态 | 说明 |
| --- | --- | --- |
| 模拟盘撮合 | ✅ | 保守成交：限价保护、涨跌停校验、无行情不猜测 |
| 决策与裁决 | ✅ | 模型建议 → 策略规则包逐条裁决，拦截原因全部落库 |
| 账本三不变量 | ✅ | 冻结资金、可用资金、冻结股数三条对账，drift=0 |
| 持仓防守线 | ✅ | 止损 / 硬止损 / 分级止盈 / 移动止损，只认 kill_switch，不调模型 |
| 自动循环 | ✅ | 认知 → 选股 → 裁决 → 执行，闸门分层记录 |
| 日亏 / 连败熔断 | ✅ | 三处同写（当日状态 + 门控 + 循环记录），解除必须给理由 |
| 复盘与经验 | ✅ | 只认已平仓事实，样本 <30 不输出胜率结论 |
| 业绩归因 | ✅ | 胜率 / 盈亏比 / 期望值 / 逐日曲线 / 按标的与离场原因分桶 |
| 交易日历 | ✅ | 法定节假日不跑循环、不生成复盘 |
| 日终委托作废 | ✅ | 跨收盘挂单作废并释放冻结；作废前先向通道撤单 |
| 运行事件流 | ✅ | 熔断、降级、自愈、日终清理全部落 `trading_events` |
| 健康自检 | ✅ | `GET /api/v1/trading/health`，critical 返回 503 |
| 告警外发 | ✅ | 企微/钉钉/webhook + 文件兜底，冷却合并，投递结果落库可查 |
| 按天回放 | ✅ | `GET /api/v1/trading/timeline?day=` 聚合六类记录成一条时间线 |
| 故障自愈 | ✅ | 账本自动对账修复；连续失败 3 轮自动降级暂停买入 |
| 通道契约 | ✅ | 异步回报幂等落账、撤单退避重试、超量回报截断告警 |
| 实盘通道 | ⛔ | 有意未接。契约与账本已就绪，见「五、接实盘还差什么」 |

## 二、运行方式

### 一条命令（推荐，含前端）

```bash
cd frontend && npm run build          # 只需在改前端后执行一次
node scripts/trading-agent.mjs --web   # 编译后端 + 守护 + 托管前端
```

打开 `http://127.0.0.1:20073/#trading` 即是完整交易页，后端在 `127.0.0.1:20081`。
静态托管只读，不代理 API——前端直接请求后端，少一层排查面。

### 守护模式（只跑后端）

```bash
node scripts/trading-agent.mjs            # 编译 + 启动 + 守护
node scripts/trading-agent.mjs --no-build # 跳过编译
```

守护进程职责：健康轮询（30 秒）、崩溃自动重启（指数退避至多 5 分钟）、连续 5 次不健康重启、日志轮转（5MB × 3 份）。日志在 `.runtime/trading-agent.log`。

### 手动模式

```bash
cd backend && go build -o bin/easy-stock-server.exe ./cmd/server
A_STOCK_ADDR=127.0.0.1:20081 ./backend/bin/easy-stock-server.exe
cd frontend && npm run dev   # 交易页：http://127.0.0.1:20073/#trading
```

## 二·五、配置告警（无人值守必做）

没有告警出口时，系统出问题只会写进数据库和页面——人不在电脑前就等于没通知。
这是无人值守最容易失效的一环，所以它被单独列成一节。

```bash
# 企微 / 钉钉机器人：贴一个 URL 就能用
export A_STOCK_TRADING_ALERT_WEBHOOK="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"
export A_STOCK_TRADING_ALERT_FORMAT=wecom        # wecom | dingtalk | json（默认 json）

# 文件出口是兜底，默认就开着，落在日志目录 trading-alerts.log
export A_STOCK_TRADING_ALERT_LOG="D:/easy-stock-main/.runtime/trading-alerts.log"

# 临时关闭全部外发
export A_STOCK_TRADING_ALERT_OFF=1
```

**配完必须验证**：页面「告警投递」面板点「发送测试」，或

```bash
curl -X POST http://127.0.0.1:20081/api/v1/trading/alerts/test
```

返回 `{"delivered":["webhook","file"]}` 才算通。启动日志也会打出
`event=trading_alerts_ready channels=webhook,file`；若打出
`trading_alerts_no_channel`，说明一个出口都没配上。

### 收敛规则

同一 `kind + symbol` 的告警有冷却窗口，避免把真正的问题淹掉：

| 等级 | 冷却 | 说明 |
| --- | --- | --- |
| critical | 10 分钟 | 严重问题反复提醒，但不会每轮循环都喊一次 |
| warning | 60 分钟 | 积压、熔断这类状态不值得每分钟推 |

- 冷却期内的同类告警**只计数不外发**，冷却结束补发一条并带上「期间另有 N 条已合并」「此问题仍在持续」。
- 严重度升高（warning → critical）**立即突破冷却**，不合并。
- 投递全部失败时不占用冷却，下一条同类告警立刻重试——没送达等于没发过。
- 全部出口失败会记 `failed` 并写失败原因，健康条也会出现 `alerts.delivery_failed`。

### 什么会触发告警

warning 及以上事件全部外发：账本漂移、连续失败降级、熔断、挂单积压、通道错误。
另外 `autopilot.recovered` 与 `ledger.drift_healed` 虽然是 info 也会发——
人收到"出问题了"之后，最想知道的是"好了没有"。

## 二·六、按天回放

```
GET /api/v1/trading/timeline?day=2026-10-20
```

把当天的循环、计划、裁决、委托、事件、复盘按时间摊平成一条线，页面「时间线回放」面板可直接切日期。
回答"那天为什么没买""这笔单是哪一轮下的"时先看它，不用再去五张表里各查一遍。

摘要里带 `model_calls` 与 `total_tokens`——自动交易一天花多少思考成本，可查而不是靠账单反推。
`notes` 非空表示某张表读失败，**这条线不完整**，不要据此下结论。

## 三、怎么判断系统是否正常

### 一眼看：页面健康条

交易页顶部「运行正常 / 注意 / 需要立即处理」：

- **绿色**：什么都不用做。
- **黄色**：系统还在工作，但有需要知道的事（熔断中、已降级、挂单积压）。
- **红色**：账本漂移、连续失败 ≥3 轮、心跳超时——必须立刻处理。

### 接口级

```
GET /api/v1/trading/health
```

- `200` = healthy / warning；`503` = critical。外部监控只看状态码即可。
- `issues[]` 每项含 `code`（稳定标识）与 `message`（人话）。

### 关键判定规则

| 条件 | 等级 | 含义 |
| --- | --- | --- |
| 账本 drift > 1 分 或股数漂移 | critical | 钱或股数对不上，不会自愈，禁止下单直到查明 |
| 连续失败 ≥ 3 轮 | critical + 自动降级 | 自动暂停买入，保护性卖出仍执行 |
| 心跳超时 10 分钟 | critical | 调度线程卡死或退出 |
| 挂单 ≥ 10 笔 | warning | 资金被持续冻结 |
| 当日熔断 | warning | 正常保护动作，非故障 |
| 告警 24h 全失败 | warning | 通知通道已失效，出事时没人会知道 |
| 告警队列丢弃 > 0 | warning | 告警产生速度超过投递速度 |
| 启动 90 秒内无心跳 | 不报 | 宽限期，避免每次部署都发一条假告警 |

## 四、故障处置

**连续失败自动降级后怎么办**
1. 打开「运行事件」面板看最新事件里的错误原文；
2. 处理根因（通常是模型网关或行情源故障）；
3. 连续 3 轮成功后自动恢复，无需手动操作；确需立即恢复用「暂停买入」开关手动切换。

**账本漂移（critical）**
1. `POST /api/v1/trading/reconcile?repair=true` 尝试修复；
2. 循环每 5 分钟也会自动修复；若事件流里出现「修复未成功」（critical），说明挂单与账本无法互证，**必须人工核对** `trading_orders` 与 `trading_accounts`，不要再次盲修。

**进程崩溃**
守护模式自动重启，重启后自动补跑：中断的循环与计划会被标记终止、跨收盘的挂单会被清理。频繁重启（uptime 总是几分钟）本身是需要排查的故障，看 `.runtime/trading-agent.log`。

**撤单失败**
事件流出现「撤单失败」时订单保持挂起、冻结不变。这是刻意设计：通道那边可能已成交，假装撤掉会造成超卖。先查通道，再手动撤销。

## 五、接实盘还差什么

账本、风控、回报、撤单的契约已全部就绪并有测试覆盖（`report_test.go`）。接真券商只需实现一个适配器：

```go
// 实现这两个接口，替换 service.broker 即可
type Broker interface {
    Name() string
    Submit(ctx context.Context, request BrokerOrder) (BrokerResult, error)
}
type CancelableChannel interface {
    Cancel(ctx context.Context, request CancelRequest) (CancelResult, error)
}
```

成交不必在 `Submit` 里同步返回：通道异步推送回报到

```
POST /api/v1/trading/reports
{"report_id":"券商回报唯一号","order_id":"本地订单号","status":"partially_filled",
 "filled_quantity":400,"price":10.5,"fee":4.2,"at":"2026-10-21T09:35:00+08:00"}
```

已由后端保证：**同一条 `report_id` 重复投递只记一次账；回报超量按本地剩余截断并告警；已收口订单再收到成交回报不改账只报警**。

接实盘前的必做清单：
1. 适配器实现 + 通道联调（建议先只读：查资金、查持仓，不下单）；
2. 决定回报投递方式（回调 or 轮询器），确保 `report_id` 全局唯一且稳定；
3. 审阅 `ChannelInfo`：`live=true` 后页面会显示「实盘通道」，但**不会**额外加锁——实盘限额应通过风控层（`RiskRules`）收紧而不是前端提示；
4. 用最小资金量真实走一轮：下单 → 部分成交回报 → 撤单失败 → 撤单成功，逐条核对事件流。

**明确不做的事**：不提供"绕过风控直接下单"的通道；不把模型输出直接变成委托——所有订单必须经过裁决层。

## 六、已知边界

- 模拟盘撮合以最新价成交，没有盘口与滑点模拟；业绩归因因此偏乐观，接实盘前请以实盘小资金数据为准。
- `positions.peak_price` 依赖行情刷新路径（账户视图 / 防守线检查），行情长期拉不到时移动止损保持上次快照，不会用陈旧价格误触发。
- 事件表无自动清理；按当前写入密度一年约几千条，SQLite 可承载，暂不做清理任务。
- 守护脚本假定单实例运行；多实例共用同一数据库不在交付范围。
- 告警冷却状态在内存里。进程重启后冷却窗口清空，若问题仍在，会立即重新发一轮告警——
  这是有意为之：重启本身要被看见，重启后问题依旧更该被看见。代价是崩溃重启循环会重复推送。
- 告警队列容量 256。满了先丢 warning，critical 会阻塞等待 2 秒再丢并写
  `event=alert_dropped`；健康条同时出现 `alerts.dropped`。丢弃数不为零说明出口太慢，换通道或降低告警频率。
- 时间线按天查询是先粗筛（SQL 时间范围 ±1 秒）再内存精确过滤。跨零点但开始于前一天的循环，
  其结束事件归到开始那天，不拆分。
- 历史数据里 2026-10-20 之前产生的被拒委托，`quantity` 已被旧逻辑置零，回放时看到的
  「买入 0 股」属于存量记录，新产生的记录不再有这个问题。
