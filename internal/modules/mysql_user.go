package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// MySQLUser is one account inside a MySQL or MariaDB server, with its
// password and its privileges. An account is a user name and the machines
// it may connect from: shop@localhost and shop@% are two accounts.
// Password, LoginUser, and LoginPassword are private values. The module
// hands the login only to the database's client (resolveMySQLLogin), and of
// Password it sends the server only the stored value that it computes.
type MySQLUser struct {
	Name           string `yaml:"name" json:"name"`
	MayConnectFrom string `yaml:"may_connect_from" json:"may_connect_from"`
	State          string `yaml:"state,omitempty" json:"state,omitempty"`
	// Password is nil when the task does not name it: MMX then does not
	// touch the password of an account that exists.
	Password *string `yaml:"password,omitempty" json:"password,omitempty"`
	// AllowCreatingAccountWithoutPassword is the written permission to create an
	// account that has no password.
	AllowCreatingAccountWithoutPassword    bool              `yaml:"allow_creating_account_without_password,omitempty" json:"allow_creating_account_without_password,omitempty"`
	Privileges                             []MySQLPrivileges `yaml:"privileges,omitempty" json:"privileges,omitempty"`
	PrivilegesNotListed                    string            `yaml:"privileges_not_listed,omitempty" json:"privileges_not_listed,omitempty"`
	DatabaseServerHost                     string            `yaml:"database_server_host,omitempty" json:"database_server_host,omitempty"`
	DatabaseServerPort                     any               `yaml:"database_server_port,omitempty" json:"database_server_port,omitempty"`
	LoginUser                              string            `yaml:"login_user,omitempty" json:"login_user,omitempty"`
	LoginPassword                          string            `yaml:"login_password,omitempty" json:"login_password,omitempty"`
	DatabaseServerCertificateAuthorityFile string            `yaml:"database_server_certificate_authority_file,omitempty" json:"database_server_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck  bool              `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
}

// MySQLPrivileges is one entry of privileges: what the privileges apply to,
// and their names.
type MySQLPrivileges struct {
	GrantOn                  string   `yaml:"grant_on,omitempty" json:"grant_on,omitempty"`
	GrantOnDatabasesMatching string   `yaml:"grant_on_databases_matching,omitempty" json:"grant_on_databases_matching,omitempty"`
	Grant                    []string `yaml:"grant" json:"grant"`
}

const (
	mysqlPrivilegesRemove = "remove"
	mysqlPrivilegesKeep   = "keep"
	// The two names in grant that are no privilege of a list: every
	// privilege of the place, and the right to pass privileges on.
	mysqlAllPrivileges = "ALL"
	mysqlGrantOption   = "GRANT OPTION"
)

var mysqlPrivilegesNotListedChoices = []string{mysqlPrivilegesRemove, mysqlPrivilegesKeep}

// mysqlUserDescription is what mmx describe-module shows for mysql_user.
var mysqlUserDescription = Description{
	Name: "mysql_user", Topic: TopicDatabases, Schema: reflect.TypeOf(MySQLUser{}),
	Summary: "one account inside a MySQL or MariaDB server that is installed and running, with its password and its privileges; MMX runs the node's own client, mariadb or mysql",
	Fields: append([]Field{
		{Name: "name", Required: true, Notes: "the account's user name"},
		{Name: "may_connect_from", Required: true, Notes: "the host part of the account: the machines this account may connect from, in the server's own pattern form, such as localhost, 10.0.%, or % for any; shop@localhost and shop@% are two accounts; compared in lower case, as the servers store it"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the account and accepts only name, may_connect_from, and the connection fields; an account that does not exist is unchanged"},
		{Name: "password", Notes: "the account's password; a private value, normally a secret; omitted: MMX does not touch the password of an account that exists, and a task that would create the account fails unless it says allow_creating_account_without_password: true; MMX compares without a login: it computes the stored value from the password as the server does and sends the server only that value; works for the authentication methods mysql_native_password and caching_sha2_password, and fails with the method's name for an account that logs in with another; a secret in it does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the password or its stored value"},
		{Name: "allow_creating_account_without_password", Default: "false", Notes: "true: a new account may be created without a password; anyone can then log in as it from the machines in may_connect_from, with all its privileges, as the account of a load balancer's health check is meant to; without it, a task that would create an account and has no password fails; changes nothing for an account that exists; refused beside password"},
		{Name: "privileges", Notes: "a list of {grant_on, grant}, or {grant_on_databases_matching, grant}; grant_on names *.* for the server, shop_db.* for one literal database, or shop_db.orders for a table; MMX escapes database _ and % automatically; grant_on_databases_matching instead takes a database pattern such as shop_% (without .*) and grants on every matching database; grant names privileges such as SELECT, ALL, or GRANT OPTION; [] with privileges_not_listed: remove takes every privilege away. With MySQL partial_revokes enabled, _ and % are literal, so grant_on_databases_matching: shop_% does not match shop_orders and the server needs one grant_on entry per database."},
		{Name: "privileges_not_listed", Values: mysqlPrivilegesNotListedChoices, Notes: "required with privileges, no default: what happens to a privilege the account has and the list does not name; remove means that the account afterwards holds exactly the listed privileges and no other, also none on another database or table, on a column, or on a stored procedure or function; roles and proxy rights are not touched; with keep the task only adds"},
	}, mysqlConnectionFieldNotes()...),
	Example: "- mysql_user:\n    name: shop\n    may_connect_from: \"10.0.%\"\n    password: \"{{ .secrets.shop_database_password }}\"\n    privileges:\n      - grant_on: \"shop.*\"\n        grant: [SELECT, INSERT, UPDATE, DELETE]\n    privileges_not_listed: remove\n",
	Results: []string{"status", "change", "reason", "value.created", "value.changed_attributes"},
	Dryrun:  "reads the server's catalog and changes nothing; reports would_change with value.created true for an account that a run would create, and with password or privileges in value.changed_attributes for what a run would change on an existing account; reason says which privileges a run would grant and revoke; it does not ask the server whether it knows a privilege's name: a run fails with the server's message for a name it refuses; fails, as a run does, for a refused login and for an account whose password MMX cannot compare; not_checked while neither the program mariadb nor the program mysql is installed, while the server does not answer, while the file of database_server_certificate_authority_file is missing, and while a table that privileges name does not exist",
}

// connection is the task's connection fields.
func (u *MySQLUser) connection() mysqlConnection {
	return mysqlConnection{Host: u.DatabaseServerHost, Port: u.DatabaseServerPort, LoginUser: u.LoginUser, LoginPassword: u.LoginPassword, CertificateAuthorityFile: u.DatabaseServerCertificateAuthorityFile, SkipServerCertificateCheck: u.DangerouslySkipServerCertificateCheck}
}

// Prepare checks the inputs when the project loads.
func (u *MySQLUser) Prepare(base string) error {
	switch {
	case u.Name == "":
		return invalid("name", "mysql_user.name is required: the account's user name")
	case u.MayConnectFrom == "":
		return invalid("may_connect_from", "mysql_user.may_connect_from is required: the machines the account may connect from, such as localhost, 10.0.%, or % for any; in MySQL and MariaDB shop@localhost and shop@% are two accounts")
	case u.State != "" && !slices.Contains(presenceStates, u.State):
		return invalid("state", "state must be "+strings.Join(presenceStates, " or "))
	}
	if u.State == "absent" {
		for _, written := range []struct {
			field string
			set   bool
		}{{"password", u.Password != nil}, {"allow_creating_account_without_password", u.AllowCreatingAccountWithoutPassword}, {"privileges", u.Privileges != nil}, {"privileges_not_listed", u.PrivilegesNotListed != ""}} {
			if written.set {
				return invalid(written.field, "state: absent accepts only name, may_connect_from, and the connection fields; remove "+written.field)
			}
		}
		return u.connection().check()
	}
	switch {
	case u.Password != nil && u.AllowCreatingAccountWithoutPassword:
		return invalid("allow_creating_account_without_password", "allow_creating_account_without_password cannot be combined with password; remove allow_creating_account_without_password or password")
	case u.Privileges != nil && u.PrivilegesNotListed == "":
		return invalid("privileges_not_listed", "privileges requires privileges_not_listed: remove or keep; no default is provided because privileges_not_listed decides whether a rerun removes account privileges")
	case u.PrivilegesNotListed != "" && !slices.Contains(mysqlPrivilegesNotListedChoices, u.PrivilegesNotListed):
		return invalid("privileges_not_listed", "privileges_not_listed must be "+strings.Join(mysqlPrivilegesNotListedChoices, " or "))
	case u.PrivilegesNotListed != "" && u.Privileges == nil:
		return invalid("privileges_not_listed", "privileges_not_listed needs privileges; to take every privilege away from the account, write privileges: [] with privileges_not_listed: remove")
	}
	for i, entry := range u.Privileges {
		switch {
		case entry.GrantOn == "" && entry.GrantOnDatabasesMatching == "":
			return invalid(fmt.Sprintf("privileges[%d].grant_on", i), "each of privileges needs grant_on for a literal database or table; to grant on databases matching a pattern, add grant_on_databases_matching: shop_% with the intended pattern")
		case entry.GrantOn != "" && entry.GrantOnDatabasesMatching != "":
			return invalid(fmt.Sprintf("privileges[%d].grant_on", i), "write exactly one of grant_on and grant_on_databases_matching; grant_on_databases_matching grants on every database matching its pattern")
		case len(entry.Grant) == 0:
			return invalid(fmt.Sprintf("privileges[%d].grant", i), "each of privileges needs grant: at least one privilege name, such as SELECT, or ALL")
		}
	}
	// What can be checked without the values of references: the same place
	// or the same name twice.
	if !Dynamic(u.privilegeTexts()) {
		if _, err := u.wantedPrivileges(); err != nil {
			return err
		}
	}
	return u.connection().check()
}

// privilegeTexts lists every text that privileges holds.
func (u *MySQLUser) privilegeTexts() []any {
	var texts []any
	for _, entry := range u.Privileges {
		texts = append(texts, entry.GrantOn, entry.GrantOnDatabasesMatching)
		for _, name := range entry.Grant {
			texts = append(texts, name)
		}
	}
	return texts
}

// The password's message never repeats the value, which is a secret.
var mysqlPasswordRule = rule("password must not be empty: the server would let anyone log in to the account without a password; leave password out to keep the password that the account has", func(s string) bool { return s != "" })

// Arguments lists the inputs that may hold a reference, with their rules.
func (u *MySQLUser) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &u.Name, mysqlNameRule("name"))
	arguments.Public("may_connect_from", &u.MayConnectFrom, mysqlNameRule("may_connect_from"))
	if u.Password != nil {
		arguments.LoginHandedOnlyToATool("password", *u.Password, mysqlPasswordRule)
	}
	for i := range u.Privileges {
		entry := &u.Privileges[i]
		if entry.GrantOn != "" {
			arguments.Public(fmt.Sprintf("privileges[%d].grant_on", i), &entry.GrantOn, parsed(func(on string) error { _, err := parseMySQLPrivilegePlace(on); return err }))
		}
		if entry.GrantOnDatabasesMatching != "" {
			arguments.Public(fmt.Sprintf("privileges[%d].grant_on_databases_matching", i), &entry.GrantOnDatabasesMatching, mysqlNameRule("grant_on_databases_matching"))
		}
		for j := range entry.Grant {
			arguments.Public(fmt.Sprintf("privileges[%d].grant[%d]", i, j), &entry.Grant[j], parsed(func(name string) error { _, err := mysqlPrivilegeName(name); return err }))
		}
	}
	mysqlConnectionArguments(arguments, &u.DatabaseServerHost, &u.DatabaseServerPort, &u.DatabaseServerCertificateAuthorityFile, u.LoginUser, u.LoginPassword)
}

// Run does the module's work on the node.
func (u *MySQLUser) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.MySQLUser(ctx, *u, task.Scope, task.Packages)
}

// The kinds of place where an account can hold privileges. A task's grant_on
// names the first three; privileges on a column and on a stored procedure
// or function are read so that privileges_not_listed: remove can take them
// away.
const (
	mysqlPlaceServer   = "server"
	mysqlPlaceDatabase = "database"
	mysqlPlaceTable    = "table"
	mysqlPlaceColumn   = "column"
	mysqlPlaceRoutine  = "routine"
)

// mysqlPlace is one place where an account holds privileges.
type mysqlPlace struct {
	Kind string
	// Database is the database's name, or for the kind database the pattern
	// as the server stores it, such as shop\_db.
	Database string
	// Table is the table, and for the kind routine the routine's name.
	Table  string
	Column string
	// RoutineKind is FUNCTION or PROCEDURE, in the server's word.
	RoutineKind string
}

// String names the place as a task writes it, for a result's reason.
func (p mysqlPlace) String() string {
	switch p.Kind {
	case mysqlPlaceServer:
		return "*.*"
	case mysqlPlaceDatabase:
		return p.Database + ".*"
	case mysqlPlaceColumn:
		return "column " + p.Column + " of " + p.Database + "." + p.Table
	case mysqlPlaceRoutine:
		return strings.ToLower(p.RoutineKind) + " " + p.Database + "." + p.Table
	}
	return p.Database + "." + p.Table
}

// statement is the place as a GRANT or REVOKE statement names it; a column
// stands with the privilege's name instead.
func (p mysqlPlace) statement() string {
	switch p.Kind {
	case mysqlPlaceServer:
		return "*.*"
	case mysqlPlaceDatabase:
		return mysqlName(p.Database) + ".*"
	case mysqlPlaceRoutine:
		return p.RoutineKind + " " + mysqlName(p.Database) + "." + mysqlName(p.Table)
	}
	return mysqlName(p.Database) + "." + mysqlName(p.Table)
}

// before orders places for a result's reason and for the statements: the
// server, then databases, tables, columns, and routines, each by name.
func (p mysqlPlace) before(other mysqlPlace) int {
	rank := func(kind string) int {
		return slices.Index([]string{mysqlPlaceServer, mysqlPlaceDatabase, mysqlPlaceTable, mysqlPlaceColumn, mysqlPlaceRoutine}, kind)
	}
	if p.Kind != other.Kind {
		return rank(p.Kind) - rank(other.Kind)
	}
	return strings.Compare(p.Database+"\x00"+p.Table+"\x00"+p.Column+"\x00"+p.RoutineKind, other.Database+"\x00"+other.Table+"\x00"+other.Column+"\x00"+other.RoutineKind)
}

// nonBreakingSpace is what YAML's double quotes make of \_: an author who
// writes "shop\_db.*" there means the backslash and the underscore.
const nonBreakingSpace = " "

// parseMySQLPrivilegePlace reads a task's grant_on: *.* for the whole server,
// <database>.* for a database, or <database>.<table> for one table. The
// The parser leaves database text intact. wantedPrivileges escapes literal
// database names before comparing them with the server's stored patterns.
func parseMySQLPrivilegePlace(on string) (mysqlPlace, error) {
	const forms = "grant_on must be *.* for the whole server, a database such as shop.*, or a table such as shop.orders"
	switch {
	case strings.Contains(on, nonBreakingSpace):
		return mysqlPlace{}, errors.New(`grant_on holds a non-breaking space; write database names literally, such as shop_db.*`)
	case strings.ContainsFunc(on, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return mysqlPlace{}, errors.New("grant_on must not hold control characters")
	case strings.Contains(on, "`"):
		// MMX would quote the backticks as a part of the name.
		return mysqlPlace{}, errors.New("grant_on must not hold a backtick: MMX quotes the names itself; write each name plain, such as shop.orders or my-shop.*")
	case on == "*.*":
		return mysqlPlace{Kind: mysqlPlaceServer}, nil
	}
	if database, found := strings.CutSuffix(on, ".*"); found {
		if database == "" || database == "*" || strings.HasSuffix(database, ".") {
			return mysqlPlace{}, errors.New(forms)
		}
		return mysqlPlace{Kind: mysqlPlaceDatabase, Database: database}, nil
	}
	database, table, found := strings.Cut(on, ".")
	switch {
	case !found || database == "" || table == "" || database == "*" || table == "*":
		return mysqlPlace{}, errors.New(forms)
	case strings.Contains(table, "."):
		return mysqlPlace{}, fmt.Errorf("%s; %q has more than one dot, so MMX cannot tell where the database's name ends", forms, on)
	}
	return mysqlPlace{Kind: mysqlPlaceTable, Database: database, Table: table}, nil
}

// mysqlPrivilegeWords is what a privilege's name looks like: words of
// letters and digits, with a space or an underscore between them.
var mysqlPrivilegeWords = regexp.MustCompile(`^[A-Z][A-Z0-9]*([ _][A-Z0-9]+)*$`)

// mysqlPrivilegeName reads one name of a task's grant and returns it as the
// server's catalog writes privileges: in upper case, with one space between
// words. A privilege's name stands in a statement as it is, so it is only
// letters, spaces, and underscores.
func mysqlPrivilegeName(written string) (string, error) {
	name := strings.ToUpper(strings.Join(strings.Fields(written), " "))
	switch {
	case !mysqlPrivilegeWords.MatchString(name):
		return "", fmt.Errorf("%q is not a privilege's name; a name is words of letters, such as SELECT, CREATE TEMPORARY TABLES, or BACKUP_ADMIN, or ALL, or GRANT OPTION", written)
	case name == "USAGE":
		return "", errors.New("grant must omit USAGE because USAGE means the account has no privileges")
	case name == "ALL PRIVILEGES":
		return mysqlAllPrivileges, nil
	}
	return name, nil
}

// mysqlWantedPrivileges is what one entry of privileges asks for.
type mysqlWantedPrivileges struct {
	Place mysqlPlace
	// Names are the privileges by name, without ALL and GRANT OPTION.
	Names []string
	// All: every privilege that the server has for the place.
	All bool
	// GrantOption: the account may pass its privileges at the place on.
	GrantOption bool
}

// wantedPrivileges reads the task's privileges. It fails for an entry that
// names a place or a privilege twice.
func (u *MySQLUser) wantedPrivileges() ([]mysqlWantedPrivileges, error) {
	var wanted []mysqlWantedPrivileges
	for i, entry := range u.Privileges {
		place, err := parseMySQLPrivilegePlace(entry.GrantOn)
		field, written := "grant_on", entry.GrantOn
		if entry.GrantOnDatabasesMatching != "" {
			field, written = "grant_on_databases_matching", entry.GrantOnDatabasesMatching
			if entry.GrantOn != "" {
				return nil, invalid(fmt.Sprintf("privileges[%d].grant_on", i), "write exactly one of grant_on and grant_on_databases_matching; grant_on_databases_matching grants on every database matching its pattern")
			}
			err = mysqlNameRule(field)(written)
			place = mysqlPlace{Kind: mysqlPlaceDatabase, Database: written}
		} else if err == nil && place.Kind == mysqlPlaceDatabase {
			place.Database = strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(place.Database)
		}
		if err != nil {
			return nil, invalid(fmt.Sprintf("privileges[%d].%s", i, field), err.Error())
		}
		for _, earlier := range wanted {
			if earlier.Place == place {
				return nil, invalid(fmt.Sprintf("privileges[%d].%s", i, field), "privileges names "+written+" twice; write all its privileges in one entry")
			}
		}
		w := mysqlWantedPrivileges{Place: place}
		for j, written := range entry.Grant {
			name, err := mysqlPrivilegeName(written)
			if err != nil {
				return nil, invalid(fmt.Sprintf("privileges[%d].grant[%d]", i, j), err.Error())
			}
			switch {
			case name == mysqlAllPrivileges && !w.All:
				w.All = true
			case name == mysqlGrantOption && !w.GrantOption:
				w.GrantOption = true
			case name == mysqlAllPrivileges || name == mysqlGrantOption || slices.Contains(w.Names, name):
				return nil, invalid(fmt.Sprintf("privileges[%d].grant[%d]", i, j), "grant names "+name+" twice")
			default:
				w.Names = append(w.Names, name)
			}
		}
		slices.Sort(w.Names)
		wanted = append(wanted, w)
	}
	return wanted, nil
}

// mysqlHeldPrivileges is what an account holds at one place.
type mysqlHeldPrivileges struct {
	Names map[string]bool
	// GrantOption: the account may pass on what it holds at the place, or
	// some of it. The servers keep one flag for each place, with one
	// exception: MySQL keeps the option of each dynamic privilege (such as
	// BACKUP_ADMIN, which exists only for the whole server) with that
	// privilege. GRANT GRANT OPTION ON *.* sets it for every dynamic
	// privilege that the account holds then, and a dynamic privilege that is
	// granted later comes without it.
	GrantOption bool
	// WithoutGrantOption: the account may not pass on everything that it
	// holds at the place. With GrantOption this is the mixed state of
	// MySQL's whole server.
	WithoutGrantOption bool
}

// mysqlAccountState is what the server's catalog says about one account.
type mysqlAccountState struct {
	Exists bool
	// Method is the account's authentication method (the servers call it a
	// plugin), and Stored its stored password value.
	Method, Stored string
	// Methods are the names of all methods of an account that may log in
	// with more than one, which MariaDB allows; it is empty for an account
	// with one method.
	Methods []string
	// Privileges are what the account holds, by place. A place where it
	// holds nothing and has no grant option is not in the map.
	Privileges map[mysqlPlace]*mysqlHeldPrivileges
}

// at returns what the account holds at place, an empty set when nothing.
func (a mysqlAccountState) at(place mysqlPlace) mysqlHeldPrivileges {
	if held := a.Privileges[place]; held != nil {
		return *held
	}
	return mysqlHeldPrivileges{}
}

// mysqlServerState is what a read of an account also learns about the
// server.
type mysqlServerState struct {
	// CachingSHA2Rounds is the server's setting
	// caching_sha2_password_digest_rounds, "" on a server without it.
	CachingSHA2Rounds string
	// RefusesStoredPasswords: a MariaDB server with a password check plugin
	// and strict_password_validation on refuses every stored value.
	RefusesStoredPasswords bool
	// PrivilegeCount is the number of privileges that the server has for a
	// kind of place: what ALL means there.
	PrivilegeCount map[string]int
	// Tables are the tables among those asked for that exist.
	Tables map[mysqlPlace]bool
	// DynamicPrivileges are the names of MySQL's dynamic privileges that
	// some account holds; MariaDB has none.
	DynamicPrivileges map[string]bool
}

// The first word of each row that the statements of account select. A row
// with another first word, outside the list of the server's privileges, is
// one that MMX does not know.
const (
	mysqlRowAccount             = "account"
	mysqlRowServerPrivilege     = "server privilege"
	mysqlRowDynamicPrivilege    = "dynamic privilege"
	mysqlRowDynamicPrivileges   = "dynamic privilege of the server"
	mysqlRowDatabasePrivilege   = "database privilege"
	mysqlRowTablePrivilege      = "table privilege"
	mysqlRowColumnPrivilege     = "column privilege"
	mysqlRowRoutinePrivilege    = "routine privilege"
	mysqlRowDatabasePrivileges  = "privileges of a database"
	mysqlRowTablePrivileges     = "privileges of a table"
	mysqlRowServerPrivileges    = "privileges of the server begin"
	mysqlRowServerPrivilegesEnd = "privileges of the server end"
	mysqlRowExistingTable       = "existing table"
	mysqlRowPasswordChecks      = "password check plugins"
	mysqlSettingRounds          = "caching_sha2_password_digest_rounds"
	mysqlSettingStrictPasswords = "strict_password_validation"
)

// account reads one account, with its privileges, from the server's
// catalog, in one call of the client. tables are the tables of which the
// caller needs to know whether they exist.
//
// The two products keep the stored password in different places: MySQL in
// the table mysql.user, MariaDB in a JSON document of mysql.global_priv,
// which alone shows a second method of an account (auth_or). Privileges are
// rows of information_schema on both, and of mysql.procs_priv for stored
// procedures and functions.
//
// One kind of privilege is not read from information_schema: MySQL's
// dynamic privileges (BACKUP_ADMIN and the like, which exist only for the
// whole server). After REVOKE GRANT OPTION ON *.*, MySQL's
// information_schema.USER_PRIVILEGES shows each of them twice, once still
// with the option, and it keeps showing a dynamic privilege that is revoked
// after that, until the server reads its tables again (measured on MySQL
// 8.0.46 and 9.7.1). The grant table mysql.global_grants is right at every
// moment. So for MySQL the account's dynamic privileges, each with its
// option, are the rows of that table, and a row of the catalog counts only
// when no account's row of the table has its name. A stale row for a
// dynamic privilege that no account holds at all would still count; root
// holds every dynamic privilege on an installed server, and a REVOKE of
// such a privilege succeeds and removes the row.
func (s *mysqlServer) account(ctx context.Context, name, host string, tables []mysqlPlace) (mysqlAccountState, mysqlServerState, error) {
	user, from := mysqlHexadecimal(name), mysqlHexadecimal(host)
	// A grantee is 'user'@'host', with quotes inside the names as they are.
	grantee := mysqlHexadecimal("'" + name + "'@'" + host + "'")
	var statements strings.Builder
	line := func(format string, arguments ...any) { fmt.Fprintf(&statements, format+";\n", arguments...) }
	if s.mariaDB {
		line("SELECT '%s', IFNULL(JSON_VALUE(Priv, '$.plugin'), ''), IFNULL(HEX(JSON_VALUE(Priv, '$.authentication_string')), ''), IFNULL(HEX(JSON_QUERY(Priv, '$.auth_or')), '') FROM mysql.global_priv WHERE HEX(User) = '%s' AND HEX(Host) = '%s'", mysqlRowAccount, user, from)
		line("SHOW GLOBAL VARIABLES LIKE '%s'", mysqlSettingStrictPasswords)
		line("SELECT '%s', COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_TYPE = 'PASSWORD VALIDATION' AND PLUGIN_STATUS = 'ACTIVE'", mysqlRowPasswordChecks)
	} else {
		line("SELECT '%s', plugin, IFNULL(HEX(authentication_string), ''), '' FROM mysql.user WHERE HEX(User) = '%s' AND HEX(Host) = '%s'", mysqlRowAccount, user, from)
	}
	line("SHOW GLOBAL VARIABLES LIKE '%s'", mysqlSettingRounds)
	if s.mariaDB {
		line("SELECT '%s', PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES WHERE HEX(GRANTEE) = '%s'", mysqlRowServerPrivilege, grantee)
	} else {
		// MySQL's dynamic privileges come from the grant table itself, and
		// the catalog's rows only for the other privileges (mysqlDynamicPrivileges).
		line("SELECT '%s', PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES WHERE HEX(GRANTEE) = '%s' AND PRIVILEGE_TYPE NOT IN (SELECT PRIV FROM mysql.global_grants)", mysqlRowServerPrivilege, grantee)
		line("SELECT '%s', PRIV, WITH_GRANT_OPTION FROM mysql.global_grants WHERE HEX(USER) = '%s' AND HEX(HOST) = '%s'", mysqlRowDynamicPrivilege, user, from)
		line("SELECT '%s', PRIV FROM mysql.global_grants GROUP BY PRIV", mysqlRowDynamicPrivileges)
	}
	line("SELECT '%s', HEX(TABLE_SCHEMA), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.SCHEMA_PRIVILEGES WHERE HEX(GRANTEE) = '%s'", mysqlRowDatabasePrivilege, grantee)
	line("SELECT '%s', HEX(TABLE_SCHEMA), HEX(TABLE_NAME), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.TABLE_PRIVILEGES WHERE HEX(GRANTEE) = '%s'", mysqlRowTablePrivilege, grantee)
	line("SELECT '%s', HEX(TABLE_SCHEMA), HEX(TABLE_NAME), HEX(COLUMN_NAME), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.COLUMN_PRIVILEGES WHERE HEX(GRANTEE) = '%s'", mysqlRowColumnPrivilege, grantee)
	line("SELECT '%s', HEX(Db), HEX(Routine_name), Routine_type, Proc_priv FROM mysql.procs_priv WHERE HEX(User) = '%s' AND HEX(Host) = '%s'", mysqlRowRoutinePrivilege, user, from)
	// What ALL means at each kind of place, from the server's own lists.
	line("SELECT '%s', COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'db' AND COLUMN_NAME LIKE '%%!_priv' ESCAPE '!' AND COLUMN_NAME <> 'Grant_priv'", mysqlRowDatabasePrivileges)
	line("SELECT '%s', COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'tables_priv' AND COLUMN_NAME = 'Table_priv'", mysqlRowTablePrivileges)
	for _, table := range tables {
		line("SELECT '%s', HEX(TABLE_SCHEMA), HEX(TABLE_NAME) FROM information_schema.TABLES WHERE HEX(TABLE_SCHEMA) = '%s' AND HEX(TABLE_NAME) = '%s'", mysqlRowExistingTable, mysqlHexadecimal(table.Database), mysqlHexadecimal(table.Table))
	}
	line("SELECT '%s'", mysqlRowServerPrivileges)
	line("SHOW PRIVILEGES")
	line("SELECT '%s'", mysqlRowServerPrivilegesEnd)
	rows, failure := s.call(ctx, statements.String())
	if failure != nil {
		return mysqlAccountState{}, mysqlServerState{}, s.failure("reading the account "+name+"@"+host, failure)
	}
	account, server, err := readMySQLAccountRows(rows)
	if err != nil {
		return account, server, fmt.Errorf("%s: reading the account %s@%s on %s: %w", s.module, name, host, s.where, err)
	}
	return account, server, nil
}

// readMySQLAccountRows reads the rows that the statements of account
// selected.
func readMySQLAccountRows(rows [][]string) (mysqlAccountState, mysqlServerState, error) {
	account := mysqlAccountState{Privileges: map[mysqlPlace]*mysqlHeldPrivileges{}}
	server := mysqlServerState{PrivilegeCount: map[string]int{}, Tables: map[mysqlPlace]bool{}, DynamicPrivileges: map[string]bool{}}
	strictPasswords, passwordChecks := false, 0
	// hold notes one row of a privilege catalog. USAGE is the servers' word
	// for "no privilege"; its row only carries the place's grant option.
	hold := func(place mysqlPlace, name, grantable string) {
		held := account.Privileges[place]
		if held == nil {
			held = &mysqlHeldPrivileges{Names: map[string]bool{}}
			account.Privileges[place] = held
		}
		if name != "" && name != "USAGE" {
			held.Names[name] = true
		}
		held.GrantOption = held.GrantOption || grantable == "YES"
		held.WithoutGrantOption = held.WithoutGrantOption || grantable == "NO"
	}
	names := func(row []string, hexadecimal ...int) error {
		for _, column := range hexadecimal {
			decoded, err := mysqlFromHexadecimal(row[column])
			if err != nil {
				return err
			}
			row[column] = decoded
		}
		return nil
	}
	// The number of columns of each row, by its first word.
	columns := map[string]int{mysqlRowAccount: 4, mysqlRowServerPrivilege: 3, mysqlRowDynamicPrivilege: 3, mysqlRowDynamicPrivileges: 2, mysqlRowDatabasePrivilege: 4, mysqlRowTablePrivilege: 5, mysqlRowColumnPrivilege: 6,
		mysqlRowRoutinePrivilege: 5, mysqlRowDatabasePrivileges: 2, mysqlRowTablePrivileges: 2, mysqlRowServerPrivileges: 1, mysqlRowServerPrivilegesEnd: 1,
		mysqlRowExistingTable: 3, mysqlRowPasswordChecks: 2, mysqlSettingRounds: 2, mysqlSettingStrictPasswords: 2}
	inServerPrivileges := false
	for _, row := range rows {
		unknown := fmt.Errorf("the server printed a row that MMX does not know, with %d columns and the first word %q", len(row), row[0])
		if inServerPrivileges && row[0] != mysqlRowServerPrivilegesEnd {
			// SHOW PRIVILEGES lists the server's privileges with three
			// entries that are no privilege of a list.
			if word := strings.ToUpper(row[0]); word != mysqlGrantOption && word != "PROXY" && word != "USAGE" {
				server.PrivilegeCount[mysqlPlaceServer]++
			}
			continue
		}
		if columns[row[0]] != len(row) {
			return account, server, unknown
		}
		var err error
		switch row[0] {
		case mysqlRowAccount:
			if err = names(row, 2, 3); err != nil {
				break
			}
			account.Exists, account.Method, account.Stored = true, row[1], row[2]
			if row[3] != "" {
				account.Methods, err = mariaDBMethods(account.Method, row[3])
			}
		case mysqlRowServerPrivilege:
			hold(mysqlPlace{Kind: mysqlPlaceServer}, row[1], row[2])
		case mysqlRowDynamicPrivileges:
			server.DynamicPrivileges[row[1]] = true
		case mysqlRowDynamicPrivilege:
			// A row of mysql.global_grants, whose option is Y or N.
			switch row[2] {
			case "Y":
				hold(mysqlPlace{Kind: mysqlPlaceServer}, row[1], "YES")
			case "N":
				hold(mysqlPlace{Kind: mysqlPlaceServer}, row[1], "NO")
			default:
				err = unknown
			}
		case mysqlRowDatabasePrivilege:
			if err = names(row, 1); err == nil {
				hold(mysqlPlace{Kind: mysqlPlaceDatabase, Database: row[1]}, row[2], row[3])
			}
		case mysqlRowTablePrivilege:
			if err = names(row, 1, 2); err == nil {
				hold(mysqlPlace{Kind: mysqlPlaceTable, Database: row[1], Table: row[2]}, row[3], row[4])
			}
		case mysqlRowColumnPrivilege:
			if err = names(row, 1, 2, 3); err == nil {
				hold(mysqlPlace{Kind: mysqlPlaceColumn, Database: row[1], Table: row[2], Column: row[3]}, row[4], "")
				// The grant option of a column's row is its table's.
				if row[5] == "YES" {
					hold(mysqlPlace{Kind: mysqlPlaceTable, Database: row[1], Table: row[2]}, "", "YES")
				}
			}
		case mysqlRowRoutinePrivilege:
			if err = names(row, 1, 2); err == nil {
				place := mysqlPlace{Kind: mysqlPlaceRoutine, Database: row[1], Table: row[2], RoutineKind: row[3]}
				// Proc_priv is a set, such as Execute,Alter Routine,Grant.
				for _, member := range strings.Split(row[4], ",") {
					if member == "Grant" {
						hold(place, "", "YES")
					} else if member != "" {
						hold(place, strings.ToUpper(member), "")
					}
				}
			}
		case mysqlRowDatabasePrivileges:
			server.PrivilegeCount[mysqlPlaceDatabase], err = strconv.Atoi(row[1])
		case mysqlRowTablePrivileges:
			// The type of mysql.tables_priv.Table_priv, such as
			// set('Select','Insert','Grant'): its members without Grant.
			members, isSet := strings.CutPrefix(row[1], "set(")
			if !isSet {
				err = unknown
				break
			}
			for _, member := range strings.Split(strings.TrimSuffix(members, ")"), ",") {
				if member != "'Grant'" {
					server.PrivilegeCount[mysqlPlaceTable]++
				}
			}
		case mysqlRowServerPrivileges:
			inServerPrivileges = true
		case mysqlRowServerPrivilegesEnd:
			inServerPrivileges = false
		case mysqlRowExistingTable:
			if err = names(row, 1, 2); err == nil {
				server.Tables[mysqlPlace{Kind: mysqlPlaceTable, Database: row[1], Table: row[2]}] = true
			}
		case mysqlRowPasswordChecks:
			passwordChecks, err = strconv.Atoi(row[1])
		case mysqlSettingRounds:
			server.CachingSHA2Rounds = row[1]
		case mysqlSettingStrictPasswords:
			strictPasswords = row[1] == "ON"
		}
		if err != nil {
			return account, server, err
		}
	}
	if inServerPrivileges {
		return account, server, errors.New("the server's answer ends inside its list of privileges")
	}
	// A place with nothing held and no grant option is no place.
	maps.DeleteFunc(account.Privileges, func(_ mysqlPlace, held *mysqlHeldPrivileges) bool { return len(held.Names) == 0 && !held.GrantOption })
	server.RefusesStoredPasswords = strictPasswords && passwordChecks > 0
	return account, server, nil
}

// mariaDBMethods lists the methods of a MariaDB account that may log in
// with more than one. The account's auth_or is a JSON list with one entry
// for each method; an empty entry stands for the method of the account's
// top level.
func mariaDBMethods(topLevel, authOr string) ([]string, error) {
	var entries []struct {
		Plugin string `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(authOr), &entries); err != nil {
		return nil, errors.New("the account's list of authentication methods (auth_or) is not a JSON list")
	}
	var methods []string
	for _, entry := range entries {
		if entry.Plugin == "" {
			entry.Plugin = topLevel
		}
		methods = append(methods, entry.Plugin)
	}
	return methods, nil
}

// mysqlPrivilegeChange is one GRANT or REVOKE statement.
type mysqlPrivilegeChange struct {
	Revoke bool
	Place  mysqlPlace
	// Names are the privileges; All stands for every privilege of the
	// place, and GrantOption for the place's grant option, each in a
	// statement of its own.
	Names       []string
	All         bool
	GrantOption bool
}

// what names the privileges of the change, for a statement and for a
// result's reason.
func (c mysqlPrivilegeChange) what() string {
	switch {
	case c.GrantOption:
		return mysqlGrantOption
	case c.All:
		return "ALL PRIVILEGES"
	}
	return strings.Join(c.Names, ", ")
}

// statement is the change as SQL for the account.
func (c mysqlPrivilegeChange) statement(account string) string {
	what := c.what()
	if c.Place.Kind == mysqlPlaceColumn {
		// A column's privilege is written with the column: SELECT (`id`).
		columns := make([]string, len(c.Names))
		for i, name := range c.Names {
			columns[i] = name + " (" + mysqlName(c.Place.Column) + ")"
		}
		what = strings.Join(columns, ", ")
	}
	if c.Revoke {
		return "REVOKE " + what + " ON " + c.Place.statement() + " FROM " + account
	}
	return "GRANT " + what + " ON " + c.Place.statement() + " TO " + account
}

// String says the change in words, for a result's reason.
func (c mysqlPrivilegeChange) String() string {
	if c.Revoke {
		return "revoke " + c.what() + " on " + c.Place.String()
	}
	return "grant " + c.what() + " on " + c.Place.String()
}

// done says a change that a run made, for a result's reason.
func (c mysqlPrivilegeChange) done() string {
	if c.Revoke {
		return "revoked " + c.what() + " on " + c.Place.String()
	}
	return "granted " + c.what() + " on " + c.Place.String()
}

// mysqlRoutinePrivileges are the privileges on a stored procedure or
// function that MMX knows how to revoke, and mysqlRoutineKinds the kinds of
// routine. The servers add to both lists: MariaDB has packages, MySQL 9
// libraries.
var (
	mysqlRoutinePrivileges = []string{"EXECUTE", "ALTER ROUTINE", "SHOW CREATE ROUTINE"}
	mysqlRoutineKinds      = []string{"FUNCTION", "PROCEDURE"}
)

// mysqlPrivilegeChanges compares what the account holds with what the task
// asks for and returns the statements that make the two equal: first the
// grants, then, with privileges_not_listed: remove, the revokes of
// everything that the list does not name. The grants come first, so that
// an account whose privileges move from one place to another never stands
// without both.
//
// ALL is compared by number. The catalog has one row for each privilege
// that an account holds at a place, and the server's own lists say how many
// privileges it has for that kind of place; the number grows with the
// server's version, so MMX keeps no list of its own.
//
// The grant option is asked for when the account lacks it for anything
// that it holds at the place, and taken away when it has it for anything.
// Both statements reach everything at the place: GRANT GRANT OPTION ON *.*
// and REVOKE GRANT OPTION ON *.* also set and clear the option of every
// dynamic privilege that a MySQL account holds.
func mysqlPrivilegeChanges(account mysqlAccountState, server mysqlServerState, wanted []mysqlWantedPrivileges, notListed string) ([]mysqlPrivilegeChange, error) {
	var changes []mysqlPrivilegeChange
	asked := map[mysqlPlace]mysqlWantedPrivileges{}
	for _, w := range wanted {
		asked[w.Place] = w
		held := account.at(w.Place)
		var missing []string
		for _, name := range w.Names {
			if !held.Names[name] {
				missing = append(missing, name)
			}
		}
		// grantsDynamic: the statements grant a dynamic privilege, which
		// comes without the grant option also to an account that has the
		// option for everything else.
		grantsDynamic := false
		switch {
		case w.All && server.PrivilegeCount[w.Place.Kind] == 0:
			return nil, fmt.Errorf("the server's catalog does not say how many privileges it has for %s, so MMX cannot compare ALL", w.Place)
		case w.All && len(held.Names) < server.PrivilegeCount[w.Place.Kind]:
			changes = append(changes, mysqlPrivilegeChange{Place: w.Place, All: true})
			grantsDynamic = w.Place.Kind == mysqlPlaceServer && len(server.DynamicPrivileges) > 0
		case !w.All && len(missing) > 0:
			changes = append(changes, mysqlPrivilegeChange{Place: w.Place, Names: missing})
			grantsDynamic = w.Place.Kind == mysqlPlaceServer && slices.ContainsFunc(missing, func(name string) bool { return server.DynamicPrivileges[name] })
		}
		if w.GrantOption && (!held.GrantOption || held.WithoutGrantOption || grantsDynamic) {
			changes = append(changes, mysqlPrivilegeChange{Place: w.Place, GrantOption: true})
		}
	}
	if notListed != mysqlPrivilegesRemove {
		return changes, nil
	}
	places := slices.SortedFunc(maps.Keys(account.Privileges), mysqlPlace.before)
	for _, place := range places {
		held, w := account.at(place), asked[place]
		var extra []string
		for _, name := range slices.Sorted(maps.Keys(held.Names)) {
			if w.All || slices.Contains(w.Names, name) {
				continue
			}
			// The name comes from the server's catalog and stands in a
			// statement as it is.
			if !mysqlPrivilegeWords.MatchString(name) {
				return nil, fmt.Errorf("the account holds a privilege on %s whose name, %q, is not one that MMX can write into a statement; revoke it yourself, or use privileges_not_listed: keep", place, name)
			}
			extra = append(extra, name)
		}
		if place.Kind == mysqlPlaceRoutine {
			if !slices.Contains(mysqlRoutineKinds, place.RoutineKind) {
				return nil, fmt.Errorf("the account holds privileges on %s, a kind of stored routine that MMX does not know how to revoke; revoke them yourself, or use privileges_not_listed: keep", place)
			}
			for _, name := range extra {
				if !slices.Contains(mysqlRoutinePrivileges, name) {
					return nil, fmt.Errorf("the account holds the privilege %s on %s, which MMX does not know how to revoke; revoke it yourself, or use privileges_not_listed: keep", name, place)
				}
			}
		}
		// The grant option goes first: while the place still has a
		// privilege, the server knows the place for certain.
		if held.GrantOption && !w.GrantOption {
			changes = append(changes, mysqlPrivilegeChange{Revoke: true, Place: place, GrantOption: true})
		}
		if len(extra) > 0 {
			changes = append(changes, mysqlPrivilegeChange{Revoke: true, Place: place, Names: extra})
		}
	}
	return changes, nil
}

// mysqlPasswordIs reports whether stored is the stored value of password
// for the method. It fails for a stored value in a form that MMX does not
// know; the failure never holds the value.
func mysqlPasswordIs(method, stored, password string) (bool, error) {
	switch {
	case stored == "":
		return false, nil // an account without a password
	case method == mysqlNativePasswordMethod:
		return strings.EqualFold(stored, mysqlNativePassword(password)), nil
	}
	salt, rounds, known := mysqlCachingSHA2Parts([]byte(stored))
	if !known {
		return false, errors.New("the account's stored caching_sha2_password value has an unsupported format: the value must start with $A$ and three hexadecimal digits and be 70 characters long")
	}
	return mysqlCachingSHA2Password(password, salt, rounds) == stored, nil
}

// A stored value that MMX computed stands in a statement as one quoted
// string, so it holds none of the characters that end a string or mean
// something in one.
var mysqlPlainStoredValue = regexp.MustCompile(`^[A-Za-z0-9./$*]+$`)

// mysqlPasswordStatement creates the account, or changes its password, from
// the stored value. The two products write the same thing in different
// words, and neither takes the other's.
func mysqlPasswordStatement(mariaDB, create bool, account, method, stored string) string {
	statement := "ALTER USER " + account
	if create {
		statement = "CREATE USER " + account
	}
	switch {
	case stored == "":
		return statement
	case mariaDB:
		return statement + " IDENTIFIED VIA " + method + " USING '" + stored + "'"
	}
	return statement + " IDENTIFIED WITH " + method + " AS '" + stored + "'"
}

// The numbers of the errors with which the servers refuse a privilege's
// name, or a name at a place where it does not apply: the task's list is
// wrong, and another try gives the same answer.
var mysqlPrivilegeRefused = []int{1064, 1149, 3619, 1221, 1144}

// MySQLUser creates the account, sets its password, and grants and revokes
// privileges until the account is what the task says, or removes the
// account. It reads the server's catalog before and after and reports what
// differs between the two, never what a statement printed.
func (run Runtime) MySQLUser(ctx context.Context, u MySQLUser, scope value.Scope, packages PackageManager) protocol.Result {
	const module = "mysql_user"
	wanted, err := u.wantedPrivileges()
	if err != nil {
		return protocol.ConfigurationError(fmt.Errorf("%s.%w", module, err))
	}
	// The servers store the host part in lower case, and MySQL compares it
	// without regard to case.
	host := strings.ToLower(u.MayConnectFrom)
	account, named := mysqlName(u.Name)+"@"+mysqlName(host), u.Name+"@"+host
	password, secretPassword := "", false
	if u.Password != nil {
		resolved, err := scope.LoginHandedOnlyToATool(*u.Password)
		if err == nil {
			password, err = value.Scalar(resolved)
		}
		if err != nil {
			return protocol.Failed(fmt.Errorf("%s.password: %w", module, err))
		}
		secretPassword = value.HasSecrets(resolved)
	}
	server, cleanup, stop := run.newMySQLServer(ctx, module, u.connection(), scope, packages)
	if stop != nil {
		return *stop
	}
	defer cleanup()
	// The two products keep an account's password in different places.
	if stop := server.askProduct(ctx); stop != nil {
		return *stop
	}
	var tables []mysqlPlace
	for _, w := range wanted {
		if w.Place.Kind == mysqlPlaceTable {
			tables = append(tables, w.Place)
		}
	}
	before, serverState, err := server.account(ctx, u.Name, host, tables)
	if err != nil {
		return unchangedFailure(err)
	}
	result := func(change string, created bool, attributes []string, reason string) protocol.Result {
		r := protocol.Success(change)
		r.Value, r.Reason = mysqlResultValue(created, attributes), reason
		return r
	}
	// refused is a task that this server cannot do as written: another try
	// gives the same answer, in a run and in a dryrun.
	refused := func(format string, arguments ...any) protocol.Result {
		r := protocol.ConfigurationError(fmt.Errorf(module+": "+format, arguments...))
		r.Change = "unchanged"
		return r
	}
	// afterwards reads the account again and reports what differs from
	// before, with the failure of a statement when there is one.
	afterwards := func(failure error, configuration bool, check func(after mysqlAccountState, afterServer mysqlServerState) error) protocol.Result {
		after, afterServer, err := server.account(ctx, u.Name, host, tables)
		if err != nil {
			return protocol.Failed(errors.Join(failure, err))
		}
		created := after.Exists && !before.Exists
		var attributes []string
		if before.Exists && after.Exists {
			if before.Method != after.Method || before.Stored != after.Stored {
				attributes = append(attributes, "password")
			}
			if !sameMySQLPrivileges(before.Privileges, after.Privileges) {
				attributes = append(attributes, "privileges")
			}
		}
		change := "unchanged"
		if before.Exists != after.Exists || len(attributes) > 0 {
			change = "changed"
		}
		if failure == nil && check != nil {
			failure = check(after, afterServer)
		}
		if failure != nil {
			r := protocol.Failed(failure)
			if configuration {
				r = protocol.ConfigurationError(failure)
			}
			r.Change, r.Value = change, mysqlResultValue(created, attributes)
			return r
		}
		return result(change, created, attributes, "")
	}

	if u.State == "absent" {
		if !before.Exists {
			return result("unchanged", false, nil, "")
		}
		if run.Dryrun {
			return result("would_change", false, nil, "would remove the account "+named)
		}
		if err := run.Mutation("mysql account " + named); err != nil {
			return unchangedFailure(err)
		}
		statement := "DROP USER " + account
		var failed error
		if failure := server.change(ctx, statement+";\n"); failure != nil {
			failed = server.failure(statement, failure)
		}
		removed := afterwards(failed, false, func(after mysqlAccountState, _ mysqlServerState) error {
			if after.Exists {
				return fmt.Errorf("%s: %s took the statement %s, and its catalog still has the account", module, server.where, statement)
			}
			return nil
		})
		if removed.Status == "succeeded" && removed.Change == "changed" {
			removed.Reason = "removed the account " + named
		}
		return removed
	}

	// The password: a new account gets the server's own method for new
	// accounts, an existing one keeps its method.
	method, setPassword := before.Method, false
	switch {
	case u.Password == nil:
	case !before.Exists:
		method, setPassword = mysqlCachingSHA2Method, true
		if server.mariaDB {
			method = mysqlNativePasswordMethod
		}
	case len(before.Methods) > 0:
		return refused("the account %s logs in with one of several authentication methods (%s), so a password would not decide how it logs in; MMX sets a password only for an account with the one method %s or %s. Remove password from the task to manage only the account's privileges", named, strings.Join(before.Methods, " or "), mysqlNativePasswordMethod, mysqlCachingSHA2Method)
	case before.Method != mysqlNativePasswordMethod && before.Method != mysqlCachingSHA2Method:
		return refused("the account %s logs in with the authentication method %s; MMX compares and sets a password only for the methods %s and %s. Remove password from the task to manage only the account's privileges", named, before.Method, mysqlNativePasswordMethod, mysqlCachingSHA2Method)
	default:
		same, err := mysqlPasswordIs(before.Method, before.Stored, password)
		if err != nil {
			return refused("the account %s: %w", named, err)
		}
		setPassword = !same
	}
	if setPassword && serverState.RefusesStoredPasswords {
		return refused("%s checks passwords with a plugin and its setting strict_password_validation is on, so it refuses every stored password value: it can check only a clear password. MMX does not send the clear password, because MariaDB writes it into its general log. Turn strict_password_validation off in the server's configuration, or remove password from the task", server.where)
	}
	create := !before.Exists
	// An account without a password is open to everyone who can reach the
	// server from may_connect_from. A forgotten password line must not make
	// one, so the task has to say that it means it.
	if create && u.Password == nil && !u.AllowCreatingAccountWithoutPassword {
		return refused("creating account %s without a password lets any machine in may_connect_from log in with all its privileges. Add password, or add allow_creating_account_without_password: true to create the account without a password", named)
	}
	// The catalog can hold privileges for an account that does not exist,
	// when someone deleted the account's row by hand; they would come to
	// life with the new account, so they count as held.
	changes, err := mysqlPrivilegeChanges(before, serverState, wanted, u.PrivilegesNotListed)
	if err != nil {
		return run.notKnown(fmt.Errorf("%s: the account %s: %w", module, named, err))
	}
	// The servers grant a privilege on a table only when the table exists,
	// unless CREATE is granted with it. An earlier node task may create it.
	for _, change := range changes {
		if !change.Revoke && !change.GrantOption && change.Place.Kind == mysqlPlaceTable && !serverState.Tables[change.Place] && !change.All && !slices.Contains(change.Names, "CREATE") {
			return run.notYet(fmt.Errorf("%s: the table %s does not exist; the server requires an existing table unless the requested privileges include CREATE; create the table in an earlier node task", module, change.Place))
		}
	}
	if !create && !setPassword && len(changes) == 0 {
		return result("unchanged", false, nil, "")
	}
	// What a run does, as a dryrun says it and as a run says it.
	var planned, done []string
	switch {
	case create && setPassword:
		planned, done = append(planned, "would create the account "+named), append(done, "created the account "+named)
	case create:
		planned, done = append(planned, "would create the account "+named+" without a password"), append(done, "created the account "+named+" without a password")
	case setPassword:
		planned, done = append(planned, "would set the password"), append(done, "set the password")
	}
	for _, change := range changes {
		planned, done = append(planned, "would "+change.String()), append(done, change.done())
	}
	if run.Dryrun {
		var attributes []string
		if !create && setPassword {
			attributes = append(attributes, "password")
		}
		if !create && len(changes) > 0 {
			attributes = append(attributes, "privileges")
		}
		return result("would_change", create, attributes, strings.Join(planned, "; "))
	}
	if err := run.Mutation("mysql account " + named); err != nil {
		return unchangedFailure(err)
	}
	if create || setPassword {
		stored := ""
		if setPassword {
			if stored, err = newMySQLStoredValue(method, password, serverState.CachingSHA2Rounds); err != nil {
				return unchangedFailure(fmt.Errorf("%s: %w", module, err))
			}
			// The stored value travels to the server in a statement, and a
			// client or a server that fails may print the statement.
			if secretPassword {
				scope.FormOfLoginHandedOnlyToATool(stored)
			}
		}
		// The statement with the stored value goes alone, so that what the
		// client prints when it fails can be searched for the value's parts.
		if failure := server.change(ctx, mysqlPasswordStatement(server.mariaDB, create, account, method, stored)+";\n"); failure != nil {
			doing := "setting the password of " + named
			if create {
				doing = "creating the account " + named
			}
			failure.Text = withoutMySQLStoredValue(failure.Text, stored)
			return afterwards(server.failure(doing, failure), false, nil)
		}
	}
	if len(changes) > 0 {
		var statements strings.Builder
		for _, change := range changes {
			statements.WriteString(change.statement(account) + ";\n")
		}
		if failure := server.change(ctx, statements.String()); failure != nil {
			failed := server.failure("changing the privileges of "+named, failure)
			if failure.is(mysqlPrivilegeRefused...) {
				return afterwards(fmt.Errorf("%w; the server refuses a privilege's name in the task's privileges, or a name at a grant_on target where it does not apply", failed), true, nil)
			}
			return afterwards(failed, false, nil)
		}
	}
	finished := afterwards(nil, false, func(after mysqlAccountState, afterServer mysqlServerState) error {
		if !after.Exists {
			return fmt.Errorf("%s: %s took the statements, and its catalog has no account %s", module, server.where, named)
		}
		if u.Password != nil {
			if same, err := mysqlPasswordIs(after.Method, after.Stored, password); err != nil || !same {
				return fmt.Errorf("%s: %s accepted the statements, but the stored password of %s still does not match the password requested by the node task", module, server.where, named)
			}
		}
		left, err := mysqlPrivilegeChanges(after, afterServer, wanted, u.PrivilegesNotListed)
		if err != nil {
			return fmt.Errorf("%s: the account %s: %w", module, named, err)
		}
		if len(left) > 0 {
			still := make([]string, len(left))
			for i, change := range left {
				still[i] = change.String()
			}
			return fmt.Errorf("%s: %s accepted the statements, but the account %s still differs from the node task's request; the remaining actions are: %s; the server may use different privilege names, so copy each privilege name exactly from SHOW GRANTS", module, server.where, named, strings.Join(still, "; "))
		}
		return nil
	})
	if finished.Status == "succeeded" && finished.Change == "changed" {
		finished.Reason = strings.Join(done, "; ")
	}
	return finished
}

// notKnown is the result for something about the server that MMX cannot
// read: a dryrun cannot check the task, and a run fails before it changes
// anything.
func (run Runtime) notKnown(err error) protocol.Result {
	if run.Dryrun {
		return protocol.Result{Status: "not_checked", Change: "unknown", Reason: err.Error()}
	}
	return unchangedFailure(err)
}

// newMySQLStoredValue computes the stored value of password for the method,
// with a new salt and the server's own number of rounds where the method
// has them.
func newMySQLStoredValue(method, password, roundsSetting string) (string, error) {
	stored := mysqlNativePassword(password)
	if method == mysqlCachingSHA2Method {
		salt, err := newMySQLSalt()
		if err != nil {
			return "", fmt.Errorf("making a salt for the stored password: %w", err)
		}
		stored = mysqlCachingSHA2Password(password, salt, mysqlCachingSHA2Rounds(roundsSetting))
	}
	if !mysqlPlainStoredValue.MatchString(stored) {
		return "", errors.New("internal error: a stored password value holds a character that a statement cannot carry")
	}
	return stored, nil
}

// sameMySQLPrivileges reports whether two readings of an account's
// privileges are equal.
func sameMySQLPrivileges(one, other map[mysqlPlace]*mysqlHeldPrivileges) bool {
	return maps.EqualFunc(one, other, func(a, b *mysqlHeldPrivileges) bool {
		return a.GrantOption == b.GrantOption && a.WithoutGrantOption == b.WithoutGrantOption && maps.Equal(a.Names, b.Names)
	})
}
