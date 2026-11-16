package modules

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// A stand-in database server for the tests of mysql_user and mysql_database.
//
// The stand-in programs mariadb, mysql, and my_print_defaults are links to
// this test program. Started under one of these names, it plays the client
// and the server instead of running tests (init, below): it reads the
// server's state from server.json in the fixture's directory, takes the
// statements that MMX sends on standard input, answers as the real products
// do, and saves the state. It knows the statements of mysql.go,
// mysql_user.go, and mysql_database.go, and where MySQL and MariaDB differ
// it differs by the version in its state. What the real products print was
// measured on MariaDB 10.5 and 11.8 and on MySQL 8.0 and 9.7.
//
// A link and no script, because a script costs this test program's many
// calls of the client most of their time on macOS.

// standInMySQL is the stand-in server's state.
type standInMySQL struct {
	// Version is what VERSION() says; a version with the word MariaDB makes
	// the server a MariaDB.
	Version string
	// Variables are the server's settings, such as
	// caching_sha2_password_digest_rounds.
	Variables map[string]string
	// PasswordCheckPlugins is the number of active password check plugins.
	PasswordCheckPlugins int
	// Accounts are the accounts by name@host.
	Accounts  map[string]*standInMySQLAccount
	Databases map[string]standInMySQLDatabase
	// Tables are the existing tables, each as database.table.
	Tables []string
	// The privileges that the server has for each kind of place.
	ServerPrivileges, DatabasePrivileges, TablePrivileges []string
	// DynamicPrivileges are MySQL's privileges for the whole server that
	// carry their own grant option, such as BACKUP_ADMIN. The stand-in has
	// the fault of MySQL 8.0 and 9.7: its catalog
	// (information_schema.USER_PRIVILEGES) keeps showing such a privilege
	// with the option after REVOKE GRANT OPTION ON *.*, also when the
	// privilege itself is revoked after that; only its grant table
	// (mysql.global_grants) is right.
	DynamicPrivileges []string
	// StoresUnderAnotherName: the server stores a granted privilege under
	// another name, as MariaDB stores REPLICATION CLIENT as BINLOG MONITOR.
	StoresUnderAnotherName map[string]string
	// Down: the server does not answer.
	Down bool
	// Socket is the server's socket when its configuration moved it; the
	// client then reaches the server only with --socket.
	Socket string
	// RootNeedsAPassword: the server refuses the node's root through the
	// socket.
	RootNeedsAPassword bool
	// Logins are the passwords of the accounts that log in with a login
	// file, by user name.
	Logins map[string]string
	// Host is the name under which the server is reached over TCP, which
	// its certificate names; AuthorityCertificate is the text of the
	// certificate of the authority that signed it, "" for a server without
	// TLS.
	Host                 string
	AuthorityCertificate string
	// ClientRefusesAServerItCannotCheck: MariaDB's client from 11.8.
	ClientRefusesAServerItCannotCheck bool
	// Fail makes a statement that starts with one of its keys fail with
	// the key's text as the server's error line; FailOnce drops the entry
	// when it has failed once.
	Fail     map[string]string
	FailOnce bool
	// SyntaxErrorFrom makes a statement that holds the word fail with a
	// syntax error that quotes the statement from that word, as the
	// servers do: at most 80 characters.
	SyntaxErrorFrom string
}

type standInMySQLAccount struct {
	Plugin, Stored string
	// AuthOr is the JSON list of a MariaDB account with several methods.
	AuthOr string
	// Privileges are what the account holds, by place: "server",
	// "database\t<pattern>", "table\t<database>\t<table>",
	// "column\t<database>\t<table>\t<column>", and
	// "routine\t<database>\t<name>\t<kind>".
	Privileges map[string]*standInMySQLPrivileges
}

type standInMySQLPrivileges struct {
	Names       []string
	GrantOption bool
	// DynamicWithGrantOption are the dynamic privileges of the whole server
	// that the account holds with their option, and StaleInTheCatalog those
	// that the catalog still shows with the option although the grant table
	// says otherwise.
	DynamicWithGrantOption, StaleInTheCatalog []string
}

type standInMySQLDatabase struct{ CharacterSet, Collation string }

func (s *standInMySQL) mariaDB() bool { return strings.Contains(s.Version, "MariaDB") }

// collations are the collations in the stand-in's catalog, each with its
// character set, as measured: MariaDB 10.5 writes utf8 where the later
// versions and MySQL write utf8mb3; MariaDB 10.11 and later list a uca1400
// collation under a short name, once for each character set that has it;
// utf8mb4_0900_ai_ci is MySQL's.
func (s *standInMySQL) collations() [][2]string {
	utf8 := "utf8mb3"
	if strings.HasPrefix(s.Version, "10.5.") {
		utf8 = "utf8"
	}
	list := [][2]string{
		{"latin1_swedish_ci", "latin1"}, {"latin1_general_ci", "latin1"}, {utf8 + "_general_ci", utf8}, {utf8 + "_unicode_ci", utf8},
		{"utf8mb4_general_ci", "utf8mb4"}, {"utf8mb4_unicode_ci", "utf8mb4"}, {"utf8mb4_bin", "utf8mb4"},
	}
	switch {
	case !s.mariaDB():
		list = append(list, [2]string{"utf8mb4_0900_ai_ci", "utf8mb4"})
	case utf8 == "utf8mb3":
		list = append(list, [2]string{"uca1400_ai_ci", utf8}, [2]string{"uca1400_ai_ci", "utf8mb4"})
	}
	return list
}

// standInCharacterSetName is a name as the stand-in's catalog writes it: in
// lower case, with the server's own name of utf8mb3.
func (s *standInMySQL) characterSetName(written string) string {
	name := strings.ToLower(written)
	for _, pair := range [][2]string{{"utf8", "utf8mb3"}, {"utf8mb3", "utf8"}} {
		if rest, found := strings.CutPrefix(name, pair[0]); found && (rest == "" || rest[0] == '_') && s.collations()[2][1] == pair[1] {
			return pair[1] + rest
		}
	}
	return name
}

// createDatabase is what CREATE DATABASE stores for the words behind the
// name, or the server's error: 1115 for a character set and 1273 for a
// collation that it does not know, 1253 for a collation of another
// character set.
func (s *standInMySQL) createDatabase(words []string) (standInMySQLDatabase, string) {
	characterSet, collation := "", ""
	for i := 0; i+1 < len(words); i++ {
		switch words[i] {
		case "SET":
			characterSet = s.characterSetName(words[i+1])
		case "COLLATE":
			collation = s.characterSetName(words[i+1])
		}
	}
	var ofCharacterSets []string
	short := false
	for _, known := range s.collations() {
		if collation == known[0] || collation == known[1]+"_"+known[0] {
			ofCharacterSets = append(ofCharacterSets, known[1])
			// A short name: the catalog's name does not start with the character set.
			short = !strings.HasPrefix(known[0], known[1]+"_")
		}
	}
	knowsCharacterSet := slices.ContainsFunc(s.collations(), func(known [2]string) bool { return known[1] == characterSet })
	switch {
	case characterSet != "" && !knowsCharacterSet:
		return standInMySQLDatabase{}, "ERROR 1115 (42000): Unknown character set: '" + characterSet + "'"
	case collation != "" && ofCharacterSets == nil:
		return standInMySQLDatabase{}, "ERROR 1273 (HY000): Unknown collation: '" + collation + "'"
	case collation != "" && characterSet != "" && !slices.Contains(ofCharacterSets, characterSet):
		return standInMySQLDatabase{}, "ERROR 1253 (42000): COLLATION '" + collation + "' is not valid for CHARACTER SET '" + characterSet + "'"
	case characterSet == "" && collation == "":
		return standInMySQLDatabase{CharacterSet: "utf8mb4", Collation: "utf8mb4_general_ci"}, ""
	case collation == "":
		return standInMySQLDatabase{CharacterSet: characterSet, Collation: characterSet + "_general_ci"}, ""
	}
	if characterSet == "" {
		// A short name alone gets the server's character set, utf8mb4.
		characterSet = ofCharacterSets[len(ofCharacterSets)-1]
	}
	if short && !strings.HasPrefix(collation, characterSet+"_") {
		collation = characterSet + "_" + collation
	}
	return standInMySQLDatabase{CharacterSet: characterSet, Collation: collation}, ""
}

// mysqlFixture is a node with the stand-in server and one client program.
type mysqlFixture struct {
	t   *testing.T
	dir string
	// privateTaskDirectoryRoot is where a task's private directory goes.
	privateTaskDirectoryRoot string
}

const (
	standInMariaDBVersion = "11.8.6-MariaDB-0+deb13u1 from Debian"
	standInMySQLVersion   = "9.7.1"
)

// newMySQLFixture makes a node whose only programs are the stand-ins: the
// client of the server's product, and my_print_defaults. PATH holds nothing
// else, so a real client on the test machine is never found.
func newMySQLFixture(t *testing.T, version string) *mysqlFixture {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := "mysql"
	if strings.Contains(version, "MariaDB") {
		client = "mariadb"
	}
	env, err := lookPathOutsideTheFixture("env")
	if err != nil {
		t.Fatal(err)
	}
	f := &mysqlFixture{t: t, dir: t.TempDir()}
	for name, target := range map[string]string{client: program, "my_print_defaults": program, "env": env} {
		if err := os.Symlink(target, filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	setWorkerEnvironment(t, "PATH", f.dir)
	f.privateTaskDirectoryRoot = filepath.Join(f.dir, "private_task_directories")
	if err := os.Mkdir(f.privateTaskDirectoryRoot, 0711); err != nil {
		t.Fatal(err)
	}
	f.save(&standInMySQL{
		Version: version, Variables: map[string]string{}, Accounts: map[string]*standInMySQLAccount{}, Databases: map[string]standInMySQLDatabase{},
		ServerPrivileges:   []string{"ALTER", "CREATE", "CREATE TEMPORARY TABLES", "CREATE USER", "DELETE", "DROP", "EXECUTE", "INDEX", "INSERT", "PROCESS", "REFERENCES", "RELOAD", "SELECT", "SHUTDOWN", "SUPER", "UPDATE"},
		DatabasePrivileges: []string{"ALTER", "CREATE", "CREATE TEMPORARY TABLES", "DELETE", "DROP", "EXECUTE", "INDEX", "INSERT", "REFERENCES", "SELECT", "UPDATE"},
		TablePrivileges:    []string{"ALTER", "CREATE", "DELETE", "DROP", "INDEX", "INSERT", "REFERENCES", "SELECT", "UPDATE"},
	})
	return f
}

// lookPathOutsideTheFixture finds a program of the test machine.
func lookPathOutsideTheFixture(name string) (string, error) {
	for _, directory := range []string{"/usr/bin", "/bin"} {
		if info, err := os.Stat(filepath.Join(directory, name)); err == nil && !info.IsDir() {
			return filepath.Join(directory, name), nil
		}
	}
	return "", fmt.Errorf("the test machine has no program %s in /usr/bin or /bin", name)
}

func (f *mysqlFixture) state() *standInMySQL {
	f.t.Helper()
	state, err := readStandInMySQL(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}

func (f *mysqlFixture) save(state *standInMySQL) {
	f.t.Helper()
	if err := writeStandInMySQL(f.dir, state); err != nil {
		f.t.Fatal(err)
	}
}

// change changes the stand-in server's state.
func (f *mysqlFixture) change(change func(*standInMySQL)) {
	f.t.Helper()
	state := f.state()
	change(state)
	f.save(state)
}

// account returns one account of the stand-in server, nil when it has none
// of that name.
func (f *mysqlFixture) account(name string) *standInMySQLAccount { return f.state().Accounts[name] }

// held returns the names that an account holds at a place, sorted, with
// "GRANT OPTION" for the place's grant option.
func (f *mysqlFixture) held(account, place string) []string {
	f.t.Helper()
	a := f.account(account)
	if a == nil || a.Privileges[place] == nil {
		return nil
	}
	names := slices.Clone(a.Privileges[place].Names)
	if a.Privileges[place].GrantOption {
		names = append(names, "GRANT OPTION")
	}
	slices.Sort(names)
	return names
}

// statements returns the statements that changed the server since the last
// time, and forgets them. Statements that only read are not kept.
func (f *mysqlFixture) statements() []string {
	f.t.Helper()
	text := strings.TrimSpace(readState(f.t, filepath.Join(f.dir, "statements")))
	writeState(f.t, filepath.Join(f.dir, "statements"), "")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// run and dryrun are a run's and a dryrun's runtime on the fixture's node.
func (f *mysqlFixture) run() Runtime {
	return withPrivateTaskDirectoryRoot(Runtime{}, f.privateTaskDirectoryRoot)
}

func (f *mysqlFixture) dryrun() Runtime {
	return withPrivateTaskDirectoryRoot(noMutation(f.t), f.privateTaskDirectoryRoot)
}

// noPrivateTaskDirectoryLeft fails the test when a task left its login file
// behind.
func (f *mysqlFixture) noPrivateTaskDirectoryLeft() {
	f.t.Helper()
	if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
		f.t.Fatalf("a private task directory is left: %v", left)
	}
}

// nowhereOutsideTheLoginFile fails the test when the text is on a command
// line or in an environment of a stand-in program.
func (f *mysqlFixture) nowhereOutsideTheLoginFile(text string) {
	f.t.Helper()
	for _, name := range []string{"calls", "environment"} {
		if strings.Contains(readState(f.t, filepath.Join(f.dir, name)), text) {
			f.t.Fatalf("%q is in the stand-in's %s", text, name)
		}
	}
	f.noPrivateTaskDirectoryLeft()
}

// mysqlResult checks a result's status and change and returns its value's
// two fields.
func mysqlResult(t *testing.T, r protocol.Result, status, change string) (created bool, changedAttributes []string) {
	t.Helper()
	if r.Status != status || r.Change != change {
		t.Fatalf("status %s and change %s, want %s and %s: %+v", r.Status, r.Change, status, change, r)
	}
	v, _ := r.Value.(map[string]any)
	if v == nil {
		return false, nil
	}
	for _, name := range v["changed_attributes"].([]any) {
		changedAttributes = append(changedAttributes, name.(string))
	}
	return v["created"].(bool), changedAttributes
}

func readStandInMySQL(dir string) (*standInMySQL, error) {
	text, err := os.ReadFile(filepath.Join(dir, "server.json"))
	if err != nil {
		return nil, err
	}
	state := &standInMySQL{}
	return state, json.Unmarshal(text, state)
}

func writeStandInMySQL(dir string, state *standInMySQL) error {
	text, err := json.MarshalIndent(state, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "server.json"), text, 0600)
}

// init makes this test program the stand-in when it is started under the
// name of one of the stand-in programs, in a fixture's directory. Like the
// stand-ins of the other tests, it notes each call and its environment in
// the files calls and environment.
func init() {
	name, dir := filepath.Base(os.Args[0]), filepath.Dir(os.Args[0])
	if name != "mariadb" && name != "mysql" && name != "my_print_defaults" {
		return
	}
	note := func(file, text string) {
		notes, err := os.OpenFile(filepath.Join(dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintln(notes, text)
			notes.Close()
		}
	}
	note("calls", strings.Join(append([]string{name}, os.Args[1:]...), " "))
	note("environment", strings.Join(os.Environ(), "\n"))
	if name == "my_print_defaults" {
		// What the server's configuration says, when a test wrote it.
		text, _ := os.ReadFile(filepath.Join(dir, "my-print-defaults"))
		os.Stdout.Write(text)
		exitStandIn(0)
	}
	input, _ := io.ReadAll(os.Stdin)
	client := &standInMySQLClient{dir: dir, name: name}
	exitStandIn(client.serve(os.Args[1:], string(input)))
}

type standInMySQLClient struct {
	dir, name string
	state     *standInMySQL
	out       strings.Builder
	// lockWaitSeconds is what the call set its session's lock_wait_timeout
	// to, "" while it did not.
	lockWaitSeconds string
}

// refuse prints an error as the client does and returns the client's exit
// code.
func (c *standInMySQLClient) refuse(code int, format string, arguments ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	return code
}

// serve is one call of the client.
func (c *standInMySQLClient) serve(options []string, input string) int {
	var err error
	if c.state, err = readStandInMySQL(c.dir); err != nil {
		return c.refuse(70, "stand-in: %v", err)
	}
	// The option that says which files the client reads must come first.
	if len(options) == 0 || (options[0] != "--no-defaults" && !strings.HasPrefix(options[0], "--defaults-file=")) {
		return c.refuse(64, "stand-in: the first option must be --no-defaults or --defaults-file, got %v", options)
	}
	for _, needed := range []string{"--batch", "--skip-column-names", "--binary-mode", "--default-character-set=utf8mb4"} {
		if !slices.Contains(options, needed) {
			return c.refuse(64, "stand-in: the client was called without %s", needed)
		}
	}
	if code := c.connect(options); code != 0 {
		return code
	}
	for number, statement := range strings.Split(strings.TrimSpace(input), ";\n") {
		statement = strings.TrimSuffix(strings.TrimSpace(statement), ";")
		if statement == "" {
			continue
		}
		if failure := c.execute(statement); failure != "" {
			// A statement that failed leaves what the earlier ones did.
			if err := writeStandInMySQL(c.dir, c.state); err != nil {
				return c.refuse(70, "stand-in: %v", err)
			}
			fmt.Print(c.out.String())
			// MariaDB's client prints the statement that failed.
			if c.name == "mariadb" {
				fmt.Fprintf(os.Stderr, "--------------\n%s\n--------------\n\n", statement)
			}
			code, text, _ := strings.Cut(failure, ": ")
			return c.refuse(1, "%s at line %d: %s", code, number+1, text)
		}
	}
	if err := writeStandInMySQL(c.dir, c.state); err != nil {
		return c.refuse(70, "stand-in: %v", err)
	}
	fmt.Print(c.out.String())
	return 0
}

// connect checks where and as whom the client logs in.
func (c *standInMySQLClient) connect(options []string) int {
	socket := ""
	for _, option := range options {
		if path, found := strings.CutPrefix(option, "--socket="); found {
			socket = path
		}
	}
	file, withLoginFile := strings.CutPrefix(options[0], "--defaults-file=")
	if !withLoginFile {
		if c.state.Down || socket != c.state.Socket {
			return c.refuse(1, "ERROR 2002 (HY000): Can't connect to local server through socket '%s' (2)", socket)
		}
		if c.state.RootNeedsAPassword {
			return c.refuse(1, "ERROR 1698 (28000): Access denied for user 'root'@'localhost'")
		}
		return 0
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return c.refuse(1, "Could not open required defaults file: %s", file)
	}
	info, _ := os.Stat(file)
	directory, _ := os.Stat(filepath.Dir(file))
	// What the login file was, for the tests.
	_ = os.WriteFile(filepath.Join(c.dir, "login-file"), text, 0600)
	_ = os.WriteFile(filepath.Join(c.dir, "login-file-modes"), fmt.Appendf(nil, "%04o %04o", info.Mode().Perm(), directory.Mode().Perm()), 0600)
	login := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(text)), "\n")[1:] {
		name, written, _ := strings.Cut(line, "=")
		if quoted, found := strings.CutPrefix(written, `"`); found {
			written = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n", `\r`, "\r", `\t`, "\t").Replace(strings.TrimSuffix(quoted, `"`))
		}
		// Each client stops at an option that only the other one knows.
		if (c.name == "mysql" && name == "ssl-verify-server-cert") || (c.name == "mariadb" && name == "ssl-mode") {
			return c.refuse(7, "%s: unknown variable '%s'", c.name, line)
		}
		login[name] = written
	}
	unreachable := 2003
	if c.name == "mariadb" {
		unreachable = 2002
	}
	switch {
	case login["host"] == "" && (c.state.Down || login["socket"] != c.state.Socket):
		return c.refuse(1, "ERROR 2002 (HY000): Can't connect to local server through socket '%s' (2)", login["socket"])
	case login["host"] != "" && (c.state.Down || login["host"] != c.state.Host || login["protocol"] != "tcp" || login["connect-timeout"] != "10"):
		return c.refuse(1, "ERROR %d (HY000): Can't connect to MySQL server on '%s:%s' (111)", unreachable, login["host"], login["port"])
	}
	if login["host"] != "" {
		_, checksName := login["ssl-verify-server-cert"]
		checksName = checksName || login["ssl-mode"] == "VERIFY_IDENTITY"
		switch authority, given := login["ssl-ca"]; {
		case !given && checksName && (c.name == "mysql" || c.state.ClientRefusesAServerItCannotCheck):
			return c.refuse(1, "ERROR 2026 (HY000): TLS/SSL error: self-signed certificate in certificate chain")
		case given && c.state.AuthorityCertificate == "":
			return c.refuse(1, "ERROR 2026 (HY000): TLS/SSL error: SSL is required, but the server does not support it")
		case given:
			certificate, _ := os.ReadFile(authority)
			if string(certificate) != c.state.AuthorityCertificate || !checksName {
				return c.refuse(1, "ERROR 2026 (HY000): TLS/SSL error: self-signed certificate in certificate chain")
			}
		}
	}
	if password, known := c.state.Logins[login["user"]]; !known || password != login["password"] {
		return c.refuse(1, "ERROR 1045 (28000): Access denied for user '%s'@'localhost' (using password: YES)", login["user"])
	}
	return 0
}

// row prints one row.
func (c *standInMySQLClient) row(columns ...any) {
	for i, column := range columns {
		if i > 0 {
			c.out.WriteByte('\t')
		}
		fmt.Fprint(&c.out, column)
	}
	c.out.WriteByte('\n')
}

var (
	standInHexadecimalValue = regexp.MustCompile(`HEX\((\w+)\) = '([0-9A-F]*)'`)
	standInFirstWord        = regexp.MustCompile(`^SELECT '([^']*)'`)
	standInVariable         = regexp.MustCompile(`^SHOW GLOBAL VARIABLES LIKE '(\w+)'$`)
)

// standInListed returns the values of the list that a statement compares
// HEX(of) with, nil when the statement has no such comparison.
func standInListed(statement, of string) []string {
	var listed []string
	list := regexp.MustCompile(regexp.QuoteMeta("HEX("+of+") IN (") + `('[0-9A-F]*'(?:, '[0-9A-F]*')*)\)`)
	if match := list.FindStringSubmatch(statement); match != nil {
		for _, value := range strings.Split(match[1], ", ") {
			decoded, _ := hex.DecodeString(strings.Trim(value, "'"))
			listed = append(listed, string(decoded))
		}
	}
	return listed
}

// compared returns the values that a statement compares HEX(column) with,
// by column.
func standInCompared(statement string) map[string]string {
	compared := map[string]string{}
	for _, match := range standInHexadecimalValue.FindAllStringSubmatch(statement, -1) {
		decoded, _ := hex.DecodeString(match[2])
		compared[match[1]] = string(decoded)
	}
	return compared
}

func standInHexadecimal(text string) string { return strings.ToUpper(hex.EncodeToString([]byte(text))) }

// titleCase writes a privilege as SHOW PRIVILEGES and the type of
// mysql.tables_priv do: Create temporary tables.
func standInTitleCase(name string) string {
	return name[:1] + strings.ToLower(name[1:])
}

// execute runs one statement and returns "" or the server's error, as
// "ERROR <number> (<SQLSTATE>): <text>".
func (c *standInMySQLClient) execute(statement string) string {
	s := c.state
	for prefix, failure := range s.Fail {
		if strings.HasPrefix(statement, prefix) {
			if s.FailOnce {
				delete(s.Fail, prefix)
			}
			return failure
		}
	}
	compared := standInCompared(statement)
	// The account that a statement's grantee or its User and Host name.
	reads := func() *standInMySQLAccount {
		if grantee, found := compared["GRANTEE"]; found {
			name, host, _ := strings.Cut(strings.Trim(grantee, "'"), "'@'")
			return s.Accounts[name+"@"+host]
		}
		// mysql.global_grants writes its columns USER and HOST.
		if user, found := compared["USER"]; found {
			return s.Accounts[user+"@"+compared["HOST"]]
		}
		return s.Accounts[compared["User"]+"@"+compared["Host"]]
	}
	word := ""
	if match := standInFirstWord.FindStringSubmatch(statement); match != nil {
		word = match[1]
	}
	// Every place of the account of one kind, in a fixed order.
	places := func(kind string) [][]string {
		var found [][]string
		if account := reads(); account != nil {
			for _, key := range slices.Sorted(mapKeys(account.Privileges)) {
				if parts := strings.Split(key, "\t"); parts[0] == kind {
					found = append(found, parts)
				}
			}
		}
		return found
	}
	grantable := func(held *standInMySQLPrivileges) string {
		if held.GrantOption {
			return "YES"
		}
		return "NO"
	}
	// rows prints one row for each privilege of a place; a place with only
	// its grant option has the row USAGE.
	rows := func(kind string, columns func(parts []string) []any) {
		for _, parts := range places(kind) {
			held := reads().Privileges[strings.Join(parts, "\t")]
			names := held.Names
			if len(names) == 0 {
				names = []string{"USAGE"}
			}
			for _, name := range names {
				c.row(append(append([]any{word}, columns(parts)...), name, grantable(held))...)
			}
		}
	}
	switch {
	case strings.Contains(statement, "VERSION()"):
		c.row(word, s.Version)
	case strings.HasPrefix(statement, "SET SESSION lock_wait_timeout = "):
		c.lockWaitSeconds = strings.TrimPrefix(statement, "SET SESSION lock_wait_timeout = ")
	case strings.Contains(statement, "information_schema.SCHEMATA"):
		if database, found := s.Databases[compared["SCHEMA_NAME"]]; found {
			c.row(word, database.CharacterSet, database.Collation)
		}
	case strings.Contains(statement, "FROM information_schema.CHARACTER_SETS"):
		listed, printed := standInListed(statement, "CHARACTER_SET_NAME"), map[string]bool{}
		for _, known := range s.collations() {
			if slices.Contains(listed, known[1]) && !printed[known[1]] {
				c.row(word, known[1])
				printed[known[1]] = true
			}
		}
	case strings.Contains(statement, "FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY"):
		names, withCharacterSet := standInListed(statement, "COLLATION_NAME"), standInListed(statement, "CONCAT(CHARACTER_SET_NAME, '_', COLLATION_NAME)")
		for _, known := range s.collations() {
			if slices.Contains(names, known[0]) || slices.Contains(withCharacterSet, known[1]+"_"+known[0]) {
				c.row(word, known[0], known[1])
			}
		}
	case strings.Contains(statement, "FROM mysql.global_priv"):
		if !s.mariaDB() {
			return "ERROR 1146 (42S02): Table 'mysql.global_priv' doesn't exist"
		}
		if account := reads(); account != nil {
			c.row(word, account.Plugin, standInHexadecimal(account.Stored), standInHexadecimal(account.AuthOr))
		}
	case strings.Contains(statement, "FROM mysql.user"):
		if account := reads(); account != nil {
			c.row(word, account.Plugin, standInHexadecimal(account.Stored), "")
		}
	case standInVariable.MatchString(statement):
		name := standInVariable.FindStringSubmatch(statement)[1]
		if setting, found := s.Variables[name]; found {
			c.row(name, setting)
		}
	case strings.Contains(statement, "information_schema.PLUGINS"):
		c.row(word, s.PasswordCheckPlugins)
	case strings.HasSuffix(statement, "FROM mysql.global_grants GROUP BY PRIV"):
		// The names of the dynamic privileges that some account holds; the
		// stand-in's root holds them all.
		for _, name := range s.DynamicPrivileges {
			c.row(word, name)
		}
	case strings.Contains(statement, "FROM mysql.global_grants WHERE HEX(USER)"):
		if s.mariaDB() {
			return "ERROR 1146 (42S02): Table 'mysql.global_grants' doesn't exist"
		}
		// The grant table: each dynamic privilege with its own option.
		if account := reads(); account != nil && account.Privileges["server"] != nil {
			held := account.Privileges["server"]
			for _, name := range held.Names {
				if slices.Contains(s.DynamicPrivileges, name) {
					option := "N"
					if slices.Contains(held.DynamicWithGrantOption, name) {
						option = "Y"
					}
					c.row(word, name, option)
				}
			}
		}
	case strings.Contains(statement, "information_schema.USER_PRIVILEGES"):
		// With the words NOT IN (SELECT PRIV FROM mysql.global_grants) the
		// catalog's rows for dynamic privileges are left out. Without them
		// the catalog shows what MySQL's shows: also the stale rows.
		withoutDynamic := strings.Contains(statement, "NOT IN (SELECT PRIV FROM mysql.global_grants)")
		account := reads()
		if account == nil {
			break
		}
		held := account.Privileges["server"]
		if held == nil {
			held = &standInMySQLPrivileges{}
		}
		static := 0
		for _, name := range held.Names {
			dynamic := slices.Contains(s.DynamicPrivileges, name)
			switch {
			case dynamic && withoutDynamic:
			case dynamic:
				option := "NO"
				if slices.Contains(held.DynamicWithGrantOption, name) {
					option = "YES"
				}
				c.row(word, name, option)
			default:
				static++
				c.row(word, name, grantable(held))
			}
		}
		if static == 0 {
			c.row(word, "USAGE", grantable(held))
		}
		for _, name := range held.StaleInTheCatalog {
			if !withoutDynamic {
				c.row(word, name, "YES")
			}
		}
	case strings.Contains(statement, "information_schema.SCHEMA_PRIVILEGES"):
		rows("database", func(parts []string) []any { return []any{standInHexadecimal(parts[1])} })
	case strings.Contains(statement, "information_schema.TABLE_PRIVILEGES"):
		rows("table", func(parts []string) []any { return []any{standInHexadecimal(parts[1]), standInHexadecimal(parts[2])} })
	case strings.Contains(statement, "information_schema.COLUMN_PRIVILEGES"):
		// A column's row says whether its table has the grant option.
		for _, parts := range places("column") {
			table := reads().Privileges["table\t"+parts[1]+"\t"+parts[2]]
			for _, name := range reads().Privileges[strings.Join(parts, "\t")].Names {
				option := "NO"
				if table != nil && table.GrantOption {
					option = "YES"
				}
				c.row(word, standInHexadecimal(parts[1]), standInHexadecimal(parts[2]), standInHexadecimal(parts[3]), name, option)
			}
		}
	case strings.Contains(statement, "mysql.procs_priv"):
		for _, parts := range places("routine") {
			held := reads().Privileges[strings.Join(parts, "\t")]
			var members []string
			// The set's members are written as Execute and Alter Routine.
			for _, name := range held.Names {
				words := strings.Fields(name)
				for i, word := range words {
					words[i] = standInTitleCase(word)
				}
				members = append(members, strings.Join(words, " "))
			}
			if held.GrantOption {
				members = append(members, "Grant")
			}
			c.row(word, standInHexadecimal(parts[1]), standInHexadecimal(parts[2]), parts[3], strings.Join(members, ","))
		}
	case strings.Contains(statement, "TABLE_NAME = 'db'"):
		c.row(word, len(s.DatabasePrivileges))
	case strings.Contains(statement, "COLUMN_NAME = 'Table_priv'"):
		members := []string{"'Grant'"}
		for _, name := range s.TablePrivileges {
			members = append(members, "'"+standInTitleCase(name)+"'")
		}
		c.row(word, "set("+strings.Join(members, ",")+")")
	case strings.Contains(statement, "information_schema.TABLES"):
		if slices.Contains(s.Tables, compared["TABLE_SCHEMA"]+"."+compared["TABLE_NAME"]) {
			c.row(word, standInHexadecimal(compared["TABLE_SCHEMA"]), standInHexadecimal(compared["TABLE_NAME"]))
		}
	case statement == "SHOW PRIVILEGES":
		for _, name := range append(slices.Clone(s.ServerPrivileges), "GRANT OPTION", "PROXY", "USAGE") {
			c.row(standInTitleCase(name), "Server Admin", "To do what the name says")
		}
		for _, name := range s.DynamicPrivileges {
			c.row(name, "Server Admin", "")
		}
	case word != "" && statement == "SELECT '"+word+"'":
		c.row(word)
	default:
		return c.change(statement)
	}
	return ""
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// standInName reads a name in backticks from the start of text and returns
// it with the rest of the text.
func standInName(text string) (name, rest string) {
	if !strings.HasPrefix(text, "`") {
		return "", text
	}
	for i := 1; i < len(text); i++ {
		if text[i] != '`' {
			name += text[i : i+1]
		} else if i+1 < len(text) && text[i+1] == '`' {
			name += "`"
			i++
		} else {
			return name, text[i+1:]
		}
	}
	return "", text
}

// standInAccountName reads `name`@`host` and returns name@host.
func standInAccountName(text string) (account, rest string) {
	name, rest := standInName(text)
	host, rest := standInName(strings.TrimPrefix(rest, "@"))
	return name + "@" + host, rest
}

var standInIdentified = regexp.MustCompile(`^ IDENTIFIED (VIA|WITH) (\w+) (USING|AS) '([^']*)'$`)

// change runs a statement that changes the server, and notes it for the
// tests.
func (c *standInMySQLClient) change(statement string) string {
	s := c.state
	notes, _ := os.OpenFile(filepath.Join(c.dir, "statements"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	fmt.Fprintln(notes, statement)
	notes.Close()
	syntaxError := func(from string) string {
		return "ERROR 1064 (42000): You have an error in your SQL syntax; check the manual that corresponds to your server version for the right syntax to use near '" + from + "'"
	}
	if s.SyntaxErrorFrom != "" && strings.Contains(statement, s.SyntaxErrorFrom) {
		quoted := statement[strings.Index(statement, s.SyntaxErrorFrom):]
		if len(quoted) > 80 {
			quoted = quoted[:80]
			// MariaDB puts three dots behind what it cut.
			if s.mariaDB() {
				quoted = quoted[:77] + "..."
			}
		}
		return syntaxError(quoted)
	}
	// A real server lets the statement wait for another session's lock for
	// as long as its lock_wait_timeout says: a day, or a year.
	if c.lockWaitSeconds != "10" {
		return "ERROR 9999 (HY000): stand-in: a statement that changes the server came without SET SESSION lock_wait_timeout = 10 before it in its call"
	}
	verb, rest, _ := strings.Cut(statement, " ")
	object, rest, _ := strings.Cut(rest, " ")
	switch verb + " " + object {
	case "CREATE DATABASE":
		name, rest := standInName(rest)
		if _, exists := s.Databases[name]; exists {
			return "ERROR 1007 (HY000): Can't create database '" + name + "'; database exists"
		}
		database, failure := s.createDatabase(strings.Fields(rest))
		if failure != "" {
			return failure
		}
		s.Databases[name] = database
	case "DROP DATABASE":
		name, _ := standInName(rest)
		if _, exists := s.Databases[name]; !exists {
			return "ERROR 1008 (HY000): Can't drop database '" + name + "'; database doesn't exist"
		}
		delete(s.Databases, name)
	case "CREATE USER", "ALTER USER":
		name, rest := standInAccountName(rest)
		account := s.Accounts[name]
		if (account != nil) == (verb == "CREATE") {
			return "ERROR 1396 (HY000): Operation " + verb + " USER failed for " + name
		}
		if account == nil {
			account = &standInMySQLAccount{Plugin: mysqlCachingSHA2Method, Privileges: map[string]*standInMySQLPrivileges{}}
			if s.mariaDB() {
				account.Plugin = mysqlNativePasswordMethod
			}
		}
		if rest != "" {
			identified := standInIdentified.FindStringSubmatch(rest)
			switch {
			case identified == nil, identified[1] == "VIA" && !s.mariaDB():
				// MySQL does not take MariaDB's words.
				return syntaxError(strings.TrimPrefix(rest, " IDENTIFIED "))
			case s.mariaDB() && s.PasswordCheckPlugins > 0 && s.Variables["strict_password_validation"] == "ON":
				return "ERROR 1290 (HY000): The MariaDB server is running with the --strict-password-validation option so it cannot execute this statement"
			}
			account.Plugin, account.Stored = identified[2], identified[4]
		}
		s.Accounts[name] = account
	case "DROP USER":
		name, _ := standInAccountName(rest)
		if s.Accounts[name] == nil {
			return "ERROR 1396 (HY000): Operation DROP USER failed for " + name
		}
		delete(s.Accounts, name)
	default:
		if verb != "GRANT" && verb != "REVOKE" {
			return syntaxError(statement)
		}
		return c.grantOrRevoke(verb, strings.TrimPrefix(statement, verb+" "))
	}
	return ""
}

var standInColumnPrivilege = regexp.MustCompile("^([A-Z ]+) \\(`([^`]+)`\\)$")

// grantOrRevoke runs "<what> ON <place> TO|FROM <account>".
func (c *standInMySQLClient) grantOrRevoke(verb, text string) string {
	s := c.state
	what, text, _ := strings.Cut(text, " ON ")
	separator := " TO "
	if verb == "REVOKE" {
		separator = " FROM "
	}
	at := strings.LastIndex(text, separator)
	on := text[:at]
	name, _ := standInAccountName(text[at+len(separator):])
	account := s.Accounts[name]
	if account == nil {
		return "ERROR 1133 (28000): Can't find any matching row in the user table"
	}
	// The place, and the privileges that the server has for it.
	var key string
	var known []string
	switch {
	case on == "*.*":
		key, known = "server", append(slices.Clone(s.ServerPrivileges), s.DynamicPrivileges...)
	case strings.HasPrefix(on, "PROCEDURE "), strings.HasPrefix(on, "FUNCTION "):
		kind, rest, _ := strings.Cut(on, " ")
		database, rest := standInName(rest)
		routine, _ := standInName(strings.TrimPrefix(rest, "."))
		key, known = "routine\t"+database+"\t"+routine+"\t"+kind, []string{"EXECUTE", "ALTER ROUTINE"}
	default:
		database, rest := standInName(on)
		if rest == ".*" {
			key, known = "database\t"+database, s.DatabasePrivileges
			break
		}
		table, _ := standInName(strings.TrimPrefix(rest, "."))
		key, known = "table\t"+database+"\t"+table, s.TablePrivileges
		if verb == "GRANT" && !slices.Contains(s.Tables, database+"."+table) && what != "ALL PRIVILEGES" && !slices.Contains(strings.Split(what, ", "), "CREATE") {
			return "ERROR 1146 (42S02): Table '" + database + "." + table + "' doesn't exist"
		}
	}
	if account.Privileges == nil {
		account.Privileges = map[string]*standInMySQLPrivileges{}
	}
	held := func(key string) *standInMySQLPrivileges {
		if account.Privileges[key] == nil {
			if verb == "REVOKE" {
				return nil
			}
			account.Privileges[key] = &standInMySQLPrivileges{}
		}
		return account.Privileges[key]
	}
	noSuchGrant := "ERROR 1141 (42000): There is no such grant defined for user '" + name + "'"
	names := strings.Split(what, ", ")
	switch what {
	case "GRANT OPTION":
		place := held(key)
		if place == nil {
			return noSuchGrant
		}
		place.GrantOption = verb == "GRANT"
		// For MySQL's whole server the statement also sets or clears the
		// option of every dynamic privilege that the account holds; after a
		// REVOKE the catalog keeps showing them with the option.
		var dynamic []string
		for _, name := range place.Names {
			if key == "server" && slices.Contains(s.DynamicPrivileges, name) {
				dynamic = append(dynamic, name)
			}
		}
		if verb == "GRANT" {
			place.DynamicWithGrantOption, place.StaleInTheCatalog = dynamic, nil
		} else {
			place.StaleInTheCatalog, place.DynamicWithGrantOption = place.DynamicWithGrantOption, nil
		}
		names = nil
	case "ALL PRIVILEGES":
		names = known
	}
	for _, name := range names {
		target := key
		if column := standInColumnPrivilege.FindStringSubmatch(name); column != nil {
			name, target = column[1], strings.Replace(key, "table\t", "column\t", 1)+"\t"+column[2]
		}
		if !slices.Contains(known, name) && s.StoresUnderAnotherName[name] == "" {
			if s.mariaDB() {
				return "ERROR 1064 (42000): You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'ON " + on + "'"
			}
			return "ERROR 3619 (HY000): Illegal privilege level specified for " + name
		}
		if stored := s.StoresUnderAnotherName[name]; stored != "" {
			name = stored
		}
		place := held(target)
		if place == nil {
			return noSuchGrant
		}
		switch has := slices.Contains(place.Names, name); {
		case verb == "GRANT" && !has:
			place.Names = append(place.Names, name)
			slices.Sort(place.Names)
		case verb == "REVOKE":
			place.Names = slices.DeleteFunc(place.Names, func(held string) bool { return held == name })
		}
	}
	// A place where the account holds nothing is gone.
	for key, place := range account.Privileges {
		if len(place.Names) == 0 && !place.GrantOption && len(place.StaleInTheCatalog) == 0 {
			delete(account.Privileges, key)
		}
	}
	return ""
}

// The stand-in itself: what the module sends is what the stand-in reads.
func TestMySQLNamesAndOptionFileValues(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"shop": "`shop`", "a`b": "`a``b`", "shop-db": "`shop-db`", `back\slash`: "`back\\slash`", "it's;--": "`it's;--`"} {
		if got := mysqlName(name); got != want {
			t.Errorf("mysqlName(%q) = %s, want %s", name, got, want)
		}
		if read, rest := standInName(mysqlName(name) + ".*"); read != name || rest != ".*" {
			t.Errorf("the stand-in reads %s as %q and %q", mysqlName(name), read, rest)
		}
	}
	if got := mysqlHexadecimal("shop"); got != "73686F70" {
		t.Fatal(got)
	}
	if got, err := mysqlFromHexadecimal("73686F705C5F6462"); err != nil || got != `shop\_db` {
		t.Fatal(got, err)
	}
	if _, err := mysqlFromHexadecimal("NULL"); err == nil {
		t.Fatal("NULL counts as a hexadecimal value")
	}
	// The values of the login file, as the clients read them inside double
	// quotes.
	for text, want := range map[string]string{`pa"ss\word`: `pa\"ss\\word`, " spaces #x ": " spaces #x ", "tab\there": `tab\there`, "line\r\nfeed": `line\r\nfeed`, `\n`: `\\n`, "pä-日本": "pä-日本"} {
		if got := mysqlOptionFileValue(text); got != want {
			t.Errorf("mysqlOptionFileValue(%q) = %s, want %s", text, got, want)
		}
	}
	login := mysqlLogin{User: "shop-admin", Password: `pa"ss\word`, HasPassword: true}
	if got, want := mysqlLoginFile(login, "db.example.com", 3307, "", "/etc/ssl/database authority.pem", true, false),
		"[client]\nuser=\"shop-admin\"\npassword=\"pa\\\"ss\\\\word\"\nhost=\"db.example.com\"\nport=3307\nprotocol=tcp\nconnect-timeout=10\nssl-ca=\"/etc/ssl/database authority.pem\"\nssl-verify-server-cert\n"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if got, want := mysqlLoginFile(login, "db.example.com", mysqlConnection{}.port(), "", "/etc/ssl/authority.pem", false, false),
		"[client]\nuser=\"shop-admin\"\npassword=\"pa\\\"ss\\\\word\"\nhost=\"db.example.com\"\nport=3306\nprotocol=tcp\nconnect-timeout=10\nssl-ca=\"/etc/ssl/authority.pem\"\nssl-mode=VERIFY_IDENTITY\n"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	// A login through the socket, with and without a password.
	if got, want := mysqlLoginFile(mysqlLogin{User: "root"}, "", 0, "/run/mysqld/moved.sock", "", true, false), "[client]\nuser=\"root\"\nsocket=\"/run/mysqld/moved.sock\"\nprotocol=socket\n"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if got, want := mysqlLoginFile(mysqlLogin{User: "root", HasPassword: true}, "", 0, "", "", false, false), "[client]\nuser=\"root\"\npassword=\"\"\nprotocol=socket\n"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// The connection fields' checks when the project loads, which both modules
// share.
func TestMySQLConnectionFieldsAreChecked(t *testing.T) {
	t.Parallel()
	for want, connection := range map[string]mysqlConnection{
		"database_server_port needs database_server_host":                               {Port: 3307},
		"database_server_certificate_authority_file needs database_server_host":         {CertificateAuthorityFile: "/etc/ssl/authority.pem"},
		"database_server_host needs login_password: a login over the network needs the": {Host: "db.example.com", LoginUser: "admin"},
	} {
		if err := connection.check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	for _, connection := range []mysqlConnection{{}, {LoginUser: "admin"}, {LoginUser: "admin", LoginPassword: "x"}, {LoginPassword: "x"},
		{Host: "db.example.com", Port: 3307, LoginPassword: "x", CertificateAuthorityFile: "/etc/ssl/authority.pem"},
		// A reference, whose value is checked once it is known.
		{Host: "db.example.com", Port: "{{ .vars.database_port }}", LoginPassword: "x"}} {
		if err := connection.check(); err != nil {
			t.Errorf("%+v: %v", connection, err)
		}
	}
	// The port is an integer, as a task writes it, as a reference gives it,
	// and as it arrives on the node.
	for _, port := range []any{1, 3307, 65535, int64(3307), float64(3307)} {
		if err := mysqlPortRule(port); err != nil {
			t.Errorf("database_server_port %#v: %v", port, err)
		}
	}
	for _, port := range []any{0, 65536, -1, 3307.5, "3307", "db", nil, true} {
		if err := mysqlPortRule(port); err == nil || err.Error() != "database_server_port must be a number from 1 to 65535" {
			t.Errorf("database_server_port %#v: %v", port, err)
		}
	}
	if got := (mysqlConnection{Port: float64(3307)}).port(); got != 3307 {
		t.Fatal(got)
	}
	if got := (mysqlConnection{}).port(); got != 3306 {
		t.Fatal(got)
	}
	for host, valid := range map[string]bool{"db.example.com": true, "10.0.0.5": true, "2001:db8::1": true, "db_1": true, "": false, "db example": false, "db\n": false, `db"`: false} {
		if err := mysqlHostRule(host); (err == nil) != valid {
			t.Errorf("database_server_host %q: %v", host, err)
		}
	}
	if err := mysqlLoginUserRule(""); err == nil {
		t.Error("an empty login_user is accepted")
	}
	// The message of a refused login never repeats the value.
	if err := mysqlLoginPasswordRule("secret\x00word"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Error(err)
	}
}

func TestDatabaseCertificateOverrideRequiresRemoteHost(t *testing.T) {
	t.Parallel()
	for _, inputs := range []Inputs{
		&MySQLDatabase{Name: "shop", DangerouslySkipServerCertificateCheck: true},
		&MySQLUser{Name: "shop", MayConnectFrom: "localhost", DangerouslySkipServerCertificateCheck: true},
		&PostgresqlDatabase{Name: "shop", Owner: "postgres", DangerouslySkipServerCertificateCheck: true},
		&PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(false), DangerouslySkipServerCertificateCheck: true},
	} {
		if err := inputs.Prepare(""); err == nil || !strings.Contains(err.Error(), "dangerously_skip_server_certificate_check") {
			t.Fatalf("%T: %v", inputs, err)
		}
	}
}

func TestDatabaseCertificateAuthorityAndSkipAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	for _, inputs := range []Inputs{
		&MySQLDatabase{Name: "shop", DatabaseServerHost: "db.example.com", LoginPassword: "admin", DatabaseServerCertificateAuthorityFile: "/etc/database/authority.crt", DangerouslySkipServerCertificateCheck: true},
		&MySQLUser{Name: "shop", MayConnectFrom: "localhost", DatabaseServerHost: "db.example.com", LoginPassword: "admin", DatabaseServerCertificateAuthorityFile: "/etc/database/authority.crt", DangerouslySkipServerCertificateCheck: true},
		&PostgresqlDatabase{Name: "shop", Owner: "postgres", DatabaseServerHost: "db.example.com", LoginPassword: "admin", DatabaseServerCertificateAuthorityFile: "/etc/database/authority.crt", DangerouslySkipServerCertificateCheck: true},
		&PostgresqlRole{Name: "shop", CanLogIn: postgresqlBool(false), DatabaseServerHost: "db.example.com", LoginPassword: "admin", DatabaseServerCertificateAuthorityFile: "/etc/database/authority.crt", DangerouslySkipServerCertificateCheck: true},
	} {
		want := "dangerously_skip_server_certificate_check: give either database_server_certificate_authority_file or dangerously_skip_server_certificate_check: true, not both"
		if err := inputs.Prepare(""); err == nil || err.Error() != want {
			t.Errorf("%T: %v; want %s", inputs, err, want)
		}
	}
}
