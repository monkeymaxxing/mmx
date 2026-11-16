package value

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSecretValuesSurviveEvaluationButNotStorage(t *testing.T) {
	s := Scope{"secrets": map[string]any{"password": &Secret{Name: "password", Data: "sentinel", Available: true}}, "vars": map[string]any{}}
	local, trace := s.Trace()
	v, err := local.Resolve("prefix-{{ .secrets.password }}")
	if err != nil || Plain(v) != "prefix-sentinel" || !HasSecrets(v) || !trace.Used {
		t.Fatal(v, err)
	}
	saved, _ := json.Marshal(v)
	if strings.Contains(string(saved), "sentinel") {
		t.Fatal("derived secret leaked")
	}
	wire, _ := json.Marshal(Transport(v))
	var decoded any
	json.Unmarshal(wire, &decoded)
	if got := Plain(Restore(decoded)); got != "prefix-sentinel" {
		t.Fatal(got)
	}
	local, trace = s.Trace()
	rendered, err := local.Template(`{{ printf "%s-transformed" .secrets.password }}`)
	if err != nil || rendered != "sentinel-transformed" || !trace.Used {
		t.Fatal(rendered, err)
	}
	local, trace = s.Trace()
	_, err = local.Template(`{{ printf "%v" . }}`)
	if err != nil || !trace.Used {
		t.Fatal("root template escaped sensitivity", err)
	}
	for _, source := range []string{
		`{{ define "nested" }}{{ printf "%s" . }}{{ end }}{{ template "nested" .secrets.password }}`,
		`{{ define "nested" }}{{ printf "%s" .secrets.password }}{{ end }}{{ template "nested" . }}`,
	} {
		local, trace = s.Trace()
		rendered, err = local.Template(source)
		if err != nil || rendered != "sentinel" || !trace.Used {
			t.Fatal("nested template escaped sensitivity", rendered, err)
		}
	}
	local, trace = s.Trace()
	if _, err = local.Template(`plain text`); err != nil || trace.Used {
		t.Fatal("public template unnecessarily hidden", err)
	}
	outputs, err := s.Evaluate(map[string]any{"equals": []any{"{{ .secrets.password }}", "sentinel"}})
	if err != nil || !HasSecrets(outputs) || Plain(outputs) != true {
		t.Fatal(outputs, err)
	}
}
func TestSavedNamedSecretsRefreshAndComputedOutputsRemainUnavailable(t *testing.T) {
	original := map[string]any{"named": &Secret{Name: "token", Data: "old", Available: true}, "computed": Sensitive("old-derived")}
	b, _ := json.Marshal(original)
	var decoded any
	json.Unmarshal(b, &decoded)
	restored, err := Refresh(Restore(decoded), func(string) (any, error) { return "rotated", nil })
	if err != nil {
		t.Fatal(err)
	}
	s := Scope{"outputs": restored}
	got, err := s.Text("{{ .outputs.named }}")
	if err != nil || got != "rotated" {
		t.Fatal(got, err)
	}
	if _, err := s.Get(".outputs.computed"); err == nil {
		t.Fatal("missing secret became a usable placeholder")
	}
	if _, err := s.Get(".outputs"); err == nil {
		t.Fatal("incomplete secret collection became usable")
	}
	if v, err := s.Resolve("/tmp/{{ .outputs.named }}"); err != nil || !HasSecrets(v) {
		t.Fatal("restored secret lost its sensitivity", v, err)
	}
}

func TestUnusedSavedSecretDoesNotReadItsSource(t *testing.T) {
	calls := 0
	outputs := Deferred(map[string]any{"public": "hello", "token": &Secret{Name: "token"}}, func(string) (any, error) { calls++; return "current", nil }, true)
	s := Scope{"outputs": outputs}
	got, err := s.Get(".outputs.public")
	if err != nil || got != "hello" || calls != 0 {
		t.Fatal(got, err, calls)
	}
	got, err = s.Text("{{ .outputs.token }}")
	if err != nil || got != "current" || calls != 1 {
		t.Fatal(got, err, calls)
	}
	// An admitted producer may return its original named input after rotation.
	// A retry refreshes that name even when the old plaintext is still available.
	outputs = Deferred(map[string]any{"token": &Secret{Name: "token", Data: "previous", Available: true}}, func(string) (any, error) { return "current", nil }, true)
	got, err = (Scope{"outputs": outputs}).Text("{{ .outputs.token }}")
	if err != nil || got != "current" {
		t.Fatal(got, err)
	}
}

// index whose keys are literals or root reads reads the selector path it
// names, so it lists, traces, and checks only the selected value. Other keys,
// such as a range element or a piped value, read the whole collection.
func TestIndexReadsItsSelectorPath(t *testing.T) {
	reads, err := TemplateReads(`{{ (index .outputs.inspect .node.name).token }}{{ index .vars.list 1 }}{{ index .vars.map "a]b" $.vars.k }}{{ range .vars.list }}{{ index $.outputs.peers . }}{{ end }}{{ .vars.k | index .vars.piped }}{{ printf "%v" (index .vars.all) }}{{ index . "vars" }}`)
	want := []string{".", `.outputs.inspect[.node.name].token`, `.outputs.peers`, `.vars.all`, `.vars.k`, `.vars.list`, `.vars.list[1]`, `.vars.map["a]b"][.vars.k]`, `.vars.piped`}
	if err != nil || strings.Join(reads, " ") != strings.Join(want, " ") {
		t.Fatal(reads, err)
	}
	for _, path := range reads[1:] {
		if _, err := Path(path); err != nil {
			t.Fatal(err)
		}
	}
	s := Scope{"node": map[string]any{"name": "a"}, "outputs": map[string]any{"inspect": map[string]any{
		"a": map[string]any{"token": Sensitive("token-a")}, "b": map[string]any{"token": &Secret{Name: "b"}},
	}}}
	for _, source := range []string{`{{ (index .outputs.inspect .node.name).token }}`, `{{ index .outputs.inspect .node.name "token" }}`, `{{ with index .outputs.inspect "a" }}{{ .token }}{{ end }}`, `{{ $a := index $.outputs.inspect .node.name }}{{ $a.token }}`} {
		local, trace := s.Trace()
		rendered, err := local.Template(source)
		if err != nil || rendered != "token-a" || !trace.Used {
			t.Fatal(source, rendered, err)
		}
	}
	// Reading the whole collection meets the other host's undelivered export.
	if _, err := s.Template(`{{ range $k, $v := .outputs.inspect }}{{ index $.outputs.inspect $k }}{{ end }}`); err == nil {
		t.Fatal("whole collection read without its undelivered export")
	}
	if _, err := s.Template(`{{ index .outputs.inspect "c" }}`); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatal("missing index key accepted", err)
	}
}

func TestChainedTemplateReadsAreTracedAndListed(t *testing.T) {
	s := Scope{"secrets": map[string]any{"creds": &Secret{Name: "creds", Data: map[string]any{"password": "sentinel"}, Available: true}}}
	for _, source := range []string{`{{ (.secrets.creds).password }}`, `{{ ((.secrets).creds).password }}`, `{{ ($.secrets.creds).password }}`, `{{ with .secrets }}{{ (.creds).password }}{{ end }}`} {
		local, trace := s.Trace()
		rendered, err := local.Template(source)
		if err != nil || rendered != "sentinel" || !trace.Used {
			t.Fatal("chained read escaped sensitivity", source, rendered, err)
		}
	}
	reads, err := TemplateReads(`{{ (.secrets.creds).password }}{{ (printf "%s" .vars.a).b }}{{ range .vars.list }}{{ .element }}{{ $.vars.outer }}{{ end }}{{ define "x" }}{{ .field }}{{ end }}{{ template "x" .vars.item }}`)
	want := ".secrets.creds.password .vars.a .vars.item .vars.list .vars.outer"
	if err != nil || strings.Join(reads, " ") != want {
		t.Fatal(reads, err)
	}
	// A template call renders from its argument, and only the read values are
	// consulted: the unrelated unavailable secret does not block rendering.
	s["vars"] = map[string]any{"a": map[string]any{"port": 1}, "b": map[string]any{"port": 2}}
	s["secrets"].(map[string]any)["other"] = &Secret{Name: "other"}
	local, trace := s.Trace()
	rendered, err := local.Template(`{{ define "port" }}{{ .port }}{{ end }}{{ template "port" .vars.a }},{{ template "port" .vars.b }},{{ template "user" .secrets.creds }}{{ define "user" }}{{ .password }}{{ end }}`)
	if err != nil || rendered != "1,2,sentinel" || !trace.Used {
		t.Fatal(rendered, err)
	}
}

// A login that MMX hands only to a tool: a secret in it marks no trace, so
// it does not hide the task's result, and the task's trace remembers its
// text for the replacement. Every other read of a secret still marks.
func TestLoginHandedOnlyToAToolMarksNoTraceAndIsRemembered(t *testing.T) {
	scope := func() (Scope, Scope, *Trace, *Trace) {
		s := Scope{"secrets": map[string]any{
			"password": &Secret{Name: "password", Data: "s3cret-sentinel", Available: true},
			"empty":    &Secret{Name: "empty", Data: "", Available: true},
			"number":   &Secret{Name: "number", Data: int64(4711), Available: true},
			"lost":     &Secret{Name: "lost"},
		}, "vars": map[string]any{"user": "deploy"}}
		// A node task inside a role: the role's trace is the parent.
		role, roleTrace := s.Trace()
		task, taskTrace := role.Trace()
		return role, task, roleTrace, taskTrace
	}
	role, task, roleTrace, taskTrace := scope()
	login, err := task.LoginHandedOnlyToATool("{{ .secrets.password }}")
	if err != nil || Plain(login) != "s3cret-sentinel" || !HasSecrets(login) {
		t.Fatal(login, err)
	}
	if taskTrace.Used || roleTrace.Used {
		t.Fatal("a login handed only to a tool marked a trace")
	}
	if got := task.LoginsHandedOnlyToATool(); len(got) != 1 || got[0] != "s3cret-sentinel" {
		t.Fatal("the task's trace did not remember the login", got)
	}
	if got := role.LoginsHandedOnlyToATool(); len(got) != 0 {
		t.Fatal("the role's trace remembered the login of a task in it", got)
	}
	// A second try resolves the login again; it is remembered once.
	if _, err := task.LoginHandedOnlyToATool("{{ .secrets.password }}"); err != nil || len(task.LoginsHandedOnlyToATool()) != 1 {
		t.Fatal(task.LoginsHandedOnlyToATool(), err)
	}
	// A login without a secret is not remembered: written in the playbook,
	// or read from a variable.
	for _, written := range []string{"deploy", "{{ .vars.user }}"} {
		if login, err := task.LoginHandedOnlyToATool(written); err != nil || login != "deploy" {
			t.Fatal(written, login, err)
		}
	}
	// A text with a secret in it is one login, remembered whole. A number
	// is remembered as the text that the tool gets.
	if login, err := task.LoginHandedOnlyToATool("robot-{{ .secrets.password }}"); err != nil || Plain(login) != "robot-s3cret-sentinel" {
		t.Fatal(login, err)
	}
	if _, err := task.LoginHandedOnlyToATool("{{ .secrets.number }}"); err != nil {
		t.Fatal(err)
	}
	// An empty login is never remembered, and neither is an empty form.
	if login, err := task.LoginHandedOnlyToATool("{{ .secrets.empty }}"); err != nil || Plain(login) != "" {
		t.Fatal(login, err)
	}
	task.FormOfLoginHandedOnlyToATool("")
	task.FormOfLoginHandedOnlyToATool("czNjcmV0")
	task.FormOfLoginHandedOnlyToATool("czNjcmV0")
	want := []string{"s3cret-sentinel", "robot-s3cret-sentinel", "4711", "czNjcmV0"}
	if got := task.LoginsHandedOnlyToATool(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("remembered %q, want %q", got, want)
	}
	if taskTrace.Used || roleTrace.Used {
		t.Fatal("a login handed only to a tool marked a trace")
	}
	// A secret that is not available fails the read, as any read of it does.
	var unavailable *UnavailableError
	if _, err := task.LoginHandedOnlyToATool("{{ .secrets.lost }}"); !errors.As(err, &unavailable) {
		t.Fatal(err)
	}
	// Every other read of a secret in the same task still marks its trace
	// and the role's.
	if _, err := task.Resolve("{{ .secrets.password }}"); err != nil || !taskTrace.Used || !roleTrace.Used {
		t.Fatal("an ordinary read of a secret no longer marks the trace", err)
	}
	// A scope without a trace resolves a login and remembers nothing.
	plain := Scope{"secrets": map[string]any{"password": Sensitive("s3cret-sentinel")}}
	if login, err := plain.LoginHandedOnlyToATool("{{ .secrets.password }}"); err != nil || Plain(login) != "s3cret-sentinel" || plain.LoginsHandedOnlyToATool() != nil {
		t.Fatal(login, err)
	}
}

func TestTextWithoutLoginsHandedOnlyToATool(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA==\n-----END OPENSSH PRIVATE KEY-----"
	for _, c := range []struct {
		name   string
		text   string
		logins []string
		want   string
	}{
		{"the rest of the text stays", "pull access denied for shop/web: login s3cret refused", []string{"s3cret"}, "pull access denied for shop/web: login [redacted] refused"},
		{"every occurrence", "s3cret, s3cret and s3cret", []string{"s3cret"}, "[redacted], [redacted] and [redacted]"},
		{"several logins", "user deploy, password s3cret", []string{"deploy", "s3cret"}, "user [redacted], password [redacted]"},
		{"one login inside another", "deploy-token and deploy", []string{"deploy", "deploy-token"}, "[redacted] and [redacted]"},
		{"two logins that share characters", "userpass", []string{"user", "rpass"}, "[redacted]"},
		{"a login that overlaps itself", "aaa and aa", []string{"aa"}, "[redacted] and [redacted]"},
		{"a key of several lines", "Load key: " + key + "\nPermission denied", []string{key}, "Load key: [redacted]\nPermission denied"},
		{"an empty login replaces nothing", "manifest unknown", []string{""}, "manifest unknown"},
		{"no login", "manifest unknown", nil, "manifest unknown"},
		{"no occurrence", "manifest unknown", []string{"s3cret"}, "manifest unknown"},
		{"only an exact occurrence", "S3CRET s3cre", []string{"s3cret"}, "S3CRET s3cre"},
		{"text of other alphabets", "Zugriff für späße verweigert: päß", []string{"päß"}, "Zugriff für s[redacted]e verweigert: [redacted]"},
	} {
		if got := TextWithoutLoginsHandedOnlyToATool(c.text, c.logins); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWithoutLoginsHandedOnlyToAToolReachesEveryTextInAValue(t *testing.T) {
	logins := []string{"s3cret"}
	secret := Sensitive("s3cret")
	var none map[string]any
	v := map[string]any{
		"image_id": "sha256:1", "count": int64(3), "ready": true, "missing": nil, "kept": secret, "empty": none,
		"services": []any{map[string]any{"reason": "login s3cret refused", "s3cret-key": Truncated{"cut s3cret"}}, "s3cret"},
		"file":     Binary{[]byte("\xff s3cret \xfe")},
	}
	got := WithoutLoginsHandedOnlyToATool(v, logins).(map[string]any)
	services := got["services"].([]any)
	first := services[0].(map[string]any)
	if got["image_id"] != "sha256:1" || got["count"] != int64(3) || got["ready"] != true || got["missing"] != nil || got["kept"] != secret {
		t.Fatalf("values without a login changed: %#v", got)
	}
	if empty, ok := got["empty"].(map[string]any); !ok || empty != nil {
		t.Fatalf("an absent mapping became %#v", got["empty"])
	}
	if first["reason"] != "login [redacted] refused" || first["[redacted]-key"] != (Truncated{"cut [redacted]"}) || services[1] != "[redacted]" {
		t.Fatalf("%#v", services)
	}
	if file := got["file"].(Binary); string(file.Bytes) != "\xff [redacted] \xfe" {
		t.Fatalf("%q", file.Bytes)
	}
	// The value that was given is not changed in place.
	if v["services"].([]any)[1] != "s3cret" {
		t.Fatal("the given value was changed in place")
	}
	if WithoutLoginsHandedOnlyToATool("s3cret", nil) != "s3cret" || WithoutLoginsHandedOnlyToATool(nil, logins) != nil {
		t.Fatal("no login, or no value, must change nothing")
	}
}

// ResolveWithoutMarkingATrace is for a caller that only passes a value on,
// such as a role call with a secret for an input of its role. No trace is
// marked, the value keeps its secrecy, and whoever reads it from where the
// caller put it marks its own trace and every trace above it.
func TestResolveWithoutMarkingATracePassesASecretOn(t *testing.T) {
	s := Scope{"secrets": map[string]any{"password": &Secret{Name: "password", Data: "s3cret-sentinel", Available: true}, "lost": &Secret{Name: "lost"}}, "vars": map[string]any{"user": "deploy"}}
	roleCall, roleCallTrace := s.Trace()
	for written, want := range map[string]string{"{{ .secrets.password }}": "s3cret-sentinel", "token-{{ .secrets.password }}": "token-s3cret-sentinel"} {
		passed, err := roleCall.ResolveWithoutMarkingATrace(written)
		if err != nil || Plain(passed) != want || !HasSecrets(passed) {
			t.Fatal(written, passed, err)
		}
		if roleCallTrace.Used || len(roleCall.LoginsHandedOnlyToATool()) != 0 {
			t.Fatal("passing a secret on marked the trace, or remembered a login")
		}
		// The role's scope holds what was passed on. A node task of the role
		// that reads it marks its own trace and the role call's.
		role := Scope{"inputs": map[string]any{"password": passed}}
		role.InheritTrace(roleCall)
		task, taskTrace := role.Trace()
		if read, err := task.Resolve("{{ .inputs.password }}"); err != nil || Plain(read) != want || !taskTrace.Used || !roleCallTrace.Used {
			t.Fatal("a read of the passed secret did not mark the reader's trace and the role call's", read, err)
		}
		roleCallTrace.Used = false
	}
	// A value without a secret, a structure, and a failure resolve as with
	// Resolve.
	if v, err := roleCall.ResolveWithoutMarkingATrace(map[string]any{"user": "{{ .vars.user }}", "password": "{{ .secrets.password }}"}); err != nil ||
		v.(map[string]any)["user"] != "deploy" || !HasSecrets(v) || roleCallTrace.Used {
		t.Fatal(v, err)
	}
	var unavailable *UnavailableError
	if _, err := roleCall.ResolveWithoutMarkingATrace("{{ .secrets.lost }}"); !errors.As(err, &unavailable) {
		t.Fatal(err)
	}
}

// A value that holds a secret only at run time cannot select either; the
// error names both references and never the secret's value.
func TestASecretValueCannotSelectAnotherValue(t *testing.T) {
	s := Scope{"vars": map[string]any{"ports": map[string]any{"http": 80}, "which_port": Sensitive("http")}}
	_, err := s.Resolve("{{ .vars.ports[.vars.which_port] }}")
	want := ".vars.ports[.vars.which_port] uses .vars.which_port as a selector, but the selector holds a secret. Secret values cannot select another value. Read the secret directly as {{ .vars.which_port }}"
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "http") {
		t.Fatal(err)
	}
}

func TestTraceRemembersSecretTextsForCommandDiagnostics(t *testing.T) {
	scope, trace := (Scope{"secrets": map[string]any{"token": Sensitive("abcde"), "nested": Sensitive(map[string]any{"tokens": []any{"cdefg", "second"}})}, "vars": map[string]any{"public": "public"}}).Trace()
	if _, err := scope.Template(`{{ .secrets.token }} {{ .vars.public }}`); err != nil {
		t.Fatal(err)
	}
	child, _ := scope.Trace()
	if _, err := child.Resolve("{{ .secrets.nested }}"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Scope{scope, child} {
		got := TextWithoutLoginsHandedOnlyToATool("abcdefg second public", s.SecretTexts())
		if !trace.Used || got != "[redacted] [redacted] public" {
			t.Fatal("read secret texts or replacement rules lost:", got)
		}
	}
}

func TestSecretValueTextsIncludesMappingKeysAndEnvironmentForms(t *testing.T) {
	secret := Sensitive(map[string]any{"  private-key\n": []any{" private-line-one\n private-line-two \n", map[string]any{}}})
	texts := SecretValueTexts(map[string]any{"public-name": "public-text", "private": secret})
	environment, err := EnvironmentText(secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{environment, "private-key", "private-line-one", "private-line-two"} {
		if got := TextWithoutLoginsHandedOnlyToATool(text, texts); got != "[redacted]" {
			t.Errorf("secret form %q was not replaced: %q", text, got)
		}
	}
	if got := TextWithoutLoginsHandedOnlyToATool("public-name public-text", texts); got != "public-name public-text" {
		t.Fatal("public sibling was treated as secret:", got)
	}
}
