package trading

// 仓位上限迟滞的验收标准：
//  1. 同一天内上限只升不降——早盘按上行打满，午后判成震荡也不许把上限调低。
//  2. 次日重新按市场算，迟滞不能跨日累积。
//  3. 恐慌档立即生效，不受迟滞保护。
//  4. 风控上限比市场预算更紧时，收紧的方向始终有效。

import (
	"context"
	"testing"
	"time"

	"easy-stock/backend/internal/trading/cognition"
)

func newCeilingService(t *testing.T) *Service {
	t.Helper()
	return &Service{store: newCooldownStore(t)}
}

// bullish 构造一个"上行"市场判断（预算 85%）。
func bullish() cognition.MarketView {
	return cognition.MarketView{Evaluated: true, Regime: cognition.RegimeUptrend, RiskBudgetPercent: 85}
}

// ranging 构造一个"震荡"市场判断（预算 55%）。
func ranging() cognition.MarketView {
	return cognition.MarketView{Evaluated: true, Regime: cognition.RegimeRange, RiskBudgetPercent: 55}
}

func TestExposureCeilingDoesNotDropWithinSameDay(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	morning := cooldownCST(9, 40, 0)
	afternoon := cooldownCST(13, 30, 0)

	if got := service.exposureCeiling(ctx, DefaultAccountID, bullish(), 90, morning); got != 85 {
		t.Fatalf("早盘上行档上限应为 85%%，实际 %.2f%%", got)
	}
	// 同一天稍后翻成震荡：不降，否则模型会为凑指标去卖刚建的仓。
	if got := service.exposureCeiling(ctx, DefaultAccountID, ranging(), 90, afternoon); got != 85 {
		t.Fatalf("同一交易日内上限不应下调，实际降到 %.2f%%——档位抖动会逼模型割肉", got)
	}
}

func TestExposureCeilingResetsNextDay(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	today := cooldownCST(9, 40, 0)
	tomorrow := cooldownCST(9, 40, 1)

	if got := service.exposureCeiling(ctx, DefaultAccountID, bullish(), 90, today); got != 85 {
		t.Fatalf("首日上行档上限应为 85%%，实际 %.2f%%", got)
	}
	if got := service.exposureCeiling(ctx, DefaultAccountID, ranging(), 90, tomorrow); got != 55 {
		t.Fatalf("次日应重新按市场计算为 55%%，实际 %.2f%%——迟滞不能跨日累积", got)
	}
}

func TestExposureCeilingPanicBypassesHysteresis(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	morning := cooldownCST(9, 40, 0)
	crash := cooldownCST(10, 30, 0)
	panicMarket := cognition.MarketView{Evaluated: true, Regime: cognition.RegimePanic, RiskBudgetPercent: 20}

	if got := service.exposureCeiling(ctx, DefaultAccountID, bullish(), 90, morning); got != 85 {
		t.Fatalf("早盘上行档上限应为 85%%，实际 %.2f%%", got)
	}
	// 跌停成片：立即压到 20%，不能因为"今天早些时候说过 85%"继续满仓。
	if got := service.exposureCeiling(ctx, DefaultAccountID, panicMarket, 90, crash); got != 20 {
		t.Fatalf("恐慌档应立即生效为 20%%，实际 %.2f%%", got)
	}
	// 恐慌过后同一天回到上行：允许回升（只升不降的方向不变），
	// 但不许再回到恐慌前的旧记忆去压仓位。
	if got := service.exposureCeiling(ctx, DefaultAccountID, bullish(), 90, crash.Add(time.Hour)); got != 85 {
		t.Fatalf("恐慌解除后上行档上限应回到 85%%，实际 %.2f%%", got)
	}
}

func TestExposureCeilingHonorsTighterRiskLimit(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	now := cooldownCST(10, 0, 0)

	// 风控上限 60 比市场预算 85 更紧：取 60。
	if got := service.exposureCeiling(ctx, DefaultAccountID, bullish(), 60, now); got != 60 {
		t.Fatalf("风控上限更紧时应取 60%%，实际 %.2f%%", got)
	}
	// 市场转震荡（55），仍然不降——迟滞对"取较小值"之后的最终值生效。
	if got := service.exposureCeiling(ctx, DefaultAccountID, ranging(), 60, now.Add(time.Hour)); got != 60 {
		t.Fatalf("同日上限不应下调，实际 %.2f%%", got)
	}
}

func TestExposureCeilingUnevaluatedFallsBackWithoutMemory(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	now := cooldownCST(10, 0, 0)

	// 行情取数失败：退回风控上限本身，不因为"没判断市场"就禁止开仓。
	if got := service.exposureCeiling(ctx, DefaultAccountID, cognition.MarketView{}, 90, now); got != 90 {
		t.Fatalf("未评估市场时应退回风控上限 90%%，实际 %.2f%%", got)
	}
	// 且不能写当日记忆：随后正常评估为震荡，就该是 55%，而不是被冻结在 90%。
	if got := service.exposureCeiling(ctx, DefaultAccountID, ranging(), 90, now.Add(time.Minute)); got != 55 {
		t.Fatalf("取数失败不该冻结当日上限，实际 %.2f%%", got)
	}
}

func TestExposureCeilingUnknownRegimeBlocksWithoutMemory(t *testing.T) {
	service := newCeilingService(t)
	ctx := context.Background()
	now := cooldownCST(10, 0, 0)
	unknown := cognition.MarketView{Evaluated: true, Regime: cognition.RegimeUnknown, RiskBudgetPercent: 0}

	// 评估了但样本不足：预算 0，本轮不该开仓。
	if got := service.exposureCeiling(ctx, DefaultAccountID, unknown, 90, now); got != 0 {
		t.Fatalf("样本不足时应为 0%%，实际 %.2f%%", got)
	}
	// 同样不写记忆：样本补足后应立即按真实市场算。
	if got := service.exposureCeiling(ctx, DefaultAccountID, ranging(), 90, now.Add(time.Minute)); got != 55 {
		t.Fatalf("样本不足不该固化为全天禁买，实际 %.2f%%", got)
	}
}
