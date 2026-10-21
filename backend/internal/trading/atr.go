package trading

// 波动率自适应的防守线。
//
// 固定百分比的止损有一个说不出口的问题：它对所有票一视同仁。
// 一只日波动 1.5% 的银行股和一只日波动 5% 的题材股，用同一个 -7%，
// 对前者是"真出事才触发"，对后者是"正常呼吸就触发"——后者会在一次普通回调里
// 被扫出局，然后在原地看着它涨回去。这不是模型判断错了，是止损距离错了。
//
// 这里用 ATR（真实波幅均值）替代固定百分比，做两件事：
//  1. 买入时校正止损距离：太近（<1.2 ATR，噪音就能打掉）往下放，
//     太远（>3.5 ATR，单笔风险敞口过大）往回收，缺失时按 2.2 ATR 兜底。
//  2. 持仓时按 ATR 上移止损（只上移，不下移）：价格走得越远，止损跟得越紧，
//     但永远保留 2.5 个 ATR 的呼吸空间。
//
// 三条边界：
//   - 没有日线数据时**什么都不做**，而不是用固定百分比假装算过。
//     调用方必须能从返回值看出"这次没算"。
//   - 校正后的止损仍受 MaxStopLossPercent 约束：ATR 只决定"多紧算合理"，
//     不决定"能亏多少"，后者是账户级的硬约束。
//   - 上移只发生在价格创新高之后（候选价高于现价止损），绝不下移。
//     下移止损等于把已经承诺的风险重新放大，比不设止损更糟。

import (
	"context"
	"fmt"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// atrProfile 是一只标的的波动率画像。
//
// 除 ATR 外还带上 MA20、20 日最高价与连涨天数：这三项与 ATR 用的是同一批日线，
// 多算它们不增加一次网络请求，却能让"现价处在什么位置"成为可判定的事实。
// 分开去取会让同一只票在一轮里拉两次日线，还可能拿到不同时点的两份数据。
type atrProfile struct {
	Symbol string
	ATR    float64
	Price  float64
	Bars   int
	At     time.Time
	// MA20 是最近 20 个交易日收盘均值，取不到 20 根时为 0。
	MA20 float64
	// High20 是最近 20 个交易日收盘最高值。用收盘价而非最高价：
	// 盘中影线创高又收回来，不构成"站上高位"，用最高价会把长上影误判成突破。
	High20 float64
	// ConsecutiveUp 是截至最后一根为止的连续上涨天数。
	ConsecutiveUp int
	// PrevClose 是最后一根的前一根收盘价，用于在没有行情快照时也能算出当日涨幅。
	PrevClose float64
}

// Valid 表示这份画像足以支撑一次止损校正。
func (p atrProfile) Valid() bool { return p.ATR > 0 && p.Price > 0 }

// ATRPercent 返回 ATR 占现价的百分比，用于日志与裁决消息。
func (p atrProfile) ATRPercent() float64 {
	if p.Price <= 0 {
		return 0
	}
	return round2(p.ATR / p.Price * 100)
}

const (
	atrPeriod = 14
	// atrCacheTTL 是画像的保鲜期。日线在收盘后才更新，
	// 30 分钟足够避免每轮循环重复拉取，又不至于跨过收盘还用旧值。
	atrCacheTTL = 30 * time.Minute
	// 止损距离的 ATR 倍数。三个数字不是调参调出来的，各自对应一种错误：
	//   atrStopMin：太近会被日常波动扫掉（小于 1.2 倍 ATR 时，一天内的正常反向波动就够触发）。
	//   atrStopTarget：正常入场距离，2.2 倍 ATR 大约覆盖两天的平均波幅。
	//   atrStopMax：太远则单笔亏损过大，超过 3.5 倍 ATR 的止损在这个账户上无法承受。
	atrStopMin    = 1.2
	atrStopTarget = 2.2
	atrStopMax    = 3.5
	// atrTrailMult 是持仓期追踪止损保留的波动空间。
	atrTrailMult = 2.5
)

// loadATR 取日线并计算 ATR。主源失败才用备源，双失败返回零值画像。
func (s *Service) loadATR(ctx context.Context, symbol string) atrProfile {
	if cached, ok := s.cachedATR(symbol); ok {
		return cached
	}
	s.mu.Lock()
	primary, fallback := s.kline, s.klineAlt
	s.mu.Unlock()
	bars := []foundation.KLine{}
	var primaryErr error
	if primary != nil {
		fetched, err := primary.KLine(ctx, symbol, "day", atrPeriod*3)
		if err == nil {
			bars = fetched
		} else {
			primaryErr = err
		}
	} else {
		primaryErr = fmt.Errorf("日线数据源未配置")
	}
	// 主源没有数据（不是报错）时也要试备源：某些源对个别代码返回空集而非错误。
	if len(cleanKLineBars(bars)) < atrPeriod+1 && fallback != nil {
		if fetched, err := fallback.KLine(ctx, symbol, "day", atrPeriod*3); err == nil && len(fetched) > 0 {
			bars = fetched
		}
	}
	bars = cleanKLineBars(bars)
	if len(bars) < atrPeriod+1 {
		if primaryErr != nil {
			s.logf("event=trading_atr_unavailable symbol=%s err=%v", symbol, primaryErr)
		}
		return atrProfile{Symbol: symbol, At: time.Now()}
	}
	profile := atrProfile{
		Symbol:        symbol,
		ATR:           round4(atrOf(bars, atrPeriod)),
		Price:         bars[len(bars)-1].Close,
		Bars:          len(bars),
		At:            time.Now(),
		MA20:          round4(meanClose(bars, entryMAPeriod)),
		High20:        round4(highestClose(bars, entryMAPeriod)),
		ConsecutiveUp: consecutiveUpDays(bars),
		PrevClose:     round4(bars[len(bars)-2].Close),
	}
	s.storeATR(profile)
	return profile
}

// meanClose 返回最近 n 根收盘均值。不足 n 根返回 0——宁可让调用方看到"算不出来"，
// 也不要用不足的窗口冒充 20 日均线：5 根均值挂在 MA20 的名义下会误导判断。
func meanClose(bars []foundation.KLine, n int) float64 {
	if n <= 0 || len(bars) < n {
		return 0
	}
	sum := 0.0
	for _, bar := range bars[len(bars)-n:] {
		sum += bar.Close
	}
	return sum / float64(n)
}

// highestClose 返回最近 n 根收盘最高值。用收盘价而不是盘中最高价：
// 长上影创高又收回来不属于"站在高位"，用最高价会把这种走势误判成突破。
func highestClose(bars []foundation.KLine, n int) float64 {
	if n <= 0 || len(bars) < n {
		return 0
	}
	high := bars[len(bars)-n].Close
	for _, bar := range bars[len(bars)-n:] {
		if bar.Close > high {
			high = bar.Close
		}
	}
	return high
}

// consecutiveUpDays 数截至最后一根的连续上涨天数。
// 平盘即中断：连涨的定义是"每天都在创新高"，持平不算。
func consecutiveUpDays(bars []foundation.KLine) int {
	streak := 0
	for i := len(bars) - 1; i > 0; i-- {
		if bars[i].Close > bars[i-1].Close {
			streak++
			continue
		}
		break
	}
	return streak
}

func (s *Service) cachedATR(symbol string) (atrProfile, bool) {
	s.atrMu.Lock()
	defer s.atrMu.Unlock()
	profile, ok := s.atrCache[symbol]
	if !ok || time.Since(profile.At) > atrCacheTTL {
		return atrProfile{}, false
	}
	// 空画像也缓存（有效期同 TTL）：数据源没有这只票的日线时，
	// 不缓存会让每轮都为同一只票重试一次，把一次失败变成持续的延迟。
	return profile, true
}

func (s *Service) storeATR(profile atrProfile) {
	s.atrMu.Lock()
	defer s.atrMu.Unlock()
	if s.atrCache == nil {
		s.atrCache = map[string]atrProfile{}
	}
	s.atrCache[profile.Symbol] = profile
}

// AdjustDraftStops 用 ATR 校正模型给出的止损距离，返回校正记录。
//
// 只在买入建议上动手，且只改 stop_loss_price——目标权重、股数、方向一概不碰。
// 原因：止损距离是"这只票正常波动有多大"的客观问题，模型的主观判断
// （为什么买、买多少）不应该被波动率参数改写。
func (s *Service) AdjustDraftStops(ctx context.Context, draft *DraftPlan, snapshot *Snapshot, rules RiskRules) []policy.Verdict {
	rules = rules.Normalize()
	verdicts := make([]policy.Verdict, 0, 4)
	quotes := map[string]float64{}
	for _, quote := range snapshot.Quotes {
		if quote.Price > 0 {
			quotes[quote.Symbol] = quote.Price
		}
	}
	if snapshot.ATRs == nil {
		snapshot.ATRs = map[string]float64{}
	}
	// 单轮最多校正 8 笔，与 MaxOrders 同量级；再多说明已经不是正常计划。
	for index := range draft.Orders {
		if index >= rules.MaxOrders {
			break
		}
		order := &draft.Orders[index]
		if order.Side != "buy" {
			continue
		}
		price := quotes[normalizeSymbolOrEmpty(order.Symbol)]
		if price <= 0 {
			continue
		}
		profile := s.loadATR(ctx, order.Symbol)
		if !profile.Valid() {
			continue
		}
		// 波动率画像同时记进快照：定价（仓位）与防守（止损）必须用同一个数，
		// 分别去取一次会在两次请求之间拿到不同的 ATR，复盘时无法解释。
		symbol := normalizeSymbolOrEmpty(order.Symbol)
		snapshot.ATRs[symbol] = profile.ATRPercent()
		// 位置事实顺手用同一份画像算出来：模型本轮可能买进一只不在候选池里的票
		// （比如加仓已持仓），那种票在提示词阶段没被算过位置。
		// 这里补上，裁决时才有一份可信的位置，而不是"未知放行"。
		if snapshot.EntryPositions == nil {
			snapshot.EntryPositions = map[string]EntryPosition{}
		}
		if _, exists := snapshot.EntryPositions[symbol]; !exists {
			snapshot.EntryPositions[symbol] = entryPositionOf(quoteOf(snapshot.Quotes, symbol), profile)
		}
		adjusted, note := atrAdjustStop(order.StopLossPrice, price, profile.ATR, rules.MaxStopLossPercent)
		if note == "" {
			continue
		}
		order.StopLossPrice = adjusted
		verdicts = append(verdicts, policy.Verdict{
			Code: policy.CodeRiskStopATR, Level: policy.LevelAdjust, Scope: policy.ScopeOrder,
			Field: fmt.Sprintf("orders[%d].stop_loss_price", index), Symbol: order.Symbol,
			Metric: round2((price - adjusted) / price * 100), Limit: profile.ATRPercent(),
			Message: fmt.Sprintf("%s 止损按 ATR 校正：%s（ATR %.2f，占现价 %.2f%%）",
				order.Symbol, note, profile.ATR, profile.ATRPercent()),
		})
	}
	return verdicts
}

// atrAdjustStop 计算校正后的止损价。返回空 note 表示"不需要改"。
//
// 优先级：缺失 → 兜底；太近 → 放宽；太远 → 收紧。三个分支都受最大止损比例限制，
// 因为 ATR 能说明"波动多大"，但能不能承受这笔亏损由账户决定。
func atrAdjustStop(current, price, atr, maxStopPercent float64) (float64, string) {
	if price <= 0 || atr <= 0 {
		return current, ""
	}
	floor := price * (1 - maxStopPercent/100)
	if floor <= 0 {
		floor = price * 0.9
	}
	clamp := func(value float64) float64 {
		if value < floor {
			value = floor
		}
		if value >= price {
			value = price * 0.99
		}
		return round2(value)
	}
	distance := price - current
	switch {
	case current <= 0:
		adjusted := clamp(price - atrStopTarget*atr)
		return adjusted, fmt.Sprintf("未给止损，按 %.1f 倍 ATR 设为 %.2f", atrStopTarget, adjusted)
	case distance < atrStopMin*atr:
		adjusted := clamp(price - atrStopTarget*atr)
		if adjusted >= current {
			return current, ""
		}
		return adjusted, fmt.Sprintf("原止损 %.2f 距离仅 %.2f%%（不足 %.1f 倍 ATR），放宽至 %.2f",
			current, round2(distance/price*100), atrStopMin, adjusted)
	case distance > atrStopMax*atr:
		adjusted := clamp(price - atrStopMax*atr)
		if adjusted <= current {
			return current, ""
		}
		return adjusted, fmt.Sprintf("原止损 %.2f 距离 %.2f%%（超过 %.1f 倍 ATR），收紧至 %.2f",
			current, round2(distance/price*100), atrStopMax, adjusted)
	}
	return current, ""
}

// TrailStopsByATR 按 ATR 上移全部持仓的止损线，返回被上移的持仓数。
//
// 触发条件不是"时间到了"，而是"价格创新高之后止损还留在原地"。
// 没有这条，一只涨了 20% 的票，止损仍在成本下方 7%，
// 一次回撤会把浮盈全部还回去——这正是记忆里"峰值回吐"的来源。
func (s *Service) TrailStopsByATR(ctx context.Context, accountID string) (int, error) {
	accountID = s.accountID(accountID)
	positions, err := s.store.ListPositions(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if len(positions) == 0 {
		return 0, nil
	}
	updated := 0
	now := time.Now().UTC()
	for _, position := range positions {
		if position.Quantity <= 0 || position.LastPrice <= 0 {
			continue
		}
		profile := s.loadATR(ctx, position.Symbol)
		if !profile.Valid() {
			continue
		}
		candidate := round2(position.LastPrice - atrTrailMult*profile.ATR)
		// 只上移，且要求至少抬高 0.5% 才写库：
		// 差几分钱就更新一次，会让事件流被无意义的微调淹没。
		if candidate <= position.StopLossPrice || candidate >= position.LastPrice*0.995 {
			continue
		}
		if (candidate-position.StopLossPrice)/position.StopLossPrice < 0.005 {
			continue
		}
		previous := position.StopLossPrice
		if err := s.store.SetStopLoss(ctx, accountID, position.Symbol, candidate, now); err != nil {
			s.logf("event=trading_atr_trail_failed symbol=%s err=%v", position.Symbol, err)
			continue
		}
		updated++
		_ = s.store.AppendEvent(ctx, Event{
			AccountID: accountID,
			Severity:  EventSeverityInfo,
			Kind:      EventTrailStopRaised,
			Symbol:    position.Symbol,
			Message: fmt.Sprintf("%s 止损随波动率上移：%.2f → %.2f（%.1f 倍 ATR %.2f，现价 %.2f）",
				position.Symbol, previous, candidate, atrTrailMult, profile.ATR, position.LastPrice),
			Payload:   eventPayload(map[string]any{"from": previous, "to": candidate, "atr": profile.ATR}),
			CreatedAt: now,
		})
		s.logf("event=trading_atr_trail symbol=%s from=%.2f to=%.2f atr=%.4f", position.Symbol, previous, candidate, profile.ATR)
	}
	return updated, nil
}

// ATRPercentMap 返回各持仓的 ATR 占现价百分比（键为规范化后的代码）。
//
// 取不到的标的不进 map，而不是塞 0：0 会被下游当成"波动为零"，
// 从而算出 0 宽的移动止损。缺失必须是"没有这个键"，由调用方退回固定参数。
func (s *Service) ATRPercentMap(ctx context.Context, positions []Position) map[string]float64 {
	if len(positions) == 0 {
		return nil
	}
	result := make(map[string]float64, len(positions))
	for _, position := range positions {
		if position.Quantity <= 0 {
			continue
		}
		symbol := normalizeSymbolOrEmpty(position.Symbol)
		if symbol == "" {
			continue
		}
		profile := s.loadATR(ctx, symbol)
		if !profile.Valid() {
			continue
		}
		result[symbol] = profile.ATRPercent()
	}
	return result
}

// SetStopLoss 只更新止损价，并对"上移"加一条 SQL 级保护。
// WHERE 条件里带 stop_loss_price < ? 而不是先读后写：ATR 追踪与人工修改
// 可能同时发生，先读后写会把人工下调的止损又抬回去。
func (s *Store) SetStopLoss(ctx context.Context, accountID, symbol string, stop float64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE trading_positions SET stop_loss_price=?, updated_at=?
		WHERE account_id=? AND symbol=? AND stop_loss_price<?`, round2(stop), nowText(now), accountID, symbol, round2(stop))
	return err
}

// normalizeSymbolOrEmpty 把模型给的代码归一成快照里的键。
// 归一失败返回空串，调用方据此跳过——用一个猜出来的代码去查行情，
// 查到的可能是另一只票的波动率。
func normalizeSymbolOrEmpty(value string) string {
	symbol, err := foundation.NormalizeSymbol(value)
	if err != nil {
		return ""
	}
	return symbol.Canonical
}

// atrDistanceOK 是给测试与诊断用的判定：止损距离是否落在合理区间。
func atrDistanceOK(price, stop, atr float64) bool {
	if price <= 0 || atr <= 0 || stop <= 0 {
		return false
	}
	distance := price - stop
	return distance >= atrStopMin*atr && distance <= atrStopMax*atr
}
