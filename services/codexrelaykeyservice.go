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
	"unicode"
	"unicode/utf8"
)

const (
	codexRelayKeysFile      = "codex-relay-keys.json"
	defaultCodexRelayKeyTag = "default"
	maxCodexRelayKeyNameLen = 128
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
	// AllowedClaudeProviderIDs is independent from AllowedProviderIDs (Codex)
	// because provider IDs are allocated per kind and may overlap.
	AllowedClaudeProviderIDs []int64 `json:"allowedClaudeProviderIds,omitempty"`
	// AllowedGeminiProviderIDs uses string IDs because Gemini providers are
	// stored by GeminiService rather than ProviderService.
	AllowedGeminiProviderIDs []string `json:"allowedGeminiProviderIds,omitempty"`
	// ProviderBinding marks a provider-dedicated key created from a provider
	// card. A bound key may only reach that single provider; every other
	// provider kind and relay endpoint is denied regardless of the allowlists.
	ProviderBinding *RelayKeyProviderBinding `json:"providerBinding,omitempty"`
}

// RelayKeyProviderBinding identifies the single provider a dedicated key may
// access. Provider IDs are allocated per kind, so both fields are required.
// Gemini providers have string IDs and are identified by ProviderRef instead
// of ProviderID.
type RelayKeyProviderBinding struct {
	Kind        string `json:"kind"`
	ProviderID  int64  `json:"providerId"`
	ProviderRef string `json:"providerRef,omitempty"`
}

// UnmarshalJSON accepts both the current camelCase representation and the
// snake_case names used by early quota prototypes.
func (key *CodexRelayKey) UnmarshalJSON(data []byte) error {
	var value struct {
		ID                 string                   `json:"id"`
		Name               string                   `json:"name"`
		Key                string                   `json:"key"`
		Enabled            bool                     `json:"enabled"`
		CreatedAt          time.Time                `json:"createdAt"`
		TokenLimit         json.RawMessage          `json:"tokenLimit"`
		TokenLimitSnake    json.RawMessage          `json:"token_limit"`
		USDLimit           json.RawMessage          `json:"usdLimit"`
		USDLimitSnake      json.RawMessage          `json:"usd_limit"`
		QuotaPeriod        string                   `json:"quotaPeriod"`
		Period             string                   `json:"period"`
		QuotaPeriodSnake   string                   `json:"quota_period"`
		AllowedProviders   []int64                  `json:"allowedProviderIds"`
		AllowedSnake       []int64                  `json:"allowed_provider_ids"`
		AllowedClaude      []int64                  `json:"allowedClaudeProviderIds"`
		AllowedClaudeSnake []int64                  `json:"allowed_claude_provider_ids"`
		AllowedGemini      []string                 `json:"allowedGeminiProviderIds"`
		AllowedGeminiSnake []string                 `json:"allowed_gemini_provider_ids"`
		ProviderBinding    *RelayKeyProviderBinding `json:"providerBinding"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*key = CodexRelayKey{
		ID: value.ID, Name: value.Name, Key: value.Key, Enabled: value.Enabled, CreatedAt: value.CreatedAt,
		ProviderBinding: value.ProviderBinding,
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
	key.AllowedClaudeProviderIDs = value.AllowedClaude
	if key.AllowedClaudeProviderIDs == nil {
		key.AllowedClaudeProviderIDs = value.AllowedClaudeSnake
	}
	key.AllowedClaudeProviderIDs, err = NormalizeCodexAllowedProviderIDs(key.AllowedClaudeProviderIDs)
	if err != nil {
		return err
	}
	key.AllowedGeminiProviderIDs = value.AllowedGemini
	if key.AllowedGeminiProviderIDs == nil {
		key.AllowedGeminiProviderIDs = value.AllowedGeminiSnake
	}
	key.AllowedGeminiProviderIDs = NormalizeGeminiAllowedProviderIDs(key.AllowedGeminiProviderIDs)
	return nil
}

type CodexRelayKeyListItem struct {
	ID                       string                   `json:"id"`
	Name                     string                   `json:"name"`
	MaskedKey                string                   `json:"maskedKey"`
	Enabled                  bool                     `json:"enabled"`
	CreatedAt                time.Time                `json:"createdAt"`
	TokenLimit               int64                    `json:"tokenLimit"`
	USDLimit                 string                   `json:"usdLimit"`
	QuotaPeriod              string                   `json:"quotaPeriod"`
	AllowedProviderIDs       []int64                  `json:"allowedProviderIds"`
	AllowedClaudeProviderIDs []int64                  `json:"allowedClaudeProviderIds"`
	AllowedGeminiProviderIDs []string                 `json:"allowedGeminiProviderIds"`
	ProviderBinding          *RelayKeyProviderBinding `json:"providerBinding,omitempty"`
	Quota                    *RelayQuotaStatus        `json:"quota,omitempty"`
}

type CodexRelayKeyCreateResult struct {
	ID                       string    `json:"id"`
	Name                     string    `json:"name"`
	Key                      string    `json:"key"`
	Enabled                  bool      `json:"enabled"`
	CreatedAt                time.Time `json:"createdAt"`
	TokenLimit               int64     `json:"tokenLimit"`
	USDLimit                 string    `json:"usdLimit"`
	QuotaPeriod              string    `json:"quotaPeriod"`
	AllowedProviderIDs       []int64   `json:"allowedProviderIds"`
	AllowedClaudeProviderIDs []int64   `json:"allowedClaudeProviderIds"`
	AllowedGeminiProviderIDs []string  `json:"allowedGeminiProviderIds"`
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
			ID:                       key.ID,
			Name:                     key.Name,
			MaskedKey:                maskCodexRelayKey(key.Key),
			Enabled:                  key.Enabled,
			CreatedAt:                key.CreatedAt,
			TokenLimit:               key.TokenLimit,
			USDLimit:                 normalizedKeyUSD(key.USDLimit),
			QuotaPeriod:              normalizeRelayQuotaPeriod(key.QuotaPeriod),
			AllowedProviderIDs:       append([]int64{}, key.AllowedProviderIDs...),
			AllowedClaudeProviderIDs: append([]int64{}, key.AllowedClaudeProviderIDs...),
			AllowedGeminiProviderIDs: append([]string{}, key.AllowedGeminiProviderIDs...),
			ProviderBinding:          cloneRelayKeyProviderBinding(key.ProviderBinding),
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

	normalizedName := strings.TrimSpace(name)
	if normalizedName != "" {
		normalizedName, err = normalizeCodexRelayKeyName(normalizedName)
		if err != nil {
			return nil, err
		}
	}

	value, err := generateCodexRelayKeyValue()
	if err != nil {
		return nil, err
	}

	key := CodexRelayKey{
		ID:          fmt.Sprintf("codex-key-%d", time.Now().UnixNano()),
		Name:        normalizedName,
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
		ID:                       key.ID,
		Name:                     key.Name,
		Key:                      key.Key,
		Enabled:                  key.Enabled,
		CreatedAt:                key.CreatedAt,
		TokenLimit:               key.TokenLimit,
		USDLimit:                 normalizedKeyUSD(key.USDLimit),
		QuotaPeriod:              normalizeRelayQuotaPeriod(key.QuotaPeriod),
		AllowedProviderIDs:       append([]int64{}, key.AllowedProviderIDs...),
		AllowedClaudeProviderIDs: append([]int64{}, key.AllowedClaudeProviderIDs...),
		AllowedGeminiProviderIDs: append([]string{}, key.AllowedGeminiProviderIDs...),
	}, nil
}

func normalizeCodexRelayKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("Key 名称不能为空")
	}
	if utf8.RuneCountInString(name) > maxCodexRelayKeyNameLen {
		return "", fmt.Errorf("Key 名称不能超过 %d 个字符", maxCodexRelayKeyNameLen)
	}
	for _, value := range name {
		if unicode.IsControl(value) {
			return "", errors.New("Key 名称不能包含控制字符")
		}
	}
	return name, nil
}

func (s *CodexRelayKeyService) DeleteKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return err
	}

	// Provider-dedicated keys cannot serve as the general relay key, so they
	// neither count toward nor are blocked by the "keep one usable key" rule.
	enabledCount := 0
	for _, key := range store.Keys {
		if key.Enabled && key.ProviderBinding == nil {
			enabledCount++
		}
	}

	filtered := make([]CodexRelayKey, 0, len(store.Keys))
	found := false
	for _, key := range store.Keys {
		if key.ID == id {
			found = true
			if key.Enabled && key.ProviderBinding == nil && enabledCount <= 1 {
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

// UpdateName changes only the human-readable label for a relay key. The key
// secret, quota ledger and provider access configuration are left untouched.
func (s *CodexRelayKeyService) UpdateName(id, name string) (string, error) {
	normalized, err := normalizeCodexRelayKeyName(name)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return "", err
	}
	for index := range store.Keys {
		if store.Keys[index].ID == id {
			store.Keys[index].Name = normalized
			if err := s.saveLocked(store); err != nil {
				return "", err
			}
			return normalized, nil
		}
	}
	return "", fmt.Errorf("未找到 Codex relay key: %s", id)
}

// UpdateAllowedProviderIDs replaces the Codex provider allowlist for one key.
// An empty list means unrestricted access, preserving the behavior of keys
// created before provider access controls were introduced.
func (s *CodexRelayKeyService) UpdateAllowedProviderIDs(id string, providerIDs []int64) error {
	return s.updateAllowlist(id, providerIDs, func(key *CodexRelayKey, ids []int64) {
		key.AllowedProviderIDs = ids
	})
}

// UpdateAllowedClaudeProviderIDs replaces the Claude provider allowlist for
// one key. An empty list means unrestricted access.
func (s *CodexRelayKeyService) UpdateAllowedClaudeProviderIDs(id string, providerIDs []int64) error {
	return s.updateAllowlist(id, providerIDs, func(key *CodexRelayKey, ids []int64) {
		key.AllowedClaudeProviderIDs = ids
	})
}

// UpdateAllowedGeminiProviderIDs replaces the Gemini provider allowlist for
// one key. An empty list means unrestricted access.
func (s *CodexRelayKeyService) UpdateAllowedGeminiProviderIDs(id string, providerIDs []string) error {
	normalized := NormalizeGeminiAllowedProviderIDs(providerIDs)
	return s.updateKeyAccess(id, func(key *CodexRelayKey) {
		key.AllowedGeminiProviderIDs = normalized
	})
}

func (s *CodexRelayKeyService) updateAllowlist(id string, providerIDs []int64, apply func(*CodexRelayKey, []int64)) error {
	normalized, err := NormalizeCodexAllowedProviderIDs(providerIDs)
	if err != nil {
		return err
	}
	return s.updateKeyAccess(id, func(key *CodexRelayKey) {
		apply(key, normalized)
	})
}

func (s *CodexRelayKeyService) updateKeyAccess(id string, apply func(*CodexRelayKey)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return err
	}
	for index := range store.Keys {
		if store.Keys[index].ID == id {
			if store.Keys[index].ProviderBinding != nil {
				return errors.New("供应商专属 Key 的访问范围固定为所属供应商，不能修改")
			}
			apply(&store.Keys[index])
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

// ListProviderKeys returns the dedicated keys bound to providers of kind.
func (s *CodexRelayKeyService) ListProviderKeys(kind string) ([]CodexRelayKeyListItem, error) {
	keys, err := s.ListKeys()
	if err != nil {
		return nil, err
	}
	result := make([]CodexRelayKeyListItem, 0)
	for _, key := range keys {
		if key.ProviderBinding != nil && key.ProviderBinding.Kind == kind {
			result = append(result, key)
		}
	}
	return result, nil
}

// EnsureProviderKey returns the dedicated key for one provider, creating it on
// first use. The caller is responsible for verifying that the provider exists.
func (s *CodexRelayKeyService) EnsureProviderKey(kind string, providerID int64, name string) (*CodexRelayKey, bool, error) {
	if !isBindableRelayProviderKind(kind) || kind == ProviderKindGemini {
		return nil, false, fmt.Errorf("不支持为 %s 供应商创建专属 Key", kind)
	}
	if providerID < 0 {
		return nil, false, fmt.Errorf("provider ID 不能为负数: %d", providerID)
	}
	return s.ensureBoundKey(RelayKeyProviderBinding{Kind: kind, ProviderID: providerID}, name)
}

// EnsureGeminiProviderKey returns the dedicated key for one Gemini provider,
// creating it on first use. The caller verifies that the provider exists.
func (s *CodexRelayKeyService) EnsureGeminiProviderKey(providerID string, name string) (*CodexRelayKey, bool, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, false, errors.New("Gemini provider ID 不能为空")
	}
	return s.ensureBoundKey(RelayKeyProviderBinding{Kind: ProviderKindGemini, ProviderRef: providerID}, name)
}

func (s *CodexRelayKeyService) ensureBoundKey(binding RelayKeyProviderBinding, name string) (*CodexRelayKey, bool, error) {
	normalizedName, err := normalizeCodexRelayKeyName(name)
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.loadLocked()
	if err != nil {
		return nil, false, err
	}
	for _, key := range store.Keys {
		if existing := key.ProviderBinding; existing != nil && *existing == binding {
			copyKey := cloneCodexRelayKey(key)
			return &copyKey, false, nil
		}
	}

	value, err := generateCodexRelayKeyValue()
	if err != nil {
		return nil, false, err
	}
	key := CodexRelayKey{
		ID:              fmt.Sprintf("codex-key-%d", time.Now().UnixNano()),
		Name:            normalizedName,
		Key:             value,
		Enabled:         true,
		CreatedAt:       time.Now().UTC(),
		QuotaPeriod:     normalizeRelayQuotaPeriod(""),
		ProviderBinding: &binding,
	}
	store.Keys = append(store.Keys, key)
	if err := s.saveLocked(store); err != nil {
		return nil, false, err
	}
	copyKey := cloneCodexRelayKey(key)
	return &copyKey, true, nil
}

// PruneProviderKeys deletes dedicated keys of kind whose provider is no longer
// configured. It runs after every provider save so a deleted provider's key is
// removed before its ID could ever be reused by a new provider.
func (s *CodexRelayKeyService) PruneProviderKeys(kind string, existingProviderIDs []int64) ([]string, error) {
	if !isBindableRelayProviderKind(kind) || kind == ProviderKindGemini {
		return nil, nil
	}
	existing := make(map[int64]struct{}, len(existingProviderIDs))
	for _, providerID := range existingProviderIDs {
		existing[providerID] = struct{}{}
	}
	return s.pruneBoundKeys(kind, func(binding *RelayKeyProviderBinding) bool {
		_, ok := existing[binding.ProviderID]
		return ok
	})
}

// PruneGeminiProviderKeys deletes Gemini dedicated keys whose provider is no
// longer configured.
func (s *CodexRelayKeyService) PruneGeminiProviderKeys(existingProviderIDs []string) ([]string, error) {
	existing := make(map[string]struct{}, len(existingProviderIDs))
	for _, providerID := range existingProviderIDs {
		existing[providerID] = struct{}{}
	}
	return s.pruneBoundKeys(ProviderKindGemini, func(binding *RelayKeyProviderBinding) bool {
		_, ok := existing[binding.ProviderRef]
		return ok
	})
}

func (s *CodexRelayKeyService) pruneBoundKeys(kind string, exists func(*RelayKeyProviderBinding) bool) ([]string, error) {
	s.mu.Lock()
	store, err := s.loadLocked()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	removed := make([]string, 0)
	kept := make([]CodexRelayKey, 0, len(store.Keys))
	for _, key := range store.Keys {
		if binding := key.ProviderBinding; binding != nil && binding.Kind == kind && !exists(binding) {
			removed = append(removed, key.ID)
			continue
		}
		kept = append(kept, key)
	}
	if len(removed) > 0 {
		store.Keys = kept
		if err := s.saveLocked(store); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()

	for _, id := range removed {
		if err := deleteRelayQuotaData(id); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *CodexRelayKeyService) EnsureDefaultKey() (*CodexRelayKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	store, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	for _, key := range store.Keys {
		// Dedicated keys are restricted to one provider and must never be
		// injected into local CLI configs as the general relay key.
		if key.Enabled && key.ProviderBinding == nil {
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
		normalizedClaude, err := NormalizeCodexAllowedProviderIDs(key.AllowedClaudeProviderIDs)
		if err != nil {
			return nil, fmt.Errorf("Codex relay key %q Claude provider access is invalid: %w", key.ID, err)
		}
		if !equalInt64Slices(key.AllowedClaudeProviderIDs, normalizedClaude) {
			key.AllowedClaudeProviderIDs = normalizedClaude
			changed = true
		}
		normalizedGemini := NormalizeGeminiAllowedProviderIDs(key.AllowedGeminiProviderIDs)
		if !equalStringSlices(key.AllowedGeminiProviderIDs, normalizedGemini) {
			key.AllowedGeminiProviderIDs = normalizedGemini
			changed = true
		}
		if binding := key.ProviderBinding; binding != nil {
			if !isBindableRelayProviderKind(binding.Kind) || binding.ProviderID < 0 ||
				(binding.Kind == ProviderKindGemini && strings.TrimSpace(binding.ProviderRef) == "") {
				return nil, fmt.Errorf("Codex relay key %q provider binding is invalid", key.ID)
			}
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

// NormalizeGeminiAllowedProviderIDs trims, deduplicates, and sorts Gemini
// provider IDs. Empty means all Gemini providers are allowed.
func NormalizeGeminiAllowedProviderIDs(providerIDs []string) []string {
	if len(providerIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(providerIDs))
	result := make([]string, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		if _, exists := seen[providerID]; exists {
			continue
		}
		seen[providerID] = struct{}{}
		result = append(result, providerID)
	}
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

func cloneCodexRelayKey(key CodexRelayKey) CodexRelayKey {
	key.AllowedProviderIDs = append([]int64(nil), key.AllowedProviderIDs...)
	key.AllowedClaudeProviderIDs = append([]int64(nil), key.AllowedClaudeProviderIDs...)
	key.AllowedGeminiProviderIDs = append([]string(nil), key.AllowedGeminiProviderIDs...)
	key.ProviderBinding = cloneRelayKeyProviderBinding(key.ProviderBinding)
	return key
}

func cloneRelayKeyProviderBinding(binding *RelayKeyProviderBinding) *RelayKeyProviderBinding {
	if binding == nil {
		return nil
	}
	copyBinding := *binding
	return &copyBinding
}

func isBindableRelayProviderKind(kind string) bool {
	return kind == ProviderKindCodex || kind == ProviderKindClaude || kind == ProviderKindGemini
}

func equalStringSlices(left, right []string) bool {
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

// MaskCodexRelayKey returns the display form of a relay key secret.
func MaskCodexRelayKey(value string) string {
	return maskCodexRelayKey(value)
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
