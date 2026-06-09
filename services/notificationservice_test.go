package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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
