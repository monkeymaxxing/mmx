package config

import (
	"encoding/json"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestDirectoryDeletionConfiguration(t *testing.T) {
	load := func(task string) (Project, error) {
		p, _, err := Load(fixture(t, "vars: {release: '1.4.2', bad: '..'}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n"), "example", "", nil)
		return p, err
	}
	for _, task := range []string{
		"directory: {path: /home/deploy/old-api, state: absent}",
		"directory: {path: /home/deploy/old-api, state: absent, also_delete_everything_inside: true}",
		"directory: {path: /srv/app/releases, delete_everything_inside_except: ['{{ .vars.release }}', '1.4.1']}",
		"directory: {path: /srv/app/releases, delete_everything_inside_except: []}",
		"directory: {path: /srv/app, state: present, owner: root}",
		"directory: {path: /srv/app, delete_everything_inside_except: [a], permissions_mode: '0755', owner: root, group: root}",
		"directory: {path: /srv/app/releases, delete_everything_inside_except: [], create_missing_parent_directories: true}",
	} {
		if _, err := load(task); err != nil {
			t.Error(task, err)
		}
	}
	for task, message := range map[string]string{
		"directory: {path: /srv, state: absent}":                                                                   "at least two levels deep",
		"directory: {path: /, state: absent, also_delete_everything_inside: true}":                                 "at least two levels deep",
		"directory: {path: /srv/../etc, state: absent}":                                                            "at least two levels deep",
		"directory: {path: /srv, delete_everything_inside_except: []}":                                             "at least two levels deep",
		"directory: {path: srv/app, state: absent}":                                                                "absolute path",
		"directory: {path: /srv/app, state: gone}":                                                                 "state must be present or absent",
		"directory: {path: /srv/app, state: absent, owner: root}":                                                  "directory with state: absent accepts only path, state, and also_delete_everything_inside",
		"directory: {path: /srv/app, state: absent, delete_everything_inside_except: []}":                          "directory with state: absent accepts only path, state, and also_delete_everything_inside",
		"directory: {path: /srv/app, state: absent, create_missing_parent_directories: true}":                      "directory with state: absent accepts only path, state, and also_delete_everything_inside",
		"directory: {path: /srv/app, also_delete_everything_inside: true}":                                         "also_delete_everything_inside goes with state: absent",
		"directory: {path: /srv/app, delete_everything_inside_except: [a], also_apply_to_everything_inside: true}": "delete_everything_inside_except accepts path, create_missing_parent_directories, owner, group, and permissions_mode",
		"directory: {path: /srv/app, delete_everything_inside_except: [a/b]}":                                      "delete_everything_inside_except[0]): must name a direct child of the directory",
		"directory: {path: /srv/app, delete_everything_inside_except: [a, '..']}":                                  "delete_everything_inside_except[1]",
		"directory: {path: /srv/app, delete_everything_inside_except: ['']}":                                       "must name a direct child of the directory",
		"directory: {path: /srv/app, delete_everything_inside_except: ['{{ .vars.bad }}']}":                        "must name a direct child of the directory",
	} {
		if _, err := load(task); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s\nwant %q, got %v", task, message, err)
		}
	}
	// A deletion is tracked in a node task block; its path is fixed as the
	// block starts, and the list is resolved once.
	p, err := load("directory: {path: '/srv/app/{{ .vars.release }}', delete_everything_inside_except: ['{{ .vars.release }}']}")
	if err != nil {
		t.Fatal(err)
	}
	task := p.FleetStages[0].NodeTasks[0]
	if !Tracked(task, "changes") || len(TrackedInputs(task)) != 1 {
		t.Fatal("a directory deletion is not a tracked change")
	}
	resolved, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"release": "1.4.2"}})
	if err != nil || InputsOf[modules.Directory](resolved).Path != "/srv/app/1.4.2" || InputsOf[modules.Directory](resolved).DeleteEverythingInsideExcept[0] != "1.4.2" || InputsOf[modules.Directory](task).DeleteEverythingInsideExcept[0] != "{{ .vars.release }}" {
		t.Fatal(InputsOf[modules.Directory](resolved), err)
	}
	if tracked := TrackedInputs(resolved); tracked[0].Path != "/srv/app/1.4.2" {
		t.Fatal(tracked)
	}
	// An empty list, which empties the directory, survives the JSON that
	// carries the task to the node; no list stays no list.
	for _, list := range [][]string{{}, nil} {
		raw, err := json.Marshal(modules.Directory{Path: "/srv/app", DeleteEverythingInsideExcept: list})
		var back modules.Directory
		if err == nil {
			err = json.Unmarshal(raw, &back)
		}
		if err != nil || (back.DeleteEverythingInsideExcept == nil) != (list == nil) || back.Deletes() != (list != nil) {
			t.Fatal(string(raw), err)
		}
	}
	empty, err := load("directory: {path: /srv/app/releases, delete_everything_inside_except: []}")
	if err != nil || InputsOf[modules.Directory](empty.FleetStages[0].NodeTasks[0]).DeleteEverythingInsideExcept == nil {
		t.Fatal("an empty list was read as no list", err)
	}
	if again, err := ResolveArguments(empty.FleetStages[0].NodeTasks[0], value.Scope{}); err != nil || InputsOf[modules.Directory](again).DeleteEverythingInsideExcept == nil {
		t.Fatal("resolving lost the empty list", err)
	}
	// Two tasks of one node task block cannot track one path.
	block := "task_block: {id: app, changes: [{directory: {path: /srv/app/releases, delete_everything_inside_except: []}}, {directory: {path: /srv/app/releases, state: absent}}], activate: [{systemd_service: {name: app, state: running}}]}"
	if _, err := load(block); err == nil || !strings.Contains(err.Error(), "duplicate service configuration path /srv/app/releases") {
		t.Fatal(err)
	}
}

func TestDownloadDecompressConfiguration(t *testing.T) {
	digest := strings.Repeat("a", 64)
	load := func(decompress string) error {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: install, nodes: all, node_tasks: [{download: {permissions_mode: '0755', url: https://example.com/restic.bz2, sha256: "+digest+", path: /usr/local/bin/restic, decompress: "+decompress+"}}]}]"), "example", "", nil)
		return err
	}
	for _, format := range []string{"gzip", "bzip2", "xz", "zstd"} {
		if err := load(format); err != nil {
			t.Fatal(format, err)
		}
	}
	for _, format := range []string{"lz4", "gz", "GZIP"} {
		if err := load(format); err == nil || !strings.Contains(err.Error(), "download.decompress must be gzip, bzip2, xz, or zstd") {
			t.Fatal(format, err)
		}
	}
}
