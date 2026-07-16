// Package config loads and validates the YAML config file. See
// config.example.yaml at the repo root for a documented example.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"jobwatch/internal/params"
)

// Config is the whole config file.
type Config struct {
	Poll struct {
		TimeoutSeconds int `yaml:"timeout_seconds"` // per-request HTTP timeout
		Concurrency    int `yaml:"concurrency"`     // companies fetched in parallel
	} `yaml:"poll"`

	Companies []Company `yaml:"companies"`

	// Matcher selects the matching algorithm; defaults to
	// {name: experience, params: {max_years: 1}}.
	Matcher Plugin `yaml:"matcher"`

	// Notifiers all receive every batch of matches; defaults to console.
	Notifiers []Plugin `yaml:"notifiers"`

	Store struct {
		Path string `yaml:"path"` // defaults to ~/.jobwatch/state.json
	} `yaml:"store"`
}

// Company is one job board to poll.
type Company struct {
	Name   string     `yaml:"name"`   // display name for notifications
	Source string     `yaml:"source"` // registered source type, e.g. "greenhouse"
	Params params.Map `yaml:"params"` // source-specific settings
}

// Plugin selects a registered matcher or notifier by name.
type Plugin struct {
	Name   string     `yaml:"name"`
	Params params.Map `yaml:"params"`
}

// Load reads, validates and applies defaults to the config at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // typos in keys become errors instead of silence
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	// Defaults.
	if cfg.Poll.TimeoutSeconds <= 0 {
		cfg.Poll.TimeoutSeconds = 20
	}
	if cfg.Poll.Concurrency <= 0 {
		cfg.Poll.Concurrency = 4
	}
	if cfg.Matcher.Name == "" {
		cfg.Matcher.Name = "experience"
	}
	if len(cfg.Notifiers) == 0 {
		cfg.Notifiers = []Plugin{{Name: "console"}}
	}
	if cfg.Store.Path == "" {
		cfg.Store.Path = "~/.jobwatch/state.json"
	}
	if p, err := expandHome(cfg.Store.Path); err == nil {
		cfg.Store.Path = p
	} else {
		return nil, err
	}

	// Validation.
	if len(cfg.Companies) == 0 {
		return nil, fmt.Errorf("%s: no companies configured", path)
	}
	for i, c := range cfg.Companies {
		if c.Name == "" {
			return nil, fmt.Errorf("%s: companies[%d]: missing name", path, i)
		}
		if c.Source == "" {
			return nil, fmt.Errorf("%s: companies[%d] (%s): missing source", path, i, c.Name)
		}
	}
	return &cfg, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		if strings.HasPrefix(path, "~") {
			return "", fmt.Errorf("store path %q: ~user expansion is not supported, use an absolute path", path)
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path[1:], "/")), nil
}
