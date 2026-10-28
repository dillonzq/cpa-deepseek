package plugin

import (
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"strings"
	"sync"
	"testing"
)

const testKey = "sk-test-secret-1"

type capturedCall struct {
	method  string
	payload []byte
}

type fakeCaller struct {
	mu                    sync.Mutex
	calls                 []capturedCall
	responder             func(method string, payload []byte) ([]byte, error)
	authFiles             map[string]string
	operationSeq          int
	passthroughOperations bool
}

func (f *fakeCaller) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, capturedCall{method: method, payload: payload})
	if method == pluginabi.MethodHostHTTPOperationOpen && !f.passthroughOperations {
		f.operationSeq++
		id := fmt.Sprint(f.operationSeq)
		f.mu.Unlock()
		return hostOK(httpOperationRequest{OperationID: id}), nil
	}
	f.mu.Unlock()
	var raw []byte
	var err error
	if f.responder != nil {
		raw, err = f.responder(method, payload)
	} else {
		raw = hostOK(map[string]any{})
	}
	if method == pluginabi.MethodHostAuthList && err == nil && !hasExplicitAuthList(raw) {
		return f.authListResponse(), nil
	}
	if method == pluginabi.MethodHostAuthSave && err == nil && hostEnvelopeOK(raw) {
		var req pluginapi.HostAuthSaveRequest
		if json.Unmarshal(payload, &req) == nil && strings.TrimSpace(req.Name) != "" {
			f.mu.Lock()
			if f.authFiles == nil {
				f.authFiles = make(map[string]string)
			}
			var record struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.JSON, &record)
			f.authFiles[req.Name] = record.ID
			f.mu.Unlock()
		}
	}
	return raw, err
}

func hostEnvelopeOK(raw []byte) bool {
	var env pluginabi.Envelope
	return json.Unmarshal(raw, &env) == nil && env.OK
}

func hasExplicitAuthList(raw []byte) bool {
	var env pluginabi.Envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return true
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(env.Result, &result) != nil {
		return true
	}
	_, ok := result["files"]
	return ok
}

func (f *fakeCaller) authListResponse() []byte {
	f.mu.Lock()
	files := make([]pluginapi.HostAuthFileEntry, 0, len(f.authFiles))
	for name, id := range f.authFiles {
		files = append(files, pluginapi.HostAuthFileEntry{
			ID: id, Name: name, Source: "file", Path: name,
		})
	}
	f.mu.Unlock()
	return hostOK(hostAuthListResponse{Files: files})
}

func (f *fakeCaller) recorded() []capturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callsOf(method string) []capturedCall {
	var out []capturedCall
	for _, c := range f.recorded() {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func hostErr(code, msg string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: msg}})
	return out
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func decodeEnv(t *testing.T, data []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if json.Unmarshal(data, &env) != nil {
		t.Fatal("invalid envelope")
	}
	return env
}
func decodeResult(t *testing.T, data []byte, dst any) {
	t.Helper()
	env := decodeEnv(t, data)
	if !env.OK {
		t.Fatalf("failed RPC: %s", data)
	}
	if err := json.Unmarshal(env.Result, dst); err != nil {
		t.Fatal(err)
	}
}
