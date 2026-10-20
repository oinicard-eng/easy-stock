package trading

import (
	"encoding/json"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/trading/policy"
)

// 失效条件的验收标准只有两条：写成人话也能被解析成可执行条件；
// 条件成立时只减仓、且只减一次。

func TestParseInvalidationTextExtractsActionableParts(t *testing.T) {
	cases := []struct {
		text     string
		below    float64
		deadline int
	}{
		{"跌破 9.5 则离场", 9.5, 0},
		{"跌穿9.80，5 个交易日内不兑现就减仓", 9.8, 5},
		{"板块资金转出就减仓", 0, 0},
		{"3 日内不能站上均线", 0, 3},
		{"", 0, 0},
	}
	for _, item := range cases {
		spec := ParseInvalidationText(item.text)
		if spec.PriceBelow != item.below {
			t.Fatalf("%q 的价格条件应为 %.2f，实际 %.2f", item.text, item.below, spec.PriceBelow)
		}
		if spec.DeadlineDays != item.deadline {
			t.Fatalf("%q 的期限应为 %d，实际 %d", item.text, item.deadline, spec.DeadlineDays)
		}
		if item.text != "" && spec.Note != item.text {
			t.Fatalf("%q 的原话应保留，实际 %q", item.text, spec.Note)
		}
	}
}

func TestInvalidationSpecAcceptsBothJSONForms(t *testing.T) {
	// 对象形态：提示词要求的格式。
	var object InvalidationSpec
	if err := json.Unmarshal([]byte(`{"price_below":9.85,"deadline_days":5,"note":"跌破就走"}`), &object); err != nil {
		t.Fatalf("解析对象失败：%v", err)
	}
	if object.PriceBelow != 9.85 || object.DeadlineDays != 5 {
		t.Fatalf("对象形态解析错误：%+v", object)
	}
	// 字符串形态：模型经常无视格式要求直接写人话，必须仍然可用。
	var text InvalidationSpec
	if err := json.Unmarshal([]byte(`"跌破 9.6 或资金转出"`), &text); err != nil {
		t.Fatalf("解析字符串失败：%v", err)
	}
	if text.PriceBelow != 9.6 {
		t.Fatalf("字符串形态应提取出价格条件：%+v", text)
	}
	if text.Note == "" {
		t.Fatal("字符串形态应保留原话")
	}
}

func TestInvalidationTriggeredByPriceAndDeadline(t *testing.T) {
	spec := InvalidationSpec{PriceBelow: 9.5, DeadlineDays: 5}
	if hit, _ := spec.Triggered(9.6, 0); hit {
		t.Fatal("未跌破时不应触发")
	}
	if hit, reason := spec.Triggered(9.4, 0); !hit {
		t.Fatalf("跌破应触发：%s", reason)
	}
	// 5 个交易日折算成自然日后才比较：周五建仓的票不该在周一就被判超期。
	if hit, _ := spec.Triggered(10, 5); hit {
		t.Fatal("5 个自然日还不该算超期（约 3.5 个交易日）")
	}
	if hit, _ := spec.Triggered(10, 9); !hit {
		t.Fatal("9 个自然日应算超期")
	}
}

// guardTestPosition 造一只持仓：成本 10 元，现价由用例给出。
func guardTestPosition(price float64) Position {
	opened := time.Now().Add(-3 * 24 * time.Hour)
	return Position{
		AccountID:   DefaultAccountID,
		Symbol:      "600000.SH",
		Name:        "浦发银行",
		Quantity:    1000,
		Available:   1000,
		AverageCost: 10,
		LastPrice:   price,
		MarketValue: price * 1000,
		OpenedAt:    &opened,
	}
}

func TestGuardReducesHalfWhenInvalidationHits(t *testing.T) {
	position := guardTestPosition(9.4)
	position.InvalidationRule = &InvalidationSpec{PriceBelow: 9.5}
	snapshot := Snapshot{
		Equity:    10000,
		Positions: []Position{position},
		Quotes:    []foundation.Quote{testQuote("600000.SH", "浦发银行", 9.4, 9.8)},
	}
	result := BuildProtectiveOrders(snapshot, DefaultRiskRules(), DefaultGuardConfig(), time.Now())
	if len(result.Orders) != 1 {
		t.Fatalf("应产生 1 笔减仓，实际 %d：%+v", len(result.Orders), result.Verdicts)
	}
	order := result.Orders[0]
	if order.Trigger != TriggerInvalidation {
		t.Fatalf("触发原因应为失效条件，实际 %s", order.Trigger)
	}
	// 关键边界：失效条件只减半，不清仓。
	if order.Quantity != 500 {
		t.Fatalf("应减仓 500 股（50%%），实际 %d 股", order.Quantity)
	}
	if len(result.Verdicts) != 1 || result.Verdicts[0].Code != policy.CodeGuardInvalidation {
		t.Fatalf("裁决码应为 guard.invalidation：%+v", result.Verdicts)
	}
	if result.Verdicts[0].Level != policy.LevelAdjust {
		t.Fatalf("失效条件是调整而非否决，实际 %s", result.Verdicts[0].Level)
	}
}

func TestGuardSkipsInvalidationAfterAutoReduce(t *testing.T) {
	position := guardTestPosition(9.4)
	position.InvalidationRule = &InvalidationSpec{PriceBelow: 9.5}
	reduced := time.Now().Add(-time.Hour)
	position.AutoReduceAt = &reduced
	snapshot := Snapshot{
		Equity:    10000,
		Positions: []Position{position},
		Quotes:    []foundation.Quote{testQuote("600000.SH", "浦发银行", 9.4, 9.8)},
	}
	result := BuildProtectiveOrders(snapshot, DefaultRiskRules(), DefaultGuardConfig(), time.Now())
	for _, order := range result.Orders {
		if order.Trigger == TriggerInvalidation {
			t.Fatalf("已自动减过一次的持仓不应再次触发失效减仓：%+v", order)
		}
	}
}

func TestGuardTimeStopClearsLosingPosition(t *testing.T) {
	position := guardTestPosition(9.6) // 浮亏 4%，未触及 10% 硬止损
	position.ExpectedDays = 5
	opened := time.Now().Add(-15 * 24 * time.Hour)
	position.OpenedAt = &opened
	snapshot := Snapshot{
		Equity:    10000,
		Positions: []Position{position},
		Quotes:    []foundation.Quote{testQuote("600000.SH", "浦发银行", 9.6, 9.8)},
	}
	result := BuildProtectiveOrders(snapshot, DefaultRiskRules(), DefaultGuardConfig(), time.Now())
	if len(result.Orders) != 1 || result.Orders[0].Trigger != TriggerTimeStop {
		t.Fatalf("超期亏损应触发时间止损：%+v", result.Verdicts)
	}
	if result.Orders[0].Quantity != 1000 {
		t.Fatalf("超期且亏损应清仓，实际卖 %d 股", result.Orders[0].Quantity)
	}
}

func TestGuardTimeStopKeepsWinner(t *testing.T) {
	position := guardTestPosition(11) // 浮盈 10%，远超及格线
	position.ExpectedDays = 5
	opened := time.Now().Add(-30 * 24 * time.Hour)
	position.OpenedAt = &opened
	snapshot := Snapshot{
		Equity:    10000,
		Positions: []Position{position},
		Quotes:    []foundation.Quote{testQuote("600000.SH", "浦发银行", 11, 9.8)},
	}
	result := BuildProtectiveOrders(snapshot, DefaultRiskRules(), DefaultGuardConfig(), time.Now())
	for _, order := range result.Orders {
		if order.Trigger == TriggerTimeStop {
			t.Fatalf("盈利超过及格线的仓位不该被时间止损：%+v", order)
		}
	}
}
