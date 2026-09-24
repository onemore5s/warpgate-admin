package inventory

import (
	"strings"
	"testing"
)

const httpYAML = `targets:
  - name: web-a
    group: environment-a
    url: https://service.example.invalid/base
    roles: [web-readers]
    external_host: web.example.invalid
    headers:
      X-Example: placeholder
    headers_env:
      Authorization: HTTP_AUTHORIZATION
  - name: web-b
    group: environment-b
    url: http://service.example.invalid:8080
    roles: []
  - enabled: false
`

func TestParseHTTPAndResolveHeaders(t *testing.T) {
	got, err := ParseSources(strings.NewReader(validGroupsCSV), strings.NewReader(validTargetsCSV), strings.NewReader(httpYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 4 {
		t.Fatalf("Target count: %d", len(got.Targets))
	}
	a, b := got.Targets[2], got.Targets[3]
	if a.Engine != EngineHTTP || a.HTTP.URL != "https://service.example.invalid/base" || a.TLSMode != TLSRequired || !a.TLSVerify || !a.RolesSpecified || !b.RolesSpecified || b.TLSMode != TLSDisabled {
		t.Fatalf("Incorrect HTTP configuration: %+v / %+v", a, b)
	}
	resolved, err := ResolveSecrets(got, func(string) string { return "Bearer placeholder" })
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Targets[2].HTTP.Headers["Authorization"] != "Bearer placeholder" || got.Targets[2].HTTP.Headers["Authorization"] != "" {
		t.Fatal("Environment variables were not resolved or the original input was mutated")
	}
	_, err = ResolveSecrets(got, func(string) string { return "" })
	if err == nil {
		t.Fatal("Missing environment variables must fail")
	}
}

func TestHTTPValidation(t *testing.T) {
	base := "targets:\n  - name: web-a\n    group: environment-a\n    url: https://service.example.invalid\n"
	tests := map[string]string{
		"unknown field":                     base + "    unexpected: true\n",
		"missing target list":               "{}",
		"empty document":                    "",
		"multiple documents":                base + "---\ntargets: []\n",
		"malformed document":                "targets: [",
		"duplicate name":                    base + "  - name: web-a\n    group: environment-a\n    url: https://other.example.invalid\n",
		"duplicate name across inventories": strings.Replace(base, "web-a", "mysql-a", 1),
		"unknown group":                     strings.Replace(base, "environment-a", "unknown", 1),
		"empty name":                        strings.Replace(base, "web-a", "''", 1),
		"relative URL":                      strings.Replace(base, "https://service.example.invalid", "/relative", 1),
		"non-HTTP URL":                      strings.Replace(base, "https://", "ftp://", 1),
		"URL credentials":                   strings.Replace(base, "https://", "https://user:secret@", 1),
		"URL fragment":                      strings.Replace(base, "service.example.invalid", "service.example.invalid/#fragment", 1),
		"invalid port":                      strings.Replace(base, "service.example.invalid", "service.example.invalid:99999", 1),
		"TLS conflict":                      base + "    tls: {mode: disabled}\n",
		"HTTP and TLS conflict":             strings.Replace(base, "https://", "http://", 1) + "    tls: {mode: required}\n",
		"invalid TLS mode":                  base + "    tls: {mode: invalid}\n",
		"domain includes scheme":            base + "    external_host: https://web.example.invalid\n",
		"domain includes port":              base + "    external_host: web.example.invalid:443\n",
		"domain too long":                   base + "    external_host: " + strings.Repeat("a.", maxDomainLength) + "invalid\n",
		"duplicate role":                    base + "    roles: [readers, readers]\n",
		"empty role":                        base + "    roles: ['']\n",
		"invalid header name":               base + "    headers: {'Bad Header': placeholder}\n",
		"duplicate header":                  base + "    headers: {Authorization: a, authorization: b}\n",
		"conflicting header sources":        base + "    headers: {Authorization: a}\n    headers_env: {authorization: HTTP_AUTHORIZATION}\n",
		"invalid environment variable":      base + "    headers_env: {Authorization: BAD-NAME}\n",
		"newline injection":                 base + "    headers: {Authorization: \"secret\\r\\nInjected: true\"}\n",
		"decode error redaction":            base + "    tls: {verify: secret-placeholder}\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSources(strings.NewReader(validGroupsCSV), strings.NewReader(validTargetsCSV), strings.NewReader(input))
			if err == nil {
				t.Fatal("Expected validation to fail")
			}
			if strings.Contains(err.Error(), "secret-placeholder") || strings.Contains(err.Error(), "user:secret") {
				t.Fatalf("Error exposed sensitive content: %v", err)
			}
		})
	}
}

func TestHTTPOptionalFieldsAndEmptyInventory(t *testing.T) {
	for _, input := range []string{"targets: []", "targets:\n  - enabled: false\n"} {
		got, err := ParseSources(strings.NewReader(validGroupsCSV), nil, strings.NewReader(input))
		if err != nil || len(got.Targets) != 0 {
			t.Fatalf("Empty inventory: %+v %v", got, err)
		}
	}
	got, err := ParseSources(strings.NewReader(validGroupsCSV), nil, strings.NewReader("targets:\n  - name: web\n    group: environment-a\n    url: https://service.example.invalid\n    tls: {mode: preferred, verify: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[0].RolesSpecified || got.Targets[0].HTTP.HeadersSpecified || got.Targets[0].TLSVerify || got.Targets[0].TLSMode != TLSPreferred {
		t.Fatal("Incorrect optional field state")
	}
	_, err = ResolveSecrets(Inventory{Targets: []Target{{Engine: EngineHTTP, HTTP: &HTTP{HeadersEnv: map[string]string{"Authorization": "AUTH"}}}}}, func(string) string { return "secret\r\n" })
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Environment variable newline validation: %v", err)
	}
	_, err = ResolveSecrets(Inventory{Targets: []Target{{Engine: EngineHTTP, HTTP: &HTTP{HeadersEnv: map[string]string{"Authorization": "AUTH"}}}}}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "AUTH") {
		t.Fatalf("Missing HTTP environment variables must fail: %v", err)
	}
	got, err = ParseSources(strings.NewReader(validGroupsCSV), nil, strings.NewReader("targets:\n  - name: web\n    group: environment-a\n    url: http://service.example.invalid\n    headers: {}\n    external_host: ''\n"))
	if err != nil || !got.Targets[0].HTTP.HeadersSpecified || got.Targets[0].HTTP.ExternalHost == nil || *got.Targets[0].HTTP.ExternalHost != "" {
		t.Fatalf("Explicitly cleared fields were not preserved: %v", err)
	}
}
