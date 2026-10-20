package trading

// 上一轮调研结果的复用。
//
// 事实背景：调研每轮最多 4 次工具调用，而这四次里经常有一次是在重复上一轮
// 已经确认过的事（同一个板块的资金流、同一只票的公告）。重复查询的代价不只是
// token：它占掉本轮唯一一次"查新东西"的机会。
//
// 因此这里把上一轮的观察压缩进本轮提示词。三条约束：
//  1. 只取同一天、且距今不超过 priorResearchMaxAge 的结果。隔了半天的资金流
//     与今天的盘面基本无关，沿用比不沿用更危险。
//  2. 只搬"工具看到了什么"，不搬"模型想买什么"。后者属于上一轮的判断，
//     搬过来会变成自我强化：模型看到自己上次的理由，就更可能重复它。
//  3. 显式标注"可能已过期，引用前复核"，并把字符数压到 priorResearchMaxChars。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// priorResearchMaxAge 是复用窗口。默认定时间隔 3 分钟，30 分钟的窗口
	// 覆盖十余轮；再放宽就会把"上一交易时段"的观察混进来。
	priorResearchMaxAge = 30 * time.Minute
	// priorResearchMaxChars 是这段内容的上限。它必须远小于一次工具结果，
	// 否则"复用"就变成了"把上一轮的上下文再灌一遍"。
	priorResearchMaxChars = 700
	// priorResearchMaxSteps 是列出的工具步数上限。
	priorResearchMaxSteps = 4
)

// PriorResearchForPrompt 返回上一轮调研中已确认的事实摘要。没有可用记录时返回空串。
//
// 读库失败只记日志返回空串：少一段可复用的事实会让模型多查一次，
// 而为此中断整轮计划，代价大得多。
func (s *Service) PriorResearchForPrompt(ctx context.Context, accountID string, now time.Time) string {
	plans, err := s.store.ListPlans(ctx, s.accountID(accountID), 3)
	if err != nil {
		s.logf("event=trading_prior_research_failed account=%s err=%v", accountID, err)
		return ""
	}
	for _, plan := range plans {
		if plan.Research == nil || !plan.Research.Enabled {
			continue
		}
		if plan.CreatedAt.IsZero() || now.Sub(plan.CreatedAt) > priorResearchMaxAge {
			// 计划按时间倒序返回，第一条超出窗口就意味着后面更早，直接停。
			return ""
		}
		if text := formatPriorResearch(*plan.Research, plan.CreatedAt, now); text != "" {
			return text
		}
	}
	return ""
}

// formatPriorResearch 把调研记录压成条目。
func formatPriorResearch(result ResearchResult, plannedAt, now time.Time) string {
	var builder strings.Builder
	minutes := int(now.Sub(plannedAt).Minutes())
	if minutes < 0 {
		minutes = 0
	}
	fmt.Fprintf(&builder, "（%d 分钟前，%s）\n", minutes, plannedAt.In(cstZone()).Format("15:04"))

	written := 0
	for _, step := range result.Steps {
		if written >= priorResearchMaxSteps {
			break
		}
		if step.Error != "" || strings.TrimSpace(step.Result) == "" {
			continue
		}
		summary := firstMeaningfulLine(step.Result)
		if summary == "" {
			continue
		}
		args := compactArgs(step.Args)
		fmt.Fprintf(&builder, "- %s%s → %s\n", step.Tool, args, clipRunes(summary, 90))
		written++
	}
	if conclusion := strings.TrimSpace(result.Conclusion); conclusion != "" {
		fmt.Fprintf(&builder, "- 当时结论：%s\n", clipRunes(conclusion, 120))
	}
	if written == 0 && strings.TrimSpace(result.Conclusion) == "" {
		return ""
	}

	text := builder.String()
	runes := []rune(text)
	if len(runes) > priorResearchMaxChars {
		text = string(runes[:priorResearchMaxChars]) + "…（已截断）\n"
	}
	return text
}

// firstMeaningfulLine 取工具结果里第一行有信息量的内容。
//
// 工具文本的第一行几乎总是标题（"板块资金流（concept）："），第二行是表头
// （"名称 | 代码 | 涨跌% |"），真正的数据从第三行开始。直接取第一行，
// 复用段里就会出现四条一模一样的标题——那等于没复用。
func firstMeaningfulLine(text string) string {
	fallback := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		isTitle := strings.HasSuffix(trimmed, "：") || strings.HasSuffix(trimmed, ":")
		if fallback == "" && !isTitle {
			fallback = trimmed
		}
		if isHeaderRow(trimmed) || isTitle {
			continue
		}
		if strings.Contains(trimmed, " | ") && containsDigit(trimmed) {
			return trimmed
		}
	}
	return fallback
}

func isHeaderRow(line string) bool {
	for _, prefix := range []string{"名称 |", "代码 |", "日期 |", "序号 |", "时间 |"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func containsDigit(text string) bool {
	for _, char := range text {
		if char >= '0' && char <= '9' {
			return true
		}
	}
	return false
}
