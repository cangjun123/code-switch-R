package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daodao97/xgo/xdb"
)

// setupNeverBlacklistTestEnv 构造带 ProviderService 注入的黑名单测试环境，
// 并返回可写入 provider 配置的 ProviderService。
func setupNeverBlacklistTestEnv(t *testing.T) (*BlacklistService, *ProviderService) {
	t.Helper()
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

	// 开启拉黑总开关（RecordFailure 的前置条件）
	if err := GlobalDBQueue.Exec(`
		INSERT INTO app_settings (key, value) VALUES ('enable_blacklist', 'true')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`); err != nil {
		t.Fatalf("开启拉黑开关失败: %v", err)
	}

	providerService := NewProviderService()
	blacklistService := NewBlacklistService(NewSettingsService(), nil)
	blacklistService.SetProviderService(providerService)

	return blacklistService, providerService
}

// TestNeverBlacklistRecordFailureSkipped 验证永不拉黑的 provider：
// 连续失败达到阈值也不会写入黑名单记录。
func TestNeverBlacklistRecordFailureSkipped(t *testing.T) {
	blacklistService, providerService := setupNeverBlacklistTestEnv(t)

	if err := providerService.SaveProviders("claude", []Provider{
		{Name: "Immune", APIURL: "https://immune.example.com", Enabled: true, NeverBlacklist: true},
	}); err != nil {
		t.Fatalf("保存 provider 失败: %v", err)
	}

	// 连续失败超过默认阈值（3 次）
	for i := 0; i < 5; i++ {
		if err := blacklistService.RecordFailure("claude", "Immune"); err != nil {
			t.Fatalf("RecordFailure 失败: %v", err)
		}
	}

	if blacklisted, _ := blacklistService.IsBlacklisted("claude", "Immune"); blacklisted {
		t.Fatal("永不拉黑的 provider 不应被拉黑")
	}

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("获取数据库连接失败: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_blacklist WHERE provider_name = 'Immune'`).Scan(&count); err != nil {
		t.Fatalf("查询黑名单记录失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("永不拉黑的 provider 不应产生黑名单记录，实际 count=%d", count)
	}
}

// TestNeverBlacklistIsBlacklistedAlwaysFalse 验证开启标志后，
// 已有的拉黑记录也立即视作未拉黑。
func TestNeverBlacklistIsBlacklistedAlwaysFalse(t *testing.T) {
	blacklistService, providerService := setupNeverBlacklistTestEnv(t)

	// 先以普通 provider 身份触发拉黑
	if err := providerService.SaveProviders("claude", []Provider{
		{Name: "FallenBack", APIURL: "https://fallback.example.com", Enabled: true},
	}); err != nil {
		t.Fatalf("保存 provider 失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := blacklistService.RecordFailure("claude", "FallenBack"); err != nil {
			t.Fatalf("RecordFailure 失败: %v", err)
		}
	}
	if blacklisted, _ := blacklistService.IsBlacklisted("claude", "FallenBack"); !blacklisted {
		t.Fatal("前置条件失败：普通 provider 应已被拉黑")
	}

	// 开启永不拉黑后应立即免疫
	providers, err := providerService.LoadProviders("claude")
	if err != nil {
		t.Fatalf("加载 provider 失败: %v", err)
	}
	providers[0].NeverBlacklist = true
	if err := providerService.SaveProviders("claude", providers); err != nil {
		t.Fatalf("保存 provider 失败: %v", err)
	}

	if blacklisted, until := blacklistService.IsBlacklisted("claude", "FallenBack"); blacklisted {
		t.Fatalf("开启永不拉黑后应立即免疫，实际仍被拉黑（until=%v）", until)
	}
}

// TestNeverBlacklistRecordSuccessSkipped 验证 RecordSuccess 对免疫 provider 是 no-op
// （不产生数据库写入，也不报错）。
func TestNeverBlacklistRecordSuccessSkipped(t *testing.T) {
	blacklistService, providerService := setupNeverBlacklistTestEnv(t)

	if err := providerService.SaveProviders("claude", []Provider{
		{Name: "Immune", APIURL: "https://immune.example.com", Enabled: true, NeverBlacklist: true},
	}); err != nil {
		t.Fatalf("保存 provider 失败: %v", err)
	}

	if err := blacklistService.RecordSuccess("claude", "Immune"); err != nil {
		t.Fatalf("RecordSuccess 失败: %v", err)
	}

	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("获取数据库连接失败: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_blacklist WHERE provider_name = 'Immune'`).Scan(&count); err != nil {
		t.Fatalf("查询黑名单记录失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("RecordSuccess 不应为免疫 provider 产生黑名单记录，实际 count=%d", count)
	}
}

// TestNeverBlacklistWithoutProviderService 验证未注入 ProviderService 时
// 保持原有拉黑行为（安全兜底：不免疫）。
func TestNeverBlacklistWithoutProviderService(t *testing.T) {
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
	if err := GlobalDBQueue.Exec(`
		INSERT INTO app_settings (key, value) VALUES ('enable_blacklist', 'true')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`); err != nil {
		t.Fatalf("开启拉黑开关失败: %v", err)
	}

	// 不注入 ProviderService
	blacklistService := NewBlacklistService(NewSettingsService(), nil)

	for i := 0; i < 5; i++ {
		if err := blacklistService.RecordFailure("claude", "NoInjection"); err != nil {
			t.Fatalf("RecordFailure 失败: %v", err)
		}
	}
	if blacklisted, _ := blacklistService.IsBlacklisted("claude", "NoInjection"); !blacklisted {
		t.Fatal("未注入 ProviderService 时应保持原有拉黑行为（provider 应被拉黑）")
	}
}

// TestNeverBlacklistFieldRoundTrip 验证 neverBlacklist 字段在配置文件中正确持久化与读取。
func TestNeverBlacklistFieldRoundTrip(t *testing.T) {
	setupRelayTestEnv(t)

	providerService := NewProviderService()
	if err := providerService.SaveProviders("codex", []Provider{
		{Name: "Guarded", APIURL: "https://guarded.example.com", Enabled: true, NeverBlacklist: true},
		{Name: "Normal", APIURL: "https://normal.example.com", Enabled: true},
	}); err != nil {
		t.Fatalf("保存 provider 失败: %v", err)
	}

	if !providerService.IsNeverBlacklist("codex", "Guarded") {
		t.Fatal("Guarded 应被识别为永不拉黑")
	}
	if providerService.IsNeverBlacklist("codex", "Normal") {
		t.Fatal("Normal 不应被识别为永不拉黑")
	}
	// 查不到的 name 应返回 false（安全兜底）
	if providerService.IsNeverBlacklist("codex", "Ghost") {
		t.Fatal("不存在的 provider 不应被识别为永不拉黑")
	}

	// 直接读配置文件验证 JSON 字段名
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("获取 home 目录失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".code-switch", "codex.json"))
	if err != nil {
		t.Fatalf("读取配置文件失败: %v", err)
	}
	if !containsBytes(data, `"neverBlacklist": true`) {
		t.Fatalf("配置文件应包含 neverBlacklist: true 字段，实际内容: %s", data)
	}
}

func containsBytes(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}
