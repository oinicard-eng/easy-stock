package trading

// 仓位上限的当日迟滞。
//
// 要解决的问题很具体：总仓位上限由市场档位决定，在上行 85% 与震荡 55%
// 之间跳。早盘按 85% 建到七成仓，一小时后宽度指标翻成震荡，上限立刻变成
// 55%——提示词里于是写着"你超了 21 个百分点"。规则本身并不强制卖出
// （超限只把新增买单缩到 0，见 risk.go 的 scaleBuysToExposure），
// 但模型看到这个数字会主动减仓；而 A 股 T+1 之下，能减的都是早盘或昨天
// 建的仓，每一次减仓都是把浮亏确认成实亏，还要再付一次价差与手续费。
//
// 实测依据：某个上午 9 笔平仓全部亏损、全部 model_exit，其中至少 5 笔
// 发生在"档位从上行翻成震荡"之后的半小时内，而当时市场并没有崩，
// 只是宽度指标从 60% 上沿滑到了中位。
//
// 规则：同一交易日内，上限**只升不降**；次日按市场重新计算。
//
// 两个例外必须写清楚，否则这条规则会变成"为了不打脸而硬扛风险"：
//  1. 恐慌档（RegimePanic）立即生效，并把当日记忆压到恐慌值。跌停成片时
//     不能因为"今天早些时候说过 85%"就继续满仓。
//  2. 读不到记忆时按市场原始值收紧，不放宽。一次读库失败不该成为
//     放宽风控的理由。

import (
	"context"
	"strconv"
	"time"

	"easy-stock/backend/internal/trading/cognition"
)

const exposureCeilingKeyPrefix = "exposure_ceiling:"

func exposureCeilingKey(accountID, day string) string {
	return exposureCeilingKeyPrefix + accountID + ":" + day
}

// exposureCeiling 返回本轮实际生效的总仓位上限。
//
// 存放位置是 trading_state KV 而不是新建表：这个值丢了只是"当天少了迟滞"，
// 下一轮会重新算出市场当前上限——属于可自愈状态，不值得为它做迁移。
func (s *Service) exposureCeiling(ctx context.Context, accountID string, market cognition.MarketView, ruleLimit float64, now time.Time) float64 {
	// 市场未评估（行情取数失败）：退回风控上限，并且**不写当日记忆**。
	// 写记忆会让一次取数失败把上限冻结在 90%，压过之后正常的市场判断。
	if !market.Evaluated {
		return ruleLimit
	}
	raw := market.MaxExposurePercent(ruleLimit)
	if raw <= 0 {
		// 评估了但样本不足（unknown）：预算为 0，本就不该开仓。
		// 这同样不写记忆，否则"样本不足"会被迟滞固化成全天禁买。
		return 0
	}
	key := exposureCeilingKey(accountID, TradingDay(now))
	// 恐慌档不参与迟滞，并且覆盖当日记忆：解除恐慌后回到当日旧上限
	// 是没有意义的——那个上限属于崩盘前的市场。
	if market.Regime == cognition.RegimePanic {
		if err := s.store.SetState(ctx, key, strconv.FormatFloat(raw, 'f', 2, 64)); err != nil {
			s.logf("event=trading_exposure_ceiling_write_failed err=%v", err)
		}
		return raw
	}

	effective := raw
	stored, ok, err := s.store.GetState(ctx, key)
	if err != nil {
		// 读失败按市场原始值收紧，不因为一次读库失败放宽风控。
		s.logf("event=trading_exposure_ceiling_read_failed err=%v", err)
	} else if ok {
		if previous, convErr := strconv.ParseFloat(stored, 64); convErr == nil && previous > effective {
			effective = previous
		}
	}
	if err := s.store.SetState(ctx, key, strconv.FormatFloat(effective, 'f', 2, 64)); err != nil {
		s.logf("event=trading_exposure_ceiling_write_failed err=%v", err)
	}
	return effective
}
