package modules

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// MySQLDatabase is one database inside a MySQL or MariaDB server.
// LoginUser and LoginPassword are private values that the module hands only
// to the database's client as a login (resolveMySQLLogin).
type MySQLDatabase struct {
	Name                                   string `yaml:"name" json:"name"`
	State                                  string `yaml:"state,omitempty" json:"state,omitempty"`
	CharacterSet                           string `yaml:"character_set,omitempty" json:"character_set,omitempty"`
	Collation                              string `yaml:"collation,omitempty" json:"collation,omitempty"`
	DatabaseServerHost                     string `yaml:"database_server_host,omitempty" json:"database_server_host,omitempty"`
	DatabaseServerPort                     any    `yaml:"database_server_port,omitempty" json:"database_server_port,omitempty"`
	LoginUser                              string `yaml:"login_user,omitempty" json:"login_user,omitempty"`
	LoginPassword                          string `yaml:"login_password,omitempty" json:"login_password,omitempty"`
	DatabaseServerCertificateAuthorityFile string `yaml:"database_server_certificate_authority_file,omitempty" json:"database_server_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck  bool   `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
}

const (
	mysqlDatabasePresent = "present"
	mysqlDatabaseDeleted = "deleted_with_all_its_data"
)

var (
	mysqlDatabaseStates = []string{mysqlDatabasePresent, mysqlDatabaseDeleted}
	// mysqlServersOwnDatabases are the databases in which the servers keep
	// their accounts and their own state. Without them a server does not
	// work, so no task deletes one.
	mysqlServersOwnDatabases = []string{"mysql", "information_schema", "performance_schema", "sys"}
	// A character set and a collation stand in a statement as they are, so
	// they are names of letters, digits, and underscores.
	mysqlCharacterSetName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// mysqlDatabaseDescription is what mmx describe-module shows for
// mysql_database.
var mysqlDatabaseDescription = Description{
	Name: "mysql_database", Topic: TopicDatabases, Schema: reflect.TypeOf(MySQLDatabase{}),
	Summary: "one database inside a MySQL or MariaDB server that is installed and running; MMX runs the node's own client, mariadb or mysql",
	Fields: append([]Field{
		{Name: "name", Required: true, Notes: "the database's name, exactly as the server keeps it; upper and lower case are different names"},
		{Name: "state", Values: mysqlDatabaseStates, Default: mysqlDatabasePresent, Notes: "deleted_with_all_its_data drops the database with every table in it and accepts only name and the connection fields; a database that does not exist is unchanged; the servers' own databases mysql, information_schema, performance_schema, and sys are refused"},
		{Name: "character_set", Notes: "used when the database is created, such as utf8mb4; a name that the server does not know fails the task before anything changes; an existing database with another character set fails the task and is not changed, because a changed default applies only to tables created later; utf8 and utf8mb3 are one character set"},
		{Name: "collation", Notes: "used when the database is created, such as utf8mb4_unicode_ci; a name that the server does not know, or one that is not a collation of character_set, fails the task before anything changes; an existing database with another collation fails the task and is not changed; MariaDB's short name of a collation, such as uca1400_ai_ci, is the same as the database's utf8mb4_uca1400_ai_ci; a task with only character_set compares only the character set, because the collation that a server picks for it differs between the products and versions"},
	}, mysqlConnectionFieldNotes()...),
	Example: "- mysql_database:\n    name: shop\n    character_set: utf8mb4\n    collation: utf8mb4_unicode_ci\n",
	Results: []string{"status", "change", "reason", "value.created", "value.changed_attributes"},
	Dryrun:  "reads the server's catalog and changes nothing; reports would_change with value.created true for a database that a run would create, and would_change for one that a run would drop; fails, as a run does, for a character set or a collation that the server does not know, for an existing database whose character set or collation differs, and for a refused login; not_checked while neither the program mariadb nor the program mysql is installed, while the server does not answer, and while the file of database_server_certificate_authority_file is missing",
}

// connection is the task's connection fields.
func (d *MySQLDatabase) connection() mysqlConnection {
	return mysqlConnection{Host: d.DatabaseServerHost, Port: d.DatabaseServerPort, LoginUser: d.LoginUser, LoginPassword: d.LoginPassword, CertificateAuthorityFile: d.DatabaseServerCertificateAuthorityFile, SkipServerCertificateCheck: d.DangerouslySkipServerCertificateCheck}
}

// Prepare checks the inputs when the project loads.
func (d *MySQLDatabase) Prepare(base string) error {
	switch {
	case d.Name == "":
		return invalid("name", "mysql_database.name is required: the database's name")
	case d.State != "" && !slices.Contains(mysqlDatabaseStates, d.State):
		return invalid("state", "state must be "+strings.Join(mysqlDatabaseStates, " or "))
	}
	if d.State == mysqlDatabaseDeleted {
		for field, set := range map[string]bool{"character_set": d.CharacterSet != "", "collation": d.Collation != ""} {
			if set {
				return invalid(field, "state: "+mysqlDatabaseDeleted+" accepts only name and the connection fields; remove "+field)
			}
		}
	}
	return d.connection().check()
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (d *MySQLDatabase) Arguments(arguments *ArgumentList) {
	name := mysqlNameRule("name")
	if d.State == mysqlDatabaseDeleted {
		name = func(v any) error {
			if err := mysqlNameRule("name")(v); err != nil {
				return err
			}
			if s, _ := value.Scalar(v); slices.Contains(mysqlServersOwnDatabases, strings.ToLower(s)) {
				return fmt.Errorf("%s is one of the server's own databases, without which the server does not work; MMX does not delete it", s)
			}
			return nil
		}
	}
	arguments.Public("name", &d.Name, name)
	for _, part := range []struct {
		field  string
		target *string
	}{{"character_set", &d.CharacterSet}, {"collation", &d.Collation}} {
		if *part.target != "" {
			arguments.Public(part.field, part.target, rule(part.field+" must be a name of letters, digits, and underscores, such as utf8mb4 or utf8mb4_unicode_ci", mysqlCharacterSetName.MatchString))
		}
	}
	mysqlConnectionArguments(arguments, &d.DatabaseServerHost, &d.DatabaseServerPort, &d.DatabaseServerCertificateAuthorityFile, d.LoginUser, d.LoginPassword)
}

// Run does the module's work on the node.
func (d *MySQLDatabase) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.MySQLDatabase(ctx, *d, task.Scope, task.Packages)
}

// mysqlDatabaseState is what the server's catalog says about one database,
// and about the character set and the collation that the task names.
type mysqlDatabaseState struct {
	Exists                  bool
	CharacterSet, Collation string
	// KnowsCharacterSet: the server has the task's character set.
	KnowsCharacterSet bool
	// CollationOfCharacterSets are the character sets that have the task's
	// collation; none when the server does not know the collation.
	CollationOfCharacterSets []string
}

// mysqlDatabaseQuery reads one database from the server's catalog, and
// whether the server knows the character set and the collation; "" for one
// that the task does not name.
//
// A collation's row holds its character set. MariaDB 10.11 and later list
// their uca1400 collations under a short name, such as uca1400_ai_ci, once
// for each character set that has it, and CREATE DATABASE takes the short
// name and the name with the character set, utf8mb4_uca1400_ai_ci. So a row
// answers to its name and to its name behind its character set.
func mysqlDatabaseQuery(name, characterSet, collation string) string {
	statements := "SELECT 'database', DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE HEX(SCHEMA_NAME) = '" + mysqlHexadecimal(name) + "';\n"
	// The names as HEX() prints them: the catalog writes them in lower case.
	names := func(written string) string {
		var list []string
		for _, name := range bothNamesOfMySQLCharacterSet(written) {
			list = append(list, "'"+mysqlHexadecimal(name)+"'")
		}
		return strings.Join(list, ", ")
	}
	if characterSet != "" {
		statements += "SELECT 'character set', CHARACTER_SET_NAME FROM information_schema.CHARACTER_SETS WHERE HEX(CHARACTER_SET_NAME) IN (" + names(characterSet) + ");\n"
	}
	if collation != "" {
		statements += "SELECT 'collation', COLLATION_NAME, CHARACTER_SET_NAME FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY WHERE HEX(COLLATION_NAME) IN (" + names(collation) + ") OR HEX(CONCAT(CHARACTER_SET_NAME, '_', COLLATION_NAME)) IN (" + names(collation) + ");\n"
	}
	return statements
}

// mysqlDatabaseRows reads what mysqlDatabaseQuery selected.
func mysqlDatabaseRows(rows [][]string) mysqlDatabaseState {
	var state mysqlDatabaseState
	for _, row := range rows {
		switch {
		case len(row) == 3 && row[0] == "database":
			state.Exists, state.CharacterSet, state.Collation = true, row[1], row[2]
		case len(row) == 2 && row[0] == "character set":
			state.KnowsCharacterSet = true
		case len(row) == 3 && row[0] == "collation":
			state.CollationOfCharacterSets = append(state.CollationOfCharacterSets, row[2])
		}
	}
	return state
}

// plainMySQLCharacterSet is the name of a character set or of a collation in
// lower case, with utf8mb3 for utf8, the servers' older name for it: MariaDB
// 10.5 says utf8 and utf8_general_ci where later versions and MySQL say
// utf8mb3 and utf8mb3_general_ci.
func plainMySQLCharacterSet(name string) string {
	name = strings.ToLower(name)
	if name == "utf8" {
		return "utf8mb3"
	}
	if rest, found := strings.CutPrefix(name, "utf8_"); found {
		return "utf8mb3_" + rest
	}
	return name
}

// bothNamesOfMySQLCharacterSet is the name of a character set or of a
// collation as the catalog may write it: in lower case, and for utf8mb3
// under both of its names. Every measured server takes both names in a
// statement and has one of them in its catalog.
func bothNamesOfMySQLCharacterSet(written string) []string {
	plain := plainMySQLCharacterSet(written)
	if rest, found := strings.CutPrefix(plain, "utf8mb3"); found {
		return []string{plain, "utf8" + rest}
	}
	return []string{plain}
}

// sameMySQLCharacterSet compares two names of a character set or of a
// collation.
func sameMySQLCharacterSet(one, other string) bool {
	return plainMySQLCharacterSet(one) == plainMySQLCharacterSet(other)
}

// hasMySQLCollation reports whether a database has the collation that a task
// writes. The catalog names a database's collation with its character set,
// also where the task wrote MariaDB's short name: uca1400_ai_ci is
// utf8mb4_uca1400_ai_ci in a database with the character set utf8mb4.
func hasMySQLCollation(database mysqlDatabaseState, written string) bool {
	return sameMySQLCharacterSet(written, database.Collation) || sameMySQLCharacterSet(database.CharacterSet+"_"+written, database.Collation)
}

// MySQLDatabase creates the database when the server does not have it, and
// drops it for state: deleted_with_all_its_data. It reads the server's
// catalog before and after and reports what differs between the two.
func (run Runtime) MySQLDatabase(ctx context.Context, d MySQLDatabase, scope value.Scope, packages PackageManager) protocol.Result {
	server, cleanup, stop := run.newMySQLServer(ctx, "mysql_database", d.connection(), scope, packages)
	if stop != nil {
		return *stop
	}
	defer cleanup()
	// The statements are the same for both products, so the task's first
	// call already reads the database.
	query := mysqlDatabaseQuery(d.Name, d.CharacterSet, d.Collation)
	rows, stop := server.firstCall(ctx, query)
	if stop != nil {
		return *stop
	}
	before := mysqlDatabaseRows(rows)
	// refused is a task that this server cannot do as written: another try
	// gives the same answer, in a run and in a dryrun.
	refused := func(format string, arguments ...any) protocol.Result {
		return notTriedAgain(fmt.Errorf("mysql_database"+format, arguments...))
	}
	// What the task names must be something that the server has, before
	// anything changes: the server's own errors for these names (1115, 1273,
	// 1253) would come only in a run, and would be tried again.
	switch {
	case d.CharacterSet != "" && !before.KnowsCharacterSet:
		return refused(".character_set: %s has no character set %s; its statement SHOW CHARACTER SET lists the names, such as utf8mb4", server.where, d.CharacterSet)
	case d.Collation != "" && len(before.CollationOfCharacterSets) == 0:
		return refused(".collation: %s has no collation %s; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci", server.where, d.Collation)
	case d.Collation != "" && d.CharacterSet != "" && !slices.ContainsFunc(before.CollationOfCharacterSets, func(set string) bool { return sameMySQLCharacterSet(set, d.CharacterSet) }):
		return refused(".collation: on %s, %s is no collation of the character set %s; its statement SHOW COLLATION WHERE Charset = '%s' lists the collations of that character set", server.where, d.Collation, d.CharacterSet, plainMySQLCharacterSet(d.CharacterSet))
	}
	result := func(change string, created bool, reason string) protocol.Result {
		r := protocol.Success(change)
		r.Value, r.Reason = mysqlResultValue(created, nil), reason
		return r
	}
	deleted := d.State == mysqlDatabaseDeleted
	var statement, reason string
	switch {
	case deleted && !before.Exists:
		return result("unchanged", false, "")
	case deleted:
		statement, reason = "DROP DATABASE "+mysqlName(d.Name), "drop the database "+d.Name+" with all its data"
	case before.Exists:
		// The servers can change the two defaults of an existing database,
		// but the change reaches only tables that are created later: the
		// database would then say one thing and its tables another.
		var differences []string
		if d.CharacterSet != "" && !sameMySQLCharacterSet(d.CharacterSet, before.CharacterSet) {
			differences = append(differences, "the character set "+d.CharacterSet)
		}
		if d.Collation != "" && !hasMySQLCollation(before, d.Collation) {
			differences = append(differences, "the collation "+d.Collation)
		}
		if len(differences) > 0 {
			return refused(": the database %s exists with the character set %s and the collation %s, and the task asks for %s. MMX does not change them on an existing database, because a changed default applies only to tables that are created later. Write what the database has into the task, or convert the database and its tables yourself", d.Name, before.CharacterSet, before.Collation, strings.Join(differences, " and "))
		}
		return result("unchanged", false, "")
	default:
		statement, reason = "CREATE DATABASE "+mysqlName(d.Name), "create the database "+d.Name
		if d.CharacterSet != "" {
			statement += " CHARACTER SET " + d.CharacterSet
		}
		if d.Collation != "" {
			statement += " COLLATE " + d.Collation
		}
	}
	if run.Dryrun {
		return result("would_change", !deleted, "would "+reason)
	}
	if err := run.Mutation("mysql database " + d.Name); err != nil {
		return unchangedFailure(err)
	}
	failure := server.change(ctx, statement+";\n")
	// The catalog says what the server has now, also after a statement that
	// failed.
	rows, readFailure := server.call(ctx, query)
	if readFailure != nil {
		err := server.failure("reading the database "+d.Name, readFailure)
		if failure != nil {
			err = fmt.Errorf("%w; and afterwards: %w", server.failure(statement, failure), err)
		}
		return protocol.Failed(err)
	}
	after := mysqlDatabaseRows(rows)
	change := "unchanged"
	if after.Exists != before.Exists {
		change = "changed"
	}
	created := after.Exists && !before.Exists
	switch {
	case failure != nil:
		r := protocol.Failed(server.failure(statement, failure))
		r.Change, r.Value = change, mysqlResultValue(created, nil)
		return r
	case after.Exists == deleted:
		r := protocol.Failed(fmt.Errorf("mysql_database: %s accepted the statement %s, but the database catalog does not reflect the requested creation or deletion", server.where, statement))
		r.Change = change
		return r
	}
	// A removal says what it removed; a result's value says nothing of it.
	if deleted {
		return result(change, created, "dropped the database "+d.Name+" with all its data")
	}
	return result(change, created, "")
}
