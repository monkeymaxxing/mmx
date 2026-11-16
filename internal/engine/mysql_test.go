package engine

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/value"
)

// A stand-in mariadb client and MariaDB server for one account. The account
// exists when the file account beside the stand-in's directory holds its
// stored password in hexadecimal. With the file refuse, the server does not
// understand CREATE USER and answers as MariaDB does: the client prints the
// statement, and the server quotes it from the word IDENTIFIED and cuts the
// quote inside the stored value. The call that creates the account starts
// with the line that sets how long its statements wait for a lock.
const fakeMariaDBForOneAccount = `#!/bin/sh
state=$(dirname "$0")/..
statements=$(cat)
case "$statements" in
*"VERSION()"*) printf 'product\t11.8.6-MariaDB\n';;
*"FROM mysql.global_priv"*)
  if [ -f "$state/account" ]; then printf 'account\tmysql_native_password\t%s\t\nserver privilege\tUSAGE\tNO\n' "$(cat "$state/account")"; fi;;
"SET SESSION lock_wait_timeout = 10;"*"CREATE USER"*)
  statement=$(printf '%s' "$statements" | sed 1d)
  statement=${statement%;}
  if [ -f "$state/refuse" ]; then
    quoted=$(printf '%s' "$statement" | sed 's/.*\(IDENTIFIED.*\)/\1/' | cut -c1-56)
    printf -- '--------------\n%s\n--------------\n\n' "$statement" >&2
    printf "ERROR 1064 (42000) at line 2: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near '%s...' at line 1\n" "$quoted" >&2
    exit 1
  fi
  stored=${statement#*USING \'}
  printf '%s' "${stored%\'}" | od -An -tx1 | tr -d ' \n' | tr a-f A-F > "$state/account";;
*) echo "unexpected statements: $statements" >&2; exit 64;;
esac
`

// A secret in the password of a mysql_user task does not hide the task's
// result: the result says that the account was created, and a later node
// task reads that. A failure keeps the server's message, and neither the
// password nor its stored value is in it, whole or in part: the module takes
// the parts of the stored value out of what the client printed, and the
// node task execution engine replaces the password and the whole value.
func TestASecretPasswordOfMySQLUserDoesNotHideTheResultAndStaysOutOfIt(t *testing.T) {
	ctx := context.Background()
	const password = "Sentinel-pass-4711"
	first := sha1.Sum([]byte(password))
	second := sha1.Sum(first[:])
	stored := fmt.Sprintf("*%X", second[:])
	dir := t.TempDir()
	bin, private := filepath.Join(dir, "bin"), filepath.Join(dir, "private_task_directories")
	for _, directory := range []string{bin, private} {
		if err := os.Mkdir(directory, 0711); err != nil {
			t.Fatal(err)
		}
	}
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "mariadb"), fakeMariaDBForOneAccount)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := Engine{PrivateTaskDirectoryRoot: func() (string, error) { return private, nil }}
	written := "{{ .secrets.shop_database_password }}"
	account := config.Task{ID: "account", Module: "mysql_user", ModuleInputs: &modules.MySQLUser{Name: "shop", MayConnectFrom: "10.0.%", Password: &written}}
	run := func(tasks ...config.Task) (string, []string) {
		t.Helper()
		j := job(tasks...)
		j.Scope = map[string]any{"secrets": map[string]any{"shop_database_password": &value.Secret{Name: "shop_database_password", Data: password, Available: true}}}
		r := e.Run(ctx, j)
		saved, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		// Neither the password nor five characters of its stored value.
		if strings.Contains(string(saved), "Sentinel") {
			t.Fatalf("the password is in the response: %s", saved)
		}
		for from := 0; from+5 <= len(stored); from++ {
			if strings.Contains(string(saved), stored[from:from+5]) {
				t.Fatalf("the piece %s of the stored value %s is in the response: %s", stored[from:from+5], stored, saved)
			}
		}
		if r.Results[0].Secret {
			t.Fatalf("the password hid the task's result: %s", saved)
		}
		reasons := []string{r.Status, r.Results[0].Reason, r.Error}
		return string(saved), reasons
	}
	saved, reasons := run(account, config.Task{ID: "later", Show: "{{ .node_tasks.account.value.created }}"})
	if reasons[0] != "succeeded" || !strings.Contains(saved, `"value":{"changed_attributes":[],"created":true}`) || !strings.Contains(saved, `"reason":"created the account shop@10.0.%"`) || !strings.Contains(saved, `"value":true`) {
		t.Fatalf("%s", saved)
	}
	if got := read(t, filepath.Join(dir, "account")); got != strings.ToUpper(fmt.Sprintf("%x", stored)) {
		t.Fatalf("the stand-in server got the stored value %s, not the one of the password", got)
	}
	if _, reasons = run(account); reasons[0] != "succeeded" {
		t.Fatal(reasons)
	}
	// The server refuses the statement that carries the stored value.
	if err := os.Remove(filepath.Join(dir, "account")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "refuse"), "")
	_, reasons = run(account)
	want := "mysql_user: creating the account shop@10.0.% on the database server on this node: --------------\nCREATE USER `shop`@`10.0.%` IDENTIFIED VIA mysql_native_password USING '[redacted]'\n--------------\n\n" +
		"ERROR 1064 (42000) at line 2: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'IDENTIFIED VIA mysql_native_password USING '[redacted]...' at line 1"
	if reasons[0] != "failed" || reasons[1] != want || reasons[2] != "account: "+want {
		t.Fatalf("status %s\nreason %s\nerror  %s\nwant   %s", reasons[0], reasons[1], reasons[2], want)
	}
	if left, _ := os.ReadDir(private); len(left) != 0 {
		t.Fatal("a private task directory is left", left)
	}
}

// A stand-in mariadb client that reaches no server: it notes its call in
// the file calls beside its directory and fails with the client's error in
// the file answer.
const fakeMariaDBThatFails = `#!/bin/sh
state=$(dirname "$0")/..
cat > /dev/null
echo called >> "$state/calls"
cat "$state/answer" >&2
exit 1
`

// A refused login is not tried again: the login that the task was given
// does not work, another try gets the same answer, and a MySQL account
// with FAILED_LOGIN_ATTEMPTS is locked after a few wrong logins. Neither is
// a server whose certificate the client does not accept. A server that
// does not answer is tried again, because a server that starts answers a
// moment later.
func TestARefusedDatabaseLoginIsNotTriedAgainAndAServerThatDoesNotAnswerIs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin, private := filepath.Join(dir, "bin"), filepath.Join(dir, "private_task_directories")
	for _, directory := range []string{bin, private} {
		if err := os.Mkdir(directory, 0711); err != nil {
			t.Fatal(err)
		}
	}
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "mariadb"), fakeMariaDBThatFails)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := Engine{PrivateTaskDirectoryRoot: func() (string, error) { return private, nil }}
	retry := &config.Retry{MaxTries: 3, PauseBetweenTries: "0s"}
	tasks := map[string]config.Task{
		"mysql_database": {ID: "task", Module: "mysql_database", Retry: retry, ModuleInputs: &modules.MySQLDatabase{Name: "shop"}},
		"mysql_user":     {ID: "task", Module: "mysql_user", Retry: retry, ModuleInputs: &modules.MySQLUser{Name: "shop", MayConnectFrom: "%"}},
	}
	for name, c := range map[string]struct {
		answer string
		// tries is how often the client is called for the first statement.
		tries       int
		failureKind string
	}{
		"a refused login with a password":                       {"ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)", 1, "config"},
		"a refused login through the socket":                    {"ERROR 1698 (28000): Access denied for user 'root'@'localhost'", 1, "config"},
		"a server whose certificate the client does not accept": {"ERROR 2026 (HY000): TLS/SSL error: self-signed certificate in certificate chain", 1, "config"},
		"no server behind the socket":                           {"ERROR 2002 (HY000): Can't connect to local server through socket '/run/mysqld/mysqld.sock' (2)", 3, ""},
		"no server at the address, MySQL's client":              {"ERROR 2003 (HY000): Can't connect to MySQL server on '127.0.0.1:3306' (111)", 3, ""},
	} {
		for module, task := range tasks {
			put(t, filepath.Join(dir, "answer"), c.answer+"\n")
			put(t, filepath.Join(dir, "calls"), "")
			r := e.Run(ctx, job(task))
			requireStatus(t, r, "failed")
			got := r.Results[0]
			calls := strings.Count(read(t, filepath.Join(dir, "calls")), "called")
			if calls != c.tries || got.FailureKind != c.failureKind || !strings.Contains(got.Reason, c.answer) || (c.tries == 1 && len(got.Tries) > 1) || (c.tries > 1 && len(got.Tries) != c.tries) {
				t.Errorf("%s, %s: the client was called %d times, want %d; failure kind %q, want %q; %d tries recorded; reason: %s", name, module, calls, c.tries, got.FailureKind, c.failureKind, len(got.Tries), got.Reason)
			}
		}
	}
}

// A login_password whose reference gives nothing, such as a secret that is
// empty, fails the task before the client is called: the login file would
// get an empty password, and the server would answer for an account without
// one.
func TestAnEmptyMySQLLoginPasswordFromAReferenceIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin, private := filepath.Join(dir, "bin"), filepath.Join(dir, "private_task_directories")
	for _, directory := range []string{bin, private} {
		if err := os.Mkdir(directory, 0711); err != nil {
			t.Fatal(err)
		}
	}
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "mariadb"), fakeMariaDBThatFails)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := Engine{PrivateTaskDirectoryRoot: func() (string, error) { return private, nil }}
	const written = "{{ .secrets.database_administrator_password }}"
	for module, task := range map[string]config.Task{
		"mysql_database": {ID: "task", Module: "mysql_database", ModuleInputs: &modules.MySQLDatabase{Name: "shop", DatabaseServerHost: "db.example.com", LoginPassword: written}},
		"mysql_user":     {ID: "task", Module: "mysql_user", ModuleInputs: &modules.MySQLUser{Name: "shop", MayConnectFrom: "%", State: "absent", LoginUser: "admin", LoginPassword: written}},
	} {
		put(t, filepath.Join(dir, "calls"), "")
		j := job(task)
		j.Scope = map[string]any{"secrets": map[string]any{"database_administrator_password": &value.Secret{Name: "database_administrator_password", Data: "", Available: true}}}
		r := e.Run(ctx, j)
		requireStatus(t, r, "failed")
		got := r.Results[0]
		if calls := read(t, filepath.Join(dir, "calls")); calls != "" || got.FailureKind != "config" || len(got.Tries) > 1 || !strings.Contains(got.Reason, "login_password must not be empty and must not hold a zero byte") {
			t.Errorf("%s: the client's calls %q; failure kind %q; %d tries recorded; reason: %s", module, calls, got.FailureKind, len(got.Tries), got.Reason)
		}
	}
}
