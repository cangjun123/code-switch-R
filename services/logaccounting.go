package services

import (
	"database/sql"
	"strings"
	"time"

	modelpricing "codeswitch/resources/model-pricing"
	"github.com/daodao97/xgo/xdb"
)

// Snapshot prices once per query so an admin edit cannot mix prices within a
// result, and no nested DB queries are needed while reading request_log rows.
type logCostCalculator struct {
	pricing *modelpricing.Service
	custom  map[string]relayModelPriceRecord
	deleted map[string]bool
}

func (ls *LogService) costCalculator() (*logCostCalculator, error) {
	c := &logCostCalculator{pricing: ls.pricing, custom: map[string]relayModelPriceRecord{}, deleted: map[string]bool{}}
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT model, input_price_nano, cached_input_price_nano, output_price_nano, reasoning_output_price_nano FROM relay_model_price`)
	if err != nil && !isNoSuchTableErr(err) {
		return nil, err
	}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p relayModelPriceRecord
			if err := rows.Scan(&p.Model, &p.InputNano, &p.CachedInputNano, &p.OutputNano, &p.ReasoningNano); err != nil {
				return nil, err
			}
			c.custom[p.Model] = p
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	rows, err = db.Query(`SELECT model FROM relay_deleted_model_price`)
	if err != nil && !isNoSuchTableErr(err) {
		return nil, err
	}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return nil, err
			}
			c.deleted[model] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Return disjoint billable token categories. Codex/OpenAI input includes cache
// hits and output includes reasoning; Gemini input includes cache hits but its
// candidatesTokenCount excludes thoughts. Anthropic input excludes cache tokens.
func requestLogUsage(entry *ReqeustLog) modelpricing.UsageSnapshot {
	u := modelpricing.UsageSnapshot{
		InputTokens: max(0, entry.InputTokens), OutputTokens: max(0, entry.OutputTokens),
		ReasoningTokens: max(0, entry.ReasoningTokens), CacheCreateTokens: max(0, entry.CacheCreateTokens), CacheReadTokens: max(0, entry.CacheReadTokens),
	}
	model := strings.ToLower(entry.Model)
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	openAI := entry.Platform == ProviderKindCodex || strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
	gemini := entry.Platform == "gemini" || strings.HasPrefix(model, "gemini-")
	if openAI || gemini {
		u.CacheReadTokens = min(u.CacheReadTokens, u.InputTokens)
		u.InputTokens -= u.CacheReadTokens
	}
	if openAI {
		u.ReasoningTokens = min(u.ReasoningTokens, u.OutputTokens)
		u.OutputTokens -= u.ReasoningTokens
	}
	return u
}

func (c *logCostCalculator) calculate(entry *ReqeustLog, u modelpricing.UsageSnapshot) modelpricing.CostBreakdown {
	model := strings.TrimSpace(entry.Model)
	if p, ok := c.custom[model]; ok {
		// Quota custom prices are USD per million tokens in nano-USD units.
		const unit = 1e15
		cost := modelpricing.CostBreakdown{HasPricing: true}
		cost.InputCost = float64(u.InputTokens) * float64(p.InputNano) / unit
		cost.CacheReadCost = float64(u.CacheReadTokens) * float64(p.CachedInputNano) / unit
		cost.OutputCost = float64(u.OutputTokens) * float64(p.OutputNano) / unit
		cost.ReasoningCost = float64(u.ReasoningTokens) * float64(p.ReasoningNano) / unit
		// The custom-price editor has no cache-write rate. Requests with cache
		// creation cannot be fully estimated from this price record.
		if u.CacheCreateTokens > 0 {
			return modelpricing.CostBreakdown{}
		}
		cost.TotalCost = cost.InputCost + cost.CacheReadCost + cost.OutputCost + cost.ReasoningCost
		return cost
	}
	if c.deleted[model] || c.pricing == nil {
		return modelpricing.CostBreakdown{}
	}
	return c.pricing.CalculateCost(model, u)
}

func decorateLogCost(entry *ReqeustLog, cost modelpricing.CostBreakdown) {
	entry.HasPricing = cost.HasPricing
	entry.InputCost = cost.InputCost
	entry.OutputCost = cost.OutputCost
	entry.ReasoningCost = cost.ReasoningCost
	entry.CacheCreateCost = cost.CacheCreateCost
	entry.CacheReadCost = cost.CacheReadCost
	entry.Ephemeral5mCost = cost.Ephemeral5mCost
	entry.Ephemeral1hCost = cost.Ephemeral1hCost
	entry.TotalCost = cost.TotalCost
}

// Price each request before aggregation: summing many short requests first can
// incorrectly trigger a long-context tier. Streaming reads bound memory usage.
func (ls *LogService) visitRequestLogs(start, end time.Time, platform string, visit func(ReqeustLog, time.Time, modelpricing.UsageSnapshot, modelpricing.CostBreakdown)) error {
	calculator, err := ls.costCalculator()
	if err != nil {
		return err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return err
	}
	query := `SELECT COALESCE(platform,''), COALESCE(model,''), COALESCE(provider,''), COALESCE(http_code,0), COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(reasoning_tokens,0), COALESCE(cache_create_tokens,0), COALESCE(cache_read_tokens,0), created_at FROM request_log WHERE datetime(created_at) >= ?`
	args := []any{formatSQLiteUTC(start)}
	if !end.IsZero() {
		query += " AND datetime(created_at) < ?"
		args = append(args, formatSQLiteUTC(end))
	}
	if platform != "" {
		query += " AND platform = ?"
		args = append(args, platform)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		if isNoSuchTableErr(err) {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var entry ReqeustLog
		var created sql.NullString
		if err := rows.Scan(&entry.Platform, &entry.Model, &entry.Provider, &entry.HttpCode, &entry.InputTokens, &entry.OutputTokens, &entry.ReasoningTokens, &entry.CacheCreateTokens, &entry.CacheReadTokens, &created); err != nil {
			return err
		}
		at, err := parseStoredLogTime(created.String)
		if err != nil {
			continue
		}
		usage := requestLogUsage(&entry)
		visit(entry, at, usage, calculator.calculate(&entry, usage))
	}
	return rows.Err()
}

func usageInput(u modelpricing.UsageSnapshot) int64 {
	return int64(u.InputTokens) + int64(u.CacheReadTokens) + int64(u.CacheCreateTokens)
}
func usageOutput(u modelpricing.UsageSnapshot) int64 {
	return int64(u.OutputTokens) + int64(u.ReasoningTokens)
}
