package services

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const codexRelayKeyHeader = "X-Code-Switch-Key"
const relayKeyIDContextKey = "relay_key_id"
const relayKeyContextKey = "relay_key"

func (prs *ProviderRelayService) claudeRelayAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if prs.codexRelayKeys == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "relay key service is unavailable"})
			c.Abort()
			return
		}

		if _, err := prs.codexRelayKeys.EnsureDefaultKey(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize relay keys"})
			c.Abort()
			return
		}

		candidate := extractClaudeRelayKey(c.Request)
		key, err := prs.codexRelayKeys.FindKey(candidate)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate relay key"})
			c.Abort()
			return
		}
		if key == nil {
			c.Header("WWW-Authenticate", "Bearer realm=\"code-switch-claude\"")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "invalid claude relay api key",
			})
			c.Abort()
			return
		}
		setRelayKeyContext(c, key)

		// 清理客户端传来的认证头，避免泄漏 relay key 给上游
		c.Request.Header.Del("Authorization")
		c.Request.Header.Del("X-Api-Key")
		c.Request.Header.Del("x-api-key")
		c.Request.Header.Del(codexRelayKeyHeader)

		c.Next()
	}
}

func extractClaudeRelayKey(req *http.Request) string {
	if req == nil {
		return ""
	}

	if key := strings.TrimSpace(req.Header.Get("x-api-key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(req.Header.Get(codexRelayKeyHeader)); key != "" {
		return key
	}
	if auth := strings.TrimSpace(req.Header.Get("Authorization")); auth != "" {
		return extractBearerToken(auth)
	}

	return ""
}

func (prs *ProviderRelayService) codexRelayAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if prs.codexRelayKeys == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "codex relay key service is unavailable"})
			c.Abort()
			return
		}

		if _, err := prs.codexRelayKeys.EnsureDefaultKey(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize codex relay keys"})
			c.Abort()
			return
		}

		candidate := extractCodexRelayKey(c.Request)
		key, err := prs.codexRelayKeys.FindKey(candidate)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate codex relay key"})
			c.Abort()
			return
		}
		if key == nil {
			c.Header("WWW-Authenticate", "Bearer realm=\"code-switch-codex\"")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "invalid codex relay api key",
			})
			c.Abort()
			return
		}
		setRelayKeyContext(c, key)

		// 上游认证由 provider 配置重新注入，避免把客户端传来的 relay key 转发出去。
		c.Request.Header.Del("Authorization")
		c.Request.Header.Del(codexRelayKeyHeader)
		c.Request.Header.Del("X-API-Key")
		c.Request.Header.Del("x-api-key")

		if trace := codexTraceFromContext(c.Request.Context()); trace != nil {
			trace.setKeyID(key.ID)
			trace.mark("auth_done")
		}
		c.Next()
	}
}

func relayKeyIDFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get(relayKeyIDContextKey)
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func setRelayKeyContext(c *gin.Context, key *CodexRelayKey) {
	if c == nil || key == nil {
		return
	}
	copyKey := cloneCodexRelayKey(*key)
	c.Set(relayKeyIDContextKey, copyKey.ID)
	c.Set(relayKeyContextKey, &copyKey)
}

func relayKeyFromContext(c *gin.Context) *CodexRelayKey {
	if c == nil {
		return nil
	}
	value, ok := c.Get(relayKeyContextKey)
	if !ok {
		return nil
	}
	key, _ := value.(*CodexRelayKey)
	if key == nil {
		return nil
	}
	copyKey := cloneCodexRelayKey(*key)
	return &copyKey
}

func (prs *ProviderRelayService) relayKeyForRequest(c *gin.Context) (*CodexRelayKey, error) {
	if key := relayKeyFromContext(c); key != nil {
		return key, nil
	}
	if prs == nil || prs.codexRelayKeys == nil {
		return nil, nil
	}
	keyID := relayKeyIDFromContext(c)
	if keyID == "" {
		return nil, nil
	}
	return prs.codexRelayKeys.GetKeyByID(keyID)
}

// codexQuotaMiddleware is mounted only on inference routes.  Authentication is
// shared by images and /v1/models, but those endpoints must never be blocked by
// Codex token/cost quotas.
func (prs *ProviderRelayService) codexQuotaMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if prs.relayQuota == nil || prs.codexRelayKeys == nil {
			c.Next()
			return
		}
		keyID := relayKeyIDFromContext(c)
		if keyID == "" {
			c.Next()
			return
		}
		key, err := prs.relayKeyForRequest(c)
		if err != nil {
			writeOpenAIQuotaServiceError(c, http.StatusUnauthorized, "relay_key_not_found", "relay key no longer exists")
			c.Abort()
			return
		}
		trace := codexTraceFromContext(c.Request.Context())
		trace.mark("quota_check_start")
		decision, err := prs.relayQuota.Check(key)
		trace.mark("quota_check_done")
		if err != nil {
			writeOpenAIQuotaServiceError(c, http.StatusServiceUnavailable, "quota_service_unavailable", "relay quota service is temporarily unavailable")
			c.Abort()
			return
		}
		if decision.Allowed {
			c.Next()
			return
		}
		writeOpenAIQuotaExceeded(c, decision)
		c.Abort()
	}
}

func (prs *ProviderRelayService) codexQuotaStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if prs.relayQuota == nil || prs.codexRelayKeys == nil {
			writeOpenAIQuotaServiceError(c, http.StatusServiceUnavailable, "quota_service_unavailable", "relay quota service is temporarily unavailable")
			return
		}
		key, err := prs.relayKeyForRequest(c)
		if err != nil || key == nil {
			writeOpenAIQuotaServiceError(c, http.StatusUnauthorized, "relay_key_not_found", "relay key no longer exists")
			return
		}
		status, err := prs.relayQuota.Status(key)
		if err != nil {
			writeOpenAIQuotaServiceError(c, http.StatusServiceUnavailable, "quota_service_unavailable", "relay quota service is temporarily unavailable")
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

func filterCodexProvidersForRelayKey(key *CodexRelayKey, providers []Provider) ([]Provider, int) {
	if key == nil || len(key.AllowedProviderIDs) == 0 {
		return providers, 0
	}
	allowed := make(map[int64]struct{}, len(key.AllowedProviderIDs))
	for _, providerID := range key.AllowedProviderIDs {
		allowed[providerID] = struct{}{}
	}
	filtered := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if _, ok := allowed[provider.ID]; ok {
			filtered = append(filtered, provider)
		}
	}
	return filtered, len(providers) - len(filtered)
}

func writeOpenAIProviderAccessDenied(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"message": "Relay key is not allowed to access any configured Codex provider",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "provider_access_denied",
		},
	})
}

func writeOpenAIQuotaServiceError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "server_error",
			"param":   nil,
			"code":    code,
		},
	})
}

func writeOpenAIQuotaExceeded(c *gin.Context, decision RelayQuotaDecision) {
	status := decision.Status
	code := "relay_key_quota_exceeded"
	message := "Relay key quota exceeded"
	switch decision.Reason {
	case "token":
		code = "relay_key_token_quota_exceeded"
		message = "Relay key token quota exceeded"
	case "usd":
		code = "relay_key_cost_quota_exceeded"
		message = "Relay key cost quota exceeded"
	case "token_and_usd":
		code = "relay_key_quota_exceeded"
		message = "Relay key token and cost quotas exceeded"
	}
	if status.ResetAt != nil && strings.TrimSpace(*status.ResetAt) != "" {
		message += "; resets at " + *status.ResetAt
		if resetAt, err := time.Parse(time.RFC3339, *status.ResetAt); err == nil {
			seconds := int64(time.Until(resetAt).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			c.Header("Retry-After", strconv.FormatInt(seconds, 10))
			c.Header("X-Quota-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		}
	} else if status.Blocked && status.Period == RelayQuotaPeriodOnce {
		message += "; requires administrator reset"
	}
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "insufficient_quota",
			"param":   nil,
			"code":    code,
		},
	})
}

func extractCodexRelayKey(req *http.Request) string {
	if req == nil {
		return ""
	}

	if key := strings.TrimSpace(req.Header.Get(codexRelayKeyHeader)); key != "" {
		return key
	}
	if key := strings.TrimSpace(req.Header.Get("X-API-Key")); key != "" {
		return key
	}
	if auth := strings.TrimSpace(req.Header.Get("Authorization")); auth != "" {
		return extractBearerToken(auth)
	}

	return ""
}

func extractBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[len("Bearer "):])
	}

	return value
}
