package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/secrets"
	"mmx/internal/value"
)

func TestJobDeliversOnlyAssignedSecretInputs(t *testing.T) {
	shared := &value.Secret{Name: "shared", Data: map[string]any{"token": "shared-token"}, Available: true}
	a := &value.Secret{Name: "a", Data: "only-a", Available: true}
	b := &value.Secret{Name: "b", Data: "only-b", Available: true}
	j := config.Job{Node: "a", Scope: map[string]any{
		"secrets": map[string]any{"shared": shared, "unused": b},
		"outputs": map[string]any{"inspect": map[string]any{"a": map[string]any{"token": a}, "b": map[string]any{"token": b}}},
	}, NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"TOKEN": "{{ .outputs.inspect[.node.name].token }}", "SHARED": "{{ .secrets.shared.token }}"}}}}}
	scoped, err := scopeJobSecrets(j)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(scoped)
	wire, err := scoped.Transport()
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"only-a", "only-b", "shared-token"} {
		if strings.Contains(string(saved), token) {
			t.Fatal("plaintext in saved job")
		}
	}
	if !strings.Contains(string(wire), "only-a") || !strings.Contains(string(wire), "shared-token") || strings.Contains(string(wire), "only-b") {
		t.Fatal("wrong node secret assignment")
	}
	var decoded config.Job
	if err := protocol.Decode(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	s := value.Scope(decoded.Scope)
	if got, err := s.Text("{{ .secrets.shared.token }}"); err != nil || got != "shared-token" {
		t.Fatal(got, err)
	}
	if _, err := s.Get(".outputs.inspect.b.token"); err == nil {
		t.Fatal("unassigned node output is readable")
	}
}

// Static fields and controller-known selectors continue into a named secret:
// each node receives only the parts its references select, united across
// references, and reads them through the same paths. A node-only selector
// still sends the whole subtree beneath it.
func TestKnownSelectorsDeliverOnlyTheSelectedPartOfASecret(t *testing.T) {
	scope := func() map[string]any {
		return map[string]any{"secrets": map[string]any{
			"passwords": &value.Secret{Name: "passwords", Data: map[string]any{"a": "pass-a", "b": "pass-b"}, Available: true},
			"db":        &value.Secret{Name: "db", Data: map[string]any{"app": "app-pw", "report": "report-pw", "admin": "admin-pw"}, Available: true},
			"tokens":    &value.Secret{Name: "tokens", Data: map[string]any{"x": "token-x", "y": "token-y"}, Available: true},
		}}
	}
	env := map[string]any{
		"PASSWORD": "{{ .secrets.passwords[.node.name] }}",
		"APP":      "{{ .secrets.db.app }}",
		"REPORT":   `{{ .secrets.db["report"] }}`,
	}
	j := config.Job{Node: "a", Scope: scope(), NodeTasks: []config.Task{
		{ID: "read", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s %s %s' "$PASSWORD" "$APP" "$REPORT"`, Env: env}},
		{ID: "each", Loop: []any{"x", "y"}, As: "item", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s' "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .secrets.tokens[.item] }}"}}},
	}, Outputs: map[string]any{"read": "{{ .node_tasks.read.stdout }}", "token": "{{ .node_tasks.each.items[1].stdout }}"}}
	scoped, err := scopeJobSecrets(j)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := scoped.Transport()
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]bool{"pass-a": true, "pass-b": false, "app-pw": true, "report-pw": true, "admin-pw": false, "token-x": true, "token-y": true} {
		if strings.Contains(string(wire), token) != want {
			t.Fatalf("delivery of %s is %v: %s", token, !want, wire)
		}
	}
	var managedNode config.Job
	if err := protocol.Decode(wire, &managedNode); err != nil {
		t.Fatal(err)
	}
	r := (&engine.Engine{}).Run(context.Background(), managedNode)
	if r.Status != "succeeded" || value.Plain(r.Outputs["read"]) != "pass-a app-pw report-pw" || value.Plain(r.Outputs["token"]) != "token-y" {
		t.Fatal(r.Status, r.Error, r.Outputs)
	}
	// A whole read of the same secret beside a selected field sends it whole.
	j = config.Job{Node: "a", Scope: scope(), NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"APP": "{{ .secrets.db.app }}"}}}},
		Outputs: map[string]any{"db": "{{ .secrets.db }}"}}
	if scoped, err = scopeJobSecrets(j); err != nil {
		t.Fatal(err)
	}
	if wire, _ = scoped.Transport(); !strings.Contains(string(wire), "admin-pw") {
		t.Fatal("a whole-secret read was narrowed")
	}
	// A key missing from the secret sends none of it; the host fails that read.
	j = config.Job{Node: "c", Scope: scope(), NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"PASSWORD": "{{ .secrets.passwords[.node.name] }}"}}}}}
	if scoped, err = scopeJobSecrets(j); err != nil {
		t.Fatal(err)
	}
	if wire, _ = scoped.Transport(); strings.Contains(string(wire), "pass-") {
		t.Fatal("a missing key delivered other nodes' values")
	}
	managedNode = config.Job{}
	if err := protocol.Decode(wire, &managedNode); err != nil {
		t.Fatal(err)
	}
	if r := (&engine.Engine{}).Run(context.Background(), managedNode); r.Status != "failed" || !strings.Contains(r.Error, "/secrets/passwords/c") {
		t.Fatal(r.Status, r.Error)
	}
}

// A fleet stage-loop item or fleet stage-loop output that selects a field of a named vault
// secret is computed on the controller; the node receives that field on the
// first run, not an unavailable placeholder.
func TestControllerValuesSelectingASecretFieldReachTheNode(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "secrets.age")
	if err := secrets.Write(vault, "vault-pass", []byte("db:\n  password: field-value\n  admin: admin-value\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_VAULT_PASSWORD", "vault-pass")
	item := filepath.Join(t.TempDir(), "item")
	output := filepath.Join(t.TempDir(), "output")
	itemContent, outputContent := "{{ .site.password }}", "{{ .outputs.sites.one.password }}"
	sites := config.FleetStage{ID: "sites", Loop: []any{map[string]any{"name": "one", "password": "{{ .secrets.db.password }}"}}, As: "site", Key: "{{ .site.name }}",
		FleetStages: []config.FleetStage{{ID: "deploy", Nodes: "a", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: item, Content: &itemContent, Mode: "0600"}}}}},
		Outputs:     map[string]any{"password": "{{ .site.password }}"}}
	use := config.FleetStage{ID: "use", Nodes: "a", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: output, Content: &outputContent, Mode: "0600"}}}}
	c, r := fleet(t, sites, use)
	r.Project.Vault = vault
	c.Secrets = &secrets.Store{}
	delivered := map[string]string{}
	c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		j, err := scopeJobSecrets(mustJob(nodeJob(r, fleetStage, en)))
		if err != nil {
			return en, err
		}
		wire, _ := j.Transport()
		delivered[fleetStage.ID] = string(wire)
		return runOnNode(ctx, r, fleetStage, en)
	}
	execute(t, c, r, false)
	for _, path := range []string{item, output} {
		if got, _ := os.ReadFile(path); r.Status != "succeeded" || string(got) != "field-value" {
			t.Fatalf("status %s, %s holds %q; use: %+v", r.Status, path, got, entry(t, r, "use", "a").Response)
		}
	}
	for fleetStage, wire := range delivered {
		if strings.Contains(wire, "admin-value") {
			t.Fatal(fleetStage, "received an unselected field")
		}
	}
}

// Node-only selectors cannot be resolved on the controller, so a reference
// delivers everything under its static prefix and the node selects from it.
func TestDynamicSelectorsDeliverTheirStaticPrefix(t *testing.T) {
	passwords := &value.Secret{Name: "passwords", Data: map[string]any{"x": "pass-x", "y": "pass-y"}, Available: true}
	j := config.Job{ID: "dynamic", Node: "a", Scope: map[string]any{
		"secrets": map[string]any{"passwords": passwords, "unused": value.Sensitive("unused-secret")},
		"outputs": map[string]any{
			"inspect": map[string]any{"x": map[string]any{"token": value.Sensitive("token-x")}, "y": map[string]any{"token": value.Sensitive("token-y")}},
			"other":   map[string]any{"x": map[string]any{"token": value.Sensitive("other-token")}},
		},
	}, NodeTasks: []config.Task{{ID: "each", Loop: []any{"x", "y"}, As: "item", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s %s' "$PASS" "$TOKEN"`, Env: map[string]any{"PASS": "{{ .secrets.passwords[.item] }}", "TOKEN": "{{ .outputs.inspect[.item].token }}"}}}},
		Outputs: map[string]any{"seen": "{{ .node_tasks.each.items[1].stdout }}"}}
	scoped, err := scopeJobSecrets(j)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := scoped.Transport()
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]bool{"pass-x": true, "pass-y": true, "token-x": true, "token-y": true, "unused-secret": false, "other-token": false} {
		if strings.Contains(string(wire), token) != want {
			t.Fatal("wrong delivery of", token)
		}
	}
	var managedNode config.Job
	if err := protocol.Decode(wire, &managedNode); err != nil {
		t.Fatal(err)
	}
	r := (&engine.Engine{}).Run(context.Background(), managedNode)
	if r.Status != "succeeded" || value.Plain(r.Outputs["seen"]) != "pass-y token-y" {
		t.Fatal(r.Status, r.Error)
	}
}

// A variable selects on the controller only while no task in the job can
// reassign it; otherwise the node chooses within the whole collection.
func TestVariableSelectorsFollowOnlyStableValues(t *testing.T) {
	peers := map[string]any{"a": map[string]any{"token": value.Sensitive("peer-a")}, "b": map[string]any{"token": value.Sensitive("peer-b")}}
	use := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"TOKEN": "{{ .outputs.peers[.vars.peer].token }}"}}}
	for _, tc := range []struct {
		nodeTasks []config.Task
		b         bool
	}{
		{[]config.Task{use}, false},
		{[]config.Task{{TaskBlock: &config.TaskBlock{ID: "switch", Changes: []config.Task{{SetVars: map[string]any{"peer": "b"}}}}}, use}, true},
	} {
		j := config.Job{Node: "c", Vars: map[string]any{"peer": "a"}, Scope: map[string]any{"outputs": map[string]any{"peers": peers}}, NodeTasks: tc.nodeTasks}
		scoped, err := scopeJobSecrets(j)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := scoped.Transport()
		if !strings.Contains(string(wire), "peer-a") || strings.Contains(string(wire), "peer-b") != tc.b {
			t.Fatal("wrong peer delivery", tc.b)
		}
	}
}

func TestRetryDoesNotReplayCompletedSecretProducer(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		restorable   bool
	}{
		{"named", "{{ .secrets.token }}", true},
		{"computed", "derived-{{ .secrets.token }}", false},
	} {
		t.Run(tc.name, func(t *testing.T) { testSecretOutputRetry(t, tc.output, tc.restorable) })
	}
}

func testSecretOutputRetry(t *testing.T, output string, restorable bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configured")
	content := "{{ .outputs.producer.a.token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "producer", Nodes: "a", Outputs: map[string]any{"token": output}},
		config.FleetStage{ID: "consumer", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: path, Content: &content, Mode: "0600"}}}},
	)
	r.Project.Secrets = map[string]string{"token": "MMX_TEST_ROTATING_SECRET"}
	// The fixture creates the run before this test supplies its secret binding.
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_ROTATING_SECRET", "before-rotation")
	attempts := map[string]int{}
	failing := true
	c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		attempts[fleetStage.ID]++
		if fleetStage.ID == "consumer" && failing {
			en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
			return en, nil
		}
		j, err := scopeJobSecrets(mustJob(nodeJob(r, fleetStage, en)))
		if err != nil {
			return en, err
		}
		en.privateInputs = j.PrivateInputs
		wire, err := j.Transport()
		if err != nil {
			return en, err
		}
		var managedNode config.Job
		if err := protocol.Decode(wire, &managedNode); err != nil {
			return en, err
		}
		en.Response = (&engine.Engine{}).Run(ctx, managedNode)
		return en, nil
	}
	execute(t, c, r, false)
	if r.Status != "failed" {
		t.Fatal(r.Status)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "before-rotation") {
		t.Fatal("run retains plaintext")
	}
	var err error
	r, err = c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_ROTATING_SECRET", "after-rotation")
	failing = false
	execute(t, c, r, true)
	if attempts["producer"] != 1 || attempts["consumer"] != 2 {
		t.Fatal(r.Status, attempts)
	}
	if restorable {
		got, err := os.ReadFile(path)
		if r.Status != "succeeded" || err != nil || string(got) != "after-rotation" || !r.SecretsRefreshed {
			t.Fatal(r.Status, string(got), err)
		}
	} else {
		if r.Status == "succeeded" || !strings.Contains(r.Entries[1].Response.Error, "secret output is unavailable") {
			t.Fatal("missing computed output was not reported", r.Entries[1].Response)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("consumer wrote a file without its secret")
		}
	}
	filepath.WalkDir(c.State, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "after-rotation") || strings.Contains(string(b), "before-rotation") {
				t.Error("secret in controller state", path)
			}
		}
		return nil
	})
}
func TestMissingTaskSecretFailsBeforeAnyNodeDispatch(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "first", Nodes: "all", NodeTasks: []config.Task{{Show: "ordinary work"}}}, config.FleetStage{ID: "second", Nodes: "a", NodeTasks: []config.Task{{Show: "{{ .secrets.missing }}"}}})
	r.Project.Secrets = map[string]string{"missing": "MMX_TEST_MISSING_SECRET"}
	t.Setenv("MMX_TEST_MISSING_SECRET", "")
	c.executeNode = func(context.Context, *Run, config.FleetStage, Entry, bool, func(Entry) error) (Entry, error) {
		t.Error("dispatched with missing secret")
		return Entry{}, nil
	}
	if err := c.Execute(context.Background(), r, false); err == nil || !strings.Contains(err.Error(), "MMX_TEST_MISSING_SECRET") {
		t.Fatal(err)
	}
	if r.Status != "failed" {
		t.Fatal("preflight failure left a running record")
	}
}

// runOnHost delivers a fleet stage's job as the controller does and runs it in-process.
func runOnNode(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry) (Entry, error) {
	j, err := scopeJobSecrets(mustJob(nodeJob(r, fleetStage, en)))
	if err != nil {
		return en, err
	}
	wire, err := j.Transport()
	if err != nil {
		return en, err
	}
	var managedNode config.Job
	if err := protocol.Decode(wire, &managedNode); err != nil {
		return en, err
	}
	en.Response = (&engine.Engine{}).Run(ctx, managedNode)
	return en, nil
}

// A controller-known selector that cannot resolve (an unavailable earlier
// output, a secret used as a key, a missing key) fails on the node too, so
// delivery sends nothing for it. A selector that reads a node-only value, even
// through a nested selector, still delivers the subtree the node selects in.
func TestUnresolvableSelectorsDeliverNothing(t *testing.T) {
	inspect := map[string]any{"a": map[string]any{"token": value.Sensitive("token-a")}, "b": map[string]any{"token": value.Sensitive("token-b")}, "c": map[string]any{"token": value.Sensitive("token-c")}}
	for name, tc := range map[string]struct {
		ref         string
		unavailable map[string]string
		pick        map[string]any
		delivered   bool
	}{
		"failed fleet_stage": {"{{ .outputs.inspect[.outputs.pick.a.peer].token }}", map[string]string{"/outputs/pick/a": "pick on a is failed"}, map[string]any{}, false},
		"secret key":         {"{{ .outputs.inspect[.secrets.peer].token }}", nil, map[string]any{}, false},
		"missing key":        {"{{ .outputs.inspect[.outputs.pick.a.peer].token }}", nil, map[string]any{"a": map[string]any{}}, false},
		"node fact":          {"{{ .outputs.inspect[.node.os.id].token }}", nil, map[string]any{}, true},
		"nested":             {"{{ .outputs.inspect[.outputs.pick.a[.node_tasks.r.stdout]].token }}", nil, map[string]any{"a": map[string]any{}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			scope := map[string]any{"outputs": map[string]any{"inspect": inspect, "pick": tc.pick}, "secrets": map[string]any{"peer": &value.Secret{Name: "peer", Data: "a", Available: true}}}
			j := config.Job{Node: "b", Scope: scope, Unavailable: tc.unavailable, NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"TOKEN": tc.ref}}}}}
			scoped, err := scopeJobSecrets(j)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := scoped.Transport()
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{"token-a", "token-b", "token-c"} {
				if strings.Contains(string(wire), token) != tc.delivered {
					t.Fatalf("delivery of %s is %v", token, !tc.delivered)
				}
			}
		})
	}
}

// index with keys the controller resolves selects like a selector path, so a
// node receives only its own export and still renders it.
func TestIndexDeliversOnlyTheSelectedExport(t *testing.T) {
	inspect := map[string]any{"a": map[string]any{"token": value.Sensitive("token-a")}, "b": map[string]any{"token": value.Sensitive("token-b")}}
	for _, source := range []string{`{{ (index .outputs.inspect .node.name).token }}`, `{{ index .outputs.inspect .node.name "token" }}`} {
		path := filepath.Join(t.TempDir(), "token")
		j := config.Job{Node: "a", Scope: map[string]any{"outputs": map[string]any{"inspect": inspect}},
			NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: path, Template: "inline", TemplateText: source, Mode: "0600"}}}}
		scoped, err := scopeJobSecrets(j)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := scoped.Transport()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wire), "token-a") || strings.Contains(string(wire), "token-b") {
			t.Fatal("wrong export delivery for", source)
		}
		var managedNode config.Job
		if err := protocol.Decode(wire, &managedNode); err != nil {
			t.Fatal(err)
		}
		managedNode.NodeTasks = j.NodeTasks // Template text travels as an asset.
		r := (&engine.Engine{}).Run(context.Background(), managedNode)
		if got, _ := os.ReadFile(path); r.Status != "succeeded" || string(got) != "token-a" {
			t.Fatal(source, r.Status, r.Error, string(got))
		}
	}
}

// A retry after a stop between fleet stage-loop iterations loads the secrets of
// iterations that never started, even though an earlier iteration's entry of
// the same static fleet stage succeeded.
func TestRetryLoadsSecretsForUnstartedLoopIterations(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "rollout", Loop: []any{"x", "y"}, As: "item", Key: "{{ .item }}", FleetStages: []config.FleetStage{
		{ID: "deploy", Nodes: "a", NodeTasks: []config.Task{{Show: "{{ .secrets.token }}"}}},
	}})
	r.Project.Secrets = map[string]string{"token": "MMX_TEST_LOOP_TOKEN"}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_LOOP_TOKEN", "value")
	retrying := false
	delivered := map[string]bool{}
	c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		j, err := scopeJobSecrets(mustJob(nodeJob(r, fleetStage, en)))
		if err != nil {
			return en, err
		}
		secret, _ := j.Scope["secrets"].(map[string]any)["token"].(*value.Secret)
		delivered[fleetStage.ID] = secret != nil && secret.Available
		if !retrying {
			// Ctrl-C while the first iteration runs.
			if err := c.requestStop(r.ID); err != nil {
				t.Fatal(err)
			}
		}
		return runOnNode(ctx, r, fleetStage, en)
	}
	execute(t, c, r, false)
	if r.Status != "stopped" || !delivered["rollout[x]/deploy"] || delivered["rollout[y]/deploy"] {
		t.Fatalf("setup: %s %v", r.Status, delivered)
	}
	var err error
	if r, err = c.Load(r.ID, true); err != nil {
		t.Fatal(err)
	}
	retrying = true
	execute(t, c, r, true)
	if y := entry(t, r, "rollout[y]/deploy", "a"); !delivered["rollout[y]/deploy"] || r.Status != "succeeded" {
		t.Fatal("retry did not load the secret for the unstarted iteration", r.Status, y.Response)
	}
}

// FleetStage-loop items that carry a named secret bind it loaded on a first run and
// refresh it on retry, like completed outputs, so a rotated secret reaches the
// retried iteration.
func TestFleetStageLoopNamedSecretRefreshesOnRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configured")
	content := "{{ .site.password }}"
	sites := config.FleetStage{ID: "sites", Loop: []any{map[string]any{"name": "one", "password": "{{ .secrets.token }}"}}, As: "site", Key: "{{ .site.name }}",
		FleetStages: []config.FleetStage{{ID: "deploy", Nodes: "a", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: path, Content: &content, Mode: "0600"}}}}}}
	inputs := map[string]string{"token": "MMX_TEST_LOOP_SECRET"}
	failing := true
	managedNode := func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		if failing {
			en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
			return en, nil
		}
		return runOnNode(ctx, r, fleetStage, en)
	}
	c, r := fleet(t, sites)
	r.Project.Secrets = inputs
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	c.executeNode = managedNode
	t.Setenv("MMX_TEST_LOOP_SECRET", "before-rotation")
	execute(t, c, r, false)
	var err error
	if r, err = c.Load(r.ID, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_LOOP_SECRET", "after-rotation")
	failing = false
	execute(t, c, r, true)
	if got, _ := os.ReadFile(path); r.Status != "succeeded" || string(got) != "after-rotation" {
		t.Fatalf("retry status %s, file %q, entries %+v", r.Status, got, r.Entries)
	}
	c, r = fleet(t, sites)
	r.Project.Secrets = inputs
	c.executeNode = managedNode
	t.Setenv("MMX_TEST_LOOP_SECRET", "first-run")
	execute(t, c, r, false)
	if got, _ := os.ReadFile(path); r.Status != "succeeded" || string(got) != "first-run" {
		t.Fatalf("first run status %s, file %q", r.Status, got)
	}
}

// A fresh run that fails before any node job starts, such as for a missing
// secret, leaves no saved run: there is nothing to resume.
func TestRunFailingBeforeAnyNodeJobSavesNoRun(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "first", Nodes: "all", NodeTasks: []config.Task{{Show: "{{ .secrets.missing }}"}}})
	r.Project.Secrets = map[string]string{"missing": "MMX_TEST_MISSING_SECRET"}
	t.Setenv("MMX_TEST_MISSING_SECRET", "")
	err := c.Execute(context.Background(), r, false)
	var notStarted *NotStartedError
	if !errors.As(err, &notStarted) || !strings.Contains(err.Error(), "MMX_TEST_MISSING_SECRET") {
		t.Fatalf("want a NotStartedError naming the secret, got %v", err)
	}
	if _, statErr := os.Stat(c.runDir(r.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("the run was saved: %v", statErr)
	}
}

// A run that started node jobs and then failed stays saved, so it can be resumed.
func TestRunFailingAfterNodeJobsStartedStaysSaved(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "first", Nodes: "a", NodeTasks: []config.Task{{Show: "work"}}})
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		en.Response = protocol.Response{Status: "failed", Error: "fixture failure"}
		return en, nil
	}
	err := c.Execute(context.Background(), r, false)
	var notStarted *NotStartedError
	if errors.As(err, &notStarted) {
		t.Fatal("a run with node jobs is not a run that failed before starting")
	}
	if r.Status != "failed" {
		t.Fatal(r.Status)
	}
	if _, statErr := os.Stat(c.runDir(r.ID)); statErr != nil {
		t.Fatalf("the run was not saved: %v", statErr)
	}
}
