// Package marketrule 收录 A 股交易规则中与账户、资金无关的纯函数：
// 涨跌停幅度、涨跌停价格、最小申报单位、交易时段判断。
//
// 这些规则被四处同时使用——市场认知（统计涨跌停家数）、候选池（剔除追不进的票）、
// 风控（拒绝越界限价）、撮合（判断能否成交）。任何一处单独实现都会随时间漂移，
// 最终出现"风控认为没涨停、撮合认为已涨停"这类无法复现的差异，因此统一收在这里。
package marketrule

import (
	"math"
	"strings"
	"time"

	"easy-stock/backend/internal/foundation"
)

// PriceLimitPercent 返回涨跌停幅度（百分比）。用于拒绝越界价格与识别涨跌停，
// 不做行情推断：调用方必须自己提供前收盘价。
func PriceLimitPercent(symbol, name string) float64 {
	code := strings.ToUpper(strings.TrimSpace(symbol))
	code = strings.TrimPrefix(strings.TrimPrefix(code, "SH"), "SZ")
	code = strings.TrimPrefix(code, "BJ")
	if index := strings.Index(code, "."); index > 0 {
		code = code[:index]
	}
	if strings.Contains(strings.ToUpper(name), "ST") {
		return 5
	}
	switch {
	case strings.HasPrefix(code, "30"), strings.HasPrefix(code, "688"), strings.HasPrefix(code, "689"):
		return 20
	case strings.HasPrefix(code, "4"), strings.HasPrefix(code, "8"), strings.HasPrefix(code, "92"):
		return 30
	default:
		return 10
	}
}

// LimitUpPrice 按前收盘与幅度算涨停价，四舍五入到分。前收盘缺失时返回 0。
func LimitUpPrice(previousClose, percent float64) float64 {
	if previousClose <= 0 {
		return 0
	}
	return math.Round(previousClose*(1+percent/100)*100) / 100
}

// LimitDownPrice 按前收盘与幅度算跌停价，四舍五入到分。前收盘缺失时返回 0。
func LimitDownPrice(previousClose, percent float64) float64 {
	if previousClose <= 0 {
		return 0
	}
	return math.Round(previousClose*(1-percent/100)*100) / 100
}

// MinBoardLot 返回最小申报数量：科创板 688 为 200 股，其余 100 股。
func MinBoardLot(symbol string) int {
	if strings.HasPrefix(strings.TrimSpace(symbol), "688") {
		return 200
	}
	return 100
}

// IsTradingDay 判断是否是交易所交易日（含法定节假日与调休）。
//
// 只判"有没有开市"，不判具体时段。日历来自 foundation 的年节假期表，
// 与行情新鲜度、复盘取数共用同一份事实——否则会出现"复盘认为今天是交易日、
// 调度认为不是"这类互相矛盾的判断。
func IsTradingDay(now time.Time) bool {
	return foundation.IsAStockTradingDay(now)
}

// IsTradingSession 判断是否处于 A 股连续竞价时段（不含集合竞价与节假日）。
// 只用于"是否值得发起一轮循环"的粗略判断，不是行情可用性的充分条件：
// 停牌、临时熔断、数据源故障时它仍可能返回 true，调用方还需结合行情时间戳。
func IsTradingSession(now time.Time) bool {
	if !IsTradingDay(now) {
		return false
	}
	cst := now.In(time.FixedZone("CST", 8*3600))
	minutes := cst.Hour()*60 + cst.Minute()
	morning := minutes >= 9*60+30 && minutes <= 11*60+30
	afternoon := minutes >= 13*60 && minutes <= 15*60
	return morning || afternoon
}

// LastSessionClose 返回最近一个"已经收盘"的交易日的 15:00（CST）。
//
// 它是委托的生命周期边界：A 股当日委托收盘即失效，任何早于这个时点仍未成交的
// 委托都不该继续占用冻结资金。用最近收盘时点而不是"今天 0 点"，是因为盘中
// 判断时今天还没结束，今天下的单不能作废；用交易日而不是自然日，是为了让
// 周一早上能一次清掉周五遗留的挂单，而不是等到"周一 0 点"。
func LastSessionClose(now time.Time) time.Time {
	cst := now.In(time.FixedZone("CST", 8*3600))
	day := cst
	// 今天已收盘：边界就是今天 15:00；否则回退到最近一个交易日。
	if !IsTradingDay(day) || cst.Hour() < 15 {
		day = day.AddDate(0, 0, -1)
	}
	for !IsTradingDay(day) {
		day = day.AddDate(0, 0, -1)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 15, 0, 0, 0, cst.Location())
}
