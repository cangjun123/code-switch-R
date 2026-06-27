package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultNotificationWebhookMethod  = http.MethodPost
	defaultNotificationWebhookHeaders = "{\n  \"Content-Type\": \"application/json\"\n}"
	defaultNotificationWebhookBody    = "{\n  \"message\": \"{content}\",\n  \"title\": \"{title}\"\n}"
	notificationWebhookTimeout        = 10 * time.Second
)

// NotificationService sends provider switch/blacklist notifications.
type NotificationService struct {
	appSettings    *AppSettingsService
	emitter        EventEmitter
	mu             sync.RWMutex
	lastNotifyTime time.Time
	minInterval    time.Duration
	httpClient     *http.Client
}

// SwitchNotification 切换通知的详细信息
type SwitchNotification struct {
	FromProvider string // 原供应商
	ToProvider   string // 新供应商
	Reason       string // 切换原因
	Platform     string // 平台：claude/codex/gemini
}

// NewNotificationService 创建通知服务
func NewNotificationService(appSettings *AppSettingsService) *NotificationService {
	return &NotificationService{
		appSettings: appSettings,
		minInterval: 3 * time.Second,
		httpClient:  &http.Client{Timeout: notificationWebhookTimeout},
	}
}

// SetEventEmitter 设置事件发送器（用于转发到 SSE/web 前端）。
func (ns *NotificationService) SetEventEmitter(emitter EventEmitter) {
	ns.emitter = emitter
}

// EnableDesktopNotifications is kept for compatibility. Web mode uses webhook notifications.
func (ns *NotificationService) EnableDesktopNotifications(_ bool) {}

// isEnabled 检查通知是否开启
func (ns *NotificationService) isEnabled() bool {
	if ns.appSettings == nil {
		return true
	}
	settings, err := ns.appSettings.GetAppSettings()
	if err != nil {
		return true
	}
	return settings.EnableSwitchNotify
}

// NotifyProviderSwitch 发送供应商切换通知（异步，不阻塞主流程）
func (ns *NotificationService) NotifyProviderSwitch(info SwitchNotification) {
	if !ns.isEnabled() {
		return
	}

	ns.mu.Lock()
	lastTime := ns.lastNotifyTime
	ns.mu.Unlock()

	if time.Since(lastTime) < ns.minInterval {
		log.Printf("[Notification] 通知被节流，距上次通知仅 %v", time.Since(lastTime))
		return
	}

	go ns.sendSwitchNotification(info)
}

// sendSwitchNotification 实际发送切换通知的内部方法
func (ns *NotificationService) sendSwitchNotification(info SwitchNotification) {
	ns.mu.Lock()
	ns.lastNotifyTime = time.Now()
	ns.mu.Unlock()

	title := "Code Switch"
	content := fmt.Sprintf("已切换到 %s", info.ToProvider)

	ns.emitSwitchEvent(info)
	if err := ns.sendWebhookNotification(title, content); err != nil {
		log.Printf("[Notification] 发送 webhook 通知失败: %v", err)
	} else {
		log.Printf("[Notification] 已处理切换通知: %s → %s", info.FromProvider, info.ToProvider)
	}
}

// emitSwitchEvent 发送切换事件到前端
func (ns *NotificationService) emitSwitchEvent(info SwitchNotification) {
	if ns.emitter == nil {
		return
	}
	ns.emitter.Emit("provider:switched", map[string]interface{}{
		"platform":     info.Platform,
		"fromProvider": info.FromProvider,
		"toProvider":   info.ToProvider,
		"reason":       info.Reason,
		"timestamp":    time.Now().UnixMilli(),
	})
}

// NotifyProviderBlacklisted 发送供应商被拉黑通知
func (ns *NotificationService) NotifyProviderBlacklisted(platform, providerName string, level int, durationMinutes int) {
	if !ns.isEnabled() {
		return
	}

	go func() {
		title := "Code Switch"
		content := fmt.Sprintf("%s 已拉黑 %d 分钟", providerName, durationMinutes)

		ns.emitBlacklistEvent(platform, providerName, level, durationMinutes)
		if err := ns.sendWebhookNotification(title, content); err != nil {
			log.Printf("[Notification] 发送拉黑 webhook 通知失败: %v", err)
		} else {
			log.Printf("[Notification] 已处理拉黑通知: %s (L%d, %d分钟)", providerName, level, durationMinutes)
		}
	}()
}

func (ns *NotificationService) TestWebhookNotification() error {
	if ns == nil || ns.appSettings == nil {
		return fmt.Errorf("通知服务未初始化")
	}

	settings, err := ns.appSettings.GetAppSettings()
	if err != nil {
		return fmt.Errorf("读取通知配置失败: %w", err)
	}
	if strings.TrimSpace(settings.NotificationWebhookURL) == "" {
		return fmt.Errorf("请先填写 Webhook URL")
	}

	return ns.sendWebhookNotification("Code Switch 测试通知", "这是一条来自 Code Switch 的测试通知")
}

// emitBlacklistEvent 发送拉黑事件到前端
func (ns *NotificationService) emitBlacklistEvent(platform, providerName string, level, durationMinutes int) {
	if ns.emitter == nil {
		return
	}
	ns.emitter.Emit("provider:blacklisted", map[string]interface{}{
		"platform":        platform,
		"providerName":    providerName,
		"level":           level,
		"durationMinutes": durationMinutes,
		"timestamp":       time.Now().UnixMilli(),
	})
}

func (ns *NotificationService) sendWebhookNotification(title, content string) error {
	if ns.appSettings == nil {
		return nil
	}

	settings, err := ns.appSettings.GetAppSettings()
	if err != nil {
		return fmt.Errorf("读取通知配置失败: %w", err)
	}
	webhookURL := strings.TrimSpace(settings.NotificationWebhookURL)
	if webhookURL == "" {
		return nil
	}

	method := normalizeNotificationWebhookMethod(settings.NotificationWebhookMethod)
	headers, err := parseNotificationWebhookHeaders(settings.NotificationWebhookHeaders)
	if err != nil {
		return err
	}

	bodyTemplate := settings.NotificationWebhookBody
	if strings.TrimSpace(bodyTemplate) == "" {
		bodyTemplate = defaultNotificationWebhookBody
	}
	body := renderNotificationBody(bodyTemplate, title, content)

	ctx, cancel := context.WithTimeout(context.Background(), notificationWebhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, webhookURL, bytes.NewBufferString(body))
	if err != nil {
		return fmt.Errorf("创建 webhook 请求失败: %w", err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := ns.httpClient
	if client == nil {
		client = &http.Client{Timeout: notificationWebhookTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送 webhook 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("webhook 状态码 %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func normalizeNotificationWebhookMethod(method string) string {
	normalized := strings.ToUpper(strings.TrimSpace(method))
	switch normalized {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch:
		return normalized
	default:
		return defaultNotificationWebhookMethod
	}
}

func parseNotificationWebhookHeaders(raw string) (http.Header, error) {
	headers := http.Header{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return headers, nil
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// The UI asks for JSON, but this tolerant retry makes copy-pasted JS-style examples less brittle.
		normalized := strings.ReplaceAll(raw, "'", "\"")
		if retryErr := json.Unmarshal([]byte(normalized), &parsed); retryErr != nil {
			return nil, fmt.Errorf("解析 webhook headers JSON 失败: %w", err)
		}
	}

	for key, value := range parsed {
		headerKey := strings.TrimSpace(key)
		if headerKey == "" || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			headers.Set(headerKey, typed)
		case []any:
			for _, item := range typed {
				headers.Add(headerKey, fmt.Sprint(item))
			}
		default:
			headers.Set(headerKey, fmt.Sprint(typed))
		}
	}
	return headers, nil
}

func renderNotificationTemplate(template, title, content string) string {
	replacer := strings.NewReplacer(
		"{title}", title,
		"{content}", content,
	)
	return replacer.Replace(template)
}

func renderNotificationBody(template, title, content string) string {
	if parsed, ok := parseNotificationBodyTemplate(template); ok {
		rendered := renderNotificationValue(parsed, title, content)
		data, err := json.Marshal(rendered)
		if err == nil {
			return string(data)
		}
	}

	return renderNotificationTemplate(template, title, content)
}

func parseNotificationBodyTemplate(template string) (any, bool) {
	raw := strings.TrimSpace(template)
	if raw == "" {
		return "", true
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed, true
	}

	normalized := strings.ReplaceAll(raw, "'", "\"")
	if err := json.Unmarshal([]byte(normalized), &parsed); err == nil {
		return parsed, true
	}

	return nil, false
}

func renderNotificationValue(value any, title, content string) any {
	switch typed := value.(type) {
	case string:
		return renderNotificationTemplate(typed, title, content)
	case []any:
		rendered := make([]any, len(typed))
		for i, item := range typed {
			rendered[i] = renderNotificationValue(item, title, content)
		}
		return rendered
	case map[string]any:
		rendered := make(map[string]any, len(typed))
		for key, item := range typed {
			rendered[key] = renderNotificationValue(item, title, content)
		}
		return rendered
	default:
		return typed
	}
}
