package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/httpapi"
	"easy-stock/backend/internal/methodology"
	"easy-stock/backend/internal/providers/quantmind"
	"easy-stock/backend/internal/runtimelog"
	"easy-stock/backend/internal/trading"
)

func main() {
	addr := os.Getenv("A_STOCK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:20081"
	}
	// TradingOnly 把同一个可执行文件变成独立的 AI交易员程序：
	// 路由表只留交易与设置，调度器只跑交易自动循环。
	// 部署目标机器上只需要：本程序 + trading.db + 一份环境变量
	// （QuantMind 的局域网地址与账号密码见 deploy/trading-standalone.env.example）。
	tradingOnly := func() bool {
		switch strings.TrimSpace(strings.ToLower(os.Getenv("A_STOCK_TRADING_ONLY"))) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	}()
	reviewDBPath := os.Getenv("A_STOCK_REVIEW_DB")
	portfolioDBPath := os.Getenv("A_STOCK_PORTFOLIO_DB")
	stockResearchDBPath := os.Getenv("A_STOCK_RESEARCH_DB")
	tradingDBPath := os.Getenv("A_STOCK_TRADING_DB")
	marketEmotionDBPath := os.Getenv("A_STOCK_MARKET_EMOTION_DB")
	themeRadarDBPath := os.Getenv("A_STOCK_THEME_RADAR_DB")
	themeLibraryDBPath := os.Getenv("A_STOCK_THEME_LIBRARY_DB")
	themeLibraryRefreshAt := strings.TrimSpace(os.Getenv("A_STOCK_THEME_LIBRARY_REFRESH_AT"))
	if themeLibraryRefreshAt == "" {
		themeLibraryRefreshAt = "20:00"
	}
	if strings.EqualFold(themeLibraryRefreshAt, "off") {
		themeLibraryRefreshAt = ""
	}
	sectorFlowSampleDBPath := os.Getenv("A_STOCK_SECTOR_FLOW_SAMPLE_DB")
	settingsPath := os.Getenv("A_STOCK_SETTINGS_PATH")
	masteryCacheDir := os.Getenv("A_STOCK_MASTERY_CACHE")
	dataDir := ""
	if configDir, err := os.UserConfigDir(); err == nil {
		dataDir = preferredDataDir(configDir)
	}
	if reviewDBPath == "" {
		reviewDBPath = dataPath(dataDir, "reviews.db")
	}
	if settingsPath == "" {
		settingsPath = dataPath(dataDir, "settings.json")
	}
	if portfolioDBPath == "" {
		portfolioDBPath = dataPath(dataDir, "portfolio-inspections.db")
	}
	if stockResearchDBPath == "" {
		stockResearchDBPath = dataPath(dataDir, "stock-research.db")
	}
	if tradingDBPath == "" {
		tradingDBPath = dataPath(dataDir, "trading.db")
	}
	if marketEmotionDBPath == "" {
		marketEmotionDBPath = dataPath(dataDir, "market-emotion.db")
	}
	if themeRadarDBPath == "" {
		themeRadarDBPath = dataPath(dataDir, "theme-radar.db")
	}
	if themeLibraryDBPath == "" {
		themeLibraryDBPath = dataPath(dataDir, "theme-library.db")
	}
	if sectorFlowSampleDBPath == "" {
		sectorFlowSampleDBPath = dataPath(dataDir, "sector-flow-samples.db")
	}
	if masteryCacheDir == "" {
		masteryCacheDir = dataPath(dataDir, "trading-mastery")
	}
	logDirectory := os.Getenv("A_STOCK_LOG_DIR")
	if logDirectory == "" {
		logDirectory = dataPath(dataDir, "logs")
	}
	if logDirectory != "" {
		logger, closer, err := runtimelog.ConfigureStandard(logDirectory, "backend")
		if err != nil {
			log.Printf("runtime logging unavailable: %v", err)
		} else {
			defer closer.Close()
			logger.Printf("level=info event=runtime_start component=backend version=%q", runtimeVersion())
		}
	}
	hermesHome := os.Getenv("A_STOCK_HERMES_HOME")
	if hermesHome == "" {
		hermesHome = dataPath(dataDir, "hermes-home")
	}
	hermesWorkDir := os.Getenv("A_STOCK_HERMES_WORKDIR")
	if hermesWorkDir == "" {
		hermesWorkDir, _ = os.Getwd()
	}
	codexHome := os.Getenv("A_STOCK_CODEX_HOME")
	if codexHome == "" {
		codexHome = dataPath(dataDir, "codex-home")
	}
	codexRoot := os.Getenv("A_STOCK_CODEX_RUNTIME_ROOT")
	if codexRoot == "" {
		codexRoot = filepath.Join(filepath.Dir(resolveHermesRuntimeRoot()), "codex-runtime")
	}
	agentGateway := agent.NewService(agent.ServiceConfig{Hermes: agent.HermesConfig{
		RuntimeRoot: resolveHermesRuntimeRoot(),
		Home:        hermesHome,
		WorkDir:     hermesWorkDir,
		PythonPath:  os.Getenv("A_STOCK_HERMES_PYTHON"),
	}, Codex: agent.CodexConfig{RuntimeRoot: codexRoot, Home: codexHome, WorkDir: filepath.Join(codexHome, "workspace")}})
	masteryLibrary := methodology.NewLibrary(methodology.Config{
		CacheDir:   masteryCacheDir,
		HermesHome: hermesHome,
	})
	var quantMindKLine httpapi.KLineProvider
	if baseURL := strings.TrimSpace(os.Getenv("A_STOCK_QUANTMIND_URL")); baseURL != "" {
		// 只读接入：客户端只发 GET，不碰 QuantMind 的任何写接口。
		// 凭据不全时保持 nil，日线链路退回东财→新浪，与接入前完全一致。
		client, err := quantmind.NewClient(
			os.Getenv("A_STOCK_QUANTMIND_TENANT"),
			os.Getenv("A_STOCK_QUANTMIND_USER"),
			os.Getenv("A_STOCK_QUANTMIND_PASSWORD"),
			quantmind.WithBaseURL(baseURL),
		)
		if err != nil {
			log.Printf("level=warn event=quantmind_disabled url=%s error=%q note=凭据不完整，日线仍走东财→新浪", baseURL, err.Error())
		} else {
			quantMindKLine = client
			log.Printf("event=quantmind_enabled url=%s note=日线主源=QuantMind(前复权,最长4000根)，东财次之，新浪兜底", baseURL)
		}
	}
	server := httpapi.NewServer(httpapi.Config{
		Token:                    os.Getenv("A_STOCK_TOKEN"),
		TradingOnly:              tradingOnly,
		QuantMind:                quantMindKLine,
		ReviewDBPath:             reviewDBPath,
		PortfolioDBPath:          portfolioDBPath,
		StockResearchDBPath:      stockResearchDBPath,
		TradingDBPath:            tradingDBPath,
		TradingAlert:             tradingAlertConfig(logDirectory),
		RemoteDailyReviewURL:     os.Getenv("A_STOCK_DAILY_REVIEW_BASE_URL"),
		MarketEmotionDBPath:      marketEmotionDBPath,
		ThemeRadarDBPath:         themeRadarDBPath,
		ThemeLibraryDBPath:       themeLibraryDBPath,
		ThemeLibraryRefreshAt:    themeLibraryRefreshAt,
		SectorFlowSampleDBPath:   sectorFlowSampleDBPath,
		SectorFlowSampleInterval: sectorFlowSampleInterval(),
		DuanxianxiaBaseURL:       os.Getenv("A_STOCK_DUANXIANXIA_BASE_URL"),
		WeChatAPIURL:             os.Getenv("A_STOCK_WECHAT_API_URL"),
		SettingsPath:             settingsPath,
		AgentGateway:             agentGateway,
		MasteryLibrary:           masteryLibrary,
		Logger:                   log.Default(),
		StrictPersistence:        true,
	})
	if err := server.StartupError(); err != nil {
		log.Fatalf("persistent data startup failed: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// 独立 AI交易员模式（A_STOCK_TRADING_ONLY=1）只跑交易自动循环；
	// 其余调度器服务的是研究侧模块，独立模式下它们的路由都没有注册，
	// 起了也是空转，还平白制造对研究侧上游的周期性请求。
	go server.RunTradingAutopilot(ctx)
	if !tradingOnly {
		go server.RunReviewScheduler(ctx)
		go server.RunDailySummaryScheduler(ctx)
		go server.RunRemoteDailyReviewScheduler(ctx)
		go server.RunMarketEmotionScheduler(ctx)
		go server.RunSectorFlowSampler(ctx)
		go server.RunMasteryScheduler(ctx)
		go server.RunPortfolioScheduler(ctx)
		go server.RunCockpitPatrol(ctx)
		go server.RunThemeLibraryDailyRefresh(ctx)
	}
	httpServer := &http.Server{Addr: addr, Handler: server}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if tradingOnly {
		log.Printf("event=trading_only mode=standalone note=AI交易员独立模式：仅交易与设置路由，研究侧不加载")
	}
	log.Printf("easy-stock data foundation listening on http://%s", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	if err := server.Close(); err != nil {
		log.Printf("close persistent data: %v", err)
	}
}

// tradingAlertConfig 组装交易告警配置。
//
// 文件出口默认落在日志目录：没有配 webhook 时，告警至少留一条本地痕迹，
// 而不是只进数据库等人去翻。环境变量可以覆盖全部字段，
// 这样部署脚本不必改代码就能接上企微或钉钉机器人。
func tradingAlertConfig(logDirectory string) trading.AlertConfig {
	config := trading.AlertConfigFromEnv(nil)
	if config.LogPath == "" && strings.TrimSpace(logDirectory) != "" {
		config.LogPath = filepath.Join(logDirectory, "trading-alerts.log")
	}
	return config
}

func runtimeVersion() string {
	if value := strings.TrimSpace(os.Getenv("A_STOCK_APP_VERSION")); value != "" {
		return value
	}
	return "development"
}

func preferredDataDir(configDir string) string {
	current := filepath.Join(configDir, "easy-stock")
	if isFile(filepath.Join(current, "settings.json")) {
		return current
	}
	legacy := filepath.Join(configDir, "a-stock-ai")
	if isFile(filepath.Join(legacy, "settings.json")) {
		return legacy
	}
	return current
}

func isFile(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && !info.IsDir()
}

func dataPath(dataDir, name string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, name)
}

// sectorFlowSampleInterval 允许通过环境变量调整采样间隔。
//
// 默认 3 分钟（与公开同类产品一致）；低于 30 秒的配置会被忽略，
// 避免误配之后把上游榜单打成突发流量。
func sectorFlowSampleInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("A_STOCK_SECTOR_FLOW_SAMPLE_INTERVAL"))
	if raw == "" {
		return 0
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < 30*time.Second {
		return 0
	}
	return parsed
}

func resolveHermesRuntimeRoot() string {
	if configured := os.Getenv("A_STOCK_HERMES_RUNTIME_ROOT"); configured != "" {
		return configured
	}
	candidates := []string{}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "hermes-runtime")))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "desktop", "resources", "hermes-runtime"),
			filepath.Join(cwd, "..", "desktop", "resources", "hermes-runtime"),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}
