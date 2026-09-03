package inventory

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

const validGroupsCSV = `enabled,name,description,color
true,environment-a,Environment A,Info
true,environment-b,Environment B,Danger
false,disabled-group,Disabled,Secondary
`

const validTargetsCSV = `enabled,group,name,description,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version
true,environment-a,mysql-a,MySQL RDS,mysql,mysql-a.example.invalid,3306,db_user,password-placeholder,,required,true,,
true,environment-b,postgres-a,PostgreSQL RDS,postgres,postgres-a.example.invalid,5432,db_user,,POSTGRES_A_PASSWORD,preferred,false,app_db,3.0
false,environment-a,disabled-target,Disabled,mysql,disabled.example.invalid,3306,db_user,password-placeholder,,required,true,,
`

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(validTargetsCSV))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got.Groups) != 2 || got.Groups[0] != (Group{Name: "environment-a", Description: "Environment A", Color: "Info"}) {
		t.Fatalf("Groups = %#v", got.Groups)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("Targets = %#v", got.Targets)
	}
	mysql := got.Targets[0]
	if mysql.Engine != EngineMySQL || mysql.Port != 3306 || mysql.Password != "password-placeholder" || mysql.TLSMode != TLSRequired || !mysql.TLSVerify {
		t.Fatalf("MySQL target = %#v", mysql)
	}
	postgres := got.Targets[1]
	if postgres.Engine != EnginePostgres || postgres.PasswordEnv != "POSTGRES_A_PASSWORD" || postgres.DefaultDatabaseName != "app_db" || postgres.ProtocolVersion != "3.0" || postgres.TLSMode != TLSPreferred || postgres.TLSVerify {
		t.Fatalf("PostgreSQL target = %#v", postgres)
	}
}

func TestParseDefaultsAndBOM(t *testing.T) {
	groups := "\ufeffenabled,name,description,color\n,environment-a,,\n,,,\n"
	targets := `enabled,group,name,description,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version
,environment-a,mysql-a,,mysql,mysql-a.example.invalid,3306,db_user,password-placeholder,,,,,
`
	got, err := Parse(strings.NewReader(groups), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got.Groups) != 1 || len(got.Targets) != 1 || got.Targets[0].TLSMode != TLSRequired || !got.Targets[0].TLSVerify {
		t.Fatalf("Inventory = %#v", got)
	}
}

func TestParsePreservesPasswordWhitespace(t *testing.T) {
	targets := strings.Replace(validTargetsCSV, "password-placeholder,,required", `"  password-placeholder  ",,required`, 1)
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Targets[0].Password != "  password-placeholder  " {
		t.Fatalf("Password = %q", got.Targets[0].Password)
	}
}

func TestParseAcceptsDisabledTLS(t *testing.T) {
	targets := strings.Replace(validTargetsCSV, "required,true", "disabled,false", 1)
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Targets[0].TLSMode != TLSDisabled || got.Targets[0].TLSVerify {
		t.Fatalf("Target = %#v", got.Targets[0])
	}
}

func TestParseAcceptsMySQLDefaultDatabase(t *testing.T) {
	targets := strings.Replace(validTargetsCSV, "required,true,,", "required,true,app_db,", 1)
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Targets[0].DefaultDatabaseName != "app_db" {
		t.Fatalf("DefaultDatabaseName = %q", got.Targets[0].DefaultDatabaseName)
	}
}

func TestParseAcceptsEmptyMySQLPassword(t *testing.T) {
	targets := strings.Replace(validTargetsCSV, "password-placeholder,,required", ",,required", 1)
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Targets[0].Engine != EngineMySQL || got.Targets[0].Password != "" || got.Targets[0].PasswordEnv != "" {
		t.Fatalf("MySQL target = %#v", got.Targets[0])
	}
}

func TestParseTargetRoles(t *testing.T) {
	targets := strings.Replace(
		validTargetsCSV,
		"enabled,group,name,description,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version",
		"enabled,group,name,description,roles,engine,host,port,username,password,password_env,tls_mode,tls_verify,default_database_name,protocol_version",
		1,
	)
	targets = strings.Replace(targets, "MySQL RDS,mysql", "MySQL RDS, database-readers ; oncall ,mysql", 1)
	targets = strings.Replace(targets, "PostgreSQL RDS,postgres", "PostgreSQL RDS,,postgres", 1)
	targets = strings.Replace(targets, "Disabled,mysql", "Disabled,,mysql", 1)

	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !slices.Equal(got.Targets[0].Roles, []string{"database-readers", "oncall"}) {
		t.Fatalf("Roles = %#v", got.Targets[0].Roles)
	}
	if !got.Targets[0].RolesSpecified || got.Targets[1].Roles != nil || !got.Targets[1].RolesSpecified {
		t.Fatalf("targets with roles column = %#v", got.Targets[:2])
	}
}

func TestParseDistinguishesMissingRolesColumn(t *testing.T) {
	got, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(validTargetsCSV))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Targets[0].RolesSpecified {
		t.Fatalf("Target.RolesSpecified = %t", got.Targets[0].RolesSpecified)
	}
}

func TestParseRejectsInvalidTargetRoles(t *testing.T) {
	targetsWithRoles := strings.Replace(
		validTargetsCSV,
		"description,engine",
		"description,roles,engine",
		1,
	)
	tests := map[string]string{
		"empty role":     "database-readers;;oncall",
		"duplicate role": "database-readers;database-readers",
		"overlong role":  strings.Repeat("r", 256),
	}
	for name, roles := range tests {
		t.Run(name, func(t *testing.T) {
			targets := strings.Replace(targetsWithRoles, "MySQL RDS,mysql", "MySQL RDS,"+roles+",mysql", 1)
			targets = strings.Replace(targets, "PostgreSQL RDS,postgres", "PostgreSQL RDS,,postgres", 1)
			targets = strings.Replace(targets, "Disabled,mysql", "Disabled,,mysql", 1)
			_, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(targets))
			if err == nil || !strings.Contains(err.Error(), "roles") {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseRejectsInvalidGroups(t *testing.T) {
	tests := map[string]struct {
		groups string
		want   string
	}{
		"empty":            {"", "header"},
		"unknown column":   {strings.Replace(validGroupsCSV, "description,color", "description,unexpected,color", 1), "unknown column"},
		"missing column":   {"enabled,name,description\n", "missing required column"},
		"duplicate column": {"enabled,name,name,color\n", "duplicate column"},
		"invalid enabled":  {strings.Replace(validGroupsCSV, "true,environment-a", "maybe,environment-a", 1), "enabled"},
		"blank name":       {strings.Replace(validGroupsCSV, "environment-a", "", 1), "name"},
		"invalid color":    {strings.Replace(validGroupsCSV, "Info", "Purple", 1), "color"},
		"duplicate name":   {strings.Replace(validGroupsCSV, "environment-b", "environment-a", 1), "duplicate group"},
		"field mismatch":   {validGroupsCSV + "true,too,few\n", "record"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.groups), strings.NewReader(validTargetsCSV))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsInvalidTargets(t *testing.T) {
	tests := map[string]struct {
		targets string
		want    string
	}{
		"empty":                     {"", "header"},
		"unknown column":            {strings.Replace(validTargetsCSV, "description,engine", "description,unexpected,engine", 1), "unknown column"},
		"invalid enabled":           {strings.Replace(validTargetsCSV, "true,environment-a", "maybe,environment-a", 1), "enabled"},
		"unknown group":             {strings.Replace(validTargetsCSV, "environment-a,mysql-a", "missing,mysql-a", 1), "unknown group"},
		"blank name":                {strings.Replace(validTargetsCSV, "mysql-a,MySQL", ",MySQL", 1), "name"},
		"duplicate name":            {strings.Replace(validTargetsCSV, "postgres-a,PostgreSQL", "mysql-a,PostgreSQL", 1), "duplicate target"},
		"invalid engine":            {strings.Replace(validTargetsCSV, ",mysql,mysql-a", ",oracle,mysql-a", 1), "engine"},
		"blank host":                {strings.Replace(validTargetsCSV, "mysql-a.example.invalid", "", 1), "host"},
		"invalid port":              {strings.Replace(validTargetsCSV, ",3306,db_user", ",70000,db_user", 1), "port"},
		"blank username":            {strings.Replace(validTargetsCSV, ",db_user,password", ",,password", 1), "username"},
		"postgres missing password": {strings.Replace(validTargetsCSV, ",POSTGRES_A_PASSWORD,preferred", ",,preferred", 1), "password"},
		"two passwords":             {strings.Replace(validTargetsCSV, "password-placeholder,,required", "password-placeholder,MYSQL_PASSWORD,required", 1), "only one"},
		"bad password env":          {strings.Replace(validTargetsCSV, ",POSTGRES_A_PASSWORD,preferred", ",bad-name,preferred", 1), "password_env"},
		"invalid TLS mode":          {strings.Replace(validTargetsCSV, "required,true", "sometimes,true", 1), "tls_mode"},
		"invalid TLS verify":        {strings.Replace(validTargetsCSV, "required,true", "required,maybe", 1), "tls_verify"},
		"mysql protocol":            {strings.Replace(validTargetsCSV, "required,true,,", "required,true,,3.0", 1), "protocol_version"},
		"bad pg protocol":           {strings.Replace(validTargetsCSV, "app_db,3.0", "app_db,4.0", 1), "protocol_version"},
		"field mismatch":            {validTargetsCSV + "true,too,few\n", "record"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(validGroupsCSV), strings.NewReader(tt.targets))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsOverlongNames(t *testing.T) {
	groups := strings.Replace(validGroupsCSV, "environment-a", strings.Repeat("a", 256), 1)
	if _, err := Parse(strings.NewReader(groups), strings.NewReader(validTargetsCSV)); err == nil || !strings.Contains(err.Error(), "255") {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseReaderErrors(t *testing.T) {
	groupsHeader := strings.SplitN(validGroupsCSV, "\n", 2)[0] + "\n"
	targetsHeader := strings.SplitN(validTargetsCSV, "\n", 2)[0] + "\n"
	tests := []struct {
		name    string
		groups  io.Reader
		targets io.Reader
	}{
		{name: "groups header", groups: errorReader{}, targets: strings.NewReader(validTargetsCSV)},
		{name: "groups record", groups: io.MultiReader(strings.NewReader(groupsHeader), errorReader{}), targets: strings.NewReader(validTargetsCSV)},
		{name: "targets header", groups: strings.NewReader(validGroupsCSV), targets: errorReader{}},
		{name: "targets record", groups: strings.NewReader(validGroupsCSV), targets: io.MultiReader(strings.NewReader(targetsHeader), errorReader{})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.groups, tt.targets); err == nil || !strings.Contains(err.Error(), "read") {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestResolvePasswords(t *testing.T) {
	input := Inventory{Targets: []Target{
		{Name: "inline", Password: "password-placeholder"},
		{Name: "from-env", PasswordEnv: "RDS_PASSWORD"},
	}}
	resolved, err := ResolvePasswords(input, func(name string) string {
		if name == "RDS_PASSWORD" {
			return "resolved-placeholder"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolvePasswords() error = %v", err)
	}
	if resolved.Targets[1].Password != "resolved-placeholder" || input.Targets[1].Password != "" {
		t.Fatalf("resolved = %#v, original = %#v", resolved, input)
	}
}

func TestResolvePasswordsRejectsMissingEnvironmentVariable(t *testing.T) {
	input := Inventory{Targets: []Target{{Name: "rds-a", PasswordEnv: "RDS_PASSWORD"}}}
	_, err := ResolvePasswords(input, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "RDS_PASSWORD") {
		t.Fatalf("ResolvePasswords() error = %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}
