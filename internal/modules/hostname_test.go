package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeState(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// takeCalls returns the calls of the stand-ins since the last time, and
// forgets them.
func takeCalls(t *testing.T, dir string) []string {
	t.Helper()
	text := strings.TrimSpace(readState(t, filepath.Join(dir, "calls")))
	writeState(t, filepath.Join(dir, "calls"), "")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// changing keeps only the calls that change something.
func changing(calls []string, readOnly ...string) []string {
	var out []string
	for _, call := range calls {
		reads := false
		for _, prefix := range readOnly {
			reads = reads || strings.HasPrefix(call, prefix)
		}
		if !reads {
			out = append(out, call)
		}
	}
	return out
}

// noMutation is a dryrun runtime that fails the test if anything asks to
// change the node.
func noMutation(t *testing.T) Runtime {
	return Runtime{Dryrun: true, BeforeMutation: func(resource string) error { t.Fatal("dryrun mutated", resource); return nil }}
}

// A stand-in hostnamectl keeps the stored name in $dir/static and the
// running one in $dir/kernel. A file "fail" makes set-hostname fail, and
// "stuck" makes it change only the stored name.
const fakeHostnamectl = `
case "$1" in
--static) cat "$dir/static";;
set-hostname)
  [ ! -e "$dir/fail" ] || { echo "Could not set static hostname: Access denied" >&2; exit 1; }
  echo "$2" > "$dir/static"
  [ -e "$dir/stuck" ] || echo "$2" > "$dir/kernel";;
esac`

func hostnameFixture(t *testing.T, stored, running string) string {
	t.Helper()
	dir := standInPrograms(t, map[string]string{"hostnamectl": fakeHostnamectl})
	writeState(t, filepath.Join(dir, "static"), stored+"\n")
	writeState(t, filepath.Join(dir, "kernel"), running+"\n")
	previous := runningHostnameFile
	runningHostnameFile = filepath.Join(dir, "kernel")
	t.Cleanup(func() { runningHostnameFile = previous })
	return dir
}

func TestHostnameSetsTheStoredAndRunningName(t *testing.T) {
	ctx := testContext(t)
	dir := hostnameFixture(t, "old", "old")
	h := Hostname{Name: "web1.example.com"}
	preview := noMutation(t).Hostname(ctx, h)
	want := map[string]any{"hostname_before": "old", "hostname_after": "web1.example.com"}
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
		t.Fatalf("%+v", preview)
	}
	if calls := changing(takeCalls(t, dir), "hostnamectl --static"); calls != nil || readState(t, filepath.Join(dir, "static")) != "old\n" {
		t.Fatal("dryrun changed the host name", calls)
	}
	mutations := 0
	made := (Runtime{BeforeMutation: func(string) error { mutations++; return nil }}).Hostname(ctx, h)
	if made.Status != "succeeded" || made.Change != "changed" || !reflect.DeepEqual(made.Value, want) || mutations != 1 {
		t.Fatalf("%+v %d", made, mutations)
	}
	if readState(t, filepath.Join(dir, "static")) != "web1.example.com\n" || readState(t, filepath.Join(dir, "kernel")) != "web1.example.com\n" {
		t.Fatal("host name not set")
	}
	if calls := changing(takeCalls(t, dir), "hostnamectl --static"); !reflect.DeepEqual(calls, []string{"hostnamectl set-hostname web1.example.com"}) {
		t.Fatal(calls)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.Hostname(ctx, h); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, dir), "hostnamectl --static"); calls != nil {
		t.Fatal(calls)
	}
	// A running name that differs from the stored one is set too.
	writeState(t, filepath.Join(dir, "kernel"), "localhost\n")
	if r := (Runtime{}).Hostname(ctx, h); r.Change != "changed" || readState(t, filepath.Join(dir, "kernel")) != "web1.example.com\n" {
		t.Fatalf("%+v", r)
	}
}

func TestHostnameFailures(t *testing.T) {
	ctx := testContext(t)
	h := Hostname{Name: "web1"}
	// hostnamectl refuses: nothing changed.
	dir := hostnameFixture(t, "old", "old")
	writeState(t, filepath.Join(dir, "fail"), "")
	if r := (Runtime{}).Hostname(ctx, h); r.Status != "failed" || r.Change != "unknown" || !strings.Contains(r.Reason, "Access denied") {
		t.Fatalf("%+v", r)
	}
	// The stored name changed and the running one did not: failed, changed;
	// the next try sets the running name.
	os.Remove(filepath.Join(dir, "fail"))
	writeState(t, filepath.Join(dir, "stuck"), "")
	r := (Runtime{}).Hostname(ctx, h)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, `the running host name is "old"`) {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(dir, "stuck"))
	if r := (Runtime{}).Hostname(ctx, h); r.Status != "succeeded" || r.Change != "changed" || r.Value.(map[string]any)["hostname_before"] != "web1" {
		t.Fatalf("%+v", r)
	}
	// Without hostnamectl, dryrun cannot check and a run fails, naming the package.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
		if r := run.Hostname(ctx, h); r.Status != status || !strings.Contains(r.Reason, "install the package systemd") {
			t.Fatalf("%+v", r)
		}
	}
	if r := (Runtime{}).Hostname(ctx, Hostname{}); r.FailureKind != "config" || !strings.Contains(r.Reason, "hostname.name is required") {
		t.Fatalf("%+v", r)
	}
}

func TestHostnameRule(t *testing.T) {
	t.Parallel()
	for name, ok := range map[string]bool{
		"web1": true, "web1.example.com": true, "Web-1.Example.COM": true, "a": true, strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false, strings.Repeat("a.", 32) + "a": false, "web_1": false, "-web": false, "web-": false,
		"web1.": false, ".web1": false, "web..example": false, "web 1": false, "": false,
	} {
		if err := hostnameRule(name); (err == nil) != ok {
			t.Error(name, err)
		}
	}
}
