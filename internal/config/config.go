// Package config loads and validates the DeepSeek plugin configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/dillonzq/cpa-deepseek/internal/modelmeta"

	"gopkg.in/yaml.v3"
)

// Defaults.
const (
	DefaultBaseURL           = "https://api.deepseek.com"
	DefaultQuotaTimeout      = 15 * time.Second
	DefaultModelPrefix       = "deepseek"
	DefaultRefreshInterval   = 15 * time.Minute
	DefaultRequestTimeout    = 5 * time.Minute
	DefaultStreamIdleTimeout = 5 * time.Minute
	DefaultMaxStreamDuration = time.Hour
	DefaultMaxResponseBytes  = int64(67108864) // 64 MiB
)

// API key limits.
const (
	MaxAPIKeys          = 256
	MaxAPIKeyLength     = 4096
	MaxAPIKeyNameLength = 256
)

var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

// decodeError reports only the line of a YAML decode failure; the decoder's
// message can quote node values, which may be credentials.
func decodeError(err error) error {
	if n := yamlLinePattern.FindStringSubmatch(err.Error()); n != nil {
		return fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
	}
	return fmt.Errorf("decode config: invalid YAML structure")
}

type ModelPrefix struct {
	Enabled bool
	Value   string
}

type APIKey struct {
	Name  string
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Config struct {
	Models       []ModelOverride
	BaseURL      string
	QuotaTimeout time.Duration
	ModelPrefix  ModelPrefix
	APIKeys      []APIKey
	Catalog      Catalog
	AllowHTTP    bool
	// RequestTimeout bounds a whole non-streaming request, or only the
	// opening of a stream.
	RequestTimeout time.Duration
	// StreamIdleTimeout bounds the silence between upstream stream chunks;
	// each chunk restarts it.
	StreamIdleTimeout time.Duration
	// MaxStreamDuration bounds one stream's whole lifetime, opening included.
	MaxStreamDuration time.Duration
	MaxResponseBytes  int64
}

type ModelOverride struct {
	Name string `yaml:"name"`
	// Alias replaces the upstream name in the published model ID; the
	// model prefix still applies. Requests still send Name upstream.
	Alias              string `yaml:"alias"`
	modelmeta.Metadata `yaml:",inline"`
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Host-owned plugin controls are accepted alongside plugin settings.
type rawConfig struct {
	Enabled  *bool           `yaml:"enabled"`
	Priority int             `yaml:"priority"`
	Store    map[string]any  `yaml:"store"`
	Models   []ModelOverride `yaml:"models"`
	BaseURL  *string         `yaml:"base-url"`
	Quota    struct {
		Timeout *string `yaml:"timeout"`
	} `yaml:"quota"`
	ModelPrefix       rawPrefix   `yaml:"model-prefix"`
	APIKeys           []rawAPIKey `yaml:"api-keys"`
	Catalog           rawCatalog  `yaml:"catalog"`
	AllowHTTP         bool        `yaml:"allow-http"`
	RequestTimeout    *string     `yaml:"request-timeout"`
	StreamIdleTimeout *string     `yaml:"stream-idle-timeout"`
	MaxStreamDuration *string     `yaml:"max-stream-duration"`
	MaxResponseBytes  *int64      `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawAPIKey struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

// Load decodes YAML, expands ${VAR} references in api-keys[].value only,
// applies defaults, and validates. Unknown settings are rejected. Decode
// errors never echo decoded node values — a malformed entry (e.g. a
// bare-scalar API key) must not leak into the invalid_config envelope the
// host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	decoder := yaml.NewDecoder(bytes.NewReader(yamlBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, decodeError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Config{}, fmt.Errorf("decode config: expected one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return Config{}, decodeError(err)
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: strings.TrimSpace(os.ExpandEnv(k.Value)), Name: strings.TrimSpace(k.Name)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parsePositiveDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	streamIdle, err := parsePositiveDuration("stream-idle-timeout", raw.StreamIdleTimeout, DefaultStreamIdleTimeout)
	if err != nil {
		return Config{}, err
	}
	maxStream, err := parsePositiveDuration("max-stream-duration", raw.MaxStreamDuration, DefaultMaxStreamDuration)
	if err != nil {
		return Config{}, err
	}
	quotaTimeout, err := parseDuration("quota.timeout", raw.Quota.Timeout, DefaultQuotaTimeout)
	if err != nil {
		return Config{}, err
	}
	if quotaTimeout <= 0 || quotaTimeout > time.Minute {
		return Config{}, fmt.Errorf("quota.timeout: must be positive and at most 1m")
	}
	c := Config{
		Models:       raw.Models,
		BaseURL:      strings.TrimRight(orDefault(raw.BaseURL, DefaultBaseURL), "/"),
		QuotaTimeout: quotaTimeout,
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		AllowHTTP:         raw.AllowHTTP,
		RequestTimeout:    requestTimeout,
		StreamIdleTimeout: streamIdle,
		MaxStreamDuration: maxStream,
		MaxResponseBytes:  orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// PublicID returns the client-facing model ID.
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

// HasAPIKey prevents persisted records for removed keys from executing.
// An unconfigured plugin has no keys, so it accepts none.
func (c Config) HasAPIKey(key string) bool {
	for _, entry := range c.APIKeys {
		if entry.Value == key {
			return true
		}
	}
	return false
}

func (c Config) validate() error {
	seenModels := map[string]bool{}
	seenAliases := map[string]bool{}
	for i, model := range c.Models {
		if strings.TrimSpace(model.Name) == "" || seenModels[model.Name] {
			return fmt.Errorf("models[%d].name: must be non-empty and unique", i)
		}
		seenModels[model.Name] = true
		if model.Alias != "" {
			if strings.TrimSpace(model.Alias) != model.Alias || seenAliases[model.Alias] {
				return fmt.Errorf("models[%d].alias: must be unique without surrounding whitespace", i)
			}
			seenAliases[model.Alias] = true
		}
		if model.Context != nil && *model.Context < 0 || model.Output != nil && *model.Output < 0 {
			return fmt.Errorf("models[%d]: token limits must be non-negative", i)
		}
		if model.Thinking != nil {
			t := model.Thinking
			if t.Min != nil && *t.Min < 0 || t.Max != nil && *t.Max < 0 || t.Min != nil && t.Max != nil && *t.Min > *t.Max {
				return fmt.Errorf("models[%d].thinking: invalid budget range", i)
			}
		}
	}
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	if len(c.APIKeys) > MaxAPIKeys {
		return fmt.Errorf("api-keys: at most %d keys are allowed", MaxAPIKeys)
	}
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
		if len(k.Value) > MaxAPIKeyLength {
			return fmt.Errorf("api-keys[%d].value: must be at most %d bytes", i, MaxAPIKeyLength)
		}
		if strings.IndexFunc(k.Value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return fmt.Errorf("api-keys[%d].value: must not contain whitespace or control characters", i)
		}
		if len(k.Name) > MaxAPIKeyNameLength || strings.IndexFunc(k.Name, unicode.IsControl) >= 0 {
			return fmt.Errorf("api-keys[%d].name: must be at most %d bytes without control characters", i, MaxAPIKeyNameLength)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	// ForceQuery catches a bare trailing "?": JoinUpstreamURL concatenates
	// strings, so the endpoint path would land inside the query.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// RawPath is set only when the path uses non-canonical percent-encoding;
	// reject it so the configured path is unambiguous.
	if u.RawPath != "" {
		return fmt.Errorf("%s: path must not use non-canonical percent-encoding", name)
	}
	// Scheme allowlist: https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func parsePositiveDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	d, err := parseDuration(name, p, def)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
