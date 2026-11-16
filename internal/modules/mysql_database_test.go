package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestMySQLDatabase(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		database := MySQLDatabase{Name: "shop-db", CharacterSet: "utf8mb4", Collation: "utf8mb4_unicode_ci"}
		// A dryrun says what a run would do and sends no statement.
		r := f.dryrun().MySQLDatabase(ctx, database, value.Scope{}, nil)
		if created, _ := mysqlResult(t, r, "succeeded", "would_change"); !created || r.Reason != "would create the database shop-db" || f.statements() != nil {
			t.Fatalf("%s: %+v", version, r)
		}
		// The first run creates the database, with the name in backticks.
		r = f.run().MySQLDatabase(ctx, database, value.Scope{}, nil)
		if created, attributes := mysqlResult(t, r, "succeeded", "changed"); !created || attributes != nil {
			t.Fatalf("%s: %+v", version, r)
		}
		if got := f.statements(); !reflect.DeepEqual(got, []string{"CREATE DATABASE `shop-db` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"}) {
			t.Fatal(got)
		}
		if got := f.state().Databases["shop-db"]; got != (standInMySQLDatabase{"utf8mb4", "utf8mb4_unicode_ci"}) {
			t.Fatal(got)
		}
		// The second run and a dryrun find it and send nothing.
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			r = run.MySQLDatabase(ctx, database, value.Scope{}, nil)
			if created, _ := mysqlResult(t, r, "succeeded", "unchanged"); created || f.statements() != nil {
				t.Fatalf("%s: %+v", version, r)
			}
		}
		// Removal: a dryrun, the run, and a database that is already gone.
		gone := MySQLDatabase{Name: "shop-db", State: "deleted_with_all_its_data"}
		r = f.dryrun().MySQLDatabase(ctx, gone, value.Scope{}, nil)
		if mysqlResult(t, r, "succeeded", "would_change"); r.Reason != "would drop the database shop-db with all its data" || f.statements() != nil {
			t.Fatalf("%+v", r)
		}
		r = f.run().MySQLDatabase(ctx, gone, value.Scope{}, nil)
		if created, _ := mysqlResult(t, r, "succeeded", "changed"); created || r.Reason != "dropped the database shop-db with all its data" || !reflect.DeepEqual(f.statements(), []string{"DROP DATABASE `shop-db`"}) {
			t.Fatalf("%+v", r)
		}
		if _, exists := f.state().Databases["shop-db"]; exists {
			t.Fatal("the database is still there")
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, run.MySQLDatabase(ctx, gone, value.Scope{}, nil), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("a removal of a missing database sent a statement")
			}
		}
		// A task without character_set and collation leaves both to the
		// server.
		if r = f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "plain"}, value.Scope{}, nil); !reflect.DeepEqual(f.statements(), []string{"CREATE DATABASE `plain`"}) {
			t.Fatalf("%+v", r)
		}
	}
}

// An existing database whose character set or collation differs fails the
// task, in a run and in a dryrun, and nothing changes.
func TestMySQLDatabaseThatDiffersIsNotChanged(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newMySQLFixture(t, standInMariaDBVersion)
	f.change(func(s *standInMySQL) {
		s.Databases["shop"] = standInMySQLDatabase{"latin1", "latin1_swedish_ci"}
		s.Databases["old"] = standInMySQLDatabase{"utf8", "utf8_general_ci"}
	})
	runs := 0
	for name, c := range map[string]struct {
		database MySQLDatabase
		want     string
	}{
		"another character set":           {MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4"}, "the task asks for the character set utf8mb4."},
		"another collation":               {MySQLDatabase{Name: "shop", Collation: "latin1_general_ci"}, "the task asks for the collation latin1_general_ci."},
		"both":                            {MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4", Collation: "utf8mb4_unicode_ci"}, "the task asks for the character set utf8mb4 and the collation utf8mb4_unicode_ci."},
		"the same character set":          {MySQLDatabase{Name: "shop", CharacterSet: "latin1"}, ""},
		"both the same, in another case":  {MySQLDatabase{Name: "shop", CharacterSet: "LATIN1", Collation: "Latin1_Swedish_CI"}, ""},
		"utf8mb3 for a database in utf8":  {MySQLDatabase{Name: "old", CharacterSet: "utf8mb3", Collation: "utf8mb3_general_ci"}, ""},
		"utf8 for a database in utf8":     {MySQLDatabase{Name: "old", CharacterSet: "utf8"}, ""},
		"utf8mb4 for a database in utf8":  {MySQLDatabase{Name: "old", CharacterSet: "utf8mb4"}, "the task asks for the character set utf8mb4."},
		"no character set and collation":  {MySQLDatabase{Name: "shop"}, ""},
		"another collation of utf8, both": {MySQLDatabase{Name: "old", CharacterSet: "utf8mb3", Collation: "utf8mb3_unicode_ci"}, "the task asks for the collation utf8mb3_unicode_ci."},
	} {
		// A run and a dryrun compare in the same way; the cases take turns.
		run := f.run()
		if runs++; runs%2 == 0 {
			run = f.dryrun()
		}
		r := run.MySQLDatabase(ctx, c.database, value.Scope{}, nil)
		if c.want == "" {
			mysqlResult(t, r, "succeeded", "unchanged")
			continue
		}
		if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || !strings.Contains(r.Reason, c.want) ||
			!strings.HasPrefix(r.Reason, "mysql_database: the database "+c.database.Name+" exists with the character set ") || !strings.Contains(r.Reason, "MMX does not change them on an existing database") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if f.statements() != nil {
		t.Fatal("a statement was sent")
	}
	// The same failure in a run and in a dryrun.
	for _, run := range []Runtime{f.run(), f.dryrun()} {
		if r := run.MySQLDatabase(ctx, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4"}, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestMySQLDatabaseRejectsInputs(t *testing.T) {
	t.Parallel()
	for want, database := range map[string]MySQLDatabase{
		"mysql_database.name is required":                                                                    {},
		"state must be present or deleted_with_all_its_data":                                                 {Name: "shop", State: "absent"},
		"state: deleted_with_all_its_data accepts only name and the connection fields; remove character_set": {Name: "shop", State: "deleted_with_all_its_data", CharacterSet: "utf8mb4"},
		"state: deleted_with_all_its_data accepts only name and the connection fields; remove collation":     {Name: "shop", State: "deleted_with_all_its_data", Collation: "utf8mb4_bin"},
		"database_server_port needs database_server_host":                                                    {Name: "shop", DatabaseServerPort: 3307},
		"database_server_host needs login_password":                                                          {Name: "shop", DatabaseServerHost: "db.example.com"},
		"database_server_certificate_authority_file needs database_server_host":                              {Name: "shop", DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.pem"},
	} {
		if err := database.Prepare(""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	// The rules of the inputs that may hold a reference.
	rules := func(database MySQLDatabase) map[string]func(any) error {
		var arguments ArgumentList
		database.Arguments(&arguments)
		found := map[string]func(any) error{}
		for _, argument := range arguments.List {
			found[argument.Field] = argument.Rule
		}
		return found
	}
	present := rules(MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4", Collation: "utf8mb4_bin", DatabaseServerHost: "db.example.com", DatabaseServerPort: "{{ .vars.database_port }}",
		DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.pem", LoginUser: "admin", LoginPassword: "x"})
	if err := present["database_server_port"]("3307"); err == nil || err.Error() != "database_server_port must be a number from 1 to 65535" {
		t.Error(err)
	}
	if err := present["database_server_port"](3307); err != nil {
		t.Error(err)
	}
	for field, values := range map[string]map[string]string{
		"name":                 {"": "name must not be empty", "line\nfeed": "must not hold control characters"},
		"character_set":        {"utf8mb4; DROP DATABASE mysql": "character_set must be a name of letters, digits, and underscores", "": "character_set must be a name"},
		"collation":            {"utf8mb4-bin": "collation must be a name of letters, digits, and underscores"},
		"database_server_host": {"db example": "database_server_host must be a host name or an address"},
		"database_server_certificate_authority_file": {"authority.pem": "path must be absolute"},
		"login_user": {"": "login_user must not be empty"},
		// A reference that resolved to nothing, such as a secret that is not
		// set.
		"login_password": {"": "login_password must not be empty and must not hold a zero byte", "a\x00b": "login_password must not be empty and must not hold a zero byte"},
	} {
		for written, want := range values {
			if err := present[field](written); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s %q: want %q, got %v", field, written, want, err)
			}
		}
	}
	for field, written := range map[string]string{"name": "shop-db `x` ; -- 日本", "character_set": "utf8mb4", "collation": "utf8mb4_0900_ai_ci", "database_server_host": "db.example.com", "login_password": "x y"} {
		if err := present[field](written); err != nil {
			t.Errorf("%s %q: %v", field, written, err)
		}
	}
	// No task deletes one of the server's own databases; one may be named
	// where nothing is deleted.
	deleted := rules(MySQLDatabase{Name: "mysql", State: "deleted_with_all_its_data"})
	for _, name := range []string{"mysql", "information_schema", "performance_schema", "sys", "MySQL"} {
		if err := deleted["name"](name); err == nil || !strings.Contains(err.Error(), "is one of the server's own databases, without which the server does not work; MMX does not delete it") {
			t.Errorf("%s: %v", name, err)
		}
		if err := present["name"](name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := deleted["name"]("shop"); err != nil {
		t.Fatal(err)
	}
}

// A statement that fails leaves the task failed with the server's message;
// the catalog says whether something changed, and the next try finishes.
func TestMySQLDatabaseFailureAndAnotherTry(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newMySQLFixture(t, standInMySQLVersion)
	database := MySQLDatabase{Name: "shop"}
	f.change(func(s *standInMySQL) {
		s.Fail, s.FailOnce = map[string]string{"CREATE DATABASE": "ERROR 1044 (42000): Access denied for user 'deploy'@'localhost' to database 'shop'"}, true
	})
	r := f.run().MySQLDatabase(ctx, database, value.Scope{}, nil)
	if want := "mysql_database: CREATE DATABASE `shop` on the database server on this node: ERROR 1044 (42000) at line 2: Access denied for user 'deploy'@'localhost' to database 'shop'"; r.Status != "failed" || r.Change != "unchanged" || r.Reason != want {
		t.Fatalf("%+v\nwant %s", r, want)
	}
	if created, _ := mysqlResult(t, f.run().MySQLDatabase(ctx, database, value.Scope{}, nil), "succeeded", "changed"); !created {
		t.Fatal("the second try did not create the database")
	}
	// A removal that the server refuses.
	f.change(func(s *standInMySQL) {
		s.Fail, s.FailOnce = map[string]string{"DROP DATABASE": "ERROR 1010 (HY000): Error dropping database (can't rmdir './shop', errno: 39)"}, true
	})
	gone := MySQLDatabase{Name: "shop", State: "deleted_with_all_its_data"}
	if r = f.run().MySQLDatabase(ctx, gone, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "ERROR 1010 (HY000) at line 2: Error dropping database") {
		t.Fatalf("%+v", r)
	}
	mysqlResult(t, f.run().MySQLDatabase(ctx, gone, value.Scope{}, nil), "succeeded", "changed")
}

// A statement that changes the server waits ten seconds for a lock of another
// session, where the server's own setting is a day or a year: the stand-in
// refuses a changing statement of a call that did not set the wait. The
// server's error 1205 is an ordinary failure, which is tried again, and
// nothing changed.
func TestMySQLChangingStatementDoesNotWaitWithoutEndForALock(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	const waited = "ERROR 1205 (HY000) at line 2: Lock wait timeout exceeded; try restarting transaction; another session of the server holds a lock that the statement needs, such as an open transaction on a table, and MMX waits 10 seconds for it"
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		f.change(func(s *standInMySQL) {
			s.Databases["shop"] = standInMySQLDatabase{"utf8mb4", "utf8mb4_general_ci"}
			s.Fail = map[string]string{"DROP DATABASE": "ERROR 1205 (HY000): Lock wait timeout exceeded; try restarting transaction", "GRANT": "ERROR 1205 (HY000): Lock wait timeout exceeded; try restarting transaction"}
		})
		gone := MySQLDatabase{Name: "shop", State: "deleted_with_all_its_data"}
		r := f.run().MySQLDatabase(ctx, gone, value.Scope{}, nil)
		if r.Status != "failed" || r.FailureKind != "" || r.Change != "unchanged" || !strings.HasPrefix(r.Reason, "mysql_database: DROP DATABASE `shop` on the database server on this node: ") || !strings.HasSuffix(r.Reason, waited) {
			t.Fatalf("%s: %+v", version, r)
		}
		if _, exists := f.state().Databases["shop"]; !exists {
			t.Fatal("the database is gone")
		}
		password := "a password"
		user := MySQLUser{Name: "shop", MayConnectFrom: "%", Password: &password, Privileges: []MySQLPrivileges{{GrantOn: "shop.*", Grant: []string{"SELECT"}}}}
		r = f.run().MySQLUser(ctx, user, value.Scope{}, nil)
		if created, _ := mysqlResult(t, r, "failed", "changed"); !created || r.FailureKind != "" || !strings.HasPrefix(r.Reason, "mysql_user: changing the privileges of shop@% on the database server on this node: ") || !strings.HasSuffix(r.Reason, waited) {
			t.Fatalf("%s: %+v", version, r)
		}
		// The other session ended: the next try finishes both.
		f.change(func(s *standInMySQL) { s.Fail = nil })
		mysqlResult(t, f.run().MySQLUser(ctx, user, value.Scope{}, nil), "succeeded", "changed")
		mysqlResult(t, f.run().MySQLDatabase(ctx, gone, value.Scope{}, nil), "succeeded", "changed")
	}
}

// A character set or a collation that the server does not have, and a
// collation of another character set, fail the task before anything changes,
// in a run and in a dryrun, and are not tried again. The server's catalog
// decides, so MySQL's collation is unknown to MariaDB and MariaDB's to MySQL.
func TestMySQLDatabaseOfACharacterSetThatTheServerDoesNotHave(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	const mariaDB105 = "10.5.29-MariaDB"
	for name, c := range map[string]struct {
		version  string
		database MySQLDatabase
		want     string
	}{
		"character set": {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf9"},
			"mysql_database.character_set: the database server on this node has no character set utf9; its statement SHOW CHARACTER SET lists the names, such as utf8mb4"},
		"character set, MySQL": {standInMySQLVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf9", Collation: "utf8mb4_bin"},
			"mysql_database.character_set: the database server on this node has no character set utf9; its statement SHOW CHARACTER SET lists the names, such as utf8mb4"},
		"collation": {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4", Collation: "utf8mb4_nosuch_ci"},
			"mysql_database.collation: the database server on this node has no collation utf8mb4_nosuch_ci; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci"},
		"a part of a name": {standInMariaDBVersion, MySQLDatabase{Name: "shop", Collation: "general_ci"},
			"mysql_database.collation: the database server on this node has no collation general_ci; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci"},
		"MySQL's collation on MariaDB 10.5": {mariaDB105, MySQLDatabase{Name: "shop", Collation: "utf8mb4_0900_ai_ci"},
			"mysql_database.collation: the database server on this node has no collation utf8mb4_0900_ai_ci; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci"},
		"MariaDB's short name on MySQL": {standInMySQLVersion, MySQLDatabase{Name: "shop", Collation: "uca1400_ai_ci"},
			"mysql_database.collation: the database server on this node has no collation uca1400_ai_ci; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci"},
		"MariaDB's short name on MariaDB 10.5": {mariaDB105, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4", Collation: "uca1400_ai_ci"},
			"mysql_database.collation: the database server on this node has no collation uca1400_ai_ci; its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci"},
		"a collation of another character set": {standInMySQLVersion, MySQLDatabase{Name: "shop", CharacterSet: "latin1", Collation: "utf8mb4_unicode_ci"},
			"mysql_database.collation: on the database server on this node, utf8mb4_unicode_ci is no collation of the character set latin1; its statement SHOW COLLATION WHERE Charset = 'latin1' lists the collations of that character set"},
		"a short name for a character set without it": {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "latin1", Collation: "uca1400_ai_ci"},
			"mysql_database.collation: on the database server on this node, uca1400_ai_ci is no collation of the character set latin1; its statement SHOW COLLATION WHERE Charset = 'latin1' lists the collations of that character set"},
		// The same answer for a database that exists.
		"a database that exists": {standInMariaDBVersion, MySQLDatabase{Name: "there", CharacterSet: "utf9"},
			"mysql_database.character_set: the database server on this node has no character set utf9; its statement SHOW CHARACTER SET lists the names, such as utf8mb4"},
	} {
		f := newMySQLFixture(t, c.version)
		f.change(func(s *standInMySQL) { s.Databases["there"] = standInMySQLDatabase{"utf8mb4", "utf8mb4_general_ci"} })
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			r := run.MySQLDatabase(ctx, c.database, value.Scope{}, nil)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != c.want {
				t.Errorf("%s: %+v\nwant %s", name, r, c.want)
			}
		}
		if f.statements() != nil {
			t.Errorf("%s: a statement was sent: %v", name, f.statements())
		}
	}
}

// The names that a server has more than one form of: utf8 and utf8mb3 are
// one character set on every server, whichever of the two its catalog
// writes; MariaDB takes the short name of a uca1400 collation and stores the
// name with the character set. Each task creates its database, and the
// second run finds it unchanged.
func TestMySQLDatabaseNamesWithTwoForms(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	const mariaDB105 = "10.5.29-MariaDB"
	for name, c := range map[string]struct {
		version  string
		database MySQLDatabase
		stored   standInMySQLDatabase
	}{
		"utf8 on a server that says utf8mb3":    {standInMySQLVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8", Collation: "utf8_general_ci"}, standInMySQLDatabase{"utf8mb3", "utf8mb3_general_ci"}},
		"utf8mb3 on a server that says utf8":    {mariaDB105, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb3", Collation: "utf8mb3_unicode_ci"}, standInMySQLDatabase{"utf8", "utf8_unicode_ci"}},
		"utf8 on a server that says utf8":       {mariaDB105, MySQLDatabase{Name: "shop", CharacterSet: "UTF8"}, standInMySQLDatabase{"utf8", "utf8_general_ci"}},
		"both names in one task":                {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8", Collation: "utf8mb3_general_ci"}, standInMySQLDatabase{"utf8mb3", "utf8mb3_general_ci"}},
		"the short name alone":                  {standInMariaDBVersion, MySQLDatabase{Name: "shop", Collation: "uca1400_ai_ci"}, standInMySQLDatabase{"utf8mb4", "utf8mb4_uca1400_ai_ci"}},
		"the short name with its character set": {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb4", Collation: "UCA1400_AI_CI"}, standInMySQLDatabase{"utf8mb4", "utf8mb4_uca1400_ai_ci"}},
		"the short name with utf8":              {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8", Collation: "uca1400_ai_ci"}, standInMySQLDatabase{"utf8mb3", "utf8mb3_uca1400_ai_ci"}},
		"the name with the character set":       {standInMariaDBVersion, MySQLDatabase{Name: "shop", Collation: "utf8mb4_uca1400_ai_ci"}, standInMySQLDatabase{"utf8mb4", "utf8mb4_uca1400_ai_ci"}},
		"both, with the character set":          {standInMariaDBVersion, MySQLDatabase{Name: "shop", CharacterSet: "utf8mb3", Collation: "utf8mb3_uca1400_ai_ci"}, standInMySQLDatabase{"utf8mb3", "utf8mb3_uca1400_ai_ci"}},
	} {
		f := newMySQLFixture(t, c.version)
		if created, _ := mysqlResult(t, f.run().MySQLDatabase(ctx, c.database, value.Scope{}, nil), "succeeded", "changed"); !created || f.state().Databases["shop"] != c.stored {
			t.Errorf("%s: the server stored %+v", name, f.state().Databases["shop"])
		}
		f.statements()
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if r := run.MySQLDatabase(ctx, c.database, value.Scope{}, nil); r.Status != "succeeded" || r.Change != "unchanged" || f.statements() != nil {
				t.Errorf("%s, the second time: %+v", name, r)
			}
		}
	}
	// A database with another collation than the short name's still differs.
	f := newMySQLFixture(t, standInMariaDBVersion)
	f.change(func(s *standInMySQL) { s.Databases["shop"] = standInMySQLDatabase{"utf8mb4", "utf8mb4_unicode_ci"} })
	r := f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "shop", Collation: "uca1400_ai_ci"}, value.Scope{}, nil)
	if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "exists with the character set utf8mb4 and the collation utf8mb4_unicode_ci, and the task asks for the collation uca1400_ai_ci.") {
		t.Fatalf("%+v", r)
	}
}

// Without the client program, and with a server that does not answer, a run
// fails and a dryrun is not checked: an earlier node task may install the
// one and start the other. A refused login fails both. The kind of each
// failure decides whether the node task execution engine tries again: a
// server that does not answer is an ordinary failure, which is tried again;
// a refused login is a configuration failure, which is not, and so is a
// server whose certificate the client does not accept (TestMySQLLoginFile).
func TestMySQLNeedsTheClientAndAServerThatAnswers(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newMySQLFixture(t, standInMariaDBVersion)
	database := MySQLDatabase{Name: "shop"}
	user := MySQLUser{Name: "shop", MayConnectFrom: "%"}
	f.change(func(s *standInMySQL) { s.Down = true })
	r := f.run().MySQLDatabase(ctx, database, value.Scope{}, nil)
	if want := "mysql_database: the database server on this node does not answer: ERROR 2002 (HY000): Can't connect to local server through socket '' (2); start the server in an earlier node task, for example with systemd_service"; r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "" || r.Reason != want {
		t.Fatalf("%+v\nwant %s", r, want)
	}
	if r = f.dryrun().MySQLDatabase(ctx, database, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || !strings.Contains(r.Reason, "does not answer") {
		t.Fatalf("%+v", r)
	}
	if r = f.dryrun().MySQLUser(ctx, user, value.Scope{}, nil); r.Status != "not_checked" || !strings.HasPrefix(r.Reason, "mysql_user: the database server on this node does not answer") {
		t.Fatalf("%+v", r)
	}
	// The server refuses the node's root: a failure also in a dryrun.
	f.change(func(s *standInMySQL) { s.Down, s.RootNeedsAPassword = false, true })
	for _, run := range []Runtime{f.run(), f.dryrun()} {
		r = run.MySQLDatabase(ctx, database, value.Scope{}, nil)
		if want := "mysql_database: the database server on this node refused the login: ERROR 1698 (28000): Access denied for user 'root'@'localhost'; the server does not accept the node's root account as root@localhost through its socket; give the task login_user and login_password"; r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != want {
			t.Fatalf("%+v\nwant %s", r, want)
		}
	}
	// Without the client program: the failure names the client's package
	// for the node's package manager.
	for _, name := range []string{"mariadb", "mysql"} {
		os.Remove(filepath.Join(f.dir, name))
	}
	for _, manager := range []string{"apt", "zypper"} {
		missing := "neither the program mariadb nor the program mysql is installed; each comes with its database server's package, and a server on another machine needs the client alone. " + missingProgramError("mariadb").Error()
		packages := NewPackageManager(manager)
		if r = f.run().MySQLDatabase(ctx, database, value.Scope{}, packages); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "mysql_database: "+missing {
			t.Fatalf("%s: %+v", manager, r)
		}
		if r = f.run().MySQLUser(ctx, user, value.Scope{}, packages); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "mysql_user: "+missing {
			t.Fatalf("%s: %+v", manager, r)
		}
		for _, r := range []protocol.Result{f.dryrun().MySQLDatabase(ctx, database, value.Scope{}, packages), f.dryrun().MySQLUser(ctx, user, value.Scope{}, packages)} {
			if r.Status != "not_checked" || r.Change != "unknown" || !strings.HasSuffix(r.Reason, missing) {
				t.Fatalf("%s: %+v", manager, r)
			}
		}
	}
	// A node whose package manager MMX does not know.
	if r = f.run().MySQLDatabase(ctx, database, value.Scope{}, nil); !strings.HasSuffix(r.Reason, missingProgramError("mariadb").Error()) {
		t.Fatalf("%+v", r)
	}
}

// How the client is called: with an empty environment, with no option file
// of the node, with its input as SQL only, and as root through the server's
// socket, whose path comes from the server's configuration when that moved
// it.
func TestMySQLClientReadsOnlyWhatTheTaskSays(t *testing.T) {
	ctx := testContext(t)
	for version, client := range map[string]string{standInMariaDBVersion: "mariadb", standInMySQLVersion: "mysql"} {
		f := newMySQLFixture(t, version)
		// What would send a plain call of the client somewhere else.
		for name, text := range map[string]string{"MYSQL_HOST": "192.0.2.1", "MYSQL_PWD": "from-the-environment", "MYSQL_UNIX_PORT": "/run/other.sock", "HOME": "/root", "MYSQL_TEST_LOGIN_FILE": "/root/.mylogin.cnf"} {
			t.Setenv(name, text)
		}
		mysqlResult(t, f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "shop"}, value.Scope{}, nil), "succeeded", "changed")
		calls := takeCalls(t, f.dir)
		wantCall := client + " --no-defaults --user=root --batch --skip-column-names --binary-mode --default-character-set=utf8mb4"
		wantGroups := "my_print_defaults mysqld server"
		if client == "mariadb" {
			wantGroups += " mariadb mariadbd client-server"
		}
		// The database's row before, the statement, and the row after.
		if !reflect.DeepEqual(calls, []string{wantGroups, wantCall, wantCall, wantCall}) {
			t.Fatalf("%s: %q", version, calls)
		}
		environment := readState(t, filepath.Join(f.dir, "environment"))
		for _, name := range []string{"MYSQL_HOST", "MYSQL_PWD", "MYSQL_UNIX_PORT", "HOME=", "MYSQL_TEST_LOGIN_FILE"} {
			if strings.Contains(environment, name) {
				t.Fatalf("%s: the client's environment holds %s:\n%s", version, name, environment)
			}
		}
		if !strings.Contains(environment, "PATH=/usr/sbin:/usr/bin:/sbin:/bin\n") {
			t.Fatalf("the client's environment has another PATH:\n%s", environment)
		}
		// A server whose configuration moved the socket: my_print_defaults
		// prints the options in the order in which the server reads them,
		// so the last socket line is the server's.
		f.change(func(s *standInMySQL) { s.Socket = "/run/mysqld/moved.sock" })
		if r := f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "shop"}, value.Scope{}, nil); r.Status != "failed" || !strings.Contains(r.Reason, "does not answer") {
			t.Fatalf("%+v", r)
		}
		writeState(t, filepath.Join(f.dir, "my-print-defaults"), "--socket=/run/mysqld/mysqld.sock\n--pid-file=/run/mysqld/mysqld.pid\n--password=Not-for-a-result\n--socket=/run/mysqld/moved.sock\n--bind-address=127.0.0.1\n")
		takeCalls(t, f.dir)
		mysqlResult(t, f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "shop"}, value.Scope{}, nil), "succeeded", "unchanged")
		if calls := takeCalls(t, f.dir); !strings.HasPrefix(calls[1], client+" --no-defaults --user=root --socket=/run/mysqld/moved.sock --batch") {
			t.Fatalf("%q", calls)
		}
		// A socket line that is no absolute path is not taken.
		writeState(t, filepath.Join(f.dir, "my-print-defaults"), "--socket=relative.sock\n")
		if r := f.run().MySQLDatabase(ctx, MySQLDatabase{Name: "shop"}, value.Scope{}, nil); r.Status != "failed" || strings.Contains(strings.Join(takeCalls(t, f.dir), " "), "--socket") {
			t.Fatalf("%+v", r)
		}
	}
}

// With connection fields the login goes into a file in the task's private
// directory, in a dryrun too, because the dryrun reads the server's catalog
// with it. The password is on no command line and in no environment, and
// the directory is gone afterwards.
func TestMySQLLoginFile(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	const password = `sentinel "pass" \word #1`
	for version, verify := range map[string]string{standInMariaDBVersion: "ssl-verify-server-cert\n", standInMySQLVersion: "ssl-mode=VERIFY_IDENTITY\n"} {
		f := newMySQLFixture(t, version)
		authority := filepath.Join(f.dir, "authority.pem")
		writeState(t, authority, "the authority's certificate\n")
		writeState(t, filepath.Join(f.dir, "other-authority.pem"), "another authority's certificate\n")
		f.change(func(s *standInMySQL) {
			s.Host, s.Logins, s.AuthorityCertificate = "db.example.com", map[string]string{"shop-admin": password}, "the authority's certificate\n"
			s.ClientRefusesAServerItCannotCheck = true
			// The local socket must not be what answers.
			s.Socket = "/nowhere"
		})
		database := MySQLDatabase{Name: "shop", DatabaseServerHost: "db.example.com", DatabaseServerPort: 3307, LoginUser: "shop-admin", LoginPassword: "{{ .secrets.login }}",
			DatabaseServerCertificateAuthorityFile: authority}
		secrets := map[string]string{"login": password}
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			s, trace := taskScope(secrets)
			r := reportedByTheEngine(run.MySQLDatabase(ctx, database, s, nil), s)
			if r.Status != "succeeded" || trace.Used {
				t.Fatalf("%s: %+v, the login marked the trace: %v", version, r, trace.Used)
			}
			wantFile := "[client]\nuser=\"shop-admin\"\npassword=\"sentinel \\\"pass\\\" \\\\word #1\"\nhost=\"db.example.com\"\nport=3307\nprotocol=tcp\nconnect-timeout=10\nssl-ca=\"" + authority + "\"\n" + verify
			if got := readState(t, filepath.Join(f.dir, "login-file")); got != wantFile {
				t.Fatalf("%s: the login file is\n%s\nwant\n%s", version, got, wantFile)
			}
			if got := readState(t, filepath.Join(f.dir, "login-file-modes")); got != "0600 0700" {
				t.Fatalf("the login file and its directory have the modes %s", got)
			}
			for _, call := range takeCalls(t, f.dir) {
				if !strings.Contains(call, " --defaults-file="+f.privateTaskDirectoryRoot+"/mysql_database-") || strings.Contains(call, "--no-defaults") {
					t.Fatalf("%s", call)
				}
			}
			f.nowhereOutsideTheLoginFile("sentinel")
		}
		// Another authority's certificate: the client refuses the server,
		// and the failure says what the field is for.
		other := database
		other.DatabaseServerCertificateAuthorityFile = filepath.Join(f.dir, "other-authority.pem")
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			s, _ := taskScope(secrets)
			r := reportedByTheEngine(run.MySQLDatabase(ctx, other, s, nil), s)
			if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != "mysql_database: the client could not establish a TLS connection to the database server at db.example.com, port 3307: ERROR 2026 (HY000): TLS/SSL error: self-signed certificate in certificate chain; For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity." {
				t.Fatalf("%+v", r)
			}
		}
		// An authority file that is not on the node yet.
		missing := database
		missing.DatabaseServerCertificateAuthorityFile = filepath.Join(f.dir, "not-installed.pem")
		s, _ := taskScope(secrets)
		if r := f.run().MySQLDatabase(ctx, missing, s, nil); r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, "database_server_certificate_authority_file: "+missing.DatabaseServerCertificateAuthorityFile+" is not a file on the node") {
			t.Fatalf("%+v", r)
		}
		if r := f.dryrun().MySQLDatabase(ctx, missing, s, nil); r.Status != "not_checked" {
			t.Fatalf("%+v", r)
		}
		// No CA file keeps verification on for both client products.
		without := database
		without.DatabaseServerCertificateAuthorityFile = ""
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			s, _ = taskScope(secrets)
			r := reportedByTheEngine(run.MySQLDatabase(ctx, without, s, nil), s)
			if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "ERROR 2026") || !strings.HasSuffix(r.Reason, "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity.") {
				t.Fatalf("%s: %+v", version, r)
			}
		}
		without.DangerouslySkipServerCertificateCheck = true
		if err := without.Prepare(""); err != nil {
			t.Fatal(err)
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			s, _ = taskScope(secrets)
			if r := reportedByTheEngine(run.MySQLDatabase(ctx, without, s, nil), s); r.Status != "succeeded" {
				t.Fatalf("%s: %+v", version, r)
			}
		}

		// A wrong password: the server's message, without the password; a
		// failure also in a dryrun.
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			s, _ = taskScope(map[string]string{"login": "wrong " + password})
			r := reportedByTheEngine(run.MySQLDatabase(ctx, database, s, nil), s)
			if want := "mysql_database: the database server at db.example.com, port 3307 refused the login: ERROR 1045 (28000): Access denied for user 'shop-admin'@'localhost' (using password: YES); check login_user and login_password"; r.Status != "failed" || r.FailureKind != "config" || r.Reason != want {
				t.Fatalf("%+v\nwant %s", r, want)
			}
		}
		// A server that does not answer at that address.
		f.change(func(s *standInMySQL) { s.Down = true })
		s, _ = taskScope(secrets)
		if r := f.run().MySQLDatabase(ctx, database, s, nil); r.Status != "failed" || r.FailureKind != "" || !strings.Contains(r.Reason, "the database server at db.example.com, port 3307 does not answer: ERROR 200") || !strings.HasSuffix(r.Reason, "check database_server_host and database_server_port, and that the server accepts connections from this node") {
			t.Fatalf("%+v", r)
		}
		if r := f.dryrun().MySQLDatabase(ctx, database, s, nil); r.Status != "not_checked" {
			t.Fatalf("%+v", r)
		}
		f.nowhereOutsideTheLoginFile("sentinel")
	}
}

// login_user and login_password without database_server_host: a login with
// a password through the server's socket, as for a MySQL server whose root
// has a password.
func TestMySQLPasswordLoginThroughTheSocket(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newMySQLFixture(t, standInMySQLVersion)
	f.change(func(s *standInMySQL) {
		s.RootNeedsAPassword, s.Logins, s.Socket = true, map[string]string{"root": "root-password"}, "/var/lib/mysql/moved.sock"
	})
	writeState(t, filepath.Join(f.dir, "my-print-defaults"), "--socket=/var/lib/mysql/moved.sock\n")
	database := MySQLDatabase{Name: "shop", LoginPassword: "{{ .secrets.root }}"}
	s, _ := taskScope(map[string]string{"root": "root-password"})
	mysqlResult(t, f.run().MySQLDatabase(ctx, database, s, nil), "succeeded", "changed")
	if got, want := readState(t, filepath.Join(f.dir, "login-file")), "[client]\nuser=\"root\"\npassword=\"root-password\"\nsocket=\"/var/lib/mysql/moved.sock\"\nprotocol=socket\n"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	f.nowhereOutsideTheLoginFile("root-password")
}
