package trading

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
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
//
// 窗口取 45 分钟而不是排班间隔（15 分钟）：一轮的结果本身就有时效性，
// 但 15 分钟的窗口在 900 秒排班下只能覆盖 1 轮，等于"第二轮才可能跳过"，
// 实际命中率接近零。45 分钟覆盖 3 轮，既能让横盘时段真正省下调用，
// 又不会拿一小时前的判断当成现在的判断。
const noChangeWindow = 45 * time.Minute

// ErrNoMaterialChange 表示本轮输入与上一次模型调用时完全一致。
// 它不是故障：调用方应把它记成"跳过"，而不是"失败"。
var ErrNoMaterialChange = errors.New("行情与账户在上一次调用后没有实质变化，本轮不调用模型")

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
		// 价格按 0.5% 的台阶量化，成交量按 10% 的台阶量化。
		//
		// 精确到分是上一版的做法，结果是"指纹永远在变"：39 只票里只要有一只
		// 跳了一分钱，整轮就被判成有变化，于是每轮都调模型，指纹检测形同虚设。
		// A 股 T+1 下，0.5% 以内的日内抖动改变不了任何一笔可执行的决策，
		// 而成交量是累计值、天生单调递增，不量化就永远是"新数据"。
		write("q=%s|%d|%d|%.2f", quote.Symbol, priceStep(quote.Price, quote.PreviousClose),
			volumeStep(quote.Volume), quote.PreviousClose)
	}

	positions := append([]Position(nil), snapshot.Positions...)
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
	for _, position := range positions {
		// 持仓现价同样按 0.5% 台阶量化：以成本为基准，
		// 因为"浮盈从 3.1% 变 3.4%"不改变任何决策，而"从 3% 变 -0.5%"会。
		write("p=%s|%d|%d|%d|%.2f|%d", position.Symbol, position.Quantity,
			position.Available, position.ReservedQuantity, position.AverageCost,
			priceStep(position.LastPrice, position.AverageCost))
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

// priceStep 把"当前价相对基准价"的变化量化成台阶编号。
// 台阶宽度 0.5%：同一步阶内视为没有变化。
// 基准价缺失（0）时退化成绝对价格本身，宁可多调一次模型，也不把不同标的混成一步。
func priceStep(price, base float64) int {
	if base <= 0 || price <= 0 {
		return int(math.Round(price * 100))
	}
	return int(math.Round((price/base - 1) / priceChangeStep))
}

// volumeStep 把成交量量化成台阶编号。台阶宽度 10%：
// 成交量是当日累计值，只要不量化就永远是"新数据"，指纹检测会因此完全失效。
func volumeStep(volume float64) int {
	if volume <= 0 {
		return 0
	}
	return int(math.Round(math.Log(volume/1e6) / math.Log(1+volumeChangeStep)))
}

// priceChangeStep / volumeChangeStep 是"算实质变化"的门槛。
// 定得太小等于没量化（每轮都在变），定得太大等于迟钝（跌了 3% 还判成没变）。
//
// 1% 的价格台阶对应的是"A 股 T+1 下改变得了决策的最小变动"：
// 止损、止盈、时间止损、失效条件全部由代码按分钟执行，不经过模型；
// 模型只在排班轮次上决定"买不买、买哪个"，1% 以内的日内漂移改不了这个结论。
//
// 台阶量化是边界敏感的：0.4% 的抖动如果正好跨过档位边界，仍会被算成变化。
// 这不可避免，但比起"精确到分"（每次抖动都跨档），它把命中率从 0 提到多数。
const (
	priceChangeStep  = 0.01
	volumeChangeStep = 0.10
)

// baseline 记录上一次真正调用模型时的指纹。
//
// 内存为主、KV 为辅：指纹本身落一份到 trading_state，进程崩溃重启后
// 可以沿用（20 分钟窗口检查天然兜住停机期间的不确定性——停机超过窗口，
// age 超界照样会问模型；只有几秒级重启才真正受益）。早先版本刻意只放
// 内存，理由是"停机期间发生了什么推不出来"，但那个顾虑已经被窗口
// 检查覆盖，持久化不会引入"拿旧结论应付新行情"的风险。
type changeBaseline struct {
	fingerprint string
	at          time.Time
	planID      string
}

const changeBaselineKey = "change_baseline"

// loadChangeBaseline 从 KV 恢复上一次的指纹基线。
// 任何解析失败都按"没有基线"处理：状态丢了能自愈，代价是多问一次模型。
func (s *Service) loadChangeBaseline() (changeBaseline, bool) {
	if s.store == nil {
		return changeBaseline{}, false
	}
	raw, ok, err := s.store.GetState(context.Background(), changeBaselineKey)
	if err != nil || !ok || raw == "" {
		return changeBaseline{}, false
	}
	parts := strings.SplitN(raw, "|", 3)
	if len(parts) != 3 {
		return changeBaseline{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return changeBaseline{}, false
	}
	return changeBaseline{fingerprint: parts[0], at: at, planID: parts[2]}, true
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
	if baseline.fingerprint == "" {
		// 重启后内存基线为空：从 KV 恢复一次。恢复失败就当没有基线。
		if loaded, ok := s.loadChangeBaseline(); ok {
			baseline = loaded
			s.change = loaded
		}
	}
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
	// 落一份到 KV 供重启恢复。写失败只记日志：它不影响本轮决策，
	// 最坏后果是重启后多问一次模型。
	if s.store != nil {
		if err := s.store.SetState(context.Background(), changeBaselineKey,
			fingerprint+"|"+now.Format(time.RFC3339Nano)+"|"+planID); err != nil {
			s.logf("event=change_baseline_persist_failed err=%v", err)
		}
	}
}
