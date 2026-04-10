package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	htmlstd "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daodao97/xgo/xrequest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	xhtml "golang.org/x/net/html"
)

var claudeWebSearchQueryPattern = regexp.MustCompile(`(?i)^\s*Perform a web search for the query:\s*(.+?)\s*$`)
var webSearchSiteOperatorPattern = regexp.MustCompile(`(?i)(^|\s)site:([^\s]+)`)

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

const (
	defaultBochaWebSearchURL = "https://api.bocha.cn/v1/web-search"
	defaultBochaTimeout      = 15 * time.Second
	defaultBingPrimeURL      = "https://cn.bing.com/?mkt=zh-CN&FORM=BEHPTB&ensearch=1"
	defaultBingSearchURL     = "https://cn.bing.com/search"
	defaultBingTimeout       = 15 * time.Second
	bingSessionRefreshTTL    = 10 * time.Minute
)

var defaultBlockedWebSearchDomains = []string{
	"csdn.net",
	"cnblogs.com",
}

var bingSessionMu sync.Mutex
var bingSessionClient *http.Client
var bingSessionPrimedAt time.Time

var bingPrimeURL = defaultBingPrimeURL
var bingSearchURL = defaultBingSearchURL
var executeBingWebSearch = bingWebSearch
var validateBingResponseHost = isSupportedBingHost

var runClaudeWebSearch = func(ctx context.Context, request claudeWebSearchFallbackRequest, maxResults int) ([]webSearchResult, error) {
	return bingWebSearchRequest(ctx, request, maxResults)
}

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

func deriveEffectiveClaudeWebSearchRequest(request claudeWebSearchFallbackRequest) claudeWebSearchFallbackRequest {
	effective := request
	extractedDomains, strippedQuery := extractSiteDomainsFromQuery(request.Query)
	effective.AllowedDomains = mergeWebSearchDomainLists(request.AllowedDomains, extractedDomains)
	effective.Query = strings.TrimSpace(strippedQuery)
	return effective
}

func extractSiteDomainsFromQuery(query string) ([]string, string) {
	matches := webSearchSiteOperatorPattern.FindAllStringSubmatch(query, -1)
	if len(matches) == 0 {
		return nil, strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
	}

	domains := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		if domain := normalizeWebSearchDomain(match[2]); domain != "" {
			domains = append(domains, domain)
		}
	}

	cleanedQuery := webSearchSiteOperatorPattern.ReplaceAllString(query, " ")
	cleanedQuery = strings.Join(strings.Fields(strings.TrimSpace(cleanedQuery)), " ")
	return mergeWebSearchDomainLists(domains), cleanedQuery
}

func buildBackendWebSearchQuery(request claudeWebSearchFallbackRequest) string {
	query := strings.TrimSpace(request.Query)
	if query == "" {
		return query
	}

	effectiveBlockedDomains := mergeWebSearchDomainLists(defaultBlockedWebSearchDomains, request.BlockedDomains)
	if len(request.AllowedDomains) == 0 {
		return appendBlockedSiteClauses(query, effectiveBlockedDomains)
	}

	if len(request.AllowedDomains) == 1 {
		query = "site:" + request.AllowedDomains[0] + " " + query
		return appendBlockedSiteClauses(query, effectiveBlockedDomains)
	}

	clauses := make([]string, 0, len(request.AllowedDomains))
	for _, domain := range request.AllowedDomains {
		clauses = append(clauses, "site:"+domain)
	}
	query = strings.Join(clauses, " OR ") + " " + query
	return appendBlockedSiteClauses(query, effectiveBlockedDomains)
}

func appendBlockedSiteClauses(query string, domains []string) string {
	query = strings.TrimSpace(query)
	if query == "" || len(domains) == 0 {
		return query
	}

	clauses := make([]string, 0, len(domains))
	for _, domain := range domains {
		if normalized := normalizeWebSearchDomain(domain); normalized != "" {
			clauses = append(clauses, "-site:"+normalized)
		}
	}
	if len(clauses) == 0 {
		return query
	}
	return query + " " + strings.Join(clauses, " ")
}

func bingWebSearchRequest(ctx context.Context, request claudeWebSearchFallbackRequest, maxResults int) ([]webSearchResult, error) {
	request = deriveEffectiveClaudeWebSearchRequest(request)

	if len(request.AllowedDomains) <= 1 {
		searchQueries := buildBingSearchQueries(request)
		return runBingSearchQueries(ctx, searchQueries, request, maxResults)
	}

	mergedResults := make([]webSearchResult, 0, maxResults)
	seenURLs := make(map[string]struct{})
	var lastErr error

	for _, domain := range request.AllowedDomains {
		domainRequest := request
		domainRequest.AllowedDomains = []string{domain}

		results, err := runBingSearchQueries(ctx, buildBingSearchQueries(domainRequest), domainRequest, maxResults)
		if err != nil {
			lastErr = err
			continue
		}

		for _, result := range results {
			if _, exists := seenURLs[result.URL]; exists {
				continue
			}
			seenURLs[result.URL] = struct{}{}
			mergedResults = append(mergedResults, result)
		}
	}

	if len(mergedResults) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("bing search returned no results")
	}
	return mergedResults, nil
}

func buildBingSearchQueries(request claudeWebSearchFallbackRequest) []string {
	effectiveBlockedDomains := mergeWebSearchDomainLists(defaultBlockedWebSearchDomains, request.BlockedDomains)
	baseQueries := generateWebSearchQueryVariants(request.Query)
	queries := make([]string, 0, len(baseQueries)+1)
	seen := map[string]struct{}{}

	for _, baseQuery := range baseQueries {
		variantRequest := request
		variantRequest.Query = baseQuery
		query := buildBackendWebSearchQuery(variantRequest)
		if query == "" {
			continue
		}
		if _, exists := seen[query]; exists {
			continue
		}
		seen[query] = struct{}{}
		queries = append(queries, query)
	}

	if len(request.AllowedDomains) == 1 {
		domainOnlyQuery := appendBlockedSiteClauses("site:"+request.AllowedDomains[0], effectiveBlockedDomains)
		if domainOnlyQuery != "" {
			if _, exists := seen[domainOnlyQuery]; !exists {
				seen[domainOnlyQuery] = struct{}{}
				queries = append(queries, domainOnlyQuery)
			}
		}
	}

	return queries
}

func generateWebSearchQueryVariants(query string) []string {
	query = strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
	if query == "" {
		return nil
	}

	variants := []string{query}

	if simplified := removeWebSearchTerms(query, "official", "website", "site"); simplified != "" && simplified != query {
		variants = append(variants, simplified)
	}
	if docsVariant := replaceWebSearchTerms(query, map[string]string{
		"docs": "documentation",
	}); docsVariant != "" && docsVariant != query {
		variants = append(variants, docsVariant)
	}
	if coreVariant := removeWebSearchTerms(query, "official", "website", "site", "documentation", "docs", "guide", "manual", "book"); coreVariant != "" && coreVariant != query {
		variants = append(variants, coreVariant)
	}
	if docsCoreVariant := removeWebSearchTerms(query, "official"); docsCoreVariant != "" && docsCoreVariant != query {
		variants = append(variants, docsCoreVariant)
	}

	deduped := make([]string, 0, len(variants))
	seen := map[string]struct{}{}
	for _, variant := range variants {
		variant = strings.Join(strings.Fields(strings.TrimSpace(variant)), " ")
		if variant == "" {
			continue
		}
		if _, exists := seen[variant]; exists {
			continue
		}
		seen[variant] = struct{}{}
		deduped = append(deduped, variant)
	}
	return deduped
}

func replaceWebSearchTerms(query string, replacements map[string]string) string {
	words := strings.Fields(query)
	for i, word := range words {
		normalized := normalizeWebSearchTerm(word)
		if replacement, exists := replacements[normalized]; exists {
			words[i] = replacement
		}
	}
	return strings.Join(words, " ")
}

func removeWebSearchTerms(query string, terms ...string) string {
	if query == "" || len(terms) == 0 {
		return strings.TrimSpace(query)
	}

	termSet := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if normalized := normalizeWebSearchTerm(term); normalized != "" {
			termSet[normalized] = struct{}{}
		}
	}

	words := strings.Fields(query)
	kept := make([]string, 0, len(words))
	for _, word := range words {
		if _, exists := termSet[normalizeWebSearchTerm(word)]; exists {
			continue
		}
		kept = append(kept, word)
	}
	return strings.Join(kept, " ")
}

func normalizeWebSearchTerm(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, " \t\r\n.,:;!?()[]{}<>\"'`")
	return value
}

func runBingSearchQueries(ctx context.Context, queries []string, request claudeWebSearchFallbackRequest, maxResults int) ([]webSearchResult, error) {
	type scoredResult struct {
		Result    webSearchResult
		Score     int
		FirstSeen int
	}

	bestResults := make(map[string]scoredResult)
	seenOrder := 0
	var lastErr error
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}

		results, err := executeBingWebSearch(ctx, query, maxResults)
		if err != nil {
			lastErr = err
			continue
		}

		for _, result := range results {
			score := scoreWebSearchResult(result, request)
			existing, exists := bestResults[result.URL]
			if !exists || score > existing.Score {
				bestResults[result.URL] = scoredResult{
					Result:    result,
					Score:     score,
					FirstSeen: seenOrder,
				}
			}
			seenOrder++
		}
	}

	if len(bestResults) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("bing search returned no results")
	}

	ranked := make([]scoredResult, 0, len(bestResults))
	for _, result := range bestResults {
		ranked = append(ranked, result)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].FirstSeen < ranked[j].FirstSeen
		}
		return ranked[i].Score > ranked[j].Score
	})

	merged := make([]webSearchResult, 0, min(len(ranked), maxResults))
	for _, result := range ranked {
		merged = append(merged, result.Result)
		if len(merged) >= maxResults {
			break
		}
	}
	return merged, nil
}

func scoreWebSearchResult(result webSearchResult, request claudeWebSearchFallbackRequest) int {
	title := strings.ToLower(result.Title)
	snippet := strings.ToLower(result.Snippet)
	url := strings.ToLower(result.URL)
	host := strings.ToLower(webSearchResultHostname(result.URL))
	score := 0

	for _, token := range tokenizeWebSearchIntent(request.Query) {
		if strings.Contains(title, token) {
			score += 12
		}
		if strings.Contains(url, token) {
			score += 8
		}
		if strings.Contains(snippet, token) {
			score += 4
		}
		if strings.Contains(host, token) {
			score += 10
		}
	}

	if len(request.AllowedDomains) > 0 && matchesWebSearchDomainList(host, request.AllowedDomains) {
		score += 40
	}
	if hasWebSearchDocumentationIntent(request.Query) && containsAnyWebSearchText(title+" "+snippet+" "+url, "docs", "documentation", "api", "reference", "manual", "guide", "book") {
		score += 10
	}
	if hasWebSearchOfficialIntent(request.Query) {
		if containsAnyWebSearchText(title+" "+snippet, "official") {
			score += 6
		}
		if hostContainsWebSearchIntentToken(host, request.Query) {
			score += 10
		}
	}
	if isLikelyLowValueWebSearchHost(host) {
		score -= 18
	}
	return score
}

func tokenizeWebSearchIntent(query string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= '0' && r <= '9':
			return false
		default:
			return true
		}
	})

	stopwords := map[string]struct{}{
		"official":      {},
		"website":       {},
		"site":          {},
		"documentation": {},
		"docs":          {},
		"guide":         {},
		"manual":        {},
		"latest":        {},
		"current":       {},
	}

	deduped := make([]string, 0, len(tokens))
	seen := map[string]struct{}{}
	for _, token := range tokens {
		if len(token) < 2 {
			continue
		}
		if _, exists := stopwords[token]; exists {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		deduped = append(deduped, token)
	}
	return deduped
}

func hasWebSearchDocumentationIntent(query string) bool {
	return containsAnyWebSearchText(strings.ToLower(query), "docs", "documentation", "api", "reference", "manual", "guide", "book")
}

func hasWebSearchOfficialIntent(query string) bool {
	return containsAnyWebSearchText(strings.ToLower(query), "official", "website")
}

func containsAnyWebSearchText(text string, values ...string) bool {
	text = strings.ToLower(text)
	for _, value := range values {
		if value != "" && strings.Contains(text, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

func hostContainsWebSearchIntentToken(host string, query string) bool {
	for _, token := range tokenizeWebSearchIntent(query) {
		if len(token) < 4 {
			continue
		}
		if strings.Contains(host, token) {
			return true
		}
	}
	return false
}

func isLikelyLowValueWebSearchHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case strings.Contains(host, "forum"):
		return true
	case strings.Contains(host, "zhidao.baidu.com"):
		return true
	case strings.Contains(host, "zhihu.com"):
		return true
	case strings.Contains(host, "csdn.net"):
		return true
	case strings.Contains(host, "cnblogs.com"):
		return true
	case strings.Contains(host, "douyin.com"):
		return true
	case strings.Contains(host, "weibo.com"):
		return true
	case strings.Contains(host, "qq.com"):
		return true
	case strings.Contains(host, "sohu.com"):
		return true
	case strings.Contains(host, "sina.com"):
		return true
	default:
		return false
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

	client, err := getOrCreateBingSessionClient()
	if err != nil {
		return nil, err
	}
	if err := ensureBingSessionPrimed(ctx, client, false); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := ensureBingSessionPrimed(ctx, client, true); err != nil {
				return nil, err
			}
		}

		results, err := bingWebSearchOnce(ctx, client, query, maxResults)
		if err == nil && len(results) > 0 {
			return results, nil
		}
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = fmt.Errorf("bing search returned no results")
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("bing search returned no results")
}

func isSupportedBingHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "www.bing.com" || host == "bing.com" || host == "cn.bing.com"
}

func getOrCreateBingSessionClient() (*http.Client, error) {
	bingSessionMu.Lock()
	defer bingSessionMu.Unlock()

	if bingSessionClient != nil {
		return bingSessionClient, nil
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	bingSessionClient = &http.Client{
		Timeout: defaultBingTimeout,
		Jar:     jar,
	}
	return bingSessionClient, nil
}

func ensureBingSessionPrimed(ctx context.Context, client *http.Client, force bool) error {
	if client == nil {
		return fmt.Errorf("bing client is nil")
	}

	bingSessionMu.Lock()
	shouldPrime := force ||
		bingSessionPrimedAt.IsZero() ||
		time.Since(bingSessionPrimedAt) > bingSessionRefreshTTL ||
		!hasBingSessionCookies(client.Jar)
	bingSessionMu.Unlock()

	if !shouldPrime {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bingPrimeURL, nil)
	if err != nil {
		return err
	}
	applyBingBrowserHeaders(req, false)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("bing session prime returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !validateBingResponseHost(strings.ToLower(strings.TrimSpace(resp.Request.URL.Hostname()))) {
		return fmt.Errorf("bing session prime landed on unsupported host: %s", resp.Request.URL.Hostname())
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256*1024))

	bingSessionMu.Lock()
	bingSessionPrimedAt = time.Now()
	bingSessionMu.Unlock()
	return nil
}

func hasBingSessionCookies(jar http.CookieJar) bool {
	if jar == nil {
		return false
	}

	primeURL, err := neturl.Parse(bingPrimeURL)
	if err != nil {
		return false
	}

	cookies := jar.Cookies(primeURL)
	for _, cookie := range cookies {
		switch cookie.Name {
		case "MUID", "SRCHHPGUSR", "_EDGE_S", "_HPVN":
			return true
		}
	}
	return false
}

func bingWebSearchOnce(ctx context.Context, client *http.Client, query string, maxResults int) ([]webSearchResult, error) {
	searchURL := buildBingSearchURL(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	applyBingBrowserHeaders(req, true)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("bing search returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !validateBingResponseHost(strings.ToLower(strings.TrimSpace(resp.Request.URL.Hostname()))) {
		return nil, fmt.Errorf("bing search landed on unsupported host: %s", resp.Request.URL.Hostname())
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

func buildBingSearchURL(query string) string {
	return bingSearchURL + "?q=" + neturl.QueryEscape(query) + "&cc=us&mkt=en-US&setlang=en-us&ensearch=1&FORM=BEHPTB"
}

func applyBingBrowserHeaders(req *http.Request, isSearch bool) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-User", "?1")
	if isSearch {
		req.Header.Set("Referer", bingPrimeURL)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		return
	}
	req.Header.Set("Sec-Fetch-Site", "none")
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

func mergeWebSearchDomainLists(lists ...[]string) []string {
	merged := make([]string, 0)
	seen := map[string]struct{}{}

	for _, list := range lists {
		for _, value := range list {
			domain := normalizeWebSearchDomain(value)
			if domain == "" {
				continue
			}
			if _, exists := seen[domain]; exists {
				continue
			}
			seen[domain] = struct{}{}
			merged = append(merged, domain)
		}
	}

	if len(merged) == 0 {
		return nil
	}
	return merged
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

	effectiveBlockedDomains := mergeWebSearchDomainLists(defaultBlockedWebSearchDomains, blockedDomains)
	filtered := make([]webSearchResult, 0, len(results))
	for _, result := range results {
		host := webSearchResultHostname(result.URL)
		if host == "" {
			continue
		}
		if matchesWebSearchDomainList(host, effectiveBlockedDomains) {
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

func buildAllowedDomainFallbackResults(allowedDomains []string) []webSearchResult {
	allowedDomains = mergeWebSearchDomainLists(allowedDomains)
	if len(allowedDomains) == 0 {
		return nil
	}

	results := make([]webSearchResult, 0, len(allowedDomains))
	for _, domain := range allowedDomains {
		if domain == "" {
			continue
		}
		results = append(results, webSearchResult{
			Title: domain,
			URL:   "https://" + domain + "/",
		})
	}
	return results
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
	effectiveRequest := deriveEffectiveClaudeWebSearchRequest(request)

	searchLimit := 5
	if len(effectiveRequest.AllowedDomains) > 0 || len(effectiveRequest.BlockedDomains) > 0 {
		searchLimit = 10
	}

	results, err := runClaudeWebSearch(ctx, effectiveRequest, searchLimit)
	if err != nil {
		return false, fmt.Errorf("local web_search fallback failed: %w", err)
	}

	results = filterWebSearchResultsByDomains(results, effectiveRequest.AllowedDomains, effectiveRequest.BlockedDomains)
	if len(results) == 0 && len(effectiveRequest.AllowedDomains) > 0 {
		results = buildAllowedDomainFallbackResults(effectiveRequest.AllowedDomains)
	}
	if len(results) > 5 {
		results = results[:5]
	}

	fmt.Printf("[WebSearchFallback] query=%q results=%d stream=%v allowed=%v blocked=%v\n", request.Query, len(results), isStream, effectiveRequest.AllowedDomains, effectiveRequest.BlockedDomains)

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
