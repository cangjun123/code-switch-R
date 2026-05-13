package services

const (
	ProviderKindClaude   = "claude"
	ProviderKindCodex    = "codex"
	ProviderKindGemini   = "gemini"
	ProviderKindGPTImage = "gpt-image"
)

var providerServicePlatforms = []string{ProviderKindClaude, ProviderKindCodex, ProviderKindGPTImage}
