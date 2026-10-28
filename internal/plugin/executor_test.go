package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func execution(format, body string) executorRequest {
	return executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: ProviderID, AuthID: credentialID("key-b"),
		AuthAttributes: map[string]string{"api_key": "key-b"}, Model: "deepseek/deepseek-flash", SourceFormat: format, Format: format, Payload: []byte(body)}, HostCallbackID: "request-scope"}
}

func TestNativeExecutionPreservesOfficialFields(t *testing.T) {
	for _, tc := range []struct{ format, path, body, response string }{
		{"openai", "/chat/completions", `{"messages":[{"role":"assistant","content":"","reasoning_content":"reason","tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"ok"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"thinking":{"type":"enabled"},"reasoning_effort":"max","extension":{"nested":true}}`, `{"id":"chat","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"reason"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"prompt_cache_hit_tokens":1}}`},
		{"openai-response", "/responses", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"reason"}]},{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"custom","name":"apply_patch"},{"type":"web_search"}],"reasoning":{"effort":"max"},"extension":true}`, `{"id":"resp","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`},
		{"claude", "/anthropic/v1/messages", `{"system":"hello","messages":[{"role":"user","content":"hi"}],"max_tokens":200,"thinking":{"type":"enabled","budget_tokens":128},"output_config":{"effort":"max"},"extension":true}`, `{"id":"msg","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			calls := 0
			m, _ := newFixture(t, func(r hostHTTPReq) pluginapi.HTTPResponse {
				calls++
				if !strings.HasSuffix(r.URL, tc.path) || r.HostCallbackID != "request-scope" {
					t.Errorf("route/scope lost: %+v", r)
				}
				key := strings.TrimPrefix(r.Headers.Get("Authorization"), "Bearer ")
				if tc.format == "claude" {
					key = r.Headers.Get("x-api-key")
				}
				if key != "key-b" || r.Headers.Get("Content-Type") != "application/json" {
					t.Error("selected auth headers wrong")
				}
				var sent, original map[string]json.RawMessage
				_ = json.Unmarshal(r.Body, &sent)
				_ = json.Unmarshal([]byte(tc.body), &original)
				delete(sent, "model")
				delete(sent, "stream")
				var actual, expected any
				_ = json.Unmarshal(mustJSON(sent), &actual)
				_ = json.Unmarshal(mustJSON(original), &expected)
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf("native fields changed: %s", r.Body)
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("\n\n" + tc.response)}
			})
			raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(execution(tc.format, tc.body)))
			var response pluginapi.ExecutorResponse
			decodeResult(t, raw, &response)
			if calls != 1 || !strings.Contains(string(response.Payload), `"usage"`) || !strings.Contains(string(response.Payload), "ok") {
				t.Fatalf("native response: %s", response.Payload)
			}
		})
	}
}

func TestThinkingSuffixOverridesAndStatelessResponses(t *testing.T) {
	for _, format := range []string{"openai", "claude", "openai-response"} {
		for _, level := range []string{"none", "low", "high", "max", "ultra"} {
			t.Run(format+"/"+level, func(t *testing.T) {
				m, _ := newFixture(t, nil)
				req := execution(format, `{"messages":[],"input":"hello","thinking":{"type":"enabled"},"reasoning_effort":"high","reasoning":{"effort":"high"},"output_config":{"effort":"high"}}`)
				req.Model += "(" + level + ")"
				res, fail := m.resolveExecution(req)
				if res == nil {
					t.Fatalf("resolution: %s", fail)
				}
				body, e := buildUpstreamBody(res, req)
				if e != nil {
					t.Fatal(e)
				}
				var doc struct {
					Model     string
					Thinking  struct{ Type string }
					Effort    string `json:"reasoning_effort"`
					Reasoning struct{ Effort string }
					Output    struct{ Effort string } `json:"output_config"`
				}
				_ = json.Unmarshal(body, &doc)
				if doc.Model != "deepseek-flash" {
					t.Fatal("prefix/suffix reached upstream")
				}
				want := level
				if level == "ultra" {
					want = "max"
				}
				effort := doc.Effort
				if format == "openai-response" {
					effort = doc.Reasoning.Effort
				} else if format == "claude" {
					effort = doc.Output.Effort
					if doc.Thinking.Type == "disabled" {
						effort = "none"
					}
				}
				if effort != want {
					t.Fatalf("wrong control: %s", body)
				}
			})
		}
	}
	m, f := newFixture(t, nil)
	before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	for _, body := range []string{`{"input":"next","previous_response_id":"resp_previous"}`, `{"input":"next","conversation":{"id":"conv"}}`} {
		raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(execution("openai-response", body)))
		env := decodeEnv(t, raw)
		if env.OK || env.Error.HTTPStatus != 400 {
			t.Fatalf("stateful continuation accepted: %s", raw)
		}
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != before {
		t.Fatal("invalid continuation reached upstream")
	}
}

func TestNativeEffortPreservesExplicitThinkingToggle(t *testing.T) {
	for _, format := range []string{"openai", "claude"} {
		for _, toggle := range []string{"disabled", "adaptive"} {
			if format == "openai" && toggle == "adaptive" {
				continue
			}
			t.Run(format+"/"+toggle, func(t *testing.T) {
				m, _ := newFixture(t, nil)
				control := `"reasoning_effort":"high"`
				if format == "claude" {
					control = `"output_config":{"effort":"high"}`
				}
				req := execution(format, fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"thinking":{"type":%q,"budget_tokens":128},%s}`, toggle, control))
				res, failure := m.resolveExecution(req)
				if res == nil {
					t.Fatalf("resolution: %s", failure)
				}
				body, err := buildUpstreamBody(res, req)
				if err != nil {
					t.Fatal(err)
				}
				var sent struct {
					Thinking struct {
						Type   string
						Budget int `json:"budget_tokens"`
					}
				}
				if json.Unmarshal(body, &sent) != nil || sent.Thinking.Type != toggle || sent.Thinking.Budget != 128 {
					t.Fatalf("native effort changed the explicit thinking toggle: %s", body)
				}
			})
		}
	}
}

func TestGeminiTranslationUsesOfficialChatEndpoint(t *testing.T) {
	m, _ := newFixture(t, func(r hostHTTPReq) pluginapi.HTTPResponse {
		if !strings.HasSuffix(r.URL, "/chat/completions") || !strings.Contains(string(r.Body), `"messages"`) || !strings.Contains(string(r.Body), "hello") {
			t.Errorf("invalid Gemini conversion: %s %s", r.URL, r.Body)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chat","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)}
	})
	raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(execution("gemini", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)))
	var response pluginapi.ExecutorResponse
	decodeResult(t, raw, &response)
	if !strings.Contains(string(response.Payload), `"candidates"`) || !strings.Contains(string(response.Payload), "hello back") {
		t.Fatalf("Gemini response: %s", response.Payload)
	}
}

func TestExecutionErrorStatusAndCredentialRedaction(t *testing.T) {
	for _, status := range []int{400, 401, 402, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, _ := newFixture(t, func(hostHTTPReq) pluginapi.HTTPResponse {
				return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(`{"error":{"message":"key-b was rejected"}}`)}
			})
			raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(execution("openai", `{"messages":[{"role":"user","content":"hi"}]}`)))
			env := decodeEnv(t, raw)
			if env.OK || env.Error.HTTPStatus != status || strings.Contains(env.Error.Message, "key-b") {
				t.Fatalf("bad status/redaction: %s", raw)
			}
		})
	}
}

func TestNativeStreamingTerminalsAndKeepAlive(t *testing.T) {
	for _, tc := range []struct{ format, frames string }{
		{"openai", ": keep-alive\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"},
		{"openai-response", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"},
		{"claude", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var mu sync.Mutex
			read := false
			closed := make(chan string, 1)
			f := &fakeCaller{responder: func(method string, data []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodHostHTTPDo:
					return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fixtureCatalog)}), nil
				case pluginabi.MethodHostHTTPDoStream:
					var r hostHTTPReq
					_ = json.Unmarshal(data, &r)
					if !strings.Contains(string(r.Body), `"stream":true`) || r.HostCallbackID != "request-scope" {
						t.Error("stream flag/scope lost")
					}
					return hostOK(hostStreamStartResp{StatusCode: 200, StreamID: "up", Headers: http.Header{"Content-Type": {"text/event-stream"}}}), nil
				case pluginabi.MethodHostHTTPStreamRead:
					mu.Lock()
					defer mu.Unlock()
					if read {
						return hostOK(hostStreamReadResp{Done: true}), nil
					}
					read = true
					return hostOK(hostStreamReadResp{Payload: []byte(tc.frames), Done: true}), nil
				case pluginabi.MethodHostStreamClose:
					var req struct{ Error string }
					_ = json.Unmarshal(data, &req)
					closed <- req.Error
				}
				return hostOK(struct{}{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte(fixtureConfig)}))
			if !decodeEnv(t, raw).OK {
				t.Fatalf("registration %s", raw)
			}
			req := execution(tc.format, `{"messages":[{"role":"user","content":"hi"}],"input":"hi"}`)
			req.StreamID = "down"
			raw, _ = m.HandleCall(pluginabi.MethodExecutorExecuteStream, mustJSON(req))
			if !decodeEnv(t, raw).OK {
				t.Fatalf("stream start: %s", raw)
			}
			select {
			case err := <-closed:
				if err != "" {
					t.Fatalf("stream ended with error: %s", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not terminate")
			}
			if len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)) != 1 {
				t.Fatal("upstream not closed exactly once")
			}
			var output strings.Builder
			for _, c := range f.callsOf(pluginabi.MethodHostStreamEmit) {
				var r struct{ Payload []byte }
				_ = json.Unmarshal(c.payload, &r)
				output.Write(r.Payload)
			}
			if !strings.Contains(output.String(), "hello") || strings.Contains(output.String(), "keep-alive") {
				t.Fatalf("bad downstream frames: %s", output.String())
			}
		})
	}
}
