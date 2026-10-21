package trading

import (
	"fmt"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/universe"
)

const maxPromptQuotes = 40

// maxPromptCandidates 限制候选依据的行数，避免提示词过长挤占模型注意力。
const maxPromptCandidates = 30

type PlanPromptInput struct {
	Account   Account
	Positions []Position
	Quotes    []foundation.Quote
	Watchlist []string
	Rules     RiskRules
	Profile   string
	Horizon   string
	Notes     string
	Now       time.Time
	// Market 为市场环境判断。Evaluated 为 false 时提示词不写该章节，
	// 模型也就不会引用一个并不存在的判断。
	Market cognition.MarketView
	// Board 为自动选股的候选池，含入选理由。显式指定标的时为空。
	Board *universe.Board
	// Experience 是系统自己从已平仓记录算出的经验，只给结论不给倾向。
	Experience []Lesson
	// Research 是生成计划前用只读工具查到的记录。它与行情快照的区别：
	// 快照是"系统给你的"，调研是"你自己去查的"，后者带有选择意图，
	// 因此必须标明出处，避免模型把两次查询的结论当成本轮新增的事实。
	Research *ResearchResult
	// Pending 是本轮已挂出、尚未成交的委托。模型必须看得见它们，
	// 否则会在 3 分钟后再给一笔同标的的建议，被"单票权重已含挂单"拦掉。
	Pending []Order
	// Blocks 是最近几小时被确定性规则拦下的建议，含原始数量与拦截原因。
	// 这是本提示词里唯一"关于模型自己历史行为"的事实，也是打破
	// "每轮重复同一个错误"的唯一手段。
	Blocks []BlockedIntent
	// Cooldowns 是止损后仍在生效的禁买约束。
	// 必须提前告诉模型：让它自己避开，比每轮给出建议再被守卫拦掉要好——
	// 后者会让界面看起来像"模型很积极、系统总在否决"。
	Cooldowns CooldownView
	// Memories 是自动挑出的相关记忆（同标的/同行业/同市场状态）。
	// 与 Experience 的分工：Experience 是聚合统计（"止损胜率 25%"），
	// Memories 是单笔情境（"那天为什么买、后来发生了什么"）。
	Memories []MemoryBrief
	// Calibration 是"你的置信度准不准"的统计结论，样本不足时为空。
	// 它放在经验段里而不是单独一章：它和"止损胜率 25%"是同一类事实，
	// 单独成章会让模型以为这是一个新的任务要求。
	Calibration string
	// PriorResearch 是上一轮调研已经确认过的事实。
	// 之所以要传：同一个板块的资金流，模型可能连续三轮各查一次，
	// 每次都是一次模型往返；而三轮之间真正变化的往往只有价格。
	PriorResearch string
	// PlanDiff 是上一轮候选池与本轮的差异（谁消失、谁新增、上一轮建议过的票还在不在）。
	// 与 Blocks 的分工：Blocks 说"你的想法被否了"，PlanDiff 说"世界变了多少"。
	PlanDiff []string
}

// BlockedIntent 是一条被拦下的建议。它保留模型的原始意图（想买多少、什么价位），
// 因为"想买 100 股但被行业上限拦住"与"根本没想过"是两件不同的事。
type BlockedIntent struct {
	Symbol  string
	Name    string
	Side    Side
	Weight  float64
	Quant   int
	Price   float64
	Code    string
	Reason  string
	Blocked time.Time
	// Repeat 是同一标的同一规则在窗口内被拦的次数。
	// 次数比次数列表有用得多：它直接说明"这不是偶发，是模型没在改"。
	Repeat int
}

// BuildPlanPrompt 组装交易计划提示词。
//
// 提示词只提供事实与约束，不提供"应该买什么"的倾向；所有数字都来自行情与账户，
// 模型不允许引入快照之外的标的或价格。
func BuildPlanPrompt(input PlanPromptInput) string {
	rules := input.Rules.Normalize()
	// 总仓位上限取市场预算与风控上限的较小值：市场不好时以市场为准，
	// 但风控阈值永远不能被市场判断放宽。
	exposureLimit := rules.MaxTotalExposurePercent
	if input.Market.Evaluated {
		exposureLimit = input.Market.MaxExposurePercent(rules.MaxTotalExposurePercent)
	}
	var builder strings.Builder

	builder.WriteString("你是 A 股模拟盘交易计划官。你的唯一输出是一份可被程序解析的 JSON 交易计划。\n")
	builder.WriteString("你不接券商、不执行交易，只负责在给定事实与风控约束内给出计划。\n\n")

	builder.WriteString("## 一、硬性约束（违反即整条建议作废）\n")
	fmt.Fprintf(&builder, "1. 只能从下方「行情快照」列出的标的中选择，禁止出现其他代码或名称。\n")
	fmt.Fprintf(&builder, "2. 禁止编造价格。限价必须落在快照给出的涨停价与跌停价之间。\n")
	fmt.Fprintf(&builder, "3. 不要输出股数、金额或费用，只输出目标权重百分比；股数由系统换算。\n")
	fmt.Fprintf(&builder, "4. 每笔买入必须给出止损价（低于买入价、距离不超过 %.1f%%）与 invalidation 失效条件，两者一起构成退出承诺。\n", rules.MaxStopLossPercent)
	fmt.Fprintf(&builder, "5. 单票目标权重不超过 %.1f%%，单次最多 %d 笔建议。\n", rules.MaxSingleWeightPercent, rules.MaxOrders)
	// 定价公式必须提前说明：模型如果以为"写多少就买多少"，就会习惯性写满，
	// 然后每次都拿到打折后的额度，却不知道为什么。把公式写出来，
	// 它才知道 confidence 是真正影响金额的输入，而不是一个装饰字段。
	builder.WriteString("   目标权重只是上限，实际额度 = 目标 × (0.5 + 0.5 × confidence) × 波动率系数；\n")
	builder.WriteString("   波动率系数 = clamp(2.5 / ATR占现价%, 0.55, 1)，只减不增。confidence 写 0.9 与写 0.5，\n")
	builder.WriteString("   拿到的钱相差两成以上；用不准的自信去换额度，只会让自己的建议被系统压小。\n")
	fmt.Fprintf(&builder, "6. 买入后现金占比不得低于 %.1f%%，总仓位不得高于 %.1f%%。\n", rules.MinCashPercent, exposureLimit)
	fmt.Fprintf(&builder, "7. 同一标的只能出现一次；卖出只能针对已有持仓，且不超过可卖数量。\n")
	fmt.Fprintf(&builder, "8. 单笔止损触发时的账户亏损不得超过总资产的 %.1f%%。\n", rules.MaxRiskPerTradePercent)
	// 行业上限必须先告诉模型，再在守卫里强制。只说给守卫听，模型会给出一半被拦的计划，
	// 用户看到的是"模型很积极、系统总在否决"，两头都不满意。
	fmt.Fprintf(&builder, "9. 同一行业（按大类归并）合计权重不超过 %.1f%%，同一行业最多持有 %d 只；\n",
		rules.MaxSectorWeightPercent, rules.MaxSectorNames)
	fmt.Fprintf(&builder, "10. 如果证据不足或没有值得交易的机会，orders 返回空数组，并在 summary 中说明原因。\n")
	// 这条来自真实浪费：模型连续两轮给出 688 开头的标的，而科创板一手 200 股，
	// 目标金额买不起一手，两笔建议全部在 sizing.below_lot 被拒。
	// 与其让守卫一次次拦，不如把规则先告诉它。
	fmt.Fprintf(&builder, "11. 688 开头的科创板标的一手为 200 股，若目标金额买不起一手（200 × 现价），不要提交该笔建议。\n\n")

	builder.WriteString("## 二、账户状态\n")
	fmt.Fprintf(&builder, "- 时间：%s（北京时间）\n", input.Now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04"))
	fmt.Fprintf(&builder, "- 可用资金：%.2f 元（这是唯一可动用的钱）\n", input.Account.AvailableCash())
	if input.Account.ReservedCash > 0 {
		fmt.Fprintf(&builder, "- 其中 %.2f 元已被未成交的买入挂单冻结，不得重复计入可用资金\n", input.Account.ReservedCash)
	}
	fmt.Fprintf(&builder, "- 账面现金：%.2f 元\n", input.Account.Cash)
	fmt.Fprintf(&builder, "- 总资产：%.2f 元\n", input.Account.Equity)
	if strings.TrimSpace(input.Profile) != "" {
		fmt.Fprintf(&builder, "- 交易者画像：%s\n", strings.TrimSpace(input.Profile))
	}
	if strings.TrimSpace(input.Horizon) != "" {
		fmt.Fprintf(&builder, "- 计划周期：%s\n", strings.TrimSpace(input.Horizon))
	}
	if len(input.Positions) == 0 {
		builder.WriteString("- 当前空仓。\n")
	} else {
		builder.WriteString("- 当前持仓：\n")
		for _, position := range input.Positions {
			fmt.Fprintf(&builder, "  - %s %s：%d 股（可卖 %d，已挂卖单占用 %d），成本 %.2f，现价 %.2f，占比 %.2f%%，浮动盈亏 %+.2f（%+.2f%%）",
				position.Symbol, position.Name, position.Quantity, position.Sellable(), position.ReservedQuantity,
				position.AverageCost, position.LastPrice, position.WeightPercent(input.Account.Equity),
				position.Unrealized, position.ProfitPercent())
			// 防守线写进提示词，模型才知道"这只票已经有人管着"，不必再为它生成卖单。
			if position.StopLossPrice > 0 {
				fmt.Fprintf(&builder, "，止损 %.2f", position.StopLossPrice)
			}
			if position.TakeProfitPrice > 0 {
				fmt.Fprintf(&builder, "，止盈 %.2f", position.TakeProfitPrice)
			}
			if position.Sector != "" {
				fmt.Fprintf(&builder, "，行业 %s", position.Sector)
			}
			// 失效条件与预期期限必须写回提示词。模型上一轮写下的条件
			// 若不在这一轮出现，它就无法判断"我当时的逻辑现在还成立吗"，
			// 只能凭当前价格重新编一个理由。
			if thesis := strings.TrimSpace(position.Thesis); thesis != "" {
				fmt.Fprintf(&builder, "，买入理由「%s」", clipRunes(thesis, 60))
			}
			if condition := describeInvalidation(position.InvalidationRule, position.Invalidation); condition != "" {
				fmt.Fprintf(&builder, "，失效条件 %s", clipRunes(condition, 60))
			}
			if position.ExpectedDays > 0 {
				fmt.Fprintf(&builder, "，预期 %d 个交易日内兑现", position.ExpectedDays)
			}
			if days := holdingDays(position.OpenedAt, input.Now); days > 0 {
				fmt.Fprintf(&builder, "，已持有 %d 天", days)
			}
			// 入场环境与入场置信度：让模型看到"这是我在什么环境下、多有把握时买的"。
			// 没有它，模型只能凭现在的价格重新解释当初的买入理由。
			if position.EntryRegime != "" {
				fmt.Fprintf(&builder, "，建仓环境 %s", position.EntryRegime)
			}
			if position.EntryConfidence > 0 {
				fmt.Fprintf(&builder, "，当时置信度 %.2f", position.EntryConfidence)
			}
			if position.ReviewCount > 0 {
				fmt.Fprintf(&builder, "，已复核 %d 轮", position.ReviewCount)
			}
			builder.WriteString("\n")
		}
		builder.WriteString("  说明：止损价由持仓防守线自动执行，不需要你在 orders 里重复卖出这些标的。\n")
	}

	// 挂单必须显式列出。快照里只有"冻结了多少钱"，模型看不到冻结在哪些标的上，
	// 于是会再提一次同标的的建议——而它一定会在"单票权重含挂单"这条被拦。
	// 这不是模型判断失误，是它拿到的信息不完整。
	if len(input.Pending) > 0 {
		builder.WriteString("- 当前挂单（尚未成交，已占用资金或股数）：\n")
		for _, order := range input.Pending {
			remaining := order.Quantity - order.FilledQuantity
			if remaining <= 0 {
				continue
			}
			fmt.Fprintf(&builder, "  - %s %s %s %d 股 @%.2f（%s）\n",
				order.Symbol, order.Name, sideLabel(order.Side), remaining, order.LimitPrice, orderStatusLabel(order.Status))
		}
		builder.WriteString("  说明：这些标的已有挂单，本轮不要为它们提交同方向的新建议；要改主意请等成交，或先撤单再重新计划。\n")
	}

	// 冷静期必须在模型做判断之前告知。放在"被拦下的建议"那一节之后讲太晚：
	// 那时模型已经想好了要买什么，看到的只是"又被拒了"。
	if lines := input.Cooldowns.Describe(input.Now); len(lines) > 0 {
		builder.WriteString("- 当前禁买（止损后的冷静期，硬约束）：\n")
		for _, line := range lines {
			fmt.Fprintf(&builder, "  - %s\n", line)
		}
		builder.WriteString("  说明：这些标的与行业在本轮不可买入，提了也会被拦。这段时间可以用来观察，而不是用来翻本。\n")
	}

	if input.Market.Evaluated {
		builder.WriteString("\n## 三、市场环境（确定性统计，不是模型观点）\n")
		for _, note := range input.Market.Notes {
			fmt.Fprintf(&builder, "- %s\n", note)
		}
		fmt.Fprintf(&builder, "\n本轮总仓位上限 %.0f%%（市场仓位预算与风控上限取较小值）。\n",
			exposureLimit)
		if input.Market.Regime == cognition.RegimeEbb || input.Market.Regime == cognition.RegimeEuphoria {
			builder.WriteString("注意：当前不是适合全面进攻的环境，宁可少做几笔，也不要为了凑仓位而降低标准。\n")
		}
	}

	builder.WriteString("\n## 四、历史经验（系统按已实现平仓统计，样本不足不给胜率）\n")
	for _, line := range ExperienceLines(input.Experience) {
		fmt.Fprintf(&builder, "- %s\n", line)
	}
	if line := strings.TrimSpace(input.Calibration); line != "" {
		fmt.Fprintf(&builder, "- %s\n", line)
		fmt.Fprintf(&builder, "  置信度会被折算成仓位上限：自评偏高时，系统按更低的有效置信度定价，不会按你写的数字给额度。\n")
	}
	// 明确边界：经验是事实描述，不是对本轮的建议。不写这句，模型会把
	// "上次止损的都亏了" 理解成 "这次不要止损"。
	builder.WriteString("经验只说明过去发生过什么，不构成对本轮的指令。每一笔仍要独立判断，止损纪律不因历史统计而放宽。\n")

	// 单笔情境与聚合统计并列给出：聚合回答"平均而言"，情境回答"那一次到底发生了什么"。
	// 只给聚合时，模型最省力的解读是"止损胜率低 → 少止损"，这是灾难性的。
	if lines := memoryPromptLines(input.Memories); len(lines) > 0 {
		builder.WriteString("\n### 相关记忆（系统按标的/行业/市场状态挑出的已平仓记录）\n")
		for _, line := range lines {
			fmt.Fprintf(&builder, "- %s\n", line)
		}
		builder.WriteString("记忆是单笔事实，不能直接推广成本轮结论：同样的环境里，第二次的结果可能完全相反。\n")
	}

	// 上一轮已经查过的事实：省掉重复查询，同时明确标注它可能过期。
	// 不标"可能过期"的话，模型会拿半小时前的资金流当成现价依据。
	if text := strings.TrimSpace(input.PriorResearch); text != "" {
		builder.WriteString("\n### 上一轮你已经查过的事实（可能已过期，引用前复核）\n")
		builder.WriteString(text)
		builder.WriteString("\n这些是上一轮的观察，不是本轮的新数据。价格、资金流、公告随时会变；\n")
		builder.WriteString("如果本轮判断依赖其中的数字，重新用工具查一次，而不是直接沿用。\n")
	}

	// 这一段是"闭环"的关键：把模型自己的历史意图与系统的否决意见送回输入端。
	// 只写"被拦了"不够，必须同时给出原始意图与拦截代码，模型才知道该改什么：
	// 是换标的、降权重，还是这个方向今天根本不该碰。
	if len(input.Blocks) > 0 {
		builder.WriteString("\n## 五、你最近被拦下的建议（不要原样重复）\n")
		for _, block := range input.Blocks {
			detail := ""
			if block.Quant > 0 {
				detail = fmt.Sprintf("，当时拟 %s %d 股 @%.2f", sideLabel(block.Side), block.Quant, block.Price)
			} else if block.Weight > 0 {
				detail = fmt.Sprintf("，当时目标权重 %.1f%%", block.Weight)
			}
			repeat := ""
			if block.Repeat > 1 {
				repeat = fmt.Sprintf("（同一处已重复 %d 次）", block.Repeat)
			}
			fmt.Fprintf(&builder, "- %s %s%s：被「%s」拦下——%s%s\n",
				block.Symbol, block.Name, detail, firstNonEmpty(block.Code, "未记录规则"),
				clipRunes(block.Reason, 80), repeat)
		}
		builder.WriteString("原样再提一次只会再被拦一次，并浪费本轮唯一的计划名额。请据此调整：换标的、降权重，或本轮放弃这个方向。\n")
		builder.WriteString("如果认为拦截本身有误（例如行业分类不对），请在 summary 中说明，不要用改代码或改数量的方式绕过。\n")
	}

	// 候选池差异与拦截记录是两件不同的事：前者说"世界变了"，
	// 后者说"你的想法被否了"。只给后者，模型会误以为池子没变。
	if len(input.PlanDiff) > 0 {
		builder.WriteString("\n### 上一轮与本轮的标的差异\n")
		for _, line := range input.PlanDiff {
			fmt.Fprintf(&builder, "- %s\n", line)
		}
		builder.WriteString("标的消失通常是因为涨跌幅或成交额掉出筛选，不代表它变差了；\n")
		builder.WriteString("但掉出候选池的票本轮买不了，不要在建议里出现。\n")
	}

	builder.WriteString("\n## 六、行情快照（唯一可用数据源）\n")
	builder.WriteString("| 代码 | 名称 | 现价 | 昨收 | 涨跌幅 | 最高 | 最低 | 成交额(元) | 涨停价 | 跌停价 |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	watch := map[string]bool{}
	for _, symbol := range input.Watchlist {
		watch[symbol] = true
	}
	written := 0
	for _, quote := range input.Quotes {
		if written >= maxPromptQuotes {
			break
		}
		percent := PriceLimitPercent(quote.Symbol, quote.Name)
		upper := limitUpPrice(quote.PreviousClose, percent)
		lower := limitDownPrice(quote.PreviousClose, percent)
		fmt.Fprintf(&builder, "| %s | %s | %.2f | %.2f | %+.2f%% | %.2f | %.2f | %.0f | %.2f | %.2f |\n",
			quote.Symbol, quote.Name, quote.Price, quote.PreviousClose, quote.ChangePercent,
			quote.High, quote.Low, quote.Amount, upper, lower)
		written++
	}
	if written == 0 {
		builder.WriteString("| （无可用行情） | | | | | | | | | |\n")
	}
	if input.Board != nil && len(input.Board.Candidates) > 0 {
		board := input.Board
		fmt.Fprintf(&builder, "\n### 候选池构成\n")
		fmt.Fprintf(&builder, "本池由系统从全市场 %d 只标的中确定性筛出（不是模型挑选），已排除 ST 与退市风险、停牌、涨停与跌停、股价低于 2 元、以及成交额低于 %.0f 万元的标的。",
			board.SourceCount, board.ThresholdAmount/1e4)
		if board.Degraded {
			builder.WriteString("当日市场缩量，成交额门槛已下调。")
		}
		builder.WriteString("\n\n以下每只标的的入选依据都附带具体数字，请据此判断，不要仅凭名称联想题材：\n\n")
		for index, candidate := range board.Candidates {
			if index >= maxPromptCandidates {
				break
			}
			// 带上行业大类：模型需要看到"这五只其实是同一个风险"才能主动分散，
			// 否则它只会按个股强弱选，最后靠守卫去拦，白白浪费一轮计划。
			sector := candidate.Sector
			if sector == "" {
				sector = candidate.Industry
			}
			if sector != "" {
				sector = "[" + sector + "] "
			}
			fmt.Fprintf(&builder, "- %s %s%s（系统评分 %.1f）：%s\n",
				candidate.Symbol, candidate.Name, sector, candidate.Score, strings.Join(candidate.Reasons, "；"))
		}
		if len(board.Exclusions) > 0 {
			builder.WriteString("\n主要排除项（用于判断本池是否偏窄）：\n")
			for _, exclusion := range board.Exclusions {
				fmt.Fprintf(&builder, "- %s：%d 只\n", exclusion.Reason, exclusion.Count)
			}
		}
	} else if len(input.Watchlist) > 0 {
		fmt.Fprintf(&builder, "\n关注池：%s\n", strings.Join(input.Watchlist, ", "))
	}

	if input.Research != nil && (len(input.Research.Steps) > 0 || strings.TrimSpace(input.Research.Conclusion) != "") {
		builder.WriteString("\n## 本轮调研记录（你用只读工具查到的，不是他人观点）\n")
		for _, step := range input.Research.Steps {
			fmt.Fprintf(&builder, "第 %d 步 %s(%s)：\n%s\n\n", step.Index, step.Tool, compactArgs(step.Args), step.Result)
		}
		if conclusion := strings.TrimSpace(input.Research.Conclusion); conclusion != "" {
			fmt.Fprintf(&builder, "调研结论：%s\n", conclusion)
		}
		if input.Research.Degraded {
			builder.WriteString("注意：本轮调研中途中断，上面的记录不完整。缺数据时按缺数据处理，不要用常识补齐。\n")
		}
		// 这句是防幻觉的关键：模型很容易把"工具没返回"读成"市场上没有"。
		builder.WriteString("调研记录是事实，不是建议；没有查到不等于不存在，只等于没有查。\n")
	}

	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&builder, "\n## 七、补充材料\n%s\n", strings.TrimSpace(input.Notes))
	}

	builder.WriteString("\n## 八、判断要求\n")
	builder.WriteString("每条建议必须区分四件事：已发生且可核对的事实、尚未兑现的预期、下单的确认条件、判断失效的条件。\n")
	builder.WriteString("不要在 thesis 里堆砌形容词；如果某个判断缺少数据支持，直接标注「待验证」并降低权重或放弃该笔。\n")
	builder.WriteString("优先考虑流动性与风险回报比，而不是题材热度。没有把握时允许空仓。\n\n")

	builder.WriteString("## 九、输出格式\n")
	builder.WriteString("只输出一个 JSON 对象，不要 Markdown 代码块、不要解释文字、不要注释：\n")
	builder.WriteString(`{
  "regime": "进攻 | 防守 | 观望",
  "summary": "不超过 120 字，说明当前计划的核心逻辑与最大风险",
  "risk_notes": ["最多 5 条，写清楚什么情况下必须放弃本计划"],
  "orders": [
    {
      "symbol": "600000.SH",
      "name": "浦发银行",
      "side": "buy",
      "target_weight_percent": 8.5,
      "order_type": "limit",
      "limit_price": 10.23,
      "stop_loss_price": 9.80,
      "take_profit_price": 11.50,
      "expected_days": 5,
      "thesis": "不超过 60 字，写明事实依据与确认条件",
      "invalidation": {
        "price_below": 9.85,
        "deadline_days": 5,
        "sector_flow": "所属板块主力资金转为连续净流出",
        "note": "不超过 40 字，写清为什么这些条件成立就说明判断错了"
      },
      "confidence": 0.62
    }
  ]
}
`)
	builder.WriteString("\n卖出示例：side 为 sell，使用 \"sell_fraction_percent\": 50 表示卖出可卖数量的一半；此字段与 target_weight_percent 互斥。\n")
	builder.WriteString("confidence 取 0 到 1 之间的小数。orders 可以为空数组。\n")
	// 这一段必须讲清"哪些条件真的会被执行"，否则模型会以为整段话都只是写给人看的，
	// 于是随手填一个永远不成立的数字——那样的失效条件等于没有。
	builder.WriteString("\ninvalidation 的字段会被系统按下面的方式执行，请按此设计：\n")
	builder.WriteString("- price_below：现价跌破该价即视为判断失效，系统自动卖出该仓位一半（不清仓，剩下的交止损）。\n")
	builder.WriteString("- deadline_days：持有超过该交易日数仍未盈利，系统自动减仓一半；若届时为亏损则清仓。\n")
	builder.WriteString("- sector_flow：写明板块名（如 \"半导体 转为净流出\"），守卫会按主力净占比 ≤ -1% 自动判定并减仓；数据缺失时该条件本轮不判定。\n")
	builder.WriteString("- note：写给人看，系统不解析。\n")
	builder.WriteString("失效条件与止损的分工：止损是「价格到线」，失效是「逻辑不成立」。不要用失效条件替代止损，也不要写成同一个价位。\n")
	builder.WriteString("expected_days 是预期兑现天数（交易日），用于时间止损；不给则按计划周期推断。\n")
	return builder.String()
}

// orderStatusLabel 把委托状态说成中文。
// 提示词里出现 "partially_filled" 这种内部枚举，模型会把它当成标的名的一部分。
func orderStatusLabel(status OrderStatus) string {
	switch status {
	case OrderStatusPending:
		return "待成交"
	case OrderStatusPartial:
		return "部分成交"
	case OrderStatusFilled:
		return "已成交"
	case OrderStatusCancelled:
		return "已撤单"
	case OrderStatusRejected:
		return "已拒绝"
	case OrderStatusExpired:
		return "已失效"
	default:
		return string(status)
	}
}
