package services

import "strings"

const (
	ProviderKindClaude   = "claude"
	ProviderKindCodex    = "codex"
	ProviderKindGemini   = "gemini"
	ProviderKindGPTImage = "gpt-image"
)

// providerServicePlatforms 可通过 ProviderService.LoadProviders 读取的平台。
// Gemini 由 GeminiService 存储，经 ProviderService.SetGeminiSource 适配后同样可读。
var providerServicePlatforms = []string{ProviderKindClaude, ProviderKindCodex, ProviderKindGPTImage, ProviderKindGemini}

// RelayPlatformForKind 把 ProviderService 使用的文件 kind 归一化为 relay/数据库里
// 实际存储的 platform 值。黑名单/请求日志/健康历史都以 platform 作为关联键，
// 而 claude 的文件 kind（"claude-code"）与 DB platform（"claude"）不一致，需要这里统一。
// 自定义 CLI 的 kind 形如 "custom:{toolId}"，其 platform 值与 kind 相同，原样返回。
func RelayPlatformForKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "claude", "claude-code", "claude_code":
		return ProviderKindClaude
	case "codex":
		return ProviderKindCodex
	case "gpt-image", "gpt_image", "gptimage":
		return ProviderKindGPTImage
	default:
		return strings.TrimSpace(kind)
	}
}
