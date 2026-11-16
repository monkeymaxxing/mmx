package modules

import (
	"errors"
	"fmt"
	"math"
	"mmx/internal/assets"
	"mmx/internal/value"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Helpers that every module's input checks share.

func Dynamic(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, "{{")
	case map[string]any:
		for _, x := range v {
			if Dynamic(x) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if Dynamic(x) {
				return true
			}
		}
	}
	return false
}

var Name = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// ConnectionSecretName accepts exactly one named secret, with no surrounding text.
func ConnectionSecretName(reference string) string {
	match := connectionSecret.FindStringSubmatch(reference)
	if match == nil {
		return ""
	}
	return match[1]
}

var connectionSecret = regexp.MustCompile(`^\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}$`)

// ControllerMachineFiles is the struct tag value, mmx:"controller_machine_files",
// of an input that is not text and names files that the module's Prepare
// reads on the controller machine as the project loads, such as
// authorized_keys.key_files_on_controller_machine. Such an input takes no whole-value reference:
// its files must be read before the node job is built.
const ControllerMachineFiles = "controller_machine_files"

// WholeSecretReference reports text that is exactly one reference to a named
// secret, such as {{ .secrets.alice_password_hash }}, or to a role input, such
// as {{ .inputs.password_hash }}. A role call must fill an input declared
// secret: true from a secret, so the value is never written in the YAML
// either way; the loader requires the input to be declared so
// (ArgumentList.WholeSecretReference).
func WholeSecretReference(reference string) bool {
	return ConnectionSecretName(reference) != "" || secretRoleInput.MatchString(reference)
}

var secretRoleInput = regexp.MustCompile(`^\{\{\s*\.inputs\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}$`)

// RoleInputName names the role input that text which is exactly one
// reference to a role input reads, or is empty.
func RoleInputName(reference string) string {
	match := secretRoleInput.FindStringSubmatch(reference)
	if match == nil {
		return ""
	}
	return match[1]
}

// SourceVariant is one file, download, or directory tree that a task's
// variable field resolves to.
type SourceVariant struct {
	Asset   *assets.Ref      `json:"asset,omitempty"`
	Entries []DirectoryEntry `json:"entries,omitempty"`
	// Keys are the key lines an authorized_keys task's key files hold.
	Keys []FileKey `json:"keys,omitempty"`
	// Nodes are the nodes whose jobs can use this variant.
	Nodes        []string `json:"nodes,omitempty"`
	LocalPath    string   `json:"-"`
	TemplateText string   `json:"-"`
	URL          string   `json:"-"`
	SHA256       string   `json:"-"`
}

// checkSourceReferences refuses, when the project loads, a variable source
// field that reads what only a run can know.
func CheckSourceReferences(field, text string) error {
	for _, m := range value.Reference.FindAllStringSubmatch(value.WithoutLiterals(text), -1) {
		parts, err := value.Path(m[1])
		if err != nil {
			return invalid(field, err.Error())
		}
		root := parts[0].Name //nolint:nilaway // Successful value.Path must parse an initial dotted field and append a selector; empty paths return errors.
		runtime := slices.Contains([]string{"node_tasks", "this_node_task", "outputs", "fleet_stages", "secrets", "new_file"}, root)
		if root == "node" && !(len(parts) == 2 && parts[1].Name == "name") {
			runtime = true
		}
		if runtime {
			return invalid(field, fmt.Sprintf("%s reads %s, which is known only during a run; this field may read the node's name (.node.name), vars, role inputs, and loop items, which the controller machine knows before it connects", field, strings.TrimSpace(m[1])))
		}
	}
	return nil
}

// FieldError is an input error that names its field, relative to the module.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }

// Unwrap lets errors.As find the error that the field's problem wraps.
func (e *FieldError) Unwrap() error { return e.Err }

func invalid(field, message string) error { return &FieldError{field, fmt.Errorf("%s", message)} }

// Invalid is an input error for one field.
func Invalid(field, message string) error { return invalid(field, message) }

func scalar(v any) error { _, err := value.Scalar(v); return err }

func nonEmptyScalar(v any) error {
	if text, err := value.Scalar(v); err != nil || text == "" {
		return errors.New("must resolve to a path")
	}
	return nil
}

// parsed accepts scalar text that parse accepts, failing with parse's error.
func parsed(parse func(string) error) func(any) error {
	return func(v any) error {
		s, err := value.Scalar(v)
		if err != nil {
			return err
		}
		return parse(s)
	}
}

// rule accepts scalar text that check accepts.
func rule(message string, check func(string) bool) func(any) error {
	return parsed(func(s string) error {
		if !check(s) {
			return errors.New(message)
		}
		return nil
	})
}

// Exported for the configuration loader's argument rules.
var (
	Scalar         = scalar
	NonEmptyScalar = nonEmptyScalar
	Parsed         = parsed
	Rule           = rule
)

var (
	absolute = rule("path must be absolute without null bytes", func(s string) bool {
		return filepath.IsAbs(s) && !strings.ContainsRune(s, 0)
	})
	// A directory MMX deletes from or deletes is never / or a top-level
	// directory such as /srv.
	deletablePath = rule("a directory that MMX deletes from or deletes must be an absolute path at least two levels deep, such as /srv/app", func(s string) bool {
		return filepath.IsAbs(s) && !strings.ContainsRune(s, 0) && strings.Count(filepath.Clean(s), "/") >= 2
	})
	entryName = rule("must name a direct child of the directory; the name must not be empty, . or .., or contain /", func(s string) bool {
		return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
	})
	accountPath = rule("account path must be absolute without colons or newlines", func(s string) bool {
		return filepath.IsAbs(s) && !strings.ContainsAny(s, ":\x00\n\r")
	})
	linkTarget      = rule("symlink target must be non-empty without null bytes", func(s string) bool { return s != "" && !strings.ContainsRune(s, 0) })
	accountNameRule = rule("invalid account name", accountName.MatchString)
	unitNameRule    = rule("unit name must be a .service, .timer, .path, .socket, .mount, .target, .swap, .slice, or .automount filename", unitName.MatchString)
	dropInRule      = rule("drop_in must be a single .conf filename", dropInName.MatchString)
	serviceRule     = rule("invalid service name", ServiceName.MatchString)
	// Root's units are system units; only another account has its own systemd.
	ordinaryAccount = func(v any) error {
		if err := accountNameRule(v); err != nil {
			return err
		}
		if s, _ := value.Scalar(v); s == "root" {
			return errors.New("root's units are system units; remove in_systemd_of_user_account")
		}
		return nil
	}
	// Systemd reloads only services; other unit kinds refuse the job.
	reloadableRule = func(v any) error {
		if err := serviceRule(v); err != nil {
			return err
		}
		if s, _ := value.Scalar(v); !strings.HasSuffix(UnitName(s), ".service") {
			return errors.New("refresh: reload applies only to .service units")
		}
		return nil
	}
	authorizedKey = parsed(func(s string) error { _, err := ParseAuthorizedKey(s); return err })
	fullName      = rule("full_name must not contain : or line breaks, which the account database cannot hold", func(s string) bool {
		return !strings.ContainsAny(s, ":\x00\n\r")
	})
	// A crypt hash such as $6$salt$hash or $y$j9T$salt$hash. The message never
	// repeats the value, which is a secret.
	passwordHash = rule("password_hash must be a password hash in crypt format, such as $6$... or $y$..., as mkpasswd or openssl passwd -6 print it", regexp.MustCompile(`^\$[0-9a-z]+\$[./0-9A-Za-z$=,]+$`).MatchString)
	// A port range needs a protocol, so the static protocol chooses the rule.
	ufwPortRule = func(ranged bool) func(any) error {
		return parsed(func(s string) error { _, err := portOrRange(s, ranged); return err })
	}
	ufwSourceRule      = parsed(func(s string) error { _, err := sourceAddress(s); return err })
	ufwDestinationRule = rule("to must be an address or a network such as 10.0.0.0/8", func(s string) bool { _, err := sourceAddress(s); return err == nil })
	ufwInterfaceRule   = rule(interfaceNameRule, interfaceName.MatchString)
	lineMatch          = parsed(func(s string) error { _, err := regexp.Compile(s); return err })
	httpURL            = rule("url must be an HTTP or HTTPS URL", func(s string) bool {
		u, err := url.Parse(s)
		return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https")
	})
	// Apt and dnf read only repository files named with these characters.
	repositoryID   = rule("a dnf repository id uses letters, digits, '_', '-', '.', and ':', starting with a letter, digit, or '_'", regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$`).MatchString)
	repositoryName = rule("apt_repository name must use letters, digits, '_', '-', and '.', starting with a letter, digit, or '_'", repositoryFile.MatchString)
	sourceWord     = rule("apt_repository values must be single words without spaces", func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, unicode.IsSpace)
	})
	moduleStream      = rule("enable_dnf_module_stream is a module and a stream, such as postgresql:16", regexp.MustCompile(`^[A-Za-z0-9_.+-]+:[A-Za-z0-9_.+-]+$`).MatchString)
	dnfRepositoryName = rule("dnf_repository name must use letters, digits, '_', '-', and '.', starting with a letter, digit, or '_'", repositoryFile.MatchString)
	dnfWord           = rule("dnf_repository values must be single words without spaces", func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, unicode.IsSpace)
	})
	dnfLine = rule("dnf_repository.description must be one line", func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\r\n")
	})
	// The comment ends the public key's one line.
	keyComment = rule("ssh_key_pair.key_comment_when_created must be one line of text", func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\x00\r\n")
	})
)

// ParseDurationWithDays reads a duration as Go writes one, such as 12h or
// 90m, or a whole number of days, such as 14d: run_history_retention,
// systemd_journal_vacuum's keep_newer_than, delete_old_files' older_than.
// The duration is above zero.
func ParseDurationWithDays(text string) (time.Duration, error) {
	var duration time.Duration
	var err error
	if days, isDays := strings.CutSuffix(text, "d"); isDays {
		var n int64
		n, err = strconv.ParseInt(days, 10, 64)
		if n <= 0 || n > math.MaxInt64/int64(24*time.Hour) {
			err = errors.New("invalid day count")
		}
		duration = time.Duration(n) * 24 * time.Hour
	} else {
		duration, err = time.ParseDuration(text)
	}
	if err != nil || duration <= 0 {
		return 0, errors.New("must be a duration above zero, such as 14d, 12h, or 30m")
	}
	return duration, nil
}

// durationWithDays is the rule of an input that ParseDurationWithDays reads.
var durationWithDays = parsed(func(s string) error { _, err := ParseDurationWithDays(s); return err })

// firewalldNameRule checks a zone or firewalld service name; selinuxNameRule
// an SELinux boolean or type. Ports and sources of firewalld and selinux_port
// use ufw's rules, which read the same written forms.
func firewalldNameRule(field string) func(any) error {
	return rule(field+" must be a name as firewalld writes it, such as public or https: letters, digits, '_', '-', '.', and '+'", firewalldName.MatchString)
}

func selinuxNameRule(field string) func(any) error {
	return rule(field+" must be a name as SELinux writes it, such as httpd_can_network_connect or http_port_t: letters, digits, and '_'", selinuxName.MatchString)
}

// dnfAddress is a single word without a login, like repositoryURI.
func dnfAddress(v any) error {
	if err := dnfWord(v); err != nil {
		return err
	}
	s, _ := value.Scalar(v)
	if u, err := url.Parse(s); err == nil && u.User != nil {
		return errors.New("a dnf_repository address must not contain a login; write it in username and password")
	}
	return nil
}

// repositoryURI is a single word without a login: the login belongs in
// username and password, which MMX keeps out of the readable source file.
func repositoryURI(v any) error {
	if err := sourceWord(v); err != nil {
		return err
	}
	s, _ := value.Scalar(v)
	if u, err := url.Parse(s); err == nil && u.User != nil {
		return errors.New("apt_repository.uris must not contain a login; set apt_repository.username and apt_repository.password instead")
	}
	return nil
}

// suiteRule is the apt_repository.suites rule that written components select: a
// suite ending in / is a flat repository, which takes none.
func suiteRule(flat bool) func(any) error {
	return func(v any) error {
		if err := sourceWord(v); err != nil {
			return err
		}
		switch s, _ := value.Scalar(v); {
		case flat && !strings.HasSuffix(s, "/"):
			return errors.New("apt_repository.components is required unless the suite ends in / (a flat repository)")
		case !flat && strings.HasSuffix(s, "/"):
			return errors.New("a suite ending in / is a flat repository and takes no components")
		}
		return nil
	}
}

var repositoryFile = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func identifierID(v any) error { _, err := AccountID(v); return err }

// lineText is the rule for line.line: a single line, which match, once known,
// must select, so the next run finds it. match's own rule reports a bad pattern.
func lineText(match string, known bool) func(any) error {
	pattern, _ := regexp.Compile(match)
	return parsed(func(s string) error {
		if strings.Contains(s, "\n") {
			return errors.New("line must be a single line")
		}
		if known && match != "" && pattern != nil && !pattern.MatchString(s) {
			return fmt.Errorf("line.line must match %q, so the next run finds the managed line", match)
		}
		return nil
	})
}

// Argument is one input that may hold a reference: its field, relative to
// the module, such as path or incoming_rules[0].port; its written value; and
// the rule its value must meet, once resolved. A public identifier (a path, a
// name, an owner, a version, a URL, an ID) has a Target, where the loader
// writes the resolved value before the module runs; a private value has none
// and stays a reference for the module to resolve, so the task's trace sees
// any secret it reads.
type Argument struct {
	Field  string
	Raw    any
	Rule   func(any) error
	Target any // *string, *Account, or *any
	// LoginHandedOnlyToATool: the module hands this private value only to its
	// tool as a login, so a secret in it does not hide the task's result
	// (value.Scope.LoginHandedOnlyToATool).
	LoginHandedOnlyToATool bool
	// WholeSecretReference: the value must be one whole reference to a named
	// secret or to a role input declared secret: true.
	WholeSecretReference bool
}

// ArgumentList collects a module's arguments.
type ArgumentList struct {
	// Resolved: the public identifiers are already literal data.
	Resolved bool
	List     []Argument
}

// Public adds a public identifier stored at target.
func (a *ArgumentList) Public(field string, target any, rule func(any) error) {
	arg := Argument{Field: field, Rule: rule, Target: target}
	switch p := target.(type) {
	case *string:
		arg.Raw = *p
	case *any:
		arg.Raw = *p
	}
	a.List = append(a.List, arg)
}

// Private adds a private value.
func (a *ArgumentList) Private(field string, raw any, rule func(any) error) {
	a.List = append(a.List, Argument{Field: field, Raw: raw, Rule: rule})
}

// WholeSecretReference adds a private value that must be one whole reference
// to a named secret or to a role input declared secret: true, so that the
// value is never written in the YAML. The module's own check accepts both
// forms (WholeSecretReference); the loader checks the role input's
// declaration.
func (a *ArgumentList) WholeSecretReference(field string, raw any, rule func(any) error) {
	a.List = append(a.List, Argument{Field: field, Raw: raw, Rule: rule, WholeSecretReference: true})
}

// LoginHandedOnlyToATool adds a private value that the module hands only to
// its tool as a login: a user name, a password, or a key that reaches the
// tool through a private file or a local socket and that the module puts
// nowhere else. It is a private value in every respect but one: a secret in
// it does not hide the task's result, and MMX replaces the login in what the
// task reports instead. The module resolves such a value only with
// value.Scope.LoginHandedOnlyToATool, and tells the scope each other form in
// which the tool gets, keeps, or sends it
// (value.Scope.FormOfLoginHandedOnlyToATool).
func (a *ArgumentList) LoginHandedOnlyToATool(field string, raw any, rule func(any) error) {
	a.List = append(a.List, Argument{Field: field, Raw: raw, Rule: rule, LoginHandedOnlyToATool: true})
}

// Text adds a public scalar text when it is written.
func (a *ArgumentList) Text(field string, target *string) {
	if *target != "" {
		a.Public(field, target, scalar)
	}
}

// Owner adds an owner or group when it is written. Its rule sees an ID as
// the integer it was written as.
func (a *ArgumentList) Owner(field string, target *Account) {
	if *target != "" {
		a.List = append(a.List, Argument{Field: field, Raw: target.literal(), Rule: accountRule, Target: target})
	}
}

// CopyInputs returns a deep copy of a module's inputs, so that resolving
// their references never changes the definition they came from.
func CopyInputs[T any](inputs T) T {
	copied := deepCopy(reflect.ValueOf(&inputs).Elem())
	return copied.Interface().(T)
}

func deepCopy(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			target := reflect.New(v.Type().Elem())
			target.Elem().Set(deepCopy(v.Elem()))
			out.Set(target)
		}
	case reflect.Interface:
		if !v.IsNil() {
			out.Set(deepCopy(v.Elem()))
		}
	case reflect.Struct:
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(deepCopy(v.Field(i)))
			}
		}
	case reflect.Slice:
		if !v.IsNil() {
			copied := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				copied.Index(i).Set(deepCopy(v.Index(i)))
			}
			out.Set(copied)
		}
	case reflect.Map:
		if !v.IsNil() {
			copied := reflect.MakeMapWithSize(v.Type(), v.Len())
			for _, key := range v.MapKeys() {
				copied.SetMapIndex(key, deepCopy(v.MapIndex(key)))
			}
			out.Set(copied)
		}
	default:
		out.Set(v)
	}
	return out
}

// WrittenArgument is one input with the value the task wrote, which mmx
// explain shows.
type WrittenArgument struct {
	Field string
	Value any
}

// WrittenArguments lists the inputs a module's input value holds, by their
// names, leaving out empty values, mappings, and nested definitions, and the
// fields named in skip.
func WrittenArguments(inputs any, skip ...string) []WrittenArgument {
	v := reflect.ValueOf(inputs)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	var out []WrittenArgument
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		raw := v.Field(i)
		if name == "" || name == "-" || slices.Contains(skip, name) || raw.IsZero() {
			continue
		}
		if raw.Kind() == reflect.Pointer {
			raw = raw.Elem()
		}
		if raw.Kind() == reflect.Struct || raw.Kind() == reflect.Map {
			continue
		}
		out = append(out, WrittenArgument{Field: name, Value: raw.Interface()})
	}
	return out
}

// Reads is what a module's inputs read beyond their argument list, which the
// loader checks against the names a task can see.
type Reads struct {
	// Templates are template texts by field, such as template.
	Templates map[string]string
	// Conditions are conditions by field that must hold when the module runs,
	// such as assert's that.
	Conditions map[string]any
	// ResultConditions are conditions by field that read the module's own
	// result, .this_node_task, such as shell's changed_when.
	ResultConditions map[string]any
	// Commands are nested shell tasks by field. A command checks a new file
	// before it replaces the live one and reads .new_file.path.
	Commands map[string]*Shell
	// ShellScripts are texts by field that run as shell scripts and are
	// literal by design, such as shell's script and cron_job's command: MMX
	// fills in no reference there, and values reach them through env. The
	// loader refuses a reference to a value MMX knows in them.
	ShellScripts map[string]string
}
