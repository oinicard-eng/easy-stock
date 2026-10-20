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
	fmt.Fprintf(&builder, "4. 每笔买入必须给出止损价，且止损价低于买入价，止损距离不超过 %.1f%%。\n", rules.MaxStopLossPercent)
	fmt.Fprintf(&builder, "5. 单票目标权重不超过 %.1f%%，单次最多 %d 笔建议。\n", rules.MaxSingleWeightPercent, rules.MaxOrders)
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
			builder.WriteString("\n")
		}
		builder.WriteString("  说明：止损价由持仓防守线自动执行，不需要你在 orders 里重复卖出这些标的。\n")
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
	// 明确边界：经验是事实描述，不是对本轮的建议。不写这句，模型会把
	// "上次止损的都亏了" 理解成 "这次不要止损"。
	builder.WriteString("经验只说明过去发生过什么，不构成对本轮的指令。每一笔仍要独立判断，止损纪律不因历史统计而放宽。\n")

	builder.WriteString("\n## 五、行情快照（唯一可用数据源）\n")
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
		fmt.Fprintf(&builder, "\n## 六、补充材料\n%s\n", strings.TrimSpace(input.Notes))
	}

	builder.WriteString("\n## 七、判断要求\n")
	builder.WriteString("每条建议必须区分四件事：已发生且可核对的事实、尚未兑现的预期、下单的确认条件、判断失效的条件。\n")
	builder.WriteString("不要在 thesis 里堆砌形容词；如果某个判断缺少数据支持，直接标注「待验证」并降低权重或放弃该笔。\n")
	builder.WriteString("优先考虑流动性与风险回报比，而不是题材热度。没有把握时允许空仓。\n\n")

	builder.WriteString("## 八、输出格式\n")
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
      "thesis": "不超过 60 字，写明事实依据与确认条件",
      "invalidation": "不超过 40 字，写明什么情况说明判断失效",
      "confidence": 0.62
    }
  ]
}
`)
	builder.WriteString("\n卖出示例：side 为 sell，使用 \"sell_fraction_percent\": 50 表示卖出可卖数量的一半；此字段与 target_weight_percent 互斥。\n")
	builder.WriteString("confidence 取 0 到 1 之间的小数。orders 可以为空数组。\n")
	return builder.String()
}
