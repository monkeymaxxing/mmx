package modules

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// text returns a pointer to a task's password as it is written.
func writtenPassword(written string) *string { return &written }

// mysqlTestPassword is the password of the tests, a secret of the task's
// scope, and mysqlTestSecrets the scope's secrets.
const mysqlTestPassword = `Sentinel "pass" \word-1x`

var mysqlTestSecrets = map[string]string{"shop_database_password": mysqlTestPassword}

// shopUser is the task of the design's example.
func shopUser() MySQLUser {
	return MySQLUser{Name: "shop", MayConnectFrom: "10.0.%", Password: writtenPassword("{{ .secrets.shop_database_password }}"), PrivilegesNotListed: "remove",
		Privileges: []MySQLPrivileges{{GrantOn: "shop.*", Grant: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}}}}
}

// applyUser runs the task as the node task execution engine does: with the
// secrets in the task's scope, and without the logins in what the task
// reports. It fails the test when the password marked the task's trace,
// which would hide the result.
func (f *mysqlFixture) applyUser(run Runtime, user MySQLUser) protocol.Result {
	f.t.Helper()
	s, trace := taskScope(mysqlTestSecrets)
	r := reportedByTheEngine(run.MySQLUser(testContext(f.t), user, s, nil), s)
	if trace.Used {
		f.t.Fatal("the password marked the task's trace, which hides the task's result")
	}
	if text, _ := json.Marshal(r); strings.Contains(string(text), "Sentinel") {
		f.t.Fatalf("the result holds the password: %s", text)
	}
	return r
}

// A first run creates the account from the stored value of the server's own
// method and grants the privileges; the second run and a dryrun find
// everything and send nothing.
func TestMySQLUser(t *testing.T) {
	t.Parallel()
	const account = "`shop`@`10.0.%`"
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		f.change(func(s *standInMySQL) { s.Variables[mysqlSettingRounds] = "10000" })
		user := shopUser()
		r := f.applyUser(f.dryrun(), user)
		created, attributes := mysqlResult(t, r, "succeeded", "would_change")
		if !created || attributes != nil || r.Reason != "would create the account shop@10.0.%; would grant DELETE, INSERT, SELECT, UPDATE on shop.*" || f.statements() != nil {
			t.Fatalf("%s: %+v", version, r)
		}
		r = f.applyUser(f.run(), user)
		if created, attributes = mysqlResult(t, r, "succeeded", "changed"); !created || attributes != nil || r.Reason != "created the account shop@10.0.%; granted DELETE, INSERT, SELECT, UPDATE on shop.*" {
			t.Fatalf("%s: %+v", version, r)
		}
		made := f.account("shop@10.0.%")
		statements := f.statements()
		if len(statements) != 2 || statements[1] != "GRANT DELETE, INSERT, SELECT, UPDATE ON `shop`.* TO "+account {
			t.Fatalf("%s: %q", version, statements)
		}
		// Each product gets its own words and its own method, and only the
		// stored value: never the password.
		if strings.Contains(version, "MariaDB") {
			if want := "CREATE USER " + account + " IDENTIFIED VIA mysql_native_password USING '" + mysqlNativePassword(mysqlTestPassword) + "'"; statements[0] != want || made.Plugin != "mysql_native_password" {
				t.Fatalf("%s\nwant %s", statements[0], want)
			}
		} else {
			salt, rounds, known := mysqlCachingSHA2Parts([]byte(made.Stored))
			if !known || rounds != 10000 || strings.Trim(string(salt), lettersAndDigits) != "" || made.Stored != mysqlCachingSHA2Password(mysqlTestPassword, salt, rounds) || made.Plugin != "caching_sha2_password" {
				t.Fatalf("the stored value %q is not the password's with a salt of letters and digits and the server's 10000 rounds", made.Stored)
			}
			if want := "CREATE USER " + account + " IDENTIFIED WITH caching_sha2_password AS '" + made.Stored + "'"; statements[0] != want {
				t.Fatalf("%s\nwant %s", statements[0], want)
			}
		}
		if got := f.held("shop@10.0.%", "database\tshop"); !reflect.DeepEqual(got, []string{"DELETE", "INSERT", "SELECT", "UPDATE"}) {
			t.Fatal(got)
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			r = f.applyUser(run, user)
			if created, attributes = mysqlResult(t, r, "succeeded", "unchanged"); created || attributes != nil || r.Reason != "" || f.statements() != nil {
				t.Fatalf("%s: %+v", version, r)
			}
		}
		// The host part is compared in lower case, as the servers store it.
		upper := user
		upper.MayConnectFrom = "10.0.%"
		upper.Name = "shop"
		upper.MayConnectFrom = strings.ToUpper("db.Example.COM")
		mysqlResult(t, f.applyUser(f.run(), upper), "succeeded", "changed")
		if f.account("shop@db.example.com") == nil {
			t.Fatal("the account's host part was not put in lower case")
		}
		f.statements()
		mysqlResult(t, f.applyUser(f.run(), upper), "succeeded", "unchanged")

		// Removal, and an account that is already gone.
		gone := MySQLUser{Name: "shop", MayConnectFrom: "10.0.%", State: "absent"}
		r = f.applyUser(f.dryrun(), gone)
		if mysqlResult(t, r, "succeeded", "would_change"); r.Reason != "would remove the account shop@10.0.%" || f.statements() != nil {
			t.Fatalf("%+v", r)
		}
		r = f.applyUser(f.run(), gone)
		if mysqlResult(t, r, "succeeded", "changed"); r.Reason != "removed the account shop@10.0.%" || !reflect.DeepEqual(f.statements(), []string{"DROP USER " + account}) || f.account("shop@10.0.%") != nil {
			t.Fatalf("the account was not removed: %+v", r)
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, f.applyUser(run, gone), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("a removal of a missing account sent a statement")
			}
		}
		// An account without a password is open to everyone who can reach
		// the server, so a task that would create one must say that it
		// means it: without the written permission it fails, in a dryrun
		// and in a run, and creates nothing.
		plain := MySQLUser{Name: "monitor", MayConnectFrom: "localhost", PrivilegesNotListed: "keep", Privileges: []MySQLPrivileges{{GrantOn: "*.*", Grant: []string{"PROCESS"}}}}
		const open = "mysql_user: creating account monitor@localhost without a password lets any machine in may_connect_from log in with all its privileges. Add password, or add allow_creating_account_without_password: true to create the account without a password"
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			if r = f.applyUser(run, plain); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != open || f.statements() != nil || f.account("monitor@localhost") != nil {
				t.Fatalf("%s: %+v", version, r)
			}
		}
		plain = MySQLUser{Name: "monitor", MayConnectFrom: "localhost", AllowCreatingAccountWithoutPassword: true}
		if r = f.applyUser(f.dryrun(), plain); r.Reason != "would create the account monitor@localhost without a password" {
			t.Fatalf("%+v", r)
		}
		if r = f.applyUser(f.run(), plain); r.Reason != "created the account monitor@localhost without a password" || !reflect.DeepEqual(f.statements(), []string{"CREATE USER `monitor`@`localhost`"}) {
			t.Fatalf("%+v", r)
		}
		mysqlResult(t, f.applyUser(f.run(), plain), "succeeded", "unchanged")
		// For an account that exists the permission changes nothing, and
		// neither does its absence: MMX does not touch the password.
		plain.AllowCreatingAccountWithoutPassword = false
		mysqlResult(t, f.applyUser(f.run(), plain), "succeeded", "unchanged")
		existing := MySQLUser{Name: "shop", MayConnectFrom: "db.example.com", AllowCreatingAccountWithoutPassword: true}
		before := f.account("shop@db.example.com").Stored
		if mysqlResult(t, f.applyUser(f.run(), existing), "succeeded", "unchanged"); f.account("shop@db.example.com").Stored != before || before == "" || f.statements() != nil {
			t.Fatal("the permission changed the password of an account that exists")
		}
	}
}

// The password is compared without a login: MMX computes the stored value
// with the account's own method, salt, and rounds. A password that differs
// is set with the method that the account has.
func TestMySQLUserPassword(t *testing.T) {
	t.Parallel()
	// A salt as the servers make it: any byte from 1 to 127.
	serverSalt := []byte("\x4D\x13\x66\x08\x23\x15\x5E\x07\x1C\x76\x3B\x43\x1B\x25\x1D\x48\x78\x43\x6C\x27")
	user := MySQLUser{Name: "shop", MayConnectFrom: "%", Password: writtenPassword("{{ .secrets.shop_database_password }}")}
	for name, c := range map[string]struct {
		version, plugin, stored string
		changed                 bool
	}{
		"MySQL, the same password with the server's salt and 5000 rounds": {standInMySQLVersion, "caching_sha2_password", mysqlCachingSHA2Password(mysqlTestPassword, serverSalt, 5000), false},
		"MySQL, the same password with 10000 rounds":                      {standInMySQLVersion, "caching_sha2_password", mysqlCachingSHA2Password(mysqlTestPassword, serverSalt, 10000), false},
		"MySQL, another password":                                         {standInMySQLVersion, "caching_sha2_password", mysqlCachingSHA2Password("another", serverSalt, 5000), true},
		"MySQL, an account without a password":                            {standInMySQLVersion, "caching_sha2_password", "", true},
		"MySQL, an account with mysql_native_password, same":              {standInMySQLVersion, "mysql_native_password", mysqlNativePassword(mysqlTestPassword), false},
		"MySQL, an account with mysql_native_password, another":           {standInMySQLVersion, "mysql_native_password", mysqlNativePassword("another"), true},
		"MariaDB, the same password":                                      {standInMariaDBVersion, "mysql_native_password", mysqlNativePassword(mysqlTestPassword), false},
		"MariaDB, the same password in lower case digits":                 {standInMariaDBVersion, "mysql_native_password", strings.ToLower(mysqlNativePassword(mysqlTestPassword)), false},
		"MariaDB, another password":                                       {standInMariaDBVersion, "mysql_native_password", mysqlNativePassword("another"), true},
		"MariaDB, an account without a password":                          {standInMariaDBVersion, "mysql_native_password", "", true},
		"MariaDB 12 with the caching_sha2_password plugin, another":       {standInMariaDBVersion, "caching_sha2_password", mysqlCachingSHA2Password("another", serverSalt, 5000), true},
	} {
		f := newMySQLFixture(t, c.version)
		f.change(func(s *standInMySQL) {
			s.Accounts["shop@%"] = &standInMySQLAccount{Plugin: c.plugin, Stored: c.stored, Privileges: map[string]*standInMySQLPrivileges{"database\tshop": {Names: []string{"SELECT"}}}}
		})
		// A run and a dryrun compare in the same way: a dryrun that finds
		// the password unchanged or changed is what a run would find.
		r := f.applyUser(f.dryrun(), user)
		if !c.changed {
			if mysqlResult(t, r, "succeeded", "unchanged"); f.statements() != nil {
				t.Fatalf("%s: a statement was sent", name)
			}
			continue
		}
		if created, attributes := mysqlResult(t, r, "succeeded", "would_change"); created || !reflect.DeepEqual(attributes, []string{"password"}) || r.Reason != "would set the password" || f.statements() != nil {
			t.Fatalf("%s: %+v", name, r)
		}
		r = f.applyUser(f.run(), user)
		if created, attributes := mysqlResult(t, r, "succeeded", "changed"); created || !reflect.DeepEqual(attributes, []string{"password"}) || r.Reason != "set the password" {
			t.Fatalf("%s: %+v", name, r)
		}
		// The account keeps its method, and the statement is in the words
		// of the server's product.
		after := f.account("shop@%")
		words := "IDENTIFIED WITH " + c.plugin + " AS '"
		if strings.Contains(c.version, "MariaDB") {
			words = "IDENTIFIED VIA " + c.plugin + " USING '"
		}
		if statements := f.statements(); len(statements) != 1 || statements[0] != "ALTER USER `shop`@`%` "+words+after.Stored+"'" || after.Plugin != c.plugin {
			t.Fatalf("%s: %q", name, statements)
		}
		if same, err := mysqlPasswordIs(after.Plugin, after.Stored, mysqlTestPassword); err != nil || !same {
			t.Fatalf("%s: the new stored value %q is not the password's: %v", name, after.Stored, err)
		}
		// The privileges of a task without privileges are not touched.
		if got := f.held("shop@%", "database\tshop"); !reflect.DeepEqual(got, []string{"SELECT"}) {
			t.Fatal(got)
		}
	}
}

// A task with password fails, in a run and in a dryrun, for an account whose
// password MMX cannot compare, and for a server that refuses stored values.
func TestMySQLUserPasswordThatCannotBeCompared(t *testing.T) {
	t.Parallel()
	user := MySQLUser{Name: "root", MayConnectFrom: "localhost", Password: writtenPassword("{{ .secrets.shop_database_password }}")}
	for name, c := range map[string]struct {
		version string
		account standInMySQLAccount
		want    string
	}{
		"root of MariaDB, which may also log in through the socket": {standInMariaDBVersion, standInMySQLAccount{Plugin: "mysql_native_password", Stored: "invalid", AuthOr: `[{},{"plugin":"unix_socket"}]`},
			"mysql_user: the account root@localhost logs in with one of several authentication methods (mysql_native_password or unix_socket), so a password would not decide how it logs in; MMX sets a password only for an account with the one method mysql_native_password or caching_sha2_password. Remove password from the task to manage only the account's privileges"},
		"root of Ubuntu's MySQL": {standInMySQLVersion, standInMySQLAccount{Plugin: "auth_socket"},
			"mysql_user: the account root@localhost logs in with the authentication method auth_socket; MMX compares and sets a password only for the methods mysql_native_password and caching_sha2_password. Remove password from the task to manage only the account's privileges"},
		"a stored value in an unknown form": {standInMySQLVersion, standInMySQLAccount{Plugin: "caching_sha2_password", Stored: "$B$005$" + strings.Repeat("x", 63)},
			"mysql_user: the account root@localhost: the account's stored caching_sha2_password value has an unsupported format: the value must start with $A$ and three hexadecimal digits and be 70 characters long"},
	} {
		f := newMySQLFixture(t, c.version)
		f.change(func(s *standInMySQL) { s.Accounts["root@localhost"] = &c.account })
		for _, run := range []Runtime{f.dryrun(), f.run()} {
			r := f.applyUser(run, user)
			if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != c.want {
				t.Errorf("%s: %+v\nwant %s", name, r, c.want)
			}
		}
		// Without password the task manages the account's privileges.
		if c.account.AuthOr != "" {
			privileges := MySQLUser{Name: "root", MayConnectFrom: "localhost", PrivilegesNotListed: "keep", Privileges: []MySQLPrivileges{{GrantOn: "*.*", Grant: []string{"PROCESS"}}}}
			mysqlResult(t, f.applyUser(f.run(), privileges), "succeeded", "changed")
			if got := f.statements(); !reflect.DeepEqual(got, []string{"GRANT PROCESS ON *.* TO `root`@`localhost`"}) {
				t.Fatal(name, got)
			}
		}
	}
	// A MariaDB server with a password check plugin refuses every stored
	// value while strict_password_validation is on.
	f := newMySQLFixture(t, standInMariaDBVersion)
	f.change(func(s *standInMySQL) { s.Variables["strict_password_validation"], s.PasswordCheckPlugins = "ON", 1 })
	const strict = "mysql_user: the database server on this node checks passwords with a plugin and its setting strict_password_validation is on, so it refuses every stored password value: it can check only a clear password. MMX does not send the clear password, because MariaDB writes it into its general log. Turn strict_password_validation off in the server's configuration, or remove password from the task"
	for _, run := range []Runtime{f.dryrun(), f.run()} {
		if r := f.applyUser(run, shopUser()); r.Status != "failed" || r.FailureKind != "config" || r.Reason != strict || f.statements() != nil {
			t.Fatalf("%+v", r)
		}
	}
	// With the setting off, or without a plugin, the server takes the value.
	f.change(func(s *standInMySQL) { s.Variables["strict_password_validation"] = "OFF" })
	mysqlResult(t, f.applyUser(f.run(), shopUser()), "succeeded", "changed")
}

// privileges_not_listed decides what happens to what the list does not
// name: keep only adds; remove leaves the account with exactly the list,
// also at places that on cannot name.
func TestMySQLUserPrivileges(t *testing.T) {
	t.Parallel()
	const account = "`shop`@`%`"
	existing := func(f *mysqlFixture) {
		f.change(func(s *standInMySQL) {
			s.Tables = []string{"shop.orders", "shop.audit"}
			s.Accounts["shop@%"] = &standInMySQLAccount{Plugin: "mysql_native_password", Privileges: map[string]*standInMySQLPrivileges{
				"server":                                 {Names: []string{"PROCESS"}},
				"database\tshop":                         {Names: []string{"DROP", "SELECT"}, GrantOption: true},
				"database\told\\_shop":                   {Names: []string{"SELECT"}},
				"table\tshop\torders":                    {Names: []string{"INSERT", "SELECT"}},
				"table\tshop\taudit":                     {GrantOption: true},
				"column\tshop\taudit\tid":                {Names: []string{"SELECT"}},
				"column\tshop\taudit\twho":               {Names: []string{"SELECT", "UPDATE"}},
				"routine\tshop\tone\tFUNCTION":           {Names: []string{"EXECUTE"}},
				"routine\tshop\tcount_orders\tPROCEDURE": {Names: []string{"ALTER ROUTINE", "EXECUTE"}, GrantOption: true},
			}}
		})
	}
	task := MySQLUser{Name: "shop", MayConnectFrom: "%", Privileges: []MySQLPrivileges{
		{GrantOn: "shop.*", Grant: []string{"select", " insert ", "Create  Temporary Tables"}},
		{GrantOn: "shop.orders", Grant: []string{"SELECT", "UPDATE", "GRANT OPTION"}},
	}}
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		// keep: only what is missing is granted.
		f := newMySQLFixture(t, version)
		existing(f)
		keep := task
		keep.PrivilegesNotListed = "keep"
		r := f.applyUser(f.dryrun(), keep)
		_, attributes := mysqlResult(t, r, "succeeded", "would_change")
		if want := "would grant CREATE TEMPORARY TABLES, INSERT on shop.*; would grant UPDATE on shop.orders; would grant GRANT OPTION on shop.orders"; !reflect.DeepEqual(attributes, []string{"privileges"}) || r.Reason != want || f.statements() != nil {
			t.Fatalf("%s: %+v\nwant %s", version, r, want)
		}
		r = f.applyUser(f.run(), keep)
		if _, attributes = mysqlResult(t, r, "succeeded", "changed"); !reflect.DeepEqual(attributes, []string{"privileges"}) || r.Reason != "granted CREATE TEMPORARY TABLES, INSERT on shop.*; granted UPDATE on shop.orders; granted GRANT OPTION on shop.orders" {
			t.Fatalf("%s: %+v", version, r)
		}
		if got, want := f.statements(), []string{"GRANT CREATE TEMPORARY TABLES, INSERT ON `shop`.* TO " + account, "GRANT UPDATE ON `shop`.`orders` TO " + account, "GRANT GRANT OPTION ON `shop`.`orders` TO " + account}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%q", got)
		}
		if got := f.held("shop@%", "database\tshop"); !reflect.DeepEqual(got, []string{"CREATE TEMPORARY TABLES", "DROP", "GRANT OPTION", "INSERT", "SELECT"}) {
			t.Fatal(got)
		}
		if got := f.held("shop@%", "server"); !reflect.DeepEqual(got, []string{"PROCESS"}) {
			t.Fatal("keep took a privilege away", got)
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, f.applyUser(run, keep), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("an unchanged rerun sent a statement")
			}
		}

		// remove: the grants first, then everything that the list does not
		// name, the grant option of a place before its privileges.
		f = newMySQLFixture(t, version)
		existing(f)
		remove := task
		remove.PrivilegesNotListed = "remove"
		wantStatements := []string{
			"GRANT CREATE TEMPORARY TABLES, INSERT ON `shop`.* TO " + account,
			"GRANT UPDATE ON `shop`.`orders` TO " + account,
			"GRANT GRANT OPTION ON `shop`.`orders` TO " + account,
			"REVOKE PROCESS ON *.* FROM " + account,
			"REVOKE SELECT ON `old\\_shop`.* FROM " + account,
			"REVOKE GRANT OPTION ON `shop`.* FROM " + account,
			"REVOKE DROP ON `shop`.* FROM " + account,
			"REVOKE GRANT OPTION ON `shop`.`audit` FROM " + account,
			"REVOKE INSERT ON `shop`.`orders` FROM " + account,
			"REVOKE SELECT (`id`) ON `shop`.`audit` FROM " + account,
			"REVOKE SELECT (`who`), UPDATE (`who`) ON `shop`.`audit` FROM " + account,
			"REVOKE GRANT OPTION ON PROCEDURE `shop`.`count_orders` FROM " + account,
			"REVOKE ALTER ROUTINE, EXECUTE ON PROCEDURE `shop`.`count_orders` FROM " + account,
			"REVOKE EXECUTE ON FUNCTION `shop`.`one` FROM " + account,
		}
		r = f.applyUser(f.dryrun(), remove)
		if want := "would grant CREATE TEMPORARY TABLES, INSERT on shop.*; would grant UPDATE on shop.orders; would grant GRANT OPTION on shop.orders; would revoke PROCESS on *.*; would revoke SELECT on old\\_shop.*; " +
			"would revoke GRANT OPTION on shop.*; would revoke DROP on shop.*; would revoke GRANT OPTION on shop.audit; would revoke INSERT on shop.orders; would revoke SELECT on column id of shop.audit; would revoke SELECT, UPDATE on column who of shop.audit; " +
			"would revoke GRANT OPTION on procedure shop.count_orders; would revoke ALTER ROUTINE, EXECUTE on procedure shop.count_orders; would revoke EXECUTE on function shop.one"; r.Reason != want || f.statements() != nil {
			t.Fatalf("%s:\n got %s\nwant %s", version, r.Reason, want)
		}
		mysqlResult(t, f.applyUser(f.run(), remove), "succeeded", "changed")
		if got := f.statements(); !reflect.DeepEqual(got, wantStatements) {
			t.Fatalf("%s:\n got %q\nwant %q", version, got, wantStatements)
		}
		// The account holds exactly the list.
		left := f.account("shop@%").Privileges
		if len(left) != 2 || !reflect.DeepEqual(f.held("shop@%", "database\tshop"), []string{"CREATE TEMPORARY TABLES", "INSERT", "SELECT"}) || !reflect.DeepEqual(f.held("shop@%", "table\tshop\torders"), []string{"GRANT OPTION", "SELECT", "UPDATE"}) {
			t.Fatalf("%s: the account holds %+v", version, left)
		}
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, f.applyUser(run, remove), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("an unchanged rerun sent a statement")
			}
		}
		// An empty list with remove takes everything away.
		none := MySQLUser{Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "remove"}
		mysqlResult(t, f.applyUser(f.run(), none), "succeeded", "changed")
		if left := f.account("shop@%").Privileges; len(left) != 0 {
			t.Fatalf("the account still holds %+v", left)
		}
	}
}

// ALL means every privilege that the server has for the place. MMX keeps no
// list of its own: it compares the number of the account's privileges with
// the server's, so a newer server with one more privilege gets ALL again.
func TestMySQLUserAllPrivileges(t *testing.T) {
	t.Parallel()
	const account = "`shop`@`%`"
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		f.change(func(s *standInMySQL) { s.Tables = []string{"shop.orders"} })
		user := MySQLUser{Name: "shop", MayConnectFrom: "%", AllowCreatingAccountWithoutPassword: true, PrivilegesNotListed: "remove", Privileges: []MySQLPrivileges{
			{GrantOn: "*.*", Grant: []string{"all privileges"}}, {GrantOn: "shop.*", Grant: []string{"ALL", "GRANT OPTION"}}, {GrantOn: "shop.orders", Grant: []string{"ALL"}}}}
		r := f.applyUser(f.run(), user)
		if mysqlResult(t, r, "succeeded", "changed"); r.Reason != "created the account shop@% without a password; granted ALL PRIVILEGES on *.*; granted ALL PRIVILEGES on shop.*; granted GRANT OPTION on shop.*; granted ALL PRIVILEGES on shop.orders" {
			t.Fatalf("%+v", r)
		}
		state := f.state()
		if got := f.held("shop@%", "server"); len(got) != len(state.ServerPrivileges) {
			t.Fatal(got)
		}
		if got := f.held("shop@%", "database\tshop"); len(got) != len(state.DatabasePrivileges)+1 {
			t.Fatal(got)
		}
		if got := f.held("shop@%", "table\tshop\torders"); len(got) != len(state.TablePrivileges) {
			t.Fatal(got)
		}
		f.statements()
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, f.applyUser(run, user), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("an unchanged rerun sent a statement")
			}
		}
		// The server's next version has one more privilege for a database.
		f.change(func(s *standInMySQL) { s.DatabasePrivileges = append(s.DatabasePrivileges, "SHOW VIEW") })
		if r = f.applyUser(f.dryrun(), user); r.Reason != "would grant ALL PRIVILEGES on shop.*" {
			t.Fatalf("%+v", r)
		}
		mysqlResult(t, f.applyUser(f.run(), user), "succeeded", "changed")
		if got := f.statements(); !reflect.DeepEqual(got, []string{"GRANT ALL PRIVILEGES ON `shop`.* TO " + account}) {
			t.Fatal(got)
		}
		// A list of names in place of ALL, with remove, takes the others.
		user.Privileges = []MySQLPrivileges{{GrantOn: "shop.*", Grant: []string{"SELECT"}}}
		mysqlResult(t, f.applyUser(f.run(), user), "succeeded", "changed")
		if got := f.account("shop@%").Privileges; len(got) != 1 || !reflect.DeepEqual(f.held("shop@%", "database\tshop"), []string{"SELECT"}) {
			t.Fatalf("%+v", got)
		}
	}
}

// MySQL keeps the grant option of each dynamic privilege (such as
// BACKUP_ADMIN) with that privilege. After REVOKE GRANT OPTION ON *.* its
// catalog keeps showing such a privilege with the option, also when the
// privilege itself is revoked after that; only the grant table is right.
// MMX reads the dynamic privileges from the grant table, so the plain
// statements, the same as for MariaDB, leave the account as the task says,
// and the next run finds nothing to do. The stand-in server has the fault.
func TestMySQLDynamicPrivilegesComeFromTheGrantTable(t *testing.T) {
	t.Parallel()
	const account = "`admin`@`%`"
	f := newMySQLFixture(t, standInMySQLVersion)
	f.change(func(s *standInMySQL) { s.DynamicPrivileges = []string{"BACKUP_ADMIN", "XA_RECOVER_ADMIN"} })
	task := func(grant ...string) MySQLUser {
		return MySQLUser{Name: "admin", MayConnectFrom: "%", Password: writtenPassword("{{ .secrets.shop_database_password }}"), PrivilegesNotListed: "remove",
			Privileges: []MySQLPrivileges{{GrantOn: "*.*", Grant: grant}}}
	}
	apply := func(user MySQLUser, statements ...string) {
		t.Helper()
		f.statements()
		mysqlResult(t, f.applyUser(f.run(), user), "succeeded", "changed")
		if got := f.statements(); !reflect.DeepEqual(got, statements) {
			t.Fatalf("got  %q\nwant %q", got, statements)
		}
		// The catalog's stale rows make no later run or dryrun find work.
		for _, run := range []Runtime{f.run(), f.dryrun()} {
			if mysqlResult(t, f.applyUser(run, user), "succeeded", "unchanged"); f.statements() != nil {
				t.Fatal("an unchanged rerun sent a statement")
			}
		}
	}
	// ALL with the grant option: every privilege of the server, the dynamic
	// ones too, and the option also for them.
	mysqlResult(t, f.applyUser(f.run(), task("ALL", "GRANT OPTION")), "succeeded", "changed")
	server := func() *standInMySQLPrivileges { return f.account("admin@%").Privileges["server"] }
	if got := server(); len(got.Names) != len(f.state().ServerPrivileges)+2 || !got.GrantOption || !reflect.DeepEqual(got.DynamicWithGrantOption, []string{"BACKUP_ADMIN", "XA_RECOVER_ADMIN"}) {
		t.Fatalf("%+v", got)
	}
	// Only the option goes: one plain statement, and no privilege is
	// revoked on the way, so an account that manages itself keeps what it
	// needs.
	apply(task("ALL"), "REVOKE GRANT OPTION ON *.* FROM "+account)
	if got := server(); got.GrantOption || got.DynamicWithGrantOption != nil || !reflect.DeepEqual(got.StaleInTheCatalog, []string{"BACKUP_ADMIN", "XA_RECOVER_ADMIN"}) {
		t.Fatalf("the stand-in's catalog is not stale as MySQL's is: %+v", got)
	}
	// Two privileges stay, one of them dynamic. The catalog still shows the
	// other dynamic privilege with the option; the grant table does not.
	revoked := slices.DeleteFunc(slices.Clone(server().Names), func(name string) bool { return name == "BACKUP_ADMIN" || name == "SELECT" })
	apply(task("SELECT", "BACKUP_ADMIN"), "REVOKE "+strings.Join(revoked, ", ")+" ON *.* FROM "+account)
	if got := f.held("admin@%", "server"); !reflect.DeepEqual(got, []string{"BACKUP_ADMIN", "SELECT"}) {
		t.Fatal(got)
	}
	// The option again, and then one more dynamic privilege: it comes
	// without the option, so the task asks for the option once more.
	apply(task("SELECT", "BACKUP_ADMIN", "GRANT OPTION"), "GRANT GRANT OPTION ON *.* TO "+account)
	apply(task("SELECT", "BACKUP_ADMIN", "XA_RECOVER_ADMIN", "GRANT OPTION"), "GRANT XA_RECOVER_ADMIN ON *.* TO "+account, "GRANT GRANT OPTION ON *.* TO "+account)
	if got := server(); !reflect.DeepEqual(got.DynamicWithGrantOption, []string{"BACKUP_ADMIN", "XA_RECOVER_ADMIN"}) {
		t.Fatalf("%+v", got)
	}
	// A privilege that carries no option of its own needs no second
	// statement.
	apply(task("SELECT", "BACKUP_ADMIN", "XA_RECOVER_ADMIN", "PROCESS", "GRANT OPTION"), "GRANT PROCESS ON *.* TO "+account)
	// A dynamic privilege that someone granted without the option, beside
	// the option for everything else: the task asks for the option.
	f.change(func(s *standInMySQL) {
		s.Accounts["admin@%"].Privileges["server"].DynamicWithGrantOption = []string{"BACKUP_ADMIN"}
	})
	apply(task("SELECT", "BACKUP_ADMIN", "XA_RECOVER_ADMIN", "PROCESS", "GRANT OPTION"), "GRANT GRANT OPTION ON *.* TO "+account)
	// Everything goes, in the plain order: the option, then the names.
	apply(MySQLUser{Name: "admin", MayConnectFrom: "%", PrivilegesNotListed: "remove"},
		"REVOKE GRANT OPTION ON *.* FROM "+account, "REVOKE BACKUP_ADMIN, PROCESS, SELECT, XA_RECOVER_ADMIN ON *.* FROM "+account)
	if got := f.account("admin@%").Privileges["server"]; got == nil || len(got.Names) != 0 || len(got.StaleInTheCatalog) != 2 {
		t.Fatalf("%+v", got)
	}
}

// What privileges_not_listed: remove cannot revoke, and a table that does
// not exist yet: a dryrun is not checked, and a run changes nothing.
func TestMySQLUserPrivilegesThatCannotBeChecked(t *testing.T) {
	t.Parallel()
	f := newMySQLFixture(t, standInMySQLVersion)
	f.change(func(s *standInMySQL) {
		s.Accounts["shop@%"] = &standInMySQLAccount{Plugin: "caching_sha2_password", Privileges: map[string]*standInMySQLPrivileges{"routine\tshop\ttools\tLIBRARY": {Names: []string{"EXECUTE"}}}}
	})
	remove := MySQLUser{Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "remove"}
	const unknownKind = "mysql_user: the account shop@%: the account holds privileges on library shop.tools, a kind of stored routine that MMX does not know how to revoke; revoke them yourself, or use privileges_not_listed: keep"
	if r := f.applyUser(f.dryrun(), remove); r.Status != "not_checked" || r.Reason != unknownKind {
		t.Fatalf("%+v", r)
	}
	if r := f.applyUser(f.run(), remove); r.Status != "failed" || r.Change != "unchanged" || r.Reason != unknownKind || f.statements() != nil {
		t.Fatalf("%+v", r)
	}
	// With keep the unknown kind does not matter.
	keep := MySQLUser{Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "keep", Privileges: []MySQLPrivileges{{GrantOn: "shop.*", Grant: []string{"SELECT"}}}}
	mysqlResult(t, f.applyUser(f.run(), keep), "succeeded", "changed")
	f.statements()
	// A privilege on a routine that MMX does not know.
	f.change(func(s *standInMySQL) {
		s.Accounts["shop@%"].Privileges = map[string]*standInMySQLPrivileges{"routine\tshop\tone\tFUNCTION": {Names: []string{"DEBUG ROUTINE"}}}
	})
	if r := f.applyUser(f.run(), remove); r.Status != "failed" || !strings.Contains(r.Reason, "the account holds the privilege DEBUG ROUTINE on function shop.one, which MMX does not know how to revoke") {
		t.Fatalf("%+v", r)
	}
	// A name in the catalog that is no privilege's name goes into no
	// statement.
	f.change(func(s *standInMySQL) {
		s.Accounts["shop@%"].Privileges = map[string]*standInMySQLPrivileges{"database\tshop": {Names: []string{"SELECT ON *.* FROM x; DROP"}}}
	})
	if r := f.applyUser(f.run(), remove); r.Status != "failed" || !strings.Contains(r.Reason, `the account holds a privilege on shop.* whose name, "SELECT ON *.* FROM x; DROP", is not one that MMX can write into a statement`) || f.statements() != nil {
		t.Fatalf("%+v", r)
	}
	// The servers grant on a table only when it exists, unless CREATE is
	// among the privileges: an earlier node task may create the table.
	f.change(func(s *standInMySQL) { s.Accounts["shop@%"].Privileges = map[string]*standInMySQLPrivileges{} })
	table := MySQLUser{Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "keep", Privileges: []MySQLPrivileges{{GrantOn: "shop.orders", Grant: []string{"SELECT"}}}}
	const noTable = "mysql_user: the table shop.orders does not exist; the server requires an existing table unless the requested privileges include CREATE; create the table in an earlier node task"
	if r := f.applyUser(f.dryrun(), table); r.Status != "not_checked" || r.Reason != noTable {
		t.Fatalf("%+v", r)
	}
	if r := f.applyUser(f.run(), table); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != noTable || f.statements() != nil {
		t.Fatalf("%+v", r)
	}
	for _, grant := range [][]string{{"SELECT", "CREATE"}, {"ALL"}} {
		table.Privileges[0].Grant = grant
		mysqlResult(t, f.applyUser(f.run(), table), "succeeded", "changed")
	}
	f.change(func(s *standInMySQL) { s.Tables = []string{"shop.audit"} })
	table.Privileges = []MySQLPrivileges{{GrantOn: "shop.audit", Grant: []string{"SELECT"}}}
	mysqlResult(t, f.applyUser(f.run(), table), "succeeded", "changed")
}

// A statement that fails after the account was created leaves the task
// failed with the server's message and with what did change; the next run
// finishes what is missing.
func TestMySQLUserFailureAfterPartialWorkAndAnotherTry(t *testing.T) {
	t.Parallel()
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		f.change(func(s *standInMySQL) {
			s.Fail, s.FailOnce = map[string]string{"GRANT": "ERROR 1142 (42000): GRANT command denied to user 'deploy'@'localhost' for table 'db'"}, true
		})
		user := shopUser()
		r := f.applyUser(f.run(), user)
		created, attributes := mysqlResult(t, r, "failed", "changed")
		if !created || attributes != nil || r.FailureKind != "" || !strings.HasPrefix(r.Reason, "mysql_user: changing the privileges of shop@10.0.% on the database server on this node: ") ||
			!strings.HasSuffix(r.Reason, "ERROR 1142 (42000) at line 2: GRANT command denied to user 'deploy'@'localhost' for table 'db'") {
			t.Fatalf("%s: %+v", version, r)
		}
		if f.account("shop@10.0.%") == nil || f.held("shop@10.0.%", "database\tshop") != nil {
			t.Fatal("the stand-in did not keep the half-done state")
		}
		f.statements()
		// A dryrun says what is left, and the next run does it.
		r = f.applyUser(f.dryrun(), user)
		if created, attributes = mysqlResult(t, r, "succeeded", "would_change"); created || !reflect.DeepEqual(attributes, []string{"privileges"}) {
			t.Fatalf("%+v", r)
		}
		r = f.applyUser(f.run(), user)
		if created, attributes = mysqlResult(t, r, "succeeded", "changed"); created || !reflect.DeepEqual(attributes, []string{"privileges"}) || len(f.statements()) != 1 {
			t.Fatalf("%+v", r)
		}
		mysqlResult(t, f.applyUser(f.run(), user), "succeeded", "unchanged")

		// A privilege's name that the server refuses is the task's mistake:
		// no further try.
		user.Privileges = []MySQLPrivileges{{GrantOn: "shop.*", Grant: []string{"SELECT", "BOGUS"}}}
		r = f.applyUser(f.run(), user)
		if mysqlResult(t, r, "failed", "unchanged"); r.FailureKind != "config" || !strings.HasSuffix(r.Reason, "; the server refuses a privilege's name in the task's privileges, or a name at a grant_on target where it does not apply") {
			t.Fatalf("%s: %+v", version, r)
		}
		// The server took the statements and the account is still not what
		// the task asks for: the server keeps the privilege under another
		// name.
		f.change(func(s *standInMySQL) {
			s.StoresUnderAnotherName = map[string]string{"REPLICATION CLIENT": "BINLOG MONITOR"}
		})
		user.Privileges = []MySQLPrivileges{{GrantOn: "*.*", Grant: []string{"REPLICATION CLIENT"}}}
		user.PrivilegesNotListed = "keep"
		r = f.applyUser(f.run(), user)
		if want := "mysql_user: the database server on this node accepted the statements, but the account shop@10.0.% still differs from the node task's request; the remaining actions are: grant REPLICATION CLIENT on *.*; the server may use different privilege names, so copy each privilege name exactly from SHOW GRANTS"; r.Status != "failed" || r.Change != "changed" || !strings.HasPrefix(r.Reason, want) {
			t.Fatalf("%+v\nwant %s", r, want)
		}
		// The creation itself fails: nothing changed.
		f.change(func(s *standInMySQL) {
			s.Fail, s.FailOnce = map[string]string{"CREATE USER": "ERROR 1227 (42000): Access denied; you need (at least one of) the CREATE USER privilege(s) for this operation"}, false
		})
		other := shopUser()
		other.Name = "other"
		r = f.applyUser(f.run(), other)
		if created, _ = mysqlResult(t, r, "failed", "unchanged"); created || !strings.HasPrefix(r.Reason, "mysql_user: creating the account other@10.0.% on the database server on this node: ") || !strings.Contains(r.Reason, "ERROR 1227 (42000) at line 2: Access denied") {
			t.Fatalf("%+v", r)
		}
		// A removal that the server refuses.
		f.change(func(s *standInMySQL) {
			s.Fail = map[string]string{"DROP USER": "ERROR 1227 (42000): Access denied; you need (at least one of) the CREATE USER privilege(s) for this operation"}
		})
		gone := MySQLUser{Name: "shop", MayConnectFrom: "10.0.%", State: "absent"}
		if r = f.applyUser(f.run(), gone); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "mysql_user: DROP USER `shop`@`10.0.%` on the database server on this node: ") {
			t.Fatalf("%+v", r)
		}
	}
}

// A secret password does not hide the task's result, and neither the
// password nor its stored value shows in it, whole or in part: not when
// MariaDB's client prints the statement that failed, and not when a server
// quotes the statement and cuts it inside the stored value.
func TestMySQLUserPasswordAndItsStoredValueStayOutOfTheResult(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		for _, existing := range []bool{false, true} {
			f := newMySQLFixture(t, version)
			if existing {
				f.change(func(s *standInMySQL) {
					s.Accounts["shop@10.0.%"] = &standInMySQLAccount{Plugin: "caching_sha2_password", Stored: mysqlCachingSHA2Password("another", []byte("ServersOwnSalt012345"), 5000), Privileges: map[string]*standInMySQLPrivileges{}}
				})
			}
			// The server does not understand a word before the stored value
			// and quotes 80 characters of the statement from there.
			f.change(func(s *standInMySQL) { s.SyntaxErrorFrom = "IDENTIFIED" })
			s, trace := taskScope(mysqlTestSecrets)
			told := f.run().MySQLUser(ctx, shopUser(), s, nil)
			statements := f.statements()
			if len(statements) != 1 {
				t.Fatalf("%q", statements)
			}
			stored := statements[0][strings.LastIndex(statements[0][:len(statements[0])-1], "'")+1 : len(statements[0])-1]
			if same, err := mysqlPasswordIs(mysqlCachingSHA2Method, stored, mysqlTestPassword); !strings.Contains(version, "MariaDB") && !existing && (err != nil || !same) {
				t.Fatalf("the statement's stored value %q is not the password's", stored)
			}
			for _, r := range []protocol.Result{told, reportedByTheEngine(told, s)} {
				text, _ := json.Marshal(r)
				if r.Status != "failed" || r.Change != "unchanged" || trace.Used || !strings.Contains(r.Reason, "ERROR 1064 (42000) at line 2: You have an error in your SQL syntax") || !strings.Contains(r.Reason, "'[redacted]") {
					t.Fatalf("%s: %s", version, text)
				}
				// No piece of the stored value of eight characters is left,
				// and none of the password. A shorter piece of a random
				// stored value is now and then a word of the result's own
				// text, such as "shop".
				for from := 0; from+8 <= len(stored); from++ {
					if piece := stored[from : from+8]; strings.Contains(string(text), piece) && !strings.Contains("caching_sha2_password mysql_native_password", piece) {
						t.Fatalf("%s: the piece %q of the stored value %q is in the result: %s", version, piece, stored, text)
					}
				}
				if strings.Contains(string(text), "Sentinel") {
					t.Fatalf("the password is in the result: %s", text)
				}
			}
			// MariaDB's client printed the statement; it shows without the
			// value.
			if strings.Contains(version, "MariaDB") && !strings.Contains(told.Reason, "--------------\n"+strings.Replace(statements[0], stored, "[redacted]", 1)+"\n--------------") {
				t.Fatalf("the statement that MariaDB's client printed is not in the reason: %s", told.Reason)
			}
		}
	}
	// The scope learns the stored value of a secret password, so that the
	// node task execution engine also replaces it where the module did not
	// expect it; a password that is written in the playbook is no secret.
	f := newMySQLFixture(t, standInMySQLVersion)
	s, _ := taskScope(mysqlTestSecrets)
	f.run().MySQLUser(ctx, shopUser(), s, nil)
	stored := f.account("shop@10.0.%").Stored
	if logins := s.LoginsHandedOnlyToATool(); !reflect.DeepEqual(logins, []string{mysqlTestPassword, stored}) {
		t.Fatalf("the scope remembers %q, want the password and its stored value %q", logins, stored)
	}
	plain := shopUser()
	plain.Name, plain.Password = "plain", writtenPassword("written in the playbook")
	s, _ = taskScope(mysqlTestSecrets)
	if f.run().MySQLUser(ctx, plain, s, nil); s.LoginsHandedOnlyToATool() != nil {
		t.Fatalf("the scope remembers %q for a password that is no secret", s.LoginsHandedOnlyToATool())
	}
}

func TestMySQLUserRejectsInputs(t *testing.T) {
	t.Parallel()
	privileges := func(on string, grant ...string) []MySQLPrivileges {
		return []MySQLPrivileges{{GrantOn: on, Grant: grant}}
	}
	for want, user := range map[string]MySQLUser{
		"mysql_user.name is required":                           {MayConnectFrom: "%"},
		"mysql_user.may_connect_from is required: the machines": {Name: "shop"},
		"state must be present or absent":                       {Name: "shop", MayConnectFrom: "%", State: "deleted"},
		"state: absent accepts only name, may_connect_from, and the connection fields; remove password":                                        {Name: "shop", MayConnectFrom: "%", State: "absent", Password: writtenPassword("x")},
		"state: absent accepts only name, may_connect_from, and the connection fields; remove privileges":                                      {Name: "shop", MayConnectFrom: "%", State: "absent", Privileges: []MySQLPrivileges{}},
		"state: absent accepts only name, may_connect_from, and the connection fields; remove privileges_not_listed":                           {Name: "shop", MayConnectFrom: "%", State: "absent", PrivilegesNotListed: "remove"},
		"privileges requires privileges_not_listed: remove or keep; no default is provided":                                                    {Name: "shop", MayConnectFrom: "%", Privileges: privileges("shop.*", "SELECT")},
		"privileges_not_listed must be remove or keep":                                                                                         {Name: "shop", MayConnectFrom: "%", Privileges: privileges("shop.*", "SELECT"), PrivilegesNotListed: "revoke"},
		"privileges_not_listed needs privileges; to take every privilege away":                                                                 {Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "remove"},
		"each of privileges needs grant_on":                                                                                                    {Name: "shop", MayConnectFrom: "%", Privileges: privileges("", "SELECT"), PrivilegesNotListed: "keep"},
		"each of privileges needs grant: at least one privilege name":                                                                          {Name: "shop", MayConnectFrom: "%", Privileges: privileges("shop.*"), PrivilegesNotListed: "keep"},
		"privileges[1].grant_on: privileges names shop.* twice; write all its privileges in one entry":                                         {Name: "shop", MayConnectFrom: "%", Privileges: append(privileges("shop.*", "SELECT"), privileges("shop.*", "INSERT")...), PrivilegesNotListed: "keep"},
		"privileges[0].grant[2]: grant names SELECT twice":                                                                                     {Name: "shop", MayConnectFrom: "%", Privileges: privileges("shop.*", "SELECT", "INSERT", "select"), PrivilegesNotListed: "keep"},
		"privileges[0].grant[1]: grant names ALL twice":                                                                                        {Name: "shop", MayConnectFrom: "%", Privileges: privileges("shop.*", "ALL", "all privileges"), PrivilegesNotListed: "keep"},
		"database_server_port needs database_server_host":                                                                                      {Name: "shop", MayConnectFrom: "%", DatabaseServerPort: 3307},
		"allow_creating_account_without_password cannot be combined with password; remove allow_creating_account_without_password or password": {Name: "shop", MayConnectFrom: "%", Password: writtenPassword("x"), AllowCreatingAccountWithoutPassword: true},
		"state: absent accepts only name, may_connect_from, and the connection fields; remove allow_creating_account_without_password":         {Name: "shop", MayConnectFrom: "%", State: "absent", AllowCreatingAccountWithoutPassword: true},
		"database_server_host needs login_password":                                                                                            {Name: "shop", MayConnectFrom: "%", DatabaseServerHost: "db.example.com"},
		"database_server_certificate_authority_file needs database_server_host":                                                                {Name: "shop", MayConnectFrom: "%", State: "absent", DatabaseServerCertificateAuthorityFile: "/etc/ssl/authority.pem"},
	} {
		if err := user.Prepare(""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	for _, user := range []MySQLUser{shopUser(), {Name: "shop", MayConnectFrom: "%"}, {Name: "shop", MayConnectFrom: "%", State: "absent"}, {Name: "monitor", MayConnectFrom: "%", AllowCreatingAccountWithoutPassword: true},
		{Name: "shop", MayConnectFrom: "%", Privileges: []MySQLPrivileges{}, PrivilegesNotListed: "remove"},
		// What a reference holds is checked when its value is known.
		{Name: "shop", MayConnectFrom: "%", Privileges: privileges("{{ .vars.database }}.*", "{{ .vars.privilege }}"), PrivilegesNotListed: "keep"}} {
		if err := user.Prepare(""); err != nil {
			t.Errorf("%+v: %v", user, err)
		}
	}
	// The rules of the inputs that may hold a reference.
	var arguments ArgumentList
	user := shopUser()
	user.Arguments(&arguments)
	rules := map[string]func(any) error{}
	for _, argument := range arguments.List {
		rules[argument.Field] = argument.Rule
	}
	for field, values := range map[string]map[string]string{
		"name":             {"": "name must not be empty and must not hold control characters", "tab\there": "must not hold control characters"},
		"may_connect_from": {"": "may_connect_from must not be empty"},
		"password":         {"": "password must not be empty: the server would let anyone log in to the account without a password; leave password out to keep the password that the account has"},
		"privileges[0].grant_on": {
			"shop":             "grant_on must be *.* for the whole server, a database such as shop.*, or a table such as shop.orders",
			"*":                "grant_on must be *.* for the whole server",
			"*.orders":         "grant_on must be *.* for the whole server",
			".*":               "grant_on must be *.* for the whole server",
			"shop.":            "grant_on must be *.* for the whole server",
			"shop.public.t":    `"shop.public.t" has more than one dot, so MMX cannot tell where the database's name ends`,
			"shop\u00a0db.*":   `grant_on holds a non-breaking space; write database names literally, such as shop_db.*`,
			"shop\n.*":         "grant_on must not hold control characters",
			"`shop`.`orders`":  "grant_on must not hold a backtick: MMX quotes the names itself; write each name plain, such as shop.orders or my-shop.*",
			"`my-shop`.*":      "grant_on must not hold a backtick: MMX quotes the names itself; write each name plain, such as shop.orders or my-shop.*",
			"shop.orders\x00x": "grant_on must not hold control characters",
		},
		"privileges[0].grant[0]": {
			"":                    "is not a privilege's name",
			"SELECT; DROP":        "is not a privilege's name",
			"SELECT (id)":         "is not a privilege's name",
			"usage":               "grant must omit USAGE because USAGE means the account has no privileges",
			"SELECT ON *.* TO x@": "is not a privilege's name",
		},
	} {
		for written, want := range values {
			if err := rules[field](written); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s %q: want %q, got %v", field, written, want, err)
			}
		}
	}
	for field, values := range map[string][]string{
		"name":                   {"shop", "shop-admin", "it's `a` name; --", "日本"},
		"may_connect_from":       {"%", "localhost", "10.0.%", "192.168.1.0/255.255.255.0", "db.Example.COM"},
		"password":               {"x", " spaces ", "pä-日本"},
		"privileges[0].grant_on": {"*.*", "shop.*", `shop\_db.*`, "shop%.*", "shop.orders", "shop-db.order items", "a.b.*"},
		"privileges[0].grant[0]": {"SELECT", "select", "Create Temporary Tables", "ALL", "all privileges", "GRANT OPTION", "BACKUP_ADMIN", "  LOCK   TABLES ", "XA_RECOVER_ADMIN", "S3_ACCESS"},
	} {
		for _, written := range values {
			if err := rules[field](written); err != nil {
				t.Errorf("%s %q: %v", field, written, err)
			}
		}
	}
	// The password's rule never repeats the value, and a place and a name
	// are read as the servers write them.
	if place, err := parseMySQLPrivilegePlace(`shop\_db.*`); err != nil || place != (mysqlPlace{Kind: "database", Database: `shop\_db`}) {
		t.Fatal(place, err)
	}
	if place, err := parseMySQLPrivilegePlace("a.b.*"); err != nil || place != (mysqlPlace{Kind: "database", Database: "a.b"}) {
		t.Fatal(place, err)
	}
	if name, err := mysqlPrivilegeName("  create   temporary tables "); err != nil || name != "CREATE TEMPORARY TABLES" {
		t.Fatal(name, err)
	}
	// A list that only a run knows is checked in the run.
	f := newMySQLFixture(t, standInMariaDBVersion)
	twice := MySQLUser{Name: "shop", MayConnectFrom: "%", PrivilegesNotListed: "keep", Privileges: append(privileges("shop.*", "SELECT"), privileges("shop.*", "INSERT")...)}
	if r := f.run().MySQLUser(testContext(t), twice, value.Scope{}, nil); r.FailureKind != "config" || r.Reason != "mysql_user.privileges[1].grant_on: privileges names shop.* twice; write all its privileges in one entry" {
		t.Fatalf("%+v", r)
	}
}

// Rows that the statements of the module do not ask for fail the read
// instead of being guessed at.
func TestMySQLAccountRowsThatMMXDoesNotKnow(t *testing.T) {
	t.Parallel()
	for name, rows := range map[string][][]string{
		"an unknown first word":                                      {{"something else", "1"}},
		"a row with too few columns":                                 {{"table privilege", "73686F70", "SELECT", "NO"}},
		"a name that is not hexadecimal":                             {{"database privilege", "shop", "SELECT", "NO"}},
		"a list of privileges that ends":                             {{"privileges of the server begin"}, {"Select", "Tables", "To retrieve rows from table"}},
		"another type than a set":                                    {{"privileges of a table", "enum('Select')"}},
		"a dynamic privilege with an option that is neither Y nor N": {{"dynamic privilege", "BACKUP_ADMIN", "YES"}},
		"auth_or that is not a JSON list":                            {{"account", "mysql_native_password", "", mysqlHexadecimal("unix_socket")}},
	} {
		if _, _, err := readMySQLAccountRows(rows); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
	// What the real servers print for an account with privileges at every
	// kind of place.
	account, server, err := readMySQLAccountRows([][]string{
		{"account", "caching_sha2_password", mysqlHexadecimal("$A$005$stored"), ""},
		{"caching_sha2_password_digest_rounds", "10000"},
		{"strict_password_validation", "ON"}, {"password check plugins", "0"},
		{"server privilege", "USAGE", "NO"},
		{"dynamic privilege", "BACKUP_ADMIN", "Y"}, {"dynamic privilege", "XA_RECOVER_ADMIN", "N"},
		{"dynamic privilege of the server", "BACKUP_ADMIN"}, {"dynamic privilege of the server", "XA_RECOVER_ADMIN"}, {"dynamic privilege of the server", "SYSTEM_USER"},
		{"database privilege", mysqlHexadecimal(`shop\_db`), "SELECT", "NO"}, {"database privilege", mysqlHexadecimal(`shop\_db`), "INSERT", "NO"},
		{"database privilege", mysqlHexadecimal("shop%"), "USAGE", "YES"},
		{"table privilege", mysqlHexadecimal("shop"), mysqlHexadecimal("orders"), "SELECT", "NO"},
		{"column privilege", mysqlHexadecimal("shop"), mysqlHexadecimal("audit"), mysqlHexadecimal("id"), "SELECT", "YES"},
		{"routine privilege", mysqlHexadecimal("shop"), mysqlHexadecimal("count_orders"), "PROCEDURE", "Execute,Alter Routine,Grant"},
		{"privileges of a database", "18"},
		{"privileges of a table", "set('Select','Insert','Update','Delete','Create','Drop','Grant','References','Index','Alter','Create View','Show view','Trigger')"},
		{"existing table", mysqlHexadecimal("shop"), mysqlHexadecimal("orders")},
		{"privileges of the server begin"}, {"Alter", "Tables", "To alter the table"}, {"Grant option", "Databases,Tables,Functions,Procedures", "To give to other users those privileges you possess"},
		{"Proxy", "Server Admin", "To make proxy user possible"}, {"Usage", "Server Admin", "No privileges - allow connect only"}, {"BACKUP_ADMIN", "Server Admin", ""},
		{"privileges of the server end"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[mysqlPlace]*mysqlHeldPrivileges{
		// The static privileges have no option (the row USAGE says NO), one
		// dynamic privilege has it and one has not.
		{Kind: "server"}:                                                                     {Names: map[string]bool{"BACKUP_ADMIN": true, "XA_RECOVER_ADMIN": true}, GrantOption: true, WithoutGrantOption: true},
		{Kind: "database", Database: `shop\_db`}:                                             {Names: map[string]bool{"SELECT": true, "INSERT": true}, WithoutGrantOption: true},
		{Kind: "database", Database: "shop%"}:                                                {Names: map[string]bool{}, GrantOption: true},
		{Kind: "table", Database: "shop", Table: "orders"}:                                   {Names: map[string]bool{"SELECT": true}, WithoutGrantOption: true},
		{Kind: "table", Database: "shop", Table: "audit"}:                                    {Names: map[string]bool{}, GrantOption: true},
		{Kind: "column", Database: "shop", Table: "audit", Column: "id"}:                     {Names: map[string]bool{"SELECT": true}},
		{Kind: "routine", Database: "shop", Table: "count_orders", RoutineKind: "PROCEDURE"}: {Names: map[string]bool{"EXECUTE": true, "ALTER ROUTINE": true}, GrantOption: true},
	}
	if !account.Exists || account.Method != "caching_sha2_password" || account.Stored != "$A$005$stored" || !sameMySQLPrivileges(account.Privileges, want) {
		t.Fatalf("%+v", account)
	}
	if server.CachingSHA2Rounds != "10000" || server.RefusesStoredPasswords || !reflect.DeepEqual(server.PrivilegeCount, map[string]int{"server": 2, "database": 18, "table": 12}) ||
		!reflect.DeepEqual(server.DynamicPrivileges, map[string]bool{"BACKUP_ADMIN": true, "XA_RECOVER_ADMIN": true, "SYSTEM_USER": true}) ||
		!server.Tables[mysqlPlace{Kind: "table", Database: "shop", Table: "orders"}] {
		t.Fatalf("%+v", server)
	}
}

// Literal names never broaden database access; patterns are an explicit choice.
func TestMySQLGrantDatabaseNamesAreLiteral(t *testing.T) {
	t.Parallel()
	for _, version := range []string{standInMariaDBVersion, standInMySQLVersion} {
		f := newMySQLFixture(t, version)
		user := MySQLUser{Name: "literal", MayConnectFrom: "localhost", AllowCreatingAccountWithoutPassword: true, PrivilegesNotListed: "remove",
			Privileges: []MySQLPrivileges{{GrantOn: "shop_db%.*", Grant: []string{"SELECT"}}}}
		if err := user.Prepare(""); err != nil {
			t.Fatal(err)
		}
		if r := f.applyUser(f.run(), user); r.Status != "succeeded" || r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		if got := f.held("literal@localhost", "database\tshop\\_db\\%"); !slices.Equal(got, []string{"SELECT"}) {
			t.Fatal(got)
		}
		if r := f.applyUser(f.run(), user); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
		user.Privileges = []MySQLPrivileges{{GrantOnDatabasesMatching: "shop_%", Grant: []string{"SELECT"}}}
		if err := user.Prepare(""); err != nil {
			t.Fatal(err)
		}
		if r := f.applyUser(f.run(), user); r.Status != "succeeded" || r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		if got := f.held("literal@localhost", "database\tshop_%"); !slices.Equal(got, []string{"SELECT"}) {
			t.Fatal(got)
		}
		if got := f.held("literal@localhost", "database\tshop\\_db\\%"); len(got) != 0 {
			t.Fatal(got)
		}
		if r := f.applyUser(f.dryrun(), user); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestMySQLGrantEntryNeedsExactlyOneTarget(t *testing.T) {
	t.Parallel()
	for _, target := range []MySQLPrivileges{{Grant: []string{"SELECT"}}, {GrantOn: "shop.*", GrantOnDatabasesMatching: "shop_%", Grant: []string{"SELECT"}}} {
		user := MySQLUser{Name: "shop", MayConnectFrom: "localhost", PrivilegesNotListed: "keep", Privileges: []MySQLPrivileges{target}}
		if err := user.Prepare(""); err == nil || !strings.Contains(err.Error(), "grant_on_databases_matching") {
			t.Fatal(err)
		}
	}
}
