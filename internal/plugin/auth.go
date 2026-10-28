package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authProvider struct{ cfg config.Config }

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

type authRecord struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	APIKey   string `json:"api_key"`
	Label    string `json:"label"`
	Disabled bool   `json:"disabled"`
	ProxyURL string `json:"proxy_url"`
	Prefix   string `json:"prefix"`
}

func (p authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var record authRecord
	if json.Unmarshal(req.RawJSON, &record) != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("invalid DeepSeek auth JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if req.Provider != "" && req.Provider != ProviderID || record.Type != ProviderID {
		return pluginapi.AuthParseResponse{}, nil
	}
	key := strings.TrimSpace(record.APIKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("DeepSeek auth has no valid api_key")
	}
	// Stable identity is derived from the key, never from names or array positions.
	id := credentialID(key)
	var metadata map[string]any
	_ = json.Unmarshal(req.RawJSON, &metadata)
	delete(metadata, "api_key")
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: ProviderID, ID: id, FileName: req.FileName, Label: accountLabel(p.cfg, key, record.Label),
		StorageJSON: req.RawJSON, Disabled: record.Disabled, ProxyURL: record.ProxyURL, Prefix: record.Prefix,
		Metadata: metadata, Attributes: map[string]string{"api_key": key},
	}}, nil
}

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("configure a DeepSeek API key")
}
func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("DeepSeek has no OAuth login flow")
}

// Static keys have no token-renewal flow. RPC refresh validates the key via
// the balance endpoint first, then uses this method to preserve host state.
func (p authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	key := strings.TrimSpace(req.Attributes["api_key"])
	if req.AuthProvider != ProviderID || key == "" || !p.cfg.HasCredential(key) {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("selected DeepSeek credential is unavailable")
	}
	var stored authRecord
	if len(req.StorageJSON) > 0 && json.Unmarshal(req.StorageJSON, &stored) != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("invalid DeepSeek auth storage")
	}
	disabled := stored.Disabled
	if current, ok := req.Metadata["disabled"].(bool); ok {
		disabled = current
	}
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: ProviderID, ID: req.AuthID,
		Label: accountLabel(p.cfg, key, stored.Label), Disabled: disabled, ProxyURL: stored.ProxyURL, Prefix: stored.Prefix,
		StorageJSON: req.StorageJSON, Metadata: maps.Clone(req.Metadata), Attributes: maps.Clone(req.Attributes)}}, nil
}

func (m *Manager) refreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, *errclass.Error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	key := strings.TrimSpace(req.Attributes["api_key"])
	if req.AuthProvider != ProviderID || !cfg.HasCredential(key) {
		return pluginapi.AuthRefreshResponse{}, authFailure()
	}
	balance, err := m.fetchBalance(ctx, cfg, key)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	if !*balance.Available {
		return pluginapi.AuthRefreshResponse{}, errclass.FromStatus(402, "DeepSeek account balance is insufficient")
	}
	response, e := (authProvider{cfg: cfg}).RefreshAuth(ctx, req)
	if e != nil {
		return pluginapi.AuthRefreshResponse{}, errclass.Translation(e.Error())
	}
	return response, nil
}

func credentialID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "deepseek-key-" + hex.EncodeToString(digest[:])
}
func accountLabel(cfg config.Config, key, stored string) string {
	for _, credential := range cfg.Credentials {
		if credential.Value == key && credential.Name != "" {
			return credential.Name
		}
	}
	if stored != "" {
		return stored
	}
	return "DeepSeek " + strings.TrimPrefix(credentialID(key), "deepseek-key-")[:8]
}
func authFailure() *errclass.Error {
	return errclass.FromStatus(401, "selected DeepSeek credential is unavailable")
}
