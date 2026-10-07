package services

import (
	"encoding/json"
	"regexp"
)

type DeepSeekBalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

type DeepSeekBalance struct {
	IsAvailable  *bool                 `json:"is_available"`
	BalanceInfos []DeepSeekBalanceInfo `json:"balance_infos"`
}

var deepSeekAmountPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var deepSeekCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *ProviderInfoService) queryDeepSeek(cacheKey, base, key string, force bool) *ProviderInfoResult {
	balance := s.section(cacheKey+":balance", base+"/user/balance", key, force, "deepseek_balance", "")
	result := &ProviderInfoResult{
		Platform:     "deepseek",
		BalanceState: &balance.state,
		UsageState:   ProviderInfoSection{Status: "unsupported"},
		BillingState: ProviderInfoSection{Status: "unsupported"},
	}
	if len(balance.data) > 0 {
		_ = json.Unmarshal(balance.data, &result.Balance)
	}
	return result
}

func decodeDeepSeekBalance(body []byte) (json.RawMessage, string) {
	var balance DeepSeekBalance
	if json.Unmarshal(body, &balance) != nil || balance.IsAvailable == nil || balance.BalanceInfos == nil {
		return nil, "invalid_response"
	}
	seen := make(map[string]bool, len(balance.BalanceInfos))
	for _, row := range balance.BalanceInfos {
		if !deepSeekCurrencyPattern.MatchString(row.Currency) || seen[row.Currency] || !deepSeekAmountPattern.MatchString(row.TotalBalance) || !deepSeekAmountPattern.MatchString(row.GrantedBalance) || !deepSeekAmountPattern.MatchString(row.ToppedUpBalance) {
			return nil, "invalid_response"
		}
		seen[row.Currency] = true
	}
	data, err := json.Marshal(balance)
	if err != nil {
		return nil, "invalid_response"
	}
	return data, "ready"
}
