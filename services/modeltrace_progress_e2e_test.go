package services

import (
	"encoding/json"
	"testing"
	"time"
)

// TestEmitProgressReachesSubscriber 验证进度事件经 EventHub 到达订阅者（SSE 同路径）
func TestEmitProgressReachesSubscriber(t *testing.T) {
	hub := NewEventHub()
	events, cancel := hub.Subscribe(32)
	defer cancel()

	svc := NewModelTraceService(nil)
	svc.SetEventEmitter(hub)

	start := time.Now()
	svc.emitProgress("mt-1-abc", 42, "gpt-5.4", "sending", 1, start, "测试进度")

	select {
	case event := <-events:
		if event.Name != "modeltrace:progress" {
			t.Fatalf("event name = %s", event.Name)
		}
		data, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		var payload ModelTraceProgressEvent
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("payload 解析失败: %v", err)
		}
		if payload.SessionID != "mt-1-abc" || payload.ProviderID != 42 ||
			payload.ExpectedModel != "gpt-5.4" || payload.Stage != "sending" ||
			payload.Attempt != 1 || payload.MaxAttempts != maxVerifyAttempts {
			t.Fatalf("payload 不符: %+v", payload)
		}
		t.Logf("payload OK: %+v", payload)
	case <-time.After(time.Second):
		t.Fatal("1 秒内未收到进度事件")
	}
}

// TestEmitterNilSafe emitter 未注入时 emitProgress 不应 panic
func TestEmitterNilSafe(t *testing.T) {
	svc := NewModelTraceService(nil)
	svc.emitProgress("mt", 1, "gpt-5.4", "sending", 1, time.Now(), "x")
}
