// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)


type RateLimit struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
}


type CircuitBreaker struct {
	FailureThreshold float64 `yaml:"failure_threshold"`
	WindowSeconds    int     `yaml:"window_seconds"`
	CooldownSeconds  int     `yaml:"cooldown_seconds"`
}


type Route struct {
	PathPrefix     string          `yaml:"path_prefix"`
	Backends       []string        `yaml:"backends"`
	RateLimit      *RateLimit      `yaml:"rate_limit,omitempty"`
	CircuitBreaker *CircuitBreaker `yaml:"circuit_breaker,omitempty"`
}


type Config struct {
	Routes []Route `yaml:"routes"`
}


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

		if rl := r.RateLimit; rl != nil {
			if rl.RequestsPerSecond <= 0 {
				return fmt.Errorf("route %d (%s): rate_limit.requests_per_second must be > 0", i, r.PathPrefix)
			}
			if rl.Burst < 1 {
				return fmt.Errorf("route %d (%s): rate_limit.burst must be >= 1", i, r.PathPrefix)
			}
		}

		if cb := r.CircuitBreaker; cb != nil {
			if cb.FailureThreshold <= 0 || cb.FailureThreshold > 1 {
				return fmt.Errorf("route %d (%s): circuit_breaker.failure_threshold must be in (0, 1]", i, r.PathPrefix)
			}
			if cb.WindowSeconds < 1 {
				return fmt.Errorf("route %d (%s): circuit_breaker.window_seconds must be >= 1", i, r.PathPrefix)
			}
			if cb.CooldownSeconds < 1 {
				return fmt.Errorf("route %d (%s): circuit_breaker.cooldown_seconds must be >= 1", i, r.PathPrefix)
			}
		}
	}

	return nil
}
