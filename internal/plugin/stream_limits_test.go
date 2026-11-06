package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
)

const (
	limitChunk  = `data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n"
	limitFinish = `data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
)

// withCatalog serves discovery from fixtureCatalog and everything else from
// next.
func withCatalog(next func(string, []byte) ([]byte, error)) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(fixtureCatalog)}), nil
		}
		return next(method, payload)
	}
}

// tickingUpstream serves one content chunk every interval. After chunks
// reads it finishes the stream; chunks < 0 streams until closed.
func tickingUpstream(interval time.Duration, chunks int) func(string, []byte) ([]byte, error) {
	var reads atomic.Int64
	var closed atomic.Bool
	return withCatalog(func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-tick"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if closed.Load() {
				return hostOK(hostStreamReadResp{Done: true}), nil
			}
			time.Sleep(interval)
			if n := reads.Add(1); chunks >= 0 && n > int64(chunks) {
				return hostOK(hostStreamReadResp{Payload: []byte(limitFinish), Done: true}), nil
			}
			return hostOK(hostStreamReadResp{Payload: []byte(limitChunk)}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			closed.Store(true)
		}
		return hostOK(struct{}{}), nil
	})
}

// runLimitedStream registers with extraYAML, runs one stream to completion,
// and returns the initial envelope and the downstream close error.
func runLimitedStream(t *testing.T, extraYAML string, responder func(string, []byte) ([]byte, error)) (pluginabi.Envelope, string) {
	t.Helper()
	f := &fakeCaller{responder: responder}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte(fixtureConfig + extraYAML)}))
	if !decodeEnv(t, raw).OK {
		t.Fatalf("registration %s", raw)
	}
	req := execution("openai", `{"messages":[{"role":"user","content":"hi"}]}`)
	req.StreamID = "down-limit"
	raw, _ = m.HandleCall(pluginabi.MethodExecutorExecuteStream, mustJSON(req))
	env := decodeEnv(t, raw)
	if !m.bridge.WaitForInFlight(5 * time.Second) {
		t.Fatal("stream did not finish")
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if !env.OK {
		if len(downCloses) != 0 {
			t.Fatalf("failed open must not close a downstream stream: %v", downCloses)
		}
		return env, ""
	}
	if len(downCloses) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(downCloses))
	}
	var closeReq struct{ Error string }
	_ = json.Unmarshal(downCloses[0].payload, &closeReq)
	return env, closeReq.Error
}

func TestExecuteStreamActiveStreamOutlivesRequestTimeout(t *testing.T) {
	// The stream lasts several times request-timeout but never goes quiet
	// for stream-idle-timeout, so it must complete normally.
	_, closeErr := runLimitedStream(t,
		"request-timeout: 30ms\nstream-idle-timeout: 1s\nmax-stream-duration: 10s\n",
		tickingUpstream(10*time.Millisecond, 15))
	if closeErr != "" {
		t.Fatalf("active stream closed with error: %s", closeErr)
	}
}

func TestExecuteStreamIdleTimeoutRestartsOnEachChunk(t *testing.T) {
	// Chunks arrive every 20ms against a 60ms idle limit for ~200ms total.
	_, closeErr := runLimitedStream(t, "stream-idle-timeout: 60ms\n", tickingUpstream(20*time.Millisecond, 10))
	if closeErr != "" {
		t.Fatalf("stream with regular chunks closed with error: %s", closeErr)
	}
}

func TestExecuteStreamIdleTimeoutClosesSilentUpstream(t *testing.T) {
	// The upstream never answers a read until it is closed; the idle limit
	// must close it, unblocking the read, and report the limit downstream.
	release := make(chan struct{})
	var once atomic.Bool
	_, closeErr := runLimitedStream(t, "stream-idle-timeout: 50ms\n", withCatalog(func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-idle"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			<-release
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			if once.CompareAndSwap(false, true) {
				close(release)
			}
		}
		return hostOK(struct{}{}), nil
	}))
	if closeErr != "stream exceeded stream-idle-timeout" {
		t.Fatalf("close error = %q, want idle timeout", closeErr)
	}
}

func TestExecuteStreamMaxDurationEndsActiveStream(t *testing.T) {
	start := time.Now()
	_, closeErr := runLimitedStream(t,
		"stream-idle-timeout: 1s\nmax-stream-duration: 80ms\n",
		tickingUpstream(10*time.Millisecond, -1))
	if closeErr != "stream exceeded max-stream-duration" {
		t.Fatalf("close error = %q, want max-stream-duration", closeErr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("max-stream-duration enforced after %v", elapsed)
	}
}

func TestExecuteStreamOpenTimeout(t *testing.T) {
	saved := streamTimeoutGrace
	streamTimeoutGrace = 0
	t.Cleanup(func() { streamTimeoutGrace = saved })
	env, _ := runLimitedStream(t, "request-timeout: 30ms\nstream-idle-timeout: 10s\n", withCatalog(func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			time.Sleep(150 * time.Millisecond)
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-slow"}), nil
		}
		return hostOK(struct{}{}), nil
	}))
	if env.OK || env.Error == nil {
		t.Fatalf("want error envelope, got %+v", env)
	}
	if env.Error.Code != string(errclass.ClassNetwork) || !env.Error.Retryable ||
		!strings.Contains(env.Error.Message, "stream did not open within request-timeout") {
		t.Fatalf("envelope error = %+v", env.Error)
	}
}

func TestStreamStopMessage(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{errStreamOpenTimeout, "stream did not open within request-timeout"},
		{errStreamIdleTimeout, "stream exceeded stream-idle-timeout"},
		{errMaxStreamDuration, "stream exceeded max-stream-duration"},
		{nil, "stream canceled"},
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tc.cause)
		if got := streamStopMessage(ctx); got != tc.want {
			t.Errorf("cause %v: message = %q, want %q", tc.cause, got, tc.want)
		}
	}
}

func newTestLimits(idle time.Duration) (*streamLimits, context.Context) {
	ctx, cancel := context.WithCancelCause(context.Background())
	return startStreamLimits(cancel, config.Config{
		RequestTimeout:    time.Hour,
		StreamIdleTimeout: idle,
		MaxStreamDuration: time.Hour,
	}), ctx
}

func TestStreamLimitsLateOpenTimerIgnoredAfterOpen(t *testing.T) {
	l, ctx := newTestLimits(time.Hour)
	defer l.stop()
	l.opened()
	l.openExpired() // a callback already running when the stream opened
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("open timer canceled an opened stream: %v", err)
	}
}

func TestStreamLimitsIdleExcludesDownstreamDelivery(t *testing.T) {
	l, ctx := newTestLimits(30 * time.Millisecond)
	defer l.stop()
	l.opened()
	l.readStarted()
	l.readDone(true)
	time.Sleep(80 * time.Millisecond) // delivering the chunk downstream
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("idle limit fired while no read was waiting: %v", err)
	}
	l.readStarted() // the chunk restarts the limit for this wait
	l.idleExpired() // a stale callback must not cancel a fresh wait
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("stale idle callback canceled a fresh wait: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := context.Cause(ctx); !errors.Is(err, errStreamIdleTimeout) {
		t.Fatalf("cause = %v, want idle timeout", err)
	}
}

func TestStreamLimitsEmptyReadKeepsIdleDeadline(t *testing.T) {
	l, ctx := newTestLimits(60 * time.Millisecond)
	defer l.stop()
	l.opened()
	deadline := time.Now().Add(time.Second)
	for context.Cause(ctx) == nil && time.Now().Before(deadline) {
		l.readStarted()
		time.Sleep(10 * time.Millisecond)
		l.readDone(false) // reads without data are not activity
	}
	if err := context.Cause(ctx); !errors.Is(err, errStreamIdleTimeout) {
		t.Fatalf("cause = %v, want idle timeout", err)
	}
}

func TestExecuteStreamSlowDownstreamIsNotUpstreamIdle(t *testing.T) {
	// Each emit takes longer than stream-idle-timeout while the upstream
	// always has the next chunk ready.
	upstream := tickingUpstream(0, 4)
	responder := func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostStreamEmit {
			time.Sleep(60 * time.Millisecond)
		}
		return upstream(method, payload)
	}
	_, closeErr := runLimitedStream(t, "stream-idle-timeout: 30ms\n", responder)
	if closeErr != "" {
		t.Fatalf("slow downstream closed the stream with error: %s", closeErr)
	}
}
