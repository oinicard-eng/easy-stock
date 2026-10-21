package trading

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

func changeTestSnapshot() Snapshot {
	return Snapshot{
		Equity: 100000,
		Cash:   100000,
		Quotes: []foundation.Quote{
			testQuote("600000.SH", "浦发银行", 10, 9.9),
			testQuote("000001.SZ", "平安银行", 12, 11.8),
		},
		Positions: []Position{
			{Symbol: "600000.SH", Quantity: 1000, Available: 1000, AverageCost: 9.5, LastPrice: 10, MarketValue: 10000},
		},
		OpenBuyValue: map[string]float64{"600000.SH": 5000},
		Universe:     []string{"600000.SH", "000001.SZ"},
	}
}

// 指纹最关键的性质是"同一份输入必须得到同一个值"。
// 行情源的返回顺序经常变，不排序就会让检测永远不生效。
func TestSnapshotFingerprintIgnoresOrder(t *testing.T) {
	first := changeTestSnapshot()
	second := changeTestSnapshot()
	second.Quotes[0], second.Quotes[1] = second.Quotes[1], second.Quotes[0]

	pending := []Order{{ID: "ord-1", Symbol: "600000.SH", Side: SideBuy, Quantity: 500, LimitPrice: 10}}
	blocks := []BlockedIntent{
		{Symbol: "000001.SZ", Side: SideBuy, Code: "risk.single_name", Repeat: 1},
		{Symbol: "600000.SH", Side: SideBuy, Code: "risk.sector_weight", Repeat: 2},
	}
	reordered := []BlockedIntent{blocks[1], blocks[0]}

	if got, want := SnapshotFingerprint(first, pending, blocks), SnapshotFingerprint(second, pending, reordered); got != want {
		t.Fatalf("同一输入的指纹应一致：%s != %s", got, want)
	}
}

func TestSnapshotFingerprintChangesOnMaterialInputs(t *testing.T) {
	base := SnapshotFingerprint(changeTestSnapshot(), nil, nil)

	priceMoved := changeTestSnapshot()
	priceMoved.Quotes[0].Price = 10.05
	if SnapshotFingerprint(priceMoved, nil, nil) == base {
		t.Fatal("价格变动必须改变指纹")
	}

	positionMoved := changeTestSnapshot()
	positionMoved.Positions[0].Available = 0
	if SnapshotFingerprint(positionMoved, nil, nil) == base {
		t.Fatal("可卖股数变动必须改变指纹")
	}

	cashMoved := changeTestSnapshot()
	cashMoved.ReservedCash = 100
	if SnapshotFingerprint(cashMoved, nil, nil) == base {
		t.Fatal("冻结资金变动必须改变指纹")
	}

	if SnapshotFingerprint(changeTestSnapshot(), []Order{{ID: "ord-9", Symbol: "600000.SH", Side: SideBuy, Quantity: 100, LimitPrice: 10}}, nil) == base {
		t.Fatal("在途挂单变动必须改变指纹")
	}
	// 同一原因被拦了第二次也要算变化：模型需要看到"同样的建议又被拦了一次"。
	blockedOnce := SnapshotFingerprint(changeTestSnapshot(), nil, []BlockedIntent{{Symbol: "600000.SH", Code: "risk.single_name", Repeat: 1}})
	blockedTwice := SnapshotFingerprint(changeTestSnapshot(), nil, []BlockedIntent{{Symbol: "600000.SH", Code: "risk.single_name", Repeat: 2}})
	if blockedOnce == blockedTwice {
		t.Fatal("重复被拒次数变化必须改变指纹")
	}
}

func TestShouldSkipOnlyWithinWindowAndWhenEnabled(t *testing.T) {
	service := newTestService(t, "", nil)
	now := time.Now()
	fingerprint := SnapshotFingerprint(changeTestSnapshot(), nil, nil)
	service.recordChange(fingerprint, "plan-a", now)

	if skip, note := service.shouldSkipNoChange(fingerprint, now.Add(5*time.Minute), true); !skip {
		t.Fatalf("5 分钟内同一输入应跳过：%s", note)
	}
	if skip, _ := service.shouldSkipNoChange(fingerprint, now.Add(noChangeWindow+time.Minute), true); skip {
		t.Fatal("超过 20 分钟必须重新问模型")
	}
	if skip, _ := service.shouldSkipNoChange("different", now.Add(time.Minute), true); skip {
		t.Fatal("指纹不同不应跳过")
	}
	// 人工触发的轮次不允许跳过：按钮必须真的问一次模型。
	if skip, _ := service.shouldSkipNoChange(fingerprint, now.Add(time.Minute), false); skip {
		t.Fatal("未启用检测时不应跳过")
	}
}

func TestPlanSkipsModelWhenNothingChanged(t *testing.T) {
	// 模型给不出任何可执行建议 → 不产生挂单与冻结，账户状态保持不变，
	// 于是第二轮输入与第一轮完全一致，正是变化检测要拦的场景。
	service := newTestService(t, `{"regime":"观望","summary":"没有机会","orders":[]}`, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	request := PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced", skipOnNoChange: true,
	}
	first, err := service.RunPlanSync(ctx, request, "cyc-1")
	if err != nil {
		t.Fatalf("首轮生成失败：%v", err)
	}
	// 模型没提建议是"看过全盘后的结论"，不是故障：状态是 empty，不是 failed。
	if first.Status != PlanStatusEmpty {
		t.Fatalf("首轮应为主动空仓（模型未提建议），实际 %s", first.Status)
	}
	second, err := service.RunPlanSync(ctx, request, "cyc-2")
	if !errors.Is(err, ErrNoMaterialChange) {
		t.Fatalf("第二轮应跳过模型调用，实际 err=%v status=%s", err, second.Status)
	}
	if second.Status != PlanStatusSkipped {
		t.Fatalf("跳过的计划状态应为 skipped，实际 %s", second.Status)
	}
	if second.DecisionID != "" {
		t.Fatalf("跳过的轮次不应产生裁决，实际 %s", second.DecisionID)
	}
	decisions, err := service.Decisions(ctx, DefaultAccountID, 10)
	if err != nil {
		t.Fatalf("读取裁决失败：%v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("应只有首轮产生 1 条裁决，实际 %d", len(decisions))
	}
}

func TestPlanDoesNotSkipWhenManual(t *testing.T) {
	service := newTestService(t, `{"regime":"观望","summary":"没有机会","orders":[]}`, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	request := PlanRequest{Watchlist: []string{"600000.SH"}, TraderProfile: "balanced"}
	if _, err := service.RunPlanSync(ctx, request, ""); err != nil {
		t.Fatalf("首轮生成失败：%v", err)
	}
	// 同样的输入、但没打开跳过开关：必须再问一次模型（这里会得到同一份建议）。
	second, err := service.RunPlanSync(ctx, request, "")
	if errors.Is(err, ErrNoMaterialChange) {
		t.Fatal("人工/未启用的轮次不应跳过")
	}
	if second.DecisionID == "" {
		t.Fatalf("第二轮应产生裁决：%+v", second)
	}
}

func TestChangeBaselineSurvivesRestart(t *testing.T) {
	service := newTestService(t, `{"regime":"观望","summary":"没有机会","orders":[]}`, map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	})
	ctx := context.Background()
	request := PlanRequest{
		Watchlist: []string{"600000.SH"}, TraderProfile: "balanced", skipOnNoChange: true,
	}
	if _, err := service.RunPlanSync(ctx, request, "cyc-1"); err != nil {
		t.Fatalf("首轮生成失败：%v", err)
	}

	// 用同一个 store 新建一个 Service，等价于进程重启：内存基线为空。
	restarted := NewService(service.store, fakeGateway{content: `{"regime":"观望","summary":"没有机会","orders":[]}`}, fakeQuotes{quotes: map[string]foundation.Quote{
		"600000.SH": testQuote("600000.SH", "浦发银行", 10, 9.9),
	}}, nil, log.New(io.Discard, "", 0))
	restarted.SetResearchEnabled(false)

	// 基线从 KV 恢复后，20 分钟窗口内的重复输入应照常跳过。
	if _, err := restarted.RunPlanSync(ctx, request, "cyc-2"); !errors.Is(err, ErrNoMaterialChange) {
		t.Fatalf("重启后应能从 KV 恢复基线并跳过，实际 err=%v", err)
	}
}
