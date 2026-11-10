package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"codeswitch/services"

	"github.com/gin-gonic/gin"
)

const maxProviderKeyNameRunes = 128

type providerKeyItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MaskedKey  string `json:"maskedKey"`
	Enabled    bool   `json:"enabled"`
	Kind       string `json:"kind"`
	ProviderID int64  `json:"providerId"`
	// ProviderRef identifies Gemini providers, whose IDs are strings.
	ProviderRef string `json:"providerRef,omitempty"`
}

type providerKeyEnsureRequest struct {
	Kind        string `json:"kind"`
	ProviderID  *int64 `json:"providerId"`
	ProviderRef string `json:"providerRef"`
}

type providerKeyEnsureResponse struct {
	providerKeyItem
	Key     string `json:"key"`
	Created bool   `json:"created"`
}

func providerKeyKind(raw string) (string, bool) {
	kind := services.RelayPlatformForKind(raw)
	switch kind {
	case services.ProviderKindCodex, services.ProviderKindClaude, services.ProviderKindGemini, services.ProviderKindGPTImage:
		return kind, true
	}
	return kind, false
}

// providerKeyName builds the display name of a dedicated key from its
// provider, stripping control characters and keeping it within the key name
// length limit.
func providerKeyName(kind, providerName string) string {
	label := "Codex"
	switch kind {
	case services.ProviderKindClaude:
		label = "Claude"
	case services.ProviderKindGemini:
		label = "Gemini"
	case services.ProviderKindGPTImage:
		label = "GPT Image"
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(providerName))
	name := fmt.Sprintf("%s · %s", label, cleaned)
	if utf8.RuneCountInString(name) > maxProviderKeyNameRunes {
		name = string([]rune(name)[:maxProviderKeyNameRunes])
	}
	return name
}

// pruneProviderRelayKeys removes dedicated keys whose provider no longer
// exists. It is registered as the provider-save hook and also run on startup
// to catch providers removed while the service was down.
func pruneProviderRelayKeys(keys *services.CodexRelayKeyService, rawKind string, providers []services.Provider) {
	if keys == nil {
		return
	}
	kind, ok := providerKeyKind(rawKind)
	if !ok {
		return
	}
	ids := make([]int64, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID)
	}
	removed, err := keys.PruneProviderKeys(kind, ids)
	if err != nil {
		log.Printf("清理 %s 供应商专属 Key 失败: %v", kind, err)
		return
	}
	if len(removed) > 0 {
		log.Printf("已删除 %d 个失效的 %s 供应商专属 Key", len(removed), kind)
	}
}

// pruneGeminiProviderRelayKeys is the Gemini counterpart of
// pruneProviderRelayKeys; Gemini providers are keyed by string IDs.
func pruneGeminiProviderRelayKeys(keys *services.CodexRelayKeyService, providers []services.GeminiProvider) {
	if keys == nil {
		return
	}
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID)
	}
	removed, err := keys.PruneGeminiProviderKeys(ids)
	if err != nil {
		log.Printf("清理 gemini 供应商专属 Key 失败: %v", err)
		return
	}
	if len(removed) > 0 {
		log.Printf("已删除 %d 个失效的 gemini 供应商专属 Key", len(removed))
	}
}

func registerProviderKeyRoutes(router *gin.Engine, rt *appRuntime, originRequired, authRequired gin.HandlerFunc) {
	router.GET("/api/admin/provider-keys", authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		kind, ok := providerKeyKind(c.Query("kind"))
		if !ok {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_kind", Message: "kind 仅支持 codex、claude、gemini 或 gpt-image"}})
			return
		}
		keys, err := rt.codexRelayKeys.ListProviderKeys(kind)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "list_keys_failed", Message: err.Error()}})
			return
		}
		items := make([]providerKeyItem, 0, len(keys))
		for _, key := range keys {
			items = append(items, providerKeyItem{
				ID: key.ID, Name: key.Name, MaskedKey: key.MaskedKey, Enabled: key.Enabled,
				Kind: kind, ProviderID: key.ProviderBinding.ProviderID, ProviderRef: key.ProviderBinding.ProviderRef,
			})
		}
		c.JSON(http.StatusOK, gin.H{"keys": items})
	})

	// Returns the provider's dedicated key including its secret, creating it
	// on first use so the card can copy it in a single round trip.
	router.POST("/api/admin/provider-keys", originRequired, authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request providerKeyEnsureRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: err.Error()}})
			return
		}
		kind, ok := providerKeyKind(request.Kind)
		if !ok {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_kind", Message: "kind 仅支持 codex、claude、gemini 或 gpt-image"}})
			return
		}
		if kind == services.ProviderKindGemini {
			ensureGeminiProviderKey(c, rt, strings.TrimSpace(request.ProviderRef))
			return
		}
		if request.ProviderID == nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: "providerId is required"}})
			return
		}
		providers, err := rt.providerService.LoadProviders(kind)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "list_providers_failed", Message: err.Error()}})
			return
		}
		var provider *services.Provider
		for index := range providers {
			if providers[index].ID == *request.ProviderID {
				provider = &providers[index]
				break
			}
		}
		if provider == nil {
			c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "provider_not_found", Message: fmt.Sprintf("未找到 %s 供应商: %d", kind, *request.ProviderID)}})
			return
		}
		key, created, err := rt.codexRelayKeys.EnsureProviderKey(kind, provider.ID, providerKeyName(kind, provider.Name))
		if err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "create_key_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, providerKeyEnsureResponse{
			providerKeyItem: providerKeyItem{
				ID: key.ID, Name: key.Name, MaskedKey: services.MaskCodexRelayKey(key.Key), Enabled: key.Enabled,
				Kind: kind, ProviderID: provider.ID,
			},
			Key:     key.Key,
			Created: created,
		})
	})
}

func ensureGeminiProviderKey(c *gin.Context, rt *appRuntime, providerRef string) {
	if providerRef == "" {
		c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: "providerRef is required"}})
		return
	}
	if rt.geminiService == nil {
		c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "provider_service_unavailable", Message: "Gemini provider service is unavailable"}})
		return
	}
	var provider *services.GeminiProvider
	providers := rt.geminiService.GetProviders()
	for index := range providers {
		if providers[index].ID == providerRef {
			provider = &providers[index]
			break
		}
	}
	if provider == nil {
		c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "provider_not_found", Message: fmt.Sprintf("未找到 gemini 供应商: %s", providerRef)}})
		return
	}
	key, created, err := rt.codexRelayKeys.EnsureGeminiProviderKey(provider.ID, providerKeyName(services.ProviderKindGemini, provider.Name))
	if err != nil {
		c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "create_key_failed", Message: err.Error()}})
		return
	}
	c.JSON(http.StatusOK, providerKeyEnsureResponse{
		providerKeyItem: providerKeyItem{
			ID: key.ID, Name: key.Name, MaskedKey: services.MaskCodexRelayKey(key.Key), Enabled: key.Enabled,
			Kind: services.ProviderKindGemini, ProviderRef: provider.ID,
		},
		Key:     key.Key,
		Created: created,
	})
}
