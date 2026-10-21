package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTradingOnlyTrimsRoutes 验证独立 AI交易员模式的路由契约：
// 交易与设置路由在，研究侧路由不在。独立部署的进程「能做什么」
// 必须由路由表直接声明，而不是靠没人去调它。
func TestTradingOnlyTrimsRoutes(t *testing.T) {
	server := NewServer(Config{TradingOnly: true})

	kept := []string{
		"/api/health",
		"/api/v1/trading/health",
		"/api/v1/trading/account",
		"/api/v1/trading/universe",
		"/api/v1/settings",
	}
	for _, path := range kept {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s should be registered in trading-only mode, got 404", path)
		}
	}

	trimmed := []string{
		"/api/v1/quotes/realtime",
		"/api/v1/market/news",
		"/api/v1/reviews/posts",
		"/api/v1/stocks/research",
		"/api/v1/portfolio-inspections",
		"/api/v1/cockpit/status",
		"/api/v1/ws/stream",
	}
	for _, path := range trimmed {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s should NOT be registered in trading-only mode, got %d", path, rec.Code)
		}
	}
}

// TestFullModeKeepsResearchRoutes 防止裁剪逻辑误伤全功能模式。
func TestFullModeKeepsResearchRoutes(t *testing.T) {
	server := NewServer(Config{})
	for _, path := range []string{"/api/v1/quotes/realtime", "/api/v1/reviews/posts", "/api/v1/cockpit/status"} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s should stay registered in full mode, got 404", path)
		}
	}
}
