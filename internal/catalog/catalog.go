// Package catalog discovers models from the official DeepSeek API.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/modelmeta"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
	// Gemini is an input/output format only; DeepSeek has no Gemini endpoint.
	RouteGemini Route = "gemini"
)

type Target struct{ Protocol Route }

func (t Target) URL(base, _ string, _ bool) string {
	path := "/chat/completions"
	switch t.Protocol {
	case RouteMessages:
		path = "/anthropic/v1/messages"
	case RouteResponses:
		path = "/responses"
	}
	return JoinUpstreamURL(base, path)
}

func JoinUpstreamURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

type ModelRecord struct {
	PublicID, UpstreamID, DisplayName, Description string
	ContextLimit, OutputLimit                      int64
	InputModes, OutputModes                        []string
	Thinking                                       *pluginapi.ThinkingSupport
	UserDefined                                    bool
}

type HostClient interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type rawModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Context     *int64   `json:"context_window"`
	Output      *int64   `json:"max_output_tokens"`
	InputModes  []string `json:"input_modalities"`
	OutputModes []string `json:"output_modalities"`
	Effort      *struct {
		Levels []string `json:"supported_levels"`
	} `json:"effort"`
}

type Manager struct {
	cfg       config.Config
	client    HostClient
	refreshMu sync.Mutex
	mu        sync.RWMutex
	raw       []rawModel
	models    []ModelRecord
	index     map[string]ModelRecord
}

func New(cfg config.Config, client HostClient) *Manager {
	m := &Manager{cfg: cfg, client: client}
	m.swap(nil)
	return m
}

func (m *Manager) SeedFrom(previous *Manager) {
	if previous == nil {
		return
	}
	previous.mu.RLock()
	raw := slices.Clone(previous.raw)
	previous.mu.RUnlock()
	m.swap(raw)
}

// Discovery tries configured keys only. It never changes CPA routing state.
// Bound each attempt so an unresponsive key cannot consume the whole refresh.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if m.client == nil {
		return fmt.Errorf("catalog transport unavailable")
	}
	for _, key := range m.cfg.Credentials {
		attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := m.client.Do(attempt, pluginapi.HTTPRequest{Method: http.MethodGet,
			URL:     JoinUpstreamURL(m.cfg.BaseURL, "/models"),
			Headers: http.Header{"Authorization": {"Bearer " + key.Value}, "Accept": {"application/json"}}})
		cancel()
		if ctx.Err() != nil {
			break
		}
		if err != nil || resp.StatusCode != http.StatusOK || len(resp.Body) > 4<<20 {
			continue
		}
		var wire struct {
			Data []rawModel `json:"data"`
		}
		if json.Unmarshal(resp.Body, &wire) != nil || wire.Data == nil || len(wire.Data) > 4096 {
			continue
		}
		seen := map[string]bool{}
		valid := true
		for _, model := range wire.Data {
			if !validID.MatchString(model.ID) || seen[model.ID] || model.Context != nil && *model.Context < 0 || model.Output != nil && *model.Output < 0 {
				valid = false
				break
			}
			seen[model.ID] = true
		}
		if !valid {
			continue
		}
		m.swap(wire.Data)
		return nil
	}
	if !m.cfg.Catalog.StaleWhileUnavailable {
		m.swap(nil)
	}
	return fmt.Errorf("DeepSeek model discovery unavailable")
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func (m *Manager) swap(raw []rawModel) {
	models := map[string]ModelRecord{}
	for _, r := range raw {
		metadata := modelmeta.Metadata{DisplayName: modelmeta.Ptr(r.Name), Context: r.Context,
			Output: r.Output, InputModalities: r.InputModes, OutputModalities: r.OutputModes}
		if r.Effort != nil {
			metadata.Thinking = &modelmeta.Thinking{Levels: r.Effort.Levels, ZeroAllowed: modelmeta.Ptr(true)}
		}
		models[r.ID] = record(m.cfg, r.ID, metadata, false)
	}
	for _, declaration := range m.cfg.Models {
		r := models[declaration.Name]
		low := modelmeta.Metadata{}
		if r.UpstreamID != "" {
			low = modelmeta.Metadata{DisplayName: &r.DisplayName, Description: &r.Description,
				Context: &r.ContextLimit, Output: &r.OutputLimit, InputModalities: r.InputModes, OutputModalities: r.OutputModes}
			if r.Thinking != nil {
				low.Thinking = &modelmeta.Thinking{Levels: r.Thinking.Levels, ZeroAllowed: &r.Thinking.ZeroAllowed}
			}
		}
		models[declaration.Name] = record(m.cfg, declaration.Name, modelmeta.Merge(low, declaration.Metadata), true)
	}
	list := make([]ModelRecord, 0, len(models))
	index := make(map[string]ModelRecord, len(models)*2)
	for _, r := range models {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b ModelRecord) int { return strings.Compare(a.PublicID, b.PublicID) })
	for _, r := range list {
		index[r.UpstreamID] = r
	}
	// Canonical public IDs take precedence over optional unprefixed aliases.
	for _, r := range list {
		index[r.PublicID] = r
	}
	m.mu.Lock()
	m.raw, m.models, m.index = slices.Clone(raw), list, index
	m.mu.Unlock()
}

func record(cfg config.Config, id string, meta modelmeta.Metadata, custom bool) ModelRecord {
	thinking := meta.Thinking.Support()
	if thinking != nil && !thinking.ZeroAllowed {
		copy := *thinking
		copy.Levels = slices.DeleteFunc(slices.Clone(thinking.Levels), func(level string) bool { return level == "none" })
		thinking = &copy
	}
	if thinking != nil && thinking.ZeroAllowed && !slices.Contains(thinking.Levels, "none") && len(thinking.Levels) > 0 {
		copy := *thinking
		copy.Levels = append([]string{"none"}, thinking.Levels...)
		thinking = &copy
	}
	name := modelmeta.Value(meta.DisplayName)
	if name == "" {
		name = id
	}
	return ModelRecord{PublicID: config.PublicID(cfg, id), UpstreamID: id, DisplayName: name,
		Description: modelmeta.Value(meta.Description), ContextLimit: modelmeta.Value(meta.Context),
		OutputLimit: modelmeta.Value(meta.Output), InputModes: meta.InputModalities, OutputModes: meta.OutputModalities,
		Thinking: thinking, UserDefined: custom}
}

func (m *Manager) Models() []ModelRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.models)
}
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.index[id]
	return r, ok
}
