//go:build debug && cgo && (darwin || linux)

package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeepSeekNativeAuthIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"deepseek-flash"}]}`)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	existingName := "deepseek-key-Personal.json"
	if err := os.MkdirAll(filepath.Join(dir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	existingPath := filepath.Join(dir, "accounts", existingName)
	original := []byte(`{"type":"deepseek","api_key":"offline-a","disabled":true,"proxy_url":"http://localhost:9999","custom":"keep"}`)
	otherPath := filepath.Join(dir, "Other.json")
	for path, data := range map[string][]byte{existingPath: original, otherPath: []byte(`{"type":"codex"}`)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := New()
	host.runtimeConfig = &config.Config{AuthDir: dir}
	file := pluginFile{ID: "cpa-deepseek", Path: os.Getenv("CPA_NATIVE_PLUGIN")}
	native, err := defaultPluginLoader().Open(file, host)
	if err != nil {
		t.Fatal(err)
	}
	client := newGuardedPluginClient(native)
	defer client.Shutdown()
	settings := func(keys string) []byte {
		return []byte(fmt.Sprintf("api-keys:\n%sbase-url: %s\nallow-http: true\n", keys, upstream.URL))
	}
	apply := func(method, keys string) {
		t.Helper()
		if _, err := registerRPCPlugin(ctx, host, file.ID, client, method, settings(keys)); err != nil {
			t.Fatal(err)
		}
	}
	assertFiles := func(want int) {
		t.Helper()
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != want {
			t.Fatalf("auth files=%d, want %d: %v", len(files), want, err)
		}
		stored, err := os.ReadFile(existingPath)
		if err != nil || !bytes.Equal(stored, original) {
			t.Fatal("materialization changed existing credential state")
		}
	}
	// Startup's root listing omits nested files, which must still be preserved.
	apply(pluginabi.MethodPluginRegister, "  - name: Personal\n    value: offline-a\n")
	assertFiles(2)
	// A live manager can list only some of the physical auth files.
	manager := coreauth.NewManager(nil, nil, nil)
	host.SetAuthManager(manager)
	other, err := host.buildAuthFromFileData(otherPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(ctx, other); err != nil {
		t.Fatal(err)
	}
	apply(pluginabi.MethodPluginReconfigure, "  - name: Renamed A\n    value: offline-a\n  - name: Team\n    value: offline-b\n")
	assertFiles(3)
	teamPath := filepath.Join(dir, "Team.json")
	team, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	// Host-saved IDs are paths, including nested readable filenames.
	existing, err := host.buildAuthFromFileData(existingPath, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(ctx, existing); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		apply(pluginabi.MethodPluginReconfigure, "  - name: Renamed B\n    value: offline-b\n  - name: Renamed A\n    value: offline-a\n")
		assertFiles(3)
		if stored, err := os.ReadFile(teamPath); err != nil || !bytes.Equal(stored, team) {
			t.Fatal("reconfiguration rewrote the new credential")
		}
	}
}

func TestDeepSeekNativeUnload(t *testing.T) {
	started, canceled := make(chan bool, 2), make(chan bool, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			io.WriteString(w, `{"data":[{"id":"deepseek-flash"}]}`)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		started <- req.Stream
		<-r.Context().Done()
		canceled <- req.Stream
	}))
	defer server.Close()
	host := New()
	host.runtimeConfig = &config.Config{AuthDir: t.TempDir()}
	file := pluginFile{ID: "cpa-deepseek", Path: os.Getenv("CPA_NATIVE_PLUGIN")}
	native, err := defaultPluginLoader().Open(file, host)
	if err != nil {
		t.Fatal(err)
	}
	client := newGuardedPluginClient(native)
	defer client.Shutdown()
	_, err = registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, []byte(fmt.Sprintf("api-keys:\n  - value: offline-test-key\nbase-url: %s\nallow-http: true\nrequest-timeout: 30s\n", server.URL)))
	if err != nil {
		t.Fatal(err)
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	req := func(stream bool) pluginapi.ExecutorRequest {
		return pluginapi.ExecutorRequest{Model: "deepseek/deepseek-flash", AuthProvider: "deepseek", AuthAttributes: map[string]string{"api_key": "offline-test-key"}, SourceFormat: "openai", Stream: stream, Payload: []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"pending"}],"stream":%t}`, stream))}
	}
	nonstreamDone := make(chan error, 1)
	go func() { _, err := rpc.Execute(context.Background(), req(false)); nonstreamDone <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("nonstream did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := rpc.ExecuteStream(ctx, req(true))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	unloaded := make(chan struct{})
	go func() {
		host.closeHostHTTPPluginResources(file.ID, pluginCallbackInstance(client))
		shutdownPluginClient(context.Background(), client)
		close(unloaded)
	}()
	select {
	case <-unloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("unload deadlocked with host HTTP closure")
	}
	if <-nonstreamDone == nil {
		t.Fatal("unload let canceled HTTP succeed")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("unload left HTTP active")
		}
	}
	cancel() // host owns downstream cleanup after its callback instance closes
	for range response.Chunks {
	}
	host.httpOperations.mu.Lock()
	operations := len(host.httpOperations.operations)
	host.httpOperations.mu.Unlock()
	host.httpStreams.mu.Lock()
	streams := len(host.httpStreams.streams)
	host.httpStreams.mu.Unlock()
	if operations != 0 || streams != 0 {
		t.Fatal("unload leaked host HTTP resources")
	}
}
