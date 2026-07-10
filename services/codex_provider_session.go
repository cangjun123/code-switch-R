package services

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const codexSessionRouteTTL = 24 * time.Hour

type codexSessionKey [sha256.Size]byte

type codexSessionRouteState struct {
	providerName   string
	pendingCallIDs map[string]struct{}
	pendingCalls   bool
	updatedAt      time.Time
}

type codexSessionRequestContext struct {
	key                  codexSessionKey
	ownerProvider        string
	pendingCallIDs       map[string]struct{}
	pendingCalls         bool
	requestToolOutputIDs map[string]struct{}
	hasToolOutput        bool
	stickyProvider       string
}

type codexHistoryAttempt struct {
	sessionRequest *codexSessionRequestContext
	providerName   string
	cacheKey       string
	cutoverPlan    *codexProviderHistorySanitizePlan
	preSanitized   bool
	unknownOwner   bool
	inspectEmpty   bool
}

const codexSessionContextKey = "codex-provider-session-request"

func newCodexSessionRequestContext(body []byte, headers map[string]string) *codexSessionRequestContext {
	key, ok := codexSessionKeyFromRequest(body, headers)
	if !ok {
		return nil
	}
	outputIDs, hasOutput := codexRequestToolOutputIDs(body)
	return &codexSessionRequestContext{
		key:                  key,
		requestToolOutputIDs: outputIDs,
		hasToolOutput:        hasOutput,
	}
}

func codexSessionKeyFromRequest(body []byte, headers map[string]string) (codexSessionKey, bool) {
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); promptCacheKey != "" {
		return sha256.Sum256([]byte("prompt_cache_key\x00" + promptCacheKey)), true
	}
	for _, headerName := range []string{"thread_id", "session_id"} {
		if value := strings.TrimSpace(headerValueCaseInsensitive(headers, headerName)); value != "" {
			return sha256.Sum256([]byte(headerName + "\x00" + value)), true
		}
	}
	return codexSessionKey{}, false
}

func headerValueCaseInsensitive(headers map[string]string, target string) string {
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), target) {
			return value
		}
	}
	return ""
}

func codexRequestToolOutputIDs(body []byte) (map[string]struct{}, bool) {
	result := make(map[string]struct{})
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return result, false
	}
	object, ok := root.(map[string]any)
	if !ok {
		return result, false
	}
	input, ok := object["input"].([]any)
	if !ok {
		return result, false
	}
	hasOutput := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !isCodexToolOutputType(stringField(item, "type")) {
			continue
		}
		hasOutput = true
		if callID := stringField(item, "call_id"); callID != "" {
			result[callID] = struct{}{}
		}
	}
	return result, hasOutput
}

func isCodexToolCallType(itemType string) bool {
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	return itemType == "function_call" || itemType == "custom_tool_call" ||
		(strings.HasSuffix(itemType, "_call") && !strings.HasSuffix(itemType, "_call_output"))
}

func isCodexToolOutputType(itemType string) bool {
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	return itemType == "function_call_output" || itemType == "custom_tool_call_output" ||
		strings.HasSuffix(itemType, "_call_output")
}

func (prs *ProviderRelayService) attachCodexSessionRequest(c *gin.Context, kind, endpoint string, body []byte, headers map[string]string) *codexSessionRequestContext {
	if c == nil || kind != ProviderKindCodex || !isResponsesEndpoint(endpoint) {
		return nil
	}
	request := newCodexSessionRequestContext(body, headers)
	if request == nil {
		return nil
	}

	prs.codexSessionMu.Lock()
	state, ok := prs.codexSessionRoutes[request.key]
	if ok && time.Since(state.updatedAt) >= codexSessionRouteTTL {
		delete(prs.codexSessionRoutes, request.key)
		ok = false
	}
	if ok {
		request.ownerProvider = state.providerName
		request.pendingCalls = state.pendingCalls
		request.pendingCallIDs = cloneStringSet(state.pendingCallIDs)
	}
	prs.codexSessionMu.Unlock()

	if request.shouldContinueWithOwner() {
		request.stickyProvider = request.ownerProvider
	}
	c.Set(codexSessionContextKey, request)
	return request
}

func (request *codexSessionRequestContext) shouldContinueWithOwner() bool {
	if request == nil || request.ownerProvider == "" || !request.pendingCalls || !request.hasToolOutput {
		return false
	}
	if len(request.pendingCallIDs) == 0 {
		return true
	}
	for callID := range request.requestToolOutputIDs {
		if _, ok := request.pendingCallIDs[callID]; ok {
			return true
		}
	}
	return false
}

func codexSessionRequestFromContext(c *gin.Context) *codexSessionRequestContext {
	if c == nil {
		return nil
	}
	value, ok := c.Get(codexSessionContextKey)
	if !ok {
		return nil
	}
	request, _ := value.(*codexSessionRequestContext)
	return request
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(source))
	for value := range source {
		result[value] = struct{}{}
	}
	return result
}

func (prs *ProviderRelayService) preferCodexStickyProvider(
	request *codexSessionRequestContext,
	active []Provider,
	configured []Provider,
	requestedModel string,
	endpoint string,
	requestHasNamespaceConflict bool,
) []Provider {
	if request == nil || request.stickyProvider == "" {
		return active
	}

	var sticky *Provider
	for index := range configured {
		provider := configured[index]
		if provider.Name != request.stickyProvider || provider.APIURL == "" || provider.APIKey == "" {
			continue
		}
		if len(provider.ValidateConfiguration()) > 0 ||
			(requestedModel != "" && !provider.IsModelSupported(requestedModel)) ||
			!provider.SupportsOpenAIEndpoint(endpoint) ||
			(requestHasNamespaceConflict && provider.CodexMultiAgentNamespaceRewrite) {
			continue
		}
		provider.Level = 1
		sticky = &provider
		break
	}
	if sticky == nil {
		return active
	}

	ordered := make([]Provider, 0, len(active)+1)
	ordered = append(ordered, *sticky)
	for _, provider := range active {
		if provider.Name != sticky.Name {
			ordered = append(ordered, provider)
		}
	}
	fmt.Printf("[Codex Provider Session] Provider=%s action=continue_tool_call\n", sticky.Name)
	return ordered
}

func (prs *ProviderRelayService) prepareCodexHistoryAttempt(
	c *gin.Context,
	providerName string,
	headers map[string]string,
	body []byte,
) ([]byte, map[string]string, *codexHistoryAttempt) {
	request := codexSessionRequestFromContext(c)
	attempt := &codexHistoryAttempt{
		sessionRequest: request,
		providerName:   providerName,
		cacheKey:       codexHistorySanitizeCacheKey(providerName, body),
		unknownOwner:   request != nil && request.ownerProvider == "",
	}
	if request == nil || request.ownerProvider == "" || request.ownerProvider == providerName {
		return prs.applyCachedCodexHistoryPlan(providerName, body, headers, attempt)
	}

	plan, err := buildCodexProviderHistorySanitizePlan(body)
	if err != nil {
		fmt.Printf("[WARN] Provider %s Codex 切换历史检查失败，继续使用原请求: %v\n", providerName, err)
		return body, headers, attempt
	}
	sanitizedBody, stats, err := sanitizeCodexProviderBoundHistoryWithPlan(body, plan)
	if err != nil {
		fmt.Printf("[WARN] Provider %s Codex 切换历史清理失败，继续使用原请求: %v\n", providerName, err)
		return body, headers, attempt
	}
	sanitizedHeaders, removedHeaders := sanitizeCodexProviderBoundHeaders(headers)
	attempt.cutoverPlan = plan
	attempt.preSanitized = true
	logCodexHistorySanitize(providerName, "provider_switch", "", stats, removedHeaders)
	return sanitizedBody, sanitizedHeaders, attempt
}

func (prs *ProviderRelayService) applyCachedCodexHistoryPlan(
	providerName string,
	body []byte,
	headers map[string]string,
	attempt *codexHistoryAttempt,
) ([]byte, map[string]string, *codexHistoryAttempt) {
	cachedPlan, ok := prs.cachedCodexHistorySanitizePlan(attempt.cacheKey)
	if !ok {
		return body, headers, attempt
	}
	sanitizedBody, stats, err := sanitizeCodexProviderBoundHistoryWithPlan(body, cachedPlan)
	if err != nil {
		fmt.Printf("[WARN] Provider %s Codex 已缓存历史清理失败，继续使用原请求: %v\n", providerName, err)
		return body, headers, attempt
	}
	sanitizedHeaders, removedHeaders := sanitizeCodexProviderBoundHeaders(headers)
	attempt.preSanitized = true
	logCodexHistorySanitize(providerName, "cached", "", stats, removedHeaders)
	return sanitizedBody, sanitizedHeaders, attempt
}

type codexResponseObservation struct {
	jsonPayloads int
	outputEvents int
	terminal     bool
	failed       bool
	toolCallSeen bool
	toolCallIDs  map[string]struct{}
	committed    bool
	onSuccess    func()
}

func (observation *codexResponseObservation) Hook() xrequest.ResponseHook {
	return func(data []byte) (bool, []byte) {
		line := bytes.TrimSpace(data)
		if bytes.HasPrefix(line, []byte("data:")) {
			observation.observePayload(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
		return true, data
	}
}

func (observation *codexResponseObservation) observePayload(payload []byte) {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
		return
	}
	observation.jsonPayloads++
	eventType := gjson.GetBytes(payload, "type").String()
	status := gjson.GetBytes(payload, "response.status").String()
	if status == "" {
		status = gjson.GetBytes(payload, "status").String()
	}
	if eventType == "error" || eventType == "response.failed" || status == "failed" {
		observation.failed = true
		observation.terminal = true
	}
	if eventType == "response.completed" || eventType == "response.incomplete" || status == "completed" || status == "incomplete" {
		observation.terminal = true
	}
	if codexPayloadHasOutput(payload) {
		observation.outputEvents++
	}

	for _, path := range []string{"item", "response.output", "output"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() {
			continue
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				observation.observeOutputItem(item)
			}
		} else {
			observation.observeOutputItem(value)
		}
	}
	if observation.terminal {
		observation.commitIfSuccessful()
	}
}

func (observation *codexResponseObservation) observeOutputItem(item gjson.Result) {
	if !isCodexToolCallType(item.Get("type").String()) {
		return
	}
	observation.toolCallSeen = true
	if observation.toolCallIDs == nil {
		observation.toolCallIDs = make(map[string]struct{})
	}
	if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
		observation.toolCallIDs[callID] = struct{}{}
	}
}

func (observation *codexResponseObservation) isSuccessful() bool {
	return observation != nil && !observation.failed && observation.jsonPayloads > 0 &&
		(observation.terminal || observation.outputEvents > 0 || observation.toolCallSeen)
}

func (observation *codexResponseObservation) commitIfSuccessful() {
	if observation == nil || observation.committed || observation.onSuccess == nil || !observation.isSuccessful() {
		return
	}
	observation.committed = true
	observation.onSuccess()
}

func (prs *ProviderRelayService) commitCodexSessionAttempt(attempt *codexHistoryAttempt, observation *codexResponseObservation) {
	if attempt == nil || !observation.isSuccessful() {
		return
	}
	if attempt.cutoverPlan != nil {
		prs.rememberCodexHistorySanitize(attempt.cacheKey, attempt.cutoverPlan)
	}
	request := attempt.sessionRequest
	if request == nil {
		return
	}

	prs.codexSessionMu.Lock()
	if prs.codexSessionRoutes == nil {
		prs.codexSessionRoutes = make(map[codexSessionKey]codexSessionRouteState)
	}
	if len(prs.codexSessionRoutes) >= codexHistorySanitizeCacheMax {
		prs.pruneCodexSessionRoutesLocked(time.Now())
	}
	pendingCallIDs := make(map[string]struct{})
	pendingUnknownCall := false
	current, hasCurrent := prs.codexSessionRoutes[request.key]
	if hasCurrent && request.ownerProvider == "" && current.providerName != attempt.providerName {
		prs.codexSessionMu.Unlock()
		return
	}
	if hasCurrent && request.ownerProvider != "" &&
		current.providerName != request.ownerProvider && current.providerName != attempt.providerName {
		prs.codexSessionMu.Unlock()
		return
	}
	if hasCurrent && attempt.providerName == request.ownerProvider && current.providerName != attempt.providerName {
		prs.codexSessionMu.Unlock()
		return
	}
	if hasCurrent && current.providerName == attempt.providerName {
		for callID := range current.pendingCallIDs {
			pendingCallIDs[callID] = struct{}{}
		}
		pendingUnknownCall = current.pendingCalls && len(current.pendingCallIDs) == 0
	} else {
		for callID := range request.pendingCallIDs {
			pendingCallIDs[callID] = struct{}{}
		}
		pendingUnknownCall = request.pendingCalls && len(request.pendingCallIDs) == 0
	}
	if request.hasToolOutput {
		pendingUnknownCall = false
	}
	for callID := range request.requestToolOutputIDs {
		delete(pendingCallIDs, callID)
	}
	for callID := range observation.toolCallIDs {
		pendingCallIDs[callID] = struct{}{}
	}
	if observation.toolCallSeen && len(observation.toolCallIDs) == 0 {
		pendingUnknownCall = true
	}
	prs.codexSessionRoutes[request.key] = codexSessionRouteState{
		providerName:   attempt.providerName,
		pendingCallIDs: pendingCallIDs,
		pendingCalls:   pendingUnknownCall || len(pendingCallIDs) > 0,
		updatedAt:      time.Now(),
	}
	prs.codexSessionMu.Unlock()
}

func (prs *ProviderRelayService) pruneCodexSessionRoutesLocked(now time.Time) {
	for key, state := range prs.codexSessionRoutes {
		if now.Sub(state.updatedAt) >= codexSessionRouteTTL {
			delete(prs.codexSessionRoutes, key)
		}
	}
	if len(prs.codexSessionRoutes) < codexHistorySanitizeCacheMax {
		return
	}
	var oldestKey codexSessionKey
	var oldestTime time.Time
	for key, state := range prs.codexSessionRoutes {
		if oldestTime.IsZero() || state.updatedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = state.updatedAt
		}
	}
	delete(prs.codexSessionRoutes, oldestKey)
}
