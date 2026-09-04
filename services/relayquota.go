package services

// Codex relay quota accounting.
//
// The relay key configuration is intentionally kept in the existing JSON key
// store, while usage and pricing live in SQLite.  request_log is asynchronous
// and subject to retention, so it is never used as the source of truth for
// quota enforcement.

import (
	modelpricing "codeswitch/resources/model-pricing"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daodao97/xgo/xdb"
)

const (
	RelayQuotaPeriodOnce    = "once"
	RelayQuotaPeriodOneTime = RelayQuotaPeriodOnce
	RelayQuotaPeriodDaily   = "daily"
	RelayQuotaPeriodWeekly  = "weekly"
	RelayQuotaPeriodMonthly = "monthly"

	// Prices and amounts are stored as nano-USD.  Price values are expressed
	// per million tokens; this gives exact results for normal provider prices
	// while keeping the full dollar amount in a signed 64 bit SQLite integer.
	nanoUSDPerDollar = int64(1_000_000_000)
	priceTokenUnit   = int64(1_000_000)

	maxQuotaBodyDecimalPlaces = 9
)

var quotaDecimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,9})?$`)

// RelayQuotaConfig is the user-facing configuration for one relay key.
type RelayQuotaConfig struct {
	TokenLimit int64  `json:"tokenLimit"`
	USDLimit   string `json:"usdLimit"`
	Period     string `json:"period"`
}

// RelayQuotaUsage is the four-way usage breakdown returned by the provider.
type RelayQuotaUsage struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

// RelayQuotaStatus is safe to expose to the admin UI.  A nil remaining value
// means that the corresponding limit is unlimited.
type RelayQuotaStatus struct {
	TokenLimit      int64           `json:"tokenLimit"`
	TokenUsed       int64           `json:"tokenUsed"`
	TokenRemaining  *int64          `json:"tokenRemaining"`
	USDLimit        string          `json:"usdLimit"`
	USDUsed         string          `json:"usdUsed"`
	USDRemaining    *string         `json:"usdRemaining"`
	Period          string          `json:"period"`
	WindowStartedAt string          `json:"windowStartedAt,omitempty"`
	ResetAt         *string         `json:"resetAt,omitempty"`
	ServerTimezone  string          `json:"serverTimezone"`
	TrackingStarted bool            `json:"trackingStarted"`
	TokenExhausted  bool            `json:"tokenExhausted"`
	USDExhausted    bool            `json:"usdExhausted"`
	Blocked         bool            `json:"blocked"`
	Usage           RelayQuotaUsage `json:"usage"`
}

// RelayQuotaDecision is returned by the request-start guard.
type RelayQuotaDecision struct {
	Allowed bool             `json:"allowed"`
	Reason  string           `json:"reason,omitempty"`
	Status  RelayQuotaStatus `json:"status"`
}

// RelayQuotaSettlementResult describes one idempotent upstream attempt.
type RelayQuotaSettlementResult struct {
	AttemptID  string          `json:"attemptId"`
	Usage      RelayQuotaUsage `json:"usage"`
	Amount     string          `json:"amount"`
	Priced     bool            `json:"priced"`
	AlreadySet bool            `json:"alreadySettled"`
}

// RelayModelPrice is the admin API representation.  Every value is USD per
// million tokens and is kept as a decimal string to avoid float precision.
type RelayModelPrice struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	CachedInput     string `json:"cachedInput"`
	Output          string `json:"output"`
	ReasoningOutput string `json:"reasoningOutput"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
	Source          string `json:"source"`
	CanRestore      bool   `json:"canRestoreDefault"`
}

type relayModelPriceRecord struct {
	Model           string
	InputNano       int64
	CachedInputNano int64
	OutputNano      int64
	ReasoningNano   int64
	UpdatedAt       time.Time
}

var (
	relayBuiltinPricesOnce sync.Once
	relayBuiltinPrices     map[string]relayModelPriceRecord
	relayBuiltinPricesErr  error
)

func builtinRelayModelPrices() (map[string]relayModelPriceRecord, error) {
	relayBuiltinPricesOnce.Do(func() {
		known, err := modelpricing.KnownOpenAIModelPrices()
		if err != nil {
			relayBuiltinPricesErr = err
			return
		}
		relayBuiltinPrices = make(map[string]relayModelPriceRecord, len(known))
		for _, price := range known {
			values := []string{price.Input, price.CachedInput, price.Output, price.ReasoningOutput}
			parsed := make([]int64, len(values))
			for i, value := range values {
				parsed[i], err = parseRelayDecimalNano(value)
				if err != nil {
					relayBuiltinPricesErr = fmt.Errorf("invalid built-in price for %s: %w", price.Model, err)
					return
				}
			}
			relayBuiltinPrices[price.Model] = relayModelPriceRecord{
				Model: price.Model, InputNano: parsed[0], CachedInputNano: parsed[1],
				OutputNano: parsed[2], ReasoningNano: parsed[3],
			}
		}
	})
	return relayBuiltinPrices, relayBuiltinPricesErr
}

func lookupBuiltinRelayModelPrice(model string) (*relayModelPriceRecord, bool, error) {
	prices, err := builtinRelayModelPrices()
	if err != nil {
		return nil, false, err
	}
	price, ok := prices[strings.TrimSpace(model)]
	if !ok {
		return nil, false, nil
	}
	copyPrice := price
	return &copyPrice, true, nil
}

func relayModelPriceResponse(price relayModelPriceRecord, source string, canRestore bool, loc *time.Location) RelayModelPrice {
	return RelayModelPrice{
		Model: price.Model, Input: formatRelayNanoUSD(price.InputNano),
		CachedInput: formatRelayNanoUSD(price.CachedInputNano), Output: formatRelayNanoUSD(price.OutputNano),
		ReasoningOutput: formatRelayNanoUSD(price.ReasoningNano),
		UpdatedAt:       formatRelayTime(price.UpdatedAt.Unix(), loc), Source: source, CanRestore: canRestore,
	}
}

type relayQuotaState struct {
	KeyID             string
	Generation        int64
	Period            string
	WindowStartedAt   int64
	ResetAt           int64
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64
	AmountNanoUSD     int64
	TrackingStartedAt int64
}

// relaySettleJob is one queued settlement for an unlimited key.  Kept small so
// the bounded queue has a predictable memory footprint.
type relaySettleJob struct {
	keyID     string
	attemptID string
	provider  string
	model     string
	usage     RelayQuotaUsage
}

// RelayQuotaService owns all quota state transitions.  now and location are
// injectable for deterministic boundary tests.
type RelayQuotaService struct {
	mu       sync.Mutex
	now      func() time.Time
	location func() *time.Location

	// settleQueue carries settlements for unlimited keys.  Synchronous
	// settlement runs on the response-read goroutine while holding s.mu, so on
	// a weak machine with SQLite write contention it delayed handler cleanup
	// and queued the next request's Check behind the mutex.  Unlimited keys
	// have no admission decision, so their settlements are stats-only and can
	// settle asynchronously; limited keys keep the synchronous path so quota
	// exhaustion is never over-run.  Jobs beyond the queue capacity are
	// dropped with a rate-limited log line: settlement is accounting-only for
	// these keys, and a process exit can equally lose pending jobs.
	settleQueue   chan relaySettleJob
	settleDropped int64

	// settleKeyLookup lets tests inject the key lookup; nil falls back to the
	// process-wide CodexRelayKeyService created in web_runtime.
	settleKeyLookup func(keyID string) *CodexRelayKey
}

// relayQuotaSettleQueueCapacity is a package-level var so tests can shrink it.
var relayQuotaSettleQueueCapacity = 1024

// globalCodexRelayKeys backs the settle worker's key lookup when no injection is
// configured; NewProviderRelayService registers the process-wide service.
var globalCodexRelayKeys relayQuotaKeySource

// relayQuotaKeySource is the subset of CodexRelayKeyService the settle worker
// needs, so the default fallback can construct its own service lazily.
type relayQuotaKeySource interface {
	GetKeyByID(id string) (*CodexRelayKey, error)
}

func NewRelayQuotaService() *RelayQuotaService {
	service := &RelayQuotaService{
		now:      time.Now,
		location: func() *time.Location { return time.Local },
	}
	if relayQuotaSettleQueueCapacity > 0 {
		service.settleQueue = make(chan relaySettleJob, relayQuotaSettleQueueCapacity)
		go service.drainSettleQueue()
	}
	return service
}

// enqueueUnlimitedSettle hands one unlimited-key settlement to the background
// worker.  It never blocks: a full queue drops the job (stats-only accounting).
func (s *RelayQuotaService) enqueueUnlimitedSettle(job relaySettleJob) {
	if s == nil || s.settleQueue == nil {
		return
	}
	select {
	case s.settleQueue <- job:
	default:
		s.mu.Lock()
		s.settleDropped++
		s.mu.Unlock()
		logRelayQuotaSettlementDropped(job.attemptID, job.provider, job.model)
	}
}

func (s *RelayQuotaService) drainSettleQueue() {
	for job := range s.settleQueue {
		s.runSettleJob(job)
	}
}

// runSettleJob resolves the key by ID at settlement time (the key may have been
// deleted while the job was queued) and records the settlement synchronously.
func (s *RelayQuotaService) runSettleJob(job relaySettleJob) {
	key, err := s.lookupSettleKey(job.keyID)
	if err != nil {
		logRelayQuotaSettlementError(job.attemptID, job.provider, job.model, err)
		return
	}
	if _, err := s.Settle(key, job.attemptID, job.provider, job.model, job.usage); err != nil {
		logRelayQuotaSettlementError(job.attemptID, job.provider, job.model, err)
	}
}

func (s *RelayQuotaService) lookupSettleKey(keyID string) (*CodexRelayKey, error) {
	if s.settleKeyLookup != nil {
		return s.settleKeyLookup(keyID), nil
	}
	if globalCodexRelayKeys != nil {
		return globalCodexRelayKeys.GetKeyByID(keyID)
	}
	// Fallback for stand-alone constructions (tests, early startup): read the
	// same on-disk key store the process-wide service uses.
	return NewCodexRelayKeyService().GetKeyByID(keyID)
}

// SetClockForTest is intentionally small and only used by package tests.
func (s *RelayQuotaService) SetClockForTest(now func() time.Time, location func() *time.Location) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
	if location != nil {
		s.location = location
	}
}

func (s *RelayQuotaService) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *RelayQuotaService) currentLocation() *time.Location {
	if s == nil || s.location == nil {
		return time.Local
	}
	if loc := s.location(); loc != nil {
		return loc
	}
	return time.Local
}

// normalizeRelayQuotaPeriod accepts the historical spellings used by early
// builds and always returns one canonical value.
func normalizeRelayQuotaPeriod(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", "once", "one_time", "one-time", "onetime", "one time":
		return RelayQuotaPeriodOnce
	case RelayQuotaPeriodDaily, "day", "daily_reset":
		return RelayQuotaPeriodDaily
	case RelayQuotaPeriodWeekly, "week", "weekly_reset":
		return RelayQuotaPeriodWeekly
	case RelayQuotaPeriodMonthly, "month", "monthly_reset":
		return RelayQuotaPeriodMonthly
	default:
		return RelayQuotaPeriodOnce
	}
}

// NormalizeRelayQuotaPeriod exposes the canonical period spelling to the
// admin HTTP layer while keeping the validation implementation internal.
func NormalizeRelayQuotaPeriod(period string) string {
	return normalizeRelayQuotaPeriod(period)
}

func validateRelayQuotaPeriod(period string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(period))
	canonical := normalizeRelayQuotaPeriod(raw)
	if raw != "" && canonical == RelayQuotaPeriodOnce && raw != "once" && raw != "one_time" && raw != "one-time" && raw != "onetime" && raw != "one time" {
		return "", fmt.Errorf("不支持的额度周期: %s", period)
	}
	if raw != "" && canonical == RelayQuotaPeriodDaily && raw != RelayQuotaPeriodDaily && raw != "day" && raw != "daily_reset" {
		return "", fmt.Errorf("不支持的额度周期: %s", period)
	}
	if raw != "" && canonical == RelayQuotaPeriodWeekly && raw != RelayQuotaPeriodWeekly && raw != "week" && raw != "weekly_reset" {
		return "", fmt.Errorf("不支持的额度周期: %s", period)
	}
	if raw != "" && canonical == RelayQuotaPeriodMonthly && raw != RelayQuotaPeriodMonthly && raw != "month" && raw != "monthly_reset" {
		return "", fmt.Errorf("不支持的额度周期: %s", period)
	}
	return canonical, nil
}

func ValidateRelayQuotaPeriod(period string) (string, error) {
	return validateRelayQuotaPeriod(period)
}

// normalizeRelayUSDLimit validates and canonicalizes a dollar amount.  Empty
// is equivalent to zero (unlimited).
func normalizeRelayUSDLimit(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "0", nil
	}
	if !quotaDecimalPattern.MatchString(value) {
		return "", errors.New("美元额度必须是非负十进制定点数（最多 9 位小数）")
	}
	nano, err := parseRelayDecimalNano(value)
	if err != nil {
		return "", err
	}
	return formatRelayNanoUSD(nano), nil
}

// ValidateRelayQuotaLimits canonicalizes the USD value and enforces the
// single active quota dimension used by the admin API. Zero still means
// unlimited for the selected dimension.
func ValidateRelayQuotaLimits(tokenLimit int64, usdLimit string) (string, error) {
	if tokenLimit < 0 {
		return "", errors.New("Token 额度不能为负数")
	}
	canonicalUSD, err := normalizeRelayUSDLimit(usdLimit)
	if err != nil {
		return "", err
	}
	usdNano, err := parseRelayDecimalNano(canonicalUSD)
	if err != nil {
		return "", err
	}
	if tokenLimit > 0 && usdNano > 0 {
		return "", errors.New("Token 额度和美元额度只能选择一种")
	}
	return canonicalUSD, nil
}

func parseRelayDecimalNano(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if !quotaDecimalPattern.MatchString(value) {
		return 0, errors.New("金额格式无效")
	}
	parts := strings.SplitN(value, ".", 2)
	whole := new(big.Int)
	if _, ok := whole.SetString(parts[0], 10); !ok {
		return 0, errors.New("金额格式无效")
	}
	scale := big.NewInt(nanoUSDPerDollar)
	whole.Mul(whole, scale)
	if len(parts) == 2 {
		fraction := parts[1]
		fraction += strings.Repeat("0", maxQuotaBodyDecimalPlaces-len(fraction))
		frac := new(big.Int)
		if _, ok := frac.SetString(fraction, 10); !ok {
			return 0, errors.New("金额格式无效")
		}
		whole.Add(whole, frac)
	}
	if !whole.IsInt64() {
		return 0, errors.New("金额超出支持范围")
	}
	return whole.Int64(), nil
}

func formatRelayNanoUSD(value int64) string {
	if value <= 0 {
		return "0"
	}
	whole := value / nanoUSDPerDollar
	frac := value % nanoUSDPerDollar
	if frac == 0 {
		return fmt.Sprintf("%d", whole)
	}
	text := fmt.Sprintf("%09d", frac)
	text = strings.TrimRight(text, "0")
	return fmt.Sprintf("%d.%s", whole, text)
}

func formatRelayTime(unix int64, loc *time.Location) string {
	if unix <= 0 {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	return time.Unix(unix, 0).In(loc).Format(time.RFC3339)
}

// relayQuotaWindow returns the current local calendar window and its next
// reset.  The once period has no next reset.
func relayQuotaWindow(now time.Time, period string) (time.Time, time.Time) {
	loc := now.Location()
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	year, month, day := now.Date()
	midnight := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	switch normalizeRelayQuotaPeriod(period) {
	case RelayQuotaPeriodDaily:
		start := midnight(year, month, day)
		return start, start.AddDate(0, 0, 1)
	case RelayQuotaPeriodWeekly:
		// Go's Sunday=0; convert to Monday-based offset.
		weekday := int(now.Weekday())
		daysSinceMonday := (weekday + 6) % 7
		start := midnight(year, month, day).AddDate(0, 0, -daysSinceMonday)
		return start, start.AddDate(0, 0, 7)
	case RelayQuotaPeriodMonthly:
		start := midnight(year, month, 1)
		return start, start.AddDate(0, 1, 0)
	default:
		return now, time.Time{}
	}
}

func relayQuotaStateUsage(state *relayQuotaState) RelayQuotaUsage {
	if state == nil {
		return RelayQuotaUsage{}
	}
	return RelayQuotaUsage{
		InputTokens:           state.InputTokens,
		CachedInputTokens:     state.CachedInputTokens,
		OutputTokens:          state.OutputTokens,
		ReasoningOutputTokens: state.ReasoningTokens,
		TotalTokens:           state.TotalTokens,
	}
}

func relayQuotaHasLimits(key *CodexRelayKey) bool {
	if key == nil {
		return false
	}
	if key.TokenLimit > 0 {
		return true
	}
	amount, err := parseRelayDecimalNano(key.USDLimit)
	return err == nil && amount > 0
}

func sanitizeRelayUsage(usage RelayQuotaUsage) RelayQuotaUsage {
	if usage.InputTokens < 0 {
		usage.InputTokens = 0
	}
	if usage.OutputTokens < 0 {
		usage.OutputTokens = 0
	}
	if usage.CachedInputTokens < 0 {
		usage.CachedInputTokens = 0
	}
	if usage.ReasoningOutputTokens < 0 {
		usage.ReasoningOutputTokens = 0
	}
	if usage.CachedInputTokens > usage.InputTokens {
		usage.CachedInputTokens = usage.InputTokens
	}
	if usage.ReasoningOutputTokens > usage.OutputTokens {
		usage.ReasoningOutputTokens = usage.OutputTokens
	}
	minimumTotal := saturatingNonNegativeAdd(usage.InputTokens, usage.OutputTokens)
	// Quota token usage is defined strictly as input_tokens + output_tokens.
	// Ignore any provider-reported total that might include cached/reasoning
	// detail fields a second time.
	usage.TotalTokens = minimumTotal
	return usage
}

func saturatingNonNegativeAdd(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func checkedAddInt64(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, errors.New("额度累计值溢出")
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, errors.New("额度累计值溢出")
	}
	return a + b, nil
}

func calculateRelayCostNanoUSD(usage RelayQuotaUsage, price *relayModelPriceRecord) (int64, bool, error) {
	usage = sanitizeRelayUsage(usage)
	if price == nil {
		return 0, false, nil
	}

	inputNormal := usage.InputTokens - usage.CachedInputTokens
	outputNormal := usage.OutputTokens - usage.ReasoningOutputTokens
	if inputNormal < 0 {
		inputNormal = 0
	}
	if outputNormal < 0 {
		outputNormal = 0
	}

	numerator := new(big.Int)
	add := func(tokens, rate int64) {
		if tokens <= 0 || rate <= 0 {
			return
		}
		term := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
		numerator.Add(numerator, term)
	}
	add(inputNormal, price.InputNano)
	add(usage.CachedInputTokens, price.CachedInputNano)
	add(outputNormal, price.OutputNano)
	add(usage.ReasoningOutputTokens, price.ReasoningNano)

	// A configured all-zero price is still a priced (free) model.
	if numerator.Sign() == 0 {
		return 0, true, nil
	}
	denominator := big.NewInt(priceTokenUnit)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, true, errors.New("本次费用超出支持范围")
	}
	return quotient.Int64(), true, nil
}

func ensureRelayQuotaTables() error {
	db, err := xdb.DB("default")
	if err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS relay_key_quota_state (
			relay_key_id TEXT PRIMARY KEY,
			quota_generation INTEGER NOT NULL DEFAULT 1,
			period TEXT NOT NULL DEFAULT 'once',
			window_started_at INTEGER NOT NULL DEFAULT 0,
			reset_at INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			amount_nano_usd INTEGER NOT NULL DEFAULT 0,
			tracking_started_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS relay_key_quota_settlement (
			attempt_id TEXT PRIMARY KEY,
			logical_request_id TEXT,
			relay_key_id TEXT NOT NULL,
			quota_generation INTEGER NOT NULL DEFAULT 0,
			provider TEXT,
			actual_model TEXT,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			amount_nano_usd INTEGER NOT NULL DEFAULT 0,
			priced INTEGER NOT NULL DEFAULT 0,
			input_price_nano INTEGER NOT NULL DEFAULT 0,
			cached_input_price_nano INTEGER NOT NULL DEFAULT 0,
			output_price_nano INTEGER NOT NULL DEFAULT 0,
			reasoning_price_nano INTEGER NOT NULL DEFAULT 0,
			settled_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_relay_quota_settlement_key_time ON relay_key_quota_settlement(relay_key_id, settled_at)`,
		`CREATE TABLE IF NOT EXISTS relay_model_price (
			model TEXT PRIMARY KEY,
			input_price_nano INTEGER NOT NULL DEFAULT 0,
			cached_input_price_nano INTEGER NOT NULL DEFAULT 0,
			output_price_nano INTEGER NOT NULL DEFAULT 0,
			reasoning_output_price_nano INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS relay_unpriced_model_seen (
			model TEXT PRIMARY KEY,
			first_seen_at INTEGER NOT NULL DEFAULT 0,
			last_seen_at INTEGER NOT NULL DEFAULT 0,
			call_count INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func scanRelayQuotaState(row interface{ Scan(...any) error }) (*relayQuotaState, error) {
	state := &relayQuotaState{}
	err := row.Scan(
		&state.KeyID,
		&state.Generation,
		&state.Period,
		&state.WindowStartedAt,
		&state.ResetAt,
		&state.InputTokens,
		&state.CachedInputTokens,
		&state.OutputTokens,
		&state.ReasoningTokens,
		&state.TotalTokens,
		&state.AmountNanoUSD,
		&state.TrackingStartedAt,
	)
	if err != nil {
		return nil, err
	}
	return state, nil
}

const relayQuotaStateSelect = `
	SELECT relay_key_id, quota_generation, period, window_started_at, reset_at,
	       input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
	       total_tokens, amount_nano_usd, tracking_started_at
	FROM relay_key_quota_state WHERE relay_key_id = ?`

func (s *RelayQuotaService) ensureStateTx(tx *sql.Tx, key *CodexRelayKey, now time.Time) (*relayQuotaState, error) {
	if key == nil || strings.TrimSpace(key.ID) == "" {
		return nil, nil
	}
	row := tx.QueryRow(relayQuotaStateSelect, key.ID)
	state, err := scanRelayQuotaState(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if !relayQuotaHasLimits(key) {
			return nil, nil
		}
		period := normalizeRelayQuotaPeriod(key.QuotaPeriod)
		windowStart, next := relayQuotaWindow(now, period)
		resetAt := int64(0)
		if !next.IsZero() {
			resetAt = next.Unix()
		}
		_, err = tx.Exec(`
				INSERT INTO relay_key_quota_state
				(relay_key_id, quota_generation, period, window_started_at, reset_at, tracking_started_at, updated_at)
				VALUES (?, 1, ?, ?, ?, ?, ?)
			`, key.ID, period, windowStart.Unix(), resetAt, now.Unix(), now.Unix())
		if err != nil {
			return nil, err
		}
		state = &relayQuotaState{
			KeyID: key.ID, Generation: 1, Period: period,
			WindowStartedAt: windowStart.Unix(), ResetAt: resetAt,
			TrackingStartedAt: now.Unix(),
		}
		return state, nil
	}

	period := normalizeRelayQuotaPeriod(key.QuotaPeriod)
	if normalizeRelayQuotaPeriod(state.Period) != period {
		// Configuration edits preserve counters but use the next boundary of the
		// newly selected calendar.  This transition is idempotent.
		windowStart, next := relayQuotaWindow(now, period)
		resetAt := int64(0)
		if !next.IsZero() {
			resetAt = next.Unix()
		}
		if _, err := tx.Exec(`UPDATE relay_key_quota_state SET period = ?, window_started_at = ?, reset_at = ?, updated_at = ? WHERE relay_key_id = ?`, period, windowStart.Unix(), resetAt, now.Unix(), key.ID); err != nil {
			return nil, err
		}
		state.Period = period
		state.WindowStartedAt = windowStart.Unix()
		state.ResetAt = resetAt
	}

	if state.ResetAt > 0 && now.Unix() >= state.ResetAt {
		start, next := relayQuotaWindow(now, period)
		resetAt := int64(0)
		if !next.IsZero() {
			resetAt = next.Unix()
		}
		if _, err := tx.Exec(`
			UPDATE relay_key_quota_state
			SET window_started_at = ?, reset_at = ?, input_tokens = 0,
			    cached_input_tokens = 0, output_tokens = 0, reasoning_tokens = 0,
			    total_tokens = 0, amount_nano_usd = 0, updated_at = ?
			WHERE relay_key_id = ?
		`, start.Unix(), resetAt, now.Unix(), key.ID); err != nil {
			return nil, err
		}
		state.WindowStartedAt = start.Unix()
		state.ResetAt = resetAt
		state.InputTokens = 0
		state.CachedInputTokens = 0
		state.OutputTokens = 0
		state.ReasoningTokens = 0
		state.TotalTokens = 0
		state.AmountNanoUSD = 0
	}
	return state, nil
}

func (s *RelayQuotaService) statusFromState(key *CodexRelayKey, state *relayQuotaState, now time.Time) RelayQuotaStatus {
	period := normalizeRelayQuotaPeriod("")
	if key != nil {
		period = normalizeRelayQuotaPeriod(key.QuotaPeriod)
	}
	status := RelayQuotaStatus{
		Period:         period,
		ServerTimezone: s.currentLocation().String(),
		Usage:          relayQuotaStateUsage(state),
	}
	if key != nil {
		status.TokenLimit = key.TokenLimit
		if status.TokenLimit < 0 {
			status.TokenLimit = 0
		}
		status.USDLimit, _ = normalizeRelayUSDLimit(key.USDLimit)
	}
	if state != nil {
		status.Period = normalizeRelayQuotaPeriod(state.Period)
		status.TokenUsed = state.TotalTokens
		status.USDUsed = formatRelayNanoUSD(state.AmountNanoUSD)
		status.WindowStartedAt = formatRelayTime(state.WindowStartedAt, s.currentLocation())
		status.TrackingStarted = state.TrackingStartedAt > 0
		if state.ResetAt > 0 {
			reset := formatRelayTime(state.ResetAt, s.currentLocation())
			status.ResetAt = &reset
		}
	} else {
		status.USDUsed = "0"
		// Keep the admin view useful before the first real call without creating
		// a ledger row or claiming that tracking has started.  The actual state
		// is created by Check/Settle when a request enters the relay.
		if relayQuotaHasLimits(key) && status.Period != RelayQuotaPeriodOnce {
			windowStart, next := relayQuotaWindow(now, status.Period)
			status.WindowStartedAt = formatRelayTime(windowStart.Unix(), s.currentLocation())
			if !next.IsZero() {
				reset := formatRelayTime(next.Unix(), s.currentLocation())
				status.ResetAt = &reset
			}
		}
	}
	if status.TokenLimit > 0 {
		remaining := status.TokenLimit - status.TokenUsed
		if remaining < 0 {
			remaining = 0
		}
		status.TokenRemaining = &remaining
		status.TokenExhausted = status.TokenUsed >= status.TokenLimit
	}
	usdLimitNano, _ := parseRelayDecimalNano(status.USDLimit)
	if usdLimitNano > 0 {
		used := int64(0)
		if state != nil {
			used = state.AmountNanoUSD
		}
		remainingNano := usdLimitNano - used
		if remainingNano < 0 {
			remainingNano = 0
		}
		remaining := formatRelayNanoUSD(remainingNano)
		status.USDRemaining = &remaining
		status.USDExhausted = used >= usdLimitNano
	}
	status.Blocked = status.TokenExhausted || status.USDExhausted
	_ = now
	return status
}

// Check performs the request-start check and lazily rolls the calendar window.
func (s *RelayQuotaService) Check(key *CodexRelayKey) (RelayQuotaDecision, error) {
	if s == nil || key == nil || strings.TrimSpace(key.ID) == "" {
		return RelayQuotaDecision{Allowed: true}, nil
	}
	// 无限额 key 不做准入判断，且不能等待 quota mutex：结算（含无限额 key 的
	// 异步结算）持锁执行 SQLite 事务，弱机上排队等锁会把新请求的发起推迟数十秒。
	// key 是每请求的副本，只读字段，锁外判定安全。
	if !relayQuotaHasLimits(key) {
		// Unlimited keys still get settlement tracking after the upstream call,
		// but do not need a request-start transaction or a state row.
		now := s.currentTime().In(s.currentLocation())
		return RelayQuotaDecision{Allowed: true, Status: s.statusFromState(key, nil, now)}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return RelayQuotaDecision{}, err
	}
	now := s.currentTime().In(s.currentLocation())
	db, err := xdb.DB("default")
	if err != nil {
		return RelayQuotaDecision{}, err
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return RelayQuotaDecision{}, err
	}
	state, err := s.ensureStateTx(tx, key, now)
	if err != nil {
		_ = tx.Rollback()
		return RelayQuotaDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return RelayQuotaDecision{}, err
	}
	status := s.statusFromState(key, state, now)
	decision := RelayQuotaDecision{Allowed: !status.Blocked, Status: status}
	if status.TokenExhausted && status.USDExhausted {
		decision.Reason = "token_and_usd"
	} else if status.TokenExhausted {
		decision.Reason = "token"
	} else if status.USDExhausted {
		decision.Reason = "usd"
	}
	return decision, nil
}

func (s *RelayQuotaService) lookupModelPrice(model string) (*relayModelPriceRecord, bool, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false, nil
	}
	db, err := xdb.DB("default")
	if err != nil {
		return nil, false, err
	}
	price := &relayModelPriceRecord{Model: model}
	var updated int64
	err = db.QueryRow(`
		SELECT model, input_price_nano, cached_input_price_nano, output_price_nano,
		       reasoning_output_price_nano, updated_at
		FROM relay_model_price WHERE model = ?`, model).Scan(
		&price.Model, &price.InputNano, &price.CachedInputNano, &price.OutputNano,
		&price.ReasoningNano, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return lookupBuiltinRelayModelPrice(model)
	}
	if err != nil {
		return nil, false, err
	}
	price.UpdatedAt = time.Unix(updated, 0)
	return price, true, nil
}

func markUnpricedModelTx(tx *sql.Tx, model string, now time.Time) error {
	if tx == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO relay_unpriced_model_seen(model, first_seen_at, last_seen_at, call_count)
		VALUES (?, ?, ?, 1)
		ON CONFLICT(model) DO UPDATE SET last_seen_at = excluded.last_seen_at, call_count = call_count + 1
	`, strings.TrimSpace(model), now.Unix(), now.Unix())
	return err
}

// Settle synchronously records one actual upstream attempt.  Duplicate
// attempt IDs are ignored, making retries from response hooks safe.
func (s *RelayQuotaService) Settle(key *CodexRelayKey, attemptID, provider, model string, usage RelayQuotaUsage) (RelayQuotaSettlementResult, error) {
	usage = sanitizeRelayUsage(usage)
	result := RelayQuotaSettlementResult{AttemptID: attemptID, Usage: usage}
	if s == nil || key == nil || strings.TrimSpace(key.ID) == "" || strings.TrimSpace(attemptID) == "" {
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return result, err
	}
	now := s.currentTime().In(s.currentLocation())
	price, priced, err := s.lookupModelPrice(model)
	if err != nil {
		return result, err
	}
	amount, priced, err := calculateRelayCostNanoUSD(usage, price)
	if err != nil {
		return result, err
	}
	result.Amount = formatRelayNanoUSD(amount)
	result.Priced = priced

	db, err := xdb.DB("default")
	if err != nil {
		return result, err
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return result, err
	}
	state, err := s.ensureStateTx(tx, key, now)
	if err != nil {
		_ = tx.Rollback()
		return result, err
	}
	generation := int64(0)
	if state != nil {
		generation = state.Generation
	}
	insertResult, err := tx.Exec(`
		INSERT OR IGNORE INTO relay_key_quota_settlement (
			attempt_id, relay_key_id, quota_generation, provider, actual_model,
			input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
			total_tokens, amount_nano_usd, priced,
			input_price_nano, cached_input_price_nano, output_price_nano,
			reasoning_price_nano, settled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, attemptID, key.ID, generation, provider, model,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ReasoningOutputTokens,
		usage.TotalTokens, amount, boolToInt(priced),
		priceValue(price, func(p *relayModelPriceRecord) int64 { return p.InputNano }),
		priceValue(price, func(p *relayModelPriceRecord) int64 { return p.CachedInputNano }),
		priceValue(price, func(p *relayModelPriceRecord) int64 { return p.OutputNano }),
		priceValue(price, func(p *relayModelPriceRecord) int64 { return p.ReasoningNano }), now.Unix())
	if err != nil {
		_ = tx.Rollback()
		return result, err
	}
	affected, err := insertResult.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return result, err
	}
	if affected == 0 {
		result.AlreadySet = true
		var (
			storedInput, storedCached, storedOutput, storedReasoning int64
			storedTotal, storedAmount                                int64
			storedPriced                                             int
		)
		if scanErr := tx.QueryRow(`
				SELECT input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
				       total_tokens, amount_nano_usd, priced
				FROM relay_key_quota_settlement WHERE attempt_id = ?`, attemptID).Scan(
			&storedInput, &storedCached, &storedOutput, &storedReasoning,
			&storedTotal, &storedAmount, &storedPriced); scanErr == nil {
			result.Usage = RelayQuotaUsage{
				InputTokens: storedInput, CachedInputTokens: storedCached,
				OutputTokens: storedOutput, ReasoningOutputTokens: storedReasoning,
				TotalTokens: storedTotal,
			}
			result.Amount = formatRelayNanoUSD(storedAmount)
			result.Priced = storedPriced != 0
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}
	if !priced {
		if err := markUnpricedModelTx(tx, model, now); err != nil {
			// The usage settlement remains authoritative even if the optional
			// reminder index cannot be updated in this transaction.
			fmt.Printf("[Relay Quota] unpriced model reminder failed model=%q: %v\n", model, err)
		}
	}
	if state != nil {
		newInput, err := checkedAddInt64(state.InputTokens, usage.InputTokens)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		newCached, err := checkedAddInt64(state.CachedInputTokens, usage.CachedInputTokens)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		newOutput, err := checkedAddInt64(state.OutputTokens, usage.OutputTokens)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		newReasoning, err := checkedAddInt64(state.ReasoningTokens, usage.ReasoningOutputTokens)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		newTotal, err := checkedAddInt64(state.TotalTokens, usage.TotalTokens)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		newAmount, err := checkedAddInt64(state.AmountNanoUSD, amount)
		if err != nil {
			_ = tx.Rollback()
			return result, err
		}
		if _, err := tx.Exec(`
			UPDATE relay_key_quota_state SET input_tokens = ?, cached_input_tokens = ?,
			output_tokens = ?, reasoning_tokens = ?, total_tokens = ?, amount_nano_usd = ?, updated_at = ?
			WHERE relay_key_id = ?
		`, newInput, newCached, newOutput, newReasoning, newTotal, newAmount, now.Unix(), key.ID); err != nil {
			_ = tx.Rollback()
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func priceValue(price *relayModelPriceRecord, getter func(*relayModelPriceRecord) int64) int64 {
	if price == nil {
		return 0
	}
	return getter(price)
}

// Status returns current usage and performs lazy period rollover.
func (s *RelayQuotaService) Status(key *CodexRelayKey) (RelayQuotaStatus, error) {
	if s == nil || key == nil {
		return RelayQuotaStatus{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return RelayQuotaStatus{}, err
	}
	now := s.currentTime().In(s.currentLocation())
	db, err := xdb.DB("default")
	if err != nil {
		return RelayQuotaStatus{}, err
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return RelayQuotaStatus{}, err
	}
	// A status read must not create tracking state for a key that has never
	// issued a real request.  Check for an existing row first; ensureStateTx is
	// still used below so existing rows retain lazy period rollover behavior.
	var stateExists int
	err = tx.QueryRow(`SELECT 1 FROM relay_key_quota_state WHERE relay_key_id = ?`, key.ID).Scan(&stateExists)
	if errors.Is(err, sql.ErrNoRows) {
		if commitErr := tx.Commit(); commitErr != nil {
			return RelayQuotaStatus{}, commitErr
		}
		return s.statusFromState(key, nil, now), nil
	}
	if err != nil {
		_ = tx.Rollback()
		return RelayQuotaStatus{}, err
	}
	state, err := s.ensureStateTx(tx, key, now)
	if err != nil {
		_ = tx.Rollback()
		return RelayQuotaStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return RelayQuotaStatus{}, err
	}
	return s.statusFromState(key, state, now), nil
}

// Reset clears current usage and advances the generation.  It is deliberately
// separate from configuration updates.
func (s *RelayQuotaService) Reset(key *CodexRelayKey) error {
	if s == nil || key == nil || strings.TrimSpace(key.ID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return err
	}
	if !relayQuotaHasLimits(key) {
		// A never-enabled unlimited key has no state to reset.
		db, err := xdb.DB("default")
		if err != nil {
			return err
		}
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM relay_key_quota_state WHERE relay_key_id = ?`, key.ID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return nil
		}
	}
	now := s.currentTime().In(s.currentLocation())
	db, err := xdb.DB("default")
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	state, err := s.ensureStateTx(tx, key, now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if state == nil {
		return tx.Commit()
	}
	period := normalizeRelayQuotaPeriod(key.QuotaPeriod)
	windowStart, next := relayQuotaWindow(now, period)
	resetAt := int64(0)
	if !next.IsZero() {
		resetAt = next.Unix()
	}
	if _, err := tx.Exec(`
		UPDATE relay_key_quota_state SET quota_generation = quota_generation + 1,
		period = ?, window_started_at = ?, reset_at = ?, input_tokens = 0,
		cached_input_tokens = 0, output_tokens = 0, reasoning_tokens = 0,
		total_tokens = 0, amount_nano_usd = 0, updated_at = ? WHERE relay_key_id = ?
	`, period, windowStart.Unix(), resetAt, now.Unix(), key.ID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func deleteRelayQuotaData(keyID string) error {
	if strings.TrimSpace(keyID) == "" {
		return nil
	}
	db, err := xdb.DB("default")
	if err != nil {
		// Key deletion should remain usable in environments where the optional
		// quota tables have not been initialized yet.
		return nil
	}
	for _, query := range []string{
		`DELETE FROM relay_key_quota_state WHERE relay_key_id = ?`,
		`DELETE FROM relay_key_quota_settlement WHERE relay_key_id = ?`,
	} {
		if _, err := db.Exec(query, keyID); err != nil && !isNoSuchTableErr(err) {
			return err
		}
	}
	return nil
}

// UpsertModelPrice creates or replaces a global model price.
func (s *RelayQuotaService) UpsertModelPrice(input RelayModelPrice) (*RelayModelPrice, error) {
	if s == nil {
		return nil, errors.New("额度服务不可用")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return nil, errors.New("模型名不能为空")
	}
	values := []string{input.Input, input.CachedInput, input.Output, input.ReasoningOutput}
	parsed := make([]int64, len(values))
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("价格字段 %d 不能为空", i+1)
		}
		nano, err := parseRelayDecimalNano(value)
		if err != nil {
			return nil, fmt.Errorf("价格字段 %d 无效: %w", i+1, err)
		}
		parsed[i] = nano
	}
	if err := ensureRelayQuotaTables(); err != nil {
		return nil, err
	}
	_, canRestore, err := lookupBuiltinRelayModelPrice(model)
	if err != nil {
		return nil, err
	}
	now := s.currentTime().In(s.currentLocation())
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		INSERT INTO relay_model_price
		(model, input_price_nano, cached_input_price_nano, output_price_nano, reasoning_output_price_nano, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
		input_price_nano = excluded.input_price_nano,
		cached_input_price_nano = excluded.cached_input_price_nano,
		output_price_nano = excluded.output_price_nano,
		reasoning_output_price_nano = excluded.reasoning_output_price_nano,
		updated_at = excluded.updated_at
	`, model, parsed[0], parsed[1], parsed[2], parsed[3], now.Unix())
	if err != nil {
		return nil, err
	}
	result := RelayModelPrice{
		Model: model, Input: formatRelayNanoUSD(parsed[0]), CachedInput: formatRelayNanoUSD(parsed[1]),
		Output: formatRelayNanoUSD(parsed[2]), ReasoningOutput: formatRelayNanoUSD(parsed[3]),
		UpdatedAt: now.Format(time.RFC3339), Source: "custom", CanRestore: canRestore,
	}
	return &result, nil
}

func (s *RelayQuotaService) ListModelPrices() ([]RelayModelPrice, error) {
	if s == nil {
		return nil, errors.New("额度服务不可用")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return nil, err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	builtin, err := builtinRelayModelPrices()
	if err != nil {
		return nil, err
	}
	merged := make(map[string]RelayModelPrice, len(builtin))
	for model, price := range builtin {
		merged[model] = relayModelPriceResponse(price, "builtin", false, s.currentLocation())
	}

	rows, err := db.Query(`SELECT model, input_price_nano, cached_input_price_nano, output_price_nano, reasoning_output_price_nano, updated_at FROM relay_model_price ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var price relayModelPriceRecord
		var updated int64
		if err := rows.Scan(&price.Model, &price.InputNano, &price.CachedInputNano, &price.OutputNano, &price.ReasoningNano, &updated); err != nil {
			return nil, err
		}
		price.UpdatedAt = time.Unix(updated, 0)
		_, canRestore := builtin[price.Model]
		merged[price.Model] = relayModelPriceResponse(price, "custom", canRestore, s.currentLocation())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	prices := make([]RelayModelPrice, 0, len(merged))
	for _, price := range merged {
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].Model < prices[j].Model })
	return prices, nil
}

func (s *RelayQuotaService) DeleteModelPrice(model string) error {
	if s == nil {
		return errors.New("额度服务不可用")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("模型名不能为空")
	}
	if err := ensureRelayQuotaTables(); err != nil {
		return err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM relay_model_price WHERE model = ?`, model)
	return err
}

type RelayUnpricedModel struct {
	Model       string `json:"model"`
	FirstSeenAt string `json:"firstSeenAt"`
	LastSeenAt  string `json:"lastSeenAt"`
	CallCount   int64  `json:"callCount"`
}

func (s *RelayQuotaService) ListUnpricedModels() ([]RelayUnpricedModel, error) {
	if s == nil {
		return nil, errors.New("额度服务不可用")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureRelayQuotaTables(); err != nil {
		return nil, err
	}
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	builtin, err := builtinRelayModelPrices()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT seen.model, seen.first_seen_at, seen.last_seen_at, seen.call_count
		FROM relay_unpriced_model_seen AS seen
		LEFT JOIN relay_model_price AS price ON price.model = seen.model
		WHERE price.model IS NULL
		ORDER BY seen.last_seen_at DESC, seen.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RelayUnpricedModel, 0)
	for rows.Next() {
		var model string
		var first, last, count int64
		if err := rows.Scan(&model, &first, &last, &count); err != nil {
			return nil, err
		}
		if _, priced := builtin[model]; priced {
			continue
		}
		result = append(result, RelayUnpricedModel{
			Model:       model,
			FirstSeenAt: formatRelayTime(first, s.currentLocation()),
			LastSeenAt:  formatRelayTime(last, s.currentLocation()),
			CallCount:   count,
		})
	}
	return result, rows.Err()
}

// MarshalJSON support for clients that send numeric USD values to the admin
// endpoint is handled by this helper rather than by using float64.
func relayUSDStringFromJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "0", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return normalizeRelayUSDLimit(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", errors.New("美元额度必须是字符串或数字")
	}
	return normalizeRelayUSDLimit(number.String())
}

// ParseRelayUSDLimit accepts the string/number forms used by the HTTP admin
// API and returns a canonical decimal string.
func ParseRelayUSDLimit(raw json.RawMessage) (string, error) {
	return relayUSDStringFromJSON(raw)
}
