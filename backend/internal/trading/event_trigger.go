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
	// 4% 在 A 股是"明显异动"而不是"正常波动"，低于它触发会变成噪音。
	eventSingleDropPercent = 4.0
	// eventSingleWeightPercent 是被考虑的单票权重下限。
	// 0.5% 的仓位跌 8% 对账户影响不到 0.05%，不值得为它调一次模型。
	eventSingleWeightPercent = 3.0
	// eventPortfolioDropPercent 是组合权益相对上次观察的跌幅阈值。
	// 单票阈值抓不到的"多只同时阴跌"由它兜底。
	eventPortfolioDropPercent = 1.5
	// eventPortfolioWindow 是组合跌幅的观察窗口。
	// 只在窗口内比较：拿上午 10 点的权益和下午 2 点比，得到的必然是大跌。
	eventPortfolioWindow = 15 * time.Minute
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
	return "", false
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
