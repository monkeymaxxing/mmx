package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// What postgresql_role and postgresql_database share: the connection fields,
// the call of psql, and the script that psql reads.
//
// Both modules run the node's psql program; MMX links no database driver.
// These rules hold for every call, and each comes from a measurement on
// PostgreSQL 13 to 18:
//
//   - The SQL goes to psql on its standard input, and every value of a task
//     (a name, a stored password form) goes into it as a psql variable, set
//     by a \set line of the same input. So no value is on a command line,
//     and psql quotes each one itself, as an identifier (:"name") or as a
//     literal (:'name').
//   - psql never asks for a password (--no-password): without that option
//     and without a terminal it reads the password from its standard input,
//     where the SQL is. A login password reaches it through a password file
//     in the node task's private directory.
//   - Every query prints one JSON value on one line, which is all MMX reads
//     of the server's state. A failure's text holds standard error only,
//     where psql and the server explain themselves, and never standard
//     output, where a query prints a stored password form.
//   - psql ends with exit code 2 for a server that does not run, a refused
//     login, and a missing database alike, and its words changed between
//     versions. So a task asks pg_isready first whether a server answers.
//   - The server's messages can be in another language. MMX decides from the
//     catalog, which it reads before it changes anything, and from exit
//     codes, never from the words of a message, and passes the message on.
//   - psql and pg_isready get only the environment that MMX sets
//     (postgresqlEnvironment). A variable of the node job worker process's own,
//     such as PGHOST, PGSERVICE, PGOPTIONS, or PGPASSWORD, would send them
//     to another server or log them in as another role than the task names;
//     and without PGPASSFILE psql reads the password file in the home
//     directory of the account that runs it.
//   - A script that changes the server sets lock_timeout first. Without it,
//     a statement waits without end for a lock that another session holds:
//     an ALTER ROLE waits as long as another session's change of the same
//     role is not committed.

// postgresqlAdministratorAccount is the operating system account that a
// PostgreSQL server on the node accepts as its administrator through the
// local socket, without a password. Every supported distribution's server
// package creates it under this name.
const postgresqlAdministratorAccount = "postgres"

// postgresqlAdministratorRole is the role that a task logs in as unless it
// names another one.
const postgresqlAdministratorRole = "postgres"

// postgresqlMaintenanceDatabase is the database that a task connects to for
// everything that is not inside one database: roles, and the list of
// databases.
const postgresqlMaintenanceDatabase = "postgres"

// postgresqlConnectionTimeLimit is how long psql and pg_isready wait for a
// connection, in seconds. Without a limit, a wrong address holds each try
// for minutes.
const postgresqlConnectionTimeLimit = "10"

// postgresqlLockTimeLimit is how long a changing statement waits for a lock
// that another session holds, as the server's setting lock_timeout takes it.
const postgresqlLockTimeLimit = "10s"

// postgresqlClientPath is the PATH of psql and pg_isready. Their
// environment holds nothing else of the node job worker process's own.
const postgresqlClientPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// postgresqlNoPasswordFile is what PGPASSFILE names for a task without
// login_password: a path that cannot exist, because /dev/null is no
// directory. Without the variable, psql reads the password file in the
// home directory of the account that runs it (~/.pgpass), also when HOME is
// not set, and would log in with a password that the task does not name.
// psql says nothing about a password file that does not exist; for
// /dev/null itself it prints a warning.
const postgresqlNoPasswordFile = "/dev/null/mmx_task_without_login_password"

// postgresqlAccountOfTheLocalServer finds the account that runs psql for a
// server on the same node; tests replace it.
var postgresqlAccountOfTheLocalServer = func(env map[string]string) (*syscall.Credential, error) {
	return shellAccount(postgresqlAdministratorAccount, env)
}

// postgresqlProgramsOfTheDebianFamily is where the client programs of each
// installed PostgreSQL version are on Debian and Ubuntu; tests replace it.
var postgresqlProgramsOfTheDebianFamily = "/usr/lib/postgresql/*/bin"

// postgresqlConnection is the connection fields of one task, as its module's
// input type holds them.
type postgresqlConnection struct {
	// module names the task's module in failures.
	module string
	// host is database_server_host, "" for the local socket.
	host string
	// port is database_server_port as digits, "" for the client's own.
	port string
	// certificateAuthorityFile is database_server_certificate_authority_file.
	certificateAuthorityFile   string
	skipServerCertificateCheck bool
	// loginRole and loginPassword are login_role and login_password as the
	// task wrote them, "" for the role postgres and for no password. Both
	// are private values that the module hands only to psql.
	loginRole, loginPassword string
}

// where names the server in a message.
func (c postgresqlConnection) where() string {
	place := "on this node's local socket"
	if c.host != "" {
		place = "at " + c.host
	}
	if c.port != "" {
		place += ", port " + c.port
	}
	return place
}

// postgresqlPort is database_server_port as digits.
func postgresqlPort(written any) string {
	if written == nil {
		return ""
	}
	return fmt.Sprint(written)
}

// validatePostgresqlConnection checks the connection fields when the project
// loads.
func validatePostgresqlConnection(host, certificateAuthorityFile, loginPassword string, skipServerCertificateCheck bool) error {
	switch {
	case skipServerCertificateCheck && certificateAuthorityFile != "":
		return invalid("dangerously_skip_server_certificate_check", "give either database_server_certificate_authority_file or dangerously_skip_server_certificate_check: true, not both")
	case skipServerCertificateCheck && host == "":
		return invalid("dangerously_skip_server_certificate_check", "dangerously_skip_server_certificate_check needs database_server_host; a local socket has no server certificate")

	case certificateAuthorityFile != "" && host == "":
		return invalid("database_server_certificate_authority_file", "database_server_certificate_authority_file needs database_server_host; a connection through the server's local socket has no certificate to check")
	case host != "" && loginPassword == "":
		return invalid("login_password", "database_server_host needs login_password: a login over the network needs the password of the role in login_role")
	}
	return nil
}

// postgresqlConnectionArguments lists the connection fields that may hold a
// reference, with their rules. The login is a private value that the module
// hands only to psql.
func postgresqlConnectionArguments(arguments *ArgumentList, host *string, port *any, certificateAuthorityFile *string, loginRole, loginPassword string) {
	if *host != "" {
		arguments.Public("database_server_host", host, postgresqlHostRule)
	}
	if *port != nil {
		arguments.Public("database_server_port", port, postgresqlPortRule)
	}
	if *certificateAuthorityFile != "" {
		arguments.Public("database_server_certificate_authority_file", certificateAuthorityFile, absolute)
	}
	if loginRole != "" {
		arguments.LoginHandedOnlyToATool("login_role", loginRole, postgresqlLoginRoleRule)
	}
	if loginPassword != "" {
		arguments.LoginHandedOnlyToATool("login_password", loginPassword, postgresqlLoginPasswordRule)
	}
}

var (
	// A name inside a database server: a role, a database, or an extension.
	// PostgreSQL accepts every character in a quoted name. How long a name
	// may be depends on the server, which the task asks.
	postgresqlNameRule = rule("must be a name: not empty, and without a null byte", func(s string) bool {
		return s != "" && !strings.ContainsRune(s, 0)
	})
	// psql takes a host that starts with a slash for the directory of a
	// socket; the password file would then not match.
	postgresqlHostName = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	postgresqlHostRule = rule("database_server_host must be a host name or an address, such as db.example.com or 10.0.0.5", postgresqlHostName.MatchString)
	// The message never repeats the value, which is a secret.
	postgresqlPasswordRule = rule("password must not be empty: PostgreSQL takes an empty password as \"remove the password\"; leave password out to keep the password that the role has", func(s string) bool {
		return s != ""
	})
	// PostgreSQL's password file holds one login on each line, with the
	// role and the password as two of its fields.
	postgresqlLoginRoleRule = rule("login_role must not be empty and must not hold a line break or a zero byte", func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\r\n\x00")
	})
	postgresqlLoginPasswordRule = rule("login_password must not be empty and must not hold a line break, which PostgreSQL's password file cannot hold", func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\r\n\x00")
	})
)

// postgresqlPortRule accepts a port number: an integer, as a task writes it
// or as a reference gives it.
func postgresqlPortRule(v any) error {
	port, err := AccountID(value.Plain(v))
	if err != nil || port < 1 || port > 65535 {
		return errors.New("database_server_port must be a number from 1 to 65535")
	}
	return nil
}

// postgresqlClient is psql for one task: the account that runs it, its
// environment, and its options.
type postgresqlClient struct {
	connection postgresqlConnection
	// role is the role that the task logs in as: login_role, or postgres.
	role string
	// account runs psql: the administrator account for the local socket,
	// nil (root) for a server on another machine.
	account *syscall.Credential
	// settings are the variables of psql's environment beside PATH: how
	// long it waits, as which role it logs in, and with which password file.
	settings map[string]string
	// options are psql's options for every call.
	options []string
	// close removes the task's private directory with the password file.
	close func()
	// hadSession names the databases in which a call of this task had a
	// session, so the task's login works there.
	hadSession map[string]bool
}

// postgresqlEnvironment is the whole environment of one call of psql or
// pg_isready: PATH and the variables in settings. Without a locale in it,
// the words of psql itself are English; PGCLIENTENCODING says that the
// text is UTF-8.
func postgresqlEnvironment(settings map[string]string) []string {
	environment := []string{"PATH=" + postgresqlClientPath}
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		environment = append(environment, name+"="+settings[name])
	}
	return environment
}

// postgresqlScriptFailure is a failed call of psql.
type postgresqlScriptFailure struct {
	reason string
	// noSession: psql got no session although a server answers: the server
	// refuses the login, the database does not exist or takes no
	// connections, or psql does not accept the server's certificate.
	// Another try cannot fix it, and each try would leave one more failed
	// login in the server's log.
	noSession bool
	// refusedStatement: the server refused one statement (psql's exit code
	// 3), and psql stopped there and ended the session in order. Nothing of
	// a transaction that was open then stays.
	refusedStatement bool
}

func (failure *postgresqlScriptFailure) Error() string { return failure.reason }

// postgresqlFailure is the result of a task that failed before it changed
// anything, for a failed call of psql that only reads or for a check: the
// result says that nothing changed. A psql without a session is a
// configuration failure, which the node task execution engine does not try
// again: the login that the task was given does not work. Its reason holds
// what psql and the server said, which names the host, the role, and the
// password file's path; MMX replaces a login that is a secret in every
// text of the result. Every other failure may be tried again.
func postgresqlFailure(err error) protocol.Result {
	r := protocol.Failed(err)
	var failure *postgresqlScriptFailure
	if errors.As(err, &failure) && failure.noSession {
		r = protocol.ConfigurationError(err)
	}
	r.Change = "unchanged"
	return r
}

// postgresqlBeforeAnyChange is the result of a check that a task makes
// before its first changing statement: a failure says that nothing
// changed. A dryrun's "not checked" stays as it is.
func postgresqlBeforeAnyChange(r protocol.Result) protocol.Result {
	if r.Status == "failed" {
		r.Change = "unchanged"
	}
	return r
}

// postgresqlClient makes psql ready for one task and asks whether a server
// answers. The second result is set when the task ends here: the client
// programs or the administrator account are missing or no server answers,
// each of which an earlier node task may still provide (not checked in a
// dryrun), or the login cannot be prepared. Nothing has changed then.
func (run Runtime) postgresqlClient(ctx context.Context, c postgresqlConnection, s value.Scope, packages PackageManager) (*postgresqlClient, *protocol.Result) {
	stop := func(r protocol.Result) (*postgresqlClient, *protocol.Result) {
		r = postgresqlBeforeAnyChange(r)
		return nil, &r
	}
	for _, program := range []string{"psql", "pg_isready"} {
		if !postgresqlProgramInstalled(ctx, program) {
			return stop(run.notYet(fmt.Errorf("%s: %w. It comes with PostgreSQL's server package; for a server on another machine, install the client alone",
				c.module, missingProgramError(program))))
		}
	}
	client := &postgresqlClient{connection: c, role: postgresqlAdministratorRole, settings: map[string]string{
		"PGCONNECT_TIMEOUT": postgresqlConnectionTimeLimit,
		// MMX's own text is UTF-8, whatever encoding a database has.
		"PGCLIENTENCODING": "UTF8",
		"PGPASSFILE":       postgresqlNoPasswordFile,
	}, close: func() {}, hadSession: map[string]bool{}}
	client.options = append([]string{"--no-psqlrc", "--no-password", "--quiet", "--no-align", "--tuples-only", "--set", "ON_ERROR_STOP=1", "--set", "VERBOSITY=verbose"}, c.target()...)
	if c.host == "" {
		// The account's own environment (HOME, USER) is not psql's.
		account, err := postgresqlAccountOfTheLocalServer(map[string]string{})
		if err != nil {
			return stop(run.accountFailure(fmt.Errorf("%s: the operating system account %s, which runs psql for a server on this node, cannot be used: %w. PostgreSQL's server package creates the account; for a server on another machine, set database_server_host and login_password",
				c.module, postgresqlAdministratorAccount, err)))
		}
		client.account = account
	}
	if c.host != "" {
		client.settings["PGSSLMODE"] = "verify-full"
		if c.skipServerCertificateCheck {
			client.settings["PGSSLMODE"] = "prefer"
		}
	}

	if c.certificateAuthorityFile != "" && !c.skipServerCertificateCheck {
		if _, err := os.Stat(c.certificateAuthorityFile); err != nil {
			if os.IsNotExist(err) {
				return stop(run.notYet(fmt.Errorf("%s: database_server_certificate_authority_file %s does not exist on the node; install the certificate of the authority in an earlier node task", c.module, c.certificateAuthorityFile)))
			}
			return stop(protocol.Failed(fmt.Errorf("%s: database_server_certificate_authority_file: %w", c.module, err)))
		}
		client.settings["PGSSLMODE"], client.settings["PGSSLROOTCERT"] = "verify-full", c.certificateAuthorityFile
	}
	// The login: the role and its password are private values that psql
	// alone gets, the role in its environment and the password in a file.
	// A secret in either does not hide the task's result; the scope
	// remembers each as it is and as the password file holds it, with a
	// backslash before each colon and backslash.
	password := ""
	for _, login := range []struct {
		field, written string
		text           *string
		rule           func(any) error
	}{{"login_role", c.loginRole, &client.role, postgresqlLoginRoleRule}, {"login_password", c.loginPassword, &password, postgresqlLoginPasswordRule}} {
		if login.written == "" {
			continue
		}
		resolved, err := s.LoginHandedOnlyToATool(login.written)
		if err == nil {
			*login.text, err = value.Scalar(resolved)
		}
		if err == nil {
			err = login.rule(*login.text)
		}
		if err != nil {
			return stop(protocol.Failed(fmt.Errorf("%s.%s: %w", c.module, login.field, err)))
		}
		if value.HasSecrets(resolved) {
			s.FormOfLoginHandedOnlyToATool(postgresqlPasswordFileField(*login.text))
		}
	}
	// The role is in psql's environment, which only the account that runs
	// psql and root can read, and on no command line.
	client.settings["PGUSER"] = client.role
	if stopped := client.serverAnswers(ctx, run); stopped != nil {
		return nil, stopped
	}
	if c.loginPassword != "" {
		// The password file belongs to the account that runs psql and is in
		// the task's private directory, which only that account can open.
		// A dryrun writes it too: it logs in to read the catalog.
		directory, remove, err := run.makePrivateTaskDirectory(c.module, client.account, func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "password_file"), []byte(c.passwordFileLine(client.role, password)), 0600)
		})
		if err != nil {
			return stop(protocol.Failed(fmt.Errorf("%s: the private directory for the login password: %w", c.module, err)))
		}
		client.close = remove
		client.settings["PGPASSFILE"] = filepath.Join(directory, "password_file")
	}
	return client, nil
}

// postgresqlProgramInstalled reports whether a client program can run. On
// Debian and Ubuntu, psql and pg_isready on the PATH are links to a wrapper
// that the package postgresql-client-common installs; the programs
// themselves come with a client package of one version and are in that
// version's own directory. The wrapper alone fails every call.
func postgresqlProgramInstalled(ctx context.Context, name string) bool {
	path, err := lookPath(ctx, name)
	if err != nil {
		return false
	}
	if target, err := filepath.EvalSymlinks(path); err == nil && filepath.Base(target) == "pg_wrapper" {
		programs, _ := filepath.Glob(filepath.Join(postgresqlProgramsOfTheDebianFamily, name))
		return len(programs) > 0
	}
	return true
}

// target is the options that say which server psql and pg_isready talk to.
func (c postgresqlConnection) target() []string {
	var options []string
	if c.host != "" {
		options = append(options, "--host="+c.host)
	}
	if c.port != "" {
		options = append(options, "--port="+c.port)
	}
	return options
}

// passwordFileLine is the one line of the task's password file:
// host:port:database:role:password. psql takes a line only when its host is
// the text of --host, and localhost for the local socket. The database is
// any, because a database task connects to two. A colon and a backslash in a
// field are written with a backslash before them.
func (c postgresqlConnection) passwordFileLine(role, password string) string {
	host, port := "localhost", "*"
	if c.host != "" {
		host = postgresqlPasswordFileField(c.host)
	}
	if c.port != "" {
		port = c.port
	}
	return host + ":" + port + ":*:" + postgresqlPasswordFileField(role) + ":" + postgresqlPasswordFileField(password) + "\n"
}

// postgresqlPasswordFileField is a text as one field of a password file's
// line.
func postgresqlPasswordFileField(text string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(text)
}

// serverAnswers asks pg_isready whether a server accepts connections, which
// needs no login. pg_isready says it with its exit code: 0 for yes, 1 for a
// server that refuses connections for now (it starts up or shuts down), 2
// for no answer, and 3 when it made no attempt. A server that does not
// answer is something an earlier node task may still start, so a dryrun is
// not checked; a run fails and may try again, because a server that starts
// answers a moment later.
//
// pg_isready gets none of the task's settings for the login or for the
// server's certificate: with the certificate settings it reports a
// certificate that it does not accept as "no response", which is the
// answer for a server that is not there.
func (client *postgresqlClient) serverAnswers(ctx context.Context, run Runtime) *protocol.Result {
	c := client.connection
	environment := postgresqlEnvironment(map[string]string{"PGCONNECT_TIMEOUT": postgresqlConnectionTimeLimit})
	asked := runProcessWithOnlyThisEnvironment(ctx, environment, "/", nil, client.account, "pg_isready", append(c.target(), "--timeout="+postgresqlConnectionTimeLimit)...).Result
	if asked.Status == "succeeded" {
		return nil
	}
	failed := func(reason string) *protocol.Result {
		r := postgresqlBeforeAnyChange(protocol.Failed(errors.New(reason)))
		return &r
	}
	if err := ctx.Err(); err != nil {
		return failed(c.module + ": " + err.Error())
	}
	var reason string
	switch asked.ExitCode {
	case 1:
		reason = fmt.Sprintf("%s: the PostgreSQL server %s is temporarily refusing connections while starting up or shutting down", c.module, c.where())
	case 2:
		advice := "install and start the server in an earlier node task"
		if c.host != "" {
			advice = "check the address and the port, and that the server accepts connections from this node"
		}
		reason = fmt.Sprintf("%s: no PostgreSQL server answers %s; %s", c.module, c.where(), advice)
	default:
		// pg_isready explains itself on standard error, or else on standard
		// output, which holds no data.
		detail := failureTextFromStandardError(asked)
		if said := strings.TrimSpace(asked.Stdout); strings.TrimSpace(asked.Stderr) == "" && said != "" {
			detail = said
		}
		return failed(fmt.Sprintf("%s: pg_isready could not ask the PostgreSQL server %s (exit code %d): %s", c.module, c.where(), asked.ExitCode, detail))
	}
	if run.Dryrun {
		return &protocol.Result{Status: "not_checked", Change: "unknown", Reason: reason}
	}
	return failed(reason)
}

// run sends one script to psql, connected to database, and returns the JSON
// values that its queries printed, one for each query. what says what the
// script does, for a failure: "read the role shop". A failure is a
// *postgresqlScriptFailure.
func (client *postgresqlClient) run(ctx context.Context, database, what string, script *postgresqlScript) ([]json.RawMessage, error) {
	c := client.connection
	settings := map[string]string{"PGDATABASE": database}
	for name, text := range client.settings {
		settings[name] = text
	}
	done := runProcessWithOnlyThisEnvironment(ctx, postgresqlEnvironment(settings), "/", strings.NewReader(script.text()), client.account, "psql", client.options...)
	r := done.Result
	// Each query prints one JSON value on a line of its own. A script that
	// failed part of the way printed the values of its first queries.
	var printed []json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(done.Output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			printed = append(printed, json.RawMessage(line))
		}
	}
	// psql ends with exit code 2 when it gets no session, and also when a
	// session ends in the middle of a script. The task's first call for a
	// database tells the two apart without the words of a message: once a
	// call had a session there, the task's login works.
	loginWorks := client.hadSession[database]
	if ctx.Err() == nil && (r.Status == "succeeded" || r.ExitCode == 3) {
		// Exit code 3 is a statement that the server refused, in a session.
		client.hadSession[database] = true
	}
	failed := func(failure postgresqlScriptFailure, format string, values ...any) ([]json.RawMessage, error) {
		failure.reason = fmt.Sprintf("%s: could not %s: ", c.module, what) + fmt.Sprintf(format, values...)
		return printed, &failure
	}
	switch {
	case r.Status == "succeeded" && done.Truncated:
		return failed(postgresqlScriptFailure{}, "psql printed more than 1 MiB, and MMX reads no more of one answer. The answer lists the memberships of a role or the extensions of a database: this one has more of them than MMX can manage, so manage it with SQL, for example in a shell task that runs psql")
	case r.Status == "succeeded":
		return printed, nil
	case ctx.Err() != nil:
		return failed(postgresqlScriptFailure{}, "%s", ctx.Err())
	case r.ExitCode == 2 && !loginWorks:
		// pg_isready found a server, so this is a login that the server
		// refuses, a database that does not exist or takes no connections,
		// or a certificate that psql does not accept.
		return failed(postgresqlScriptFailure{noSession: true}, "psql got no session with the PostgreSQL server %s as the role %s: %s; %s", c.where(), client.role, postgresqlMessage(r), client.loginAdvice()) //nolint:nilaway // Both task entry points check stop before any direct or nested client method call.
	case r.ExitCode == 2:
		return failed(postgresqlScriptFailure{}, "the session with the PostgreSQL server %s ended, or psql got no new one: %s", c.where(), postgresqlMessage(r))
	}
	return failed(postgresqlScriptFailure{refusedStatement: r.ExitCode == 3}, "%s", postgresqlMessage(r))
}

// loginAdvice says what to check when psql got no session: the two login
// fields, the server's login rules, and certificate verification for a
// remote connection that checks the server's identity.
func (client *postgresqlClient) loginAdvice() string {
	c := client.connection
	if c.loginPassword == "" {
		return "the server must accept the operating system account " + postgresqlAdministratorAccount + " as this role through its local socket, which a line of its pg_hba.conf decides; or give the task login_role and login_password"
	}
	advice := "check login_role and login_password, and that the server's pg_hba.conf has a line that lets this role log in from this node with a password"
	if c.host != "" && !c.skipServerCertificateCheck {
		advice += ". " + databaseServerCertificateAdvice
	}
	return advice
}

// change runs a script that changes the server and ends with
// statementsDone and, after it, the script's reading queries. It returns
// what those queries printed, and what is known of the change when the
// call failed: "changed" when the script's statements were done and only a
// read after them failed, "unchanged" when the server refused a statement
// or psql got no session, so that nothing of the script stays, and
// "unknown" when the session ended somewhere in the script.
func (client *postgresqlClient) change(ctx context.Context, database, what string, script *postgresqlScript) ([]json.RawMessage, string, error) {
	printed, err := client.run(ctx, database, what, script)
	if len(printed) > 0 && string(printed[0]) == postgresqlStatementsDone {
		return printed[1:], "changed", err
	}
	var failure *postgresqlScriptFailure
	switch {
	case err == nil:
		return nil, "unknown", fmt.Errorf("%s: could not %s: psql did not print the JSON value that MMX asked for", client.connection.module, what) //nolint:nilaway // PostgresqlDatabase and PostgresqlRole return on constructor failure before calling change.
	case errors.As(err, &failure) && (failure.refusedStatement || failure.noSession):
		return nil, "unchanged", err
	}
	return nil, "unknown", err
}

// postgresqlMessage is what psql and the server said about a failure, as
// they wrote it: the end of standard error, without the lines that name the
// place in the server's source code (LOCATION), which differ with every
// version. It never holds standard output.
func postgresqlMessage(r protocol.Result) string {
	var lines []string
	for _, line := range strings.Split(r.Stderr, "\n") {
		if !strings.HasPrefix(line, "LOCATION:  ") {
			lines = append(lines, line)
		}
	}
	if said := lastLines(strings.TrimSpace(strings.Join(lines, "\n")), 20); said != "" {
		return said
	}
	return r.Reason
}

// postgresqlScript is the text that psql reads on its standard input: the
// \set lines of the task's values, and SQL statements that name those
// values as psql variables.
type postgresqlScript struct {
	lines     []string
	variables int
}

// variable makes a psql variable with this value and returns its name.
//
// A \set line gives the value in single quotes. There psql reads ” as one
// quote and a backslash as the start of an escape, and the line ends at a
// line break. So a quote is doubled, and a backslash and every control
// character is written as a backslash and three octal digits.
func (script *postgresqlScript) variable(text string) string {
	script.variables++
	name := fmt.Sprintf("mmx_value_%d", script.variables)
	var quoted strings.Builder
	for i := 0; i < len(text); i++ {
		switch b := text[i]; {
		case b == '\'':
			quoted.WriteString("''")
		case b == '\\' || b < 0x20 || b == 0x7f:
			fmt.Fprintf(&quoted, `\%03o`, b)
		default:
			quoted.WriteByte(b)
		}
	}
	script.lines = append(script.lines, `\set `+name+` '`+quoted.String()+`'`)
	return name
}

// identifier is text as a quoted name in a statement, such as a role's name.
func (script *postgresqlScript) identifier(text string) string {
	return `:"` + script.variable(text) + `"`
}

// literal is text as a quoted value in a statement.
func (script *postgresqlScript) literal(text string) string {
	return `:'` + script.variable(text) + `'`
}

// list is a list of texts as a JSON value in a statement.
func (script *postgresqlScript) list(texts []string) string {
	if texts == nil {
		texts = []string{}
	}
	encoded, _ := json.Marshal(texts)
	return script.literal(string(encoded)) + "::pg_catalog.jsonb"
}

// statement adds one SQL statement, on one line.
func (script *postgresqlScript) statement(sql string) {
	script.lines = append(script.lines, strings.Join(strings.Fields(sql), " ")+";")
}

// text is the whole script.
func (script *postgresqlScript) text() string {
	return strings.Join(script.lines, "\n") + "\n"
}

// postgresqlChangingScript starts a script that changes the server. Its
// statements wait at most postgresqlLockTimeLimit for a lock that another
// session holds; the server then refuses the statement, and the task fails
// and may be tried again.
func postgresqlChangingScript() *postgresqlScript {
	script := &postgresqlScript{}
	script.statement("set lock_timeout = '" + postgresqlLockTimeLimit + "'")
	return script
}

// postgresqlStatementsDone is the mark that psql prints when every changing
// statement of a script is done.
const postgresqlStatementsDone = `{"statements_done": true}`

// statementsDone adds the line that says that every statement before it is
// done, and committed where the statements are one transaction. psql prints
// the mark itself, without the server, and reaches the line only when no
// statement before it failed. Without the mark, a script that failed would
// not say whether it failed before its COMMIT or in the read after it.
func (script *postgresqlScript) statementsDone() {
	script.lines = append(script.lines, `\echo '`+postgresqlStatementsDone+`'`)
}

// postgresqlReadOnly starts a script that only reads. The server refuses
// every change inside the transaction, so a script that reads the state can
// change nothing, also not by a mistake in MMX.
func postgresqlReadOnly() *postgresqlScript {
	script := &postgresqlScript{}
	script.statement("begin read only")
	return script
}

// postgresqlNameTooLong is the failure for a name that is longer than the
// server takes. PostgreSQL cuts such a name with only a notice, and the
// statement then acts on the object with the shorter name.
func postgresqlNameTooLong(module, field, name string, limit int) error {
	if limit <= 0 || len(name) <= limit {
		return nil
	}
	return fmt.Errorf("%s.%s: the name %q has %d bytes, and this PostgreSQL server takes names of at most %d bytes. The server would cut the name and act on %q; write a shorter name", module, field, name, len(name), limit, strings.ToValidUTF8(name[:limit], ""))
}

// postgresqlChangeResult is the result of a task of both modules: whether
// the object was created, and the names of the attributes that differed.
func postgresqlChangeResult(change string, created bool, changedAttributes []string) protocol.Result {
	r := protocol.Success(change)
	names := make([]any, len(changedAttributes))
	for i, name := range changedAttributes {
		names[i] = name
	}
	r.Value = map[string]any{"created": created, "changed_attributes": names}
	return r
}

// postgresqlListedTwice checks a list of names that a task owns: a written
// name appears once.
func postgresqlListedTwice(field string, names []string) error {
	seen := map[string]bool{}
	for i, name := range names {
		if !Dynamic(name) && seen[name] {
			return invalid(fmt.Sprintf("%s[%d]", field, i), field+" lists "+name+" twice")
		}
		seen[name] = true
	}
	return nil
}
