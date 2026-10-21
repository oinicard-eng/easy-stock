package httpapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"easy-stock/backend/internal/foundation"
)

// klineChain 按顺序尝试多个日线源，第一个成功即返回。
//
// 存在的理由：日线源现在是三层（QuantMind → 东财 → 新浪），而
// trading.SetKLine 只接受「主源 + 备源」两个位置。把前两层合成一个主源，
// 新浪仍占备源位，就不必改动交易模块的接口。
//
// 全部失败时返回**第一个**源的错误而不是最后一个：调用方问的是
// 「为什么今天拿不到日线」，主源的失败原因（局域网不通）比备源的
// 失败原因（公网限流）更能定位问题。
type klineChain struct {
	sources []KLineProvider
}

func newKLineChain(sources ...KLineProvider) KLineProvider {
	kept := make([]KLineProvider, 0, len(sources))
	for _, source := range sources {
		// 接口里装着 nil 指针时 != nil 判定会失效，这里用反射避免坏源进入链路。
		if source != nil && !isNilProvider(source) {
			kept = append(kept, source)
		}
	}
	if len(kept) == 1 {
		return kept[0]
	}
	return klineChain{sources: kept}
}

func (c klineChain) KLine(ctx context.Context, symbol string, period string, limit int) ([]foundation.KLine, error) {	if len(c.sources) == 0 {
		return nil, fmt.Errorf("没有可用的日线数据源")
	}
	var firstErr error
	for _, source := range c.sources {
		bars, err := source.KLine(ctx, symbol, period, limit)
		if err == nil && len(bars) > 0 {
			return bars, nil
		}
		if err == nil {
			err = fmt.Errorf("数据源返回空日线")
		}
		if firstErr == nil {
			firstErr = err
		}
		// 上下文取消就没必要继续试后面的源，重试只会拖长超时。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return nil, firstErr
}

// isNilProvider 识别「接口非 nil、底层指针为 nil」的坏源。
// 这种源一旦进入链路，会在调用时 panic，而 panic 发生在交易决策的中途，
// 代价远大于这里多做一次判断。
func isNilProvider(provider KLineProvider) bool {
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}
