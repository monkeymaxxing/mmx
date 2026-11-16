package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A script that runs as written refuses a reference to a value MMX knows:
// MMX fills in none there, and the shell would get the braces. The message
// names the field and the reference and shows env.
func TestLiteralScriptsRefuseMMXReferences(t *testing.T) {
	for task, want := range map[string]string{
		`shell: {script: "echo {{ .vars.swap_mib }}"}`:                                                                                                            `shell.script): shell.script runs as written, so the shell would receive the literal text {{ .vars.swap_mib }}`,
		`shell: {script: "echo {{.vars.swap_mib}}"}`:                                                                                                              `env: {SWAP_MIB: "{{ .vars.swap_mib }}"} and $SWAP_MIB`,
		`shell: {script: "true", dryrun: "echo {{ .node.name }}"}`:                                                                                                "shell.dryrun): shell.dryrun runs as written",
		`shell: {script: "true", run_only_if_this_command_succeeds: {script: "test -e {{ .vars.path }}"}}`:                                                        "shell.run_only_if_this_command_succeeds.script)",
		`shell: {script: "true", skip_if_this_command_succeeds: {script: "test -e {{ .secrets.token }}"}}`:                                                        "shell.skip_if_this_command_succeeds.script)",
		`cron_job: {name: wp, cron_schedule: "*/5 * * * *", run_as_user_account: www-data, command: "cd {{ .vars.sites_dir }}/x && wp cron event run --due-now"}`: `cron_job.command): cron_job.command runs as written, so the shell would receive the literal text {{ .vars.sites_dir }}`,
		`wait_for: {command_succeeds: "curl {{ .node.name }}", wait_at_most: 10s}`:                                                                                "wait_for.command_succeeds)",
		`reboot: {skip_if_this_command_succeeds: {script: "test -e {{ .vars.reboot_done_marker }}"}}`:                                                             "reboot.skip_if_this_command_succeeds.script runs as written, so the shell would receive the literal text {{ .vars.reboot_done_marker }}",
		`reboot: {run_only_if_this_command_succeeds: "test -f {{ .vars.reboot_required_file }}"}`:                                                                 "and the module has no env input; use the node task's when condition",
		`systemd_scheduled_job: {name: report, systemd_calendar_schedule: daily, script: "report {{ .vars.x }}", run_as_user_account: root}`:                      "systemd_scheduled_job.script)",
		`file: {permissions_mode: '0644', path: /etc/app.conf, content: x, check_new_file: {shell: {script: "check {{ .new_file.path }}"}}}`:                      "file.check_new_file.shell.script)",
		`file: {permissions_mode: '0644', path: /etc/app.conf, content: x, check_new_file: {shell: {script: "true", dryrun: "check {{ .new_file.path }}"}}}`:      `env: {NEW_FILE_PATH: "{{ .new_file.path }}"} and $NEW_FILE_PATH`,
		`edit_file: {path: /etc/a, content_filter: {shell: {script: "sed s/a/{{ .vars.b }}/"}}}`:                                                                  "edit_file.content_filter.shell.script)",
		`shell: {script: "echo {{ index .vars \"x\" }}"}`:                                                                                                         "shell.script)",
	} {
		_, err := loadTask(t, task)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	// A loop's as name is a value MMX knows too.
	path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{loop: [a, b], as: site, shell: {script: 'echo {{ .site }}'}}]}]\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "{{ .site }}") {
		t.Fatal(err)
	}
	// So is a role input, in a role.
	path = fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{role: wp, inputs: {sites_dir: /srv/www}}]}]\n")
	write(t, filepath.Join(filepath.Dir(path), "roles/wp/main.yml"), "inputs: {sites_dir: {type: string, required: true}}\nnode_tasks: [{cron_job: {name: wp, cron_schedule: '*/5 * * * *', run_as_user_account: www-data, command: 'cd {{ .inputs.sites_dir }}/current && wp cron event run --due-now'}}]\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), `env: {SITES_DIR: "{{ .inputs.sites_dir }}"} and $SITES_DIR`) {
		t.Fatal(err)
	}
}

// Templates for other tools name no value MMX knows, and stay.
func TestLiteralScriptsKeepTemplatesForOtherTools(t *testing.T) {
	for _, task := range []string{
		`shell: {script: "docker inspect -f '{{.State.Status}}' app"}`,
		`shell: {script: "docker ps --format '{{ .Names }}'"}`,
		`shell: {script: "helm template x --set a={{ .Values.a }}"}`,
		`shell: {script: "echo '{{ item }}' | jinja2"}`,
		"shell: {script: \"echo {{`{{ .vars.x }}`}}\"}",
		`cron_job: {name: ps, cron_schedule: "@daily", run_as_user_account: root, command: "docker ps --format '{{.ID}}'"}`,
		`shell: {script: 'echo "$SITE"', env: {SITE: "{{ .vars.site }}"}}`,
	} {
		project := fixture(t, "vars: {site: a}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(project, "example", "", nil); err != nil {
			t.Error(task, err)
		}
	}
}
