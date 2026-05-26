package services

import (
	"database/sql"
	"errors"
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

func (ls *LogService) CostSince(start string, platform string) (float64, error) {
	startTime, err := parseTimeInput(start)
	if err != nil {
		return 0, err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return 0, err
	}
	query := `
		SELECT
			COALESCE(model, ''),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cache_create_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0)
		FROM request_log
		WHERE created_at >= ?
	`
	args := []any{startTime.Format(timeLayout)}
	if platform != "" {
		query += " AND platform = ?"
		args = append(args, platform)
	}
	query += " GROUP BY COALESCE(model, '')"

	rows, err := db.Query(query, args...)
	if err != nil {
		if errors.Is(err, xdb.ErrNotFound) || isNoSuchTableErr(err) {
			return 0, nil
		}
		return 0, err
	}
	defer rows.Close()

	total := 0.0
	for rows.Next() {
		modelName, usage, err := scanUsageAggregate(rows)
		if err != nil {
			return 0, err
		}
		cost := ls.calculateCost(modelName, usage)
		total += cost.TotalCost
	}
	return total, rows.Err()
}

func NewLogService() *LogService {
	svc, err := modelpricing.DefaultService()
	if err != nil {
		log.Printf("pricing service init failed: %v", err)
	}
	return &LogService{pricing: svc}
}

func (ls *LogService) ListRequestLogs(platform string, provider string, limit int) ([]ReqeustLog, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
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
	logs := make([]ReqeustLog, 0, len(records))
	for _, record := range records {
		logEntry := ReqeustLog{
			ID:                record.GetInt64("id"),
			Platform:          record.GetString("platform"),
			Model:             record.GetString("model"),
			Provider:          record.GetString("provider"),
			HttpCode:          record.GetInt("http_code"),
			InputTokens:       record.GetInt("input_tokens"),
			OutputTokens:      record.GetInt("output_tokens"),
			CacheCreateTokens: record.GetInt("cache_create_tokens"),
			CacheReadTokens:   record.GetInt("cache_read_tokens"),
			ReasoningTokens:   record.GetInt("reasoning_tokens"),
			CreatedAt:         record.GetString("created_at"),
			IsStream:          record.GetBool("is_stream"),
			DurationSec:       record.GetFloat64("duration_sec"),
		}
		ls.decorateCost(&logEntry)
		logs = append(logs, logEntry)
	}
	return logs, nil
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

func (ls *LogService) HeatmapStats(days int) ([]HeatmapStat, error) {
	if days <= 0 {
		days = 30
	}
	totalHours := days * 24
	if totalHours <= 0 {
		totalHours = 24
	}
	rangeStart := startOfHour(time.Now())
	if totalHours > 1 {
		rangeStart = rangeStart.Add(-time.Duration(totalHours-1) * time.Hour)
	}
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT
			COALESCE(strftime('%Y-%m-%d %H:00:00', created_at), substr(created_at, 1, 13) || ':00:00') AS hour_bucket,
			COALESCE(model, ''),
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cache_create_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0)
		FROM request_log
		WHERE created_at >= ?
		GROUP BY hour_bucket, COALESCE(model, '')
		ORDER BY hour_bucket ASC
	`, rangeStart.Format(timeLayout))
	if err != nil {
		if errors.Is(err, xdb.ErrNotFound) || isNoSuchTableErr(err) {
			return []HeatmapStat{}, nil
		}
		return nil, err
	}
	defer rows.Close()

	hourBuckets := map[int64]*HeatmapStat{}
	for rows.Next() {
		var hourBucket string
		var modelName string
		var totalRequests int64
		var input, output, reasoning, cacheCreate, cacheRead int64
		if err := rows.Scan(&hourBucket, &modelName, &totalRequests, &input, &output, &reasoning, &cacheCreate, &cacheRead); err != nil {
			return nil, err
		}
		createdAt, err := parseLogTime(hourBucket)
		if err != nil {
			continue
		}
		if createdAt.IsZero() {
			continue
		}
		hourStart := startOfHour(createdAt)
		hourKey := hourStart.Unix()
		bucket := hourBuckets[hourKey]
		if bucket == nil {
			bucket = &HeatmapStat{Day: hourStart.Format("01-02 15")}
			hourBuckets[hourKey] = bucket
		}
		bucket.TotalRequests += totalRequests
		bucket.InputTokens += input
		bucket.OutputTokens += output
		bucket.ReasoningTokens += reasoning
		usage := modelpricing.UsageSnapshot{
			InputTokens:       int(input),
			OutputTokens:      int(output),
			ReasoningTokens:   int(reasoning),
			CacheCreateTokens: int(cacheCreate),
			CacheReadTokens:   int(cacheRead),
		}
		cost := ls.calculateCost(modelName, usage)
		bucket.TotalCost += cost.TotalCost
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(hourBuckets) == 0 {
		return []HeatmapStat{}, nil
	}
	hourKeys := make([]int64, 0, len(hourBuckets))
	for key := range hourBuckets {
		hourKeys = append(hourKeys, key)
	}
	sort.Slice(hourKeys, func(i, j int) bool {
		return hourKeys[i] < hourKeys[j]
	})
	stats := make([]HeatmapStat, 0, min(len(hourKeys), totalHours))
	for i := len(hourKeys) - 1; i >= 0 && len(stats) < totalHours; i-- {
		stats = append(stats, *hourBuckets[hourKeys[i]])
	}
	return stats, nil
}

func (ls *LogService) StatsSince(platform string) (LogStats, error) {
	const seriesHours = 24

	stats := LogStats{
		Series: make([]LogStatsSeries, 0, seriesHours),
	}
	now := time.Now()
	seriesStart := startOfDay(now)
	seriesEnd := seriesStart.Add(seriesHours * time.Hour)
	db, err := xdb.DB("default")
	if err != nil {
		return stats, err
	}
	query := `
		SELECT
			COALESCE(strftime('%Y-%m-%d %H:00:00', created_at), substr(created_at, 1, 13) || ':00:00') AS hour_bucket,
			COALESCE(model, ''),
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cache_create_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0)
		FROM request_log
		WHERE created_at >= ? AND created_at < ?
	`
	args := []any{seriesStart.Format(timeLayout), seriesEnd.Format(timeLayout)}
	if platform != "" {
		query += " AND platform = ?"
		args = append(args, platform)
	}
	query += " GROUP BY hour_bucket, COALESCE(model, '') ORDER BY hour_bucket ASC"

	rows, err := db.Query(query, args...)
	if err != nil {
		if errors.Is(err, xdb.ErrNotFound) || isNoSuchTableErr(err) {
			return stats, nil
		}
		return stats, err
	}
	defer rows.Close()

	seriesBuckets := make([]*LogStatsSeries, seriesHours)
	for i := 0; i < seriesHours; i++ {
		bucketTime := seriesStart.Add(time.Duration(i) * time.Hour)
		seriesBuckets[i] = &LogStatsSeries{
			Day: bucketTime.Format(timeLayout),
		}
	}

	for rows.Next() {
		var hourBucket string
		var modelName string
		var totalRequests int64
		var input, output, reasoning, cacheCreate, cacheRead int64
		if err := rows.Scan(&hourBucket, &modelName, &totalRequests, &input, &output, &reasoning, &cacheCreate, &cacheRead); err != nil {
			return stats, err
		}
		createdAt, err := parseLogTime(hourBucket)
		if err != nil || createdAt.Before(seriesStart) || !createdAt.Before(seriesEnd) {
			continue
		}
		bucketIndex := int(createdAt.Sub(seriesStart) / time.Hour)
		if bucketIndex < 0 {
			bucketIndex = 0
		}
		if bucketIndex >= seriesHours {
			bucketIndex = seriesHours - 1
		}

		bucket := seriesBuckets[bucketIndex]
		usage := modelpricing.UsageSnapshot{
			InputTokens:       int(input),
			OutputTokens:      int(output),
			ReasoningTokens:   int(reasoning),
			CacheCreateTokens: int(cacheCreate),
			CacheReadTokens:   int(cacheRead),
		}
		cost := ls.calculateCost(modelName, usage)

		bucket.TotalRequests += totalRequests
		bucket.InputTokens += input
		bucket.OutputTokens += output
		bucket.ReasoningTokens += reasoning
		bucket.CacheCreateTokens += cacheCreate
		bucket.CacheReadTokens += cacheRead
		bucket.TotalCost += cost.TotalCost

		stats.TotalRequests += totalRequests
		stats.InputTokens += input
		stats.OutputTokens += output
		stats.ReasoningTokens += reasoning
		stats.CacheCreateTokens += cacheCreate
		stats.CacheReadTokens += cacheRead
		stats.CostInput += cost.InputCost
		stats.CostOutput += cost.OutputCost
		stats.CostCacheCreate += cost.CacheCreateCost
		stats.CostCacheRead += cost.CacheReadCost
		stats.CostTotal += cost.TotalCost
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	for i := 0; i < seriesHours; i++ {
		if bucket := seriesBuckets[i]; bucket != nil {
			stats.Series = append(stats.Series, *bucket)
		} else {
			bucketTime := seriesStart.Add(time.Duration(i) * time.Hour)
			stats.Series = append(stats.Series, LogStatsSeries{
				Day: bucketTime.Format(timeLayout),
			})
		}
	}

	return stats, nil
}

func (ls *LogService) ProviderDailyStats(platform string) ([]ProviderDailyStat, error) {
	start := startOfDay(time.Now())
	end := start.Add(24 * time.Hour)
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	query := `
		SELECT
			COALESCE(NULLIF(TRIM(provider), ''), '(unknown)') AS provider_key,
			COALESCE(model, ''),
			COUNT(*),
			COALESCE(SUM(CASE WHEN http_code >= 200 AND http_code < 300 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN http_code < 200 OR http_code >= 300 OR http_code IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cache_create_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0)
		FROM request_log
		WHERE created_at >= ? AND created_at < ?
	`
	args := []any{start.Format(timeLayout), end.Format(timeLayout)}
	if platform != "" {
		query += " AND platform = ?"
		args = append(args, platform)
	}
	query += " GROUP BY provider_key, COALESCE(model, '')"

	rows, err := db.Query(query, args...)
	if err != nil {
		if errors.Is(err, xdb.ErrNotFound) || isNoSuchTableErr(err) {
			return []ProviderDailyStat{}, nil
		}
		return nil, err
	}
	defer rows.Close()

	statMap := map[string]*ProviderDailyStat{}
	for rows.Next() {
		var provider, modelName string
		var totalRequests, successfulRequests, failedRequests int64
		var input, output, reasoning, cacheCreate, cacheRead int64
		if err := rows.Scan(&provider, &modelName, &totalRequests, &successfulRequests, &failedRequests, &input, &output, &reasoning, &cacheCreate, &cacheRead); err != nil {
			return nil, err
		}
		stat := statMap[provider]
		if stat == nil {
			stat = &ProviderDailyStat{Provider: provider}
			statMap[provider] = stat
		}
		usage := modelpricing.UsageSnapshot{
			InputTokens:       int(input),
			OutputTokens:      int(output),
			ReasoningTokens:   int(reasoning),
			CacheCreateTokens: int(cacheCreate),
			CacheReadTokens:   int(cacheRead),
		}
		cost := ls.calculateCost(modelName, usage)
		stat.TotalRequests += totalRequests
		stat.SuccessfulRequests += successfulRequests
		stat.FailedRequests += failedRequests
		stat.InputTokens += input
		stat.OutputTokens += output
		stat.ReasoningTokens += reasoning
		stat.CacheCreateTokens += cacheCreate
		stat.CacheReadTokens += cacheRead
		stat.CostTotal += cost.TotalCost
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stats := make([]ProviderDailyStat, 0, len(statMap))
	for _, stat := range statMap {
		if stat.TotalRequests > 0 {
			stat.SuccessRate = float64(stat.SuccessfulRequests) / float64(stat.TotalRequests)
		}
		stats = append(stats, *stat)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].TotalRequests == stats[j].TotalRequests {
			return stats[i].Provider < stats[j].Provider
		}
		return stats[i].TotalRequests > stats[j].TotalRequests
	})
	return stats, nil
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

func (ls *LogService) decorateCost(logEntry *ReqeustLog) {
	if ls == nil || ls.pricing == nil || logEntry == nil {
		return
	}
	usage := modelpricing.UsageSnapshot{
		InputTokens:       logEntry.InputTokens,
		OutputTokens:      logEntry.OutputTokens,
		ReasoningTokens:   logEntry.ReasoningTokens,
		CacheCreateTokens: logEntry.CacheCreateTokens,
		CacheReadTokens:   logEntry.CacheReadTokens,
	}
	cost := ls.pricing.CalculateCost(logEntry.Model, usage)
	logEntry.HasPricing = cost.HasPricing
	logEntry.InputCost = cost.InputCost
	logEntry.OutputCost = cost.OutputCost
	logEntry.ReasoningCost = cost.ReasoningCost
	logEntry.CacheCreateCost = cost.CacheCreateCost
	logEntry.CacheReadCost = cost.CacheReadCost
	logEntry.Ephemeral5mCost = cost.Ephemeral5mCost
	logEntry.Ephemeral1hCost = cost.Ephemeral1hCost
	logEntry.TotalCost = cost.TotalCost
}

func (ls *LogService) calculateCost(model string, usage modelpricing.UsageSnapshot) modelpricing.CostBreakdown {
	if ls == nil || ls.pricing == nil {
		return modelpricing.CostBreakdown{}
	}
	return ls.pricing.CalculateCost(model, usage)
}

func parseCreatedAt(record xdb.Record) (time.Time, bool) {
	if t := record.GetTime("created_at"); t != nil {
		return t.In(time.Local), true
	}
	raw := strings.TrimSpace(record.GetString("created_at"))
	if raw == "" {
		return time.Time{}, false
	}

	layouts := []string{
		timeLayout,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02T15:04:05-0700",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.In(time.Local), true
		}
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed.In(time.Local), true
		}
	}

	if normalized := strings.Replace(raw, " ", "T", 1); normalized != raw {
		if parsed, err := time.Parse(time.RFC3339, normalized); err == nil {
			return parsed.In(time.Local), true
		}
	}

	if len(raw) >= len("2006-01-02") {
		if parsed, err := time.ParseInLocation("2006-01-02", raw[:10], time.Local); err == nil {
			return parsed, false
		}
	}

	return time.Time{}, false
}

func parseTimeInput(value string) (time.Time, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return startOfDay(time.Now()), nil
	}
	layouts := []string{
		time.RFC3339,
		timeLayout,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02T15:04:05-0700",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.In(time.Local), nil
		}
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed.In(time.Local), nil
		}
	}
	if normalized := strings.Replace(raw, " ", "T", 1); normalized != raw {
		if parsed, err := time.Parse(time.RFC3339, normalized); err == nil {
			return parsed.In(time.Local), nil
		}
	}
	if len(raw) >= len("2006-01-02") {
		if parsed, err := time.ParseInLocation("2006-01-02", raw[:10], time.Local); err == nil {
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

func scanUsageAggregate(rows *sql.Rows) (string, modelpricing.UsageSnapshot, error) {
	var modelName string
	var input, output, reasoning, cacheCreate, cacheRead int64
	if err := rows.Scan(&modelName, &input, &output, &reasoning, &cacheCreate, &cacheRead); err != nil {
		return "", modelpricing.UsageSnapshot{}, err
	}
	return modelName, modelpricing.UsageSnapshot{
		InputTokens:       int(input),
		OutputTokens:      int(output),
		ReasoningTokens:   int(reasoning),
		CacheCreateTokens: int(cacheCreate),
		CacheReadTokens:   int(cacheRead),
	}, nil
}

func parseLogTime(value string) (time.Time, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	layouts := []string{
		timeLayout,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02T15:04:05-0700",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.In(time.Local), nil
		}
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed.In(time.Local), nil
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
	Day               string  `json:"day"`
	TotalRequests     int64   `json:"total_requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	TotalCost         float64 `json:"total_cost"`
}
