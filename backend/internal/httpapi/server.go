package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"easy-stock/backend/internal/agent"
	"easy-stock/backend/internal/appsettings"
	"easy-stock/backend/internal/cockpit"
	"easy-stock/backend/internal/foundation"
	"easy-stock/backend/internal/marketemotion"
	"easy-stock/backend/internal/methodology"
	"easy-stock/backend/internal/notification"
	"easy-stock/backend/internal/portfolioinspection"
	"easy-stock/backend/internal/portfoliooptimization"
	"easy-stock/backend/internal/providers/cls"
	"easy-stock/backend/internal/providers/duanxianxia"
	"easy-stock/backend/internal/providers/eastmoney"
	"easy-stock/backend/internal/providers/fund"
	futurespositionprovider "easy-stock/backend/internal/providers/futuresposition"
	"easy-stock/backend/internal/providers/hotstock"
	marketoverviewprovider "easy-stock/backend/internal/providers/marketoverview"
	"easy-stock/backend/internal/providers/sina"
	"easy-stock/backend/internal/providers/tencent"
	"easy-stock/backend/internal/review"
	"easy-stock/backend/internal/runtimelog"
	"easy-stock/backend/internal/sector"
	"easy-stock/backend/internal/sectorflowsample"
	"easy-stock/backend/internal/stockanalysis"
	"easy-stock/backend/internal/strategy/inflection"
	"easy-stock/backend/internal/themeindex"
	"easy-stock/backend/internal/themelibrary"
	"easy-stock/backend/internal/trading"
)

type Server struct {
	etfDirectionMarket       ETFDirectionMarketProvider
	etfUnderlyingFinancials  StockFinancialSupplementProvider
	etfUnderlyingValuation   StockValuationProvider
	etfExecution             ETFExecutionProvider
	etfProvider              ETFProvider
	etfKLinePrimary          RawKLineProvider
	etfKLineFallback         RawKLineProvider
	ladderThemeAI            *ladderThemeAI
	cockpit                  *cockpit.Service
	cockpitLive              cockpitLiveFacts
	mux                      *http.ServeMux
	token                    string
	realtimeProvider         RealtimeProvider
	kLinePrimary             KLineProvider
	kLineFallback            KLineProvider
	newsProvider             NewsProvider
	stockNewsSearch          StockNewsSearchProvider
	sectorMap                SectorMapProvider
	themeOverview            ThemeOverviewProvider
	themeIndex               *themeindex.Service
	limitUpProvider          LimitUpProvider
	marketPools              MarketPoolProvider
	stockConcepts            StockConceptProvider
	stockValuation           StockValuationProvider
	stockBusiness            StockBusinessProfileProvider
	stockFinancialSupplement StockFinancialSupplementProvider
	stockDirectory           StockDirectoryProvider
	industryStocks           IndustryStocksProvider
	hotStockProvider         HotStockProvider
	futuresPosition          FuturesPositionProvider
	marketOverview           MarketOverviewProvider
	// sectorFlowSamples 是板块资金流本地采样库：上游分时接口不可用时，曲线由它拼接。
	sectorFlowSamples        *sectorflowsample.Store
	sectorFlowSampleInterval time.Duration
	inflection               InflectionEvaluator
	themeSnapshots           *themeSnapshotCache
	limitUpSnapshots         *limitUpLadderCache
	limitUpProgress          *shortTermCache[limitUpLadderData]
	emotionProgress          *shortTermCache[marketemotion.History]
	stockDirectories         *stockDirectoryCache
	hotStockRanks            *hotStockRankCache
	marketSnapshots          *marketOverviewCache
	marketEmotion            *marketEmotionEngine
	marketEmotionIntraday    *marketEmotionIntradayCache
	reviewStore              *review.Store
	portfolioStore           *portfolioinspection.Store
	portfolioInspection      *portfolioinspection.Service
	portfolioScheduler       *portfolioinspection.Scheduler
	portfolioOptimization    *portfoliooptimization.Service
	portfolioExpectation     *portfolioinspection.ExpectationService
	trading                  *trading.Service
	tradingStore             *trading.Store
	tradingOnly              bool
	autopilot                *trading.Autopilot
	alertManager             *trading.AlertManager
	stockResearchStore       *stockanalysis.ResearchStore
	stockResearch            *stockanalysis.ResearchService
	reviewImporter           ReviewImporter
	wechatAPIURL             string
	settingsStore            *appsettings.Store
	notificationSender       notificationSender
	notifications            *notification.Dispatcher
	reviewAutomation         *review.Automation
	reviewScheduler          *review.DailyScheduler
	remoteDailySync          *review.RemoteDailySync
	agentGateway             agent.Gateway
	usageGateway             agent.Gateway
	masteryLibrary           *methodology.Library
	marketEmotionStore       *marketemotion.Store
	themeRadarStore          *duanxianxia.Store
	themeLibrary             *themelibrary.Store
	themeLibraryRefreshAt    string
	themeProgress            *themeProgressCache
	startupError             error
	logger                   *log.Logger
	tokenUsage               *tokenUsageStore
}

// eastMoneyDesktopOptions 接入桌面端提供的东财能力：浏览器会话（nid18）与 push2 转发桥。
//
// 分时资金流（push2 fflow/kline）有会话门禁；此外 push2 整条报价链路会按客户端
// 指纹拦截非浏览器请求（Go 直接被断连），所以桌面端把请求交给 Chromium 代发。
// 网页版两项都拿不到，保持直连 + 无会话的降级行为。
func eastMoneyDesktopOptions() []eastmoney.Option {
	var options []eastmoney.Option
	if provider := eastmoney.NewBrowserSessionProviderFromEnv(); provider != nil {
		options = append(options, eastmoney.WithSessionProvider(provider))
	}
	if proxy := eastmoney.NewPush2ProxyFromEnv(); proxy != nil {
		options = append(options, eastmoney.WithPush2Proxy(proxy))
	}
	return options
}

func NewServer(config any) *Server {
	cfg := normalizeConfig(config)
	if cfg.TradingOnly {
		// 独立 AI交易员模式：研究侧的持久化一律退化为内存实例。
		// 空路径在各个 OpenStore 里都定义为「开内存库」，所以这里
		// 只需要把路径清空，不需要逐个特判；主题雷达/题材库/板块采样
		// 本来就以「路径为空则跳过」为约定，清空即关闭。
		cfg.ReviewDBPath = ""
		cfg.PortfolioDBPath = ""
		cfg.StockResearchDBPath = ""
		cfg.MarketEmotionDBPath = ""
		cfg.ThemeRadarDBPath = ""
		cfg.ThemeLibraryDBPath = ""
		cfg.SectorFlowSampleDBPath = ""
		cfg.RemoteDailyReviewURL = ""
	}
	tokenUsage := newTokenUsageStore(cfg.SettingsPath)
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	var startupErrors []error
	sinaClient := sina.NewClient()
	eastMoneyClient := eastmoney.NewClient(eastMoneyDesktopOptions()...)
	tencentClient := tencent.NewClient()
	if cfg.ETF == nil {
		cfg.ETF = fund.NewClient()
		if cfg.ETFDirectionMarket == nil {
			cfg.ETFDirectionMarket = eastMoneyClient
		}
		if cfg.ETFUnderlyingFinancials == nil {
			cfg.ETFUnderlyingFinancials = sinaClient
		}
		if cfg.ETFUnderlyingValuation == nil {
			cfg.ETFUnderlyingValuation = tencentClient
		}
		if cfg.ETFExecution == nil {
			cfg.ETFExecution = sinaClient
		}
	}
	if cfg.ETFKLinePrimary == nil {
		cfg.ETFKLinePrimary = eastMoneyClient
	}
	if cfg.ETFKLineFallback == nil {
		cfg.ETFKLineFallback = sinaClient
	}
	clsClient := cls.NewClient()
	if cfg.Realtime == nil {
		if cfg.StockValuation == nil {
			cfg.StockValuation = tencentClient
		}
		cfg.Realtime = sinaClient
	}
	if cfg.KLinePrimary == nil {
		// QuantMind 在链路最前：它能给 4000 根前复权日线，而东财给不了那么长、
		// 新浪是不复权。它挂掉时自动落到东财，再落到新浪，决策不会因为
		// 局域网抖动而失去日线。没有配置 QuantMind 时行为与接入前一致。
		if cfg.QuantMind != nil {
			cfg.KLinePrimary = newKLineChain(cfg.QuantMind, eastMoneyClient)
		} else {
			cfg.KLinePrimary = eastMoneyClient
		}
	}
	if cfg.KLineFallback == nil {
		cfg.KLineFallback = sinaClient
	}
	if cfg.News == nil {
		if cfg.StockNews == nil {
			cfg.StockNews = eastMoneyClient
		}
		cfg.News = clsClient
	}
	var kaipanlaService *duanxianxia.Service
	if strings.TrimSpace(cfg.ThemeRadarDBPath) != "" {
		if store, err := duanxianxia.OpenStore(cfg.ThemeRadarDBPath); err == nil {
			client := duanxianxia.NewClient(duanxianxia.ClientConfig{BaseURL: cfg.DuanxianxiaBaseURL})
			kaipanlaService = duanxianxia.NewService(client, store, duanxianxia.ServiceConfig{
				RefreshInterval:  5 * time.Minute,
				LeaderThemeLimit: 3,
			})
		} else if cfg.StrictPersistence {
			startupErrors = append(startupErrors, fmt.Errorf("open theme radar database: %w", err))
		}
	}
	usingDefaultLimitUp := cfg.LimitUp == nil
	if usingDefaultLimitUp {
		if kaipanlaService != nil {
			cfg.LimitUp = duanxianxia.NewLimitUpProvider(kaipanlaService, eastMoneyClient)
		} else {
			cfg.LimitUp = eastMoneyClient
		}
	}
	if cfg.MarketPools == nil {
		cfg.MarketPools = eastMoneyClient
	}
	if cfg.StockConcept == nil && usingDefaultLimitUp {
		cfg.StockConcept = eastMoneyClient
	}
	if cfg.StockBusiness == nil {
		cfg.StockBusiness = eastMoneyClient
		if cfg.StockFinancialSupplement == nil {
			cfg.StockFinancialSupplement = sinaClient
		}
	}
	if cfg.StockDirectory == nil {
		cfg.StockDirectory = eastMoneyClient
	}
	if cfg.IndustryStocks == nil {
		cfg.IndustryStocks = tencentClient
	}
	if cfg.MarketOverview == nil {
		cfg.MarketOverview = marketoverviewprovider.New(eastMoneyClient, tencentClient, tencentClient, sinaClient)
	}
	if cfg.SectorMap == nil {
		mapper := sector.NewMapper(
			eastMoneyClient,
			sector.WithQuoteProvider(sinaClient),
			sector.WithLimitUpProvider(cfg.LimitUp),
			sector.WithStockCatalogProvider(eastMoneyClient),
		)
		var defaultSectorMap SectorMapProvider = mapper
		var defaultThemeOverview ThemeOverviewProvider = mapper
		var radarFallback sector.RadarFallback = mapper
		if cfg.ThemeRadarFallback != nil {
			radarFallback = cfg.ThemeRadarFallback
		}
		var radarSource sector.RadarSnapshotSource
		if kaipanlaService != nil {
			radarSource = kaipanlaService
		}
		radar := sector.NewRadarProvider(radarSource, radarFallback, cfg.Realtime, sector.RadarProviderConfig{
			IndustryMomentum:  cfg.MarketOverview,
			IndustryStocks:    tencentClient,
			FallbackFillLimit: 16,
		})
		defaultSectorMap = radar
		defaultThemeOverview = radar
		cfg.SectorMap = defaultSectorMap
		if cfg.ThemeOverview == nil {
			cfg.ThemeOverview = defaultThemeOverview
		}
	}
	if cfg.Inflection == nil {
		cfg.Inflection = inflection.NewEngine(inflection.DefaultConfig())
	}
	if cfg.ReviewStore == nil {
		store, err := review.OpenStore(cfg.ReviewDBPath)
		if err == nil {
			cfg.ReviewStore = store
		} else if cfg.StrictPersistence {
			startupErrors = append(startupErrors, fmt.Errorf("open review database: %w", err))
			cfg.ReviewStore, _ = review.OpenStore(":memory:")
		} else {
			cfg.ReviewStore, _ = review.OpenStore(":memory:")
		}
	}
	if cfg.MarketEmotionStore == nil {
		store, err := marketemotion.OpenStore(cfg.MarketEmotionDBPath)
		if err == nil {
			cfg.MarketEmotionStore = store
		} else if cfg.StrictPersistence {
			startupErrors = append(startupErrors, fmt.Errorf("open market emotion database: %w", err))
			cfg.MarketEmotionStore, _ = marketemotion.OpenStore("")
		} else {
			cfg.MarketEmotionStore, _ = marketemotion.OpenStore("")
		}
	}
	if cfg.SectorFlowSampleStore == nil && strings.TrimSpace(cfg.SectorFlowSampleDBPath) != "" {
		store, err := sectorflowsample.OpenStore(cfg.SectorFlowSampleDBPath)
		if err == nil {
			cfg.SectorFlowSampleStore = store
		} else {
			// 采样库只用于降级，打不开就退回纯上游模式，不应阻塞启动。
			cfg.Logger.Printf("level=warn event=startup feature=market-data task=sector_flow_sampler error=%q", runtimelog.Redact(err.Error()))
		}
	}
	if cfg.ThemeLibraryStore == nil && strings.TrimSpace(cfg.ThemeLibraryDBPath) != "" {
		store, err := themelibrary.Open(cfg.ThemeLibraryDBPath)
		if err == nil {
			cfg.ThemeLibraryStore = store
		} else {
			cfg.Logger.Printf("level=warn event=startup feature=market-data task=theme_library error=%q", runtimelog.Redact(err.Error()))
		}
	}
	if cfg.PortfolioStore == nil {
		store, err := portfolioinspection.OpenStore(cfg.PortfolioDBPath)
		if err == nil {
			cfg.PortfolioStore = store
		} else if cfg.StrictPersistence {
			startupErrors = append(startupErrors, fmt.Errorf("open portfolio inspection database: %w", err))
			cfg.PortfolioStore, _ = portfolioinspection.OpenStore(":memory:")
		} else {
			cfg.PortfolioStore, _ = portfolioinspection.OpenStore(":memory:")
		}
	}
	if cfg.TradingStore == nil {
		path := strings.TrimSpace(cfg.TradingDBPath)
		store, err := trading.OpenStore(path)
		if err != nil {
			if cfg.StrictPersistence {
				startupErrors = append(startupErrors, fmt.Errorf("open trading database: %w", err))
			}
			store, _ = trading.OpenStore(":memory:")
		}
		cfg.TradingStore = store
	}
	if cfg.StockResearchStore == nil {
		store, err := stockanalysis.OpenResearchStore(cfg.StockResearchDBPath)
		if err != nil {
			if cfg.StrictPersistence {
				startupErrors = append(startupErrors, fmt.Errorf("open stock research database: %w", err))
			}
			store, _ = stockanalysis.OpenResearchStore(":memory:")
		}
		cfg.StockResearchStore = store
	}
	if cfg.ReviewHTTP == nil {
		cfg.ReviewHTTP = &http.Client{Timeout: 90 * time.Second}
	}
	if cfg.SettingsStore == nil {
		store, err := appsettings.Open(cfg.SettingsPath)
		if err == nil {
			cfg.SettingsStore = store
		} else if cfg.StrictPersistence {
			startupErrors = append(startupErrors, fmt.Errorf("open settings: %w", err))
			cfg.SettingsStore, _ = appsettings.Open("")
		} else {
			cfg.SettingsStore, _ = appsettings.Open("")
		}
	}
	if cfg.HotStocks == nil {
		cfg.HotStocks = hotstock.NewClient()
	}
	if cfg.FuturesPosition == nil {
		cfg.FuturesPosition = futurespositionprovider.NewClient()
	}
	if cfg.AgentGateway != nil && (!cfg.StrictPersistence || len(startupErrors) == 0) {
		values := cfg.SettingsStore.Snapshot()
		var migratedKey *string
		if strings.TrimSpace(values.LLM.APIKey) != "" {
			key := strings.TrimSpace(values.LLM.APIKey)
			migratedKey = &key
			if updated, err := cfg.SettingsStore.Update(func(next *appsettings.Values) error {
				next.LLM.APIKey = ""
				return nil
			}); err == nil {
				values = updated
			}
		}
		if profileGateway, ok := cfg.AgentGateway.(agent.ProfileGateway); ok {
			if migratedKey == nil {
				if key, err := cfg.AgentGateway.ModelAPIKey(); err == nil && strings.TrimSpace(key) != "" {
					migratedKey = &key
				}
			}
			_ = profileGateway.SyncLLMProfile(values.LLM, values.ActiveLLMProfileID, migratedKey)
		} else {
			_ = cfg.AgentGateway.SyncLLM(values.LLM, migratedKey)
		}
	}
	usageGateway := newTokenUsageGateway(cfg.AgentGateway, tokenUsage)
	if service, ok := cfg.AgentGateway.(*agent.Service); ok {
		service.RestoreSelection(cfg.SettingsStore.Snapshot().AgentRuntime)
	}
	if cfg.ReviewImporter == nil {
		cfg.ReviewImporter = review.NewImporter(cfg.ReviewHTTP, cfg.WeChatAPIURL)
	}
	if cfg.ReviewAutomation == nil {
		cfg.ReviewAutomation = review.NewAutomation(cfg.ReviewStore, cfg.ReviewImporter, cfg.SettingsStore, cfg.ReviewHTTP, cfg.WeChatAPIURL, usageGateway)
	}
	if dailyMarketProvider := newReviewDailyMarketProvider(cfg.MarketOverview); dailyMarketProvider != nil {
		cfg.ReviewAutomation.SetDailyMarketProvider(dailyMarketProvider)
	}
	if cfg.RemoteDailySync == nil {
		cfg.RemoteDailySync = review.NewRemoteDailySync(cfg.ReviewStore, review.RemoteDailySyncConfig{
			BaseURL: cfg.RemoteDailyReviewURL,
			Client:  cfg.ReviewHTTP,
		})
	}
	s := &Server{
		etfUnderlyingFinancials: cfg.ETFUnderlyingFinancials, etfUnderlyingValuation: cfg.ETFUnderlyingValuation,
		etfDirectionMarket: cfg.ETFDirectionMarket, etfExecution: cfg.ETFExecution, etfProvider: cfg.ETF, etfKLinePrimary: cfg.ETFKLinePrimary, etfKLineFallback: cfg.ETFKLineFallback,
		mux:                      http.NewServeMux(),
		token:                    cfg.Token,
		realtimeProvider:         cfg.Realtime,
		kLinePrimary:             cfg.KLinePrimary,
		kLineFallback:            cfg.KLineFallback,
		newsProvider:             cfg.News,
		stockNewsSearch:          cfg.StockNews,
		sectorMap:                cfg.SectorMap,
		themeOverview:            cfg.ThemeOverview,
		limitUpProvider:          cfg.LimitUp,
		marketPools:              cfg.MarketPools,
		stockConcepts:            cfg.StockConcept,
		stockBusiness:            cfg.StockBusiness,
		stockValuation:           cfg.StockValuation,
		stockFinancialSupplement: cfg.StockFinancialSupplement,
		stockDirectory:           cfg.StockDirectory,
		industryStocks:           cfg.IndustryStocks,
		hotStockProvider:         cfg.HotStocks,
		futuresPosition:          cfg.FuturesPosition,
		marketOverview:           cfg.MarketOverview,
		sectorFlowSamples:        cfg.SectorFlowSampleStore,
		sectorFlowSampleInterval: cfg.SectorFlowSampleInterval,
		inflection:               cfg.Inflection,
		themeSnapshots:           newThemeSnapshotCache(30 * time.Second),
		themeProgress:            newThemeProgressCache(),
		limitUpSnapshots:         newLimitUpLadderCache(30 * time.Second),
		limitUpProgress:          &shortTermCache[limitUpLadderData]{},
		emotionProgress:          &shortTermCache[marketemotion.History]{},
		stockDirectories:         newStockDirectoryCache(6 * time.Hour),
		hotStockRanks:            newHotStockRankCache(2 * time.Minute),
		marketSnapshots:          newMarketOverviewCache(45 * time.Second),
		marketEmotionIntraday:    newMarketEmotionIntradayCache(marketEmotionIntradayTTL),
		reviewStore:              cfg.ReviewStore,
		portfolioStore:           cfg.PortfolioStore,
		stockResearchStore:       cfg.StockResearchStore,
		reviewImporter:           cfg.ReviewImporter,
		wechatAPIURL:             strings.TrimSpace(cfg.WeChatAPIURL),
		settingsStore:            cfg.SettingsStore,
		notificationSender:       notification.NewSender(),
		ladderThemeAI:            newLadderThemeAI(cfg.SettingsPath),
		reviewAutomation:         cfg.ReviewAutomation,
		remoteDailySync:          cfg.RemoteDailySync,
		agentGateway:             cfg.AgentGateway,
		usageGateway:             usageGateway,
		masteryLibrary:           cfg.MasteryLibrary,
		marketEmotionStore:       cfg.MarketEmotionStore,
		themeLibrary:             cfg.ThemeLibraryStore,
		themeLibraryRefreshAt:    cfg.ThemeLibraryRefreshAt,
		startupError:             errors.Join(startupErrors...),
		logger:                   cfg.Logger,
		tokenUsage:               tokenUsage,
	}
	if kaipanlaService != nil {
		s.themeRadarStore = kaipanlaService.Store()
		if payload, err := s.themeRadarStore.LoadOverview(context.Background()); err == nil && len(payload) > 0 {
			var cached foundation.ThemeProgress
			if json.Unmarshal(payload, &cached) == nil {
				cached.Refreshing = false
				cached.Meta.Stale = true
				s.themeProgress.value = cached
			}
		}
	}
	if s.themeRadarStore != nil {
		if payload, err := s.themeRadarStore.LoadLadder(context.Background()); err == nil && len(payload) > 0 {
			var cached shortTermProgress[limitUpLadderData]
			if json.Unmarshal(payload, &cached) == nil && cached.Data != nil {
				cached.Refreshing, cached.Stale = false, true
				cached.Data.Meta.Stale = true
				if cached.Data.Intraday != nil {
					cached.Data.Intraday.Stale = true
				}
				s.limitUpProgress.value = cached
			}
		}
	}
	s.marketEmotion = newMarketEmotionEngine(
		cfg.MarketEmotionStore,
		s.limitUpProvider,
		s.marketPools,
		s.kLinePrimary,
		s.kLineFallback,
		s.stockConcepts,
	)
	s.notifications = notification.NewDispatcher(func(ctx context.Context, channel string, cfg appsettings.NotificationChannel, message notification.Message) error {
		return s.notificationSender.Send(ctx, channel, cfg, message)
	}, func() appsettings.Notifications { return s.settingsStore.Snapshot().Notifications }, cfg.Logger)
	s.tradingStore = cfg.TradingStore
	s.tradingOnly = cfg.TradingOnly
	if s.tradingStore != nil {
		s.trading = trading.NewService(cfg.TradingStore, usageGateway, s.realtimeProvider, s.stockDirectory, cfg.Logger)
		// 实盘通道门控：地址与令牌齐全才启用，缺任何一项都留在模拟盘并明确记日志。
		// fail-closed——半套配置比没有配置危险，它让人以为实盘已经接好。
		if endpoint := strings.TrimSpace(os.Getenv("A_STOCK_TRADING_BROKER_ENDPOINT")); endpoint != "" {
			liveConfig := trading.LiveBrokerConfig{
				Endpoint: endpoint,
				Token:    os.Getenv("A_STOCK_TRADING_BROKER_TOKEN"),
			}
			if liveBroker, err := trading.NewLiveBroker(liveConfig); err != nil {
				startupErrors = append(startupErrors, fmt.Errorf("enable live trading broker: %w", err))
				s.logger.Printf("level=error event=trading_live_broker_disabled error=%q note=实盘通道配置不完整，继续使用模拟盘", err.Error())
			} else {
				s.trading.SetBroker(liveBroker)
				s.logger.Printf("level=warn event=trading_live_broker_enabled endpoint=%s note=实盘通道已启用，本页操作会真实下单", endpoint)
			}
		}
		// 板块资金流是可选能力：行情总览服务碰巧实现了它就直接接上，
		// 没实现也不影响交易，只是模型查不到资金流。
		if flow, ok := s.marketOverview.(trading.SectorFlowProvider); ok && flow != nil {
			s.trading.SetSectorFlow(flow)
		}
		// 日线同理：有主源就接，备源可为空。没有日线时交易照常跑，
		// 只是模型看不到均线与波动率——工具会明确说"数据源未配置"。
		if s.kLinePrimary != nil {
			s.trading.SetKLine(s.kLinePrimary, s.kLineFallback)
		}
		// 公告/龙虎榜/大盘/两融：行情总览已经全都有，这里只做接口收窄。
		// 缺了不影响交易，只是模型回答不了"为什么涨"。
		if s.marketOverview != nil {
			s.trading.SetMarketIntel(tradingMarketIntelAdapter{inner: s.marketOverview})
		}
		if news := newTradingNewsAdapter(s.newsProvider, s.stockNewsSearch); news != nil {
			s.trading.SetNews(news)
		}
		// 情绪周期：回答"市场人气冷不冷"，与指数涨跌不是同一个量。
		if emotion := newTradingEmotionAdapter(cfg.MarketEmotionStore); emotion != nil {
			s.trading.SetEmotion(emotion)
		}
		// 自动循环只构造不启动：Start 由 main 在服务可用之后显式调用。
		// 构造时启动会让"刚部署完就自动开始交易"变成默认行为。
		s.autopilot = trading.NewAutopilot(s.trading, cfg.Logger)
		// 告警外发：出口按配置逐个装上，一个都没配时仍然会构造管理器，
		// 它会把该发的告警落库但不外送——"没有出口"和"没有告警"必须能区分开。
		if !cfg.TradingAlert.Disabled {
			notifiers := make([]trading.Notifier, 0, 2)
			if cfg.TradingAlert.WebhookURL != "" {
				notifiers = append(notifiers, trading.NewWebhookNotifier("webhook", cfg.TradingAlert.WebhookURL, cfg.TradingAlert.WebhookType))
			}
			if fileNotifier := trading.NewFileNotifier(cfg.TradingAlert.LogPath); fileNotifier != nil {
				notifiers = append(notifiers, fileNotifier)
			}
			s.alertManager = trading.NewAlertManager(cfg.TradingStore, cfg.Logger, cfg.TradingAlert, notifiers...)
			s.alertManager.Start()
			s.trading.SetAlerts(s.alertManager)
		}
	}
	s.stockResearch = stockanalysis.NewResearchServiceWithPreparation(cfg.StockResearchStore, s.runStockResearch, s.prepareStockResearch, s.notifyStockResearch)
	s.portfolioInspection = portfolioinspection.NewService(cfg.PortfolioStore, usageGateway, s.analyzeStock, cfg.Logger, s.analyzeHoldingResearch)
	s.portfolioInspection.ConfigureCompletion(s.notifyPortfolioInspection)
	s.portfolioInspection.ConfigureResearch(s.resolvePortfolioResearch, s.refreshPortfolioQuotes)
	s.portfolioScheduler = portfolioinspection.NewScheduler(cfg.PortfolioStore, s.portfolioInspection)
	if cfg.ReviewCalendar == nil {
		cfg.ReviewCalendar = review.NewExchangeCalendar(cfg.ReviewStore, cfg.ReviewHTTP)
	}
	s.reviewScheduler = review.NewDailyScheduler(cfg.ReviewStore, s.reviewAutomation, cfg.ReviewCalendar)
	s.reviewAutomation.SetDailySummaryCompleted(s.notifyDailySummary)
	s.portfolioOptimization = portfoliooptimization.NewService(cfg.PortfolioStore, usageGateway, portfoliooptimization.Dependencies{Collect: s.collectOptimizationUniverse, Research: s.resolvePortfolioResearch, Quotes: s.refreshOptimizationQuotes})
	s.portfolioExpectation = portfolioinspection.NewExpectationService(cfg.PortfolioStore, cfg.ReviewStore, usageGateway, s.analyzeStock, cfg.Logger, s.analyzeHoldingResearch)
	s.themeIndex = cfg.ThemeIndex
	if s.themeIndex == nil {
		history := &themeIndexHistory{primary: eastMoneyClient, fallback: tencentClient}
		s.themeIndex = themeindex.New(themeindex.Config{
			Boards: eastMoneyClient,
			Resolve: func(ctx context.Context, theme, snapshot string) (foundation.ThemeIndexTarget, error) {
				if resolver, ok := s.sectorMap.(interface {
					ResolveThemeIndex(context.Context, string, string) (foundation.ThemeIndexTarget, error)
				}); ok {
					return resolver.ResolveThemeIndex(ctx, theme, snapshot)
				}
				value, err := s.loadThemeIndexMembers(ctx, theme, snapshot)
				return foundation.ThemeIndexTarget{Name: value.Name, Names: []string{value.Name}}, err
			},
			Members: s.loadThemeIndexMembers,
			KLines:  history.load,
		})
	}
	s.cockpit = s.newCockpitPatrolService(cfg.SettingsPath, cfg.Logger)
	s.routes()
	return s
}

func (s *Server) StartupError() error {
	if s == nil {
		return errors.New("server is nil")
	}
	return s.startupError
}

func (s *Server) Close() error {
	if s != nil && s.themeProgress != nil {
		s.themeProgress.close()
	}
	if s == nil {
		return nil
	}
	if s.limitUpProgress != nil {
		s.limitUpProgress.close()
	}
	if s.emotionProgress != nil {
		s.emotionProgress.close()
	}
	if s.reviewScheduler != nil {
		s.reviewScheduler.Close()
	}
	if s.portfolioScheduler != nil {
		s.portfolioScheduler.Close()
	}
	if s.autopilot != nil {
		s.autopilot.Stop()
	}
	// 告警在自动循环之后停：循环退出前最后产生的事件仍然值得发出去。
	if s.alertManager != nil {
		s.alertManager.Stop()
	}
	var closeErrors []error
	if s.notifications != nil {
		s.notifications.Close()
	}
	if s.portfolioOptimization != nil {
		s.portfolioOptimization.Close()
	}
	if s.portfolioInspection != nil {
		s.portfolioInspection.Close()
	}
	if s.stockResearch != nil {
		s.stockResearch.Close()
	}
	if s.stockResearchStore != nil {
		closeErrors = append(closeErrors, s.stockResearchStore.Close())
	}
	if s.reviewStore != nil {
		closeErrors = append(closeErrors, s.reviewStore.Close())
	}
	if s.marketEmotionStore != nil {
		closeErrors = append(closeErrors, s.marketEmotionStore.Close())
	}
	if s.sectorFlowSamples != nil {
		closeErrors = append(closeErrors, s.sectorFlowSamples.Close())
	}
	if s.portfolioStore != nil {
		closeErrors = append(closeErrors, s.portfolioStore.Close())
	}
	if s.themeRadarStore != nil {
		closeErrors = append(closeErrors, s.themeRadarStore.Close())
	}
	if s.themeLibrary != nil {
		closeErrors = append(closeErrors, s.themeLibrary.Close())
	}
	return errors.Join(closeErrors...)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	requestID := runtimeRequestID(r)
	loggedWriter := &requestLogWriter{ResponseWriter: w}
	loggedWriter.Header().Set("X-Request-ID", requestID)
	defer s.logRequest(r, loggedWriter, requestID, startedAt)

	s.withCORS(loggedWriter, r)
	if r.Method == http.MethodOptions {
		loggedWriter.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.authorized(r) {
		writeError(loggedWriter, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mux.ServeHTTP(loggedWriter, r)
}

func (s *Server) RunReviewScheduler(ctx context.Context) {
	if s.reviewAutomation != nil {
		s.logSchedulerLifecycle(ctx, "reviews", "subscription_sync", func() {
			s.reviewAutomation.RunScheduler(ctx, s.logger)
		})
	}
}

func (s *Server) RunRemoteDailyReviewScheduler(ctx context.Context) {
	if s.remoteDailySync != nil {
		s.logSchedulerLifecycle(ctx, "reviews", "remote_daily_sync", func() {
			s.remoteDailySync.Run(ctx, s.logger)
		})
	}
}

func (s *Server) RunMarketEmotionScheduler(ctx context.Context) {
	if s.marketEmotion != nil {
		s.logSchedulerLifecycle(ctx, "short-term", "market_emotion", func() {
			s.marketEmotion.runScheduler(ctx, s.logger)
		})
	}
}

// RunTradingAutopilot 常驻运行自动交易循环。
//
// 开关不在启动时判断：循环要不要跑是运行期事实，由配置决定。
// 这里只负责让它"活着"，否则用户打开开关后必须重启进程才生效。
func (s *Server) RunTradingAutopilot(ctx context.Context) {
	if s.autopilot == nil {
		return
	}
	// 启动时把告警出口喊出来。"没人收到通知"和"一直没出事"从外面看是一样的，
	// 只有把出口清单打进日志，事后才能分清到底属于哪一种。
	if s.alertManager != nil {
		channels := s.alertManager.Channels()
		if len(channels) == 0 {
			s.logger.Printf("event=trading_alerts_no_channel note=告警只会落库不会外发，请配置 A_STOCK_TRADING_ALERT_WEBHOOK")
		} else {
			s.logger.Printf("event=trading_alerts_ready channels=%s", strings.Join(channels, ","))
		}
	}
	s.autopilot.Start(ctx)
	defer s.autopilot.Stop()
	<-ctx.Done()
}

// RunSectorFlowSampler 常驻记录板块资金流快照，让分时资金/价格曲线在上游
// 分钟序列接口不可用时仍能从本地采样拼接出来。
func (s *Server) RunSectorFlowSampler(ctx context.Context) {
	provider, ok := s.marketOverview.(SectorFlowProvider)
	if !ok || s.sectorFlowSamples == nil {
		return
	}
	interval := s.sectorFlowSampleInterval
	if interval <= 0 {
		interval = sectorflowsample.DefaultInterval
	}
	sampler := sectorflowsample.NewSampler(s.sectorFlowSamples, provider, sectorflowsample.SamplerConfig{
		Interval: interval,
		Logger:   s.logger,
	})
	sampler.Run(ctx)
}

func (s *Server) RunMasteryScheduler(ctx context.Context) {
	if s.masteryLibrary == nil {
		return
	}
	if s.logger != nil {
		s.logger.Printf("level=info event=scheduler_start feature=trading-mastery task=mastery_snapshot")
		defer s.logger.Printf("level=info event=scheduler_stop feature=trading-mastery task=mastery_snapshot")
	}
	s.refreshMasterySnapshot(ctx, false)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshMasterySnapshot(ctx, true)
		}
	}
}

func (s *Server) logSchedulerLifecycle(_ context.Context, feature, task string, run func()) {
	if s.logger != nil {
		s.logger.Printf("level=info event=scheduler_start feature=%s task=%s", feature, task)
		defer s.logger.Printf("level=info event=scheduler_stop feature=%s task=%s", feature, task)
	}
	run()
}

func (s *Server) refreshMasterySnapshot(ctx context.Context, force bool) {
	startedAt := time.Now()
	_, err := s.masteryLibrary.Snapshot(ctx, force)
	if s.logger == nil {
		return
	}
	if err != nil {
		s.logger.Printf("level=warn event=scheduler_error feature=trading-mastery task=mastery_snapshot force=%t duration_ms=%d error=%q", force, time.Since(startedAt).Milliseconds(), runtimelog.Redact(err.Error()))
		return
	}
	s.logger.Printf("level=info event=scheduler_run feature=trading-mastery task=mastery_snapshot force=%t duration_ms=%d", force, time.Since(startedAt).Milliseconds())
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/v1/sources", s.sources)
	if !s.tradingOnly {
		// 行情总览 / 复盘 / 组合检视等研究侧路由：独立 AI交易员模式下不注册，
		// 让「这个进程能做什么」由路由表直接声明，而不是靠没人去调它。
		s.mux.HandleFunc("GET /api/v1/quotes/realtime", s.realtime)
	s.mux.HandleFunc("GET /api/v1/quotes/kline", s.kline)
	s.mux.HandleFunc("GET /api/v1/quotes/kline/batch", s.klineBatch)
	s.mux.HandleFunc("GET /api/v1/market/news", s.news)
	s.mux.HandleFunc("GET /api/v1/market/indexes", s.marketIndexesHandler)
	s.mux.HandleFunc("GET /api/v1/market/index-series", s.marketIndexSeriesHandler)
	s.mux.HandleFunc("GET /api/v1/market/industries", s.marketIndustriesHandler)
	s.mux.HandleFunc("GET /api/v1/market/flows", s.marketFlowsHandler)
	s.mux.HandleFunc("GET /api/v1/market/sector-flows", s.marketSectorFlowsHandler)
	s.mux.HandleFunc("GET /api/v1/market/sector-flow-intraday", s.marketSectorFlowIntradayHandler)
	s.mux.HandleFunc("GET /api/v1/market/sector-price-intraday", s.marketSectorPriceIntradayHandler)
	s.mux.HandleFunc("GET /api/v1/market/theme-library", s.themeLibraryListHandler)
	s.mux.HandleFunc("GET /api/v1/market/theme-library/{id}", s.themeLibraryDetailHandler)
	s.mux.HandleFunc("GET /api/v1/market/margin-balance", s.marketMarginBalanceHandler)
	s.mux.HandleFunc("GET /api/v1/market/billboard", s.marketBillboardHandler)
	s.mux.HandleFunc("GET /api/v1/market/billboard/detail", s.marketBillboardDetailHandler)
	s.mux.HandleFunc("GET /api/v1/market/futures-position", s.marketFuturesPositionHandler)
	s.mux.HandleFunc("GET /api/v1/market/futures-members", s.marketFuturesMembersHandler)
	s.mux.HandleFunc("GET /api/v1/market/futures-consensus", s.marketFuturesConsensusHandler)
	s.mux.HandleFunc("GET /api/v1/research/announcements", s.marketAnnouncementsHandler)
	s.mux.HandleFunc("GET /api/v1/research/institution-reports", s.marketInstitutionReportsHandler)
	s.mux.HandleFunc("GET /api/v1/research/industries", s.marketIndustryResearchHandler)
	s.mux.HandleFunc("GET /api/v1/research/report-content", s.marketReportContentHandler)
	s.mux.HandleFunc("GET /api/v1/research/announcement-content", s.marketAnnouncementContentHandler)
	s.mux.HandleFunc("GET /api/v1/themes/overview", s.themeOverviewHandler)
	s.mux.HandleFunc("GET /api/v1/themes/index", s.themeIndexHandler)
	s.mux.HandleFunc("GET /api/v1/themes/screen", s.themeScreenHandler)
	s.mux.HandleFunc("GET /api/v1/sector-map", s.sectorMapHandler)
	s.mux.HandleFunc("GET /api/v1/short-term/limit-up-ladder", s.limitUpLadderHandler)
	s.mux.HandleFunc("POST /api/v1/short-term/ladder-theme-ai", s.ladderThemeAIIdentify)
	s.mux.HandleFunc("GET /api/v1/short-term/ladder-theme-ai", s.ladderThemeAIResults)
	s.mux.HandleFunc("GET /api/v1/short-term/emotion-history", s.marketEmotionHistoryHandler)
	s.mux.HandleFunc("GET /api/v1/short-term/mastery", s.masteryIndex)
	s.mux.HandleFunc("GET /api/v1/short-term/mastery/trader", s.masteryTrader)
	s.mux.HandleFunc("POST /api/v1/short-term/mastery/refresh", s.masteryRefresh)
	s.mux.HandleFunc("POST /api/v1/stocks/ai-analysis", s.stockAIAnalysis)
	s.mux.HandleFunc("POST /api/v1/stocks/research", s.stockResearchCreate)
	s.mux.HandleFunc("GET /api/v1/stocks/research", s.stockResearchList)
	s.mux.HandleFunc("GET /api/v1/stocks/research/{id}", s.stockResearchGet)
	s.mux.HandleFunc("DELETE /api/v1/stocks/research/{id}", s.stockResearchDelete)
	s.mux.HandleFunc("POST /api/v1/stocks/research/{id}/cancel", s.stockResearchCancel)
	s.mux.HandleFunc("POST /api/v1/stocks/research/{id}/resume", s.stockResearchResume)
	s.mux.HandleFunc("POST /api/v1/stocks/research/{id}/verify", s.stockResearchVerify)
	s.mux.HandleFunc("GET /api/v1/stocks/research/{id}/snapshot", s.stockResearchSnapshot)
	s.mux.HandleFunc("GET /api/v1/stocks/directory", s.stockDirectoryHandler)
	s.mux.HandleFunc("GET /api/v1/instruments", s.instrumentDirectoryHandler)
	s.mux.HandleFunc("POST /api/v1/instruments/research", s.stockResearchCreate)
	s.mux.HandleFunc("GET /api/v1/instruments/research", s.stockResearchList)
	s.mux.HandleFunc("GET /api/v1/instruments/research/{id}", s.stockResearchGet)
	s.mux.HandleFunc("DELETE /api/v1/instruments/research/{id}", s.stockResearchDelete)
	s.mux.HandleFunc("POST /api/v1/instruments/research/{id}/cancel", s.stockResearchCancel)
	s.mux.HandleFunc("POST /api/v1/instruments/research/{id}/resume", s.stockResearchResume)
	s.mux.HandleFunc("POST /api/v1/instruments/research/{id}/verify", s.stockResearchVerify)
	s.mux.HandleFunc("GET /api/v1/instruments/research/{id}/snapshot", s.stockResearchSnapshot)
	s.mux.HandleFunc("GET /api/v1/stocks/hot-ranks", s.hotStockRanksHandler)
	s.mux.HandleFunc("POST /api/v1/portfolio-inspections/{id}/optimizations", s.portfolioOptimizationCreate)
	s.mux.HandleFunc("GET /api/v1/portfolio-inspections/{id}/optimizations", s.portfolioOptimizationList)
	s.mux.HandleFunc("GET /api/v1/portfolio-optimizations/{id}", s.portfolioOptimizationGet)
	s.mux.HandleFunc("POST /api/v1/portfolio-optimizations/{id}/cancel", s.portfolioOptimizationCancel)
	s.mux.HandleFunc("POST /api/v1/portfolio-optimizations/{id}/resume", s.portfolioOptimizationResume)
	s.mux.HandleFunc("GET /api/v1/portfolio-schedules/{id}", s.portfolioScheduleGet)
	s.mux.HandleFunc("PUT /api/v1/portfolio-schedules/{id}", s.portfolioScheduleSave)
	s.mux.HandleFunc("DELETE /api/v1/portfolio-schedules/{id}", s.portfolioScheduleDelete)
	s.mux.HandleFunc("GET /api/v1/portfolio-inspections", s.portfolioInspectionList)
	s.mux.HandleFunc("POST /api/v1/portfolio-inspections", s.portfolioInspectionCreate)
	s.mux.HandleFunc("GET /api/v1/portfolio-inspections/{id}", s.portfolioInspectionGet)
	s.mux.HandleFunc("DELETE /api/v1/portfolio-inspections/{id}", s.portfolioInspectionDelete)
	s.mux.HandleFunc("POST /api/v1/portfolio-inspections/{id}/resume", s.portfolioInspectionResume)
	s.mux.HandleFunc("POST /api/v1/portfolio-inspections/{id}/bind-plan", s.portfolioInspectionBindPlan)
	s.mux.HandleFunc("POST /api/v1/portfolio-inspections/{id}/cancel", s.portfolioInspectionCancel)
	}
	// 交易路由：独立模式的全部业务面，全功能模式下与其他模块并列。
	s.mux.HandleFunc("GET /api/v1/trading/account", s.tradingAccount)
	s.mux.HandleFunc("POST /api/v1/trading/account/reset", s.tradingResetAccount)
	s.mux.HandleFunc("GET /api/v1/trading/market", s.tradingMarket)
	s.mux.HandleFunc("GET /api/v1/trading/universe", s.tradingUniverse)
	s.mux.HandleFunc("GET /api/v1/trading/control", s.tradingControlGet)
	s.mux.HandleFunc("POST /api/v1/trading/control", s.tradingControlSet)
	s.mux.HandleFunc("GET /api/v1/trading/autopilot", s.tradingAutopilotStatus)
	s.mux.HandleFunc("POST /api/v1/trading/autopilot", s.tradingAutopilotConfigure)
	s.mux.HandleFunc("POST /api/v1/trading/autopilot/breaker/reset", s.tradingBreakerReset)
	// 持仓管理：防守线由确定性代码执行，人工只能设置线本身。
	s.mux.HandleFunc("GET /api/v1/trading/positions", s.tradingPositions)
	s.mux.HandleFunc("POST /api/v1/trading/positions/{symbol}/protection", s.tradingPositionProtection)
	s.mux.HandleFunc("POST /api/v1/trading/protective/run", s.tradingProtectiveRun)
	// 复盘：事实由账本算，接口只读或触发重算。
	s.mux.HandleFunc("GET /api/v1/trading/closed-trades", s.tradingClosedTrades)
	s.mux.HandleFunc("GET /api/v1/trading/performance", s.tradingPerformance)
	s.mux.HandleFunc("GET /api/v1/trading/memory", s.tradingMemory)
	s.mux.HandleFunc("POST /api/v1/trading/memory/sync", s.tradingMemorySync)
	s.mux.HandleFunc("GET /api/v1/trading/plans/{id}/research", s.tradingPlanResearch)
	s.mux.HandleFunc("GET /api/v1/trading/reviews", s.tradingReviewList)
	s.mux.HandleFunc("POST /api/v1/trading/reviews/run", s.tradingReviewRun)
	s.mux.HandleFunc("GET /api/v1/trading/reviews/{day}", s.tradingReviewGet)
	s.mux.HandleFunc("GET /api/v1/trading/cycles", s.tradingCycleList)
	s.mux.HandleFunc("POST /api/v1/trading/cycles/run", s.tradingCycleRun)
	s.mux.HandleFunc("POST /api/v1/trading/reconcile", s.tradingReconcile)
	// 运维面：健康检查给监控用，事件流给人排障用。
	s.mux.HandleFunc("GET /api/v1/trading/health", s.tradingHealth)
	s.mux.HandleFunc("GET /api/v1/trading/calibration", s.tradingCalibration)
	// 风险面：回撤与总闸。它是账户级事实，与"系统健康"分开看。
	s.mux.HandleFunc("GET /api/v1/trading/risk", s.tradingRisk)
	s.mux.HandleFunc("POST /api/v1/trading/risk/drawdown/release", s.tradingDrawdownRelease)
	s.mux.HandleFunc("GET /api/v1/trading/events", s.tradingEvents)
	s.mux.HandleFunc("GET /api/v1/trading/channel", s.tradingChannel)
	s.mux.HandleFunc("GET /api/v1/trading/alerts", s.tradingAlerts)
	s.mux.HandleFunc("POST /api/v1/trading/alerts/test", s.tradingAlertTest)
	s.mux.HandleFunc("GET /api/v1/trading/timeline", s.tradingTimeline)
	// 委托回报入口：接实盘时由券商回调或轮询器推入，ReportID 保证幂等。
	s.mux.HandleFunc("POST /api/v1/trading/reports", s.tradingReportSubmit)
	s.mux.HandleFunc("POST /api/v1/trading/plans", s.tradingPlanCreate)
	s.mux.HandleFunc("GET /api/v1/trading/plans", s.tradingPlanList)
	s.mux.HandleFunc("GET /api/v1/trading/decisions", s.tradingDecisionList)
	s.mux.HandleFunc("GET /api/v1/trading/decisions/{id}", s.tradingDecisionGet)
	s.mux.HandleFunc("GET /api/v1/trading/plans/{id}", s.tradingPlanGet)
	s.mux.HandleFunc("POST /api/v1/trading/plans/{id}/execute", s.tradingPlanExecute)
	s.mux.HandleFunc("POST /api/v1/trading/plans/{id}/reject", s.tradingPlanReject)
	s.mux.HandleFunc("POST /api/v1/trading/plans/{id}/sandbox", s.tradingPlanSandbox)
	s.mux.HandleFunc("POST /api/v1/trading/match", s.tradingMatchPending)
	s.mux.HandleFunc("POST /api/v1/trading/orders/{id}/cancel", s.tradingOrderCancel)
	if !s.tradingOnly {
		s.mux.HandleFunc("POST /api/v1/reviews/portfolio-expectations", s.portfolioExpectationCreate)
	s.mux.HandleFunc("GET /api/v1/reviews/portfolio-expectations/latest", s.portfolioExpectationLatest)
	s.mux.HandleFunc("GET /api/v1/reviews/portfolio-expectations/{id}", s.portfolioExpectationGet)
	s.mux.HandleFunc("GET /api/v1/reviews/sources", s.reviewSources)
	s.mux.HandleFunc("GET /api/v1/reviews/authors", s.reviewAuthors)
	s.mux.HandleFunc("DELETE /api/v1/reviews/authors/{id}", s.reviewAuthorDelete)
	s.mux.HandleFunc("GET /api/v1/reviews/posts", s.reviewPosts)
	s.mux.HandleFunc("GET /api/v1/reviews/posts/{id}", s.reviewPost)
	s.mux.HandleFunc("DELETE /api/v1/reviews/posts/{id}", s.reviewPostDelete)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-summary/schedule", s.reviewScheduleGet)
	s.mux.HandleFunc("PUT /api/v1/reviews/daily-summary/schedule", s.reviewScheduleSave)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-summary", s.reviewDailySummaryGet)
	s.mux.HandleFunc("POST /api/v1/reviews/daily-summary/anonymize", s.reviewDailySummaryAnonymize)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-summary/window", s.reviewDailySummaryWindow)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-summary/status", s.reviewDailySummaryStatus)
	s.mux.HandleFunc("POST /api/v1/reviews/daily-summary", s.reviewDailySummaryCreate)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-validation", s.reviewDailyValidation)
	s.mux.HandleFunc("GET /api/v1/reviews/daily-validation/status", s.reviewDailyValidationStatus)
	s.mux.HandleFunc("POST /api/v1/reviews/daily-validation", s.reviewDailyValidationCreate)
	s.mux.HandleFunc("POST /api/v1/reviews/import", s.reviewImport)
	s.mux.HandleFunc("GET /api/v1/reviews/subscriptions", s.reviewSubscriptions)
	s.mux.HandleFunc("POST /api/v1/reviews/subscriptions", s.reviewSubscriptionCreate)
	s.mux.HandleFunc("DELETE /api/v1/reviews/subscriptions/{id}", s.reviewSubscriptionDelete)
	s.mux.HandleFunc("POST /api/v1/reviews/sync", s.reviewSyncAll)
	s.mux.HandleFunc("POST /api/v1/reviews/subscriptions/{id}/sync", s.reviewSyncOne)
	s.mux.HandleFunc("POST /api/v1/reviews/posts/{id}/analyze", s.reviewAnalyzePost)
	s.mux.HandleFunc("GET /api/v1/reviews/remote-daily/status", s.reviewRemoteDailyStatus)
	s.mux.HandleFunc("POST /api/v1/reviews/remote-daily/sync", s.reviewRemoteDailySync)
	}
	// 设置与 LLM 配置路由：独立模式也保留——换大脑、配模型档位
	// 是运维 AI交易员本身的动作，不是研究侧功能。
	s.mux.HandleFunc("GET /api/v1/settings", s.settingsGet)
	s.mux.HandleFunc("GET /api/v1/settings/token-usage", s.tokenUsageSummary)
	s.mux.HandleFunc("GET /api/v1/settings/token-usage/calls", s.tokenUsageCalls)
	s.mux.HandleFunc("POST /api/v1/settings/token-usage", s.tokenUsageRecord)
	s.mux.HandleFunc("PUT /api/v1/settings", s.settingsUpdate)
	s.mux.HandleFunc("GET /api/v1/settings/agent", s.settingsAgentGet)
	s.mux.HandleFunc("PUT /api/v1/settings/agent", s.settingsAgentUpdate)
	s.mux.HandleFunc("POST /api/v1/settings/agent/skills/import", s.settingsAgentSkillImport)
	s.mux.HandleFunc("POST /api/v1/settings/agent/skills/delete", s.settingsAgentSkillDelete)
	s.mux.HandleFunc("POST /api/v1/settings/agent/skills/install-git", s.settingsAgentSkillInstallGit)
	s.mux.HandleFunc("GET /api/v1/settings/agent/skills/market", s.settingsAgentSkillMarket)
	s.mux.HandleFunc("GET /api/v1/settings/agent/skills/market/sources", s.settingsAgentSkillMarketSources)
	s.mux.HandleFunc("POST /api/v1/settings/llm/models", s.settingsLLMModels)
	s.mux.HandleFunc("POST /api/v1/settings/llm/api-key/reveal", s.settingsLLMAPIKeyReveal)
	s.mux.HandleFunc("POST /api/v1/settings/llm/test", s.settingsLLMTest)
	s.mux.HandleFunc("GET /api/v1/settings/notifications", s.settingsNotificationsGet)
	s.mux.HandleFunc("PUT /api/v1/settings/notifications", s.settingsNotificationsUpdate)
	s.mux.HandleFunc("POST /api/v1/settings/notifications/reveal", s.settingsNotificationsReveal)
	s.mux.HandleFunc("POST /api/v1/settings/notifications/test", s.settingsNotificationsTest)
	if !s.tradingOnly {
		s.mux.HandleFunc("GET /api/v1/ai/ws", s.aiChatWebSocket)
		s.mux.HandleFunc("POST /api/v1/strategy/inflections/evaluate", s.inflectionEvaluate)
		s.mux.HandleFunc("GET /api/v1/cockpit/status", s.cockpitStatus)
		s.mux.HandleFunc("GET /api/v1/cockpit/facts", s.cockpitFacts)
		s.mux.HandleFunc("GET /api/v1/cockpit/briefs", s.cockpitBriefs)
		s.mux.HandleFunc("GET /api/v1/cockpit/briefs/{id}", s.cockpitBrief)
		s.mux.HandleFunc("POST /api/v1/cockpit/briefs", s.cockpitRun)
		s.mux.HandleFunc("POST /api/v1/cockpit/auto-patrol", s.cockpitAutoPatrol)
		s.mux.HandleFunc("GET /api/v1/ws/stream", s.stream)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"name": "easy-stock data foundation",
		"time": time.Now(),
	})
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sources": []foundation.SourceHealth{
			{ID: "duanxianxia", Name: "短线侠 / 开盘啦", Category: "theme,leaders,limit-up,concept", OK: true, CheckedAt: time.Now()},
			{ID: "eastmoney", Name: "东方财富", Category: "quote,kline,f10,report", OK: true, CheckedAt: time.Now()},
			{ID: "sina", Name: "新浪财经", Category: "quote,kline,money-flow", OK: true, CheckedAt: time.Now()},
			{ID: "tencent", Name: "腾讯财经", Category: "quote,index,hk", OK: true, CheckedAt: time.Now()},
			{ID: "cls", Name: "财联社", Category: "news,calendar", OK: true, CheckedAt: time.Now()},
			{ID: "tradingview", Name: "TradingView", Category: "news", OK: true, CheckedAt: time.Now()},
			{ID: "tushare", Name: "Tushare", Category: "basic,daily,index", OK: false, Message: "requires token", CheckedAt: time.Now()},
		},
	})
}

func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	symbolsParam := strings.TrimSpace(r.URL.Query().Get("symbols"))
	if symbolsParam == "" {
		writeError(w, http.StatusBadRequest, "symbols is required")
		return
	}
	symbols, err := foundation.SplitSymbols(symbolsParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	quotes, err := s.realtimeProvider.Realtime(r.Context(), symbols)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": quotes})
}

func (s *Server) kline(w http.ResponseWriter, r *http.Request) {
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if symbol == "" {
		writeError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	period := firstNonEmpty(r.URL.Query().Get("period"), "day")
	limit := 120
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	lines, err := s.loadKLine(ctx, symbol, period, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": lines})
}

func (s *Server) klineBatch(w http.ResponseWriter, r *http.Request) {
	symbolsParam := strings.TrimSpace(r.URL.Query().Get("symbols"))
	if symbolsParam == "" {
		writeError(w, http.StatusBadRequest, "symbols is required")
		return
	}
	symbols, err := foundation.SplitSymbols(symbolsParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(symbols) > 30 {
		writeError(w, http.StatusBadRequest, "batch kline supports at most 30 symbols")
		return
	}
	period := firstNonEmpty(r.URL.Query().Get("period"), "day")
	limit := 40
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 || parsed > 240 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 240")
			return
		}
		limit = parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	data := make(map[string][]foundation.KLine, len(symbols))
	errorsBySymbol := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for _, symbol := range symbols {
		symbol := symbol
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				mu.Lock()
				errorsBySymbol[symbol] = ctx.Err().Error()
				mu.Unlock()
				return
			}
			lines, loadErr := s.loadKLine(ctx, symbol, period, limit)
			mu.Lock()
			defer mu.Unlock()
			if loadErr != nil {
				errorsBySymbol[symbol] = loadErr.Error()
				return
			}
			data[symbol] = lines
		}()
	}
	wg.Wait()
	if len(data) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "all batch kline requests failed", "errors": errorsBySymbol})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "errors": errorsBySymbol})
}

func (s *Server) loadKLine(ctx context.Context, symbol string, period string, limit int) ([]foundation.KLine, error) {
	lines, err := s.kLinePrimary.KLine(ctx, symbol, period, limit)
	if err == nil {
		return normalizeKLinePeriod(lines, period), nil
	}
	lines, err = s.kLineFallback.KLine(ctx, symbol, period, limit)
	if err != nil {
		return nil, err
	}
	return normalizeKLinePeriod(lines, period), nil
}

func normalizeKLinePeriod(lines []foundation.KLine, period string) []foundation.KLine {
	if strings.TrimSpace(period) != "1" || len(lines) == 0 {
		return lines
	}

	chinaTime := time.FixedZone("Asia/Shanghai", 8*60*60)
	latestTime := time.Time{}
	for _, line := range lines {
		if !line.Time.IsZero() && line.Time.After(latestTime) {
			latestTime = line.Time
		}
	}
	if latestTime.IsZero() {
		return lines
	}

	latestDate := latestTime.In(chinaTime).Format("2006-01-02")
	previousTime := time.Time{}
	previousClose := 0.0
	for _, line := range lines {
		if line.Time.IsZero() || line.Time.In(chinaTime).Format("2006-01-02") == latestDate {
			continue
		}
		if line.Close > 0 && line.Time.Before(latestTime) && line.Time.After(previousTime) {
			previousTime = line.Time
			previousClose = line.Close
		}
	}

	filtered := make([]foundation.KLine, 0, len(lines))
	for _, line := range lines {
		if line.Time.IsZero() || line.Time.In(chinaTime).Format("2006-01-02") != latestDate {
			continue
		}
		if line.PreviousClose <= 0 && previousClose > 0 {
			line.PreviousClose = previousClose
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return lines
	}
	return filtered
}

func (s *Server) news(w http.ResponseWriter, r *http.Request) {
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if source == "" {
		source = "cls"
	}
	if source != "cls" {
		writeError(w, http.StatusBadRequest, "unsupported news source")
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	items, err := s.newsProvider.LatestNews(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (s *Server) sectorMapHandler(w http.ResponseWriter, r *http.Request) {
	theme := strings.TrimSpace(r.URL.Query().Get("theme"))
	if theme == "" {
		theme = "semiconductor_materials"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sectorMap, err := s.sectorMap.Build(ctx, theme)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": sectorMap})
}

func (s *Server) themeOverviewHandler(w http.ResponseWriter, r *http.Request) {
	if s.themeOverview == nil {
		writeError(w, http.StatusServiceUnavailable, "theme overview provider is unavailable")
		return
	}
	if r.URL.Query().Get("delivery") == "progressive" {
		s.progressiveThemeOverview(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	items, meta, err := s.themeOverview.Overviews(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items, "meta": meta})
}

func (s *Server) inflectionEvaluate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request inflection.EvaluationRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid inflection request: "+err.Error())
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.inflection.Evaluate(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": result})
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func writeError(w http.ResponseWriter, status int, message string) {
	if status >= http.StatusInternalServerError {
		log.Printf("level=error event=http_error status=%d message=%q", status, runtimelog.Redact(message))
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
