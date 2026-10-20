package trading

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParseDraftPlan 从模型输出中提取交易计划 JSON。
// 模型偶尔会包一层 Markdown 代码块或前后加解释文字，这里只做结构提取，
// 不做语义猜测：找不到完整 JSON 对象就返回错误，让上层标记计划失败。
func ParseDraftPlan(content string) (DraftPlan, error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return DraftPlan{}, errors.New("模型返回为空")
	}
	text = stripCodeFence(text)
	object, err := extractJSONObject(text)
	if err != nil {
		return DraftPlan{}, err
	}
	var draft DraftPlan
	decoder := json.NewDecoder(strings.NewReader(object))
	if err := decoder.Decode(&draft); err != nil {
		return DraftPlan{}, fmt.Errorf("交易计划 JSON 解析失败: %w", err)
	}
	if len(draft.Orders) == 0 {
		// 允许空计划，但必须是显式空数组，而不是字段缺失。
		if !strings.Contains(object, "\"orders\"") {
			return DraftPlan{}, errors.New("交易计划缺少 orders 字段")
		}
	}
	for index := range draft.Orders {
		draft.Orders[index].Side = strings.ToLower(strings.TrimSpace(draft.Orders[index].Side))
		draft.Orders[index].OrderType = strings.ToLower(strings.TrimSpace(draft.Orders[index].OrderType))
		if draft.Orders[index].OrderType == "" {
			draft.Orders[index].OrderType = string(OrderTypeLimit)
		}
		draft.Orders[index].Symbol = strings.TrimSpace(draft.Orders[index].Symbol)
	}
	draft.Regime = strings.TrimSpace(draft.Regime)
	draft.Summary = strings.TrimSpace(draft.Summary)
	return draft, nil
}

func stripCodeFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return text
	}
	start := 1
	end := len(lines)
	for index := len(lines) - 1; index > 0; index-- {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "```") {
			end = index
			break
		}
	}
	if end <= start {
		return text
	}
	return strings.Join(lines[start:end], "\n")
}

// extractJSONObject 用括号配对定位第一个完整 JSON 对象，能正确处理字符串内的花括号。
func extractJSONObject(text string) (string, error) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", errors.New("模型输出中没有 JSON 对象")
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : index+1], nil
			}
		}
	}
	return "", errors.New("模型输出的 JSON 不完整")
}
