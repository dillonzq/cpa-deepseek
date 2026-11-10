package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNewConfiguration(t *testing.T) {
	t.Setenv("DEEPSEEK_TEST_KEY", "secret-value")
	c, err := Load([]byte("enabled: true\npriority: 10\nstore: {}\napi-keys:\n  - name: Personal\n    value: ${DEEPSEEK_TEST_KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://api.deepseek.com" || c.APIKeys[0].Value != "secret-value" || c.RequestTimeout != 5*time.Minute || c.StreamIdleTimeout != 5*time.Minute || c.MaxStreamDuration != time.Hour || c.QuotaTimeout != 15*time.Second || PublicID(c, "deepseek-flash") != "deepseek/deepseek-flash" {
		t.Fatalf("wrong defaults: %+v", c)
	}
	if c.HasAPIKey("removed-key") || !c.HasAPIKey("secret-value") {
		t.Fatal("credential membership is wrong")
	}
}

func TestRejectInvalidConfigurationWithoutLeakingValues(t *testing.T) {
	base := "api-keys:\n  - value: sensitive-secret-123\n"
	for _, tc := range []string{
		"api-keys: [sensitive-secret-123]", "api-key: sensitive-secret-123",
		"api-keys: [{api-key: sensitive-secret-123}]",
		"api-keys: [{value: valid-key, api-key: sensitive-secret-123}]", base + "unknown: sensitive-secret-123\n",
		base + "---\napi-keys: []", base + "base-url: https://user:sensitive-secret-123@example.test\n",
		base + "base-url: http://example.test\n", base + "base-url: https://example.test?key=sensitive-secret-123\n",
		base + "base-url: file:///tmp/key\n", base + "quota: {timeout: 0s}\n", base + "quota: {timeout: 2m}\n",
		base + "request-timeout: -1s\n", base + "request-timeout: 0s\n", base + "stream-idle-timeout: 0s\n", base + "stream-idle-timeout: -1s\n",
		base + "stream-idle-timeout: bogus\n", base + "max-stream-duration: 0s\n", base + "max-stream-duration: -1m\n", base + "max-stream-duration: 5\n",
		base + "catalog: {refresh-interval: 1s}\n", base + "max-response-bytes: 0\n",
		base + "model-prefix: {value: ../invalid}\n", base + "models: [{name: ' '}]\n",
		base + "models: [{name: a, alias: x}, {name: b, alias: x}]\n", base + "models: [{name: a, alias: ' x'}]\n",
		"api-keys:\n  - value: sensitive-secret-123\n  - value: sensitive-secret-123\n",
		"api-keys:\n  - value: ${DEEPSEEK_MISSING_TEST_ENV}\n",
		"api-keys:\n  - value: \"sensitive-secret-123\\nsecond-line\"\n",
		base + "base-url: https://:443\n", base + "base-url: https://api.example.test?\n",
		base + "base-url: https://api.example.test/a%2fb\n",
		"api-keys:\n  - value: \"sensitive-secret-123\\tx\"\n",
		"api-keys:\n  - value: \"sensitive-secret-123\\u0000x\"\n",
		"api-keys:\n  - value: sensitive-secret-123" + strings.Repeat("x", MaxAPIKeyLength) + "\n",
		"api-keys:\n  - {value: sensitive-secret-123, name: \"a\\u0007b\"}\n",
		"api-keys:\n  - {value: sensitive-secret-123, name: " + strings.Repeat("n", MaxAPIKeyNameLength+1) + "}\n",
		tooManyKeys(),
	} {
		t.Run(tc, func(t *testing.T) {
			_, err := Load([]byte(tc))
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			if strings.Contains(err.Error(), "sensitive-secret") {
				t.Fatal("error leaked credential")
			}
		})
	}
}

func tooManyKeys() string {
	var b strings.Builder
	b.WriteString("api-keys:\n")
	for i := 0; i <= MaxAPIKeys; i++ {
		fmt.Fprintf(&b, "  - value: sensitive-secret-%d\n", i)
	}
	return b.String()
}

func TestRejectExtraYAMLDocument(t *testing.T) {
	base := "api-keys: [{value: sensitive-secret-123}]\n"
	for _, tc := range []struct{ yaml, want string }{
		{base + "---\napi-keys: []\n", "expected one YAML document"},
		{base + "---\n[sensitive-secret-123\n", "invalid YAML structure near line"},
	} {
		_, err := Load([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "sensitive-secret") {
			t.Fatalf("Load(%q) error = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

func TestMetadataOverridesAndBasePath(t *testing.T) {
	c, err := Load([]byte("api-keys: [{value: key}]\nbase-url: http://localhost:8080/proxy/\nallow-http: true\nmodel-prefix: {enabled: false}\nmodels:\n  - name: deepseek-flash\n    max-context-length: 1048576\n    max-tokens: 393216\n    input-modalities: [text, image]\n    thinking: {zero-allowed: true, levels: [low, high, max]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://localhost:8080/proxy" || PublicID(c, "deepseek-flash") != "deepseek-flash" || *c.Models[0].Context != 1048576 {
		t.Fatal("overrides were not retained")
	}
}

func TestModelAlias(t *testing.T) {
	c, err := Load([]byte("api-keys: [{value: key}]\nmodels: [{name: deepseek-flash, alias: flash}, {name: deepseek-v4-pro}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Models[0].Alias != "flash" || c.Models[1].Alias != "" {
		t.Fatalf("aliases = %q, %q", c.Models[0].Alias, c.Models[1].Alias)
	}
}

func TestStreamTimeoutSettings(t *testing.T) {
	c, err := Load([]byte("api-keys: [{value: key}]\nrequest-timeout: 30s\nstream-idle-timeout: 90s\nmax-stream-duration: 2h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestTimeout != 30*time.Second || c.StreamIdleTimeout != 90*time.Second || c.MaxStreamDuration != 2*time.Hour {
		t.Fatalf("stream timeouts = %v/%v/%v", c.RequestTimeout, c.StreamIdleTimeout, c.MaxStreamDuration)
	}
}
