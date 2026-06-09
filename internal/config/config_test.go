package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func baseCfg() *Config {
	return &Config{
		ListenPorts: map[string]int{AdapterPostgres: 6432},
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
	cfg.ListenPorts = map[string]int{AdapterMySQL: 3306}
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("all-mysql config should validate: %v", err)
	}
}

func TestValidate_MongoAdapterOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Adapter = AdapterMongo
	cfg.ListenPorts = map[string]int{AdapterMongo: 27017}
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("all-mongo config should validate: %v", err)
	}
}

func TestValidate_MongoAuthSourceOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Adapter = AdapterMongo
	cfg.ListenPorts = map[string]int{AdapterMongo: 27017}
	cfg.Databases[0].Targets[0].AuthSource = "admin"
	cfg.Databases[0].Targets[1].AuthSource = "app_users"
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("mongo auth_source should validate: %v", err)
	}
}

// TestValidate_AuthSourceWarnsOnNonMongo verifies auth_source is accepted but
// flagged as ignored when set on a non-mongodb database.
func TestValidate_AuthSourceWarnsOnNonMongo(t *testing.T) {
	cfg := baseCfg() // postgres adapter
	cfg.Databases[0].Targets[0].AuthSource = "admin"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := Validate(cfg, logger); err != nil {
		t.Fatalf("auth_source on a postgres db should warn, not error: %v", err)
	}
	if !strings.Contains(buf.String(), "auth_source is only used by the mongodb adapter") {
		t.Errorf("expected warning about ignored auth_source, got: %q", buf.String())
	}
}

func TestValidate_MySQLSSLModeRequireOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Databases[0].Adapter = AdapterMySQL
	cfg.ListenPorts = map[string]int{AdapterMySQL: 3306}
	cfg.Databases[0].Targets[1].SSLMode = SSLModeRequire
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("mysql sslmode=require should validate: %v", err)
	}
}

// TestValidate_MixedAdaptersOK verifies that databases on different adapters now
// coexist in one config, provided each adapter has a listen port.
func TestValidate_MixedAdaptersOK(t *testing.T) {
	cfg := baseCfg()
	cfg.ListenPorts = map[string]int{AdapterPostgres: 6432, AdapterMySQL: 3306}
	cfg.Databases = append(cfg.Databases, Database{
		Adapter:     AdapterMySQL,
		VirtualName: "mydb",
		Targets: []Target{
			{Name: "local", Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Database: "app_dev"},
			{Name: "staging", ForwardTo: "ssm-staging", User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
		},
	})
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("mixed adapters with both ports should validate: %v", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		errPart string
	}{
		{"no_listen_ports", func(c *Config) { c.ListenPorts = nil }, "listen_ports must define"},
		{"listen_port_zero", func(c *Config) { c.ListenPorts[AdapterPostgres] = 0 }, "must be in"},
		{"listen_port_too_large", func(c *Config) { c.ListenPorts[AdapterPostgres] = 70000 }, "must be in"},
		{"listen_port_unsupported_adapter", func(c *Config) { c.ListenPorts["oracle"] = 1521 }, "unsupported adapter"},
		{"listen_port_duplicate", func(c *Config) { c.ListenPorts[AdapterMySQL] = 6432 }, "both use port"},
		{"adapter_no_listen_port", func(c *Config) {
			second := c.Databases[0]
			second.VirtualName = "other"
			second.Adapter = AdapterMySQL
			c.Databases = append(c.Databases, second)
		}, "no listen_ports entry"},
		{"no_databases", func(c *Config) { c.Databases = nil }, "at least one database"},
		{"empty_virtual_name", func(c *Config) { c.Databases[0].VirtualName = "" }, "virtual_name is empty"},
		{"bad_virtual_name", func(c *Config) { c.Databases[0].VirtualName = "has space" }, "valid identifier"},
		{"duplicate_virtual_name", func(c *Config) {
			c.Databases = append(c.Databases, c.Databases[0])
		}, "duplicate virtual_name"},
		{"adapter_missing", func(c *Config) { c.Databases[0].Adapter = "" }, "adapter is required"},
		{"adapter_unsupported", func(c *Config) { c.Databases[0].Adapter = "oracle" }, "unsupported adapter"},
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
		{"auth_source_invalid", func(c *Config) {
			c.Databases[0].Targets[0].AuthSource = "bad source"
		}, "auth_source"},
		{"forward_target_bad_port", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "h", Port: 0}
		}, "port out of range"},
		{"forward_target_no_host", func(c *Config) {
			c.ForwardTargets["x"] = ForwardTarget{Host: "", Port: 1}
		}, "host is empty"},
		{"sslmode_unsupported", func(c *Config) {
			c.Databases[0].Targets[1].SSLMode = "verify-full"
		}, "unsupported sslmode"},
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
