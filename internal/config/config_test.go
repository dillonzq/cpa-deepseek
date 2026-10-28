package config

import (
	"strings"
	"testing"
	"time"
)

func TestNewConfiguration(t *testing.T) {
	t.Setenv("DEEPSEEK_TEST_KEY", "secret-value")
	c, err := Load([]byte("enabled: true\npriority: 10\nstore: {}\ncredentials:\n  - name: Personal\n    api-key: ${DEEPSEEK_TEST_KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://api.deepseek.com" || c.Credentials[0].Value != "secret-value" || c.RequestTimeout != 15*time.Minute || c.QuotaTimeout != 15*time.Second || PublicID(c, "deepseek-flash") != "deepseek/deepseek-flash" {
		t.Fatalf("wrong defaults: %+v", c)
	}
	if c.HasCredential("removed-key") || !c.HasCredential("secret-value") {
		t.Fatal("credential membership is wrong")
	}
}

func TestRejectInvalidConfigurationWithoutLeakingValues(t *testing.T) {
	base := "credentials:\n  - api-key: sensitive-secret-123\n"
	for _, tc := range []string{
		"credentials: [sensitive-secret-123]", "api-key: sensitive-secret-123", "credentials: []", base + "unknown: sensitive-secret-123\n",
		base + "---\ncredentials: []", base + "base-url: https://user:sensitive-secret-123@example.test\n",
		base + "base-url: http://example.test\n", base + "base-url: https://example.test?key=sensitive-secret-123\n",
		base + "base-url: file:///tmp/key\n", base + "quota: {timeout: 0s}\n", base + "quota: {timeout: 2m}\n",
		base + "request-timeout: -1s\n", base + "catalog: {refresh-interval: 1s}\n", base + "max-response-bytes: 0\n",
		base + "model-prefix: {value: ../invalid}\n", base + "models: [{name: bad(id)}]\n",
		"credentials:\n  - api-key: sensitive-secret-123\n  - api-key: sensitive-secret-123\n",
		"credentials:\n  - api-key: ${DEEPSEEK_MISSING_TEST_ENV}\n",
		"credentials:\n  - api-key: 'sensitive-secret-123 space'\n",
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

func TestMetadataOverridesAndBasePath(t *testing.T) {
	c, err := Load([]byte("credentials: [{api-key: key}]\nbase-url: http://localhost:8080/proxy/\nallow-http: true\nmodel-prefix: {enabled: false}\nmodels:\n  - name: deepseek-flash\n    max-context-length: 1048576\n    max-tokens: 393216\n    input-modalities: [text, image]\n    thinking: {zero-allowed: true, levels: [low, high, max]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://localhost:8080/proxy" || PublicID(c, "deepseek-flash") != "deepseek-flash" || *c.Models[0].Context != 1048576 {
		t.Fatal("overrides were not retained")
	}
}
