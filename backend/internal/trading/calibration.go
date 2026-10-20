package trading

// 置信度校准：回答"模型说 0.9 的时候，是不是真的更准"。
//
// 这是一个纯统计模块，不参与任何交易决策。它存在的理由很直接：
// confidence 已经在参与仓位定价（confidenceFactor 会打折），
// 如果这个字段本身没有校准过，那么"按置信度定价"等于把一个未经验证的
// 自评数字直接放大成资金暴露。没有校准曲线，置信度就是装饰。
//
// 两条口径上的克制：
//  1. flat（打平）不计入胜率，也不算进 Brier。把"没赚没亏"当失败会系统性
//     压低胜率，进而让系统误判自己过度自信。
//  2. confidence=0 视为"未填"而不是"零信心"。保护性卖出写 0 是刻意的
//     （见 guard.go），那类样本根本不该进入校准。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// calibrationMinSamples 是给出结论所需的最小样本。
// 30 是经验下限：更少的样本里，一个 0.2 的胜率差完全可能只是运气。
const calibrationMinSamples = 30

// calibrationMemoryLimit 是参与统计的平仓记忆上限。
// 只取最近 500 笔：三年前的市场结构与现在不同，把老样本混进来
// 会让"当前模型的置信度准不准"变成"历史平均准不准"。
const calibrationMemoryLimit = 500

// ConfidenceBucket 是一个置信度分桶的统计结果。
type ConfidenceBucket struct {
	Label string `json:"label"`
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
	// Samples 只统计有置信度且结果为 win/loss 的样本。
	Samples int `json:"samples"`
	Wins    int `json:"wins"`
	Losses  int `json:"losses"`
	// Flats 是这个区间里打平的笔数。单独列出而不是丢弃：
	// 一个区间里大量打平，本身就说明这类信号没有方向性。
	Flats         int     `json:"flats"`
	WinRate       float64 `json:"win_rate"`
	AvgConfidence float64 `json:"avg_confidence"`
	AvgPnLPercent float64 `json:"avg_pnl_percent"`
	// Gap = 真实胜率 − 平均自评置信度。负值表示过度自信。
	Gap float64 `json:"gap"`
}

// CalibrationReport 是校准报告。
type CalibrationReport struct {
	AccountID string `json:"account_id"`
	// Samples 是参与胜率统计的样本数（有置信度且非 flat）。
	Samples int `json:"samples"`
	// Flats 与 Missing 是两类被排除的样本，必须显式返回，
	// 否则"样本 8 笔"会让人以为是全部历史只有 8 笔。
	Flats   int `json:"flats"`
	Missing int `json:"missing"`
	// Brier 是均方误差，0 完美、0.25 等于"永远猜 50%"。
	Brier   float64 `json:"brier"`
	Buckets []ConfidenceBucket `json:"buckets"`
	// Verdict 是一句可直接进提示词的中文结论。
	Verdict     string    `json:"verdict"`
	GeneratedAt time.Time `json:"generated_at"`
}

// ListMemoriesForCalibration 按平仓时间倒序取回记忆。
// 用 trading_memory 而不是 trading_closed_trades：outcome 已经在这里算好了，
// 两处各算一遍是分歧的开始。
func (s *Store) ListMemoriesForCalibration(ctx context.Context, accountID string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > calibrationMemoryLimit {
		limit = calibrationMemoryLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT content_json FROM trading_memory WHERE account_id=? ORDER BY trading_day DESC, created_at DESC LIMIT ?`,
		accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	memories := make([]Memory, 0, 64)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var memory Memory
		if err := json.Unmarshal([]byte(payload), &memory); err != nil {
			continue
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

// calibrationBuckets 是分桶边界。0.3 以下单独一桶：
// 低置信度建议本来就该被拦，把它并入 0.3–0.5 会让低区间看起来没那么差。
var calibrationBuckets = []struct {
	label string
	lower float64
	upper float64
}{
	{"<0.3", 0, 0.3},
	{"0.3–0.5", 0.3, 0.5},
	{"0.5–0.7", 0.5, 0.7},
	{"0.7–0.9", 0.7, 0.9},
	{"≥0.9", 0.9, 1.0001},
}

// buildCalibration 把记忆汇总成分桶统计。纯函数，便于测试。
func buildCalibration(accountID string, memories []Memory, now time.Time) CalibrationReport {
	report := CalibrationReport{AccountID: accountID, GeneratedAt: now}
	type acc struct {
		samples, wins, losses, flats int
		confSum, pnlSum             float64
	}
	accumulators := make([]acc, len(calibrationBuckets))
	brierSum := 0.0
	brierCount := 0

	for _, memory := range memories {
		confidence := memory.Confidence
		if confidence <= 0 {
			report.Missing++
			continue
		}
		if confidence > 1 {
			confidence = 1
		}
		index := bucketIndex(confidence)
		outcome := strings.ToLower(strings.TrimSpace(memory.Outcome))
		if outcome == "flat" || outcome == "" {
			report.Flats++
			accumulators[index].flats++
			continue
		}
		accumulators[index].samples++
		accumulators[index].confSum += confidence
		accumulators[index].pnlSum += memory.PnLPercent
		target := 0.0
		if outcome == "win" {
			accumulators[index].wins++
			target = 1
		} else {
			accumulators[index].losses++
		}
		brierSum += (confidence - target) * (confidence - target)
		brierCount++
		report.Samples++
	}

	if brierCount > 0 {
		report.Brier = round4(brierSum / float64(brierCount))
	}
	for index, bucket := range calibrationBuckets {
		item := ConfidenceBucket{Label: bucket.label, Lower: bucket.lower, Upper: bucket.upper}
		current := accumulators[index]
		item.Samples, item.Wins, item.Losses, item.Flats = current.samples, current.wins, current.losses, current.flats
		if current.samples > 0 {
			item.WinRate = round4(float64(current.wins) / float64(current.samples))
			item.AvgConfidence = round4(current.confSum / float64(current.samples))
			item.AvgPnLPercent = round2(current.pnlSum / float64(current.samples))
			item.Gap = round4(item.WinRate - item.AvgConfidence)
		}
		report.Buckets = append(report.Buckets, item)
	}
	report.Verdict = calibrationVerdict(report)
	return report
}

func bucketIndex(confidence float64) int {
	for index, bucket := range calibrationBuckets {
		if confidence >= bucket.lower && confidence < bucket.upper {
			return index
		}
	}
	return len(calibrationBuckets) - 1
}

// calibrationVerdict 生成一句结论。它只描述数据，不给交易建议——
// 建议由提示词里的经验段统一表述，两处各说一套会互相矛盾。
func calibrationVerdict(report CalibrationReport) string {
	if report.Samples == 0 {
		if report.Flats+report.Missing > 0 {
			return fmt.Sprintf("还没有可用于校准的样本（%d 笔打平、%d 笔未填置信度）。",
				report.Flats, report.Missing)
		}
		return "还没有可用于校准的平仓样本。"
	}
	if report.Samples < calibrationMinSamples {
		return fmt.Sprintf("样本仅 %d 笔（建议至少 %d 笔），下面的胜率只能当参考。", report.Samples, calibrationMinSamples)
	}

	// 找样本足够且有明显偏差的桶。8 笔以下不点名：
	// 五笔里错三笔是 40% 还是 60%，换个顺序就变了。
	worst := ConfidenceBucket{}
	found := false
	for _, bucket := range report.Buckets {
		if bucket.Samples < 8 {
			continue
		}
		if !found || bucket.Gap < worst.Gap {
			worst, found = bucket, true
		}
	}
	if found && worst.Gap <= -0.15 {
		return fmt.Sprintf("你在 %s 区间自评 %.0f%%、实际胜率 %.0f%%（%d 笔），偏差 %.0f 个百分点——这个区间的高置信度需要打折。",
			worst.Label, worst.AvgConfidence*100, worst.WinRate*100, worst.Samples, -worst.Gap*100)
	}
	if report.Brier >= 0.25 {
		return fmt.Sprintf("Brier %.3f 接近「永远猜 50%%」（0.25），置信度目前几乎没有区分度。", report.Brier)
	}
	if found {
		return fmt.Sprintf("置信度大体可信：%s 区间自评 %.0f%%、实际 %.0f%%（%d 笔），Brier %.3f。",
			worst.Label, worst.AvgConfidence*100, worst.WinRate*100, worst.Samples, report.Brier)
	}
	return fmt.Sprintf("样本 %d 笔，Brier %.3f，但各区间样本都不足 8 笔，无法定位偏差。", report.Samples, report.Brier)
}

// Calibration 返回本账户的置信度校准报告。
func (s *Service) Calibration(ctx context.Context, accountID string, now time.Time) (CalibrationReport, error) {
	memories, err := s.store.ListMemoriesForCalibration(ctx, s.accountID(accountID), calibrationMemoryLimit)
	if err != nil {
		return CalibrationReport{}, fmt.Errorf("读取平仓记忆失败：%w", err)
	}
	return buildCalibration(s.accountID(accountID), memories, now), nil
}

// PromptLine 把报告压成一行给提示词用。样本不足时返回空串：
// 一句"你的置信度不准"如果只有 5 笔样本支撑，会教模型得出错误的自省。
func (r CalibrationReport) PromptLine() string {
	if r.Samples < calibrationMinSamples {
		return ""
	}
	return fmt.Sprintf("置信度校准（%d 笔平仓）：%s", r.Samples, r.Verdict)
}

// calibrationPromptLine 是提示词里用的版本。读库失败返回空串而不是中断生成计划：
// 少一段校准结论只会让模型少一条自省，不该因此拿不到今天的计划。
func (s *Service) calibrationPromptLine(ctx context.Context, accountID string, now time.Time) string {
	report, err := s.Calibration(ctx, accountID, now)
	if err != nil {
		s.logf("event=trading_calibration_failed account=%s err=%v", accountID, err)
		return ""
	}
	return report.PromptLine()
}
