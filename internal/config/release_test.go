package config

import (
	"context"
	"fmt"
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
)

func TestArchiveConfiguration(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pinned := fmt.Sprintf(`url: https://example.com/app.tar.gz, sha256: %q, path: /srv/app/releases/1.0`, digest)
	load := func(input string) error {
		path := fixture(t, "vars: {release: v1.0}\nfleet_stages: [{id: deploy, nodes: all, node_tasks: [{unpack_archive: {"+input+"}}]}]")
		write(t, filepath.Join(filepath.Dir(path), "files/app.zip"), "PK")
		_, _, err := Load(path, "example", "", nil)
		return err
	}
	for _, good := range []string{
		pinned + ", owner: app, group: app, unpack_without_wrapping_folder: true",
		"source: files/app.zip, path: '/srv/app/releases/{{ .vars.release }}'",
		"path: /srv/app/releases/0.9, state: absent, also_delete_everything_inside: true",
		"url: https://example.com/app.tar.gz, dangerously_skip_checksum: true, path: /srv/app/releases/1.0",
		// A web address and its SHA-256 may read values known before the run.
		fmt.Sprintf(`url: 'https://example.com/{{ .vars.release }}.tar.gz', sha256: %q, path: /srv/app/1.0`, digest),
	} {
		if err := load(good); err != nil {
			t.Fatal(good, err)
		}
	}
	for _, bad := range []string{
		"path: /srv/app/releases/1.0",
		pinned + ", source: files/app.zip",
		pinned + ", dangerously_skip_checksum: true",
		"url: https://example.com/app.tar.gz, path: /srv/app/releases/1.0",
		"source: files/app.zip, dangerously_skip_checksum: true, path: /srv/app/1.0",
		"path: /srv/app/releases/0.9, state: absent, dangerously_skip_checksum: true",
		"source: files/app.zip, sha256: " + digest + ", path: /srv/app/1.0",
		"source: files/missing.zip, path: /srv/app/1.0",
		"source: 'files/{{ .vars.name }}', path: /srv/app/1.0",
		strings.Replace(pinned, "https:", "http:", 1),
		// A web address may not read what only a run knows.
		fmt.Sprintf(`url: 'https://example.com/{{ .node.arch }}.tar.gz', sha256: %q, path: /srv/app/1.0`, digest),
		strings.Replace(pinned, digest, "xyz", 1),
		strings.Replace(pinned, "/srv/app/releases/1.0", "releases/1.0", 1),
		pinned + ", unpack_without_wrapping_folder: 1",
		pinned + ", owner: '{{ .vars.missing }}'",
		"path: /srv/app/releases/0.9, state: absent, owner: app",
		pinned + ", state: latest",
	} {
		if err := load(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestGitConfiguration(t *testing.T) {
	load := func(input string) error {
		_, _, err := Load(fixture(t, "vars: {release: v1.0}\nsecrets_from_env: {token: MMX_TEST_TOKEN}\nfleet_stages: [{id: deploy, nodes: all, node_tasks: [{git: {"+input+"}}]}]"), "example", "", nil)
		return err
	}
	known := "ssh_known_hosts_lines: 'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl'"
	for _, good := range []string{
		"repo: https://github.com/example/app.git, check_out_tag: v1.4.2, path: /srv/app/releases/v1.4.2, run_as_user_account: app",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app/main, username: deploy, password: '{{ .secrets.token }}'",
		"repo: git@github.com:example/app.git, follow_branch: release/2024-01, path: /srv/app/current, ssh_private_key: '{{ .secrets.token }}', " + known,
		"repo: ssh://git@github.com/example/app.git, check_out_commit: 0123456789abcdef0123456789abcdef01234567, path: /srv/app/c, " + known,
		"repo: /srv/git/app.git, follow_branch: main, path: /srv/app/local, full_history: true",
		"repo: file:///srv/git/app.git, check_out_tag: '{{ .vars.release }}', path: '/srv/app/releases/{{ .vars.release }}'",
	} {
		if err := load(good); err != nil {
			t.Fatal(good, err)
		}
	}
	for _, bad := range []string{
		"follow_branch: main, path: /srv/app",
		"repo: https://github.com/example/app.git, path: /srv/app",
		"repo: https://github.com/example/app.git, follow_branch: main",
		"repo: https://user:token@github.com/example/app.git, follow_branch: main, path: /srv/app",
		"repo: https://token@github.com/example/app.git, follow_branch: main, path: /srv/app",
		"repo: git@github.com:example/app.git, follow_branch: main, path: /srv/app",
		"repo: '-oProxyCommand=touch /tmp/x', follow_branch: main, path: /srv/app",
		"repo: git@github.com:example/app.git, follow_branch: main, path: /srv/app, password: x, " + known,
		"repo: http://example.com/app.git, follow_branch: main, path: /srv/app, password: x",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app, ssh_private_key: x",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app, " + known,
		"repo: https://github.com/example/app.git, follow_branch: --upload-pack=x, path: /srv/app",
		"repo: https://github.com/example/app.git, follow_branch: 'a..b', path: /srv/app",
		"repo: https://github.com/example/app.git, follow_branch: 'main:refs/heads/x', path: /srv/app",
		"repo: https://github.com/example/app.git, follow_branch: main, path: relative",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app, run_as_user_account: 'no such!'",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app, password: '{{ .vars.missing }}'",
		"repo: https://github.com/example/app.git, follow_branch: main, path: /srv/app, submodules: true",
	} {
		if err := load(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

// An archive's URL is fetched once per run with every download of the same
// URL and digest, and its source is frozen like a file's; the node job
// carries both as ordinary assets.
func TestArchivesFreezeLikeFileSources(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "release") }))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	pin, _ := assets.Hash(context.Background(), strings.NewReader("release"))
	local := filepath.Join(t.TempDir(), "app.zip")
	write(t, local, "local archive")
	p := Project{FleetStages: []FleetStage{{NodeTasks: []Task{
		{Module: "file", ModuleInputs: &modules.File{Mode: "0644", URL: server.URL, SHA256: pin.SHA256, Path: "/srv/app.tar.gz"}},
		{TaskBlock: &TaskBlock{ID: "release", Changes: []Task{{Module: "unpack_archive", ModuleInputs: &modules.Archive{URL: server.URL, SHA256: pin.SHA256, Path: "/srv/app/1.0"}}}}},
		{Module: "unpack_archive", ModuleInputs: &modules.Archive{Source: "app.zip", LocalPath: local, Path: "/srv/app/local"}},
		{Module: "unpack_archive", ModuleInputs: &modules.Archive{Path: "/srv/app/0.9", State: "absent"}},
	}}}}
	if err := FreezeAssets(context.Background(), &p, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	nodeTasks := p.FleetStages[0].NodeTasks
	if hits.Load() != 1 || InputsOf[modules.Archive](nodeTasks[1].TaskBlock.Changes[0]).Asset.SHA256 != pin.SHA256 || InputsOf[modules.Archive](nodeTasks[3]).Asset != nil {
		t.Fatal(hits.Load(), nodeTasks)
	}
	saved, _ := os.ReadFile(InputsOf[modules.Archive](nodeTasks[2]).Asset.Path)
	if string(saved) != "local archive" {
		t.Fatal(string(saved))
	}
	if refs, err := RequiredAssets(nodeTasks, nil); err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
	InputsOf[modules.Archive](nodeTasks[1].TaskBlock.Changes[0]).SHA256 = strings.Repeat("0", 64)
	if err := FreezeAssets(context.Background(), &Project{FleetStages: []FleetStage{{NodeTasks: nodeTasks[1:2]}}}, t.TempDir(), nil); err == nil {
		t.Fatal("accepted an archive that does not match its pin")
	}
}

func TestGitSelectorConfiguration(t *testing.T) {
	for input, want := range map[string]string{
		"":                                       "exactly one of follow_branch, check_out_tag, or check_out_commit",
		"follow_branch: main, check_out_tag: v1": "exactly one of follow_branch, check_out_tag, or check_out_commit",
		"check_out_commit: abc123":               "full 40-character hexadecimal commit ID",
		"check_out_commit: v1":                   "full 40-character hexadecimal commit ID",
		"follow_branch: main":                    "",
		"check_out_tag: main":                    "",
		"check_out_commit: 0123456789abcdef0123456789abcdef01234567": "",
	} {
		task := "git: {repo: https://example.com/app.git, path: /srv/app"
		if input != "" {
			task += ", " + input
		}
		_, _, err := Load(fixture(t, "fleet_stages: [{id: deploy, nodes: all, node_tasks: [{"+task+"}}]}]"), "example", "", nil)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatal(input, err)
		}
	}
}
