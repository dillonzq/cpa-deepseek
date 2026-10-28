package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type catalogClient func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)

func (f catalogClient) Do(c context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return f(c, r)
}

const discovery = `{"data":[{"id":"deepseek-flash","name":"DeepSeek Flash","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"output_modalities":["text"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"}}]}`

func TestDiscoveryFallbackMetadataAndStale(t *testing.T) {
	cfg, err := config.Load([]byte("credentials: [{api-key: rejected}, {api-key: working}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	fail := false
	m := New(cfg, catalogClient(func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		keys = append(keys, req.Headers.Get("Authorization"))
		if req.URL != "https://api.deepseek.com/models" {
			t.Fatal(req.URL)
		}
		if fail {
			return pluginapi.HTTPResponse{}, fmt.Errorf("offline")
		}
		if req.Headers.Get("Authorization") == "Bearer rejected" {
			return pluginapi.HTTPResponse{StatusCode: 401}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(discovery)}, nil
	}))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "Bearer rejected,Bearer working" {
		t.Fatal("discovery failed to try a working key")
	}
	r, ok := m.Lookup("deepseek/deepseek-flash")
	if !ok || r.ContextLimit != 1048576 || r.OutputLimit != 393216 || len(r.InputModes) != 2 || r.Thinking == nil || !r.Thinking.ZeroAllowed || strings.Join(r.Thinking.Levels, ",") != "none,low,high,max" {
		t.Fatalf("metadata: %+v", r)
	}
	fail = true
	if err := m.Refresh(context.Background()); err == nil || len(m.Models()) != 1 {
		t.Fatal("stale models lost")
	}
	cfg.ModelPrefix.Value = "ds"
	next := New(cfg, nil)
	next.SeedFrom(m)
	if _, ok := next.Lookup("ds/deepseek-flash"); !ok {
		t.Fatal("stale model was not rebuilt with new prefix")
	}
	if _, ok := next.Lookup("deepseek/deepseek-flash"); ok {
		t.Fatal("old prefix survived")
	}
}

func TestFailedRefreshPreservesOverridesAndRejectsMalformedCatalog(t *testing.T) {
	cfg, err := config.Load([]byte("credentials: [{api-key: key}]\ncatalog: {stale-while-unavailable: false}\nmodels: [{name: manual, display-name: Custom}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[{"id":"bad\nid"}]}`, `{"data":[{"id":"duplicate"},{"id":"duplicate"}]}`} {
		m := New(cfg, catalogClient(func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
		}))
		if err := m.Refresh(context.Background()); err == nil {
			t.Fatal("invalid catalog accepted")
		}
		if len(m.Models()) != 1 || m.Models()[0].DisplayName != "Custom" {
			t.Fatal("explicit model lost")
		}
	}
}

func TestPublicModelNamesRemainUnambiguous(t *testing.T) {
	cfg, err := config.Load([]byte("credentials: [{api-key: key}]\nmodels: [{name: model}, {name: deepseek/model}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, nil)
	for _, id := range []string{"model", "deepseek/model"} {
		r, ok := m.Lookup(config.PublicID(cfg, id))
		if !ok || r.UpstreamID != id {
			t.Fatalf("canonical public ID routed to another model: %+v", r)
		}
	}
}

func TestExplicitThinkingOverrideWinsOverDiscovery(t *testing.T) {
	cfg, err := config.Load([]byte("credentials: [{api-key: key}]\nmodels: [{name: deepseek-flash, thinking: {zero-allowed: false}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, catalogClient(func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(discovery)}, nil
	}))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Lookup("deepseek/deepseek-flash")
	if r.Thinking == nil || r.Thinking.ZeroAllowed || slices.Contains(r.Thinking.Levels, "none") {
		t.Fatal("discovery overrode explicit thinking metadata")
	}
}
