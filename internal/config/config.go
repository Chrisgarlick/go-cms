// Package config loads and validates runtime configuration from one YAML file
// plus CMS_* environment variable overrides. Unknown keys are an error.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Env string

const (
	EnvProduction  Env = "production"
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
)

type Config struct {
	Env     Env    `yaml:"env"`
	BaseURL string `yaml:"base_url"`
	// Hosts the public site answers for. Requests for any other Host get 421.
	Hosts []string `yaml:"hosts"`
	// MediaURL is the separate media origin, e.g. https://media.example.com.
	// Empty means media is served from BaseURL under /media/.
	MediaURL string `yaml:"media_url"`

	HTTP     HTTP     `yaml:"http"`
	Database Database `yaml:"database"`
	Paths    Paths    `yaml:"paths"`
	Keys     Keys     `yaml:"keys"`
	Media    Media    `yaml:"media"`
	Mail     Mail     `yaml:"mail"`
	Security Security `yaml:"security"`
	Jobs     Jobs     `yaml:"jobs"`
	Upgrade  Upgrade  `yaml:"upgrade"`
	API      API      `yaml:"api"`
}

type API struct {
	// CORSOrigins may call the read API from browsers (headless front ends).
	CORSOrigins []string `yaml:"cors_origins"`
	// Disabled turns the API off entirely.
	Disabled bool `yaml:"disabled"`
	// Headless disables the public site renderer (API only).
	Headless bool `yaml:"headless"`
}

type HTTP struct {
	Addr              string        `yaml:"addr"`
	TrustedProxies    []string      `yaml:"trusted_proxies"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	// ServeStatic makes Go serve rendered files itself (dev, or no Caddy).
	ServeStatic bool `yaml:"serve_static"`
	// MetricsTokenFile allows non-loopback scrapes of /metrics with a bearer token.
	MetricsTokenFile string `yaml:"metrics_token_file"`

	trusted []netip.Prefix
}

type Database struct {
	URL         string `yaml:"url"`
	MigratorURL string `yaml:"migrator_url"`
	MaxConns    int32  `yaml:"max_conns"`
	JobMaxConns int32  `yaml:"job_max_conns"`
}

type Paths struct {
	Data string `yaml:"data"` // root for public/, media/, tmp/
	Site string `yaml:"site"` // site bundle directory (site.yaml, theme/, content-types/ ...)
}

type Keys struct {
	// KEKFile holds the 32-byte key-encryption key (hex or raw). With systemd
	// LoadCredential it lives under $CREDENTIALS_DIRECTORY.
	KEKFile string `yaml:"kek_file"`
}

type Media struct {
	MaxUploadMB int    `yaml:"max_upload_mb"`
	MaxPixels   int    `yaml:"max_pixels"`
	VipsBinary  string `yaml:"vips_binary"`
	Storage     string `yaml:"storage"` // local
}

type Mail struct {
	From     string `yaml:"from"`
	SMTPAddr string `yaml:"smtp_addr"` // host:port; empty = log emails instead of sending
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Security struct {
	VulnLookup bool   `yaml:"vuln_lookup"`
	AlertEmail string `yaml:"alert_email"`
	// DevSkip2FA lets `cms dev` sign in without enrolling an authenticator.
	// Refused outside development.
	DevSkip2FA bool `yaml:"dev_skip_2fa"`
}

type Jobs struct {
	Concurrency int `yaml:"concurrency"`
}

type Upgrade struct {
	Channel     string `yaml:"channel"`       // edge | canary | stable
	FeedURL     string `yaml:"feed_url"`      // release feed (private GitHub releases API)
	SiteFeedURL string `yaml:"site_feed_url"` // the site repo's releases (site bundles)
	// SiteTagPrefix picks this site's bundles out of a feed that may hold other
	// streams (a monorepo): tags <prefix>-YYYYMMDD-HHMMSS-<sha7>. Default "site".
	SiteTagPrefix string `yaml:"site_tag_prefix"`
	TokenFile     string `yaml:"token_file"` // read-only token for the feed
	PublicKey     string `yaml:"public_key"` // cosign public key path (core releases)
	// SitePublicKey verifies site bundles when they are signed with a different
	// key than core releases (a site repo has its own signing key). Defaults to PublicKey.
	SitePublicKey string `yaml:"site_public_key"`
}

// SiteKey is the public key that verifies site bundles.
func (u Upgrade) SiteKey() string {
	if u.SitePublicKey != "" {
		return u.SitePublicKey
	}
	return u.PublicKey
}

// Defaults returns a config suitable for development before overrides.
func Defaults() Config {
	return Config{
		Env:     EnvProduction,
		BaseURL: "http://localhost:8080",
		HTTP: HTTP{
			Addr:              "127.0.0.1:8080",
			TrustedProxies:    []string{"127.0.0.1/32", "::1/128"},
			ReadHeaderTimeout: 10 * time.Second,
			ShutdownTimeout:   30 * time.Second,
		},
		Database: Database{MaxConns: 10, JobMaxConns: 4},
		Paths:    Paths{Data: "/var/lib/cms", Site: "/opt/cms/site/current"},
		Media:    Media{MaxUploadMB: 64, MaxPixels: 50_000_000, VipsBinary: "vips", Storage: "local"},
		Security: Security{VulnLookup: true},
		Jobs:     Jobs{Concurrency: 4},
		Upgrade:  Upgrade{Channel: "stable"},
	}
}

// Load reads path (optional; "" skips the file), applies env overrides, and validates.
func Load(path string) (*Config, error) {
	c := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	applyEnv(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func applyEnv(c *Config) {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	str("CMS_BASE_URL", &c.BaseURL)
	str("CMS_MEDIA_URL", &c.MediaURL)
	str("CMS_ADDR", &c.HTTP.Addr)
	str("CMS_DATA_DIR", &c.Paths.Data)
	str("CMS_SITE_DIR", &c.Paths.Site)
	str("CMS_KEK_FILE", &c.Keys.KEKFile)
	str("DATABASE_URL", &c.Database.URL)
	str("DATABASE_MIGRATOR_URL", &c.Database.MigratorURL)
	str("CMS_SMTP_ADDR", &c.Mail.SMTPAddr)
	str("CMS_MAIL_FROM", &c.Mail.From)
	if v, ok := os.LookupEnv("CMS_ENV"); ok {
		c.Env = Env(v)
	}
	if v, ok := os.LookupEnv("CMS_HOSTS"); ok {
		c.Hosts = splitList(v)
	}
	if v, ok := os.LookupEnv("CMS_SERVE_STATIC"); ok {
		c.HTTP.ServeStatic, _ = strconv.ParseBool(v)
	}
	// systemd LoadCredential: default the KEK path when present.
	if c.Keys.KEKFile == "" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			c.Keys.KEKFile = filepath.Join(dir, "kek")
		}
	}
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks every field and returns all problems at once.
func (c *Config) Validate() error {
	var errs []error
	switch c.Env {
	case EnvProduction, EnvDevelopment, EnvTest:
	default:
		errs = append(errs, fmt.Errorf("env: must be production, development or test, got %q", c.Env))
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("base_url: must be an absolute URL, got %q", c.BaseURL))
	} else {
		if len(c.Hosts) == 0 {
			c.Hosts = []string{u.Hostname()}
		}
		if c.Env == EnvProduction && u.Scheme != "https" {
			errs = append(errs, errors.New("base_url: must use https in production"))
		}
	}
	if c.MediaURL != "" {
		if mu, err := url.Parse(c.MediaURL); err != nil || mu.Host == "" {
			errs = append(errs, fmt.Errorf("media_url: must be an absolute URL, got %q", c.MediaURL))
		}
	}
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url (or DATABASE_URL) is required"))
	}
	if c.Database.MaxConns < 2 || c.Database.MaxConns > 200 {
		errs = append(errs, errors.New("database.max_conns: must be between 2 and 200"))
	}
	if c.Database.JobMaxConns < 1 || c.Database.JobMaxConns > 100 {
		errs = append(errs, errors.New("database.job_max_conns: must be between 1 and 100"))
	}
	if c.Paths.Data == "" {
		errs = append(errs, errors.New("paths.data is required"))
	}
	c.HTTP.trusted = c.HTTP.trusted[:0]
	for _, p := range c.HTTP.TrustedProxies {
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			addr, aerr := netip.ParseAddr(p)
			if aerr != nil {
				errs = append(errs, fmt.Errorf("http.trusted_proxies: %q is not an IP or CIDR", p))
				continue
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		c.HTTP.trusted = append(c.HTTP.trusted, pfx)
	}
	if c.Media.MaxUploadMB < 1 {
		errs = append(errs, errors.New("media.max_upload_mb: must be at least 1"))
	}
	if c.Jobs.Concurrency < 1 {
		errs = append(errs, errors.New("jobs.concurrency: must be at least 1"))
	}
	if c.Security.DevSkip2FA && c.Env != EnvDevelopment {
		errs = append(errs, errors.New("security.dev_skip_2fa is only allowed when env is development"))
	}
	switch c.Upgrade.Channel {
	case "edge", "canary", "stable":
	default:
		errs = append(errs, fmt.Errorf("upgrade.channel: must be edge, canary or stable, got %q", c.Upgrade.Channel))
	}
	return errors.Join(errs...)
}

// IsTrustedProxy reports whether addr may set X-Forwarded-For.
func (h *HTTP) IsTrustedProxy(addr netip.Addr) bool {
	for _, p := range h.trusted {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

func (c *Config) Dev() bool { return c.Env == EnvDevelopment }

// MetricsToken reads the metrics bearer token ("" when unset).
func (h *HTTP) MetricsToken() string {
	if h.MetricsTokenFile == "" {
		return ""
	}
	b, err := os.ReadFile(h.MetricsTokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *Config) PublicDir() string { return filepath.Join(c.Paths.Data, "public") }
func (c *Config) MediaDir() string  { return filepath.Join(c.Paths.Data, "media") }
func (c *Config) TmpDir() string    { return filepath.Join(c.Paths.Data, "tmp") }

// MediaBase returns the URL prefix media is served from.
func (c *Config) MediaBase() string {
	if c.MediaURL != "" {
		return strings.TrimRight(c.MediaURL, "/")
	}
	return strings.TrimRight(c.BaseURL, "/") + "/media"
}
