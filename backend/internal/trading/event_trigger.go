package trading

// 事件驱动的临时轮次。
//
// 定时间隔（默认 3 分钟）在平静行情里是浪费，在剧烈行情里是延误：
// 一只重仓票在 40 秒内跌 5%，等到下一轮定时循环才处理，损失已经发生完了。
//
// 这里补一条独立通路：tick 每 15 秒看一眼异动，命中就立刻发起一轮，
// 不等定时器。但它必须比定时器更克制，因为模型调用是要花钱的：
//   - 同一只票每天只触发一次；
//   - 两轮事件之间至少隔 6 分钟；
//   - 每天最多 6 次。
//
// 三条限制合起来的效果是：事件通道只处理"今天还没见过的、足够大的"异动，
// 而不是把 3 分钟定时变成 15 秒定时。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
)

const (
	// eventMinInterval 是两轮事件循环之间的最小间隔。
	// 6 分钟大于默认定时间隔（3 分钟）的两倍：事件轮次不该比定时更频繁。
	eventMinInterval = 6 * time.Minute
	// eventMaxPerDay 是事件轮次的每日上限。
	eventMaxPerDay = 6
	// eventSingleDropPercent 是单票日内跌幅阈值（相对昨收）。
	// 5% 在 A 股是"明显异动"而不是"正常波动"：日波动 4% 在高波动票上很常见，
	// 拿它当触发线会让事件通道退化成"每天开几次会"。
	eventSingleDropPercent = 5.0
	// eventSingleWeightPercent 是被考虑的单票权重下限。
	// 0.5% 的仓位跌 8% 对账户影响不到 0.05%，不值得为它调一次模型。
	eventSingleWeightPercent = 3.0
	// eventPortfolioDropPercent 是组合权益相对上次观察的跌幅阈值。
	// 单票阈值抓不到的"多只同时阴跌"由它兜底。
	eventPortfolioDropPercent = 1.5
	// eventPortfolioWindow 是组合跌幅的观察窗口。
	// 只在窗口内比较：拿上午 10 点的权益和下午 2 点比，得到的必然是大跌。
	eventPortfolioWindow = 15 * time.Minute
	// eventSectorFlowInterval 是板块资金流的取数间隔。
	// 它是外部接口，不能跟着 15 秒的 tick 打：15 秒一次等于一天几千次请求，
	// 而资金流本身是分钟级更新，取那么勤也拿不到新东西。
	eventSectorFlowInterval = 5 * time.Minute
	// eventSectorFlowNegativeRatio 是"资金流转负"的判定线：主力净流入占比低于 -1%。
	// 不用"净额为负"直接判：净额为负但占比只有 -0.1% 的板块，
	// 属于正常抖动，拿它触发一次模型调用是纯浪费。
	eventSectorFlowNegativeRatio = -1.0
	// eventSectorFlowDimensionLimit 是每次取数的板块条数。
	// 60 条足够覆盖主力资金流排名靠前的板块，再多也匹配不到持仓所在的行业。
	eventSectorFlowDimensionLimit = 60
)

// eventWatch 是事件通道的内存状态。
type eventWatch struct {
	// fired 记录"某标的在某天已经触发过"，避免日内跌幅持续成立时反复触发。
	fired map[string]string
	// lastAt 是上一次事件触发的时刻，用于最小间隔。
	lastAt time.Time
	// countDay / count 是当日已触发次数。
	countDay string
	count    int
	// equity / equityAt 是组合权益的观察基线。
	equity   float64
	equityAt time.Time
	// sectorFlow / sectorFlowAt 缓存板块资金流（名称 → 主力净占比）。
	// 缓存的意义不只是省请求：它还是"板块资金流是否转负"的比较基准，
	// 每次都取最新值的话，只能看到"现在为负"，看不到"刚刚转负"。
	sectorFlow   map[string]float64
	sectorFlowAt time.Time
}

// eventTriggered 判断此刻是否该发起一轮事件循环，返回触发原因。
//
// 顺序：先检查配额与间隔，再取行情。取行情有网络成本，
// 在被配额挡住的时候不该付这笔成本。
func (a *Autopilot) eventTriggered(ctx context.Context, accountID string, now time.Time) (string, bool) {
	a.mu.Lock()
	if a.watch.fired == nil {
		a.watch.fired = map[string]string{}
	}
	if !a.watch.lastAt.IsZero() && now.Sub(a.watch.lastAt) < eventMinInterval {
		a.mu.Unlock()
		return "", false
	}
	day := TradingDay(now)
	if a.watch.countDay != day {
		a.watch.countDay = day
		a.watch.count = 0
		a.watch.fired = map[string]string{}
	}
	if a.watch.count >= eventMaxPerDay {
		a.mu.Unlock()
		return "", false
	}
	a.mu.Unlock()

	account, positions, err := a.service.AccountView(ctx, accountID)
	if err != nil || len(positions) == 0 {
		return "", false
	}
	quotes, err := a.service.positionQuotes(ctx, positions)
	if err != nil {
		// 行情取不到就什么都不做：没有价格判断异动，等于编造异动。
		a.logf("event=autopilot_event_quote_failed err=%v", err)
		return "", false
	}
	price := map[string]float64{}
	for _, quote := range quotes {
		if quote.Price > 0 {
			price[quote.Symbol] = quote.Price
		}
	}

	// 1. 组合级：与窗口内的基线比较。
	a.mu.Lock()
	baseline := a.watch.equity
	baselineAt := a.watch.equityAt
	a.watch.equity = account.Equity
	a.watch.equityAt = now
	a.mu.Unlock()
	// 基线为空或已超出窗口时只重置，不判断：没有可比的过去，就没有"跌了多少"。
	if baseline > 0 && !baselineAt.IsZero() && now.Sub(baselineAt) <= eventPortfolioWindow {
		if drop := (1 - account.Equity/baseline) * 100; drop >= eventPortfolioDropPercent {
			a.markEventFired(now)
			return fmt.Sprintf("组合权益 %.2f 分钟内从 %.2f 降至 %.2f（-%.2f%%），发起临时轮次",
				round2(now.Sub(baselineAt).Minutes()), baseline, account.Equity, drop), true
		}
	}

	// 2. 单票级：相对昨收的日内跌幅 + 权重门槛。
	for _, position := range positions {
		current := price[position.Symbol]
		if current <= 0 || position.Quantity <= 0 {
			continue
		}
		weight := position.WeightPercent(account.Equity)
		if weight < eventSingleWeightPercent {
			continue
		}
		previous := previousCloseOf(quotes, position.Symbol)
		if previous <= 0 {
			// 没有昨收就算不出日内跌幅。不用成本价替代：
			// 那算出来的是"持仓浮亏"，而浮亏到 -4% 时往往早已被止损线接走。
			continue
		}
		drop := (current/previous - 1) * 100
		if drop > -eventSingleDropPercent {
			continue
		}
		key := day + "|" + position.Symbol
		a.mu.Lock()
		already := a.watch.fired[key] != ""
		if !already {
			a.watch.fired[key] = day
		}
		a.mu.Unlock()
		if already {
			continue
		}
		a.markEventFired(now)
		return fmt.Sprintf("%s（权重 %.2f%%）日内跌 %.2f%% 至 %.2f，发起临时轮次",
			position.Symbol, weight, drop, current), true
	}

	// 3. 板块级：价格还没跌，但主力资金已经在撤。
	// 这一类早于价格信号：等板块指数跌下去再反应，持仓已经跟着跌了一截。
	if reason, ok := a.sectorFlowNegative(ctx, account, positions, now); ok {
		a.markEventFired(now)
		return reason, true
	}
	return "", false
}

// sectorFlowNegative 检查有没有持仓所在板块的主力资金转负。
//
// 只对权重够大的持仓判断：0.5% 的仓位即使板块崩了，对账户的影响也不值得
// 叫一次模型。匹配用包含关系（"银行" ↔ "银行Ⅱ"），因为持仓存的是行业大类，
// 而资金流给的是板块全名，两者很少逐字相同。命中多条时取最差的那条：
// 一个板块同时命中几个概念时，挑最好看的来自我安慰是最没意义的做法。
func (a *Autopilot) sectorFlowNegative(ctx context.Context, account Account, positions []Position, now time.Time) (string, bool) {
	provider := a.service.SectorFlow()
	if provider == nil {
		return "", false
	}
	a.mu.Lock()
	// 这里也要兜底初始化：本函数可能先于 eventTriggered 被调用（测试、
	// 或将来别处的探测），而 nil map 写入会直接 panic。
	if a.watch.fired == nil {
		a.watch.fired = map[string]string{}
	}
	flows, fetchedAt := a.watch.sectorFlow, a.watch.sectorFlowAt
	a.mu.Unlock()
	if len(flows) == 0 || now.Sub(fetchedAt) >= eventSectorFlowInterval {
		flows = a.fetchSectorFlow(ctx, provider)
		a.mu.Lock()
		a.watch.sectorFlow = flows
		a.watch.sectorFlowAt = now
		a.mu.Unlock()
	}
	if len(flows) == 0 {
		return "", false
	}

	day := TradingDay(now)
	for _, position := range positions {
		if position.Quantity <= 0 {
			continue
		}
		if position.WeightPercent(account.Equity) < eventSingleWeightPercent {
			continue
		}
		sector := strings.TrimSpace(position.Sector)
		if sector == "" {
			// 没有行业信息就不猜。用代码前缀或名称去猜板块，
			// 猜错的代价是发一轮"因为某个不相干板块跌了"的临时轮次。
			continue
		}
		ratio, ok := matchSectorFlow(flows, sector)
		if !ok || ratio > eventSectorFlowNegativeRatio {
			continue
		}
		key := "flow|" + day + "|" + sector
		a.mu.Lock()
		already := a.watch.fired[key] != ""
		if !already {
			a.watch.fired[key] = day
		}
		a.mu.Unlock()
		if already {
			continue
		}
		return fmt.Sprintf("%s 所属板块「%s」主力资金净占比 %.2f%%（转负），发起临时轮次",
			position.Symbol, sector, ratio), true
	}
	return "", false
}

// fetchSectorFlow 取行业与概念两个维度的资金流，合成"名称 → 主力净占比"。
// 任一维度失败就只用另一个维度的结果：缺一半数据也比完全没有好，
// 而两个都失败时返回空表，调用方会直接放弃判断。
func (a *Autopilot) fetchSectorFlow(ctx context.Context, provider SectorFlowProvider) map[string]float64 {
	flows := make(map[string]float64, eventSectorFlowDimensionLimit*2)
	for _, dimension := range []string{"industry", "concept"} {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rows, _, err := provider.SectorFlowRanking(callCtx, dimension, eventSectorFlowDimensionLimit)
		cancel()
		if err != nil {
			a.logf("event=autopilot_sector_flow_failed dimension=%s err=%v", dimension, err)
			continue
		}
		for _, row := range rows {
			if row.Name == "" {
				continue
			}
			flows[row.Name] = row.MainNetInflowRatio
		}
	}
	return flows
}

// matchSectorFlow 按包含关系匹配板块名，返回命中项里最差（最低）的净占比。
func matchSectorFlow(flows map[string]float64, sector string) (float64, bool) {
	worst, found := 0.0, false
	for name, ratio := range flows {
		if name != sector && !strings.Contains(name, sector) && !strings.Contains(sector, name) {
			continue
		}
		if !found || ratio < worst {
			worst, found = ratio, true
		}
	}
	return worst, found
}

// markEventFired 记账：次数 +1、记录时刻。间隔判定依赖 lastAt，
// 忘记更新它会让事件通道退化成每 15 秒一次。
func (a *Autopilot) markEventFired(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watch.lastAt = now
	a.watch.count++
}

// previousCloseOf 取某只标的的昨收。行情结构里的字段在两条路径上命名不同，
// 这里统一处理，避免调用方各写一份判断。
func previousCloseOf(quotes []foundation.Quote, symbol string) float64 {
	for _, quote := range quotes {
		if quote.Symbol == symbol && quote.PreviousClose > 0 {
			return quote.PreviousClose
		}
	}
	return 0
}
