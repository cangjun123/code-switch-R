package services

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexRelayKeysFile      = "codex-relay-keys.json"
	defaultCodexRelayKeyTag = "default"
)

type CodexRelayKey struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Key                string    `json:"key"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"createdAt"`
	TokenLimit         int64     `json:"tokenLimit"`
	USDLimit           string    `json:"usdLimit"`
	QuotaPeriod        string    `json:"quotaPeriod"`
	AllowedProviderIDs []int64   `json:"allowedProviderIds,omitempty"`
}

// UnmarshalJSON accepts both the current camelCase representation and the
// snake_case names used by early quota prototypes.
func (key *CodexRelayKey) UnmarshalJSON(data []byte) error {
	var value struct {
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		Key              string          `json:"key"`
		Enabled          bool            `json:"enabled"`
		CreatedAt        time.Time       `json:"createdAt"`
		TokenLimit       json.RawMessage `json:"tokenLimit"`
		TokenLimitSnake  json.RawMessage `json:"token_limit"`
		USDLimit         json.RawMessage `json:"usdLimit"`
		USDLimitSnake    json.RawMessage `json:"usd_limit"`
		QuotaPeriod      string          `json:"quotaPeriod"`
		Period           string          `json:"period"`
		QuotaPeriodSnake string          `json:"quota_period"`
		AllowedProviders []int64         `json:"allowedProviderIds"`
		AllowedSnake     []int64         `json:"allowed_provider_ids"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*key = CodexRelayKey{
		ID: value.ID, Name: value.Name, Key: value.Key, Enabled: value.Enabled, CreatedAt: value.CreatedAt,
	}
	tokenRaw := value.TokenLimit
	if len(tokenRaw) == 0 {
		tokenRaw = value.TokenLimitSnake
	}
	if len(tokenRaw) > 0 && string(tokenRaw) != "null" {
		var number json.Number
		if err := json.Unmarshal(tokenRaw, &number); err == nil {
			if parsed, parseErr := strconv.ParseInt(number.String(), 10, 64); parseErr == nil {
				key.TokenLimit = parsed
			} else {
				return fmt.Errorf("invalid tokenLimit: %w", parseErr)
			}
		} else {
			var text string
			if textErr := json.Unmarshal(tokenRaw, &text); textErr != nil {
				return fmt.Errorf("invalid tokenLimit: %w", textErr)
			}
			parsed, parseErr := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if parseErr != nil {
				return fmt.Errorf("invalid tokenLimit: %w", parseErr)
			}
			key.TokenLimit = parsed
		}
	}
	usdRaw := value.USDLimit
	if len(usdRaw) == 0 {
		usdRaw = value.USDLimitSnake
	}
	if len(usdRaw) > 0 && string(usdRaw) != "null" {
		var text string
		if err := json.Unmarshal(usdRaw, &text); err == nil {
			key.USDLimit = strings.TrimSpace(text)
		} else {
			var number json.Number
			if err := json.Unmarshal(usdRaw, &number); err != nil {
				return fmt.Errorf("invalid usdLimit: %w", err)
			}
			key.USDLimit = number.String()
		}
	}
	key.QuotaPeriod = strings.TrimSpace(value.QuotaPeriod)
	if key.QuotaPeriod == "" {
		key.QuotaPeriod = strings.TrimSpace(value.Period)
	}
	if key.QuotaPeriod == "" {
		key.QuotaPeriod = strings.TrimSpace(value.QuotaPeriodSnake)
	}
	key.AllowedProviderIDs = value.AllowedProviders
	if key.AllowedProviderIDs == nil {
		key.AllowedProviderIDs = value.AllowedSnake
	}
	var err error
	key.AllowedProviderIDs, err = NormalizeCodexAllowedProviderIDs(key.AllowedProviderIDs)
	if err != nil {
		return err
	}
	return nil
}

type CodexRelayKeyListItem struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	MaskedKey          string            `json:"maskedKey"`
	Enabled            bool              `json:"enabled"`
	CreatedAt          time.Time         `json:"createdAt"`
	TokenLimit         int64             `json:"tokenLimit"`
	USDLimit           string            `json:"usdLimit"`
	QuotaPeriod        string            `json:"quotaPeriod"`
	AllowedProviderIDs []int64           `json:"allowedProviderIds"`
	Quota              *RelayQuotaStatus `json:"quota,omitempty"`
}

type CodexRelayKeyCreateResult struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Key                string    `json:"key"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"createdAt"`
	TokenLimit         int64     `json:"tokenLimit"`
	USDLimit           string    `json:"usdLimit"`
	QuotaPeriod        string    `json:"quotaPeriod"`
	AllowedProviderIDs []int64   `json:"allowedProviderIds"`
}

// CodexRelayKeyMatch is the minimal authentication result retained for
// callers that do not need the full quota configuration.
type CodexRelayKeyMatch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type codexRelayKeyStore struct {
	Keys []CodexRelayKey `json:"keys"`
}

type CodexRelayKeyService struct {
	path string
	mu   sync.Mutex
}

func NewCodexRelayKeyService() *CodexRelayKeyService {
	home, err := getUserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		home = "."
	}

	return &CodexRelayKeyService{
		path: filepath.Join(home, appSettingsDir, codexRelayKeysFile),
	}
}

func (s *CodexRelayKeyService) ListKeys() ([]CodexRelayKeyListItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	keys := make([]CodexRelayKeyListItem, 0, len(store.Keys))
	for _, key := range store.Keys {
		keys = append(keys, CodexRelayKeyListItem{
			ID:                 key.ID,
			Name:               key.Name,
			MaskedKey:          maskCodexRelayKey(key.Key),
			Enabled:            key.Enabled,
			CreatedAt:          key.CreatedAt,
			TokenLimit:         key.TokenLimit,
			USDLimit:           normalizedKeyUSD(key.USDLimit),
			QuotaPeriod:        normalizeRelayQuotaPeriod(key.QuotaPeriod),
			AllowedProviderIDs: append([]int64{}, key.AllowedProviderIDs...),
		})
	}

	return keys, nil
}

func (s *CodexRelayKeyService) CreateKey(name string) (*CodexRelayKeyCreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	value, err := generateCodexRelayKeyValue()
	if err != nil {
		return nil, err
	}

	key := CodexRelayKey{
		ID:          fmt.Sprintf("codex-key-%d", time.Now().UnixNano()),
		Name:        strings.TrimSpace(name),
		Key:         value,
		Enabled:     true,
		CreatedAt:   time.Now().UTC(),
		QuotaPeriod: normalizeRelayQuotaPeriod(""),
	}
	if key.Name == "" {
		key.Name = fmt.Sprintf("key-%d", len(store.Keys)+1)
	}

	store.Keys = append(store.Keys, key)
	if err := s.saveLocked(store); err != nil {
		return nil, err
	}

	return &CodexRelayKeyCreateResult{
		ID:                 key.ID,
		Name:               key.Name,
		Key:                key.Key,
		Enabled:            key.Enabled,
		CreatedAt:          key.CreatedAt,
		TokenLimit:         key.TokenLimit,
		USDLimit:           normalizedKeyUSD(key.USDLimit),
		QuotaPeriod:        normalizeRelayQuotaPeriod(key.QuotaPeriod),
		AllowedProviderIDs: append([]int64{}, key.AllowedProviderIDs...),
	}, nil
}

func (s *CodexRelayKeyService) DeleteKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return err
	}

	enabledCount := 0
	for _, key := range store.Keys {
		if key.Enabled {
			enabledCount++
		}
	}

	filtered := make([]CodexRelayKey, 0, len(store.Keys))
	found := false
	for _, key := range store.Keys {
		if key.ID == id {
			found = true
			if key.Enabled && enabledCount <= 1 {
				return errors.New("至少需要保留一个可用的 Codex relay key")
			}
			continue
		}
		filtered = append(filtered, key)
	}
	if !found {
		return fmt.Errorf("未找到 Codex relay key: %s", id)
	}

	store.Keys = filtered
	if err := s.saveLocked(store); err != nil {
		return err
	}
	// 额度账本独立于 request_log，删除 Key 时同步清理其当前状态和去重记录。
	return deleteRelayQuotaData(id)
}

// ListKeyRecords 返回完整的 Key 配置。调用方不得修改返回值并期待自动持久化。
func (s *CodexRelayKeyService) ListKeyRecords() ([]CodexRelayKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	result := make([]CodexRelayKey, len(store.Keys))
	for index := range store.Keys {
		result[index] = cloneCodexRelayKey(store.Keys[index])
	}
	return result, nil
}

// GetKeyByID 返回指定 Key 的配置副本。
func (s *CodexRelayKeyService) GetKeyByID(id string) (*CodexRelayKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	for _, key := range store.Keys {
		if key.ID == id {
			copyKey := cloneCodexRelayKey(key)
			copyKey.QuotaPeriod = normalizeRelayQuotaPeriod(copyKey.QuotaPeriod)
			return &copyKey, nil
		}
	}
	return nil, fmt.Errorf("未找到 Codex relay key: %s", id)
}

// UpdateAllowedProviderIDs replaces the Codex provider allowlist for one key.
// An empty list means unrestricted access, preserving the behavior of keys
// created before provider access controls were introduced.
func (s *CodexRelayKeyService) UpdateAllowedProviderIDs(id string, providerIDs []int64) error {
	normalized, err := NormalizeCodexAllowedProviderIDs(providerIDs)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return err
	}
	for index := range store.Keys {
		if store.Keys[index].ID == id {
			store.Keys[index].AllowedProviderIDs = normalized
			return s.saveLocked(store)
		}
	}
	return fmt.Errorf("未找到 Codex relay key: %s", id)
}

// UpdateQuotaConfig 更新额度配置。修改配置不会清除已累计用量；状态服务会在下一次访问时同步周期变化。
func (s *CodexRelayKeyService) UpdateQuotaConfig(id string, tokenLimit int64, usdLimit string, period string) error {
	canonicalPeriod, err := validateRelayQuotaPeriod(period)
	if err != nil {
		return err
	}
	canonicalUSD, err := ValidateRelayQuotaLimits(tokenLimit, usdLimit)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return err
	}
	for index := range store.Keys {
		if store.Keys[index].ID == id {
			store.Keys[index].TokenLimit = tokenLimit
			store.Keys[index].USDLimit = canonicalUSD
			store.Keys[index].QuotaPeriod = canonicalPeriod
			return s.saveLocked(store)
		}
	}
	return fmt.Errorf("未找到 Codex relay key: %s", id)
}

func (s *CodexRelayKeyService) GetKeySecret(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return "", err
	}

	for _, key := range store.Keys {
		if key.ID == id {
			return key.Key, nil
		}
	}

	return "", fmt.Errorf("未找到 Codex relay key: %s", id)
}

func (s *CodexRelayKeyService) EnsureDefaultKey() (*CodexRelayKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	for _, key := range store.Keys {
		if key.Enabled {
			copyKey := cloneCodexRelayKey(key)
			return &copyKey, nil
		}
	}

	value, err := generateCodexRelayKeyValue()
	if err != nil {
		return nil, err
	}

	key := CodexRelayKey{
		ID:          fmt.Sprintf("codex-key-%d", time.Now().UnixNano()),
		Name:        defaultCodexRelayKeyTag,
		Key:         value,
		Enabled:     true,
		CreatedAt:   time.Now().UTC(),
		QuotaPeriod: normalizeRelayQuotaPeriod(""),
	}
	store.Keys = append(store.Keys, key)
	if err := s.saveLocked(store); err != nil {
		return nil, err
	}

	return &key, nil
}

func (s *CodexRelayKeyService) ValidateKey(candidate string) (bool, error) {
	key, err := s.FindKey(candidate)
	return key != nil, err
}

func (s *CodexRelayKeyService) ValidateKeyMatch(candidate string) (*CodexRelayKeyMatch, error) {
	key, err := s.FindKey(candidate)
	if err != nil || key == nil {
		return nil, err
	}
	return &CodexRelayKeyMatch{ID: key.ID, Name: key.Name}, nil
}

// FindKey 在常量时间比较后返回完整 Key 配置。保留 ValidateKey 供旧调用方使用。
func (s *CodexRelayKeyService) FindKey(candidate string) (*CodexRelayKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil, nil
	}

	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	for _, key := range store.Keys {
		if !key.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(key.Key)) == 1 {
			copyKey := cloneCodexRelayKey(key)
			copyKey.QuotaPeriod = normalizeRelayQuotaPeriod(copyKey.QuotaPeriod)
			return &copyKey, nil
		}
	}

	return nil, nil
}

func (s *CodexRelayKeyService) loadLocked() (*codexRelayKeyStore, error) {
	store := &codexRelayKeyStore{
		Keys: []CodexRelayKey{},
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return store, nil
	}

	if err := json.Unmarshal(data, store); err != nil {
		return nil, err
	}
	if store.Keys == nil {
		store.Keys = []CodexRelayKey{}
	}

	// 兼容早期 JSON：补齐稳定 ID 和周期默认值。只在确有变化时写回，避免每个请求都产生磁盘写入。
	changed := false
	seenIDs := make(map[string]struct{}, len(store.Keys))
	for index := range store.Keys {
		key := &store.Keys[index]
		if strings.TrimSpace(key.ID) == "" {
			key.ID = fmt.Sprintf("codex-key-%d-%d", time.Now().UnixNano(), index)
			changed = true
		}
		if _, exists := seenIDs[key.ID]; exists {
			key.ID = fmt.Sprintf("codex-key-%d-%d", time.Now().UnixNano(), index)
			changed = true
		}
		seenIDs[key.ID] = struct{}{}
		if key.TokenLimit < 0 {
			// Treat invalid legacy values as unlimited instead of allowing a
			// negative limit to produce misleading status or block every request.
			key.TokenLimit = 0
			changed = true
		}
		canonicalPeriod := normalizeRelayQuotaPeriod(key.QuotaPeriod)
		if key.QuotaPeriod != canonicalPeriod {
			key.QuotaPeriod = canonicalPeriod
			changed = true
		}
		normalizedProviders, err := NormalizeCodexAllowedProviderIDs(key.AllowedProviderIDs)
		if err != nil {
			return nil, fmt.Errorf("Codex relay key %q provider access is invalid: %w", key.ID, err)
		}
		if !equalInt64Slices(key.AllowedProviderIDs, normalizedProviders) {
			key.AllowedProviderIDs = normalizedProviders
			changed = true
		}
	}
	if changed {
		if err := s.saveLocked(store); err != nil {
			return nil, err
		}
	}

	return store, nil
}

// NormalizeCodexAllowedProviderIDs validates, deduplicates, and sorts provider
// IDs. Empty means all Codex providers are allowed.
func NormalizeCodexAllowedProviderIDs(providerIDs []int64) ([]int64, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(providerIDs))
	result := make([]int64, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		if providerID < 0 {
			return nil, fmt.Errorf("provider ID 不能为负数: %d", providerID)
		}
		if _, exists := seen[providerID]; exists {
			continue
		}
		seen[providerID] = struct{}{}
		result = append(result, providerID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func cloneCodexRelayKey(key CodexRelayKey) CodexRelayKey {
	key.AllowedProviderIDs = append([]int64(nil), key.AllowedProviderIDs...)
	return key
}

func equalInt64Slices(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *CodexRelayKeyService) saveLocked(store *codexRelayKeyStore) error {
	if err := EnsureDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	return AtomicWriteJSON(s.path, store)
}

func generateCodexRelayKeyValue() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 Codex relay key 失败: %w", err)
	}
	return "csk_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func maskCodexRelayKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return value[:4] + "..." + value[len(value)-2:]
	}
	return value[:7] + "..." + value[len(value)-4:]
}

func normalizedKeyUSD(value string) string {
	normalized, err := normalizeRelayUSDLimit(value)
	if err != nil {
		return "0"
	}
	return normalized
}
