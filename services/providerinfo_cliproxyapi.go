package services

import (
	"encoding/json"
	"sort"
	"strings"
)

type CLIProxyModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

type CLIProxyModels struct {
	Data []CLIProxyModel `json:"data"`
}

func (s *ProviderInfoService) queryCLIProxyAPI(cacheKey, base, key string, force bool) *ProviderInfoResult {
	models := s.section(cacheKey+":models", base+"/v1/models", key, force, "cliproxyapi_models", "")
	result := &ProviderInfoResult{
		Platform:     "cliproxyapi",
		ModelsState:  &models.state,
		UsageState:   ProviderInfoSection{Status: "unsupported"},
		BillingState: ProviderInfoSection{Status: "unsupported"},
	}
	if len(models.data) > 0 {
		_ = json.Unmarshal(models.data, &result.Models)
	}
	return result
}

func decodeCLIProxyModels(body []byte) (json.RawMessage, string) {
	var models CLIProxyModels
	if json.Unmarshal(body, &models) != nil || models.Data == nil {
		return nil, "invalid_response"
	}
	seen := make(map[string]bool, len(models.Data))
	filtered := make([]CLIProxyModel, 0, len(models.Data))
	for _, model := range models.Data {
		if strings.TrimSpace(model.ID) == "" {
			return nil, "invalid_response"
		}
		if !seen[model.ID] {
			seen[model.ID] = true
			filtered = append(filtered, model)
		}
	}
	sort.Slice(filtered, func(first, second int) bool { return filtered[first].ID < filtered[second].ID })
	models.Data = filtered
	data, err := json.Marshal(models)
	if err != nil {
		return nil, "invalid_response"
	}
	return data, "ready"
}
