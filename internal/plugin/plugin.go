// Package plugin implements the DeepSeek native plugin and its CPA v8 RPC boundary.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dillonzq/cpa-deepseek/internal/catalog"
	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const ProviderID = "deepseek"
const pluginName = "cpa-deepseek"
const pluginDisplayName = "DeepSeek"

var pluginVersion = "0.0.0-dev"

var initialDiscoveryTimeout = 10 * time.Second

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
		active := m.cfg.HasAPIKey(strings.TrimSpace(req.Attributes["api_key"]))
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
		return ErrEnvelope("unsupported", "configure DeepSeek API keys under api-keys"), nil
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
	}{pluginabi.SchemaVersion, pluginapi.Metadata{Name: pluginDisplayName, Version: pluginVersion, Author: "dillonzq",
		GitHubRepository: "https://github.com/dillonzq/cpa-deepseek",
		Logo:             "https://raw.githubusercontent.com/dillonzq/cpa-deepseek/main/assets/logo.png",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "API keys: name and value. Environment variables expand in value only."},
			{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "DeepSeek API root (default https://api.deepseek.com)."},
			{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model declarations and metadata overrides: name, alias (published model name; the prefix still applies), display-name, max-context-length, max-tokens, input-modalities, output-modalities, thinking."},
			{Name: "model-prefix", Type: pluginapi.ConfigFieldTypeObject, Description: "enabled (default true), value (default deepseek)."},
			{Name: "catalog", Type: pluginapi.ConfigFieldTypeObject, Description: "refresh-interval (15m), stale-while-unavailable (true)."},
			{Name: "quota", Type: pluginapi.ConfigFieldTypeObject, Description: "timeout (15s, maximum 1m). Fetches fresh balance for the selected credential."},
			{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream request timeout: a whole non-streaming request, or opening a stream (5m)."},
			{Name: "stream-idle-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Max silence between upstream stream chunks; each chunk restarts it (5m)."},
			{Name: "max-stream-duration", Type: pluginapi.ConfigFieldTypeString, Description: "Max lifetime of one stream, opening included (1h)."},
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
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	catalogue := catalog.New(cfg, client)
	m.mu.RLock()
	previous := m.mgr
	m.mu.RUnlock()
	if len(cfg.APIKeys) > 0 && cfg.Catalog.StaleWhileUnavailable {
		catalogue.SeedFrom(previous)
	}
	discoveryCtx, cancelDiscovery := context.WithTimeout(m.workContext(), initialDiscoveryTimeout)
	var discoveryErr error
	if len(cfg.APIKeys) > 0 {
		discoveryErr = catalogue.Refresh(discoveryCtx)
	}
	cancelDiscovery()
	if err := discoveryErr; err != nil {
		if len(catalogue.Models()) == 0 {
			return ErrEnvelope("model_discovery_failed", err.Error()), nil
		}
		if m.bridge != nil {
			_ = m.bridge.Log("warn", "DeepSeek model discovery failed; serving configured or last known models", map[string]any{"reason": err.Error()})
		}
	}
	m.logAliasConflicts(nil, catalogue)
	// Discovery may exhaust its deadline while configured or cached models
	// remain usable. Give credential initialization its own bounded context.
	ctx, cancel := context.WithTimeout(m.workContext(), 10*time.Second)
	defer cancel()
	if len(cfg.APIKeys) > 0 {
		if err := m.materializeAuthRecords(ctx, cfg); err != nil {
			return ErrEnvelope("auth_materialization_failed", err.Error()), nil
		}
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
	// Install the new membership first: even a cleanup failure must not let a
	// removed key execute. Files left by a failed cleanup are retried on reload.
	if err := m.cleanupAuthRecords(ctx, cfg); err != nil {
		if m.bridge != nil {
			_ = m.bridge.Log("warn", "DeepSeek credential cleanup incomplete; reload to retry and check the auth directory for *.deepseek-cleanup-* files", map[string]any{"reason": err.Error()})
		}
	}
	return registrationEnvelope(), nil
}

// materializeAuthRecords discovers identities before saving missing keys, while
// reserving filenames independently to preserve existing files and host state.
func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return fmt.Errorf("host bridge unavailable")
	}
	if err := m.bridge.waitAuthWrite(ctx); err != nil {
		return err
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	existingIDs := map[string]bool{}
	reservedNames := map[string]bool{}
	listedPaths := make(map[string]bool, len(entries))
	authDir := ""
	for _, entry := range entries {
		reservedNames[entry.Name], reservedNames[entry.ID] = true, true
		if filepath.IsAbs(entry.Path) {
			// A listing without provider metadata still needs a disk check.
			if entry.Provider != "" || entry.Type != "" {
				listedPaths[filepath.Clean(entry.Path)] = true
			}
			if authDir == "" {
				authDir = filepath.Dir(entry.Path)
			}
		}
		// v8 lists disk files without IDs before the auth manager is ready, and
		// identifies host-saved files by path rather than the record's id.
		// Only a full key digest establishes identity; a filename can also
		// start with deepseek-key-. Never treat an unreadable identity as
		// a missing credential and create a duplicate.
		deepseek := entry.Provider == ProviderID || entry.Type == ProviderID
		unknownProvider := entry.Provider == "" && entry.Type == ""
		if (deepseek || unknownProvider) && isCredentialID(entry.ID) {
			existingIDs[entry.ID] = true
		} else if deepseek {
			key, err := readAuthRecordKey(entry.Path)
			if err != nil {
				return err
			}
			if key == "" {
				return fmt.Errorf("existing DeepSeek auth record is invalid")
			}
			existingIDs[credentialID(key)] = true
		}
	}
	// A live auth manager may not have loaded every file yet. Discover keys
	// from disk as well, before choosing a new name for any missing key.
	if authDir != "" {
		err := fs.WalkDir(os.DirFS(authDir), ".", func(relative string, file fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("cannot read auth directory")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if file.IsDir() || !strings.HasSuffix(strings.ToLower(file.Name()), ".json") {
				return nil
			}
			path := filepath.Join(authDir, filepath.FromSlash(relative))
			if filepath.Dir(path) == authDir {
				reservedNames[file.Name()] = true
			}
			if listedPaths[path] {
				return nil
			}
			// Invalid or unrelated unlisted files still reserve their names.
			// A read failure leaves identity unknown, so do not risk a duplicate.
			key, err := readAuthRecordKey(path)
			if errors.Is(err, errReadAuthRecord) {
				return err
			}
			if err == nil && key != "" {
				existingIDs[credentialID(key)] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, key := range cfg.APIKeys {
		id := credentialID(key.Value)
		if existingIDs[id] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		label := accountLabel(cfg, key.Value, "")
		// AuthSave overwrites, and AuthList can omit files the auth manager did
		// not load (e.g. invalid JSON), so never pick a name already on disk.
		// Without a listed path the directory cannot be checked; fall back to
		// the key-derived name, which only this key's record can occupy.
		name := id + ".json"
		if authDir != "" {
			name = availableAuthName(label, reservedNames)
			for fileExists(filepath.Join(authDir, name)) {
				reservedNames[name] = true
				name = availableAuthName(label, reservedNames)
			}
		} else if availableAuthName(id, reservedNames) != name {
			return fmt.Errorf("existing credential filename has no verifiable identity")
		}
		record, _ := json.Marshal(map[string]any{"type": ProviderID, "id": id, "label": label, "api_key": key.Value})
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
			return err
		}
		existingIDs[id], reservedNames[name] = true, true
	}
	return nil
}

var errReadAuthRecord = errors.New("cannot read existing auth record")

// readAuthRecordKey reads a DeepSeek file's api_key for identity only. Other
// providers return an empty key; file contents and paths never reach errors.
func readAuthRecordKey(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("existing DeepSeek auth record has no absolute path")
	}
	file, err := openAuthRecord(path)
	if err != nil {
		return "", errReadAuthRecord
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errReadAuthRecord
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", errReadAuthRecord
	}
	// A replacement at the same path must not inherit the opened file's key.
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() {
		return "", errReadAuthRecord
	}
	var record struct {
		Type   string `json:"type"`
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return "", fmt.Errorf("existing DeepSeek auth record is invalid")
	}
	if record.Type != ProviderID {
		return "", nil
	}
	key := strings.TrimSpace(record.APIKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("existing DeepSeek auth record has no valid api_key")
	}
	return key, nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// availableAuthName derives a readable, unused credential filename from the
// label: management clients show the filename instead of Label. Identity
// stays the key-derived ID stored inside the record.
func availableAuthName(label string, existing map[string]bool) string {
	base := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, label)
	base = strings.Trim(base, "-")
	if base == "" {
		base = pluginDisplayName
	}
	if runes := []rune(base); len(runes) > 80 {
		base = string(runes[:80])
	}
	// Device basenames stay reserved on Windows even with an extension.
	if isWindowsDeviceName(base) {
		base = pluginDisplayName + "-" + base
	}
	name := base + ".json"
	for n := 2; ; n++ {
		collision := false
		for existingName := range existing {
			if strings.EqualFold(name, existingName) {
				collision = true
				break
			}
		}
		if !collision {
			return name
		}
		name = fmt.Sprintf("%s-%d.json", base, n)
	}
}

func isWindowsDeviceName(base string) bool {
	upper := strings.ToUpper(base)
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(upper) == 4 && (upper[:3] == "COM" || upper[:3] == "LPT") && upper[3] >= '1' && upper[3] <= '9'
}

func (m *Manager) handleModels(active bool) ([]byte, error) {
	m.mu.RLock()
	catalogue := m.mgr
	configured := len(m.cfg.APIKeys) > 0
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if active && catalogue != nil && configured {
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
	if len(cfg.APIKeys) == 0 {
		close(done)
		return
	}
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
				m.refreshCatalog(attempt, catalogue)
				release()
			}
		}
	}()
}

// refreshCatalog updates the local snapshot. The SDK has no callback to
// invalidate the host registry, so changes require a plugin reload.
func (m *Manager) refreshCatalog(ctx context.Context, catalogue *catalog.Manager) {
	previous, previousConflicts := catalogue.Models(), catalogue.Conflicts()
	if err := catalogue.Refresh(ctx); err != nil && m.bridge != nil {
		_ = m.bridge.Log("warn", "DeepSeek model discovery failed", map[string]any{"reason": err.Error()})
	}
	m.logAliasConflicts(previousConflicts, catalogue)
	if m.bridge != nil && !reflect.DeepEqual(previous, catalogue.Models()) {
		_ = m.bridge.Log("warn", "DeepSeek model catalog changed; reload the plugin to update the host model list", nil)
	}
}

// logAliasConflicts reports excluded aliased models when the set changes.
func (m *Manager) logAliasConflicts(previous []string, catalogue *catalog.Manager) {
	conflicts := catalogue.Conflicts()
	if m.bridge == nil || len(conflicts) == 0 || slices.Equal(previous, conflicts) {
		return
	}
	_ = m.bridge.Log("warn", "DeepSeek model aliases excluded", map[string]any{"reason": strings.Join(conflicts, "; ")})
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
