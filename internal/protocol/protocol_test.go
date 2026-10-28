package protocol

import (
	"testing"

	"github.com/dillonzq/cpa-deepseek/internal/catalog"
)

func TestStreamFailureEventsRemainFailures(t *testing.T) {
	for _, tc := range []struct {
		route         catalog.Route
		format, event string
	}{
		{catalog.RouteChatCompletions, "openai", "data: {\"error\":{\"message\":\"service down\",\"type\":\"server_error\"}}\n\n"},
		{catalog.RouteResponses, "openai-response", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"service down\",\"code\":\"server_error\"}}}\n\n"},
		{catalog.RouteMessages, "claude", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"service down\"}}\n\n"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			stream, e := NewStreamConverter(tc.route, tc.format, &State{})
			if e != nil {
				t.Fatal(e)
			}
			_, _, e = stream.Feed([]byte(tc.event))
			if e == nil || e.Message == "" {
				t.Fatal("upstream failure became a successful stream")
			}
		})
	}
}

func TestMalformedStreamingJSONIsNotSuccessfulEOF(t *testing.T) {
	stream, e := NewStreamConverter(catalog.RouteChatCompletions, "openai", &State{})
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = stream.Feed([]byte("data: {\"id\":"))
	if e == nil {
		_, e = stream.Finish()
	}
	if e == nil {
		t.Fatal("truncated JSON became a successful stream")
	}
}
