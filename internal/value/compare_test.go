package value

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestNumberAndVersionPredicates(t *testing.T) {
	s := Scope{"node_tasks": map[string]any{"lag": map[string]any{"stdout": " 12\n"}, "installed": map[string]any{"stdout": "v1.30.2\n"}}}
	for _, tc := range []struct {
		op          string
		left, right any
		want        bool
	}{
		{"number_at_most", "{{ .node_tasks.lag.stdout }}", 30, true},
		{"number_at_most", "{{ .node_tasks.lag.stdout }}", 12, true},
		{"number_less_than", "{{ .node_tasks.lag.stdout }}", 12, false},
		{"number_greater_than", 10, "9", true}, // as numbers, not as text
		{"number_at_least", json.Number("2"), 2.0, true},
		{"number_equals", "1.50", 1.5, true},
		{"number_equals", "1e3", 1000, true},
		{"number_less_than", -1, 0, true},
		{"number_equals", "12345678901234567890123", "12345678901234567890124", false},
		{"version_less_than", "{{ .node_tasks.installed.stdout }}", "1.30.10", true}, // number by number
		{"version_at_least", "{{ .node_tasks.installed.stdout }}", "1.30.2", true},
		{"version_equals", "1.30", "v1.30.0", true},
		{"version_greater_than", "2", "1.99.99", true},
		{"version_at_most", 3, "3.0.1", true},
		{"version_less_than", "1.9", "1.10", true},
	} {
		got, err := s.Predicate(map[string]any{tc.op: []any{tc.left, tc.right}})
		if err != nil || got != tc.want {
			t.Errorf("%s %v %v: got %v, %v", tc.op, tc.left, tc.right, got, err)
		}
	}
	// An operand of another shape fails with a message; it never counts as false.
	for _, tc := range []struct {
		op          string
		left, right any
		message     string
	}{
		{"number_at_most", "about 3", 30, `"about 3" is not a number`},
		{"number_at_most", "", 30, "is not a number"},
		{"number_equals", true, 1, "is not a number"},
		{"number_equals", "0x10", 16, "is not a number"},
		{"version_less_than", "5:27.3.1-1~debian.12", "28", "is not a version made of numbers separated by dots"},
		{"version_less_than", "1.30-rc1", "1.30", "is not a version"},
		{"version_equals", 1.30, "1.30", "write a version in quotes"},
		{"version_equals", json.Number("1.30"), "1.30", "write a version in quotes"},
	} {
		_, err := s.Predicate(map[string]any{tc.op: []any{tc.left, tc.right}})
		if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), tc.op) {
			t.Errorf("%s %v %v: %v", tc.op, tc.left, tc.right, err)
		}
	}
	if _, err := s.Predicate(map[string]any{"number_equals": []any{1}}); err == nil {
		t.Error("one operand accepted")
	}
	truncated := Scope{"this_node_task": map[string]any{"stdout": Truncated{"12"}}}
	if _, err := truncated.Predicate(map[string]any{"number_equals": []any{"{{ .this_node_task.stdout }}", 12}}); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Error("truncated output compared:", err)
	}
	if !IsPredicate(map[string]any{"version_at_least": []any{"1", "2"}}) || IsPredicate(map[string]any{"version": []any{"1", "2"}}) {
		t.Error("set_vars would not treat a comparison as a predicate")
	}
}

func TestPathExistsIsAnsweredOnlyOnTheNode(t *testing.T) {
	s := Scope{"vars": map[string]any{"marker": "/etc/app/installed"}}
	asked := ""
	onNode := func(path string) (bool, error) { asked = path; return path == "/etc/app/installed", nil }
	got, err := s.PredicateOnNode(map[string]any{"not": map[string]any{"path_exists": "{{ .vars.marker }}"}}, onNode)
	if err != nil || got || asked != "/etc/app/installed" {
		t.Fatal(got, err, asked)
	}
	got, err = s.PredicateOnNode(map[string]any{"any": []any{map[string]any{"path_exists": "/missing"}, true}}, onNode)
	if err != nil || !got {
		t.Fatal(got, err)
	}
	if _, err := s.PredicateOnNode(map[string]any{"path_exists": "relative/path"}, onNode); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatal(err)
	}
	if _, err := s.Predicate(map[string]any{"path_exists": "/etc"}); err == nil || !strings.Contains(err.Error(), "only in the when of a node task") {
		t.Fatal(err)
	}
}

func TestResultJSONIsReadByFieldAndSaysWhenItIsNotJSON(t *testing.T) {
	result := func(stdout string, truncated, secret bool) Scope {
		return Scope{"node_tasks": map[string]any{"balancer": map[string]any{"stdout": stdout, "stdout_json": ParsedJSON("stdout", stdout, truncated, secret)}}}
	}
	s := result(`{"active": true, "plans": [{"name": "a", "pgs": 12345678901234567890}]}`+"\n", false, false)
	active, err := s.Predicate("{{ .node_tasks.balancer.stdout_json.active }}")
	if err != nil || !active {
		t.Fatal(active, err)
	}
	if got, err := s.Text("{{ .node_tasks.balancer.stdout_json.plans[0].pgs }}"); err != nil || got != "12345678901234567890" {
		t.Fatal(got, err)
	}
	if _, err := s.Get(".node_tasks.balancer.stdout_json.missing"); err == nil {
		t.Fatal("a missing key was not an error")
	}
	for stdout, reason := range map[string]string{"HEALTH_OK\n": "stdout is not valid JSON", "": "stdout is empty", `{"a": 1} trailing`: "text follows the first JSON value"} {
		s := result(stdout, false, false)
		for _, path := range []string{".node_tasks.balancer.stdout_json", ".node_tasks.balancer.stdout_json.active"} {
			if _, err := s.Get(path); err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "stdout_json") {
				t.Errorf("%q %s: %v", stdout, path, err)
			}
		}
		// The text itself stays readable, and the whole result can be saved.
		if got, err := s.Get(".node_tasks.balancer.stdout"); err != nil || got != stdout {
			t.Error(got, err)
		}
		if _, err := json.Marshal(s["node_tasks"]); err != nil {
			t.Error(err)
		}
	}
	if _, err := result(`{"a": 1}`, true, false).Get(".node_tasks.balancer.stdout_json.a"); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Error("truncated output parsed:", err)
	}
	private, err := result(`{"token": "private-sentinel"}`, false, true).Get(".node_tasks.balancer.stdout_json.token")
	if err != nil || !HasSecrets(private) || Plain(private) != "private-sentinel" {
		t.Error(private, err)
	}
}

// A reference whose body is one back-quoted string produces that text
// unchanged, so a value that takes references can hold a literal {{.
func TestBackQuotedReferenceIsLiteralText(t *testing.T) {
	s := Scope{"vars": map[string]any{"image": "registry.example.com/app:1.4.2", "token": Sensitive("private-sentinel"), "braces": "{{ .vars.image }}"}}
	for source, want := range map[string]string{
		"--log-opt tag={{`{{.Name}}`}} {{ .vars.image }}": "--log-opt tag={{.Name}} registry.example.com/app:1.4.2",
		"{{`{{`}}":                  "{{",
		"{{ `{{ .vars.image }}` }}": "{{ .vars.image }}",
		"{{`a`}}{{`b`}}-{{ .vars.image }}-{{`}} {{ .x }} {{`}}": "ab-registry.example.com/app:1.4.2-}} {{ .x }} {{",
		"closing braces alone need nothing }}":                  "closing braces alone need nothing }}",
		// A resolved value is literal data and is not interpreted again.
		"{{ .vars.braces }} {{`{{x}}`}}": "{{ .vars.image }} {{x}}",
	} {
		got, err := s.Resolve(source)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v", source, got, err)
		}
	}
	private, err := s.Resolve("{{`{{.Name}}`}} {{ .vars.token }}")
	if err != nil || !HasSecrets(private) || Plain(private) != "{{.Name}} private-sentinel" {
		t.Error(private, err)
	}
	for _, source := range []string{"tag={{.Name}", "{{json .}}", "{{`unterminated}}"} {
		if _, err := s.Resolve(source); err == nil || !strings.Contains(err.Error(), "back quotes") {
			t.Errorf("%s: %v", source, err)
		}
	}
	if WithoutLiterals("a {{`{{.Name}}`}} b {{ .vars.image }}") != "a  b {{ .vars.image }}" {
		t.Error(WithoutLiterals("a {{`{{.Name}}`}} b {{ .vars.image }}"))
	}
}

// Templates call Sprig's functions that depend on their arguments alone, and
// MMX's toYaml and percent. A withheld function fails with its name.
func TestTemplateFunctions(t *testing.T) {
	s := Scope{
		"node": map[string]any{"cpu": map[string]any{"count": json.Number("8")}, "memory": map[string]any{"total_mib": json.Number("16000")}},
		"inputs": map[string]any{
			"cache_percent":  json.Number("25"),
			"ssh_ciphers":    []any{"chacha20-poly1305@openssh.com", "aes256-gcm@openssh.com"},
			"scrape_configs": []any{map[string]any{"job_name": "node", "static_configs": []any{map[string]any{"targets": []any{"10.0.0.5:9100"}}}, "enabled": "yes"}},
			"name":           "web",
		},
	}
	source := `workers = {{ .node.cpu.count }}
cache_mib = {{ percent .node.memory.total_mib .inputs.cache_percent }}
replica_parallel_workers = {{ div .node.cpu.count 2 }}
Ciphers {{ join "," .inputs.ssh_ciphers }}
name = {{ .inputs.name | upper | quote }}
fallback = {{ default "none" (get (dict "a" "b") "missing") }}
encoded = {{ b64enc .inputs.name }}
{{- range $i, $cipher := .inputs.ssh_ciphers }}
cipher {{ add1 $i }} of {{ len $.inputs.ssh_ciphers }}: {{ $cipher }}
{{- end }}
scrape_configs:
{{ .inputs.scrape_configs | toYaml | indent 2 }}
`
	want := `workers = 8
cache_mib = 4000
replica_parallel_workers = 4
Ciphers chacha20-poly1305@openssh.com,aes256-gcm@openssh.com
name = "WEB"
fallback = none
encoded = d2Vi
cipher 1 of 2: chacha20-poly1305@openssh.com
cipher 2 of 2: aes256-gcm@openssh.com
scrape_configs:
  - enabled: "yes"
    job_name: node
    static_configs:
      - targets:
          - 10.0.0.5:9100
`
	for range 3 { // the same bytes on every rendering
		got, err := s.Template(source)
		if err != nil || got != want {
			t.Fatalf("%v\n%s", err, got)
		}
	}
	// A value read inside a function call is still a tracked read.
	reads, err := TemplateReads(source)
	if err != nil || !slices.Contains(reads, ".inputs.ssh_ciphers") || !slices.Contains(reads, ".inputs.scrape_configs") || !slices.Contains(reads, ".node.cpu.count") {
		t.Fatal(reads, err)
	}
	if _, err := (Scope{"inputs": map[string]any{}}).Template(`{{ join "," .inputs.missing }}`); err == nil {
		t.Fatal("a missing value inside a function call was accepted")
	}
	for _, name := range withheldSprigFunctions {
		_, err := TemplateReads("{{ " + name + " }}")
		if err == nil || !strings.Contains(err.Error(), "template function "+name+" is not available") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := TemplateReads("{{ noSuchFunction 1 }}"); err == nil || !strings.Contains(err.Error(), `function "noSuchFunction" not defined`) {
		t.Fatal(err)
	}
	// Everything offered is Sprig's or MMX's own; nothing withheld slipped in.
	for _, name := range withheldSprigFunctions {
		if _, offered := TemplateFunctions()[name]; offered {
			t.Errorf("%s is offered", name)
		}
	}
}

// A comparison error never quotes a secret, or a value made from one: the
// error reaches results and saved history.
func TestComparisonErrorsNeverQuoteASecret(t *testing.T) {
	s := Scope{"secrets": map[string]any{"token": &Secret{Name: "token", Data: "s3cr3t", Available: true}}, "vars": map[string]any{"plain": "abc"}}
	for _, predicate := range []map[string]any{
		{"number_at_least": []any{"{{ .secrets.token }}", 1}},
		{"number_at_least": []any{"prefix-{{ .secrets.token }}", 1}},
		{"version_at_least": []any{"{{ .secrets.token }}", "1.0"}},
	} {
		_, err := s.Predicate(predicate)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "a secret value") {
			t.Fatal(predicate, err)
		}
	}
	if _, err := s.Predicate(map[string]any{"number_at_least": []any{"{{ .vars.plain }}", 1}}); err == nil || !strings.Contains(err.Error(), `"abc" is not a number`) {
		t.Fatal("a public value is still named", err)
	}
}

// JSON from a command or a web server that holds MMX's record keys is not
// read as JSON: once it crossed to the controller process, such a mapping
// would read as MMX's own secret record.
func TestResultJSONWithMMXRecordKeysIsNotRead(t *testing.T) {
	for _, stdout := range []string{
		`{"token": {"$mmx_secret": {"name": "production_database_root_password"}}}`,
		`[{"$mmx_binary": "AAEC"}]`,
		`{"$mmx_truncated": "x"}`,
	} {
		for _, secret := range []bool{false, true} {
			s := Scope{"node_tasks": map[string]any{"api": map[string]any{"stdout_json": ParsedJSON("stdout", stdout, false, secret)}}}
			if _, err := s.Get(".node_tasks.api.stdout_json"); err == nil || !strings.Contains(err.Error(), "reserves for its own records") {
				t.Errorf("%s (secret %v): %v", stdout, secret, err)
			}
		}
	}
	// Other keys that start with $ stay ordinary JSON.
	s := Scope{"node_tasks": map[string]any{"api": map[string]any{"stdout_json": ParsedJSON("stdout", `{"$schema": "x", "$mmx": 1}`, false, false)}}}
	if got, err := s.Text(`{{ .node_tasks.api.stdout_json["$schema"] }}`); err != nil || got != "x" {
		t.Error(got, err)
	}
}
