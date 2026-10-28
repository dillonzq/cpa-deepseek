package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHTTPOperationsOwnIndependentScopes(t *testing.T) {
	type operation struct {
		scope   string
		claimed bool
		cancel  chan struct{}
	}
	var mu sync.Mutex
	operations := map[string]*operation{}
	started := make(chan string, 2)
	caller := func(method string, body []byte) ([]byte, error) {
		var req hostHTTPReq
		if json.Unmarshal(body, &req) != nil {
			return nil, fmt.Errorf("invalid request")
		}
		mu.Lock()
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			id := fmt.Sprint(len(operations) + 1)
			operations[id] = &operation{scope: req.HostCallbackID, cancel: make(chan struct{})}
			mu.Unlock()
			return hostOK(httpOperationRequest{OperationID: id}), nil
		case pluginabi.MethodHostHTTPDo:
			op := operations[req.OperationID]
			if op == nil || op.claimed || op.scope != req.HostCallbackID {
				mu.Unlock()
				return nil, fmt.Errorf("operation claim rejected")
			}
			op.claimed = true
			mu.Unlock()
			started <- op.scope
			<-op.cancel
			return nil, context.Canceled
		case pluginabi.MethodHostHTTPCancel:
			op := operations[req.OperationID]
			if op == nil || op.scope != req.HostCallbackID {
				mu.Unlock()
				return nil, fmt.Errorf("operation owner rejected")
			}
			select {
			case <-op.cancel:
			default:
				close(op.cancel)
			}
			mu.Unlock()
			return hostOK(struct{}{}), nil
		}
		mu.Unlock()
		return nil, fmt.Errorf("unsupported method")
	}
	bridge := NewHostBridge(caller)
	ctxA, cancelA := context.WithCancel(withHostCallbackScope(context.Background(), "scope-a"))
	defer cancelA()
	ctxB, cancelB := context.WithCancel(withHostCallbackScope(context.Background(), "scope-b"))
	defer cancelB()
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { _, err := bridge.Do(ctxA, pluginapi.HTTPRequest{Method: http.MethodPost}); doneA <- err }()
	<-started
	go func() { _, err := bridge.Do(ctxB, pluginapi.HTTPRequest{Method: http.MethodPost}); doneB <- err }()
	<-started
	cancelA()
	select {
	case err := <-doneA:
		if err == nil {
			t.Fatal("canceled HTTP succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach host")
	}
	select {
	case <-doneB:
		t.Fatal("scope A canceled scope B")
	case <-time.After(20 * time.Millisecond):
	}
	cancelB()
	select {
	case err := <-doneB:
		if err == nil {
			t.Fatal("canceled HTTP succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("scope B did not cancel")
	}
	if !bridge.WaitForInFlight(time.Second) {
		t.Fatal("actual HTTP callbacks did not drain")
	}
}

func TestLateOperationReservationIsCanceled(t *testing.T) {
	release := make(chan struct{})
	canceled := make(chan httpOperationRequest, 1)
	caller := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			<-release
			return hostOK(httpOperationRequest{OperationID: "late-op"}), nil
		case pluginabi.MethodHostHTTPCancel:
			var req httpOperationRequest
			_ = json.Unmarshal(payload, &req)
			canceled <- req
			return hostOK(struct{}{}), nil
		default:
			return nil, fmt.Errorf("HTTP must not start after canceled reservation")
		}
	}
	bridge := NewHostBridge(caller)
	ctx, cancel := context.WithTimeout(withHostCallbackScope(context.Background(), "late-scope"), 10*time.Millisecond)
	defer cancel()
	if _, err := bridge.Do(ctx, pluginapi.HTTPRequest{}); err == nil {
		t.Fatal("expired open succeeded")
	}
	close(release)
	select {
	case req := <-canceled:
		if req.HostCallbackID != "late-scope" || req.OperationID != "late-op" {
			t.Fatal("wrong reservation released")
		}
	case <-time.After(time.Second):
		t.Fatal("late reservation leaked")
	}
	if !bridge.WaitForInFlight(time.Second) {
		t.Fatal("late cleanup did not drain")
	}
}

func TestHTTPOperationOpenFailsClosed(t *testing.T) {
	for _, raw := range []string{`{"ok":false,"error":{"code":"unknown_method"}}`, `{"ok":true,"result":{}}`, `{`} {
		calls := 0
		bridge := NewHostBridge(func(method string, _ []byte) ([]byte, error) {
			calls++
			if method != pluginabi.MethodHostHTTPOperationOpen {
				t.Error("HTTP ran without ownership")
			}
			return []byte(raw), nil
		})
		if _, err := bridge.Do(context.Background(), pluginapi.HTTPRequest{}); err == nil {
			t.Fatal("invalid operation open succeeded")
		}
		if calls != 1 {
			t.Fatalf("callbacks=%d", calls)
		}
	}
}
