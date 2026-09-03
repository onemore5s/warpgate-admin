package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

func TestPlanCreatesUpdatesAndKeepsUnmanagedTargets(t *testing.T) {
	colorInfo := "Info"
	api := &fakeAPI{
		groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a", Description: "Old", Color: &colorInfo}},
		targets: []warpgate.Target{
			mysqlTarget("mysql-a-id", "mysql-a", "group-a", "old.example.invalid"),
			mysqlTarget("unmanaged-id", "unmanaged", "group-a", "unmanaged.example.invalid"),
		},
	}
	desired := inventory.Inventory{
		Groups: []inventory.Group{
			{Name: "environment-a", Description: "New", Color: "Info"},
			{Name: "environment-b", Description: "Environment B", Color: "Danger"},
		},
		Targets: []inventory.Target{
			mysqlInventoryTarget("mysql-a", "environment-a", "new.example.invalid"),
			postgresInventoryTarget("postgres-a", "environment-b"),
		},
	}

	plan, err := New(api).Plan(context.Background(), desired)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Create: 2, Update: 2}) {
		t.Fatalf("Summary() = %#v", got)
	}
	text := plan.String()
	for _, want := range []string{"~ group environment-a", "+ group environment-b", "~ target mysql-a", "+ target postgres-a", "2 to create, 2 to update, 0 to delete, 0 unchanged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "unmanaged") || strings.Contains(text, "password-placeholder") {
		t.Fatalf("plan contains unmanaged target or password:\n%s", text)
	}
}

func TestPlanSyncsUnlistedRDSTargetsInManagedGroups(t *testing.T) {
	api := &fakeAPI{
		groups: []warpgate.TargetGroup{
			{ID: "group-a", Name: "environment-a"},
			{ID: "group-b", Name: "environment-b"},
		},
		targets: []warpgate.Target{
			mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid"),
			postgresTarget("stale-id", "stale-postgres", "group-a"),
			mysqlTarget("outside-id", "outside", "group-b", "outside.example.invalid"),
			{ID: "ssh-id", Name: "ssh-a", GroupID: "group-a", Options: json.RawMessage(`{"kind":"Ssh"}`)},
		},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}

	plan, err := New(api, WithTargetSync()).Plan(context.Background(), desired)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Delete: 1, Unchanged: 2}) {
		t.Fatalf("Summary() = %#v", got)
	}
	output := plan.String()
	if !strings.Contains(output, "- target stale-postgres") || strings.Contains(output, "outside") || strings.Contains(output, "ssh-a") {
		t.Fatalf("plan output = %q", output)
	}
}

func TestPlanTargetSyncDeletesAllRDSFromEmptyManagedGroup(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("stale-id", "stale-mysql", "group-a", "stale.example.invalid")},
	}
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}}

	plan, err := New(api, WithTargetSync()).Plan(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Delete: 1, Unchanged: 1}) {
		t.Fatalf("Plan() = %#v, %v", plan.Summary(), err)
	}
}

func TestPlanTargetSyncRejectsMalformedUnlistedTargetOptions(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{{ID: "target-id", Name: "unknown", GroupID: "group-a", Options: json.RawMessage(`{`)}},
	}
	_, err := New(api, WithTargetSync()).Plan(context.Background(), inventory.Inventory{
		Groups: []inventory.Group{{Name: "environment-a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "classify target") {
		t.Fatalf("Plan() error = %v", err)
	}
}

func TestPlanUnchangedForMySQLAndPostgres(t *testing.T) {
	colorInfo := "Info"
	api := &fakeAPI{
		groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a", Color: &colorInfo}},
		targets: []warpgate.Target{
			mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid"),
			postgresTarget("postgres-a-id", "postgres-a", "group-a"),
		},
	}
	desired := inventory.Inventory{
		Groups: []inventory.Group{{Name: "environment-a", Color: "Info"}},
		Targets: []inventory.Target{
			mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid"),
			postgresInventoryTarget("postgres-a", "environment-a"),
		},
	}
	plan, err := New(api).Plan(context.Background(), desired)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Unchanged: 3}) {
		t.Fatalf("Summary() = %#v", got)
	}
	if output := plan.String(); strings.Contains(output, "target mysql-a") || !strings.Contains(output, "3 unchanged") {
		t.Fatalf("plan output = %q", output)
	}
}

func TestPlanAddsMissingTargetRoles(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
		roles: []warpgate.Role{
			{ID: "readers-id", Name: "database-readers"},
			{ID: "oncall-id", Name: "oncall"},
		},
		targetRoles: map[string][]warpgate.Role{
			"mysql-a-id": {{ID: "readers-id", Name: "database-readers"}},
		},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"database-readers", "oncall"}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api).Plan(context.Background(), desired)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Create: 1, Unchanged: 3}) {
		t.Fatalf("Summary() = %#v", got)
	}
	if output := plan.String(); !strings.Contains(output, "+ role oncall -> target mysql-a") || strings.Contains(output, "role database-readers") {
		t.Fatalf("plan output = %q", output)
	}
}

func TestPlanSyncsTargetRoles(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
		roles: []warpgate.Role{
			{ID: "admin-id", Name: "admin"},
			{ID: "developer-id", Name: "developer"},
		},
		targetRoles: map[string][]warpgate.Role{
			"mysql-a-id": {
				{ID: "admin-id", Name: "admin"},
				{ID: "developer-id", Name: "developer"},
			},
		},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"admin"}
	target.RolesSpecified = true
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api, WithRoleSync()).Plan(context.Background(), desired)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Delete: 1, Unchanged: 3}) {
		t.Fatalf("Summary() = %#v", got)
	}
	if output := plan.String(); !strings.Contains(output, "- role developer -> target mysql-a") || strings.Contains(output, "role admin") || !strings.Contains(output, "1 to delete") {
		t.Fatalf("plan output = %q", output)
	}
}

func TestPlanRoleSyncPreservesRolesWhenColumnIsMissing(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	plan, err := New(api, WithRoleSync()).Plan(context.Background(), inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got := plan.Summary(); got != (Summary{Unchanged: 2}) {
		t.Fatalf("Summary() = %#v", got)
	}
	if api.listTargetRolesCalls != 0 {
		t.Fatalf("ListTargetRoles() calls = %d", api.listTargetRolesCalls)
	}
}

func TestPlanRejectsUnknownTargetRole(t *testing.T) {
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"missing-role"}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}
	_, err := New(&fakeAPI{}).Plan(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "unknown role") || !strings.Contains(err.Error(), "missing-role") {
		t.Fatalf("Plan() error = %v", err)
	}
}

func TestPlanTargetRoleErrors(t *testing.T) {
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"database-readers"}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}
	currentTarget := mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")
	tests := map[string]struct {
		api              *fakeAPI
		duplicateDesired bool
		want             string
	}{
		"list roles": {
			api:  &fakeAPI{listRolesErr: errors.New("roles failed")},
			want: "roles failed",
		},
		"duplicate roles": {
			api: &fakeAPI{roles: []warpgate.Role{
				{ID: "role-a", Name: "database-readers"},
				{ID: "role-b", Name: "database-readers"},
			}},
			want: "duplicate role",
		},
		"list target roles": {
			api: &fakeAPI{
				groups:             []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets:            []warpgate.Target{currentTarget},
				roles:              []warpgate.Role{{ID: "role-a", Name: "database-readers"}},
				listTargetRolesErr: errors.New("target roles failed"),
			},
			want: "target roles failed",
		},
		"duplicate target roles": {
			api: &fakeAPI{
				groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets: []warpgate.Target{currentTarget},
				roles:   []warpgate.Role{{ID: "role-a", Name: "database-readers"}},
				targetRoles: map[string][]warpgate.Role{
					"mysql-a-id": {
						{ID: "role-a", Name: "database-readers"},
						{ID: "role-b", Name: "database-readers"},
					},
				},
			},
			want: "duplicate role",
		},
		"duplicate desired roles": {
			api:              &fakeAPI{roles: []warpgate.Role{{ID: "role-a", Name: "database-readers"}}},
			duplicateDesired: true,
			want:             "duplicate desired role",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			input := desired
			input.Targets = append([]inventory.Target(nil), desired.Targets...)
			if tt.duplicateDesired {
				input.Targets[0].Roles = []string{"database-readers", "database-readers"}
			}
			_, err := New(tt.api).Plan(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Plan() error = %v", err)
			}
		})
	}
}

func TestPlanTreatsEncryptedOrRedactedPasswordAsUnknown(t *testing.T) {
	tests := []string{"", "wgenc:v1:fingerprint:ciphertext"}
	for _, password := range tests {
		t.Run(password, func(t *testing.T) {
			current := mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")
			var options warpgate.MySQLOptions
			if err := json.Unmarshal(current.Options, &options); err != nil {
				t.Fatal(err)
			}
			options.Auth.Password = password
			current.Options, _ = json.Marshal(options)
			api := &fakeAPI{
				groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets: []warpgate.Target{current},
			}
			desired := inventory.Inventory{
				Groups:  []inventory.Group{{Name: "environment-a"}},
				Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
			}
			plan, err := New(api).Plan(context.Background(), desired)
			if err != nil || plan.Summary() != (Summary{Unchanged: 2}) {
				t.Fatalf("Plan() = %#v, %v", plan.Summary(), err)
			}
		})
	}
}

func TestPlanDetectsPlaintextPasswordChange(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	desired.Targets[0].Password = "new-password-placeholder"
	plan, err := New(api).Plan(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Update: 1, Unchanged: 1}) {
		t.Fatalf("Plan() = %#v, %v", plan.Summary(), err)
	}
}

func TestPlanCanForcePasswordUpdates(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	plan, err := New(api, WithPasswordUpdates()).Plan(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Update: 1, Unchanged: 1}) {
		t.Fatalf("Plan() = %#v, %v", plan.Summary(), err)
	}
}

func TestPlanRejectsExistingDifferentTargetKind(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{{ID: "target-id", Name: "mysql-a", GroupID: "group-a", Options: json.RawMessage(`{"kind":"Ssh"}`)}},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	_, err := New(api).Plan(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("Plan() error = %v", err)
	}
}

func TestPlanRejectsMalformedRDSOptions(t *testing.T) {
	tests := []struct {
		name    string
		target  inventory.Target
		options string
		want    string
	}{
		{name: "mysql options", target: mysqlInventoryTarget("target-a", "environment-a", "mysql-a.example.invalid"), options: `{"kind":"MySql","port":"bad"}`, want: "MySQL options"},
		{name: "postgres kind", target: postgresInventoryTarget("target-a", "environment-a"), options: `{"kind":"MySql"}`, want: "expected \"Postgres\""},
		{name: "postgres options", target: postgresInventoryTarget("target-a", "environment-a"), options: `{"kind":"Postgres","port":"bad"}`, want: "PostgreSQL options"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{
				groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets: []warpgate.Target{{ID: "target-id", Name: "target-a", GroupID: "group-a", Options: json.RawMessage(tt.options)}},
			}
			desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}, Targets: []inventory.Target{tt.target}}
			_, err := New(api).Plan(context.Background(), desired)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Plan() error = %v", err)
			}
		})
	}
}

func TestPlanErrors(t *testing.T) {
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}}
	tests := map[string]struct {
		api  *fakeAPI
		want string
	}{
		"list groups":      {api: &fakeAPI{listGroupsErr: errors.New("groups failed")}, want: "groups failed"},
		"list targets":     {api: &fakeAPI{listTargetsErr: errors.New("targets failed")}, want: "targets failed"},
		"duplicate groups": {api: &fakeAPI{groups: []warpgate.TargetGroup{{Name: "duplicate"}, {Name: "duplicate"}}}, want: "duplicate group"},
		"duplicate targets": {
			api:  &fakeAPI{targets: []warpgate.Target{{Name: "duplicate"}, {Name: "duplicate"}}},
			want: "duplicate target",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := New(tt.api).Plan(context.Background(), desired)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Plan() error = %v", err)
			}
		})
	}

	t.Run("invalid current options", func(t *testing.T) {
		api := &fakeAPI{
			groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
			targets: []warpgate.Target{{Name: "mysql-a", Options: json.RawMessage(`{`)}},
		}
		withTarget := inventory.Inventory{
			Groups:  desired.Groups,
			Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
		}
		_, err := New(api).Plan(context.Background(), withTarget)
		if err == nil || !strings.Contains(err.Error(), "decode current options") {
			t.Fatalf("Plan() error = %v", err)
		}
	})
}

func TestApplyCreatesRDSGroupsAndTargets(t *testing.T) {
	api := &fakeAPI{}
	desired := inventory.Inventory{
		Groups: []inventory.Group{{Name: "environment-a", Color: "Info"}},
		Targets: []inventory.Target{
			mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid"),
			postgresInventoryTarget("postgres-a", "environment-a"),
		},
	}
	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if plan.Summary().Create != 3 || len(api.createdGroups) != 1 || len(api.createdTargets) != 2 {
		t.Fatalf("apply = %#v, groups = %#v, targets = %#v", plan.Summary(), api.createdGroups, api.createdTargets)
	}
	if api.createdTargets[0].GroupID != "new-group-id" {
		t.Fatalf("target group ID = %q", api.createdTargets[0].GroupID)
	}
	mysql, ok := api.createdTargets[0].Options.(warpgate.MySQLOptions)
	if !ok || mysql.Kind != warpgate.MySQLKind || mysql.Auth == nil || mysql.Auth.Password != "password-placeholder" || mysql.TLS.Mode != warpgate.TLSRequired {
		t.Fatalf("MySQL options = %#v", api.createdTargets[0].Options)
	}
	postgres, ok := api.createdTargets[1].Options.(warpgate.PostgresOptions)
	if !ok || postgres.Kind != warpgate.PostgresKind || postgres.DefaultDatabaseName != "app_db" || postgres.ProtocolVersion != "3.0" {
		t.Fatalf("PostgreSQL options = %#v", api.createdTargets[1].Options)
	}
}

func TestApplyDeletesUnlistedRDSTargets(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("stale-id", "stale-mysql", "group-a", "stale.example.invalid")},
	}
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}}

	plan, err := New(api, WithTargetSync()).Apply(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Delete: 1, Unchanged: 1}) {
		t.Fatalf("Apply() = %#v, %v", plan.Summary(), err)
	}
	if len(api.deletedTargets) != 1 || api.deletedTargets[0] != "stale-id" {
		t.Fatalf("deleted targets = %#v", api.deletedTargets)
	}
}

func TestApplyAssignsRolesToNewTarget(t *testing.T) {
	api := &fakeAPI{roles: []warpgate.Role{{ID: "readers-id", Name: "database-readers"}}}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"database-readers"}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if plan.Summary() != (Summary{Create: 3}) {
		t.Fatalf("Summary() = %#v", plan.Summary())
	}
	if len(api.addedTargetRoles) != 1 || api.addedTargetRoles[0] != (targetRoleAssignment{TargetID: "new-target-id", RoleID: "readers-id"}) {
		t.Fatalf("target role assignments = %#v", api.addedTargetRoles)
	}
}

func TestApplyKeepsExistingAndUnlistedTargetRoles(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
		roles: []warpgate.Role{
			{ID: "readers-id", Name: "database-readers"},
			{ID: "existing-id", Name: "existing-unlisted-role"},
		},
		targetRoles: map[string][]warpgate.Role{
			"mysql-a-id": {
				{ID: "readers-id", Name: "database-readers"},
				{ID: "existing-id", Name: "existing-unlisted-role"},
			},
		},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"database-readers"}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Unchanged: 3}) {
		t.Fatalf("Apply() = %#v, %v", plan.Summary(), err)
	}
	if len(api.addedTargetRoles) != 0 {
		t.Fatalf("target role assignments = %#v", api.addedTargetRoles)
	}
}

func TestApplySyncsTargetRoles(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
		roles:   []warpgate.Role{{ID: "admin-id", Name: "admin"}},
		targetRoles: map[string][]warpgate.Role{
			"mysql-a-id": {
				{ID: "admin-id", Name: "admin"},
				{ID: "developer-id", Name: "developer"},
			},
		},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.Roles = []string{"admin"}
	target.RolesSpecified = true
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api, WithRoleSync()).Apply(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Delete: 1, Unchanged: 3}) {
		t.Fatalf("Apply() = %#v, %v", plan.Summary(), err)
	}
	if len(api.removedTargetRoles) != 1 || api.removedTargetRoles[0] != (targetRoleAssignment{TargetID: "mysql-a-id", RoleID: "developer-id"}) {
		t.Fatalf("removed target role assignments = %#v", api.removedTargetRoles)
	}
}

func TestApplyRoleSyncRemovesAllRolesForEmptySpecifiedRoles(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
		targetRoles: map[string][]warpgate.Role{
			"mysql-a-id": {{ID: "admin-id", Name: "admin"}},
		},
	}
	target := mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")
	target.RolesSpecified = true
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{target},
	}

	plan, err := New(api, WithRoleSync()).Apply(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Delete: 1, Unchanged: 2}) {
		t.Fatalf("Apply() = %#v, %v", plan.Summary(), err)
	}
	if len(api.removedTargetRoles) != 1 || api.removedTargetRoles[0].RoleID != "admin-id" {
		t.Fatalf("removed target role assignments = %#v", api.removedTargetRoles)
	}
}

func TestApplyAssignsExistingTargetToNewGroup(t *testing.T) {
	api := &fakeAPI{targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "", "mysql-a.example.invalid")}}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if plan.Summary() != (Summary{Create: 1, Update: 1}) || len(api.updatedTargets) != 1 || api.updatedTargets[0].GroupID != "new-group-id" {
		t.Fatalf("apply = %#v, updated targets = %#v", plan.Summary(), api.updatedTargets)
	}
}

func TestApplyPreservesUnmanagedTargetFields(t *testing.T) {
	rateLimit := int64(1024)
	ticketDisabled := true
	current := mysqlTarget("mysql-a-id", "mysql-a", "group-a", "old.example.invalid")
	current.RateLimitBytesPerSecond = &rateLimit
	current.TicketRequestsDisabled = &ticketDisabled
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{current},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "new.example.invalid")},
	}
	if _, err := New(api).Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	request := api.updatedTargets[0]
	if request.RateLimitBytesPerSecond == nil || *request.RateLimitBytesPerSecond != rateLimit || request.TicketRequestsDisabled == nil || !*request.TicketRequestsDisabled {
		t.Fatalf("target request = %#v", request)
	}
}

func TestApplyPreservesPostgresIdleTimeout(t *testing.T) {
	idleTimeout := "5m"
	current := postgresTarget("postgres-a-id", "postgres-a", "group-a")
	var options warpgate.PostgresOptions
	if err := json.Unmarshal(current.Options, &options); err != nil {
		t.Fatal(err)
	}
	options.IdleTimeout = &idleTimeout
	current.Options, _ = json.Marshal(options)
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{current},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{postgresInventoryTarget("postgres-a", "environment-a")},
	}
	desired.Targets[0].Description = "Updated"
	if _, err := New(api).Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	requestOptions := api.updatedTargets[0].Options.(warpgate.PostgresOptions)
	if requestOptions.IdleTimeout == nil || *requestOptions.IdleTimeout != idleTimeout {
		t.Fatalf("PostgreSQL options = %#v", requestOptions)
	}
}

func TestApplyUpdatesGroup(t *testing.T) {
	api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a", Description: "Old"}}}
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a", Description: "New", Color: "Info"}}}
	if _, err := New(api).Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(api.updatedGroups) != 1 || api.updatedGroups[0].Description != "New" {
		t.Fatalf("updated groups = %#v", api.updatedGroups)
	}
}

func TestApplyUnchangedDoesNotMutate(t *testing.T) {
	api := &fakeAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
		targets: []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
	}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil || plan.Summary() != (Summary{Unchanged: 2}) {
		t.Fatalf("Apply() = %#v, %v", plan.Summary(), err)
	}
	if len(api.createdGroups)+len(api.updatedGroups)+len(api.createdTargets)+len(api.updatedTargets) != 0 {
		t.Fatalf("unexpected mutations: %#v", api)
	}
}

func TestApplyRejectsUnsupportedEngine(t *testing.T) {
	api := &fakeAPI{}
	desired := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{{Name: "target-a", Group: "environment-a", Engine: "oracle"}},
	}
	_, err := New(api).Apply(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "unsupported RDS engine") {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestApplyMutationErrors(t *testing.T) {
	base := inventory.Inventory{
		Groups:  []inventory.Group{{Name: "environment-a"}},
		Targets: []inventory.Target{mysqlInventoryTarget("mysql-a", "environment-a", "mysql-a.example.invalid")},
	}
	tests := map[string]struct {
		api     *fakeAPI
		desired inventory.Inventory
		want    string
	}{
		"create group": {api: &fakeAPI{createGroupErr: errors.New("create group failed")}, desired: base, want: "create group failed"},
		"update group": {
			api:     &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a", Description: "Old"}}, updateGroupErr: errors.New("update group failed")},
			desired: inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a", Description: "New"}}},
			want:    "update group failed",
		},
		"create target": {api: &fakeAPI{createTargetErr: errors.New("create target failed")}, desired: base, want: "create target failed"},
		"update target": {
			api: &fakeAPI{
				groups:          []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets:         []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "old.example.invalid")},
				updateTargetErr: errors.New("update target failed"),
			},
			desired: base,
			want:    "update target failed",
		},
		"delete target": {
			api: &fakeAPI{
				groups:          []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets:         []warpgate.Target{mysqlTarget("stale-id", "stale-mysql", "group-a", "stale.example.invalid")},
				deleteTargetErr: errors.New("delete target failed"),
			},
			desired: inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}},
			want:    "delete target failed",
		},
		"add target role": {
			api: &fakeAPI{
				roles:            []warpgate.Role{{ID: "readers-id", Name: "database-readers"}},
				addTargetRoleErr: errors.New("add target role failed"),
			},
			desired: func() inventory.Inventory {
				withRole := base
				withRole.Targets = append([]inventory.Target(nil), base.Targets...)
				withRole.Targets[0].Roles = []string{"database-readers"}
				return withRole
			}(),
			want: "add target role failed",
		},
		"remove target role": {
			api: &fakeAPI{
				groups:              []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}},
				targets:             []warpgate.Target{mysqlTarget("mysql-a-id", "mysql-a", "group-a", "mysql-a.example.invalid")},
				targetRoles:         map[string][]warpgate.Role{"mysql-a-id": {{ID: "developer-id", Name: "developer"}}},
				removeTargetRoleErr: errors.New("remove target role failed"),
			},
			desired: func() inventory.Inventory {
				withoutRoles := base
				withoutRoles.Targets = append([]inventory.Target(nil), base.Targets...)
				withoutRoles.Targets[0].RolesSpecified = true
				return withoutRoles
			}(),
			want: "remove target role failed",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			options := []Option(nil)
			if name == "remove target role" {
				options = append(options, WithRoleSync())
			}
			if name == "delete target" {
				options = append(options, WithTargetSync())
			}
			_, err := New(tt.api, options...).Apply(context.Background(), tt.desired)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply() error = %v", err)
			}
		})
	}
}

func mysqlInventoryTarget(name, group, host string) inventory.Target {
	return inventory.Target{
		Name: name, Group: group, Engine: inventory.EngineMySQL, Host: host, Port: 3306,
		Username: "db_user", Password: "password-placeholder", TLSMode: inventory.TLSRequired, TLSVerify: true,
	}
}

func postgresInventoryTarget(name, group string) inventory.Target {
	return inventory.Target{
		Name: name, Group: group, Engine: inventory.EnginePostgres, Host: name + ".example.invalid", Port: 5432,
		Username: "db_user", Password: "password-placeholder", TLSMode: inventory.TLSPreferred,
		DefaultDatabaseName: "app_db", ProtocolVersion: "3.0",
	}
}

func mysqlTarget(id, name, groupID, host string) warpgate.Target {
	options, _ := json.Marshal(warpgate.MySQLOptions{
		Kind: warpgate.MySQLKind, Host: host, Port: 3306, Username: "db_user", Auth: &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: "password-placeholder"},
		TLS: warpgate.TLS{Mode: warpgate.TLSRequired, Verify: true},
	})
	return warpgate.Target{ID: id, Name: name, GroupID: groupID, Options: options}
}

func postgresTarget(id, name, groupID string) warpgate.Target {
	options, _ := json.Marshal(warpgate.PostgresOptions{
		Kind: warpgate.PostgresKind, Host: name + ".example.invalid", Port: 5432, Username: "db_user", Auth: &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: "password-placeholder"},
		TLS: warpgate.TLS{Mode: warpgate.TLSPreferred}, DefaultDatabaseName: "app_db", ProtocolVersion: "3.0",
	})
	return warpgate.Target{ID: id, Name: name, GroupID: groupID, Options: options}
}

type fakeAPI struct {
	groups               []warpgate.TargetGroup
	targets              []warpgate.Target
	createdGroups        []warpgate.GroupRequest
	updatedGroups        []warpgate.GroupRequest
	createdTargets       []warpgate.TargetRequest
	updatedTargets       []warpgate.TargetRequest
	deletedTargets       []string
	createGroupErr       error
	updateGroupErr       error
	createTargetErr      error
	updateTargetErr      error
	deleteTargetErr      error
	listGroupsErr        error
	listTargetsErr       error
	roles                []warpgate.Role
	targetRoles          map[string][]warpgate.Role
	addedTargetRoles     []targetRoleAssignment
	removedTargetRoles   []targetRoleAssignment
	listTargetRolesCalls int
	listRolesErr         error
	listTargetRolesErr   error
	addTargetRoleErr     error
	removeTargetRoleErr  error
}

type targetRoleAssignment struct {
	TargetID string
	RoleID   string
}

func (api *fakeAPI) ListGroups(context.Context) ([]warpgate.TargetGroup, error) {
	return api.groups, api.listGroupsErr
}

func (api *fakeAPI) CreateGroup(_ context.Context, request warpgate.GroupRequest) (warpgate.TargetGroup, error) {
	if api.createGroupErr != nil {
		return warpgate.TargetGroup{}, api.createGroupErr
	}
	api.createdGroups = append(api.createdGroups, request)
	return warpgate.TargetGroup{ID: "new-group-id", Name: request.Name, Description: request.Description, Color: request.Color}, nil
}

func (api *fakeAPI) UpdateGroup(_ context.Context, id string, request warpgate.GroupRequest) (warpgate.TargetGroup, error) {
	if api.updateGroupErr != nil {
		return warpgate.TargetGroup{}, api.updateGroupErr
	}
	api.updatedGroups = append(api.updatedGroups, request)
	return warpgate.TargetGroup{ID: id, Name: request.Name, Description: request.Description, Color: request.Color}, nil
}

func (api *fakeAPI) ListTargets(context.Context) ([]warpgate.Target, error) {
	return api.targets, api.listTargetsErr
}

func (api *fakeAPI) CreateTarget(_ context.Context, request warpgate.TargetRequest) (warpgate.Target, error) {
	if api.createTargetErr != nil {
		return warpgate.Target{}, api.createTargetErr
	}
	api.createdTargets = append(api.createdTargets, request)
	return warpgate.Target{ID: "new-target-id", Name: request.Name}, nil
}

func (api *fakeAPI) UpdateTarget(_ context.Context, id string, request warpgate.TargetRequest) (warpgate.Target, error) {
	if api.updateTargetErr != nil {
		return warpgate.Target{}, api.updateTargetErr
	}
	api.updatedTargets = append(api.updatedTargets, request)
	return warpgate.Target{ID: id, Name: request.Name}, nil
}

func (api *fakeAPI) DeleteTarget(_ context.Context, id string) error {
	if api.deleteTargetErr != nil {
		return api.deleteTargetErr
	}
	api.deletedTargets = append(api.deletedTargets, id)
	return nil
}

func (api *fakeAPI) ListRoles(context.Context) ([]warpgate.Role, error) {
	return api.roles, api.listRolesErr
}

func (api *fakeAPI) ListTargetRoles(_ context.Context, targetID string) ([]warpgate.Role, error) {
	api.listTargetRolesCalls++
	return api.targetRoles[targetID], api.listTargetRolesErr
}

func (api *fakeAPI) AddTargetRole(_ context.Context, targetID, roleID string) error {
	if api.addTargetRoleErr != nil {
		return api.addTargetRoleErr
	}
	api.addedTargetRoles = append(api.addedTargetRoles, targetRoleAssignment{TargetID: targetID, RoleID: roleID})
	return nil
}

func (api *fakeAPI) RemoveTargetRole(_ context.Context, targetID, roleID string) error {
	if api.removeTargetRoleErr != nil {
		return api.removeTargetRoleErr
	}
	api.removedTargetRoles = append(api.removedTargetRoles, targetRoleAssignment{TargetID: targetID, RoleID: roleID})
	return nil
}
