package config

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

func TestDownloadConfiguration(t *testing.T) {
	digest := strings.Repeat("a", 64)
	base := fmt.Sprintf(`url: https://example.com/app, sha256: %q, path: /usr/local/bin/app, permissions_mode: "0755"`, digest)
	load := func(input string) error {
		_, _, err := Load(fixture(t, "fleet_stages: [{id: install, nodes: all, node_tasks: [{download: {"+input+"}}]}]"), "example", "", nil)
		return err
	}
	if err := load(base + `, check_new_file: {shell: {script: 'test -s "$CANDIDATE"', env: {CANDIDATE: '{{ .new_file.path }}'}}}`); err != nil {
		t.Fatal(err)
	}
	// Exactly one of the SHA-256 or dangerously_skip_checksum: true.
	unpinned := "url: https://example.com/app, path: /usr/local/bin/app, permissions_mode: '0755'"
	if err := load(unpinned + ", dangerously_skip_checksum: true"); err != nil {
		t.Fatal(err)
	}
	if err := load(base + ", dangerously_skip_checksum: false"); err != nil {
		t.Fatal(err)
	}
	for input, message := range map[string]string{
		unpinned: "download.sha256 is required",
		unpinned + ", dangerously_skip_checksum: false": "download.sha256 is required",
		base + ", dangerously_skip_checksum: true":      "download takes sha256 or dangerously_skip_checksum: true, not both",
		unpinned + ", dangerously_skip_checksum: yes!":  "dangerously_skip_checksum",
	} {
		if err := load(input); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatal(input, err)
		}
	}
	for _, bad := range []string{
		strings.Replace(base, "https:", "http:", 1),
		strings.Replace(base, "https://", "https://user:password@", 1),
		strings.Replace(base, digest, "xyz", 1),
		strings.Replace(base, digest, "{{ .vars.digest }}", 1),
		strings.Replace(base, "example.com/app", "example.com/{{ .facts.arch }}", 1),
		strings.Replace(base, "/usr/local/bin/app", "relative", 1),
		base + ", state: absent", base + ", content: nope", base + ", source: local",
		base + ", owner: '{{ .vars.missing }}'", base + `, permissions_mode: "0999"`,
		base + `, check_new_file: {shell: {script: 'true', env: {VALUE: '{{ .vars.missing }}'}}}`,
	} {
		if err := load(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestDownloadLoadsAsAFileReportedAsDownload(t *testing.T) {
	digest := strings.Repeat("a", 64)
	p, inv, err := Load(fixture(t, fmt.Sprintf("fleet_stages: [{id: install, nodes: all, node_tasks: [{download: {url: https://example.com/app, sha256: %q, path: /usr/local/bin/app, permissions_mode: '0755'}}]}]", digest)), "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := p.FleetStages[0].NodeTasks[0]
	if InputsOf[modules.Download](task) != nil || InputsOf[modules.File](task) == nil || InputsOf[modules.File](task).URL != "https://example.com/app" || InputsOf[modules.File](task).SHA256 != digest {
		t.Fatal(task)
	}
	raw, err := json.Marshal(task)
	var saved map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &saved)
	}
	if inputs, _ := saved["module_inputs"].(map[string]any); err != nil || saved["module"] != "download" || inputs["url"] != "https://example.com/app" {
		t.Fatal(string(raw), err)
	}
	var restored Task
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if action, err := restored.Action(); err != nil || action.Name != "download" || !action.RetryByDefault {
		t.Fatal(action, err)
	}
	view, err := Inspect(p, inv, []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	explained := view.FleetStages[0].NodeTasks[0]
	fields := map[string]Argument{}
	for _, a := range explained.Arguments {
		fields[a.Field] = a
	}
	if explained.Action != "download" || fields["download.path"].Value != "/usr/local/bin/app" || fields["download.path"].Source.Field != "fleet_stages[0].node_tasks[0].download.path" || fields["download.url"].Value != "https://example.com/app" || fields["download.sha256"].Status != "literal" || fields["download.permissions_mode"].Value != "0755" {
		t.Fatal(explained)
	}
	both := fmt.Sprintf("{download: {permissions_mode: '0644', url: https://example.com/app, sha256: %q, path: /a}, file: {permissions_mode: '0644', path: /b, content: x}}", digest)
	if _, _, err := Load(fixture(t, "fleet_stages: [{id: install, nodes: all, node_tasks: ["+both+"]}]"), "example", "", nil); err == nil || !strings.Contains(err.Error(), "a node task must name exactly one module") {
		t.Fatal(err)
	}
}

func TestDownloadFrozenOnceAcrossBranchesAndRestoredWithoutSource(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "frozen") }))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	ref, _ := assets.Hash(context.Background(), strings.NewReader("frozen"))
	d := func(path string) Task {
		return Task{Module: "download", ModuleInputs: &modules.File{Mode: "0644", URL: server.URL, SHA256: ref.SHA256, Path: path}}
	}
	p := Project{FleetStages: []FleetStage{{FleetStages: []FleetStage{{NodeTasks: []Task{{RoleNodeTasks: []Task{d("/first")}}, {TaskBlock: &TaskBlock{ID: "download", Changes: []Task{d("/second")}, Cleanup: []Task{d("/third")}}}, {Loop: []any{"one", "two"}, As: "item", Module: "download", ModuleInputs: InputsOf[modules.File](d("/{{ .item }}"))}}}}}}}
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
	var saved Project
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&saved, dir); err != nil {
		t.Fatal(err)
	}
	nodeTasks := saved.FleetStages[0].FleetStages[0].NodeTasks
	refs, err := RequiredAssets(nodeTasks, nil)
	if err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
	data, err := os.ReadFile(refs[0].Path)
	if err != nil || string(data) != "frozen" {
		t.Fatal(string(data), err)
	}
	if err := FreezeAssets(context.Background(), &saved, t.TempDir(), nil); err == nil {
		t.Fatal("new run skipped source verification")
	}
	InputsOf[modules.File](nodeTasks[0].RoleNodeTasks[0]).SHA256 = strings.Repeat("0", 64)
	if _, err := RequiredAssets(nodeTasks, nil); err == nil {
		t.Fatal("accepted different pin at admission")
	}
}

// A pinned download that an earlier run saved is reused after its SHA-256 is
// checked again; changed bytes are fetched again, and an unchecked download is
// always fetched.
func TestPinnedDownloadReusedFromEarlierRun(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "archive") }))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	ref, _ := assets.Hash(context.Background(), strings.NewReader("archive"))
	project := func(digest string) Project {
		return Project{FleetStages: []FleetStage{{NodeTasks: []Task{{Module: "download", ModuleInputs: &modules.File{Mode: "0644", URL: server.URL, SHA256: digest, Path: "/a"}}}}}}
	}
	freeze := func(digest string, earlier ...string) (Project, string) {
		t.Helper()
		p, dir := project(digest), t.TempDir()
		if err := FreezeAssets(context.Background(), &p, dir, earlier); err != nil {
			t.Fatal(err)
		}
		return p, dir
	}
	_, first := freeze(ref.SHA256)
	p, second := freeze(ref.SHA256, filepath.Join(t.TempDir(), "removed"), first)
	if hits.Load() != 1 {
		t.Fatal("fetched again", hits.Load())
	}
	asset := *InputsOf[modules.File](p.FleetStages[0].NodeTasks[0]).Asset
	asset.Path = filepath.Join(second, ref.SHA256)
	if err := assets.Verify(context.Background(), asset); err != nil {
		t.Fatal(err)
	}

	changed := t.TempDir()
	if err := os.WriteFile(filepath.Join(changed, ref.SHA256), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, third := freeze(ref.SHA256, changed); hits.Load() != 2 {
		t.Fatal("reused changed bytes", hits.Load())
	} else if data, err := os.ReadFile(filepath.Join(third, ref.SHA256)); err != nil || string(data) != "archive" {
		t.Fatal(string(data), err)
	}

	freeze("", first)
	if hits.Load() != 3 {
		t.Fatal("reused an unchecked download", hits.Load())
	}
}

// A download with decompress is saved decompressed; its sha256 checks the file
// as published, also for a web address that reads vars.
func TestDownloadDecompressedIsFrozenDecompressed(t *testing.T) {
	var published bytes.Buffer
	gz := gzip.NewWriter(&published)
	io.WriteString(gz, "decompressed tool")
	gz.Close()
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write(published.Bytes()) }))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	digest, _ := assets.Hash(context.Background(), bytes.NewReader(published.Bytes()))
	tool, _ := assets.Hash(context.Background(), strings.NewReader("decompressed tool"))
	variable := &modules.File{Mode: "0644", URL: "{{ .vars.address }}", SHA256: digest.SHA256, Decompress: "gzip", Path: "/c",
		Variants: map[string]*modules.SourceVariant{"one": {URL: server.URL, SHA256: digest.SHA256}}}
	p := Project{FleetStages: []FleetStage{{NodeTasks: []Task{
		{Module: "file", ModuleInputs: &modules.File{Mode: "0644", URL: server.URL, SHA256: digest.SHA256, Decompress: "gzip", Path: "/a"}},
		{Module: "file", ModuleInputs: &modules.File{Mode: "0644", URL: server.URL, SHA256: digest.SHA256, Path: "/b"}},
		{Module: "file", ModuleInputs: variable},
	}}}}
	dir := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, dir, nil); err != nil {
		t.Fatal(err)
	}
	nodeTasks := p.FleetStages[0].NodeTasks
	if InputsOf[modules.File](nodeTasks[0]).Asset.SHA256 != tool.SHA256 || InputsOf[modules.File](nodeTasks[1]).Asset.SHA256 != digest.SHA256 || variable.Variants["one"].Asset.SHA256 != tool.SHA256 {
		t.Fatal(InputsOf[modules.File](nodeTasks[0]).Asset, InputsOf[modules.File](nodeTasks[1]).Asset, variable.Variants["one"].Asset)
	}
	if hits.Load() != 2 {
		t.Fatal("fetches", hits.Load())
	}
	// The node accepts the decompressed asset for the published file's digest.
	raw, err := json.Marshal(p)
	var saved Project
	if err == nil {
		err = json.Unmarshal(raw, &saved)
	}
	if err == nil {
		err = BindProjectAssets(&saved, dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := RequiredAssets(saved.FleetStages[0].NodeTasks, nil); err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
}
