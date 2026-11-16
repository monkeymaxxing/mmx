package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// PostgresqlDatabase is one database inside a PostgreSQL server.
// login_password is a private value that the module hands only to psql as a
// login.
type PostgresqlDatabase struct {
	Name                string   `yaml:"name" json:"name"`
	State               string   `yaml:"state,omitempty" json:"state,omitempty"`
	Owner               string   `yaml:"owner,omitempty" json:"owner,omitempty"`
	Encoding            string   `yaml:"encoding,omitempty" json:"encoding,omitempty"`
	Locale              string   `yaml:"locale,omitempty" json:"locale,omitempty"`
	TemplateDatabase    string   `yaml:"template_database,omitempty" json:"template_database,omitempty"`
	Extensions          []string `yaml:"extensions,omitempty" json:"extensions"`
	ExtensionsNotListed string   `yaml:"extensions_not_listed,omitempty" json:"extensions_not_listed,omitempty"`

	DatabaseServerHost                     string `yaml:"database_server_host,omitempty" json:"database_server_host,omitempty"`
	DatabaseServerPort                     any    `yaml:"database_server_port,omitempty" json:"database_server_port,omitempty"`
	DatabaseServerCertificateAuthorityFile string `yaml:"database_server_certificate_authority_file,omitempty" json:"database_server_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck  bool   `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
	LoginRole                              string `yaml:"login_role,omitempty" json:"login_role,omitempty"`
	LoginPassword                          string `yaml:"login_password,omitempty" json:"login_password,omitempty"`
}

// The states of a database. The removal state says that the data goes.
const (
	postgresqlDatabasePresent = "present"
	postgresqlDatabaseDeleted = "deleted_with_all_its_data"
)

var postgresqlDatabaseStates = []string{postgresqlDatabasePresent, postgresqlDatabaseDeleted}

// postgresqlTemplateOfNewDatabases is the database that CREATE DATABASE
// copies unless the statement names another one.
const postgresqlTemplateOfNewDatabases = "template1"

// postgresqlBuiltInExtension is the extension that every database has from
// the start and that extensions_not_listed: remove never removes. The server
// itself would let a superuser drop it.
const postgresqlBuiltInExtension = "plpgsql"

// postgresqlDatabaseDescription is what mmx describe-module shows for postgresql_database.
var postgresqlDatabaseDescription = Description{
	Name: "postgresql_database", Topic: TopicDatabases, Schema: reflect.TypeOf(PostgresqlDatabase{}),
	Summary: "one database inside a PostgreSQL server that is installed and runs: its owner and its extensions, or its removal with all its data; runs the node's psql program",
	Fields: append([]Field{
		{Name: "name", Required: true, Notes: "the database's name; a name longer than the server takes (63 bytes unless the server was built otherwise) fails the task before it changes anything"},
		{Name: "state", Values: postgresqlDatabaseStates, Default: postgresqlDatabasePresent, Notes: "deleted_with_all_its_data drops the database with everything in it and accepts only name and the connection fields; a missing database is unchanged; the task fails with the server's message while sessions are connected to the database"},
		{Name: "owner", Notes: "required with state: present; the role that owns the database; it must exist; a different owner of an existing database is changed"},
		{Name: "encoding", Notes: "the character encoding, such as UTF8; used when the database is created; an existing database with another encoding fails the task, because PostgreSQL cannot change it; compared by the server's own name for the encoding, so utf8 is UTF8"},
		{Name: "locale", Notes: "the locale, such as en_US.UTF-8; used when the database is created; an existing database with another locale fails the task; en_US.UTF-8 and en_US.utf8 are the same locale; another locale or encoding than the template database has needs template_database: template0, and fails the task without it before anything changes"},
		{Name: "template_database", Default: postgresqlTemplateOfNewDatabases, Notes: "the database that a new database is a copy of; used when the database is created; it must exist"},
		{Name: "extensions", Notes: "extensions that must be installed in the database, such as pg_trgm; the files of each must be on the server's machine (the package postgresql-contrib on the Red Hat family); needs extensions_not_listed"},
		{Name: "extensions_not_listed", Values: notListed, Notes: "required with extensions: remove drops every installed extension that the list does not name, and fails with the server's message when other objects need one; keep leaves them alone; PostgreSQL's built-in plpgsql is never removed"},
	}, postgresqlConnectionFieldNotes()...),
	Example: "- postgresql_database:\n    name: shop\n    owner: shop\n    encoding: UTF8\n    extensions: [pg_trgm]\n    extensions_not_listed: keep\n",
	Results: []string{"status", "change", "reason", "value.created", "value.changed_attributes"},
	Dryrun:  "reads the server's catalog and changes nothing; reports would_change with value.created and value.changed_attributes as a run would report them; not_checked while the psql program, the operating system account postgres, a running server, the owner role, the template database, or the files of an extension are missing, because an earlier node task may provide each; an existing database with another encoding or locale, and a new one with another encoding or locale than its template database has, fail the dryrun as they fail a run; it does not look for sessions that would keep a run from dropping the database",
}

// Prepare checks the inputs when the project loads.
func (d *PostgresqlDatabase) Prepare(base string) error {
	if d.State != "" && !slices.Contains(postgresqlDatabaseStates, d.State) {
		return invalid("state", "state must be "+strings.Join(postgresqlDatabaseStates, " or "))
	}
	if d.State == postgresqlDatabaseDeleted && (d.Owner != "" || d.Encoding != "" || d.Locale != "" || d.TemplateDatabase != "" || d.Extensions != nil || d.ExtensionsNotListed != "") {
		return invalid("state", "postgresql_database with state: "+postgresqlDatabaseDeleted+" accepts only name and the connection fields")
	}
	switch {
	case d.ExtensionsNotListed != "" && !slices.Contains(notListed, d.ExtensionsNotListed):
		return invalid("extensions_not_listed", "extensions_not_listed must be remove or keep")
	case d.Extensions != nil && d.ExtensionsNotListed == "":
		return invalid("extensions_not_listed", "extensions_not_listed is required with extensions: remove drops every installed extension that the list does not name, keep leaves the other extensions alone")
	case d.Extensions == nil && d.ExtensionsNotListed != "":
		return invalid("extensions_not_listed", "extensions_not_listed requires extensions (use extensions: [] with remove to drop every extension but the built-in "+postgresqlBuiltInExtension+")")
	}
	if err := postgresqlListedTwice("extensions", d.Extensions); err != nil {
		return err
	}
	if d.Owner == "" && d.State != postgresqlDatabaseDeleted {
		return invalid("owner", "owner is required with state: present; add owner with the role that must own the database")
	}
	return validatePostgresqlConnection(d.DatabaseServerHost, d.DatabaseServerCertificateAuthorityFile, d.LoginPassword, d.DangerouslySkipServerCertificateCheck)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (d *PostgresqlDatabase) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &d.Name, postgresqlNameRule)
	if d.Owner != "" {
		arguments.Public("owner", &d.Owner, postgresqlNameRule)
	}
	arguments.Text("encoding", &d.Encoding)
	arguments.Text("locale", &d.Locale)
	if d.TemplateDatabase != "" {
		arguments.Public("template_database", &d.TemplateDatabase, postgresqlNameRule)
	}
	for i := range d.Extensions {
		arguments.Public(fmt.Sprintf("extensions[%d]", i), &d.Extensions[i], postgresqlNameRule)
	}
	postgresqlConnectionArguments(arguments, &d.DatabaseServerHost, &d.DatabaseServerPort, &d.DatabaseServerCertificateAuthorityFile, d.LoginRole, d.LoginPassword)
}

// Run does the module's work on the node.
func (d *PostgresqlDatabase) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PostgresqlDatabase(ctx, *d, task.Scope, task.Packages)
}

func (d PostgresqlDatabase) connection() postgresqlConnection {
	return postgresqlConnection{module: "postgresql_database", host: d.DatabaseServerHost, port: postgresqlPort(d.DatabaseServerPort),
		certificateAuthorityFile: d.DatabaseServerCertificateAuthorityFile, skipServerCertificateCheck: d.DangerouslySkipServerCertificateCheck, loginRole: d.LoginRole, loginPassword: d.LoginPassword}
}

// postgresqlDatabaseState is what one query says about a database, and about
// the server, that the task needs to know before it changes anything.
type postgresqlDatabaseState struct {
	// MaxIdentifierLength is the longest name the server takes, in bytes.
	MaxIdentifierLength int `json:"max_identifier_length"`
	// Database is nil when the database does not exist.
	Database *postgresqlDatabaseAttributes `json:"database"`
	// Template is the database that a new database would be a copy of, nil
	// when it does not exist.
	Template *postgresqlDatabaseAttributes `json:"template"`
	// OwnerExists: the role of the task's owner exists.
	OwnerExists bool `json:"owner_exists"`
	// EncodingAsTheServerNamesIt is the task's encoding in the server's own
	// spelling (UTF8 for utf-8), "" for a name the server does not know.
	EncodingAsTheServerNamesIt string `json:"encoding_as_the_server_names_it"`
	// AvailableExtensions are the extensions of the task's list whose files
	// the server has.
	AvailableExtensions []string `json:"available_extensions"`
}

// postgresqlDatabaseAttributes is one database as the catalog describes it.
type postgresqlDatabaseAttributes struct {
	Owner    string `json:"owner"`
	Encoding string `json:"encoding"`
	// Collation and CharacterType are the two parts of the database's
	// locale, in the spelling that created the database.
	Collation          string `json:"collation"`
	CharacterType      string `json:"character_type"`
	AcceptsConnections bool   `json:"accepts_connections"`
}

// readDatabaseState adds the query for a database's state to a script.
func (d PostgresqlDatabase) readDatabaseState(script *postgresqlScript) {
	template := d.TemplateDatabase
	if template == "" {
		template = postgresqlTemplateOfNewDatabases
	}
	database := func(name string) string {
		return `(select pg_catalog.jsonb_build_object('owner', pg_catalog.pg_get_userbyid(d.datdba), 'encoding', pg_catalog.pg_encoding_to_char(d.encoding),
			'collation', d.datcollate, 'character_type', d.datctype, 'accepts_connections', d.datallowconn)
			from pg_catalog.pg_database d where d.datname = ` + script.literal(name) + `::pg_catalog.text)`
	}
	script.statement(`select pg_catalog.jsonb_build_object(
		'max_identifier_length', pg_catalog.current_setting('max_identifier_length')::pg_catalog.int4,
		'database', ` + database(d.Name) + `,
		'template', ` + database(template) + `,
		'owner_exists', exists (select 1 from pg_catalog.pg_roles r where r.rolname = ` + script.literal(d.Owner) + `::pg_catalog.text),
		'encoding_as_the_server_names_it', pg_catalog.pg_encoding_to_char(pg_catalog.pg_char_to_encoding(` + script.literal(d.Encoding) + `::pg_catalog.name)),
		'available_extensions', (select coalesce(pg_catalog.jsonb_agg(wanted.name), '[]'::pg_catalog.jsonb)
			from pg_catalog.jsonb_array_elements_text(` + script.list(d.Extensions) + `) as wanted(name)
			where exists (select 1 from pg_catalog.pg_available_extensions a where a.name = wanted.name)))`)
}

// readPostgresqlInstalledExtensions adds the query for the extensions that are
// installed in the database of the session.
func readPostgresqlInstalledExtensions(script *postgresqlScript) {
	script.statement(`select pg_catalog.jsonb_build_object('installed_extensions',
		(select coalesce(pg_catalog.jsonb_agg(e.extname order by e.extname), '[]'::pg_catalog.jsonb) from pg_catalog.pg_extension e))`)
}

// postgresqlSameLocale reports whether two names mean one locale. The server keeps the
// spelling that created a database, en_US.UTF-8 on Debian and en_US.utf8 on
// Enterprise Linux, and compares names as text. The part after the dot is
// the same without regard to case and hyphens, and POSIX is C.
func postgresqlSameLocale(one, other string) bool {
	plain := func(locale string) string {
		if locale == "POSIX" {
			return "C"
		}
		name, codeset, found := strings.Cut(locale, ".")
		if !found {
			return locale
		}
		return name + "." + strings.ToLower(strings.ReplaceAll(codeset, "-", ""))
	}
	return plain(one) == plain(other)
}

// PostgresqlDatabase brings one database to the state its task says. It
// reads the database from the server's catalog first and changes only what
// differs: the database itself, its owner, and its extensions, each a step
// of its own, because CREATE DATABASE and DROP DATABASE cannot be part of a
// transaction. A run that ended between two steps is finished by the next.
func (run Runtime) PostgresqlDatabase(ctx context.Context, d PostgresqlDatabase, s value.Scope, packages PackageManager) protocol.Result {
	const module = "postgresql_database"
	client, stop := run.postgresqlClient(ctx, d.connection(), s, packages)
	if stop != nil {
		return *stop
	}
	defer client.close()
	where := client.connection.where() //nolint:nilaway // The preceding stop check excludes the constructor's nil client returns.

	// state reads the database's state from what a script's one query printed.
	state := func(what string, printed []json.RawMessage, err error) (postgresqlDatabaseState, error) {
		var read postgresqlDatabaseState
		if err == nil && (len(printed) != 1 || json.Unmarshal(printed[0], &read) != nil) {
			err = fmt.Errorf("%s: could not %s: psql did not print the JSON value that MMX asked for", module, what)
		}
		return read, err
	}
	script := postgresqlReadOnly()
	d.readDatabaseState(script)
	script.statement("commit")
	printed, err := client.run(ctx, postgresqlMaintenanceDatabase, "read the database "+d.Name, script)
	before, err := state("read the database "+d.Name, printed, err)
	if err != nil {
		return postgresqlFailure(err)
	}
	for _, name := range []struct{ field, text string }{{"name", d.Name}, {"template_database", d.TemplateDatabase}} {
		if err := postgresqlNameTooLong(module, name.field, name.text, before.MaxIdentifierLength); err != nil {
			return postgresqlBeforeAnyChange(protocol.ConfigurationError(err))
		}
	}
	exists := before.Database != nil

	if d.State == postgresqlDatabaseDeleted {
		if !exists {
			return postgresqlChangeResult("unchanged", false, nil)
		}
		if run.Dryrun {
			r := postgresqlChangeResult("would_change", false, nil)
			r.Reason = "would drop the database " + d.Name + " with all its data"
			return r
		}
		if err := run.Mutation("postgresql database " + d.Name); err != nil {
			return postgresqlBeforeAnyChange(protocol.Failed(err))
		}
		script := postgresqlChangingScript()
		script.statement("drop database " + script.identifier(d.Name))
		script.statementsDone()
		d.readDatabaseState(script)
		printed, change, err := client.change(ctx, postgresqlMaintenanceDatabase, "drop the database "+d.Name, script)
		dropped := postgresqlChangeResult("changed", false, nil)
		dropped.Reason = "dropped the database " + d.Name + " with all its data"
		after, err := state("read the database "+d.Name+" after its removal", printed, err)
		switch {
		case err != nil:
			// The server drops a database whole or not at all. When it
			// refused, for example while sessions are connected to the
			// database, nothing changed; when only the read after the
			// statement failed, the database is gone.
			r := postgresqlFailure(err)
			if r.Change = change; change == "changed" {
				r.Value = dropped.Value
			}
			return r
		case after.Database != nil:
			return protocol.Failed(fmt.Errorf("%s: the server accepted DROP DATABASE, but the database %s still exists", module, d.Name))
		}
		return dropped
	}

	// What the task names must fit the server before anything changes.
	if d.Encoding != "" && before.EncodingAsTheServerNamesIt == "" {
		return postgresqlBeforeAnyChange(protocol.ConfigurationError(fmt.Errorf("%s.encoding: %q is no character encoding that this PostgreSQL server knows; write a name such as UTF8 or LATIN1", module, d.Encoding)))
	}
	if exists {
		// PostgreSQL cannot change the encoding or the locale of a database.
		var differences []string
		if d.Encoding != "" && before.EncodingAsTheServerNamesIt != before.Database.Encoding {
			differences = append(differences, fmt.Sprintf("the database encoding is %s, but the node task requests %s", before.Database.Encoding, d.Encoding))
		}
		if d.Locale != "" && !(postgresqlSameLocale(d.Locale, before.Database.Collation) && postgresqlSameLocale(d.Locale, before.Database.CharacterType)) {
			differences = append(differences, fmt.Sprintf("the database locale is %s, but the node task requests %s", before.Database.locale(), d.Locale))
		}
		if len(differences) > 0 {
			return postgresqlBeforeAnyChange(protocol.ConfigurationError(fmt.Errorf("%s: the database %s exists and differs from the task: %s. PostgreSQL cannot change the encoding or the locale of an existing database, so MMX changed nothing. Correct the task, or move the data into a new database",
				module, d.Name, strings.Join(differences, "; "))))
		}
	}
	if d.Owner != "" && !before.OwnerExists {
		return postgresqlBeforeAnyChange(run.notYet(fmt.Errorf("%s.owner: the role %s does not exist in the PostgreSQL server %s; create it in an earlier node task, with postgresql_role", module, d.Owner, where)))
	}
	if !exists {
		// A new database is a copy of its template database. A dryrun must
		// not promise a database that the server then refuses to create.
		if stop := d.fitsTheTemplate(run, before, where); stop != nil {
			return *stop
		}
	}

	// The extensions that are installed now, which only a session in the
	// database itself can read.
	var installed []string
	if exists && d.Extensions != nil {
		if !before.Database.AcceptsConnections {
			return postgresqlBeforeAnyChange(protocol.ConfigurationError(fmt.Errorf("%s: the database %s accepts no connections (datallowconn is false), so MMX cannot read or change its extensions", module, d.Name)))
		}
		script := postgresqlReadOnly()
		readPostgresqlInstalledExtensions(script)
		script.statement("commit")
		what := "read the extensions of the database " + d.Name
		printed, err := client.run(ctx, d.Name, what, script)
		if installed, err = client.installedExtensions(what, printed, err); err != nil {
			return postgresqlFailure(err)
		}
	}
	var install, remove []string
	for _, extension := range d.Extensions {
		if !slices.Contains(installed, extension) {
			install = append(install, extension)
		}
	}
	if d.ExtensionsNotListed == "remove" {
		for _, extension := range installed {
			if extension != postgresqlBuiltInExtension && !slices.Contains(d.Extensions, extension) {
				remove = append(remove, extension)
			}
		}
	}
	// The server knows whether an extension's files are on its machine.
	var unavailable []string
	for _, extension := range install {
		if !slices.Contains(before.AvailableExtensions, extension) {
			unavailable = append(unavailable, extension)
		}
	}
	if len(unavailable) > 0 {
		extensions := "the extension " + unavailable[0] + " (pg_available_extensions does not list it)"
		if len(unavailable) > 1 {
			extensions = "the extensions " + strings.Join(unavailable, ", ") + " (pg_available_extensions lists none of them)"
		}
		return postgresqlBeforeAnyChange(run.notYet(fmt.Errorf("%s.extensions: the PostgreSQL server %s does not have the files of %s. Install them on the server's machine in an earlier node task: %s",
			module, where, extensions, postgresqlExtensionPackage(packages))))
	}

	changeOwner := exists && d.Owner != "" && before.Database.Owner != d.Owner
	var differing []string
	if changeOwner {
		differing = append(differing, "owner")
	}
	if len(install) > 0 || len(remove) > 0 {
		differing = append(differing, "extensions")
	}
	if exists && len(differing) == 0 {
		return postgresqlChangeResult("unchanged", false, nil)
	}
	if run.Dryrun {
		r := postgresqlChangeResult("would_change", true, nil)
		r.Reason = "would create the database " + d.Name
		if exists {
			r = postgresqlChangeResult("would_change", false, differing)
			r.Reason = "would change " + strings.Join(differing, ", ") + " of the database " + d.Name
		}
		return r
	}
	if err := run.Mutation("postgresql database " + d.Name); err != nil {
		return postgresqlBeforeAnyChange(protocol.Failed(err))
	}

	// From here on the result says what the task changed so far, also when
	// a later step fails. Each step is one statement or one transaction,
	// so a step that the server refuses changes nothing itself.
	created := false
	var changed []string
	failed := func(err error, changeOfTheFailedStep string) protocol.Result {
		r := postgresqlFailure(err)
		r.Change = changeOfTheFailedStep
		if created || len(changed) > 0 {
			r.Change = "changed"
			r.Value = postgresqlChangeResult("changed", created, changed).Value
		}
		return r
	}
	if !exists {
		script := postgresqlChangingScript()
		statement := "create database " + script.identifier(d.Name)
		if d.Owner != "" {
			statement += " owner " + script.identifier(d.Owner)
		}
		if d.Encoding != "" {
			statement += " encoding " + script.literal(d.Encoding)
		}
		if d.Locale != "" {
			statement += " locale " + script.literal(d.localeForTheServer(before.Template))
		}
		if d.TemplateDatabase != "" {
			statement += " template " + script.identifier(d.TemplateDatabase)
		}
		script.statement(statement)
		script.statementsDone()
		d.readDatabaseState(script)
		printed, change, err := client.change(ctx, postgresqlMaintenanceDatabase, "create the database "+d.Name, script)
		// The statement was done, also when the read after it failed.
		created = change == "changed"
		after, err := state("read the database "+d.Name+" after its creation", printed, err)
		if err != nil {
			return failed(err, change)
		}
		if after.Database == nil {
			created = false
			return failed(fmt.Errorf("%s: the server accepted CREATE DATABASE, but the database %s does not exist", module, d.Name), "unknown")
		}
	}
	if changeOwner {
		script := postgresqlChangingScript()
		script.statement("alter database " + script.identifier(d.Name) + " owner to " + script.identifier(d.Owner))
		script.statementsDone()
		d.readDatabaseState(script)
		printed, change, err := client.change(ctx, postgresqlMaintenanceDatabase, "change the owner of the database "+d.Name, script)
		after, err := state("read the database "+d.Name+" after the change of its owner", printed, err)
		if err == nil && (after.Database == nil || after.Database.Owner != d.Owner) {
			return failed(fmt.Errorf("%s: the server accepted ALTER DATABASE, and the owner of the database %s is not %s", module, d.Name, d.Owner), "unknown")
		}
		// The statement was done, also when the read after it failed.
		if change == "changed" {
			changed = append(changed, "owner")
		}
		if err != nil {
			return failed(err, change)
		}
	}
	if len(install) > 0 || len(remove) > 0 {
		// One transaction in the database itself: every extension of the
		// task is installed and removed, or none.
		script := postgresqlChangingScript()
		script.statement("begin")
		for _, extension := range install {
			script.statement("create extension " + script.identifier(extension))
		}
		for _, extension := range remove {
			// No CASCADE: the server refuses while other objects need the
			// extension, and its message names them.
			script.statement("drop extension " + script.identifier(extension))
		}
		script.statement("commit")
		script.statementsDone()
		readPostgresqlInstalledExtensions(script)
		what := "change the extensions of the database " + d.Name
		printed, change, err := client.change(ctx, d.Name, what, script)
		after, err := client.installedExtensions(what, printed, err)
		if err != nil {
			// The transaction was committed when only the read after it
			// failed.
			if change == "changed" && !created {
				changed = append(changed, "extensions")
			}
			return failed(err, change)
		}
		if !created && !slices.Equal(after, installed) {
			changed = append(changed, "extensions")
		}
		for _, extension := range install {
			if !slices.Contains(after, extension) {
				return failed(fmt.Errorf("%s: the server accepted CREATE EXTENSION, and the extension %s is not installed in the database %s", module, extension, d.Name), "unknown")
			}
		}
		for _, extension := range remove {
			if slices.Contains(after, extension) {
				return failed(fmt.Errorf("%s: the server accepted DROP EXTENSION, and the extension %s is still installed in the database %s", module, extension, d.Name), "unknown")
			}
		}
	}
	return postgresqlChangeResult("changed", created, changed)
}

// locale is the database's locale in a message: one name, or two where the
// order of text and the kinds of characters follow two locales.
func (database postgresqlDatabaseAttributes) locale() string {
	if database.CharacterType == database.Collation {
		return database.Collation
	}
	return fmt.Sprintf("%s for the order of text and %s for the kinds of characters", database.Collation, database.CharacterType)
}

// fitsTheTemplate checks, before a database is created, what the server
// checks when it copies the template database, so that a dryrun and a run
// say the same and a dryrun promises no database that the server refuses.
// The template must exist; an earlier node task may still create it. And
// unless the template is template0, the server takes only the template's
// own encoding and its own locale, which it compares as text.
func (d PostgresqlDatabase) fitsTheTemplate(run Runtime, before postgresqlDatabaseState, where string) *protocol.Result {
	const module = "postgresql_database"
	stop := func(r protocol.Result) *protocol.Result {
		r = postgresqlBeforeAnyChange(r)
		return &r
	}
	name := d.TemplateDatabase
	if name == "" {
		name = postgresqlTemplateOfNewDatabases
	}
	if before.Template == nil {
		return stop(run.notYet(fmt.Errorf("%s.template_database: the database %s, which the new database %s would be a copy of, does not exist in the PostgreSQL server %s; create it in an earlier node task", module, name, d.Name, where)))
	}
	if name == "template0" {
		return nil
	}
	var differences []string
	if d.Encoding != "" && before.EncodingAsTheServerNamesIt != before.Template.Encoding {
		differences = append(differences, fmt.Sprintf("the template database encoding is %s, but the node task requests %s", before.Template.Encoding, d.Encoding))
	}
	if sent := d.localeForTheServer(before.Template); d.Locale != "" && (sent != before.Template.Collation || sent != before.Template.CharacterType) {
		differences = append(differences, fmt.Sprintf("the template database locale is %s, but the node task requests %s", before.Template.locale(), d.Locale))
	}
	if len(differences) == 0 {
		return nil
	}
	return stop(protocol.ConfigurationError(fmt.Errorf("%s: the database %s cannot be created as a copy of the template database %s: %s. PostgreSQL creates a database with another encoding or locale than its template has only from template0; write template_database: template0, or the template's own encoding and locale",
		module, d.Name, name, strings.Join(differences, "; "))))
}

// localeForTheServer is the task's locale in the spelling that CREATE
// DATABASE gets. The server compares the new database's locale with the
// template's as text and refuses another spelling of the same locale, so
// MMX sends the template's spelling where the task names the template's
// locale. Any other locale goes as the task wrote it; it needs
// template_database: template0, and the server's message says so.
func (d PostgresqlDatabase) localeForTheServer(template *postgresqlDatabaseAttributes) string {
	if template != nil && template.Collation == template.CharacterType && postgresqlSameLocale(d.Locale, template.Collation) {
		return template.Collation
	}
	return d.Locale
}

// installedExtensions reads the extensions that a script's one query
// listed, sorted, from what the script printed. what says what the script
// did, for a failure.
func (client *postgresqlClient) installedExtensions(what string, printed []json.RawMessage, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	var answer struct {
		InstalledExtensions []string `json:"installed_extensions"`
	}
	if len(printed) != 1 || json.Unmarshal(printed[0], &answer) != nil {
		return nil, fmt.Errorf("%s: could not %s: psql did not print the JSON value that MMX asked for", client.connection.module, what) //nolint:nilaway // installedExtensions receives the client only after PostgresqlDatabase checks constructor failure.
	}
	return answer.InstalledExtensions, nil
}

// postgresqlExtensionPackage names the package with the common extensions
// for the node's package manager.
func postgresqlExtensionPackage(packages PackageManager) string {
	manager := ""
	if packages != nil {
		manager = packages.Name()
	}
	switch manager {
	case "apt":
		return "the common extensions, such as pg_trgm and hstore, come with the server's own package here, so this extension has a package of its own"
	case "dnf":
		return "the common extensions, such as pg_trgm and hstore, are in the package postgresql-contrib; another extension has a package of its own"
	}
	return "the common extensions, such as pg_trgm and hstore, are in the package postgresql-contrib on the Red Hat family and in the server's own package on Debian and Ubuntu; another extension has a package of its own"
}
