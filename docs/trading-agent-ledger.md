# 交易账本的冻结与对账（P2.1）

## 为什么需要这一层

P1 的账本只记两件事：现金和持仓。挂单在成交前不动账，于是出现一个隐蔽但确定的漏洞：

> 账户现金 10 万。计划 A 挂出 6 万买入单，未成交。
> 计划 B 生成时读到「现金 10 万」，又规划了 6 万买入。
> 两笔挂单都以为钱是自己的。第二笔成交时现金不足，被拒。

表面上只是「一单失败」，实际是两个更严重的问题：

1. **风控失效**：仓位上限、单笔风险、现金底线全部基于错误的可用资金计算，算出来的风险敞口是假的。
2. **故障不自报**：账本不会报错，界面显示「10 万可用」，直到某天同时触发两笔成交才发现对不上。

P2 要上自动交易，这个漏洞必须先补掉。补法是引入**冻结（reservation）**：挂单即刻占用资金，成交或撤销时释放。

## 三条不变量

任何后续改动都必须保持这三条，它们是账本正确性的全部内容：

```
1. account.reserved_cash == Σ(所有未成交买入单的 reserved_amount)
2. account.cash - account.reserved_cash >= 0
3. 释放额 = 按剩余股数重算的应冻结额，不做比例推算
```

第 3 条是为了避免四舍五入漂移。若用「成交比例 × 冻结额」释放，部分成交多次后会出现几分钱的残差，残差累积会让对账永远不通过，最后只能靠人工调账——那就等于没有账本。

## 实现要点

### 唯一的金额算法

`expectedReserve(order, remaining)` 是冻结、释放、对账三处共用的唯一实现。三个地方各算一套是这类账本最常见的错误来源。

```go
func expectedReserve(order Order, remaining int) float64 {
	if remaining <= 0 || order.LimitPrice <= 0 {
		return 0
	}
	notional := float64(remaining) * order.LimitPrice
	return round2(notional + TradeFee(SideBuy, notional))
}
```

### 冻结与落库同事务

`CreateOrders` 在同一事务内完成「冻结 → 插入订单」。若分成两步，进程中断会留下「订单存在但没冻结」的半状态，之后每次风控都会按错误的可用资金算仓位。

冻结用条件更新守住下界，并发下也不会把余额冻成负数：

```sql
UPDATE trading_accounts
SET reserved_cash = reserved_cash + ?
WHERE id = ? AND cash - reserved_cash >= ?
```

`RowsAffected() == 0` 即资金不足，该笔订单被标记为 `rejected` 并写明「需要冻结 X 元，可用 Y 元」。

**这里刻意不做自动缩减**：缩减会隐式改变模型给出的相对权重，且用户看不到发生了什么。P1 已有的「按可用资金逐手缩减」发生在风控层（`BuildOrders`），那里有 warning 提示；账本层只做拒绝。

### 成交时按剩余重算

```go
remaining := order.Quantity - order.FilledQuantity - filledQuantity
target := expectedReserve(*order, remaining)
delta := order.ReservedAmount - target   // >0 释放，<0 追加
```

全部成交时 `target = 0`，冻结清零；部分成交时冻结额自动收敛到剩余股数对应的金额。

### 撮合口径

`Account.CashForOrder(order)` 是撮合时的唯一口径：

```go
others := a.ReservedCash - order.ReservedAmount   // 其他挂单的冻结
return a.Cash - others
```

即「扣掉别人的冻结，不扣自己的」。少了这个区分会走向两个错误：用自己的冻结当不可用 → 自己的单永远买不动；完全忽略冻结 → 回到超买。

### 对账

`ReconcileReservations(accountID, repair)` 按挂单重算应收冻结额，与账本记账值比对，报告 `Drift`：

- 差异 > 0.01 元 → error 级问题；
- 已收口（成交/撤销/拒绝）却仍挂着冻结 → 孤儿冻结，warning；
- 可用资金为负 → error；
- `repair=true` 时才回写，默认只报告。

**默认不自动修复**是刻意的：自动改账会在排查阶段掩盖问题，让人以为「账本一直是对的」。

## 数据迁移

SQLite 的 `ALTER TABLE` 不支持 `IF NOT EXISTS`，因此 `ensureColumn` 先查 `PRAGMA table_info` 再决定是否改动。改动项：

| 表 | 新增列 | 用途 |
| --- | --- | --- |
| `trading_accounts` | `reserved_cash` | 冻结额记账 |
| `trading_orders` | `decision_id` | 追溯到哪次模型决策 |
| `trading_orders` | `cycle_id` | 追溯到哪一轮循环（P2.3 起写入） |
| `trading_control` | 新表 | 账户总闸 |

旧库升级不丢数据，已有账户的 `reserved_cash` 默认 0。

## 门控：两级，不是一个开关

```go
type Control struct {
	PauseBuying bool
	KillSwitch  bool
	Reason      string
}
```

| 状态 | 生成计划 | 买入成交 | 卖出成交 |
| --- | --- | --- | --- |
| 正常 | 允许 | 允许 | 允许 |
| `pause_buying` | 允许（禁止买入建议） | 保持挂起 | 允许 |
| `kill_switch` | 拒绝 | 拒绝 | 拒绝 |

两点设计判断：

1. **暂停买入必须放行卖出**。止损、减仓是保护性动作，如果总闸一刀切把它们也停了，风控在最需要生效的时候恰好失效。
2. **暂停期间买入挂单保持挂起而不是被拒绝**。暂停是临时状态，恢复后该单仍应成交；拒绝则等于变相撤单。

`kill_switch` 打开时强制 `pause_buying = true`，避免出现「已全部停止但仍可买入」这种前端与门控必然有一处漏判的状态。

## 验收

单元测试（`backend/internal/trading/ledger_test.go`）：

| 测试 | 验证点 |
| --- | --- |
| `TestCreateOrdersBlocksSecondPendingBuyFromSameCash` | 两笔 6 万挂单共享 10 万现金时，第二笔被拒 |
| `TestBuildOrdersUsesAvailableCash` | 风控按可用资金缩减，而不是账面现金 |
| `TestApplyFillReleasesReservation` | 全部成交后冻结清零，现金扣减正确 |
| `TestPartialFillKeepsResidualReservation` | 部分成交后冻结收敛到剩余股数 |
| `TestReleaseReservationOnCancel` | 撤销归还冻结 |
| `TestReconcileDetectsAndRepairsDrift` | 注入 5000 元漂移能被发现并修复 |
| `TestKillSwitchBlocksPlanAndExecute` | 总闸阻断生成与执行 |
| `TestPauseBuyingKeepsBuysPendingButAllowsSell` | 暂停买入时卖出仍成交、买入保持挂起 |

接口验证（真实服务）：

```
POST /api/v1/trading/control {"kill_switch":true}  -> pause_buying 被强制为 true
POST /api/v1/trading/plans                          -> HTTP 409
POST /api/v1/trading/reconcile                      -> ok=true, drift=0
```

## 已知限制

- **可卖数量未冻结**。同一批股票可以被两个卖单同时占用，第二笔在 `ApplyFill` 时因 `available < quantity` 被拒。不会算错账，但会造成「挂单看着能卖、实际卖不掉」。P3 做持仓生命周期时补 `reserved_quantity`。
- 市价单在落库前被写入保护限价（买入 `现价 × 1.02`、卖出 `现价 × 0.98`，并夹在涨跌停区间内），因为真实市场不存在「按现价必然成交」。冻结依赖这个价格。
- 冻结按限价计算。若实际成交价高于限价（当前撮合不会发生，取现价且现价 ≤ 限价），冻结额会不足。
- 对账一次扫描账户下最近 1000 笔订单，账户长期运行后需要改为只扫未收口订单。
