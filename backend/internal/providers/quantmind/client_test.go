package quantmind

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tokenForTest 造一个带 exp 的 JWT 形状字符串，只为本包 jwtExpiry 服务。
func tokenForTest(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".sig"
}

// newTestServer 起一个最小 QuantMind 替身。
// loginHits / klineHits 用来断言「登录只做一次」「401 只重试一次」这类行为，
// 这类计数只能从服务端视角观察。
func newTestServer(t *testing.T, opts testServerOptions) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var loginHits, klineHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		if opts.loginFails {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"HTTP_401","message":"用户名或密码错误"}}`))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"refresh_token":"r"}`, opts.token)))
	})
	mux.HandleFunc("/api/v1/market/kline", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&klineHits, 1)
		if opts.rejectToken != "" && r.Header.Get("Authorization") != "Bearer "+opts.rejectToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"HTTP_401","message":"Missing authentication token"}}`))
			return
		}
		if opts.klineBody != "" {
			_, _ = w.Write([]byte(opts.klineBody))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[{"date":"2026-10-19","open":10,"high":11,"low":9.5,"close":10.5,"volume":1000,"amount":1050},{"date":"2026-10-20","open":10.6,"high":11.2,"low":10.4,"close":11,"volume":1200,"amount":1320}]}`))
	})
	mux.HandleFunc("/api/v1/stocks/all", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"market":"A","count":1,"items":[{"symbol":"600519.SH","code":"600519","name":"贵州茅台","exchange":"SH","close":1331.88,"pe":21.5,"pb":7.2,"marketCap":16730.0,"floatMarketCap":16730.0,"pctChange":1.2,"turnoverRate":0.27,"isSt":false,"listedDays":6000}]}`))
	})
	mux.HandleFunc("/api/v1/market-analysis/money-flow/stocks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"symbol":"688018.SH","name":"乐鑫科技","close_price":140.72,"pct_change":16.86,"net_inflow":633889172,"gross_inflow":1545992962,"gross_outflow":912103789,"main_ratio":8.7,"super_large":69376422,"large":144369805,"medium":244973688,"small":175169255,"trend_30d":[6.34,-0.14]}]`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &loginHits, &klineHits
}

type testServerOptions struct {
	token       string
	rejectToken string
	klineBody   string
	loginFails  bool
}

func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := NewClient("default", "admin", "secret", WithBaseURL(serverURL))
	if err != nil {
		t.Fatalf("NewClient 失败：%v", err)
	}
	return client
}

// 服务端两种 data 形态都必须认：days 大时是数组，days 小时是 {items}。
// 只认一种会在某个 days 取值上突然拿不到数据，而且不报错。
func TestKLineParsesBothDataShapes(t *testing.T) {
	cases := map[string]string{
		"数组":  `{"success":true,"data":[{"date":"2026-10-19","open":10,"high":11,"low":9.5,"close":10.5,"volume":1000,"amount":1050},{"date":"2026-10-20","open":10.6,"high":11.2,"low":10.4,"close":11,"volume":1200,"amount":1320}]}`,
		"对象":  `{"success":true,"data":{"items":[{"date":"2026-10-19","open":10,"high":11,"low":9.5,"close":10.5,"volume":1000,"amount":1050},{"date":"2026-10-20","open":10.6,"high":11.2,"low":10.4,"close":11,"volume":1200,"amount":1320}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server, _, _ := newTestServer(t, testServerOptions{token: "t", klineBody: body})
			bars, err := newTestClient(t, server.URL).KLine(context.Background(), "600519", "day", 60)
			if err != nil {
				t.Fatalf("KLine 失败：%v", err)
			}
			if len(bars) != 2 {
				t.Fatalf("期望 2 根，得到 %d", len(bars))
			}
			if bars[0].Symbol != "600519.SH" {
				t.Errorf("代码应规范成 600519.SH，得到 %q", bars[0].Symbol)
			}
			if bars[1].PreviousClose != 10.5 || bars[1].ChangePercent <= 0 {
				t.Errorf("第二根应带上日收盘与涨跌幅，得到 prev=%v pct=%v", bars[1].PreviousClose, bars[1].ChangePercent)
			}
			if bars[1].Meta.Source != "quantmind" {
				t.Errorf("Meta.Source 应为 quantmind，得到 %q", bars[1].Meta.Source)
			}
		})
	}
}

// 停牌行（close=0）必须丢掉：留着会把 MA/ATR 拉低，而且不报错。
func TestKLineDropsZeroCloseBars(t *testing.T) {
	body := `{"success":true,"data":[{"date":"2026-10-16","open":10,"high":11,"low":9.5,"close":10.5},{"date":"2026-10-19","open":0,"high":0,"low":0,"close":0},{"date":"2026-10-20","open":10.6,"high":11.2,"low":10.4,"close":11}]}`
	server, _, _ := newTestServer(t, testServerOptions{token: "t", klineBody: body})
	bars, err := newTestClient(t, server.URL).KLine(context.Background(), "600519.SH", "daily", 30)
	if err != nil {
		t.Fatalf("KLine 失败：%v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("停牌行应被剔除，期望 2 根，得到 %d", len(bars))
	}
	for _, bar := range bars {
		if bar.Close <= 0 {
			t.Errorf("不应保留下跌零收盘：%+v", bar)
		}
	}
}

// token 要复用。每轮决策 5 次工具调用，如果每次都重新登录，
// 一轮里就多打 4 次登录请求，还容易被服务端限流。
func TestTokenIsReusedAcrossCalls(t *testing.T) {
	token := tokenForTest(time.Now().Add(2 * time.Hour))
	server, loginHits, _ := newTestServer(t, testServerOptions{token: token})
	client := newTestClient(t, server.URL)
	for i := 0; i < 3; i++ {
		if _, err := client.KLine(context.Background(), "600519", "day", 30); err != nil {
			t.Fatalf("第 %d 次 KLine 失败：%v", i+1, err)
		}
	}
	if hits := atomic.LoadInt32(loginHits); hits != 1 {
		t.Errorf("期望只登录 1 次，实际 %d 次", hits)
	}
}

// token 过期（服务端 401）时必须自动重登并成功，不能把 401 抛给上层。
// 上层看到 401 只会转备源，等于白白丢掉 QuantMind 的 4000 根历史。
func TestUnauthorizedTriggersRelogin(t *testing.T) {
	stale := tokenForTest(time.Now().Add(time.Hour))
	fresh := tokenForTest(time.Now().Add(2 * time.Hour))
	var loginHits int32
	var phase int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		// 第一次发过期 token，第二次开始发有效的。
		issued := stale
		if atomic.LoadInt32(&loginHits) > 1 {
			issued = fresh
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q}`, issued)))
	})
	mux.HandleFunc("/api/v1/market/kline", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fresh {
			atomic.StoreInt32(&phase, 1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"HTTP_401","message":"expired"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[{"date":"2026-10-19","open":10,"high":11,"low":9.5,"close":10.5},{"date":"2026-10-20","open":10.6,"high":11,"low":10.4,"close":11}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	bars, err := newTestClient(t, server.URL).KLine(context.Background(), "600519", "day", 30)
	if err != nil {
		t.Fatalf("401 后应自动重登成功，实际失败：%v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("期望 2 根，得到 %d", len(bars))
	}
	if atomic.LoadInt32(&loginHits) != 2 {
		t.Errorf("期望登录 2 次（首次 + 401 后重登），实际 %d 次", loginHits)
	}
}

// 业务错误用 200 包封时必须被识别。否则上层会拿到空 data，
// 误判成"今天没有这只票的行情"，而真实原因是查询本身被拒绝。
func TestBusinessErrorInSuccessEnvelope(t *testing.T) {
	body := `{"success":false,"error":{"code":"VALIDATION_ERROR","message":"Request validation failed"}}`
	server, _, _ := newTestServer(t, testServerOptions{token: "t", klineBody: body})
	_, err := newTestClient(t, server.URL).KLine(context.Background(), "600519", "day", 30)
	if err == nil {
		t.Fatal("success=false 应报错，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "VALIDATION_ERROR") {
		t.Errorf("错误信息应包含服务端 code，得到：%v", err)
	}
}

// 凭据不全时构造必须失败。返回一个每次调用都报错的客户端，
// 会让装配处以为"已启用"，从而把主源位置占掉。
func TestNewClientRejectsIncompleteCredentials(t *testing.T) {
	if _, err := NewClient("", "admin", "secret"); err == nil {
		t.Error("tenant 为空时应返回错误")
	}
	if _, err := NewClient("default", "", "secret"); err == nil {
		t.Error("user 为空时应返回错误")
	}
	if _, err := NewClient("default", "admin", ""); err == nil {
		t.Error("password 为空时应返回错误")
	}
}

// 服务端的 days 是自然日、返回条数比请求多；东财/新浪的 limit 是根数。
// 客户端必须截到 limit 根，否则同一个"看一年"在三个源上窗口不同。
func TestKLineTrimsToRequestedBarCount(t *testing.T) {
	body := `{"success":true,"data":[
		{"date":"2026-10-14","open":9,"high":10,"low":8.5,"close":9.5},
		{"date":"2026-10-15","open":9.6,"high":10.2,"low":9.4,"close":10},
		{"date":"2026-10-16","open":10.1,"high":10.8,"low":10,"close":10.5},
		{"date":"2026-10-19","open":10.6,"high":11.2,"low":10.4,"close":11},
		{"date":"2026-10-20","open":11.1,"high":11.6,"low":10.9,"close":11.3}]}`
	server, _, _ := newTestServer(t, testServerOptions{token: "t", klineBody: body})
	bars, err := newTestClient(t, server.URL).KLine(context.Background(), "600519", "day", 2)
	if err != nil {
		t.Fatalf("KLine 失败：%v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("应截到 2 根，得到 %d", len(bars))
	}
	// 必须留最新两根，截错方向会把最旧的数据当成当前。
	if bars[len(bars)-1].Close != 11.3 {
		t.Errorf("末根应是最新一天，得到 close=%v", bars[len(bars)-1].Close)
	}
}

// 请求根数超过服务端能给的（如 4000 只给全历史 2620）时按全量返回，
// 不报错也不补造。
func TestKLineKeepsAllWhenFewerThanRequested(t *testing.T) {
	body := `{"success":true,"data":[
		{"date":"2026-10-19","open":10,"high":11,"low":9.5,"close":10.5},
		{"date":"2026-10-20","open":10.6,"high":11.2,"low":10.4,"close":11}]}`
	server, _, _ := newTestServer(t, testServerOptions{token: "t", klineBody: body})
	bars, err := newTestClient(t, server.URL).KLine(context.Background(), "600519", "day", 750)
	if err != nil {
		t.Fatalf("KLine 失败：%v", err)
	}
	if len(bars) != 2 {
		t.Errorf("不足请求根数时应返回全部 2 根，得到 %d", len(bars))
	}
}

// 资金流接口的 symbol 是 SH688018 前缀式，必须转成 Canonical，
// 否则调用方按代码匹配会全部落空。
func TestMoneyFlowNormalizesSymbol(t *testing.T) {
	server, _, _ := newTestServer(t, testServerOptions{token: "t"})
	flows, err := newTestClient(t, server.URL).MoneyFlow(context.Background(), 5)
	if err != nil {
		t.Fatalf("MoneyFlow 失败：%v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(flows))
	}
	if flows[0].Symbol != "688018.SH" {
		t.Errorf("symbol 应规范成 688018.SH，得到 %q", flows[0].Symbol)
	}
}

func TestSnapshotsAndMoneyFlow(t *testing.T) {
	server, _, _ := newTestServer(t, testServerOptions{token: "t"})
	client := newTestClient(t, server.URL)

	snapshots, err := client.Snapshots(context.Background())
	if err != nil {
		t.Fatalf("Snapshots 失败：%v", err)
	}
	if len(snapshots) != 1 || snapshots[0].PE != 21.5 || snapshots[0].FloatMarketCap == 0 {
		t.Fatalf("基本面字段未正确解析：%+v", snapshots[0])
	}

	flows, err := client.MoneyFlow(context.Background(), 5)
	if err != nil {
		t.Fatalf("MoneyFlow 失败：%v", err)
	}
	if len(flows) != 1 || flows[0].MainRatio != 8.7 || len(flows[0].Trend30D) != 2 {
		t.Fatalf("资金流字段未正确解析：%+v", flows[0])
	}
}

// 服务端不可达时必须返回错误而不是空结果，让 tools.go 能转到备源。
func TestUnreachableServerReturnsError(t *testing.T) {
	client, err := NewClient("default", "admin", "secret", WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewClient 失败：%v", err)
	}
	client.http = &http.Client{Timeout: 500 * time.Millisecond}
	if _, err := client.KLine(context.Background(), "600519", "day", 30); err == nil {
		t.Fatal("服务端不可达时应返回错误")
	}
}

func TestJWTExpiry(t *testing.T) {
	want := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	got := jwtExpiry(tokenForTest(want))
	if got.IsZero() {
		t.Fatal("应解析出 exp")
	}
	if diff := got.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("exp 偏差过大：want %v got %v", want, got)
	}
	if !jwtExpiry("not-a-jwt").IsZero() {
		t.Error("非法 token 应返回零值")
	}
}
