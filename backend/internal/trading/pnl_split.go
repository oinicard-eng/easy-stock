package trading

import (
	"context"
	"math"
)

// pnlSplit 生成账户盈亏的口径拆解，供前端做一次能被用户验算的展示。
//
// 为什么需要它：账户卡片上"总资产少了 630"和"浮动赚了 672"并排摆着，
// 看着就是矛盾。真实原因是两个数字口径不同——总资产是**已实现 + 浮动**的合成，
// 而浮动只描述还没卖的仓位。不把中间项列出来，用户只能怀疑账错了。
//
// 取数失败时返回 nil 而不是零值：零值会让残差凭空等于全部变动，
// 看起来像"账错得很离谱"，那比不显示更糟。
func (s *Service) pnlSplit(ctx context.Context, accountID string, account Account, positions []Position) *PnLSplit {
	realized, err := s.store.SumRealized(ctx, accountID)
	if err != nil {
		s.logf("event=trading_pnl_realized_failed account=%s err=%v", accountID, err)
		return nil
	}
	fees, err := s.store.SumFees(ctx, accountID)
	if err != nil {
		s.logf("event=trading_pnl_fees_failed account=%s err=%v", accountID, err)
		return nil
	}
	unrealized := 0.0
	for _, position := range positions {
		unrealized += position.Unrealized
	}
	unrealized = round2(unrealized)
	change := round2(account.Equity - account.InitialCash)
	// 残差 = 变动 - 已实现 - 浮动。口径统一时它约等于 0；
	// 不为 0 说明账面上还有没被这两项解释的钱，必须原样暴露。
	residual := round2(change - realized - unrealized)
	return &PnLSplit{
		InitialCash: account.InitialCash,
		Equity:      account.Equity,
		Change:      change,
		Realized:    realized,
		Unrealized:  unrealized,
		Fees:        fees,
		Residual:    residual,
		// 容差 1 元：逐笔四舍五入的累积残差可以放过，
		// 一整笔佣金（5 元）那种口径错误不能。
		Aligned: math.Abs(residual) < 1,
	}
}
