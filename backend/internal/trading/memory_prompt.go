package trading

// 记忆的自动注入。
//
// 之前记忆只有一条通路：模型主动调 recall_trades。实际跑下来它几乎不调——
// 手里已经有四十行行情表、一堆候选依据，"再查一次历史"这一步最容易被跳过。
// 于是系统记了几百条教训，却从不影响下一次决策。
//
// 这里改成"不问也给"：生成计划时自动挑出与本轮最相关的几条放进提示词。
// 但注入必须克制，三条约束：
//  1. 只挑与本轮标的、行业、市场状态相关的，不按"最近"一刀切。
//     全市场最近八条与今天要买的票毫无关系时，它只会稀释注意力。
//  2. 有衰减。三个月前的同标的记忆仍然参考，但排在最近的前面之后。
//  3. 只给事实不给倾向。记忆里没有"应该"两个字，判断留给本轮。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MemoryBrief 是一条被挑中的记忆，附带它为什么被挑中。
type MemoryBrief struct {
	Memory
	// AgeDays 是记忆距今的天数，前端与提示词都用它标注"多久以前"。
	AgeDays int `json:"age_days"`
	// Reason 是命中原因（symbol / sector / regime），
	// 让人看得出这条为什么出现在这里，而不是系统随手塞的。
	Reason string `json:"reason"`
}

const (
	// maxInjectedMemories 是一次注入的上限。超过这个数，
	// 模型会开始把个别案例当成规律——而这恰恰是记忆最危险的用法。
	maxInjectedMemories = 8
	memoryScanLimit     = 200
	memoryFreshDays     = 30
	memoryStaleDays     = 90
)

// MemoriesForPrompt 挑出与本轮最相关的记忆。
//
// 打分规则（分值即重要性，不是精确科学）：
//   - 同标的 +3、同行业 +2、同市场状态 +1；
//   - 30 天内 +2、90 天内 +1，更早的只保留同标的（它仍然是这只票的历史）。
//
// 排序先按分数，再按时间：同分时新的在前，避免一条三年前的止损长期霸占位置。
func (s *Service) MemoriesForPrompt(ctx context.Context, accountID string, snapshot Snapshot, now time.Time) []MemoryBrief {
	memories, err := s.store.SearchMemories(ctx, MemoryQuery{
		AccountID: s.accountID(accountID),
		Limit:     memoryScanLimit,
	})
	if err != nil {
		s.logf("event=trading_memory_inject_failed err=%v", err)
		return nil
	}
	if len(memories) == 0 {
		return nil
	}
	// 关注集合包含候选池、持仓与关注列表：三者任一命中的记忆都可能改变本轮判断。
	watch := map[string]bool{}
	for _, symbol := range snapshot.Universe {
		watch[symbol] = true
	}
	for _, position := range snapshot.Positions {
		watch[position.Symbol] = true
	}
	sectors := map[string]bool{}
	for _, sector := range snapshot.Sectors {
		if strings.TrimSpace(sector) != "" {
			sectors[sector] = true
		}
	}
	regime := string(snapshot.Market.Regime)

	scored := make([]MemoryBrief, 0, len(memories))
	for _, memory := range memories {
		score := 0
		reason := ""
		switch {
		case watch[memory.Symbol]:
			score += 3
			reason = "symbol"
		case sectors[memory.Sector] && memory.Sector != "":
			score += 2
			reason = "sector"
		case regime != "" && memory.Regime == regime:
			score += 1
			reason = "regime"
		}
		if score == 0 {
			continue
		}
		age := memoryAgeDays(memory, now)
		switch {
		case age <= memoryFreshDays:
			score += 2
		case age <= memoryStaleDays:
			score += 1
		case reason != "symbol":
			// 超过三个月、又不是同一只票的记忆，只在没有别的材料时才用。
			// 行业与市场状态三个月就会换一轮，旧结论套上来往往是错的。
			continue
		}
		scored = append(scored, MemoryBrief{Memory: memory, AgeDays: age, Reason: reason})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].AgeDays != scored[j].AgeDays {
			return scored[i].AgeDays < scored[j].AgeDays
		}
		return scored[i].TradingDay > scored[j].TradingDay
	})
	if len(scored) > maxInjectedMemories {
		scored = scored[:maxInjectedMemories]
	}
	return scored
}

// memoryAgeDays 按平仓日计算年龄。解析失败按"很旧"处理而不是 0：
// 0 会让一条解析不出日期的记录获得"最新"的加分。
func memoryAgeDays(memory Memory, now time.Time) int {
	closed, err := time.Parse("2006-01-02", strings.TrimSpace(memory.TradingDay))
	if err != nil {
		return memoryStaleDays + 1
	}
	days := int(now.Sub(closed).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// memoryPromptLines 把记忆渲染成提示词里的几行。
//
// 只输出可核对的事实：哪一天、哪只票、什么结果、当时的理由是什么。
// 不写"所以这次要小心"——那等于替模型做判断，而这个判断恰恰是它该做的。
func memoryPromptLines(memories []MemoryBrief) []string {
	lines := make([]string, 0, len(memories))
	for _, brief := range memories {
		parts := make([]string, 0, 4)
		parts = append(parts, fmt.Sprintf("%s %s %s", brief.TradingDay, brief.Symbol, brief.Name))
		if brief.Sector != "" {
			parts = append(parts, "["+brief.Sector+"]")
		}
		parts = append(parts, fmt.Sprintf("%s %.2f%%（持有 %d 天）",
			memoryOutcomeLabel(brief.ExitTrigger, brief.Outcome), brief.PnLPercent, brief.HoldDays))
		if brief.RegimeLabel != "" && brief.RegimeLabel != "未知" {
			parts = append(parts, "买入时"+brief.RegimeLabel)
		}
		line := strings.Join(parts, " · ")
		if thesis := strings.TrimSpace(brief.Thesis); thesis != "" {
			line += fmt.Sprintf("；当时理由「%s」", clipRunes(thesis, 50))
		}
		if lesson := strings.TrimSpace(brief.Lesson); lesson != "" {
			line += fmt.Sprintf("；%s", clipRunes(lesson, 60))
		}
		lines = append(lines, line)
	}
	return lines
}

// memoryOutcomeLabel 用离场原因而不是"赢/亏"来描述结果。
// "止损 -3.2%" 与 "止盈 +3.2%" 金额接近，教训完全相反。
func memoryOutcomeLabel(trigger, outcome string) string {
	switch trigger {
	case TriggerStopLoss, TriggerHardStop:
		return "止损离场"
	case TriggerTrailStop:
		return "移动止损离场"
	case TriggerTakeProfit:
		return "止盈离场"
	case TriggerInvalidation:
		return "失效条件减仓"
	case TriggerTimeStop:
		return "时间止损离场"
	case TriggerModelExit:
		return "模型主动离场"
	default:
		if outcome == "win" {
			return "盈利离场"
		}
		return "亏损离场"
	}
}
