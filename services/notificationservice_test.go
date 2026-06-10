package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func newTestAppSettingsService(t *testing.T) *AppSettingsService {
	t.Helper()
	return &AppSettingsService{path: filepath.Join(t.TempDir(), "app.json")}
}

func TestNotificationWebhookSkipsEmptyURL(t *testing.T) {
	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = ""
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	if err := notificationService.sendWebhookNotification("Code Switch", "已切换到 eto"); err != nil {
		t.Fatalf("空 URL 应该静默跳过，收到错误: %v", err)
	}
}

func TestNotificationWebhookSendsNtfyStylePayload(t *testing.T) {
	type receivedRequest struct {
		method        string
		path          string
		authorization string
		values        []string
		body          map[string]any
	}
	received := make(chan receivedRequest, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook body 不是有效 JSON: %v", err)
		}
		received <- receivedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			values:        r.Header.Values("X-Values"),
			body:          body,
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = server.URL + "/notify"
	settings.NotificationWebhookMethod = "post"
	settings.NotificationWebhookHeaders = "{'Content-Type': 'application/json', 'Authorization': 'Basic token', 'X-Values': ['a', 'b']}"
	settings.NotificationWebhookBody = "{'topic': 'alex-starrail', 'message': '{content}', 'title': '{title}', 'priority': 4, 'tags': ['white_check_mark']}"
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	if err := notificationService.sendWebhookNotification("Code Switch", `已切换到 "eto"`); err != nil {
		t.Fatalf("发送 webhook 失败: %v", err)
	}

	got := <-received
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/notify" {
		t.Fatalf("path = %s, want /notify", got.path)
	}
	if got.authorization != "Basic token" {
		t.Fatalf("Authorization = %q, want Basic token", got.authorization)
	}
	if strings.Join(got.values, ",") != "a,b" {
		t.Fatalf("X-Values = %v, want [a b]", got.values)
	}
	if got.body["topic"] != "alex-starrail" {
		t.Fatalf("topic = %v, want alex-starrail", got.body["topic"])
	}
	if got.body["message"] != `已切换到 "eto"` {
		t.Fatalf("message = %v, want quoted provider content", got.body["message"])
	}
	if got.body["title"] != "Code Switch" {
		t.Fatalf("title = %v, want Code Switch", got.body["title"])
	}
	if got.body["priority"] != float64(4) {
		t.Fatalf("priority = %v, want 4", got.body["priority"])
	}
}

func TestNotificationWebhookTestRequiresURL(t *testing.T) {
	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = ""
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	err := notificationService.TestWebhookNotification()
	if err == nil {
		t.Fatal("空 Webhook URL 应该返回提示错误")
	}
	if !strings.Contains(err.Error(), "Webhook URL") {
		t.Fatalf("错误信息 = %q, want Webhook URL hint", err.Error())
	}
}

func TestNotificationWebhookTestSendsPayload(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook body 不是有效 JSON: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = server.URL
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	if err := notificationService.TestWebhookNotification(); err != nil {
		t.Fatalf("测试通知发送失败: %v", err)
	}

	got := <-received
	if got["title"] != "Code Switch 测试通知" {
		t.Fatalf("title = %v, want test title", got["title"])
	}
	if got["message"] != "这是一条来自 Code Switch 的测试通知" {
		t.Fatalf("message = %v, want test message", got["message"])
	}
}

func TestNotificationWebhookReportsNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()

	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = server.URL
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	err := notificationService.sendWebhookNotification("Code Switch", "已切换到 eto")
	if err == nil {
		t.Fatal("非 2xx webhook 响应应该返回错误")
	}
	if !strings.Contains(err.Error(), "webhook 状态码 502") {
		t.Fatalf("错误信息 = %q, want status code context", err.Error())
	}
}

func TestFixedModeBlacklistSendsWebhookNotification(t *testing.T) {
	setupRelayTestEnv(t)
	if GlobalDBQueue == nil {
		if err := InitGlobalDBQueue(); err != nil {
			t.Fatalf("初始化测试写入队列失败: %v", err)
		}
	}

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("获取数据库连接失败: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM provider_blacklist`); err != nil {
		t.Fatalf("清理黑名单表失败: %v", err)
	}

	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook body 不是有效 JSON: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	appSettings := newTestAppSettingsService(t)
	settings := appSettings.defaultSettings()
	settings.NotificationWebhookURL = server.URL
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatalf("保存测试配置失败: %v", err)
	}

	settingsService := NewSettingsService()
	if err := settingsService.UpdateBlacklistEnabled(true); err != nil {
		t.Fatalf("开启拉黑失败: %v", err)
	}
	if err := settingsService.SetLevelBlacklistEnabled(false); err != nil {
		t.Fatalf("关闭等级拉黑失败: %v", err)
	}
	if err := settingsService.UpdateBlacklistSettings(2, 5); err != nil {
		t.Fatalf("更新固定拉黑配置失败: %v", err)
	}

	notificationService := NewNotificationService(appSettings)
	blacklistService := NewBlacklistService(settingsService, notificationService)

	if err := blacklistService.RecordFailure("claude", "fixed-webhook-provider"); err != nil {
		t.Fatalf("记录首次失败失败: %v", err)
	}
	if err := blacklistService.RecordFailure("claude", "fixed-webhook-provider"); err != nil {
		t.Fatalf("触发固定拉黑失败: %v", err)
	}

	select {
	case got := <-received:
		if got["title"] != "Code Switch" {
			t.Fatalf("title = %v, want Code Switch", got["title"])
		}
		if got["message"] != "fixed-webhook-provider 已拉黑 5 分钟" {
			t.Fatalf("message = %v, want fixed-mode blacklist message", got["message"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("固定拉黑模式未发送 webhook 通知")
	}
}

func TestRenderNotificationBodyEscapesJSONPlaceholders(t *testing.T) {
	rendered := renderNotificationBody(
		"{'message': '{content}', 'title': '{title}', 'tags': ['white_check_mark']}",
		`Code "Switch"`,
		`已切换到 "eto"`,
	)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(rendered), &parsed); err != nil {
		t.Fatalf("渲染结果不是有效 JSON: %v\n%s", err, rendered)
	}
	if parsed["message"] != `已切换到 "eto"` {
		t.Fatalf("message = %v, want escaped content", parsed["message"])
	}
	if parsed["title"] != `Code "Switch"` {
		t.Fatalf("title = %v, want escaped title", parsed["title"])
	}
}

func TestNormalizeNotificationWebhookMethod(t *testing.T) {
	if got := normalizeNotificationWebhookMethod(" patch "); got != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", got)
	}
	if got := normalizeNotificationWebhookMethod("delete"); got != http.MethodPost {
		t.Fatalf("unsupported method = %s, want POST", got)
	}
}
