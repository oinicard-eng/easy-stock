package trading

import (
	"strings"
	"testing"
	"time"
)

// 调研提示词的体积控制：工具说明与早期步骤结果都是"已经付过一次钱"的文本，
// 每步重传一遍等于为同一段内容重复付费。4 步调研时这是单轮输入里最大的一块。
func TestResearchPromptOnlyExplainsToolsOnce(t *testing.T) {
	in := ResearchInput{
		Now:      time.Now(),
		Snapshot: Snapshot{Equity: 100000, Cash: 60000},
	}
	first := buildResearchPrompt(in, nil, 4)
	if !strings.Contains(first, "## 一、可用工具（只能从这里选）") {
		t.Fatal("第 1 步必须给出完整工具说明")
	}

	steps := []ResearchStep{
		{Index: 1, Tool: "list_sector_flow", Args: map[string]any{"limit": 5}, Result: "半导体 主力净流入 12 亿"},
	}
	second := buildResearchPrompt(in, steps, 3)
	if !strings.Contains(second, "参数同第 1 步") {
		t.Fatal("第 2 步应只列工具名，不重复参数说明")
	}
	if strings.Contains(second, "## 一、可用工具（只能从这里选）") {
		t.Fatal("第 2 步不该再给完整工具说明")
	}
}

// 较早步骤只留要点：模型的下一步几乎总是基于上一步看到的数据，
// 更早的结果只用来防止重复查询。全部保留会让第 4 步的输入变成前三步的简单相加。
func TestResearchPromptCompactsEarlierSteps(t *testing.T) {
	in := ResearchInput{
		Now:      time.Now(),
		Snapshot: Snapshot{Equity: 100000, Cash: 60000},
	}
	long := strings.Repeat("第 1 步查到的明细数据行\n", 100)
	steps := []ResearchStep{
		{Index: 1, Tool: "get_kline", Result: long},
		{Index: 2, Tool: "list_sector_flow", Result: "第二行"},
		{Index: 3, Tool: "get_announcements", Result: "第三行"},
	}
	prompt := buildResearchPrompt(in, steps, 1)

	if !strings.Contains(prompt, "已查过，要点：") {
		t.Fatal("第 1 步应被压缩成要点")
	}
	if strings.Contains(prompt, long) {
		t.Fatal("较早步骤的完整结果不该继续占用输入")
	}
	// 最近两步必须完整保留，否则模型会看不到刚查到的数据。
	if !strings.Contains(prompt, "第二行") || !strings.Contains(prompt, "第三行") {
		t.Fatal("最近两步的结果应完整保留")
	}
	// 要点只取前 3 行：压缩后同一句明细最多出现 3 次，而不是 100 次。
	if got := strings.Count(prompt, "明细数据行"); got > 3 {
		t.Fatalf("较早步骤应压成 3 行以内，实际出现 %d 行", got)
	}
}
