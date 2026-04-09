package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestDetectClaudeWebSearchFallbackRequest(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"system": "You are an assistant for performing a web search tool use",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type":"text","text":"Perform a web search for the query: Anthropic Claude Code documentation April 2026"}
				]
			}
		],
		"tools": [
			{
				"type":"web_search_20250305",
				"name":"web_search",
				"allowed_domains":["docs.anthropic.com","https://www.anthropic.com"],
				"blocked_domains":["status.anthropic.com"]
			}
		]
	}`)

	request, ok := detectClaudeWebSearchFallbackRequest(body)
	if !ok {
		t.Fatalf("expected fallback request to be detected")
	}
	if request.Query != "Anthropic Claude Code documentation April 2026" {
		t.Fatalf("query = %q", request.Query)
	}
	if request.ToolType != "web_search_20250305" {
		t.Fatalf("tool type = %q", request.ToolType)
	}
	if request.ToolName != "web_search" {
		t.Fatalf("tool name = %q", request.ToolName)
	}
	if !reflect.DeepEqual(request.AllowedDomains, []string{"docs.anthropic.com", "www.anthropic.com"}) {
		t.Fatalf("allowed domains = %#v", request.AllowedDomains)
	}
	if !reflect.DeepEqual(request.BlockedDomains, []string{"status.anthropic.com"}) {
		t.Fatalf("blocked domains = %#v", request.BlockedDomains)
	}
}

func TestDetectClaudeWebSearchFallbackRequestRejectsRegularPrompt(t *testing.T) {
	body := []byte(`{
		"messages": [
			{
				"role": "user",
				"content": [
					{"type":"text","text":"Search it for me"}
				]
			}
		],
		"tools": [
			{"type":"web_search_20250305","name":"web_search"}
		]
	}`)

	if _, ok := detectClaudeWebSearchFallbackRequest(body); ok {
		t.Fatalf("expected regular prompt not to trigger fallback")
	}
}

func TestParseBingSearchResultsHTML(t *testing.T) {
	html := `
<html><body>
<ul>
  <li class="b_algo">
    <h2><a href="https://www.bing.com/ck/a?!&&p=abc&u=a1aHR0cHM6Ly9kb2NzLmFudGhyb3BpYy5jb20vZW4vZG9jcy9jbGF1ZGUtY29kZQ&ntb=1">Claude Code overview - Anthropic</a></h2>
    <div class="b_caption"><p>Official Claude Code documentation.</p></div>
  </li>
  <li class="b_algo">
    <h2><a href="https://docs.anthropic.com/en/docs/claude-code/tutorials">Claude Code tutorials</a></h2>
    <div class="b_caption"><p>Getting started with Claude Code workflows.</p></div>
  </li>
</ul>
</body></html>`

	results, err := parseBingSearchResultsHTML(html, 5)
	if err != nil {
		t.Fatalf("parseBingSearchResultsHTML returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].URL != "https://docs.anthropic.com/en/docs/claude-code" {
		t.Fatalf("unexpected first url: %q", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "Official Claude Code documentation") {
		t.Fatalf("unexpected first snippet: %q", results[0].Snippet)
	}
}

func TestNormalizeBingResultURL(t *testing.T) {
	rawURL := "https://www.bing.com/ck/a?!&&p=abc&u=a1aHR0cHM6Ly9kb2NzLmFudGhyb3BpYy5jb20vZW4vZG9jcy9jbGF1ZGUtY29kZQ&ntb=1"
	if got := normalizeBingResultURL(rawURL); got != "https://docs.anthropic.com/en/docs/claude-code" {
		t.Fatalf("normalizeBingResultURL() = %q", got)
	}
}

func TestParseBochaWebSearchResults(t *testing.T) {
	body := []byte(`{
		"code": 200,
		"msg": null,
		"data": {
			"webPages": {
				"value": [
					{
						"name": "Claude Code 概览 - Claude Code Docs",
						"url": "https://code.claude.com/docs/zh-CN/overview",
						"snippet": "了解 Claude Code。",
						"summary": "Claude Code 概览的详细介绍。"
					},
					{
						"name": "Claude Code 概览 - Claude Code Docs",
						"url": "https://code.claude.com/docs/zh-CN/overview",
						"snippet": "duplicate"
					},
					{
						"name": "Claude Code overview - Anthropic",
						"url": "https://docs.anthropic.com/en/docs/claude-code",
						"summary": "Official Claude Code documentation."
					}
				]
			}
		}
	}`)

	results, err := parseBochaWebSearchResults(body)
	if err != nil {
		t.Fatalf("parseBochaWebSearchResults returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].URL != "https://code.claude.com/docs/zh-CN/overview" {
		t.Fatalf("unexpected first url: %q", results[0].URL)
	}
	if results[0].Snippet != "了解 Claude Code。" {
		t.Fatalf("unexpected first snippet: %q", results[0].Snippet)
	}
	if results[1].Snippet != "Official Claude Code documentation." {
		t.Fatalf("unexpected second snippet: %q", results[1].Snippet)
	}
}

func TestFilterWebSearchResultsByDomains(t *testing.T) {
	results := []webSearchResult{
		{Title: "Docs", URL: "https://docs.anthropic.com/en/docs/claude-code"},
		{Title: "Status", URL: "https://status.anthropic.com/"},
		{Title: "Example", URL: "https://example.com/"},
	}

	filtered := filterWebSearchResultsByDomains(results, []string{"anthropic.com"}, []string{"status.anthropic.com"})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered result, got %d", len(filtered))
	}
	if filtered[0].URL != "https://docs.anthropic.com/en/docs/claude-code" {
		t.Fatalf("unexpected filtered result: %#v", filtered[0])
	}
}

func TestWriteClaudeWebSearchFallbackSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	requestLog := &ReqeustLog{}
	request := claudeWebSearchFallbackRequest{
		Query:          "Claude Code",
		AllowedDomains: []string{"docs.anthropic.com"},
		ToolType:       "web_search_20250305",
		ToolName:       "web_search",
	}
	results := []webSearchResult{
		{
			Title: "Docs",
			URL:   "https://docs.anthropic.com/en/docs/claude-code",
		},
	}

	err := writeClaudeWebSearchFallbackSSE(recorder, "gpt-5.4", request, results, requestLog)
	if err != nil {
		t.Fatalf("writeClaudeWebSearchFallbackSSE returned error: %v", err)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		`"type":"server_tool_use"`,
		`"type":"input_json_delta"`,
		`"type":"web_search_tool_result"`,
		`"type":"search_result"`,
		`"tool_use_id":"srvtoolu_`,
		"https://docs.anthropic.com",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %q, got %s", want, body)
		}
	}
	if requestLog.HttpCode != 200 {
		t.Fatalf("requestLog.HttpCode = %d, want 200", requestLog.HttpCode)
	}
}

func TestIsUnsupportedWebSearchToolError(t *testing.T) {
	if !isUnsupportedWebSearchToolError(nil, assertableError("upstream status 400: {\"detail\":\"Unsupported tool type: web_search_preview\"}")) {
		t.Fatalf("expected unsupported tool type error to be detected")
	}
}

func TestClaudeWebSearchFallbackProxyHandlerJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRunClaudeWebSearch := runClaudeWebSearch
	runClaudeWebSearch = func(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
		return []webSearchResult{
			{
				Title:   "Claude Code overview - Anthropic",
				URL:     "https://docs.anthropic.com/en/docs/claude-code",
				Snippet: "Official Claude Code documentation.",
			},
		}, nil
	}
	defer func() {
		runClaudeWebSearch = originalRunClaudeWebSearch
	}()

	var upstreamCalled int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&upstreamCalled, 1)
		http.Error(w, `{"detail":"should not call upstream"}`, http.StatusBadGateway)
	}))
	defer upstreamServer.Close()

	providerService, relayService := newTestRelayService(t)
	err := providerService.SaveProviders("claude", []Provider{
		{
			ID:                   1,
			Name:                 "test-openai-responses",
			APIURL:               upstreamServer.URL,
			APIKey:               "test-upstream-key",
			Enabled:              true,
			APIEndpoint:          "/v1/responses",
			ModelMapping:         map[string]string{"claude-*": "gpt-5.4"},
			Level:                1,
			ConnectivityAuthType: "bearer",
			UpstreamProtocol:     "openai_chat",
			SupportsWebSearch:    true,
		},
	})
	if err != nil {
		t.Fatalf("SaveProviders failed: %v", err)
	}

	router := gin.New()
	relayService.registerRoutes(router)

	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":256,
		"stream":false,
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"Perform a web search for the query: Anthropic Claude Code documentation April 2026"}
				]
			}
		],
		"tools":[
			{"type":"web_search_20250305","name":"web_search"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "code-switch-r")
	req.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&upstreamCalled) != 0 {
		t.Fatalf("expected local fallback to bypass upstream")
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	bodyText := w.Body.String()
	if got := gjson.Get(bodyText, "content.0.type").String(); got != "server_tool_use" {
		t.Fatalf("content[0].type = %q, body = %s", got, bodyText)
	}
	toolUseID := gjson.Get(bodyText, "content.0.id").String()
	if !strings.HasPrefix(toolUseID, "srvtoolu_") {
		t.Fatalf("unexpected tool_use_id = %q, body = %s", toolUseID, bodyText)
	}
	if got := gjson.Get(bodyText, "content.1.type").String(); got != "web_search_tool_result" {
		t.Fatalf("content[1].type = %q, body = %s", got, bodyText)
	}
	if got := gjson.Get(bodyText, "content.1.tool_use_id").String(); got != toolUseID {
		t.Fatalf("content[1].tool_use_id = %q, want %q", got, toolUseID)
	}
	if got := gjson.Get(bodyText, "content.1.content.0.url").String(); got != "https://docs.anthropic.com/en/docs/claude-code" {
		t.Fatalf("content[1].content[0].url = %q, body = %s", got, bodyText)
	}
	if got := gjson.Get(bodyText, "usage.server_tool_use.web_search_requests").Int(); got != 1 {
		t.Fatalf("usage.server_tool_use.web_search_requests = %d, body = %s", got, bodyText)
	}
}

func TestClaudeWebSearchFallbackProxyHandlerSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRunClaudeWebSearch := runClaudeWebSearch
	runClaudeWebSearch = func(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
		return []webSearchResult{
			{
				Title:   "Claude Code overview - Anthropic",
				URL:     "https://docs.anthropic.com/en/docs/claude-code",
				Snippet: "Official Claude Code documentation.",
			},
		}, nil
	}
	defer func() {
		runClaudeWebSearch = originalRunClaudeWebSearch
	}()

	var upstreamCalled int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&upstreamCalled, 1)
		http.Error(w, `{"detail":"should not call upstream"}`, http.StatusBadGateway)
	}))
	defer upstreamServer.Close()

	providerService, relayService := newTestRelayService(t)
	err := providerService.SaveProviders("claude", []Provider{
		{
			ID:                   1,
			Name:                 "test-openai-responses",
			APIURL:               upstreamServer.URL,
			APIKey:               "test-upstream-key",
			Enabled:              true,
			APIEndpoint:          "/v1/responses",
			ModelMapping:         map[string]string{"claude-*": "gpt-5.4"},
			Level:                1,
			ConnectivityAuthType: "bearer",
			UpstreamProtocol:     "openai_chat",
			SupportsWebSearch:    true,
		},
	})
	if err != nil {
		t.Fatalf("SaveProviders failed: %v", err)
	}

	router := gin.New()
	relayService.registerRoutes(router)

	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":256,
		"stream":true,
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"Perform a web search for the query: Anthropic Claude Code documentation April 2026"}
				]
			}
		],
		"tools":[
			{"type":"web_search_20250305","name":"web_search"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "code-switch-r")
	req.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&upstreamCalled) != 0 {
		t.Fatalf("expected local fallback to bypass upstream")
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	bodyText := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"server_tool_use"`,
		"event: content_block_delta",
		`"type":"input_json_delta"`,
		`"type":"web_search_tool_result"`,
		"https://docs.anthropic.com/en/docs/claude-code",
		"event: message_stop",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected SSE body to contain %q, got %s", want, bodyText)
		}
	}
}

type assertableError string

func (e assertableError) Error() string {
	return string(e)
}
