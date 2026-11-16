package modules

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fake semanage, setsebool, and restorecon keep the policy in files and
// record their calls. booleans holds "name running stored" lines; port.base
// and port.local "type protocol ports"; fcontext.base and fcontext.local
// "pattern<TAB>type"; relabel the paths restorecon -n lists. A file "fail"
// makes every change fail, and "stuck" makes restorecon change nothing.
const fakeSemanage = `
echo "semanage $*" >> "$dir/calls"
object=$1; shift
case "$object" in
boolean)
  while read -r name running stored; do printf '%-30s (%-3s,%4s)  Allow %s\n' "$name" "$running" "$stored" "$name"; done < "$dir/booleans"
  exit 0;;
esac
operation=$1; shift
if [ "$operation" = --list ]; then
  files="$dir/$object.local"
  [ "$*" = "--noheading --locallist" ] || files="$dir/$object.local $dir/$object.base"
  if [ "$object" = port ]; then
    cat $files | while read -r type proto ports; do printf '%-30s %-8s %s\n' "$type" "$proto" "$ports"; done
  else
    cat $files | while IFS='	' read -r pattern type; do printf '%-50s all files          system_u:object_r:%s:s0 \n' "$pattern" "$type"; done
  fi
  exit 0
fi
[ ! -e "$dir/fail" ] || { echo "ValueError: injected failure" >&2; exit 1; }
type= proto=
while [ $# -gt 1 ]; do case "$1" in --type) type=$2; shift 2;; --proto) proto=$2; shift 2;; *) shift;; esac; done
key=$1 local="$dir/$object.local"
if [ "$object" = port ]; then
  awk -v p="$proto" -v n="$key" '!($2 == p && $3 == n)' "$local" > "$local.new"
  [ "$operation" = --delete ] || echo "$type $proto $key" >> "$local.new"
else
  key="$key" awk -F '	' '$1 != ENVIRON["key"]' "$local" > "$local.new"
  [ "$operation" = --delete ] || printf '%s\t%s\n' "$key" "$type" >> "$local.new"
fi
mv "$local.new" "$local"`

const fakeSetsebool = `
echo "setsebool $*" >> "$dir/calls"
[ ! -e "$dir/fail" ] || { echo "Could not change boolean" >&2; exit 1; }
awk -v n="$2" -v v="$3" '$1 == n {print n, v, v; next} {print}' "$dir/booleans" > "$dir/booleans.new" && mv "$dir/booleans.new" "$dir/booleans"`

const fakeRestorecon = `
echo "restorecon $*" >> "$dir/calls"
if [ "$1" = -n ]; then cat "$dir/relabel"; exit 0; fi
[ -e "$dir/stuck" ] || : > "$dir/relabel"`

type selinuxFixture struct{ dir string }

func newSELinuxFixture(t *testing.T) selinuxFixture {
	t.Helper()
	dir := t.TempDir()
	f := selinuxFixture{dir}
	for name, contents := range map[string]string{
		"enforce":        "1\n",
		"booleans":       "httpd_can_network_connect off off\nhttpd_enable_homedirs on on\n",
		"port.base":      "http_port_t tcp 80, 443, 8443\nwebsm_port_t tcp 9090\n",
		"port.local":     "",
		"fcontext.base":  "/var/www(/.*)?\thttpd_sys_content_t\n",
		"fcontext.local": "",
		"relabel":        "",
		"calls":          "",
	} {
		f.set(t, name, contents)
	}
	for name, script := range map[string]string{"semanage": fakeSemanage, "setsebool": fakeSetsebool, "restorecon": fakeRestorecon} {
		writeStandInProgram(t, filepath.Join(dir, name), "#!/bin/sh\ndir="+dir+"\n"+script+"\n")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous, previousContext := selinuxEnforceFile, selinuxContextFile
	selinuxEnforceFile = filepath.Join(dir, "enforce")
	selinuxContextFile = filepath.Join(dir, "context")
	if err := os.WriteFile(selinuxContextFile, []byte("unconfined_u:unconfined_r:unconfined_t:s0\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { selinuxEnforceFile, selinuxContextFile = previous, previousContext })
	return f
}

func (f selinuxFixture) set(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f selinuxFixture) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// changes returns and forgets the calls that changed something.
func (f selinuxFixture) changes(t *testing.T) []string {
	var out []string
	for _, call := range strings.Split(strings.TrimSpace(f.read(t, "calls")), "\n") {
		if call != "" && !strings.Contains(call, "--list") && !strings.HasPrefix(call, "restorecon -n") {
			out = append(out, call)
		}
	}
	f.set(t, "calls", "")
	return out
}

func TestSELinuxModulesSkipWhereSELinuxIsNotEnabled(t *testing.T) {
	// No selinuxfs at all.
	testSELinuxModulesSkip(t, func(f selinuxFixture) { os.Remove(selinuxEnforceFile) })
	// selinuxfs mounted without a loaded policy, as on images that mount it
	// only: every process has the context "kernel".
	testSELinuxModulesSkip(t, func(f selinuxFixture) {
		f.set(t, "enforce", "0")
		f.set(t, "context", "kernel\x00")
	})
}

func testSELinuxModulesSkip(t *testing.T, notRunning func(selinuxFixture)) {
	ctx := context.Background()
	f := newSELinuxFixture(t)
	notRunning(f)
	previous := selinuxConfigFile
	selinuxConfigFile = filepath.Join(f.dir, "no-config")
	t.Cleanup(func() { selinuxConfigFile = previous })
	yes := true
	for _, r := range []struct {
		name   string
		result func(Runtime) any
	}{
		{"boolean", func(run Runtime) any {
			return run.SELinuxBoolean(ctx, SELinuxBoolean{Name: "httpd_can_network_connect", Enabled: &yes})
		}},
		{"file context", func(run Runtime) any {
			return run.SELinuxFileContext(ctx, SELinuxFileContext{Path: f.dir, Recurse: &yes, SELinuxType: "httpd_sys_content_t"})
		}},
		{"port", func(run Runtime) any {
			return run.SELinuxPort(ctx, SELinuxPort{Port: "8443", Proto: "tcp", SELinuxType: "http_port_t"})
		}},
		{"mode", func(run Runtime) any { return run.SELinuxMode(ctx, SELinuxMode{Mode: "enforcing"}) }},
	} {
		for _, dryrun := range []bool{true, false} {
			got := r.result(Runtime{Dryrun: dryrun})
			want := selinuxNotEnabled()
			if !reflect.DeepEqual(got, want) || want.Reason != "SELinux is not enabled on this node" || want.Status != "skipped" {
				t.Fatalf("%s: %+v", r.name, got)
			}
		}
	}
	if f.read(t, "calls") != "" {
		t.Fatal("ran SELinux tools on a node without SELinux:", f.read(t, "calls"))
	}
}

func TestSELinuxToolsMissing(t *testing.T) {
	ctx := context.Background()
	newSELinuxFixture(t)
	t.Setenv("PATH", t.TempDir())
	yes := true
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).SELinuxPort(ctx, SELinuxPort{Port: "8443", Proto: "tcp", SELinuxType: "http_port_t"})
		if (dryrun && r.Status != "not_checked") || (!dryrun && r.Status != "failed") || r.Reason != missingProgramError("semanage").Error() {
			t.Fatalf("%+v", r)
		}
	}
	if r := (Runtime{}).SELinuxBoolean(ctx, SELinuxBoolean{Name: "x", Enabled: &yes}); !strings.Contains(r.Reason, "policycoreutils-python-utils") {
		t.Fatalf("%+v", r)
	}
}

func TestSELinuxBoolean(t *testing.T) {
	ctx := context.Background()
	f := newSELinuxFixture(t)
	yes := true
	task := SELinuxBoolean{Name: "httpd_can_network_connect", Enabled: &yes}
	if r := (Runtime{Dryrun: true}).SELinuxBoolean(ctx, task); r.Status != "succeeded" || r.Change != "would_change" || len(f.changes(t)) != 0 {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxBoolean(ctx, task); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := f.changes(t); !reflect.DeepEqual(calls, []string{"setsebool -P httpd_can_network_connect on"}) || !strings.Contains(f.read(t, "booleans"), "httpd_can_network_connect on on") {
		t.Fatal(calls, f.read(t, "booleans"))
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).SELinuxBoolean(ctx, task); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// A value set only in the running policy is stored too.
	f.set(t, "booleans", "httpd_can_network_connect on off\n")
	if r := (Runtime{}).SELinuxBoolean(ctx, task); r.Status != "succeeded" || r.Change != "changed" || !strings.Contains(f.read(t, "booleans"), "on on") {
		t.Fatalf("%+v", r)
	}
	// A failed setsebool changes nothing; another try succeeds.
	no := false
	off := SELinuxBoolean{Name: "httpd_can_network_connect", Enabled: &no}
	f.set(t, "fail", "")
	if r := (Runtime{}).SELinuxBoolean(ctx, off); r.Status != "failed" || !strings.Contains(r.Reason, "Could not change boolean") {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(f.dir, "fail"))
	if r := (Runtime{}).SELinuxBoolean(ctx, off); r.Status != "succeeded" || r.Change != "changed" || !strings.Contains(f.read(t, "booleans"), "off off") {
		t.Fatalf("%+v", r)
	}
	unknown := SELinuxBoolean{Name: "container_manage_cgroup", Enabled: &yes}
	if r := (Runtime{Dryrun: true}).SELinuxBoolean(ctx, unknown); r.Status != "not_checked" || !strings.Contains(r.Reason, "has no boolean container_manage_cgroup") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxBoolean(ctx, unknown); r.Status != "failed" {
		t.Fatalf("%+v", r)
	}
}

func TestSELinuxPort(t *testing.T) {
	ctx := context.Background()
	f := newSELinuxFixture(t)
	port := func(p, proto, selinuxType, state string) SELinuxPort {
		return SELinuxPort{Port: PortOrRange(p), Proto: proto, SELinuxType: selinuxType, State: state}
	}
	// The base policy already gives 8443 this type; nothing to add, also when
	// the port is written 08443 or +8443.
	for _, written := range []string{"8443", "08443", "+8443"} {
		if r := (Runtime{}).SELinuxPort(ctx, port(written, "tcp", "http_port_t", "")); r.Change != "unchanged" {
			t.Fatalf("%s: %+v", written, r)
		}
	}
	// semanage lists the port in decimal, so a rerun finds what it added.
	if r := (Runtime{}).SELinuxPort(ctx, port("07001", "tcp", "http_port_t", "")); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := f.changes(t); !reflect.DeepEqual(calls, []string{"semanage port --add --type http_port_t --proto tcp 7001"}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).SELinuxPort(ctx, port("7001", "tcp", "http_port_t", "")); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxPort(ctx, port("07001", "tcp", "", "absent")); r.Status != "succeeded" || r.Change != "changed" || f.read(t, "port.local") != "" {
		t.Fatalf("%+v", r)
	}
	f.changes(t)
	for _, c := range []struct {
		task SELinuxPort
		call string
	}{
		{port("9090", "tcp", "http_port_t", ""), "semanage port --modify --type http_port_t --proto tcp 9090"},
		{port("8100:8102", "udp", "http_port_t", ""), "semanage port --add --type http_port_t --proto udp 8100-8102"},
		{port("8100:8102", "udp", "dns_port_t", ""), "semanage port --modify --type dns_port_t --proto udp 8100-8102"},
		{port("9090", "tcp", "", "absent"), "semanage port --delete --proto tcp 9090"},
	} {
		if r := (Runtime{Dryrun: true}).SELinuxPort(ctx, c.task); r.Change != "would_change" || len(f.changes(t)) != 0 {
			t.Fatalf("%+v: %+v", c.task, r)
		}
		if r := (Runtime{}).SELinuxPort(ctx, c.task); r.Status != "succeeded" || r.Change != "changed" {
			t.Fatalf("%+v: %+v", c.task, r)
		}
		if calls := f.changes(t); !reflect.DeepEqual(calls, []string{c.call}) {
			t.Fatal(calls)
		}
		for _, dryrun := range []bool{false, true} {
			if r := (Runtime{Dryrun: dryrun}).SELinuxPort(ctx, c.task); r.Change != "unchanged" {
				t.Fatalf("%+v: %+v", c.task, r)
			}
		}
	}
	if got := f.read(t, "port.local"); got != "dns_port_t udp 8100-8102\n" {
		t.Fatalf("%q", got)
	}
	// The base policy's definition cannot be removed; absent leaves it.
	if r := (Runtime{}).SELinuxPort(ctx, port("8443", "tcp", "", "absent")); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	f.set(t, "fail", "")
	if r := (Runtime{}).SELinuxPort(ctx, port("7000", "tcp", "http_port_t", "")); r.Status != "failed" || !strings.Contains(r.Reason, "injected failure") {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(f.dir, "fail"))
	if r := (Runtime{}).SELinuxPort(ctx, port("7000", "tcp", "http_port_t", "")); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
}

func TestSELinuxFileContext(t *testing.T) {
	ctx := context.Background()
	f := newSELinuxFixture(t)
	site := filepath.Join(f.dir, "my.site")
	if err := os.Mkdir(site, 0755); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	task := SELinuxFileContext{Path: site, Recurse: &yes, SELinuxType: "httpd_sys_content_t"}
	pattern := strings.ReplaceAll(site, ".", `\.`) + "(/.*)?"
	f.set(t, "relabel", "Would relabel a\nWould relabel b\nWould relabel c\n")

	preview := (Runtime{Dryrun: true}).SELinuxFileContext(ctx, task)
	if preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, map[string]any{"rule": "added"}) || !strings.Contains(preview.Reason, "not checked until the rule is in the policy") || len(f.changes(t)) != 0 {
		t.Fatalf("%+v", preview)
	}
	applied := (Runtime{}).SELinuxFileContext(ctx, task)
	if applied.Status != "succeeded" || applied.Change != "changed" || !reflect.DeepEqual(applied.Value, map[string]any{"rule": "added", "paths_relabeled": 3}) {
		t.Fatalf("%+v", applied)
	}
	if calls := f.changes(t); !reflect.DeepEqual(calls, []string{"semanage fcontext --add --type httpd_sys_content_t " + pattern, "restorecon -R " + site}) {
		t.Fatal(calls)
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).SELinuxFileContext(ctx, task); r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}

	// Labels that differ from the policy are repaired on every run, with the
	// rule unchanged.
	f.set(t, "relabel", "Would relabel moved-in\n")
	if r := (Runtime{Dryrun: true}).SELinuxFileContext(ctx, task); r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"paths_relabeled": 1}) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxFileContext(ctx, task); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"paths_relabeled": 1}) {
		t.Fatalf("%+v", r)
	}
	f.changes(t)

	// A path alone is relabeled without -R; another type updates the rule.
	single := SELinuxFileContext{Path: site, Recurse: &no, SELinuxType: "public_content_t"}
	f.set(t, "relabel", "Would relabel site\n")
	if r := (Runtime{}).SELinuxFileContext(ctx, single); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "added", "paths_relabeled": 1}) {
		t.Fatalf("%+v", r)
	}
	if calls := f.changes(t); calls[1] != "restorecon "+site {
		t.Fatal(calls)
	}
	single.SELinuxType = "httpd_sys_content_t"
	if r := (Runtime{}).SELinuxFileContext(ctx, single); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "updated"}) {
		t.Fatalf("%+v", r)
	}
	f.changes(t)

	// A rule for a base policy pattern with another type modifies it.
	www := SELinuxFileContext{Path: "/var/www", Recurse: &yes, SELinuxType: "public_content_t"}
	if r := (Runtime{}).SELinuxFileContext(ctx, www); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "updated"}) {
		t.Fatalf("%+v", r)
	}
	if calls := f.changes(t); calls[0] != `semanage fcontext --modify --type public_content_t /var/www(/.*)?` {
		t.Fatal(calls)
	}

	// A missing path is not an error.
	missing := SELinuxFileContext{Path: filepath.Join(f.dir, "missing"), Recurse: &yes, SELinuxType: "httpd_sys_content_t"}
	f.set(t, "relabel", "Would relabel x\n")
	if r := (Runtime{}).SELinuxFileContext(ctx, missing); r.Status != "succeeded" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "added"}) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxFileContext(ctx, missing); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}

	// restorecon that leaves wrong labels fails the task, which keeps its
	// rule; another try relabels.
	f.set(t, "stuck", "")
	r := (Runtime{}).SELinuxFileContext(ctx, task)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "1 paths below") {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(f.dir, "stuck"))
	if r := (Runtime{}).SELinuxFileContext(ctx, task); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"paths_relabeled": 1}) {
		t.Fatalf("%+v", r)
	}

	// A failed task reports in its value only what it did. semanage fails:
	// no rule changed. semanage works and restorecon leaves wrong labels:
	// the rule changed and no path was relabeled.
	retyped := task
	retyped.SELinuxType = "public_content_t"
	f.set(t, "fail", "")
	if r := (Runtime{}).SELinuxFileContext(ctx, retyped); r.Status != "failed" || r.Value != nil {
		t.Fatalf("a failed run reports %+v", r)
	}
	os.Remove(filepath.Join(f.dir, "fail"))
	f.set(t, "relabel", "Would relabel x\n")
	f.set(t, "stuck", "")
	if r := (Runtime{}).SELinuxFileContext(ctx, retyped); r.Status != "failed" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "updated"}) {
		t.Fatalf("a failed run reports %+v", r)
	}
	os.Remove(filepath.Join(f.dir, "stuck"))
	if r := (Runtime{}).SELinuxFileContext(ctx, retyped); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}

	absent := SELinuxFileContext{Path: site, Recurse: &yes, State: "absent"}
	if r := (Runtime{}).SELinuxFileContext(ctx, absent); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"rule": "removed"}) {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(f.read(t, "fcontext.local"), pattern) {
		t.Fatal(f.read(t, "fcontext.local"))
	}
	if r := (Runtime{}).SELinuxFileContext(ctx, absent); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestSELinuxMode(t *testing.T) {
	ctx := context.Background()
	f := newSELinuxFixture(t)
	for variable, name := range map[*string]string{&selinuxConfigFile: "config", &selinuxAutorelabel: "autorelabel", &kernelCommandLine: "cmdline"} {
		previous := *variable
		*variable = filepath.Join(f.dir, name)
		t.Cleanup(func() { *variable = previous })
	}
	const configText = "# SELINUX= can take one of these three values:\nSELINUX=%s\nSELINUXTYPE=targeted\n"
	f.set(t, "config", strings.Replace(configText, "%s", "permissive", 1))
	f.set(t, "enforce", "0")
	f.set(t, "cmdline", "BOOT_IMAGE=/vmlinuz root=UUID=1 ro\n")
	enforcing := SELinuxMode{Mode: "enforcing"}

	if r := (Runtime{Dryrun: true}).SELinuxMode(ctx, enforcing); r.Change != "would_change" || f.read(t, "enforce") != "0" || strings.Contains(f.read(t, "config"), "SELINUX=enforcing") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SELinuxMode(ctx, enforcing); r.Status != "succeeded" || r.Change != "changed" || r.Value != nil {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "enforce") != "1" || f.read(t, "config") != strings.Replace(configText, "%s", "enforcing", 1) {
		t.Fatal(f.read(t, "enforce"), f.read(t, "config"))
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).SELinuxMode(ctx, enforcing); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// A running mode changed by hand is set back; the file stays.
	f.set(t, "enforce", "0")
	if r := (Runtime{}).SELinuxMode(ctx, enforcing); r.Status != "succeeded" || r.Change != "changed" || f.read(t, "enforce") != "1" {
		t.Fatalf("%+v", r)
	}

	// On a kernel that runs without SELinux, the next boot turns it on and
	// labels every file.
	os.Remove(selinuxEnforceFile)
	f.set(t, "config", strings.Replace(configText, "%s", "disabled", 1))
	if r := (Runtime{Dryrun: true}).SELinuxMode(ctx, enforcing); r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"reboot_required": true}) {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(selinuxAutorelabel); err == nil {
		t.Fatal("dryrun created /.autorelabel")
	}
	r := (Runtime{}).SELinuxMode(ctx, enforcing)
	if r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"reboot_required": true}) {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "autorelabel") != "" || !strings.Contains(f.read(t, "config"), "\nSELINUX=enforcing\n") {
		t.Fatal(f.read(t, "config"))
	}
	if r := (Runtime{}).SELinuxMode(ctx, enforcing); r.Change != "unchanged" || !reflect.DeepEqual(r.Value, map[string]any{"reboot_required": true}) {
		t.Fatalf("until the reboot, the task still says one is needed: %+v", r)
	}
	// selinux=0 on the kernel command line would keep SELinux off.
	os.Remove(selinuxAutorelabel)
	f.set(t, "config", strings.Replace(configText, "%s", "disabled", 1))
	f.set(t, "cmdline", "BOOT_IMAGE=/vmlinuz root=UUID=1 ro selinux=0\n")
	if r := (Runtime{}).SELinuxMode(ctx, enforcing); r.FailureKind != "config" || !strings.Contains(r.Reason, "remove it first") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(selinuxAutorelabel); err == nil || strings.Contains(f.read(t, "config"), "SELINUX=enforcing") {
		t.Fatal("changed the node despite selinux=0")
	}
}
