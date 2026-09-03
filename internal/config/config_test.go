package config

import (
	"strings"
	"testing"
)

const validYAML = `warpgate:
  host: https://warpgate.example.invalid
  token: token-placeholder
  insecure_skip_verify: true
inventory:
  groups: groups.csv
  targets: targets.csv
apply:
  update_passwords: true
  sync_roles: true
  sync_targets: true
`

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Warpgate.Host != "https://warpgate.example.invalid" || got.Warpgate.Token != "token-placeholder" || !got.Warpgate.InsecureSkipVerify {
		t.Fatalf("Warpgate = %#v", got.Warpgate)
	}
	if got.Inventory.Groups != "groups.csv" || got.Inventory.Targets != "targets.csv" || !got.Apply.UpdatePasswords || !got.Apply.SyncRoles || !got.Apply.SyncTargets {
		t.Fatalf("Config = %#v", got)
	}
}

func TestParseDefaultsSyncOptionsToFalse(t *testing.T) {
	got, err := Parse(strings.NewReader(`inventory:
  groups: groups.csv
  targets: targets.csv
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Apply.SyncRoles || got.Apply.SyncTargets {
		t.Fatalf("Apply = %#v", got.Apply)
	}
}

func TestParseAllowsMissingWarpgateConnectionForValidate(t *testing.T) {
	input := `inventory:
  groups: groups.csv
  targets: targets.csv
`
	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Warpgate.Token != "" {
		t.Fatalf("Token = %q", got.Warpgate.Token)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"empty":           {"", "empty"},
		"invalid yaml":    {"warpgate: [", "YAML"},
		"unknown field":   {validYAML + "unexpected: true\n", "field unexpected"},
		"nested unknown":  {strings.Replace(validYAML, "  host:", "  unexpected: true\n  host:", 1), "field unexpected"},
		"multiple docs":   {validYAML + "---\ninventory: {}\n", "one YAML document"},
		"missing groups":  {strings.Replace(validYAML, "  groups: groups.csv\n", "", 1), "inventory.groups"},
		"missing targets": {strings.Replace(validYAML, "  targets: targets.csv\n", "", 1), "inventory.targets"},
		"old token env":   {strings.Replace(validYAML, "  token: token-placeholder", "  token_env: WARPGATE_TOKEN", 1), "field token_env"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestResolvePaths(t *testing.T) {
	input := File{Inventory: Inventory{Groups: "../inventory/groups.csv", Targets: "/absolute/targets.csv"}}
	got := input.ResolvePaths("/workspace/config/warpgate-admin.yaml")
	if got.Inventory.Groups != "/workspace/inventory/groups.csv" || got.Inventory.Targets != "/absolute/targets.csv" {
		t.Fatalf("ResolvePaths() = %#v", got.Inventory)
	}
	if input.Inventory.Groups != "../inventory/groups.csv" {
		t.Fatalf("ResolvePaths() mutated input: %#v", input.Inventory)
	}
}
