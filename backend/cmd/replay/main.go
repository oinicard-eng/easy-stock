// Command replay 用真实历史数据回放选股链路，回答"这套筛选到底有没有用"。
//
// 它不调用模型，也不碰账户：只把「全市场快照 → 打分 → 位置筛 → 分散约束」
// 这一段搬到历史日期上重跑，然后用真实的次日开盘价买入、N 日后收盘卖出，
// 量出每一版池子的前瞻收益。这是目前唯一能自证"改好了还是改坏了"的手段——
// 实盘样本只有 9 笔，而且那 9 笔本身是收盘后假成交的产物，基线不可信。
//
// 口径必须说清（否则数字会被误读）：
//
//  1. **次日开盘入场**。委托不会在当日收盘成交；用收盘价算前瞻收益会系统性
//     高估，因为涨幅大的票次日往往高开，那部分溢价我们吃不到。
//  2. **前复权价**。历史价被按今日的除权因子折算过，所以百分比变化正确、
//     分红视同再投入；但"股价低于 2 元"这类绝对价格门槛会被折价扭曲，
//     因此这里把 equity 设为 0，跳过"单手金额占比"那条约束。
//  3. **有一定的存活偏差**。标的池取自当前仍在交易的名单，退市股不在其中。
//     这会让绝对收益偏乐观，但两版用的是同一份名单，比较结论仍然成立。
//  4. **行业分类用的是当前分类**，套到历史上。分散约束因此是近似的。
//  5. 不含交易成本、印花税与冲击成本。两版都不含，不改变相对比较。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/providers/eastmoney"
	"easy-stock/backend/internal/providers/quantmind"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
	"easy-stock/backend/internal/trading/universe"
)

type bar struct {
	Date   string  `json:"d"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
	Amount float64 `json:"a"`
}

type series struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Industry string `json:"industry"`
	Bars     []bar  `json:"bars"`
}

type options struct {
	symbols     int
	concurrency int
	hold        int
	limit       int
	warmup      int
	cacheDir    string
	baseURL     string
	tenant      string
	user        string
	pass        string
}

func main() {
	opt := options{}
	flag.IntVar(&opt.symbols, "symbols", 400, "参与回放的标的数量（按代码顺序等距抽样）")
	flag.IntVar(&opt.concurrency, "concurrency", 6, "拉取日线的并发数")
	flag.IntVar(&opt.hold, "hold", 5, "持仓天数（次日开盘买入，N 日后收盘卖出）")
	flag.IntVar(&opt.limit, "limit", 30, "候选池目标数量")
	flag.IntVar(&opt.warmup, "warmup", 25, "首日之前需要的最少预热根数")
	flag.StringVar(&opt.cacheDir, "cache", ".runtime/replay", "日线缓存目录")
	flag.StringVar(&opt.baseURL, "url", envOr("A_STOCK_QUANTMIND_URL", "http://192.168.5.6:8000"), "QuantMind 地址")
	flag.StringVar(&opt.tenant, "tenant", envOr("A_STOCK_QUANTMIND_TENANT", ""), "QuantMind 租户")
	flag.StringVar(&opt.user, "user", envOr("A_STOCK_QUANTMIND_USER", ""), "QuantMind 账号")
	flag.StringVar(&opt.pass, "pass", envOr("A_STOCK_QUANTMIND_PASSWORD", ""), "QuantMind 密码")
	flag.Parse()

	if err := run(opt); err != nil {
		fmt.Fprintf(os.Stderr, "回放失败：%v\n", err)
		os.Exit(1)
	}
}

func run(opt options) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	client, err := quantmind.NewClient(opt.tenant, opt.user, opt.pass, quantmind.WithBaseURL(opt.baseURL))
	if err != nil {
		return err
	}
	lists, err := loadSymbols(ctx, opt.symbols)
	if err != nil {
		return err
	}
	fmt.Printf("标的池 %d 只（当前在市、非 ST、非北交所），开始拉取日线…\n", len(lists))

	seriesList := fetchAll(ctx, client, lists, opt.cacheDir, opt.concurrency)
	if len(seriesList) < 30 {
		return fmt.Errorf("可用日线只有 %d 只，样本不足以得出结论", len(seriesList))
	}
	fmt.Printf("日线就绪 %d 只，开始逐日回放…\n\n", len(seriesList))

	report := replay(seriesList, opt)
	printReport(report, opt)
	return nil
}

// ---------------------------------------------------------------- 标的与数据

type symInfo struct {
	Symbol   string
	Name     string
	Industry string
}

func loadSymbols(ctx context.Context, want int) ([]symInfo, error) {
	client := eastmoney.NewClient()
	entries, err := client.StockCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取全市场目录失败：%w", err)
	}
	filtered := make([]symInfo, 0, len(entries))
	for _, entry := range entries {
		symbol := strings.ToUpper(strings.TrimSpace(entry.Symbol))
		name := strings.TrimSpace(entry.Name)
		if symbol == "" || strings.HasSuffix(symbol, ".BJ") {
			continue
		}
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "ST") || strings.Contains(name, "退") {
			continue
		}
		filtered = append(filtered, symInfo{Symbol: symbol, Name: name, Industry: strings.TrimSpace(entry.Industry)})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Symbol < filtered[j].Symbol })
	if want > 0 && len(filtered) > want {
		// 等距抽样：按代码顺序取，避免"只挑大盘股"带来的隐性选择偏差。
		step := float64(len(filtered)) / float64(want)
		sampled := make([]symInfo, 0, want)
		for index := 0; index < want; index++ {
			sampled = append(sampled, filtered[int(float64(index)*step)])
		}
		return sampled, nil
	}
	return filtered, nil
}

func fetchAll(ctx context.Context, client *quantmind.Client, lists []symInfo, cacheDir string, concurrency int) []series {
	if concurrency <= 0 {
		concurrency = 4
	}
	dir := filepath.Join(cacheDir, "klines")
	_ = os.MkdirAll(dir, 0o755)

	out := make([]series, len(lists))
	var wg sync.WaitGroup
	slots := make(chan struct{}, concurrency)
	for index, info := range lists {
		wg.Add(1)
		go func(index int, info symInfo) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			bars, err := loadSeries(ctx, client, dir, info.Symbol)
			if err != nil || len(bars) < 60 {
				return
			}
			out[index] = series{Symbol: info.Symbol, Name: info.Name, Industry: info.Industry, Bars: bars}
		}(index, info)
	}
	wg.Wait()

	kept := make([]series, 0, len(out))
	for _, item := range out {
		if item.Symbol != "" {
			kept = append(kept, item)
		}
	}
	return kept
}

func loadSeries(ctx context.Context, client *quantmind.Client, dir, symbol string) ([]bar, error) {
	path := filepath.Join(dir, symbol+".json")
	if raw, err := os.ReadFile(path); err == nil {
		var bars []bar
		if json.Unmarshal(raw, &bars) == nil && len(bars) > 0 {
			return bars, nil
		}
	}
	klines, err := client.KLine(ctx, symbol, "day", 750)
	if err != nil {
		return nil, err
	}
	bars := make([]bar, 0, len(klines))
	for _, item := range klines {
		bars = append(bars, bar{
			Date:   item.Time.Format("2006-01-02"),
			Open:   item.Open,
			High:   item.High,
			Low:    item.Low,
			Close:  item.Close,
			Volume: item.Volume,
			Amount: item.Amount,
		})
	}
	if raw, err := json.Marshal(bars); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
	return bars, nil
}

// ---------------------------------------------------------------- 回放主体

type variant struct {
	name string
	// picks 是每次选出的候选数，rets 是对应的前瞻收益（次日开盘 → N 日后收盘）。
	picks int
	rets  float64
	// topRets 只统计每天排名前 5 的标的，更接近"实际会买的那几只"。
	topPicks int
	topRets  float64
	// positive 是收益为正的笔数，用于算胜率。
	positive int
}

type report struct {
	days       int
	start, end string
	baseline   variant
	filtered   variant
	// skippedDays 记录"改后清空、改前照买"的那些日子在改前版本下的表现。
	// 这是整组回放里最有说服力的一项：如果这些日子改前就是亏的，
	// 那么"今天没有候选"不是系统的故障，而是它在拒绝一笔坏交易。
	skippedDays variant
	excluded    int
	// emptyDays 是改后候选池为空的天数（含市场状态禁开仓的情形）。
	emptyDays int
	// positionEmptyDays 是"改前有得选、改后一只都不剩"的天数——
	// 与 emptyDays 分开统计，否则会把"市场恐慌不开仓"算到位置筛选头上。
	positionEmptyDays int
}

func replay(list []series, opt options) report {
	dates, index := calendarOf(list)
	result := report{days: 0, start: dates[0], end: dates[len(dates)-1]}
	result.baseline.name = "仅打分（改前）"
	result.filtered.name = "打分+位置筛选（改后）"
	result.skippedDays.name = "改后清空的日子里，改前"

	rules := policy.Default().Normalize()
	limits := universe.EntryLimits{
		MaxChangePercent: rules.MaxEntryChangePercent,
		MaxMA20Percent:   rules.MaxEntryMA20Percent,
		MaxUpDays:        rules.MaxEntryUpDays,
	}

	for dayIndex := opt.warmup; dayIndex+opt.hold+1 < len(dates); dayIndex++ {
		entries := snapshotAt(list, index, dayIndex)
		if len(entries) < 50 {
			// 样本太薄时市场认知没有意义，跳过而不是给一个假结论。
			continue
		}
		market := cognition.View(entries, mustDate(dates[dayIndex]))

		baseOptions := universe.DefaultOptions()
		baseOptions.Limit = opt.limit
		// equity 置 0：前复权价会把"单手金额占比"这条约束扭成噪音。
		baseOptions.Equity = 0
		boardBase := universe.Build(entries, market, baseOptions)

		filteredOptions := baseOptions
		filteredOptions.EntryLimits = limits
		filteredOptions.PositionBatch = positionBatchAt(list, index, dayIndex)
		boardFiltered := universe.Build(entries, market, filteredOptions)

		forward := forwardReturns(list, index, dayIndex, opt.hold)
		if len(forward) == 0 {
			continue
		}
		result.days++
		result.excluded += countPositionExcluded(boardFiltered)
		if len(boardFiltered.Candidates) == 0 {
			result.emptyDays++
			if len(boardBase.Candidates) > 0 {
				result.positionEmptyDays++
				collect(&result.skippedDays, boardBase, forward)
			}
		}
		collect(&result.baseline, boardBase, forward)
		collect(&result.filtered, boardFiltered, forward)
	}
	return result
}

func collect(into *variant, board universe.Board, forward map[string]float64) {
	for rank, candidate := range board.Candidates {
		ret, ok := forward[candidate.Symbol]
		if !ok {
			continue
		}
		into.picks++
		into.rets += ret
		if ret > 0 {
			into.positive++
		}
		if rank < 5 {
			into.topPicks++
			into.topRets += ret
		}
	}
}

func countPositionExcluded(board universe.Board) int {
	total := 0
	for _, exclusion := range board.Exclusions {
		if strings.HasPrefix(exclusion.Reason, "入场位置超限") {
			total += exclusion.Count
		}
	}
	return total
}

// snapshotAt 把第 dayIndex 天的日线还原成一份"当日全市场快照"。
//
// 这一步是回放能成立的关键：生产里 universe.Build 吃的是行情源给的快照，
// 这里用历史日线拼出同样的字段，日期一换就是另一天。
func snapshotAt(list []series, index map[string]int, dayIndex int) []foundation.StockCatalogEntry {
	date := ""
	entries := make([]foundation.StockCatalogEntry, 0, len(list))
	for _, item := range list {
		at, ok := index[item.Symbol]
		if !ok || dayIndex < at || dayIndex-at >= len(item.Bars) {
			continue
		}
		position := dayIndex - at
		if position < 6 || position >= len(item.Bars) {
			continue
		}
		current := item.Bars[position]
		previous := item.Bars[position-1]
		fiveDayAgo := item.Bars[position-5]
		if current.Close <= 0 || previous.Close <= 0 || fiveDayAgo.Close <= 0 || current.Open <= 0 {
			continue
		}
		if date == "" {
			date = current.Date
		}
		entries = append(entries, foundation.StockCatalogEntry{
			BoardStock: foundation.BoardStock{
				Symbol:               item.Symbol,
				Name:                 item.Name,
				Price:                current.Close,
				ChangePercent:        (current.Close/previous.Close - 1) * 100,
				FiveDayChangePercent: (current.Close/fiveDayAgo.Close - 1) * 100,
				Volume:               current.Volume,
				Amount:               current.Amount * 1e4, // 接口给的是万元
			},
			Industry: item.Industry,
		})
	}
	return entries
}

// positionBatchAt 用当日及以前的数据算位置，口径与生产一致：
// 只用已经发生的收盘价，不偷看未来。
func positionBatchAt(list []series, index map[string]int, dayIndex int) func([]string) map[string]universe.Position {
	byName := make(map[string]int, len(list))
	for position, item := range list {
		byName[item.Symbol] = position
	}
	return func(symbols []string) map[string]universe.Position {
		out := make(map[string]universe.Position, len(symbols))
		for _, symbol := range symbols {
			position, ok := byName[symbol]
			if !ok {
				continue
			}
			item := list[position]
			at := index[symbol]
			if at < 0 || dayIndex < at {
				continue
			}
			offset := dayIndex - at
			if offset < 20 || offset >= len(item.Bars) {
				continue
			}
			closes := make([]float64, 0, 21)
			for cursor := offset - 20; cursor <= offset; cursor++ {
				closes = append(closes, item.Bars[cursor].Close)
			}
			ma20 := mean(closes)
			if ma20 <= 0 {
				continue
			}
			current := item.Bars[offset]
			previous := item.Bars[offset-1]
			out[symbol] = universe.Position{
				Measured:        true,
				ChangePercent:   round2((current.Close/previous.Close - 1) * 100),
				FromMA20Percent: round2((current.Close/ma20 - 1) * 100),
				ConsecutiveUp:   streak(item.Bars, offset),
			}
		}
		return out
	}
}

// forwardReturns 计算每只标的"次日开盘买入、N 日后收盘卖出"的收益（%）。
func forwardReturns(list []series, index map[string]int, dayIndex, hold int) map[string]float64 {
	out := make(map[string]float64, len(list))
	for _, item := range list {
		at, ok := index[item.Symbol]
		if !ok {
			continue
		}
		entry := dayIndex - at + 1
		exit := dayIndex - at + hold
		if entry < 0 || exit >= len(item.Bars) {
			continue
		}
		open := item.Bars[entry].Open
		close := item.Bars[exit].Close
		if open <= 0 || close <= 0 {
			continue
		}
		out[item.Symbol] = (close/open - 1) * 100
	}
	return out
}

// calendarOf 取样本里最长的一条日线作为交易日历，并返回每只票的起始偏移。
func calendarOf(list []series) ([]string, map[string]int) {
	longest := 0
	for position, item := range list {
		if len(item.Bars) > len(list[longest].Bars) {
			longest = position
		}
	}
	dates := make([]string, 0, len(list[longest].Bars))
	for _, item := range list[longest].Bars {
		dates = append(dates, item.Date)
	}
	index := make(map[string]int, len(list))
	for _, item := range list {
		if len(item.Bars) == 0 {
			continue
		}
		// 用日期反查偏移，避免"停牌少几根"导致的错位。
		first := item.Bars[0].Date
		offset := sort.SearchStrings(dates, first)
		index[item.Symbol] = offset
	}
	return dates, index
}

// ---------------------------------------------------------------- 输出

func printReport(result report, opt options) {
	if result.days == 0 {
		fmt.Println("没有可用的回放日，检查日线是否取到。")
		return
	}
	fmt.Printf("回放窗口 %s ~ %s，有效交易日 %d，持仓 %d 日（次日开盘买入）\n\n",
		result.start, result.end, result.days, opt.hold)
	fmt.Printf("%-16s %10s %10s %10s %8s\n", "版本", "候选/日", "5日均收益", "前5名收益", "胜率")
	printVariant(result.baseline, result.days)
	printVariant(result.filtered, result.days)
	fmt.Println()
	fmt.Printf("位置筛选：平均每日剔除 %.1f 只；候选池被清空 %d 天（%.1f%%）\n",
		float64(result.excluded)/float64(result.days), result.emptyDays,
		float64(result.emptyDays)/float64(result.days)*100)
	fmt.Printf("其中因位置被清空 %d 天（%.1f%%）——这些日子改前是有得选的\n",
		result.positionEmptyDays, float64(result.positionEmptyDays)/float64(result.days)*100)
	if result.skippedDays.picks > 0 && result.positionEmptyDays > 0 {
		fmt.Printf("%-16s %10.1f %9.3f%% %9.3f%% %7.1f%%\n",
			result.skippedDays.name,
			float64(result.skippedDays.picks)/float64(result.positionEmptyDays),
			meanOf(result.skippedDays), topMean(result.skippedDays),
			float64(result.skippedDays.positive)/float64(result.skippedDays.picks)*100)
	}

	base := meanOf(result.baseline)
	after := meanOf(result.filtered)
	fmt.Printf("\n结论：候选池平均 5 日收益 %+.3f%% → %+.3f%%（%+.3f 个百分点），前 5 名 %+.3f%% → %+.3f%%\n",
		base, after, after-base, topMean(result.baseline), topMean(result.filtered))
}

func printVariant(item variant, days int) {
	fmt.Printf("%-16s %10.1f %9.3f%% %9.3f%% %7.1f%%\n",
		item.name,
		float64(item.picks)/float64(days),
		meanOf(item),
		topMean(item),
		float64(item.positive)/math.Max(float64(item.picks), 1)*100)
}

func meanOf(item variant) float64 {
	if item.picks == 0 {
		return 0
	}
	return item.rets / float64(item.picks)
}

func topMean(item variant) float64 {
	if item.topPicks == 0 {
		return 0
	}
	return item.topRets / float64(item.topPicks)
}

// ---------------------------------------------------------------- 小工具

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func streak(bars []bar, offset int) int {
	count := 0
	for cursor := offset; cursor > 0; cursor-- {
		if bars[cursor].Close > bars[cursor-1].Close {
			count++
			continue
		}
		break
	}
	return count
}

func round2(value float64) float64 { return math.Round(value*100) / 100 }

func mustDate(value string) time.Time {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Now()
	}
	return parsed
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
