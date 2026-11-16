package modules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// mongodb_user runs MongoDB's shell mongosh with a JavaScript program of its
// own, which prints one line of JSON. mongosh is not on the machine that runs
// these tests, so a stand-in answers. Every answer below is what the module's
// program printed through the real mongosh 2.13.0 on a test guest, on
// 2026-10-10: against MongoDB 9.0.2 on Debian 13 and, where a comment says
// so, against MongoDB 8.0.32 on Rocky Linux 9. The integration suite
// tests/integration/mongodb_users.py proves the program itself on real
// servers; these tests cover what the module makes of each answer.

// The three calls of one mongosh run of the module. The stand-in for env
// starts the stand-in for mongosh, as the real env does.
const (
	mongoshVersionCall = "mongosh --version"
	mongoshThroughEnv  = "env HOME=/dev/null mongosh --quiet --norc --nodb --file /dev/stdin"
	mongoshItself      = "mongosh --quiet --norc --nodb --file /dev/stdin"
	// mongoshHomeWarning is the line that mongosh from 2.2.0 prints before
	// the program's output when it cannot write into HOME.
	mongoshHomeWarning = "Warning: Could not access file: ENOTDIR: not a directory, mkdir '/dev/null/.mongodb/mongosh'"
)

// mongoshCannotReadTheProgram is what mongosh 2.13.0 printed on standard
// error, with exit code 1, for a program with a mistake in its first line:
// the text of the program stands behind the first line, with a password.
const mongoshCannotReadTheProgram = "SyntaxError: Unexpected token, expected \",\" (1:46)\n\n> 1 | const input = {\"password\": \"" + mongodbLoginPassword + "\" \"name\": 1};\n    |                                               ^\n  2 | print(1);\n  3 |\n"

// Passwords that these tests give the module. None may appear in a command
// line or in a result.
const (
	mongodbUserPassword  = `Made-Up "user" pass\word ü 1`
	mongodbLoginPassword = "Made-Up-Admin-1"
)

// Answers of the reading run. A server that needs a login, and the login
// worked:
const (
	mongodbNoSuchUser        = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":null,"roles_that_do_not_exist":[]}`
	mongodbUserWithTwoRoles  = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":{"roles":[{"role":"read","db":"reports"},{"role":"readWrite","db":"shop"}]},"roles_that_do_not_exist":[],"password_is_the_users":true}`
	mongodbAnotherPassword   = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":{"roles":[{"role":"read","db":"reports"},{"role":"readWrite","db":"shop"}]},"roles_that_do_not_exist":[],"password_is_the_users":false}`
	mongodbPasswordNotAsked  = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":{"roles":[{"role":"read","db":"reports"},{"role":"readWrite","db":"shop"}]},"roles_that_do_not_exist":[]}`
	mongodbRoleDoesNotExist  = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":null,"roles_that_do_not_exist":[{"role":"noSuchRole","db":"reports"}]}`
	mongodbNoUserYet         = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"no_user_yet"}`
	mongodbLoginNeeded       = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed"}`
	mongodbLoginRefused      = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","login_refused":true}`
	mongodbWithoutAccessCtrl = `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","user":null,"access":"off","roles_that_do_not_exist":[]}`
)

// mongoshStandIn is the stand-in for env and mongosh of one test.
type mongoshStandIn struct {
	t *testing.T
	// files holds the answers that the stand-in prints, and the standard
	// input that it got.
	files string
	calls func() []string
}

// standInForMongosh gives the module the programs env and mongosh. mongosh
// --version prints 2.13.0, or what the test set with version. A run with the
// module's program keeps its standard input in a file, prints mongosh's
// warning about HOME, and then the answer that the test set for a reading or
// for a changing run.
func standInForMongosh(t *testing.T) *mongoshStandIn {
	t.Helper()
	files := t.TempDir()
	calls := stubTools(t, map[string]string{
		"env": `[ "$1" = HOME=/dev/null ] || { echo "the stand-in for env got: $*" >&2; exit 64; }
shift
program=$1
shift
exec "$STUBS/$program" "$@"`,
		"mongosh": `if [ "$1" = --version ]; then cat '` + files + `'/version; exit 0; fi
input=$(cat; echo .)
input=${input%.}
case "$input" in *'"mode":"read"'*) mode=read ;; *) mode=change ;; esac
printf '%s' "$input" > '` + files + `'/standard_input_of_the_${mode}_run
if [ -e '` + files + `'/standard_error_of_the_${mode}_run ]; then
  echo "` + mongoshHomeWarning + `"
  cat '` + files + `'/standard_error_of_the_${mode}_run >&2
  exit 1
fi
echo "` + mongoshHomeWarning + `"
if [ $mode = read ] && [ -e '` + files + `'/standard_input_of_the_change_run ] && [ -e '` + files + `'/answer_of_the_read_run_after_the_change ]; then
  cat '` + files + `'/answer_of_the_read_run_after_the_change
  exit 0
fi
cat '` + files + `'/answer_of_the_${mode}_run`,
	})
	stand := &mongoshStandIn{t: t, files: files, calls: calls}
	stand.version("2.13.0")
	return stand
}

// version sets what mongosh --version prints.
func (m *mongoshStandIn) version(text string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.files, "version"), []byte(text+"\n"), 0644); err != nil {
		m.t.Fatal(err)
	}
}

// answers sets what the next reading run and the next changing run print,
// and forgets what earlier runs got. An empty text leaves that run without
// an answer, so a run that must not happen fails its test.
func (m *mongoshStandIn) answers(reading, changing string) {
	m.t.Helper()
	entries, err := os.ReadDir(m.files)
	if err != nil {
		m.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "version" {
			continue
		}
		if err := os.Remove(filepath.Join(m.files, entry.Name())); err != nil {
			m.t.Fatal(err)
		}
	}
	for mode, answer := range map[string]string{"read": reading, "change": changing} {
		if answer == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(m.files, "answer_of_the_"+mode+"_run"), []byte(answer+"\n"), 0644); err != nil {
			m.t.Fatal(err)
		}
	}
}

// afterTheChange sets what a reading run prints that follows the changing
// run, and endsWithoutAResult makes the changing run end with exit code 1 and
// this text on standard error, without a result.
func (m *mongoshStandIn) afterTheChange(reading string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.files, "answer_of_the_read_run_after_the_change"), []byte(reading+"\n"), 0644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mongoshStandIn) endsWithoutAResult(mode, standardError string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.files, "standard_error_of_the_"+mode+"_run"), []byte(standardError), 0644); err != nil {
		m.t.Fatal(err)
	}
}

// standardInput returns what the reading or the changing run got on its
// standard input, and whether that run happened.
func (m *mongoshStandIn) standardInput(mode string) (string, bool) {
	m.t.Helper()
	b, err := os.ReadFile(filepath.Join(m.files, "standard_input_of_the_"+mode+"_run"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		m.t.Fatal(err)
	}
	return string(b), true
}

// valuesSentTo returns the task's values at the top of the program that the
// reading or the changing run got: the JSON of its first line. It also checks
// what every program must hold.
func (m *mongoshStandIn) valuesSentTo(mode string) map[string]any {
	m.t.Helper()
	text, ran := m.standardInput(mode)
	if !ran {
		m.t.Fatalf("no %s run of mongosh happened; calls: %q", mode, m.calls())
	}
	first, program, _ := strings.Cut(text, "\n")
	if !strings.HasPrefix(first, "const input = ") || !strings.HasSuffix(first, ";") {
		m.t.Fatalf("the first line of the program: %q", first)
	}
	// mongosh leaves a program with one of these words out of its log.
	for _, word := range []string{"new Mongo(", ".auth("} {
		if !strings.Contains(program, word) {
			m.t.Errorf("the program does not hold %q", word)
		}
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(first, "const input = "), ";")), &values); err != nil {
		m.t.Fatalf("the values of the program are no JSON: %v: %q", err, first)
	}
	return values
}

// runsOfTheProgram returns the calls since the test began without the
// version question: each run of the program is the two calls
// mongoshThroughEnv and mongoshItself.
func (m *mongoshStandIn) runsOfTheProgram() int {
	m.t.Helper()
	calls := m.calls()
	runs := 0
	for i := 0; i < len(calls); i++ {
		switch calls[i] {
		case mongoshVersionCall:
		case mongoshThroughEnv:
			if i+1 >= len(calls) || calls[i+1] != mongoshItself {
				m.t.Fatalf("calls: %q", calls)
			}
			runs++
			i++
		default:
			m.t.Fatalf("an unexpected call %q in %q", calls[i], calls)
		}
	}
	return runs
}

// mongodbShopTask is the design's example task.
func mongodbShopTask(changed map[string]any) map[string]any {
	inputs := map[string]any{
		"name": "shop", "database": "shop", "password": mongodbUserPassword,
		"roles":            []any{map[string]any{"role": "readWrite", "database": "shop"}, map[string]any{"role": "read", "database": "reports"}},
		"roles_not_listed": "remove",
		"login_user":       "admin", "login_password": mongodbLoginPassword,
	}
	for name, value := range changed {
		if value == nil {
			delete(inputs, name)
			continue
		}
		inputs[name] = value
	}
	return inputs
}

// mongodbRemovalTask removes the user shop: a removal takes no password and
// no roles.
func mongodbRemovalTask() map[string]any {
	return mongodbShopTask(map[string]any{"state": "absent", "password": nil, "roles": nil, "roles_not_listed": nil})
}

// mongodbAdministratorTask is the design's task for a server's first user.
func mongodbAdministratorTask() map[string]any {
	return mongodbShopTask(map[string]any{"name": "admin", "database": "admin", "password": mongodbLoginPassword,
		"roles": []any{map[string]any{"role": "root", "database": "admin"}}})
}

// holdsNoPassword fails the test when a command line or the result holds one
// of the tests' passwords, plain or as JSON writes it.
func holdsNoPassword(t *testing.T, stand *mongoshStandIn, r protocol.Result) {
	t.Helper()
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{mongodbUserPassword, mongodbLoginPassword} {
		quoted, _ := json.Marshal(password)
		for where, text := range map[string]string{"the calls": strings.Join(stand.calls(), "\n"), "the result": string(encoded) + r.Reason + r.Diff + r.Stderr} {
			if strings.Contains(text, password) || strings.Contains(text, strings.Trim(string(quoted), `"`)) {
				t.Errorf("%s hold a password: %s", where, text)
			}
		}
	}
}

func TestMongoDBUserRejectsInputs(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	role := func(entry map[string]any) []any { return []any{entry} }
	rolesForm := "a list of roles, each with the two keys role and database, such as {role: readWrite, database: shop}"
	removalAccepts := "mongodb_user.state: absent accepts only name, database, login_user, login_password, login_database, and mongodb_connection_string; remove "
	for name, c := range map[string]struct {
		changed map[string]any
		reason  string
	}{
		"an empty name":                    {map[string]any{"name": ""}, "mongodb_user.name: must not be empty"},
		"an empty database":                {map[string]any{"database": ""}, "mongodb_user.database: must not be empty"},
		"an empty password":                {map[string]any{"password": ""}, "mongodb_user.password: must not be empty"},
		"an empty login password":          {map[string]any{"login_password": ""}, "mongodb_user.login_password: must not be empty"},
		"an empty login database":          {map[string]any{"login_database": ""}, "mongodb_user.login_database: must not be empty"},
		"an empty connection string":       {map[string]any{"mongodb_connection_string": ""}, "mongodb_user.mongodb_connection_string: must not be empty"},
		"a login user without password":    {map[string]any{"login_password": nil}, "mongodb_user.login_password: is required with login_user"},
		"a login password without user":    {map[string]any{"login_user": nil}, "mongodb_user.login_user: is required with login_password"},
		"roles without roles_not_listed":   {map[string]any{"roles_not_listed": nil}, "mongodb_user.roles_not_listed: is required with roles: remove takes every role that roles does not list away from the user, and keep leaves those roles"},
		"roles_not_listed without roles":   {map[string]any{"roles": nil}, "mongodb_user.roles: is required with roles_not_listed"},
		"roles that are no list":           {map[string]any{"roles": map[string]any{"role": "readWrite", "database": "shop"}}, "mongodb_user.roles: expected " + rolesForm},
		"a role that is a text":            {map[string]any{"roles": []any{"readWrite"}}, "mongodb_user.roles: entry 1: expected " + rolesForm},
		"a role without its database":      {map[string]any{"roles": role(map[string]any{"role": "readWrite"})}, "mongodb_user.roles: entry 1: expected " + rolesForm},
		"a role with MongoDB's own key":    {map[string]any{"roles": []any{map[string]any{"role": "read", "database": "shop"}, map[string]any{"role": "readWrite", "db": "shop"}}}, "mongodb_user.roles: entry 2: expected " + rolesForm},
		"a role with an empty name":        {map[string]any{"roles": role(map[string]any{"role": "", "database": "shop"})}, "mongodb_user.roles: entry 1: role must be a text that is not empty"},
		"a role with a number":             {map[string]any{"roles": role(map[string]any{"role": "read", "database": 5})}, "mongodb_user.roles: entry 1: database must be a text that is not empty"},
		"a removal with a password":        {map[string]any{"state": "absent"}, removalAccepts + "password"},
		"a removal with roles":             {map[string]any{"state": "absent", "password": nil}, removalAccepts + "roles"},
		"a removal with roles_not_listed":  {map[string]any{"state": "absent", "password": nil, "roles": nil}, removalAccepts + "roles_not_listed"},
		"a removal with an empty password": {map[string]any{"state": "absent", "password": ""}, removalAccepts + "password"},
		"a connection string with a login": {map[string]any{"mongodb_connection_string": "mongodb://admin:" + mongodbLoginPassword + "@127.0.0.1:27017/?directConnection=true"},
			"mongodb_user.mongodb_connection_string: must hold no user name and no password; give them as login_user and login_password"},
		"a connection string with a user name": {map[string]any{"mongodb_connection_string": "mongodb+srv://admin@cluster.example.com"},
			"mongodb_user.mongodb_connection_string: must hold no user name and no password; give them as login_user and login_password"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(c.changed), dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != c.reason {
				t.Errorf("%s (dryrun %v): %+v", name, dryrun, r)
			}
			if strings.Contains(r.Reason, mongodbLoginPassword) {
				t.Errorf("%s: the reason holds the password of the connection string", name)
			}
		}
	}
	// A rule of the description is refused by MMX before the script runs.
	if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"state": "gone"}), true); r.FailureKind != "config" || r.Reason != "mongodb_user.state: expected one of present, absent" {
		t.Errorf("a state that the description does not allow: %+v", r)
	}
	if calls := stand.calls(); calls != nil {
		t.Errorf("a task with rejected inputs ran a program: %q", calls)
	}
}

func TestMongoDBUserMissingPrograms(t *testing.T) {
	t.Parallel()
	inputs := mongodbShopTask(nil)
	// No mongosh: only the system's own env is on PATH.
	calls := stubTools(t, map[string]string{})
	reason := "mongodb_user: the program mongosh is not installed; install the package mongodb-mongosh, from MongoDB's own package repository, in an earlier node task"
	if r := runOfficialExtraModule(t, "mongodb_user", inputs, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Errorf("a dryrun without mongosh: %+v", r)
	}
	if r := runOfficialExtraModule(t, "mongodb_user", inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != reason {
		t.Errorf("a run without mongosh: %+v", r)
	}
	if got := calls(); got != nil {
		t.Errorf("calls without mongosh: %q", got)
	}
	// No env either.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if r := runOfficialExtraModule(t, "mongodb_user", inputs, false); r.FailureKind != "missing_prerequisite" ||
		r.Reason != "mongodb_user: the program env is not installed; install the package coreutils in an earlier node task" {
		t.Errorf("a run without env: %+v", r)
	}
}

// With HOME=/dev/null, mongosh 2.2.0 and newer print one warning line before
// the program's output. Older ones print more lines and terminal codes into
// the output, so the module asks for the version first and refuses them.
func TestMongoDBUserNeedsMongosh220(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	for version, accepted := range map[string]bool{"1.9.0": false, "2.1.5": false, "2.2.0": true, "2.13.0": true, "3.0.0": true} {
		stand.version(version)
		stand.answers(mongodbUserWithTwoRoles, "")
		runsBefore := stand.runsOfTheProgram()
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), dryrun)
			if accepted {
				if r.Status != "succeeded" || r.Change != "unchanged" {
					t.Errorf("mongosh %s (dryrun %v): %+v", version, dryrun, r)
				}
				continue
			}
			reason := "mongodb_user: mongosh " + version + " is installed, and the module needs mongosh 2.2.0 or newer; install a newer package mongodb-mongosh in an earlier node task"
			if r.Reason != reason || (dryrun && r.Status != "not_checked") || (!dryrun && r.FailureKind != "missing_prerequisite") {
				t.Errorf("mongosh %s (dryrun %v): %+v", version, dryrun, r)
			}
		}
		if !accepted && stand.runsOfTheProgram() != runsBefore {
			t.Errorf("mongosh %s ran the program: %q", version, stand.calls())
		}
	}
	stand.version("mongosh: cannot open shared object file")
	runsBefore := stand.runsOfTheProgram()
	if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false); r.Status != "failed" || r.FailureKind != "" ||
		r.Reason != `mongodb_user: mongosh --version printed "mongosh: cannot open shared object file", which the module cannot read as a version` || stand.runsOfTheProgram() != runsBefore {
		t.Errorf("a version text that is no version: %+v", r)
	}
}

// The first run creates the user, the second finds nothing to do, and a
// dryrun says what a run would do and runs no changing command.
func TestMongoDBUserCreatesAUser(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbNoSuchUser, "")
	created := "the user shop in the database shop with the roles readWrite on the database shop, read on the database reports"

	r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), true)
	if r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "would create "+created || r.ModuleOutputs["created"] != true ||
		r.Diff != "would run: "+mongoshThroughEnv+"\n" {
		t.Fatalf("the dryrun: %+v", r)
	}
	if got := stand.calls(); !slices.Equal(got, []string{mongoshVersionCall, mongoshThroughEnv, mongoshItself}) {
		t.Fatalf("the dryrun's calls: %q", got)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Fatal("the dryrun ran the changing program")
	}
	// What the reading program gets: the task's values, and the roles to look up.
	read := stand.valuesSentTo("read")
	wantRead := map[string]any{"mode": "read", "mongodb_connection_string": "mongodb://127.0.0.1:27017/?directConnection=true",
		"name": "shop", "database": "shop", "password": mongodbUserPassword, "login_user": "admin", "login_password": mongodbLoginPassword, "login_database": "admin",
		"roles_to_look_up": []any{map[string]any{"role": "readWrite", "db": "shop"}, map[string]any{"role": "read", "db": "reports"}}}
	if !reflect.DeepEqual(read, wantRead) {
		t.Errorf("the values of the reading program:\n got %v\nwant %v", read, wantRead)
	}
	holdsNoPassword(t, stand, r)

	stand.answers(mongodbNoSuchUser, `{"done":["create_user"]}`)
	r = runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "created "+created || r.ModuleOutputs["created"] != true ||
		r.Diff != "ran: "+mongoshThroughEnv+"\n" || r.Fingerprint == "" {
		t.Fatalf("the run: %+v", r)
	}
	if stand.runsOfTheProgram() != 3 {
		t.Fatalf("the calls after the dryrun and the run: %q", stand.calls())
	}
	change := stand.valuesSentTo("change")
	wantChange := map[string]any{"mode": "change", "mongodb_connection_string": "mongodb://127.0.0.1:27017/?directConnection=true",
		"name": "shop", "database": "shop", "password": mongodbUserPassword, "login_user": "admin", "login_password": mongodbLoginPassword, "login_database": "admin",
		"log_in": true, "changes": []any{map[string]any{"change": "create_user",
			"roles": []any{map[string]any{"role": "readWrite", "db": "shop"}, map[string]any{"role": "read", "db": "reports"}}}}}
	if !reflect.DeepEqual(change, wantChange) {
		t.Errorf("the values of the changing program:\n got %v\nwant %v", change, wantChange)
	}
	holdsNoPassword(t, stand, r)

	// The server gives the roles in another order than the task: unchanged.
	stand.answers(mongodbUserWithTwoRoles, "")
	for _, dryrun := range []bool{false, true} {
		r = runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), dryrun)
		if r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "" || r.ModuleOutputs["created"] != false || r.Diff != "" {
			t.Errorf("the unchanged task (dryrun %v): %+v", dryrun, r)
		}
	}
	if _, ran := stand.standardInput("change"); ran || stand.runsOfTheProgram() != 5 {
		t.Errorf("an unchanged task ran a changing command: %q", stand.calls())
	}

	// A user without roles, a login database of its own, and another server.
	stand.answers(mongodbNoSuchUser, `{"done":["create_user"]}`)
	r = runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"roles": nil, "roles_not_listed": nil, "login_database": "operators",
		"mongodb_connection_string": "mongodb://db01.example.com:27018/?tls=true&serverSelectionTimeoutMS=5000"}), false)
	if r.Change != "changed" || r.Reason != "created the user shop in the database shop without a role" {
		t.Errorf("a user without roles: %+v", r)
	}
	change = stand.valuesSentTo("change")
	if change["login_database"] != "operators" || change["mongodb_connection_string"] != "mongodb://db01.example.com:27018/?tls=true&serverSelectionTimeoutMS=5000" ||
		!reflect.DeepEqual(change["changes"], []any{map[string]any{"change": "create_user", "roles": []any{}}}) {
		t.Errorf("the values of the changing program: %v", change)
	}
	if looked := stand.valuesSentTo("read")["roles_to_look_up"]; !reflect.DeepEqual(looked, []any{}) {
		t.Errorf("a task without roles looked roles up: %v", looked)
	}
}

// Only what differs changes: the password when the server refuses a login
// with it, and the roles as roles_not_listed says.
func TestMongoDBUserChangesOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	shop := "the user shop in the database shop"
	role := func(name, database string) map[string]any { return map[string]any{"role": name, "database": database} }
	sent := func(name, database string) map[string]any { return map[string]any{"role": name, "db": database} }
	stand := standInForMongosh(t)
	for name, c := range map[string]struct {
		changed map[string]any
		reading string
		done    string
		changes []any
		reason  string
	}{
		"another password": {nil, mongodbAnotherPassword, `{"done":["set_password"]}`,
			[]any{map[string]any{"change": "set_password", "roles": []any{}}}, "set the password of " + shop},
		"a password that is not given is not compared and not set": {map[string]any{"password": nil}, mongodbPasswordNotAsked, "", nil, ""},
		"remove: a role more and a role less": {map[string]any{"roles": []any{role("readWrite", "shop"), role("dbAdmin", "shop")}}, mongodbUserWithTwoRoles, `{"done":["set_roles"]}`,
			[]any{map[string]any{"change": "set_roles", "roles": []any{sent("readWrite", "shop"), sent("dbAdmin", "shop")}}},
			"granted the role dbAdmin on the database shop to " + shop + "; revoked the role read on the database reports from " + shop},
		"remove: an empty list takes every role": {map[string]any{"roles": []any{}}, mongodbUserWithTwoRoles, `{"done":["set_roles"]}`,
			[]any{map[string]any{"change": "set_roles", "roles": []any{}}},
			"revoked the roles read on the database reports, readWrite on the database shop from " + shop},
		"keep: only the missing role is granted": {map[string]any{"roles": []any{role("readWrite", "shop"), role("dbAdmin", "shop"), role("read", "orders")}, "roles_not_listed": "keep"},
			mongodbUserWithTwoRoles, `{"done":["grant_roles"]}`,
			[]any{map[string]any{"change": "grant_roles", "roles": []any{sent("dbAdmin", "shop"), sent("read", "orders")}}},
			"granted the roles dbAdmin on the database shop, read on the database orders to " + shop},
		"keep: a role that the task does not list stays":       {map[string]any{"roles": []any{role("readWrite", "shop")}, "roles_not_listed": "keep"}, mongodbUserWithTwoRoles, "", nil, ""},
		"keep: an empty list changes nothing":                  {map[string]any{"roles": []any{}, "roles_not_listed": "keep"}, mongodbUserWithTwoRoles, "", nil, ""},
		"a role that the task lists twice is one role":         {map[string]any{"roles": []any{role("readWrite", "shop"), role("read", "reports"), role("readWrite", "shop")}}, mongodbUserWithTwoRoles, "", nil, ""},
		"without roles, the roles of the user are not touched": {map[string]any{"roles": nil, "roles_not_listed": nil}, mongodbUserWithTwoRoles, "", nil, ""},
		"the password and the roles": {map[string]any{"roles": []any{role("readWrite", "shop")}}, mongodbAnotherPassword, `{"done":["set_password","set_roles"]}`,
			[]any{map[string]any{"change": "set_password", "roles": []any{}}, map[string]any{"change": "set_roles", "roles": []any{sent("readWrite", "shop")}}},
			"set the password of " + shop + "; revoked the role read on the database reports from " + shop},
	} {
		stand.answers(c.reading, c.done)
		runsBefore := stand.runsOfTheProgram()
		wantChange, wantDryrun := "changed", "would_change"
		if c.changes == nil {
			wantChange, wantDryrun = "unchanged", "unchanged"
		}
		dryrun := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(c.changed), true)
		would := strings.NewReplacer("set the", "would set the", "granted", "would grant", "revoked", "would revoke").Replace(c.reason)
		if dryrun.Status != "succeeded" || dryrun.Change != wantDryrun || dryrun.Reason != would || dryrun.ModuleOutputs["created"] != false {
			t.Errorf("%s: the dryrun: %+v", name, dryrun)
		}
		if _, ran := stand.standardInput("change"); ran {
			t.Errorf("%s: the dryrun ran the changing program", name)
		}
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(c.changed), false)
		if r.Status != "succeeded" || r.Change != wantChange || r.Reason != c.reason || r.ModuleOutputs["created"] != false {
			t.Errorf("%s: the run: %+v", name, r)
		}
		holdsNoPassword(t, stand, r)
		if c.changes == nil {
			if _, ran := stand.standardInput("change"); ran || stand.runsOfTheProgram() != runsBefore+2 {
				t.Errorf("%s: an unchanged task ran a changing command: %q", name, stand.calls())
			}
			continue
		}
		if got := stand.valuesSentTo("change")["changes"]; !reflect.DeepEqual(got, c.changes) {
			t.Errorf("%s: the changes:\n got %v\nwant %v", name, got, c.changes)
		}
	}
	// Without a password in the task, the reading program gets none, so it
	// logs in as nobody.
	stand.answers(mongodbPasswordNotAsked, "")
	runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"password": nil}), false)
	if password, sent := stand.valuesSentTo("read")["password"]; !sent || password != nil {
		t.Errorf("the password of a task without password: %v", password)
	}
}

func TestMongoDBUserRemovesAUser(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	absent := mongodbRemovalTask()
	stand.answers(mongodbPasswordNotAsked, `{"done":["remove_user"]}`)
	if r := runOfficialExtraModule(t, "mongodb_user", absent, true); r.Change != "would_change" || r.Reason != "would remove the user shop in the database shop" || r.ModuleOutputs["created"] != false {
		t.Errorf("the dryrun: %+v", r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("the dryrun ran the changing program")
	}
	// A removal compares no password and looks no role up.
	if read := stand.valuesSentTo("read"); read["password"] != nil || !reflect.DeepEqual(read["roles_to_look_up"], []any{}) {
		t.Errorf("the values of the reading program: %v", read)
	}
	r := runOfficialExtraModule(t, "mongodb_user", absent, false)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "removed the user shop in the database shop" {
		t.Errorf("the run: %+v", r)
	}
	if got := stand.valuesSentTo("change")["changes"]; !reflect.DeepEqual(got, []any{map[string]any{"change": "remove_user", "roles": []any{}}}) {
		t.Errorf("the changes: %v", got)
	}
	// A user that the server does not have: nothing to do.
	stand.answers(mongodbNoSuchUser, "")
	for _, dryrun := range []bool{true, false} {
		if r := runOfficialExtraModule(t, "mongodb_user", mongodbRemovalTask(), dryrun); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "" {
			t.Errorf("a missing user (dryrun %v): %+v", dryrun, r)
		}
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("the removal of a missing user ran the changing program")
	}
}

// A server without access control needs no login: the module does not log
// in, also when the task gives one.
func TestMongoDBUserWithoutAccessControl(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	for name, changed := range map[string]map[string]any{
		"without a login": {"login_user": nil, "login_password": nil},
		"with a login":    nil,
	} {
		stand.answers(mongodbWithoutAccessCtrl, `{"done":["create_user"]}`)
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(changed), false)
		if r.Status != "succeeded" || r.Change != "changed" || r.ModuleOutputs["created"] != true {
			t.Errorf("%s: %+v", name, r)
		}
		if logsIn := stand.valuesSentTo("change")["log_in"]; logsIn != false {
			t.Errorf("%s: log_in is %v", name, logsIn)
		}
	}
}

// A server with access control and no user: only its first administrator
// can be created, without a login.
func TestMongoDBUserFirstUserOfAServer(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbNoUserYet, `{"done":["create_user"]}`)
	first := "the user admin in the database admin with the role root on the database admin, as the first user of this MongoDB server"
	if r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), true); r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "would create "+first || r.ModuleOutputs["created"] != true {
		t.Errorf("the dryrun of the first administrator: %+v", r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("the dryrun ran the changing program")
	}
	r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "created "+first || r.ModuleOutputs["created"] != true {
		t.Errorf("the run of the first administrator: %+v", r)
	}
	change := stand.valuesSentTo("change")
	if change["log_in"] != false || !reflect.DeepEqual(change["changes"], []any{map[string]any{"change": "create_user", "roles": []any{map[string]any{"role": "root", "db": "admin"}}}}) {
		t.Errorf("the values of the changing program: %v", change)
	}
	holdsNoPassword(t, stand, r)

	// Each of the three roles that manage users makes a first administrator.
	for _, role := range []string{"userAdminAnyDatabase", "userAdmin"} {
		task := mongodbAdministratorTask()
		task["roles"] = []any{map[string]any{"role": "readWrite", "database": "admin"}, map[string]any{"role": role, "database": "admin"}}
		if r := runOfficialExtraModule(t, "mongodb_user", task, true); r.Status != "succeeded" || r.Change != "would_change" {
			t.Errorf("a first user with the role %s: %+v", role, r)
		}
	}

	// Every other task waits for the administrator: not checked in a
	// dryrun, and a failure without another try in a run.
	noAdministrator := "mongodb_user: this MongoDB server has access control on and no administrator yet; create the first user in the database admin, with the role root, in an earlier mongodb_user task"
	absentAdministrator := mongodbRemovalTask()
	absentAdministrator["name"], absentAdministrator["database"] = "admin", "admin"
	stand.answers(mongodbNoUserYet, "")
	for name, inputs := range map[string]map[string]any{"a user in another database": mongodbShopTask(nil), "the removal of a user in admin": absentAdministrator} {
		if r := runOfficialExtraModule(t, "mongodb_user", inputs, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != noAdministrator {
			t.Errorf("%s, dryrun: %+v", name, r)
		}
		if r := runOfficialExtraModule(t, "mongodb_user", inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != noAdministrator {
			t.Errorf("%s, run: %+v", name, r)
		}
	}

	// A first user that could not create the next one is refused: MongoDB
	// would accept it.
	situation := "mongodb_user.roles: this MongoDB server has access control on and no user yet, so this task creates its first user. "
	cannotManage := situation + "The first user must be allowed to manage users: give it the role root, userAdminAnyDatabase, or userAdmin on the database admin. " +
		"MongoDB would accept a first user without such a role, and nobody could create a user afterwards"
	for name, c := range map[string]struct {
		roles  any
		reason string
	}{
		"a role that manages no users": {[]any{map[string]any{"role": "readWrite", "database": "admin"}}, cannotManage},
		"no roles":                     {nil, cannotManage},
		"an empty list of roles":       {[]any{}, cannotManage},
		"a role on another database": {[]any{map[string]any{"role": "root", "database": "admin"}, map[string]any{"role": "readWrite", "database": "shop"}},
			situation + "MongoDB lets the first user hold only roles on the database admin, and the task lists the role readWrite on the database shop. Give the first user the role root on admin"},
		"a managing role on another database": {[]any{map[string]any{"role": "userAdmin", "database": "shop"}},
			situation + "MongoDB lets the first user hold only roles on the database admin, and the task lists the role userAdmin on the database shop. Give the first user the role root on admin"},
	} {
		task := mongodbAdministratorTask()
		if c.roles == nil {
			delete(task, "roles")
			delete(task, "roles_not_listed")
		} else {
			task["roles"] = c.roles
		}
		for _, dryrun := range []bool{true, false} {
			if r := runOfficialExtraModule(t, "mongodb_user", task, dryrun); r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != c.reason {
				t.Errorf("%s (dryrun %v): %+v", name, dryrun, r)
			}
		}
	}
	// A task without password for the administrator: an earlier node task
	// of the same playbook may be the one that creates it.
	withoutPassword := mongodbAdministratorTask()
	delete(withoutPassword, "password")
	noPassword := "mongodb_user: the user admin in the database admin does not exist, and the task has no password to create it; add password, or create the user in an earlier mongodb_user task"
	if r := runOfficialExtraModule(t, "mongodb_user", withoutPassword, true); r.Status != "not_checked" || r.Reason != noPassword {
		t.Errorf("a first administrator without a password, dryrun: %+v", r)
	}
	if r := runOfficialExtraModule(t, "mongodb_user", withoutPassword, false); r.FailureKind != "missing_prerequisite" || r.Reason != noPassword {
		t.Errorf("a first administrator without a password, run: %+v", r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a task that cannot create the first user ran the changing program")
	}
}

// The server refuses the first administrator's creation, which runs without
// a login: a user appeared between the read and the change. The next try
// reads the server again and logs in, so this is an ordinary failure. The
// answer was measured on MongoDB 8.0.32.
func TestMongoDBUserFirstUserRefusedWithoutALogin(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbNoUserYet, `{"done":[],"error":{"step":"create_user","name":"MongoServerError","code":13,"code_name":"Unauthorized","message":"Command createUser requires authentication","database_input":"database","role":null}}`)
	r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false)
	if r.Status != "failed" || r.FailureKind != "" || r.Reason != "mongodb_user: creating the user admin in the database admin with the role root on the database admin, as the first user of this MongoDB server: "+
		"Command createUser requires authentication (MongoDB's error 13, Unauthorized)" {
		t.Errorf("%+v", r)
	}
}

// A server that has users in other databases and none in admin, with access
// control on. MongoDB 8.2 and newer still let the first administrator in
// without a login, and MongoDB before 8.2 asks for a login that nobody has.
// Both answers were measured: 9.0.2 on Debian 13 and 8.0.32 on Rocky Linux
// 9, each as the primary of a replica set.
func TestMongoDBUserUsersOnlyOutsideAdmin(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(`{"is_primary":true,"primary":"127.0.0.1:27017","replica_set":"rs0","replica_set_note":"","access":"no_user_yet"}`, `{"done":["create_user"]}`)
	if r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false); r.Status != "succeeded" || r.Change != "changed" || r.ModuleOutputs["created"] != true {
		t.Errorf("MongoDB 9.0.2: %+v", r)
	}
	stand.answers(`{"is_primary":true,"primary":"127.0.0.1:27017","replica_set":"rs0","replica_set_note":"","access":"login_needed","login_refused":true}`, "")
	r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false)
	if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" ||
		r.Reason != "mongodb_user: the MongoDB server refused the login of the user admin in the database admin; check login_user, login_database, and login_password" {
		t.Errorf("MongoDB 8.0.32: %+v", r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a refused login ran the changing program")
	}
}

func TestMongoDBUserLogin(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	// The server needs a login and the task has none.
	stand.answers(mongodbLoginNeeded, "")
	for _, dryrun := range []bool{true, false} {
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"login_user": nil, "login_password": nil}), dryrun)
		if r.Status != "failed" || r.FailureKind != "config" || r.Reason != "mongodb_user.login_user: this MongoDB server needs a login; give login_user and login_password" {
			t.Errorf("no login (dryrun %v): %+v", dryrun, r)
		}
	}
	if login, sent := stand.valuesSentTo("read")["login_user"]; !sent || login != nil {
		t.Errorf("the login user of a task without login: %v", login)
	}
	// A refused login fails the task in a dryrun as in a run, without
	// another try: the next try would get the same answer.
	stand.answers(mongodbLoginRefused, "")
	for _, dryrun := range []bool{true, false} {
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"login_user": "operator", "login_database": "operators"}), dryrun)
		if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" ||
			r.Reason != "mongodb_user: the MongoDB server refused the login of the user operator in the database operators; check login_user, login_database, and login_password" {
			t.Errorf("a refused login (dryrun %v): %+v", dryrun, r)
		}
		holdsNoPassword(t, stand, r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a task without a working login ran the changing program")
	}
	// The login worked for the reading program and is refused for the
	// changing one, a moment later.
	stand.answers(mongodbPasswordNotAsked, `{"done":[],"error":{"step":"log_in","name":"MongoServerError","code":18,"code_name":"AuthenticationFailed","message":"Authentication failed.","database_input":"login_database","role":null}}`)
	r := runOfficialExtraModule(t, "mongodb_user", mongodbRemovalTask(), false)
	if r.Status != "failed" || r.FailureKind != "config" ||
		r.Reason != "mongodb_user: the MongoDB server refused the login of the user admin in the database admin; check login_user, login_database, and login_password" {
		t.Errorf("a login that the changing program lost: %+v", r)
	}
}

// While the server does not answer, a dryrun cannot check the task, because
// an earlier node task may start the server; a run fails and is tried again.
// A connection string that mongosh cannot read is a rejected input.
func TestMongoDBUserServerDoesNotAnswer(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	for answer, reason := range map[string]string{
		`{"error":{"step":"connect","name":"MongoNetworkError","code":null,"code_name":null,"message":"connect ECONNREFUSED 127.0.0.1:27999","database_input":null,"role":null}}`:             "mongodb_user: the MongoDB server does not answer: connect ECONNREFUSED 127.0.0.1:27999",
		`{"error":{"step":"connect","name":"MongoServerSelectionError","code":null,"code_name":null,"message":"Server selection timed out after 1000 ms","database_input":null,"role":null}}`: "mongodb_user: the MongoDB server does not answer: Server selection timed out after 1000 ms",
	} {
		stand.answers(answer, "")
		if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("the dryrun: %+v", r)
		}
		if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false); r.Status != "failed" || r.FailureKind != "" || r.Change != "unchanged" || r.Reason != reason {
			t.Errorf("the run: %+v", r)
		}
	}
	for answer, reason := range map[string]string{
		`{"error":{"step":"connect","name":"MongoshInvalidInputError","code":"COMMON-10001","code_name":null,"message":"[COMMON-10001] Invalid URI: not a connection string","database_input":null,"role":null}}`: "[COMMON-10001] Invalid URI: not a connection string",
		`{"error":{"step":"connect","name":"MongoParseError","code":null,"code_name":null,"message":"option nosuchoption is not supported","database_input":null,"role":null}}`:                                   "option nosuchoption is not supported",
		// mongosh takes an @ behind the question mark for the end of a login:
		// mongodb://127.0.0.1:27017/?directConnection=true&appName=mmx@controller
		`{"error":{"step":"connect","name":"MongoParseError","code":null,"code_name":null,"message":"Password contains unescaped characters","database_input":null,"role":null}}`: "Password contains unescaped characters",
	} {
		stand.answers(answer, "")
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"mongodb_connection_string": "mongodb://127.0.0.1:27017/?directConnection=true&appName=mmx@controller"}), dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Reason != "mongodb_user.mongodb_connection_string: mongosh cannot read this connection string: "+reason {
				t.Errorf("a connection string that mongosh cannot read (dryrun %v): %+v", dryrun, r)
			}
		}
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a task without a server ran the changing program")
	}
}

// Only the primary of a replica set changes users. The three answers were
// measured on MongoDB 9.0.2: a secondary, a member that was never initiated,
// and the secondary after its primary stopped.
func TestMongoDBUserNotThePrimary(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	start := "mongodb_user: this MongoDB server is a member of the replica set rs0 and not its primary, and only the primary changes users. "
	// The replica set has another primary: another try reaches the same
	// server, so the task has one try, in a dryrun as in a run.
	stand.answers(`{"is_primary":false,"primary":"127.0.0.1:27017","replica_set":"rs0","replica_set_note":""}`, "")
	for _, dryrun := range []bool{true, false} {
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), dryrun)
		if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != start+
			"The primary is 127.0.0.1:27017: run this task on that node, or put replicaSet=rs0 into mongodb_connection_string and leave directConnection=true out, so that mongosh goes to the primary by itself" {
			t.Errorf("a secondary (dryrun %v): %+v", dryrun, r)
		}
	}
	// The replica set has no primary now. It chooses one a few seconds after
	// an earlier node task initiated it: a run is tried again, and a dryrun
	// cannot check the task.
	for answer, reason := range map[string]string{
		`{"is_primary":false,"primary":"","replica_set":"","replica_set_note":"Does not have a valid replica set config"}`: "mongodb_user: this MongoDB server is a member of a replica set and not its primary, and only the primary changes users. " +
			"The replica set has no primary now (Does not have a valid replica set config)",
		`{"is_primary":false,"primary":"","replica_set":"rs0","replica_set_note":""}`: start + "The replica set has no primary now",
	} {
		stand.answers(answer, "")
		if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("no primary, dryrun: %+v", r)
		}
		if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false); r.Status != "failed" || r.FailureKind != "" || r.NoOtherTry || r.Change != "unchanged" || r.Reason != reason {
			t.Errorf("no primary, run: %+v", r)
		}
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a task on a server that is not the primary ran the changing program")
	}
}

// A name that MongoDB cannot take for a database is a rejected input of the
// input that holds it. mongosh refuses a dot, a space, a slash, a backslash,
// a double quote, and an empty name when it opens the database; the server
// refuses a name of more than 63 bytes with its error 73. Each answer was
// measured on MongoDB 9.0.2.
func TestMongoDBUserDatabaseNames(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	tooLong := "db name must be at most 63 characters, found: 64 (MongoDB's error 73, InvalidNamespace)"
	primary := `{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed",`
	for answer, reason := range map[string]string{
		`{"error":{"step":"open_database","name":"MongoshInvalidInputError","code":"COMMON-10001","code_name":null,"message":"[COMMON-10001] Invalid database name: a.b","database_input":"database","role":null}}`:                                                "mongodb_user.database: MongoDB cannot take this as the name of a database: [COMMON-10001] Invalid database name: a.b",
		`{"error":{"step":"open_database","name":"MongoshInvalidInputError","code":"COMMON-10001","code_name":null,"message":"[COMMON-10001] Invalid database name: a b","database_input":"login_database","role":null}}`:                                          "mongodb_user.login_database: MongoDB cannot take this as the name of a database: [COMMON-10001] Invalid database name: a b",
		`{"error":{"step":"open_database","name":"MongoshInvalidInputError","code":"COMMON-10001","code_name":null,"message":"[COMMON-10001] Invalid database name: a/b","database_input":"roles","role":"read"}}`:                                                 "mongodb_user.roles: MongoDB cannot take the database of the role read as the name of a database: [COMMON-10001] Invalid database name: a/b",
		primary + `"error":{"step":"read_user","name":"MongoServerError","code":73,"code_name":"InvalidNamespace","message":"db name must be at most 63 characters, found: 64","database_input":"database","role":null}}`:                                          "mongodb_user.database: MongoDB cannot take this as the name of a database: " + tooLong,
		primary + `"error":{"step":"log_in","name":"MongoServerError","code":73,"code_name":"InvalidNamespace","message":"db name must be at most 63 characters, found: 64","database_input":"login_database","role":null}}`:                                       "mongodb_user.login_database: MongoDB cannot take this as the name of a database: " + tooLong,
		primary + `"user":null,"roles_that_do_not_exist":[],"error":{"step":"read_roles","name":"MongoServerError","code":73,"code_name":"InvalidNamespace","message":"db name must be at most 63 characters, found: 64","database_input":"roles","role":"read"}}`: "mongodb_user.roles: MongoDB cannot take the database of the role read as the name of a database: " + tooLong,
		// The same name on a server without access control.
		`{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","error":{"step":"read_access_state","name":"MongoServerError","code":73,"code_name":"InvalidNamespace","message":"db name must be at most 63 characters, found: 64","database_input":"database","role":null}}`: "mongodb_user.database: MongoDB cannot take this as the name of a database: " + tooLong,
	} {
		stand.answers(answer, "")
		for _, dryrun := range []bool{true, false} {
			if r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), dryrun); r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != reason {
				t.Errorf("dryrun %v: %+v\nwant the reason %s", dryrun, r, reason)
			}
		}
	}
	// mongosh opens every database that the task names: the user's, the
	// login user's, and each role's.
	stand.answers(mongodbUserWithTwoRoles, "")
	runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"login_database": "operators"}), true)
	program, _ := stand.standardInput("read")
	for _, opened := range []string{`connection.getDB(input.database)`, `connection.getDB(input.login_database)`, `connection.getDB(wanted.db)`} {
		if !strings.Contains(program, opened) {
			t.Errorf("the reading program does not hold %s", opened)
		}
	}
	// The database $external holds users without a password: mongosh
	// refuses the creation of one with a password, and another try would be
	// refused too.
	stand.answers(mongodbNoSuchUser, `{"done":[],"error":{"step":"create_user","name":"MongoshInvalidInputError","code":"COMMON-10001","code_name":null,"message":"[COMMON-10001] Cannot set password for users on the $external database","database_input":"database","role":null}}`)
	r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(map[string]any{"database": "$external", "roles": nil, "roles_not_listed": nil}), false)
	if r.Status != "failed" || r.FailureKind != "config" || r.Reason != "mongodb_user: creating the user shop in the database $external without a role: [COMMON-10001] Cannot set password for users on the $external database" {
		t.Errorf("a user with a password in $external: %+v", r)
	}
}

// A run that was to create the user ends without saying whether it did:
// mongosh fails after it sent the command, or the connection ends before the
// server's answer arrives. Another try would find a user that the server
// created, change nothing, and report created: false. So the module reads
// the server once more, and the task has no other try unless that read
// shows that the user is not there.
func TestMongoDBUserCreationWithUnknownOutcome(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	shop := "the user shop in the database shop"
	creating := "mongodb_user: creating " + shop + " with the roles readWrite on the database shop, read on the database reports: "
	withoutAResult := creating + `mongosh ended with exit code 1 and without a result that the module can read: SyntaxError: Unexpected token, expected "," (1:46)`
	// The answer of the program whose server stopped between the connection
	// and the creation, measured on MongoDB 9.0.2.
	connectionEnded := `{"done":[],"error":{"step":"create_user","name":"MongoServerSelectionError","code":null,"code_name":null,"message":"connect ECONNREFUSED 127.0.0.1:27017","database_input":"database","role":null}}`
	serverGone := `{"error":{"step":"connect","name":"MongoNetworkError","code":null,"code_name":null,"message":"connect ECONNREFUSED 127.0.0.1:27017","database_input":null,"role":null}}`

	for name, c := range map[string]struct {
		changing, afterTheChange string
		noOtherTry               bool
		created                  any
		reason                   string
	}{
		"mongosh failed, and the server has the user": {"", mongodbPasswordNotAsked, true, true,
			withoutAResult + ". The server has " + shop + " now: it created the user before this failure, and the next run compares the user with the task"},
		"mongosh failed, and the server does not have the user": {"", mongodbNoSuchUser, false, nil, withoutAResult},
		"mongosh failed, and the server does not answer the question": {"", serverGone, true, nil,
			withoutAResult + ". The module could not find out whether the server created " + shop + "; the next run compares the user with the task"},
		"the connection ended, and the server does not answer the question": {connectionEnded, serverGone, true, nil,
			creating + "connect ECONNREFUSED 127.0.0.1:27017. The module could not find out whether the server created " + shop + "; the next run compares the user with the task"},
		"the connection ended, and the server does not have the user": {connectionEnded, mongodbNoSuchUser, false, nil, creating + "connect ECONNREFUSED 127.0.0.1:27017"},
	} {
		stand.answers(mongodbNoSuchUser, c.changing)
		if c.changing == "" {
			stand.endsWithoutAResult("change", mongoshCannotReadTheProgram)
		}
		stand.afterTheChange(c.afterTheChange)
		runsBefore := stand.runsOfTheProgram()
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false)
		if r.Status != "failed" || r.FailureKind != "" || r.Change != "changed" || r.NoOtherTry != c.noOtherTry || r.ModuleOutputs["created"] != c.created || r.Reason != c.reason {
			t.Errorf("%s: %+v\nwant the reason %s", name, r, c.reason)
		}
		// The read, the change, and the read after it.
		if runs := stand.runsOfTheProgram() - runsBefore; runs != 3 {
			t.Errorf("%s: %d runs of the program: %q", name, runs, stand.calls())
		}
		holdsNoPassword(t, stand, r)
	}

	// The first administrator: after its creation the server needs a
	// login, and the task's own login reads the user.
	stand.answers(mongodbNoUserYet, "")
	stand.endsWithoutAResult("change", mongoshCannotReadTheProgram)
	stand.afterTheChange(`{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","user":{"roles":[{"role":"root","db":"admin"}]},"roles_that_do_not_exist":[]}`)
	r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false)
	if r.Status != "failed" || !r.NoOtherTry || r.ModuleOutputs["created"] != true || !strings.HasSuffix(r.Reason, ". The server has the user admin in the database admin now: it created the user before this failure, and the next run compares the user with the task") {
		t.Errorf("the first administrator: %+v", r)
	}
	// The server still has no user: the next try creates the administrator.
	stand.answers(mongodbNoUserYet, "")
	stand.endsWithoutAResult("change", mongoshCannotReadTheProgram)
	stand.afterTheChange(mongodbNoUserYet)
	if r := runOfficialExtraModule(t, "mongodb_user", mongodbAdministratorTask(), false); r.Status != "failed" || r.NoOtherTry {
		t.Errorf("the first administrator that the server does not have: %+v", r)
	}

	// A run that only changes a user is tried again: the next try reads the
	// server and does what is still missing, and no output turns wrong.
	stand.answers(mongodbAnotherPassword, "")
	stand.endsWithoutAResult("change", mongoshCannotReadTheProgram)
	runsBefore := stand.runsOfTheProgram()
	r = runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false)
	if r.Status != "failed" || r.NoOtherTry || r.Change != "changed" ||
		r.Reason != `mongodb_user: mongosh ended with exit code 1 and without a result that the module can read: SyntaxError: Unexpected token, expected "," (1:46)` || stand.runsOfTheProgram()-runsBefore != 2 {
		t.Errorf("a change of a user with an unknown outcome: %+v", r)
	}
}

// A role that the server does not have is a missing prerequisite, because an
// earlier node task may create it. So is a user that the server does not
// have for a task without password: a playbook can create the user in one
// task and change its roles in a second one, which has no password. A
// dryrun reports both not checked, and a run fails once.
func TestMongoDBUserMissingRoleAndMissingPassword(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbRoleDoesNotExist, "")
	task := mongodbShopTask(map[string]any{"roles": []any{map[string]any{"role": "readWrite", "database": "shop"}, map[string]any{"role": "noSuchRole", "database": "reports"}}})
	reason := "mongodb_user: the role noSuchRole on the database reports does not exist on this MongoDB server; create it in an earlier node task"
	if r := runOfficialExtraModule(t, "mongodb_user", task, true); r.Status != "not_checked" || r.Reason != reason {
		t.Errorf("the dryrun: %+v", r)
	}
	if r := runOfficialExtraModule(t, "mongodb_user", task, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != reason {
		t.Errorf("the run: %+v", r)
	}
	// The missing user comes first: with it, the role is not looked at.
	delete(task, "password")
	noPassword := "mongodb_user: the user shop in the database shop does not exist, and the task has no password to create it; add password, or create the user in an earlier mongodb_user task"
	if r := runOfficialExtraModule(t, "mongodb_user", task, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != noPassword {
		t.Errorf("a missing user without a password, dryrun: %+v", r)
	}
	if r := runOfficialExtraModule(t, "mongodb_user", task, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != noPassword {
		t.Errorf("a missing user without a password, run: %+v", r)
	}
	if _, ran := stand.standardInput("change"); ran {
		t.Error("a task with a missing role ran the changing program")
	}
}

func TestMongoDBUserFailures(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	shop := "the user shop in the database shop"

	// A read that the server refuses: the login user may not read users.
	// Another try gets the same answer, so the task has one try.
	stand.answers(`{"is_primary":true,"primary":"","replica_set":"","replica_set_note":"","access":"login_needed","error":{"step":"read_user","name":"MongoServerError","code":13,"code_name":"Unauthorized","message":"not authorized on shop to execute command { usersInfo: { user: \"shop\", db: \"shop\" }, lsid: { id: UUID(\"d4adb2a4-1571-41b2-871e-cd91d2cc2f14\") }, $db: \"shop\" }","database_input":"database","role":null}}`, "")
	for _, dryrun := range []bool{true, false} {
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), dryrun)
		if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "mongodb_user: reading "+shop+
			`: not authorized on shop to execute command { usersInfo: { user: "shop", db: "shop" }, lsid: { id: UUID("d4adb2a4-1571-41b2-871e-cd91d2cc2f14") }, $db: "shop" } (MongoDB's error 13, Unauthorized)`+
			"; login_user must be a user that is allowed to manage users" {
			t.Errorf("a refused read (dryrun %v): %+v", dryrun, r)
		}
	}

	// A program that mongosh cannot read: mongosh prints the program's text,
	// with the task's values, behind its first line. Only the first line
	// reaches the reason.
	stand.answers("", "")
	stand.endsWithoutAResult("read", mongoshCannotReadTheProgram)
	r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false)
	if r.Status != "failed" || r.Reason != `mongodb_user: mongosh ended with exit code 1 and without a result that the module can read: SyntaxError: Unexpected token, expected "," (1:46)` {
		t.Errorf("a program that mongosh cannot read: %+v", r)
	}
	holdsNoPassword(t, stand, r)

	// The first change is refused: nothing was done. The user appeared
	// between the read and the change, so another try may find it: an
	// ordinary failure.
	stand.answers(mongodbNoSuchUser, `{"done":[],"error":{"step":"create_user","name":"MongoServerError","code":51003,"code_name":"Location51003","message":"User \"shop@shop\" already exists","database_input":"database","role":null}}`)
	r = runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(nil), false)
	if r.Status != "failed" || r.FailureKind != "" || r.ModuleOutputs["created"] != false || r.Reason != "mongodb_user: creating "+shop+
		` with the roles readWrite on the database shop, read on the database reports: User "shop@shop" already exists (MongoDB's error 51003, Location51003)` {
		t.Errorf("a refused creation: %+v", r)
	}

	// A change fails after an earlier change of the same run: the password
	// is set, and the server refuses the grant to the login user. The result
	// says both, and the task has one try: the login user may not grant at
	// the next try either.
	stand.answers(mongodbAnotherPassword, `{"done":["set_password"],"error":{"step":"grant_roles","name":"MongoServerError","code":13,"code_name":"Unauthorized","message":"not authorized on shop to execute command { grantRolesToUser: \"shop\", roles: [ { role: \"dbAdmin\", db: \"reports\" } ], lsid: { id: UUID(\"194cfae1-075b-4172-bb09-ea2934dd73e3\") }, $db: \"shop\" }","database_input":"database","role":null}}`)
	task := mongodbShopTask(map[string]any{"roles": []any{map[string]any{"role": "dbAdmin", "database": "reports"}}, "roles_not_listed": "keep", "login_user": "limited"})
	r = runOfficialExtraModule(t, "mongodb_user", task, false)
	if r.Status != "failed" || r.Change != "changed" || r.FailureKind != "config" || r.Reason != "mongodb_user: granting the role dbAdmin on the database reports to "+shop+
		`: not authorized on shop to execute command { grantRolesToUser: "shop", roles: [ { role: "dbAdmin", db: "reports" } ], lsid: { id: UUID("194cfae1-075b-4172-bb09-ea2934dd73e3") }, $db: "shop" } (MongoDB's error 13, Unauthorized)`+
		"; login_user must be a user that is allowed to manage users. Before this failure, the task set the password of "+shop {
		t.Errorf("a change that fails after an earlier change: %+v", r)
	}
	holdsNoPassword(t, stand, r)
	// The next run finds the password set and only grants the role.
	stand.answers(mongodbUserWithTwoRoles, `{"done":["grant_roles"]}`)
	task["login_user"] = "admin"
	r = runOfficialExtraModule(t, "mongodb_user", task, false)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "granted the role dbAdmin on the database reports to "+shop {
		t.Errorf("the run after the failure: %+v", r)
	}
	if got := stand.valuesSentTo("change")["changes"]; !reflect.DeepEqual(got, []any{map[string]any{"change": "grant_roles", "roles": []any{map[string]any{"role": "dbAdmin", "db": "reports"}}}}) {
		t.Errorf("the changes of the run after the failure: %v", got)
	}
}

// Passwords from secrets do not hide the task's result, and are in none of
// its texts.
func TestMongoDBUserWithSecretPasswords(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbNoSuchUser, `{"done":[],"error":{"step":"create_user","name":"MongoServerError","code":13,"code_name":"Unauthorized","message":"not authorized on shop to execute command { createUser: \"shop\", pwd: \"xxx\", roles: [ { role: \"readWrite\", db: \"shop\" } ] }","database_input":"database","role":null}}`)
	scope, trace := taskScope(map[string]string{"shop_mongodb_password": mongodbUserPassword, "mongodb_admin_password": mongodbLoginPassword})
	inputs := mongodbShopTask(map[string]any{"password": "{{ .secrets.shop_mongodb_password }}", "login_password": "{{ .secrets.mongodb_admin_password }}"})
	r := reportedByTheEngine(Runtime{}.Starlark(testContext(t), "mongodb_user", officialExtraModule(t, "mongodb_user"), inputs, scope), scope)
	if r.Secret || trace.Used || r.Status != "failed" || !strings.Contains(r.Reason, `pwd: "xxx"`) {
		t.Errorf("a failure of a task with secret passwords: %+v", r)
	}
	holdsNoPassword(t, stand, r)
	// The passwords reached mongosh, through its standard input only.
	if change := stand.valuesSentTo("change"); change["password"] != mongodbUserPassword || change["login_password"] != mongodbLoginPassword {
		t.Errorf("the passwords of the changing program: %v", change)
	}
}

// The module's page shows, under "Set up", the playbook, the inventory, and
// the settings template of the example project word for word. The integration suite runs
// the example project, so the page's node tasks are tested.
func TestMongoDBUserPageShowsTheExampleProject(t *testing.T) {
	t.Parallel()
	example := filepath.Join(officialExtraModulesDirectory, "..", "examples", "mongodb-server-for-one-application")
	if _, err := os.Stat(example); os.IsNotExist(err) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	page, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, "mongodb_user", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"playbooks/configure.yml", "inventories/example.yml", "templates/mongod.conf"} {
		text, err := os.ReadFile(filepath.Join(example, file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(page), "```yaml\n"+string(text)+"```\n") {
			t.Errorf("README.md of mongodb_user does not show %s of the example project as it is", file)
		}
	}
}

// login_user is a private value, as the passwords are: a secret in it does
// not hide the task's result, and MMX shows [redacted] where a text of the
// result holds the name. The reason of a refused login keeps its meaning.
func TestMongoDBUserWithASecretLoginUser(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	secrets := map[string]string{"mongodb_operator": "operator-of-the-shop", "mongodb_operator_password": mongodbLoginPassword}
	inputs := mongodbShopTask(map[string]any{"login_user": "{{ .secrets.mongodb_operator }}", "login_password": "{{ .secrets.mongodb_operator_password }}", "login_database": "operators"})

	stand.answers(mongodbLoginRefused, "")
	scope, trace := taskScope(secrets)
	r := reportedByTheEngine(Runtime{Dryrun: true}.Starlark(testContext(t), "mongodb_user", officialExtraModule(t, "mongodb_user"), inputs, scope), scope)
	if r.Secret || trace.Used || r.FailureKind != "config" ||
		r.Reason != "mongodb_user: the MongoDB server refused the login of the user [redacted] in the database operators; check login_user, login_database, and login_password" {
		t.Errorf("a refused login of a secret login user: %+v", r)
	}
	if login := stand.valuesSentTo("read")["login_user"]; login != "operator-of-the-shop" {
		t.Errorf("the login user of the reading program: %v", login)
	}

	// A task that works names only its own user, so nothing is replaced.
	stand.answers(mongodbNoSuchUser, `{"done":["create_user"]}`)
	scope, _ = taskScope(secrets)
	r = reportedByTheEngine(Runtime{}.Starlark(testContext(t), "mongodb_user", officialExtraModule(t, "mongodb_user"), inputs, scope), scope)
	if r.Secret || r.Status != "succeeded" || r.Reason != "created the user shop in the database shop with the roles readWrite on the database shop, read on the database reports" {
		t.Errorf("a created user with a secret login user: %+v", r)
	}
}

// The fingerprint is the state that the task asks for. A password and a
// login do not change it; the order of the roles does not either.
func TestMongoDBUserFingerprint(t *testing.T) {
	t.Parallel()
	stand := standInForMongosh(t)
	stand.answers(mongodbUserWithTwoRoles, "")
	fingerprint := func(changed map[string]any) string {
		t.Helper()
		r := runOfficialExtraModule(t, "mongodb_user", mongodbShopTask(changed), true)
		if r.Status != "succeeded" || r.Fingerprint == "" {
			t.Fatalf("%+v", r)
		}
		return r.Fingerprint
	}
	role := func(name, database string) map[string]any { return map[string]any{"role": name, "database": database} }
	same := fingerprint(nil)
	for name, changed := range map[string]map[string]any{
		"another login":              {"login_user": "operator", "login_password": "Made-Up-Operator-1"},
		"another password":           {"password": "Made-Up-Other-1"},
		"the roles in another order": {"roles": []any{role("read", "reports"), role("readWrite", "shop")}},
	} {
		if fingerprint(changed) != same {
			t.Errorf("%s changed the fingerprint", name)
		}
	}
	for name, changed := range map[string]map[string]any{
		"another role":             {"roles": []any{role("readWrite", "shop")}},
		"another roles_not_listed": {"roles_not_listed": "keep"},
		"another user":             {"name": "orders"},
	} {
		if fingerprint(changed) == same {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
}
