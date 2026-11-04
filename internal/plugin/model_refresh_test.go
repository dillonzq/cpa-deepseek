package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/catalog"
	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestStartupDiscoveryFailureAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		ok          bool
	}{
		{"no-models", "", false}, {"configured-model", "models: [{name: manual}]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodHostHTTPDo {
					return hostOK(pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("secret-key")}), nil
				}
				return hostOK(struct{}{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			defer m.HandleCall(pluginabi.MethodPluginShutdown, nil)
			raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte("api-keys: [{value: secret-key}]\n" + tc.extra)}))
			env := decodeEnv(t, raw)
			if env.OK != tc.ok {
				t.Fatalf("unexpected registration result: %s", raw)
			}
			if !tc.ok && (env.Error.Code != "model_discovery_failed" || !strings.Contains(env.Error.Message, "HTTP 401")) {
				t.Fatalf("missing diagnostic: %s", raw)
			}
			if strings.Contains(string(raw), "secret-key") {
				t.Fatal("key leaked")
			}
			if !tc.ok && len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatal("failed discovery created credentials")
			}
		})
	}
}

func TestRefreshReportsLocalChangesWithoutRewritingAuth(t *testing.T) {
	cfg, _ := config.Load([]byte("api-keys: [{value: secret-key}]\ncatalog: {stale-while-unavailable: false}\n"))
	body := fixtureCatalog
	offline := false
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			if offline {
				return hostOK(pluginapi.HTTPResponse{StatusCode: 503}), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
		case pluginabi.MethodHostLog, pluginabi.MethodHostHTTPCancel:
			return hostOK(struct{}{}), nil
		default:
			t.Errorf("unexpected host callback: %s", method)
			return hostErr("unsupported", "unexpected callback"), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))
	catalogue := catalog.New(cfg, m.bridge)
	if err := catalogue.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.refreshCatalog(context.Background(), catalogue)
	if len(f.callsOf(pluginabi.MethodHostLog)) != 0 {
		t.Fatal("unchanged catalog produced a warning")
	}
	body = `{"data":[{"id":"new-model"}]}`
	m.refreshCatalog(context.Background(), catalogue)
	if models := catalogue.Models(); len(models) != 1 || models[0].UpstreamID != "new-model" {
		t.Fatal("local catalog not refreshed")
	}
	logs := f.callsOf(pluginabi.MethodHostLog)
	if len(logs) != 1 || !strings.Contains(string(logs[0].payload), "reload the plugin") {
		t.Fatal("missing reload warning")
	}
	m.refreshCatalog(context.Background(), catalogue)
	if len(f.callsOf(pluginabi.MethodHostLog)) != 1 {
		t.Fatal("unchanged refresh repeated the warning")
	}
	offline = true
	m.refreshCatalog(context.Background(), catalogue)
	if len(catalogue.Models()) != 0 {
		t.Fatal("stale models were retained despite configuration")
	}
	logs = f.callsOf(pluginabi.MethodHostLog)
	if len(logs) != 3 || !strings.Contains(string(logs[1].payload), "HTTP 503") || !strings.Contains(string(logs[2].payload), "reload the plugin") {
		t.Fatal("missing failure or model-removal diagnostic")
	}
	for _, call := range logs {
		if strings.Contains(string(call.payload), "secret-key") {
			t.Fatal("diagnostic leaked credential")
		}
	}
}

func TestFailedReconfigurePreservesRunningCatalog(t *testing.T) {
	m, f := newFixture(t, nil)
	original := m.mgr
	f.responder = func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 503}), nil
		}
		return hostOK(struct{}{}), nil
	}
	raw, _ := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte(fixtureConfig + "catalog: {stale-while-unavailable: false}\n")}))
	if decodeEnv(t, raw).OK || m.mgr != original {
		t.Fatal("failed reload replaced running catalog")
	}
	raw, _ = m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte(fixtureConfig)}))
	if !decodeEnv(t, raw).OK || len(m.mgr.Models()) != 1 {
		t.Fatal("stale fallback did not preserve catalog")
	}
}

func TestDiscoveryDeadlineStillAllowsConfiguredModelStartup(t *testing.T) {
	previousTimeout := initialDiscoveryTimeout
	initialDiscoveryTimeout = 40 * time.Millisecond
	defer func() { initialDiscoveryTimeout = previousTimeout }()
	release := make(chan struct{})
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			<-release
			return hostOK(pluginapi.HTTPResponse{StatusCode: 503}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	defer m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	defer close(release)
	raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{
		ConfigYAML: []byte("api-keys: [{value: secret-key}]\nmodels: [{name: manual}]\n"),
	}))
	if !decodeEnv(t, raw).OK {
		t.Fatalf("discovery deadline prevented configured model startup: %s", raw)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 1 {
		t.Fatal("credential was not initialized after discovery deadline")
	}
	raw, _ = m.HandleCall(pluginabi.MethodModelStatic, nil)
	var response pluginapi.ModelResponse
	decodeResult(t, raw, &response)
	if len(response.Models) != 1 || response.Models[0].ID != "deepseek/manual" {
		t.Fatal("configured model unavailable after discovery deadline")
	}
}
