// Package config loads and validates the portal's YAML configuration file.
package config

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

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
}

// LoginConfig holds settings for calls to Rancher.
type LoginConfig struct {
	// Timeout applies to each request to a Rancher site.
	Timeout time.Duration `yaml:"timeout"`
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
}

// APIConfig holds settings for Rancher/cluster API calls after login.
type APIConfig struct {
	// Timeout applies to each such request.
	Timeout time.Duration `yaml:"timeout"`
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
	if c.Login.Timeout == 0 {
		c.Login.Timeout = 15 * time.Second
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
	if c.Session.IdleTimeout > c.Session.AbsoluteTimeout {
		return errors.New("session: idle_timeout must not exceed absolute_timeout")
	}
	if c.Clusters.CacheTTL < 0 {
		return errors.New("clusters.cache_ttl must be positive")
	}
	if c.API.Timeout < 0 {
		return errors.New("api.timeout must be positive")
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
