package trading

// 上一轮与本轮的差异。
//
// 光给"你被拦了什么"还不够：模型还需要知道"我上一轮看的那批标的，
// 这一轮还剩哪些"。没有这一层，它会把一只已经掉出候选池的票再提一次，
// 或者对一只刚进池的票完全无感——两种情况都在浪费当轮唯一的计划名额。
//
// 这里只搬事实（谁消失、谁新增、上一轮建议过的票还在不在），
// 不做"所以该买什么"的推断：推断留给模型，事实由系统保证准确。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
)

const (
	// planDiffMaxAge 是可比较的时间窗。超过一天的计划，候选池早已换过几轮，
	// 拿它做差异只会制造噪音。
	planDiffMaxAge = 24 * time.Hour
	// planDiffMaxItems 是每类差异最多列出的标的数，避免提示词被长名单吃掉。
	planDiffMaxItems = 6
)

// PlanDiffForPrompt 返回上一轮与本轮的标的差异，供提示词直接渲染。
//
// 取不到上一轮计划、或上一轮太旧时返回 nil：宁可不说，也不要拿一份
// 与当前无关的名单去暗示"这是延续"。
func (s *Service) PlanDiffForPrompt(ctx context.Context, accountID, planID string, snapshot Snapshot, now time.Time) []string {
	plans, err := s.store.ListPlans(ctx, s.accountID(accountID), 3)
	if err != nil {
		s.logf("event=trading_plan_diff_failed account=%s err=%v", accountID, err)
		return nil
	}
	var previous *Plan
	for index := range plans {
		if plans[index].ID == planID {
			continue
		}
		previous = &plans[index]
		break
	}
	if previous == nil || previous.CreatedAt.IsZero() || now.Sub(previous.CreatedAt) > planDiffMaxAge {
		return nil
	}
	current := symbolSet(snapshot.Universe)
	before := symbolSet(previous.Snapshot.Universe)
	if len(current) == 0 || len(before) == 0 {
		return nil
	}

	names := quoteNames(snapshot.Quotes, previous.Snapshot.Quotes)
	added, removed := diffSymbolSets(before, current)
	lines := make([]string, 0, 4)
	if len(added) == 0 && len(removed) == 0 {
		lines = append(lines, fmt.Sprintf(
			"候选池与上一轮完全一致（%d 只），没有新增或消失；标的没变时，更要说明「为什么这一轮还值得动手」。",
			len(current)))
	} else {
		lines = append(lines, fmt.Sprintf("候选池上一轮 %d 只 → 本轮 %d 只。", len(before), len(current)))
		if len(removed) > 0 {
			lines = append(lines, "已掉出候选池："+describeSymbols(removed, names))
		}
		if len(added) > 0 {
			lines = append(lines, "本轮新进候选池："+describeSymbols(added, names))
		}
	}

	// 上一轮真正提交过买单的标的，这一轮还在不在池子里。
	// 它比"候选池变化"更贴近模型自己的意图：这些是它上一轮已经决定要买的。
	traded := make([]string, 0, len(previous.Orders))
	for _, order := range previous.Orders {
		if order.Side == SideBuy {
			traded = append(traded, order.Symbol)
		}
	}
	traded = uniqueSymbols(traded)
	if len(traded) > 0 {
		gone := make([]string, 0, len(traded))
		for _, symbol := range traded {
			if !current[symbol] {
				gone = append(gone, symbol)
			}
		}
		line := "上一轮建议买入：" + describeSymbols(traded, names)
		if len(gone) > 0 {
			line += "；其中 " + describeSymbols(gone, names) + " 已不在本轮候选池，本轮无法再买"
		}
		lines = append(lines, line)
	}
	return lines
}

// symbolSet 把列表转成集合，同时忽略空白项。
func symbolSet(symbols []string) map[string]bool {
	set := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		trimmed := strings.TrimSpace(symbol)
		if trimmed != "" {
			set[trimmed] = true
		}
	}
	return set
}

// diffSymbolSets 返回 added（在 current 不在 before）与 removed（在 before 不在 current），
// 两者都按代码排序，保证同一份输入每次渲染出同样的顺序。
func diffSymbolSets(before, current map[string]bool) (added, removed []string) {
	for symbol := range current {
		if !before[symbol] {
			added = append(added, symbol)
		}
	}
	for symbol := range before {
		if !current[symbol] {
			removed = append(removed, symbol)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func uniqueSymbols(symbols []string) []string {
	seen := make(map[string]bool, len(symbols))
	result := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		result = append(result, symbol)
	}
	return result
}

// quoteNames 合并两份行情的名称，本轮优先：同一只票的名称可能已经更正。
func quoteNames(current, previous []foundation.Quote) map[string]string {
	names := make(map[string]string, len(current)+len(previous))
	for _, quote := range previous {
		if quote.Symbol != "" && quote.Name != "" {
			names[quote.Symbol] = quote.Name
		}
	}
	for _, quote := range current {
		if quote.Symbol != "" && quote.Name != "" {
			names[quote.Symbol] = quote.Name
		}
	}
	return names
}

// describeSymbols 把代码列表渲染成"代码 名称"的短句，超出上限只报数量。
// 不写"等 N 只"之外的省略细节：模型不需要看完整名单，它需要知道变了什么。
func describeSymbols(symbols []string, names map[string]string) string {
	limit := len(symbols)
	if limit > planDiffMaxItems {
		limit = planDiffMaxItems
	}
	parts := make([]string, 0, limit)
	for _, symbol := range symbols[:limit] {
		if name := names[symbol]; name != "" {
			parts = append(parts, fmt.Sprintf("%s %s", symbol, name))
			continue
		}
		parts = append(parts, symbol)
	}
	text := strings.Join(parts, "、")
	if len(symbols) > limit {
		text += fmt.Sprintf(" 等 %d 只", len(symbols))
	}
	return text
}
