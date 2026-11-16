package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// secretsUsageProject adds playbooks, a role, and connection secrets to the
// project of secretsProject, and a vault that defines some of the keys.
func secretsUsageProject(t *testing.T) (root, passwordFile string) {
	t.Helper()
	root, passwordFile = secretsProject(t)
	for name, text := range map[string]string{
		"mmx_project_settings.yml":   "project_name: shop\ndefault_inventory: production\nsecrets_from_env:\n  api_token: SHOP_API_TOKEN\n",
		"inventories/production.yml": "nodes:\n  web01:\n    address: 192.0.2.10\n    sudo_password: \"{{ .secrets.sudo_password }}\"\n",
		"playbooks/site.yml": `fleet_stages:
  - id: database
    nodes: web01
    node_tasks:
      - role: database
        inputs:
          root_password: "{{ .secrets.database_password }}"
      - shell:
          script: deploy
          env:
            API_TOKEN: "{{ .secrets.api_token }}"
            SESSION_COOKIE_SECRET: "{{ .secrets.session_cookie_secret }}"
`,
		"playbooks/deploy_platform.yml": `fleet_stages:
  - id: platform
    nodes: web01
    node_tasks:
      - shell:
          script: deploy
          env:
            DATABASE_PASSWORD: "{{ .secrets.database_password }}"
`,
		"roles/database/main.yml": `inputs:
  root_password: {type: string, required: true, secret: true}
node_tasks:
  - file:
      path: /root/.my.cnf
      content: "[client]\npassword={{ .inputs.root_password }}\n"
      permissions_mode: "0600"
`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// The vault also has a value for api_token, which the project reads from
	// the environment instead.
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase,
		"database_password: sentinel-database-value\nsudo_password: sentinel-token-value\nold_cdn_token: sentinel-changed-value\napi_token: sentinel-nested-value\n")
	return root, passwordFile
}

func TestSecretsUsageShowsWhereEachKeyIsUsed(t *testing.T) {
	root, passwordFile := secretsUsageProject(t)
	stdout, stderr, code := secretsOutput(t, "usage", "--vault-password-file", passwordFile)
	want := `api_token               environment variable SHOP_API_TOKEN
  playbooks/site.yml:11             shell env API_TOKEN

database_password       vault secrets_vault.age
  playbooks/deploy_platform.yml:8   shell env DATABASE_PASSWORD
  playbooks/site.yml:7              role database, input root_password
    roles/database/main.yml:6       file /root/.my.cnf (content)

old_cdn_token           vault secrets_vault.age
  (never referenced)

session_cookie_secret   not defined
  playbooks/site.yml:12             shell env SESSION_COOKIE_SECRET

sudo_password           vault secrets_vault.age
  inventories/production.yml:4      node web01, sudo_password

Referenced but not defined:   session_cookie_secret (playbooks/site.yml:12)
Defined but never referenced: old_cdn_token
`
	// A referenced key that is not defined makes the exit code 1, for CI.
	if code != 1 || stderr != "" || stdout != want {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}

	// One key: its lines are the whole answer, and the exit code is about
	// that key.
	stdout, stderr, code = secretsOutput(t, "usage", "database_password", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != `database_password   vault secrets_vault.age
  playbooks/deploy_platform.yml:8   shell env DATABASE_PASSWORD
  playbooks/site.yml:7              role database, input root_password
    roles/database/main.yml:6       file /root/.my.cnf (content)
` {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if stdout, _, code = secretsOutput(t, "usage", "--vault-password-file", passwordFile, "session_cookie_secret"); code != 1 || stdout != "session_cookie_secret   not defined\n  playbooks/site.yml:12   shell env SESSION_COOKIE_SECRET\n" {
		t.Fatalf("exit code %d\n%s", code, stdout)
	}
	if stdout, _, code = secretsOutput(t, "usage", "old_cdn_token", "--vault-password-file", passwordFile); code != 0 || stdout != "old_cdn_token   vault secrets_vault.age\n  (never referenced)\n" {
		t.Fatalf("exit code %d\n%s", code, stdout)
	}

	// JSON gives the same to agents.
	type use struct {
		File              string
		Line              int
		UsedBy            string `json:"used_by"`
		UsesInsideTheRole []use  `json:"uses_inside_the_role"`
	}
	type answer struct {
		SchemaVersion int `json:"schema_version"`
		Keys          []struct {
			Key    string
			Source map[string]string
			Uses   []use
		}
		ReferencedButNotDefined   []string `json:"referenced_but_not_defined"`
		DefinedButNeverReferenced []string `json:"defined_but_never_referenced"`
	}
	var all answer
	stdout, stderr, code = secretsOutput(t, "usage", "--json", "--vault-password-file", passwordFile)
	if code != 1 || stderr != "" || json.Unmarshal([]byte(stdout), &all) != nil || all.SchemaVersion != 1 || len(all.Keys) != 5 {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	vault := filepath.Join(root, "secrets_vault.age")
	if key := all.Keys[0]; key.Key != "api_token" || !reflect.DeepEqual(key.Source, map[string]string{"env_var": "SHOP_API_TOKEN"}) ||
		!reflect.DeepEqual(key.Uses, []use{{"playbooks/site.yml", 11, "shell env API_TOKEN", nil}}) {
		t.Fatal(key)
	}
	if key := all.Keys[1]; key.Key != "database_password" || !reflect.DeepEqual(key.Source, map[string]string{"vault": vault}) || !reflect.DeepEqual(key.Uses, []use{
		{"playbooks/deploy_platform.yml", 8, "shell env DATABASE_PASSWORD", nil},
		{"playbooks/site.yml", 7, "role database, input root_password", []use{{"roles/database/main.yml", 6, "file /root/.my.cnf (content)", nil}}},
	}) {
		t.Fatal(key)
	}
	// A key without a source and a key without a use are both in keys.
	if !strings.Contains(stdout, `"key": "session_cookie_secret",`+"\n      "+`"source": null,`) || !strings.Contains(stdout, `"key": "old_cdn_token",`) || !strings.Contains(stdout, `"uses": []`) {
		t.Fatal(stdout)
	}
	if !reflect.DeepEqual(all.ReferencedButNotDefined, []string{"session_cookie_secret"}) || !reflect.DeepEqual(all.DefinedButNeverReferenced, []string{"old_cdn_token"}) {
		t.Fatal(all.ReferencedButNotDefined, all.DefinedButNeverReferenced)
	}
	var one answer
	stdout, _, code = secretsOutput(t, "usage", "database_password", "--json", "--vault-password-file", passwordFile)
	if code != 0 || json.Unmarshal([]byte(stdout), &one) != nil || len(one.Keys) != 1 || one.Keys[0].Key != "database_password" ||
		!strings.Contains(stdout, `"referenced_but_not_defined": []`) || !strings.Contains(stdout, `"defined_but_never_referenced": []`) {
		t.Fatalf("exit code %d\n%s", code, stdout)
	}

	// Every referenced key is defined: the exit code is 0.
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\nsudo_password: sentinel-token-value\nsession_cookie_secret: sentinel-changed-value\n")
	stdout, stderr, code = secretsOutput(t, "usage", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || !strings.HasSuffix(stdout, "\nReferenced but not defined:   (none)\nDefined but never referenced: (none)\n") {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}

func TestSecretsUsageUnlocksTheVaultOnlyWhenThereIsOne(t *testing.T) {
	root, passwordFile := secretsUsageProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	wrong := filepath.Join(t.TempDir(), "wrong")
	os.WriteFile(wrong, []byte("another passphrase\n"), 0600)
	broken := filepath.Join(root, "playbooks/broken.yml")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no passphrase for a vault that exists": {[]string{"usage", "--non-interactive"}, "vault passphrase required; use --vault-password-file"},
		"a wrong passphrase":                    {[]string{"usage", "--vault-password-file", wrong}, "cannot decrypt vault: wrong passphrase or invalid age file"},
		"a key that nothing defines or reads":   {[]string{"usage", "no_such_key", "--vault-password-file", passwordFile}, "no secret key no_such_key: the vault secrets_vault.age and secrets_from_env do not define it, and no playbook, role, or inventory node reads .secrets.no_such_key"},
		"two keys":                              {[]string{"usage", "database_password", "sudo_password", "--vault-password-file", passwordFile}, "mmx secrets usage takes at most one key"},
		"another vault":                         {[]string{"usage", "--vault", vault, "--vault-password-file", passwordFile}, "flag provided but not defined: -vault"},
		"an inventory that is not there":        {[]string{"usage", "--inventory", "missing", "--vault-password-file", passwordFile}, "inventory missing"},
	} {
		t.Run(name, func(t *testing.T) {
			if stdout, stderr, code := secretsOutput(t, tc.args...); code != 4 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
		})
	}
	// A playbook that does not load is an error, not a shorter answer.
	os.WriteFile(broken, []byte("fleet_stages:\n  - id: broken\n    nodes: nowhere\n    node_tasks: [{show: hi}]\n"), 0644)
	if stdout, stderr, code := secretsOutput(t, "usage", "--vault-password-file", passwordFile); code != 4 || stdout != "" || !strings.Contains(stderr, "nowhere") {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	os.Remove(broken)

	// The staging inventory names a vault that does not exist: the command
	// needs no passphrase, the vault defines nothing, and the connection
	// secret of the other inventory is no use.
	stdout, stderr, code := secretsOutput(t, "usage", "--inventory", "staging", "--non-interactive")
	if code != 1 || stderr != "" || strings.Contains(stdout, "sudo_password") || !strings.Contains(stdout, "database_password       not defined\n") ||
		!strings.Contains(stdout, "api_token               environment variable SHOP_API_TOKEN\n") ||
		!strings.HasSuffix(stdout, "\nReferenced but not defined:   database_password (playbooks/deploy_platform.yml:8), session_cookie_secret (playbooks/site.yml:12)\nDefined but never referenced: (none)\n") {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	// The same from a directory below the project root. The places stay as
	// the loader names them; the vault file is named from that directory.
	t.Chdir(filepath.Join(root, "playbooks"))
	if again, _, _ := secretsOutput(t, "usage", "--inventory", "staging", "--non-interactive"); again != stdout {
		t.Fatal(again)
	}
	if one, _, code := secretsOutput(t, "usage", "sudo_password", "--vault-password-file", passwordFile); code != 0 || one != "sudo_password   vault ../secrets_vault.age\n  inventories/production.yml:4   node web01, sudo_password\n" {
		t.Fatalf("exit code %d\n%s", code, one)
	}
	if _, stderr, code := secretsOutput(t, "usage", "no_such_key", "--vault-password-file", passwordFile); code != 4 || !strings.Contains(stderr, "no secret key no_such_key: the vault ../secrets_vault.age and secrets_from_env do not define it") {
		t.Fatal(code, stderr)
	}
	// Outside a project there is nothing to read.
	t.Chdir(t.TempDir())
	if _, stderr, code := secretsOutput(t, "usage"); code != 4 || !strings.Contains(stderr, "no mmx_project_settings.yml found") {
		t.Fatal(code, stderr)
	}
}

// With the selected inventory, a key that only fleet stages which select no
// nodes read is listed with its uses and marked as not needed, and is not
// referenced but not defined: a team that leaves an optional group empty
// needs no placeholder secret for it. The fleet stages inside a
// for_each_node loop that selects no nodes count too. A key that a fleet
// stage which selects nodes also reads, or one whose selection resolves
// during the run, is needed. With an inventory whose group has a node, every
// such key is needed.
func TestSecretsUsageNeedsNoKeyThatOnlyFleetStagesWhichSelectNoNodesRead(t *testing.T) {
	root, passwordFile := secretsProject(t)
	for name, text := range map[string]string{
		"inventories/production.yml": "nodes:\n  web01: {address: 192.0.2.10}\ngroups:\n  staging_server:\n    nodes: []\n",
		"inventories/staging.yml":    "vault_file: staging_vault.age\nnodes:\n  web01: {address: 192.0.2.20}\ngroups:\n  staging_server:\n    nodes: [web01]\n",
		"playbooks/site.yml": `fleet_stages:
  - id: configure_web
    nodes: web01
    outputs:
      none: []
    node_tasks:
      - shell:
          script: deploy
          env:
            SHARED_TOKEN: "{{ .secrets.shared_token }}"

  - id: configure_staging_server
    nodes: staging_server
    skip_when_no_nodes_are_selected: true
    node_tasks:
      - shell:
          script: deploy
          env:
            DATABASE_PASSWORD: "{{ .secrets.staging_database_password }}"
            SHARED_TOKEN: "{{ .secrets.shared_token }}"

  - id: staging_rollout
    for_each_node: staging_server
    as: server
    skip_when_no_nodes_are_selected: true
    fleet_stages:
      - id: update
        nodes: "{{ .server }}"
        node_tasks:
          - shell:
              script: update
              env:
                ROLLOUT_TOKEN: "{{ .secrets.staging_rollout_token }}"

  - id: configure_resolved
    nodes: "{{ .outputs.configure_web.web01.none }}"
    skip_when_no_nodes_are_selected: true
    node_tasks:
      - shell:
          script: deploy
          env:
            RESOLVED_TOKEN: "{{ .secrets.resolved_token }}"
`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase, "shared_token: sentinel-token-value\n")
	stdout, stderr, code := secretsOutput(t, "usage", "--vault-password-file", passwordFile)
	want := `resolved_token              not defined
  playbooks/site.yml:42   shell env RESOLVED_TOKEN

shared_token                vault secrets_vault.age
  playbooks/site.yml:10   shell env SHARED_TOKEN
  playbooks/site.yml:20   shell env SHARED_TOKEN

staging_database_password   not defined
  (secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)
  playbooks/site.yml:19   shell env DATABASE_PASSWORD

staging_rollout_token       not defined
  (secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)
  playbooks/site.yml:33   shell env ROLLOUT_TOKEN

Referenced but not defined:   resolved_token (playbooks/site.yml:42)
Defined but never referenced: (none)
`
	if code != 1 || stderr != "" || stdout != want {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	// One key that is not needed: the exit code is 0.
	if stdout, _, code = secretsOutput(t, "usage", "staging_database_password", "--vault-password-file", passwordFile); code != 0 ||
		stdout != "staging_database_password   not defined\n  (secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)\n  playbooks/site.yml:19   shell env DATABASE_PASSWORD\n" {
		t.Fatalf("exit code %d\n%s", code, stdout)
	}
	// JSON adds the mark to such a key and leaves it off every other key.
	var answer struct {
		Keys []struct {
			Key                                      string
			OnlyReadByFleetStagesLeftOutBeforeTheRun *bool `json:"only_read_by_fleet_stages_left_out_before_the_run"`
		}
		ReferencedButNotDefined []string `json:"referenced_but_not_defined"`
	}
	stdout, _, code = secretsOutput(t, "usage", "--json", "--vault-password-file", passwordFile)
	if code != 1 || json.Unmarshal([]byte(stdout), &answer) != nil || len(answer.Keys) != 4 || !reflect.DeepEqual(answer.ReferencedButNotDefined, []string{"resolved_token"}) {
		t.Fatalf("exit code %d\n%s", code, stdout)
	}
	for _, key := range answer.Keys {
		notNeeded := key.Key == "staging_database_password" || key.Key == "staging_rollout_token"
		if marked := key.OnlyReadByFleetStagesLeftOutBeforeTheRun; notNeeded != (marked != nil && *marked) || !notNeeded && marked != nil {
			t.Fatal(key.Key, marked)
		}
	}
	// In the staging inventory the group has a node: every key is needed.
	stdout, stderr, code = secretsOutput(t, "usage", "--inventory", "staging", "--non-interactive")
	if code != 1 || stderr != "" || strings.Contains(stdout, "not needed") ||
		!strings.HasSuffix(stdout, "\nReferenced but not defined:   resolved_token (playbooks/site.yml:42), shared_token (playbooks/site.yml:10), staging_database_password (playbooks/site.yml:19), staging_rollout_token (playbooks/site.yml:33)\nDefined but never referenced: (none)\n") {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}
