package config

import (
	"strings"
	"testing"
)

func baseCfg() *Config {
	return &Config{
		Port: 6432,
		ForwardTargets: map[string]ForwardTarget{
			"ssm-staging": {Host: "127.0.0.1", Port: 15432},
		},
		Pools: []Pool{
			{
				Name:    "appdb",
				Default: "local",
				Targets: []Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "postgres", Password: "pass", Database: "app_dev"},
					{Name: "staging", ForwardTo: "ssm-staging", User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
}

func TestValidate_Ok(t *testing.T) {
	if err := Validate(baseCfg(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		errPart string
	}{
		{"port_zero", func(c *Config) { c.Port = 0 }, "port must be in"},
		{"port_too_large", func(c *Config) { c.Port = 70000 }, "port must be in"},
		{"no_pools", func(c *Config) { c.Pools = nil }, "at least one pool"},
		{"empty_pool_name", func(c *Config) { c.Pools[0].Name = "" }, "name is empty"},
		{"bad_pool_name", func(c *Config) { c.Pools[0].Name = "has space" }, "valid identifier"},
		{"duplicate_pool", func(c *Config) {
			c.Pools = append(c.Pools, c.Pools[0])
		}, "duplicate pool"},
		{"no_targets", func(c *Config) { c.Pools[0].Targets = nil }, "no targets"},
		{"duplicate_target", func(c *Config) {
			c.Pools[0].Targets = append(c.Pools[0].Targets, c.Pools[0].Targets[0])
		}, "duplicate target"},
		{"both_inline_and_forward", func(c *Config) {
			c.Pools[0].Targets[0].ForwardTo = "ssm-staging"
		}, "mutually exclusive"},
		{"neither_inline_nor_forward", func(c *Config) {
			c.Pools[0].Targets[0].Host = ""
			c.Pools[0].Targets[0].Port = 0
		}, "host+port or forward_to is required"},
		{"inline_missing_port", func(c *Config) { c.Pools[0].Targets[0].Port = 0 }, "port out of range"},
		{"forward_to_undefined", func(c *Config) { c.Pools[0].Targets[1].ForwardTo = "missing" }, "not defined"},
		{"target_user_missing", func(c *Config) { c.Pools[0].Targets[0].User = "" }, "user is required"},
		{"target_password_missing", func(c *Config) { c.Pools[0].Targets[0].Password = "" }, "password is required"},
		{"default_not_in_targets", func(c *Config) { c.Pools[0].Default = "nope" }, "default target"},
		{"default_database_has_variable", func(c *Config) {
			c.Pools[0].Targets[0].Database = "app_${BRANCH}_dev"
		}, "must not require variables"},
		{"default_database_invalid_chars", func(c *Config) {
			c.Pools[0].Targets[0].Database = "bad name"
		}, "invalid characters"},
		{"plain_nondefault_database_invalid", func(c *Config) {
			c.Pools[0].Targets[1].Database = "bad name"
		}, "not a valid identifier"},
		{"forward_target_bad_port", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "h", Port: 0}
		}, "port out of range"},
		{"forward_target_no_host", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "", Port: 1}
		}, "host is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseCfg()
			c.mutate(cfg)
			err := Validate(cfg, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.errPart) {
				t.Errorf("error %q does not contain %q", err.Error(), c.errPart)
			}
		})
	}
}

func TestValidate_DefaultEmptyDatabase_OK(t *testing.T) {
	cfg := baseCfg()
	cfg.Pools[0].Targets[0].Database = ""
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("empty default database should validate (with warn), got: %v", err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	cfg := baseCfg()
	host, port := cfg.Pools[0].Targets[0].ResolveEndpoint(cfg.ForwardTargets)
	if host != "127.0.0.1" || port != 5432 {
		t.Errorf("inline: got %s:%d", host, port)
	}
	host, port = cfg.Pools[0].Targets[1].ResolveEndpoint(cfg.ForwardTargets)
	if host != "127.0.0.1" || port != 15432 {
		t.Errorf("forward_to: got %s:%d", host, port)
	}
}
