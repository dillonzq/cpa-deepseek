//go:build debug && cgo && (darwin || linux)

package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

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
	_, err = registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, []byte(fmt.Sprintf("credentials:\n  - api-key: offline-test-key\nbase-url: %s\nallow-http: true\nrequest-timeout: 30s\n", server.URL)))
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
