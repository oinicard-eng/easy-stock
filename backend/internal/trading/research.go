package trading

// 调研循环：让模型在出计划之前，自己决定查什么。
//
// 与控制流的关键分工（这一点决定了它是不是"真 agent"）：
//
//	模型持有"查什么"的决定权 —— 它看结果、改主意、再查，这是自主的部分；
//	代码持有"怎么查、查几次、查到什么"的执行权 —— 白名单、上限、超时、落库。
//
// 没有把 Hermes 的原生工具直接打开，是因为原生工具会让模型的调用脱离账本：
// 同一个提示词两次运行可能看到不同的外部世界，计划就无法回放。
// 这里用纯文本 JSON 协议（thought / action / tool / args），模型只表达意图，
// 工具由 Go 执行，每一步的结果都落库。于是"能自己查"和"每一步可审计"
// 不再是二选一。
//
// 降级策略：调研是增强，不是前置条件。模型超时、返回坏 JSON、工具报错，
// 都只降级为"直接出计划"，绝不阻断交易——把可观测性做成了单点故障，
// 是最常见的过度设计。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/agent"
)

const (
	// maxResearchSteps 是单轮调研允许的工具调用次数。
	//
	// 从 3 提到 4 是实测结论：真实跑下来，模型的三步几乎总是
	// "查概念资金流 → 验证板块内部结构 → 补查行业维度"，
	// 恰好用完三次，没有一次机会留给收尾，于是结论永远是"步数用完"。
	// 多给一次，它才会把观察整理成一句可用的判断。
	maxResearchSteps = 4
	// researchBudget 是整轮调研的时间上限。到点必须收手去出计划。
	researchBudget = 90 * time.Second
	// researchCallTimeout 是单次模型调用的上限。调研只是查资料，不需要长推理。
	researchCallTimeout = 55 * time.Second
	// researchResultChars 是单条工具结果注入下一轮提示词的字符上限。
	researchResultChars = 1400
)

// ResearchStep 是调研中的一步，既是提示词的输入，也是回放的记录。
type ResearchStep struct {
	Index     int            `json:"index"`
	Thought   string         `json:"thought"`
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args,omitempty"`
	Result    string         `json:"result,omitempty"`
	Rows      int            `json:"rows,omitempty"`
	LatencyMS int64          `json:"latency_ms"`
	Error     string         `json:"error,omitempty"`
	At        time.Time      `json:"at"`
}

// ResearchResult 是一轮调研的全部产出。
type ResearchResult struct {
	// Enabled 表示这一轮是否真的发起了调研（模型不可用时为 false）。
	Enabled bool `json:"enabled"`
	// Degraded 表示调研中途降级。计划照常生成，但记录里要留下原因。
	Degraded bool   `json:"degraded,omitempty"`
	Note     string `json:"note,omitempty"`
	// Conclusion 是模型自己给的收尾结论，只允许是事实陈述。
	Conclusion string         `json:"conclusion,omitempty"`
	Steps      []ResearchStep `json:"steps"`
	// ElapsedMS 是调研总耗时，用于观察它对交易时段的影响。
	ElapsedMS int64 `json:"elapsed_ms"`
}

// researchAction 是模型每轮必须输出的唯一结构。
type researchAction struct {
	Thought    string         `json:"thought"`
	Action     string         `json:"action"`
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args"`
	Conclusion string         `json:"conclusion"`
}

const agentStepsSchema = `
	CREATE TABLE IF NOT EXISTS trading_agent_steps (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		plan_id TEXT NOT NULL DEFAULT '',
		cycle_id TEXT NOT NULL DEFAULT '',
		step_index INTEGER NOT NULL DEFAULT 0,
		tool TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		content_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_agent_steps_plan ON trading_agent_steps(plan_id, step_index);
	CREATE INDEX IF NOT EXISTS idx_agent_steps_account ON trading_agent_steps(account_id, created_at DESC);
`

func (s *Store) migrateAgentSteps(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, agentStepsSchema)
	return err
}

// SaveResearchSteps 落库一轮调研的全部步骤。整批替换而不是追加：
// 同一 plan 重跑调研时，旧步骤留着会让回放出现两条时间线。
func (s *Store) SaveResearchSteps(ctx context.Context, accountID, planID, cycleID string, steps []ResearchStep) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM trading_agent_steps WHERE plan_id=?`, planID); err != nil {
		return err
	}
	for _, step := range steps {
		payload, err := json.Marshal(step)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO trading_agent_steps
			(id,account_id,plan_id,cycle_id,step_index,tool,error,content_json,created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			newID("step"), accountID, planID, cycleID, step.Index, step.Tool, step.Error,
			string(payload), nowText(step.At)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListResearchSteps 按计划取回调研步骤，用于前端回放与排障。
func (s *Store) ListResearchSteps(ctx context.Context, planID string) ([]ResearchStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT content_json FROM trading_agent_steps WHERE plan_id=? ORDER BY step_index ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	steps := make([]ResearchStep, 0, 4)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var step ResearchStep
		if err := json.Unmarshal([]byte(payload), &step); err != nil {
			continue
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

// runResearch 执行一轮受限调研。任何失败都降级返回，不向上抛错。
// 命名返回值是必需的：ElapsedMS 在 defer 里写。
// 用局部变量 + 匿名返回值时，defer 改的是副本，耗时永远是 0——
// 这个字段本来用来判断"调研会不会拖慢交易时段"，恒为 0 等于没有这个指标。
func (s *Service) runResearch(ctx context.Context, in ResearchInput) (result ResearchResult) {
	started := time.Now()
	if !s.researchEnabled.Load() {
		return ResearchResult{Enabled: false, Steps: []ResearchStep{}, Note: "本轮未启用调研"}
	}
	result = ResearchResult{Enabled: true, Steps: make([]ResearchStep, 0, maxResearchSteps)}
	defer func() { result.ElapsedMS = time.Since(started).Milliseconds() }()

	budgetCtx, cancel := context.WithTimeout(ctx, researchBudget)
	defer cancel()

	for index := 0; index < maxResearchSteps; index++ {
		if budgetCtx.Err() != nil {
			result.Degraded = true
			result.Note = "调研超出时间预算，按已有结果继续"
			break
		}
		remaining := maxResearchSteps - index
		prompt := buildResearchPrompt(in, result.Steps, remaining)

		callCtx, callCancel := context.WithTimeout(budgetCtx, researchCallTimeout)
		reply, err := agent.PromptUsingOptions(callCtx, s.gateway, prompt, agent.PromptOptions{
			Sandbox:     true,
			AutoApprove: true,
			// 这里仍然禁工具：工具由 Go 执行，模型只输出意图。
			// 打开原生工具会绕过白名单与落库，等于放弃可回放。
			DisableTools:         true,
			FirstResponseTimeout: 45 * time.Second,
			IdleTimeout:          60 * time.Second,
			MaxAttempts:          1,
		})
		callCancel()
		if err != nil {
			result.Degraded = true
			result.Note = fmt.Sprintf("第 %d 步调研调用失败：%v", index+1, err)
			break
		}

		action, err := parseResearchAction(reply.Content)
		if err != nil {
			result.Degraded = true
			result.Note = fmt.Sprintf("第 %d 步调研输出无法解析：%v", index+1, err)
			break
		}
		if strings.EqualFold(action.Action, "done") {
			result.Conclusion = strings.TrimSpace(action.Conclusion)
			break
		}
		if !strings.EqualFold(action.Action, "tool") {
			result.Degraded = true
			result.Note = fmt.Sprintf("第 %d 步返回了未知 action %q，按已有结果继续", index+1, action.Action)
			break
		}

		stepStarted := time.Now()
		outcome, toolErr := in.Toolbox.Call(budgetCtx, action.Tool, action.Args)
		step := ResearchStep{
			Index:     index + 1,
			Thought:   clipRunes(strings.TrimSpace(action.Thought), 200),
			Tool:      action.Tool,
			Args:      action.Args,
			Rows:      outcome.Rows,
			LatencyMS: time.Since(stepStarted).Milliseconds(),
			At:        time.Now().UTC(),
		}
		if toolErr != nil {
			// 工具报错也记一步：模型看到错误后常常会自己改正参数重试，
			// 这个"改错"的过程本身就是有价值的推理痕迹。
			step.Error = toolErr.Error()
			step.Result = "工具报错：" + toolErr.Error()
		} else {
			step.Result = clipRunes(outcome.Text, researchResultChars)
		}
		result.Steps = append(result.Steps, step)
	}

	if result.Conclusion == "" && len(result.Steps) > 0 {
		// 步数用完时模型没有机会收尾。用最后一步的想法作结论，比写一句
		// "未给出结论"有用：那句话对出计划毫无信息量，而最后一步的思考
		// 恰好说明它当时在追什么线索。
		last := result.Steps[len(result.Steps)-1]
		if thought := strings.TrimSpace(last.Thought); thought != "" {
			result.Conclusion = "（步数用完，未及收尾）最后一步的想法：" + thought
		} else {
			result.Conclusion = "（调研未给出收尾结论，仅保留查询记录）"
		}
	}
	if len(result.Steps) == 0 && !result.Degraded {
		// 模型第一步就 done：这也是一次有效判断（它认为资料已足够），
		// 不能记成失败，否则统计里会把"克制"算成"故障"。
		result.Note = "模型判断无需额外查询"
	}
	return result
}

// parseResearchAction 从模型输出里取出动作对象。
func parseResearchAction(content string) (researchAction, error) {
	text := stripCodeFence(strings.TrimSpace(content))
	if text == "" {
		return researchAction{}, fmt.Errorf("模型返回为空")
	}
	object, err := extractJSONObject(text)
	if err != nil {
		return researchAction{}, err
	}
	var action researchAction
	if err := json.NewDecoder(strings.NewReader(object)).Decode(&action); err != nil {
		return researchAction{}, fmt.Errorf("调研指令 JSON 解析失败: %w", err)
	}
	return action, nil
}

// saveResearch 落库调研步骤。失败只记日志：回放丢了是遗憾，阻断交易是事故。
func (s *Service) saveResearch(ctx context.Context, in ResearchInput, result ResearchResult) {
	if len(result.Steps) == 0 {
		return
	}
	if err := s.store.SaveResearchSteps(ctx, in.AccountID, in.PlanID, in.CycleID, result.Steps); err != nil {
		s.logf("event=trading_research_save_failed plan=%s err=%v", in.PlanID, err)
	}
}

// ResearchSteps 供 HTTP 层读取回放记录。
func (s *Service) ResearchSteps(ctx context.Context, planID string) ([]ResearchStep, error) {
	if strings.TrimSpace(planID) == "" {
		return nil, sql.ErrNoRows
	}
	return s.store.ListResearchSteps(ctx, planID)
}

// ResearchInput 是一轮调研的输入。它只带"查资料需要知道的东西"，
// 不带风控阈值——调研员不需要知道仓位上限，知道了反而会往"该怎么买"上偏。
type ResearchInput struct {
	PlanID    string
	CycleID   string
	AccountID string
	Snapshot  Snapshot
	Toolbox   *Toolbox
	Now       time.Time
}

// buildResearchPrompt 组装调研提示词。
//
// 三条设计约束：
//  1. 只给事实，不给倾向。调研员一旦知道"该买什么"，就会去查支持这个结论的证据。
//  2. 已查结果全文回灌，包括报错。模型看到报错才能改参数，看不到就会原样重试。
//  3. 明确剩余次数。不告诉它还剩几次，它会在最后一步才开始查有用的东西。
func buildResearchPrompt(in ResearchInput, steps []ResearchStep, remaining int) string {
	var builder strings.Builder
	builder.WriteString("你是 A 股交易调研员。任务是在生成交易计划之前，把关键事实查清楚。\n")
	builder.WriteString("你不下单、不给买卖建议、不预测涨跌，只负责查询并陈述查到的事实。\n\n")

	builder.WriteString("## 一、可用工具（只能从这里选）\n")
	for _, spec := range in.Toolbox.Specs() {
		params := make([]string, 0, len(spec.Params))
		for _, param := range spec.Params {
			mark := ""
			if param.Required {
				mark = "*"
			}
			params = append(params, fmt.Sprintf("%s%s:%s", param.Name, mark, param.Type))
		}
		fmt.Fprintf(&builder, "- %s(%s)：%s\n", spec.Name, strings.Join(params, ", "), spec.Description)
	}

	builder.WriteString("\n## 二、当前上下文\n")
	fmt.Fprintf(&builder, "- 时间：%s（北京时间）\n", in.Now.In(cstZone()).Format("2006-01-02 15:04"))
	fmt.Fprintf(&builder, "- 总资产 %.2f 元，可用资金 %.2f 元，已持仓 %d 只\n",
		in.Snapshot.Equity, in.Snapshot.Cash-in.Snapshot.ReservedCash, len(in.Snapshot.Positions))
	if in.Snapshot.Market.Evaluated {
		fmt.Fprintf(&builder, "- 系统判定的市场状态：%s（%s），本轮仓位预算 %.1f%%\n",
			in.Snapshot.Market.Label, in.Snapshot.Market.Regime, in.Snapshot.Market.RiskBudgetPercent)
		for _, note := range in.Snapshot.Market.Notes {
			fmt.Fprintf(&builder, "  · %s\n", note)
		}
	}
	if board := in.Snapshot.Board; board != nil && len(board.Candidates) > 0 {
		builder.WriteString("- 本轮候选池（前 15 名）：\n")
		builder.WriteString("  代码 | 名称 | 涨跌% | 行业 | 分数\n")
		for index, candidate := range board.Candidates {
			if index >= 15 {
				break
			}
			fmt.Fprintf(&builder, "  %s | %s | %.2f | %s | %.1f\n",
				candidate.Symbol, candidate.Name, candidate.ChangePercent,
				firstNonEmpty(candidate.Industry, "未知"), candidate.Score)
		}
	}
	if len(in.Snapshot.Positions) > 0 {
		builder.WriteString("- 当前持仓：")
		parts := make([]string, 0, len(in.Snapshot.Positions))
		for _, position := range in.Snapshot.Positions {
			parts = append(parts, fmt.Sprintf("%s %d股", position.Symbol, position.Quantity))
		}
		builder.WriteString(strings.Join(parts, "，"))
		builder.WriteString("\n")
	}

	if len(steps) > 0 {
		builder.WriteString("\n## 三、你已经查到的\n")
		for _, step := range steps {
			fmt.Fprintf(&builder, "第 %d 步 %s(%s)：\n%s\n\n", step.Index, step.Tool, compactArgs(step.Args), step.Result)
		}
	}

	fmt.Fprintf(&builder, "\n## 四、本轮还可以查 %d 次\n", remaining)
	if remaining <= 2 {
		// 越到最后越要提醒收尾。不提醒的话，模型会把每一次都当成"再补一个维度"，
		// 四步全花在收集上，主提示词里就只剩原始数据而没有判断。
		fmt.Fprintf(&builder, "你已接近次数上限。如果现有资料已能支撑「今天该不该买、买哪个方向」，请直接 done 并给出结论；\n")
		fmt.Fprintf(&builder, "只有在缺少关键事实、且缺了它就会导致判断方向相反时，才继续查。\n")
	}
	builder.WriteString("## 五、输出格式\n")
	builder.WriteString("只输出一个 JSON 对象，不要 markdown 代码块，不要任何解释文字：\n")
	builder.WriteString("继续查：{\"thought\":\"为什么查这个\",\"action\":\"tool\",\"tool\":\"工具名\",\"args\":{...}}\n")
	builder.WriteString("可以收尾：{\"thought\":\"为什么够了\",\"action\":\"done\",\"conclusion\":\"一句话事实汇总\"}\n\n")

	builder.WriteString("## 六、规则\n")
	builder.WriteString("1. 只查与「今天该不该买、买什么方向」直接相关的事实。\n")
	builder.WriteString("2. 不要用同样的参数重复查询；工具报错时按错误信息改参数，不要原样重试。\n")
	builder.WriteString("3. conclusion 只陈述事实（如「算力板块主力净流入 12 亿，居概念板块第一」），\n")
	builder.WriteString("   禁止出现「建议买入」「可以加仓」这类判断——判断是下一步的事。\n")
	builder.WriteString("4. 资料够用就 done。不要为了用满次数而查无关的东西。\n")
	// 这条不是"多用工具"的号召，而是针对一个具体缺陷：
	// 只看当日涨幅就给权重，等于不知道这笔交易要承担多大波动。
	// K 线是唯一能给出"位置 + 波动"的工具，值得在收尾前对重点标的查一次。
	builder.WriteString("5. 准备给某只票较高权重时，先用 get_kline 看它离均线多远、ATR 占现价多少、处于 20 日区间的什么位置。\n")
	builder.WriteString("   同样 8% 的权重，放在日波动 1% 和 5% 的票上完全是两种风险；没有这个数据就不要声称自己评估过风险。\n")
	// 工具带扩到 12 个之后，最容易犯的错不是"用错工具"，而是"只用价格类工具"。
	// 只查资金流与涨幅，写出来的理由必然和价格同义反复——这不是判断，是复述。
	builder.WriteString("6. 如果你的理由只有「资金流入 + 涨幅居前」，说明你还没查清为什么涨：\n")
	builder.WriteString("   用 get_announcements 看有无业绩预告/减持/问询，用 search_news 看有无政策或订单，\n")
	builder.WriteString("   用 get_billboard 看是机构还是游资在做。查不到就如实写「未查到」，不要用板块涨幅顶替个股原因。\n")
	return builder.String()
}

// compactArgs 把参数压成一行，避免回灌时占满版面。
func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return clipRunes(string(payload), 120)
}
