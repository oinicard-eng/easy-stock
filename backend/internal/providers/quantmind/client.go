// Package quantmind 对接局域网 QuantMind 平台（FastAPI）的只读数据接口。
//
// 这里刻意只实现「历史行情 + 基本面快照 + 个股资金流」，不碰任何写入端点：
// QuantMind 的 /api/v1/admin/* 会触发数据同步、重建 Qlib、重跑推理，
// 交易模块没有理由调用它们。
//
// QuantMind 的实时行情接口实测返回 503（QuantDB 无实时推送），所以实时行情
// 仍然由 sina/eastmoney 提供，本包只解决「历史与截面」这两件本地没数据的事。
package quantmind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"easy-stock/backend/internal/foundation"
)

// 默认值写在这里而不是散落在调用处：改一处即可整体调整。
const (
	defaultBaseURL = "http://192.168.5.6:8000"
	// 局域网实测往返 87-98ms，15 秒足够覆盖一次登录加一次查询。
	defaultTimeout = 15 * time.Second
	// days 参数服务端下限 5、上限 4000（读取自 openapi）。
	minDays = 5
	maxDays = 4000
	// token 按 JWT exp 提前刷新，避免临界点上刚好过期。
	tokenRefreshMargin = 5 * time.Minute
)

// Client 是 QuantMind 的只读客户端。零值不可用，必须经 NewClient 构造。
type Client struct {
	baseURL   string
	tenantID  string
	username  string
	password  string
	http      *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// Option 用于覆盖默认配置，测试里主要用来替换 baseURL。
type Option func(*Client)

func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/"); trimmed != "" {
			c.baseURL = trimmed
		}
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// NewClient 构造客户端。凭据不全时返回 nil 与错误，
// 调用方据此决定「不启用」而不是拿到一个每次调用都失败的半成品。
func NewClient(tenantID, username, password string, opts ...Option) (*Client, error) {
	tenantID = strings.TrimSpace(tenantID)
	username = strings.TrimSpace(username)
	if tenantID == "" || username == "" || password == "" {
		return nil, fmt.Errorf("quantmind 凭据不完整：tenant/user/password 均不能为空")
	}
	client := &Client{
		baseURL:  defaultBaseURL,
		tenantID: tenantID,
		username: username,
		password: password,
		http:     &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

// Enabled 供装配处判断是否接入，避免到处判空。
func (c *Client) Enabled() bool { return c != nil }

// ---------------------------------------------------------------------------
// 日线：trading.KLineProvider 的实现
// ---------------------------------------------------------------------------

// KLine 返回按时间升序排列的日线，根数不超过 limit。
//
// 与服务端的三处差异在这里抹平：
//  1. period 我方一律传 "day"，服务端只认 "daily"；
//  2. 复权默认取前复权（qfq）。不传的话服务端默认不复权，
//     除权日会在指标上造出一个不存在的跳空；
//  3. days 的语义是**自然日**而不是交易日——实测 days=250 会回溯约 500 个
//     自然日、返回 332 根。东财与新浪的 limit 是根数。若直接把 limit 透传，
//     同一句「看一年分位」在三个源上会算出三种不同的窗口。这里统一成根数：
//     请求 limit、再截末尾 limit 根，不足则全取（如 limit=4000 时服务端只给
//     2620 根全历史，此时按全历史返回，不补造也不报错）。
//
// 返回值第二项错误非空时，调用方（tools.go）会转到备源，所以这里不做任何
// 兜底填充：拿不到就是拿不到。
func (c *Client) KLine(ctx context.Context, symbol string, period string, limit int) ([]foundation.KLine, error) {
	if c == nil {
		return nil, fmt.Errorf("quantmind 客户端未启用")
	}
	normalized, err := foundation.NormalizeSymbol(symbol)
	if err != nil {
		return nil, err
	}
	if kind := normalizePeriod(period); kind == "" {
		return nil, fmt.Errorf("quantmind 不支持周期 %q", period)
	}
	// requested 是调用方要的根数，用于最终截取；days 是发给服务端的参数，
	// 会被下限/上限改写。两者必须分开，否则 limit=2 会被服务端下限 5 抬高，
	// 截出 5 根，上层拿到的窗口与它要求的对不上。
	requested := limit
	if requested <= 0 {
		requested = 120
	}
	days := requested
	if days < minDays {
		days = minDays
	}
	if days > maxDays {
		days = maxDays
	}

	query := url.Values{}
	query.Set("symbol", normalized.Canonical)
	query.Set("market", "A")
	query.Set("period", "daily")
	query.Set("days", strconv.Itoa(days))
	// 只用前复权。后复权实测与不复权返回同一组数字（服务端未生效），
	// 用它会得到一个看起来正常、实际未复权的序列。
	query.Set("adjust", "qfq")

	var payload klineResponse
	if err := c.getJSON(ctx, "/api/v1/market/kline", query, &payload); err != nil {
		return nil, err
	}
	rows, err := payload.rows()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("quantmind 未返回 %s 的日线", normalized.Canonical)
	}

	meta := foundation.SourceMeta{
		Source:          "quantmind",
		FetchedAt:       time.Now(),
		AvailableFields: []string{"open", "high", "low", "close", "volume", "amount"},
	}
	bars := make([]foundation.KLine, 0, len(rows))
	var previousClose float64
	for _, row := range rows {
		day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(row.Date), cstZone())
		if err != nil {
			return nil, fmt.Errorf("quantmind 日线日期无法解析 %q: %w", row.Date, err)
		}
		if row.Close <= 0 {
			// 停牌日服务端可能给出 0 收盘。这类行留着会把均线与 ATR 拉低，
			// 直接跳过，与 cleanKLineBars 的判定口径一致。
			continue
		}
		bar := foundation.KLine{
			Symbol: normalized.Canonical,
			Time:   day,
			Open:   row.Open,
			High:   row.High,
			Low:    row.Low,
			Close:  row.Close,
			Volume: row.Volume,
			Amount: row.Amount,
			Meta:   meta,
		}
		if previousClose > 0 {
			bar.PreviousClose = previousClose
			bar.ChangePercent = (row.Close/previousClose - 1) * 100
		}
		previousClose = row.Close
		bars = append(bars, bar)
	}
	if len(bars) < 2 {
		return nil, fmt.Errorf("quantmind 返回 %s 的有效日线不足 2 根", normalized.Canonical)
	}
	// 统一成"根数"语义，见方法注释第 3 条。
	if len(bars) > requested {
		bars = bars[len(bars)-requested:]
	}
	return bars, nil
}

// ---------------------------------------------------------------------------
// 全市场截面：基本面字段
// ---------------------------------------------------------------------------

// StockSnapshot 是全市场一只标的的截面快照。字段取自 /api/v1/stocks/all?enrich=true。
type StockSnapshot struct {
	Symbol          string  `json:"symbol"`
	Code            string  `json:"code"`
	Name            string  `json:"name"`
	Exchange        string  `json:"exchange"`
	Close           float64 `json:"close"`
	PE              float64 `json:"pe"`
	PB              float64 `json:"pb"`
	MarketCap       float64 `json:"marketCap"`
	FloatMarketCap  float64 `json:"floatMarketCap"`
	ChangePercent   float64 `json:"pctChange"`
	TurnoverRate    float64 `json:"turnoverRate"`
	IsST            bool    `json:"isSt"`
	ListedDays      int     `json:"listedDays"`
}

// Snapshots 拉取全市场（A 股）带基本面的截面。
// 实测 5579 只、约 1.5MB、1.5 秒，适合每轮决策前取一次并缓存。
func (c *Client) Snapshots(ctx context.Context) ([]StockSnapshot, error) {
	if c == nil {
		return nil, fmt.Errorf("quantmind 客户端未启用")
	}
	query := url.Values{}
	query.Set("market", "A")
	query.Set("enrich", "true")

	var payload snapshotsResponse
	if err := c.getJSON(ctx, "/api/v1/stocks/all", query, &payload); err != nil {
		return nil, err
	}
	if len(payload.Items) == 0 {
		return nil, fmt.Errorf("quantmind 未返回全市场快照")
	}
	return payload.Items, nil
}

// ---------------------------------------------------------------------------
// 个股资金流
// ---------------------------------------------------------------------------

// StockMoneyFlow 是一只标的的资金流明细，含主力/超大单/大单等多层净额，
// 以及最近 30 个交易日的逐日流入流出。这是我方本地完全没有的数据。
type StockMoneyFlow struct {
	Symbol        string  `json:"symbol"`
	Name          string  `json:"name"`
	ClosePrice    float64 `json:"close_price"`
	ChangePercent float64 `json:"pct_change"`
	NetInflow     float64 `json:"net_inflow"`
	GrossInflow   float64 `json:"gross_inflow"`
	GrossOutflow  float64 `json:"gross_outflow"`
	MainRatio     float64 `json:"main_ratio"`
	SuperLarge    float64 `json:"super_large"`
	Large         float64 `json:"large"`
	Medium        float64 `json:"medium"`
	Small         float64 `json:"small"`
	// Trend30D 是最近 30 日净流入序列（单位与 net_inflow 一致）。
	Trend30D []float64 `json:"trend_30d"`
}

// MoneyFlow 返回个股资金流排行。
// 显式关掉 realtime：服务端 realtime=true 会绕过快照实时计算，实测在非交易时段
// 反而更容易失败；这里要的是「最近一个可用快照」，不是逐笔实时。
func (c *Client) MoneyFlow(ctx context.Context, limit int) ([]StockMoneyFlow, error) {
	if c == nil {
		return nil, fmt.Errorf("quantmind 客户端未启用")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))

	var rows []StockMoneyFlow
	if err := c.getJSON(ctx, "/api/v1/market-analysis/money-flow/stocks", query, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("quantmind 未返回个股资金流")
	}
	// 资金流接口的 symbol 是 SH688018 这种前缀式，与 market/kline 的
	// 688018.SH 不同。统一成 Canonical，否则调用方按代码匹配会全部落空。
	// 单只格式异常不改原值，也不整体失败——列表里其他票仍然可用。
	for i := range rows {
		if normalized, err := foundation.NormalizeSymbol(rows[i].Symbol); err == nil {
			rows[i].Symbol = normalized.Canonical
		}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// 认证与传输
// ---------------------------------------------------------------------------

// ensureToken 返回可用的 access token，必要时登录。
// token 不可用时清空而不是返回旧值：拿着过期 token 去请求只会换回 401，
// 而 401 之后还要再走一次重试，等于一次失败请求变两次。
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(tokenRefreshMargin).Before(c.expiresAt) {
		return c.token, nil
	}
	body := map[string]string{
		"tenant_id": c.tenantID,
		"username":  c.username,
		"password":  c.password,
	}
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/login", nil, body)
	if err != nil {
		return "", fmt.Errorf("quantmind 登录失败：%w", err)
	}
	var result loginResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("quantmind 登录响应无法解析：%w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("quantmind 登录未返回 access_token")
	}
	// 服务端不给 exp 时按 30 分钟保守处理，反正下次 401 会重新登录。
	c.token = result.AccessToken
	c.expiresAt = time.Now().Add(30 * time.Minute)
	if exp := jwtExpiry(result.AccessToken); !exp.IsZero() {
		c.expiresAt = exp
	}
	return c.token, nil
}

// invalidateToken 在收到 401 时丢掉本地 token，下一次调用重新登录。
func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

// getJSON 发起带鉴权的 GET 并把 data 解到 out。
// 401 只重试一次：重试第二次说明凭据本身有问题，继续只会拖长一轮决策的耗时。
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	raw, err := c.get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("quantmind 响应解析失败（%s）：%w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.ensureToken(ctx)
		if err != nil {
			return nil, err
		}
		raw, status, err := c.do(ctx, http.MethodGet, path, query, nil, token)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.invalidateToken()
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("quantmind %s 返回 HTTP %d：%s", path, status, summarizeBody(raw))
		}
		// 业务错误也用 200 包裹（{"success":false,"error":...}）时在这里拦下，
		// 否则上层会拿到一份空 data 并误判成"今天没有数据"。
		if err := apiError(raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	return nil, fmt.Errorf("quantmind %s 鉴权失败：重新登录后仍返回 401", path)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	raw, status, err := c.do(ctx, method, path, query, reader, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d：%s", status, summarizeBody(raw))
	}
	return raw, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, token string) ([]byte, int, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("quantmind 请求失败（%s）：%w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("quantmind 读取响应失败（%s）：%w", path, err)
	}
	return raw, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 响应解析
// ---------------------------------------------------------------------------

// klineResponse 兼容服务端两种截然不同的 data 形态：
// days=4000 时 data 是数组，days=5 时 data 是 {items:[...]}。
// 这不是笔误，是同一个端点的实测行为，两套都要认。
type klineResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *apiErrorBody   `json:"error"`
}

type klineRow struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
	Amount float64 `json:"amount"`
}

func (r klineResponse) rows() ([]klineRow, error) {
	trimmed := strings.TrimSpace(string(r.Data))
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("quantmind 日线响应缺少 data")
	}
	if strings.HasPrefix(trimmed, "[") {
		var rows []klineRow
		if err := json.Unmarshal(r.Data, &rows); err != nil {
			return nil, fmt.Errorf("quantmind 日线数组解析失败：%w", err)
		}
		return rows, nil
	}
	var wrapped struct {
		Items []klineRow `json:"items"`
	}
	if err := json.Unmarshal(r.Data, &wrapped); err != nil {
		return nil, fmt.Errorf("quantmind 日线对象解析失败：%w", err)
	}
	return wrapped.Items, nil
}

type snapshotsResponse struct {
	Count int             `json:"count"`
	Items []StockSnapshot `json:"items"`
}

type loginResponse struct {
	AccessToken string `json:"access_token"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// apiError 识别 {"error":{...}} 与 {"success":false,...} 两种失败包装。
func apiError(raw []byte) error {
	var envelope struct {
		Success *bool         `json:"success"`
		Error   *apiErrorBody `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil // 不是包封结构，交给具体解析处理
	}
	if envelope.Error != nil && (envelope.Error.Message != "" || envelope.Error.Code != "") {
		message := envelope.Error.Message
		if message == "" {
			message = envelope.Error.Detail
		}
		return fmt.Errorf("quantmind 返回错误 %s：%s", envelope.Error.Code, message)
	}
	if envelope.Success != nil && !*envelope.Success {
		return fmt.Errorf("quantmind 返回 success=false")
	}
	return nil
}

func summarizeBody(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return "（空响应）"
	}
	return text
}

// jwtExpiry 只读 JWT 的 exp，不做签名校验——这里不是信任边界，
// 目的是避免在 token 刚过期时发起一次必然失败的请求。
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func normalizePeriod(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", "day", "daily", "d":
		return "daily"
	default:
		return ""
	}
}

// decodeSegment 解 base64url（JWT 段不带 padding）。
func decodeSegment(segment string) ([]byte, error) {
	if padding := len(segment) % 4; padding > 0 {
		segment += strings.Repeat("=", 4-padding)
	}
	return base64.URLEncoding.DecodeString(segment)
}

func cstZone() *time.Location {
	return time.FixedZone("CST", 8*3600)
}
