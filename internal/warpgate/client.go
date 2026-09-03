package warpgate

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	adminAPIPath     = "/@warpgate/admin/api"
	defaultTimeout   = 30 * time.Second
	maxErrorBodySize = 64 * 1024
	userAgent        = "warpgate-admin"

	MySQLKind        = "MySql"
	PostgresKind     = "Postgres"
	PasswordAuthKind = "Password"
	TLSDisabled      = "Disabled"
	TLSPreferred     = "Preferred"
	TLSRequired      = "Required"
)

type Config struct {
	Host               string
	Token              string
	Timeout            time.Duration
	InsecureSkipVerify bool
}

type TargetGroup struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Color       *string `json:"color"`
}

type GroupRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Color       *string `json:"color,omitempty"`
}

type Role struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

type TLS struct {
	Mode   string `json:"mode"`
	Verify bool   `json:"verify"`
}

type DatabaseAuth struct {
	Kind     string `json:"kind"`
	Password string `json:"password"`
}

type MySQLOptions struct {
	Kind                string        `json:"kind"`
	Host                string        `json:"host"`
	Port                int           `json:"port"`
	Username            string        `json:"username"`
	Auth                *DatabaseAuth `json:"auth,omitempty"`
	LegacyPassword      string        `json:"password,omitempty"`
	TLS                 TLS           `json:"tls"`
	DefaultDatabaseName string        `json:"default_database_name,omitempty"`
}

type PostgresOptions struct {
	Kind                string        `json:"kind"`
	Host                string        `json:"host"`
	Port                int           `json:"port"`
	Username            string        `json:"username"`
	Auth                *DatabaseAuth `json:"auth,omitempty"`
	LegacyPassword      string        `json:"password,omitempty"`
	TLS                 TLS           `json:"tls"`
	DefaultDatabaseName string        `json:"default_database_name,omitempty"`
	ProtocolVersion     string        `json:"protocol_version,omitempty"`
	IdleTimeout         *string       `json:"idle_timeout,omitempty"`
}

type Target struct {
	ID                       string          `json:"id"`
	Name                     string          `json:"name"`
	Description              string          `json:"description"`
	GroupID                  string          `json:"group_id"`
	Options                  json.RawMessage `json:"options"`
	RateLimitBytesPerSecond  *int64          `json:"rate_limit_bytes_per_second"`
	TicketMaxDurationSeconds *int64          `json:"ticket_max_duration_seconds"`
	TicketRequestsDisabled   *bool           `json:"ticket_requests_disabled"`
	TicketRequireApproval    *bool           `json:"ticket_require_approval"`
	TicketMaxUses            *int            `json:"ticket_max_uses"`
}

type TargetRequest struct {
	Name                     string `json:"name"`
	Description              string `json:"description,omitempty"`
	GroupID                  string `json:"group_id,omitempty"`
	Options                  any    `json:"options"`
	RateLimitBytesPerSecond  *int64 `json:"rate_limit_bytes_per_second,omitempty"`
	TicketMaxDurationSeconds *int64 `json:"ticket_max_duration_seconds,omitempty"`
	TicketRequestsDisabled   *bool  `json:"ticket_requests_disabled,omitempty"`
	TicketRequireApproval    *bool  `json:"ticket_require_approval,omitempty"`
	TicketMaxUses            *int   `json:"ticket_max_uses,omitempty"`
}

type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.Host) == "" {
		return nil, fmt.Errorf("Warpgate host is required")
	}
	baseURL, err := url.Parse(config.Host)
	if err != nil {
		return nil, fmt.Errorf("parse Warpgate host: %w", err)
	}
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, fmt.Errorf("Warpgate host must be an absolute HTTP(S) URL")
	}
	baseURL.RawQuery = ""
	baseURL.Fragment = ""
	baseURL.Path = strings.TrimSuffix(baseURL.Path, "/")
	if !strings.HasSuffix(baseURL.Path, adminAPIPath) {
		baseURL.Path += adminAPIPath
	}

	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.InsecureSkipVerify}

	return &Client{
		baseURL: baseURL,
		token:   config.Token,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

func (client *Client) ListGroups(ctx context.Context) ([]TargetGroup, error) {
	var result []TargetGroup
	if err := client.do(ctx, http.MethodGet, "/target-groups", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (client *Client) CreateGroup(ctx context.Context, request GroupRequest) (TargetGroup, error) {
	var result TargetGroup
	err := client.do(ctx, http.MethodPost, "/target-groups", request, &result)
	return result, err
}

func (client *Client) UpdateGroup(ctx context.Context, id string, request GroupRequest) (TargetGroup, error) {
	var result TargetGroup
	err := client.do(ctx, http.MethodPut, "/target-groups/"+url.PathEscape(id), request, &result)
	return result, err
}

func (client *Client) ListTargets(ctx context.Context) ([]Target, error) {
	var result []Target
	if err := client.do(ctx, http.MethodGet, "/targets", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (client *Client) CreateTarget(ctx context.Context, request TargetRequest) (Target, error) {
	var result Target
	err := client.do(ctx, http.MethodPost, "/targets", request, &result)
	return result, err
}

func (client *Client) UpdateTarget(ctx context.Context, id string, request TargetRequest) (Target, error) {
	var result Target
	err := client.do(ctx, http.MethodPut, "/targets/"+url.PathEscape(id), request, &result)
	return result, err
}

func (client *Client) DeleteTarget(ctx context.Context, id string) error {
	return client.do(ctx, http.MethodDelete, "/targets/"+url.PathEscape(id), nil, nil)
}

func (client *Client) ListRoles(ctx context.Context) ([]Role, error) {
	var result []Role
	if err := client.do(ctx, http.MethodGet, "/roles", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (client *Client) ListTargetRoles(ctx context.Context, targetID string) ([]Role, error) {
	var result []Role
	endpoint := "/targets/" + url.PathEscape(targetID) + "/roles"
	if err := client.do(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (client *Client) AddTargetRole(ctx context.Context, targetID, roleID string) error {
	endpoint := "/targets/" + url.PathEscape(targetID) + "/roles/" + url.PathEscape(roleID)
	return client.do(ctx, http.MethodPost, endpoint, nil, nil)
}

func (client *Client) RemoveTargetRole(ctx context.Context, targetID, roleID string) error {
	endpoint := "/targets/" + url.PathEscape(targetID) + "/roles/" + url.PathEscape(roleID)
	return client.do(ctx, http.MethodDelete, endpoint, nil, nil)
}

func (client *Client) do(ctx context.Context, method, endpoint string, requestBody, result any) error {
	requestURL := *client.baseURL
	requestURL.Path = path.Join(client.baseURL.Path, endpoint)

	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode %s %s request: %w", method, endpoint, err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return fmt.Errorf("create %s %s request: %w", method, endpoint, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)
	if client.token != "" {
		request.Header.Set("X-Warpgate-Token", client.token)
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send %s %s request: %w", method, endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBodySize))
		if readErr != nil {
			return fmt.Errorf("Warpgate API %s %s returned %d and the response body could not be read: %w", method, endpoint, response.StatusCode, readErr)
		}
		return fmt.Errorf("Warpgate API %s %s returned %d: %s", method, endpoint, response.StatusCode, strings.TrimSpace(string(message)))
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
	}
	return nil
}
