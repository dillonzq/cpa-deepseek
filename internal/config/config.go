// Package config defines the plugin's v8 configuration.
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

const (
	DefaultBaseURL                = "https://api.deepseek.com"
	DefaultRequestTimeout         = 15 * time.Minute
	DefaultQuotaTimeout           = 15 * time.Second
	DefaultRefreshInterval        = 15 * time.Minute
	DefaultMaxResponseBytes int64 = 64 << 20
)

type Credential struct {
	Name  string `yaml:"name"`
	Value string `yaml:"api-key"`
}

type Model struct {
	Name               string `yaml:"name"`
	modelmeta.Metadata `yaml:",inline"`
}

type Config struct {
	Credentials []Credential
	BaseURL     string
	Models      []Model
	ModelPrefix struct {
		Enabled bool
		Value   string
	}
	Catalog struct {
		RefreshInterval       time.Duration
		StaleWhileUnavailable bool
	}
	QuotaTimeout     time.Duration
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	AllowHTTP        bool
}

type rawConfig struct {
	// CPA passes its plugin controls alongside provider-owned settings.
	Enabled     *bool          `yaml:"enabled"`
	Priority    int            `yaml:"priority"`
	Store       map[string]any `yaml:"store"`
	Credentials []Credential   `yaml:"credentials"`
	BaseURL     *string        `yaml:"base-url"`
	Models      []Model        `yaml:"models"`
	ModelPrefix struct {
		Enabled *bool   `yaml:"enabled"`
		Value   *string `yaml:"value"`
	} `yaml:"model-prefix"`
	Catalog struct {
		RefreshInterval       *string `yaml:"refresh-interval"`
		StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
	} `yaml:"catalog"`
	Quota struct {
		Timeout *string `yaml:"timeout"`
	} `yaml:"quota"`
	RequestTimeout   *string `yaml:"request-timeout"`
	MaxResponseBytes *int64  `yaml:"max-response-bytes"`
	AllowHTTP        bool    `yaml:"allow-http"`
}

// Load never returns YAML values in errors: even malformed nodes can contain keys.
func Load(data []byte) (Config, error) {
	var raw rawConfig
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("invalid plugin YAML structure or unknown setting")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("expected one YAML document")
	}
	c := Config{Credentials: raw.Credentials, Models: raw.Models, AllowHTTP: raw.AllowHTTP,
		BaseURL:          strings.TrimRight(value(raw.BaseURL, DefaultBaseURL), "/"),
		MaxResponseBytes: value(raw.MaxResponseBytes, DefaultMaxResponseBytes)}
	c.ModelPrefix.Enabled, c.ModelPrefix.Value = value(raw.ModelPrefix.Enabled, true), value(raw.ModelPrefix.Value, "deepseek")
	c.Catalog.StaleWhileUnavailable = value(raw.Catalog.StaleWhileUnavailable, true)
	var err error
	if c.Catalog.RefreshInterval, err = duration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval); err != nil {
		return Config{}, err
	}
	if c.RequestTimeout, err = duration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout); err != nil {
		return Config{}, err
	}
	if c.QuotaTimeout, err = duration("quota.timeout", raw.Quota.Timeout, DefaultQuotaTimeout); err != nil {
		return Config{}, err
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return Config{}, fmt.Errorf("catalog.refresh-interval must be at least 1m")
	}
	if c.QuotaTimeout > time.Minute {
		return Config{}, fmt.Errorf("quota.timeout must be at most 1m")
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > 1<<30 {
		return Config{}, fmt.Errorf("max-response-bytes must be between 1 and 1073741824")
	}
	if c.ModelPrefix.Enabled && !identifier.MatchString(c.ModelPrefix.Value) {
		return Config{}, fmt.Errorf("invalid model-prefix.value")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.AllowHTTP)) {
		return Config{}, fmt.Errorf("base-url must be an HTTPS URL without userinfo, query or fragment (HTTP requires allow-http)")
	}
	if len(c.Credentials) == 0 || len(c.Credentials) > 256 {
		return Config{}, fmt.Errorf("credentials must contain 1 to 256 entries")
	}
	seen := map[string]bool{}
	for i := range c.Credentials {
		k := &c.Credentials[i]
		k.Value = strings.TrimSpace(os.ExpandEnv(k.Value))
		k.Name = strings.TrimSpace(k.Name)
		if k.Value == "" || len(k.Value) > 4096 || strings.IndexFunc(k.Value, unicode.IsSpace) >= 0 || strings.IndexFunc(k.Value, unicode.IsControl) >= 0 {
			return Config{}, fmt.Errorf("credentials[%d].api-key is empty or invalid", i)
		}
		if len(k.Name) > 256 || strings.IndexFunc(k.Name, unicode.IsControl) >= 0 {
			return Config{}, fmt.Errorf("credentials[%d].name is invalid", i)
		}
		if seen[k.Value] {
			return Config{}, fmt.Errorf("duplicate credential key")
		}
		seen[k.Value] = true
	}
	models := map[string]bool{}
	for i, m := range c.Models {
		if !modelID.MatchString(m.Name) || models[m.Name] {
			return Config{}, fmt.Errorf("models[%d].name is invalid or duplicated", i)
		}
		models[m.Name] = true
		if m.Context != nil && *m.Context < 0 || m.Output != nil && *m.Output < 0 {
			return Config{}, fmt.Errorf("models[%d] token limits must be non-negative", i)
		}
		if t := m.Thinking; t != nil {
			if t.Min != nil && *t.Min < 0 || t.Max != nil && *t.Max < 0 || t.Min != nil && t.Max != nil && *t.Min > *t.Max {
				return Config{}, fmt.Errorf("models[%d].thinking has invalid bounds", i)
			}
		}
	}
	return c, nil
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func duration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func value[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

func (c Config) HasCredential(key string) bool {
	for _, k := range c.Credentials {
		if k.Value == key {
			return true
		}
	}
	return false
}

func PublicID(c Config, model string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + model
	}
	return model
}
