package modules

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// postgresqlChange is what a postgresql_role or postgresql_database result says:
// its change, value.created, and value.changed_attributes joined by commas.
func postgresqlChange(t *testing.T, r protocol.Result) [3]any {
	t.Helper()
	v, isValue := r.Value.(map[string]any)
	if !isValue {
		t.Fatalf("the result has no value: %+v", r)
	}
	var names []string
	for _, name := range v["changed_attributes"].([]any) {
		names = append(names, name.(string))
	}
	return [3]any{r.Change, v["created"], strings.Join(names, ",")}
}

// postgresqlBool and postgresqlText are a task's written yes-or-no value
// and its written text, for the fields that a task can omit.
func postgresqlBool(v bool) *bool { return &v }

func postgresqlText(v string) *string { return &v }

func TestPostgresqlRoleRejectedInputs(t *testing.T) {
	t.Parallel()
	for message, role := range map[string]PostgresqlRole{
		"can_log_in: can_log_in is required with state: present; add can_log_in: true to allow login or can_log_in: false for a role used only for privileges": {Name: "shop"},
		"state: state must be present or absent":                                                                                                                                                                 {Name: "shop", State: "removed"},
		"state: postgresql_role with state: absent accepts only name and the connection fields":                                                                                                                  {Name: "shop", State: "absent", CanLogIn: postgresqlBool(false)},
		"state: postgresql_role with state: absent accepts only name and the connection fields ":                                                                                                                 {Name: "shop", State: "absent", Password: postgresqlText("x")},
		"state: postgresql_role with state: absent accepts only name and the connection fields  ":                                                                                                                {Name: "shop", State: "absent", MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"},
		`password: password must not be empty: PostgreSQL takes an empty password as "remove the password"; leave password out to keep the password that the role has`:                                           {Name: "shop", Password: postgresqlText("")},
		"member_of_roles_not_listed: member_of_roles_not_listed must be remove or keep":                                                                                                                          {Name: "shop", MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "delete"},
		"member_of_roles_not_listed: member_of_roles_not_listed is required with member_of_roles: remove makes member_of_roles the whole set of the role's memberships, keep leaves its other memberships alone": {Name: "shop", MemberOfRoles: []string{"pg_monitor"}},
		"member_of_roles_not_listed: member_of_roles_not_listed requires member_of_roles (use member_of_roles: [] with remove to remove every membership)":                                                       {Name: "shop", MemberOfRolesNotListed: "remove"},
		"member_of_roles[1]: member_of_roles lists pg_monitor twice":                                                                                                                                             {Name: "shop", MemberOfRoles: []string{"pg_monitor", "pg_monitor"}, MemberOfRolesNotListed: "keep"},
	} {
		if err := role.Prepare(""); err == nil || err.Error() != strings.TrimSpace(message) {
			t.Errorf("%+v: %v; want %s", role, err, message)
		}
	}
	for _, role := range []PostgresqlRole{
		{Name: "shop", CanLogIn: postgresqlBool(false)},
		{Name: "shop", State: "absent", LoginRole: "admin", LoginPassword: "x", DatabaseServerPort: 5433},
		{Name: "shop", Password: postgresqlText("{{ .secrets.shop }}"), CanLogIn: postgresqlBool(true), MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"},
	} {
		if err := role.Prepare(""); err != nil {
			t.Errorf("%+v: %v", role, err)
		}
	}
	// The arguments that may hold a reference, in the order the task reads:
	// the role's password and the login are handed only to psql.
	role := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", Password: postgresqlText("{{ .secrets.shop }}"), MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "keep",
		DatabaseServerHost: "db.example.com", DatabaseServerPort: 5433, DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.crt", LoginRole: "admin", LoginPassword: "{{ .secrets.admin }}"}
	var arguments ArgumentList
	role.Arguments(&arguments)
	var fields, logins []string
	for _, argument := range arguments.List {
		fields = append(fields, argument.Field)
		if argument.LoginHandedOnlyToATool {
			logins = append(logins, argument.Field)
		}
	}
	if !slices.Equal(fields, []string{"name", "member_of_roles[0]", "password", "database_server_host", "database_server_port", "database_server_certificate_authority_file", "login_role", "login_password"}) ||
		!slices.Equal(logins, []string{"password", "login_role", "login_password"}) {
		t.Error(fields, logins)
	}
}

// A first run creates the role with what the task writes, in one
// transaction, and sends the password only as its SCRAM-SHA-256 form. A
// second run and a dryrun find nothing to change, and a dryrun before the
// first run changes nothing.
func TestPostgresqlRoleFirstRunUnchangedRerunAndDryrun(t *testing.T) {
	ctx := testContext(t)
	const password = "sentinel pässwörd 4711"
	for _, version := range []int{13, 17} {
		dir := postgresqlFixture(t, newStandInPostgresql(version))
		role := PostgresqlRole{Name: `sh"op O'Brien`, Password: postgresqlText(password), CanLogIn: postgresqlBool(true), CanCreateDatabases: postgresqlBool(false), MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "keep"}
		before := readState(t, filepath.Join(dir, standInPostgresqlServerFile))

		r := noMutation(t).PostgresqlRole(ctx, role, value.Scope{}, nil)
		if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"would_change", true, ""} || r.Reason != `would create the role sh"op O'Brien` {
			t.Fatalf("version %d, dryrun: %+v", version, r)
		}
		if readState(t, filepath.Join(dir, standInPostgresqlServerFile)) != before || takePostgresqlStatements(t, dir) != nil {
			t.Fatalf("version %d: the dryrun changed the server", version)
		}

		r = (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil)
		if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
			t.Fatalf("version %d, run: %+v", version, r)
		}
		if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{
			`create role "sh"op O'Brien" LOGIN NOCREATEDB`,
			`alter role "sh"op O'Brien" password '<SCRAM form>'`,
			`grant "pg_monitor" to "sh"op O'Brien"`,
		}) {
			t.Errorf("version %d: %q", version, statements)
		}
		server := readStandInPostgresql(t, dir)
		created := server.Roles[`sh"op O'Brien`]
		if created == nil || !created.CanLogIn || created.Superuser || !postgresqlStoredFormHolds(created.StoredPassword, password) ||
			!slices.Equal(server.Memberships, []standInPostgresqlMembership{{Role: "pg_monitor", Member: `sh"op O'Brien`, GrantedBy: "postgres"}}) {
			t.Errorf("version %d: the role is %+v, a member of %+v", version, created, server.Memberships)
		}
		// The form has the server's own iteration count where the server
		// has the setting, and 4096 elsewhere.
		if !strings.HasPrefix(created.StoredPassword, "SCRAM-SHA-256$4096:") {
			t.Errorf("version %d: %s", version, created.StoredPassword)
		}
		// The clear password went nowhere: the script holds the form.
		if saw := everythingPostgresqlStandInsSaw(t, dir); strings.Contains(saw, password) || strings.Contains(saw, "pässwörd") || !strings.Contains(saw, created.StoredPassword) {
			t.Errorf("version %d: the password is on a command line, in an environment, or in a script", version)
		}

		before = readState(t, filepath.Join(dir, standInPostgresqlServerFile))
		for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
			if r := run.PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
				t.Errorf("version %d, second %s: %+v", version, name, r)
			}
		}
		if readState(t, filepath.Join(dir, standInPostgresqlServerFile)) != before || takePostgresqlStatements(t, dir) != nil {
			t.Errorf("version %d: an unchanged run changed the server", version)
		}
	}
}

// An existing role is changed in what differs and keeps what the task
// omits. The result names the attributes that differed.
func TestPostgresqlRoleChangesOnlyWhatDiffers(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.ScramIterations = 10000
	server.Roles["shop"] = &standInPostgresqlRole{CanLogIn: true, CanCreateDatabases: true, CanCreateRoles: true}
	server.Roles["reporting"] = &standInPostgresqlRole{}
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", Password: postgresqlText("new password"), CanLogIn: postgresqlBool(true), Superuser: postgresqlBool(false), CanCreateDatabases: postgresqlBool(false), CanStartReplication: postgresqlBool(true),
		MemberOfRoles: []string{"reporting", "pg_monitor"}, MemberOfRolesNotListed: "remove"}

	r := noMutation(t).PostgresqlRole(ctx, role, value.Scope{}, nil)
	if postgresqlChange(t, r) != [3]any{"would_change", false, "can_create_databases,can_start_replication,password,member_of_roles"} ||
		r.Reason != "would change can_create_databases, can_start_replication, password, member_of_roles of the role shop" {
		t.Fatalf("dryrun: %+v", r)
	}
	if takePostgresqlStatements(t, dir) != nil {
		t.Fatal("the dryrun sent a statement")
	}
	r = (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil)
	if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "can_create_databases,can_start_replication,password,member_of_roles"} {
		t.Fatalf("run: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{
		`alter role "shop" NOCREATEDB REPLICATION`,
		`alter role "shop" password '<SCRAM form>'`,
		`grant "reporting" to "shop"`,
		`grant "pg_monitor" to "shop"`,
	}) {
		t.Errorf("%q", statements)
	}
	changed := readStandInPostgresql(t, dir).Roles["shop"]
	// can_create_roles is not in the task and stays.
	if !changed.CanLogIn || changed.CanCreateDatabases || !changed.CanCreateRoles || !changed.CanStartReplication ||
		!strings.HasPrefix(changed.StoredPassword, "SCRAM-SHA-256$10000:") || !postgresqlStoredFormHolds(changed.StoredPassword, "new password") {
		t.Errorf("%+v", changed)
	}
	if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("rerun: %+v", r)
	}
}

// The stored form decides whether a password differs: the form that the
// server itself made of the same password is unchanged, a form of another
// password is replaced, and the older md5 form is replaced also for the
// same password, which the result says.
func TestPostgresqlRolePasswordComparedWithTheStoredForm(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(13)
	// As PostgreSQL 13.23 stored them for these passwords.
	server.Roles["as_the_server_made_it"] = &standInPostgresqlRole{StoredPassword: postgresqlPasswordsMeasuredFirst[4].stored}
	server.Roles["pw_md5hba"] = &standInPostgresqlRole{StoredPassword: "md5d80d949e1be2dd4a1cfa9e41f5240240"}
	server.Roles["without_a_password"] = &standInPostgresqlRole{}
	dir := postgresqlFixture(t, server)

	prepared := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "as_the_server_made_it", Password: postgresqlText(postgresqlPasswordsMeasuredFirst[4].password)}
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlRole(ctx, prepared, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("%s with the server's own form: %+v", name, r)
		}
	}
	prepared.Password = postgresqlText("another password")
	if r := (Runtime{}).PostgresqlRole(ctx, prepared, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "password"} || r.Reason != "" {
		t.Errorf("another password: %+v", r)
	}

	md5 := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "pw_md5hba", Password: postgresqlText("correct horse 1")}
	r := noMutation(t).PostgresqlRole(ctx, md5, value.Scope{}, nil)
	if postgresqlChange(t, r) != [3]any{"would_change", false, "password"} ||
		r.Reason != "would change password of the role pw_md5hba; its password is stored in the older md5 form, which a run replaces with the SCRAM-SHA-256 form" {
		t.Errorf("dryrun of an md5 form: %+v", r)
	}
	r = (Runtime{}).PostgresqlRole(ctx, md5, value.Scope{}, nil)
	if postgresqlChange(t, r) != [3]any{"changed", false, "password"} ||
		r.Reason != "the password of the role pw_md5hba was stored in the older md5 form and is now stored in the SCRAM-SHA-256 form" {
		t.Errorf("run of an md5 form: %+v", r)
	}
	if stored := readStandInPostgresql(t, dir).Roles["pw_md5hba"].StoredPassword; !postgresqlStoredFormHolds(stored, "correct horse 1") {
		t.Error("the md5 form was not replaced by a SCRAM form of the password")
	}
	if r := (Runtime{}).PostgresqlRole(ctx, md5, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("after the replacement: %+v", r)
	}

	// A role without a password gets one; a task without password leaves
	// the stored form alone and does not read it.
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "without_a_password", Password: postgresqlText("first password")}, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "password"} {
		t.Errorf("%+v", r)
	}
	writeState(t, filepath.Join(dir, standInPostgresqlScriptsFile), "")
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "pw_md5hba", CanLogIn: postgresqlBool(true)}, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "can_log_in"} {
		t.Errorf("%+v", r)
	}
	if scripts := readState(t, filepath.Join(dir, standInPostgresqlScriptsFile)); strings.Contains(scripts, "rolpassword") {
		t.Error("a task without password read the stored form")
	}
}

// Only a superuser can read stored passwords. A task with password and
// another login role fails before it changes anything, also for a role
// that does not exist yet; without password such a login role works.
func TestPostgresqlRolePasswordNeedsASuperuserLogin(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["deploy"] = &standInPostgresqlRole{CanLogIn: true, CanCreateRoles: true}
	server.RolesThatAreNoSuperuser = []string{"deploy"}
	dir := postgresqlFixture(t, server)
	reason := "postgresql_role: the login role deploy is not a superuser and cannot read stored passwords from pg_catalog.pg_authid; MMX needs the stored password to compare with the password requested by the node task; log in as a superuser, or omit password"
	role := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", Password: postgresqlText("x"), LoginRole: "deploy"}
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || r.Reason != reason {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if takePostgresqlStatements(t, dir) != nil {
		t.Error("a statement went out")
	}
	role.Password = nil
	if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Errorf("%+v", r)
	}
}

// A secret in password does not hide the task's result. The result shows
// created and changed_attributes; and when the server quotes the statement
// in a failure, the result keeps the message and shows [redacted] for the
// stored form that MMX computed. The form that the server already holds of
// the same password is replaced in the same way.
func TestPostgresqlRoleSecretPasswordDoesNotHideTheResult(t *testing.T) {
	ctx := testContext(t)
	const password = "sentinel role password 4711"
	secrets := map[string]string{"shop_database_password": password}
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	// The server answers the password statement as it answers a statement
	// with a syntax error: with the statement's text.
	server.FailsStatementsStartingWith = map[string]string{`alter role "shop" password`: "ERROR:  42601: syntax error at or near \"password\"\nLINE 1: <the statement>"}
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", Password: postgresqlText("{{ .secrets.shop_database_password }}"), CanLogIn: postgresqlBool(true)}

	s, trace := taskScope(secrets)
	r := reportedByTheEngine(noMutation(t).PostgresqlRole(ctx, role, s, nil), s)
	if trace.Used || postgresqlChange(t, r) != [3]any{"would_change", false, "can_log_in,password"} {
		t.Fatalf("dryrun: %+v (hidden: %v)", r, trace.Used)
	}

	s, trace = taskScope(secrets)
	told := (Runtime{}).PostgresqlRole(ctx, role, s, nil)
	if !strings.Contains(told.Reason, "password 'SCRAM-SHA-256$4096:") {
		t.Fatalf("the server's message did not hold the form: %+v", told)
	}
	r = reportedByTheEngine(told, s)
	if trace.Used || r.Status != "failed" || r.Reason != "postgresql_role: could not change the role shop: ERROR:  42601: syntax error at or near \"password\"\nLINE 1: alter role \"shop\" password '[redacted]'" {
		t.Errorf("%+v (hidden: %v)", r, trace.Used)
	}

	// A run that works: the result names what changed.
	server.FailsStatementsStartingWith = nil
	writeStandInPostgresql(t, dir, server)
	s, trace = taskScope(secrets)
	r = reportedByTheEngine((Runtime{}).PostgresqlRole(ctx, role, s, nil), s)
	stored := readStandInPostgresql(t, dir).Roles["shop"].StoredPassword
	if trace.Used || r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "can_log_in,password"} || !postgresqlStoredFormHolds(stored, password) {
		t.Fatalf("run: %+v (hidden: %v)", r, trace.Used)
	}
	if !slices.Contains(s.LoginsHandedOnlyToATool(), password) || !slices.Contains(s.LoginsHandedOnlyToATool(), stored) {
		t.Error("the task's scope does not remember the password and the form that MMX sent")
	}
	// An unchanged run computes the stored form with the server's salt, so
	// the scope remembers that form too.
	s, _ = taskScope(secrets)
	if r := (Runtime{}).PostgresqlRole(ctx, role, s, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} || !slices.Contains(s.LoginsHandedOnlyToATool(), stored) {
		t.Errorf("unchanged: %+v, remembered %d texts", r, len(s.LoginsHandedOnlyToATool()))
	}
	// A password that is no secret is not remembered, and neither is its form.
	plain := role
	plain.Password = postgresqlText(password)
	s, _ = taskScope(nil)
	if r := (Runtime{}).PostgresqlRole(ctx, plain, s, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} || len(s.LoginsHandedOnlyToATool()) != 0 {
		t.Errorf("a written password: %+v, remembered %q", r, s.LoginsHandedOnlyToATool())
	}
}

// Memberships. With keep, the task adds what is missing. With remove, it
// removes every membership that the list does not name, record by record
// in the name of the role that granted it: from PostgreSQL 16 a membership
// has one record for each grantor, and a plain REVOKE would remove only the
// record of the login role and still report success. A record that only
// lets the role administer another role is removed too.
func TestPostgresqlRoleMemberships(t *testing.T) {
	ctx := testContext(t)
	for _, version := range []int{13, 17} {
		server := newStandInPostgresql(version)
		for _, name := range []string{"shop", "reporting", "billing", "made_by_shop", "team_lead"} {
			server.Roles[name] = &standInPostgresqlRole{}
		}
		server.Memberships = []standInPostgresqlMembership{
			{Role: "billing", Member: "shop", GrantedBy: "postgres"},
			{Role: "reporting", Member: "shop", GrantedBy: "team_lead"},
			// Another role's membership is never touched.
			{Role: "billing", Member: "team_lead", GrantedBy: "postgres"},
		}
		if version >= 16 {
			server.Memberships = append(server.Memberships,
				standInPostgresqlMembership{Role: "reporting", Member: "shop", GrantedBy: "postgres"},
				standInPostgresqlMembership{Role: "made_by_shop", Member: "shop", GrantedBy: "postgres", OnlyAdministers: true})
		}
		dir := postgresqlFixture(t, server)

		keep := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", MemberOfRoles: []string{"pg_monitor", "billing"}, MemberOfRolesNotListed: "keep"}
		if r := (Runtime{}).PostgresqlRole(ctx, keep, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "member_of_roles"} {
			t.Fatalf("version %d, keep: %+v", version, r)
		}
		if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`grant "pg_monitor" to "shop"`}) {
			t.Errorf("version %d, keep: %q", version, statements)
		}
		if r := (Runtime{}).PostgresqlRole(ctx, keep, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("version %d, keep again: %+v", version, r)
		}

		remove := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "remove"}
		if r := noMutation(t).PostgresqlRole(ctx, remove, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", false, "member_of_roles"} {
			t.Fatalf("version %d, dryrun of remove: %+v", version, r)
		}
		if r := (Runtime{}).PostgresqlRole(ctx, remove, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "member_of_roles"} {
			t.Fatalf("version %d, remove: %+v", version, r)
		}
		want := []string{`revoke "billing" from "shop" granted by "postgres"`, `revoke "reporting" from "shop" granted by "team_lead"`}
		left := []standInPostgresqlMembership{{Role: "billing", Member: "team_lead", GrantedBy: "postgres"}, {Role: "pg_monitor", Member: "shop", GrantedBy: "postgres"}}
		if version >= 16 {
			want = []string{`revoke "billing" from "shop" granted by "postgres"`, `revoke "made_by_shop" from "shop" granted by "postgres"`, `revoke "reporting" from "shop" granted by "postgres"`, `revoke "reporting" from "shop" granted by "team_lead"`}
		}
		if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, want) {
			t.Errorf("version %d, remove: %q", version, statements)
		}
		memberships := readStandInPostgresql(t, dir).Memberships
		slices.SortFunc(memberships, func(a, b standInPostgresqlMembership) int {
			return strings.Compare(a.Role+a.Member+a.GrantedBy, b.Role+b.Member+b.GrantedBy)
		})
		if !slices.Equal(memberships, left) {
			t.Errorf("version %d, remove left %+v", version, memberships)
		}
		if r := (Runtime{}).PostgresqlRole(ctx, remove, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("version %d, remove again: %+v", version, r)
		}
		// member_of_roles: [] with remove removes every membership.
		none := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"}
		if r := (Runtime{}).PostgresqlRole(ctx, none, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "member_of_roles"} {
			t.Errorf("version %d, none: %+v", version, r)
		}
		if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`revoke "pg_monitor" from "shop" granted by "postgres"`}) {
			t.Errorf("version %d, none: %q", version, statements)
		}
	}
}

// On PostgreSQL 15 and older, the role that granted a membership can be
// gone. Such a record is removed with a plain REVOKE, which removes the one
// record there.
func TestPostgresqlRoleMembershipWhoseGrantorIsGone(t *testing.T) {
	server := newStandInPostgresql(13)
	server.Roles["shop"], server.Roles["reporting"] = &standInPostgresqlRole{}, &standInPostgresqlRole{}
	server.Memberships = []standInPostgresqlMembership{{Role: "reporting", Member: "shop"}}
	dir := postgresqlFixture(t, server)
	r := (Runtime{}).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"}, value.Scope{}, nil)
	if postgresqlChange(t, r) != [3]any{"changed", false, "member_of_roles"} || !slices.Equal(takePostgresqlStatements(t, dir), []string{`revoke "reporting" from "shop"`}) {
		t.Errorf("%+v", r)
	}
}

// A role in member_of_roles that does not exist is one that an earlier node task
// may still create: a run fails without another try and names it, before
// it changes anything, and a dryrun is not checked.
func TestPostgresqlRoleMemberOfARoleThatDoesNotExist(t *testing.T) {
	ctx := testContext(t)
	dir := postgresqlFixture(t, newStandInPostgresql(13))
	role := PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(true), MemberOfRoles: []string{"pg_monitor", "pg_read_all_data", "reporting"}, MemberOfRolesNotListed: "keep"}
	reason := "postgresql_role.member_of_roles: the roles pg_read_all_data, reporting do not exist in the PostgreSQL server on this node's local socket; create each in an earlier node task. A predefined role exists only from the PostgreSQL version that brought it: pg_read_all_data and pg_write_all_data from version 14"
	if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Errorf("run: %+v", r)
	}
	if r := noMutation(t).PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Errorf("dryrun: %+v", r)
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Roles["shop"] != nil {
		t.Error("the task changed the server before it failed")
	}
}

// After its statements the task reads the role again and fails when the
// role is not as the task says: a GRANT that the server accepts without a
// change, as it does while a record that only lets the role administer the
// other role stands in the way. What did change is in the result.
func TestPostgresqlRoleFailsWhenADifferenceIsLeft(t *testing.T) {
	server := newStandInPostgresql(17)
	server.Roles["shop"], server.Roles["made_by_shop"] = &standInPostgresqlRole{}, &standInPostgresqlRole{}
	server.Memberships = []standInPostgresqlMembership{{Role: "made_by_shop", Member: "shop", GrantedBy: "postgres", OnlyAdministers: true}}
	server.GrantsThatChangeNothing = []string{"made_by_shop"}
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(true), MemberOfRoles: []string{"made_by_shop"}, MemberOfRolesNotListed: "keep"}
	r := (Runtime{}).PostgresqlRole(testContext(t), role, value.Scope{}, nil)
	if r.Status != "failed" || postgresqlChange(t, r) != [3]any{"changed", false, "can_log_in"} ||
		!strings.HasPrefix(r.Reason, "postgresql_role: the server accepted the statements for the role shop, and the role is not as the task says: the role is still not a member of made_by_shop. PostgreSQL 16 and newer keep one membership record for each role that granted it;") {
		t.Errorf("%+v", r)
	}
	if !readStandInPostgresql(t, dir).Roles["shop"].CanLogIn {
		t.Error("the committed change is not on the server")
	}
	// The next try has only the GRANT left, which changes nothing again.
	r = (Runtime{}).PostgresqlRole(testContext(t), role, value.Scope{}, nil)
	if r.Status != "failed" || r.Change != "unchanged" || r.Value != nil || !strings.Contains(r.Reason, "the role is still not a member of made_by_shop") {
		t.Errorf("the next try: %+v", r)
	}
}

// What PostgreSQL 16 changed about memberships explains only a membership
// that is left, not an attribute.
func TestPostgresqlRoleDifferenceLeftNamesMembershipRecordsOnlyForAMembership(t *testing.T) {
	t.Parallel()
	role := PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(true), MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "remove"}
	member := []postgresqlMembership{{Role: "pg_monitor"}}
	for want, after := range map[string]postgresqlRoleState{
		"the server accepted the statements for the role shop, and the role is not as the task says: can_log_in is false": {Role: map[string]bool{"can_log_in": false}, Memberships: member},
		"the server accepted the statements for the role shop, and the role is not as the task says: the role is still not a member of pg_monitor. PostgreSQL 16 and newer keep one membership record for each role that granted it; a record that only lets the role administer another role keeps a GRANT of the same role from taking effect. Compare the role's records in pg_auth_members": {Role: map[string]bool{"can_log_in": true}},
		"the server accepted the statements for the role shop, and the role is not as the task says: can_log_in is false; the role is still a member of made_by_shop. PostgreSQL 16 and newer keep one membership record for each role that granted it; compare the role's records in pg_auth_members":                                                                                          {Role: map[string]bool{"can_log_in": false}, Memberships: append([]postgresqlMembership{{Role: "made_by_shop"}}, member...)},
	} {
		if err := role.differenceLeft(after); err == nil || err.Error() != want {
			t.Errorf("%v\nwant %s", err, want)
		}
	}
	if err := role.differenceLeft(postgresqlRoleState{Role: map[string]bool{"can_log_in": true}, Memberships: member}); err != nil {
		t.Error(err)
	}
}

// The statements of one task are one transaction: when the server refuses
// one of them, nothing of the task stays, and the next try does all of it.
func TestPostgresqlRoleFailureChangesNothingAndTheNextTryWorks(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.FailsStatementsStartingWith = map[string]string{`grant "pg_monitor"`: "ERROR:  42501: permission denied to grant role \"pg_monitor\"\nDETAIL:  Only roles with the ADMIN option on role \"pg_monitor\" may grant this role."}
	server.FailsOnlyOnce = true
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", Password: postgresqlText("first password"), CanLogIn: postgresqlBool(true), MemberOfRoles: []string{"pg_monitor"}, MemberOfRolesNotListed: "keep"}
	r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil)
	// The server's message, without the line that names its source code.
	want := "postgresql_role: could not create the role shop: ERROR:  42501: permission denied to grant role \"pg_monitor\"\nDETAIL:  Only roles with the ADMIN option on role \"pg_monitor\" may grant this role."
	// The server refused a statement, so nothing of the transaction stays,
	// and the result says so.
	if r.Status != "failed" || r.Change != "unchanged" || r.Value != nil || r.FailureKind != "" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	if readStandInPostgresql(t, dir).Roles["shop"] != nil {
		t.Fatal("a part of the failed transaction stayed")
	}
	takePostgresqlStatements(t, dir)
	if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Fatalf("the next try: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); len(statements) != 3 || readStandInPostgresql(t, dir).Roles["shop"] == nil {
		t.Errorf("%q", statements)
	}
}

// state: absent removes the role. A missing role is unchanged, a dryrun
// removes nothing, and a role that still owns a database stays: the task
// fails with the server's message.
func TestPostgresqlRoleAbsent(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["shop"], server.Roles["owner"] = &standInPostgresqlRole{}, &standInPostgresqlRole{}
	server.Memberships = []standInPostgresqlMembership{{Role: "pg_monitor", Member: "shop", GrantedBy: "postgres"}}
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "owner"}
	dir := postgresqlFixture(t, server)
	absent := PostgresqlRole{Name: "shop", State: "absent"}

	if r := noMutation(t).PostgresqlRole(ctx, absent, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", false, ""} || r.Reason != "would remove the role shop" {
		t.Fatalf("dryrun: %+v", r)
	}
	if readStandInPostgresql(t, dir).Roles["shop"] == nil || takePostgresqlStatements(t, dir) != nil {
		t.Fatal("the dryrun removed the role")
	}
	if r := (Runtime{}).PostgresqlRole(ctx, absent, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, ""} || r.Reason != "removed the role shop" {
		t.Fatalf("run: %+v", r)
	}
	if after := readStandInPostgresql(t, dir); after.Roles["shop"] != nil || len(after.Memberships) != 0 || !slices.Equal(takePostgresqlStatements(t, dir), []string{`drop role "shop"`}) {
		t.Error("the role is still there")
	}
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlRole(ctx, absent, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("%s for a missing role: %+v", name, r)
		}
	}
	r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "owner", State: "absent"}, value.Scope{}, nil)
	if r.Status != "failed" || r.Change != "unchanged" || r.Reason != "postgresql_role: could not remove the role owner: ERROR:  2BP01: role \"owner\" cannot be dropped because some objects depend on it\nDETAIL:  owner of database shop" {
		t.Errorf("%+v", r)
	}
	if readStandInPostgresql(t, dir).Roles["owner"] == nil {
		t.Error("the role that owns a database is gone")
	}
}

// PostgreSQL cuts a name that is longer than its limit and acts on the
// shorter name with only a notice. The task asks the server for the limit
// and fails before it changes anything, also for a removal.
func TestPostgresqlRoleNameLongerThanTheServerTakes(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	short := strings.Repeat("a", 63)
	server.Roles[short] = &standInPostgresqlRole{}
	dir := postgresqlFixture(t, server)
	long := short + "b"
	reason := `postgresql_role.name: the name "` + long + `" has 64 bytes, and this PostgreSQL server takes names of at most 63 bytes. The server would cut the name and act on "` + short + `"; write a shorter name`
	for _, role := range []PostgresqlRole{{Name: long, CanLogIn: postgresqlBool(true)}, {Name: long, State: "absent"}} {
		for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
			if r := run.PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || r.Reason != reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Roles[short] == nil {
		t.Error("the task acted on the role with the shorter name")
	}
	// A name of exactly the limit is fine, and the limit is the server's.
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: short}, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("%+v", r)
	}
	server.MaxIdentifierLength = 127
	writeStandInPostgresql(t, dir, server)
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: long}, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Errorf("a server with longer names: %+v", r)
	}
}

func TestPostgresqlRoleRemovesAdministrationOnlyMembership(t *testing.T) {
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	server.Roles["made_by_shop"] = &standInPostgresqlRole{}
	server.Memberships = []standInPostgresqlMembership{{Role: "made_by_shop", Member: "shop", GrantedBy: "postgres", OnlyAdministers: true}}
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(false), MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"}
	if err := role.Prepare(""); err != nil {
		t.Fatal(err)
	}
	for _, run := range []Runtime{noMutation(t), {}} {
		r := run.PostgresqlRole(testContext(t), role, value.Scope{}, nil)
		change := "changed"
		if run.Dryrun {
			change = "would_change"
		}
		if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{change, false, "member_of_roles"} {
			t.Fatalf("%+v", r)
		}
	}
	if held := readStandInPostgresql(t, dir).Memberships; len(held) != 0 {
		t.Fatal(held)
	}
	if r := (Runtime{}).PostgresqlRole(testContext(t), role, value.Scope{}, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}
