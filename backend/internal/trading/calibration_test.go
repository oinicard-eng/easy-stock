package trading

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func memEntry(outcome string, confidence, pnl float64) Memory {
	return Memory{Outcome: outcome, Confidence: confidence, PnLPercent: pnl}
}

// 校准的第一条底线：flat 与未填置信度都不能进胜率，
// 否则"没赚没亏"会被当成失败，系统会误判自己过度自信。
func TestBuildCalibrationExcludesFlatAndMissing(t *testing.T) {
	memories := []Memory{
		memEntry("win", 0.9, 5),
		memEntry("loss", 0.9, -3),
		memEntry("flat", 0.8, 0),
		memEntry("win", 0, 4), // 保护性卖出写的 0，视为未填
	}
	report := buildCalibration("paper-main", memories, time.Now())

	if report.Samples != 2 {
		t.Fatalf("参与统计的样本应为 2（1 胜 1 负），实际 %d", report.Samples)
	}
	if report.Flats != 1 {
		t.Fatalf("打平样本应单独计数，实际 %d", report.Flats)
	}
	if report.Missing != 1 {
		t.Fatalf("未填置信度应单独计数，实际 %d", report.Missing)
	}
	// Brier = ((0.9-1)^2 + (0.9-0)^2)/2 = (0.01+0.81)/2 = 0.41
	if report.Brier != 0.41 {
		t.Fatalf("Brier 应为 0.41，实际 %.4f", report.Brier)
	}
}

func TestBuildCalibrationBucketsByConfidence(t *testing.T) {
	memories := []Memory{
		memEntry("win", 0.95, 6),
		memEntry("win", 0.92, 4),
		memEntry("loss", 0.78, -2),
		memEntry("loss", 0.62, -1),
		memEntry("win", 0.42, 1),
	}
	report := buildCalibration("paper-main", memories, time.Now())

	byLabel := map[string]ConfidenceBucket{}
	for _, bucket := range report.Buckets {
		byLabel[bucket.Label] = bucket
	}
	high := byLabel["≥0.9"]
	if high.Samples != 2 || high.Wins != 2 {
		t.Fatalf("≥0.9 桶应有 2 笔全胜，实际 %+v", high)
	}
	mid := byLabel["0.7–0.9"]
	if mid.Samples != 1 || mid.Losses != 1 {
		t.Fatalf("0.7–0.9 桶应有 1 笔亏损，实际 %+v", mid)
	}
	if low := byLabel["0.3–0.5"]; low.Samples != 1 {
		t.Fatalf("0.42 应落在 0.3–0.5 桶，实际 %+v", low)
	}
	// 高置信桶自评 0.935、实际 1.0，Gap 为正（低估），不能是负数。
	if high.Gap <= 0 {
		t.Fatalf("≥0.9 桶的 Gap 应接近 0 且非负，实际 %.4f", high.Gap)
	}
}

// 样本不足时必须拒绝下结论：5 笔样本里的胜率差异是噪音，
// 把噪音写进提示词会教模型错误的自省。
func TestCalibrationVerdictRefusesSmallSample(t *testing.T) {
	memories := []Memory{memEntry("loss", 0.9, -3), memEntry("loss", 0.9, -2)}
	report := buildCalibration("paper-main", memories, time.Now())
	if !strings.Contains(report.Verdict, "样本仅") {
		t.Fatalf("小样本结论必须自曝样本量：%s", report.Verdict)
	}
	if report.PromptLine() != "" {
		t.Fatalf("样本不足时不应写入提示词，实际：%s", report.PromptLine())
	}
}

func TestCalibrationVerdictNamesOverconfidentBucket(t *testing.T) {
	memories := make([]Memory, 0, 32)
	// 0.9 区间 16 笔里只有 4 笔赚：真实胜率 25%，明显过度自信。
	for index := 0; index < 16; index++ {
		if index < 4 {
			memories = append(memories, memEntry("win", 0.9, 3))
			continue
		}
		memories = append(memories, memEntry("loss", 0.9, -2))
	}
	for index := 0; index < 16; index++ {
		memories = append(memories, memEntry("win", 0.55, 1))
	}
	report := buildCalibration("paper-main", memories, time.Now())
	if report.Samples != 32 {
		t.Fatalf("样本应为 32，实际 %d", report.Samples)
	}
	if !strings.Contains(report.Verdict, "≥0.9") || !strings.Contains(report.Verdict, "打折") {
		t.Fatalf("应点名 ≥0.9 区间并给出打折结论：%s", report.Verdict)
	}
	if line := report.PromptLine(); !strings.Contains(line, "置信度校准（32 笔平仓）") {
		t.Fatalf("提示词行应带样本量：%s", line)
	}
}

// 端到端：记忆从库里读出来必须能被校准消费。
// 这条守的是 SQL 与结构体字段错位——JSON 字段改名后编译不会报错，只会静默少样本。
func TestCalibrationReadsMemoriesFromStore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for index, memory := range []Memory{
		{ID: newID("mem"), AccountID: DefaultAccountID, TradeID: newID("trd"), Symbol: "600519", Outcome: "win", Confidence: 0.8, PnLPercent: 4, TradingDay: "2026-10-20"},
		{ID: newID("mem"), AccountID: DefaultAccountID, TradeID: newID("trd"), Symbol: "000001", Outcome: "loss", Confidence: 0.8, PnLPercent: -3, TradingDay: "2026-10-19"},
	} {
		payload, err := json.Marshal(memory)
		if err != nil {
			t.Fatalf("序列化记忆失败：%v", err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO trading_memory
			(id,account_id,trade_id,symbol,sector,regime,trigger,outcome,trading_day,content_json,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			memory.ID, memory.AccountID, memory.TradeID, memory.Symbol, "", "range", "", memory.Outcome, memory.TradingDay, string(payload), nowText(now)); err != nil {
			t.Fatalf("写入记忆 %d 失败：%v", index, err)
		}
	}

	service := &Service{store: store}
	report, err := service.Calibration(ctx, DefaultAccountID, now)
	if err != nil {
		t.Fatalf("校准读取失败：%v", err)
	}
	if report.Samples != 2 {
		t.Fatalf("应读到 2 笔样本，实际 %d（检查 content_json 字段是否对得上）", report.Samples)
	}
	if report.Buckets[3].Samples == 0 {
		t.Fatalf("0.8 应落在 0.7–0.9 桶：%+v", report.Buckets)
	}
}
