package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const fixtureConfig = "credentials:\n  - name: A\n    api-key: key-a\n  - name: B\n    api-key: key-b\n"
const fixtureCatalog = `{"data":[{"id":"deepseek-flash","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"effort":{"supported_levels":["low","high","max"]}}]}`
const fixtureBalance = `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.00000001","granted_balance":"10.00000001","topped_up_balance":"100.00"}]}`

func newFixture(t *testing.T, handler func(hostHTTPReq) pluginapi.HTTPResponse) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: func(method string, data []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(struct{}{}), nil
		}
		var r hostHTTPReq
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		if strings.HasSuffix(r.URL, "/models") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fixtureCatalog)}), nil
		}
		if handler != nil {
			return hostOK(handler(r)), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fixtureBalance)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte(fixtureConfig)}))
	if !decodeEnv(t, raw).OK {
		t.Fatalf("registration: %s", raw)
	}
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	return m, f
}

func selectedQuota(key string) pluginapi.QuotaFetchRequest {
	return pluginapi.QuotaFetchRequest{Provider: ProviderID, AuthID: credentialID(key), AuthIndex: "index-" + key, Attributes: map[string]string{"api_key": key}}
}

func TestQuotaUsesSelectedCredentialConcurrently(t *testing.T) {
	m, f := newFixture(t, func(r hostHTTPReq) pluginapi.HTTPResponse {
		if !strings.HasSuffix(r.URL, "/user/balance") || r.Method != http.MethodGet {
			t.Errorf("unexpected balance request %s", r.URL)
		}
		value := "10.01"
		if r.Headers.Get("Authorization") == "Bearer key-b" {
			value = "20.02"
		} else if r.Headers.Get("Authorization") != "Bearer key-a" {
			t.Error("wrong selected key")
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fmt.Sprintf(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":%q,"granted_balance":"0","topped_up_balance":%q}]}`, value, value))}
	})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, want := "key-a", 10.01
			if i%2 == 1 {
				key, want = "key-b", 20.02
			}
			q, err := m.FetchQuota(context.Background(), selectedQuota(key))
			if err != nil || len(q.Summary) != 4 || q.Summary[1].Value != want || q.Summary[1].Currency != "USD" || len(q.Groups) != 0 {
				t.Errorf("selected quota: %+v, %v", q, err)
			}
		}()
	}
	wg.Wait()
	for _, call := range f.recorded() {
		if strings.Contains(call.method, "cooldown") || call.method == pluginabi.MethodHostAuthSave && strings.Contains(string(call.payload), "quota") {
			t.Fatal("quota mutated routing/auth")
		}
	}
}

func TestQuotaCurrencyAndAvailability(t *testing.T) {
	var b accountBalance
	if json.Unmarshal([]byte(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"-0.10","granted_balance":"0","topped_up_balance":"0"},{"currency":"USD","total_balance":"1.123456789","granted_balance":"0.123456789","topped_up_balance":"1"}]}`), &b) != nil {
		t.Fatal("bad fixture")
	}
	q, err := normalizeBalance(b)
	if err != nil || q.Summary[0].Value != 0 || q.Summary[1].Value != -0.1 || q.Summary[4].Value != 1.123456789 || q.Summary[4].Currency != "USD" || len(q.Summary) != 7 || len(q.Groups) != 0 {
		t.Fatalf("balance normalization: %+v, %v", q, err)
	}
	for _, raw := range []string{"NaN", "Inf", "1e3", "1/2", "", " 1.0", "10000000000000000"} {
		if _, ok := decimalMoney(&raw); ok {
			t.Errorf("invalid amount accepted: %q", raw)
		}
	}
}

func TestQuotaErrorsDoNotFallbackOrInventBalances(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `{"error":{"message":"invalid key-a"}}`}, {402, `{"error":{"message":"Insufficient Balance"}}`}, {429, `{"error":{"message":"Rate Limit"}}`}, {503, `{}`},
		{200, `{}`}, {200, `{"is_available":true,"balance_infos":null}`}, {200, `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"NaN"}]}`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			calls := 0
			m, _ := newFixture(t, func(r hostHTTPReq) pluginapi.HTTPResponse {
				calls++
				if r.Headers.Get("Authorization") != "Bearer key-a" {
					t.Error("fallback key used")
				}
				return pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}
			})
			raw, _ := m.HandleCall(pluginabi.MethodQuotaFetch, mustJSON(selectedQuota("key-a")))
			env := decodeEnv(t, raw)
			if env.OK || env.Error == nil || strings.Contains(env.Error.Message, "key-a") || calls != 1 {
				t.Fatalf("quota failed incorrectly: %s, calls %d", raw, calls)
			}
			if tc.status != 200 && env.Error.HTTPStatus != tc.status {
				t.Fatalf("lost status: %s", raw)
			}
		})
	}
	m, _ := newFixture(t, nil)
	if _, err := m.FetchQuota(context.Background(), selectedQuota("removed")); err == nil {
		t.Fatal("removed credential accepted")
	}
	raw, _ := m.HandleCall(pluginabi.MethodQuotaReset, []byte(`{}`))
	if decodeEnv(t, raw).OK {
		t.Fatal("balance reset was advertised")
	}
}

func TestCredentialIdentityReloadAndRemoval(t *testing.T) {
	m, f := newFixture(t, nil)
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 2 {
		t.Fatal("credentials were not materialized independently")
	}
	stored := []byte(`{"type":"deepseek","id":"ignored","api_key":"key-a","label":"old","disabled":true,"proxy_url":"http://localhost:9999","prefix":"local","custom":"keep"}`)
	provider := authProvider{cfg: m.cfg}
	parsed, err := provider.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "existing.json", RawJSON: stored})
	if err != nil || !parsed.Handled || parsed.Auth.ID != credentialID("key-a") || parsed.Auth.Label != "A" || parsed.Auth.Metadata["api_key"] != nil || !parsed.Auth.Disabled {
		t.Fatalf("auth parse: %+v %v", parsed, err)
	}
	settings := "credentials:\n  - name: Renamed B\n    api-key: key-b\n  - name: Renamed A\n    api-key: key-a\n"
	raw, _ := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte(settings)}))
	if !decodeEnv(t, raw).OK || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 2 {
		t.Fatal("reload overwrote persisted auths")
	}
	newParsed, _ := (authProvider{cfg: m.cfg}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, RawJSON: stored})
	if newParsed.Auth.ID != parsed.Auth.ID || newParsed.Auth.Label != "Renamed A" || newParsed.Auth.ProxyURL != parsed.Auth.ProxyURL {
		t.Fatal("rename/reorder changed identity or host fields")
	}
	raw, _ = m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte("credentials: [{name: B, api-key: key-b}]\n")}))
	if !decodeEnv(t, raw).OK {
		t.Fatal("removal reload failed")
	}
	raw, _ = m.HandleCall(pluginabi.MethodModelForAuth, mustJSON(pluginapi.AuthModelRequest{AuthProvider: ProviderID, Attributes: map[string]string{"api_key": "key-a"}}))
	var models pluginapi.ModelResponse
	decodeResult(t, raw, &models)
	if len(models.Models) != 0 {
		t.Fatal("removed credential still publishes models")
	}
	if _, err := m.FetchQuota(context.Background(), selectedQuota("key-a")); err == nil {
		t.Fatal("removed key still fetches quota")
	}
	request := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": "key-a"}, Model: "deepseek/deepseek-flash"}}
	raw, _ = m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(request))
	if decodeEnv(t, raw).OK {
		t.Fatal("removed credential still executes")
	}
}

func TestRefreshValidatesSelectedKeyAndPreservesHostState(t *testing.T) {
	m, _ := newFixture(t, nil)
	req := pluginapi.AuthRefreshRequest{AuthID: credentialID("key-b"), AuthProvider: ProviderID,
		StorageJSON: []byte(`{"disabled":true,"proxy_url":"http://localhost:9090","prefix":"p","label":"stored"}`),
		Metadata:    map[string]any{"disabled": true, "custom": "value"}, Attributes: map[string]string{"api_key": "key-b", "priority": "5"}}
	got, err := m.refreshAuth(context.Background(), req)
	if err != nil || got.Auth.ID != req.AuthID || !got.Auth.Disabled || got.Auth.ProxyURL != "http://localhost:9090" || got.Auth.Prefix != "p" || !reflect.DeepEqual(got.Auth.StorageJSON, req.StorageJSON) || !reflect.DeepEqual(got.Auth.Metadata, req.Metadata) || !reflect.DeepEqual(got.Auth.Attributes, req.Attributes) {
		t.Fatalf("refresh: %+v, %v", got, err)
	}
	got.Auth.Attributes["api_key"] = "changed"
	if req.Attributes["api_key"] != "key-b" {
		t.Fatal("refresh aliases host maps")
	}
	bad, _ := newFixture(t, func(hostHTTPReq) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"is_available":false,"balance_infos":[]}`)}
	})
	if _, err := bad.refreshAuth(context.Background(), req); err == nil || err.StatusCode != 402 {
		t.Fatal("insufficient balance was reported as a recovered credential")
	}
}
