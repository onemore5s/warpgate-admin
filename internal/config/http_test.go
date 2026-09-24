package config

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func TestNamedInventories(t *testing.T) {
	for _, input := range []string{
		"inventory:\n  groups: groups.csv\n  database_targets: database.csv\n  http_targets: http.yaml\napply:\n  sync_database_targets: true\n  sync_http_targets: true\n",
	} {
		got, err := Parse(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		got = got.ResolvePaths("/workspace/test/config.yaml")
		if got.Inventory.DatabaseTargets != "/workspace/test/database.csv" || got.Inventory.HTTPTargets != "/workspace/test/http.yaml" || !got.Apply.SyncDatabaseTargets || !got.Apply.SyncHTTPTargets {
			t.Fatalf("Configuration was not normalized correctly: %+v", got)
		}
	}
	got, err := Parse(strings.NewReader("inventory:\n  groups: groups.csv\n  http_targets: http.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ResolvePaths("/workspace/test/config.yaml").Inventory.DatabaseTargets != "" {
		t.Fatal("Unconfigured inventory paths must remain empty")
	}
}

func TestConfigReadAndTrailingYAMLErrors(t *testing.T) {
	if _, err := Parse(iotest.ErrReader(errors.New("read failed"))); err == nil {
		t.Fatal("Expected a read error")
	}
	if _, err := Parse(strings.NewReader(validYAML + "---\ninvalid: [")); err == nil {
		t.Fatal("Expected malformed trailing YAML to be rejected")
	}
}

func TestRejectRemovedConfigNames(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"inventory:\n  groups: groups.csv\n  targets: old.csv\n", "field targets"},
		{"inventory:\n  groups: groups.csv\n  database_targets: database.csv\n  targets: ''\n", "field targets"},
		{"inventory:\n  groups: groups.csv\n  <<: {targets: old.csv}\n  database_targets: database.csv\n", "field targets"},
		{"inventory:\n  groups: groups.csv\n  database_targets: database.csv\napply:\n  sync_targets: false\n", "field sync_targets"},
		{"inventory:\n  groups: groups.csv\n  database_targets: database.csv\napply:\n  sync_database_targets: false\n  sync_targets: false\n", "field sync_targets"},
	} {
		_, err := Parse(strings.NewReader(tt.input))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("Parse() error = %v, want %q", err, tt.want)
		}
	}
}
