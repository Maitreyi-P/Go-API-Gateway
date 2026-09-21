// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Route defines a single routing rule: requests whose path starts with
// PathPrefix are forwarded to one of Backends (round-robin if more than
// one is configured).
//
// The YAML schema also supports rate_limit and circuit_breaker keys per
// route (see PRD section 6); those are intentionally not modeled here yet
// and are ignored during parsing rather than rejected, since they'll be
// added in a later phase.
type Route struct {
	PathPrefix string   `yaml:"path_prefix"`
	Backends   []string `yaml:"backends"`
}

// Config is the top-level gateway configuration.
type Config struct {
	Routes []Route `yaml:"routes"`
}

// Load reads the YAML config file at path, parses it, and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return &cfg, nil
}

// Validate checks that every route has a usable, unique path prefix and at
// least one valid backend URL.
func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("no routes defined")
	}

	seen := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		if r.PathPrefix == "" {
			return fmt.Errorf("route %d: path_prefix is required", i)
		}
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route %d: path_prefix %q must start with \"/\"", i, r.PathPrefix)
		}
		if seen[r.PathPrefix] {
			return fmt.Errorf("route %d: duplicate path_prefix %q", i, r.PathPrefix)
		}
		seen[r.PathPrefix] = true

		if len(r.Backends) == 0 {
			return fmt.Errorf("route %d (%s): at least one backend is required", i, r.PathPrefix)
		}
		for j, b := range r.Backends {
			u, err := url.Parse(b)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("route %d (%s): backend %d %q is not a valid absolute URL", i, r.PathPrefix, j, b)
			}
		}
	}

	return nil
}
