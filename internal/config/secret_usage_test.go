package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// usageLines prints a key's uses as mmx secrets usage shows them: the place,
// what uses the key there, and below a role call the uses inside the role.
func usageLines(uses []SecretUse, indent string) []string {
	var lines []string
	for _, use := range uses {
		lines = append(lines, indent+use.Place()+"  "+use.UsedBy)
		lines = append(lines, usageLines(use.UsesInsideTheRole, indent+"  ")...)
	}
	return lines
}

func secretUsageFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range map[string]string{
		"mmx_project_settings.yml": `project_name: shop
default_inventory: production
allow_role_calls_within_roles: true
secrets_from_env:
  api_token: SHOP_API_TOKEN
vars:
  ports: {web: 443}
  note: "vars are literal data: {{ .secrets.text_in_a_variable }}"
`,
		"inventories/production.yml": `nodes:
  web01:
    address: 192.0.2.10
    sudo_password: "{{ .secrets.sudo_password }}"
  db01:
    address: 192.0.2.11
    ssh_password: "{{ .secrets.ssh_password }}"
    sudo_password: "{{ .secrets.sudo_password }}"
`,
		"inventories/staging.yml": `vault_file: staging_vault.age
nodes:
  stage01:
    address: 192.0.2.20
  web01:
    address: 192.0.2.21
  db01:
    address: 192.0.2.22
    sudo_password: "{{ .secrets.staging_sudo_password }}"
`,
		"playbooks/site.yml": `fleet_stages:
  - id: database
    nodes: db01
    when: {equals: ["{{ .secrets.feature_switch }}", "on"]}
    node_tasks:
      - role: database
        inputs:
          root_password: "{{ .secrets.database_password }}"
          port: 3306
      - shell:
          script: mysqladmin ping
          env:
            DATABASE_PASSWORD: "{{ .secrets.database_password }}"
            API_TOKEN: "{{ .secrets.api_token }}"
          changed_when: {contains: ["{{ .this_node_task.stdout }}", "{{ .secrets.marker }}"]}
        retry:
          until: {equals: ["{{ .this_node_task.stdout }}", "{{ .secrets.marker }}"]}
      - file:
          path: /etc/app/cookie
          content: "{{ .secrets.session_cookie_secret }}"
          permissions_mode: "0600"
          check_new_file:
            shell:
              script: test -s "$FILE"
              env:
                FILE: "{{ .new_file.path }}"
                TOKEN: "{{ .secrets.api_token }}"
        when: {equals: ["{{ .secrets.feature_switch }}", "on"]}
      - file:
          path: /etc/app/users
          template: templates/users.tmpl
          permissions_mode: "0600"
        loop: "{{ .secrets.application_users }}"
        as: user
      - file:
          path: /etc/app/by-node
          template: "templates/{{ .node.name }}.tmpl"
          permissions_mode: "0600"
      - show: "{{ .vars.note }}"
      - set_vars:
          database_address: "mysql://app:{{ .secrets.database_password }}@localhost"
      - assert:
          that: {equals: ["{{ .secrets.feature_switch }}", "on"]}
      - greeting:
          person: "{{ .secrets.person }}"
      - task_block:
          id: web
          changes:
            - file:
                path: /etc/app/web.conf
                content: "token={{ .secrets.api_token }}\n"
                permissions_mode: "0600"
        when: {equals: ["{{ .secrets.feature_switch }}", "on"]}
    outputs:
      token: "{{ .secrets.api_token }}"
    save_on_controller_machine:
      - template: templates/summary.tmpl
        path: summary.txt
        permissions_mode: "0600"
`,
		"playbooks/deploy.yml": `fleet_stages:
  - id: each
    loop: "{{ .secrets.release_targets }}"
    as: target
    key: "{{ .target.name }}"
    fleet_stages:
      - id: deploy
        nodes: web01
        node_tasks:
          - role: database
            inputs:
              root_password: "{{ .secrets.database_password }}"
`,
		"templates/users.tmpl":   "{{ .user.name }}:{{ .secrets.user_salt }}\n",
		"templates/db01.tmpl":    "{{ .secrets.per_node_template_key }}\n",
		"templates/summary.tmpl": "{{ .secrets.summary_key }}\n",
		"roles/database/main.yml": `inputs:
  root_password: {type: string, required: true, secret: true}
  port: {type: integer, default: 3306}
node_tasks:
  - apt_package:
      packages: [{name: mariadb-server, version: newest_when_first_installed}]
  - file:
      path: /root/.my.cnf
      content: "[client]\nport={{ .inputs.port }}\npassword={{ .inputs.root_password }}\n"
      permissions_mode: "0600"
  - file:
      path: /etc/mysql/app.cnf
      template: app.cnf.tmpl
      permissions_mode: "0600"
    when: {not: {equals: ["{{ .inputs.root_password }}", ""]}}
  - task_block:
      id: reload
      changes:
        - file:
            path: /etc/mysql/extra.cnf
            content: "{{ .inputs.root_password }}"
            permissions_mode: "0600"
  - role: backup
    inputs:
      login_password: "{{ .inputs.root_password }}"
outputs:
  address: "mysql://root:{{ .inputs.root_password }}@localhost"
`,
		"roles/database/app.cnf.tmpl": "password={{ .inputs.root_password }}\n",
		"roles/backup/main.yml": `inputs:
  login_password: {type: string, required: true, secret: true}
node_tasks:
  - shell:
      script: backup
      env:
        MYSQL_PWD: "{{ .inputs.login_password }}"
`,
		"external_modules/greeting/module.star": "def run_node_task(inputs):\n    return None\n",
		"external_modules/greeting/module_description.yml": `summary: Greets one person.
inputs:
  person: {type: string, required: true, description: The person to greet.}
examples:
  - description: Greet Barbara.
    config: "greeting: {person: Barbara}"
`,
		"one_time_playbooks/2026-10-05_remove_old_token.yml": `fleet_stages:
  - id: remove
    nodes: all
    node_tasks:
      - shell:
          script: revoke
          env:
            OLD_TOKEN: "{{ .secrets.old_cdn_token }}"
`,
		"one_time_playbooks/completed/2026-09-01_remove_older_token.yml": `# Completed 2026-09-02: done on all 2 servers.
fleet_stages:
  - id: remove
    nodes: all
    node_tasks:
      - shell:
          script: revoke
          env:
            OLDER_TOKEN: "{{ .secrets.completed_only }}"
`,
	} {
		write(t, filepath.Join(root, name), text)
	}
	return filepath.Join(root, "mmx_project_settings.yml")
}

// Every place where the loader accepts a .secrets reference is a use, and a
// key that a role call passes to a role input is followed into the role.
func TestSecretUsageListsEveryPlaceThatReadsAKey(t *testing.T) {
	settings := secretUsageFixture(t)
	usage, err := SecretUsageOf(settings, "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Vault != filepath.Join(filepath.Dir(settings), "secrets_vault.age") || !reflect.DeepEqual(usage.FromEnvironment, map[string]string{"api_token": "SHOP_API_TOKEN"}) {
		t.Fatal(usage.Vault, usage.FromEnvironment)
	}
	insideTheRole := []string{
		"  roles/database/main.yml:9  file /root/.my.cnf (content)",
		"  roles/database/main.yml:13  file /etc/mysql/app.cnf (template)",
		"  roles/database/main.yml:15  file /etc/mysql/app.cnf (when)",
		"  roles/database/main.yml:21  file /etc/mysql/extra.cnf (content)",
		"  roles/database/main.yml:25  role backup, input login_password",
		"    roles/backup/main.yml:7  shell env MYSQL_PWD",
		"  roles/database/main.yml:27  role database, output address",
	}
	want := map[string][]string{
		"sudo_password": {"inventories/production.yml:4  node web01, sudo_password", "inventories/production.yml:8  node db01, sudo_password"},
		"ssh_password":  {"inventories/production.yml:7  node db01, ssh_password"},
		"database_password": append(append(append([]string{"playbooks/deploy.yml:12  role database, input root_password"}, insideTheRole...),
			"playbooks/site.yml:8  role database, input root_password"), append(insideTheRole,
			"playbooks/site.yml:13  shell env DATABASE_PASSWORD",
			"playbooks/site.yml:41  set_vars database_address")...),
		"api_token": {
			"playbooks/site.yml:14  shell env API_TOKEN",
			"playbooks/site.yml:27  file /etc/app/cookie (check_new_file.shell.env.TOKEN)",
			"playbooks/site.yml:51  file /etc/app/web.conf (content)",
			"playbooks/site.yml:55  fleet stage database, output token",
		},
		"marker": {"playbooks/site.yml:15  shell changed_when", "playbooks/site.yml:17  shell retry until"},
		"feature_switch": {
			"playbooks/site.yml:4  fleet stage database, when",
			"playbooks/site.yml:28  file /etc/app/cookie (when)",
			"playbooks/site.yml:43  assert that",
			"playbooks/site.yml:53  task_block web (when)",
		},
		"session_cookie_secret": {"playbooks/site.yml:20  file /etc/app/cookie (content)"},
		"application_users":     {"playbooks/site.yml:33  file /etc/app/users (loop)"},
		"user_salt":             {"playbooks/site.yml:31  file /etc/app/users (template)"},
		// The template is chosen for each node; the key is listed at the node task.
		"per_node_template_key": {"playbooks/site.yml:35  file /etc/app/by-node"},
		"person":                {"playbooks/site.yml:45  greeting person"},
		"summary_key":           {"playbooks/site.yml:57  fleet stage database, save_on_controller_machine[0].template"},
		"release_targets":       {"playbooks/deploy.yml:3  fleet stage each, loop"},
		// An outstanding one-time playbook is read; a completed one is not.
		"old_cdn_token": {"one_time_playbooks/2026-10-05_remove_old_token.yml:8  shell env OLD_TOKEN"},
	}
	got := map[string][]string{}
	for key, uses := range usage.Uses {
		got[key] = usageLines(uses, "")
	}
	for key := range want {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s:\n got:\n  %s\nwant:\n  %s", key, strings.Join(got[key], "\n  "), strings.Join(want[key], "\n  "))
		}
	}
	// A variable is literal data: the text of a reference inside it reads
	// no key, so text_in_a_variable is not among the keys.
	for key := range got {
		if want[key] == nil {
			t.Errorf("unexpected key %s:\n  %s", key, strings.Join(got[key], "\n  "))
		}
	}
	// The same keys as a run loads: every key of a playbook's fleet stages
	// that SecretNames finds is among the uses.
	for _, playbook := range []string{"site", "deploy", "2026-10-05_remove_old_token"} {
		p, _, err := Load(settings, playbook, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range SecretNames(p.FleetStages) {
			if len(usage.Uses[key]) == 0 {
				t.Errorf("a run of %s loads %s, which has no use", playbook, key)
			}
		}
	}
}

// The selected inventory decides the vault and the connection secrets.
func TestSecretUsageReadsTheSelectedInventory(t *testing.T) {
	settings := secretUsageFixture(t)
	usage, err := SecretUsageOf(settings, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Vault != filepath.Join(filepath.Dir(settings), "inventories/staging_vault.age") {
		t.Fatal(usage.Vault)
	}
	if got := usageLines(usage.Uses["staging_sudo_password"], ""); !reflect.DeepEqual(got, []string{"inventories/staging.yml:9  node db01, sudo_password"}) {
		t.Fatal(got)
	}
	if len(usage.Uses["sudo_password"])+len(usage.Uses["ssh_password"]) != 0 {
		t.Fatal("connection secrets of another inventory are listed")
	}
	if _, err := SecretUsageOf(settings, "missing"); err == nil {
		t.Fatal("an inventory that does not exist was accepted")
	}
	if _, err := SecretUsageOf(settings, "../production"); err == nil || !strings.Contains(err.Error(), "--inventory must be a name") {
		t.Fatal(err)
	}
}

// A project file that does not load is an error, never a shorter answer.
func TestSecretUsageFailsOnAPlaybookThatDoesNotLoad(t *testing.T) {
	settings := secretUsageFixture(t)
	write(t, filepath.Join(filepath.Dir(settings), "playbooks/broken.yml"), "fleet_stages:\n  - id: broken\n    nodes: nowhere\n    node_tasks: [{show: hi}]\n")
	if _, err := SecretUsageOf(settings, ""); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatal(err)
	}
}

func TestUsedByNamesTheNodeTaskAndTheField(t *testing.T) {
	settings := fixture(t, `fleet_stages:
  - id: names
    nodes: web1
    node_tasks:
      - {file: {path: /root/.my.cnf, content: x, permissions_mode: "0600"}}
      - {shell: {script: "true", env: {DATABASE_PASSWORD: x}}}
      - {systemd_unit: {name: app.service, content: "[Service]\nExecStart=/bin/true\n"}}
      - {http: {url: "https://api.example.net/health", headers: {Authorization: x}}}
      - {docker_image: {image: "registry.example.net/app:1", pull_image: when_missing}}
      - {show: hello}
`)
	p, _, err := Load(settings, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := p.FleetStages[0].NodeTasks
	for i, tc := range []struct{ field, want string }{
		{"file.content", "file /root/.my.cnf (content)"},
		{"shell.env.DATABASE_PASSWORD", "shell env DATABASE_PASSWORD"},
		{"systemd_unit.content", "systemd_unit app.service (content)"},
		{"http.headers.Authorization", "http https://api.example.net/health (headers.Authorization)"},
		{"docker_image.registry_password", "docker_image registry.example.net/app:1 (registry_password)"},
		{"show", "show"},
	} {
		if got := secretUsedBy(tasks[i], tc.field); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.field, got, tc.want)
		}
	}
	if got := secretUsedBy(tasks[0], ""); got != "file /root/.my.cnf" {
		t.Error(got)
	}
	if got := secretUsedBy(tasks[1], "when"); got != "shell when" {
		t.Error(got)
	}
}
