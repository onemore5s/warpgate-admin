package inventory

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxNameLength = 255
	roleDelimiter = ";"
)

type Engine string

const (
	EngineMySQL    Engine = "mysql"
	EnginePostgres Engine = "postgres"
	EngineHTTP     Engine = "http"
)

type TLSMode string

const (
	TLSDisabled  TLSMode = "Disabled"
	TLSPreferred TLSMode = "Preferred"
	TLSRequired  TLSMode = "Required"
)

type Group struct {
	Name        string
	Description string
	Color       string
}

type Target struct {
	Group               string
	Name                string
	Description         string
	Roles               []string
	RolesSpecified      bool
	Engine              Engine
	Host                string
	Port                int
	Username            string
	Password            string
	PasswordEnv         string
	TLSMode             TLSMode
	TLSVerify           bool
	DefaultDatabaseName string
	ProtocolVersion     string
	HTTP                *HTTP
}

type Inventory struct {
	Groups  []Group
	Targets []Target
}

var groupColumns = []string{"enabled", "name", "description", "color"}

var targetColumns = []string{
	"enabled",
	"group",
	"name",
	"description",
	"engine",
	"host",
	"port",
	"username",
	"password",
	"password_env",
	"tls_mode",
	"tls_verify",
	"default_database_name",
	"protocol_version",
}

var targetOptionalColumns = []string{"roles"}

var validColors = map[string]struct{}{
	"Primary":   {},
	"Secondary": {},
	"Success":   {},
	"Danger":    {},
	"Warning":   {},
	"Info":      {},
	"Light":     {},
	"Dark":      {},
}

var environmentVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Parse(groupsInput, targetsInput io.Reader) (Inventory, error) {
	return ParseSources(groupsInput, targetsInput, nil)
}

func ParseSources(groupsInput, databaseInput, httpInput io.Reader) (Inventory, error) {
	groups, err := parseGroups(groupsInput)
	if err != nil {
		return Inventory{}, fmt.Errorf("groups CSV: %w", err)
	}
	var targets []Target
	if databaseInput != nil {
		targets, err = parseTargets(databaseInput, groups)
		if err != nil {
			return Inventory{}, fmt.Errorf("database targets CSV: %w", err)
		}
	}
	if httpInput != nil {
		httpTargets, err := parseHTTP(httpInput, groups)
		if err != nil {
			return Inventory{}, fmt.Errorf("HTTP targets YAML: %w", err)
		}
		targets = append(targets, httpTargets...)
	}
	names := make(map[string]bool)
	for _, target := range targets {
		if names[target.Name] {
			return Inventory{}, fmt.Errorf("duplicate target %q across inventories", target.Name)
		}
		names[target.Name] = true
	}
	return Inventory{Groups: groups, Targets: targets}, nil
}

func ResolvePasswords(input Inventory, getenv func(string) string) (Inventory, error) {
	result := Inventory{
		Groups:  append([]Group(nil), input.Groups...),
		Targets: append([]Target(nil), input.Targets...),
	}
	for index := range result.Targets {
		target := &result.Targets[index]
		if target.PasswordEnv == "" {
			continue
		}
		password := getenv(target.PasswordEnv)
		if password == "" {
			return Inventory{}, fmt.Errorf("target %q: environment variable %s is empty or unset", target.Name, target.PasswordEnv)
		}
		target.Password = password
	}
	return result, nil
}

func parseGroups(input io.Reader) ([]Group, error) {
	reader, indexes, err := newCSVReader(input, groupColumns, nil)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0)
	names := make(map[string]int)
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read record: %w", readErr)
		}
		if blankRecord(record) {
			continue
		}
		line, _ := reader.FieldPos(0)
		row := rowAccessor(record, indexes)
		enabled, parseErr := parseOptionalBool(row("enabled"), true)
		if parseErr != nil {
			return nil, fmt.Errorf("line %d: enabled: %w", line, parseErr)
		}
		if !enabled {
			continue
		}
		name := row("name")
		if validateErr := validateName("name", name); validateErr != nil {
			return nil, fmt.Errorf("line %d: %w", line, validateErr)
		}
		if previousLine, exists := names[name]; exists {
			return nil, fmt.Errorf("line %d: duplicate group %q, first seen on line %d", line, name, previousLine)
		}
		color := row("color")
		if color != "" {
			if _, valid := validColors[color]; !valid {
				return nil, fmt.Errorf("line %d: color %q is invalid", line, color)
			}
		}
		names[name] = line
		groups = append(groups, Group{Name: name, Description: row("description"), Color: color})
	}
	return groups, nil
}

func parseTargets(input io.Reader, groups []Group) ([]Target, error) {
	reader, indexes, err := newCSVReader(input, targetColumns, targetOptionalColumns)
	if err != nil {
		return nil, err
	}
	_, rolesSpecified := indexes["roles"]
	groupNames := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		groupNames[group.Name] = struct{}{}
	}
	targets := make([]Target, 0)
	names := make(map[string]int)
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read record: %w", readErr)
		}
		if blankRecord(record) {
			continue
		}
		line, _ := reader.FieldPos(0)
		row := rowAccessor(record, indexes)
		enabled, parseErr := parseOptionalBool(row("enabled"), true)
		if parseErr != nil {
			return nil, fmt.Errorf("line %d: enabled: %w", line, parseErr)
		}
		if !enabled {
			continue
		}

		group := row("group")
		if _, exists := groupNames[group]; !exists {
			return nil, fmt.Errorf("line %d: unknown group %q", line, group)
		}
		name := row("name")
		if validateErr := validateName("name", name); validateErr != nil {
			return nil, fmt.Errorf("line %d: %w", line, validateErr)
		}
		if previousLine, exists := names[name]; exists {
			return nil, fmt.Errorf("line %d: duplicate target %q, first seen on line %d", line, name, previousLine)
		}
		roles, parseErr := parseRoles(row("roles"))
		if parseErr != nil {
			return nil, fmt.Errorf("line %d: roles: %w", line, parseErr)
		}

		engine := Engine(strings.ToLower(row("engine")))
		if engine != EngineMySQL && engine != EnginePostgres {
			return nil, fmt.Errorf("line %d: engine must be %q or %q", line, EngineMySQL, EnginePostgres)
		}
		host := row("host")
		if host == "" {
			return nil, fmt.Errorf("line %d: host is required", line)
		}
		port, parseErr := strconv.Atoi(row("port"))
		if parseErr != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("line %d: port must be an integer from 1 to 65535", line)
		}
		username := row("username")
		if username == "" {
			return nil, fmt.Errorf("line %d: username is required", line)
		}
		password := record[indexes["password"]]
		passwordEnv := row("password_env")
		if password == "" && passwordEnv == "" && engine != EngineMySQL {
			return nil, fmt.Errorf("line %d: password or password_env is required", line)
		}
		if password != "" && passwordEnv != "" {
			return nil, fmt.Errorf("line %d: only one of password and password_env may be set", line)
		}
		if passwordEnv != "" && !environmentVariablePattern.MatchString(passwordEnv) {
			return nil, fmt.Errorf("line %d: password_env %q is not a valid environment variable name", line, passwordEnv)
		}

		tlsMode, parseErr := parseTLSMode(row("tls_mode"))
		if parseErr != nil {
			return nil, fmt.Errorf("line %d: tls_mode: %w", line, parseErr)
		}
		tlsVerify, parseErr := parseOptionalBool(row("tls_verify"), true)
		if parseErr != nil {
			return nil, fmt.Errorf("line %d: tls_verify: %w", line, parseErr)
		}
		defaultDatabaseName := row("default_database_name")
		protocolVersion := row("protocol_version")
		if engine == EngineMySQL {
			if protocolVersion != "" {
				return nil, fmt.Errorf("line %d: protocol_version is only valid for postgres", line)
			}
		} else if protocolVersion != "" && protocolVersion != "3.0" && protocolVersion != "3.2" {
			return nil, fmt.Errorf("line %d: protocol_version must be 3.0 or 3.2", line)
		}

		names[name] = line
		targets = append(targets, Target{
			Group:               group,
			Name:                name,
			Description:         row("description"),
			Roles:               roles,
			RolesSpecified:      rolesSpecified,
			Engine:              engine,
			Host:                host,
			Port:                port,
			Username:            username,
			Password:            password,
			PasswordEnv:         passwordEnv,
			TLSMode:             tlsMode,
			TLSVerify:           tlsVerify,
			DefaultDatabaseName: defaultDatabaseName,
			ProtocolVersion:     protocolVersion,
		})
	}
	return targets, nil
}

func newCSVReader(input io.Reader, requiredColumns, optionalColumns []string) (*csv.Reader, map[string]int, error) {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("header is missing")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	indexes, err := validateHeader(header, requiredColumns, optionalColumns)
	if err != nil {
		return nil, nil, err
	}
	reader.FieldsPerRecord = len(header)
	return reader, indexes, nil
}

func validateHeader(header, requiredColumns, optionalColumns []string) (map[string]int, error) {
	wanted := make(map[string]struct{}, len(requiredColumns)+len(optionalColumns))
	for _, name := range requiredColumns {
		wanted[name] = struct{}{}
	}
	for _, name := range optionalColumns {
		wanted[name] = struct{}{}
	}
	indexes := make(map[string]int, len(header))
	for index, rawName := range header {
		name := strings.TrimSpace(rawName)
		if _, valid := wanted[name]; !valid {
			return nil, fmt.Errorf("unknown column %q", name)
		}
		if _, exists := indexes[name]; exists {
			return nil, fmt.Errorf("duplicate column %q", name)
		}
		indexes[name] = index
	}
	for _, name := range requiredColumns {
		if _, exists := indexes[name]; !exists {
			return nil, fmt.Errorf("missing required column %q", name)
		}
	}
	return indexes, nil
}

func parseRoles(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}

	values := strings.Split(value, roleDelimiter)
	roles := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		role := strings.TrimSpace(value)
		if role == "" {
			return nil, errors.New("must not contain an empty role name")
		}
		if err := validateName("role name", role); err != nil {
			return nil, err
		}
		if _, exists := seen[role]; exists {
			return nil, fmt.Errorf("duplicate role %q", role)
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	return roles, nil
}

func parseTLSMode(value string) (TLSMode, error) {
	if value == "" {
		return TLSRequired, nil
	}
	switch strings.ToLower(value) {
	case "disabled":
		return TLSDisabled, nil
	case "preferred":
		return TLSPreferred, nil
	case "required":
		return TLSRequired, nil
	default:
		return "", fmt.Errorf("must be disabled, preferred, or required, got %q", value)
	}
}

func parseOptionalBool(value string, defaultValue bool) (bool, error) {
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("must be true or false, got %q", value)
	}
	return parsed, nil
}

func validateName(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if utf8.RuneCountInString(value) > maxNameLength {
		return fmt.Errorf("%s must not exceed %d characters", field, maxNameLength)
	}
	return nil
}

func rowAccessor(record []string, indexes map[string]int) func(string) string {
	return func(name string) string {
		index, exists := indexes[name]
		if !exists {
			return ""
		}
		return strings.TrimSpace(record[index])
	}
}

func blankRecord(record []string) bool {
	for _, value := range record {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}
