package config

import (
	"encoding/json"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// login_role and login_password are logins that the PostgreSQL modules hand
// only to psql. A secret in either is accepted, explain names the secret and
// never shows it, and checking it does not mark the task's trace, so it does
// not hide the task's result. The name of the role or the database that the
// task manages is an identifier, where a secret is refused.
func TestPostgresqlLoginIsCheckedWithoutMarkingTheTrace(t *testing.T) {
	secrets := value.Scope{"secrets": map[string]any{
		"admin_role":     &value.Secret{Name: "admin_role", Data: "role-sentinel", Available: true},
		"admin_password": &value.Secret{Name: "admin_password", Data: "password-sentinel", Available: true},
		"empty":          &value.Secret{Name: "empty", Data: "", Available: true}}}
	for module, inputs := range map[string]func(loginRole string) any{
		"postgresql_role": func(loginRole string) any {
			return &modules.PostgresqlRole{CanLogIn: new(bool), Name: "shop", DatabaseServerHost: "db.example.com", LoginRole: loginRole, LoginPassword: "{{ .secrets.admin_password }}"}
		},
		"postgresql_database": func(loginRole string) any {
			return &modules.PostgresqlDatabase{Owner: "postgres", Name: "shop", DatabaseServerHost: "db.example.com", LoginRole: loginRole, LoginPassword: "{{ .secrets.admin_password }}"}
		},
	} {
		s, trace := secrets.Trace()
		resolved, err := ResolveArguments(Task{Module: module, ModuleInputs: inputs("{{ .secrets.admin_role }}")}, s)
		if err != nil {
			t.Fatal(module, err)
		}
		arguments, err := InspectArguments(resolved, s, false)
		if err != nil || trace.Used {
			t.Fatal(module, "checking the login marked the task's trace", err)
		}
		checked := 0
		for _, argument := range arguments {
			if argument.Field == module+".login_role" || argument.Field == module+".login_password" {
				checked++
				if argument.Status != "secret" || argument.Value != nil || len(argument.Secrets) != 1 {
					t.Fatalf("%s: explain must name the secret and never show it: %+v", module, argument)
				}
			}
		}
		if text, _ := json.Marshal(arguments); checked != 2 || strings.Contains(string(text), "sentinel") {
			t.Fatal(module, "the inspected arguments hold a login, or one of the two was not checked", checked)
		}
		if got := s.LoginsHandedOnlyToATool(); len(got) != 2 {
			t.Fatal(module, "the checked logins were not remembered for the replacement", got)
		}
		// A login role that resolves to nothing is refused, without its value.
		s, _ = secrets.Trace()
		if _, err := InspectArguments(Task{Module: module, ModuleInputs: inputs("{{ .secrets.empty }}")}, s, false); err == nil || !strings.Contains(err.Error(), module+".login_role") || !strings.Contains(err.Error(), "login_role must not be empty") {
			t.Fatal(module, err)
		}
	}
	// A secret in the name of the role that the task manages is refused, as
	// in every identifier.
	if _, err := ResolveArguments(Task{Module: "postgresql_role", ModuleInputs: &modules.PostgresqlRole{CanLogIn: new(bool), Name: "{{ .secrets.admin_role }}"}}, secrets); err == nil || !strings.Contains(err.Error(), "resource identifier must not contain a secret") {
		t.Fatal(err)
	}
}
