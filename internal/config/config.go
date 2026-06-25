// Package config loads and validates the YAML configuration file.
// Environment variables are expanded from ${VAR} or $VAR placeholders.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration object loaded from config.yaml.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Storage  StorageConfig  `yaml:"storage"`
	Admin    AdminConfig    `yaml:"admin"`
	Limits   LimitsConfig   `yaml:"limits"`
}

type ServerConfig struct {
	Listen        string        `yaml:"listen"`
	AdminListen   string        `yaml:"admin_listen"`
	ReadTimeout   time.Duration `yaml:"read_timeout"`
	WriteTimeout  time.Duration `yaml:"write_timeout"`
	IdleTimeout   time.Duration `yaml:"idle_timeout"`
}

type UpstreamConfig struct {
	BaseURL        string        `yaml:"base_url"`
	APIKey         string        `yaml:"api_key"`
	PathPrefix     string        `yaml:"path_prefix"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

type StorageConfig struct {
	DBPath        string `yaml:"db_path"`
	RetentionDays int    `yaml:"retention_days"`
}

type AdminConfig struct {
	Token        string        `yaml:"token"`
	CookieSecret string        `yaml:"cookie_secret"`
	CookieTTL    time.Duration `yaml:"cookie_ttl"`
}

type LimitsConfig struct {
	MaxBodyBytes  int64 `yaml:"max_body_bytes"`
	MaxConcurrent int   `yaml:"max_concurrent"`
}

// Default returns sensible defaults used when keys are missing in YAML.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:       "0.0.0.0:8080",
			AdminListen:  "127.0.0.1:8081",
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 0,
			IdleTimeout:  120 * time.Second,
		},
		Upstream: UpstreamConfig{
			BaseURL:        "https://nano-gpt.com",
			PathPrefix:     "/api/v1",
			RequestTimeout: 300 * time.Second,
		},
		Storage: StorageConfig{
			DBPath:        "./data/nano-proxy.db",
			RetentionDays: 90,
		},
		Admin: AdminConfig{
			CookieTTL: 12 * time.Hour,
		},
		Limits: LimitsConfig{
			MaxBodyBytes:  2 << 20, // 2 MiB
			MaxConcurrent: 64,
		},
	}
}

// Load reads config.yaml from path, applies defaults, expands ${ENV} vars,
// and validates required secrets.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	expanded := os.Expand(string(raw), func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok {
			return ""
		}
		return v
	})
	// Also expand ${VAR} syntax for keys that os.Expand misses.
	expanded = expandBraced(expanded, os.Getenv)

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse yaml: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func expandBraced(s string, getter func(string) string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue
			}
			name := s[i+2 : i+2+end]
			b.WriteString(getter(name))
			i = i + 2 + end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func (c Config) validate() error {
	// Upstream.APIKey is OPTIONAL here. The real bootstrap happens in main():
	//   1. If the env NANOGPT_API_KEY is set and DB settings is empty,
	//      seed the env value into the settings table.
	//   2. Otherwise load whatever is in the settings table.
	//   3. If both are empty the operator can configure the key from the
	//      admin UI at first run.
	if c.Admin.Token == "" {
		return fmt.Errorf("admin.token is required (set ADMIN_TOKEN env)")
	}
	if c.Admin.CookieSecret == "" {
		c.Admin.CookieSecret = c.Admin.Token // fallback; warn in main()
	}
	if c.Server.Listen == "" || c.Server.AdminListen == "" {
		return fmt.Errorf("server.listen and server.admin_listen must be set")
	}
	if c.Limits.MaxBodyBytes <= 0 {
		return fmt.Errorf("limits.max_body_bytes must be > 0")
	}
	return nil
}