package plugin

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"

	"github.com/dillonzq/cpa-deepseek/internal/catalog"
	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var _ pluginapi.QuotaProvider = (*Manager)(nil)

func (*Manager) Identifier() string { return ProviderID }
func (*Manager) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{ProviderID}, DisplayName: "DeepSeek account balance", SupportsReset: false}, nil
}
func (*Manager) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, &errclass.Error{Class: errclass.ClassUnsupported, Message: "DeepSeek does not support resetting account balance"}
}

type balanceInfo struct {
	Currency string  `json:"currency"`
	Total    *string `json:"total_balance"`
	Granted  *string `json:"granted_balance"`
	ToppedUp *string `json:"topped_up_balance"`
}
type accountBalance struct {
	Available *bool         `json:"is_available"`
	Balances  []balanceInfo `json:"balance_infos"`
}

// FetchQuota is a fresh, read-only request for exactly the selected credential.
// It never retries with a different key or modifies CPA cooldown state.
func (m *Manager) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	key := strings.TrimSpace(req.Attributes["api_key"])
	if req.Provider != ProviderID || !cfg.HasCredential(key) {
		return pluginapi.QuotaFetchResponse{}, authFailure()
	}
	balance, err := m.fetchBalance(ctx, cfg, key)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	return normalizeBalance(balance)
}

func (m *Manager) fetchBalance(parent context.Context, cfg config.Config, key string) (accountBalance, *errclass.Error) {
	var balance accountBalance
	if m.bridge == nil {
		return balance, errclass.FromNetwork(nil)
	}
	ctx, cancel := context.WithTimeout(parent, cfg.QuotaTimeout)
	defer cancel()
	response, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet,
		URL:     catalog.JoinUpstreamURL(cfg.BaseURL, "/user/balance"),
		Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if err != nil {
		return balance, errclass.FromNetwork(err)
	}
	if response.StatusCode != http.StatusOK {
		return balance, selectedStatusError(response.StatusCode, response.Body, key)
	}
	if len(response.Body) > 1<<20 || json.Unmarshal(response.Body, &balance) != nil || balance.Available == nil || balance.Balances == nil || len(balance.Balances) > 16 {
		return accountBalance{}, quotaDataError()
	}
	// Validate the complete response for both quota and credential refresh.
	if _, err := normalizeBalance(balance); err != nil {
		return accountBalance{}, quotaDataError()
	}
	return balance, nil
}

func normalizeBalance(balance accountBalance) (pluginapi.QuotaFetchResponse, error) {
	result := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Pay-as-you-go"}}
	available := 0.0
	if balance.Available == nil {
		return result, quotaDataError()
	}
	if *balance.Available {
		available = 1
	}
	result.Summary = []pluginapi.QuotaMetric{{Key: "api_available", Label: "API available (1=yes, 0=no)", Value: available, Format: "number"}}
	seen := map[string]bool{}
	for _, b := range balance.Balances {
		if (b.Currency != "CNY" && b.Currency != "USD") || seen[b.Currency] {
			return pluginapi.QuotaFetchResponse{}, quotaDataError()
		}
		seen[b.Currency] = true
		for _, field := range []struct {
			key, label string
			raw        *string
		}{
			{"total_balance", "Available balance", b.Total}, {"granted_balance", "Granted balance", b.Granted}, {"topped_up_balance", "Topped-up balance", b.ToppedUp},
		} {
			value, ok := decimalMoney(field.raw)
			if !ok {
				return pluginapi.QuotaFetchResponse{}, quotaDataError()
			}
			result.Summary = append(result.Summary, pluginapi.QuotaMetric{Key: strings.ToLower(b.Currency) + "_" + field.key, Label: field.label + " (" + b.Currency + ")", Value: value, Format: "currency", Currency: b.Currency})
		}
	}
	return result, nil
}

var moneyPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

func decimalMoney(raw *string) (float64, bool) {
	if raw == nil || len(*raw) > 64 || !moneyPattern.MatchString(*raw) {
		return 0, false
	}
	rational, ok := new(big.Rat).SetString(*raw)
	if !ok {
		return 0, false
	}
	value, _ := rational.Float64() // CPA's wire schema requires a float64.
	return value, !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 1e15
}
func quotaDataError() *errclass.Error {
	return &errclass.Error{Class: errclass.ClassUpstream, StatusCode: 502, Message: "invalid DeepSeek balance response"}
}
