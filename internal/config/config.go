package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Identifier matches PostgreSQL-ish identifiers used for virtual_name and
// resolved database values. The leading character must be a letter, digit, or
// underscore; subsequent characters may also include "$" and "-".
var identRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)

// ValidIdentifier reports whether s is a permitted virtual_name / database identifier.
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

type Database struct {
	VirtualName string   `yaml:"virtual_name"`
	Targets     []Target `yaml:"targets"`
}

type Config struct {
	Port           int                      `yaml:"port"`
	ControlSocket  string                   `yaml:"control_socket"`
	ForwardTargets map[string]ForwardTarget `yaml:"forward_targets,omitempty"`
	Databases      []Database               `yaml:"databases"`
}

const DefaultControlSocket = "/tmp/dbpivot.sock"

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

// Validate enforces the configuration rules. Non-fatal observations are
// logged as warnings via the provided logger (may be nil).
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

	if len(cfg.Databases) == 0 {
		return fmt.Errorf("at least one database must be defined")
	}

	seenDatabases := make(map[string]struct{})
	var canonicalTargets []string // sorted target names from the first database
	for i := range cfg.Databases {
		d := &cfg.Databases[i]
		if d.VirtualName == "" {
			return fmt.Errorf("databases[%d].virtual_name is empty", i)
		}
		if !ValidIdentifier(d.VirtualName) {
			return fmt.Errorf("virtual_name %q is not a valid identifier", d.VirtualName)
		}
		if _, dup := seenDatabases[d.VirtualName]; dup {
			return fmt.Errorf("duplicate virtual_name %q", d.VirtualName)
		}
		seenDatabases[d.VirtualName] = struct{}{}

		if len(d.Targets) == 0 {
			return fmt.Errorf("database %q has no targets", d.VirtualName)
		}

		seenTargets := make(map[string]struct{})
		for j := range d.Targets {
			t := &d.Targets[j]
			if t.Name == "" {
				return fmt.Errorf("database %q targets[%d].name is empty", d.VirtualName, j)
			}
			if _, dup := seenTargets[t.Name]; dup {
				return fmt.Errorf("database %q has duplicate target name %q", d.VirtualName, t.Name)
			}
			seenTargets[t.Name] = struct{}{}

			// XOR: inline (host+port) vs forward_to.
			inline := t.Host != "" || t.Port != 0
			forward := t.ForwardTo != ""
			if inline && forward {
				return fmt.Errorf("database %q target %q: host/port and forward_to are mutually exclusive", d.VirtualName, t.Name)
			}
			if !inline && !forward {
				return fmt.Errorf("database %q target %q: either host+port or forward_to is required", d.VirtualName, t.Name)
			}
			if inline {
				if t.Host == "" {
					return fmt.Errorf("database %q target %q: host is required when using inline endpoint", d.VirtualName, t.Name)
				}
				if t.Port < 1 || t.Port > 65535 {
					return fmt.Errorf("database %q target %q: port out of range: %d", d.VirtualName, t.Name, t.Port)
				}
			} else {
				if _, ok := cfg.ForwardTargets[t.ForwardTo]; !ok {
					return fmt.Errorf("database %q target %q: forward_to %q not defined in forward_targets", d.VirtualName, t.Name, t.ForwardTo)
				}
			}

			if t.User == "" {
				return fmt.Errorf("database %q target %q: user is required", d.VirtualName, t.Name)
			}
			if t.Password == "" {
				return fmt.Errorf("database %q target %q: password is required", d.VirtualName, t.Name)
			}

			// Plain (no-variable) target.database must still be a valid identifier.
			if t.Database != "" && len(RequiredVars(t.Database)) == 0 && !ValidIdentifier(t.Database) {
				return fmt.Errorf("database %q target %q: database %q is not a valid identifier", d.VirtualName, t.Name, t.Database)
			}
			if t.Database == "" && logger != nil {
				logger.Warn("target has empty database; client-supplied dbname will be passed through",
					"virtual_name", d.VirtualName, "target", t.Name)
			}
		}

		// Cross-database target-set consistency: every database must declare
		// the exact same set of target names so that `switch <target>` applies
		// uniformly across all databases.
		names := sortedTargetNames(d.Targets)
		if i == 0 {
			canonicalTargets = names
		} else if !equalStringSlices(names, canonicalTargets) {
			return fmt.Errorf("database %q has targets %v but first database has %v; all databases must declare the same target set",
				d.VirtualName, names, canonicalTargets)
		}
	}

	return nil
}

func sortedTargetNames(targets []Target) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Name
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TargetNames returns the sorted set of target names declared by the first
// database. Because Validate enforces all databases share the same set, this
// is the canonical list. Returns nil if cfg has no databases.
func (cfg *Config) TargetNames() []string {
	if len(cfg.Databases) == 0 {
		return nil
	}
	return sortedTargetNames(cfg.Databases[0].Targets)
}

// HasTarget reports whether name is one of the declared target names.
func (cfg *Config) HasTarget(name string) bool {
	for _, t := range cfg.Databases[0].Targets {
		if t.Name == name {
			return true
		}
	}
	return false
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

