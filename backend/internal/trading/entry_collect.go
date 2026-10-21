package trading

// 入场位置的批量取数。
//
// 为什么要在提示词之前就把位置算出来：实测 251 步决策里 get_kline 被调用 58 次，
// 但"个股位置"这个视角仍然缺席——模型拿日线是在看形态，不是在回答
// "现价离 20 日均线多远"。把位置变成候选池里的一列数字，
// 比指望它每次主动去查要可靠：数据摆在那儿，不用它也得看见。
//
// 成本控制：
//   - 复用 loadATR 的 30 分钟缓存，同一只票一轮内只取一次，跨轮也不再重复取；
//   - 并发 4 路，30 只候选约 3 秒；串行会是 12 秒；
//   - 整体 12 秒硬超时，超时的票按"位置未知"处理，不阻断本轮计划。

import (
	"context"
	"sync"
	"time"

	"easy-stock/backend/internal/foundation"
)

const (
	entryFetchWorkers = 4
	entryFetchTimeout = 12 * time.Second
)

// collectEntryPositions 批量取位置。返回的 map 一定非 nil（可能为空），
// 空 map 与 nil 的区别很重要：nil 表示"这批快照本就不带位置"，
// 空 map 表示"带了位置但一只都没取到"，后者会被裁决记成 warn。
func (s *Service) collectEntryPositions(ctx context.Context, symbols []string, quotes []foundation.Quote) map[string]EntryPosition {
	bySymbol := make(map[string]foundation.Quote, len(quotes))
	for _, quote := range quotes {
		bySymbol[quote.Symbol] = quote
	}
	unique := make([]string, 0, len(symbols))
	seen := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		symbol = normalizeSymbolOrEmpty(symbol)
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		unique = append(unique, symbol)
	}
	positions := make(map[string]EntryPosition, len(unique))
	if len(unique) == 0 {
		return positions
	}

	ctx, cancel := context.WithTimeout(ctx, entryFetchTimeout)
	defer cancel()

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, entryFetchWorkers)
	for _, symbol := range unique {
		wg.Add(1)
		go func(symbol string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			profile := s.loadATR(ctx, symbol)
			if !profile.Valid() || profile.MA20 <= 0 {
				return
			}
			position := entryPositionOf(bySymbol[symbol], profile)
			mu.Lock()
			positions[symbol] = position
			mu.Unlock()
		}(symbol)
	}
	wg.Wait()
	if len(positions) < len(unique) {
		s.logf("event=trading_entry_position_partial got=%d want=%d", len(positions), len(unique))
	}
	return positions
}
