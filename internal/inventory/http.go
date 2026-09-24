package inventory

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	httpScheme           = "http"
	httpsScheme          = "https"
	maxPort              = 65535
	maxDomainLength      = 253
	maxDomainLabelLength = 63
)

var headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var domainLabelPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

type HTTP struct {
	URL              string
	ExternalHost     *string
	Headers          map[string]string
	HeadersEnv       map[string]string
	HeadersSpecified bool
}

type httpEntry struct {
	Enabled      *bool             `yaml:"enabled"`
	Name         string            `yaml:"name"`
	Group        string            `yaml:"group"`
	Description  string            `yaml:"description"`
	Roles        []string          `yaml:"roles"`
	URL          string            `yaml:"url"`
	ExternalHost *string           `yaml:"external_host"`
	Headers      map[string]string `yaml:"headers"`
	HeadersEnv   map[string]string `yaml:"headers_env"`
	TLS          struct {
		Mode   string `yaml:"mode"`
		Verify *bool  `yaml:"verify"`
	} `yaml:"tls"`
}

func parseHTTP(input io.Reader, groups []Group) ([]Target, error) {
	decoder := yaml.NewDecoder(input)
	decoder.KnownFields(true)
	var document struct {
		Targets *[]httpEntry `yaml:"targets"`
	}
	if err := decoder.Decode(&document); err != nil {
		// YAML type errors may contain raw header values; do not log parser errors directly.
		return nil, errors.New("Invalid HTTP YAML: check field names, types, and syntax")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("HTTP inventory must contain exactly one YAML document")
	}
	if document.Targets == nil {
		return nil, errors.New("A targets list is required; use targets: [] for an empty inventory")
	}
	groupNames := make(map[string]bool)
	for _, group := range groups {
		groupNames[group.Name] = true
	}
	var result []Target
	for i, entry := range *document.Targets {
		if entry.Enabled != nil && !*entry.Enabled {
			continue
		}
		target, err := entry.target(groupNames)
		if err != nil {
			return nil, fmt.Errorf("HTTP target %d: %w", i+1, err)
		}
		result = append(result, target)
	}
	return result, nil
}

func (entry httpEntry) target(groups map[string]bool) (Target, error) {
	entry.Name, entry.Group = strings.TrimSpace(entry.Name), strings.TrimSpace(entry.Group)
	if err := validateName("name", entry.Name); err != nil {
		return Target{}, err
	}
	if !groups[entry.Group] {
		return Target{}, fmt.Errorf("unknown group %q", entry.Group)
	}
	roles := make([]string, 0, len(entry.Roles))
	seenRoles := make(map[string]bool)
	for _, raw := range entry.Roles {
		role := strings.TrimSpace(raw)
		if err := validateName("role", role); err != nil {
			return Target{}, err
		}
		if seenRoles[role] {
			return Target{}, fmt.Errorf("duplicate role %q", role)
		}
		seenRoles[role] = true
		roles = append(roles, role)
	}
	u, err := url.Parse(entry.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != httpScheme && u.Scheme != httpsScheme) || u.User != nil || u.Fragment != "" {
		return Target{}, errors.New("url must be an absolute HTTP(S) URL without a username, password, or fragment")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > maxPort {
			return Target{}, errors.New("url port must be between 1 and 65535")
		}
	}
	mode, err := parseTLSMode(entry.TLS.Mode)
	if err != nil {
		return Target{}, errors.New("tls.mode must be disabled, preferred, or required")
	}
	if entry.TLS.Mode == "" && u.Scheme == httpScheme {
		mode = TLSDisabled
	}
	if (u.Scheme == httpsScheme && mode == TLSDisabled) || (u.Scheme == httpScheme && mode != TLSDisabled) {
		return Target{}, errors.New("url scheme conflicts with tls.mode: use disabled for HTTP and required or preferred for HTTPS")
	}
	verify := entry.TLS.Verify == nil || *entry.TLS.Verify
	if entry.ExternalHost != nil && *entry.ExternalHost != "" && !validDomain(*entry.ExternalHost) {
		return Target{}, errors.New("external_host must be a bare domain without a scheme, port, or path")
	}
	headers, env, err := normalizeHeaders(entry.Headers, entry.HeadersEnv)
	if err != nil {
		return Target{}, err
	}
	return Target{Name: entry.Name, Group: entry.Group, Description: entry.Description, Roles: roles, RolesSpecified: entry.Roles != nil,
		Engine: EngineHTTP, TLSMode: mode, TLSVerify: verify,
		HTTP: &HTTP{URL: entry.URL, ExternalHost: entry.ExternalHost, Headers: headers, HeadersEnv: env, HeadersSpecified: entry.Headers != nil || entry.HeadersEnv != nil}}, nil
}

func validDomain(host string) bool {
	if len(host) > maxDomainLength {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > maxDomainLabelLength || !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func normalizeHeaders(values, variables map[string]string) (map[string]string, map[string]string, error) {
	headers, env := make(map[string]string), make(map[string]string)
	seen := make(map[string]bool)
	for index, source := range []map[string]string{values, variables} {
		for key, value := range source {
			if !headerNamePattern.MatchString(key) {
				return nil, nil, errors.New("Invalid header name")
			}
			name := http.CanonicalHeaderKey(key)
			if seen[name] {
				return nil, nil, fmt.Errorf("Header %s is duplicated or configured in both headers and headers_env", name)
			}
			seen[name] = true
			if index == 0 {
				if !validHeaderValue(value) {
					return nil, nil, fmt.Errorf("Header %s contains invalid control characters", name)
				}
				headers[name] = value
			} else {
				if !environmentVariablePattern.MatchString(value) {
					return nil, nil, fmt.Errorf("Header %s references an invalid environment variable name", name)
				}
				env[name] = value
			}
		}
	}
	return headers, env, nil
}

func validHeaderValue(value string) bool {
	for _, c := range []byte(value) {
		if (c < ' ' && c != '\t') || c == 127 {
			return false
		}
	}
	return true
}

func ResolveSecrets(input Inventory, getenv func(string) string) (Inventory, error) {
	result, err := ResolvePasswords(input, getenv)
	if err != nil {
		return Inventory{}, err
	}
	for i := range result.Targets {
		target := &result.Targets[i]
		if target.HTTP == nil {
			continue
		}
		options := *target.HTTP
		options.Headers = maps.Clone(options.Headers)
		if options.Headers == nil {
			options.Headers = make(map[string]string)
		}
		for header, variable := range options.HeadersEnv {
			value := getenv(variable)
			if value == "" {
				return Inventory{}, fmt.Errorf("target %q: environment variable %s is empty or unset", target.Name, variable)
			}
			if !validHeaderValue(value) {
				return Inventory{}, fmt.Errorf("target %q: environment variable %s contains invalid header control characters", target.Name, variable)
			}
			options.Headers[header] = value
		}
		target.HTTP = &options
	}
	return result, nil
}
