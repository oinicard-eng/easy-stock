package trading

// 提示词层的验收：当存量仓位已经高于本轮上限时，提示词必须明确
// "这个差额不构成卖出理由"。少这句话，模型会把仓位指标当成止损信号。

import (
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/trading/cognition"
)

func overCeilingPrompt(t *testing.T, ceiling float64) string {
	t.Helper()
	// 权益 10 万、持仓 76%，上限 55%：正是实测里那个上午的处境。
	return BuildPlanPrompt(PlanPromptInput{
		Account: Account{ID: DefaultAccountID, Cash: 24000, Equity: 100000},
		Positions: []Position{{
			AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行",
			Quantity: 7600, Available: 0, AverageCost: 10, LastPrice: 10, MarketValue: 76000,
		}},
		Rules:           RulesForProfile("balanced"),
		Profile:         "balanced",
		Now:             time.Now(),
		Market:          ranging(),
		ExposureCeiling: ceiling,
	})
}

func TestPromptStatesOverCeilingDoesNotForceSell(t *testing.T) {
	prompt := overCeilingPrompt(t, 55)
	if !strings.Contains(prompt, "不构成卖出理由") {
		t.Fatal("存量超限时提示词必须写明差额不构成卖出理由，否则模型会为凑指标割肉")
	}
	if !strings.Contains(prompt, "76.00%") {
		t.Fatal("应给出当前实际仓位，让模型看得见差额有多大")
	}
	if !strings.Contains(prompt, "55.0%") {
		t.Fatalf("应写明本轮上限 55%%，实际提示词未包含该数字")
	}
}

func TestPromptOmitsOverCeilingNoteWhenWithinLimit(t *testing.T) {
	// 仓位 76% 但上限 85%：没有超限，不该出现这段提示，
	// 否则会让模型误以为自己在超限状态。
	// 先确认市场判断为上行且上限确实为 85。
	if got := bullish().MaxExposurePercent(90); got != 85 {
		t.Fatalf("夹具前提不成立：上行档上限应为 85%%，实际 %.2f%%", got)
	}
	prompt := BuildPlanPrompt(PlanPromptInput{
		Account: Account{ID: DefaultAccountID, Cash: 24000, Equity: 100000},
		Positions: []Position{{
			AccountID: DefaultAccountID, Symbol: "600000.SH", Name: "浦发银行",
			Quantity: 7600, Available: 0, AverageCost: 10, LastPrice: 10, MarketValue: 76000,
		}},
		Rules:           RulesForProfile("balanced"),
		Profile:         "balanced",
		Now:             time.Now(),
		Market:          cognition.MarketView{Evaluated: true, Regime: cognition.RegimeUptrend, RiskBudgetPercent: 85},
		ExposureCeiling: 85,
	})
	if strings.Contains(prompt, "不构成卖出理由") {
		t.Fatal("未超限时不该出现该说明")
	}
}
