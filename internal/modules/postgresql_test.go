package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A stand-in PostgreSQL server for the tests of postgresql_role and
// postgresql_database.
//
// The stand-ins for psql and pg_isready are this test program itself: a
// fixture puts two links to it on PATH, named psql and pg_isready, and the
// program acts as the tool whose name it was started under (init, below).
// A shell script would do for a tool that prints a line; this one has to
// keep a server's roles, memberships, and databases, and a link to a
// program that already ran starts in a few milliseconds also on macOS,
// which checks every new script file for a third of a second.
//
// The stand-in psql reads the script that the module sends on standard
// input as psql reads it (\set lines, and statements that name the
// variables), keeps the server's state in the file server.json of the
// fixture's directory, and prints what the real queries print: one JSON
// value on a line. It knows exactly the statements that the two modules
// send; the integration suite postgresql_databases.py proves the same
// statements against real servers.
type standInPostgresql struct {
	// Version is the server's major version. From 16 on, a membership has
	// one record for each role that granted it, and the setting
	// scram_iterations exists.
	Version int `json:"version"`
	// ScramIterations is the setting of that name, for version 16 and newer.
	ScramIterations     int                                   `json:"scram_iterations"`
	MaxIdentifierLength int                                   `json:"max_identifier_length"`
	Roles               map[string]*standInPostgresqlRole     `json:"roles"`
	Memberships         []standInPostgresqlMembership         `json:"memberships"`
	Databases           map[string]*standInPostgresqlDatabase `json:"databases"`
	// AvailableExtensions are the extensions whose files the server has.
	AvailableExtensions []string `json:"available_extensions"`

	// PgIsReadyExitCode is what pg_isready answers.
	PgIsReadyExitCode int `json:"pg_isready_exit_code"`
	// LoginPasswords are the passwords of the roles that log in with one.
	// A login with a password file must name the role's password.
	LoginPasswords map[string]string `json:"login_passwords"`
	// RolesThatAreNoSuperuser cannot read stored passwords.
	RolesThatAreNoSuperuser []string `json:"roles_that_are_no_superuser"`
	// DatabasesThatRefuseTheLogin are databases that the server's access
	// rules close to every login.
	DatabasesThatRefuseTheLogin []string `json:"databases_that_refuse_the_login"`
	// RefusesEveryLoginSaying makes every login fail with this message.
	RefusesEveryLoginSaying string `json:"refuses_every_login_saying"`
	// CertificateAuthority is the text of the certificate of the authority
	// that signed the server's certificate, for a client that checks it.
	CertificateAuthority string `json:"certificate_authority"`
	// FailsStatementsStartingWith maps the start of a statement to the
	// error line that the server answers it with. An entry in
	// FailsOnlyOnce is removed when it was used.
	FailsStatementsStartingWith map[string]string `json:"fails_statements_starting_with"`
	FailsOnlyOnce               bool              `json:"fails_only_once"`
	// EndsTheSessionOnStatementsStartingWith is the start of a statement at
	// which the server ends the session, as it does when an administrator
	// terminates it: psql then ends with exit code 2, as without a session.
	EndsTheSessionOnStatementsStartingWith string `json:"ends_the_session_on_statements_starting_with"`
	// EndsTheSessionWhenTheStatementsAreDone: the session ends right after
	// the changing statements of a script, where psql prints its mark, and
	// before the read that follows.
	EndsTheSessionWhenTheStatementsAreDone bool `json:"ends_the_session_when_the_statements_are_done"`
	// HangsOnStatementsStartingWith is the start of a statement that the
	// server never answers, also one of a script that only reads.
	HangsOnStatementsStartingWith string `json:"hangs_on_statements_starting_with"`
	// HangsInPgIsReady: the server never answers pg_isready.
	HangsInPgIsReady bool `json:"hangs_in_pg_isready"`
	// DatabasesWithSessions cannot be dropped.
	DatabasesWithSessions []string `json:"databases_with_sessions"`
	// GrantsThatChangeNothing are roles whose GRANT the server accepts
	// without a change, as it does while a record that only lets the member
	// administer the role stands in the way.
	GrantsThatChangeNothing []string `json:"grants_that_change_nothing"`
}

type standInPostgresqlRole struct {
	CanLogIn            bool   `json:"can_log_in"`
	Superuser           bool   `json:"superuser"`
	CanCreateDatabases  bool   `json:"can_create_databases"`
	CanCreateRoles      bool   `json:"can_create_roles"`
	CanStartReplication bool   `json:"can_start_replication"`
	StoredPassword      string `json:"stored_password"`
}

type standInPostgresqlMembership struct {
	Role      string `json:"role"`
	Member    string `json:"member"`
	GrantedBy string `json:"granted_by"`
	// OnlyAdministers: the record gives the member none of the role's
	// privileges (version 16 and newer).
	OnlyAdministers bool `json:"only_administers"`
}

type standInPostgresqlDatabase struct {
	Owner              string   `json:"owner"`
	Encoding           string   `json:"encoding"`
	Collation          string   `json:"collation"`
	CharacterType      string   `json:"character_type"`
	RefusesConnections bool     `json:"refuses_connections"`
	Extensions         []string `json:"extensions"`
}

// newStandInPostgresql is a server as a distribution installs it: the role
// postgres, the databases postgres, template0, and template1 in the
// distribution's locale, and the common extensions.
func newStandInPostgresql(version int) *standInPostgresql {
	database := func() *standInPostgresqlDatabase {
		return &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	}
	server := &standInPostgresql{Version: version, MaxIdentifierLength: 63,
		Roles:               map[string]*standInPostgresqlRole{"postgres": {CanLogIn: true, Superuser: true, CanCreateDatabases: true, CanCreateRoles: true, CanStartReplication: true}, "pg_monitor": {}},
		Databases:           map[string]*standInPostgresqlDatabase{"postgres": database(), "template0": database(), "template1": database()},
		AvailableExtensions: []string{"plpgsql", "pg_trgm", "hstore", "uuid-ossp", "cube", "earthdistance"}}
	server.Databases["template0"].RefusesConnections = true
	if version >= 14 {
		server.Roles["pg_read_all_data"] = &standInPostgresqlRole{}
	}
	if version >= 16 {
		server.ScramIterations = 4096
	}
	return server
}

// The files of a fixture's directory, beside the stand-ins themselves and
// the file calls with their command lines.
const (
	standInPostgresqlServerFile       = "server.json"
	standInPostgresqlStatementsFile   = "statements"
	standInPostgresqlScriptsFile      = "scripts"
	standInPostgresqlEnvironmentsFile = "environments"
	// The line that the stand-in psql notes before each script it gets.
	standInPostgresqlScriptStart = "-- script\n"
	// The names of every variable in the stand-ins' environments.
	standInPostgresqlEnvironmentNamesFile = "environment-names"
	standInPostgresqlPasswordFileSeen     = "password-file-seen"
	// The process ID of a stand-in that is inside a call that the server
	// never answers, noted when the stand-in starts to wait.
	standInPostgresqlHangsFile = "hangs"
)

// postgresqlFixture puts the stand-in psql and pg_isready on PATH, with
// server as the server they talk to, and makes this account the one that
// runs psql for the local socket. It returns the fixture's directory, where
// the stand-ins keep the server and note their calls.
func postgresqlFixture(t *testing.T, server *standInPostgresql) string {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range standInPostgresqlPrograms {
		if err := os.Symlink(program, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	putProgramsFirst(t, dir)
	writeStandInPostgresql(t, dir, server)
	postgresqlAsThisAccount(t)
	return dir
}

// standInPostgresqlPrograms are the names under which this test program is
// a stand-in.
var standInPostgresqlPrograms = []string{"psql", "pg_isready"}

// When this test program was started under the name of a stand-in, through
// a fixture's link, it is that tool for one call: its directory is the
// fixture's. This runs before the tests' own flags are read, which would
// refuse psql's options.
func init() {
	if name := filepath.Base(os.Args[0]); slices.Contains(standInPostgresqlPrograms, name) {
		exitStandIn(standInPostgresqlProgram(name, filepath.Dir(os.Args[0]), os.Args[1:]))
	}
}

// postgresqlAsThisAccount makes the account of the test the one that runs psql for a
// server on the node, where a real node has the account postgres.
func postgresqlAsThisAccount(t *testing.T) {
	t.Helper()
	found := postgresqlAccountOfTheLocalServer
	t.Cleanup(func() { postgresqlAccountOfTheLocalServer = found })
	home := t.TempDir()
	postgresqlAccountOfTheLocalServer = func(env map[string]string) (*syscall.Credential, error) {
		env["HOME"], env["USER"], env["LOGNAME"] = home, postgresqlAdministratorAccount, postgresqlAdministratorAccount
		return &syscall.Credential{Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid()), NoSetGroups: true}, nil
	}
}

func writeStandInPostgresql(t *testing.T, dir string, server *standInPostgresql) {
	t.Helper()
	text, err := json.MarshalIndent(server, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, standInPostgresqlServerFile), string(text))
}

func readStandInPostgresql(t *testing.T, dir string) *standInPostgresql {
	t.Helper()
	server, err := loadStandInPostgresql(dir)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func loadStandInPostgresql(dir string) (*standInPostgresql, error) {
	text, err := os.ReadFile(filepath.Join(dir, standInPostgresqlServerFile))
	if err != nil {
		return nil, err
	}
	server := &standInPostgresql{}
	return server, json.Unmarshal(text, server)
}

// takePostgresqlStatements returns the statements that changed something, or tried
// to, since the last time, and forgets them. A SCRAM form is shown as
// <SCRAM form>, because its salt is new each time.
func takePostgresqlStatements(t *testing.T, dir string) []string {
	t.Helper()
	path := filepath.Join(dir, standInPostgresqlStatementsFile)
	text := strings.TrimSpace(readState(t, path))
	writeState(t, path, "")
	if text == "" {
		return nil
	}
	return strings.Split(regexp.MustCompile(`SCRAM-SHA-256\$[^']+`).ReplaceAllString(text, "<SCRAM form>"), "\n")
}

// everythingPostgresqlStandInsSaw is the command lines, the environments, and the
// scripts of every call so far: the places where a password must not be.
func everythingPostgresqlStandInsSaw(t *testing.T, dir string) string {
	t.Helper()
	return readState(t, filepath.Join(dir, "calls")) + readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)) + readState(t, filepath.Join(dir, standInPostgresqlScriptsFile))
}

// postgresqlPrivateRuntime is a Runtime whose node tasks make their private
// directories in a new directory, which it returns too.
func postgresqlPrivateRuntime(t *testing.T, run Runtime) (Runtime, string) {
	t.Helper()
	root := t.TempDir()
	return withPrivateTaskDirectoryRoot(run, root), root
}

// postgresqlNoPrivateDirectoryLeft fails when a node task left its private directory behind.
func postgresqlNoPrivateDirectoryLeft(t *testing.T, privateTaskDirectoryRoot string) {
	t.Helper()
	entries, err := os.ReadDir(privateTaskDirectoryRoot)
	if err != nil || len(entries) != 0 {
		t.Errorf("a node task left %d private directories behind: %v", len(entries), err)
	}
}

// standInPostgresqlProgram is one call of the stand-in psql or pg_isready;
// it returns the exit code.
func standInPostgresqlProgram(program, dir string, arguments []string) int {
	note := func(file, text string) {
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			_, _ = io.WriteString(f, text)
			_ = f.Close()
		}
	}
	// The command line, as the shell stand-ins of other tests note theirs.
	note("calls", strings.TrimSpace(program+" "+strings.Join(arguments, " "))+"\n")
	var environment, names []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PG") {
			environment = append(environment, entry)
		}
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	slices.Sort(environment)
	slices.Sort(names)
	note(standInPostgresqlEnvironmentsFile, program+": "+strings.Join(environment, " ")+"\n")
	note(standInPostgresqlEnvironmentNamesFile, program+": "+strings.Join(names, " ")+"\n")
	server, err := loadStandInPostgresql(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the stand-in has no server:", err)
		return 127
	}
	option := func(name string) string {
		for _, argument := range arguments {
			if text, found := strings.CutPrefix(argument, "--"+name+"="); found {
				return text
			}
		}
		return ""
	}
	// hang waits as a program does whose server never answers. A test
	// learns from the note that the program is inside that call now.
	hang := func() {
		note(standInPostgresqlHangsFile, strconv.Itoa(os.Getpid())+"\n")
		time.Sleep(time.Minute)
	}
	if program == "pg_isready" {
		if server.HangsInPgIsReady {
			hang()
		}
		place := "/var/run/postgresql:5432"
		if option("host") != "" {
			place = option("host") + ":5432"
		}
		switch server.PgIsReadyExitCode {
		case 0:
			fmt.Println(place + " - accepting connections")
		case 1:
			fmt.Println(place + " - rejecting connections")
		case 2:
			fmt.Println(place + " - no response")
		default:
			fmt.Println(place + " - no attempt")
		}
		return server.PgIsReadyExitCode
	}
	script, _ := io.ReadAll(os.Stdin)
	note(standInPostgresqlScriptsFile, standInPostgresqlScriptStart+string(script))
	// What psql does before the first statement: the certificate, then the
	// login, then the database.
	refuse := func(message string) int {
		fmt.Fprintln(os.Stderr, `psql: error: connection to server failed: `+message)
		// A real psql prints nothing on standard output here. The stand-in
		// prints a line that must never reach a failure's text.
		fmt.Println("standard output of a failed psql")
		return 2
	}
	if os.Getenv("PGSSLMODE") == "verify-full" {
		authority, err := os.ReadFile(os.Getenv("PGSSLROOTCERT"))
		if err != nil || string(authority) != server.CertificateAuthority {
			return refuse("SSL error: certificate verify failed")
		}
	}
	// The role comes from the environment, as MMX gives it to psql.
	role, host := os.Getenv("PGUSER"), option("host")
	if server.RefusesEveryLoginSaying != "" {
		return refuse(server.RefusesEveryLoginSaying)
	}
	// A password file that does not exist is no password file, and psql
	// says nothing about it.
	file := os.Getenv("PGPASSFILE")
	if line, err := os.ReadFile(file); err == nil {
		seen := map[string]any{"line": string(line)}
		if info, statErr := os.Stat(file); statErr == nil {
			seen["mode"] = info.Mode().Perm().String()
			seen["owner"] = info.Sys().(*syscall.Stat_t).Uid
		}
		if info, statErr := os.Stat(filepath.Dir(file)); statErr == nil {
			seen["directory_mode"] = info.Mode().Perm().String()
			seen["directory_owner"] = info.Sys().(*syscall.Stat_t).Uid
		}
		text, _ := json.Marshal(seen)
		_ = os.WriteFile(filepath.Join(dir, standInPostgresqlPasswordFileSeen), text, 0644)
		if standInPostgresqlPasswordFromFile(string(line), host, option("port"), role) != server.LoginPasswords[role] {
			return refuse(`FATAL:  password authentication failed for user "` + role + `"`)
		}
	} else if host != "" || server.LoginPasswords[role] != "" {
		return refuse("fe_sendauth: no password supplied")
	}
	if _, exists := server.Roles[role]; !exists {
		return refuse(`FATAL:  role "` + role + `" does not exist`)
	}
	database := os.Getenv("PGDATABASE")
	if slices.Contains(server.DatabasesThatRefuseTheLogin, database) {
		return refuse(`FATAL:  no pg_hba.conf entry for user "` + role + `", database "` + database + `"`)
	}
	switch held, exists := server.Databases[database]; {
	case !exists:
		return refuse(`FATAL:  database "` + database + `" does not exist`)
	case held.RefusesConnections:
		return refuse(`FATAL:  database "` + database + `" is not currently accepting connections`)
	}
	session := &standInPostgresqlSession{server: server, dir: dir, role: role, database: database, note: note, hang: hang}
	return session.run(string(script))
}

// standInPostgresqlPasswordFromFile is the password that a password file's line gives
// for a login, "" when the line does not match, by libpq's rules: the host
// is the text of --host, or localhost for the local socket; a field can be
// *; a colon or a backslash in a field has a backslash before it.
func standInPostgresqlPasswordFromFile(line, host, port, role string) string {
	var fields []string
	var field strings.Builder
	text := strings.TrimSuffix(line, "\n")
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\\' && i+1 < len(text):
			i++
			field.WriteByte(text[i])
		case text[i] == ':' && len(fields) < 4:
			fields = append(fields, field.String())
			field.Reset()
		default:
			field.WriteByte(text[i])
		}
	}
	fields = append(fields, field.String())
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5432"
	}
	if len(fields) != 5 || fields[0] != host || (fields[1] != "*" && fields[1] != port) || fields[2] != "*" || fields[3] != role {
		return ""
	}
	return fields[4]
}

// standInPostgresqlSession is one session of the stand-in psql.
type standInPostgresqlSession struct {
	server   *standInPostgresql
	dir      string
	role     string
	database string
	note     func(file, text string)
	// hang waits as a session does whose server never answers.
	hang func()

	variables     map[string]string
	readOnly      bool
	inTransaction bool
	// lockTimeLimit is the session's lock_timeout, "" while it is not set.
	lockTimeLimit string
	// changed: a statement changed the server since the last save.
	changed bool
}

var (
	standInPostgresqlSetLine  = regexp.MustCompile(`^\\set (\w+) '(.*)'$`)
	standInPostgresqlVariable = regexp.MustCompile(`:(["'])(\w+)["']`)
	standInPostgresqlOctal    = regexp.MustCompile(`\\([0-7]{3})`)
)

// run reads a script as psql does with ON_ERROR_STOP: it stops at the first
// statement that fails, with exit code 3.
func (session *standInPostgresqlSession) run(script string) int {
	session.variables = map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(script, "\n"), "\n") {
		if set := standInPostgresqlSetLine.FindStringSubmatch(line); set != nil {
			text := strings.ReplaceAll(set[2], "''", "'")
			session.variables[set[1]] = standInPostgresqlOctal.ReplaceAllStringFunc(text, func(escape string) string {
				code, _ := strconv.ParseUint(escape[1:], 8, 8)
				return string([]byte{byte(code)})
			})
			continue
		}
		if said, isEcho := strings.CutPrefix(line, `\echo '`); isEcho {
			// psql prints the text itself; the server does not see the line.
			fmt.Println(strings.TrimSuffix(said, "'"))
			if session.server.EndsTheSessionWhenTheStatementsAreDone {
				fmt.Fprintln(os.Stderr, "FATAL:  57P01: terminating connection due to administrator command")
				return 2
			}
			continue
		}
		if !strings.HasSuffix(line, ";") {
			fmt.Fprintln(os.Stderr, "the stand-in psql does not know the line: "+line)
			return 1
		}
		// The statement as the server gets it: with each variable's value.
		var values []string
		shown := standInPostgresqlVariable.ReplaceAllStringFunc(strings.TrimSuffix(line, ";"), func(token string) string {
			held := session.variables[standInPostgresqlVariable.FindStringSubmatch(token)[2]]
			values = append(values, held)
			if token[1] == '"' {
				return `"` + held + `"`
			}
			return `'` + held + `'`
		})
		pattern := standInPostgresqlVariable.ReplaceAllString(strings.TrimSuffix(line, ";"), "?")
		if ends := session.server.EndsTheSessionOnStatementsStartingWith; ends != "" && strings.HasPrefix(shown, ends) {
			fmt.Fprintln(os.Stderr, "FATAL:  57P01: terminating connection due to administrator command")
			return 2
		}
		if message := session.statement(pattern, shown, values); message != "" {
			fmt.Fprintln(os.Stderr, message)
			fmt.Fprintln(os.Stderr, "LOCATION:  stand_in, postgresql_test.go:1")
			// The session ends: the server forgets an open transaction.
			return 3
		}
		if session.changed && !session.inTransaction {
			session.save()
		}
	}
	return 0
}

func (session *standInPostgresqlSession) save() {
	text, _ := json.MarshalIndent(session.server, "", "  ")
	_ = os.WriteFile(filepath.Join(session.dir, standInPostgresqlServerFile), text, 0644)
	session.changed = false
}

// statement runs one statement and returns the server's error line, "" for
// none. pattern is the statement with ? for each value, shown the statement
// with the values.
func (session *standInPostgresqlSession) statement(pattern, shown string, values []string) string {
	server := session.server
	words := strings.Fields(pattern)
	if server.HangsOnStatementsStartingWith != "" && strings.HasPrefix(shown, server.HangsOnStatementsStartingWith) {
		session.hang()
	}
	switch {
	case pattern == "begin read only":
		session.readOnly, session.inTransaction = true, true
		return ""
	case pattern == "begin":
		session.inTransaction = true
		return ""
	case pattern == "commit":
		if session.changed {
			session.save()
		}
		session.readOnly, session.inTransaction = false, false
		return ""
	case strings.HasPrefix(pattern, "select "):
		return session.query(pattern, values)
	case strings.HasPrefix(pattern, "set lock_timeout = "):
		session.lockTimeLimit = strings.Trim(strings.TrimPrefix(pattern, "set lock_timeout = "), "'")
		return ""
	}
	session.note(standInPostgresqlStatementsFile, shown+"\n")
	if session.lockTimeLimit != "10s" {
		// A real server would wait without end for another session's lock.
		return "ERROR:  XX000: the stand-in server takes a changing statement only in a session whose lock_timeout is 10s; it is \"" + session.lockTimeLimit + "\""
	}
	if session.readOnly {
		return "ERROR:  25006: cannot execute " + strings.ToUpper(words[0]+" "+words[1]) + " in a read-only transaction"
	}
	for start, message := range server.FailsStatementsStartingWith {
		if strings.HasPrefix(shown, start) {
			if server.FailsOnlyOnce {
				// Remembered at once, also when the transaction is lost.
				kept, _ := loadStandInPostgresql(session.dir)
				delete(kept.FailsStatementsStartingWith, start)
				text, _ := json.MarshalIndent(kept, "", "  ")
				_ = os.WriteFile(filepath.Join(session.dir, standInPostgresqlServerFile), text, 0644)
			}
			// A real server quotes the statement in some errors.
			return strings.ReplaceAll(message, "<the statement>", shown)
		}
	}
	session.changed = true
	attributes := func(role *standInPostgresqlRole, words []string) {
		for _, word := range words {
			on := !strings.HasPrefix(word, "NO")
			switch strings.TrimPrefix(word, "NO") {
			case "LOGIN":
				role.CanLogIn = on
			case "SUPERUSER":
				role.Superuser = on
			case "CREATEDB":
				role.CanCreateDatabases = on
			case "CREATEROLE":
				role.CanCreateRoles = on
			case "REPLICATION":
				role.CanStartReplication = on
			}
		}
	}
	missingRole := func(names ...string) string {
		for _, name := range names {
			if _, exists := server.Roles[name]; !exists {
				return `ERROR:  42704: role "` + name + `" does not exist`
			}
		}
		return ""
	}
	switch {
	case strings.HasPrefix(pattern, "create role ?"):
		if _, exists := server.Roles[values[0]]; exists {
			return `ERROR:  42710: role "` + values[0] + `" already exists`
		}
		server.Roles[values[0]] = &standInPostgresqlRole{}
		attributes(server.Roles[values[0]], words[3:])
	case pattern == "alter role ? password ?":
		if message := missingRole(values[0]); message != "" {
			return message
		}
		server.Roles[values[0]].StoredPassword = values[1]
	case strings.HasPrefix(pattern, "alter role ?"):
		if message := missingRole(values[0]); message != "" {
			return message
		}
		attributes(server.Roles[values[0]], words[3:])
	case pattern == "grant ? to ?":
		if message := missingRole(values[0], values[1]); message != "" {
			return message
		}
		granted := slices.ContainsFunc(server.Memberships, func(m standInPostgresqlMembership) bool {
			return m.Role == values[0] && m.Member == values[1] && (server.Version < 16 || m.GrantedBy == session.role)
		})
		if granted || slices.Contains(server.GrantsThatChangeNothing, values[0]) {
			fmt.Fprintln(os.Stderr, `NOTICE:  00000: role "`+values[1]+`" has already been granted membership in role "`+values[0]+`"`)
			return ""
		}
		server.Memberships = append(server.Memberships, standInPostgresqlMembership{Role: values[0], Member: values[1], GrantedBy: session.role})
	case pattern == "revoke ? from ?" || pattern == "revoke ? from ? granted by ?":
		// Version 16 and newer remove only the record of the named role,
		// or of the session's role; older versions remove the one record.
		grantor := session.role
		if len(values) == 3 {
			grantor = values[2]
		}
		kept := server.Memberships[:0]
		for _, m := range server.Memberships {
			if m.Role == values[0] && m.Member == values[1] && (server.Version < 16 || m.GrantedBy == grantor) {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == len(server.Memberships) {
			fmt.Fprintln(os.Stderr, `WARNING:  01000: role "`+values[1]+`" has not been granted membership in role "`+values[0]+`" by role "`+grantor+`"`)
		}
		server.Memberships = kept
	case pattern == "drop role ?":
		if message := missingRole(values[0]); message != "" {
			return message
		}
		for name, database := range server.Databases {
			if database.Owner == values[0] {
				return `ERROR:  2BP01: role "` + values[0] + `" cannot be dropped because some objects depend on it` + "\nDETAIL:  owner of database " + name
			}
		}
		delete(server.Roles, values[0])
		server.Memberships = slices.DeleteFunc(server.Memberships, func(m standInPostgresqlMembership) bool { return m.Role == values[0] || m.Member == values[0] })
	case strings.HasPrefix(pattern, "create database ?"):
		return session.createDatabase(words[2:], values)
	case pattern == "alter database ? owner to ?":
		if _, exists := server.Databases[values[0]]; !exists {
			return `ERROR:  3D000: database "` + values[0] + `" does not exist`
		}
		if message := missingRole(values[1]); message != "" {
			return message
		}
		server.Databases[values[0]].Owner = values[1]
	case pattern == "drop database ?":
		if _, exists := server.Databases[values[0]]; !exists {
			return `ERROR:  3D000: database "` + values[0] + `" does not exist`
		}
		if session.inTransaction {
			return "ERROR:  25001: DROP DATABASE cannot run inside a transaction block"
		}
		if slices.Contains(server.DatabasesWithSessions, values[0]) {
			return `ERROR:  55006: database "` + values[0] + `" is being accessed by other users` + "\nDETAIL:  There is 1 other session using the database."
		}
		delete(server.Databases, values[0])
	case pattern == "create extension ?":
		database := server.Databases[session.database]
		switch {
		case !slices.Contains(server.AvailableExtensions, values[0]):
			return `ERROR:  0A000: extension "` + values[0] + `" is not available`
		case slices.Contains(database.Extensions, values[0]):
			return `ERROR:  42710: extension "` + values[0] + `" already exists`
		case values[0] == "earthdistance" && !slices.Contains(database.Extensions, "cube"):
			return `ERROR:  42704: required extension "cube" is not installed` + "\nHINT:  Use CREATE EXTENSION ... CASCADE to install required extensions too."
		}
		database.Extensions = append(database.Extensions, values[0])
		slices.Sort(database.Extensions)
	case pattern == "drop extension ?":
		database := server.Databases[session.database]
		switch {
		case !slices.Contains(database.Extensions, values[0]):
			return `ERROR:  42704: extension "` + values[0] + `" does not exist`
		case values[0] == "cube" && slices.Contains(database.Extensions, "earthdistance"):
			return "ERROR:  2BP01: cannot drop extension cube because other objects depend on it\nDETAIL:  extension earthdistance depends on extension cube"
		}
		database.Extensions = slices.DeleteFunc(database.Extensions, func(name string) bool { return name == values[0] })
	default:
		return "ERROR:  42601: the stand-in server does not know the statement: " + pattern
	}
	return ""
}

// createDatabase is CREATE DATABASE with the options in the order in which
// the module writes them.
func (session *standInPostgresqlSession) createDatabase(options, values []string) string {
	server := session.server
	name, values := values[0], values[1:]
	created := &standInPostgresqlDatabase{Owner: session.role}
	template := postgresqlTemplateOfNewDatabases
	given := map[string]string{}
	for i := 1; i+1 < len(options); i += 2 {
		given[options[i]], values = values[0], values[1:]
	}
	if given["template"] != "" {
		template = given["template"]
	}
	source, exists := server.Databases[template]
	switch {
	case session.inTransaction:
		return "ERROR:  25001: CREATE DATABASE cannot run inside a transaction block"
	case server.Databases[name] != nil:
		return `ERROR:  42P04: database "` + name + `" already exists`
	case !exists:
		return `ERROR:  3D000: template database "` + template + `" does not exist`
	}
	*created = *source
	created.Owner, created.RefusesConnections, created.Extensions = session.role, false, slices.Clone(source.Extensions)
	if owner := given["owner"]; owner != "" {
		if _, exists := server.Roles[owner]; !exists {
			return `ERROR:  42704: role "` + owner + `" does not exist`
		}
		created.Owner = owner
	}
	if encoding := given["encoding"]; encoding != "" {
		asTheServerNamesIt := standInPostgresqlEncodingName(encoding)
		if template != "template0" && asTheServerNamesIt != source.Encoding {
			return "ERROR:  22023: new encoding (" + asTheServerNamesIt + ") is incompatible with the encoding of the template database (" + source.Encoding + ")\nHINT:  Use the same encoding as in the template database, or use template0 as template."
		}
		created.Encoding = asTheServerNamesIt
	}
	if locale := given["locale"]; locale != "" {
		// The server compares the names as text, unless the template is
		// template0.
		if template != "template0" && locale != source.Collation {
			return "ERROR:  22023: new collation (" + locale + ") is incompatible with the collation of the template database (" + source.Collation + ")\nHINT:  Use the same collation as in the template database, or use template0 as template."
		}
		if locale == "POSIX" {
			locale = "C"
		}
		created.Collation, created.CharacterType = locale, locale
	}
	server.Databases[name] = created
	return ""
}

// standInPostgresqlEncodingName is an encoding's name as the server spells it, "" for
// one it does not know.
func standInPostgresqlEncodingName(written string) string {
	switch strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(written)) {
	case "utf8", "unicode":
		return "UTF8"
	case "latin1", "iso88591":
		return "LATIN1"
	case "sqlascii":
		return "SQL_ASCII"
	}
	return ""
}

// query answers one of the modules' queries with one JSON value on a line.
func (session *standInPostgresqlSession) query(pattern string, values []string) string {
	server := session.server
	database := func(name string) any {
		held, exists := server.Databases[name]
		if !exists {
			return nil
		}
		return map[string]any{"owner": held.Owner, "encoding": held.Encoding, "collation": held.Collation, "character_type": held.CharacterType, "accepts_connections": !held.RefusesConnections}
	}
	var answer map[string]any
	switch {
	case strings.Contains(pattern, "'stored_password'"):
		if slices.Contains(server.RolesThatAreNoSuperuser, session.role) {
			return "ERROR:  42501: permission denied for table pg_authid"
		}
		var stored any
		if role, exists := server.Roles[values[0]]; exists && role.StoredPassword != "" {
			stored = role.StoredPassword
		}
		answer = map[string]any{"stored_password": stored}
	case strings.Contains(pattern, "'installed_extensions'"):
		answer = map[string]any{"installed_extensions": server.Databases[session.database].Extensions}
	case strings.Contains(pattern, "'memberships'"):
		name := values[0]
		var wanted, existing []string
		_ = json.Unmarshal([]byte(values[2]), &wanted)
		for _, role := range wanted {
			if _, exists := server.Roles[role]; exists {
				existing = append(existing, role)
			}
		}
		memberships := []any{}
		// In the order of the real query: by role, then by grantor.
		records := slices.Clone(server.Memberships)
		slices.SortStableFunc(records, func(a, b standInPostgresqlMembership) int {
			return strings.Compare(a.Role+"\x00"+a.GrantedBy, b.Role+"\x00"+b.GrantedBy)
		})
		for _, m := range records {
			if m.Member != name {
				continue
			}
			record := map[string]any{"role": m.Role, "granted_by": nil, "inherit_option": nil, "set_option": nil}
			if m.GrantedBy != "" {
				record["granted_by"] = m.GrantedBy
			}
			if server.Version >= 16 {
				record["inherit_option"], record["set_option"] = !m.OnlyAdministers, !m.OnlyAdministers
			}
			memberships = append(memberships, record)
		}
		var role any
		if held, exists := server.Roles[name]; exists {
			role = map[string]any{"can_log_in": held.CanLogIn, "superuser": held.Superuser, "can_create_databases": held.CanCreateDatabases,
				"can_create_roles": held.CanCreateRoles, "can_start_replication": held.CanStartReplication}
		}
		var iterations any
		if server.Version >= 16 {
			iterations = server.ScramIterations
		}
		answer = map[string]any{"max_identifier_length": server.MaxIdentifierLength, "scram_iterations": iterations,
			"may_read_stored_passwords": !slices.Contains(server.RolesThatAreNoSuperuser, session.role),
			"role":                      role, "memberships": memberships, "existing_roles": append([]string{}, existing...)}
	case strings.Contains(pattern, "'database'"):
		var wanted, available []string
		_ = json.Unmarshal([]byte(values[4]), &wanted)
		for _, extension := range wanted {
			if slices.Contains(server.AvailableExtensions, extension) {
				available = append(available, extension)
			}
		}
		_, ownerExists := server.Roles[values[2]]
		answer = map[string]any{"max_identifier_length": server.MaxIdentifierLength, "database": database(values[0]), "template": database(values[1]),
			"owner_exists": ownerExists, "encoding_as_the_server_names_it": standInPostgresqlEncodingName(values[3]), "available_extensions": append([]string{}, available...)}
	default:
		return "ERROR:  42601: the stand-in server does not know the query: " + pattern
	}
	text, _ := json.Marshal(answer)
	fmt.Println(string(text))
	return ""
}

// postgresqlTestAccount is the user ID of the test, which owns what the stand-ins see.
func postgresqlTestAccount(t *testing.T) uint32 {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(current.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(uid)
}

// A value of a task reaches psql as a variable that a \set line sets. psql
// reads ” as one quote and a backslash as the start of an escape, and a
// line break ends the line, so each of them is written in a form that
// cannot end the value.
func TestPostgresqlScriptWritesEveryValueAsAVariable(t *testing.T) {
	t.Parallel()
	script := &postgresqlScript{}
	script.statement("create role " + script.identifier("we\"ird O'Brien \\ Ro\tle\nx; drop role postgres") + "   LOGIN")
	script.statement("alter role " + script.identifier("ünï") + " password " + script.literal("SCRAM-SHA-256$4096:c2FsdA==$a:b"))
	script.statement("select " + script.list([]string{"a\"b", "c'd"}))
	want := `\set mmx_value_1 'we"ird O''Brien \134 Ro\011le\012x; drop role postgres'
create role :"mmx_value_1" LOGIN;
\set mmx_value_2 'ünï'
\set mmx_value_3 'SCRAM-SHA-256$4096:c2FsdA==$a:b'
alter role :"mmx_value_2" password :'mmx_value_3';
\set mmx_value_4 '["a\134"b","c''d"]'
select :'mmx_value_4'::pg_catalog.jsonb;
`
	if got := script.text(); got != want {
		t.Errorf("the script is\n%s\nwant\n%s", got, want)
	}
	// Every line of the script is a \set line or one statement: no value
	// can add a line of its own.
	for _, line := range strings.Split(strings.TrimSuffix(script.text(), "\n"), "\n") {
		if !strings.HasPrefix(line, `\set mmx_value_`) && !strings.HasSuffix(line, ";") {
			t.Errorf("a value broke the line %q", line)
		}
	}
}

// The one line of the password file: the host is the text of --host, or
// localhost for the local socket, and a colon and a backslash in a field
// have a backslash before them.
func TestPostgresqlPasswordFileLine(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		line       string
		connection postgresqlConnection
		role       string
	}{
		{`localhost:*:*:postgres:pa\:ss\\wo\:rd`, postgresqlConnection{}, "postgres"},
		{`localhost:5433:*:ad\:min:pa\:ss\\wo\:rd`, postgresqlConnection{port: "5433"}, "ad:min"},
		{`db.example.com:*:*:postgres:pa\:ss\\wo\:rd`, postgresqlConnection{host: "db.example.com"}, "postgres"},
		{`2001\:db8\:\:5:6432:*:deploy:pa\:ss\\wo\:rd`, postgresqlConnection{host: "2001:db8::5", port: "6432"}, "deploy"},
		{`db.example.com:5432:*:back\\slash:pa\:ss\\wo\:rd`, postgresqlConnection{host: "db.example.com", port: "5432"}, `back\slash`},
	} {
		c := test.connection
		if got := c.passwordFileLine(test.role, `pa:ss\wo:rd`); got != test.line+"\n" {
			t.Errorf("%+v: %q, want %q", c, got, test.line)
		}
		// libpq reads the line back to the password.
		if read := standInPostgresqlPasswordFromFile(c.passwordFileLine(test.role, `pa:ss\wo:rd`), c.host, c.port, test.role); read != `pa:ss\wo:rd` {
			t.Errorf("%+v: the line gives the password %q", c, read)
		}
	}
}

func TestPostgresqlConnectionRules(t *testing.T) {
	t.Parallel()
	for host, accepted := range map[string]bool{"db.example.com": true, "10.0.0.5": true, "2001:db8::5": true, "db_1.internal-net": true,
		"": false, "/var/run/postgresql": false, "two words": false, "db.example.com/shop": false, "user@db.example.com": false} {
		err := postgresqlHostRule(host)
		if (err == nil) != accepted || (err != nil && err.Error() != "database_server_host must be a host name or an address, such as db.example.com or 10.0.0.5") {
			t.Errorf("database_server_host %q: %v", host, err)
		}
	}
	for _, port := range []any{5433, int64(1), 65535, json.Number("6432")} {
		if err := postgresqlPortRule(port); err != nil {
			t.Errorf("database_server_port %v: %v", port, err)
		}
	}
	for _, port := range []any{0, 65536, -1, "5433", 54.5, true} {
		if err := postgresqlPortRule(port); err == nil || err.Error() != "database_server_port must be a number from 1 to 65535" {
			t.Errorf("database_server_port %v: %v", port, err)
		}
	}
	// The messages of the rules for the login never repeat the value, which
	// can be a secret.
	for _, password := range []string{"", "line\nbreak", "carriage\rreturn"} {
		if err := postgresqlLoginPasswordRule(password); err == nil || (password != "" && strings.Contains(err.Error(), password)) {
			t.Errorf("login_password %q: %v", password, err)
		}
		if err := postgresqlLoginRoleRule(password); err == nil || err.Error() != "login_role must not be empty and must not hold a line break or a zero byte" {
			t.Errorf("login_role %q: %v", password, err)
		}
	}
	if err := postgresqlLoginRoleRule(`we"ird: O'Brien \ role`); err != nil {
		t.Error(err)
	}
	if err := postgresqlPasswordRule(""); err == nil || !strings.Contains(err.Error(), "leave password out") {
		t.Error(err)
	}
	if err := postgresqlPasswordRule("line\nbreak"); err != nil {
		t.Error("a role's password with a line break was refused", err)
	}
	for name, accepted := range map[string]bool{"shop": true, "we\"ird O'Brien": true, "": false, "nul\x00l": false} {
		if err := postgresqlNameRule(name); (err == nil) != accepted {
			t.Errorf("name %q: %v", name, err)
		}
	}
}

// The connection fields of both modules are checked alike when the project
// loads, with the words that the MySQL modules have for the same rules.
func TestPostgresqlConnectionFieldsRejectedInputs(t *testing.T) {
	t.Parallel()
	for message, role := range map[string]PostgresqlRole{
		"login_password: database_server_host needs login_password: a login over the network needs the password of the role in login_role":                                                              {Name: "shop", DatabaseServerHost: "db.example.com"},
		"database_server_certificate_authority_file: database_server_certificate_authority_file needs database_server_host; a connection through the server's local socket has no certificate to check": {Name: "shop", DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.crt", LoginPassword: "x"},
	} {
		role.CanLogIn = postgresqlBool(false)
		if err := role.Prepare(""); err == nil || err.Error() != message {
			t.Errorf("postgresql_role %+v: %v; want %s", role, err, message)
		}
		database := PostgresqlDatabase{Owner: "postgres", Name: role.Name, DatabaseServerHost: role.DatabaseServerHost, DatabaseServerPort: role.DatabaseServerPort,
			DatabaseServerCertificateAuthorityFile: role.DatabaseServerCertificateAuthorityFile, LoginPassword: role.LoginPassword}
		database.Owner = "postgres"
		if err := database.Prepare(""); err == nil || err.Error() != message {
			t.Errorf("postgresql_database %+v: %v; want %s", database, err, message)
		}
	}
	for _, role := range []PostgresqlRole{
		{Name: "shop", DatabaseServerHost: "db.example.com", DatabaseServerPort: 5433, DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.crt", LoginRole: "{{ .secrets.admin_role }}", LoginPassword: "{{ .secrets.admin }}"},
		// A port selects among several servers on one node too.
		{Name: "shop", DatabaseServerPort: "{{ .vars.port }}", LoginRole: "admin", LoginPassword: "x"},
		{Name: "shop", DatabaseServerPort: 5433},
	} {
		role.CanLogIn = postgresqlBool(false)
		if err := role.Prepare(""); err != nil {
			t.Errorf("%+v: %v", role, err)
		}
	}
}

// A missing psql or pg_isready is something an earlier node task may still
// install: a run fails without another try and names the program and its
// package, and a dryrun is not checked.
func TestPostgresqlWithoutTheClientPrograms(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	role := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}
	for _, manager := range []string{"apt", "unknown"} {
		setWorkerEnvironment(t, "PATH", t.TempDir())
		reason := "postgresql_role: " + missingProgramError("psql").Error() + ". It comes with PostgreSQL's server package; for a server on another machine, install the client alone"
		if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, NewPackageManager(manager)); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
			t.Errorf("%s run: %+v", manager, r)
		}
		if r := noMutation(t).PostgresqlRole(ctx, role, value.Scope{}, NewPackageManager(manager)); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("%s dryrun: %+v", manager, r)
		}
	}
	// psql alone is not enough: the task asks pg_isready first.
	dir := standInPrograms(t, map[string]string{"psql": "exit 0"})
	setWorkerEnvironment(t, "PATH", dir)
	r := (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "postgres", Name: "shop"}, value.Scope{}, nil)
	if r.FailureKind != "missing_prerequisite" || !strings.HasPrefix(r.Reason, "postgresql_database: "+missingProgramError("pg_isready").Error()) {
		t.Errorf("%+v", r)
	}
	if calls := takeCalls(t, dir); calls != nil {
		t.Error("a program ran before both were found", calls)
	}
}

// On Debian and Ubuntu, psql on the PATH can be only the wrapper of the
// package postgresql-client-common, without a client of any version. That
// is a missing program too.
func TestPostgresqlWrapperOfTheDebianFamilyWithoutAClient(t *testing.T) {
	dir := t.TempDir()
	writeState(t, filepath.Join(dir, "share/pg_wrapper"), "#!/bin/sh\necho 'Error: You must install at least one postgresql-client-<version> package' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(dir, "share/pg_wrapper"), 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, program := range []string{"psql", "pg_isready"} {
		if err := os.Symlink("../share/pg_wrapper", filepath.Join(bin, program)); err != nil {
			t.Fatal(err)
		}
	}
	setWorkerEnvironment(t, "PATH", bin)
	found := postgresqlProgramsOfTheDebianFamily
	t.Cleanup(func() { postgresqlProgramsOfTheDebianFamily = found })
	postgresqlProgramsOfTheDebianFamily = filepath.Join(dir, "lib/*/bin")
	if postgresqlProgramInstalled(testContext(t), "psql") {
		t.Fatal("the wrapper alone counts as psql")
	}
	r := (Runtime{}).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, NewPackageManager("apt"))
	if r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, missingProgramError("psql").Error()) {
		t.Errorf("%+v", r)
	}
	// With the client of one version, the wrapper works.
	for _, program := range []string{"psql", "pg_isready"} {
		writeState(t, filepath.Join(dir, "lib/17/bin", program), "")
	}
	if !postgresqlProgramInstalled(testContext(t), "psql") || !postgresqlProgramInstalled(testContext(t), "pg_isready") {
		t.Error("the wrapper with a client of version 17 does not count")
	}
}

// A node that has the client and not the server has no account postgres.
func TestPostgresqlWithoutTheAdministratorAccount(t *testing.T) {
	dir := postgresqlFixture(t, newStandInPostgresql(17))
	postgresqlAccountOfTheLocalServer = func(map[string]string) (*syscall.Credential, error) {
		return nil, user.UnknownUserError(postgresqlAdministratorAccount)
	}
	reason := "postgresql_role: the operating system account postgres, which runs psql for a server on this node, cannot be used: user: unknown user postgres. PostgreSQL's server package creates the account; for a server on another machine, set database_server_host and login_password"
	if r := (Runtime{}).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Errorf("run: %+v", r)
	}
	if r := noMutation(t).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Errorf("dryrun: %+v", r)
	}
	if calls := takeCalls(t, dir); calls != nil {
		t.Error("a program ran without the account", calls)
	}
}

// pg_isready says with its exit code whether a server answers. No answer
// (2) and a server that refuses connections for now (1) fail a run, which
// may try again, and leave a dryrun not checked; any other code is a
// failure in both.
func TestPostgresqlServerThatDoesNotAnswer(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	dir := postgresqlFixture(t, server)
	for code, reason := range map[int]string{
		1: "postgresql_database: the PostgreSQL server at db.example.com, port 5433 is temporarily refusing connections while starting up or shutting down",
		2: "postgresql_database: no PostgreSQL server answers at db.example.com, port 5433; check the address and the port, and that the server accepts connections from this node",
	} {
		server.PgIsReadyExitCode = code
		writeStandInPostgresql(t, dir, server)
		database := PostgresqlDatabase{Owner: "postgres", Name: "shop", DatabaseServerHost: "db.example.com", DatabaseServerPort: 5433, LoginPassword: "admin password"}
		run, private := postgresqlPrivateRuntime(t, Runtime{})
		if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "" || r.Reason != reason {
			t.Errorf("exit code %d, run: %+v", code, r)
		}
		postgresqlNoPrivateDirectoryLeft(t, private)
		dry, private := postgresqlPrivateRuntime(t, noMutation(t))
		if r := dry.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("exit code %d, dryrun: %+v", code, r)
		}
		postgresqlNoPrivateDirectoryLeft(t, private)
		// Only pg_isready ran, with the task's host and port and the time
		// limit, and psql never.
		if calls := takeCalls(t, dir); !slices.Equal(calls, []string{"pg_isready --host=db.example.com --port=5433 --timeout=10", "pg_isready --host=db.example.com --port=5433 --timeout=10"}) {
			t.Errorf("exit code %d: %q", code, calls)
		}
	}
	// The local socket: the server is the node's own to install and start.
	server.PgIsReadyExitCode = 2
	writeStandInPostgresql(t, dir, server)
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Reason != "postgresql_role: no PostgreSQL server answers on this node's local socket; install and start the server in an earlier node task" {
		t.Errorf("%+v", r)
	}
	server.PgIsReadyExitCode = 3
	writeStandInPostgresql(t, dir, server)
	reason := "postgresql_role: pg_isready could not ask the PostgreSQL server on this node's local socket (exit code 3): /var/run/postgresql:5432 - no attempt"
	if r := (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.Reason != reason {
		t.Errorf("run: %+v", r)
	}
	if r := noMutation(t).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.Reason != reason {
		t.Errorf("dryrun: %+v", r)
	}
}

// After pg_isready found a server, a psql that gets no session fails the
// task in a run and in a dryrun, with the server's message. Another try
// cannot fix a login that the server refuses, and each try would leave a
// failed login in the server's log: the failure's kind is configuration,
// which the node task execution engine does not try again. The other side
// is TestPostgresqlServerThatDoesNotAnswer: a server that does not answer
// is a failure without a kind, which is tried again. The message holds what
// psql wrote to standard error and never what it wrote to standard output.
func TestPostgresqlRefusedLogin(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(13)
	server.RefusesEveryLoginSaying = `FATAL:  Peer authentication failed for user "deploy"`
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", LoginRole: "deploy", DatabaseServerPort: 5433}
	// The reason ends with what to check: the two login fields, and the
	// server's own rules for logins.
	reason := `postgresql_role: could not read the role shop: psql got no session with the PostgreSQL server on this node's local socket, port 5433 as the role deploy: psql: error: connection to server failed: FATAL:  Peer authentication failed for user "deploy"; the server must accept the operating system account postgres as this role through its local socket, which a line of its pg_hba.conf decides; or give the task login_role and login_password`
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		// The task read nothing and changed nothing.
		if r := run.PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != reason {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// The local socket: psql runs with the port, never asks for a password,
	// and reads the SQL from its standard input. The role is in its
	// environment, not on its command line.
	want := "psql --no-psqlrc --no-password --quiet --no-align --tuples-only --set ON_ERROR_STOP=1 --set VERBOSITY=verbose --port=5433"
	if calls := takeCalls(t, dir); !slices.Equal(calls, []string{"pg_isready --port=5433 --timeout=10", want, "pg_isready --port=5433 --timeout=10", want}) {
		t.Errorf("%q", calls)
	}
	if environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)); !strings.Contains(environments, " PGUSER=deploy\n") {
		t.Error(environments)
	}
}

// With login_password, the password reaches psql only through a password
// file in the task's private directory, which belongs to the account that
// runs psql and is gone when the task ends. It is on no command line, in no
// environment, and not in the script. A secret in it does not hide the
// result, and a failure that tells it shows [redacted].
func TestPostgresqlLoginPasswordGoesThroughAPrivatePasswordFile(t *testing.T) {
	ctx := testContext(t)
	const password = `sentinel login:pass\word 4711`
	server := newStandInPostgresql(17)
	server.Roles["deploy"] = &standInPostgresqlRole{CanLogIn: true, Superuser: true}
	server.LoginPasswords = map[string]string{"deploy": password}
	server.CertificateAuthority = "the certificate of the server's authority\n"
	dir := postgresqlFixture(t, server)
	authority := filepath.Join(t.TempDir(), "authority.crt")
	writeState(t, authority, server.CertificateAuthority)

	yes := true
	role := PostgresqlRole{Name: "shop", CanLogIn: &yes, DatabaseServerHost: "db.example.com", DatabaseServerPort: 6432, DatabaseServerCertificateAuthorityFile: authority,
		LoginRole: "deploy", LoginPassword: "{{ .secrets.deploy_password }}"}
	// The dryrun logs in too: it reads the catalog as the run does.
	for _, try := range []struct {
		name string
		run  Runtime
	}{{"dryrun", noMutation(t)}, {"run", Runtime{}}} {
		name := try.name
		run, private := postgresqlPrivateRuntime(t, try.run)
		s, trace := taskScope(map[string]string{"deploy_password": password})
		r := reportedByTheEngine(run.PostgresqlRole(ctx, role, s, nil), s)
		if r.Status != "succeeded" || trace.Used || r.Value.(map[string]any)["created"] != true {
			t.Fatalf("%s: %+v (hidden: %v)", name, r, trace.Used)
		}
		postgresqlNoPrivateDirectoryLeft(t, private)
		var seen struct {
			Line           string `json:"line"`
			Mode           string `json:"mode"`
			Owner          uint32 `json:"owner"`
			DirectoryMode  string `json:"directory_mode"`
			DirectoryOwner uint32 `json:"directory_owner"`
		}
		if err := json.Unmarshal([]byte(readState(t, filepath.Join(dir, standInPostgresqlPasswordFileSeen))), &seen); err != nil {
			t.Fatal(name, err)
		}
		if seen.Line != `db.example.com:6432:*:deploy:sentinel login\:pass\\word 4711`+"\n" || seen.Mode != "-rw-------" || seen.DirectoryMode != "-rwx------" ||
			seen.Owner != postgresqlTestAccount(t) || seen.DirectoryOwner != postgresqlTestAccount(t) {
			t.Errorf("%s: the password file was %+v", name, seen)
		}
		saw := everythingPostgresqlStandInsSaw(t, dir)
		if strings.Contains(saw, password) || strings.Contains(saw, `pass\\word`) {
			t.Errorf("%s: the login password is on a command line, in an environment, or in a script:\n%s", name, saw)
		}
		// For a server on another machine psql runs as root, with the host
		// and the port on its command line and the role in its
		// environment, and checks the server's certificate against the
		// authority file and the host name. pg_isready gets neither the
		// login nor the certificate settings.
		environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile))
		if !strings.Contains(environments, "psql: PGCLIENTENCODING=UTF8 PGCONNECT_TIMEOUT=10 PGDATABASE=postgres PGPASSFILE="+private+"/postgresql_role-") ||
			!strings.Contains(environments, " PGSSLMODE=verify-full PGSSLROOTCERT="+authority+" PGUSER=deploy\n") || !strings.Contains(environments, "pg_isready: PGCONNECT_TIMEOUT=10\n") {
			t.Errorf("%s: the environments were\n%s", name, environments)
		}
		if calls := takeCalls(t, dir); calls[0] != "pg_isready --host=db.example.com --port=6432 --timeout=10" ||
			calls[1] != "psql --no-psqlrc --no-password --quiet --no-align --tuples-only --set ON_ERROR_STOP=1 --set VERBOSITY=verbose --host=db.example.com --port=6432" {
			t.Errorf("%s: %q", name, calls)
		}
		writeState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile), "")
	}
	if !readStandInPostgresql(t, dir).Roles["shop"].CanLogIn {
		t.Error("the run did not create the role")
	}

	// A wrong password: the server's message, in a run and in a dryrun.
	for name, run := range map[string]Runtime{"dryrun": noMutation(t), "run": {}} {
		run, private := postgresqlPrivateRuntime(t, run)
		s, _ := taskScope(map[string]string{"deploy_password": "another password"})
		r := reportedByTheEngine(run.PostgresqlRole(ctx, role, s, nil), s)
		if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != `postgresql_role: could not read the role shop: psql got no session with the PostgreSQL server at db.example.com, port 6432 as the role deploy: psql: error: connection to server failed: FATAL:  password authentication failed for user "deploy"; check login_role and login_password, and that the server's pg_hba.conf has a line that lets this role log in from this node with a password. For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity.` {
			t.Errorf("%s: %+v", name, r)
		}
		postgresqlNoPrivateDirectoryLeft(t, private)
	}

	// A server that tells the password in its refusal: the result keeps the
	// message and shows [redacted] for the secret.
	server = readStandInPostgresql(t, dir)
	server.RefusesEveryLoginSaying = "FATAL:  the login deploy with " + password + " is refused"
	writeStandInPostgresql(t, dir, server)
	run, _ := postgresqlPrivateRuntime(t, Runtime{})
	s, trace := taskScope(map[string]string{"deploy_password": password})
	r := reportedByTheEngine(run.PostgresqlRole(ctx, role, s, nil), s)
	if trace.Used || !strings.Contains(r.Reason, "FATAL:  the login deploy with [redacted] is refused; check login_role and login_password") || strings.Contains(r.Reason, "4711") {
		t.Errorf("%+v (hidden: %v)", r, trace.Used)
	}
	// The scope remembers the password as it is and as the password file
	// holds it, and nothing of a password that is no secret.
	if remembered := s.LoginsHandedOnlyToATool(); !slices.Equal(remembered, []string{password, `sentinel login\:pass\\word 4711`}) {
		t.Errorf("the task's scope remembers %q", remembered)
	}
	written := role
	written.LoginPassword = password
	s, _ = taskScope(nil)
	if run.PostgresqlRole(ctx, written, s, nil); len(s.LoginsHandedOnlyToATool()) != 0 {
		t.Errorf("a written password is remembered: %q", s.LoginsHandedOnlyToATool())
	}
}

// With login_password and no database_server_host, the task logs in with
// the password through the local socket: psql runs as the account that
// runs it for the server, and the password file's host is localhost.
func TestPostgresqlLoginPasswordThroughTheLocalSocket(t *testing.T) {
	server := newStandInPostgresql(17)
	server.Roles["deploy"] = &standInPostgresqlRole{CanLogIn: true, Superuser: true}
	server.LoginPasswords = map[string]string{"deploy": "socket password"}
	dir := postgresqlFixture(t, server)
	run, private := postgresqlPrivateRuntime(t, Runtime{})
	r := run.PostgresqlDatabase(testContext(t), PostgresqlDatabase{Owner: "deploy", Name: "shop", LoginRole: "deploy", LoginPassword: "socket password"}, value.Scope{}, nil)
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	postgresqlNoPrivateDirectoryLeft(t, private)
	if seen := readState(t, filepath.Join(dir, standInPostgresqlPasswordFileSeen)); !strings.Contains(seen, `"line":"localhost:*:*:deploy:socket password\n"`) {
		t.Error(seen)
	}
	if environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)); !strings.Contains(environments, "PGPASSFILE="+private) || strings.Contains(environments, "PGSSL") {
		t.Error(environments)
	}
	if readStandInPostgresql(t, dir).Databases["shop"].Owner != "deploy" {
		t.Error("the database is not the login role's")
	}
}

// With database_server_certificate_authority_file, psql accepts only a
// server whose certificate that authority signed. Another authority's file
// fails the task with the client's message, in a run and in a dryrun,
// although pg_isready finds the server. A file that is not on the node yet
// is something an earlier node task may still install.
func TestPostgresqlCertificateAuthorityFile(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.LoginPasswords = map[string]string{"postgres": "admin password"}
	server.CertificateAuthority = "the certificate of the server's authority\n"
	dir := postgresqlFixture(t, server)
	other := filepath.Join(t.TempDir(), "other.crt")
	writeState(t, other, "the certificate of another authority\n")
	database := PostgresqlDatabase{Owner: "postgres", Name: "shop", DatabaseServerHost: "db.example.com", LoginPassword: "admin password", DatabaseServerCertificateAuthorityFile: other}
	reason := "postgresql_database: could not read the database shop: psql got no session with the PostgreSQL server at db.example.com as the role postgres: psql: error: connection to server failed: SSL error: certificate verify failed; check login_role and login_password, and that the server's pg_hba.conf has a line that lets this role log in from this node with a password. For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity."
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		run, private := postgresqlPrivateRuntime(t, run)
		// A certificate that psql does not accept is not tried again either.
		if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || r.Reason != reason {
			t.Errorf("%s: %+v", name, r)
		}
		postgresqlNoPrivateDirectoryLeft(t, private)
	}
	if readStandInPostgresql(t, dir).Databases["shop"] != nil {
		t.Error("a task created the database on a server that it did not accept")
	}
	takeCalls(t, dir)

	database.DatabaseServerCertificateAuthorityFile = filepath.Join(t.TempDir(), "not-installed.crt")
	reason = "postgresql_database: database_server_certificate_authority_file " + database.DatabaseServerCertificateAuthorityFile + " does not exist on the node; install the certificate of the authority in an earlier node task"
	run, private := postgresqlPrivateRuntime(t, Runtime{})
	if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Errorf("run: %+v", r)
	}
	dry, _ := postgresqlPrivateRuntime(t, noMutation(t))
	if r := dry.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "not_checked" || r.Reason != reason {
		t.Errorf("dryrun: %+v", r)
	}
	postgresqlNoPrivateDirectoryLeft(t, private)
	if calls := takeCalls(t, dir); calls != nil {
		t.Error("a program ran without the authority file", calls)
	}

	// Without a CA file, verification stays enabled and refuses this untrusted certificate.
	database.DatabaseServerCertificateAuthorityFile = ""
	for name, mode := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		run, _ := postgresqlPrivateRuntime(t, mode)
		if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || !strings.HasSuffix(r.Reason, "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity.") {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	database.DangerouslySkipServerCertificateCheck = true
	if err := database.Prepare(""); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile), "")
	if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)); !strings.Contains(environments, "PGSSLMODE=prefer") {
		t.Error(environments)
	}

}

// psql ends with exit code 2 also when a session ends in the middle of a
// script. The task's first call had a session, so its login works: this
// failure has no kind and may be tried again, and the next try works. The
// first call for another database, a database task's extensions, is the
// one that tells whether the login works there.
func TestPostgresqlSessionThatEndsIsNoRefusedLogin(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.EndsTheSessionOnStatementsStartingWith = "create role"
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(true)}
	r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil)
	// The session ended somewhere in the changing script, so the result
	// does not say whether the role changed.
	if r.Status != "failed" || r.Change != "unknown" || r.FailureKind != "" || r.Reason != "postgresql_role: could not create the role shop: the session with the PostgreSQL server on this node's local socket ended, or psql got no new one: FATAL:  57P01: terminating connection due to administrator command" {
		t.Fatalf("%+v", r)
	}
	server.EndsTheSessionOnStatementsStartingWith = ""
	writeStandInPostgresql(t, dir, server)
	if r := (Runtime{}).PostgresqlRole(ctx, role, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Fatalf("the next try: %+v", r)
	}

	// A database that the login cannot enter: the task's first call there
	// gets no session, which no other try can fix. The database itself was
	// created by then, and the result says so.
	server = readStandInPostgresql(t, dir)
	server.DatabasesThatRefuseTheLogin = []string{"shop"}
	writeStandInPostgresql(t, dir, server)
	created := (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil)
	if created.Status != "failed" || created.FailureKind != "config" || postgresqlChange(t, created) != [3]any{"changed", true, ""} ||
		created.Reason != `postgresql_database: could not change the extensions of the database shop: psql got no session with the PostgreSQL server on this node's local socket as the role postgres: psql: error: connection to server failed: FATAL:  no pg_hba.conf entry for user "postgres", database "shop"; the server must accept the operating system account postgres as this role through its local socket, which a line of its pg_hba.conf decides; or give the task login_role and login_password` {
		t.Errorf("%+v", created)
	}
}

// login_role is a login that the task hands only to psql, as login_password
// is. A secret in it does not hide the result. It reaches psql in psql's
// environment and on no command line, and wherever the task's result names
// the role, it shows [redacted].
func TestPostgresqlLoginRoleThatIsASecret(t *testing.T) {
	ctx := testContext(t)
	const loginRole, password = `sentinel:role 4711`, "login password 4712"
	server := newStandInPostgresql(17)
	server.Roles[loginRole] = &standInPostgresqlRole{CanLogIn: true}
	server.LoginPasswords, server.RolesThatAreNoSuperuser = map[string]string{loginRole: password}, []string{loginRole}
	dir := postgresqlFixture(t, server)
	role := PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", Password: postgresqlText("the role's password"), LoginRole: "{{ .secrets.admin_role }}", LoginPassword: "{{ .secrets.admin_password }}"}
	// The login role is no superuser, and the failure names it.
	run, private := postgresqlPrivateRuntime(t, Runtime{})
	s, trace := taskScope(map[string]string{"admin_role": loginRole, "admin_password": password})
	r := reportedByTheEngine(run.PostgresqlRole(ctx, role, s, nil), s)
	if trace.Used || r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" ||
		!strings.HasPrefix(r.Reason, "postgresql_role: the login role [redacted] is not a superuser and cannot read stored passwords from pg_catalog.pg_authid") {
		t.Fatalf("%+v (hidden: %v)", r, trace.Used)
	}
	postgresqlNoPrivateDirectoryLeft(t, private)
	if seen := readState(t, filepath.Join(dir, standInPostgresqlPasswordFileSeen)); !strings.Contains(seen, `"line":"localhost:*:*:sentinel\\:role 4711:login password 4712\n"`) {
		t.Error(seen)
	}
	if environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)); !strings.Contains(environments, " PGUSER="+loginRole+"\n") {
		t.Error(environments)
	}
	if elsewhere := readState(t, filepath.Join(dir, "calls")) + readState(t, filepath.Join(dir, standInPostgresqlScriptsFile)); strings.Contains(elsewhere, "4711") {
		t.Errorf("the login role is on a command line or in a script:\n%s", elsewhere)
	}
	// The scope remembers the role and the password as they are, and the
	// role also as the password file holds it.
	if remembered := s.LoginsHandedOnlyToATool(); !slices.Equal(remembered, []string{loginRole, `sentinel\:role 4711`, password}) {
		t.Errorf("the task's scope remembers %q", remembered)
	}

	// A refused login: psql's message and MMX's own name the role.
	run, _ = postgresqlPrivateRuntime(t, Runtime{})
	s, trace = taskScope(map[string]string{"admin_role": loginRole, "admin_password": "another password"})
	r = reportedByTheEngine(run.PostgresqlRole(ctx, role, s, nil), s)
	if trace.Used || r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" ||
		r.Reason != `postgresql_role: could not read the role shop: psql got no session with the PostgreSQL server on this node's local socket as the role [redacted]: psql: error: connection to server failed: FATAL:  password authentication failed for user "[redacted]"; check login_role and login_password, and that the server's pg_hba.conf has a line that lets this role log in from this node with a password` {
		t.Errorf("%+v (hidden: %v)", r, trace.Used)
	}
}

// psql and pg_isready get only the environment that MMX sets. A variable of
// the node job worker process's own that would send them to another server,
// log them in as another role, or give them a password reaches neither. A
// task without login_password names a password file that cannot exist, so
// psql does not read the one in the home directory of its account.
func TestPostgresqlClientsGetOnlyTheEnvironmentThatMMXSets(t *testing.T) {
	for name, text := range map[string]string{"PGHOST": "other.example.com", "PGPORT": "5999", "PGSERVICE": "other", "PGOPTIONS": "-c lock_timeout=0", "PGSSLMODE": "disable",
		"PGUSER": "other_role", "PGPASSWORD": "inherited password", "PGPASSFILE": "/root/.pgpass", "PGDATABASE": "other", "HOME": "/root", "LANG": "de_DE.UTF-8"} {
		t.Setenv(name, text)
	}
	dir := postgresqlFixture(t, newStandInPostgresql(17))
	if r := (Runtime{}).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	environments := strings.Split(strings.TrimSpace(readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile))), "\n")
	psql := "psql: PGCLIENTENCODING=UTF8 PGCONNECT_TIMEOUT=10 PGDATABASE=postgres PGPASSFILE=/dev/null/mmx_task_without_login_password PGUSER=postgres"
	if !slices.Equal(environments, []string{"pg_isready: PGCONNECT_TIMEOUT=10", psql, psql}) {
		t.Errorf("%q", environments)
	}
	// Nothing else is there, beside what the shell that starts a program
	// sets itself (PWD, and with some shells SHLVL, OLDPWD, and _).
	for _, line := range strings.Split(strings.TrimSpace(readState(t, filepath.Join(dir, standInPostgresqlEnvironmentNamesFile))), "\n") {
		program, names, _ := strings.Cut(line, ": ")
		want := []string{"PATH", "PGCONNECT_TIMEOUT"}
		if program == "psql" {
			want = []string{"PATH", "PGCLIENTENCODING", "PGCONNECT_TIMEOUT", "PGDATABASE", "PGPASSFILE", "PGUSER"}
		}
		got := slices.DeleteFunc(strings.Fields(names), func(name string) bool { return slices.Contains([]string{"PWD", "OLDPWD", "SHLVL", "_"}, name) })
		if !slices.Equal(got, want) {
			t.Errorf("%s had the environment %q, want %q", program, got, want)
		}
	}
}

// Every script that changes the server first limits how long a statement
// waits for another session's lock; a script that only reads does not. The
// stand-in server refuses a changing statement in a session without the
// limit, so every other test of a change proves the same.
func TestPostgresqlChangingScriptsLimitTheWaitForALock(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["old"], server.Roles["owner"] = &standInPostgresqlRole{}, &standInPostgresqlRole{}
	server.Databases["old"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	server.Databases["kept"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	dir := postgresqlFixture(t, server)
	for name, r := range map[string]protocol.Result{
		"create role":       (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop"}, value.Scope{}, nil),
		"drop role":         (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "old", State: "absent"}, value.Scope{}, nil),
		"create database":   (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil),
		"owner of database": (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Name: "kept", Owner: "owner"}, value.Scope{}, nil),
		"drop database":     (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Name: "old", State: "deleted_with_all_its_data"}, value.Scope{}, nil),
	} {
		if r.Status != "succeeded" || r.Change != "changed" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// The stand-in notes each script with a line "-- script" before it.
	changing := 0
	for _, script := range strings.Split(readState(t, filepath.Join(dir, standInPostgresqlScriptsFile)), standInPostgresqlScriptStart)[1:] {
		limits, reads := strings.HasPrefix(script, "set lock_timeout = '10s';\n"), strings.HasPrefix(script, "begin read only;\n")
		if limits == reads || strings.Count(script, "lock_timeout") != map[bool]int{true: 1, false: 0}[limits] {
			t.Errorf("a script neither only reads nor limits the wait for a lock, or does both:\n%s", script)
		}
		if limits {
			changing++
		}
	}
	// Two for the roles; the database, its extensions, the owner, the drop.
	if changing != 6 {
		t.Errorf("%d changing scripts", changing)
	}
}

// When the server did every statement of a changing script and only the
// read after them failed, the result says that the task changed the
// server, with what the statements do.
func TestPostgresqlFailedReadAfterTheStatementsWereDone(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["old"], server.Roles["owner"] = &standInPostgresqlRole{}, &standInPostgresqlRole{}
	server.Databases["kept"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	server.EndsTheSessionWhenTheStatementsAreDone = true
	dir := postgresqlFixture(t, server)
	ended := ": the session with the PostgreSQL server on this node's local socket ended, or psql got no new one: FATAL:  57P01: terminating connection due to administrator command"
	for _, test := range []struct {
		reason string
		result protocol.Result
		want   [3]any
	}{
		{"postgresql_role: could not create the role shop", (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(true)}, value.Scope{}, nil), [3]any{"changed", true, ""}},
		{"postgresql_role: could not change the role owner", (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "owner", CanLogIn: postgresqlBool(true), Password: postgresqlText("a password")}, value.Scope{}, nil), [3]any{"changed", false, "can_log_in,password"}},
		{"postgresql_role: could not remove the role old", (Runtime{}).PostgresqlRole(ctx, PostgresqlRole{Name: "old", State: "absent"}, value.Scope{}, nil), [3]any{"changed", false, ""}},
		{"postgresql_database: could not create the database shop", (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil), [3]any{"changed", true, ""}},
		{"postgresql_database: could not change the owner of the database kept", (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Name: "kept", Owner: "owner", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil), [3]any{"changed", false, "owner"}},
		{"postgresql_database: could not change the extensions of the database kept", (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "owner", Name: "kept", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil), [3]any{"changed", false, "extensions"}},
		{"postgresql_database: could not drop the database kept", (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Name: "kept", State: "deleted_with_all_its_data"}, value.Scope{}, nil), [3]any{"changed", false, ""}},
	} {
		if r := test.result; r.Status != "failed" || r.FailureKind != "" || r.Reason != test.reason+ended || postgresqlChange(t, r) != test.want {
			t.Errorf("%s: %+v", test.reason, r)
		}
	}
	// The server has every change of which a result says "changed".
	after := readStandInPostgresql(t, dir)
	if after.Roles["shop"] == nil || !after.Roles["owner"].CanLogIn || after.Roles["old"] != nil || after.Databases["shop"] == nil || after.Databases["kept"] != nil {
		t.Errorf("%+v %+v", after.Roles, after.Databases)
	}
}

// One answer of psql that is longer than MMX reads fails the task, and the
// failure names the limit and what to do.
func TestPostgresqlAnswerLongerThanMMXReads(t *testing.T) {
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	for i := range 40000 {
		server.Memberships = append(server.Memberships, standInPostgresqlMembership{Role: fmt.Sprintf("a_role_with_a_long_name_%06d", i), Member: "shop", GrantedBy: "postgres"})
	}
	postgresqlFixture(t, server)
	r := (Runtime{}).PostgresqlRole(testContext(t), PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", MemberOfRoles: []string{}, MemberOfRolesNotListed: "remove"}, value.Scope{}, nil)
	if r.Status != "failed" || r.Change != "unchanged" || r.Reason != "postgresql_role: could not read the role shop: psql printed more than 1 MiB, and MMX reads no more of one answer. The answer lists the memberships of a role or the extensions of a database: this one has more of them than MMX can manage, so manage it with SQL, for example in a shell task that runs psql" {
		t.Errorf("%+v", r)
	}
}

// A task that is cancelled while pg_isready or psql waits for the server
// ends that program and fails with the reason of the cancellation; its
// private directory goes. What the result says about the change depends on
// the call that the cancellation meets: before the changing script nothing
// changed, and inside it MMX does not know.
//
// The test cancels the task when the program is inside the call that the
// server never answers, however long the calls before it took. So the test
// does not depend on the speed of the machine: a time limit instead would
// meet an earlier call where each start of a program is slow, as it is with
// the race detector.
func TestPostgresqlCancelledTaskEndsPsql(t *testing.T) {
	for _, test := range []struct {
		neverAnswered  string
		hangs          func(server *standInPostgresql)
		reason, change string
	}{
		{"pg_isready", func(server *standInPostgresql) { server.HangsInPgIsReady = true },
			"postgresql_role: context canceled", "unchanged"},
		{"the read of the role", func(server *standInPostgresql) { server.HangsOnStatementsStartingWith = "begin read only" },
			"postgresql_role: could not read the role shop: context canceled", "unchanged"},
		{"the statement that creates the role", func(server *standInPostgresql) { server.HangsOnStatementsStartingWith = "create role" },
			"postgresql_role: could not create the role shop: context canceled", "unknown"},
	} {
		t.Run(test.neverAnswered, func(t *testing.T) {
			server := newStandInPostgresql(17)
			server.LoginPasswords = map[string]string{"postgres": "admin password"}
			test.hangs(server)
			dir := postgresqlFixture(t, server)
			run, private := postgresqlPrivateRuntime(t, Runtime{})
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			// The stand-in notes its process ID when it starts to wait.
			taskEnded, waitingProcess := make(chan struct{}), make(chan int, 1)
			go func() {
				for {
					noted, _ := os.ReadFile(filepath.Join(dir, standInPostgresqlHangsFile))
					if process, err := strconv.Atoi(strings.TrimSuffix(string(noted), "\n")); err == nil && strings.HasSuffix(string(noted), "\n") {
						waitingProcess <- process
						cancel()
						return
					}
					select {
					case <-taskEnded:
						return
					case <-time.After(2 * time.Millisecond):
					}
				}
			}()
			r := run.PostgresqlRole(ctx, PostgresqlRole{CanLogIn: postgresqlBool(false), Name: "shop", DangerouslySkipServerCertificateCheck: true, DatabaseServerHost: "db.example.com", LoginPassword: "admin password"}, value.Scope{}, nil)
			close(taskEnded)
			if r.Status != "failed" || r.Change != test.change || r.FailureKind != "" || r.Reason != test.reason {
				t.Errorf("%+v", r)
			}
			select {
			case process := <-waitingProcess:
				// The task ended the program and waited for its end, so the
				// process is gone now. The stand-in itself would wait a
				// minute, then answer, and the task would go on.
				if err := syscall.Kill(process, 0); err != syscall.ESRCH {
					t.Errorf("the program (process %d) still runs after its task was cancelled: %v", process, err)
				}
			default:
				t.Error("the task ended before a program was inside the call that the server never answers")
			}
			postgresqlNoPrivateDirectoryLeft(t, private)
			if readStandInPostgresql(t, dir).Roles["shop"] != nil {
				t.Error("the cancelled task left the role")
			}
		})
	}
}

// A failure's text is the end of what psql wrote to standard error, without
// the lines that name a place in the server's source code.
func TestPostgresqlMessage(t *testing.T) {
	t.Parallel()
	r := protocol.Result{Stdout: "a stored password form", Reason: "exit status 3",
		Stderr: "NOTICE:  00000: role \"x\" does not exist, skipping\nLOCATION:  DropRole, user.c:1139\nFEHLER:  2BP01: Rolle »shop« kann nicht gelöscht werden\nDETAIL:  Eigentümer von Datenbank shop\nLOCATION:  DropRole, user.c:1165\n"}
	want := "NOTICE:  00000: role \"x\" does not exist, skipping\nFEHLER:  2BP01: Rolle »shop« kann nicht gelöscht werden\nDETAIL:  Eigentümer von Datenbank shop"
	if got := postgresqlMessage(r); got != want {
		t.Errorf("%q", got)
	}
	if got := postgresqlMessage(protocol.Result{Stdout: "a stored password form", Reason: "signal: killed"}); got != "signal: killed" {
		t.Errorf("%q", got)
	}
}
