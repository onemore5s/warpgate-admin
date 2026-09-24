package reconcile

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/inventory"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

func httpInventoryTarget() inventory.Target {
	return inventory.Target{Name: "web-a", Group: "environment-a", Engine: inventory.EngineHTTP, TLSMode: inventory.TLSRequired, TLSVerify: true, HTTP: &inventory.HTTP{URL: "https://service.example.invalid"}}
}

func httpTarget(id, group string) warpgate.Target {
	options, _ := json.Marshal(warpgate.HTTPOptions{Kind: warpgate.HTTPKind, URL: "https://service.example.invalid", TLS: warpgate.TLS{Mode: warpgate.TLSRequired, Verify: true}, Headers: map[string]string{"Authorization": "secret-placeholder"}})
	return warpgate.Target{ID: id, Name: "web-a", GroupID: group, Options: options}
}

func TestHTTPApplyAndIdempotence(t *testing.T) {
	target := httpInventoryTarget()
	target.Roles, target.RolesSpecified = []string{"readers"}, true
	target.HTTP.HeadersSpecified = true
	target.HTTP.Headers = map[string]string{"Authorization": "secret-placeholder"}
	api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}}, roles: []warpgate.Role{{ID: "role-a", Name: "readers"}}}
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}, Targets: []inventory.Target{target}}
	plan, err := New(api).Apply(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.createdTargets) != 1 || len(api.addedTargetRoles) != 1 || strings.Contains(plan.String(), "secret-placeholder") {
		t.Fatal("Creation, role assignment, or redaction failed")
	}
	if api.createdTargets[0].Options.(warpgate.HTTPOptions).Headers["Authorization"] != "secret-placeholder" {
		t.Fatal("Headers were not sent")
	}
	api.targets = []warpgate.Target{httpTarget("web-id", "group-a")}
	api.targetRoles = map[string][]warpgate.Role{"web-id": {{ID: "role-a", Name: "readers"}}}
	plan, err = New(api, WithPasswordUpdates()).Plan(context.Background(), desired)
	if err != nil || plan.Summary().Update != 0 {
		t.Fatalf("HTTP must not be affected by the password update flag: %+v %v", plan.Summary(), err)
	}
}

func TestHTTPPreserveAndClearOptionalSettings(t *testing.T) {
	current := httpTarget("web-id", "group-a")
	var options warpgate.HTTPOptions
	_ = json.Unmarshal(current.Options, &options)
	domain := "web.example.invalid"
	options.ExternalHost = &domain
	current.Options, _ = json.Marshal(options)
	limit := int64(42)
	current.RateLimitBytesPerSecond = &limit
	approval := true
	current.RequireApproval = &approval
	target := httpInventoryTarget()
	target.Description = "Updated"
	request, err := targetRequest(target, "group-a", &current)
	if err != nil {
		t.Fatal(err)
	}
	got := request.Options.(warpgate.HTTPOptions)
	if got.Headers["Authorization"] != "secret-placeholder" || got.ExternalHost == nil || *got.ExternalHost != domain || request.RateLimitBytesPerSecond != &limit {
		t.Fatal("Unmanaged fields were not preserved")
	}
	if request.RequireApproval == nil || !*request.RequireApproval {
		t.Fatal("Session approval policy was not preserved")
	}
	empty := ""
	target.HTTP.ExternalHost, target.HTTP.HeadersSpecified = &empty, true
	request, err = targetRequest(target, "group-a", &current)
	if err != nil {
		t.Fatal(err)
	}
	got = request.Options.(warpgate.HTTPOptions)
	if len(got.Headers) != 0 || got.ExternalHost != nil {
		t.Fatal("Explicit clearing did not take effect")
	}
	matched, err := targetMatches(target, "group-a", current)
	if err != nil || matched {
		t.Fatalf("Expected an update to be planned: %v %v", matched, err)
	}
}

func TestHTTPEmptyAPIDefaultsAndRoles(t *testing.T) {
	current := httpTarget("web-id", "group-a")
	current.Options = json.RawMessage(`{"kind":"Http","url":"https://service.example.invalid","tls":{"mode":"Required","verify":true},"external_host":""}`)
	target := httpInventoryTarget()
	matched, err := targetMatches(target, "group-a", current)
	if err != nil || !matched {
		t.Fatalf("Empty API values should be normalized: %v %v", matched, err)
	}
	api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}}, targets: []warpgate.Target{current}, targetRoles: map[string][]warpgate.Role{"web-id": {{ID: "role-a", Name: "readers"}}}}
	desired := inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}, Targets: []inventory.Target{target}}
	_, err = New(api, WithRoleSync()).Apply(context.Background(), desired)
	if err != nil || len(api.removedTargetRoles) != 0 {
		t.Fatalf("Omitted roles should be preserved: %v", err)
	}
	desired.Targets[0].RolesSpecified = true
	_, err = New(api, WithRoleSync()).Apply(context.Background(), desired)
	if err != nil || len(api.removedTargetRoles) != 1 {
		t.Fatalf("Explicitly empty roles should be cleared: %v", err)
	}
}

func TestHTTPInvalidInternalOptions(t *testing.T) {
	target := httpInventoryTarget()
	target.HTTP = nil
	if _, err := targetOptions(target); err == nil {
		t.Fatal("Missing HTTP options must be rejected")
	}
	if _, err := targetMatches(target, "", warpgate.Target{}); err == nil {
		t.Fatal("Missing HTTP options must be rejected")
	}
	if _, err := targetRequest(target, "", nil); err == nil {
		t.Fatal("Missing HTTP options must be rejected")
	}
	current := warpgate.Target{Options: json.RawMessage(`{`)}
	if _, err := targetRequest(httpInventoryTarget(), "", &current); err == nil {
		t.Fatal("Malformed existing HTTP options must be rejected")
	}
}

func TestHTTPRejectsTypeChanges(t *testing.T) {
	for _, raw := range []string{`{"kind":"MySql"}`, `{"kind":"Ssh"}`, `{`} {
		_, err := targetMatches(httpInventoryTarget(), "group-a", warpgate.Target{Options: json.RawMessage(raw)})
		if err == nil {
			t.Fatal("Type changes or malformed data must be rejected")
		}
	}
}

func TestHTTPSyncScopes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options []Option
		want    []string
	}{
		{"default", nil, nil},
		{"database only", []Option{WithDatabaseTargetSync()}, []string{"db-id"}},
		{"HTTP only", []Option{WithHTTPTargetSync()}, []string{"web-id"}},
		{"both types", []Option{WithDatabaseTargetSync(), WithHTTPTargetSync()}, []string{"db-id", "web-id"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}}, targets: []warpgate.Target{mysqlTarget("db-id", "db-a", "group-a", "db.example.invalid"), httpTarget("web-id", "group-a"), {ID: "ssh-id", Name: "ssh-a", GroupID: "group-a", Options: json.RawMessage(`{"kind":"Ssh"}`)}}}
			outside := httpTarget("outside-id", "group-b")
			outside.Name = "outside"
			api.targets = append(api.targets, outside)
			_, err := New(api, tt.options...).Apply(context.Background(), inventory.Inventory{Groups: []inventory.Group{{Name: "environment-a"}}})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(api.deletedTargets, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("Deletion exceeded the configured scope: %v", api.deletedTargets)
			}
		})
	}
}
