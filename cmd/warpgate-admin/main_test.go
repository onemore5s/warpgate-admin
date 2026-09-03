package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/reconcile"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

const commandGroupsCSV = `enabled,name,description,color
true,environment-a,Environment A,Info
`

const commandTargetsCSV = `enabled,group,name,description,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version
true,environment-a,mysql-a,MySQL RDS,mysql,mysql-a.example.invalid,3306,db_user,password-placeholder,,required,true,,
`

const commandConfigYAML = `warpgate:
  host: https://yaml-warpgate.example.invalid
  token: yaml-token-placeholder
  insecure_skip_verify: true
inventory:
  groups: groups.csv
  targets: targets.csv
apply:
  update_passwords: true
`

func TestRunWithYAMLConfig(t *testing.T) {
	currentOptions, _ := json.Marshal(warpgate.MySQLOptions{
		Kind: warpgate.MySQLKind, Host: "mysql-a.example.invalid", Port: 3306, Username: "db_user",
		Auth: &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: "wgenc:v1:fingerprint:ciphertext"},
		TLS:  warpgate.TLS{Mode: warpgate.TLSRequired, Verify: true},
	})
	color := "Info"
	api := &commandAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-id", Name: "environment-a", Description: "Environment A", Color: &color}},
		targets: []warpgate.Target{{ID: "target-id", Name: "mysql-a", Description: "MySQL RDS", GroupID: "group-id", Options: currentOptions}},
	}
	deps := dependencies{
		getenv:   mapEnv(map[string]string{}),
		openFile: configFiles(commandConfigYAML, commandGroupsCSV, commandTargetsCSV),
		newAPI: func(config warpgate.Config) (reconcile.API, error) {
			if config.Host != "https://yaml-warpgate.example.invalid" || config.Token != "yaml-token-placeholder" || !config.InsecureSkipVerify {
				t.Fatalf("Warpgate config = %#v", config)
			}
			return api, nil
		},
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), []string{"plan", "--config", "config/warpgate-admin.yaml"}, deps, stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "~ target mysql-a") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunFlagsOverrideYAMLInventoryPaths(t *testing.T) {
	configYAML := strings.Replace(commandConfigYAML, "groups.csv", "ignored-groups.csv", 1)
	configYAML = strings.Replace(configYAML, "targets.csv", "ignored-targets.csv", 1)
	deps := testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{})
	baseOpen := deps.openFile
	deps.openFile = func(name string) (io.ReadCloser, error) {
		if name == "config.yaml" {
			return io.NopCloser(strings.NewReader(configYAML)), nil
		}
		return baseOpen(name)
	}
	args := append(commandArgs("validate"), "--config", "config.yaml")
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	if code := run(context.Background(), args, deps, stdout, stderr); code != 0 || !strings.Contains(stdout.String(), "1 group, 1 target") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunYAMLConfigIgnoresWarpgateEnvironment(t *testing.T) {
	configYAML := strings.Replace(commandConfigYAML, "  insecure_skip_verify: true", "  insecure_skip_verify: false", 1)
	deps := dependencies{
		getenv: mapEnv(map[string]string{
			"WARPGATE_HOST":                 "https://env-warpgate.example.invalid",
			"WARPGATE_TOKEN":                "env-token-placeholder",
			"WARPGATE_INSECURE_SKIP_VERIFY": "true",
		}),
		openFile: configFiles(configYAML, commandGroupsCSV, commandTargetsCSV),
		newAPI: func(config warpgate.Config) (reconcile.API, error) {
			if config.Host != "https://yaml-warpgate.example.invalid" || config.Token != "yaml-token-placeholder" || config.InsecureSkipVerify {
				t.Fatalf("Warpgate config = %#v", config)
			}
			return &commandAPI{}, nil
		},
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	if code := run(context.Background(), []string{"plan", "--config", "config/warpgate-admin.yaml"}, deps, stdout, stderr); code != 0 {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunValidate(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), commandArgs("validate"), testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{}), stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "1 group, 1 target") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunPlan(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), configuredCommandArgs("plan"), testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{}), stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "+ group environment-a") || strings.Contains(stdout.String(), "password-placeholder") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunApply(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	api := &commandAPI{}
	code := run(context.Background(), configuredCommandArgs("apply"), testDependencies(commandGroupsCSV, commandTargetsCSV, api), stdout, stderr)
	if code != 0 || stderr.Len() != 0 || len(api.createdGroups) != 1 || len(api.createdTargets) != 1 || !strings.Contains(stdout.String(), "Apply complete") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q, groups = %d, targets = %d", code, stdout, stderr, len(api.createdGroups), len(api.createdTargets))
	}
}

func TestRunApplyAssignsTargetRoles(t *testing.T) {
	targets := strings.Replace(commandTargetsCSV, "description,engine", "description,roles,engine", 1)
	targets = strings.Replace(targets, "MySQL RDS,mysql", "MySQL RDS,database-readers,mysql", 1)
	api := &commandAPI{roles: []warpgate.Role{{ID: "role-id", Name: "database-readers"}}}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), configuredCommandArgs("apply"), testDependencies(commandGroupsCSV, targets, api), stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "+ role database-readers -> target mysql-a") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(api.addedTargetRoles) != 1 || api.addedTargetRoles[0] != (commandTargetRoleAssignment{TargetID: "target-id", RoleID: "role-id"}) {
		t.Fatalf("target role assignments = %#v", api.addedTargetRoles)
	}
}

func TestRunApplySyncsTargetRolesFromYAMLConfig(t *testing.T) {
	targetsCSV := strings.Replace(commandTargetsCSV, "description,engine", "description,roles,engine", 1)
	targetsCSV = strings.Replace(targetsCSV, "MySQL RDS,mysql", "MySQL RDS,admin,mysql", 1)
	configYAML := strings.Replace(commandConfigYAML, "  update_passwords: true", "  update_passwords: false\n  sync_roles: true", 1)
	currentOptions, _ := json.Marshal(warpgate.MySQLOptions{
		Kind: warpgate.MySQLKind, Host: "mysql-a.example.invalid", Port: 3306, Username: "db_user",
		Auth: &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: "wgenc:v1:fingerprint:ciphertext"},
		TLS:  warpgate.TLS{Mode: warpgate.TLSRequired, Verify: true},
	})
	color := "Info"
	api := &commandAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-id", Name: "environment-a", Description: "Environment A", Color: &color}},
		targets: []warpgate.Target{{ID: "target-id", Name: "mysql-a", Description: "MySQL RDS", GroupID: "group-id", Options: currentOptions}},
		roles: []warpgate.Role{
			{ID: "admin-id", Name: "admin"},
			{ID: "developer-id", Name: "developer"},
		},
		targetRoles: map[string][]warpgate.Role{
			"target-id": {
				{ID: "admin-id", Name: "admin"},
				{ID: "developer-id", Name: "developer"},
			},
		},
	}
	deps := dependencies{
		getenv:   mapEnv(map[string]string{}),
		openFile: configFiles(configYAML, commandGroupsCSV, targetsCSV),
		newAPI: func(warpgate.Config) (reconcile.API, error) {
			return api, nil
		},
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), []string{"apply", "--config", "config/warpgate-admin.yaml"}, deps, stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "- role developer -> target mysql-a") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(api.removedTargetRoles) != 1 || api.removedTargetRoles[0] != (commandTargetRoleAssignment{TargetID: "target-id", RoleID: "developer-id"}) {
		t.Fatalf("removed target role assignments = %#v", api.removedTargetRoles)
	}
}

func TestRunApplySyncsTargetsFromYAMLConfig(t *testing.T) {
	configYAML := strings.Replace(commandConfigYAML, "  update_passwords: true", "  update_passwords: false\n  sync_targets: true", 1)
	color := "Info"
	api := &commandAPI{
		groups:  []warpgate.TargetGroup{{ID: "group-id", Name: "environment-a", Description: "Environment A", Color: &color}},
		targets: []warpgate.Target{mysqlCommandTarget("stale-id", "stale-mysql", "group-id", "stale.example.invalid")},
	}
	deps := dependencies{
		getenv:   mapEnv(map[string]string{}),
		openFile: configFiles(configYAML, commandGroupsCSV, commandTargetsCSV),
		newAPI: func(warpgate.Config) (reconcile.API, error) {
			return api, nil
		},
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := run(context.Background(), []string{"apply", "--config", "config/warpgate-admin.yaml"}, deps, stdout, stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "- target stale-mysql") {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(api.deletedTargets) != 1 || api.deletedTargets[0] != "stale-id" {
		t.Fatalf("deleted targets = %#v", api.deletedTargets)
	}
}

func TestRunResolvesPasswordEnvironmentVariable(t *testing.T) {
	targets := strings.Replace(commandTargetsCSV, "password-placeholder,,required", ",MYSQL_A_PASSWORD,required", 1)
	api := &commandAPI{}
	deps := testDependencies(commandGroupsCSV, targets, api)
	deps.getenv = mapEnv(map[string]string{
		"MYSQL_A_PASSWORD": "resolved-placeholder",
	})
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	if code := run(context.Background(), configuredCommandArgs("apply"), deps, stdout, stderr); code != 0 {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	options, ok := api.createdTargets[0].Options.(warpgate.MySQLOptions)
	if !ok || options.Auth == nil || options.Auth.Password != "resolved-placeholder" {
		t.Fatalf("target options = %#v", api.createdTargets[0].Options)
	}
}

func TestRunAcceptsPasswordUpdateFlag(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	args := append(configuredCommandArgs("plan"), "--update-passwords")
	if code := run(context.Background(), args, testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{}), stdout, stderr); code != 0 {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}} {
		stdout, stderr := &strings.Builder{}, &strings.Builder{}
		if code := run(context.Background(), args, testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{}), stdout, stderr); code != 0 || stderr.Len() != 0 || stdout.Len() == 0 {
			t.Fatalf("run(%v) = %d, stdout = %q, stderr = %q", args, code, stdout, stderr)
		}
	}
}

func TestRunCommandHelpAndZeroPlural(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	if code := run(context.Background(), []string{"plan", "--help"}, testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{}), stdout, stderr); code != 0 || !strings.Contains(stderr.String(), "Usage") {
		t.Fatalf("run(help) = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	emptyGroups := "enabled,name,description,color\n"
	emptyTargets := "enabled,group,name,description,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version\n"
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), commandArgs("validate"), testDependencies(emptyGroups, emptyTargets, &commandAPI{}), stdout, stderr); code != 0 || !strings.Contains(stdout.String(), "0 groups, 0 targets") {
		t.Fatalf("run(validate) = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestRunPlanAndApplyErrors(t *testing.T) {
	tests := []struct {
		command string
		api     *commandAPI
		want    string
	}{
		{command: "plan", api: &commandAPI{listGroupsErr: errors.New("list failed")}, want: "Failed to build plan"},
		{command: "apply", api: &commandAPI{createGroupErr: errors.New("create failed")}, want: "Apply failed"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			stdout, stderr := &strings.Builder{}, &strings.Builder{}
			code := run(context.Background(), configuredCommandArgs(tt.command), testDependencies(commandGroupsCSV, commandTargetsCSV, tt.api), stdout, stderr)
			if code == 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	base := testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{})
	tests := map[string]struct {
		args []string
		deps dependencies
		want string
	}{
		"missing command": {nil, base, "Usage"},
		"unknown command": {[]string{"destroy"}, base, "Unknown command"},
		"missing files":   {[]string{"validate"}, base, "--groups"},
		"missing targets": {[]string{"validate", "--groups", "groups.csv"}, base, "--targets"},
		"bad flag":        {[]string{"validate", "--unknown"}, base, "flag provided"},
		"position arg":    {append(commandArgs("validate"), "extra"), base, "Positional arguments"},
		"open groups": {
			commandArgs("validate"),
			dependencies{getenv: func(string) string { return "" }, openFile: func(string) (io.ReadCloser, error) { return nil, errors.New("missing groups") }},
			"missing groups",
		},
		"open targets": {
			commandArgs("validate"),
			dependencies{getenv: func(string) string { return "" }, openFile: func(name string) (io.ReadCloser, error) {
				if name == "groups.csv" {
					return io.NopCloser(strings.NewReader(commandGroupsCSV)), nil
				}
				return nil, errors.New("missing targets")
			}},
			"missing targets",
		},
		"parse groups":  {commandArgs("validate"), testDependencies("bad-header\n", commandTargetsCSV, &commandAPI{}), "unknown column"},
		"parse targets": {commandArgs("validate"), testDependencies(commandGroupsCSV, "bad-header\n", &commandAPI{}), "unknown column"},
		"missing host": {
			configuredCommandArgs("plan"),
			dependencies{getenv: func(string) string { return "" }, openFile: configFiles(strings.Replace(commandConfigYAML, "  host: https://yaml-warpgate.example.invalid\n", "", 1), commandGroupsCSV, commandTargetsCSV)},
			"warpgate.host",
		},
		"missing token": {
			configuredCommandArgs("plan"),
			dependencies{getenv: func(string) string { return "" }, openFile: configFiles(strings.Replace(commandConfigYAML, "  token: yaml-token-placeholder\n", "", 1), commandGroupsCSV, commandTargetsCSV)},
			"warpgate.token",
		},
		"bad insecure": {
			configuredCommandArgs("plan"),
			dependencies{getenv: func(string) string { return "" }, openFile: configFiles(strings.Replace(commandConfigYAML, "  insecure_skip_verify: true", "  insecure_skip_verify: maybe", 1), commandGroupsCSV, commandTargetsCSV)},
			"Configuration validation failed",
		},
		"missing password env": {
			configuredCommandArgs("plan"),
			testDependencies(commandGroupsCSV, strings.Replace(commandTargetsCSV, "password-placeholder,,required", ",MYSQL_A_PASSWORD,required", 1), &commandAPI{}),
			"MYSQL_A_PASSWORD",
		},
		"client": {
			configuredCommandArgs("plan"),
			dependencies{
				getenv:   func(string) string { return "" },
				openFile: configFiles(commandConfigYAML, commandGroupsCSV, commandTargetsCSV),
				newAPI:   func(warpgate.Config) (reconcile.API, error) { return nil, errors.New("client failed") },
			},
			"client failed",
		},
		"open config": {
			[]string{"validate", "--config", "missing.yaml"},
			dependencies{getenv: func(string) string { return "" }, openFile: func(string) (io.ReadCloser, error) { return nil, errors.New("missing config") }},
			"missing config",
		},
		"parse config": {
			[]string{"validate", "--config", "config.yaml"},
			dependencies{getenv: func(string) string { return "" }, openFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("invalid: true\n")), nil }, newAPI: func(warpgate.Config) (reconcile.API, error) { return &commandAPI{}, nil }},
			"Configuration validation failed",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stdout, stderr := &strings.Builder{}, &strings.Builder{}
			code := run(context.Background(), tt.args, tt.deps, stdout, stderr)
			if code == 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("run() = %d, stdout = %q, stderr = %q, want %q", code, stdout, stderr, tt.want)
			}
		})
	}
}

func TestRunAcceptsInsecureTLSConfiguration(t *testing.T) {
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	deps := testDependencies(commandGroupsCSV, commandTargetsCSV, &commandAPI{})
	deps.newAPI = func(config warpgate.Config) (reconcile.API, error) {
		if !config.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify = false")
		}
		return &commandAPI{}, nil
	}
	if code := run(context.Background(), configuredCommandArgs("plan"), deps, stdout, stderr); code != 0 {
		t.Fatalf("run() = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func commandArgs(command string) []string {
	return []string{command, "--groups", "groups.csv", "--targets", "targets.csv"}
}

func configuredCommandArgs(command string) []string {
	return []string{command, "--config", "config/warpgate-admin.yaml"}
}

func testDependencies(groupsCSV, targetsCSV string, api reconcile.API) dependencies {
	return dependencies{
		getenv:   mapEnv(map[string]string{}),
		openFile: configFiles(commandConfigYAML, groupsCSV, targetsCSV),
		newAPI: func(warpgate.Config) (reconcile.API, error) {
			return api, nil
		},
	}
}

func csvFiles(groupsCSV, targetsCSV string) func(string) (io.ReadCloser, error) {
	return func(name string) (io.ReadCloser, error) {
		switch name {
		case "groups.csv":
			return io.NopCloser(strings.NewReader(groupsCSV)), nil
		case "targets.csv":
			return io.NopCloser(strings.NewReader(targetsCSV)), nil
		default:
			return nil, errors.New("unexpected file")
		}
	}
}

func configFiles(configYAML, groupsCSV, targetsCSV string) func(string) (io.ReadCloser, error) {
	return func(name string) (io.ReadCloser, error) {
		switch name {
		case "config/warpgate-admin.yaml":
			return io.NopCloser(strings.NewReader(configYAML)), nil
		case "config/groups.csv", "groups.csv":
			return io.NopCloser(strings.NewReader(groupsCSV)), nil
		case "config/targets.csv", "targets.csv":
			return io.NopCloser(strings.NewReader(targetsCSV)), nil
		default:
			return nil, errors.New("unexpected file " + name)
		}
	}
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

type commandAPI struct {
	groups             []warpgate.TargetGroup
	targets            []warpgate.Target
	createdGroups      []warpgate.GroupRequest
	createdTargets     []warpgate.TargetRequest
	updatedTargets     []warpgate.TargetRequest
	deletedTargets     []string
	listGroupsErr      error
	createGroupErr     error
	roles              []warpgate.Role
	targetRoles        map[string][]warpgate.Role
	addedTargetRoles   []commandTargetRoleAssignment
	removedTargetRoles []commandTargetRoleAssignment
}

type commandTargetRoleAssignment struct {
	TargetID string
	RoleID   string
}

func (api *commandAPI) ListGroups(context.Context) ([]warpgate.TargetGroup, error) {
	return api.groups, api.listGroupsErr
}

func (api *commandAPI) CreateGroup(_ context.Context, request warpgate.GroupRequest) (warpgate.TargetGroup, error) {
	if api.createGroupErr != nil {
		return warpgate.TargetGroup{}, api.createGroupErr
	}
	api.createdGroups = append(api.createdGroups, request)
	return warpgate.TargetGroup{ID: "group-id", Name: request.Name}, nil
}

func (api *commandAPI) UpdateGroup(_ context.Context, id string, request warpgate.GroupRequest) (warpgate.TargetGroup, error) {
	return warpgate.TargetGroup{ID: id, Name: request.Name}, nil
}

func (api *commandAPI) ListTargets(context.Context) ([]warpgate.Target, error) {
	return api.targets, nil
}

func (api *commandAPI) CreateTarget(_ context.Context, request warpgate.TargetRequest) (warpgate.Target, error) {
	api.createdTargets = append(api.createdTargets, request)
	return warpgate.Target{ID: "target-id", Name: request.Name}, nil
}

func (api *commandAPI) UpdateTarget(_ context.Context, id string, request warpgate.TargetRequest) (warpgate.Target, error) {
	api.updatedTargets = append(api.updatedTargets, request)
	return warpgate.Target{ID: id, Name: request.Name}, nil
}

func (api *commandAPI) DeleteTarget(_ context.Context, id string) error {
	api.deletedTargets = append(api.deletedTargets, id)
	return nil
}

func (api *commandAPI) ListRoles(context.Context) ([]warpgate.Role, error) {
	return api.roles, nil
}

func (api *commandAPI) ListTargetRoles(_ context.Context, targetID string) ([]warpgate.Role, error) {
	return api.targetRoles[targetID], nil
}

func (api *commandAPI) AddTargetRole(_ context.Context, targetID, roleID string) error {
	api.addedTargetRoles = append(api.addedTargetRoles, commandTargetRoleAssignment{TargetID: targetID, RoleID: roleID})
	return nil
}

func (api *commandAPI) RemoveTargetRole(_ context.Context, targetID, roleID string) error {
	api.removedTargetRoles = append(api.removedTargetRoles, commandTargetRoleAssignment{TargetID: targetID, RoleID: roleID})
	return nil
}

func mysqlCommandTarget(id, name, groupID, host string) warpgate.Target {
	options, _ := json.Marshal(warpgate.MySQLOptions{
		Kind: warpgate.MySQLKind, Host: host, Port: 3306, Username: "db_user",
		Auth: &warpgate.DatabaseAuth{Kind: warpgate.PasswordAuthKind, Password: "password-placeholder"},
		TLS:  warpgate.TLS{Mode: warpgate.TLSRequired, Verify: true},
	})
	return warpgate.Target{ID: id, Name: name, GroupID: groupID, Options: options}
}
