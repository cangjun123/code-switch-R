package main

import (
	"codeswitch/services"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	legacyAdminSessionCookieName = "code_switch_admin_session"
	adminSessionCookieNamePrefix = legacyAdminSessionCookieName + "_"
)

type adminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminInitializeRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	SetupToken string `json:"setupToken"`
}

type adminUpdateCredentialsRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewUsername     string `json:"newUsername"`
	NewPassword     string `json:"newPassword"`
}

type codexRelayKeyCreateRequest struct {
	Name                    string          `json:"name"`
	TokenLimit              int64           `json:"tokenLimit"`
	TokenLimitSnake         *int64          `json:"token_limit"`
	USDLimit                json.RawMessage `json:"usdLimit"`
	USDLimitSnake           json.RawMessage `json:"usd_limit"`
	Period                  string          `json:"period"`
	QuotaPeriod             string          `json:"quotaPeriod"`
	PeriodSnake             string          `json:"quota_period"`
	AllowedProviderIDs      []int64         `json:"allowedProviderIds"`
	AllowedProviderIDsSnake []int64         `json:"allowed_provider_ids"`
}

// UnmarshalJSON keeps the admin endpoint tolerant of clients that encode an
// integer quota as either a JSON number or a quoted decimal string.  The UI
// sends numbers, while older scripts commonly persisted strings.
func (r *codexRelayKeyCreateRequest) UnmarshalJSON(data []byte) error {
	type rawRequest struct {
		Name                    string          `json:"name"`
		TokenLimit              json.RawMessage `json:"tokenLimit"`
		TokenLimitSnake         json.RawMessage `json:"token_limit"`
		USDLimit                json.RawMessage `json:"usdLimit"`
		USDLimitSnake           json.RawMessage `json:"usd_limit"`
		Period                  string          `json:"period"`
		QuotaPeriod             string          `json:"quotaPeriod"`
		PeriodSnake             string          `json:"quota_period"`
		AllowedProviderIDs      []int64         `json:"allowedProviderIds"`
		AllowedProviderIDsSnake []int64         `json:"allowed_provider_ids"`
	}
	var raw rawRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = codexRelayKeyCreateRequest{
		Name: raw.Name, USDLimit: raw.USDLimit, USDLimitSnake: raw.USDLimitSnake,
		Period: raw.Period, QuotaPeriod: raw.QuotaPeriod, PeriodSnake: raw.PeriodSnake,
		AllowedProviderIDs: raw.AllowedProviderIDs, AllowedProviderIDsSnake: raw.AllowedProviderIDsSnake,
	}
	tokenRaw := raw.TokenLimit
	if len(tokenRaw) == 0 {
		tokenRaw = raw.TokenLimitSnake
	}
	if len(tokenRaw) > 0 && strings.TrimSpace(string(tokenRaw)) != "null" {
		value, err := parseAdminInt64(tokenRaw)
		if err != nil {
			return fmt.Errorf("invalid tokenLimit: %w", err)
		}
		r.TokenLimit = value
	}
	return nil
}

type codexRelayKeyProviderAccessRequest struct {
	AllowedProviderIDs      []int64 `json:"allowedProviderIds"`
	AllowedProviderIDsSnake []int64 `json:"allowed_provider_ids"`
}

type codexRelayKeyNameRequest struct {
	Name string `json:"name"`
}

type codexRelayProviderOption struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type codexRelayKeyQuotaRequest struct {
	TokenLimit      *int64          `json:"tokenLimit"`
	TokenLimitSnake *int64          `json:"token_limit"`
	USDLimit        json.RawMessage `json:"usdLimit"`
	USDLimitSnake   json.RawMessage `json:"usd_limit"`
	Period          string          `json:"period"`
	QuotaPeriod     string          `json:"quotaPeriod"`
	PeriodSnake     string          `json:"quota_period"`
}

func (r *codexRelayKeyQuotaRequest) UnmarshalJSON(data []byte) error {
	type rawRequest struct {
		TokenLimit      json.RawMessage `json:"tokenLimit"`
		TokenLimitSnake json.RawMessage `json:"token_limit"`
		USDLimit        json.RawMessage `json:"usdLimit"`
		USDLimitSnake   json.RawMessage `json:"usd_limit"`
		Period          string          `json:"period"`
		QuotaPeriod     string          `json:"quotaPeriod"`
		PeriodSnake     string          `json:"quota_period"`
	}
	var raw rawRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = codexRelayKeyQuotaRequest{
		USDLimit: raw.USDLimit, USDLimitSnake: raw.USDLimitSnake,
		Period: raw.Period, QuotaPeriod: raw.QuotaPeriod, PeriodSnake: raw.PeriodSnake,
	}
	tokenRaw := raw.TokenLimit
	if len(tokenRaw) == 0 {
		tokenRaw = raw.TokenLimitSnake
	}
	if len(tokenRaw) > 0 && strings.TrimSpace(string(tokenRaw)) != "null" {
		value, err := parseAdminInt64(tokenRaw)
		if err != nil {
			return fmt.Errorf("invalid tokenLimit: %w", err)
		}
		r.TokenLimit = &value
	}
	return nil
}

func parseAdminInt64(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, parseErr := number.Int64()
		if parseErr == nil {
			return value, nil
		}
		return 0, parseErr
	}
	var quoted string
	if err := json.Unmarshal(raw, &quoted); err != nil {
		return 0, errors.New("must be an integer number or string")
	}
	value, err := json.Number(strings.TrimSpace(quoted)).Int64()
	if err != nil {
		return 0, err
	}
	return value, nil
}

type relayModelPriceRequest struct {
	Model                 string `json:"model"`
	Input                 string `json:"input"`
	InputSnake            string `json:"input_price"`
	CachedInput           string `json:"cachedInput"`
	CachedInputSnake      string `json:"cached_input"`
	CachedInputPriceSnake string `json:"cached_input_price"`
	Output                string `json:"output"`
	OutputSnake           string `json:"output_price"`
	ReasoningOutput       string `json:"reasoningOutput"`
	ReasoningOutputSnake  string `json:"reasoning_output"`
	ReasoningPriceSnake   string `json:"reasoning_output_price"`
	InputPriceCamel       string `json:"inputPrice"`
	CachedInputPriceCamel string `json:"cachedInputPrice"`
	OutputPriceCamel      string `json:"outputPrice"`
	ReasoningPriceCamel   string `json:"reasoningOutputPrice"`
}

// UnmarshalJSON accepts both decimal strings (the canonical API form) and
// JSON numbers.  Keeping the values as strings after decoding avoids float
// rounding before the service converts them to nano-USD.
func (r *relayModelPriceRequest) UnmarshalJSON(data []byte) error {
	type rawRequest struct {
		Model                 string          `json:"model"`
		Input                 json.RawMessage `json:"input"`
		InputSnake            json.RawMessage `json:"input_price"`
		CachedInput           json.RawMessage `json:"cachedInput"`
		CachedInputSnake      json.RawMessage `json:"cached_input"`
		CachedInputPriceSnake json.RawMessage `json:"cached_input_price"`
		Output                json.RawMessage `json:"output"`
		OutputSnake           json.RawMessage `json:"output_price"`
		ReasoningOutput       json.RawMessage `json:"reasoningOutput"`
		ReasoningOutputSnake  json.RawMessage `json:"reasoning_output"`
		ReasoningPriceSnake   json.RawMessage `json:"reasoning_output_price"`
		InputPriceCamel       json.RawMessage `json:"inputPrice"`
		CachedInputPriceCamel json.RawMessage `json:"cachedInputPrice"`
		OutputPriceCamel      json.RawMessage `json:"outputPrice"`
		ReasoningPriceCamel   json.RawMessage `json:"reasoningOutputPrice"`
	}
	var raw rawRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	read := func(label string, values ...json.RawMessage) (string, error) {
		for _, value := range values {
			if len(value) == 0 {
				continue
			}
			text := strings.TrimSpace(string(value))
			if text == "null" {
				continue
			}
			if text == "" {
				continue
			}
			var number json.Number
			if err := json.Unmarshal(value, &number); err == nil {
				return number.String(), nil
			}
			var quoted string
			if err := json.Unmarshal(value, &quoted); err != nil {
				return "", fmt.Errorf("%s must be a decimal number or string", label)
			}
			return strings.TrimSpace(quoted), nil
		}
		return "", nil
	}
	var err error
	*r = relayModelPriceRequest{Model: raw.Model}
	if r.Input, err = read("input", raw.Input, raw.InputSnake, raw.InputPriceCamel); err != nil {
		return err
	}
	if r.CachedInput, err = read("cachedInput", raw.CachedInput, raw.CachedInputSnake, raw.CachedInputPriceSnake, raw.CachedInputPriceCamel); err != nil {
		return err
	}
	if r.Output, err = read("output", raw.Output, raw.OutputSnake, raw.OutputPriceCamel); err != nil {
		return err
	}
	if r.ReasoningOutput, err = read("reasoningOutput", raw.ReasoningOutput, raw.ReasoningOutputSnake, raw.ReasoningPriceSnake, raw.ReasoningPriceCamel); err != nil {
		return err
	}
	return nil
}

func (r relayModelPriceRequest) toServiceModel() services.RelayModelPrice {
	input := r.Input
	if strings.TrimSpace(input) == "" {
		input = r.InputSnake
	}
	if strings.TrimSpace(input) == "" {
		input = r.InputPriceCamel
	}
	cached := r.CachedInput
	if strings.TrimSpace(cached) == "" {
		cached = r.CachedInputSnake
	}
	if strings.TrimSpace(cached) == "" {
		cached = r.CachedInputPriceSnake
	}
	if strings.TrimSpace(cached) == "" {
		cached = r.CachedInputPriceCamel
	}
	output := r.Output
	if strings.TrimSpace(output) == "" {
		output = r.OutputSnake
	}
	if strings.TrimSpace(output) == "" {
		output = r.OutputPriceCamel
	}
	reasoning := r.ReasoningOutput
	if strings.TrimSpace(reasoning) == "" {
		reasoning = r.ReasoningOutputSnake
	}
	if strings.TrimSpace(reasoning) == "" {
		reasoning = r.ReasoningPriceSnake
	}
	if strings.TrimSpace(reasoning) == "" {
		reasoning = r.ReasoningPriceCamel
	}
	return services.RelayModelPrice{Model: r.Model, Input: input, CachedInput: cached, Output: output, ReasoningOutput: reasoning}
}

func requestTokenLimit(tokenLimit int64, snake *int64) int64 {
	if snake != nil {
		return *snake
	}
	return tokenLimit
}

func requestUSDLimit(primary, snake json.RawMessage) (string, error) {
	raw := primary
	if len(raw) == 0 {
		raw = snake
	}
	return services.ParseRelayUSDLimit(raw)
}

func requestQuotaPeriod(primary, quotaPeriod, snake string) string {
	if strings.TrimSpace(quotaPeriod) != "" {
		return quotaPeriod
	}
	if strings.TrimSpace(snake) != "" {
		return snake
	}
	return primary
}

func requestAllowedProviderIDs(primary, snake []int64) []int64 {
	if primary != nil {
		return primary
	}
	return snake
}

func validateCodexAllowedProviderIDs(rt *appRuntime, requested []int64) ([]int64, error) {
	normalized, err := services.NormalizeCodexAllowedProviderIDs(requested)
	if err != nil || len(normalized) == 0 {
		return normalized, err
	}
	if rt == nil || rt.providerService == nil {
		return nil, errors.New("Codex provider service is unavailable")
	}
	providers, err := rt.providerService.LoadProviders(services.ProviderKindCodex)
	if err != nil {
		return nil, fmt.Errorf("读取 Codex provider 失败: %w", err)
	}
	configured := make(map[int64]struct{}, len(providers))
	for _, provider := range providers {
		configured[provider.ID] = struct{}{}
	}
	missing := make([]int64, 0)
	for _, providerID := range normalized {
		if _, exists := configured[providerID]; !exists {
			missing = append(missing, providerID)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Codex provider 不存在: %v", missing)
	}
	return normalized, nil
}

func registerAdminAuthRoutes(router *gin.Engine, rt *appRuntime) {
	authRequired := requireAdminSession(rt.adminAuth, rt.adminSecurity)
	originRequired := requireTrustedOrigin(rt.adminSecurity)

	router.GET("/api/admin/status", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		sessionToken := adminSessionTokenFromRequest(c.Request, rt.adminSecurity)
		status, err := rt.adminAuth.GetStatus(sessionToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "status_failed", Message: err.Error()},
			})
			return
		}
		if adminSessionCookiePresent(c.Request, rt.adminSecurity) && !status.Authenticated {
			clearAdminSessionCookie(c, rt.adminSecurity)
		}
		c.JSON(http.StatusOK, status)
	})

	router.POST("/api/admin/initialize", originRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		subject, limited := rejectWhenRateLimited(c, rt.adminSecurity, "initialize")
		if limited {
			return
		}

		var request adminInitializeRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "invalid_request", Message: err.Error()},
			})
			return
		}
		if rt.adminSecurity != nil && !rt.adminSecurity.isSetupAllowed(c.Request, request.SetupToken) {
			recordRateLimitFailure(rt.adminSecurity, "initialize", subject)
			c.JSON(http.StatusForbidden, apiErrorResponse{
				Error: apiError{Code: "setup_token_required", Message: "首次初始化需要有效的 setup token"},
			})
			return
		}

		token, status, err := rt.adminAuth.InitializeAdmin(request.Username, request.Password)
		if err != nil {
			recordRateLimitFailure(rt.adminSecurity, "initialize", subject)
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "initialize_failed", Message: err.Error()},
			})
			return
		}

		recordRateLimitSuccess(rt.adminSecurity, "initialize", subject)
		setAdminSessionCookie(c, rt.adminSecurity, token)
		c.JSON(http.StatusOK, status)
	})

	router.POST("/api/admin/login", originRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		subject, limited := rejectWhenRateLimited(c, rt.adminSecurity, "login")
		if limited {
			return
		}

		var request adminLoginRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "invalid_request", Message: err.Error()},
			})
			return
		}

		token, status, err := rt.adminAuth.Login(request.Username, request.Password)
		if err != nil {
			recordRateLimitFailure(rt.adminSecurity, "login", subject)
			c.JSON(http.StatusUnauthorized, apiErrorResponse{
				Error: apiError{Code: "login_failed", Message: err.Error()},
			})
			return
		}

		recordRateLimitSuccess(rt.adminSecurity, "login", subject)
		setAdminSessionCookie(c, rt.adminSecurity, token)
		c.JSON(http.StatusOK, status)
	})

	router.POST("/api/admin/logout", originRequired, authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if err := rt.adminAuth.Logout(adminSessionTokenFromRequest(c.Request, rt.adminSecurity)); err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "logout_failed", Message: err.Error()},
			})
			return
		}
		clearAdminSessionCookie(c, rt.adminSecurity)
		c.Status(http.StatusNoContent)
	})

	router.POST("/api/admin/credentials", originRequired, authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request adminUpdateCredentialsRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "invalid_request", Message: err.Error()},
			})
			return
		}

		token, status, err := rt.adminAuth.UpdateCredentials(
			request.CurrentPassword,
			request.NewUsername,
			request.NewPassword,
		)
		if err != nil {
			statusCode := http.StatusBadRequest
			if strings.Contains(err.Error(), "当前密码错误") {
				statusCode = http.StatusUnauthorized
			}
			c.JSON(statusCode, apiErrorResponse{
				Error: apiError{Code: "update_credentials_failed", Message: err.Error()},
			})
			return
		}

		setAdminSessionCookie(c, rt.adminSecurity, token)
		c.JSON(http.StatusOK, status)
	})

	router.GET("/api/admin/codex-providers", authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.providerService == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "provider_service_unavailable", Message: "Codex provider service is unavailable"}})
			return
		}
		providers, err := rt.providerService.LoadProviders(services.ProviderKindCodex)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "list_providers_failed", Message: err.Error()}})
			return
		}
		options := make([]codexRelayProviderOption, 0, len(providers))
		for _, provider := range providers {
			options = append(options, codexRelayProviderOption{ID: provider.ID, Name: provider.Name, Enabled: provider.Enabled})
		}
		c.JSON(http.StatusOK, gin.H{"providers": options})
	})

	router.GET("/api/admin/codex-keys", authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		keys, err := rt.codexRelayKeys.ListKeys()
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "list_keys_failed", Message: err.Error()},
			})
			return
		}
		if rt.relayQuota != nil {
			for index := range keys {
				key, keyErr := rt.codexRelayKeys.GetKeyByID(keys[index].ID)
				if keyErr != nil {
					continue
				}
				status, statusErr := rt.relayQuota.Status(key)
				if statusErr == nil {
					keys[index].Quota = &status
				}
			}
		}
		c.JSON(http.StatusOK, gin.H{"keys": keys})
	})

	router.POST("/api/admin/codex-keys", originRequired, authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request codexRelayKeyCreateRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "invalid_request", Message: err.Error()},
			})
			return
		}

		// Validate optional quota fields before creating the key so malformed
		// admin input cannot leave an unconfigured orphan key behind.
		tokenLimit := requestTokenLimit(request.TokenLimit, request.TokenLimitSnake)
		usdLimit, usdErr := requestUSDLimit(request.USDLimit, request.USDLimitSnake)
		if usdErr != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: usdErr.Error()}})
			return
		}
		usdLimit, quotaErr := services.ValidateRelayQuotaLimits(tokenLimit, usdLimit)
		if quotaErr != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: quotaErr.Error()}})
			return
		}
		period := requestQuotaPeriod(request.Period, request.QuotaPeriod, request.PeriodSnake)
		if strings.TrimSpace(period) != "" {
			if _, err := services.ValidateRelayQuotaPeriod(period); err != nil {
				c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: err.Error()}})
				return
			}
		}
		allowedProviderIDs, accessErr := validateCodexAllowedProviderIDs(rt, requestAllowedProviderIDs(request.AllowedProviderIDs, request.AllowedProviderIDsSnake))
		if accessErr != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_provider_access", Message: accessErr.Error()}})
			return
		}

		result, err := rt.codexRelayKeys.CreateKey(request.Name)
		if err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{
				Error: apiError{Code: "create_key_failed", Message: err.Error()},
			})
			return
		}
		if rt.relayQuota != nil {
			if tokenLimit != 0 || strings.TrimSpace(usdLimit) != "0" || strings.TrimSpace(period) != "" {
				if err := rt.codexRelayKeys.UpdateQuotaConfig(result.ID, tokenLimit, usdLimit, period); err != nil {
					c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: err.Error()}})
					return
				}
				result.TokenLimit = tokenLimit
				result.USDLimit = usdLimit
				result.QuotaPeriod = services.NormalizeRelayQuotaPeriod(period)
			}
		}
		if err := rt.codexRelayKeys.UpdateAllowedProviderIDs(result.ID, allowedProviderIDs); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_provider_access", Message: err.Error()}})
			return
		}
		result.AllowedProviderIDs = append([]int64{}, allowedProviderIDs...)
		c.JSON(http.StatusOK, result)
	})

	router.GET("/api/admin/codex-keys/:id/secret", authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		secret, err := rt.codexRelayKeys.GetKeySecret(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, apiErrorResponse{
				Error: apiError{Code: "key_not_found", Message: err.Error()},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"key": secret})
	})

	updateKeyName := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request codexRelayKeyNameRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: err.Error()}})
			return
		}
		name, err := rt.codexRelayKeys.UpdateName(c.Param("id"), request.Name)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "未找到") {
				status = http.StatusNotFound
			}
			c.JSON(status, apiErrorResponse{Error: apiError{Code: "update_key_name_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "name": name})
	}
	router.PATCH("/api/admin/codex-keys/:id/name", originRequired, authRequired, updateKeyName)
	router.PUT("/api/admin/codex-keys/:id/name", originRequired, authRequired, updateKeyName)

	// Quota status is exposed separately so clients can refresh it without
	// retrieving key secrets.
	router.GET("/api/admin/codex-keys/:id/quota", authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		key, err := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: err.Error()}})
			return
		}
		status, err := rt.relayQuota.Status(key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "quota_status_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, status)
	})

	updateQuota := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		var request codexRelayKeyQuotaRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: err.Error()}})
			return
		}
		tokenLimit := int64(0)
		if request.TokenLimit != nil {
			tokenLimit = *request.TokenLimit
		} else if request.TokenLimitSnake != nil {
			tokenLimit = *request.TokenLimitSnake
		} else {
			key, keyErr := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
			if keyErr != nil {
				c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: keyErr.Error()}})
				return
			}
			tokenLimit = key.TokenLimit
		}
		usdLimit, usdErr := requestUSDLimit(request.USDLimit, request.USDLimitSnake)
		if len(request.USDLimit) == 0 && len(request.USDLimitSnake) == 0 {
			key, keyErr := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
			if keyErr != nil {
				c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: keyErr.Error()}})
				return
			}
			usdLimit = key.USDLimit
		}
		if usdErr != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: usdErr.Error()}})
			return
		}
		usdLimit, quotaErr := services.ValidateRelayQuotaLimits(tokenLimit, usdLimit)
		if quotaErr != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_quota", Message: quotaErr.Error()}})
			return
		}
		period := requestQuotaPeriod(request.Period, request.QuotaPeriod, request.PeriodSnake)
		if strings.TrimSpace(period) == "" {
			key, keyErr := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
			if keyErr != nil {
				c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: keyErr.Error()}})
				return
			}
			period = key.QuotaPeriod
		}
		if err := rt.codexRelayKeys.UpdateQuotaConfig(c.Param("id"), tokenLimit, usdLimit, period); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "未找到") {
				status = http.StatusNotFound
			}
			c.JSON(status, apiErrorResponse{Error: apiError{Code: "update_quota_failed", Message: err.Error()}})
			return
		}
		key, err := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: err.Error()}})
			return
		}
		quota, err := rt.relayQuota.Status(key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "quota_status_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, quota)
	}
	router.PATCH("/api/admin/codex-keys/:id/quota", originRequired, authRequired, updateQuota)
	router.PUT("/api/admin/codex-keys/:id/quota", originRequired, authRequired, updateQuota)
	router.PATCH("/api/admin/codex-keys/:id", originRequired, authRequired, updateQuota)

	updateProviderAccess := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request codexRelayKeyProviderAccessRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: err.Error()}})
			return
		}
		if request.AllowedProviderIDs == nil && request.AllowedProviderIDsSnake == nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: "allowedProviderIds is required"}})
			return
		}
		providerIDs, err := validateCodexAllowedProviderIDs(rt, requestAllowedProviderIDs(request.AllowedProviderIDs, request.AllowedProviderIDsSnake))
		if err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_provider_access", Message: err.Error()}})
			return
		}
		if err := rt.codexRelayKeys.UpdateAllowedProviderIDs(c.Param("id"), providerIDs); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "未找到") {
				status = http.StatusNotFound
			}
			c.JSON(status, apiErrorResponse{Error: apiError{Code: "update_provider_access_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"allowedProviderIds": append([]int64{}, providerIDs...)})
	}
	router.PATCH("/api/admin/codex-keys/:id/providers", originRequired, authRequired, updateProviderAccess)
	router.PUT("/api/admin/codex-keys/:id/providers", originRequired, authRequired, updateProviderAccess)

	resetQuota := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		key, err := rt.codexRelayKeys.GetKeyByID(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, apiErrorResponse{Error: apiError{Code: "key_not_found", Message: err.Error()}})
			return
		}
		if err := rt.relayQuota.Reset(key); err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "reset_quota_failed", Message: err.Error()}})
			return
		}
		status, err := rt.relayQuota.Status(key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "quota_status_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, status)
	}
	router.POST("/api/admin/codex-keys/:id/reset-quota", originRequired, authRequired, resetQuota)
	// Alias retained for clients that model reset as a REST resource action.
	router.POST("/api/admin/codex-keys/:id/quota/reset", originRequired, authRequired, resetQuota)

	router.DELETE("/api/admin/codex-keys/:id", originRequired, authRequired, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if err := rt.codexRelayKeys.DeleteKey(c.Param("id")); err != nil {
			statusCode := http.StatusBadRequest
			if strings.Contains(err.Error(), "未找到") {
				statusCode = http.StatusNotFound
			}
			c.JSON(statusCode, apiErrorResponse{
				Error: apiError{Code: "delete_key_failed", Message: err.Error()},
			})
			return
		}
		if err := refreshCodexProxyKey(rt); err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "refresh_codex_key_failed", Message: err.Error()},
			})
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Global Codex model prices. Values are USD per million tokens and are
	// deliberately strings in the JSON API.
	priceList := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		prices, err := rt.relayQuota.ListModelPrices()
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "list_model_prices_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"prices": prices})
	}
	priceUpsert := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		var request relayModelPriceRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "invalid_request", Message: err.Error()}})
			return
		}
		if strings.TrimSpace(request.Model) == "" {
			request.Model = c.Param("model")
		}
		if strings.TrimSpace(request.Model) == "" {
			request.Model = strings.TrimSpace(c.Query("model"))
		}
		price, err := rt.relayQuota.UpsertModelPrice(request.toServiceModel())
		if err != nil {
			c.JSON(http.StatusBadRequest, apiErrorResponse{Error: apiError{Code: "upsert_model_price_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, price)
	}
	priceDelete := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		model := strings.TrimSpace(c.Param("model"))
		if model == "" {
			model = strings.TrimSpace(c.Query("model"))
		}
		if model == "" {
			var request struct {
				Model string `json:"model"`
			}
			if err := c.ShouldBindJSON(&request); err == nil {
				model = strings.TrimSpace(request.Model)
			}
		}
		if err := rt.relayQuota.DeleteModelPrice(model); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "未找到") {
				status = http.StatusNotFound
			}
			c.JSON(status, apiErrorResponse{Error: apiError{Code: "delete_model_price_failed", Message: err.Error()}})
			return
		}
		c.Status(http.StatusNoContent)
	}
	priceUnpriced := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rt.relayQuota == nil {
			c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Error: apiError{Code: "quota_unavailable", Message: "relay quota service is unavailable"}})
			return
		}
		models, err := rt.relayQuota.ListUnpricedModels()
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{Error: apiError{Code: "list_unpriced_models_failed", Message: err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"models": models})
	}
	router.GET("/api/admin/relay-model-prices", authRequired, priceList)
	router.GET("/api/admin/model-prices", authRequired, priceList)
	router.POST("/api/admin/relay-model-prices", originRequired, authRequired, priceUpsert)
	router.PUT("/api/admin/relay-model-prices", originRequired, authRequired, priceUpsert)
	router.PUT("/api/admin/relay-model-prices/:model", originRequired, authRequired, priceUpsert)
	router.POST("/api/admin/model-prices", originRequired, authRequired, priceUpsert)
	router.PUT("/api/admin/model-prices", originRequired, authRequired, priceUpsert)
	router.PUT("/api/admin/model-prices/:model", originRequired, authRequired, priceUpsert)
	router.DELETE("/api/admin/relay-model-prices/:model", originRequired, authRequired, priceDelete)
	router.DELETE("/api/admin/relay-model-prices", originRequired, authRequired, priceDelete)
	router.DELETE("/api/admin/model-prices/:model", originRequired, authRequired, priceDelete)
	router.DELETE("/api/admin/model-prices", originRequired, authRequired, priceDelete)
	router.GET("/api/admin/relay-model-prices/unpriced", authRequired, priceUnpriced)
	router.GET("/api/admin/unpriced-models", authRequired, priceUnpriced)
}

func requireAdminSession(authService *services.AdminAuthService, security *adminSecurity) gin.HandlerFunc {
	return func(c *gin.Context) {
		if authService == nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "auth_unavailable", Message: "admin auth service is unavailable"},
			})
			c.Abort()
			return
		}

		username, ok, err := authService.ValidateSession(adminSessionTokenFromRequest(c.Request, security))
		if err != nil {
			c.JSON(http.StatusInternalServerError, apiErrorResponse{
				Error: apiError{Code: "auth_failed", Message: err.Error()},
			})
			c.Abort()
			return
		}
		if !ok {
			clearAdminSessionCookie(c, security)
			c.JSON(http.StatusUnauthorized, apiErrorResponse{
				Error: apiError{Code: "unauthorized", Message: "admin login required"},
			})
			c.Abort()
			return
		}

		c.Set("admin_username", username)
		c.Next()
	}
}

func sanitizeCookieNamePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))

	lastUnderscore := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			builder.WriteByte(ch)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}

	return strings.Trim(builder.String(), "_")
}

func adminSessionCookieNameForRequest(r *http.Request, security *adminSecurity) string {
	host := ""
	if security != nil {
		host = security.effectiveHost(r)
	}
	if host == "" && r != nil {
		host = strings.TrimSpace(r.Host)
	}

	suffix := sanitizeCookieNamePart(host)
	if suffix == "" {
		return legacyAdminSessionCookieName
	}

	return adminSessionCookieNamePrefix + suffix
}

func adminSessionCookiePresent(r *http.Request, security *adminSecurity) bool {
	if r == nil {
		return false
	}
	if _, err := r.Cookie(adminSessionCookieNameForRequest(r, security)); err == nil {
		return true
	}
	if _, err := r.Cookie(legacyAdminSessionCookieName); err == nil {
		return true
	}
	return false
}

func adminSessionTokenFromRequest(r *http.Request, security *adminSecurity) string {
	if r == nil {
		return ""
	}
	cookie, err := r.Cookie(adminSessionCookieNameForRequest(r, security))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func setAdminSessionCookie(c *gin.Context, security *adminSecurity, token string) {
	name := adminSessionCookieNameForRequest(c.Request, security)
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		MaxAge:   int(services.AdminSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   adminSessionCookieSecure(c.Request, security),
		SameSite: http.SameSiteStrictMode,
	})
	if name != legacyAdminSessionCookieName {
		clearAdminSessionCookieByName(c, security, legacyAdminSessionCookieName)
	}
}

func clearAdminSessionCookie(c *gin.Context, security *adminSecurity) {
	name := adminSessionCookieNameForRequest(c.Request, security)
	clearAdminSessionCookieByName(c, security, name)
	if name != legacyAdminSessionCookieName {
		clearAdminSessionCookieByName(c, security, legacyAdminSessionCookieName)
	}
}

func clearAdminSessionCookieByName(c *gin.Context, security *adminSecurity, name string) {
	if c == nil || strings.TrimSpace(name) == "" {
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   adminSessionCookieSecure(c.Request, security),
		SameSite: http.SameSiteStrictMode,
	})
}

func refreshCodexProxyKey(rt *appRuntime) error {
	if rt == nil || rt.codexSettings == nil {
		return nil
	}

	status, err := rt.codexSettings.ProxyStatus()
	if err != nil {
		return err
	}
	if !status.Enabled {
		return nil
	}

	if err := rt.codexSettings.EnableProxy(); err != nil {
		return err
	}
	return nil
}
