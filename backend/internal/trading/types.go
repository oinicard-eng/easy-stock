// Package trading implements the paper-trading account ledger and the
// LLM-driven trade planning agent.
//
// 设计边界（必须遵守）：
//  1. 模型只负责判断与排序，不负责取数、算股数、算费用。行情快照、可用资金、
//     持仓可卖数量全部由 Go 侧注入，避免模型编造价格或超买。
//  2. 所有订单在下发前必须通过确定性风控校验；校验失败的计划不会被送入撮合。
//  3. 默认 Broker 是本地模拟撮合，永远不触达券商。真实券商通道只保留接口，
//     未实现即返回 ErrBrokerUnavailable。
package trading

import (
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/cognition"
	"easy-stock/backend/internal/trading/policy"
	"easy-stock/backend/internal/trading/universe"
)

type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

type OrderType string

const (
	OrderTypeLimit  OrderType = "limit"
	OrderTypeMarket OrderType = "market"
)

// 订单来源。复盘必须按来源分组：止损认赔与模型主动离场是两种能力，
// 混在一起统计会让人得出"模型胜率 40%"这种既不能证明也不能证伪的结论。
const (
	OrderSourceModel      = "model"
	OrderSourceProtective = "protective"
	OrderSourceManual     = "manual"
)

// 离场原因。它是复盘的第一维：什么原因离场，决定了该修哪一段。
const (
	TriggerStopLoss   = "stop_loss"
	TriggerTakeProfit = "take_profit"
	TriggerTrailStop  = "trail_stop"
	TriggerHardStop   = "hard_stop"
	TriggerModelExit  = "model_exit"
	TriggerManualExit = "manual_exit"
	// TriggerInvalidation 表示买入时写下的失效条件已成立，触发减仓。
	// 与 stop_loss 的区别是力度：失效是"论点弱了"，止损是"价格到线了"。
	TriggerInvalidation = "invalidation"
	// TriggerTimeStop 表示超出预期兑现期限仍未盈利，触发减仓或清仓。
	TriggerTimeStop  = "time_stop"
	TriggerNoTrigger = ""
)

type PlanStatus string

const (
	// PlanStatusRunning 表示模型正在生成计划。
	PlanStatusRunning PlanStatus = "running"
	// PlanStatusDraft 表示计划已生成并通过或部分通过校验，等待人工确认。
	PlanStatusDraft PlanStatus = "draft"
	// PlanStatusExecuted 表示用户已确认，订单已下发到模拟撮合。
	PlanStatusExecuted PlanStatus = "executed"
	// PlanStatusRejected 表示用户主动放弃该计划。
	PlanStatusRejected PlanStatus = "rejected"
	// PlanStatusSkipped 表示本轮输入与上次模型调用时完全一致，刻意没有调用模型。
	// 它与 failed 必须分开：跳过是系统在省钱，失败是系统坏了，混在一起会让
	// 失败率虚高，最后没人再看那个数字。
	PlanStatusSkipped PlanStatus = "skipped"
	// PlanStatusEmpty 表示模型看过全盘后主动选择不操作（一笔建议都没提）。
	// 它必须与 failed 分开，理由和 skipped 一样：空仓是一条结论，不是故障。
	// 混成 failed 之后，计划列表会连着排"未通过"，用户以为系统坏了，
	// 实际是它正确地拒绝了交易——这种误判会让人去改一个不该改的地方。
	PlanStatusEmpty PlanStatus = "empty"
	// PlanStatusFailed 表示生成或校验失败。
	PlanStatusFailed PlanStatus = "failed"
)

type OrderStatus string

const (
	OrderStatusPending   OrderStatus = "pending"
	OrderStatusFilled    OrderStatus = "filled"
	OrderStatusPartial   OrderStatus = "partially_filled"
	OrderStatusCancelled OrderStatus = "cancelled"
	OrderStatusRejected  OrderStatus = "rejected"
	// OrderStatusExpired 表示当日委托在收盘后自动失效。
	// 它与 cancelled 的区别在于责任归属：cancelled 是有人主动撤单，
	// expired 是市场规则到期——复盘时把两者混在一起，会看不清"为什么没成交"。
	OrderStatusExpired OrderStatus = "expired"
)

type Account struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	InitialCash float64 `json:"initial_cash"`
	// Cash 是账面现金总额，包含已被挂单冻结的部分。
	Cash float64 `json:"cash"`
	// ReservedCash 是未成交买入挂单占用的资金，不能用于新开仓。
	// 它不是一个统计口径，而是账本的一部分：由 Reserve/Release 在同一事务内维护。
	ReservedCash float64   `json:"reserved_cash"`
	MarketValue  float64   `json:"market_value"`
	Equity       float64   `json:"equity"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// PnL 是账户盈亏的口径拆解，只在对外展示时填充，不落库。
	// 存在的理由只有一个：让人能自己验算，而不是面对两个互相矛盾的汇总数字。
	PnL *PnLSplit `json:"pnl,omitempty"`
}

// PnLSplit 把"总资产为什么变成现在这样"拆成可以直接相加的项。
//
//	Change = Realized + Unrealized + Residual
//
// Realized 与 Unrealized 都是净额：成本已含买入费，已实现已扣卖出费。
// 因此 Fees 只是信息项（告诉用户摩擦成本有多大），不参与上面的等式——
// 再减一次就等于把费用算两遍。
//
// Residual 是未解释的残差。口径统一时它应当约等于 0；
// 不为 0 就说明账面还有没对齐的地方，要如实报出来，而不是抹平。
type PnLSplit struct {
	InitialCash float64 `json:"initial_cash"`
	Equity      float64 `json:"equity"`
	Change      float64 `json:"change"`
	Realized    float64 `json:"realized"`
	Unrealized  float64 `json:"unrealized"`
	Fees        float64 `json:"fees"`
	Residual    float64 `json:"residual"`
	Aligned     bool    `json:"aligned"`
}

// AvailableCash 是本账本唯一的「还能花多少钱」口径。
// 任何风控、下单、提示词都必须用它，而不是 Cash。
func (a Account) AvailableCash() float64 {
	return round2(a.Cash - a.ReservedCash)
}

// CashForOrder 返回撮合某笔订单时真正可用于它的资金。
// 已扣除其他挂单的冻结额，但不扣除本单自身冻结——本单成交正是要花这笔钱。
func (a Account) CashForOrder(order Order) float64 {
	others := a.ReservedCash - order.ReservedAmount
	if others < 0 {
		others = 0
	}
	return round2(a.Cash - others)
}

type Position struct {
	AccountID   string  `json:"account_id"`
	Symbol      string  `json:"symbol"`
	Name        string  `json:"name"`
	Quantity    int     `json:"quantity"`
	Available   int     `json:"available_quantity"`
	AverageCost float64 `json:"average_cost"`
	LastPrice   float64 `json:"last_price"`
	MarketValue float64 `json:"market_value"`
	Unrealized  float64 `json:"unrealized_pnl"`
	// ReservedQuantity 是未成交卖出挂单占用的股数。
	// 没有它，同一个持仓可以被两笔卖单各卖一次：两笔都通过"可卖数量"校验，
	// 成交时第二笔才发现不够。资金有 reserved_cash，股数也必须有一份。
	ReservedQuantity int `json:"reserved_quantity"`
	// StopLossPrice / TakeProfitPrice 是这只持仓的防守线，由买入时的模型建议写入，
	// 之后由保护逻辑维护（止盈后把止损上移到成本价）。
	StopLossPrice   float64 `json:"stop_loss_price,omitempty"`
	TakeProfitPrice float64 `json:"take_profit_price,omitempty"`
	// PeakPrice 是持仓期间见过的最高价，移动止损用它算回撤。
	PeakPrice float64 `json:"peak_price,omitempty"`
	// ReduceCount 记录已经执行过几级止盈。分级止盈必须记住"减过几次"，
	// 否则每次触价都会再卖一次，最后把仓位清空在最不该清空的时候。
	ReduceCount int `json:"reduce_count,omitempty"`
	// Sector 是行业大类，用于复盘归因（没有行业信息时为空，不猜）。
	Sector string `json:"sector,omitempty"`
	// OpenedAt 是建仓时间，持仓天数与复盘归因都要用。
	OpenedAt    *time.Time `json:"opened_at,omitempty"`
	LastBuyDate string     `json:"last_buy_date,omitempty"`
	// Thesis / Invalidation 是买入时的理由与失效条件原文。
	// 持仓上必须留一份：计划会过期被清理，而持仓还在，界面要能回答
	// "我当初为什么买它"——这个问题不能靠翻三个月前的计划来找。
	Thesis       string `json:"thesis,omitempty"`
	Invalidation string `json:"invalidation,omitempty"`
	// InvalidationRule 是失效条件的可执行形态，由守卫按现价与持有天数判定。
	InvalidationRule *InvalidationSpec `json:"invalidation_rule,omitempty"`
	// ExpectedDays 是预期兑现天数（交易日），时间止损的阈值来源。
	ExpectedDays int `json:"expected_days,omitempty"`
	// AutoReduceAt 记录失效条件触发过一次自动减仓的时刻。
	// 同一持仓只自动减一次：每轮都砍半，会在一次误判里用四个轮次清空仓位。
	AutoReduceAt *time.Time `json:"auto_reduce_at,omitempty"`
	// EntryRegime / EntryConfidence 是建仓时的市场状态与模型自评置信度。
	// 它们回答"这笔仓位是在什么环境下、以多大把握建的"——两三个月后
	// 复盘时，只有这两个字段还在持仓上，计划早就被清理了。
	EntryRegime     string  `json:"entry_regime,omitempty"`
	EntryConfidence float64 `json:"entry_confidence,omitempty"`
	// ReviewCount 是这只持仓被论点复核过的轮次数。
	// 与持有天数并列给出："持有了 12 天、复核 40 次都没失效"比"12 天"
	// 更能说明论点是被持续验证过的，而不是买入后没人再看。
	ReviewCount int       `json:"review_count,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Sellable 是本账本唯一的「还能卖多少股」口径。
// 与 AvailableCash 对称：可用股数 = 已解禁股数 - 挂单已占用股数。
func (p Position) Sellable() int {
	sellable := p.Available - p.ReservedQuantity
	if sellable < 0 {
		return 0
	}
	return sellable
}

// ProfitPercent 返回当前浮盈百分比，成本无效时返回 0。
func (p Position) ProfitPercent() float64 {
	if p.AverageCost <= 0 {
		return 0
	}
	return round2((p.LastPrice/p.AverageCost - 1) * 100)
}

// WeightPercent 返回该持仓占账户总市值的百分比；equity<=0 时返回 0。
func (p Position) WeightPercent(equity float64) float64 {
	if equity <= 0 {
		return 0
	}
	return round2(p.MarketValue / equity * 100)
}

type Order struct {
	ID         string `json:"id"`
	PlanID     string `json:"plan_id"`
	AccountID  string `json:"account_id"`
	DecisionID string `json:"decision_id,omitempty"`
	CycleID    string `json:"cycle_id,omitempty"`
	Symbol     string `json:"symbol"`
	Name       string `json:"name"`
	// Sector 是行业归并后的大类，用于行业集中度守卫与复盘归因。
	// 为空表示当时无行业信息：此时不参与行业上限判断，而不是猜一个。
	Sector     string    `json:"sector,omitempty"`
	Side       Side      `json:"side"`
	OrderType  OrderType `json:"order_type"`
	Quantity   int       `json:"quantity"`
	LimitPrice float64   `json:"limit_price"`
	// TargetWeightPercent 是模型给出的目标权重，股数由 Go 侧按该权重换算。
	TargetWeightPercent float64 `json:"target_weight_percent,omitempty"`
	StopLossPrice       float64 `json:"stop_loss_price,omitempty"`
	TakeProfitPrice     float64 `json:"take_profit_price,omitempty"`
	// ChannelOrderID 是券商侧的委托编号。模拟盘为空；
	// 实盘必须记下来，否则撤单与回报匹配都无从下手。
	ChannelOrderID string      `json:"channel_order_id,omitempty"`
	Status         OrderStatus `json:"status"`
	FilledQuantity int         `json:"filled_quantity"`
	FilledPrice    float64     `json:"filled_price"`
	Fee            float64     `json:"fee"`
	// ReservedAmount 是该订单当前仍冻结的资金额。成交与撤销时按实际金额释放，
	// 不做比例推算，避免四舍五入把冻结额算漂。
	ReservedAmount float64 `json:"reserved_amount,omitempty"`
	// ReservedQuantity 是卖单冻结的股数，逻辑与 ReservedAmount 对称。
	ReservedQuantity int `json:"reserved_quantity,omitempty"`
	// Source 说明这笔订单是谁的意志：model（模型建议）、protective（防守线自动触发）、
	// manual（人工）。复盘时把"止损认赔"和"模型看错"混在一起统计，胜率就没有意义了。
	Source string `json:"source,omitempty"`
	// Trigger 是触发原因（stop_loss / take_profit / trail_stop / model_exit）。
	// 它是复盘的第一维：什么原因离场，决定了这套系统该修哪一段。
	Trigger string `json:"trigger,omitempty"`
	// RealizedPnL 是卖单成交后的已实现盈亏，由账本写入，模型无法提供。
	RealizedPnL float64 `json:"realized_pnl,omitempty"`
	// FrozenAt 记录冻结发生的时间，用于识别长期未成交的僵尸挂单。
	FrozenAt     *time.Time `json:"frozen_at,omitempty"`
	RejectReason string     `json:"reject_reason,omitempty"`
	// RejectCode 是拦截规则的代码（如 sector.weight）。只存中文原因不存代码，
	// 下一轮提示词就只能原样复述一句人话，模型无法据此判断"该改什么"。
	RejectCode   string `json:"reject_code,omitempty"`
	Thesis       string `json:"thesis,omitempty"`
	Invalidation string `json:"invalidation,omitempty"`
	// InvalidationRule 是失效条件的可执行形态。与上面的 Invalidation 文本并存：
	// 文本给人看、给模型看，结构给守卫判。两者必须同时落库，
	// 否则要么复盘读不懂、要么代码判不了。
	InvalidationRule *InvalidationSpec `json:"invalidation_rule,omitempty"`
	// ExpectedDays 是模型预期的兑现天数（交易日）。时间止损的口径，
	// 与 stop_loss_price 一起构成"什么价位、什么时限"这对完整约束。
	ExpectedDays int `json:"expected_days,omitempty"`
	// EntryRegime 是下单时的市场状态，成交后原样写进持仓。
	// 在订单上而不是仅在计划上：持仓要能独立回答"建仓时的环境"，
	// 而计划的生命周期比持仓短得多。
	EntryRegime string    `json:"entry_regime,omitempty"`
	Confidence  float64   `json:"confidence,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IsOpen 表示该订单仍可能占用资金与可卖数量。
func (o Order) IsOpen() bool {
	return o.Status == OrderStatusPending || o.Status == OrderStatusPartial
}

type Fill struct {
	ID        string    `json:"id"`
	OrderID   string    `json:"order_id"`
	AccountID string    `json:"account_id"`
	Symbol    string    `json:"symbol"`
	Side      Side      `json:"side"`
	Quantity  int       `json:"quantity"`
	Price     float64   `json:"price"`
	Fee       float64   `json:"fee"`
	CreatedAt time.Time `json:"created_at"`
}

// Issue 是校验产生的问题。Level=error 时该笔订单会被拒绝，warning 仅提示。
// Code 是稳定标识（见 policy 包的 Code* 常量），用于统计与前端着色；
// 它是后加的字段，旧数据没有 Code 时按空串处理。
type Issue struct {
	Level   string `json:"level"`
	Field   string `json:"field"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

const (
	IssueError   = "error"
	IssueWarning = "warning"
)

// Snapshot 是本次计划所依据的确定性输入，随计划一起持久化，便于事后复盘。
type Snapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Equity  float64   `json:"equity"`
	Cash    float64   `json:"cash"`
	// ReservedCash 是快照时刻已被挂单冻结的资金。风控可花的钱是 Cash-ReservedCash，
	// 少了这一项，多笔挂单会共享同一笔现金。
	ReservedCash float64 `json:"reserved_cash"`
	// OpenBuyValue 按标的汇总未成交买入挂单的剩余金额，用于把挂单计入单票权重上限。
	OpenBuyValue map[string]float64 `json:"open_buy_value,omitempty"`
	// PendingBuyValue 是上述挂单金额合计，用于计算买入后的预计总仓位。
	PendingBuyValue float64            `json:"pending_buy_value"`
	Quotes          []foundation.Quote `json:"quotes"`
	Positions       []Position         `json:"positions"`
	// ATRs 是裁决时刻各票 ATR 占现价的百分比，供波动率定价使用。
	//
	// 为什么放在快照里而不是议决时现取：沙盘要用同一份输入重放，
	// 现取会拿今天的波动率去重算上周的计划，差异里就混进了"时间变化"。
	// 它不进变化检测指纹——ATR 每天都在变，算进去会让安静模式永不生效。
	ATRs          map[string]float64 `json:"atrs,omitempty"`
	Universe      []string           `json:"universe"`
	MissingQuotes []string           `json:"missing_quotes,omitempty"`
	// Market 是本轮对市场状态的判断。未评估时 Evaluated 为 false，
	// 与"评估了但样本不足（unknown）"是两件事：前者不限制交易，后者必须禁止开仓。
	Market cognition.MarketView `json:"market"`
	// Board 是自动选股的完整结果，含入选理由与被排除项。
	// 显式指定标的时为空，此时 Universe 直接等于调用方给出的列表。
	Board *universe.Board `json:"board,omitempty"`
	// AutoUniverse 表示本轮候选池由系统自动生成，而非调用方指定。
	AutoUniverse bool `json:"auto_universe"`
	// Sectors 是本次涉及标的的行业归并结果（代码 → 大类）。
	// 只存本次涉及的几十个，不是全市场映射：快照要随计划一起落库，不能塞进 5000 条。
	Sectors map[string]string `json:"sectors,omitempty"`
	// Cooldowns 是本次计划时生效的冷静期约束（止损后禁买）。
	// 随快照落库，被拦的那笔建议三个月后还能回答"当时为什么不让我买"。
	Cooldowns CooldownView `json:"cooldowns,omitempty"`
	// ATRPercent 是本次涉及标的的波动率（ATR14 占现价百分比）。
	// 它不是行情，而是"这只票正常一天走多远"的度量；守卫用它决定移动止损
	// 什么时候启用、留多宽的呼吸空间。缺失时不猜：退回配置里的固定百分比。
	// 刻意不进变化检测指纹——ATR 每天都在变，把它算进指纹会让"什么都没变"
	// 这一判断永远不成立，安静模式立刻失效。
	ATRPercent map[string]float64 `json:"atr_percent,omitempty"`
	// SectorFlows 是板块资金流（板块名 → 主力净占比），供失效条件里的
	// sector_flow 成员做代码判定。nil 表示守卫判不了这一条件：
	// 数据源未配置或取数失败都要与"资金未转负"严格区分。
	// 只在持仓带板块失效条件时才取数，普通轮次不多花这个请求。
	SectorFlows map[string]float64 `json:"sector_flows,omitempty"`
	// ExposureCeiling 是本轮实际生效的总仓位上限（百分比）。
	// 它已经含了"市场预算与风控上限取较小值"，并叠加了同日内只升不降的
	// 迟滞（见 exposure_ceiling.go）。为 0 表示未计算，消费方退回按
	// Market.MaxExposurePercent 现算——沙盘回放老快照时会走到这条路径。
	ExposureCeiling float64 `json:"exposure_ceiling,omitempty"`
}

// AvailableCash 返回快照时刻真正可用于新开仓的资金。
func (s Snapshot) AvailableCash() float64 {
	return round2(s.Cash - s.ReservedCash)
}

type Plan struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Status        PlanStatus `json:"status"`
	TraderProfile string     `json:"trader_profile"`
	Horizon       string     `json:"horizon"`
	Regime        string     `json:"regime"`
	// Gate 记录生成计划时账户总闸的状态，用于复盘时区分「模型判断错了」
	// 与「当时本来就不该开仓」。
	Gate      string   `json:"gate,omitempty"`
	Summary   string   `json:"summary"`
	RiskNotes []string `json:"risk_notes,omitempty"`
	// DecisionID 指向那一次的裁决流水。计划是"结果"，裁决才是"为什么"。
	DecisionID string `json:"decision_id,omitempty"`
	// CycleID 指向产生该计划的自动循环。手工生成的计划为空。
	// 有了它，"这笔单是哪个时间点、哪一轮判断下的"才能一跳到底。
	CycleID string `json:"cycle_id,omitempty"`
	// Verdicts 是裁决流水的副本，随计划一起返回，前端不必再发一次请求。
	// 副本只用于展示；统计与复盘一律读 decisions 表，避免两处口径不一致。
	Verdicts []policy.Verdict `json:"verdicts"`
	// Exposure 记录决策前后的仓位与预算，回答"为什么只买了这么点"。
	Exposure *Exposure `json:"exposure,omitempty"`
	// RiskReview 是计划级风险复核报告：它从组合视角回答"这一整套方案安全吗"。
	// 与 Verdicts 的区别：裁决是逐笔的通过/拒绝，复核是整体额度的余量。
	RiskReview *PlanRiskReview `json:"risk_review,omitempty"`
	Orders     []Order         `json:"orders"`
	Issues     []Issue         `json:"issues,omitempty"`
	Snapshot   Snapshot        `json:"snapshot"`
	Rules      RiskRules       `json:"rules"`
	// Research 是生成计划前的调研记录：查了什么、看到什么、得出什么事实结论。
	// 它不是计划的一部分，而是计划的依据；放在这里是为了让"为什么这么买"
	// 能和计划一起被打开，而不是散落在另一张表里等人去拼。
	Research     *ResearchResult `json:"research,omitempty"`
	ErrorMessage string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	ExecutedAt   *time.Time      `json:"executed_at,omitempty"`
}

// RejectedCount 统计被风控拒绝的订单数量，前端据此提示"模型建议未被全部采纳"。
func (p Plan) RejectedCount() int {
	count := 0
	for _, order := range p.Orders {
		if order.Status == OrderStatusRejected {
			count++
		}
	}
	return count
}

// Control 是账户级的总闸。它必须落库，因为自动交易在进程重启后
// 仍然要保持「我昨天下午被熔断了」这个事实。
//
// 两级语义刻意分开：
//   - PauseBuying：只停新增买入，卖出与减仓照常执行（保护性动作不该被拦）；
//   - KillSwitch：全部停止，不生成计划、不允许任何挂单成交。
//     kill_switch 打开时 pause_buying 自动为真，反之不成立。
type Control struct {
	AccountID   string    `json:"account_id"`
	PauseBuying bool      `json:"pause_buying"`
	KillSwitch  bool      `json:"kill_switch"`
	Reason      string    `json:"reason,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Gate 返回当前允许的交易动作描述，供前端与决策层显示。
func (c Control) Gate() string {
	switch {
	case c.KillSwitch:
		return "全部停止"
	case c.PauseBuying:
		return "暂停买入"
	default:
		return "正常运行"
	}
}

// ReconcileReport 是对账结果。它不是日志，而是可被前端与复盘读取的结构化事实。
type ReconcileReport struct {
	AccountID string    `json:"account_id"`
	TakenAt   time.Time `json:"taken_at"`
	OK        bool      `json:"ok"`
	// Cash 与 ReservedCash 是账本记账值；ExpectedReserved 是按挂单重算出的应收冻结额。
	Cash             float64 `json:"cash"`
	ReservedCash     float64 `json:"reserved_cash"`
	ExpectedReserved float64 `json:"expected_reserved"`
	Drift            float64 `json:"drift"`
	// DriftBefore 是修复发生前的原始差额。修复会把 Drift 归零，
	// 没有这个字段时"修了多少"只能从 Issues 的文案里反解析，非常脆弱。
	DriftBefore   float64 `json:"drift_before,omitempty"`
	MarketValue   float64 `json:"market_value"`
	Equity        float64 `json:"equity"`
	OpenBuyOrders int     `json:"open_buy_orders"`
	// ExpectedReservedQuantity 按未成交卖单重算的应收股数，与持仓里的
	// ReservedQuantity 逐标的比对；ReservedQuantityDrift 非空即为不一致。
	ExpectedReservedQuantity int               `json:"expected_reserved_quantity"`
	ReservedQuantity         int               `json:"reserved_quantity"`
	ReservedQuantityDrift    map[string]string `json:"reserved_quantity_drift,omitempty"`
	OrphanOrders             []string          `json:"orphan_orders,omitempty"`
	Issues                   []Issue           `json:"issues,omitempty"`
	Repaired                 bool              `json:"repaired"`
}

// ClosedTrade 是一笔已实现平仓。它是「复盘」唯一可信的原子事实来源：
// 未实现盈亏会随行情跳动，做不了归因；已实现盈亏是既成事实。
//
// 一次卖出成交记一条，而不是把一轮持仓合并成一条：
// 分批止盈的每一批成交价不同，合并后"这笔赚了多少"会变成加权平均，
// 而复盘要回答的恰恰是"第一次止盈的位置选得对不对"。
type ClosedTrade struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	Sector    string `json:"sector,omitempty"`
	Quantity  int    `json:"quantity"`
	// EntryPrice 是卖出时刻的持仓均价，含买入费用分摊。
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	Fee        float64 `json:"fee"`
	// PnL 已扣除本笔卖出费用；买入费用已含在均价里，不重复扣。
	PnL        float64 `json:"pnl"`
	PnLPercent float64 `json:"pnl_percent"`
	// Trigger 是离场原因，与 Order.Trigger 同源。
	Trigger string `json:"trigger,omitempty"`
	// Source 区分模型主动离场与防守线自动离场。
	Source     string     `json:"source,omitempty"`
	OrderID    string     `json:"order_id"`
	DecisionID string     `json:"decision_id,omitempty"`
	CycleID    string     `json:"cycle_id,omitempty"`
	OpenedAt   *time.Time `json:"opened_at,omitempty"`
	ClosedAt   time.Time  `json:"closed_at"`
	HoldDays   int        `json:"hold_days"`
	TradingDay string     `json:"trading_day"`
	// PeakPrice 是持仓期间出现过的最高价。
	// 平仓后持仓行被删除，这个值不写进平仓记录就永久丢失；
	// 而"曾经浮盈 12%、最后只赚 1%"是最有复盘价值的一类事实，
	// 它直接回答"止盈是不是设得太贪"。
	PeakPrice float64 `json:"peak_price,omitempty"`
}

// Win 表示这笔平仓是赚钱的。手续费已计入，因此"赚 1 分钱"也算赢，
// 不做"超过手续费才算赢"的二次判断——那会让统计口径与账本不一致。
func (c ClosedTrade) Win() bool { return c.PnL > 0 }

// RiskRules 是确定性风控阈值。模型只能在提示词中被约束，真正的拒绝发生在这里。
//
// 它是 policy.Limits 的别名而不是另一套结构：两处各写一份数字，
// 迟早会出现"提示词说 15%、实际校验 20%"这种无从察觉的错位。
type RiskRules = policy.Limits

// DefaultRiskRules 返回基准阈值。
func DefaultRiskRules() RiskRules { return policy.Default() }
