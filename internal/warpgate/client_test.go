package warpgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClientGroupLifecycle(t *testing.T) {
	var requests []string
	client, err := NewClient(Config{Host: "https://warpgate.example.invalid", Token: "token-placeholder", InsecureSkipVerify: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("X-Warpgate-Token"); got != "token-placeholder" {
			t.Errorf("X-Warpgate-Token = %q", got)
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /@warpgate/admin/api/target-groups":
			return jsonResponse(t, http.StatusOK, []TargetGroup{{ID: "group-id", Name: "environment-a"}}), nil
		case "POST /@warpgate/admin/api/target-groups":
			return jsonResponse(t, http.StatusCreated, TargetGroup{ID: "created-id", Name: "environment-b"}), nil
		case "PUT /@warpgate/admin/api/target-groups/group-id":
			return jsonResponse(t, http.StatusOK, TargetGroup{ID: "group-id", Name: "environment-a", Description: "Updated"}), nil
		default:
			return jsonResponse(t, http.StatusNotFound, "not found"), nil
		}
	})
	ctx := context.Background()
	groups, err := client.ListGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("ListGroups() = %#v, %v", groups, err)
	}
	created, err := client.CreateGroup(ctx, GroupRequest{Name: "environment-b"})
	if err != nil || created.ID != "created-id" {
		t.Fatalf("CreateGroup() = %#v, %v", created, err)
	}
	updated, err := client.UpdateGroup(ctx, "group-id", GroupRequest{Name: "environment-a", Description: "Updated"})
	if err != nil || updated.Description != "Updated" {
		t.Fatalf("UpdateGroup() = %#v, %v", updated, err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %v", requests)
	}
}

func TestClientTargetLifecycle(t *testing.T) {
	client, err := NewClient(Config{Host: "https://warpgate.example.invalid/custom/@warpgate/admin/api", Token: "token-placeholder"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method + " " + r.URL.Path {
		case "GET /custom/@warpgate/admin/api/targets":
			return jsonResponse(t, http.StatusOK, []Target{{ID: "target-id", Name: "target-a", Options: json.RawMessage(`{"kind":"MySql","host":"mysql-a.example.invalid","port":3306,"username":"db_user","password":"password-placeholder","tls":{"mode":"Required","verify":true}}`)}}), nil
		case "POST /custom/@warpgate/admin/api/targets":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			encoded := string(body)
			if !strings.Contains(encoded, `"auth":{"kind":"Password","password":"password-placeholder"}`) || strings.Contains(encoded, `"options":{"kind":"MySql","host":"mysql-b.example.invalid","port":3306,"username":"db_user","password":`) {
				t.Fatalf("request body = %s", encoded)
			}
			return jsonResponse(t, http.StatusCreated, Target{ID: "created-id", Name: "target-b"}), nil
		case "PUT /custom/@warpgate/admin/api/targets/target-id":
			return jsonResponse(t, http.StatusOK, Target{ID: "target-id", Name: "target-a", Description: "Updated"}), nil
		case "DELETE /custom/@warpgate/admin/api/targets/target-id":
			return textResponse(http.StatusNoContent, ""), nil
		default:
			return jsonResponse(t, http.StatusNotFound, "not found"), nil
		}
	})
	ctx := context.Background()
	targets, err := client.ListTargets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListTargets() = %#v, %v", targets, err)
	}
	req := TargetRequest{Name: "target-b", Options: MySQLOptions{Kind: MySQLKind, Host: "mysql-b.example.invalid", Port: 3306, Username: "db_user", Auth: &DatabaseAuth{Kind: PasswordAuthKind, Password: "password-placeholder"}, TLS: TLS{Mode: TLSRequired, Verify: true}}}
	created, err := client.CreateTarget(ctx, req)
	if err != nil || created.ID != "created-id" {
		t.Fatalf("CreateTarget() = %#v, %v", created, err)
	}
	req.Name, req.Description = "target-a", "Updated"
	updated, err := client.UpdateTarget(ctx, "target-id", req)
	if err != nil || updated.Description != "Updated" {
		t.Fatalf("UpdateTarget() = %#v, %v", updated, err)
	}
	if err := client.DeleteTarget(ctx, "target-id"); err != nil {
		t.Fatalf("DeleteTarget() error = %v", err)
	}
}

func TestClientTargetRoleLifecycle(t *testing.T) {
	client, err := NewClient(Config{Host: "https://warpgate.example.invalid", Token: "token-placeholder"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method + " " + r.URL.Path {
		case "GET /@warpgate/admin/api/roles":
			return jsonResponse(t, http.StatusOK, []Role{{ID: "role-id", Name: "database-readers"}}), nil
		case "GET /@warpgate/admin/api/targets/target-id/roles":
			return jsonResponse(t, http.StatusOK, []Role{{ID: "role-id", Name: "database-readers"}}), nil
		case "POST /@warpgate/admin/api/targets/target-id/roles/role-id":
			return textResponse(http.StatusCreated, ""), nil
		case "DELETE /@warpgate/admin/api/targets/target-id/roles/role-id":
			return textResponse(http.StatusNoContent, ""), nil
		default:
			return jsonResponse(t, http.StatusNotFound, "not found"), nil
		}
	})

	ctx := context.Background()
	roles, err := client.ListRoles(ctx)
	if err != nil || len(roles) != 1 || roles[0].Name != "database-readers" {
		t.Fatalf("ListRoles() = %#v, %v", roles, err)
	}
	targetRoles, err := client.ListTargetRoles(ctx, "target-id")
	if err != nil || len(targetRoles) != 1 || targetRoles[0].ID != "role-id" {
		t.Fatalf("ListTargetRoles() = %#v, %v", targetRoles, err)
	}
	if err := client.AddTargetRole(ctx, "target-id", "role-id"); err != nil {
		t.Fatalf("AddTargetRole() error = %v", err)
	}
	if err := client.RemoveTargetRole(ctx, "target-id", "role-id"); err != nil {
		t.Fatalf("RemoveTargetRole() error = %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	t.Run("config", func(t *testing.T) {
		for _, cfg := range []Config{{}, {Host: "://bad"}, {Host: "ftp://warpgate.example.invalid"}} {
			if _, err := NewClient(cfg); err == nil {
				t.Fatalf("NewClient(%#v) succeeded", cfg)
			}
		}
	})

	t.Run("marshal", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.do(context.Background(), http.MethodPost, "/targets", make(chan int), nil); err == nil || !strings.Contains(err.Error(), "encode") {
			t.Fatalf("do() error = %v", err)
		}
	})

	t.Run("request", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.baseURL = &url.URL{Scheme: ":bad"}
		if err := client.do(context.Background(), http.MethodGet, "/targets", nil, nil); err == nil || !strings.Contains(err.Error(), "create") {
			t.Fatalf("do() error = %v", err)
		}
	})

	t.Run("api status", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return textResponse(http.StatusForbidden, "denied"), nil
		})
		_, err = client.ListTargets(context.Background())
		if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "denied") {
			t.Fatalf("ListTargets() error = %v", err)
		}
	})

	t.Run("role api status", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return textResponse(http.StatusForbidden, "denied"), nil
		})
		if _, err := client.ListRoles(context.Background()); err == nil {
			t.Fatal("ListRoles() succeeded")
		}
		if _, err := client.ListTargetRoles(context.Background(), "target-id"); err == nil {
			t.Fatal("ListTargetRoles() succeeded")
		}
	})

	t.Run("error body read", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: errorReadCloser{}}, nil
		})
		if _, err := client.ListGroups(context.Background()); err == nil || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("ListGroups() error = %v", err)
		}
	})

	t.Run("no content", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return textResponse(http.StatusNoContent, ""), nil
		})
		var result Target
		if err := client.do(context.Background(), http.MethodGet, "/targets/id", nil, &result); err != nil {
			t.Fatalf("do() error = %v", err)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return textResponse(http.StatusOK, "not-json"), nil
		})
		if _, err := client.ListGroups(context.Background()); err == nil {
			t.Fatal("ListGroups() succeeded")
		}
	})

	t.Run("transport", func(t *testing.T) {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})
		if _, err := client.ListGroups(context.Background()); err == nil || !strings.Contains(err.Error(), "network unavailable") {
			t.Fatalf("ListGroups() error = %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorReadCloser) Close() error             { return nil }

func jsonResponse(t *testing.T, status int, value any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return textResponse(status, string(encoded))
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
