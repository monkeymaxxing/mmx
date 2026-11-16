package config

import (
	"bytes"
	"embed"
	"mmx/internal/modules"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fuzz tests below feed untrusted bytes to the readers of the four YAML
// files a project holds. A file that reads without error must keep the rules
// its reader promises, and a second YAML document after it must be refused:
// MMX reads exactly one document from each file.

// secondYAMLDocument starts a second document after any complete first one.
const secondYAMLDocument = "\n---\n{}\n"

// fuzzSeeds holds copies of the nginx-role example's files. They are built
// into the test program, because the Go tests also run inside Linux guests
// that have only the test program and no source tree.
//
//go:embed testdata/fuzz_seeds
var fuzzSeeds embed.FS

// readSeedFile reads a seed for a fuzz test.
func readSeedFile(f *testing.F, pathInExample string) []byte {
	f.Helper()
	contents, err := fuzzSeeds.ReadFile("testdata/fuzz_seeds/" + pathInExample)
	if err != nil {
		f.Fatal(err)
	}
	return contents
}

func FuzzProjectSettingsFile(f *testing.F) {
	f.Add(readSeedFile(f, "mmx_project_settings.yml"))
	f.Add([]byte("project_name: test\ndefault_inventory: example\nrun_history_retention: 7d\nmax_nodes_at_once: 5\nsecrets_from_env: {login: MMX_TEST_LOGIN}\nvars: {greeting: hello}\n"))
	f.Add([]byte(""))
	f.Add([]byte("project_name: [test\ndefault_inventory: example\n"))
	f.Add([]byte("project_name: test\nproject_name: other\ndefault_inventory: example\n"))
	f.Add([]byte("project_name: test\ndefault_inventory: example\nfleet_stages: []\n"))
	f.Add([]byte("project_name: test\ndefault_inventory: example\nmax_nodes_at_once: many\n"))
	f.Fuzz(func(t *testing.T, settingsFileContents []byte) {
		settingsFile := filepath.Join(t.TempDir(), "mmx_project_settings.yml")
		if err := os.WriteFile(settingsFile, settingsFileContents, 0600); err != nil {
			t.Fatal(err)
		}
		project, err := ReadSettings(settingsFile)
		if err != nil {
			return
		}
		if !modules.Name.MatchString(project.Project) {
			t.Fatalf("accepted project_name %q", project.Project)
		}
		if !modules.Name.MatchString(project.Inventory) {
			t.Fatalf("accepted default_inventory %q", project.Inventory)
		}
		if project.MaxNodesAtOnce < 1 {
			t.Fatalf("accepted max_nodes_at_once %d", project.MaxNodesAtOnce)
		}
		if _, err := HistoryRetention(project.HistoryRetention); err != nil {
			t.Fatalf("accepted run_history_retention %q: %v", project.HistoryRetention, err)
		}
		if err := os.WriteFile(settingsFile, append(bytes.Clone(settingsFileContents), secondYAMLDocument...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSettings(settingsFile); err == nil {
			t.Fatal("accepted project settings followed by a second YAML document")
		}
	})
}

func FuzzPlaybookFile(f *testing.F) {
	f.Add(readSeedFile(f, "playbooks/web_server.yml"))
	f.Add([]byte(`fleet_stages:
  - id: setup
    nodes: all
    node_tasks:
      - show: ready
      - task_block:
          id: motd
          changes:
            - file: {permissions_mode: '0644', path: /etc/motd, content: "hello\n"}
          verify:
            - shell: {script: test -f /etc/motd, changed_when: false}
command_line_vars:
  release: {type: string, default: stable}
`))
	f.Add([]byte(""))
	f.Add([]byte("fleet_stages: [{id: setup, nodes: all\n"))
	f.Add([]byte("fleet_stages: []\nfleet_stages: []\n"))
	f.Add([]byte("fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: a}], speed: fast}]\n"))
	f.Add([]byte("fleet_stages: [{id: 5, nodes: all, node_tasks: [{show: a}]}]\n"))
	f.Fuzz(func(t *testing.T, playbookFileContents []byte) {
		// Only Decode: a full load reads the files that a module's inputs name.
		if err := Decode("playbooks/example.yml", playbookFileContents, &Playbook{}); err != nil {
			return
		}
		if err := Decode("playbooks/example.yml", append(bytes.Clone(playbookFileContents), secondYAMLDocument...), &Playbook{}); err == nil {
			t.Fatal("accepted a playbook followed by a second YAML document")
		}
	})
}

func FuzzInventoryFile(f *testing.F) {
	f.Add(readSeedFile(f, "inventories/example.yml"))
	f.Add([]byte(`nodes:
  web1: {address: 192.0.2.10, ssh_user: deploy, ssh_port: 2222, https_address: 'https://web1.example.com:7443/', ssh_identity_file: keys/deploy}
  web2: {address: web2.example.com, connection: ssh_only, mmx_runner_download_url: 'https://mirror.example.com/mmx/'}
groups:
  web: {nodes: [web2, web1], vars: {tier: front}}
vars: {region: north}
`))
	f.Add([]byte(""))
	f.Add([]byte("nodes: {web: {address: example.com}\n"))
	f.Add([]byte("nodes:\n  web: {address: a.example.com}\n  web: {address: b.example.com}\n"))
	f.Add([]byte("nodes: {web: {address: example.com, hostname: example.com}}\n"))
	f.Add([]byte("nodes: {web: {address: example.com, ssh_port: '22'}}\n"))
	f.Fuzz(func(t *testing.T, inventoryFileContents []byte) {
		projectDirectory := t.TempDir()
		// Official extra modules are looked up in the release directory; keep
		// that lookup inside this test's directory.
		t.Setenv("MMX_RUNNER_DIR", projectDirectory)
		settingsFile := filepath.Join(projectDirectory, "mmx_project_settings.yml")
		if err := os.WriteFile(settingsFile, []byte("project_name: test\ndefault_inventory: example\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(projectDirectory, "inventories"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectDirectory, "inventories", "example.yml"), inventoryFileContents, 0600); err != nil {
			t.Fatal(err)
		}
		_, inventory, err := Load(settingsFile, "", "example", nil)
		if err != nil {
			return
		}
		if len(inventory.Nodes) == 0 {
			t.Fatal("accepted an inventory without nodes")
		}
		for nodeName, node := range inventory.Nodes {
			if !modules.Name.MatchString(nodeName) {
				t.Fatalf("accepted node name %q", nodeName)
			}
			// ssh reads an address that starts with - as an option.
			if node.Address == "" || strings.HasPrefix(node.Address, "-") || strings.ContainsAny(node.Address, " \t\r\n\x00") {
				t.Fatalf("node %s: accepted address %q", nodeName, node.Address)
			}
			if node.Port < 0 || node.Port > 65535 {
				t.Fatalf("node %s: accepted ssh_port %d", nodeName, node.Port)
			}
			if node.Transport != "https_with_ssh_fallback" && node.Transport != "ssh_only" {
				t.Fatalf("node %s: loaded connection %q", nodeName, node.Transport)
			}
			for field, path := range map[string]string{"ssh_config": node.SSHConfig, "ssh_known_hosts": node.KnownHosts, "ssh_identity_file": node.IdentityFile} {
				if path != "" && !filepath.IsAbs(path) {
					t.Fatalf("node %s: loaded %s %q, which is not an absolute path", nodeName, field, path)
				}
			}
			if node.Endpoint != "" {
				if httpsAddress, err := url.Parse(node.Endpoint); err != nil || httpsAddress.Scheme != "https" || httpsAddress.Hostname() == "" || httpsAddress.User != nil {
					t.Fatalf("node %s: accepted https_address %q", nodeName, node.Endpoint)
				}
			}
			if node.RunnerURL != "" {
				if downloadURL, err := url.Parse(node.RunnerURL); err != nil || (downloadURL.Scheme != "http" && downloadURL.Scheme != "https") || downloadURL.User != nil {
					t.Fatalf("node %s: accepted mmx_runner_download_url %q", nodeName, node.RunnerURL)
				}
			}
		}
		for groupName, group := range inventory.Groups {
			if _, isNode := inventory.Nodes[groupName]; isNode || groupName == "all" || !modules.Name.MatchString(groupName) {
				t.Fatalf("accepted group name %q", groupName)
			}
			for i, member := range group.Nodes {
				if _, found := inventory.Nodes[member]; !found {
					t.Fatalf("group %s: accepted unknown node %q", groupName, member)
				}
				// Membership lookups rely on sorted members without repeats.
				if i > 0 && group.Nodes[i-1] >= member {
					t.Fatalf("group %s: loaded members %q, which are not sorted without repeats", groupName, group.Nodes)
				}
			}
		}
		if err := Decode("inventories/example.yml", append(bytes.Clone(inventoryFileContents), secondYAMLDocument...), &Inventory{}); err == nil {
			t.Fatal("accepted an inventory followed by a second YAML document")
		}
	})
}

func FuzzRoleFile(f *testing.F) {
	f.Add(readSeedFile(f, "roles/web_server/main.yml"))
	f.Add([]byte(`inputs:
  name: {type: string, required: true}
  count: {type: integer, default: 3}
  token: {type: string, secret: true}
  settings: {type: mapping, default: {}}
node_tasks:
  - show: "{{ .inputs.name }}"
`))
	f.Add([]byte(""))
	f.Add([]byte("node_tasks: [{show: a}\n"))
	f.Add([]byte("inputs: {}\ninputs: {}\nnode_tasks: [{show: a}]\n"))
	f.Add([]byte("description: a role\nnode_tasks: [{show: a}]\n"))
	f.Add([]byte("inputs: {port: {type: integer, required: maybe}}\nnode_tasks: [{show: a}]\n"))
	f.Fuzz(func(t *testing.T, roleFileContents []byte) {
		// Only Decode: expanding a role call reads the role's files.
		if err := Decode("roles/example/main.yml", roleFileContents, &Role{}); err != nil {
			return
		}
		if err := Decode("roles/example/main.yml", append(bytes.Clone(roleFileContents), secondYAMLDocument...), &Role{}); err == nil {
			t.Fatal("accepted a role followed by a second YAML document")
		}
	})
}
