package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

// Since 0.29.0, every write must include three booleans; false must not be omitted or encoded as null.
// Contract: TargetDataRequest in warp-tech/warpgate tags v0.29.0, v0.29.2, and v0.30.0.
func TestApplyCreatesTargetsWithExplicitApprovalPolicies(t *testing.T) {
	for _, desired := range []inventory.Target{
		mysqlInventoryTarget("mysql-a", "environment-a", "db.example.invalid"),
		postgresInventoryTarget("postgres-a", "environment-a"),
		httpInventoryTarget(),
	} {
		t.Run(string(desired.Engine), func(t *testing.T) {
			api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: desired.Group}}}
			input := inventory.Inventory{Groups: []inventory.Group{{Name: desired.Group}}, Targets: []inventory.Target{desired}}
			if _, err := New(api).Apply(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			if len(api.createdTargets) != 1 {
				t.Fatalf("create requests = %d, want 1", len(api.createdTargets))
			}
			assertApprovalPolicyJSON(t, api.createdTargets[0], false, false, false)
		})
	}
}

func TestApplyPreservesEveryApprovalPolicyCombination(t *testing.T) {
	for _, desired := range []inventory.Target{
		mysqlInventoryTarget("mysql-a", "environment-a", "db.example.invalid"),
		postgresInventoryTarget("postgres-a", "environment-a"),
		httpInventoryTarget(),
	} {
		for mask := range 8 {
			t.Run(fmt.Sprintf("%s/%03b", desired.Engine, mask), func(t *testing.T) {
				options, err := targetOptions(desired)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(options)
				if err != nil {
					t.Fatal(err)
				}
				disabled, ticketApproval, sessionApproval := mask&1 != 0, mask&2 != 0, mask&4 != 0
				current := warpgate.Target{
					ID: "target-a", Name: desired.Name, GroupID: "group-a", Options: encoded,
					TicketRequestsDisabled: &disabled, TicketRequireApproval: &ticketApproval, RequireApproval: &sessionApproval,
				}
				desired.Description = "Preserve existing approval policies when updating the description"
				api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: desired.Group}}, targets: []warpgate.Target{current}}
				input := inventory.Inventory{Groups: []inventory.Group{{Name: desired.Group}}, Targets: []inventory.Target{desired}}
				if _, err := New(api).Apply(context.Background(), input); err != nil {
					t.Fatal(err)
				}
				if len(api.updatedTargets) != 1 {
					t.Fatalf("update requests = %d, want 1", len(api.updatedTargets))
				}
				assertApprovalPolicyJSON(t, api.updatedTargets[0], disabled, ticketApproval, sessionApproval)
			})
		}
	}
}

func TestTargetRequestUsesDefaultsForLegacyMissingPolicies(t *testing.T) {
	for _, raw := range []string{
		`{"ticket_requests_disabled":true,"ticket_require_approval":true}`,
		`{}`,
	} {
		var current warpgate.Target
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			t.Fatal(err)
		}
		request, err := targetRequest(mysqlInventoryTarget("mysql-a", "environment-a", "db.example.invalid"), "group-a", &current)
		if err != nil {
			t.Fatal(err)
		}
		assertApprovalPolicyJSON(t, request, current.TicketRequestsDisabled != nil, current.TicketRequireApproval != nil, false)
	}
}

func assertApprovalPolicyJSON(t *testing.T, request warpgate.TargetRequest, disabled, ticketApproval, sessionApproval bool) {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"ticket_requests_disabled": disabled,
		"ticket_require_approval":  ticketApproval,
		"require_approval":         sessionApproval,
	} {
		got, ok := body[name].(bool)
		if !ok || got != want {
			t.Errorf("field %s = %v, want explicit boolean %t", name, body[name], want)
		}
	}
}
