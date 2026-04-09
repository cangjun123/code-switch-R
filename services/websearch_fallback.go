package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	htmlstd "html"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	xhtml "golang.org/x/net/html"
)

var claudeWebSearchQueryPattern = regexp.MustCompile(`(?i)^\s*Perform a web search for the query:\s*(.+?)\s*$`)

type claudeWebSearchFallbackRequest struct {
	Query          string
	AllowedDomains []string
	BlockedDomains []string
	ToolType       string
	ToolName       string
}

type webSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

var runClaudeWebSearch = func(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	return bochaWebSearch(ctx, query, maxResults)
}

const (
	defaultBochaWebSearchURL = "https://api.bocha.cn/v1/web-search"
	defaultBochaTimeout      = 15 * time.Second
)

type bochaWebSearchRequest struct {
	Query     string `json:"query"`
	Summary   bool   `json:"summary"`
	Freshness string `json:"freshness"`
	Count     int    `json:"count"`
}

type bochaWebSearchResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		WebPages struct {
			Value []struct {
				Name          string `json:"name"`
				URL           string `json:"url"`
				Snippet       string `json:"snippet"`
				Summary       string `json:"summary"`
				DatePublished string `json:"datePublished"`
			} `json:"value"`
		} `json:"webPages"`
	} `json:"data"`
}

func detectClaudeWebSearchFallbackRequest(body []byte) (claudeWebSearchFallbackRequest, bool) {
	req, err := decodeJSONObject(body)
	if err != nil {
		return claudeWebSearchFallbackRequest{}, false
	}
	tool, ok := findAnthropicWebSearchTool(req["tools"])
	if !ok {
		return claudeWebSearchFallbackRequest{}, false
	}

	query := extractClaudeWebSearchQuery(req["messages"])
	if query == "" {
		return claudeWebSearchFallbackRequest{}, false
	}

	return claudeWebSearchFallbackRequest{
		Query:          query,
		AllowedDomains: normalizeWebSearchDomains(asSlice(tool["allowed_domains"])),
		BlockedDomains: normalizeWebSearchDomains(asSlice(tool["blocked_domains"])),
		ToolType:       asString(tool["type"]),
		ToolName:       firstNonEmptyWebSearchString(asString(tool["name"]), "web_search"),
	}, true
}

func findAnthropicWebSearchTool(toolsValue interface{}) (map[string]interface{}, bool) {
	for _, rawTool := range asSlice(toolsValue) {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			continue
		}
		toolType := asString(tool["type"])
		toolName := asString(tool["name"])
		if strings.HasPrefix(toolType, "web_search") || toolName == "web_search" {
			return tool, true
		}
	}
	return nil, false
}

func extractClaudeWebSearchQuery(messagesValue interface{}) string {
	for _, rawMessage := range asSlice(messagesValue) {
		message, ok := rawMessage.(map[string]interface{})
		if !ok || asString(message["role"]) != "user" {
			continue
		}

		text := anthropicContentToText(message["content"])
		if text == "" {
			continue
		}

		if matches := claudeWebSearchQueryPattern.FindStringSubmatch(strings.TrimSpace(text)); len(matches) == 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func anthropicContentToText(content interface{}) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []interface{}:
		parts := make([]string, 0, len(value))
		for _, rawBlock := range value {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				continue
			}
			if asString(block["type"]) != "text" {
				continue
			}
			if text := strings.TrimSpace(asString(block["text"])); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func bochaWebSearch(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("web search query is empty")
	}
	if maxResults <= 0 {
		maxResults = 5
	}

	apiKey := firstNonEmptyWebSearchString(
		strings.TrimSpace(os.Getenv("BOCHA_API_KEY")),
		strings.TrimSpace(os.Getenv("BOCHA_SEARCH_API_KEY")),
	)
	if apiKey == "" {
		return nil, fmt.Errorf("bocha api key is not configured; set BOCHA_API_KEY")
	}

	endpoint := firstNonEmptyWebSearchString(
		strings.TrimSpace(os.Getenv("BOCHA_WEB_SEARCH_URL")),
		defaultBochaWebSearchURL,
	)
	requestBody := bochaWebSearchRequest{
		Query:     query,
		Summary:   true,
		Freshness: "noLimit",
		Count:     maxResults,
	}
	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: defaultBochaTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("bocha web search returned %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	results, err := parseBochaWebSearchResults(responseBody)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("bocha web search returned no results")
	}
	return results, nil
}

func parseBochaWebSearchResults(body []byte) ([]webSearchResult, error) {
	var response bochaWebSearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Code != 0 && response.Code != 200 {
		return nil, fmt.Errorf("bocha web search failed: code=%d msg=%s", response.Code, strings.TrimSpace(response.Msg))
	}

	items := response.Data.WebPages.Value
	results := make([]webSearchResult, 0, len(items))
	seenURLs := make(map[string]struct{}, len(items))
	for _, item := range items {
		url := strings.TrimSpace(item.URL)
		title := strings.TrimSpace(item.Name)
		if title == "" || url == "" {
			continue
		}
		if !isHTTPURL(url) {
			continue
		}
		if _, exists := seenURLs[url]; exists {
			continue
		}
		seenURLs[url] = struct{}{}

		snippet := strings.TrimSpace(item.Snippet)
		if snippet == "" {
			snippet = strings.TrimSpace(item.Summary)
		}
		results = append(results, webSearchResult{
			Title:   title,
			URL:     url,
			Snippet: snippet,
		})
	}
	return results, nil
}

func bingWebSearch(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("web search query is empty")
	}
	if maxResults <= 0 {
		maxResults = 5
	}

	searchURL := "https://www.bing.com/search?q=" + neturl.QueryEscape(query) + "&setlang=en-us&cc=us&ensearch=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 CodeSwitchWebSearch/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("bing search returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	htmlBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	results, err := parseBingSearchResultsHTML(string(htmlBody), maxResults)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("bing search returned no results")
	}
	return results, nil
}

func parseBingSearchResultsHTML(htmlBody string, maxResults int) ([]webSearchResult, error) {
	doc, err := xhtml.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return nil, err
	}

	results := make([]webSearchResult, 0, maxResults)
	seenURLs := make(map[string]struct{})

	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node == nil || len(results) >= maxResults {
			return
		}

		if node.Type == xhtml.ElementNode && node.Data == "li" && hasHTMLClass(node, "b_algo") {
			if result, ok := parseBingSearchResultNode(node); ok {
				if _, exists := seenURLs[result.URL]; !exists {
					seenURLs[result.URL] = struct{}{}
					results = append(results, result)
				}
			}
		}

		for child := node.FirstChild; child != nil && len(results) < maxResults; child = child.NextSibling {
			walk(child)
		}
	}

	walk(doc)
	return results, nil
}

func parseBingSearchResultNode(node *xhtml.Node) (webSearchResult, bool) {
	link := findHeadingAnchor(node)
	if link == nil {
		return webSearchResult{}, false
	}

	url := strings.TrimSpace(getHTMLAttr(link, "href"))
	title := normalizeHTMLText(extractHTMLText(link))
	if title == "" || url == "" {
		return webSearchResult{}, false
	}
	url = normalizeBingResultURL(url)
	if parsed, err := neturl.Parse(url); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return webSearchResult{}, false
	}

	snippet := normalizeHTMLText(extractFirstParagraphText(node))
	return webSearchResult{
		Title:   title,
		URL:     url,
		Snippet: snippet,
	}, true
}

func findHeadingAnchor(node *xhtml.Node) *xhtml.Node {
	var walk func(*xhtml.Node) *xhtml.Node
	walk = func(current *xhtml.Node) *xhtml.Node {
		if current == nil {
			return nil
		}
		if current.Type == xhtml.ElementNode && current.Data == "h2" {
			return findFirstAnchor(current)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if found := walk(child); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(node)
}

func findFirstAnchor(node *xhtml.Node) *xhtml.Node {
	if node == nil {
		return nil
	}
	if node.Type == xhtml.ElementNode && node.Data == "a" && strings.TrimSpace(getHTMLAttr(node, "href")) != "" {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findFirstAnchor(child); found != nil {
			return found
		}
	}
	return nil
}

func extractFirstParagraphText(node *xhtml.Node) string {
	var walk func(*xhtml.Node) string
	walk = func(current *xhtml.Node) string {
		if current == nil {
			return ""
		}
		if current.Type == xhtml.ElementNode && current.Data == "p" {
			if text := normalizeHTMLText(extractHTMLText(current)); text != "" {
				return text
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if text := walk(child); text != "" {
				return text
			}
		}
		return ""
	}
	return walk(node)
}

func extractHTMLText(node *xhtml.Node) string {
	var builder strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current == nil {
			return
		}
		if current.Type == xhtml.TextNode {
			builder.WriteString(current.Data)
			builder.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func normalizeHTMLText(text string) string {
	text = htmlstd.UnescapeString(text)
	text = strings.Join(strings.Fields(text), " ")
	return strings.TrimSpace(text)
}

func hasHTMLClass(node *xhtml.Node, className string) bool {
	for _, class := range strings.Fields(getHTMLAttr(node, "class")) {
		if class == className {
			return true
		}
	}
	return false
}

func getHTMLAttr(node *xhtml.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func normalizeBingResultURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	host := strings.ToLower(parsed.Hostname())
	if !strings.Contains(host, "bing.com") {
		return rawURL
	}

	for _, key := range []string{"u", "r", "url"} {
		if decoded := decodeBingRedirectTarget(parsed.Query().Get(key)); decoded != "" {
			return decoded
		}
	}

	return rawURL
}

func decodeBingRedirectTarget(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if unescaped, err := neturl.QueryUnescape(value); err == nil && isHTTPURL(unescaped) {
		return unescaped
	}

	trimmed := value
	if strings.HasPrefix(trimmed, "a1") {
		trimmed = trimmed[2:]
	}

	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(trimmed)
		if err != nil {
			continue
		}
		target := strings.TrimSpace(string(decoded))
		if isHTTPURL(target) {
			return target
		}
	}

	return ""
}

func isHTTPURL(value string) bool {
	parsed, err := neturl.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func normalizeWebSearchDomains(values []interface{}) []string {
	if len(values) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, rawValue := range values {
		domain := normalizeWebSearchDomain(asString(rawValue))
		if domain == "" {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func normalizeWebSearchDomain(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}

	if strings.Contains(value, "://") {
		if parsed, err := neturl.Parse(value); err == nil {
			value = parsed.Hostname()
		}
	} else {
		trimmed := strings.TrimPrefix(value, ".")
		if parsed, err := neturl.Parse("https://" + trimmed); err == nil && parsed.Hostname() != "" {
			value = parsed.Hostname()
		}
	}

	value = strings.Trim(value, ".")
	return value
}

func filterWebSearchResultsByDomains(results []webSearchResult, allowedDomains []string, blockedDomains []string) []webSearchResult {
	if len(results) == 0 {
		return nil
	}

	filtered := make([]webSearchResult, 0, len(results))
	for _, result := range results {
		host := webSearchResultHostname(result.URL)
		if host == "" {
			continue
		}
		if matchesWebSearchDomainList(host, blockedDomains) {
			continue
		}
		if len(allowedDomains) > 0 && !matchesWebSearchDomainList(host, allowedDomains) {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func webSearchResultHostname(rawURL string) string {
	parsed, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.Trim(strings.ToLower(parsed.Hostname()), ".")
}

func matchesWebSearchDomainList(host string, domains []string) bool {
	for _, domain := range domains {
		if matchesWebSearchDomain(host, domain) {
			return true
		}
	}
	return false
}

func matchesWebSearchDomain(host string, domain string) bool {
	host = strings.Trim(strings.ToLower(host), ".")
	domain = strings.Trim(strings.ToLower(domain), ".")
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func firstNonEmptyWebSearchString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func GenerateAnthropicServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
}

func buildClaudeWebSearchFallbackToolInput(request claudeWebSearchFallbackRequest) map[string]interface{} {
	input := map[string]interface{}{
		"query": request.Query,
	}
	if len(request.AllowedDomains) > 0 {
		input["allowed_domains"] = request.AllowedDomains
	}
	if len(request.BlockedDomains) > 0 {
		input["blocked_domains"] = request.BlockedDomains
	}
	return input
}

func buildClaudeWebSearchFallbackResultContent(results []webSearchResult) []interface{} {
	content := make([]interface{}, 0, len(results))
	for _, result := range results {
		content = append(content, map[string]interface{}{
			"type":  "search_result",
			"title": result.Title,
			"url":   result.URL,
		})
	}
	return content
}

func buildClaudeWebSearchFallbackContentBlocks(request claudeWebSearchFallbackRequest, results []webSearchResult) ([]interface{}, string) {
	toolUseID := GenerateAnthropicServerToolUseID()
	toolName := firstNonEmptyWebSearchString(request.ToolName, "web_search")
	input := buildClaudeWebSearchFallbackToolInput(request)
	resultContent := buildClaudeWebSearchFallbackResultContent(results)

	content := []interface{}{
		map[string]interface{}{
			"type":  "server_tool_use",
			"id":    toolUseID,
			"name":  toolName,
			"input": input,
		},
		map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     resultContent,
		},
	}

	return content, toolUseID
}

func writeClaudeWebSearchFallbackJSON(w http.ResponseWriter, model string, request claudeWebSearchFallbackRequest, results []webSearchResult, requestLog *ReqeustLog) error {
	content, _ := buildClaudeWebSearchFallbackContentBlocks(request, results)
	response := map[string]interface{}{
		"id":            GenerateAnthropicMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":                0,
			"output_tokens":               0,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
				"web_fetch_requests":  0,
			},
		},
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}

	requestLog.HttpCode = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return err
}

func writeClaudeWebSearchFallbackSSE(w http.ResponseWriter, model string, request claudeWebSearchFallbackRequest, results []webSearchResult, requestLog *ReqeustLog) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support streaming")
	}

	messageID := GenerateAnthropicMessageID()
	toolUseID := GenerateAnthropicServerToolUseID()
	toolName := firstNonEmptyWebSearchString(request.ToolName, "web_search")
	input := buildClaudeWebSearchFallbackToolInput(request)
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return err
	}
	resultContent := buildClaudeWebSearchFallbackResultContent(results)
	requestLog.HttpCode = http.StatusOK

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(event string, payload interface{}) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(body)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"server_tool_use": map[string]interface{}{
					"web_search_requests": 0,
					"web_fetch_requests":  0,
				},
			},
		},
	}); err != nil {
		return err
	}

	if err := writeEvent("content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type":  "server_tool_use",
			"id":    toolUseID,
			"name":  toolName,
			"input": map[string]interface{}{},
		},
	}); err != nil {
		return err
	}

	for _, chunk := range chunkTextByRune(string(inputJSON), 256) {
		if err := writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": chunk,
			},
		}); err != nil {
			return err
		}
	}

	if err := writeEvent("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}); err != nil {
		return err
	}

	if err := writeEvent("content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     resultContent,
		},
	}); err != nil {
		return err
	}

	if err := writeEvent("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 1,
	}); err != nil {
		return err
	}

	if err := writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": 0,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
				"web_fetch_requests":  0,
			},
		},
	}); err != nil {
		return err
	}

	return writeEvent("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

func chunkTextByRune(text string, chunkSize int) []string {
	if chunkSize <= 0 {
		chunkSize = 900
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}

	chunks := make([]string, 0, (len(runes)+chunkSize-1)/chunkSize)
	for start := 0; start < len(runes); start += chunkSize {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

func isUnsupportedWebSearchToolError(resp *xrequest.Response, reqErr error) bool {
	candidates := make([]string, 0, 3)
	if reqErr != nil {
		candidates = append(candidates, reqErr.Error())
	}
	if resp != nil {
		if respErr := resp.Error(); respErr != nil {
			candidates = append(candidates, respErr.Error())
		}
		if upstreamBody := extractUpstreamError(resp); upstreamBody != "" {
			candidates = append(candidates, upstreamBody)
		}
	}

	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		if strings.Contains(lower, "unsupported tool type") && strings.Contains(lower, "web_search_preview") {
			return true
		}
	}
	return false
}

func (prs *ProviderRelayService) serveClaudeWebSearchFallback(
	c *gin.Context,
	request claudeWebSearchFallbackRequest,
	isStream bool,
	model string,
	requestLog *ReqeustLog,
) (bool, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	searchLimit := 5
	if len(request.AllowedDomains) > 0 || len(request.BlockedDomains) > 0 {
		searchLimit = 10
	}

	results, err := runClaudeWebSearch(ctx, request.Query, searchLimit)
	if err != nil {
		return false, fmt.Errorf("local web_search fallback failed: %w", err)
	}

	results = filterWebSearchResultsByDomains(results, request.AllowedDomains, request.BlockedDomains)
	if len(results) > 5 {
		results = results[:5]
	}

	fmt.Printf("[WebSearchFallback] query=%q results=%d stream=%v allowed=%v blocked=%v\n", request.Query, len(results), isStream, request.AllowedDomains, request.BlockedDomains)

	if isStream {
		if err := writeClaudeWebSearchFallbackSSE(c.Writer, model, request, results, requestLog); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := writeClaudeWebSearchFallbackJSON(c.Writer, model, request, results, requestLog); err != nil {
		return false, err
	}
	return true, nil
}
