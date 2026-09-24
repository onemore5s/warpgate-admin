package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/onemore5s/warpgate-admin/internal/reconcile"
	"github.com/onemore5s/warpgate-admin/internal/warpgate"
)

const commandHTTPYAML = `targets:
  - name: web-a
    group: environment-a
    url: https://service.example.invalid
    headers_env:
      Authorization: HTTP_AUTHORIZATION
`

func namedDependencies(configYAML, httpYAML string, api *commandAPI) dependencies {
	files := map[string]string{"config.yaml": configYAML, "groups.csv": commandGroupsCSV, "database.csv": commandTargetsCSV, "http.yaml": httpYAML}
	return dependencies{
		getenv: func(string) string { return "Bearer secret-placeholder" },
		openFile: func(name string) (io.ReadCloser, error) {
			data, ok := files[name]
			if !ok {
				return nil, errors.New("file not found")
			}
			return io.NopCloser(strings.NewReader(data)), nil
		},
		newAPI: func(warpgate.Config) (reconcile.API, error) { return api, nil },
	}
}

func TestRunHTTPOnlyValidateDoesNotResolveSecretsOrCallAPI(t *testing.T) {
	deps := namedDependencies("inventory:\n  groups: groups.csv\n  http_targets: http.yaml\n", commandHTTPYAML, &commandAPI{})
	deps.getenv = func(string) string { t.Fatal("validate must not read environment variables"); return "" }
	deps.newAPI = func(warpgate.Config) (reconcile.API, error) {
		t.Fatal("validate must not access the API")
		return nil, nil
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	if code := run(context.Background(), []string{"validate", "--config", "config.yaml"}, deps, stdout, stderr); code != 0 || !strings.Contains(stdout.String(), "1 target") {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
}

func TestRunNamedInventoriesFromConfig(t *testing.T) {
	for _, tt := range []struct {
		config   string
		wantCode int
		want     string
	}{
		{"inventory:\n  groups: groups.csv\n  database_targets: database.csv\n  http_targets: http.yaml\n", 0, "2 targets"},
		{"inventory:\n  groups: groups.csv\n  http_targets: missing.yaml\n", 1, "Failed to open HTTP targets inventory"},
		{"inventory:\n  groups: groups.csv\n", 1, "inventory.database_targets"},
	} {
		out, errOut := &strings.Builder{}, &strings.Builder{}
		code := run(context.Background(), []string{"validate", "--config", "config.yaml"}, namedDependencies(tt.config, commandHTTPYAML, &commandAPI{}), out, errOut)
		if code != tt.wantCode || !strings.Contains(out.String()+errOut.String(), tt.want) {
			t.Fatalf("%d %s %s", code, out, errOut)
		}
	}
}

func TestInventoryCommandsRequireConfigAndRejectOverrides(t *testing.T) {
	deps := dependencies{
		openFile: func(string) (io.ReadCloser, error) {
			t.Error("invalid arguments must fail before opening files")
			return nil, errors.New("unexpected file access")
		},
		getenv: func(string) string { t.Error("invalid arguments must not read environment variables"); return "" },
		newAPI: func(warpgate.Config) (reconcile.API, error) {
			t.Error("invalid arguments must not access the API")
			return nil, nil
		},
	}
	for _, command := range []string{"validate", "plan", "apply"} {
		for _, args := range [][]string{{command}, {command, "--config="}} {
			out, errOut := &strings.Builder{}, &strings.Builder{}
			if code := run(context.Background(), args, deps, out, errOut); code != 2 || !strings.Contains(errOut.String(), "--config") {
				t.Fatalf("%v: %d %s %s", args, code, out, errOut)
			}
		}
		for _, option := range []string{"groups", "database-targets", "http-targets", "targets", "update-passwords"} {
			t.Run(command+"/"+option, func(t *testing.T) {
				out, errOut := &strings.Builder{}, &strings.Builder{}
				args := []string{command, "--config", "config.yaml", "--" + option + "=ignored"}
				if code := run(context.Background(), args, deps, out, errOut); code != 2 || !strings.Contains(errOut.String(), "flag provided but not defined: -"+option) {
					t.Fatalf("%d %s %s", code, out, errOut)
				}
			})
		}
	}
}

func TestRunNamedSyncScopesAndApplyHTTP(t *testing.T) {
	connection := "warpgate:\n  host: https://warpgate.example.invalid\n  token: token-placeholder\n"
	for _, tt := range []struct {
		name, inventories, http string
		wantDeleted             int
		wantCreated             int
	}{
		{"HTTP inventory omitted", "  database_targets: database.csv\n", commandHTTPYAML, 1, 1},
		{"database inventory omitted", "  http_targets: http.yaml\n", commandHTTPYAML, 1, 1},
		{"explicitly empty HTTP inventory", "  http_targets: http.yaml\n", "targets: []", 1, 0},
		{"both inventory types", "  database_targets: database.csv\n  http_targets: http.yaml\n", commandHTTPYAML, 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &commandAPI{groups: []warpgate.TargetGroup{{ID: "group-a", Name: "environment-a"}}, targets: []warpgate.Target{mysqlCommandTarget("db-stale", "stale-db", "group-a", "db.example.invalid"), {ID: "http-stale", Name: "stale-http", GroupID: "group-a", Options: []byte(`{"kind":"Http"}`)}}}
			config := connection + "inventory:\n  groups: groups.csv\n" + tt.inventories + "apply:\n  sync_database_targets: true\n  sync_http_targets: true\n"
			out, errOut := &strings.Builder{}, &strings.Builder{}
			code := run(context.Background(), []string{"apply", "--config", "config.yaml"}, namedDependencies(config, tt.http, api), out, errOut)
			if code != 0 || len(api.deletedTargets) != tt.wantDeleted || len(api.createdTargets) != tt.wantCreated {
				t.Fatalf("%d %s %s; deleted %v, created %d", code, out, errOut, api.deletedTargets, len(api.createdTargets))
			}
			if strings.Contains(out.String()+errOut.String(), "secret-placeholder") {
				t.Fatal("Logs exposed credentials")
			}
			for _, request := range api.createdTargets {
				if options, ok := request.Options.(warpgate.HTTPOptions); ok && options.Headers["Authorization"] != "Bearer secret-placeholder" {
					t.Fatal("Credentials were not included in the request")
				}
			}
		})
	}
}
