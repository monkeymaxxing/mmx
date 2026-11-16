package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// PostgresqlRole is one role (account) inside a PostgreSQL server. password
// and login_password are private values that the module hands only to psql:
// the login password as a login, and the role's password as the stored form
// that the module computes from it (postgresql_password.go).
type PostgresqlRole struct {
	Name  string `yaml:"name" json:"name"`
	State string `yaml:"state,omitempty" json:"state,omitempty"`
	// Password is nil when the task leaves the role's password alone.
	Password               *string  `yaml:"password,omitempty" json:"password,omitempty"`
	CanLogIn               *bool    `yaml:"can_log_in,omitempty" json:"can_log_in,omitempty"`
	Superuser              *bool    `yaml:"superuser,omitempty" json:"superuser,omitempty"`
	CanCreateDatabases     *bool    `yaml:"can_create_databases,omitempty" json:"can_create_databases,omitempty"`
	CanCreateRoles         *bool    `yaml:"can_create_roles,omitempty" json:"can_create_roles,omitempty"`
	CanStartReplication    *bool    `yaml:"can_start_replication,omitempty" json:"can_start_replication,omitempty"`
	MemberOfRoles          []string `yaml:"member_of_roles,omitempty" json:"member_of_roles"`
	MemberOfRolesNotListed string   `yaml:"member_of_roles_not_listed,omitempty" json:"member_of_roles_not_listed,omitempty"`

	DatabaseServerHost                     string `yaml:"database_server_host,omitempty" json:"database_server_host,omitempty"`
	DatabaseServerPort                     any    `yaml:"database_server_port,omitempty" json:"database_server_port,omitempty"`
	DatabaseServerCertificateAuthorityFile string `yaml:"database_server_certificate_authority_file,omitempty" json:"database_server_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck  bool   `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
	LoginRole                              string `yaml:"login_role,omitempty" json:"login_role,omitempty"`
	LoginPassword                          string `yaml:"login_password,omitempty" json:"login_password,omitempty"`
}

// postgresqlConnectionFieldNotes are the notes of the connection fields,
// which postgresql_role and postgresql_database share.
func postgresqlConnectionFieldNotes() []Field {
	return []Field{
		{Name: "database_server_host", Notes: "the server's host name or address, for a server on another machine; without it MMX runs psql as the operating system account postgres and uses the server on this node through its local socket, which accepts that account as the role postgres without a password; needs login_password"},
		{Name: "database_server_port", Type: "integer", Notes: "the server's port when it is not 5432; with the local socket it selects among several PostgreSQL servers on one node"},
		{Name: "database_server_certificate_authority_file", Notes: "only with database_server_host: the absolute path on the node of a trusted authority certificate; checks the server certificate and host name; without it verification remains enabled using the client's trust configuration; see the module page for client requirements"},
		{Name: "dangerously_skip_server_certificate_check", Default: "false", Notes: "only with database_server_host; true cannot be combined with database_server_certificate_authority_file and turns off verification of the server certificate, allowing an impersonated server to receive SQL and login information; the client encrypts when the server offers TLS and may connect without encryption otherwise"},
		{Name: "login_role", Default: postgresqlAdministratorRole, Notes: "the role MMX logs in as; a private value; it must be allowed to do what the task asks; it reaches psql in psql's environment, never through a command line"},
		{Name: "login_password", Notes: "that role's password; a private value, normally a secret; required with database_server_host; without database_server_host the task logs in with the password through the local socket; it reaches psql through a password file in a private directory under /run that the task removes, never through a command line; a secret in login_role or login_password does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
	}
}

// postgresqlRoleDescription is what mmx describe-module shows for postgresql_role.
var postgresqlRoleDescription = Description{
	Name: "postgresql_role", Topic: TopicDatabases, Schema: reflect.TypeOf(PostgresqlRole{}),
	Summary: "one role (account) inside a PostgreSQL server that is installed and runs: its password, its attributes, and the roles it is a member of, or its removal; runs the node's psql program",
	Fields: append([]Field{
		{Name: "name", Required: true, Notes: "the role's name; a name longer than the server takes (63 bytes unless the server was built otherwise) fails the task before it changes anything"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the role and accepts only name and the connection fields; a missing role is unchanged; PostgreSQL refuses to remove a role that still owns a database or other objects"},
		{Name: "password", Notes: "the role's password; a private value, normally a secret; omitted: MMX does not touch the password; an empty password is rejected; MMX compares it with the stored form without a login attempt and sends the server only the SCRAM-SHA-256 form that it computes, never the password; a password stored in the older md5 form is replaced by the SCRAM-SHA-256 form; needs a login role that is a superuser, because only a superuser can read the stored form; a secret in it does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the password or the form"},
		{Name: "can_log_in", Notes: "required with state: present; true allows the role to log in; false makes it a role used only for privileges"},
		{Name: "superuser", Notes: "the role attribute SUPERUSER; omitted: an existing role keeps what it has, and a new role gets PostgreSQL's default, false"},
		{Name: "can_create_databases", Notes: "the role attribute CREATEDB; omitted as for superuser"},
		{Name: "can_create_roles", Notes: "the role attribute CREATEROLE; omitted as for superuser"},
		{Name: "can_start_replication", Notes: "the role attribute REPLICATION; omitted as for superuser"},
		{Name: "member_of_roles", Notes: "roles that this role is a member of, among them PostgreSQL's predefined roles such as pg_monitor and, from PostgreSQL 14, pg_read_all_data; each must exist; needs member_of_roles_not_listed"},
		{Name: "member_of_roles_not_listed", Values: notListed, Notes: "required with member_of_roles: remove makes member_of_roles the whole set, and member_of_roles: [] removes every membership; keep adds the listed memberships and leaves the others alone; remove also deletes records that only grant administration of another role"},
	}, postgresqlConnectionFieldNotes()...),
	Example: "- postgresql_role:\n    name: shop\n    can_log_in: true\n    member_of_roles: [pg_monitor]\n    member_of_roles_not_listed: keep\n",
	Results: []string{"status", "change", "reason", "value.created", "value.changed_attributes"},
	Dryrun:  "reads the server's catalog and changes nothing; reports would_change with value.created and value.changed_attributes as a run would report them; not_checked while the psql program, the operating system account postgres, a running server, or a role in member_of_roles is missing, because an earlier node task may provide each; a login that the server refuses fails the dryrun as it fails a run",
}

// postgresqlRoleAttributes are the yes-or-no attributes of a role that a
// task can set, by the names of the task's fields.
var postgresqlRoleAttributes = []struct {
	field string
	// on and off are the words of CREATE ROLE and ALTER ROLE.
	on, off string
	written func(*PostgresqlRole) *bool
}{
	{"can_log_in", "LOGIN", "NOLOGIN", func(p *PostgresqlRole) *bool { return p.CanLogIn }},
	{"superuser", "SUPERUSER", "NOSUPERUSER", func(p *PostgresqlRole) *bool { return p.Superuser }},
	{"can_create_databases", "CREATEDB", "NOCREATEDB", func(p *PostgresqlRole) *bool { return p.CanCreateDatabases }},
	{"can_create_roles", "CREATEROLE", "NOCREATEROLE", func(p *PostgresqlRole) *bool { return p.CanCreateRoles }},
	{"can_start_replication", "REPLICATION", "NOREPLICATION", func(p *PostgresqlRole) *bool { return p.CanStartReplication }},
}

// Prepare checks the inputs when the project loads.
func (p *PostgresqlRole) Prepare(base string) error {
	if p.State != "" && !slices.Contains(presenceStates, p.State) {
		return invalid("state", "state must be present or absent")
	}
	if p.State == "absent" {
		given := p.Password != nil || p.MemberOfRoles != nil || p.MemberOfRolesNotListed != ""
		for _, attribute := range postgresqlRoleAttributes {
			given = given || attribute.written(p) != nil
		}
		if given {
			return invalid("state", "postgresql_role with state: absent accepts only name and the connection fields")
		}
	}
	switch {
	case p.Password != nil && *p.Password == "":
		return invalid("password", `password must not be empty: PostgreSQL takes an empty password as "remove the password"; leave password out to keep the password that the role has`)
	case p.MemberOfRolesNotListed != "" && !slices.Contains(notListed, p.MemberOfRolesNotListed):
		return invalid("member_of_roles_not_listed", "member_of_roles_not_listed must be remove or keep")
	case p.MemberOfRoles != nil && p.MemberOfRolesNotListed == "":
		return invalid("member_of_roles_not_listed", "member_of_roles_not_listed is required with member_of_roles: remove makes member_of_roles the whole set of the role's memberships, keep leaves its other memberships alone")
	case p.MemberOfRoles == nil && p.MemberOfRolesNotListed != "":
		return invalid("member_of_roles_not_listed", "member_of_roles_not_listed requires member_of_roles (use member_of_roles: [] with remove to remove every membership)")
	}
	if err := postgresqlListedTwice("member_of_roles", p.MemberOfRoles); err != nil {
		return err
	}
	if p.CanLogIn == nil && p.State != "absent" {
		return invalid("can_log_in", "can_log_in is required with state: present; add can_log_in: true to allow login or can_log_in: false for a role used only for privileges")
	}
	return validatePostgresqlConnection(p.DatabaseServerHost, p.DatabaseServerCertificateAuthorityFile, p.LoginPassword, p.DangerouslySkipServerCertificateCheck)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *PostgresqlRole) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &p.Name, postgresqlNameRule)
	for i := range p.MemberOfRoles {
		arguments.Public(fmt.Sprintf("member_of_roles[%d]", i), &p.MemberOfRoles[i], postgresqlNameRule)
	}
	if p.Password != nil {
		arguments.LoginHandedOnlyToATool("password", *p.Password, postgresqlPasswordRule)
	}
	postgresqlConnectionArguments(arguments, &p.DatabaseServerHost, &p.DatabaseServerPort, &p.DatabaseServerCertificateAuthorityFile, p.LoginRole, p.LoginPassword)
}

// Run does the module's work on the node.
func (p *PostgresqlRole) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PostgresqlRole(ctx, *p, task.Scope, task.Packages)
}

func (p PostgresqlRole) connection() postgresqlConnection {
	return postgresqlConnection{module: "postgresql_role", host: p.DatabaseServerHost, port: postgresqlPort(p.DatabaseServerPort),
		certificateAuthorityFile: p.DatabaseServerCertificateAuthorityFile, skipServerCertificateCheck: p.DangerouslySkipServerCertificateCheck, loginRole: p.LoginRole, loginPassword: p.LoginPassword}
}

// postgresqlRoleState is what one query says about a role, and about the
// server and the login that the task needs to know first.
type postgresqlRoleState struct {
	// MaxIdentifierLength is the longest name the server takes, in bytes.
	MaxIdentifierLength int `json:"max_identifier_length"`
	// ScramIterations is the server's setting of that name, which
	// PostgreSQL 16 and newer have; nil on an older server.
	ScramIterations *int `json:"scram_iterations"`
	// MayReadStoredPasswords: the login role may read pg_authid, which
	// holds the stored form of every password. Only a superuser may.
	MayReadStoredPasswords bool `json:"may_read_stored_passwords"`
	// Role is nil when the role does not exist. Its keys are the task's
	// field names.
	Role map[string]bool `json:"role"`
	// Memberships are the records of the roles that this role is a member
	// of.
	Memberships []postgresqlMembership `json:"memberships"`
	// ExistingRoles are the roles of the task's member_of_roles that exist.
	ExistingRoles []string `json:"existing_roles"`
}

// postgresqlMembership is one record of a membership. PostgreSQL 16 and
// newer keep one record for each role that granted the membership, and each
// says what it gives.
type postgresqlMembership struct {
	Role string `json:"role"`
	// GrantedBy is nil when the role that granted the membership is gone,
	// which only PostgreSQL 15 and older allow.
	GrantedBy *string `json:"granted_by"`
	// InheritOption and SetOption are nil on PostgreSQL 15 and older, where
	// every record gives the member the role's privileges.
	InheritOption *bool `json:"inherit_option"`
	SetOption     *bool `json:"set_option"`
}

// givesPrivileges reports whether the record makes its member a member in
// the ordinary sense: the member has the role's privileges, by itself or
// after SET ROLE. A record that gives neither only lets the member
// administer the role; PostgreSQL 16 and newer make such a record for the
// role that created another role.
func (m postgresqlMembership) givesPrivileges() bool {
	if m.InheritOption == nil && m.SetOption == nil {
		return true
	}
	return (m.InheritOption != nil && *m.InheritOption) || (m.SetOption != nil && *m.SetOption)
}

// memberOf lists the roles that the state's role is a member of, sorted.
func (state postgresqlRoleState) memberOf() []string {
	var roles []string
	for _, membership := range state.Memberships {
		if membership.givesPrivileges() && !slices.Contains(roles, membership.Role) {
			roles = append(roles, membership.Role)
		}
	}
	slices.Sort(roles)
	return roles
}

// readPostgresqlRoleState adds the query for a role's state to a script.
func readPostgresqlRoleState(script *postgresqlScript, name string, memberOf []string) {
	role := script.literal(name) + "::pg_catalog.text"
	script.statement(`select pg_catalog.jsonb_build_object(
		'max_identifier_length', pg_catalog.current_setting('max_identifier_length')::pg_catalog.int4,
		'scram_iterations', pg_catalog.current_setting('scram_iterations', true)::pg_catalog.int4,
		'may_read_stored_passwords', pg_catalog.has_table_privilege('pg_catalog.pg_authid', 'select'),
		'role', (select pg_catalog.jsonb_build_object('can_log_in', r.rolcanlogin, 'superuser', r.rolsuper, 'can_create_databases', r.rolcreatedb,
				'can_create_roles', r.rolcreaterole, 'can_start_replication', r.rolreplication)
			from pg_catalog.pg_roles r where r.rolname = ` + role + `),
		'memberships', (select coalesce(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('role', granted_role.rolname, 'granted_by', grantor_role.rolname,
				'inherit_option', pg_catalog.to_jsonb(m) -> 'inherit_option', 'set_option', pg_catalog.to_jsonb(m) -> 'set_option')
				order by granted_role.rolname, grantor_role.rolname), '[]'::pg_catalog.jsonb)
			from pg_catalog.pg_auth_members m
			join pg_catalog.pg_roles member_role on member_role.oid = m.member
			join pg_catalog.pg_roles granted_role on granted_role.oid = m.roleid
			left join pg_catalog.pg_roles grantor_role on grantor_role.oid = m.grantor
			where member_role.rolname = ` + role + `),
		'existing_roles', (select coalesce(pg_catalog.jsonb_agg(wanted.name), '[]'::pg_catalog.jsonb)
			from pg_catalog.jsonb_array_elements_text(` + script.list(memberOf) + `) as wanted(name)
			where exists (select 1 from pg_catalog.pg_roles r where r.rolname = wanted.name)))`)
}

// PostgresqlRole brings one role to the state its task says. It reads the
// role from the server's catalog, changes what differs in one transaction,
// and reads the role again, so its result comes from the two states and
// never from what psql printed while it worked.
func (run Runtime) PostgresqlRole(ctx context.Context, p PostgresqlRole, s value.Scope, packages PackageManager) protocol.Result {
	const module = "postgresql_role"
	// The role's password is a private value that psql gets only as the
	// stored form MMX computes from it, so a secret in it does not hide the
	// result; the scope remembers the password and each form.
	password, passwordIsSecret := "", false
	if p.Password != nil {
		resolved, err := s.LoginHandedOnlyToATool(*p.Password)
		if err == nil {
			password, err = value.Scalar(resolved)
		}
		if err == nil {
			err = postgresqlPasswordRule(password)
		}
		if err != nil {
			return postgresqlBeforeAnyChange(protocol.Failed(fmt.Errorf("%s.password: %w", module, err)))
		}
		passwordIsSecret = value.HasSecrets(resolved)
	}
	client, stop := run.postgresqlClient(ctx, p.connection(), s, packages)
	if stop != nil {
		return *stop
	}
	defer client.close()

	// state reads the role's state from what a script's one query printed.
	state := func(what string, printed []json.RawMessage, err error) (postgresqlRoleState, error) {
		var read postgresqlRoleState
		if err == nil && (len(printed) != 1 || json.Unmarshal(printed[0], &read) != nil) {
			err = fmt.Errorf("%s: could not %s: psql did not print the JSON value that MMX asked for", module, what)
		}
		return read, err
	}
	script := postgresqlReadOnly()
	readPostgresqlRoleState(script, p.Name, p.MemberOfRoles)
	script.statement("commit")
	printed, err := client.run(ctx, postgresqlMaintenanceDatabase, "read the role "+p.Name, script)
	before, err := state("read the role "+p.Name, printed, err)
	if err != nil {
		return postgresqlFailure(err)
	}
	if err := postgresqlNameTooLong(module, "name", p.Name, before.MaxIdentifierLength); err != nil {
		return postgresqlBeforeAnyChange(protocol.ConfigurationError(err))
	}
	exists := before.Role != nil

	if p.State == "absent" {
		if !exists {
			return postgresqlChangeResult("unchanged", false, nil)
		}
		if run.Dryrun {
			r := postgresqlChangeResult("would_change", false, nil)
			r.Reason = "would remove the role " + p.Name
			return r
		}
		if err := run.Mutation("postgresql role " + p.Name); err != nil {
			return postgresqlBeforeAnyChange(protocol.Failed(err))
		}
		script := postgresqlChangingScript()
		script.statement("drop role " + script.identifier(p.Name))
		script.statementsDone()
		readPostgresqlRoleState(script, p.Name, nil)
		printed, change, err := client.change(ctx, postgresqlMaintenanceDatabase, "remove the role "+p.Name, script)
		removed := postgresqlChangeResult("changed", false, nil)
		removed.Reason = "removed the role " + p.Name
		after, err := state("read the role "+p.Name+" after its removal", printed, err)
		switch {
		case err != nil:
			// The server removes a role whole or not at all. When it
			// refused, nothing changed; when only the read after the
			// removal failed, the role is gone.
			r := postgresqlFailure(err)
			if r.Change = change; change == "changed" {
				r.Value = removed.Value
			}
			return r
		case after.Role != nil:
			return protocol.Failed(fmt.Errorf("%s: the server accepted DROP ROLE, but the role %s still exists", module, p.Name))
		}
		return removed
	}

	// A role of member_of_roles that does not exist is one that an earlier node
	// task may still create.
	var missing []string
	for _, role := range p.MemberOfRoles {
		if !slices.Contains(before.ExistingRoles, role) {
			missing = append(missing, role)
		}
	}
	if len(missing) > 0 {
		roles := "the role " + missing[0] + " does not exist"
		if len(missing) > 1 {
			roles = "the roles " + strings.Join(missing, ", ") + " do not exist"
		}
		return postgresqlBeforeAnyChange(run.notYet(fmt.Errorf("%s.member_of_roles: %s in the PostgreSQL server %s; create each in an earlier node task. A predefined role exists only from the PostgreSQL version that brought it: pg_read_all_data and pg_write_all_data from version 14",
			module, roles, client.connection.where()))) //nolint:nilaway // The preceding constructor failure check guarantees that client exists.
	}

	// What differs. A new role gets what the task writes and PostgreSQL's
	// defaults for the rest; an existing role keeps what the task omits.
	var attributes, differing []string
	for _, attribute := range postgresqlRoleAttributes {
		wanted := attribute.written(&p)
		if wanted == nil {
			continue
		}
		word := attribute.off
		if *wanted {
			word = attribute.on
		}
		if !exists || before.Role[attribute.field] != *wanted {
			attributes = append(attributes, word)
			differing = append(differing, attribute.field)
		}
	}
	setPassword, replacesMD5Form := false, false
	if p.Password != nil {
		if !before.MayReadStoredPasswords {
			return postgresqlBeforeAnyChange(protocol.ConfigurationError(fmt.Errorf("%s: the login role %s is not a superuser and cannot read stored passwords from pg_catalog.pg_authid; MMX needs the stored password to compare with the password requested by the node task; log in as a superuser, or omit password",
				module, client.role))) //nolint:nilaway // The preceding constructor failure check guarantees that client exists.
		}
		stored := ""
		if exists {
			if stored, err = client.storedPassword(ctx, p.Name); err != nil {
				return postgresqlFailure(err)
			}
		}
		holds := postgresqlStoredFormHolds(stored, password)
		if holds && passwordIsSecret {
			// The stored form is the password in the form that MMX computed
			// with the server's salt.
			s.FormOfLoginHandedOnlyToATool(stored)
		}
		setPassword = !holds
		replacesMD5Form = exists && postgresqlStoredFormIsMD5(stored)
		if setPassword {
			differing = append(differing, "password")
		}
	}
	current := before.memberOf()
	var grant []string
	var revoke []postgresqlMembership
	if p.MemberOfRoles != nil {
		for _, role := range p.MemberOfRoles {
			if !slices.Contains(current, role) {
				grant = append(grant, role)
			}
		}
		if p.MemberOfRolesNotListed == "remove" {
			for _, membership := range before.Memberships {
				if !slices.Contains(p.MemberOfRoles, membership.Role) {
					revoke = append(revoke, membership)
				}
			}
		}
		if len(grant) > 0 || len(revoke) > 0 {
			differing = append(differing, "member_of_roles")
		}
	}
	if exists && len(differing) == 0 {
		return postgresqlChangeResult("unchanged", false, nil)
	}
	// What the task's statements do when the server takes them all: a new
	// role, or these attributes of an existing one.
	planned := postgresqlChangeResult("changed", true, nil)
	if exists {
		planned = postgresqlChangeResult("changed", false, differing)
	}
	if run.Dryrun {
		r := planned
		r.Change, r.Reason = "would_change", "would create the role "+p.Name
		if exists {
			r.Reason = "would change " + strings.Join(differing, ", ") + " of the role " + p.Name
		}
		if replacesMD5Form {
			r.Reason += "; its password is stored in the older md5 form, which a run replaces with the SCRAM-SHA-256 form"
		}
		return r
	}
	if err := run.Mutation("postgresql role " + p.Name); err != nil {
		return postgresqlBeforeAnyChange(protocol.Failed(err))
	}

	// One transaction: the role is created with everything the task says,
	// or changed in everything that differs, or nothing happens at all.
	script = postgresqlChangingScript()
	role := script.identifier(p.Name)
	script.statement("begin")
	switch {
	case !exists:
		script.statement("create role " + role + " " + strings.Join(attributes, " "))
	case len(attributes) > 0:
		script.statement("alter role " + role + " " + strings.Join(attributes, " "))
	}
	if setPassword {
		iterations := 0
		if before.ScramIterations != nil {
			iterations = *before.ScramIterations
		}
		form, err := newPostgresqlScramForm(password, iterations)
		if err != nil {
			return postgresqlBeforeAnyChange(protocol.Failed(fmt.Errorf("%s: could not compute the stored form of the password: %w", module, err)))
		}
		if passwordIsSecret {
			s.FormOfLoginHandedOnlyToATool(form)
		}
		// The statement holds the form and never the password, as the
		// \password command of psql sends it.
		script.statement("alter role " + role + " password " + script.literal(form))
	}
	for _, granted := range grant {
		script.statement("grant " + script.identifier(granted) + " to " + role)
	}
	for _, membership := range revoke {
		// One REVOKE for each record, in the name of the role that granted
		// it: from PostgreSQL 16 a plain REVOKE removes only the record of
		// the role that runs it, and reports success.
		statement := "revoke " + script.identifier(membership.Role) + " from " + role
		if membership.GrantedBy != nil {
			statement += " granted by " + script.identifier(*membership.GrantedBy)
		}
		script.statement(statement)
	}
	script.statement("commit")
	script.statementsDone()
	readPostgresqlRoleState(script, p.Name, p.MemberOfRoles)
	what := "change the role " + p.Name
	if !exists {
		what = "create the role " + p.Name
	}
	printed, change, err := client.change(ctx, postgresqlMaintenanceDatabase, what, script)
	after, err := state("read the role "+p.Name+" after its change", printed, err)
	if err == nil && after.Role == nil {
		err = fmt.Errorf("%s: the server accepted the statements for the role %s, but the role does not exist afterwards", module, p.Name)
	}
	if err != nil {
		// A statement that the server refuses ends the script before its
		// COMMIT, and the server drops the open transaction with the
		// session: the role is as it was. A script whose statements were
		// done committed them; only the read after them failed then, and
		// the result says what the statements do. A session that ended
		// somewhere in the script leaves the change unknown.
		r := postgresqlFailure(err)
		if r.Change = change; change == "changed" {
			r.Value = planned.Value
		}
		return r
	}

	// The result comes from the two states. The password is the exception:
	// its statement was part of the transaction that the server committed.
	var changed []string
	for _, attribute := range postgresqlRoleAttributes {
		if exists && before.Role[attribute.field] != after.Role[attribute.field] {
			changed = append(changed, attribute.field)
		}
	}
	if exists && setPassword {
		changed = append(changed, "password")
	}
	if exists && !reflect.DeepEqual(before.Memberships, after.Memberships) {
		changed = append(changed, "member_of_roles")
	}
	r := postgresqlChangeResult("changed", !exists, changed)
	if replacesMD5Form {
		r.Reason = "the password of the role " + p.Name + " was stored in the older md5 form and is now stored in the SCRAM-SHA-256 form"
	}
	// A statement can succeed and leave a difference: a GRANT changes
	// nothing while a record that only lets the role administer the other
	// role stands in its way.
	if failure := p.differenceLeft(after); failure != nil {
		failed := protocol.Failed(fmt.Errorf("%s: %w", module, failure))
		failed.Change = "unchanged"
		if !exists || len(changed) > 0 {
			failed.Change, failed.Value = "changed", r.Value
		}
		return failed
	}
	return r
}

// differenceLeft compares the role after the task's statements with what
// the task says, and returns what still differs.
func (p PostgresqlRole) differenceLeft(after postgresqlRoleState) error {
	var left []string
	for _, attribute := range postgresqlRoleAttributes {
		if wanted := attribute.written(&p); wanted != nil && after.Role[attribute.field] != *wanted {
			left = append(left, fmt.Sprintf("%s is %v", attribute.field, after.Role[attribute.field]))
		}
	}
	// What PostgreSQL 16 changed explains only a membership that is left.
	aboutMemberships := ""
	if p.MemberOfRoles != nil {
		memberOf := after.memberOf()
		for _, role := range p.MemberOfRoles {
			if !slices.Contains(memberOf, role) {
				left = append(left, "the role is still not a member of "+role)
				aboutMemberships = ". PostgreSQL 16 and newer keep one membership record for each role that granted it; a record that only lets the role administer another role keeps a GRANT of the same role from taking effect. Compare the role's records in pg_auth_members"
			}
		}
		if p.MemberOfRolesNotListed == "remove" {
			for _, membership := range after.Memberships {
				role := membership.Role
				if !slices.Contains(p.MemberOfRoles, role) {
					left = append(left, "the role is still a member of "+role)
					aboutMemberships = ". PostgreSQL 16 and newer keep one membership record for each role that granted it; compare the role's records in pg_auth_members"
				}
			}
		}
	}
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("the server accepted the statements for the role %s, and the role is not as the task says: %s%s", p.Name, strings.Join(left, "; "), aboutMemberships)
}

// storedPassword reads the stored form of a role's password, "" for a role
// without one. Only a superuser can read it, so a task with password needs
// a login role that is one.
func (client *postgresqlClient) storedPassword(ctx context.Context, name string) (string, error) {
	script := postgresqlReadOnly()
	script.statement("select pg_catalog.jsonb_build_object('stored_password', (select a.rolpassword from pg_catalog.pg_authid a where a.rolname = " + script.literal(name) + "::pg_catalog.text))")
	script.statement("commit")
	printed, err := client.run(ctx, postgresqlMaintenanceDatabase, "read the stored form of the password of the role "+name, script)
	if err != nil {
		return "", err
	}
	var answer struct {
		StoredPassword *string `json:"stored_password"`
	}
	if len(printed) != 1 || json.Unmarshal(printed[0], &answer) != nil {
		return "", fmt.Errorf("%s: could not read the stored form of the password of the role %s: psql did not print the JSON value that MMX asked for", client.connection.module, name) //nolint:nilaway // storedPassword runs only after PostgresqlRole checks constructor failure.
	}
	if answer.StoredPassword == nil {
		return "", nil
	}
	return *answer.StoredPassword, nil
}
