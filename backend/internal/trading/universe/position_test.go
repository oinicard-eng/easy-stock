package universe

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

// spread 构造一批"每只一个独立行业"的标定样本。
//
// 不能用 noise()：它的行业按 index%20 循环，而行业强度评分会把同一热度的
// 标的顶到最前面，分散约束于是先于位置约束把它们砍到个位数——
// 那样测到的是分散逻辑，不是位置逻辑。这里的行业名唯一，分散约束
// 只剩"同板块最多 5 只"，结果可预期。
func spread(count int) []foundation.StockCatalogEntry {
	items := make([]foundation.StockCatalogEntry, 0, count)
	for index := 0; index < count; index++ {
		prefix, suffix := "60", "SH"
		if index%2 == 1 {
			prefix, suffix = "00", "SZ"
		}
		items = append(items, entry(
			fmt.Sprintf("%s%04d.%s", prefix, 1000+index, suffix),
			"标定股", fmt.Sprintf("标定行业%03d", index), 12, 0.8, 2.0, 3e8))
	}
	return items
}

// samePosition 返回一个把所有标的判成同一位置的批量函数。
func samePosition(position Position) func([]string) map[string]Position {
	return func(symbols []string) map[string]Position {
		out := make(map[string]Position, len(symbols))
		for _, symbol := range symbols {
			out[symbol] = position
		}
		return out
	}
}

// 位置筛选的核心承诺：挡掉一只，就要补上一只。
//
// 这一条比"能不能拦住"更重要。此前位置只在选股之后拦，结果是候选池被掏空——
// 2026-10-21 实测 28 只候选里 13 只不可买，前 6 名有 5 只不可买，
// 模型拿到的是一份大半不能执行的清单，而且它不知道这一点。
func TestBuildBackfillsAfterPositionExclusion(t *testing.T) {
	entries := spread(800)
	options := DefaultOptions()
	options.Limit = 10
	options.EntryLimits = EntryLimits{MaxChangePercent: 6, MaxMA20Percent: 12, MaxUpDays: 5}
	// 每三只判一只超限（距 MA20 +20%，超过 12% 的上限）。
	//
	// 不能按代码尾号挑：样本的代码与板块是交替对齐的，按尾号挑会连带
	// 把整个沪市或整个深市筛掉，最后卡在板块上限上，测不到回补。
	options.PositionBatch = func(symbols []string) map[string]Position {
		out := make(map[string]Position, len(symbols))
		for index, symbol := range symbols {
			out[symbol] = Position{Measured: true, FromMA20Percent: 2}
			if index%3 == 0 {
				out[symbol] = Position{Measured: true, FromMA20Percent: 20}
			}
		}
		return out
	}
	board := Build(entries, marketOf(entries, time.Now()), options)

	if !board.PositionChecked {
		t.Fatal("应标记本轮做过位置筛选")
	}
	if len(board.Candidates) != options.Limit {
		t.Fatalf("候选数 = %d, want %d（应向下补位而不是让池子变小）", len(board.Candidates), options.Limit)
	}
	for _, candidate := range board.Candidates {
		position := board.Positions[candidate.Symbol]
		if !position.Measured {
			t.Fatalf("%s 进了候选却没有位置事实", candidate.Symbol)
		}
		if reason := PositionBlockReason(position, options.EntryLimits); reason != "" {
			t.Fatalf("%s 位置超限（%s）却进了候选池", candidate.Symbol, reason)
		}
	}
	if countReason(board, "入场位置超限") == 0 {
		t.Fatal("被剔除的标的应留下排除原因")
	}
}

// 全场都超限时必须给出空池，而不是退回到"未知也算合格"。
//
// 这条守的是一个很容易被"优化掉"的分支：取数预算用完时顺手把剩下的
// 标的全放进来，看起来池子满了、指标好看了，实际是在一轮所有标的
// 都已透支的行情里闭眼买入。
func TestBuildReturnsEmptyWhenEverythingIsExtended(t *testing.T) {
	entries := spread(800)
	options := DefaultOptions()
	options.Limit = 10
	options.EntryLimits = EntryLimits{MaxChangePercent: 6, MaxMA20Percent: 12, MaxUpDays: 5}
	options.PositionBatch = samePosition(Position{Measured: true, FromMA20Percent: 30})
	board := Build(entries, marketOf(entries, time.Now()), options)
	if len(board.Candidates) != 0 {
		t.Fatalf("全部超限时候选数 = %d, want 0", len(board.Candidates))
	}
	if !board.Capped {
		t.Fatal("空池应标记 Capped，否则前端会显示成'筛不出票'")
	}
	if countReason(board, "入场位置超限") == 0 {
		t.Fatal("被位置剔除的标的必须计入排除原因，否则复盘看不出池子为什么空")
	}
}

// 取不到日线时保留候选，只当作"位置未知"。
//
// 理由：数据源抖动不该让候选池空掉，而它在下单前只会记一条 warn，
// 不会误拦。把"未知"当成"不好"，等于让一次网络故障变成一次空仓。
func TestBuildKeepsUnmeasuredPositions(t *testing.T) {
	entries := spread(800)
	options := DefaultOptions()
	options.Limit = 10
	options.EntryLimits = EntryLimits{MaxChangePercent: 6, MaxMA20Percent: 12, MaxUpDays: 5}
	options.PositionBatch = samePosition(Position{Measured: false})
	board := Build(entries, marketOf(entries, time.Now()), options)
	if len(board.Candidates) != options.Limit {
		t.Fatalf("候选数 = %d, want %d", len(board.Candidates), options.Limit)
	}
	if countReason(board, "入场位置超限") != 0 {
		t.Fatal("位置未知不该被计成超限")
	}
}

// 不注入 PositionBatch 时，行为必须与引入位置筛选之前完全一致。
// 这条守着兼容性：沙盘回放与老测试都走这条路。
func TestBuildWithoutPositionBatchIsUnchanged(t *testing.T) {
	entries := spread(800)
	options := DefaultOptions()
	options.Limit = 10
	board := Build(entries, marketOf(entries, time.Now()), options)
	if board.PositionChecked {
		t.Fatal("未注入位置来源时不应标记已筛选")
	}
	if len(board.Positions) != 0 {
		t.Fatalf("未注入位置来源时不该有位置事实，got %d", len(board.Positions))
	}
	if countReason(board, "入场位置超限") != 0 {
		t.Fatal("未注入位置来源时不该出现位置排除项")
	}
	if len(board.Candidates) != options.Limit {
		t.Fatalf("候选数 = %d, want %d", len(board.Candidates), options.Limit)
	}
}

// 三条阈值的判定顺序：涨幅优先，其次均线，最后连涨。
// 顺序影响的是"为什么被拒"的文案，不影响是否被拒，但复盘要能看懂主因。
func TestPositionBlockReasonOrder(t *testing.T) {
	limits := EntryLimits{MaxChangePercent: 6, MaxMA20Percent: 12, MaxUpDays: 5}
	cases := []struct {
		name     string
		position Position
		contains string
	}{
		{"位置正常", Position{Measured: true, ChangePercent: 2, FromMA20Percent: 3, ConsecutiveUp: 1}, ""},
		{"当日涨幅超限", Position{Measured: true, ChangePercent: 6.5, FromMA20Percent: 30, ConsecutiveUp: 9}, "当日涨幅"},
		{"均线偏离超限", Position{Measured: true, ChangePercent: 3, FromMA20Percent: 12.5, ConsecutiveUp: 1}, "20 日均线"},
		{"连涨到末端", Position{Measured: true, ChangePercent: 1.2, FromMA20Percent: 4, ConsecutiveUp: 5}, "连续上涨"},
		{"边界值放行", Position{Measured: true, ChangePercent: 5.99, FromMA20Percent: 11.99, ConsecutiveUp: 4}, ""},
		{"未测量不拦", Position{Measured: false, ChangePercent: 99, FromMA20Percent: 99, ConsecutiveUp: 99}, ""},
	}
	for _, item := range cases {
		got := PositionBlockReason(item.position, limits)
		if item.contains == "" {
			if got != "" {
				t.Fatalf("%s：不该拦，却返回 %q", item.name, got)
			}
			continue
		}
		if !strings.Contains(got, item.contains) {
			t.Fatalf("%s：拦截原因 = %q，应包含 %q", item.name, got, item.contains)
		}
	}
}

// 位置取数必须有硬上限，否则普涨行情里会把整个市场都拉一遍日线。
func TestBuildPositionWindowIsBounded(t *testing.T) {
	entries := spread(1200)
	options := DefaultOptions()
	options.Limit = 10
	options.EntryLimits = EntryLimits{MaxChangePercent: 6, MaxMA20Percent: 12, MaxUpDays: 5}
	calls := 0
	options.PositionBatch = func(symbols []string) map[string]Position {
		calls++
		if len(symbols) > 150 {
			t.Fatalf("单窗取数 %d 只，超过 150 的上限", len(symbols))
		}
		// 返回空集合：模拟"一只都没测到"，逼它把三个窗口走完。
		return map[string]Position{}
	}
	Build(entries, marketOf(entries, time.Now()), options)
	if calls > 3 {
		t.Fatalf("取数窗口 = %d 次，最多 3 次", calls)
	}
	if calls == 0 {
		t.Fatal("注入了位置来源却没有调用")
	}
}

func countReason(board Board, prefix string) int {
	total := 0
	for _, exclusion := range board.Exclusions {
		if strings.HasPrefix(exclusion.Reason, prefix) {
			total += exclusion.Count
		}
	}
	return total
}
