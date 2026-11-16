package modules

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmx/internal/value"
)

func TestPostgresqlDatabaseRejectedInputs(t *testing.T) {
	t.Parallel()
	for message, database := range map[string]PostgresqlDatabase{
		"owner: owner is required with state: present; add owner with the role that must own the database":                                                                                     {Name: "shop"},
		"state: state must be present or deleted_with_all_its_data":                                                                                                                            {Name: "shop", State: "absent"},
		"state: postgresql_database with state: deleted_with_all_its_data accepts only name and the connection fields":                                                                         {Name: "shop", State: "deleted_with_all_its_data", Owner: "shop"},
		"state: postgresql_database with state: deleted_with_all_its_data accepts only name and the connection fields ":                                                                        {Name: "shop", State: "deleted_with_all_its_data", Encoding: "UTF8"},
		"state: postgresql_database with state: deleted_with_all_its_data accepts only name and the connection fields  ":                                                                       {Name: "shop", State: "deleted_with_all_its_data", Extensions: []string{}, ExtensionsNotListed: "remove"},
		"extensions_not_listed: extensions_not_listed must be remove or keep":                                                                                                                  {Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "drop"},
		"extensions_not_listed: extensions_not_listed is required with extensions: remove drops every installed extension that the list does not name, keep leaves the other extensions alone": {Name: "shop", Extensions: []string{"pg_trgm"}},
		"extensions_not_listed: extensions_not_listed requires extensions (use extensions: [] with remove to drop every extension but the built-in plpgsql)":                                   {Name: "shop", ExtensionsNotListed: "keep"},
		"extensions[2]: extensions lists pg_trgm twice":                                                                                                                                        {Name: "shop", Extensions: []string{"pg_trgm", "hstore", "pg_trgm"}, ExtensionsNotListed: "keep"},
	} {
		if err := database.Prepare(""); err == nil || err.Error() != strings.TrimSpace(message) {
			t.Errorf("%+v: %v; want %s", database, err, message)
		}
	}
	for _, database := range []PostgresqlDatabase{
		{Name: "shop", Owner: "postgres"},
		{Name: "shop", State: "deleted_with_all_its_data", DatabaseServerPort: 5433},
		{Name: "shop", State: "present", Owner: "shop", Encoding: "UTF8", Locale: "en_US.UTF-8", TemplateDatabase: "template0", Extensions: []string{}, ExtensionsNotListed: "remove"},
	} {
		if err := database.Prepare(""); err != nil {
			t.Errorf("%+v: %v", database, err)
		}
	}
	database := PostgresqlDatabase{Name: "shop", Owner: "shop", Encoding: "UTF8", Locale: "C", TemplateDatabase: "template0", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep",
		DatabaseServerHost: "db.example.com", LoginPassword: "{{ .secrets.admin }}"}
	var arguments ArgumentList
	database.Arguments(&arguments)
	var fields, logins []string
	for _, argument := range arguments.List {
		fields = append(fields, argument.Field)
		if argument.LoginHandedOnlyToATool {
			logins = append(logins, argument.Field)
		}
	}
	if !slices.Equal(fields, []string{"name", "owner", "encoding", "locale", "template_database", "extensions[0]", "database_server_host", "login_password"}) || !slices.Equal(logins, []string{"login_password"}) {
		t.Error(fields, logins)
	}
}

// Two names of one locale: the part after the dot without regard to case
// and hyphens, and POSIX is C.
func TestPostgresqlSameLocale(t *testing.T) {
	t.Parallel()
	for _, same := range [][2]string{{"en_US.UTF-8", "en_US.utf8"}, {"en_US.utf8", "en_US.UTF-8"}, {"C.UTF-8", "C.utf8"}, {"POSIX", "C"}, {"C", "C"}, {"de_DE.ISO-8859-1", "de_DE.iso88591"}} {
		if !postgresqlSameLocale(same[0], same[1]) {
			t.Errorf("%s and %s count as two locales", same[0], same[1])
		}
	}
	for _, different := range [][2]string{{"en_US.UTF-8", "en_GB.UTF-8"}, {"en_US.UTF-8", "EN_US.UTF-8"}, {"C", "C.UTF-8"}, {"en_US", "en_US.UTF-8"}, {"POSIX", "posix"}} {
		if postgresqlSameLocale(different[0], different[1]) {
			t.Errorf("%s and %s count as one locale", different[0], different[1])
		}
	}
}

// A first run creates the database with its owner, its encoding, and its
// locale, and installs its extensions in it. A second run and a dryrun
// find nothing to change, and a dryrun before the first run changes
// nothing.
func TestPostgresqlDatabaseFirstRunUnchangedRerunAndDryrun(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	dir := postgresqlFixture(t, server)
	// The task writes Debian's spelling of the locale; this server's
	// template has Enterprise Linux's.
	database := PostgresqlDatabase{Name: "shop db", Owner: "shop", Encoding: "utf-8", Locale: "en_US.UTF-8", Extensions: []string{"pg_trgm", "uuid-ossp"}, ExtensionsNotListed: "keep"}
	before := readState(t, filepath.Join(dir, standInPostgresqlServerFile))

	r := noMutation(t).PostgresqlDatabase(ctx, database, value.Scope{}, nil)
	if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"would_change", true, ""} || r.Reason != "would create the database shop db" {
		t.Fatalf("dryrun: %+v", r)
	}
	if readState(t, filepath.Join(dir, standInPostgresqlServerFile)) != before || takePostgresqlStatements(t, dir) != nil {
		t.Fatal("the dryrun changed the server")
	}
	writeState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile), "")

	r = (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil)
	if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Fatalf("run: %+v", r)
	}
	// The locale goes to the server in the template's spelling: the server
	// compares the two names as text and would refuse the task's.
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{
		`create database "shop db" owner "shop" encoding 'utf-8' locale 'en_US.utf8'`,
		`create extension "pg_trgm"`,
		`create extension "uuid-ossp"`,
	}) {
		t.Errorf("%q", statements)
	}
	created := readStandInPostgresql(t, dir).Databases["shop db"]
	if created == nil || created.Owner != "shop" || created.Encoding != "UTF8" || created.Collation != "en_US.utf8" || !slices.Equal(created.Extensions, []string{"pg_trgm", "plpgsql", "uuid-ossp"}) {
		t.Errorf("%+v", created)
	}
	// The database's name is in psql's environment, never on its command
	// line: psql reads a name with an equals sign on the command line as a
	// connection string. The extensions are installed in a session of the
	// database itself.
	if environments := readState(t, filepath.Join(dir, standInPostgresqlEnvironmentsFile)); !strings.Contains(environments, "PGDATABASE=postgres") || !strings.Contains(environments, "PGDATABASE=shop db") {
		t.Error(environments)
	}
	if calls := strings.Join(takeCalls(t, dir), "\n"); strings.Contains(calls, "shop db") {
		t.Error(calls)
	}

	before = readState(t, filepath.Join(dir, standInPostgresqlServerFile))
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("second %s: %+v", name, r)
		}
	}
	if readState(t, filepath.Join(dir, standInPostgresqlServerFile)) != before || takePostgresqlStatements(t, dir) != nil {
		t.Error("an unchanged run changed the server")
	}
}

// Another locale or encoding than the template's goes to the server as the
// task wrote it, with the template the task names.
func TestPostgresqlDatabaseWithAnotherTemplate(t *testing.T) {
	ctx := testContext(t)
	dir := postgresqlFixture(t, newStandInPostgresql(13))
	database := PostgresqlDatabase{Owner: "postgres", Name: "reports", Encoding: "LATIN1", Locale: "POSIX", TemplateDatabase: "template0"}
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
		t.Fatalf("%+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`create database "reports" owner "postgres" encoding 'LATIN1' locale 'POSIX' template "template0"`}) {
		t.Errorf("%q", statements)
	}
	// The server stores POSIX as C and the encoding under its own name;
	// the next run compares both as the server names them.
	if created := readStandInPostgresql(t, dir).Databases["reports"]; created.Collation != "C" || created.Encoding != "LATIN1" || created.Owner != "postgres" {
		t.Errorf("%+v", created)
	}
	database.Encoding = "latin1"
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("rerun: %+v", r)
	}
}

// The server creates a database with another encoding or locale than its
// template has only from template0, and only from a template that exists.
// MMX reads the template first, so a dryrun promises no database that a run
// could not create: both say the same, and neither sends a statement.
func TestPostgresqlDatabaseThatItsTemplateCannotGive(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Databases["two_locales"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "LATIN1", Collation: "de_DE.iso88591", CharacterType: "C", Extensions: []string{"plpgsql"}}
	dir := postgresqlFixture(t, server)
	fix := ". PostgreSQL creates a database with another encoding or locale than its template has only from template0; write template_database: template0, or the template's own encoding and locale"
	for reason, database := range map[string]PostgresqlDatabase{
		"postgresql_database: the database other cannot be created as a copy of the template database template1: the template database locale is en_US.utf8, but the node task requests C" + fix:                                                                                                                                          {Owner: "postgres", Name: "other", Locale: "C"},
		"postgresql_database: the database other cannot be created as a copy of the template database template1: the template database encoding is UTF8, but the node task requests latin1" + fix:                                                                                                                                         {Owner: "postgres", Name: "other", Encoding: "latin1", TemplateDatabase: "template1"},
		"postgresql_database: the database other cannot be created as a copy of the template database two_locales: the template database encoding is LATIN1, but the node task requests UTF8; the template database locale is de_DE.iso88591 for the order of text and C for the kinds of characters, but the node task requests C" + fix: {Owner: "postgres", Name: "other", Encoding: "UTF8", Locale: "C", TemplateDatabase: "two_locales"},
	} {
		for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
			if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || r.Reason != reason {
				t.Errorf("%s: %+v\nwant %s", name, r, reason)
			}
		}
	}
	// A template database that does not exist is one that an earlier node
	// task may still create.
	reason := "postgresql_database.template_database: the database golden, which the new database other would be a copy of, does not exist in the PostgreSQL server on this node's local socket; create it in an earlier node task"
	missing := PostgresqlDatabase{Owner: "postgres", Name: "other", TemplateDatabase: "golden"}
	if r := (Runtime{}).PostgresqlDatabase(ctx, missing, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Errorf("run: %+v", r)
	}
	if r := noMutation(t).PostgresqlDatabase(ctx, missing, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Errorf("dryrun: %+v", r)
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Databases["other"] != nil {
		t.Error("a task whose template cannot give the database sent a statement")
	}
	// What the template has, in whatever spelling, and what the task leaves
	// out are accepted; so is everything from template0.
	for name, database := range map[string]PostgresqlDatabase{
		"the template's own":          {Owner: "postgres", Name: "same", Encoding: "utf-8", Locale: "en_US.UTF-8"},
		"another template, no locale": {Owner: "postgres", Name: "copy", Encoding: "ISO_8859_1", TemplateDatabase: "two_locales"},
		"template0":                   {Owner: "postgres", Name: "plain", Encoding: "LATIN1", Locale: "C", TemplateDatabase: "template0"},
	} {
		if r := noMutation(t).PostgresqlDatabase(ctx, database, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", true, ""} {
			t.Errorf("%s, dryrun: %+v", name, r)
		}
		if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", true, ""} {
			t.Errorf("%s, run: %+v", name, r)
		}
	}
}

// PostgreSQL cannot change the encoding or the locale of a database. A task
// that names another one for an existing database fails, in a run and in a
// dryrun, names the difference, and changes nothing, also not the owner.
func TestPostgresqlDatabaseWithAnotherEncodingOrLocaleIsNotChanged(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "LATIN1", Collation: "de_DE.iso88591", CharacterType: "C", Extensions: []string{"plpgsql"}}
	dir := postgresqlFixture(t, server)
	for reason, database := range map[string]PostgresqlDatabase{
		"the database encoding is LATIN1, but the node task requests UTF8":                                                                                                                              {Name: "shop", Owner: "shop", Encoding: "UTF8"},
		"the database locale is de_DE.iso88591 for the order of text and C for the kinds of characters, but the node task requests de_DE.ISO-8859-1":                                                    {Name: "shop", Owner: "shop", Locale: "de_DE.ISO-8859-1"},
		"the database encoding is LATIN1, but the node task requests utf8; the database locale is de_DE.iso88591 for the order of text and C for the kinds of characters, but the node task requests C": {Owner: "postgres", Name: "shop", Encoding: "utf8", Locale: "C"},
	} {
		want := "postgresql_database: the database shop exists and differs from the task: " + reason + ". PostgreSQL cannot change the encoding or the locale of an existing database, so MMX changed nothing. Correct the task, or move the data into a new database"
		for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
			if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || r.Reason != want {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	reason := `postgresql_database.encoding: "KLINGON" is no character encoding that this PostgreSQL server knows; write a name such as UTF8 or LATIN1`
	if r := (Runtime{}).PostgresqlDatabase(ctx, PostgresqlDatabase{Owner: "postgres", Name: "new", Encoding: "KLINGON"}, value.Scope{}, nil); r.FailureKind != "config" || r.Reason != reason {
		t.Errorf("%+v", r)
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Databases["shop"].Owner != "postgres" {
		t.Error("a refused task changed the server")
	}
	// The same encoding and the same locale under other names are no
	// difference; the owner is then changed.
	same := PostgresqlDatabase{Name: "shop", Owner: "shop", Encoding: "latin1"}
	if r := (Runtime{}).PostgresqlDatabase(ctx, same, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "owner"} {
		t.Errorf("%+v", r)
	}
}

// An existing database gets the owner the task names. The owner must
// exist: a missing one is a role that an earlier node task may still
// create.
func TestPostgresqlDatabaseOwner(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	dir := postgresqlFixture(t, server)
	database := PostgresqlDatabase{Name: "shop", Owner: "shop"}
	if r := noMutation(t).PostgresqlDatabase(ctx, database, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", false, "owner"} || r.Reason != "would change owner of the database shop" {
		t.Fatalf("dryrun: %+v", r)
	}
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "owner"} {
		t.Fatalf("run: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`alter database "shop" owner to "shop"`}) || readStandInPostgresql(t, dir).Databases["shop"].Owner != "shop" {
		t.Errorf("%q", statements)
	}
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("rerun: %+v", r)
	}

	reason := "postgresql_database.owner: the role billing does not exist in the PostgreSQL server on this node's local socket; create it in an earlier node task, with postgresql_role"
	for _, missing := range []PostgresqlDatabase{{Name: "shop", Owner: "billing"}, {Name: "billing", Owner: "billing"}} {
		if r := (Runtime{}).PostgresqlDatabase(ctx, missing, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
			t.Errorf("run: %+v", r)
		}
		if r := noMutation(t).PostgresqlDatabase(ctx, missing, value.Scope{}, nil); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("dryrun: %+v", r)
		}
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Databases["billing"] != nil {
		t.Error("a task without its owner changed the server")
	}
}

// Extensions of an existing database: keep installs what is missing; remove
// also drops what the list does not name, and never the built-in plpgsql.
// All of one task's extension statements are one transaction.
func TestPostgresqlDatabaseExtensions(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"hstore", "plpgsql"}}
	dir := postgresqlFixture(t, server)

	keep := PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}
	if r := noMutation(t).PostgresqlDatabase(ctx, keep, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", false, "extensions"} {
		t.Fatalf("dryrun: %+v", r)
	}
	if r := (Runtime{}).PostgresqlDatabase(ctx, keep, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "extensions"} {
		t.Fatalf("keep: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`create extension "pg_trgm"`}) ||
		!slices.Equal(readStandInPostgresql(t, dir).Databases["shop"].Extensions, []string{"hstore", "pg_trgm", "plpgsql"}) {
		t.Errorf("keep: %q", statements)
	}

	remove := PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"uuid-ossp", "pg_trgm"}, ExtensionsNotListed: "remove"}
	if r := (Runtime{}).PostgresqlDatabase(ctx, remove, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "extensions"} {
		t.Fatalf("remove: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`create extension "uuid-ossp"`, `drop extension "hstore"`}) ||
		!slices.Equal(readStandInPostgresql(t, dir).Databases["shop"].Extensions, []string{"pg_trgm", "plpgsql", "uuid-ossp"}) {
		t.Errorf("remove: %q", statements)
	}
	// extensions: [] with remove drops every extension but plpgsql.
	none := PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{}, ExtensionsNotListed: "remove"}
	if r := (Runtime{}).PostgresqlDatabase(ctx, none, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "extensions"} {
		t.Fatalf("none: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`drop extension "pg_trgm"`, `drop extension "uuid-ossp"`}) ||
		!slices.Equal(readStandInPostgresql(t, dir).Databases["shop"].Extensions, []string{"plpgsql"}) {
		t.Errorf("none: %q", statements)
	}
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlDatabase(ctx, none, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("%s again: %+v", name, r)
		}
	}

	// The server refuses to drop an extension that another object needs.
	// Nothing of the task's transaction stays: the extension that the same
	// task installed first is not there.
	server = readStandInPostgresql(t, dir)
	server.Databases["shop"].Extensions = []string{"cube", "earthdistance", "plpgsql"}
	writeStandInPostgresql(t, dir, server)
	needed := PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"earthdistance", "hstore"}, ExtensionsNotListed: "remove"}
	r := (Runtime{}).PostgresqlDatabase(ctx, needed, value.Scope{}, nil)
	if r.Status != "failed" || r.Change != "unchanged" || r.Reason != "postgresql_database: could not change the extensions of the database shop: ERROR:  2BP01: cannot drop extension cube because other objects depend on it\nDETAIL:  extension earthdistance depends on extension cube" {
		t.Errorf("%+v", r)
	}
	if !slices.Equal(readStandInPostgresql(t, dir).Databases["shop"].Extensions, []string{"cube", "earthdistance", "plpgsql"}) {
		t.Error("a part of the refused transaction stayed")
	}
}

// Before MMX installs an extension, it asks the server whether the
// extension's files are on the server's machine. Missing files are
// something an earlier node task may still install: a run fails without
// another try and names the package, before the database is created, and a
// dryrun is not checked.
func TestPostgresqlDatabaseExtensionWithoutItsFiles(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(13)
	server.AvailableExtensions = []string{"plpgsql"}
	dir := postgresqlFixture(t, server)
	database := PostgresqlDatabase{Owner: "postgres", Name: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}
	for manager, hint := range map[string]string{
		"dnf": "the common extensions, such as pg_trgm and hstore, are in the package postgresql-contrib; another extension has a package of its own",
		"apt": "the common extensions, such as pg_trgm and hstore, come with the server's own package here, so this extension has a package of its own",
	} {
		reason := "postgresql_database.extensions: the PostgreSQL server on this node's local socket does not have the files of the extension pg_trgm (pg_available_extensions does not list it). Install them on the server's machine in an earlier node task: " + hint
		if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, NewPackageManager(manager)); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
			t.Errorf("%s run: %+v", manager, r)
		}
		if r := noMutation(t).PostgresqlDatabase(ctx, database, value.Scope{}, NewPackageManager(manager)); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Errorf("%s dryrun: %+v", manager, r)
		}
	}
	if takePostgresqlStatements(t, dir) != nil || readStandInPostgresql(t, dir).Databases["shop"] != nil {
		t.Error("the task created the database before it failed")
	}
	// An extension that is installed already needs no files check.
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"pg_trgm", "plpgsql"}}
	writeStandInPostgresql(t, dir, server)
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
		t.Errorf("%+v", r)
	}
}

// The extensions of a database that takes no connections cannot be read.
func TestPostgresqlDatabaseThatAcceptsNoConnections(t *testing.T) {
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	server.Databases["archive"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", RefusesConnections: true}
	dir := postgresqlFixture(t, server)
	r := (Runtime{}).PostgresqlDatabase(testContext(t), PostgresqlDatabase{Name: "archive", Owner: "shop", Extensions: []string{"pg_trgm"}, ExtensionsNotListed: "keep"}, value.Scope{}, nil)
	if r.Status != "failed" || r.FailureKind != "config" || r.Reason != "postgresql_database: the database archive accepts no connections (datallowconn is false), so MMX cannot read or change its extensions" {
		t.Errorf("%+v", r)
	}
	if takePostgresqlStatements(t, dir) != nil {
		t.Error("the task changed the owner before it failed")
	}
	// Without extensions, the task needs no session in the database.
	if r := (Runtime{}).PostgresqlDatabase(testContext(t), PostgresqlDatabase{Name: "archive", Owner: "shop"}, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"changed", false, "owner"} {
		t.Errorf("%+v", r)
	}
}

// A database task has steps that cannot be one transaction. When a later
// step fails, the result says what the task did so far, and the next try
// finishes the rest.
func TestPostgresqlDatabaseFailureAfterPartialWorkAndTheNextTry(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Roles["shop"] = &standInPostgresqlRole{}
	dir := postgresqlFixture(t, server)
	// earthdistance needs cube, which the list names after it.
	database := PostgresqlDatabase{Name: "shop", Owner: "shop", Extensions: []string{"earthdistance", "cube"}, ExtensionsNotListed: "keep"}
	r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil)
	if r.Status != "failed" || postgresqlChange(t, r) != [3]any{"changed", true, ""} ||
		r.Reason != "postgresql_database: could not change the extensions of the database shop: ERROR:  42704: required extension \"cube\" is not installed\nHINT:  Use CREATE EXTENSION ... CASCADE to install required extensions too." {
		t.Fatalf("%+v", r)
	}
	if created := readStandInPostgresql(t, dir).Databases["shop"]; created == nil || created.Owner != "shop" || !slices.Equal(created.Extensions, []string{"plpgsql"}) {
		t.Fatalf("after the failure: %+v", created)
	}
	takePostgresqlStatements(t, dir)
	database.Extensions = []string{"cube", "earthdistance"}
	r = (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil)
	if r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "extensions"} {
		t.Fatalf("the next try: %+v", r)
	}
	if statements := takePostgresqlStatements(t, dir); !slices.Equal(statements, []string{`create extension "cube"`, `create extension "earthdistance"`}) {
		t.Errorf("%q", statements)
	}

	// A statement that waited the whole time limit for another session's
	// lock: the server refuses it, nothing changed, and the failure is one
	// that is tried again. The try after it works.
	server = readStandInPostgresql(t, dir)
	server.Roles["billing"] = &standInPostgresqlRole{}
	server.FailsStatementsStartingWith, server.FailsOnlyOnce = map[string]string{`alter database "shop"`: "ERROR:  55P03: canceling statement due to lock timeout\nCONTEXT:  while updating tuple (0,11) in relation \"pg_database\""}, true
	writeStandInPostgresql(t, dir, server)
	database.Owner = "billing"
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "" ||
		r.Reason != "postgresql_database: could not change the owner of the database shop: ERROR:  55P03: canceling statement due to lock timeout\nCONTEXT:  while updating tuple (0,11) in relation \"pg_database\"" {
		t.Errorf("%+v", r)
	}
	if r := (Runtime{}).PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, "owner"} {
		t.Errorf("the next try: %+v", r)
	}
}

// state: deleted_with_all_its_data drops the database. A missing database
// is unchanged, a dryrun drops nothing, and the server refuses while
// sessions are connected: the task then fails with the server's message.
func TestPostgresqlDatabaseDeletedWithAllItsData(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	server.Databases["shop"] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8", Extensions: []string{"plpgsql"}}
	server.DatabasesWithSessions = []string{"shop"}
	dir := postgresqlFixture(t, server)
	deleted := PostgresqlDatabase{Name: "shop", State: "deleted_with_all_its_data"}

	if r := noMutation(t).PostgresqlDatabase(ctx, deleted, value.Scope{}, nil); postgresqlChange(t, r) != [3]any{"would_change", false, ""} || r.Reason != "would drop the database shop with all its data" {
		t.Fatalf("dryrun: %+v", r)
	}
	if readStandInPostgresql(t, dir).Databases["shop"] == nil || takePostgresqlStatements(t, dir) != nil {
		t.Fatal("the dryrun dropped the database")
	}
	r := (Runtime{}).PostgresqlDatabase(ctx, deleted, value.Scope{}, nil)
	if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "" ||
		r.Reason != "postgresql_database: could not drop the database shop: ERROR:  55006: database \"shop\" is being accessed by other users\nDETAIL:  There is 1 other session using the database." {
		t.Fatalf("with a session: %+v", r)
	}
	if readStandInPostgresql(t, dir).Databases["shop"] == nil {
		t.Fatal("the database is gone although the server refused")
	}
	takePostgresqlStatements(t, dir)
	server.DatabasesWithSessions = nil
	writeStandInPostgresql(t, dir, server)
	if r := (Runtime{}).PostgresqlDatabase(ctx, deleted, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"changed", false, ""} || r.Reason != "dropped the database shop with all its data" {
		t.Fatalf("run: %+v", r)
	}
	if readStandInPostgresql(t, dir).Databases["shop"] != nil || !slices.Equal(takePostgresqlStatements(t, dir), []string{`drop database "shop"`}) {
		t.Error("the database is still there")
	}
	for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
		if r := run.PostgresqlDatabase(ctx, deleted, value.Scope{}, nil); r.Status != "succeeded" || postgresqlChange(t, r) != [3]any{"unchanged", false, ""} {
			t.Errorf("%s for a missing database: %+v", name, r)
		}
	}
}

// A name that is longer than the server takes fails the task before it
// changes anything: the server would cut it and act on another database.
func TestPostgresqlDatabaseNameLongerThanTheServerTakes(t *testing.T) {
	ctx := testContext(t)
	server := newStandInPostgresql(17)
	short := strings.Repeat("d", 63)
	server.Databases[short] = &standInPostgresqlDatabase{Owner: "postgres", Encoding: "UTF8", Collation: "en_US.utf8", CharacterType: "en_US.utf8"}
	dir := postgresqlFixture(t, server)
	long := short + "x"
	for field, database := range map[string]PostgresqlDatabase{
		"name":              {Name: long, State: "deleted_with_all_its_data"},
		"name ":             {Owner: "postgres", Name: long},
		"template_database": {Owner: "postgres", Name: "copy", TemplateDatabase: long},
	} {
		field = strings.TrimSpace(field)
		reason := "postgresql_database." + field + `: the name "` + long + `" has 64 bytes, and this PostgreSQL server takes names of at most 63 bytes. The server would cut the name and act on "` + short + `"; write a shorter name`
		for name, run := range map[string]Runtime{"run": {}, "dryrun": noMutation(t)} {
			if r := run.PostgresqlDatabase(ctx, database, value.Scope{}, nil); r.Status != "failed" || r.FailureKind != "config" || r.Reason != reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	if takePostgresqlStatements(t, dir) != nil || len(readStandInPostgresql(t, dir).Databases) != 4 {
		t.Error("the task acted on the database with the shorter name")
	}
}
