package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// UpstreamInfoConfig is independent of the relay protocol and its accounting.
type UpstreamInfoConfig struct {
	Type    string `json:"type"`
	BaseURL string `json:"baseUrl,omitempty"`
}
type ProviderInfoRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type ProviderInfoDraft struct {
	APIURL       string              `json:"apiUrl"`
	APIKey       string              `json:"apiKey"`
	UpstreamInfo *UpstreamInfoConfig `json:"upstreamInfo"`
}

// Optional amounts remain nil when the upstream does not supply them.
type Sub2Quota struct {
	Limit       *float64 `json:"limit,omitempty"`
	Used        *float64 `json:"used,omitempty"`
	Remaining   *float64 `json:"remaining,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Window      string   `json:"window,omitempty"`
	WindowStart string   `json:"window_start,omitempty"`
	ResetAt     string   `json:"reset_at,omitempty"`
}
type Sub2Stats struct {
	Requests            *int64   `json:"requests,omitempty"`
	InputTokens         *int64   `json:"input_tokens,omitempty"`
	OutputTokens        *int64   `json:"output_tokens,omitempty"`
	CacheCreationTokens *int64   `json:"cache_creation_tokens,omitempty"`
	CacheWriteTokens    *int64   `json:"cache_write_tokens,omitempty"`
	CacheReadTokens     *int64   `json:"cache_read_tokens,omitempty"`
	TotalTokens         *int64   `json:"total_tokens,omitempty"`
	Cost                *float64 `json:"cost,omitempty"`
	ActualCost          *float64 `json:"actual_cost,omitempty"`
	Date                string   `json:"date,omitempty"`
	Model               string   `json:"model,omitempty"`
}
type Sub2UsageSummary struct {
	Today             *Sub2Stats `json:"today,omitempty"`
	Total             *Sub2Stats `json:"total,omitempty"`
	RPM               *float64   `json:"rpm,omitempty"`
	TPM               *float64   `json:"tpm,omitempty"`
	AverageDurationMS *float64   `json:"average_duration_ms,omitempty"`
}
type Sub2Subscription struct {
	DailyUsage        *float64 `json:"daily_usage_usd,omitempty"`
	WeeklyUsage       *float64 `json:"weekly_usage_usd,omitempty"`
	MonthlyUsage      *float64 `json:"monthly_usage_usd,omitempty"`
	DailyLimit        *float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimit       *float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimit      *float64 `json:"monthly_limit_usd,omitempty"`
	WeeklyWindowStart string   `json:"weekly_window_start,omitempty"`
	ExpiresAt         string   `json:"expires_at,omitempty"`
}
type Sub2Usage struct {
	Mode         string            `json:"mode,omitempty"`
	IsValid      *bool             `json:"isValid,omitempty"`
	Status       string            `json:"status,omitempty"`
	PlanName     string            `json:"planName,omitempty"`
	Unit         string            `json:"unit,omitempty"`
	Balance      *float64          `json:"balance,omitempty"`
	Remaining    *float64          `json:"remaining,omitempty"`
	Quota        *Sub2Quota        `json:"quota,omitempty"`
	RateLimits   []Sub2Quota       `json:"rate_limits,omitempty"`
	Subscription *Sub2Subscription `json:"subscription,omitempty"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	Usage        *Sub2UsageSummary `json:"usage,omitempty"`
	DailyUsage   []Sub2Stats       `json:"daily_usage,omitempty"`
	ModelStats   []Sub2Stats       `json:"model_stats,omitempty"`
}
type Sub2Billing struct {
	Object        string   `json:"object"`
	SchemaVersion int      `json:"schema_version"`
	BillingScope  string   `json:"billing_scope"`
	GroupRate     *float64 `json:"group_rate_multiplier,omitempty"`
	UserRate      *float64 `json:"user_rate_multiplier,omitempty"`
	ResolvedRate  *float64 `json:"resolved_rate_multiplier,omitempty"`
	EffectiveRate *float64 `json:"effective_rate_multiplier,omitempty"`
	PeakEnabled   bool     `json:"peak_rate_enabled"`
	PeakStart     string   `json:"peak_start,omitempty"`
	PeakEnd       string   `json:"peak_end,omitempty"`
	PeakRate      *float64 `json:"peak_rate_multiplier,omitempty"`
	AppliedPeak   *float64 `json:"applied_peak_multiplier,omitempty"`
	Timezone      string   `json:"timezone,omitempty"`
	ObservedAt    string   `json:"observed_at,omitempty"`
}
type ProviderInfoSection struct {
	Status    string `json:"status"` // ready, auth, unsupported, rate_limited, network, invalid_response
	UpdatedAt string `json:"updatedAt,omitempty"`
	RetryAt   string `json:"retryAt,omitempty"`
	Stale     bool   `json:"stale"`
}
type ProviderInfoResult struct {
	Usage         *Sub2Usage          `json:"usage,omitempty"`
	Billing       *Sub2Billing        `json:"billing,omitempty"`
	UsageState    ProviderInfoSection `json:"usageState"`
	BillingState  ProviderInfoSection `json:"billingState"`
	DailyTimezone string              `json:"dailyTimezone"`
	ModelPeriod   string              `json:"modelPeriod"`
}
type providerInfoCache struct {
	data    json.RawMessage
	state   ProviderInfoSection
	expires time.Time
	retry   time.Time
}
type ProviderInfoService struct {
	providers *ProviderService
	gemini    *GeminiService
	client    *http.Client
	mu        sync.Mutex
	cache     map[string]providerInfoCache
	flights   singleflight.Group
	slots     chan struct{}
}

func NewProviderInfoService(providers *ProviderService, gemini *GeminiService) *ProviderInfoService {
	return &ProviderInfoService{providers: providers, gemini: gemini,
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cache:  make(map[string]providerInfoCache), slots: make(chan struct{}, 4)}
}
func (s *ProviderInfoService) GetInfo(ref ProviderInfoRef, forceRefresh bool, timezone string) (*ProviderInfoResult, error) {
	var draft ProviderInfoDraft
	found := false
	if ref.Kind == "gemini" && s.gemini != nil {
		for _, p := range s.gemini.GetProviders() {
			if p.ID == ref.ID {
				draft = ProviderInfoDraft{p.BaseURL, p.APIKey, p.UpstreamInfo}
				found = true
				break
			}
		}
	} else if s.providers != nil {
		providers, err := s.providers.LoadProviders(ref.Kind)
		if err != nil {
			return nil, errors.New("provider_unavailable")
		}
		for _, p := range providers {
			if strconv.FormatInt(p.ID, 10) == ref.ID {
				draft = ProviderInfoDraft{p.APIURL, p.APIKey, p.UpstreamInfo}
				found = true
				break
			}
		}
	}
	if !found {
		return nil, errors.New("provider_not_found")
	}
	return s.query(draft, ref.Kind+":"+ref.ID, forceRefresh, timezone)
}
func (s *ProviderInfoService) TestConnection(draft ProviderInfoDraft, timezone string) (*ProviderInfoResult, error) {
	return s.query(draft, "preview", true, timezone)
}
func providerInfoBase(draft ProviderInfoDraft) (string, error) {
	if draft.UpstreamInfo == nil || draft.UpstreamInfo.Type != "sub2api" {
		return "", errors.New("info_disabled")
	}
	raw := strings.TrimSpace(draft.UpstreamInfo.BaseURL)
	explicit := raw != ""
	if !explicit {
		raw = strings.TrimSpace(draft.APIURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid_url")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !explicit {
		u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/v1beta"), "/v1")
	}
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}
func (s *ProviderInfoService) query(draft ProviderInfoDraft, ref string, force bool, tz string) (*ProviderInfoResult, error) {
	base, err := providerInfoBase(draft)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(draft.APIKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("missing_key")
	}
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, errors.New("invalid_timezone")
	}
	// Hash the complete identity: no plaintext credentials in cache keys or errors.
	identity, _ := json.Marshal([]string{ref, base, key, tz})
	sum := sha256.Sum256(identity)
	cacheKey := hex.EncodeToString(sum[:])
	result := &ProviderInfoResult{DailyTimezone: tz, ModelPeriod: "upstream_last_30_days"}
	var usage, billing providerInfoCache
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		usage = s.section(cacheKey+":usage", base+"/v1/usage?days=30&timezone="+url.QueryEscape(tz), key, force, false)
	}()
	go func() {
		defer wg.Done()
		billing = s.section(cacheKey+":billing", base+"/v1/sub2api/billing", key, force, true)
	}()
	wg.Wait()
	result.UsageState = usage.state
	result.BillingState = billing.state
	if len(usage.data) > 0 {
		_ = json.Unmarshal(usage.data, &result.Usage)
	}
	if len(billing.data) > 0 {
		_ = json.Unmarshal(billing.data, &result.Billing)
	}
	return result, nil
}
func (s *ProviderInfoService) section(cacheKey, endpoint, key string, force, billing bool) providerInfoCache {
	s.mu.Lock()
	cached, ok := s.cache[cacheKey]
	s.mu.Unlock()
	if ok && (time.Now().Before(cached.retry) || (!force && time.Now().Before(cached.expires))) {
		return cached
	}
	value, _, _ := s.flights.Do(cacheKey, func() (any, error) {
		s.mu.Lock()
		previous, ok := s.cache[cacheKey]
		s.mu.Unlock()
		// A completed overlapping refresh must not trigger a second request.
		if ok && (time.Now().Before(previous.retry) || (!force && time.Now().Before(previous.expires)) || previous.expires.After(cached.expires)) {
			return previous, nil
		}
		next := s.fetch(endpoint, key, billing)
		now := time.Now()
		next.expires = now.Add(5 * time.Minute)
		if next.state.Status != "ready" {
			next.data = previous.data
			next.state.UpdatedAt = previous.state.UpdatedAt
			next.state.Stale = len(next.data) > 0
		}
		s.mu.Lock()
		// Bound memory even when configurations or preview credentials change repeatedly.
		if len(s.cache) >= 512 {
			for k, v := range s.cache {
				if now.After(v.expires.Add(time.Hour)) && now.After(v.retry) {
					delete(s.cache, k)
				}
			}
			if len(s.cache) >= 512 {
				for k := range s.cache {
					delete(s.cache, k)
					break
				}
			}
		}
		s.cache[cacheKey] = next
		s.mu.Unlock()
		return next, nil
	})
	return value.(providerInfoCache)
}
func (s *ProviderInfoService) fetch(endpoint, key string, billing bool) providerInfoCache {
	out := providerInfoCache{state: ProviderInfoSection{Status: "network"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 401, 403:
		out.state.Status = "auth"
		return out
	case 404, 405:
		out.state.Status = "unsupported"
		return out
	case 429:
		out.state.Status = "rate_limited"
		out.retry = time.Now().Add(5 * time.Minute)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 && seconds <= 86400*365 {
			out.retry = time.Now().Add(time.Duration(seconds) * time.Second)
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && at.After(time.Now()) {
			out.retry = at
		}
		out.state.RetryAt = out.retry.UTC().Format(time.RFC3339)
		return out
	}
	if resp.StatusCode != http.StatusOK {
		out.state.Status = "upstream_error"
		return out
	}
	out.state.Status = "invalid_response"
	const maxBody = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return out
	}
	// Decode into an allowlist of fields; never forward raw error bodies or account metadata.
	if billing {
		var data Sub2Billing
		if json.Unmarshal(body, &data) != nil || data.Object != "sub2api.key_billing" || data.SchemaVersion != 1 || data.EffectiveRate == nil {
			return out
		}
		out.data, _ = json.Marshal(data)
	} else {
		var data Sub2Usage
		if json.Unmarshal(body, &data) != nil || (data.Mode != "quota_limited" && data.Mode != "unrestricted") || data.IsValid == nil {
			return out
		}
		out.data, _ = json.Marshal(data)
	}
	out.state.Status = "ready"
	out.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return out
}
