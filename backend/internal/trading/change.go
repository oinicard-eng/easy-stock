package trading

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 变化检测：没有实质变化的一轮不调模型。
//
// 自动循环最短 30 秒一轮，一天最多四十轮。但行情是按分钟级跳的，
// 三分钟内价格只动了 0.05%、持仓与挂单都没变的情况下，再问一次模型
// 得到的答案与上一轮几乎必然相同——这既浪费 token，也会让"模型一天下了
// 二十次同样的建议"变成噪声。
//
// 三条约束，缺一不可：
//
//  1. 必须在快照构建之后判定。快照之前没有任何可比对的事实，
//     用"距上次多久"来跳过，会在行情剧烈波动时正好漏掉最该问的一轮。
//  2. 指纹只包含"会改变结论的输入"：价格、持仓、可用资金、在途挂单、
//     近期被拒记录、市场状态。时间戳、延迟、token 数一律不进指纹，
//     否则每一轮都"有变化"，检测形同虚设。
//  3. 只对定时触发的轮次生效。人工点"生成计划"时跳过会让人以为按钮坏了。
const noChangeWindow = 20 * time.Minute

// ErrNoMaterialChange 表示本轮输入与上一次模型调用时完全一致。
// 它不是故障：调用方应把它记成"跳过"，而不是"失败"。
var ErrNoMaterialChange = errors.New("行情与账户在 20 分钟内没有实质变化，本轮不调用模型")

// SnapshotFingerprint 把一次判断的全部输入压成一个短哈希。
//
// 所有集合都先排序再入哈希：快照里的行情顺序取决于数据源返回顺序，
// 不排序会让同一份行情算出两个指纹，检测永远不生效。
func SnapshotFingerprint(snapshot Snapshot, pending []Order, blocks []BlockedIntent) string {
	h := sha256.New()
	write := func(format string, args ...any) {
		fmt.Fprintf(h, format+"\n", args...)
	}
	write("equity=%.2f cash=%.2f reserved=%.2f pending=%.2f",
		snapshot.Equity, snapshot.Cash, snapshot.ReservedCash, snapshot.PendingBuyValue)

	quotes := append([]foundation.Quote(nil), snapshot.Quotes...)
	sort.Slice(quotes, func(i, j int) bool { return quotes[i].Symbol < quotes[j].Symbol })
	for _, quote := range quotes {
		// 价格保留两位：A 股最小变动单位是 0.01，第三位小数只是数据源的噪声。
		write("q=%s|%.2f|%.2f|%.0f", quote.Symbol, quote.Price, quote.PreviousClose, quote.Volume)
	}

	positions := append([]Position(nil), snapshot.Positions...)
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
	for _, position := range positions {
		write("p=%s|%d|%d|%d|%.2f|%.2f", position.Symbol, position.Quantity,
			position.Available, position.ReservedQuantity, position.AverageCost, position.LastPrice)
	}

	symbols := make([]string, 0, len(snapshot.OpenBuyValue))
	for symbol := range snapshot.OpenBuyValue {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		write("ob=%s|%.2f", symbol, snapshot.OpenBuyValue[symbol])
	}

	orders := append([]Order(nil), pending...)
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	for _, order := range orders {
		write("o=%s|%s|%s|%d|%.2f", order.ID, order.Symbol, order.Side, order.Quantity, order.LimitPrice)
	}

	// 被拒记录要进指纹：模型上一轮被"行业集中度"拦了，这一轮即使行情没变，
	// 它也需要看到"同样的建议又被拦了一次"，否则它永远不会改。
	sortedBlocks := append([]BlockedIntent(nil), blocks...)
	sort.Slice(sortedBlocks, func(i, j int) bool {
		if sortedBlocks[i].Symbol == sortedBlocks[j].Symbol {
			return sortedBlocks[i].Code < sortedBlocks[j].Code
		}
		return sortedBlocks[i].Symbol < sortedBlocks[j].Symbol
	})
	for _, block := range sortedBlocks {
		write("b=%s|%s|%s|%d", block.Symbol, block.Side, block.Code, block.Repeat)
	}

	write("mkt=%s|%t|%.2f", snapshot.Market.Regime, snapshot.Market.Evaluated, snapshot.Market.Breadth.MedianChange)
	// 冷静期进指纹：止损之后的那一轮是"最该重新问一次"的一轮。
	// 漏掉它会出现最坏的组合——行情没动、持仓没动，但可买范围已经变了，
	// 系统却认为"什么都没变"，继续沿用止损前的结论。
	cooldownSymbols := make([]string, 0, len(snapshot.Cooldowns.Symbols))
	for symbol := range snapshot.Cooldowns.Symbols {
		cooldownSymbols = append(cooldownSymbols, symbol)
	}
	sort.Strings(cooldownSymbols)
	for _, symbol := range cooldownSymbols {
		write("cd=%s|%s", symbol, snapshot.Cooldowns.Symbols[symbol].Until.Format(time.RFC3339))
	}
	cooldownSectors := make([]string, 0, len(snapshot.Cooldowns.Sectors))
	for sector := range snapshot.Cooldowns.Sectors {
		cooldownSectors = append(cooldownSectors, sector)
	}
	sort.Strings(cooldownSectors)
	for _, sector := range cooldownSectors {
		write("cs=%s|%s", sector, snapshot.Cooldowns.Sectors[sector].Until.Format(time.RFC3339))
	}
	write("universe=%s", strings.Join(snapshot.Universe, ","))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:16]
}

// baseline 记录上一次真正调用模型时的指纹。
//
// 它是内存值而不是数据库标记：进程重启后重新问一次模型是正确行为——
// 中间停机的那段时间发生了什么，只靠指纹是推不出来的。
type changeBaseline struct {
	fingerprint string
	at          time.Time
	planID      string
}

// shouldSkipNoChange 判断本轮是否可以跳过模型调用。
// 返回的第二个值是可以直接写进计划错误信息的说明。
func (s *Service) shouldSkipNoChange(fingerprint string, now time.Time, enabled bool) (bool, string) {
	if !enabled || fingerprint == "" {
		return false, ""
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	baseline := s.change
	if baseline.fingerprint == "" || baseline.fingerprint != fingerprint {
		return false, ""
	}
	age := now.Sub(baseline.at)
	if age < 0 || age >= noChangeWindow {
		return false, ""
	}
	return true, fmt.Sprintf("行情与账户自 %s 起无实质变化（已 %.0f 分钟），本轮不调用模型，沿用上一轮结论 %s",
		baseline.at.In(time.FixedZone("CST", 8*3600)).Format("15:04"),
		age.Minutes(), baseline.planID)
}

// recordChange 在模型调用成功后记下指纹。
//
// 只在成功后记录：模型超时、解析失败时若也记指纹，下一轮会被判成"无变化"，
// 于是一次失败的调用会连带把接下来 20 分钟的决策全部静音。
func (s *Service) recordChange(fingerprint, planID string, now time.Time) {
	if fingerprint == "" {
		return
	}
	s.changeMu.Lock()
	s.change = changeBaseline{fingerprint: fingerprint, at: now, planID: planID}
	s.changeMu.Unlock()
}
