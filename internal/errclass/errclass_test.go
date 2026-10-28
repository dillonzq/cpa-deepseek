package errclass

import (
	"strings"
	"testing"
)

func TestDiagnosticRedactionAndHTTPStatus(t *testing.T) {
	for _, message := range []string{
		"Authorization: Bearer secret-value", `"x-api-key": "secret-value"`, "x-api-key=secret-value", "https://host/path?api_key=secret-value&other=ok",
	} {
		clean := Redact(message)
		if strings.Contains(clean, "secret-value") || Redact(clean) != clean {
			t.Fatalf("redaction was unsafe or not idempotent: %s", clean)
		}
	}
	for _, status := range []int{401, 402, 429, 500, 503} {
		err := UpstreamStatusError(status, []byte(`{"error":{"message":"upstream rejected the call"}}`))
		wire := ToEnvelopeError(err)
		if wire.HTTPStatus != status || wire.Message == "" {
			t.Fatal("lost error status or message")
		}
	}
}
