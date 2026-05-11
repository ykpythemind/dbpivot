package config

import (
	"reflect"
	"testing"
)

func TestRequiredVars(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"app_dev", nil},
		{"app_${BRANCH}_staging", []string{"BRANCH"}},
		{"${A}_${B}_${A}_${C}", []string{"A", "B", "C"}},
		{"$$literal", nil},
		{"prefix_${UNCLOSED", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := RequiredVars(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("RequiredVars(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		got, missing, err := Resolve("app_${BRANCH}_staging", map[string]string{"BRANCH": "main"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(missing) != 0 {
			t.Fatalf("unexpected missing: %v", missing)
		}
		if got != "app_main_staging" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("multiple_substitutions", func(t *testing.T) {
		got, _, err := Resolve("${A}_${B}_${A}", map[string]string{"A": "x", "B": "y"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "x_y_x" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("missing_variables_listed", func(t *testing.T) {
		_, missing, err := Resolve("${A}_${B}_${C}", map[string]string{"A": "x"})
		if err == nil {
			t.Fatal("expected error")
		}
		want := []string{"B", "C"}
		if !reflect.DeepEqual(missing, want) {
			t.Errorf("missing = %v, want %v", missing, want)
		}
	})

	t.Run("extra_vars_allowed", func(t *testing.T) {
		got, _, err := Resolve("${A}", map[string]string{"A": "x", "UNUSED": "y"})
		if err != nil || got != "x" {
			t.Errorf("got=%q err=%v", got, err)
		}
	})

	t.Run("dollar_literal", func(t *testing.T) {
		got, _, err := Resolve("$$amount", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "$amount" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("unclosed_brace", func(t *testing.T) {
		_, _, err := Resolve("prefix_${BRANCH", nil)
		if err == nil {
			t.Error("expected error for unclosed brace")
		}
	})

	t.Run("bare_dollar_rejected", func(t *testing.T) {
		_, _, err := Resolve("$X", nil)
		if err == nil {
			t.Error("expected error for bare $")
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		got, _, err := Resolve("", nil)
		if err != nil || got != "" {
			t.Errorf("got=%q err=%v", got, err)
		}
	})
}
