package trading

import (
	"strings"
	"testing"
)

// 移动止损必须随波动率缩放，这条守的是"同一套 4%/6% 对所有票"的老问题。
// 场景：成本 10、峰值 12、现价 11.2 —— 浮盈 12%，距峰值回撤 6.67%。
// 低波动股（ATR 1%）3% 的宽度就该走；高波动股（ATR 5%）12% 的宽度不该走。
func TestGuardTrailParamsScaleWithATR(t *testing.T) {
	cases := []struct {
		atrPercent   float64
		wantArm      float64
		wantDistance float64
	}{
		{0, 4, 6},     // 没有 ATR：退回配置值，不猜
		{1, 2, 3},     // 下限：1.5×1=1.5 → 2；2.5×1=2.5 → 3
		{3, 4.5, 7.5}, // 正常区间
		{6, 8, 12},    // 上限：9 → 8；15 → 12
		{0.5, 2, 3},   // 极低波动仍保底
	}
	for _, item := range cases {
		arm, distance := guardTrailParams(DefaultGuardConfig(), item.atrPercent)
		if arm != item.wantArm || distance != item.wantDistance {
			t.Fatalf("ATR %.1f%% 应为门槛 %.1f/宽度 %.1f，实际 %.1f/%.1f",
				item.atrPercent, item.wantArm, item.wantDistance, arm, distance)
		}
		if arm > distance {
			t.Fatalf("ATR %.1f%%：门槛 %.1f 不得高于宽度 %.1f（否则一浮盈就卖）", item.atrPercent, arm, distance)
		}
	}
}

func TestEvaluatePositionTrailRespectsVolatility(t *testing.T) {
	position := Position{
		Symbol: "600000.SH", Quantity: 1000, AverageCost: 10,
		StopLossPrice: 9, TakeProfitPrice: 20, PeakPrice: 12,
	}
	rules := DefaultRiskRules()
	config := DefaultGuardConfig()

	// 无 ATR：沿用 4%/6%，回撤 6.67% 触发。
	if trigger, _, _ := evaluatePosition(position, 11.2, 3, 0, rules, config, nil); trigger != TriggerTrailStop {
		t.Fatalf("没有波动率数据时应按 4%%/6%% 判定并触发，实际 %s", trigger)
	}

	// 高波动（5%）：宽度 12%，同样的回撤不该离场。
	if trigger, _, _ := evaluatePosition(position, 11.2, 3, 5, rules, config, nil); trigger != TriggerNoTrigger {
		t.Fatalf("ATR 5%% 的票在 -6.67%% 回撤时不应触发移动止损，实际 %s", trigger)
	}

	// 低波动（1%）：宽度 3%，同样回撤必然触发。
	trigger, fraction, verdict := evaluatePosition(position, 11.2, 3, 1, rules, config, nil)
	if trigger != TriggerTrailStop {
		t.Fatalf("ATR 1%% 的票在 -6.67%% 回撤时应触发，实际 %s", trigger)
	}
	if fraction != 100 {
		t.Fatalf("移动止损应清仓，实际卖出 %.0f%%", fraction)
	}
	if !strings.Contains(verdict.Message, "门槛") || !strings.Contains(verdict.Message, "宽度") {
		t.Fatalf("裁决消息应写明实际门槛与宽度，否则复盘看不出为什么这次卖了：%s", verdict.Message)
	}
}

// 启动门槛的意义：浮盈不够时，再大的回撤也不算"移动止损"。
// 这条防的是"刚买入的正常波动被当成从高点回撤"直接清仓。
func TestEvaluatePositionTrailNeedsArm(t *testing.T) {
	position := Position{
		Symbol: "600000.SH", Quantity: 1000, AverageCost: 10,
		StopLossPrice: 9, TakeProfitPrice: 20, PeakPrice: 10.5,
	}
	// 现价 10.1：浮盈 1%，峰值 10.5，回撤 3.8%。门槛 2%（ATR 1%）→ 已启动；
	// 门槛 4%（无 ATR）→ 未启动，不该触发。
	if trigger, _, _ := evaluatePosition(position, 10.1, 1, 0, DefaultRiskRules(), DefaultGuardConfig(), nil); trigger == TriggerTrailStop {
		t.Fatalf("浮盈 1%% 未达 4%% 门槛时不应触发移动止损")
	}
}
