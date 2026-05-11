package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Identifier matches PostgreSQL-ish identifiers used for pool names and
// resolved database values. The leading character must be a letter, digit, or
// underscore; subsequent characters may also include "$" and "-".
var identRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)

// ValidIdentifier reports whether s is a permitted pool/database identifier.
func ValidIdentifier(s string) bool {
	return identRE.MatchString(s)
}

type ForwardTarget struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type Target struct {
	Name string `yaml:"name"`

	// Inline endpoint (mutually exclusive with ForwardTo).
	Host string `yaml:"host,omitempty"`
	Port int    `yaml:"port,omitempty"`

	// Named reference to a forward_targets entry (mutually exclusive with Host/Port).
	ForwardTo string `yaml:"forward_to,omitempty"`

	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database,omitempty"`
}

type Pool struct {
	Name    string   `yaml:"name"`
	Default string   `yaml:"default"`
	Targets []Target `yaml:"targets"`
}

type Config struct {
	Port           int                      `yaml:"port"`
	ControlSocket  string                   `yaml:"control_socket"`
	ForwardTargets map[string]ForwardTarget `yaml:"forward_targets,omitempty"`
	Pools          []Pool                   `yaml:"pools"`
}

const DefaultControlSocket = "/tmp/db-pool-switch.sock"

// Load reads, parses, and validates a YAML config from path.
func Load(path string, logger *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ControlSocket == "" {
		cfg.ControlSocket = DefaultControlSocket
	}
	if err := Validate(&cfg, logger); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces the rules described in the plan. Non-fatal observations
// are logged as warnings via the provided logger (may be nil).
func Validate(cfg *Config, logger *slog.Logger) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port must be in [1, 65535], got %d", cfg.Port)
	}

	for name, ft := range cfg.ForwardTargets {
		if name == "" {
			return fmt.Errorf("forward_targets has an empty key")
		}
		if ft.Host == "" {
			return fmt.Errorf("forward_targets[%q].host is empty", name)
		}
		if ft.Port < 1 || ft.Port > 65535 {
			return fmt.Errorf("forward_targets[%q].port out of range: %d", name, ft.Port)
		}
	}

	if len(cfg.Pools) == 0 {
		return fmt.Errorf("at least one pool must be defined")
	}

	seenPools := make(map[string]struct{})
	for i := range cfg.Pools {
		p := &cfg.Pools[i]
		if p.Name == "" {
			return fmt.Errorf("pools[%d].name is empty", i)
		}
		if !ValidIdentifier(p.Name) {
			return fmt.Errorf("pool name %q is not a valid identifier", p.Name)
		}
		if _, dup := seenPools[p.Name]; dup {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seenPools[p.Name] = struct{}{}

		if len(p.Targets) == 0 {
			return fmt.Errorf("pool %q has no targets", p.Name)
		}

		seenTargets := make(map[string]struct{})
		var defaultTarget *Target
		for j := range p.Targets {
			t := &p.Targets[j]
			if t.Name == "" {
				return fmt.Errorf("pool %q targets[%d].name is empty", p.Name, j)
			}
			if _, dup := seenTargets[t.Name]; dup {
				return fmt.Errorf("pool %q has duplicate target name %q", p.Name, t.Name)
			}
			seenTargets[t.Name] = struct{}{}

			// XOR: inline (host+port) vs forward_to.
			inline := t.Host != "" || t.Port != 0
			forward := t.ForwardTo != ""
			if inline && forward {
				return fmt.Errorf("pool %q target %q: host/port and forward_to are mutually exclusive", p.Name, t.Name)
			}
			if !inline && !forward {
				return fmt.Errorf("pool %q target %q: either host+port or forward_to is required", p.Name, t.Name)
			}
			if inline {
				if t.Host == "" {
					return fmt.Errorf("pool %q target %q: host is required when using inline endpoint", p.Name, t.Name)
				}
				if t.Port < 1 || t.Port > 65535 {
					return fmt.Errorf("pool %q target %q: port out of range: %d", p.Name, t.Name, t.Port)
				}
			} else {
				if _, ok := cfg.ForwardTargets[t.ForwardTo]; !ok {
					return fmt.Errorf("pool %q target %q: forward_to %q not defined in forward_targets", p.Name, t.Name, t.ForwardTo)
				}
			}

			if t.User == "" {
				return fmt.Errorf("pool %q target %q: user is required", p.Name, t.Name)
			}
			if t.Password == "" {
				return fmt.Errorf("pool %q target %q: password is required", p.Name, t.Name)
			}

			if t.Name == p.Default {
				defaultTarget = t
			}
		}

		if defaultTarget == nil {
			return fmt.Errorf("pool %q: default target %q not found in targets", p.Name, p.Default)
		}

		if vars := RequiredVars(defaultTarget.Database); len(vars) > 0 {
			return fmt.Errorf("pool %q default target %q: database must not require variables (found %v)", p.Name, defaultTarget.Name, vars)
		}
		if defaultTarget.Database == "" {
			if logger != nil {
				logger.Warn("default target has empty database; client-supplied dbname will be passed through",
					"pool", p.Name, "target", defaultTarget.Name)
			}
		} else if !ValidIdentifier(defaultTarget.Database) {
			return fmt.Errorf("pool %q default target %q: resolved database %q contains invalid characters", p.Name, defaultTarget.Name, defaultTarget.Database)
		}

		// Sanity-check non-default targets whose database is a plain string
		// (no variables): must still be a valid identifier when present.
		for j := range p.Targets {
			t := &p.Targets[j]
			if t == defaultTarget {
				continue
			}
			if t.Database == "" {
				continue
			}
			if len(RequiredVars(t.Database)) == 0 {
				if !ValidIdentifier(t.Database) {
					return fmt.Errorf("pool %q target %q: database %q is not a valid identifier", p.Name, t.Name, t.Database)
				}
			}
		}
	}

	return nil
}

// ResolveEndpoint returns the inline host/port if set, otherwise resolves
// ForwardTo via cfg.ForwardTargets. Caller has already validated.
func (t *Target) ResolveEndpoint(fwd map[string]ForwardTarget) (host string, port int) {
	if t.Host != "" {
		return t.Host, t.Port
	}
	ft := fwd[t.ForwardTo]
	return ft.Host, ft.Port
}
