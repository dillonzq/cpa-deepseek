// Package plugin implements the DeepSeek native plugin and its CPA v8 RPC boundary.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/catalog"
	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const ProviderID = "deepseek"
const pluginName = "cpa-deepseek"

var pluginVersion = "0.0.0-dev"

type Manager struct {
	bridge     *HostBridge
	lifeMu     sync.Mutex
	drainMu    sync.Mutex
	workMu     sync.Mutex
	work       sync.WaitGroup
	quiescing  bool
	workCtx    context.Context
	cancelWork context.CancelFunc
	mu         sync.RWMutex
	cfg        config.Config
	mgr        *catalog.Manager
	stop       chan struct{}
	done       chan struct{}
}

func NewManager(bridge *HostBridge) *Manager { return &Manager{bridge: bridge} }

func (m *Manager) HandleCall(method string, data []byte) (response []byte, err error) {
	defer func() {
		if recover() != nil {
			response, err = ErrEnvelope("plugin_error", "internal DeepSeek plugin error"), nil
		}
	}()
	if method != pluginabi.MethodPluginShutdown && method != pluginabi.MethodPluginQuiesce {
		m.workMu.Lock()
		if m.quiescing {
			m.workMu.Unlock()
			return ErrEnvelope("plugin_quiescing", "plugin is stopping"), nil
		}
		m.initWorkContextLocked()
		m.work.Add(1)
		m.workMu.Unlock()
		defer m.work.Done()
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(data)
	case pluginabi.MethodPluginQuiesce:
		return m.handleQuiesce(false)
	case pluginabi.MethodPluginShutdown:
		return m.handleQuiesce(true)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodModelStatic:
		return m.handleModels(true)
	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if json.Unmarshal(data, &req) != nil {
			return ErrEnvelope("invalid_request", "invalid model request"), nil
		}
		m.mu.RLock()
		active := m.cfg.HasCredential(strings.TrimSpace(req.Attributes["api_key"]))
		m.mu.RUnlock()
		return m.handleModels(req.AuthProvider == ProviderID && active)
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(data, &req) != nil {
			return ErrEnvelope("invalid_request", "invalid auth request"), nil
		}
		m.mu.RLock()
		cfg := m.cfg
		m.mu.RUnlock()
		resp, e := (authProvider{cfg: cfg}).ParseAuth(m.workContext(), req)
		if e != nil {
			return ErrEnvelope("auth_failure", e.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthRefresh:
		var req struct {
			pluginapi.AuthRefreshRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if json.Unmarshal(data, &req) != nil {
			return ErrEnvelope("invalid_request", "invalid refresh request"), nil
		}
		resp, e := m.refreshAuth(withHostCallbackScope(m.workContext(), req.HostCallbackID), req.AuthRefreshRequest)
		if e != nil {
			return classEnvelope(e), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return ErrEnvelope("unsupported", "configure DeepSeek API keys under credentials"), nil
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{ProviderID}, DisplayName: "DeepSeek account balance", SupportsReset: false}), nil
	case pluginabi.MethodQuotaFetch:
		var req struct {
			pluginapi.QuotaFetchRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if json.Unmarshal(data, &req) != nil {
			return ErrEnvelope("invalid_request", "invalid quota request"), nil
		}
		resp, e := m.FetchQuota(withHostCallbackScope(m.workContext(), req.HostCallbackID), req.QuotaFetchRequest)
		if e != nil {
			return classEnvelope(classified(e)), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodQuotaReset:
		return ErrEnvelope("unsupported", "DeepSeek balance cannot be reset; use CPA routing/cooldown/reset for local cooldown"), nil
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(data)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(data)
	case pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{Class: errclass.ClassUnsupported, Message: "this operation has no configured DeepSeek endpoint"}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown plugin method"), nil
	}
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "openai-response", "claude", "gemini"}
	return okEnvelope(struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  capabilities       `json:"capabilities"`
	}{pluginabi.SchemaVersion, pluginapi.Metadata{Name: pluginName, Version: pluginVersion, Author: "dillonzq",
		GitHubRepository: "https://github.com/dillonzq/cpa-deepseek",
		Logo:             "https://api-docs.deepseek.com/img/favicon.svg",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "credentials", Type: pluginapi.ConfigFieldTypeArray, Description: "API keys: name and api-key. Environment variables expand in api-key only."},
			{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "DeepSeek API root (default https://api.deepseek.com)."},
			{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model declarations and metadata overrides: name, display-name, max-context-length, max-tokens, input-modalities, output-modalities, thinking."},
			{Name: "model-prefix", Type: pluginapi.ConfigFieldTypeObject, Description: "enabled (default true), value (default deepseek)."},
			{Name: "catalog", Type: pluginapi.ConfigFieldTypeObject, Description: "refresh-interval (15m), stale-while-unavailable (true)."},
			{Name: "quota", Type: pluginapi.ConfigFieldTypeObject, Description: "timeout (15s, maximum 1m). Fetches fresh balance for the selected credential."},
			{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Total upstream request timeout, including streaming (15m)."},
			{Name: "max-response-bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum response/stream size (67108864)."},
			{Name: "allow-http", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow HTTP for local test servers (false)."},
		}}, capabilities{true, true, true, pluginapi.ExecutorModelScopeOAuth, formats, formats, true}})
}

func (m *Manager) handleLifecycle(data []byte) ([]byte, error) {
	var req lifecycleRequest
	if json.Unmarshal(data, &req) != nil {
		return ErrEnvelope("invalid_request", "invalid lifecycle request"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	ctx, cancel := context.WithTimeout(m.workContext(), 10*time.Second)
	defer cancel()
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		return ErrEnvelope("auth_materialization_failed", err.Error()), nil
	}
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	catalogue := catalog.New(cfg, client)
	m.mu.RLock()
	previous := m.mgr
	m.mu.RUnlock()
	if cfg.Catalog.StaleWhileUnavailable {
		catalogue.SeedFrom(previous)
	}
	if err := catalogue.Refresh(ctx); err != nil && m.bridge != nil {
		_ = m.bridge.Log("warn", "DeepSeek model discovery unavailable; serving configured or last known models", nil)
	}
	if done := m.closeStop(); done != nil {
		<-done
	}
	m.mu.Lock()
	m.cfg, m.mgr = cfg, catalogue
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, catalogue, stop, done)
	return registrationEnvelope(), nil
}

func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return fmt.Errorf("host bridge unavailable")
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, entry := range entries {
		existing[entry.Name], existing[entry.ID] = true, true
	}
	for _, key := range cfg.Credentials {
		id := credentialID(key.Value)
		name := id + ".json"
		if existing[id] || existing[name] {
			continue
		}
		record, _ := json.Marshal(map[string]any{"type": ProviderID, "id": id, "label": accountLabel(cfg, key.Value, ""), "api_key": key.Value})
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
			return err
		}
		existing[id], existing[name] = true, true
	}
	return nil
}

func (m *Manager) handleModels(active bool) ([]byte, error) {
	m.mu.RLock()
	catalogue := m.mgr
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if active && catalogue != nil {
		for _, r := range catalogue.Models() {
			models = append(models, pluginapi.ModelInfo{ID: r.PublicID, Object: "model", OwnedBy: ProviderID,
				Name: r.UpstreamID, DisplayName: r.DisplayName, Description: r.Description,
				ContextLength: r.ContextLimit, MaxCompletionTokens: r.OutputLimit,
				SupportedInputModalities: r.InputModes, SupportedOutputModalities: r.OutputModes,
				Thinking: r.Thinking, UserDefined: r.UserDefined})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

func (m *Manager) initWorkContextLocked() {
	if m.workCtx == nil {
		m.workCtx, m.cancelWork = context.WithCancel(context.Background())
	}
}
func (m *Manager) workContext() context.Context {
	m.workMu.Lock()
	defer m.workMu.Unlock()
	m.initWorkContextLocked()
	return m.workCtx
}

// Admission holds a work count until asynchronous stream ownership is transferred.
// Quiesce cancels producers and drains every callback before the host can dlclose.
func (m *Manager) handleQuiesce(shutdown bool) ([]byte, error) {
	m.drainMu.Lock()
	defer m.drainMu.Unlock()
	m.workMu.Lock()
	m.initWorkContextLocked()
	m.quiescing = true
	m.cancelWork()
	m.workMu.Unlock()
	m.work.Wait()
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if done := m.closeStop(); done != nil {
		<-done
	}
	if m.bridge != nil {
		m.bridge.inFlight.Wait()
	}
	if shutdown {
		m.mu.Lock()
		m.cfg, m.mgr = config.Config{}, nil
		m.mu.Unlock()
	}
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

func (m *Manager) startRefreshLoop(cfg config.Config, catalogue *catalog.Manager, stop, done chan struct{}) {
	ctx, cancel := context.WithCancel(m.workContext())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(cfg.Catalog.RefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				attempt, release := context.WithTimeout(ctx, 10*time.Second)
				_ = catalogue.Refresh(attempt)
				release()
			}
		}
	}()
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}
func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
