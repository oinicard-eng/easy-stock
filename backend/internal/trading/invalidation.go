package trading

// 失效条件：把"什么情况下这笔判断不成立了"从一句给人看的话，变成代码能判的条件。
//
// 为什么必须结构化：自由文本的 invalidation 是这套系统里最像"智能"、实际上最没用
// 的一部分——模型写下"跌破 9.5 或板块资金转出则离场"，之后就再没有程序读过它。
// 判断失效的条件写而不判，等于买入时承诺了两个止损，只执行了其中一个。
//
// 但结构化不等于把清仓权交给一句自然语言。三条边界写死在这里：
//  1. 触发后只做**减仓**（默认 50%），不直接清仓。失效条件是"论点弱了"，
//     不是"价格到线了"——后者由止损负责，两者的执行力度必须不同。
//  2. **同一持仓只自动减一次**。每轮都判一次并把仓位砍半，会在一次误判里
//     用四个轮次清空一个本来只是震荡的仓位。
//  3. 板块资金这类需要外部序列的条件**不参与自动执行**，只回灌给模型。
//     代码判不了的事就不要假装判得，否则会出现"系统替我判断了板块资金"的错觉。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// InvalidationSpec 是一笔持仓的结构化失效条件。字段全部可缺省，
// 空结构表示"模型只给了文字，没有可执行条件"。
type InvalidationSpec struct {
	// PriceBelow：现价跌破此价，论点失效。最常见的形态。
	PriceBelow float64 `json:"price_below,omitempty"`
	// PriceAbove：现价涨破此价，论点失效。用于"涨到目标位之后逻辑已兑现"的情形。
	PriceAbove float64 `json:"price_above,omitempty"`
	// SectorFlow：板块资金条件，例如 "industry:半导体 转为净流出"。
	// 只作为事实回灌提示词，不参与代码判定——守卫在盘中拿不到板块资金序列。
	SectorFlow string `json:"sector_flow,omitempty"`
	// DeadlineDays：预期兑现的交易日数。超过后仍未盈利即视为判断失效。
	DeadlineDays int `json:"deadline_days,omitempty"`
	// Note 是模型的原话。保留它是因为结构化字段一定会丢信息：
	// "跌破 9.5" 背后的理由，只有那句话说得清。
	Note string `json:"note,omitempty"`
}

// Empty 表示没有任何可执行条件。此时不该落库、也不该在提示词里假装有。
func (s InvalidationSpec) Empty() bool {
	return s.PriceBelow <= 0 && s.PriceAbove <= 0 && strings.TrimSpace(s.SectorFlow) == "" && s.DeadlineDays <= 0
}

// Triggered 判断条件是否已经成立。heldDays 是建仓至今的**自然日**数。
//
// 返回的 reason 直接进入裁决消息与事件，必须写清"哪个条件、什么数字"：
// 复盘时要能一眼看出这次减仓是价格触发的还是时间触发的。
func (s InvalidationSpec) Triggered(price float64, heldDays int) (bool, string) {
	if price > 0 && s.PriceBelow > 0 && price <= s.PriceBelow {
		return true, fmt.Sprintf("现价 %.2f 跌破失效价 %.2f", price, s.PriceBelow)
	}
	if price > 0 && s.PriceAbove > 0 && price >= s.PriceAbove {
		return true, fmt.Sprintf("现价 %.2f 涨破失效价 %.2f（逻辑已兑现）", price, s.PriceAbove)
	}
	if limit := deadlineNaturalDays(s.DeadlineDays); limit > 0 && heldDays > limit {
		return true, fmt.Sprintf("已持有 %d 天（约 %d 个交易日），超过预期 %d 个交易日仍未兑现",
			heldDays, heldDays*5/7, s.DeadlineDays)
	}
	return false, ""
}

// UnmarshalJSON 兼容两种形态：结构化对象，或模型习惯输出的那句中文。
//
// 兼容不是宽容，而是必需：提示词改一次，历史计划里的字符串仍然是字符串。
// 只认对象的话，所有旧计划的失效条件都会在读取时静默消失。
func (s *InvalidationSpec) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*s = ParseInvalidationText(text)
		return nil
	}
	type alias InvalidationSpec
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*s = InvalidationSpec(parsed)
	return nil
}

// 从中文描述里提取可执行条件的两个模式。
//
// 只提取最明确的两类："跌破/跌穿 X 元"和"N 日内/交易日"。其余一律留给 Note：
// 试图从自然语言里抽出"板块资金转弱"这种条件，抽出来的只会是错误的条件。
var (
	priceBelowPattern = regexp.MustCompile(`(?:跌破|跌穿|低于|下破)\s*(\d+(?:\.\d+)?)`)
	deadlinePattern   = regexp.MustCompile(`(\d+)\s*(?:个)?\s*(?:交易)?日`)
)

// ParseInvalidationText 把模型写的一句中文转成结构化条件。
// 解析不出任何数字时返回只带 Note 的空条件——这比编一个默认值诚实。
func ParseInvalidationText(text string) InvalidationSpec {
	text = strings.TrimSpace(text)
	spec := InvalidationSpec{Note: text}
	if text == "" {
		return spec
	}
	if match := priceBelowPattern.FindStringSubmatch(text); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil && value > 0 {
			spec.PriceBelow = value
		}
	}
	if match := deadlinePattern.FindStringSubmatch(text); len(match) == 2 {
		if value, err := strconv.Atoi(match[1]); err == nil && value > 0 && value <= 120 {
			spec.DeadlineDays = value
		}
	}
	return spec
}

// describeInvalidation 生成一句给人看的条件摘要，用于提示词与界面。
// 没有结构化条件时退回原话，不输出"（无）"——那会让人以为模型没写。
func describeInvalidation(spec *InvalidationSpec, text string) string {
	if spec == nil || spec.Empty() {
		return strings.TrimSpace(text)
	}
	parts := make([]string, 0, 3)
	if spec.PriceBelow > 0 {
		parts = append(parts, fmt.Sprintf("跌破 %.2f", spec.PriceBelow))
	}
	if spec.PriceAbove > 0 {
		parts = append(parts, fmt.Sprintf("涨破 %.2f", spec.PriceAbove))
	}
	if spec.DeadlineDays > 0 {
		parts = append(parts, fmt.Sprintf("%d 个交易日内未兑现", spec.DeadlineDays))
	}
	if flow := strings.TrimSpace(spec.SectorFlow); flow != "" {
		parts = append(parts, "板块资金："+flow)
	}
	if note := strings.TrimSpace(firstNonEmpty(spec.Note, text)); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "；")
}

// holdingDays 返回建仓至今的自然日数。
//
// 刻意不接交易日历：那需要一份每年的休市表，一旦没更新就会在春节后
// 把所有持仓判成"超期"。自然日偏大，方向是更早触发减仓，属于保守的一侧；
// 折算成交易日时按 5/7 放大阈值，避免周五建的仓周一就被算成"持有了三天"。
func holdingDays(openedAt *time.Time, now time.Time) int {
	if openedAt == nil || openedAt.IsZero() {
		return 0
	}
	days := int(now.Sub(*openedAt).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// deadlineNaturalDays 把"预期 N 个交易日"折算成自然日阈值。
// +1 是给周末留的余量：5 个交易日可能是 7 个自然日，也可能是 9 个（跨长假）。
func deadlineNaturalDays(tradingDays int) int {
	if tradingDays <= 0 {
		return 0
	}
	return tradingDays*7/5 + 1
}
