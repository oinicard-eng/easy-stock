package trading

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 告警外发解决的是无人值守的最后一个断点：
// 系统出事时，事件流写进库、健康条变红，但人不在电脑前，
// 这两件事等于没有发生。告警必须"离开这台机器"才算送达。
//
// 设计上刻意把「出口」和「收敛」分开：
//   - 出口（Notifier）只负责把一条已经定稿的告警送出去，不判断该不该送；
//   - 收敛（AlertManager）负责决定发不发、合并多少条、发完记什么。
//
// 分开的原因是出口会换（企微、钉钉、Slack、短信），而"什么算重复告警"
// 的语义不该随出口变化。

const (
	AlertStatusSent       = "sent"
	AlertStatusFailed     = "failed"
	AlertStatusSuppressed = "suppressed"
)

// 出口格式。wecom 与 dingtalk 的文本消息结构相同，但保留两个名字，
// 是因为将来钉钉要加签名、企微不会，混成一个名字会在那时变成隐患。
const (
	AlertFormatWeCom    = "wecom"
	AlertFormatDingTalk = "dingtalk"
	AlertFormatJSON     = "json"
)

// Alert 是一条待外发告警。它是 Event 的外发投影：
// 只保留"人要立刻知道的部分"，不把内部 payload 全量推出去。
type Alert struct {
	AccountID  string
	Severity   string
	Kind       string
	Symbol     string
	Message    string
	OccurredAt time.Time
	// Merged 是冷却期内被合并掉的同类告警条数。
	// 带上它，收件人才知道"这是一次偶发"还是"已经连续炸了 40 分钟"。
	Merged int
	// Summary 为 true 表示这是一条"仍在持续"的汇总，
	// 不是新问题，避免收件人重复排查。
	Summary bool
}

// Title 给告警配一句短标题。收件人先看标题决定要不要点开。
func (a Alert) Title() string {
	label := "提示"
	switch a.Severity {
	case EventSeverityCritical:
		label = "严重"
	case EventSeverityWarning:
		label = "警告"
	}
	name := a.Symbol
	if strings.TrimSpace(name) == "" {
		name = a.Kind
	}
	return fmt.Sprintf("[easy-stock 交易][%s] %s", label, name)
}

// Text 把告警渲染成一段人能直接读的文字。
func (a Alert) Text() string {
	var b strings.Builder
	b.WriteString(a.Title())
	b.WriteString("\n")
	b.WriteString(a.OccurredAt.Format("2006-01-02 15:04:05"))
	b.WriteString("\n")
	b.WriteString(a.Message)
	if a.Merged > 0 {
		b.WriteString(fmt.Sprintf("\n（冷却期内另有 %d 条同类告警已合并）", a.Merged))
	}
	if a.Summary {
		b.WriteString("\n（此问题仍在持续，本条为周期汇总）")
	}
	b.WriteString("\n类型：")
	b.WriteString(a.Kind)
	return b.String()
}

// Notifier 是一个告警出口。实现方只做"送出去"，
// 返回 error 表示这条告警没有送达，由 AlertManager 记账。
type Notifier interface {
	Name() string
	Notify(ctx context.Context, alert Alert) error
}

// WebhookNotifier 把告警 POST 到一个 HTTP 端点。
//
// 它同时覆盖企微机器人与钉钉机器人，因为两者都是"贴一个 URL 就能用"，
// 这是自建系统里成本最低、也最少出故障的推送方式。
type WebhookNotifier struct {
	name   string
	url    string
	format string
	client *http.Client
}

// NewWebhookNotifier 构造 webhook 出口。format 为空时按通用 JSON 处理。
func NewWebhookNotifier(name, url, format string) *WebhookNotifier {
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case AlertFormatWeCom, AlertFormatDingTalk, AlertFormatJSON:
	default:
		format = AlertFormatJSON
	}
	return &WebhookNotifier{
		name:   name,
		url:    strings.TrimSpace(url),
		format: format,
		// 超时给 8 秒：告警通道本身慢的时候，不能把告警队列堵死。
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

func (w *WebhookNotifier) Name() string { return w.name }

func (w *WebhookNotifier) Notify(ctx context.Context, alert Alert) error {
	if w.url == "" {
		return fmt.Errorf("webhook 地址为空")
	}
	body, err := w.encode(alert)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 读掉响应体才能复用连接，也才能把对端返回的错误带进日志。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 返回 %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

func (w *WebhookNotifier) encode(alert Alert) ([]byte, error) {
	if w.format == AlertFormatJSON {
		return json.Marshal(map[string]any{
			"severity":    alert.Severity,
			"kind":        alert.Kind,
			"symbol":      alert.Symbol,
			"message":     alert.Message,
			"occurred_at": alert.OccurredAt.Format(time.RFC3339),
			"merged":      alert.Merged,
			"summary":     alert.Summary,
			"text":        alert.Text(),
		})
	}
	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": alert.Text()},
	}
	if w.format == AlertFormatDingTalk {
		payload["at"] = map[string]any{"isAtAll": false}
	}
	return json.Marshal(payload)
}

// FileNotifier 把告警追加写到本地文件。
//
// 它是兜底出口：没有配 webhook 时，告警至少留下一条带时间戳的痕迹，
// 而不是只存在于数据库里等人去翻。写文件失败不重试——
// 磁盘写不进去说明问题比告警本身更大，重试只会拖慢队列。
type FileNotifier struct {
	path string
	mu   sync.Mutex
}

// NewFileNotifier 构造文件出口。路径为空时返回 nil，表示不启用。
func NewFileNotifier(path string) *FileNotifier {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return &FileNotifier{path: path}
}

func (f *FileNotifier) Name() string { return "file" }

func (f *FileNotifier) Notify(ctx context.Context, alert Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	line, err := json.Marshal(map[string]any{
		"at":       alert.OccurredAt.Format(time.RFC3339),
		"severity": alert.Severity,
		"kind":     alert.Kind,
		"symbol":   alert.Symbol,
		"message":  alert.Message,
		"merged":   alert.Merged,
	})
	if err != nil {
		return err
	}
	_, err = file.Write(append(line, '\n'))
	return err
}

// AlertConfig 是告警配置。全部来自环境变量：
// 无人值守场景下配置要能随部署脚本走，不该依赖手工改数据库。
type AlertConfig struct {
	WebhookURL  string
	WebhookType string
	LogPath     string
	Disabled    bool
	// CooldownCritical / CooldownWarning 是同类告警的冷却窗口。
	// critical 短一些（10 分钟）：严重问题反复提醒是对的；
	// warning 长一些（1 小时）：积压、熔断这类状态不需要每分钟喊一次。
	CooldownCritical time.Duration
	CooldownWarning  time.Duration
}

// AlertConfigFromEnv 读取告警配置。
//
// 没有任何出口时不算错误：本地开发默认就该安静，
// 但会在启动日志里明确写"告警未配置"，避免上线后才发现一直没人收到通知。
func AlertConfigFromEnv(getenv func(string) string) AlertConfig {
	if getenv == nil {
		getenv = os.Getenv
	}
	config := AlertConfig{
		WebhookURL:       strings.TrimSpace(getenv("A_STOCK_TRADING_ALERT_WEBHOOK")),
		WebhookType:      strings.TrimSpace(getenv("A_STOCK_TRADING_ALERT_FORMAT")),
		LogPath:          strings.TrimSpace(getenv("A_STOCK_TRADING_ALERT_LOG")),
		CooldownCritical: 10 * time.Minute,
		CooldownWarning:  time.Hour,
	}
	switch strings.ToLower(strings.TrimSpace(getenv("A_STOCK_TRADING_ALERT_OFF"))) {
	case "1", "true", "yes":
		config.Disabled = true
	}
	return config
}

// alertState 是单个 key 的冷却状态。
type alertState struct {
	lastSent     time.Time
	lastSeverity string
	merged       int
}

// AlertManager 负责收敛与投递。
//
// 它必须是非阻塞的：调用方是交易主路径，一条告警发得慢不能拖住下单。
// 所以 Submit 只入队，发送发生在独立 worker 里。
type AlertManager struct {
	store     *Store
	notifiers []Notifier
	logger    *log.Logger
	now       func() time.Time

	cooldownCritical time.Duration
	cooldownWarning  time.Duration

	mu    sync.Mutex
	state map[string]*alertState

	ch       chan Alert
	done     chan struct{}
	started  bool
	stopOnce sync.Once
	wg       sync.WaitGroup

	// 计数用于自证：告警系统自己也会坏，坏的时候要能看出来。
	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
	merged  atomic.Int64
}

// NewAlertManager 构造告警管理器。notifiers 为空时仍然会落库，
// 只是不往外送——这样"该发的告警"这件事本身不会丢。
func NewAlertManager(store *Store, logger *log.Logger, config AlertConfig, notifiers ...Notifier) *AlertManager {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	manager := &AlertManager{
		store:            store,
		notifiers:        notifiers,
		logger:           logger,
		now:              time.Now,
		cooldownCritical: config.CooldownCritical,
		cooldownWarning:  config.CooldownWarning,
		state:            make(map[string]*alertState),
		ch:               make(chan Alert, 256),
		done:             make(chan struct{}),
	}
	if manager.cooldownCritical <= 0 {
		manager.cooldownCritical = 10 * time.Minute
	}
	if manager.cooldownWarning <= 0 {
		manager.cooldownWarning = time.Hour
	}
	return manager
}

// Start 启动投递 worker。
func (m *AlertManager) Start() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	m.wg.Add(1)
	go m.loop()
	m.logger.Printf("event=alerts_started channels=%d", len(m.notifiers))
}

// Stop 停止 worker。已经在队列里的告警会被尽量发完：
// 关闭瞬间丢掉最后一条 critical，恰恰是最不该丢的那一条。
func (m *AlertManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		m.mu.Lock()
		started := m.started
		m.started = false
		m.mu.Unlock()
		if !started {
			return
		}
		close(m.done)
		m.wg.Wait()
	})
}

func (m *AlertManager) loop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.done:
			// 排空剩余告警后再退出，最多处理 32 条，避免退出被长队列拖住。
			for i := 0; i < 32; i++ {
				select {
				case alert := <-m.ch:
					m.deliver(alert)
				default:
					return
				}
			}
			return
		case alert := <-m.ch:
			m.deliver(alert)
		}
	}
}

// Submit 把一条事件转成告警入队。它从不阻塞交易路径。
func (m *AlertManager) Submit(event Event) {
	if m == nil || !shouldAlert(event) {
		return
	}
	alert := Alert{
		AccountID:  event.AccountID,
		Severity:   event.Severity,
		Kind:       event.Kind,
		Symbol:     event.Symbol,
		Message:    event.Message,
		OccurredAt: event.CreatedAt,
	}
	if alert.OccurredAt.IsZero() {
		alert.OccurredAt = m.now()
	}
	select {
	case m.ch <- alert:
		return
	default:
	}
	if alert.Severity != EventSeverityCritical {
		m.dropped.Add(1)
		return
	}
	// critical 不能静默丢弃：给它一次短暂等待的机会，
	// 队列满说明告警出口已经严重堵塞，这时候丢掉的恰好是最要紧的一条。
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case m.ch <- alert:
	case <-timer.C:
		m.dropped.Add(1)
		m.logger.Printf("event=alert_dropped kind=%s severity=%s reason=queue_full", alert.Kind, alert.Severity)
	}
}

// shouldAlert 判断一条事件要不要外发。
//
// 规则很短：warning 以上要发；恢复类事件即使 info 也发——
// 人收到"出问题了"之后，最想知道的下一件事是"好了没有"。
func shouldAlert(event Event) bool {
	switch event.Severity {
	case EventSeverityCritical, EventSeverityWarning:
		return true
	}
	switch event.Kind {
	case EventAutopilotRecovered, EventLedgerDriftHealed, EventDrawdownRelease:
		return true
	}
	return false
}

// cooldownKey 把"同一类问题"收敛到同一个 key。
// 包含 symbol 而不包含 message：同一标的的同一类问题反复发生时，
// message 里的数字会变（漂移 1.2 元变成 1.8 元），但它在人眼里是同一件事。
func cooldownKey(severity, kind, symbol string) string {
	return kind + "|" + symbol
}

func severityRank(severity string) int {
	switch severity {
	case EventSeverityCritical:
		return 2
	case EventSeverityWarning:
		return 1
	}
	return 0
}

func (m *AlertManager) cooldownFor(severity string) time.Duration {
	if severity == EventSeverityCritical {
		return m.cooldownCritical
	}
	return m.cooldownWarning
}

// deliver 做冷却判断、投递、记账。它只由 worker goroutine 调用，
// 因此对 state 的读改写不需要额外加锁以外的同步。
func (m *AlertManager) deliver(alert Alert) {
	now := m.now()
	if alert.OccurredAt.IsZero() {
		alert.OccurredAt = now
	}
	key := cooldownKey(alert.Severity, alert.Kind, alert.Symbol)

	m.mu.Lock()
	state := m.state[key]
	if state == nil {
		state = &alertState{}
		m.state[key] = state
	}
	elapsed := now.Sub(state.lastSent)
	window := m.cooldownFor(alert.Severity)
	escalated := severityRank(alert.Severity) > severityRank(state.lastSeverity)
	// 冷却期内的同类告警不重复打扰，但计数要留住：
	// "这个问题已经连续发生 12 次"比"又发生了一次"有用得多。
	if !state.lastSent.IsZero() && elapsed < window && !escalated {
		state.merged++
		m.merged.Add(1)
		m.mu.Unlock()
		m.record(alert, "", AlertStatusSuppressed, nil)
		return
	}
	alert.Merged = state.merged
	if !state.lastSent.IsZero() && elapsed >= window && state.merged > 0 {
		// 冷却到期后第一次重新发声，标记为"仍在持续"，
		// 让收件人知道这不是新问题，不用从头排查。
		alert.Summary = true
	}
	state.merged = 0
	state.lastSent = now
	state.lastSeverity = alert.Severity
	m.mu.Unlock()

	if len(m.notifiers) == 0 {
		m.record(alert, "", AlertStatusSuppressed, nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failures := make([]string, 0, len(m.notifiers))
	for _, notifier := range m.notifiers {
		if err := notifier.Notify(ctx, alert); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", notifier.Name(), err))
			m.record(alert, notifier.Name(), AlertStatusFailed, err)
			continue
		}
		m.record(alert, notifier.Name(), AlertStatusSent, nil)
	}
	if len(failures) > 0 {
		m.failed.Add(1)
		m.logger.Printf("event=alert_delivery_failed kind=%s err=%s", alert.Kind, strings.Join(failures, "; "))
		// 全部出口都失败时，把冷却状态整个退回：
		// 没送达就等于没发过，下一个同类告警应当立刻重试，
		// 否则接下来一整个冷却窗口内，人都不会知道告警其实没送到。
		if len(failures) == len(m.notifiers) {
			m.mu.Lock()
			if st := m.state[key]; st != nil {
				st.lastSent = time.Time{}
				st.lastSeverity = ""
			}
			m.mu.Unlock()
		}
		return
	}
	m.sent.Add(1)
}

// record 把投递结果落库。落库失败只打日志：
// 告警已经发出去了，记账失败不该反过来影响"已送达"这个事实。
func (m *AlertManager) record(alert Alert, channel, status string, cause error) {
	if m.store == nil {
		return
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.store.AppendAlert(ctx, Record{
		AccountID: alert.AccountID,
		Severity:  alert.Severity,
		Kind:      alert.Kind,
		Symbol:    alert.Symbol,
		Message:   alert.Message,
		Channel:   channel,
		Status:    status,
		Error:     message,
		Merged:    alert.Merged,
		CreatedAt: alert.OccurredAt,
	}); err != nil {
		m.logger.Printf("event=alert_record_failed kind=%s err=%v", alert.Kind, err)
	}
}

// Test 主动发一条测试告警，用于验证出口配置是否可用。
//
// 没有这个方法时，人配好 webhook 只能等真出事才知道配错了，
// 而"配错了"这件事本身恰恰是最该被立刻发现的。
func (m *AlertManager) Test(ctx context.Context) ([]string, error) {
	if len(m.notifiers) == 0 {
		return nil, fmt.Errorf("未配置任何告警出口")
	}
	alert := Alert{
		AccountID:  DefaultAccountID,
		Severity:   EventSeverityInfo,
		Kind:       "alert.test",
		Message:    "这是一条测试告警：收到说明告警通道配置正确。",
		OccurredAt: m.now(),
	}
	names := make([]string, 0, len(m.notifiers))
	failures := make([]string, 0)
	for _, notifier := range m.notifiers {
		if err := notifier.Notify(ctx, alert); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", notifier.Name(), err))
			continue
		}
		names = append(names, notifier.Name())
	}
	if len(failures) > 0 {
		return names, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return names, nil
}

// Stats 返回告警系统自身的计数。
// 一个从不报错的告警系统和一个从没收到过告警的系统，从外表看是一样的；
// 只有把计数暴露出来，才能区分"没事发生"和"坏了"。
func (m *AlertManager) Stats() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"sent":    m.sent.Load(),
		"failed":  m.failed.Load(),
		"dropped": m.dropped.Load(),
		"merged":  m.merged.Load(),
	}
}

// Channels 返回已配置的出口名，供健康检查与页面展示。
func (m *AlertManager) Channels() []string {
	if m == nil {
		return nil
	}
	names := make([]string, 0, len(m.notifiers))
	for _, notifier := range m.notifiers {
		names = append(names, notifier.Name())
	}
	return names
}
