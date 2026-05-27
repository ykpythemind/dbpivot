package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func baseCfg() *Config {
	return &Config{
		Port: 6432,
		ForwardTargets: map[string]ForwardTarget{
			"ssm-staging": {Host: "127.0.0.1", Port: 15432},
		},
		Databases: []Database{
			{
				Adapter:     AdapterPostgres,
				VirtualName: "appdb",
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

func TestValidate_SSLModeRequireOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Targets[1].SSLMode = SSLModeRequire
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("sslmode=require should be valid: %v", err)
	}
}

func TestValidate_MySQLAdapterOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Adapter = AdapterMySQL
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("all-mysql config should validate: %v", err)
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
		{"no_databases", func(c *Config) { c.Databases = nil }, "at least one database"},
		{"empty_virtual_name", func(c *Config) { c.Databases[0].VirtualName = "" }, "virtual_name is empty"},
		{"bad_virtual_name", func(c *Config) { c.Databases[0].VirtualName = "has space" }, "valid identifier"},
		{"duplicate_virtual_name", func(c *Config) {
			c.Databases = append(c.Databases, c.Databases[0])
		}, "duplicate virtual_name"},
		{"adapter_missing", func(c *Config) { c.Databases[0].Adapter = "" }, "adapter is required"},
		{"adapter_unsupported", func(c *Config) { c.Databases[0].Adapter = "mongodb" }, "unsupported adapter"},
		{"no_targets", func(c *Config) { c.Databases[0].Targets = nil }, "no targets"},
		{"duplicate_target", func(c *Config) {
			c.Databases[0].Targets = append(c.Databases[0].Targets, c.Databases[0].Targets[0])
		}, "duplicate target"},
		{"both_inline_and_forward", func(c *Config) {
			c.Databases[0].Targets[0].ForwardTo = "ssm-staging"
		}, "mutually exclusive"},
		{"neither_inline_nor_forward", func(c *Config) {
			c.Databases[0].Targets[0].Host = ""
			c.Databases[0].Targets[0].Port = 0
		}, "host+port or forward_to is required"},
		{"inline_missing_port", func(c *Config) { c.Databases[0].Targets[0].Port = 0 }, "port out of range"},
		{"forward_to_undefined", func(c *Config) { c.Databases[0].Targets[1].ForwardTo = "missing" }, "not defined"},
		{"target_user_missing", func(c *Config) { c.Databases[0].Targets[0].User = "" }, "user is required"},
		{"target_password_missing", func(c *Config) { c.Databases[0].Targets[0].Password = "" }, "password is required"},
		{"plain_target_database_invalid", func(c *Config) {
			c.Databases[0].Targets[0].Database = "bad name"
		}, "not a valid identifier"},
		{"forward_target_bad_port", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "h", Port: 0}
		}, "port out of range"},
		{"forward_target_no_host", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "", Port: 1}
		}, "host is empty"},
		{"sslmode_unsupported", func(c *Config) {
			c.Databases[0].Targets[1].SSLMode = "verify-full"
		}, "unsupported sslmode"},
		{"mixed_adapters", func(c *Config) {
			second := c.Databases[0]
			second.VirtualName = "other"
			second.Adapter = AdapterMySQL
			c.Databases = append(c.Databases, second)
		}, "must share one adapter"},
		{"mysql_sslmode_require_unsupported", func(c *Config) {
			c.Databases[0].Adapter = AdapterMySQL
			c.Databases[0].Targets[1].SSLMode = SSLModeRequire
		}, "not yet supported for the mysql adapter"},
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

func TestValidate_EmptyTargetDatabaseOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Targets[0].Database = ""
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("empty target database should validate (with warn), got: %v", err)
	}
}

// TestValidate_TargetSetMismatchWarnsOnly verifies that databases with
// different target sets validate (no error) and that a warning is emitted
// instead. This is the "DB not ready yet" use case.
func TestValidate_TargetSetMismatchWarnsOnly(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases = append(cfg.Databases, Database{
		Adapter:     AdapterPostgres,
		VirtualName: "analytics",
		Targets: []Target{
			{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "an_dev"},
			// missing "staging" — different target set
		},
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := Validate(cfg, logger); err != nil {
		t.Fatalf("target set mismatch should now warn, not error: %v", err)
	}
	if !strings.Contains(buf.String(), "different target set") {
		t.Errorf("expected warning about differing target sets, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "analytics") {
		t.Errorf("expected warning to mention the offending database, got: %q", buf.String())
	}
}

func TestTargetNames(t *testing.T) {
	cfg := baseCfg()
	got := cfg.TargetNames()
	if len(got) != 2 || got[0] != "local" || got[1] != "staging" {
		t.Errorf("TargetNames = %v", got)
	}
	if !cfg.HasTarget("local") || !cfg.HasTarget("staging") || cfg.HasTarget("prod") {
		t.Errorf("HasTarget logic wrong")
	}
}

func TestResolveEndpoint(t *testing.T) {
	cfg := baseCfg()
	host, port := cfg.Databases[0].Targets[0].ResolveEndpoint(cfg.ForwardTargets)
	if host != "127.0.0.1" || port != 5432 {
		t.Errorf("inline: got %s:%d", host, port)
	}
	host, port = cfg.Databases[0].Targets[1].ResolveEndpoint(cfg.ForwardTargets)
	if host != "127.0.0.1" || port != 15432 {
		t.Errorf("forward_to: got %s:%d", host, port)
	}
}
