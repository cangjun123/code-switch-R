package services

import (
	"testing"

	"github.com/daodao97/xgo/xdb"
)

// TestBlacklistServiceResetProviderBlacklist 验证改名清理：删除指定
// platform/providerName 的黑名单记录（供供应商改名后清理旧 name 状态使用）。
func TestBlacklistServiceResetProviderBlacklist(t *testing.T) {
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

	blacklistService := NewBlacklistService(NewSettingsService(), nil)

	// 造一条旧 name 的黑名单记录（直接写表，绕过阈值逻辑）
	if _, err := db.Exec(`
		INSERT INTO provider_blacklist (platform, provider_name, failure_count, blacklist_level, blacklisted_until)
		VALUES ('claude', 'OldName', 3, 2, datetime('now', '+1 hour'))
	`); err != nil {
		t.Fatalf("插入测试黑名单记录失败: %v", err)
	}
	// 另一条不相关记录，确保清理只命中目标
	if _, err := db.Exec(`
		INSERT INTO provider_blacklist (platform, provider_name, failure_count, blacklist_level)
		VALUES ('codex', 'Untouched', 1, 1)
	`); err != nil {
		t.Fatalf("插入无关黑名单记录失败: %v", err)
	}

	// 重置旧 name（改名清理）
	if err := blacklistService.ResetProviderBlacklist("claude", "OldName"); err != nil {
		t.Fatalf("ResetProviderBlacklist 失败: %v", err)
	}

	// 旧 name 记录应已删除
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_blacklist WHERE platform = 'claude' AND provider_name = 'OldName'`).Scan(&count); err != nil {
		t.Fatalf("查询旧 name 黑名单记录失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("期望旧 name 黑名单记录已删除，实际 count=%d", count)
	}

	// 不相关记录应保留
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_blacklist WHERE platform = 'codex' AND provider_name = 'Untouched'`).Scan(&count); err != nil {
		t.Fatalf("查询无关黑名单记录失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("期望无关黑名单记录保留，实际 count=%d", count)
	}

	// 对不存在的 name 调用应无副作用、不报错（幂等）
	if err := blacklistService.ResetProviderBlacklist("claude", "NeverExisted"); err != nil {
		t.Fatalf("对不存在的 name 调用应无错，实际: %v", err)
	}
}
