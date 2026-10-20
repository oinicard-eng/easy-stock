package trading

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingNotifier 记录收到的告警，用于断言"到底发了几条"。
type recordingNotifier struct {
	name   string
	mu     sync.Mutex
	alerts []Alert
	err    error
}

func (r *recordingNotifier) Name() string { return r.name }

func (r *recordingNotifier) Notify(ctx context.Context, alert Alert) error {
	if r.err != nil {
		return r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, alert)
	return nil
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.alerts)
}

func (r *recordingNotifier) last() Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.alerts) == 0 {
		return Alert{}
	}
	return r.alerts[len(r.alerts)-1]
}

func newAlertTestManager(t *testing.T, notifiers ...Notifier) (*AlertManager, *Store) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := NewAlertManager(store, nil, AlertConfig{
		CooldownCritical: 10 * time.Minute,
		CooldownWarning:  time.Hour,
	}, notifiers...)
	return manager, store
}

func testAlert(severity, kind, symbol, message string) Alert {
	return Alert{
		AccountID:  DefaultAccountID,
		Severity:   severity,
		Kind:       kind,
		Symbol:     symbol,
		Message:    message,
		OccurredAt: time.Now().UTC(),
	}
}

// 冷却期内同一类告警只发一次，其余全部合并计数。
// 这是告警系统最基本的要求：账本漂移每 5 分钟被对账一次，
// 不去重的话人会在半小时内收到 6 条一模一样的消息，然后开始忽略它们。
func TestAlertCooldownMergesRepeatedAlerts(t *testing.T) {
	notifier := &recordingNotifier{name: "fake"}
	manager, store := newAlertTestManager(t, notifier)

	for i := 0; i < 5; i++ {
		manager.deliver(testAlert(EventSeverityWarning, EventLedgerDrift, "", "账本对不上"))
	}
	if got := notifier.count(); got != 1 {
		t.Fatalf("冷却期内应只发 1 条，实际 %d 条", got)
	}

	records, err := store.ListAlerts(context.Background(), DefaultAccountID, "", 50)
	if err != nil {
		t.Fatalf("读取告警记录失败: %v", err)
	}
	var sent, suppressed int
	for _, record := range records {
		switch record.Status {
		case AlertStatusSent:
			sent++
		case AlertStatusSuppressed:
			suppressed++
		}
	}
	if sent != 1 || suppressed != 4 {
		t.Fatalf("应记录 1 条 sent、4 条 suppressed，实际 sent=%d suppressed=%d", sent, suppressed)
	}
}

// 冷却期结束后重新发声，要带上"期间合并了多少条"和"仍在持续"两个标记。
// 没有这两个标记，收件人会以为这是一次新的偶发故障，从头开始排查。
func TestAlertResumesAfterCooldownWithMergedCount(t *testing.T) {
	notifier := &recordingNotifier{name: "fake"}
	manager, _ := newAlertTestManager(t, notifier)
	base := time.Now()
	manager.now = func() time.Time { return base }

	manager.deliver(testAlert(EventSeverityWarning, EventBreakerArmed, "", "挂单积压"))
	manager.deliver(testAlert(EventSeverityWarning, EventBreakerArmed, "", "挂单积压"))
	manager.deliver(testAlert(EventSeverityWarning, EventBreakerArmed, "", "挂单积压"))

	// 越过 warning 冷却窗口（1 小时）。
	manager.now = func() time.Time { return base.Add(61 * time.Minute) }
	manager.deliver(testAlert(EventSeverityWarning, EventBreakerArmed, "", "挂单积压"))

	if got := notifier.count(); got != 2 {
		t.Fatalf("冷却结束后应补发一条，期望 2 条，实际 %d 条", got)
	}
	last := notifier.last()
	if last.Merged != 2 {
		t.Fatalf("补发时应带上合并数 2，实际 %d", last.Merged)
	}
	if !last.Summary {
		t.Fatal("冷却后补发的告警应标记为仍在持续")
	}
}

// 严重度升高必须突破冷却。积压是警告，漂移是严重，
// 用同一套冷却会把"问题升级了"这件事一起压掉。
func TestAlertEscalationBypassesCooldown(t *testing.T) {
	notifier := &recordingNotifier{name: "fake"}
	manager, _ := newAlertTestManager(t, notifier)
	base := time.Now()
	manager.now = func() time.Time { return base }

	manager.deliver(testAlert(EventSeverityWarning, EventLedgerDrift, "", "账本轻微不一致"))
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))

	if got := notifier.count(); got != 2 {
		t.Fatalf("升级应突破冷却、连续 major 应被冷却，期望 2 条，实际 %d 条", got)
	}
	if last := notifier.last(); last.Severity != EventSeverityCritical {
		t.Fatalf("第二条应为 critical，实际 %s", last.Severity)
	}
}

// 出口全失败时不占用冷却：没送达等于没发过，下一个同类告警要立刻重试。
func TestAlertFailureDoesNotConsumeCooldown(t *testing.T) {
	failing := &recordingNotifier{name: "broken", err: errors.New("连接超时")}
	manager, store := newAlertTestManager(t, failing)

	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))

	if got := failing.count(); got != 0 {
		t.Fatalf("失败的出口不应记录成功，实际 %d", got)
	}
	records, err := store.ListAlerts(context.Background(), DefaultAccountID, AlertStatusFailed, 50)
	if err != nil {
		t.Fatalf("读取告警记录失败: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("两次失败都应落库，实际 %d 条", len(records))
	}
	if records[0].Error == "" {
		t.Fatal("失败的告警必须记录失败原因")
	}
}

// 一个出口成功、一个失败，整体算送达成功，不重置冷却——
// 否则会为了一个配错的次要出口，把已经送达的主通道也一起重发。
func TestAlertPartialFailureKeepsCooldown(t *testing.T) {
	ok := &recordingNotifier{name: "ok"}
	broken := &recordingNotifier{name: "broken", err: errors.New("dns 解析失败")}
	manager, _ := newAlertTestManager(t, ok, broken)

	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))

	if got := ok.count(); got != 1 {
		t.Fatalf("成功出口应只收到 1 条，实际 %d 条", got)
	}
	if got := broken.count(); got != 0 {
		t.Fatalf("失败出口不应被算作成功，实际 %d 条", got)
	}
}

// 文件出口是兜底：没有配 webhook 时，告警必须留下本地痕迹。
func TestFileNotifierAppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts", "trading-alerts.log")
	notifier := NewFileNotifier(path)
	if notifier == nil {
		t.Fatal("给定路径时应返回可用的文件出口")
	}
	manager, _ := newAlertTestManager(t, notifier)
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "600519.SH", "账本对不上"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("告警文件应被创建: %v", err)
	}
	var line map[string]any
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatalf("告警文件应为合法 JSON 行: %v\n%s", err, raw)
	}
	if line["kind"] != EventLedgerDrift {
		t.Fatalf("写入的 kind 不正确: %v", line["kind"])
	}
	if line["severity"] != EventSeverityCritical {
		t.Fatalf("写入的 severity 不正确: %v", line["severity"])
	}
}

// 空路径返回 nil 表示不启用，避免调用方到处判断空字符串。
func TestFileNotifierDisabledWithEmptyPath(t *testing.T) {
	if notifier := NewFileNotifier("   "); notifier != nil {
		t.Fatal("空路径应返回 nil")
	}
}

// webhook 出口按企微文本消息格式投递，且请求体要能被对端直接消费。
func TestWebhookNotifierSendsWeComTextMessage(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := NewWebhookNotifier("webhook", server.URL, AlertFormatWeCom)
	alert := testAlert(EventSeverityCritical, EventLedgerDrift, "600519.SH", "账本对不上")
	if err := notifier.Notify(context.Background(), alert); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if received["msgtype"] != "text" {
		t.Fatalf("企微消息类型应为 text，实际 %v", received["msgtype"])
	}
	text, _ := received["text"].(map[string]any)
	content, _ := text["content"].(string)
	if content == "" {
		t.Fatal("消息内容不能为空")
	}
	if !containsAll(content, "600519.SH", "账本对不上") {
		t.Fatalf("消息应包含标的与原因，实际：%s", content)
	}
}

// 对端返回非 2xx 必须算失败，否则"配错 token 但返回 200 页面"这类情况会被静默吞掉。
func TestWebhookNotifierTreatsNon2xxAsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":40001}`))
	}))
	defer server.Close()

	notifier := NewWebhookNotifier("webhook", server.URL, AlertFormatJSON)
	err := notifier.Notify(context.Background(), testAlert(EventSeverityWarning, EventBreakerArmed, "", "挂单积压"))
	if err == nil {
		t.Fatal("非 2xx 响应必须返回错误")
	}
	if !containsAll(err.Error(), "401", "40001") {
		t.Fatalf("错误信息应带状态码与对端返回内容，实际：%v", err)
	}
}

// 只有 warning 以上才外发；恢复类事件虽然允许是 info，也要发出去。
func TestShouldAlertRules(t *testing.T) {
	cases := []struct {
		event Event
		want  bool
	}{
		{Event{Severity: EventSeverityCritical, Kind: EventLedgerDrift}, true},
		{Event{Severity: EventSeverityWarning, Kind: EventBreakerArmed}, true},
		{Event{Severity: EventSeverityInfo, Kind: "order.filled"}, false},
		{Event{Severity: EventSeverityInfo, Kind: EventAutopilotRecovered}, true},
		{Event{Severity: EventSeverityInfo, Kind: EventLedgerDriftHealed}, true},
	}
	for _, tc := range cases {
		if got := shouldAlert(tc.event); got != tc.want {
			t.Fatalf("kind=%s severity=%s 期望 %v，实际 %v", tc.event.Kind, tc.event.Severity, tc.want, got)
		}
	}
}

// 事件写入必须触达钩子。挂在 Store 上而不是某一个调用点，
// 是因为自愈、作废、回报落账都直接写事件，漏掉任何一个都会漏掉最该告警的那条。
func TestStoreEventSinkReceivesAppendedEvents(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer store.Close()

	var got []Event
	store.SetEventSink(func(event Event) { got = append(got, event) })
	if err := store.AppendEvent(context.Background(), Event{
		AccountID: DefaultAccountID,
		Severity:  EventSeverityCritical,
		Kind:      EventLedgerDrift,
		Message:   "账本对不上",
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("钩子应收到 1 条事件，实际 %d 条", len(got))
	}
	if got[0].Kind != EventLedgerDrift {
		t.Fatalf("钩子收到的事件类型不正确: %s", got[0].Kind)
	}

	// 摘除后不应再收到。
	store.SetEventSink(nil)
	if err := store.AppendEvent(context.Background(), Event{
		AccountID: DefaultAccountID, Severity: EventSeverityWarning, Kind: EventBreakerArmed, Message: "积压",
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("摘除钩子后不应再收到事件，实际 %d 条", len(got))
	}
}

// 队列投递要能真正跑通：Submit 入队，worker 发送并落库。
func TestAlertManagerDeliversThroughQueue(t *testing.T) {
	notifier := &recordingNotifier{name: "fake"}
	manager, store := newAlertTestManager(t, notifier)
	manager.Start()
	defer manager.Stop()

	manager.Submit(Event{
		AccountID: DefaultAccountID,
		Severity:  EventSeverityCritical,
		Kind:      EventLedgerDrift,
		Message:   "账本对不上",
		CreatedAt: time.Now().UTC(),
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if notifier.count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if notifier.count() != 1 {
		t.Fatalf("队列应投递 1 条告警，实际 %d 条", notifier.count())
	}
	records, err := store.ListAlerts(context.Background(), DefaultAccountID, AlertStatusSent, 10)
	if err != nil {
		t.Fatalf("读取告警记录失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("应落库 1 条已送达记录，实际 %d 条", len(records))
	}
}

// 没有配置任何出口时，告警仍然要落库：该发的东西不能因为"没配通道"就消失。
func TestAlertWithoutNotifierStillRecords(t *testing.T) {
	manager, store := newAlertTestManager(t)
	manager.deliver(testAlert(EventSeverityCritical, EventLedgerDrift, "", "账本对不上"))

	records, err := store.ListAlerts(context.Background(), DefaultAccountID, "", 10)
	if err != nil {
		t.Fatalf("读取告警记录失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("无出口时也应落库 1 条，实际 %d 条", len(records))
	}
	if records[0].Status != AlertStatusSuppressed {
		t.Fatalf("无出口时应标记为 suppressed，实际 %s", records[0].Status)
	}
}

// 告警文本要能独立读懂：标题、时间、原因、类型四样齐全。
func TestAlertTextIsSelfContained(t *testing.T) {
	alert := testAlert(EventSeverityCritical, EventLedgerDrift, "600519.SH", "资金冻结漂移 12.30 元")
	alert.Merged = 3
	text := alert.Text()
	if !containsAll(text, "严重", "600519.SH", "资金冻结漂移 12.30 元", "另有 3 条", EventLedgerDrift) {
		t.Fatalf("告警文本缺少必要信息：\n%s", text)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
