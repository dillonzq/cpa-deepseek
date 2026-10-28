//go:build debug && cgo && (darwin || linux)

package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

// Load the real shared library through public host discovery and exercise the
// actual v8 management handlers and core credential scheduler end to end.
func TestDeepSeekNativeQuotaAndCooldown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var funds atomic.Bool
	var limited atomic.Bool
	var mu sync.Mutex
	var executed []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/models" {
			if r.Header.Get("Authorization") == "" {
				t.Error("catalog did not use a configured credential")
			}
			io.WriteString(w, `{"data":[{"id":"deepseek-flash","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"effort":{"supported_levels":["low","high","max"]}}]}`)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != "offline-a" && key != "offline-b" {
			http.Error(w, "wrong selected key", 401)
			return
		}
		if r.URL.Path == "/user/balance" {
			balance, available := "0.00", false
			if key == "offline-b" {
				balance, available = "20.00", true
			}
			if key == "offline-a" && funds.Load() {
				balance, available = "10.00", true
			}
			fmt.Fprintf(w, `{"is_available":%t,"balance_infos":[{"currency":"CNY","total_balance":%q,"granted_balance":"0","topped_up_balance":%q}]}`, available, balance, balance)
			return
		}
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		executed = append(executed, key)
		mu.Unlock()
		if key == "offline-a" && !funds.Load() {
			w.WriteHeader(http.StatusPaymentRequired)
			io.WriteString(w, `{"error":{"message":"Insufficient account funds"}}`)
			return
		}
		if key == "offline-a" && limited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Rate Limit Reached"}}`)
			return
		}
		io.WriteString(w, `{"id":"native-payg","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	pluginDir := t.TempDir()
	binary, err := os.ReadFile(os.Getenv("CPA_NATIVE_PLUGIN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "cpa-deepseek"+pluginhost.PluginExtension(runtime.GOOS)), binary, 0600); err != nil {
		t.Fatal(err)
	}
	var item config.PluginInstanceConfig
	settings := fmt.Sprintf(`enabled: true
credentials:
  - name: A
    api-key: offline-a
  - name: B
    api-key: offline-b
base-url: %s
allow-http: true
`, upstream.URL)
	if err := yaml.Unmarshal([]byte(settings), &item); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: pluginDir, Configs: map[string]config.PluginInstanceConfig{"cpa-deepseek": item}}}
	host := pluginhost.New()
	host.ApplyConfig(ctx, cfg)
	defer host.ShutdownAll()
	if !host.PluginLoaded("cpa-deepseek") {
		t.Fatal("native PAYG plugin was not loaded")
	}
	providers := host.QuotaProviders(ctx)
	if len(providers) != 1 || providers[0].Provider != "deepseek" || providers[0].SupportsReset {
		t.Fatalf("quota provider registration: %+v", providers)
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 2)
	host.SetAuthManager(manager)
	host.RegisterModels(ctx, registry.GetGlobalRegistry())
	host.RegisterExecutors(manager, registry.GetGlobalRegistry())
	auths := map[string]*coreauth.Auth{}
	files, err := os.ReadDir(cfg.AuthDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		path := filepath.Join(cfg.AuthDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Label string `json:"label"`
		}
		_ = json.Unmarshal(raw, &record)
		name := record.Label
		auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "deepseek", Path: path, FileName: entry.Name(), RawJSON: raw})
		if err != nil || !handled {
			t.Fatalf("parse %s: %v", name, err)
		}
		if name == "A" {
			auth.Attributes["priority"] = "10"
		}
		auth, err = manager.Register(ctx, auth)
		if err != nil {
			t.Fatal(err)
		}
		auths[name] = auth
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "deepseek", []*registry.ModelInfo{{ID: "deepseek/deepseek-flash", Object: "model", OwnedBy: "deepseek"}})
		defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	}
	if auths["A"].ID == auths["B"].ID || auths["A"].Index == auths["B"].Index {
		t.Fatal("credentials share identity")
	}
	payload := []byte(`{"model":"deepseek/deepseek-flash","messages":[{"role":"user","content":"offline quota test"}]}`)
	execute := func() {
		t.Helper()
		got, err := manager.Execute(ctx, []string{"deepseek"}, coreexecutor.Request{Model: "deepseek/deepseek-flash", Payload: payload}, coreexecutor.Options{SourceFormat: translator.FormatOpenAI, OriginalRequest: payload})
		if err != nil || !strings.Contains(string(got.Payload), "ok") {
			t.Fatalf("native execute: %s %v", got.Payload, err)
		}
	}
	execute()
	mu.Lock()
	seen := append([]string(nil), executed...)
	mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"offline-a", "offline-b"}) {
		t.Fatalf("402 did not switch credential: %v", seen)
	}
	isCooled := func(a *coreauth.Auth) bool {
		if a.Unavailable || a.Quota.Exceeded || !a.NextRetryAfter.IsZero() {
			return true
		}
		for _, s := range a.ModelStates {
			if s != nil && (s.Unavailable || s.Quota.Exceeded || !s.NextRetryAfter.IsZero()) {
				return true
			}
		}
		return false
	}
	aBefore, _ := manager.GetByID(auths["A"].ID)
	if !isCooled(aBefore) {
		t.Fatal("402 did not enter CPA cooldown")
	}
	manager.MarkResult(ctx, coreauth.Result{AuthID: auths["B"].ID, Provider: "deepseek", Model: "deepseek/deepseek-flash", Success: false, Error: &coreauth.Error{HTTPStatus: 429, Message: "rate limited"}})
	bBefore, _ := manager.GetByID(auths["B"].ID)
	if !isCooled(bBefore) {
		t.Fatal("429 did not enter independent cooldown")
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.SetPluginHost(host)
	router := gin.New()
	router.POST("/v8/management/credentials/quota/fetch", h.FetchCredentialQuota)
	router.POST("/v8/management/routing/cooldown/reset", h.ResetQuota)
	router.GET("/v8/management/plugins/:id/quota", h.GetPluginQuota)
	router.POST("/v8/management/credentials/refresh", h.RefreshAuthFiles)
	post := func(path string, auth *coreauth.Auth) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(fmt.Sprintf(`{"auth_index":%q}`, auth.Index))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec
	}
	for _, name := range []string{"A", "B"} {
		rec := post("/v8/management/credentials/quota/fetch", auths[name])
		var quota pluginapi.QuotaFetchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &quota); err != nil {
			t.Fatal(err)
		}
		want := 0.0
		if name == "B" {
			want = 20
		}
		if len(quota.Groups) != 0 || len(quota.Summary) != 4 || quota.Summary[1].Value != want || quota.Summary[1].Currency != "CNY" {
			t.Fatalf("wrong selected quota: %+v", quota)
		}
	}
	aAfter, _ := manager.GetByID(auths["A"].ID)
	bAfter, _ := manager.GetByID(auths["B"].ID)
	if !reflect.DeepEqual(aBefore.ModelStates, aAfter.ModelStates) || !reflect.DeepEqual(bBefore.ModelStates, bAfter.ModelStates) {
		t.Fatal("quota refresh mutated cooldown")
	}
	post("/v8/management/routing/cooldown/reset", auths["A"])
	aAfter, _ = manager.GetByID(auths["A"].ID)
	bAfter, _ = manager.GetByID(auths["B"].ID)
	if isCooled(aAfter) || !reflect.DeepEqual(bBefore.ModelStates, bAfter.ModelStates) {
		t.Fatal("cooldown reset affected wrong credential")
	}
	funds.Store(true)
	execute()
	mu.Lock()
	last := executed[len(executed)-1]
	mu.Unlock()
	if last != "offline-a" {
		t.Fatal("reset credential was not restored to routing")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v8/management/plugins/cpa-deepseek/quota?auth_index="+auths["A"].Index, nil).WithContext(ctx))
	var refreshed pluginapi.QuotaFetchResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &refreshed) != nil || len(refreshed.Summary) != 4 || refreshed.Summary[1].Value != 10 {
		t.Fatalf("quota did not refresh after top-up: %s", rec.Body.String())
	}
	// A real upstream 429 traverses the native ABI and CPA scheduler.
	post("/v8/management/routing/cooldown/reset", auths["B"])
	limited.Store(true)
	execute()
	mu.Lock()
	lastTwo := append([]string(nil), executed[len(executed)-2:]...)
	mu.Unlock()
	if !reflect.DeepEqual(lastTwo, []string{"offline-a", "offline-b"}) {
		t.Fatalf("429 did not fail over: %v", lastTwo)
	}
	aBefore, _ = manager.GetByID(auths["A"].ID)
	post("/v8/management/credentials/quota/fetch", auths["A"])
	aAfter, _ = manager.GetByID(auths["A"].ID)
	if !isCooled(aAfter) || !reflect.DeepEqual(aBefore.ModelStates, aAfter.ModelStates) {
		t.Fatal("available balance incorrectly cleared a rate-limit cooldown")
	}
	// Exercise the real v8 credential-refresh handler for a static key.
	refreshRequest := httptest.NewRequest("POST", "/v8/management/credentials/refresh", strings.NewReader(fmt.Sprintf(`{"name":%q,"auth_index":%q}`, auths["B"].FileName, auths["B"].Index))).WithContext(ctx)
	refreshRequest.Header.Set("Content-Type", "application/json")
	refreshRecorder := httptest.NewRecorder()
	router.ServeHTTP(refreshRecorder, refreshRequest)
	if refreshRecorder.Code != 200 {
		t.Fatalf("static key refresh: %d %s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	afterRefresh, _ := manager.GetByID(auths["B"].ID)
	if afterRefresh.ID != auths["B"].ID || afterRefresh.Index != auths["B"].Index || afterRefresh.Attributes["api_key"] != "offline-b" {
		t.Fatal("refresh changed credential identity or key")
	}
}
