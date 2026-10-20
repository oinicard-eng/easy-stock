package trading

import (
	"context"
	"strings"
	"testing"
	"time"

	"easy-stock/backend/internal/foundation"
)

func planDiffService(t *testing.T, universe []string, orders []Order, createdAt time.Time) (*Service, Snapshot) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	previous := Plan{
		ID: "plan-prev", AccountID: DefaultAccountID, Status: PlanStatusDraft,
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Snapshot: Snapshot{Universe: universe, Quotes: []foundation.Quote{
			testQuote("600519.SH", "贵州茅台", 1680, 1670),
			testQuote("300750.SZ", "宁德时代", 180, 179),
		}},
		Orders: orders,
	}
	if err := store.CreatePlan(ctx, previous); err != nil {
		t.Fatalf("写入上一轮计划失败：%v", err)
	}
	snapshot := Snapshot{
		Universe: []string{"600519.SH", "000001.SZ"},
		Quotes: []foundation.Quote{
			testQuote("600519.SH", "贵州茅台", 1690, 1680),
			testQuote("000001.SZ", "平安银行", 10, 9.9),
		},
	}
	return &Service{store: store}, snapshot
}

func TestPlanDiffListsCandidateChanges(t *testing.T) {
	now := time.Now().UTC()
	service, snapshot := planDiffService(t,
		[]string{"600519.SH", "300750.SZ"},
		[]Order{{Symbol: "300750.SZ", Side: SideBuy, Status: OrderStatusFilled}},
		now.Add(-10*time.Minute))

	lines := service.PlanDiffForPrompt(context.Background(), DefaultAccountID, "plan-current", snapshot, now)
	if len(lines) == 0 {
		t.Fatal("应有差异说明")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "300750.SZ") || !strings.Contains(joined, "宁德时代") {
		t.Fatalf("应指出消失的标的，实际：%s", joined)
	}
	if !strings.Contains(joined, "000001.SZ") || !strings.Contains(joined, "平安银行") {
		t.Fatalf("应指出新增的标的，实际：%s", joined)
	}
	// 上一轮建议买过、本轮已不在池中：必须明说买不了，
	// 否则模型会把它当成"可以继续推进"的标的。
	if !strings.Contains(joined, "无法再买") {
		t.Fatalf("应提示掉出候选池的票本轮无法买入，实际：%s", joined)
	}
}

func TestPlanDiffStableWhenUniverseUnchanged(t *testing.T) {
	now := time.Now().UTC()
	service, snapshot := planDiffService(t,
		[]string{"600519.SH", "000001.SZ"}, nil, now.Add(-5*time.Minute))

	lines := service.PlanDiffForPrompt(context.Background(), DefaultAccountID, "plan-current", snapshot, now)
	if len(lines) != 1 || !strings.Contains(lines[0], "完全一致") {
		t.Fatalf("候选未变时应只给一行「完全一致」，实际：%v", lines)
	}
}

func TestPlanDiffSkipsStaleOrMissingPlan(t *testing.T) {
	now := time.Now().UTC()
	// 超过窗口的上一轮计划不再比较：候选池早已换过几轮，比出来的差异是噪音。
	service, snapshot := planDiffService(t,
		[]string{"600519.SH"}, nil, now.Add(-48*time.Hour))
	if lines := service.PlanDiffForPrompt(context.Background(), DefaultAccountID, "plan-current", snapshot, now); lines != nil {
		t.Fatalf("过期计划不应参与比较，实际：%v", lines)
	}

	// 只有本轮计划自己时（首次运行）同样返回 nil，而不是拿自己和自己比。
	empty := &Service{store: newTestStore(t)}
	if lines := empty.PlanDiffForPrompt(context.Background(), DefaultAccountID, "plan-current", snapshot, now); lines != nil {
		t.Fatalf("没有上一轮计划时不应输出差异，实际：%v", lines)
	}
}

func TestPlanDiffIgnoresCurrentPlanItself(t *testing.T) {
	now := time.Now().UTC()
	service, snapshot := planDiffService(t, []string{"600519.SH"}, nil, now.Add(-5*time.Minute))
	// 本轮计划已经落库的情况下，不能把"上一轮"错认成自己：
	// 那会让差异永远是"完全一致"，等于这一整段失效。
	if err := service.store.CreatePlan(context.Background(), Plan{
		ID: "plan-current", AccountID: DefaultAccountID, Status: PlanStatusDraft,
		CreatedAt: now, UpdatedAt: now, Snapshot: snapshot,
	}); err != nil {
		t.Fatalf("写入本轮计划失败：%v", err)
	}
	lines := service.PlanDiffForPrompt(context.Background(), DefaultAccountID, "plan-current", snapshot, now)
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "000001.SZ") {
		t.Fatalf("应跳过本轮计划、与真正上一轮比较，实际：%v", lines)
	}
}
