package config

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestRepositoryAndUpgradeConfiguration(t *testing.T) {
	digest := strings.Repeat("a", 64)
	load := func(task string) error {
		p := fixture(t, "fleet_stages: [{id: sources, nodes: all, node_tasks: [{"+task+"}]}]\n")
		write(t, filepath.Join(filepath.Dir(p), "vendor.asc"), "-----BEGIN PGP PUBLIC KEY BLOCK-----\n")
		_, _, err := Load(p, "example", "", nil)
		return err
	}
	vendor := "name: vendor, uris: [https://example.com/debian], suites: [trixie], components: [main]"
	for _, task := range []string{
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc}",
		"apt_repository: {" + vendor + ", architectures: [amd64, arm64], signing_key_url: https://example.com/key, signing_key_sha256: " + digest + "}",
		"apt_repository: {name: local, uris: ['file:/srv/repo'], suites: ['./'], signing_key_source: vendor.asc}",
		`apt_repository: {name: vendor, uris: [https://example.com/debian], suites: ['{{ .node.os.version_codename }}'], components: [main], signing_key_source: vendor.asc}`,
		"apt_repository: {" + vendor + ", signing_key_url: https://example.com/key, dangerously_skip_checksum: true}",
		"apt_repository: {" + vendor + ", signing_key_url: https://example.com/key, signing_key_sha256: " + digest + ", dangerously_skip_checksum: false}",
		"apt_repository: {name: vendor, state: absent}",
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc, username: customer, password: '{{ .secrets.vendor_password }}'}",
		"apt_package: {upgrade_all_packages_within_their_version_rules: true}",
		"dnf_package: {upgrade_all_packages_within_their_version_rules: true, repositories_enabled_only_for_this_task: [x]}",
		"dnf_package: {packages: [{name: nginx, version: newest_when_first_installed}]}",
		"dnf_package: {packages: [{name: postgresql-server, version: newest_when_first_installed}], enable_dnf_module_stream: 'postgresql:16'}",
		"dnf_package: {packages: [{name: postgresql-server, version: newest_when_first_installed}], enable_dnf_module_stream: 'postgresql:16'}}, {dnf_package: {packages: [{name: postgresql-contrib, version: newest_when_first_installed}], enable_dnf_module_stream: 'postgresql:16'}",
		"dnf_repository: {name: docker-ce-stable, description: Docker CE Stable, baseurl: ['https://download.docker.com/linux/rhel/$releasever/$basearch/stable'], signing_key_source: vendor.asc}",
		"dnf_repository: {name: epel, description: EPEL, metalink: 'https://mirrors.fedoraproject.org/metalink?repo=epel-9&arch=$basearch', enabled: false, priority: 50, includepkgs: [htop], exclude: ['kernel*'], signing_key_source: vendor.asc, username: u, password: '{{ .secrets.p }}'}",
		"dnf_repository: {existing_repository: crb, enabled: true}",
		"dnf_repository: {name: epel, state: absent}",
		"dnf_package: {packages: [{name: docker-ce, version: '3:27.3.*', allow_downgrade: true}], repositories_enabled_only_for_this_task: [docker-ce-stable]}",
		"dnf_package: {packages: [{name: kernel-ml, version: newest_on_every_run}], repositories_enabled_only_for_this_task: ['elrepo-kernel']}",
		"dnf_package: {packages: [{name: vendor, version: '1', package_file_source: vendor.asc}]}",
		"dnf_package: {packages: [{name: vendor}], state: absent, also_remove_packages_only_it_needed: true}",
		"dnf_package: {upgrade_all_packages_within_their_version_rules: true, also_remove_packages_no_longer_needed: true}",
		"dnf_package: {upgrade_all_packages_within_their_version_rules: true, only_security_updates: true}",
		"dnf_package: {upgrade_all_packages_within_their_version_rules: true, only_security_updates: false, also_remove_packages_no_longer_needed: true}",
		"apt_package: {packages: [{name: docker-ce, version: '5:27.*'}]}}, {when: false, apt_package: {packages: [{name: docker-ce, version: newest_when_first_installed}]}",
		"apt_package: {upgrade_all_packages_within_their_version_rules: true, also_remove_packages_no_longer_needed: true}",
		"apt_package: {packages: [{name: nginx, version: newest_on_every_run}, {name: '{{ .node.name }}', version: newest_on_every_run}]}",
		"apt_package: {packages: [{name: vendor, version: '1', allow_downgrade: true, package_file_source: vendor.asc}]}",
		"apt_package: {packages: [{name: vendor, version: '1', package_file_url: https://example.com/vendor.deb, package_file_sha256: " + digest + "}]}",
		"apt_package: {packages: [{name: vendor}], state: absent, also_delete_its_config_files: true, also_remove_packages_no_longer_needed: true}",
		"apt_package: {packages: [{name: vendor, version: '2.*', allow_downgrade: true}], allow_package_to_start_its_services: true}",
	} {
		if err := load(task); err != nil {
			t.Fatal(task, err)
		}
	}
	for task, want := range map[string]string{
		"apt_repository: {name: vendor, state: absent, uris: [https://example.com]}":                                                                                                                                                                     "absent apt_repository accepts only name and state",
		"apt_repository: {name: vendor, state: absent, signing_key_source: vendor.asc}":                                                                                                                                                                  "absent apt_repository accepts only name and state",
		"apt_repository: {name: vendor, suites: [trixie], components: [main], signing_key_source: x}":                                                                                                                                                    "apt_repository requires uris and suites",
		"apt_repository: {" + vendor + "}":                                                                                                                                                                                                               "apt_repository requires one key",
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc, signing_key_url: https://example.com/k}":                                                                                                                                       "apt_repository requires one key",
		"apt_repository: {" + vendor + ", signing_key_url: http://example.com/key, signing_key_sha256: " + digest + "}":                                                                                                                                  "(fleet_stages[0].node_tasks[0].apt_repository.signing_key_url): apt_repository.signing_key_url must be an HTTPS URL",
		"apt_repository: {" + vendor + ", signing_key_url: https://example.com/key}":                                                                                                                                                                     "apt_repository.signing_key_sha256 is required",
		"apt_repository: {" + vendor + ", signing_key_url: https://example.com/key, signing_key_sha256: abc}":                                                                                                                                            "apt_repository.signing_key_sha256 requires 64 hexadecimal digits",
		"apt_repository: {" + vendor + ", signing_key_url: https://example.com/key, signing_key_sha256: " + digest + ", dangerously_skip_checksum: true}":                                                                                                "takes signing_key_sha256 or dangerously_skip_checksum: true, not both",
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc, dangerously_skip_checksum: true}":                                                                                                                                              "apt_repository requires one key",
		"apt_repository: {" + vendor + ", signing_key_source: missing.asc}":                                                                                                                                                                              "apt_repository.signing_key_source",
		"apt_repository: {" + vendor + ", signing_key_source: '{{ .vars.key }}'}":                                                                                                                                                                        "reference .vars.key is not declared for node ",
		"apt_repository: {name: vendor, uris: [https://example.com], suites: [trixie], signing_key_source: vendor.asc}":                                                                                                                                  "(fleet_stages[0].node_tasks[0].apt_repository.suites[0]): apt_repository.components is required unless the suite ends in /",
		"apt_repository: {name: vendor, uris: [https://example.com], suites: ['./'], components: [main], signing_key_source: vendor.asc}":                                                                                                                "(fleet_stages[0].node_tasks[0].apt_repository.suites[0]): a suite ending in / is a flat repository and takes no components",
		"apt_repository: {name: ../vendor, uris: [https://example.com], suites: ['./'], signing_key_source: vendor.asc}":                                                                                                                                 "(fleet_stages[0].node_tasks[0].apt_repository.name): apt_repository name must use",
		"apt_repository: {name: vendor, uris: ['https://a.example https://b.example'], suites: ['./'], signing_key_source: vendor.asc}":                                                                                                                  "(fleet_stages[0].node_tasks[0].apt_repository.uris[0]): apt_repository values must be single words",
		"apt_repository: {name: vendor, uris: [https://example.com], suites: ['./'], architectures: [''], signing_key_source: vendor.asc}":                                                                                                               "(fleet_stages[0].node_tasks[0].apt_repository.architectures[0])",
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc, username: customer}":                                                                                                                                                           "apt_repository.username and apt_repository.password come together",
		"apt_repository: {" + vendor + ", signing_key_source: vendor.asc, username: customer, password: p4ss}":                                                                                                                                           "apt_repository.password must be a whole-value secret reference",
		"apt_repository: {name: vendor, state: absent, username: customer}":                                                                                                                                                                              "absent apt_repository accepts only name and state",
		"apt_repository: {name: vendor, uris: ['https://u:p@example.com'], suites: ['./'], signing_key_source: vendor.asc}":                                                                                                                              "apt_repository.uris must not contain a login",
		"dnf_repository: {name: r, baseurl: [https://e.com], signing_key_source: vendor.asc}":                                                                                                                                                            "dnf_repository requires description",
		"dnf_repository: {name: r, description: R, signing_key_source: vendor.asc}":                                                                                                                                                                      "dnf_repository requires exactly one of baseurl, metalink, or mirrorlist",
		"dnf_repository: {name: r, description: R, baseurl: [https://e.com], metalink: https://m.com, signing_key_source: vendor.asc}":                                                                                                                   "dnf_repository requires exactly one of baseurl, metalink, or mirrorlist",
		"dnf_repository: {name: r, description: R, baseurl: [https://e.com]}":                                                                                                                                                                            "dnf_repository requires one key",
		"dnf_repository: {name: r, description: R, baseurl: [https://e.com], priority: 0, signing_key_source: vendor.asc}":                                                                                                                               "dnf_repository.priority must be from 1 to 99",
		"dnf_repository: {name: r, description: R, baseurl: ['https://u:p@e.com'], signing_key_source: vendor.asc}":                                                                                                                                      "must not contain a login",
		"dnf_repository: {name: r, description: R, baseurl: [https://e.com], signing_key_source: vendor.asc, password: x, username: u}":                                                                                                                  "dnf_repository.password must be a whole-value secret reference",
		"dnf_repository: {name: r, description: R, baseurl: [https://e.com, 'http://mirror.internal/r'], signing_key_source: vendor.asc, username: u, password: '{{ .secrets.p }}'}":                                                                     "dnf_repository.baseurl[1] http://mirror.internal/r does not start with https://",
		"dnf_repository: {name: r, description: R, mirrorlist: 'http://mirror.internal/list', signing_key_source: vendor.asc, username: u, password: '{{ .secrets.p }}'}":                                                                                "dnf_repository.mirrorlist http://mirror.internal/list does not start with https://",
		"dnf_repository: {existing_repository: crb}":                                                                                                                                                                                                     "dnf_repository with existing_repository requires enabled",
		"dnf_repository: {existing_repository: crb, enabled: true, description: x}":                                                                                                                                                                      "dnf_repository with existing_repository accepts only enabled",
		"dnf_repository: {name: r, state: absent, enabled: true}":                                                                                                                                                                                        "absent dnf_repository accepts only name and state",
		"dnf_repository: {name: ../r, description: R, baseurl: [https://e.com], signing_key_source: vendor.asc}":                                                                                                                                         "dnf_repository name must use letters",
		"dnf_repository: {name: r, description: R, uris: [https://e.com], signing_key_source: vendor.asc}":                                                                                                                                               "unknown field",
		"apt_package: {packages: [{name: docker-ce, version: '5:27.*'}]}}, {apt_package: {packages: [{name: docker-ce, version: newest_on_every_run}]}":                                                                                                  "docker-ce has a version rule",
		"apt_package: {packages: [{name: nginx, version: '1'}, {name: nginx, version: '2'}]}":                                                                                                                                                            "nginx is listed twice in packages",
		"dnf_package: {packages: [{name: nginx, version: '1', package_file_source: vendor.asc}], enable_dnf_module_stream: 'nginx:1'}":                                                                                                                   "enable_dnf_module_stream is not allowed with package files",
		"apt_package: {packages: [{name: nginx, version: newest_when_first_installed, package_file_source: vendor.asc}]}":                                                                                                                                "a package file entry requires the exact version inside the file",
		"apt_package: {packages: [{name: nginx}]}":                                                                                                                                                                                                       "packages[0].version is required",
		"dnf_package: {packages: [{version: '1'}]}":                                                                                                                                                                                                      "requires each package's name",
		"apt_package: {packages: [{name: nginx, version: '1'}], state: absent}":                                                                                                                                                                          "accepts only name on each package entry",
		"apt_package: {packages: [{name: nginx, version: newest_when_first_installed, allow_downgrade: true}]}":                                                                                                                                          "allow_downgrade requires a version",
		"apt_package: {packages: [{name: nginx, version: '1', package_file_source: missing.deb}]}":                                                                                                                                                       "packages[0].package_file_source",
		"apt_package: {packages: [{name: nginx, version: '1.*', package_file_source: vendor.asc}]}":                                                                                                                                                      "requires the exact version inside the file",
		"apt_package: {packages: [{name: nginx, version: '1', package_file_url: https://example.com/nginx.deb}]}":                                                                                                                                        "package_file_sha256 is required",
		"dnf_package: {packages: [{name: nginx, version: newest_when_first_installed}], only_security_updates: false}":                                                                                                                                   "only_security_updates requires upgrade_all_packages_within_their_version_rules: true",
		"dnf_package: {upgrade_all_packages_within_their_version_rules: true, packages: [{name: nginx, version: '1'}]}":                                                                                                                                  "takes only",
		"dnf_package: {packages: [{name: nginx}], state: absent, repositories_enabled_only_for_this_task: [x]}":                                                                                                                                          "repositories_enabled_only_for_this_task requires state: present or upgrade_all_packages_within_their_version_rules: true, and at least one repository ID",
		"dnf_package: {packages: [{name: nginx, version: newest_when_first_installed}], repositories_enabled_only_for_this_task: ['bad id']}":                                                                                                            "a dnf repository id uses letters",
		"apt_package: {packages: [{name: nginx, version: newest_when_first_installed}], also_remove_packages_no_longer_needed: true}":                                                                                                                    "also_remove_packages_no_longer_needed requires state: absent or upgrade_all_packages_within_their_version_rules: true",
		"apt_package: {packages: [{name: nginx, version: newest_when_first_installed}], also_remove_packages_that_depend_on_these: true}":                                                                                                                "also_remove_packages_that_depend_on_these requires state: absent",
		"dnf_package: {packages: [{name: a, version: newest_when_first_installed}], enable_dnf_module_stream: 'postgresql:16'}}, {dnf_package: {packages: [{name: b, version: newest_when_first_installed}], enable_dnf_module_stream: 'postgresql:15'}": "this fleet stage enables both postgresql:16 and postgresql:15",
	} {
		if err := load(task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

// Suites are checked as the node resolves them, with the rule the written
// components select.
func TestResolvedSuitesGetTheirRule(t *testing.T) {
	flat := Task{Module: "apt_repository", ModuleInputs: &modules.Repository{Manager: "apt", Name: "local", URIs: []string{"file:/srv/repo"}, Suites: []string{"{{ .vars.suite }}"}}}
	components := Task{Module: "apt_repository", ModuleInputs: &modules.Repository{Manager: "apt", Name: "vendor", URIs: []string{"https://example.com"}, Suites: []string{"{{ .vars.suite }}"}, Components: []string{"main"}}}
	for _, tc := range []struct {
		task         Task
		suite, error string
	}{
		{flat, "./", ""}, {flat, "trixie", "apt_repository.components is required unless the suite ends in /"},
		{components, "trixie", ""}, {components, "./", "a suite ending in / is a flat repository"},
		{components, "trixie updates", "apt_repository values must be single words"},
	} {
		resolved, err := ResolveArguments(tc.task, value.Scope{"vars": map[string]any{"suite": tc.suite}})
		if tc.error == "" && (err != nil || InputsOf[modules.Repository](resolved).Suites[0] != tc.suite) {
			t.Error(tc.suite, err)
		}
		if tc.error != "" && (err == nil || !strings.Contains(err.Error(), "apt_repository.suites[0]: "+tc.error)) {
			t.Error(tc.suite, err)
		}
		if InputsOf[modules.Repository](tc.task).Suites[0] != "{{ .vars.suite }}" {
			t.Fatal("resolution changed the written task")
		}
	}
}

// A key from a local file or a pinned URL is frozen like a file source: the
// URL is fetched once, and a saved run restores both without the sources. A
// package file is frozen the same way.
func TestRepositoryKeysAreFrozenAssets(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "remote key") }))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	remote, _ := assets.Hash(context.Background(), strings.NewReader("remote key"))
	local := filepath.Join(t.TempDir(), "vendor.asc")
	write(t, local, "local key")
	packageFile := filepath.Join(t.TempDir(), "tool.deb")
	write(t, packageFile, "package file")
	secondPackageFile := filepath.Join(t.TempDir(), "helper.deb")
	write(t, secondPackageFile, "second package file")
	repository := func(name string) Task {
		return Task{Module: "apt_repository", ModuleInputs: &modules.Repository{Manager: "apt", Name: name, URIs: []string{"https://example.com"}, Suites: []string{"./"}, SigningKeyURL: server.URL, SigningKeySHA256: remote.SHA256}}
	}
	p := Project{FleetStages: []FleetStage{{NodeTasks: []Task{
		repository("one"), {TaskBlock: &TaskBlock{ID: "repository", Changes: []Task{repository("two")}}},
		{Module: "apt_repository", ModuleInputs: &modules.Repository{Manager: "apt", Name: "local", URIs: []string{"https://example.com"}, Suites: []string{"./"}, SigningKeySource: "vendor.asc", LocalPath: local}},
		{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "tool", Version: "1", PackageFileSource: "tool.deb", LocalPath: packageFile}, {Name: "helper", Version: "2", PackageFileSource: "helper.deb", LocalPath: secondPackageFile}}}},
	}}}}
	dir := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, dir, nil); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatal("fetched more than once", hits.Load())
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	var saved Project
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&saved, dir); err != nil {
		t.Fatal(err)
	}
	refs, err := RequiredAssets(saved.FleetStages[0].NodeTasks, nil)
	if err != nil || len(refs) != 4 {
		t.Fatal(refs, err)
	}
	for i, want := range []string{"remote key", "local key", "package file", "second package file"} {
		if data, err := os.ReadFile(refs[i].Path); err != nil || string(data) != want {
			t.Fatal(i, "example", string(data), err)
		}
	}
	InputsOf[modules.Repository](saved.FleetStages[0].NodeTasks[0]).SigningKeySHA256 = strings.Repeat("0", 64)
	if _, err := RequiredAssets(saved.FleetStages[0].NodeTasks, nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatal("accepted a different pin at admission", err)
	}
}
