package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// mysqlTasks loads a project whose one fleet stage has these node tasks,
// written as a YAML list.
func mysqlTasks(t *testing.T, tasks string) (Project, error) {
	t.Helper()
	p, _, err := Load(fixture(t, "vars:\n  database_host: db.example.com\n  database_port: 3307\nfleet_stages:\n  - id: configure\n    nodes: all\n    node_tasks:\n"+tasks), "example", "", nil)
	return p, err
}

// The tasks of the design load, and what a task of mysql_user or
// mysql_database writes wrong is refused when the project loads, at its
// field.
func TestMySQLTasksAreCheckedWhenTheProjectLoads(t *testing.T) {
	const user = `      - mysql_user:
          name: shop
          may_connect_from: "10.0.%"
          password: "{{ .secrets.shop_database_password }}"
          privileges:
            - grant_on: "shop.*"
              grant: [SELECT, INSERT, UPDATE, DELETE]
            - grant_on: 'shop_archive.*'
              grant: [ALL, GRANT OPTION]
          privileges_not_listed: remove
`
	const database = `      - mysql_database:
          name: shop
          character_set: utf8mb4
          collation: utf8mb4_unicode_ci
          database_server_host: "{{ .vars.database_host }}"
          database_server_port: "{{ .vars.database_port }}"
          login_user: admin
          login_password: "{{ .secrets.database_administrator_password }}"
          database_server_certificate_authority_file: /etc/ssl/database-authority.pem
`
	p, err := mysqlTasks(t, user+database)
	if err != nil {
		t.Fatal(err)
	}
	tasks := p.FleetStages[0].NodeTasks
	loaded := InputsOf[modules.MySQLUser](tasks[0])
	// Database names are literal; callers do not escape underscores.
	if loaded.Privileges[1].GrantOn != `shop_archive.*` || *loaded.Password != "{{ .secrets.shop_database_password }}" {
		t.Fatalf("%+v", loaded)
	}
	// The port may be a reference, which resolves to the inventory's number.
	resolved, err := ResolveArguments(tasks[1], value.Scope{"vars": map[string]any{"database_host": "db.example.com", "database_port": 3307}})
	if err != nil || InputsOf[modules.MySQLDatabase](resolved).DatabaseServerPort != int64(3307) || InputsOf[modules.MySQLDatabase](resolved).DatabaseServerHost != "db.example.com" {
		t.Fatal(InputsOf[modules.MySQLDatabase](resolved), err)
	}
	if _, err := ResolveArguments(tasks[1], value.Scope{"vars": map[string]any{"database_host": "db.example.com", "database_port": "the standard one"}}); err == nil || !strings.Contains(err.Error(), "database_server_port must be a number from 1 to 65535") {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ task, field, want string }{
		"YAML's double quotes make a non-breaking space of \\_": {
			"      - mysql_user: {name: shop, may_connect_from: '%', privileges: [{grant_on: \"shop\\_db.*\", grant: [SELECT]}], privileges_not_listed: keep}\n",
			"mysql_user.privileges[0].grant_on", `grant_on holds a non-breaking space; write database names literally, such as shop_db.*`},
		"an on that the task quotes itself": {
			"      - mysql_user: {name: shop, may_connect_from: '%', privileges: [{grant_on: '`my-shop`.*', grant: [SELECT]}], privileges_not_listed: keep}\n",
			"mysql_user.privileges[0].grant_on", "grant_on must not hold a backtick: MMX quotes the names itself; write each name plain, such as shop.orders or my-shop.*"},
		"an empty password": {
			"      - mysql_user: {name: shop, may_connect_from: '%', password: ''}\n",
			"mysql_user.password", "password must not be empty: the server would let anyone log in to the account without a password"},
		"privileges without privileges_not_listed": {
			"      - mysql_user: {name: shop, may_connect_from: '%', privileges: []}\n",
			// A field that the task does not write has no place of its own.
			"mysql_user)", "privileges requires privileges_not_listed: remove or keep"},
		"a privilege's name that is no name": {
			"      - mysql_user: {name: shop, may_connect_from: '%', privileges: [{grant_on: 'shop.*', grant: ['SELECT; DROP DATABASE shop']}], privileges_not_listed: keep}\n",
			"mysql_user.privileges[0].grant[0]", "is not a privilege's name"},
		"without may_connect_from": {
			"      - mysql_user: {name: shop}\n",
			"mysql_user.may_connect_from", "mysql_user.may_connect_from is required"},
		"a field of another module": {
			"      - mysql_user: {name: shop, may_connect_from: '%', login_role: postgres}\n",
			"mysql_user.login_role", "unknown field"},
		"a port in quotes": {
			"      - mysql_database: {name: shop, database_server_host: db.example.com, database_server_port: '3307', login_password: x}\n",
			"mysql_database.database_server_port", "database_server_port must be a number from 1 to 65535"},
		"a port that no machine has": {
			"      - mysql_database: {name: shop, database_server_host: db.example.com, database_server_port: 70000, login_password: x}\n",
			"mysql_database.database_server_port", "database_server_port must be a number from 1 to 65535"},
		"the permission for an account without a password beside a password": {
			"      - mysql_user: {name: shop, may_connect_from: '%', password: x, allow_creating_account_without_password: true}\n",
			"mysql_user.allow_creating_account_without_password", "allow_creating_account_without_password cannot be combined with password"},
		"a host without a password": {
			"      - mysql_database: {name: shop, database_server_host: db.example.com}\n",
			"mysql_database)", "database_server_host needs login_password"},
		"a character set that is no name": {
			"      - mysql_database: {name: shop, character_set: 'utf8mb4 COLLATE x'}\n",
			"mysql_database.character_set", "character_set must be a name of letters, digits, and underscores"},
		"deleting the server's own database": {
			"      - mysql_database: {name: mysql, state: deleted_with_all_its_data}\n",
			"mysql_database.name", "mysql is one of the server's own databases"},
		"an authority file that is no absolute path": {
			"      - mysql_database: {name: shop, database_server_host: db.example.com, login_password: x, database_server_certificate_authority_file: authority.pem}\n",
			"mysql_database.database_server_certificate_authority_file", "path must be absolute"},
	} {
		if _, err := mysqlTasks(t, c.task); err == nil || !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %s: %s, got %v", name, c.field, c.want, err)
		}
	}
	for name, task := range map[string]string{
		"an empty list that takes every privilege away": "      - mysql_user: {name: shop, may_connect_from: '%', privileges: [], privileges_not_listed: remove}\n",
		"an account that is meant to have no password":  "      - mysql_user: {name: health-check, may_connect_from: '10.0.%', allow_creating_account_without_password: true}\n",
		"a login with a password through the socket":    "      - mysql_user: {name: shop, may_connect_from: localhost, state: absent, login_user: root, login_password: '{{ .secrets.root }}'}\n",
		"the tasks in the changes of a node task block": "      - task_block:\n          id: shop\n          changes:\n            - mysql_database: {name: shop}\n            - mysql_user: {name: shop, may_connect_from: localhost}\n",
	} {
		if _, err := mysqlTasks(t, task); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A task travels to the node as JSON. An empty list of privileges with
// privileges_not_listed: remove still means "take everything away" there,
// and a password that the task does not name stays unnamed.
func TestMySQLUserKeepsItsMeaningOnTheWayToTheNode(t *testing.T) {
	p, err := mysqlTasks(t, "      - mysql_user: {name: shop, may_connect_from: '%', privileges: [], privileges_not_listed: remove}\n"+
		"      - mysql_user: {name: shop, may_connect_from: '%', password: '{{ .secrets.shop }}', database_server_host: db.example.com, database_server_port: 3307, login_password: '{{ .secrets.admin }}'}\n")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := json.Marshal(p.FleetStages[0].NodeTasks)
	if err != nil {
		t.Fatal(err)
	}
	var received []Task
	if err := json.Unmarshal(sent, &received); err != nil {
		t.Fatal(err)
	}
	if first := InputsOf[modules.MySQLUser](received[0]); first.PrivilegesNotListed != "remove" || len(first.Privileges) != 0 || first.Password != nil {
		t.Fatalf("%+v", first)
	}
	if second := InputsOf[modules.MySQLUser](received[1]); second.Password == nil || *second.Password != "{{ .secrets.shop }}" || fmt.Sprint(second.DatabaseServerPort) != "3307" || second.LoginPassword != "{{ .secrets.admin }}" {
		t.Fatalf("%+v", second)
	}
}

// The password of an account and the login are private values that the
// module hands only to the database's client: explain names the secret and
// never shows it, and checking them does not mark the task's trace, so a
// secret in them does not hide the task's result.
func TestMySQLPasswordsAreCheckedWithoutMarkingTheTrace(t *testing.T) {
	secrets := value.Scope{"vars": map[string]any{"database_host": "db.example.com"}, "secrets": map[string]any{
		"shop":  &value.Secret{Name: "shop", Data: "shop-sentinel", Available: true},
		"admin": &value.Secret{Name: "admin", Data: "admin-sentinel", Available: true},
		"empty": &value.Secret{Name: "empty", Data: "", Available: true}}}
	password := "{{ .secrets.shop }}"
	task := Task{Module: "mysql_user", ModuleInputs: &modules.MySQLUser{Name: "shop", MayConnectFrom: "%", Password: &password,
		DatabaseServerHost: "{{ .vars.database_host }}", LoginUser: "admin", LoginPassword: "{{ .secrets.admin }}"}}
	s, trace := secrets.Trace()
	resolved, err := ResolveArguments(task, s)
	if err != nil || InputsOf[modules.MySQLUser](resolved).DatabaseServerHost != "db.example.com" {
		t.Fatal(err)
	}
	arguments, err := InspectArguments(resolved, s, false)
	if err != nil || trace.Used {
		t.Fatal("checking the passwords marked the task's trace", err)
	}
	for _, argument := range arguments {
		switch argument.Field {
		case "mysql_user.password", "mysql_user.login_password":
			if argument.Status != "secret" || argument.Value != nil || len(argument.Secrets) != 1 {
				t.Fatalf("explain must name the secret and never show it: %+v", argument)
			}
		case "mysql_user.login_user":
			if argument.Status != "resolved" || argument.Value != "admin" {
				t.Fatalf("%+v", argument)
			}
		}
	}
	if text, _ := json.Marshal(arguments); strings.Contains(string(text), "sentinel") {
		t.Fatal("the inspected arguments hold a password")
	}
	if got := s.LoginsHandedOnlyToATool(); len(got) != 2 {
		t.Fatal("the checked passwords were not remembered for the replacement", got)
	}
	// A password that resolves to nothing is refused, without its value.
	empty := "{{ .secrets.empty }}"
	s, _ = secrets.Trace()
	if _, err := InspectArguments(Task{Module: "mysql_user", ModuleInputs: &modules.MySQLUser{Name: "shop", MayConnectFrom: "%", Password: &empty}}, s, false); err == nil || !strings.Contains(err.Error(), "mysql_user.password") || !strings.Contains(err.Error(), "password must not be empty") {
		t.Fatal(err)
	}
	// A secret in the account's name is refused, as in every identifier.
	if _, err := ResolveArguments(Task{Module: "mysql_user", ModuleInputs: &modules.MySQLUser{Name: "{{ .secrets.shop }}", MayConnectFrom: "%"}}, secrets); err == nil || !strings.Contains(err.Error(), "resource identifier must not contain a secret") {
		t.Fatal(err)
	}
}
