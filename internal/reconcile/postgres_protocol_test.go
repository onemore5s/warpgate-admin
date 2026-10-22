package reconcile

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

func TestApplyPostgresProtocolVersion(t *testing.T) {
	for _, tt := range []struct {
		name       string
		create     bool
		configured string
		current    string
		want       string
	}{
		{name: "create default", create: true, want: "3.2"},
		{name: "create explicit 3.0", create: true, configured: "3.0", want: "3.0"},
		{name: "create explicit 3.2", create: true, configured: "3.2", want: "3.2"},
		{name: "preserve 3.0", current: `"3.0"`, want: "3.0"},
		{name: "preserve 3.2", current: `"3.2"`, want: "3.2"},
		{name: "legacy missing", want: "3.2"},
		{name: "legacy null", current: "null", want: "3.2"},
		{name: "legacy empty", current: `""`, want: "3.2"},
		{name: "explicit upgrade", configured: "3.2", current: `"3.0"`, want: "3.2"},
		{name: "explicit downgrade", configured: "3.0", current: `"3.2"`, want: "3.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := postgresInventoryTarget("postgres-a", "environment-a")
			target.ProtocolVersion = tt.configured
			target.Description = "Updated target"
			input := inventory.Inventory{Groups: []inventory.Group{{Name: target.Group}}, Targets: []inventory.Target{target}}
			api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: target.Group}}}
			if !tt.create {
				api.targets = []warpgate.Target{postgresTargetWithProtocol(t, tt.current)}
			}
			if _, err := New(api).Apply(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			requests := api.updatedTargets
			if tt.create {
				requests = api.createdTargets
			}
			if len(requests) != 1 {
				t.Fatalf("write requests = %d, want 1", len(requests))
			}
			encoded, err := json.Marshal(requests[0])
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Options map[string]json.RawMessage `json:"options"`
			}
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			if got := string(body.Options["protocol_version"]); got != `"`+tt.want+`"` {
				t.Errorf("protocol_version = %s, want explicit string %q", got, tt.want)
			}
			if !tt.create && string(body.Options["idle_timeout"]) != `"37s"` {
				t.Errorf("idle_timeout = %s, want preserved timeout", body.Options["idle_timeout"])
			}

			var saved warpgate.Target
			if err := json.Unmarshal(encoded, &saved); err != nil {
				t.Fatal(err)
			}
			saved.ID = "target-a"
			api.targets = []warpgate.Target{saved}
			api.createdTargets, api.updatedTargets = nil, nil
			plan, err := New(api).Apply(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Summary() != (Summary{Unchanged: 2}) || len(api.createdTargets)+len(api.updatedTargets) != 0 {
				t.Fatalf("repeated apply = %#v, want no writes", plan.Summary())
			}
		})
	}
}

func TestPlanPreservesUnspecifiedPostgresProtocolVersion(t *testing.T) {
	for _, current := range []string{`"3.0"`, `"3.2"`, "", "null", `""`} {
		t.Run(current, func(t *testing.T) {
			target := postgresInventoryTarget("postgres-a", "environment-a")
			target.ProtocolVersion = ""
			api := &fakeAPI{
				groups:  []warpgate.TargetGroup{{ID: "group-a", Name: target.Group}},
				targets: []warpgate.Target{postgresTargetWithProtocol(t, current)},
			}
			plan, err := New(api).Plan(context.Background(), inventory.Inventory{
				Groups: []inventory.Group{{Name: target.Group}}, Targets: []inventory.Target{target},
			})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Summary() != (Summary{Unchanged: 2}) {
				t.Fatalf("plan = %#v, want no protocol changes", plan.Summary())
			}
		})
	}
}

func postgresTargetWithProtocol(t *testing.T, protocol string) warpgate.Target {
	t.Helper()
	target := postgresTarget("target-a", "postgres-a", "group-a")
	var options map[string]json.RawMessage
	if err := json.Unmarshal(target.Options, &options); err != nil {
		t.Fatal(err)
	}
	delete(options, "protocol_version")
	if protocol != "" {
		options["protocol_version"] = json.RawMessage(protocol)
	}
	options["idle_timeout"] = json.RawMessage(`"37s"`)
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	target.Options = encoded
	return target
}
