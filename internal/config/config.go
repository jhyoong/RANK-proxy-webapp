// Package config loads and validates the portal's YAML configuration file.
package config

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Config is the whole portal configuration.
type Config struct {
	Listen       string         `yaml:"listen"`
	CookieSecure *bool          `yaml:"cookie_secure"`
	TLS          TLSConfig      `yaml:"tls"`
	Session      SessionConfig  `yaml:"session"`
	Login        LoginConfig    `yaml:"login"`
	Kagent       KagentConfig   `yaml:"kagent"`
	Clusters     ClustersConfig `yaml:"clusters"`
	API          APIConfig      `yaml:"api"`
	Chat         ChatConfig     `yaml:"chat"`
	UI           UIConfig       `yaml:"ui"`
	// Sites are tried in order at login; Sites[0] is "site 1".
	Sites []Site `yaml:"sites"`
}

// TLSConfig enables HTTPS on the portal itself. Both empty means plain HTTP.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// SessionConfig holds the portal session timeouts.
type SessionConfig struct {
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	AbsoluteTimeout time.Duration `yaml:"absolute_timeout"`
	// MaxPerUser caps live sessions per user; the oldest is ended when
	// exceeded. Default 5.
	MaxPerUser int `yaml:"max_per_user"`
}

// LoginConfig holds settings for calls to Rancher.
type LoginConfig struct {
	// Timeout applies to each request to a Rancher site.
	Timeout time.Duration `yaml:"timeout"`
	// MaxAttemptsPerUser and MaxAttemptsPerIP cap login attempts per
	// username and per client IP within AttemptWindow.
	MaxAttemptsPerUser int           `yaml:"max_attempts_per_user"`
	MaxAttemptsPerIP   int           `yaml:"max_attempts_per_ip"`
	AttemptWindow      time.Duration `yaml:"attempt_window"`
	// TrustedProxies are CIDRs whose X-Forwarded-For header is believed.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// KagentConfig locates kagent and its agent inside each downstream cluster.
// All four values become URL path segments.
type KagentConfig struct {
	Namespace      string `yaml:"namespace"`
	Service        string `yaml:"service"`
	AgentNamespace string `yaml:"agent_namespace"`
	AgentName      string `yaml:"agent_name"`
}

// ClustersConfig holds cluster list settings.
type ClustersConfig struct {
	// CacheTTL is how long a session's cluster list is cached.
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// IncludeLocal lists each site's "local" management cluster. Off by
	// default; turn on only where kagent is deployed in the local cluster.
	IncludeLocal bool `yaml:"include_local"`
}

// APIConfig holds settings for Rancher/cluster API calls after login.
type APIConfig struct {
	// Timeout applies to each such request.
	Timeout time.Duration `yaml:"timeout"`
}

// ChatConfig holds settings for chat replies streamed from kagent.
type ChatConfig struct {
	// MaxDuration is the total cap per reply.
	MaxDuration time.Duration `yaml:"max_duration"`
	// IdleTimeout caps the silence between upstream events.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// MaxMessageBytes is the longest message a user can send.
	MaxMessageBytes int `yaml:"max_message_bytes"`
	// Heartbeat is how often a comment line goes to the browser while upstream is silent.
	Heartbeat time.Duration `yaml:"heartbeat"`
	// LogEvents logs the shape (never content) of every upstream event.
	LogEvents bool `yaml:"log_events"`
}

// UIConfig holds optional end-user text shown by the web UI. Empty means the
// element is not shown.
type UIConfig struct {
	// Banner is a one-line announcement at the top of the login page.
	Banner string `yaml:"banner"`
	// BannerLevel styles the banner: "info" (default) or "warning".
	BannerLevel string `yaml:"banner_level"`
	// LoginHelp is a help box on the login page; blank lines separate paragraphs.
	LoginHelp string `yaml:"login_help"`
	// SupportEmail is a bare address shown as a mailto: link.
	SupportEmail string `yaml:"support_email"`
}

// Site is one Rancher site.
type Site struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// CAFile is a PEM bundle used to verify the site. Empty means system roots.
	CAFile string `yaml:"ca_file"`
}

// SecureCookies reports whether the session cookie gets the Secure attribute.
func (c *Config) SecureCookies() bool {
	return c.CookieSecure == nil || *c.CookieSecure
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Session.IdleTimeout == 0 {
		c.Session.IdleTimeout = 30 * time.Minute
	}
	if c.Session.AbsoluteTimeout == 0 {
		c.Session.AbsoluteTimeout = 8 * time.Hour
	}
	if c.Session.MaxPerUser == 0 {
		c.Session.MaxPerUser = 5
	}
	if c.Login.Timeout == 0 {
		c.Login.Timeout = 15 * time.Second
	}
	if c.Login.MaxAttemptsPerUser == 0 {
		c.Login.MaxAttemptsPerUser = 3
	}
	if c.Login.MaxAttemptsPerIP == 0 {
		c.Login.MaxAttemptsPerIP = 20
	}
	if c.Login.AttemptWindow == 0 {
		c.Login.AttemptWindow = 15 * time.Minute
	}
	k := &c.Kagent
	for _, f := range []struct {
		v *string
		d string
	}{
		{&k.Namespace, "kagent"},
		{&k.Service, "http:kagent-controller:8083"},
		{&k.AgentNamespace, "kagent"},
	} {
		if *f.v == "" {
			*f.v = f.d
		}
	}
	if c.Clusters.CacheTTL == 0 {
		c.Clusters.CacheTTL = 2 * time.Minute
	}
	if c.API.Timeout == 0 {
		c.API.Timeout = 15 * time.Second
	}
	if c.Chat.MaxDuration == 0 {
		c.Chat.MaxDuration = 30 * time.Minute
	}
	if c.Chat.IdleTimeout == 0 {
		c.Chat.IdleTimeout = 10 * time.Minute
	}
	if c.Chat.MaxMessageBytes == 0 {
		c.Chat.MaxMessageBytes = 8192
	}
	if c.Chat.Heartbeat == 0 {
		c.Chat.Heartbeat = 15 * time.Second
	}
	if c.UI.BannerLevel == "" {
		c.UI.BannerLevel = "info"
	}
}

func (c *Config) validate() error {
	if len(c.Sites) == 0 {
		return errors.New("sites: at least one site is required")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("tls: set both cert_file and key_file, or neither")
	}
	if c.Session.IdleTimeout < 0 || c.Session.AbsoluteTimeout < 0 || c.Login.Timeout < 0 {
		return errors.New("session/login: timeouts must be positive")
	}
	if c.Session.MaxPerUser < 0 {
		return errors.New("session.max_per_user must not be negative")
	}
	if c.Login.MaxAttemptsPerUser < 0 || c.Login.MaxAttemptsPerIP < 0 || c.Login.AttemptWindow < 0 {
		return errors.New("login: max_attempts_per_user, max_attempts_per_ip and attempt_window must not be negative")
	}
	for _, p := range c.Login.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			return fmt.Errorf("login.trusted_proxies: %q: %w", p, err)
		}
	}
	if c.Session.IdleTimeout > c.Session.AbsoluteTimeout {
		return errors.New("session: idle_timeout must not exceed absolute_timeout")
	}
	if c.Clusters.CacheTTL < 0 {
		return errors.New("clusters.cache_ttl must be positive")
	}
	if c.API.Timeout < 0 {
		return errors.New("api.timeout must be positive")
	}
	if c.Chat.MaxDuration < 0 {
		return errors.New("chat.max_duration must be positive")
	}
	if c.Chat.IdleTimeout < 0 {
		return errors.New("chat.idle_timeout must be positive")
	}
	if c.Chat.Heartbeat < 0 {
		return errors.New("chat.heartbeat must be positive")
	}
	if c.Chat.MaxMessageBytes < 1 || c.Chat.MaxMessageBytes > 65536 {
		return errors.New("chat.max_message_bytes must be between 1 and 65536")
	}
	if c.Chat.IdleTimeout > c.Chat.MaxDuration {
		return errors.New("chat: idle_timeout must not exceed max_duration")
	}
	if c.UI.BannerLevel != "info" && c.UI.BannerLevel != "warning" {
		return fmt.Errorf("ui.banner_level %q must be info or warning", c.UI.BannerLevel)
	}
	if utf8.RuneCountInString(c.UI.Banner) > 300 {
		return errors.New("ui.banner must be at most 300 characters")
	}
	if strings.ContainsAny(c.UI.Banner, "\r\n") {
		return errors.New("ui.banner must be a single line")
	}
	if utf8.RuneCountInString(c.UI.LoginHelp) > 2000 {
		return errors.New("ui.login_help must be at most 2000 characters")
	}
	if e := c.UI.SupportEmail; e != "" {
		addr, err := mail.ParseAddress(e)
		if err != nil || addr.Name != "" || addr.Address != e {
			return fmt.Errorf("ui.support_email %q must be a bare email address", e)
		}
	}
	for _, f := range []struct{ name, v string }{
		{"namespace", c.Kagent.Namespace},
		{"service", c.Kagent.Service},
		{"agent_namespace", c.Kagent.AgentNamespace},
		{"agent_name", c.Kagent.AgentName},
	} {
		if strings.ContainsAny(f.v, "/?#") {
			return fmt.Errorf("kagent.%s %q must not contain '/', '?' or '#'", f.name, f.v)
		}
	}
	seen := map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		if s.Name == "" {
			return fmt.Errorf("sites[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("sites[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("sites[%d] %s: url %q is not a valid URL", i, s.Name, s.URL)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("sites[%d] %s: url must use https", i, s.Name)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("sites[%d] %s: url must not have a path, query or fragment", i, s.Name)
		}
		s.URL = strings.TrimSuffix(s.URL, "/")
		if s.CAFile != "" {
			pem, err := os.ReadFile(s.CAFile)
			if err != nil {
				return fmt.Errorf("sites[%d] %s: ca_file: %w", i, s.Name, err)
			}
			if !x509.NewCertPool().AppendCertsFromPEM(pem) {
				return fmt.Errorf("sites[%d] %s: ca_file %s contains no PEM certificate", i, s.Name, s.CAFile)
			}
		}
	}
	if c.Kagent.AgentName == "" {
		return errors.New("kagent.agent_name is required: set it to the name of the deployed kagent agent")
	}
	return nil
}
