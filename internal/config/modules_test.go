package config

import (
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestFileMetadataConfiguration(t *testing.T) {
	for _, task := range []string{
		"file: {path: /etc/example, permissions_mode: '0640'}",
		"file: {path: /etc/example, owner: root, group: root}",
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []string{
		"file: {path: /etc/example}",
		"file: {path: /etc/example, permissions_mode: '0999'}",
		"file: {path: /etc/example, permissions_mode: '0600', check_new_file: {shell: {script: 'exit 0'}}}",
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err == nil {
			t.Fatal("accepted invalid file task", task)
		}
	}
}

// owner and group take an account name as text or a numeric ID as an integer;
// text made only of digits is refused, so one spelling has one meaning.
func TestOwnerAndGroupAreANameOrANumber(t *testing.T) {
	load := func(vars, task string) (Project, error) {
		p, _, err := Load(fixture(t, "vars: "+vars+"\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n"), "example", "", nil)
		return p, err
	}
	for task, message := range map[string]string{
		"file: {permissions_mode: '0644', path: /srv/data/x, content: x, owner: 1000, group: 1000}":                                              "",
		"file: {permissions_mode: '0644', path: /srv/data/x, content: x, owner: 0, group: app}":                                                  "",
		"directory: {path: /srv/data, owner: 999, group: 999}":                                                                                   "",
		"download: {permissions_mode: '0644', url: 'https://example.com/x', sha256: " + strings.Repeat("a", 64) + ", path: /srv/x, owner: 1000}": "",
		"directory: {path: /srv/data, owner: '{{ .vars.app_uid }}', group: '{{ .vars.app_group }}'}":                                             "",
		"file: {permissions_mode: '0644', path: /srv/data/x, content: x, owner: '1000'}":                                                         `"1000" is text made only of digits; to use the numeric ID, write it without quotes`,
		"directory: {path: /srv/data, group: \"999\"}":                                                                                           "write it without quotes",
		"directory: {path: /srv/data, owner: -1}":                                                                                                "account ID must be an integer from 0 through 4294967294",
		"directory: {path: /srv/data, owner: 1.5}":                                                                                               "must be an account name or a numeric ID",
		"directory: {path: /srv/data, owner: true}":                                                                                              "must be an account name or a numeric ID",
		"directory: {path: /srv/data, owner: [app]}":                                                                                             "",
	} {
		_, err := load("{app_uid: 1000, app_group: app, quoted: '1000'}", task)
		if strings.Contains(task, "[app]") {
			if err == nil {
				t.Error("a list was accepted as an owner")
			}
			continue
		}
		if message == "" && err != nil || message != "" && (err == nil || !strings.Contains(err.Error(), message)) {
			t.Errorf("%s\nwant %q, got %v", task, message, err)
		}
	}
	// A reference resolves to a number or a name by the same rule.
	reference := "directory: {path: /srv/data, owner: '{{ .vars.app_uid }}', group: '{{ .vars.quoted }}'}"
	if _, err := load("{app_uid: 1000, quoted: '1000'}", reference); err == nil || !strings.Contains(err.Error(), "directory.group") || !strings.Contains(err.Error(), "write it without quotes") {
		t.Fatal(err)
	}
	p, err := load("{app_uid: 1000, quoted: app}", reference)
	if err != nil {
		t.Fatal(err)
	}
	task := p.FleetStages[0].NodeTasks[0]
	scope := value.Scope{"vars": map[string]any{"app_uid": 1000, "quoted": "1000"}}
	if _, err := ResolveArguments(task, scope); err == nil || !strings.Contains(err.Error(), "write it without quotes") {
		t.Fatal(err)
	}
	scope["vars"].(map[string]any)["quoted"] = "app"
	resolved, err := ResolveArguments(task, scope)
	if err != nil || InputsOf[modules.Directory](resolved).Owner != "1000" || !InputsOf[modules.Directory](resolved).Owner.IsID() || InputsOf[modules.Directory](resolved).Group != "app" || InputsOf[modules.Directory](resolved).Group.IsID() {
		t.Fatal(InputsOf[modules.Directory](resolved), err)
	}
	// A resolved task keeps the ID through the JSON that carries it to the node.
	if again, err := ResolveArguments(resolved, scope); err != nil || InputsOf[modules.Directory](again).Owner.Literal() != int64(1000) {
		t.Fatal(InputsOf[modules.Directory](again), err)
	}
}

// A service task names a unit of any kind MMX installs; anything else is a
// .service, as systemctl reads a bare name.
func TestUnitName(t *testing.T) {
	for name, want := range map[string]string{
		"nginx": "nginx.service", "app@1": "app@1.service", "app.v2": "app.v2.service",
		"app.device": "app.device.service", "nginx.service": "nginx.service",
		"workers.target": "workers.target", "swapfile.swap": "swapfile.swap", "batch.slice": "batch.slice", "srv-data.automount": "srv-data.automount",
		"backup.timer": "backup.timer", "restart.path": "restart.path", "app.socket": "app.socket",
		`srv-my\x2ddata.mount`: `srv-my\x2ddata.mount`,
	} {
		if got := modules.UnitName(name); got != want {
			t.Errorf("UnitName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A reload is checked against the resolved name, as the node would run it.
func TestResolvedServiceNameGetsTheReloadRule(t *testing.T) {
	task := Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "{{ .vars.unit }}", Refresh: "reload"}}
	for name, ok := range map[string]bool{"app": true, "app.service": true, "backup.timer": false, "data.mount": false} {
		_, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"unit": name}})
		if (err == nil) != ok || (!ok && !strings.Contains(err.Error(), "service.name: refresh: reload applies only to .service units")) {
			t.Fatal(name, err)
		}
	}
}

// An invalid service setting names its field and the accepted values.
func TestServiceSettingsNameTheirField(t *testing.T) {
	for task, want := range map[string]string{
		"systemd_service: {name: app}":                                                         "systemd_service.state is required",
		"systemd_service: {name: app, state: reloaded}":                                        "(fleet_stages[0].node_tasks[0].systemd_service.state): systemd_service.state is required and must be running, stopped, or do_not_start_or_stop",
		"systemd_service: {name: app, masked: true, state: running}":                           "(fleet_stages[0].node_tasks[0].systemd_service.masked): systemd_service.masked: true cannot go with state: running",
		"systemd_service: {name: app, masked: true, state: stopped, start_at_boot: true}":      "(fleet_stages[0].node_tasks[0].systemd_service.masked): systemd_service.masked: true cannot go with start_at_boot: true",
		"shell: {script: 'true', run_only_once_marker_file: relative/marker}":                  "(fleet_stages[0].node_tasks[0].shell.run_only_once_marker_file): path must be absolute",
		"systemd_service: {name: app, in_systemd_of_user_account: root, state: running}":       "(fleet_stages[0].node_tasks[0].systemd_service.in_systemd_of_user_account): root's units are system units",
		"systemd_unit: {name: app.service, content: x, in_systemd_of_user_account: 'no such'}": "(fleet_stages[0].node_tasks[0].systemd_unit.in_systemd_of_user_account): invalid account name",
		"wait_for: {command_succeeds: 'true', tcp_port_accepts_connections: 80}":               "wait_for requires exactly one of tcp_port_accepts_connections, tcp_port_refuses_connections, path_exists, path_is_missing, url_responds, command_succeeds",
		"wait_for: {tcp_port_accepts_connections: 80, run_as_user_account: app}":               "wait_for.run_as_user_account, run_from_directory, and env require command_succeeds",
		"file: {permissions_mode: '0644', path: /etc/app.conf, content: x, check_new_file: {shell: {script: 'true', run_only_once_marker_file: /var/lib/mmx/commands_run_once/x}}}": "check_new_file.shell does not take run_only_once_marker_file",
		"systemd_service: {name: app, refresh: reboot, state: running}":          "(fleet_stages[0].node_tasks[0].systemd_service.refresh): systemd_service.refresh must be reload or restart",
		"systemd_service: {name: backup.timer, refresh: reload, state: running}": "(fleet_stages[0].node_tasks[0].systemd_service.name): refresh: reload applies only to .service units",
		"systemd_service: {name: 'bad name', refresh: reload, state: running}":   "(fleet_stages[0].node_tasks[0].systemd_service.name): invalid service name",
		"systemd_unit: {name: backup.timers, content: '[Timer]\n'}":              "(fleet_stages[0].node_tasks[0].systemd_unit.name): unit name must be a .service, .timer, .path, .socket, .mount, .target, .swap, .slice, or .automount filename",
	} {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n"), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(task, err)
		}
	}
}

// restart_now restarts a running unit once, so it needs state: running and
// its marker file, and it stays out of a task block's activate phase, where
// the block's tracked inputs decide restarts.
func TestRestartNowNeedsItsMarkerFileAndStaysOutOfActivate(t *testing.T) {
	restart := "systemd_service: {name: app, state: running, restart_now: true, run_only_once_marker_file: /var/lib/mmx/commands_run_once/restart_app}"
	block := func(phase string) string {
		return "task_block: {id: app, changes: [{file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}], " + phase + ": [{" + restart + "}]}"
	}
	for task, want := range map[string]string{
		restart:           "",
		block("verify"):   "",
		block("cleanup"):  "",
		block("activate"): "(fleet_stages[0].node_tasks[0].task_block.activate[0].systemd_service.restart_now): systemd_service.restart_now is not allowed in a task_block's activate phase",
		"systemd_service: {name: app, state: running, restart_now: true}":                                                                  "(fleet_stages[0].node_tasks[0].systemd_service.restart_now): systemd_service.restart_now requires run_only_once_marker_file",
		"systemd_service: {name: app, state: do_not_start_or_stop, restart_now: true, run_only_once_marker_file: /var/lib/mmx/x}":          "(fleet_stages[0].node_tasks[0].systemd_service.restart_now): systemd_service.restart_now goes with state: running",
		"systemd_service: {name: app, state: stopped, restart_now: true, run_only_once_marker_file: /var/lib/mmx/x}":                       "systemd_service.restart_now goes with state: running",
		"systemd_service: {name: app, state: running, run_only_once_marker_file: /var/lib/mmx/x}":                                          "(fleet_stages[0].node_tasks[0].systemd_service.run_only_once_marker_file): systemd_service.run_only_once_marker_file goes with restart_now: true",
		"systemd_service: {name: app, state: running, restart_now: true, run_only_once_marker_file: relative/marker}":                      "(fleet_stages[0].node_tasks[0].systemd_service.run_only_once_marker_file): path must be absolute",
		"systemd_service: {name: app, state: running, restart_now: true, run_only_once_marker_file: /x, in_systemd_of_user_account: shop}": "",
	} {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n"), "example", "", nil)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s\nwant %q, got %v", task, want, err)
		}
	}
}

// only_when_tracked_inputs_changed belongs to a shell task with an id in a
// task block's activate phase; checks_before_restart_or_reload holds only
// tasks that change nothing, in a block that can restart something.
func TestCommandThatFollowsAChangeAndChecksConfiguration(t *testing.T) {
	block := func(extra string) string {
		return "task_block: {id: postfix_map, changes: [{file: {permissions_mode: '0644', path: /etc/postfix/virtual, content: x}}], " + extra + "}"
	}
	command := "{id: rebuild, shell: {script: postmap /etc/postfix/virtual, only_when_tracked_inputs_changed: true}}"
	service := "{systemd_service: {name: postfix, state: running}}"
	for task, want := range map[string]string{
		block("activate: [" + command + "]"): "",
		block("activate: [" + command + "], checks_before_restart_or_reload: [{assert: {that: true}}, {shell: {script: 'true', changed_when: false}}, {wait_for: {tcp_port_accepts_connections: 25}}, {show: ready}, {http: {url: 'http://127.0.0.1/'}}]"): "",
		block("activate: [" + service + "], checks_before_restart_or_reload: [{assert: {that: true}}]"): "",
		"shell: {script: postmap /etc/postfix/virtual, only_when_tracked_inputs_changed: true}":         "only_when_tracked_inputs_changed requires a shell node task in a node task block's activate phase",
		block("validate: [" + command + "]"): "only_when_tracked_inputs_changed requires a shell node task in a node task block's activate phase",
		block("activate: [{shell: {script: postmap /etc/postfix/virtual, only_when_tracked_inputs_changed: true}}]"):                                    "needs an id",
		block("checks_before_restart_or_reload: [{assert: {that: true}}]"):                                                                              "checks_before_restart_or_reload would never run",
		block("activate: [" + service + "], checks_before_restart_or_reload: [{shell: {script: 'true'}}]"):                                              "must say changed_when: false",
		block("activate: [" + service + "], checks_before_restart_or_reload: [{file: {permissions_mode: '0644', path: /etc/approved, content: x}}]"):    "accepts only tasks that change nothing",
		block("activate: [" + service + "], checks_before_restart_or_reload: [{systemd_service: {name: postfix, state: running}}]"):                     "accepts only tasks that change nothing",
		block("activate: [" + service + "], checks_before_restart_or_reload: [{http: {url: 'http://127.0.0.1/', method: POST}}]"):                       "must read: method GET or HEAD",
		"http: {url: 'http://127.0.0.1/', method: POST, json_body: {name: '{{ .vars.nobody }}'}}":                                                       "vars.nobody",
		"file: {permissions_mode: '0644', path: /etc/x, content: x, check_new_file: {shell: {script: 'true', only_when_tracked_inputs_changed: true}}}": "check_new_file.shell does not take only_when_tracked_inputs_changed",
	} {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n"), "example", "", nil)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s\nwant %q, got %v", task, want, err)
		}
	}
	// Two different commands with the same ids in two fleet stages would share
	// one record on a node.
	stage := func(id, script string) string {
		return "{id: " + id + ", nodes: all, node_tasks: [{task_block: {id: postfix_map, changes: [{file: {permissions_mode: '0644', path: /etc/postfix/virtual, content: x}}], activate: [{id: rebuild, shell: {script: " + script + ", only_when_tracked_inputs_changed: true}}]}}]}"
	}
	_, _, err := Load(fixture(t, "fleet_stages: ["+stage("first", "postmap /etc/postfix/virtual")+", "+stage("second", "newaliases")+"]\n"), "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "would share a command record on the node because the commands have the task path postfix_map/activate/rebuild") {
		t.Fatal(err)
	}
}

// A command record's key is the project's name and the task's result path, so
// two different commands anywhere in the project must have different paths:
// in two playbooks, or in a one-time playbook, as in one. Both mmx validate
// and a run of either playbook refuse them and name both places. A loop whose
// iterations each hold such a command needs a key, so that an iteration keeps
// its record when items are added or removed.
func TestCommandRecordKeysAreUniqueInTheProject(t *testing.T) {
	blockWith := func(script string) string {
		return "{task_block: {id: postfix_map, changes: [{file: {permissions_mode: '0644', path: /etc/postfix/virtual, content: x}}], activate: [{id: rebuild, shell: {script: " + script + ", only_when_tracked_inputs_changed: true}}]}}"
	}
	stage := func(id, task string) string {
		return "fleet_stages: [{id: " + id + ", nodes: all, node_tasks: [" + task + "]}]\n"
	}
	const shared = "would share a command record on the node because the commands have the task path postfix_map/activate/rebuild"
	refused := func(t *testing.T, path, playbook string, want ...string) {
		t.Helper()
		want = append(want, shared)
		_, _, err := Load(path, playbook, "", nil)
		for _, w := range want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Fatalf("run of %s: want %q, got %v", playbook, w, err)
			}
		}
		_, err = ValidateProject(path)
		for _, w := range want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Fatalf("validate: want %q, got %v", w, err)
			}
		}
	}

	t.Run("two playbooks", func(t *testing.T) {
		path := fixture(t, stage("first", blockWith("postmap /etc/postfix/virtual")))
		write(t, filepath.Join(filepath.Dir(path), "playbooks/aliases.yml"), stage("second", blockWith("newaliases")))
		refused(t, path, "example", "playbooks/example.yml", "playbooks/aliases.yml")
		refused(t, path, "aliases", "playbooks/example.yml", "playbooks/aliases.yml")
	})
	t.Run("a one-time playbook", func(t *testing.T) {
		path := fixture(t, stage("first", blockWith("postmap /etc/postfix/virtual")))
		write(t, filepath.Join(filepath.Dir(path), "one_time_playbooks/2026-10-05_rebuild_aliases.yml"), stage("second", blockWith("newaliases")))
		refused(t, path, "example", "playbooks/example.yml", "one_time_playbooks/2026-10-05_rebuild_aliases.yml")
	})
	t.Run("one role called from two playbooks is one command", func(t *testing.T) {
		path := fixture(t, stage("first", "{role: postfix}"))
		root := filepath.Dir(path)
		write(t, filepath.Join(root, "roles/postfix/main.yml"), "node_tasks: ["+blockWith("postmap /etc/postfix/virtual")+"]\n")
		write(t, filepath.Join(root, "playbooks/again.yml"), stage("second", "{role: postfix}"))
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateProject(path); err != nil {
			t.Fatal(err)
		}
	})

	const needsKey = "a loop whose iterations run a shell command with only_when_tracked_inputs_changed requires key"
	// Each task is written once without key, and once with key: '{{ .item }}'
	// in place of KEY.
	for name, task := range map[string]string{
		"a looped role call":  "{role: postfix, loop: [one, two], as: item KEY}",
		"a looped shell task": "{task_block: {id: postfix_map, changes: [{file: {permissions_mode: '0644', path: /etc/postfix/virtual, content: x}}], activate: [{id: rebuild, shell: {script: 'postmap \"$MAP\"', env: {MAP: '{{ .item }}'}, only_when_tracked_inputs_changed: true}, loop: [one, two], as: item KEY}]}}",
	} {
		t.Run(name, func(t *testing.T) {
			path := fixture(t, stage("first", strings.Replace(task, " KEY", "", 1)))
			write(t, filepath.Join(filepath.Dir(path), "roles/postfix/main.yml"), "node_tasks: ["+blockWith("postmap /etc/postfix/virtual")+"]\n")
			if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), needsKey) {
				t.Fatal(err)
			}
			write(t, filepath.Join(filepath.Dir(path), "playbooks/example.yml"), stage("first", strings.Replace(task, " KEY", ", key: '{{ .item }}'", 1)))
			if _, _, err := Load(path, "example", "", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// systemd can neither enable nor disable the service that Podman generates
// from a Quadlet file. A systemd_service task that says start_at_boot for the
// generated service of a podman_quadlet task of the same fleet stage, in the
// same systemd, is refused when the project loads: on the node, a dryrun
// before the first run has no such unit to look at.
func TestEnabledOnTheServiceOfAQuadletFileIsRefusedAtLoad(t *testing.T) {
	quadlet := func(extra string) string {
		return "{podman_quadlet: {name: shop-web.container, content: \"[Container]\\nImage=registry.example.com/shop/web:2.4\\n\"" + extra + "}}"
	}
	const refused = "unit shop-web.service is generated from the Quadlet file shop-web.container, so systemd can neither enable nor disable it; remove start_at_boot from this task. Whether the unit starts at boot is the [Install] section of that file: WantedBy=multi-user.target for the system's systemd, WantedBy=default.target for an account's; the podman_quadlet node task at "
	for tasks, want := range map[string]string{
		quadlet("") + ", {systemd_service: {name: shop-web, start_at_boot: true, state: running}}":                                                                     refused,
		quadlet("") + ", {systemd_service: {name: shop-web.service, start_at_boot: false, state: running}}":                                                            refused,
		"{systemd_service: {name: shop-web, start_at_boot: true, state: running}}, " + quadlet(""):                                                                     refused,
		"{task_block: {id: shop, changes: [" + quadlet("") + "], activate: [{systemd_service: {name: shop-web, start_at_boot: true, state: running}}]}}":               refused,
		quadlet(", in_systemd_of_user_account: shop") + ", {systemd_service: {name: shop-web, start_at_boot: true, in_systemd_of_user_account: shop, state: running}}": refused,
		// Without start_at_boot, another unit, another systemd, or a removal.
		quadlet("") + ", {systemd_service: {name: shop-web, state: running}}":                                                                                                        "",
		quadlet("") + ", {systemd_service: {name: shop-worker, start_at_boot: true, state: running}}":                                                                                "",
		quadlet(", in_systemd_of_user_account: shop") + ", {systemd_service: {name: shop-web, start_at_boot: true, state: running}}":                                                 "",
		"{podman_quadlet: {name: shop-web.container, state: file_and_service_removed_named_volumes_kept}}, {systemd_service: {name: shop-web, start_at_boot: true, state: running}}": "",
	} {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: ["+tasks+"]}]\n"), "example", "", nil)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "systemd_service.start_at_boot")) {
			t.Errorf("%s\nwant %q, got %v", tasks, want, err)
		}
	}
	// Another fleet stage runs on its own nodes.
	stages := "fleet_stages: [{id: files, nodes: all, node_tasks: [" + quadlet("") + "]}, {id: services, nodes: all, node_tasks: [{systemd_service: {name: shop-web, start_at_boot: true, state: running}}]}]\n"
	if _, _, err := Load(fixture(t, stages), "example", "", nil); err != nil {
		t.Fatal(err)
	}
}

// A unit in an account's own systemd has a path only once the node has
// looked the account up; until then it is not a tracked path.
func TestUnitOfAnAccountIsLocatedOnTheNode(t *testing.T) {
	content := "[Service]\n"
	u := modules.Unit{Name: "report-worker.service", DropIn: "10-limits.conf", InSystemdOfUserAccount: "reports", Content: &content}
	if u.Located() || TrackedInputs(Task{Module: "systemd_unit", ModuleInputs: &u}) != nil {
		t.Fatal("an account's unit had a path before the node looked the account up")
	}
	u.UserUnitDirectory, u.UserUnitOwner, u.UserUnitGroup = "/home/reports/.config/systemd/user", "1001", "1001"
	f := u.File()
	if f.Path != "/home/reports/.config/systemd/user/report-worker.service.d/10-limits.conf" || f.Owner != "1001" || f.Group != "1001" || !f.Owner.IsID() {
		t.Fatal(f.Path, f.Owner, f.Group)
	}
	if system := (modules.Unit{Name: "app.service", Content: &content}).File(); system.Path != "/etc/systemd/system/app.service" || system.Owner != "root" {
		t.Fatal(system.Path, system.Owner)
	}
}

func TestScheduledJobRequiresAnAccountForCreationAndRemoval(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		for _, account := range []string{"", ", run_as_user_account: root", ", run_as_user_account: 'not an account'"} {
			scheduledJob := "systemd_scheduled_job: {name: report, state: " + state + account
			if state == "present" {
				scheduledJob += ", script: 'true', systemd_calendar_schedule: daily"
			}
			scheduledJob += "}"
			_, _, err := Load(fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+scheduledJob+"}]}]\n"), "example", "", nil)
			if account == ", run_as_user_account: root" {
				if err != nil {
					t.Fatal(state, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "run_as_user_account") {
				t.Fatalf("%s account %q: %v", state, account, err)
			}
		}
	}
}
