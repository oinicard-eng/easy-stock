package trading

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"time"

	"easy-stock/backend/internal/trading/marketrule"
)

const (
	// 佣金按万分之 2.5、最低 5 元估算；印花税卖出万分之 5；过户费万分之 0.1。
	commissionRate = 0.00025
	minCommission  = 5.0
	stampTaxRate   = 0.0005
	transferRate   = 0.00001
)

func newID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + "-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return prefix + "-" + hex.EncodeToString(buffer)
}

func round2(value float64) float64 { return math.Round(value*100) / 100 }
func round4(value float64) float64 { return math.Round(value*10000) / 10000 }

// PriceLimitPercent 返回涨跌停幅度。规则本体在 marketrule 包，这里保留同包别名，
// 避免 trading 包内二十余处调用点全部改写。
func PriceLimitPercent(symbol, name string) float64 {
	return marketrule.PriceLimitPercent(symbol, name)
}

func limitUpPrice(previousClose, percent float64) float64 {
	return marketrule.LimitUpPrice(previousClose, percent)
}

func limitDownPrice(previousClose, percent float64) float64 {
	return marketrule.LimitDownPrice(previousClose, percent)
}

// MinBoardLot 返回最小申报数量：科创板 688 为 200 股，其余 100 股。
func MinBoardLot(symbol string) int {
	return marketrule.MinBoardLot(symbol)
}

// roundSharesDown 按整手向下取整；科创板允许 200 股以上以 1 股递增，这里统一按整手处理。
func roundSharesDown(quantity, lot int) int {
	if quantity <= 0 || lot <= 0 {
		return 0
	}
	return quantity / lot * lot
}

// TradeFee 计算单边费用。买入不收印花税。
func TradeFee(side Side, notional float64) float64 {
	if notional <= 0 {
		return 0
	}
	fee := math.Max(minCommission, notional*commissionRate) + notional*transferRate
	if side == SideSell {
		fee += notional * stampTaxRate
	}
	return round2(fee)
}

// isTradingSession 判断当前是否处于 A 股连续竞价时段，规则本体在 marketrule 包。
func isTradingSession(now time.Time) bool {
	return marketrule.IsTradingSession(now)
}

// LastSessionClose 返回最近一个已收盘交易日的 15:00，是委托的生命周期边界。
func LastSessionClose(now time.Time) time.Time {
	return marketrule.LastSessionClose(now)
}
