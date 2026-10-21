package policy

import "testing"

func TestLimitsNormalizeClampsOutOfRange(t *testing.T) {
	limits := Limits{MaxOrders: 99, MaxSingleWeightPercent: -1, MaxTotalExposurePercent: 0, BoardLot: 0}.Normalize()
	if limits.MaxOrders != 8 || limits.MaxSingleWeightPercent != 20 || limits.MaxTotalExposurePercent != 95 || limits.BoardLot != 100 {
		t.Fatalf("越界阈值未回落到默认值：%+v", limits)
	}
	if limits.MaxSectorWeightPercent != 45 || limits.MaxSectorNames != 3 {
		t.Fatalf("行业上限未取默认值：%+v", limits)
	}
}

func TestBookBlocksSingleNameAccumulation(t *testing.T) {
	book := NewBook(100000, Default())
	book.Seed("600000.SH", "金融", 12)
	if verdict, ok := book.TryBuy("600000.SH", "金融", 10); ok {
		t.Fatal("已持仓 12% 再加 10% 应突破单票上限 20%")
	} else if verdict.Code != CodeRiskSingleName || verdict.Metric != 22 {
		t.Fatalf("裁决异常：%+v", verdict)
	}
	// 被拒绝的买入不能污染账本。
	if book.SymbolWeight("600000.SH") != 12 {
		t.Fatalf("被拒后暴露应保持 12，实际 %.2f", book.SymbolWeight("600000.SH"))
	}
}

func TestBookBlocksSectorConcentration(t *testing.T) {
	book := NewBook(100000, Default())
	book.Seed("600001.SH", "电子", 20)
	book.Seed("600002.SH", "电子", 20)
	verdict, ok := book.TryBuy("600003.SH", "电子", 10)
	if ok {
		t.Fatal("同行业 40% 再加 10% 应突破行业上限 45%")
	}
	if verdict.Code != CodeRiskSectorWeight || verdict.Metric != 50 {
		t.Fatalf("裁决异常：%+v", verdict)
	}
	book.Seed("600004.SH", "医药", 20)
	if _, ok := book.TryBuy("600005.SH", "医药", 10); !ok {
		t.Fatal("不同行业不应受电子行业上限影响")
	}
}

func TestBookBlocksSectorNameCount(t *testing.T) {
	book := NewBook(100000, Default())
	for _, symbol := range []string{"600001.SH", "600002.SH", "600003.SH"} {
		if _, ok := book.TryBuy(symbol, "电子", 5); !ok {
			t.Fatalf("%s 应被接受", symbol)
		}
	}
	verdict, ok := book.TryBuy("600004.SH", "电子", 5)
	if ok {
		t.Fatal("同行业第 4 只应被只数上限拦下")
	}
	if verdict.Code != CodeRiskSectorNameCount || verdict.Limit != 3 {
		t.Fatalf("裁决异常：%+v", verdict)
	}
	// 加仓同一只不算新增只数。
	if _, ok := book.TryBuy("600001.SH", "电子", 5); !ok {
		t.Fatal("对已持有标的加仓不应受只数上限限制")
	}
}

func TestBookIgnoresUnknownSector(t *testing.T) {
	book := NewBook(100000, Default())
	for _, symbol := range []string{"600001.SH", "600002.SH", "600003.SH", "600004.SH"} {
		if _, ok := book.TryBuy(symbol, "", 5); !ok {
			t.Fatalf("无行业信息时 %s 不应被行业守卫拦下", symbol)
		}
	}
	if book.Exposure() != 20 {
		t.Fatalf("总暴露应为 20%%，实际 %.2f", book.Exposure())
	}
}

func TestCheckStopLoss(t *testing.T) {
	limits := Default()
	cases := []struct {
		name   string
		entry  float64
		stop   float64
		code   string
		reject bool
	}{
		{"缺失", 10, 0, CodeRiskStopLossMissing, true},
		{"高于买入价", 10, 10.5, CodeRiskStopLossInvalid, true},
		{"距离过宽", 10, 8.5, CodeRiskStopLossWide, true},
		{"正常", 10, 9.5, "", false},
	}
	for _, item := range cases {
		verdict, ok := limits.CheckStopLoss("600000.SH", item.entry, item.stop)
		if item.reject == ok {
			t.Fatalf("%s：期望拒绝=%t，实际 ok=%t（%+v）", item.name, item.reject, ok, verdict)
		}
		if item.reject && verdict.Code != item.code {
			t.Fatalf("%s：期望代码 %s，实际 %s", item.name, item.code, verdict.Code)
		}
	}
}

func TestCheckTradeRiskAndCashFloor(t *testing.T) {
	limits := Default()
	if _, ok := limits.CheckTradeRisk("600000.SH", 2500, 100000); ok {
		t.Fatal("2.5% 亏损应超过 2% 单笔风险上限")
	}
	if _, ok := limits.CheckTradeRisk("600000.SH", 1500, 100000); !ok {
		t.Fatal("1.5% 亏损应通过")
	}
	verdict, ok := limits.CheckCashFloor(3)
	if ok || verdict.Code != CodeRiskCashFloor {
		t.Fatalf("现金 3%% 应触发底线提示：%+v ok=%t", verdict, ok)
	}
	if _, ok := limits.CheckCashFloor(30); !ok {
		t.Fatal("现金 30% 不应触发底线")
	}
}

// 只数上限：二十来只票、每只三四个点，既摊平了收益又要为每只付一次最低佣金。
// 这条只拦"开新标的"，不能拦"已持有标的加仓"——前者是稀释，后者是集中。
func TestBookBlocksNewNameAtPositionLimit(t *testing.T) {
	limits := Default()
	limits.MaxPositions = 3
	book := NewBook(100000, limits)
	book.Seed("600001.SH", "电子", 10)
	book.Seed("600002.SH", "医药", 10)
	book.Seed("600003.SH", "金融", 10)

	verdict, ok := book.TryBuy("600004.SH", "汽车", 5)
	if ok {
		t.Fatal("已满 3 只时开第 4 只新标的应被拦")
	}
	if verdict.Code != CodeRiskPositionCount || verdict.Metric != 3 || verdict.Limit != 3 {
		t.Fatalf("裁决异常：%+v", verdict)
	}
	// 已持有的标的加仓不受只数上限约束。
	if _, ok := book.TryBuy("600001.SH", "电子", 5); !ok {
		t.Fatal("已持有标的加仓不该被只数上限拦住")
	}
	// 被拒的新标的不能占住名字：账本只数应仍为 3。
	if book.Exposure() == 0 {
		t.Fatal("组合暴露不应归零")
	}
}

func TestLimitsNormalizeKeepsPositionLimit(t *testing.T) {
	if got := (Limits{MaxPositions: 0}).Normalize().MaxPositions; got != 20 {
		t.Fatalf("只数上限缺失时应回落到 20，实际 %d", got)
	}
	if got := (Limits{MaxPositions: 500}).Normalize().MaxPositions; got != 20 {
		t.Fatalf("只数上限越界时应回落到 20，实际 %d", got)
	}
	if got := (Limits{MaxPositions: 8}).Normalize().MaxPositions; got != 8 {
		t.Fatalf("合法只数上限应保留，实际 %d", got)
	}
}
