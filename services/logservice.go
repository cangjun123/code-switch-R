package services

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	modelpricing "codeswitch/resources/model-pricing"

	"github.com/daodao97/xgo/xdb"
)

const timeLayout = "2006-01-02 15:04:05"

const (
	defaultRequestLogRetentionDays = 30
	requestLogRetentionSettingKey  = "request_log_retention_days"
)

type LogService struct {
	pricing *modelpricing.Service
}

func (ls *LogService) CostSince(start string, platform string, timeZone string) (float64, error) {
	startTime, err := parseTimeInput(start, resolveLogLocation(timeZone))
	if err != nil {
		return 0, err
	}
	total := 0.0
	err = ls.visitRequestLogs(startTime, time.Time{}, platform, func(_ ReqeustLog, _ time.Time, _ modelpricing.UsageSnapshot, cost modelpricing.CostBreakdown) {
		total += cost.TotalCost
	})
	return total, err
}

func NewLogService() *LogService {
	svc, err := modelpricing.DefaultService()
	if err != nil {
		log.Printf("pricing service init failed: %v", err)
	}
	return &LogService{pricing: svc}
}

func (ls *LogService) ListRequestLogs(platform string, provider string, limit int, timeZone string) ([]ReqeustLog, error) {
	loc := resolveLogLocation(timeZone)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	activeLogs := defaultActiveRequestTracker.List(platform, provider, loc)
	model := xdb.New("request_log")
	options := []xdb.Option{
		xdb.OrderByDesc("id"),
		xdb.Limit(limit),
	}
	if platform != "" {
		options = append(options, xdb.WhereEq("platform", platform))
	}
	if provider != "" {
		options = append(options, xdb.WhereEq("provider", provider))
	}
	records, err := model.Selects(options...)
	if err != nil {
		return nil, err
	}
	calculator, err := ls.costCalculator()
	if err != nil {
		return nil, err
	}
	logs := make([]ReqeustLog, 0, len(records))
	for _, record := range records {
		createdAt := record.GetString("created_at")
		if parsed, ok := parseCreatedAt(record, loc); ok {
			createdAt = parsed.Format(timeLayout)
		}
		logEntry := ReqeustLog{
			ID:                    record.GetInt64("id"),
			Platform:              record.GetString("platform"),
			Model:                 record.GetString("model"),
			RequestedModel:        record.GetString("requested_model"),
			ResponseModel:         record.GetString("response_model"),
			RelayKeyName:          record.GetString("relay_key_name"),
			Provider:              record.GetString("provider"),
			RelayKeyID:            record.GetString("relay_key_id"),
			HttpCode:              record.GetInt("http_code"),
			InputTokens:           record.GetInt("input_tokens"),
			OutputTokens:          record.GetInt("output_tokens"),
			CacheCreateTokens:     record.GetInt("cache_create_tokens"),
			CacheReadTokens:       record.GetInt("cache_read_tokens"),
			ReasoningTokens:       record.GetInt("reasoning_tokens"),
			CreatedAt:             createdAt,
			IsStream:              record.GetBool("is_stream"),
			DurationSec:           record.GetFloat64("duration_sec"),
			FirstTokenDurationSec: record.GetFloat64("first_token_duration_sec"),
			ClientIP:              record.GetString("client_ip"),
			ErrorMessage:          record.GetString("error_message"),
			Status:                requestLogStatusCompleted,
		}
		decorateLogCost(&logEntry, calculator.calculate(&logEntry, requestLogUsage(&logEntry)))
		logs = append(logs, logEntry)
	}
	if len(activeLogs) == 0 {
		return logs, nil
	}
	merged := make([]ReqeustLog, 0, len(activeLogs)+len(logs))
	merged = append(merged, activeLogs...)
	merged = append(merged, logs...)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (ls *LogService) ListProviders(platform string) ([]string, error) {
	model := xdb.New("request_log")
	options := []xdb.Option{
		xdb.Field("DISTINCT provider as provider"),
		xdb.WhereNotEq("provider", ""),
		xdb.OrderByAsc("provider"),
	}
	if platform != "" {
		options = append(options, xdb.WhereEq("platform", platform))
	}
	records, err := model.Selects(options...)
	if err != nil {
		return nil, err
	}
	providers := make([]string, 0, len(records))
	for _, record := range records {
		name := strings.TrimSpace(record.GetString("provider"))
		if name != "" {
			providers = append(providers, name)
		}
	}
	return providers, nil
}

func (ls *LogService) HeatmapStats(days int, timeZone string) ([]HeatmapStat, error) {
	loc := resolveLogLocation(timeZone)
	if days <= 0 {
		days = 30
	}
	end := startOfHour(time.Now().In(loc)).Add(time.Hour)
	start := end.Add(-time.Duration(days*24) * time.Hour)
	buckets := map[int64]*HeatmapStat{}
	err := ls.visitRequestLogs(start, end, "", func(_ ReqeustLog, at time.Time, u modelpricing.UsageSnapshot, cost modelpricing.CostBreakdown) {
		hour := startOfHour(at.In(loc))
		key := hour.Unix()
		bucket := buckets[key]
		if bucket == nil {
			bucket = &HeatmapStat{Day: hour.Format(timeLayout)}
			buckets[key] = bucket
		}
		bucket.TotalRequests++
		bucket.InputTokens += usageInput(u)
		bucket.OutputTokens += usageOutput(u)
		bucket.ReasoningTokens += int64(u.ReasoningTokens)
		bucket.TotalCost += cost.TotalCost
	})
	if err != nil {
		return nil, err
	}
	keys := make([]int64, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	result := make([]HeatmapStat, 0, len(keys))
	for _, key := range keys {
		result = append(result, *buckets[key])
	}
	return result, nil
}

func (ls *LogService) StatsSince(platform string, timeZone string) (LogStats, error) {
	loc := resolveLogLocation(timeZone)
	start := startOfDay(time.Now().In(loc))
	end := start.AddDate(0, 0, 1)
	stats := LogStats{Series: make([]LogStatsSeries, 0, 25)}
	for at := start; at.Before(end); at = at.Add(time.Hour) {
		stats.Series = append(stats.Series, LogStatsSeries{Day: at.Format(timeLayout)})
	}
	err := ls.visitRequestLogs(start, end, platform, func(_ ReqeustLog, at time.Time, u modelpricing.UsageSnapshot, cost modelpricing.CostBreakdown) {
		index := int(at.In(loc).Sub(start) / time.Hour)
		if index < 0 || index >= len(stats.Series) {
			return
		}
		bucket := &stats.Series[index]
		bucket.TotalRequests++
		bucket.InputTokens += usageInput(u)
		bucket.OutputTokens += usageOutput(u)
		bucket.ReasoningTokens += int64(u.ReasoningTokens)
		bucket.CacheCreateTokens += int64(u.CacheCreateTokens)
		bucket.CacheReadTokens += int64(u.CacheReadTokens)
		bucket.TotalCost += cost.TotalCost
		if !cost.HasPricing {
			bucket.UnpricedRequests++
			stats.UnpricedRequests++
		}
		stats.TotalRequests++
		stats.InputTokens += usageInput(u)
		stats.OutputTokens += usageOutput(u)
		stats.ReasoningTokens += int64(u.ReasoningTokens)
		stats.CacheCreateTokens += int64(u.CacheCreateTokens)
		stats.CacheReadTokens += int64(u.CacheReadTokens)
		stats.CostInput += cost.InputCost
		stats.CostOutput += cost.OutputCost + cost.ReasoningCost
		stats.CostCacheCreate += cost.CacheCreateCost
		stats.CostCacheRead += cost.CacheReadCost
		stats.CostTotal += cost.TotalCost
	})
	return stats, err
}

func (ls *LogService) ProviderDailyStats(platform string, timeZone string) ([]ProviderDailyStat, error) {
	start := startOfDay(time.Now().In(resolveLogLocation(timeZone)))
	end := start.AddDate(0, 0, 1)
	buckets := map[string]*ProviderDailyStat{}
	err := ls.visitRequestLogs(start, end, platform, func(entry ReqeustLog, _ time.Time, u modelpricing.UsageSnapshot, cost modelpricing.CostBreakdown) {
		provider := strings.TrimSpace(entry.Provider)
		if provider == "" {
			provider = "(unknown)"
		}
		stat := buckets[provider]
		if stat == nil {
			stat = &ProviderDailyStat{Provider: provider}
			buckets[provider] = stat
		}
		stat.TotalRequests++
		if entry.HttpCode >= 200 && entry.HttpCode < 300 {
			stat.SuccessfulRequests++
		} else {
			stat.FailedRequests++
		}
		stat.InputTokens += usageInput(u)
		stat.OutputTokens += usageOutput(u)
		stat.ReasoningTokens += int64(u.ReasoningTokens)
		stat.CacheCreateTokens += int64(u.CacheCreateTokens)
		stat.CacheReadTokens += int64(u.CacheReadTokens)
		stat.CostTotal += cost.TotalCost
		if !cost.HasPricing {
			stat.UnpricedRequests++
		}
	})
	if err != nil {
		return nil, err
	}
	result := make([]ProviderDailyStat, 0, len(buckets))
	for _, stat := range buckets {
		stat.SuccessRate = float64(stat.SuccessfulRequests) / float64(stat.TotalRequests)
		result = append(result, *stat)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TotalRequests == result[j].TotalRequests {
			return result[i].Provider < result[j].Provider
		}
		return result[i].TotalRequests > result[j].TotalRequests
	})
	return result, nil
}

func (ls *LogService) GetRequestLogRetentionDays() (int, error) {
	return getConfiguredRequestLogRetentionDays()
}

func (ls *LogService) SetRequestLogRetentionDays(days int) error {
	if days < 0 || days > 3650 {
		return fmt.Errorf("日志保留天数必须在 0-3650 之间，0 表示关闭自动清理")
	}
	db, err := xdb.DB("default")
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, requestLogRetentionSettingKey, strconv.Itoa(days))
	if err != nil {
		return fmt.Errorf("更新日志保留天数失败: %w", err)
	}
	return nil
}

func (ls *LogService) GetRequestLogMaintenanceInfo(retentionDays int) (RequestLogMaintenanceInfo, error) {
	days, err := resolveRequestLogRetentionDays(retentionDays)
	if err != nil {
		return RequestLogMaintenanceInfo{}, err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return RequestLogMaintenanceInfo{}, err
	}

	info := RequestLogMaintenanceInfo{
		RetentionDays: days,
		DatabasePath:  requestLogDatabasePath(),
	}
	info.DatabaseSizeBytes = fileSize(info.DatabasePath)
	info.WALSizeBytes = fileSize(info.DatabasePath + "-wal")
	info.SHMSizeBytes = fileSize(info.DatabasePath + "-shm")

	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&info.TotalRows); err != nil {
		if isNoSuchTableErr(err) {
			return info, nil
		}
		return info, err
	}
	if err := db.QueryRow(`SELECT COALESCE(MIN(created_at), ''), COALESCE(MAX(created_at), '') FROM request_log`).Scan(&info.OldestCreatedAt, &info.NewestCreatedAt); err != nil {
		return info, err
	}
	if days > 0 {
		info.Cutoff = requestLogRetentionCutoff(days).Format(timeLayout)
		if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE created_at < ?`, info.Cutoff).Scan(&info.ExpiredRows); err != nil {
			return info, err
		}
	}
	info.ManualVacuumRecommended = info.ExpiredRows > 0 || info.WALSizeBytes > 128*1024*1024
	return info, nil
}

func (ls *LogService) CleanupRequestLogs(retentionDays int) (RequestLogCleanupResult, error) {
	days, err := resolveRequestLogRetentionDays(retentionDays)
	if err != nil {
		return RequestLogCleanupResult{}, err
	}
	result := RequestLogCleanupResult{
		RetentionDays: days,
		DatabasePath:  requestLogDatabasePath(),
	}
	if days <= 0 {
		return result, nil
	}
	cutoff := requestLogRetentionCutoff(days).Format(timeLayout)
	result.Cutoff = cutoff

	db, err := xdb.DB("default")
	if err != nil {
		return result, err
	}
	res, err := db.Exec(`DELETE FROM request_log WHERE created_at < ?`, cutoff)
	if err != nil {
		if isNoSuchTableErr(err) {
			return result, nil
		}
		return result, fmt.Errorf("清理 request_log 失败: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil {
		result.DeletedRows = affected
	}
	result.DatabaseSizeBytes = fileSize(result.DatabasePath)
	result.WALSizeBytes = fileSize(result.DatabasePath + "-wal")
	result.ManualVacuumRecommended = result.DeletedRows > 0
	return result, nil
}

func resolveLogLocation(timeZone string) *time.Location {
	timeZone = strings.TrimSpace(timeZone)
	if timeZone != "" {
		if loc, err := time.LoadLocation(timeZone); err == nil {
			return loc
		}
	}
	return time.Local
}

func formatSQLiteUTC(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseCreatedAt(record xdb.Record, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	if t := record.GetTime("created_at"); t != nil {
		return t.In(loc), true
	}
	raw := strings.TrimSpace(record.GetString("created_at"))
	if raw == "" {
		return time.Time{}, false
	}

	if parsed, err := parseStoredLogTime(raw); err == nil {
		return parsed.In(loc), true
	}

	if len(raw) >= len("2006-01-02") {
		if parsed, err := time.ParseInLocation("2006-01-02", raw[:10], loc); err == nil {
			return parsed, false
		}
	}

	return time.Time{}, false
}

func parseTimeInput(value string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	raw := strings.TrimSpace(value)
	if raw == "" {
		return startOfDay(time.Now().In(loc)), nil
	}

	localLayouts := []string{
		timeLayout,
		"2006-01-02T15:04:05",
	}
	for _, layout := range localLayouts {
		if parsed, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return parsed, nil
		}
	}

	zoneLayouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02T15:04:05-0700",
	}
	for _, layout := range zoneLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.In(loc), nil
		}
	}

	if normalized := strings.Replace(raw, " ", "T", 1); normalized != raw {
		if parsed, err := time.Parse(time.RFC3339, normalized); err == nil {
			return parsed.In(loc), nil
		}
	}

	if len(raw) >= len("2006-01-02") {
		if parsed, err := time.ParseInLocation("2006-01-02", raw[:10], loc); err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, fmt.Errorf("invalid time format: %s", raw)
}

func dayFromTimestamp(value string) string {
	if len(value) >= len("2006-01-02") {
		if t, err := time.ParseInLocation(timeLayout, value, time.Local); err == nil {
			return t.Format("2006-01-02")
		}
		return value[:10]
	}
	return value
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func startOfHour(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, t.Hour(), 0, 0, 0, t.Location())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseStoredLogTime(value string) (time.Time, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	utcLayouts := []string{
		timeLayout,
		"2006-01-02T15:04:05",
	}
	for _, layout := range utcLayouts {
		if parsed, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return parsed, nil
		}
	}

	zoneLayouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02T15:04:05-0700",
	}
	for _, layout := range zoneLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time format: %s", raw)
}

func resolveRequestLogRetentionDays(days int) (int, error) {
	if days > 0 {
		if days > 3650 {
			return 0, fmt.Errorf("日志保留天数不能超过 3650")
		}
		return days, nil
	}
	if days < 0 {
		return 0, fmt.Errorf("日志保留天数不能为负数")
	}
	return getConfiguredRequestLogRetentionDays()
}

func getConfiguredRequestLogRetentionDays() (int, error) {
	db, err := xdb.DB("default")
	if err != nil {
		return defaultRequestLogRetentionDays, err
	}
	var value string
	err = db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, requestLogRetentionSettingKey).Scan(&value)
	if err != nil {
		return defaultRequestLogRetentionDays, nil
	}
	days, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || days < 0 || days > 3650 {
		return defaultRequestLogRetentionDays, nil
	}
	return days, nil
}

func requestLogRetentionCutoff(days int) time.Time {
	return time.Now().AddDate(0, 0, -days)
}

func requestLogDatabasePath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".code-switch", "app.db")
	}
	return filepath.Join(home, ".code-switch", "app.db")
}

func fileSize(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func isNoSuchTableErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}

type HeatmapStat struct {
	Day             string  `json:"day"`
	TotalRequests   int64   `json:"total_requests"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	TotalCost       float64 `json:"total_cost"`
}

type LogStats struct {
	UnpricedRequests  int64            `json:"unpriced_requests"`
	TotalRequests     int64            `json:"total_requests"`
	InputTokens       int64            `json:"input_tokens"`
	OutputTokens      int64            `json:"output_tokens"`
	ReasoningTokens   int64            `json:"reasoning_tokens"`
	CacheCreateTokens int64            `json:"cache_create_tokens"`
	CacheReadTokens   int64            `json:"cache_read_tokens"`
	CostTotal         float64          `json:"cost_total"`
	CostInput         float64          `json:"cost_input"`
	CostOutput        float64          `json:"cost_output"`
	CostCacheCreate   float64          `json:"cost_cache_create"`
	CostCacheRead     float64          `json:"cost_cache_read"`
	Series            []LogStatsSeries `json:"series"`
}

type ProviderDailyStat struct {
	UnpricedRequests   int64   `json:"unpriced_requests"`
	Provider           string  `json:"provider"`
	TotalRequests      int64   `json:"total_requests"`
	SuccessfulRequests int64   `json:"successful_requests"`
	FailedRequests     int64   `json:"failed_requests"`
	SuccessRate        float64 `json:"success_rate"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	CacheCreateTokens  int64   `json:"cache_create_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CostTotal          float64 `json:"cost_total"`
}

type RequestLogMaintenanceInfo struct {
	RetentionDays           int    `json:"retention_days"`
	TotalRows               int64  `json:"total_rows"`
	ExpiredRows             int64  `json:"expired_rows"`
	OldestCreatedAt         string `json:"oldest_created_at"`
	NewestCreatedAt         string `json:"newest_created_at"`
	Cutoff                  string `json:"cutoff"`
	DatabasePath            string `json:"database_path"`
	DatabaseSizeBytes       int64  `json:"database_size_bytes"`
	WALSizeBytes            int64  `json:"wal_size_bytes"`
	SHMSizeBytes            int64  `json:"shm_size_bytes"`
	ManualVacuumRecommended bool   `json:"manual_vacuum_recommended"`
}

type RequestLogCleanupResult struct {
	RetentionDays           int    `json:"retention_days"`
	Cutoff                  string `json:"cutoff"`
	DeletedRows             int64  `json:"deleted_rows"`
	DatabasePath            string `json:"database_path"`
	DatabaseSizeBytes       int64  `json:"database_size_bytes"`
	WALSizeBytes            int64  `json:"wal_size_bytes"`
	ManualVacuumRecommended bool   `json:"manual_vacuum_recommended"`
}

type LogStatsSeries struct {
	UnpricedRequests  int64   `json:"unpriced_requests"`
	Day               string  `json:"day"`
	TotalRequests     int64   `json:"total_requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	TotalCost         float64 `json:"total_cost"`
}
