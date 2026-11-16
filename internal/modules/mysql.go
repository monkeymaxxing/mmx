package modules

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// What mysql_user and mysql_database share: the connection fields, finding
// the database's client program, the one way MMX calls it, and reading what
// it prints. Both modules work with MySQL and with MariaDB. MMX links no
// database driver: it runs the node's own client, mariadb or mysql, sends
// SQL through its standard input, and reads the rows it prints.
//
// The client is called so that only the task decides where it connects:
//
//   - with an empty environment (env -i), because variables such as
//     MYSQL_HOST and MYSQL_PWD send the client to another server or account,
//     and MySQL's client reads ~/.mylogin.cnf through HOME;
//   - with --no-defaults, or with --defaults-file and MMX's own login file,
//     so that it reads no option file of the node, such as /root/.my.cnf;
//   - with --binary-mode, which makes it take its input as SQL only: without
//     it a line that starts with \! runs a command on the node.
//
// A task without connection fields uses the server on the node through its
// socket as root, which the server accepts as root@localhost. With
// connection fields the login goes into a file in the task's private
// directory (makePrivateTaskDirectory), so a password is on no command line
// and in no environment.

const databaseServerCertificateAdvice = "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity."

const (
	// mysqlClientPath is the PATH of the client's otherwise empty
	// environment.
	mysqlClientPath = "/usr/sbin:/usr/bin:/sbin:/bin"
	// mysqlConnectSeconds is how long a connection to database_server_host
	// may take.
	mysqlConnectSeconds = 10
	// mysqlLockWaitSeconds is how long a statement that changes the server
	// waits for a lock that another session holds.
	mysqlLockWaitSeconds = 10
	mysqlStandardPort    = 3306
	mysqlDefaultUser     = "root"
)

// The numbers of the errors that MMX acts on. It reads the number of an
// error, never its words: the words differ between the two products and
// between their versions.
const (
	mysqlErrorNoConnectionThroughSocket = 2002 // also MariaDB's client over TCP
	mysqlErrorNoConnectionOverTCP       = 2003
	mysqlErrorTLS                       = 2026
	mysqlErrorLoginRefused              = 1045
	mysqlErrorLoginRefusedForMethod     = 1698
	mysqlErrorLockWaitTimeout           = 1205
)

// mysqlConnection is the connection fields of a task, which both modules
// have under the same names. Host, Port, and CertificateAuthorityFile
// arrive resolved; LoginUser and LoginPassword are private values as the
// task wrote them. Port is nil when the task does not name it, and else an
// integer, or a reference to one until the task's arguments are resolved.
type mysqlConnection struct {
	Host                       string
	Port                       any
	LoginUser                  string
	LoginPassword              string
	CertificateAuthorityFile   string
	SkipServerCertificateCheck bool
}

// port is the server's port: the task's, or the standard one.
func (c mysqlConnection) port() int {
	if number, err := AccountID(c.Port); c.Port != nil && err == nil {
		return int(number)
	}
	return mysqlStandardPort
}

// check checks the connection fields when the project loads.
func (c mysqlConnection) check() error {
	switch {
	case c.SkipServerCertificateCheck && c.CertificateAuthorityFile != "":
		return invalid("dangerously_skip_server_certificate_check", "give either database_server_certificate_authority_file or dangerously_skip_server_certificate_check: true, not both")
	case c.SkipServerCertificateCheck && c.Host == "":
		return invalid("dangerously_skip_server_certificate_check", "dangerously_skip_server_certificate_check needs database_server_host; a local socket has no server certificate")

	case c.Port != nil && c.Host == "":
		return invalid("database_server_port", "database_server_port needs database_server_host; without a host the task uses the server's local socket, which has no port")
	case c.CertificateAuthorityFile != "" && c.Host == "":
		return invalid("database_server_certificate_authority_file", "database_server_certificate_authority_file needs database_server_host; a connection through the server's local socket has no certificate to check")
	case c.Host != "" && c.LoginPassword == "":
		return invalid("login_password", "database_server_host needs login_password: a login over the network needs the password of the account in login_user")
	}
	return nil
}

var (
	// A port is an integer, as a task writes it or as a reference gives it.
	mysqlPortRule = func(v any) error {
		if number, err := AccountID(v); err != nil || number < 1 || number > 65535 {
			return errors.New("database_server_port must be a number from 1 to 65535")
		}
		return nil
	}
	mysqlHostName = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	mysqlHostRule = rule("database_server_host must be a host name or an address, such as db.example.com or 10.0.0.5", mysqlHostName.MatchString)
	// A login's text goes into an option file, whose values end at a zero
	// byte. The messages never repeat the value, which is a secret.
	mysqlLoginUserRule = rule("login_user must not be empty and must not hold a zero byte", func(s string) bool {
		return s != "" && !strings.ContainsRune(s, 0)
	})
	// An empty login_password is a reference that resolved to nothing, such
	// as a secret that is not set: an account without a password has no
	// login_password in its task.
	mysqlLoginPasswordRule = rule("login_password must not be empty and must not hold a zero byte", func(s string) bool {
		return s != "" && !strings.ContainsRune(s, 0)
	})
)

// mysqlConnectionArguments lists the connection fields that may hold a
// reference. The login is a private value that the module hands only to the
// database's client.
func mysqlConnectionArguments(arguments *ArgumentList, host *string, port *any, certificateAuthorityFile *string, loginUser, loginPassword string) {
	if *host != "" {
		arguments.Public("database_server_host", host, mysqlHostRule)
	}
	if *port != nil {
		arguments.Public("database_server_port", port, mysqlPortRule)
	}
	if *certificateAuthorityFile != "" {
		arguments.Public("database_server_certificate_authority_file", certificateAuthorityFile, absolute)
	}
	if loginUser != "" {
		arguments.LoginHandedOnlyToATool("login_user", loginUser, mysqlLoginUserRule)
	}
	if loginPassword != "" {
		arguments.LoginHandedOnlyToATool("login_password", loginPassword, mysqlLoginPasswordRule)
	}
}

// mysqlConnectionFieldNotes are the notes of the connection fields in a
// module's description.
func mysqlConnectionFieldNotes() []Field {
	return []Field{
		{Name: "database_server_host", Notes: "the server's host name or address; without it the task uses the server on the node through its local socket, as root, which the server accepts as root@localhost"},
		{Name: "database_server_port", Type: "integer", Default: strconv.Itoa(mysqlStandardPort), Notes: "only with database_server_host: the server's port when it is not the standard one, a number from 1 to 65535"},
		{Name: "login_user", Default: mysqlDefaultUser, Notes: "the database account MMX logs in as; a private value; it must be allowed to create accounts and databases and to grant what the task grants"},
		{Name: "login_password", Notes: "that account's password; a private value, normally a secret; an empty value is refused; required with database_server_host; without database_server_host the task logs in with the password through the local socket; it reaches the client through a file in a private directory under /run that the task removes, never through a command line or an environment; a secret in it does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
		{Name: "database_server_certificate_authority_file", Notes: "only with database_server_host: the absolute path on the node of a trusted authority certificate; checks the server certificate and host name; without it verification remains enabled using the client's trust configuration; see the module page for client requirements"},
		{Name: "dangerously_skip_server_certificate_check", Default: "false", Notes: "only with database_server_host; true cannot be combined with database_server_certificate_authority_file and turns off verification of the server certificate, allowing an impersonated server to receive SQL and login information; the client encrypts when the server offers TLS and may connect without encryption otherwise"},
	}
}

// mysqlServer is the database server of one task, with the client program
// that reaches it.
type mysqlServer struct {
	run Runtime
	// module names the task's module in failures.
	module string
	// program is the client: mariadb, or mysql where there is no mariadb.
	program string
	// options are the client's options that say where and as whom it logs in.
	options []string
	// where names the server in a failure.
	where string
	// local: the task has no connection fields.
	local bool
	// host is database_server_host, "" for the local socket.
	host string
	// mariaDB says which of the two products the server is, and version is
	// what its VERSION() says.
	mariaDB bool
	version string
}

// mysqlFailure is a call of the client that failed.
type mysqlFailure struct {
	// Number is the number of the server's or the client's error, 0 when the
	// client printed none.
	Number int
	// Text is what the client wrote to standard error, never what it wrote
	// to standard output, where the rows of the catalog are.
	Text string
}

// is reports whether the failure has one of these error numbers.
func (f *mysqlFailure) is(numbers ...int) bool {
	for _, number := range numbers {
		if f.Number == number {
			return true
		}
	}
	return false
}

// mysqlErrorLine is the line with which both clients report an error of the
// server or of the connection, such as
// "ERROR 1146 (42S02) at line 2: Table 'x.t' doesn't exist".
var mysqlErrorLine = regexp.MustCompile(`(?m)^ERROR ([0-9]+) \([0-9A-Za-z]{5}\)`)

// findMySQLClient finds the client program. MariaDB's is called mariadb; a
// newer MariaDB warns when its client is called mysql.
func findMySQLClient(ctx context.Context) (program string, mariaDBClient bool) {
	if program, err := lookPath(ctx, "mariadb"); err == nil {
		return program, true
	}
	if program, err := lookPath(ctx, "mysql"); err == nil {
		return program, false
	}
	return "", false
}

// newMySQLServer finds the client and writes the task's login file when the
// task has connection fields. The returned function removes the login file.
// A missing client is something an earlier node task may still provide. The
// server is first asked by firstCall.
func (run Runtime) newMySQLServer(ctx context.Context, module string, connection mysqlConnection, scope value.Scope, packages PackageManager) (*mysqlServer, func(), *protocol.Result) {
	stop := func(r protocol.Result) (*mysqlServer, func(), *protocol.Result) { return nil, nil, &r }
	program, mariaDBClient := findMySQLClient(ctx)
	if program == "" {
		return stop(run.notYet(fmt.Errorf("%s: neither the program mariadb nor the program mysql is installed; each comes with its database server's package, and a server on another machine needs the client alone. %w",
			module, missingProgramError("mariadb"))))
	}
	server := &mysqlServer{run: run, module: module, program: program, host: connection.Host, where: "the database server on this node"}
	if connection.Host != "" {
		server.where = fmt.Sprintf("the database server at %s, port %d", connection.Host, connection.port())
	}
	login, err := resolveMySQLLogin(scope, module, connection)
	if err != nil {
		return stop(protocol.Failed(err))
	}
	authorityFile := ""
	if connection.CertificateAuthorityFile != "" && !connection.SkipServerCertificateCheck {
		authorityFile = filepath.Clean(connection.CertificateAuthorityFile)
		// The clients name a missing file in different ways, and MariaDB
		// 10.5's prints no readable message at all.
		if info, err := os.Stat(authorityFile); err != nil || !info.Mode().IsRegular() {
			return stop(run.notYet(fmt.Errorf("%s: database_server_certificate_authority_file: %s is not a file on the node; install the certificate of the authority that signed the server's certificate there in an earlier node task", module, authorityFile)))
		}
	}
	cleanup := func() {}
	if connection.Host == "" && connection.LoginUser == "" && connection.LoginPassword == "" {
		server.local = true
		server.options = []string{"--no-defaults", "--user=" + mysqlDefaultUser}
		if socket := mysqlServerSocket(ctx, mariaDBClient); socket != "" {
			server.options = append(server.options, "--socket="+socket)
		}
	} else {
		socket := ""
		if connection.Host == "" {
			socket = mysqlServerSocket(ctx, mariaDBClient)
		}
		text := mysqlLoginFile(login, connection.Host, connection.port(), socket, authorityFile, mariaDBClient, connection.SkipServerCertificateCheck)
		directory, remove, err := run.makePrivateTaskDirectory(module, nil, func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "login.cnf"), []byte(text), 0600)
		})
		if err != nil {
			return stop(protocol.Failed(fmt.Errorf("%s: the private directory for the login file: %w", module, err)))
		}
		cleanup = remove
		server.options = []string{"--defaults-file=" + filepath.Join(directory, "login.cnf")}
	}
	return server, cleanup, nil
}

// firstCall is a task's first call of the client, which finds out whether
// the server answers and takes the login. A server that does not answer
// may still be started by an earlier node task, so a dryrun reports the
// task not checked, and a run fails and is tried again. A refused login,
// and a server whose certificate the client does not accept, are failures
// in a run and in a dryrun, and are never tried again.
func (s *mysqlServer) firstCall(ctx context.Context, statements string) ([][]string, *protocol.Result) {
	rows, failure := s.call(ctx, statements)
	if failure == nil {
		return rows, nil
	}
	r := s.noAnswer(failure)
	return nil, &r
}

// askProduct asks the server which of the two products it is. VERSION() of
// every MariaDB holds the word, such as 11.8.6-MariaDB-0+deb13u1; MySQL's
// is 8.0.46 or 9.7.1.
func (s *mysqlServer) askProduct(ctx context.Context) *protocol.Result {
	rows, stop := s.firstCall(ctx, "SELECT 'product', VERSION();\n")
	if stop != nil {
		return stop
	}
	for _, row := range rows {
		if len(row) == 2 && row[0] == "product" {
			s.version, s.mariaDB = row[1], strings.Contains(row[1], "MariaDB")
			return nil
		}
	}
	r := unchangedFailure(fmt.Errorf("%s: %s did not say which version it is", s.module, s.where))
	return &r
}

// noAnswer is the result of a first call that failed.
func (s *mysqlServer) noAnswer(failure *mysqlFailure) protocol.Result {
	switch {
	case failure.is(mysqlErrorNoConnectionThroughSocket, mysqlErrorNoConnectionOverTCP):
		advice := "start the server in an earlier node task, for example with systemd_service"
		if s.host != "" {
			advice = "check database_server_host and database_server_port, and that the server accepts connections from this node"
		}
		err := fmt.Errorf("%s: %s does not answer: %s; %s", s.module, s.where, failure.Text, advice)
		if s.run.Dryrun {
			return protocol.Result{Status: "not_checked", Change: "unknown", Reason: err.Error()}
		}
		return unchangedFailure(err)
	case failure.is(mysqlErrorLoginRefused, mysqlErrorLoginRefusedForMethod):
		advice := "check login_user and login_password"
		if s.local {
			advice = "the server does not accept the node's root account as root@localhost through its socket; give the task login_user and login_password"
		}
		// The login that the task was given does not work, and another try
		// gets the same answer. A repeated wrong login also has a cost: a
		// MySQL account with FAILED_LOGIN_ATTEMPTS is locked after a few.
		// So the failure is one that the node task execution engine does
		// not try again. A server that does not answer is tried again: a
		// server that starts answers a moment later.
		return notTriedAgain(fmt.Errorf("%s: %s refused the login: %s; %s", s.module, s.where, failure.Text, advice))
	case failure.is(mysqlErrorTLS):
		// Another try does not make the client accept the certificate, or
		// give the server TLS: not tried again, like a refused login.
		return notTriedAgain(fmt.Errorf("%s: the client could not establish a TLS connection to %s: %s; %s", s.module, s.where, failure.Text, databaseServerCertificateAdvice))
	}
	return unchangedFailure(fmt.Errorf("%s: connecting to %s and reading its catalog failed: %s", s.module, s.where, failure.Text))
}

// notTriedAgain is a failure before the task changed anything that another
// try cannot fix: the node task execution engine does not repeat a failure
// of the kind configuration.
func notTriedAgain(err error) protocol.Result {
	r := protocol.ConfigurationError(err)
	r.Change = "unchanged"
	return r
}

// unchangedFailure is a failure before the task changed anything.
func unchangedFailure(err error) protocol.Result {
	r := protocol.Failed(err)
	r.Change = "unchanged"
	return r
}

// call runs the client with statements on its standard input and returns
// the rows that it printed: one line for each row, a tab between columns.
// The statements select HEX() of every value that a person or a server
// chose, and start each row with a fixed word, so the rows need no
// unescaping and say which statement they answer.
func (s *mysqlServer) call(ctx context.Context, statements string) ([][]string, *mysqlFailure) {
	arguments := append([]string{"-i", "PATH=" + mysqlClientPath, s.program}, s.options...)
	arguments = append(arguments, "--batch", "--skip-column-names", "--binary-mode", "--default-character-set=utf8mb4")
	r := runProcess(ctx, nil, "/", strings.NewReader(statements), nil, "env", arguments...).Result
	if r.Status != "succeeded" {
		failure := &mysqlFailure{Text: failureTextFromStandardError(r)}
		if match := mysqlErrorLine.FindStringSubmatch(r.Stderr); match != nil {
			failure.Number, _ = strconv.Atoi(match[1])
		}
		return nil, failure
	}
	if r.StdoutTruncated {
		return nil, &mysqlFailure{Text: "the server's answer is larger than 1 MiB"}
	}
	var rows [][]string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// change runs statements that change the server, and gives each of them
// mysqlLockWaitSeconds to wait for a lock that another session holds.
//
// Without that a statement waits as long as the server's lock_wait_timeout
// says, which is a year on MySQL and a day on MariaDB, and a node task has
// no time limit of its own. Measured on MariaDB 10.5 and 11.8 and on MySQL
// 8.0: while another session has an open transaction that read a table,
// DROP DATABASE of the table's database waits for it. On MySQL the statement
// went on waiting in the server after its client was ended, and dropped the
// database when the other session ended. With the setting the server ends
// the statement with its error 1205 and has changed nothing. That is an
// ordinary failure, which is tried again: the other session may have ended.
func (s *mysqlServer) change(ctx context.Context, statements string) *mysqlFailure {
	_, failure := s.call(ctx, fmt.Sprintf("SET SESSION lock_wait_timeout = %d;\n", mysqlLockWaitSeconds)+statements)
	return failure
}

// failure is a failed call as a task's failure; it starts with the task's
// module, as every failure of these modules does.
func (s *mysqlServer) failure(doing string, failure *mysqlFailure) error {
	if failure.is(mysqlErrorLockWaitTimeout) {
		return fmt.Errorf("%s: %s on %s: %s; another session of the server holds a lock that the statement needs, such as an open transaction on a table, and MMX waits %d seconds for it", s.module, doing, s.where, failure.Text, mysqlLockWaitSeconds)
	}
	return fmt.Errorf("%s: %s on %s: %s", s.module, doing, s.where, failure.Text)
}

// mysqlServerSocket asks my_print_defaults where the server's configuration
// puts the server's socket. It is the one thing that MMX takes from the
// node's option files: the client finds a socket at the place it was built
// with, and a server whose configuration moves the socket is not there. The
// answer is "" when the configuration names no socket or my_print_defaults
// is missing; the client's built-in path then holds.
//
// my_print_defaults prints the options of the named groups in the order in
// which the server reads them, so the last socket line is the server's.
// MariaDB's server also reads the groups mariadb, mariadbd, and
// client-server. The program prints every option of these groups, so
// nothing but the socket's line is read from its output, and nothing of it
// goes into a result.
func mysqlServerSocket(ctx context.Context, mariaDBClient bool) string {
	program, err := lookPath(ctx, "my_print_defaults")
	if err != nil {
		return ""
	}
	groups := []string{"mysqld", "server"}
	if mariaDBClient {
		groups = append(groups, "mariadb", "mariadbd", "client-server")
	}
	r := runProcess(ctx, nil, "/", nil, nil, "env", append([]string{"-i", "PATH=" + mysqlClientPath, program}, groups...)...).Result
	socket := ""
	if r.Status != "succeeded" {
		return socket
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if path, found := strings.CutPrefix(strings.TrimSpace(line), "--socket="); found && filepath.IsAbs(path) {
			socket = path
		}
	}
	return socket
}

// mysqlLogin is a task's login, resolved.
type mysqlLogin struct {
	User, Password string
	// HasPassword: the task gives login_password.
	HasPassword bool
}

// resolveMySQLLogin resolves login_user and login_password. The two are
// logins that MMX hands only to a tool: a secret in them does not hide the
// task's result, and the node task execution engine replaces them in what
// the task reports. The client gets each of them in the login file, in the
// file's quoted form, so the scope learns that form too.
func resolveMySQLLogin(scope value.Scope, module string, connection mysqlConnection) (mysqlLogin, error) {
	login := mysqlLogin{User: mysqlDefaultUser, HasPassword: connection.LoginPassword != ""}
	for _, part := range []struct {
		field, written string
		text           *string
	}{{"login_user", connection.LoginUser, &login.User}, {"login_password", connection.LoginPassword, &login.Password}} {
		if part.written == "" {
			continue
		}
		resolved, err := scope.LoginHandedOnlyToATool(part.written)
		if err == nil {
			*part.text, err = value.Scalar(resolved)
		}
		if err != nil {
			return login, fmt.Errorf("%s.%s: %w", module, part.field, err)
		}
		if value.HasSecrets(resolved) {
			scope.FormOfLoginHandedOnlyToATool(mysqlOptionFileValue(*part.text))
		}
	}
	return login, nil
}

// mysqlOptionFileValue is text as a value of an option file, without the
// double quotes around it: a backslash, a double quote, a line feed, a
// carriage return, and a tab each written with a backslash. Unquoted, the
// clients cut a value at a #, drop spaces at its ends, and read \s and \n
// as a space and a line feed.
func mysqlOptionFileValue(text string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(text)
}

// mysqlLoginFile is the text of the file from which the client reads where
// and as whom it logs in (--defaults-file). It holds the password, so its
// place is the task's private directory. It always names the host and the
// port, or the socket, so that nothing else decides them.
//
// With an authority file the client must check the server's certificate and
// the host name in it. MariaDB's client does that with
// ssl-verify-server-cert and MySQL's with ssl-mode=VERIFY_IDENTITY; each
// client stops at the other's option, so the client's product decides.
// Both then refuse a server without TLS.
func mysqlLoginFile(login mysqlLogin, host string, port int, socket, authorityFile string, mariaDBClient, skipServerCertificateCheck bool) string {
	var text strings.Builder
	line := func(name, value string) { fmt.Fprintf(&text, "%s=\"%s\"\n", name, mysqlOptionFileValue(value)) }
	text.WriteString("[client]\n")
	line("user", login.User)
	if login.HasPassword {
		line("password", login.Password)
	}
	if host == "" {
		if socket != "" {
			line("socket", socket)
		}
		text.WriteString("protocol=socket\n")
		return text.String()
	}
	line("host", host)
	// Without protocol=tcp the clients use the socket for the host
	// localhost.
	fmt.Fprintf(&text, "port=%d\nprotocol=tcp\nconnect-timeout=%d\n", port, mysqlConnectSeconds)
	if authorityFile != "" && !skipServerCertificateCheck {
		line("ssl-ca", authorityFile)
	}
	if mariaDBClient {
		if skipServerCertificateCheck {
			text.WriteString("skip-ssl-verify-server-cert\n")
		} else {
			text.WriteString("ssl-verify-server-cert\n")
		}
	} else if skipServerCertificateCheck {
		text.WriteString("ssl-mode=PREFERRED\n")
	} else {
		text.WriteString("ssl-mode=VERIFY_IDENTITY\n")
	}
	return text.String()
}

// mysqlName is a name as a statement holds it: in backticks, with a
// backtick inside doubled. Both servers read such a name exactly in every
// SQL mode; a name in quotes they read differently with ANSI_QUOTES and
// NO_BACKSLASH_ESCAPES.
func mysqlName(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// mysqlHexadecimal is text as the servers' HEX() prints it. A statement
// compares HEX(column) with it, which is exact on both products: a plain
// comparison of names ignores case on some versions.
func mysqlHexadecimal(text string) string {
	return strings.ToUpper(hex.EncodeToString([]byte(text)))
}

// mysqlFromHexadecimal reads a value that a statement selected with HEX().
func mysqlFromHexadecimal(text string) (string, error) {
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return "", errors.New("the server printed a value that is not hexadecimal")
	}
	return string(decoded), nil
}

// mysqlNameRule is the rule for a name that a statement holds in backticks:
// not empty, and without control characters, because MMX sends a statement
// as one line of text.
func mysqlNameRule(field string) func(any) error {
	return rule(field+" must not be empty and must not hold control characters", func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
	})
}

// mysqlResultValue is the value of a task's result: whether the task
// created the account or the database, and the names of the attributes that
// differed.
func mysqlResultValue(created bool, changedAttributes []string) map[string]any {
	attributes := make([]any, len(changedAttributes))
	for i, name := range changedAttributes {
		attributes[i] = name
	}
	return map[string]any{"created": created, "changed_attributes": attributes}
}
