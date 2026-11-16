package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestSecretFilePermissionsCheckedWhileLoading(t *testing.T) {
	for _, tc := range []struct {
		mode, extra, refusal string
	}{
		{"0600", "", ""}, {"0700", "", ""}, {"0622", "", ""},
		{"0660", ", group: admin", "/etc/shop/database.env holds a secret, and permissions_mode 0660 lets group admin read it. If the group should read it, add secret_readable_by_group: true."},
		{"0644", "", "/etc/shop/database.env holds a secret, and permissions_mode 0644 lets every account on the node read it. If every account should read it, add dangerously_let_every_account_read_secret: true."},
		{"0640", ", secret_readable_by_group: true", ""},
		{"0644", ", dangerously_let_every_account_read_secret: true", ""},
		{"{{ .node.permissions_mode }}", "", ""},
		{"0640", ", secret_readable_by_group: '{{ .node.allow_group }}'", ""},
		{"0644", ", dangerously_let_every_account_read_secret: '{{ .node.allow_everyone }}'", ""},
	} {
		t.Run(tc.mode+tc.extra, func(t *testing.T) {
			path := fixture(t, fmt.Sprintf("fleet_stages: [{id: configure, nodes: all, node_tasks: [{show: before}, {file: {path: /etc/shop/database.env, content: '{{ .secrets.password }}', permissions_mode: '%s'%s}}]}]", tc.mode, tc.extra))
			_, _, err := Load(path, "example", "", nil)
			if tc.refusal == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("load must refuse before starting any node job: %v", err)
			}
		})
	}
}

func TestSecretRoleFilePermissionsCheckedWhileLoading(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{role: shop, inputs: {password: '{{ .secrets.password }}'}}]}]")
	write(t, filepath.Join(filepath.Dir(path), "roles/shop/main.yml"), "inputs: {password: {type: string, required: true, secret: true}}\nnode_tasks: [{file: {path: /etc/shop/database.env, content: '{{ .inputs.password }}', permissions_mode: '0640'}}]\n")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "/etc/shop/database.env holds a secret, and permissions_mode 0640") {
		t.Fatal(err)
	}
}

func TestReferencedSecretFileModeIsCheckedBeforeWriting(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "database.env")
	path := fixture(t, fmt.Sprintf("fleet_stages: [{id: configure, nodes: all, node_tasks: [{file: {path: %q, content: '{{ .secrets.password }}', permissions_mode: '{{ .node.permissions_mode }}'}}]}]", destination))
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []any{"0600", "0644", "0999", 420} {
		scope := value.Scope{"node": map[string]any{"permissions_mode": mode}, "secrets": map[string]any{"password": value.Sensitive("private")}}
		resolved, err := ResolveArguments(p.FleetStages[0].NodeTasks[0], scope)
		if mode == "0999" || mode == 420 {
			if err == nil || !strings.Contains(err.Error(), "permissions_mode") {
				t.Fatal("accepted invalid resolved mode", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		r := (modules.Runtime{Dryrun: true, BeforeMutation: func(string) error { t.Fatal("preview mutated"); return nil }}).File(context.Background(), *InputsOf[modules.File](resolved), scope)
		if mode == "0600" && (r.Status != "succeeded" || r.Change != "would_change") {
			t.Fatal(r)
		}
		if mode == "0644" && (r.Status != "failed" || !strings.Contains(r.Reason, destination+" holds a secret, and permissions_mode 0644")) {
			t.Fatal(r)
		}
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("refused write changed the file", err)
	}
}

func TestPublicFileWithSecretValidatorCanLoad(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{file: {path: /etc/shop/public.conf, content: public, permissions_mode: '0644', check_new_file: {shell: {script: 'test -n \"$TOKEN\"', env: {TOKEN: '{{ .secrets.token }}'}}}}}]}]")
	if _, _, err := Load(path, "example", "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBytesFrozenWithoutReferenceInterpretation(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("{{ .secrets.not_a_reference }}\n"), {0, 255, 254, 128, 10}} {
		path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{file: {permissions_mode: '0644', path: /etc/example, source: input}}]}]")
		source := filepath.Join(filepath.Dir(path), "input")
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		p, _, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if names := SecretNames(p); len(names) != 0 {
			t.Fatal("scanned source bytes", names)
		}
		dir := t.TempDir()
		if err := FreezeAssets(context.Background(), &p, dir, nil); err != nil {
			t.Fatal(err)
		}
		os.Remove(source)
		frozen, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var saved Project
		if err = json.Unmarshal(frozen, &saved); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(frozen, []byte("source_data")) {
			t.Fatal("embedded source")
		}
		if err := BindProjectAssets(&saved, dir); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(InputsOf[modules.File](saved.FleetStages[0].NodeTasks[0]).Asset.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("lost bytes: %x != %x", data, got)
		}
	}
}

func TestSourceAndLinkConfiguration(t *testing.T) {
	for _, task := range []string{
		"file: {path: /etc/example, state: absent}",
		"symlink: {path: /etc/link, target: '../config'}",
		"symlink: {path: /etc/link, state: absent}",
	} {
		path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		p, _, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if p.FleetStages[0].NodeTasks[0].Retry == nil {
			t.Fatal("missing default retry", task)
		}
	}
	for _, task := range []string{
		"file: {permissions_mode: '0644', path: /etc/example, source: missing}",
		"file: {permissions_mode: '0644', path: /etc/example, source: .}",
		"file: {permissions_mode: '0644', path: /etc/example, source: '{{ .vars.name }}'}",
		"file: {permissions_mode: '0644', path: /etc/example, source: x, content: y}",
		"file: {permissions_mode: '0644', path: /etc/example, source: x, template: y}",
		"file: {path: /etc/example, state: absent, content: y}",
		"file: {path: /etc/example, state: absent, permissions_mode: '0644'}",
		"file: {path: /etc/example, state: absent, check_new_file: {shell: {script: 'true'}}}",
		"file: {permissions_mode: '0644', path: /etc/example, state: typo, content: y}",
		"symlink: {path: relative, target: x}",
		"symlink: {path: /etc/link}",
		"symlink: {path: /etc/link, target: '{{ .vars.missing }}'}",
		"symlink: {path: /etc/link, target: x, state: absent}",
		"symlink: {path: /etc/link, target: x, state: typo}",
	} {
		path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", task)
		}
	}
}

func TestLineConfiguration(t *testing.T) {
	for _, task := range []string{
		`line: {insert_at_end_of_file: true, path: /etc/ssh/sshd_config, match: '^#?PasswordAuthentication\s', line: PasswordAuthentication no}`,
		`line: {insert_at_end_of_file: true, path: /etc/sysctl.conf, line: 'vm.swappiness = 10'}`,
		`line: {path: /etc/app.conf, match: '^a=', state: absent}`,
		`line: {path: /etc/app.conf, line: 'a=1', state: absent}`,
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '^{{ .vars.key }}=', line: 'b=1'}`,
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '^a=', line: '{{ .vars.good }}'}`,
		`loop: [{key: vm.swappiness, value: 10}], as: s, line: {insert_at_end_of_file: true, path: /etc/sysctl.conf, match: '^{{ .s.key }}\s*=', line: '{{ .s.key }} = {{ .s.value }}'}`,
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, line: 'a=1', check_new_file: {shell: {script: 'check "$CANDIDATE"', env: {CANDIDATE: '{{ .new_file.path }}'}}}}`,
	} {
		path := fixture(t, "vars: {key: a, good: 'a=1'}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		p, _, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if p.FleetStages[0].NodeTasks[0].Retry == nil {
			t.Fatal("missing default retry", task)
		}
	}
	for task, want := range map[string]string{
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '^a='}`:                                                   "line.line is required",
		`line: {path: /etc/app.conf, state: absent}`:                                                                               "exactly one of match or line",
		`line: {path: /etc/app.conf, match: '^a=', line: 'a=1', state: absent}`:                                                    "exactly one of match or line",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, line: x, state: typo}`:                                           "present or absent",
		`line: {insert_at_end_of_file: true, path: relative, line: x}`:                                                             "path must be absolute",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '(', line: x}`:                                            "line.match",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '^a=', line: 'b=1'}`:                                      "line must match",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, line: "a\nb"}`:                                                   "single line",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, line: x, check_new_file: {}}`:                                    "check_new_file requires shell",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, line: x, check_new_file: {shell: {script: 'true'}}, content: x}`: "unknown field",
		`line: {insert_at_end_of_file: true, path: /etc/app.conf, match: '^a=', line: '{{ .vars.bad }}'}`:                          "line must match",
	} {
		path := fixture(t, "vars: {bad: 'b=1'}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(task, err)
		}
	}
	// A match known only when the task runs is checked with its resolved value.
	task := Task{Module: "line", ModuleInputs: &modules.Line{InsertAtEndOfFile: true, Path: "/etc/app.conf", Match: "^{{ .vars.key }}=", Text: "{{ .vars.line }}"}}
	for line, ok := range map[string]bool{"a=1": true, "b=1": false} {
		s := value.Scope{"vars": map[string]any{"key": "a", "line": line}}
		r, err := ResolveArguments(task, s)
		if err != nil || InputsOf[modules.Line](r).Match != "^a=" {
			t.Fatal(InputsOf[modules.Line](r), err)
		}
		if _, err := InspectArguments(r, s, false); (err == nil) != ok {
			t.Fatal(line, err)
		}
	}
}

func TestFileOptionsLinePlacementEditFileAndLinkReplacement(t *testing.T) {
	for _, task := range []string{
		`file: {permissions_mode: '0644', path: /etc/a, content: x, if_file_exists: keep_existing_content}`,
		`file: {permissions_mode: '0644', path: /etc/a, content: x, if_file_exists: replace_existing_content}`,
		`download: {permissions_mode: '0644', url: 'https://example.com/a', sha256: '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', path: /usr/local/bin/a, if_file_exists: keep_existing_content}`,
		`file: {path: /etc/haproxy/site.pem, content_from_node_paths: [/etc/ssl/a.pem, '{{ .vars.key }}'], permissions_mode: '0640'}`,
		`file: {permissions_mode: '0644', path: /etc/ssh/sshd_config, content: x, backup_original_file_to: /etc/ssh/sshd_config.original}`,
		`line: {path: /etc/a, line: x, insert_at_start_of_file: true}`,
		`line: {path: /etc/a, line: x, insert_at_end_of_file: true}`,
		`line: {path: /etc/a, line: x, insert_before_first_line_matching: '^host\s'}`,
		`line: {path: /etc/a, line: x, insert_after_last_line_matching: '^{{ .vars.section }}$'}`,
		`edit_file: {path: /etc/fstab, content_filter: {shell: {script: 'sed s/a/b/'}}}`,
		`edit_file: {path: /etc/a, content_filter: {shell: {script: 'sed "s/a/$TO/"', env: {TO: '{{ .vars.section }}'}}}, check_new_file: {shell: {script: 'check "$C"', env: {C: '{{ .new_file.path }}'}}}}`,
		`symlink: {path: /etc/a, target: b, replace_existing_file: true}`,
	} {
		path := fixture(t, "vars: {key: /etc/ssl/key.pem, section: main}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		p, _, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if p.FleetStages[0].NodeTasks[0].Retry == nil {
			t.Fatal("missing default retry", task)
		}
	}
	for task, want := range map[string]string{
		`file: {permissions_mode: '0644', path: /etc/a, content: x, if_file_exists: keep}`:                                                                                                      "replace_existing_content or keep_existing_content",
		`file: {path: /etc/a, permissions_mode: '0644', if_file_exists: keep_existing_content}`:                                                                                                 "if_file_exists requires content",
		`file: {path: /etc/a, state: absent, if_file_exists: keep_existing_content}`:                                                                                                            "file with state: absent accepts only path and state",
		`file: {permissions_mode: '0644', path: /etc/a, content: x, content_from_node_paths: [/etc/b]}`:                                                                                         "only one of content, template, source, or content_from_node_paths",
		`file: {permissions_mode: '0644', path: /etc/a, content_from_node_paths: []}`:                                                                                                           "at least one path",
		`file: {permissions_mode: '0644', path: /etc/a, content_from_node_paths: [relative]}`:                                                                                                   "path must be absolute",
		`file: {path: /etc/a, state: absent, content_from_node_paths: [/etc/b]}`:                                                                                                                "file with state: absent accepts only path and state",
		`file: {permissions_mode: '0644', path: /etc/a, content: x, backup_original_file_to: relative}`:                                                                                         "path must be absolute",
		`file: {permissions_mode: '0644', path: /etc/a, content: x, backup_original_file_to: /etc/a}`:                                                                                           "must differ from file.path",
		`file: {path: /etc/a, state: absent, backup_original_file_to: /etc/b}`:                                                                                                                  "file with state: absent accepts only path and state",
		`download: {permissions_mode: '0644', url: 'https://example.com/a', sha256: '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', path: /a, backup_original_file_to: /b}`: "unknown field",
		`line: {path: /etc/a, line: x, insert_at_start_of_file: true, insert_at_end_of_file: true}`:                                                                                             "at most one",
		`line: {path: /etc/a, match: '^x', state: absent, insert_at_start_of_file: true}`:                                                                                                       "state: absent adds none",
		`line: {path: /etc/a, line: x, insert_after_first_line_matching: '('}`:                                                                                                                  "line.insert_after_first_line_matching",
		`edit_file: {path: /etc/a}`:                                                                        "content_filter is required",
		`edit_file: {path: /etc/a, content_filter: {shell: {script: ''}}}`:                                 "script is required",
		`edit_file: {path: /etc/a, content_filter: {shell: {script: x, dryrun: x}}}`:                       "unknown field",
		`edit_file: {path: /etc/a, content_filter: {shell: {script: x, env: {'1BAD': x}}}}`:                "invalid environment variable name",
		`edit_file: {path: relative, content_filter: {shell: {script: x}}}`:                                "path must be absolute",
		`edit_file: {path: /etc/a, content_filter: {shell: {script: x, env: {A: '{{ .vars.missing }}'}}}}`: "missing",
		`symlink: {path: /etc/a, state: absent, replace_existing_file: true}`:                              "absent symlink accepts only path and state",
	} {
		path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(task, err)
		}
	}
}

func TestSourceMustBeRegularBeforeDispatch(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{file: {permissions_mode: '0644', path: /etc/example, source: link}}]}]")
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, "example", "", nil); err == nil {
		t.Fatal("accepted symlink source")
	}
}
