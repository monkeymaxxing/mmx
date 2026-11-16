package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A fake firewall-cmd keeps zone public's services, ports, and rich rules in
// one file each for the running firewall and the stored configuration, as
// firewall-cmd prints them, and records its calls. A file "fail-on" names an
// argument it refuses; "stopped" makes firewalld not running.
const fakeFirewallCmd = `
echo "$*" >> "$dir/calls"
layer=running zone=
for a in "$@"; do case "$a" in --permanent) layer=permanent;; --zone=*) zone=${a#--zone=};; esac; done
[ ! -e "$dir/stopped" ] || { echo "not running"; exit 252; }
for a in "$@"; do
  case "$a" in
    --state) echo running; exit 0;;
    --info-service=ssh) printf 'ssh\n  ports: 22/tcp\n  protocols: \n'; exit 0;;
    --info-service=*) printf '%s\n  ports: 443/tcp\n' "${a#--info-service=}"; exit 0;;
  esac
done
[ "$zone" = public ] || { echo "Error: INVALID_ZONE: $zone" >&2; exit 112; }
f="$dir/$layer"
for a in "$@"; do
  if [ -e "$dir/fail-on" ] && [ "$a" = "$(cat "$dir/fail-on")" ]; then echo "Error: injected failure" >&2; exit 1; fi
  case "$a" in
    --list-services) tr '\n' ' ' < "$f/service"; echo; exit 0;;
    --list-ports) tr '\n' ' ' < "$f/port"; echo; exit 0;;
    --list-rich-rules) cat "$f/rich-rule"; exit 0;;
    --add-*) kind=${a#--add-}; kind=${kind%%=*}; v=${a#*=}
      grep -qxF -- "$v" "$f/$kind" || printf '%s\n' "$v" >> "$f/$kind"; echo success; exit 0;;
    --remove-*) kind=${a#--remove-}; kind=${kind%%=*}; v=${a#*=}
      grep -vxF -- "$v" "$f/$kind" > "$f/$kind.new"; mv "$f/$kind.new" "$f/$kind"; echo success; exit 0;;
  esac
done
exit 2`

type firewalldFixture struct{ dir string }

// newFirewalldFixture starts zone public as a fresh Red Hat family server has
// it: services ssh, dhcpv6-client, and cockpit, in both layers.
func newFirewalldFixture(t *testing.T) firewalldFixture {
	t.Helper()
	dir := t.TempDir()
	for _, layer := range []string{"running", "permanent"} {
		for name, lines := range map[string]string{"service": "ssh\ndhcpv6-client\ncockpit\n", "port": "", "rich-rule": ""} {
			if err := os.MkdirAll(filepath.Join(dir, layer), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, layer, name), []byte(lines), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	scripts := map[string]string{
		"firewall-cmd": "dir=" + dir + "\n" + fakeFirewallCmd,
		"sshd":         `printf 'port 22\nlistenaddress [::]:22\nlistenaddress 0.0.0.0:22\n'`,
	}
	for name, script := range scripts {
		writeStandInProgram(t, filepath.Join(dir, name), "#!/bin/sh\n"+script+"\n")
	}
	putProgramsFirst(t, dir)
	return firewalldFixture{dir}
}

// zone returns one layer's entries, each list sorted.
func (f firewalldFixture) zone(t *testing.T, layer string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, kind := range []string{"service", "port", "rich-rule"} {
		b, err := os.ReadFile(filepath.Join(f.dir, layer, kind))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Fields(strings.ReplaceAll(string(b), " ", "\x00"))
		for i := range lines {
			lines[i] = strings.ReplaceAll(lines[i], "\x00", " ")
		}
		slices.Sort(lines)
		out[kind] = lines
	}
	return out
}

func (f firewalldFixture) calls(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	os.Remove(filepath.Join(f.dir, "calls"))
	return string(b)
}

func (f firewalldFixture) set(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFirewalldEntriesInFirewalldsOwnForm(t *testing.T) {
	t.Parallel()
	for want, rule := range map[string]FirewalldRule{
		"service ssh":        {Action: "allow", FirewalldService: "ssh"},
		"port 7443/tcp":      {Action: "allow", Port: "7443", Proto: "tcp"},
		"port 8000-8010/udp": {Action: "allow", Port: "8000:8010", Proto: "udp"},
		`rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept`: {Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.0.0/8"},
		`rule port port="23" protocol="tcp" reject`:                                             {Action: "reject", Port: "023", Proto: "tcp"},
		`rule family="ipv6" source address="2001:db8::/32" service name="https" drop`:           {Action: "drop", FirewalldService: "https", From: "2001:DB8::/32"},
		`rule family="ipv4" source address="192.0.2.7" accept`:                                  {Action: "allow", From: "192.0.2.7/32"},
		`rule service name="ssh" drop`:                                                          {Action: "drop", FirewalldService: "ssh"},
	} {
		e, err := firewalldEntryFor(rule)
		if err != nil || e.String() != want {
			t.Errorf("%+v: %q %v", rule, e.String(), err)
		}
		// firewalld lists a rich rule in the same text, which reads back as
		// the same rule.
		if e.kind == "rich-rule" {
			if read := readFirewalldRichRule(e.value); read != e {
				t.Errorf("%q reads back as %+v, not %+v", e.value, read, e)
			}
		}
	}
	other := readFirewalldRichRule(`rule family="ipv4" source address="10.0.0.0/8" service name="ssh" log prefix="ssh" level="info" accept`)
	if other.action != "" || !strings.HasPrefix(other.identity, "rich rule ") {
		t.Fatalf("%+v", other)
	}
}

func TestFirewalldZone(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newFirewalldFixture(t)
	task := Firewalld{Zone: "public", RulesNotListed: "remove", Rules: []FirewalldRule{
		{Action: "allow", FirewalldService: "ssh"},
		{Action: "allow", Port: "7443", Proto: "tcp"},
		{Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.0.0/8"},
		{Action: "reject", Port: "23", Proto: "tcp"},
	}}
	want := map[string]any{
		"added":   []any{"port 7443/tcp", `rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept`, `rule port port="23" protocol="tcp" reject`},
		"removed": []any{"service dhcpv6-client", "service cockpit"},
	}
	before := f.zone(t, "running")
	preview := (Runtime{Dryrun: true}).Firewalld(ctx, task)
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
		t.Fatalf("%+v", preview)
	}
	if calls := f.calls(t); strings.Contains(calls, "--add") || strings.Contains(calls, "--remove") || !reflect.DeepEqual(f.zone(t, "running"), before) {
		t.Fatalf("dryrun changed the zone: %s", calls)
	}

	applied := (Runtime{}).Firewalld(ctx, task)
	if applied.Status != "succeeded" || applied.Change != "changed" || !reflect.DeepEqual(applied.Value, want) {
		t.Fatalf("%+v", applied)
	}
	expected := map[string][]string{"service": {"ssh"}, "port": {"7443/tcp"}, "rich-rule": {`rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept`, `rule port port="23" protocol="tcp" reject`}}
	for _, layer := range []string{"running", "permanent"} {
		if got := f.zone(t, layer); !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: %q", layer, got)
		}
	}
	calls := f.calls(t)
	if strings.Contains(calls, "--reload") || !strings.Contains(calls, "--zone=public --add-port=7443/tcp\n--permanent --zone=public --add-port=7443/tcp\n") {
		t.Fatalf("each rule goes to the running firewall and the stored configuration together, never a reload:\n%s", calls)
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).Firewalld(ctx, task); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}

	// A rule lost from the stored configuration only is written there again.
	f.set(t, "permanent/port", "")
	r := (Runtime{}).Firewalld(ctx, task)
	if r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"added": []any{"port 7443/tcp"}}) || strings.Count(f.calls(t), "--add-port") != 1 {
		t.Fatalf("%+v", r)
	}

	// Another action replaces the entry with the same identity.
	keep := Firewalld{Zone: "public", RulesNotListed: "keep", Rules: []FirewalldRule{{Action: "reject", Port: "7443", Proto: "tcp"}, {Action: "allow", Port: "23", Proto: "tcp"}}}
	r = (Runtime{}).Firewalld(ctx, keep)
	if r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{
		"added":   []any{`rule port port="7443" protocol="tcp" reject`, "port 23/tcp"},
		"removed": []any{"port 7443/tcp", `rule port port="23" protocol="tcp" reject`},
	}) {
		t.Fatalf("%+v", r)
	}
	if got := f.zone(t, "permanent"); !reflect.DeepEqual(got["port"], []string{"23/tcp"}) || !slices.Contains(got["rich-rule"], `rule port port="7443" protocol="tcp" reject`) || len(got["rich-rule"]) != 2 {
		t.Fatalf("%q", got)
	}

	// absent removes the listed identities whatever their action.
	absent := Firewalld{Zone: "public", State: "absent", Rules: []FirewalldRule{{Port: "7443", Proto: "tcp"}, {Port: "5432", Proto: "tcp", From: "10.0.0.0/8"}, {FirewalldService: "https"}}}
	r = (Runtime{}).Firewalld(ctx, absent)
	if r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"removed": []any{
		`rule family="ipv4" source address="10.0.0.0/8" port port="5432" protocol="tcp" accept`, `rule port port="7443" protocol="tcp" reject`}}) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).Firewalld(ctx, absent); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestFirewalldFailureAfterPartialWorkAndAnotherTry(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newFirewalldFixture(t)
	task := Firewalld{Zone: "public", RulesNotListed: "keep", Rules: []FirewalldRule{
		{Action: "allow", Port: "7443", Proto: "tcp"}, {Action: "allow", Port: "8080", Proto: "tcp"}}}
	f.set(t, "fail-on", "--add-port=8080/tcp")
	r := (Runtime{}).Firewalld(ctx, task)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "injected failure") {
		t.Fatalf("%+v", r)
	}
	if got := f.zone(t, "permanent")["port"]; !reflect.DeepEqual(got, []string{"7443/tcp"}) {
		t.Fatalf("%q", got)
	}
	// The value lists the rule that was added and not the rule that failed.
	if !reflect.DeepEqual(r.Value, map[string]any{"added": []any{"port 7443/tcp"}}) {
		t.Fatalf("a failed run reports %v", r.Value)
	}
	// Nothing was added when the first command fails.
	f.set(t, "fail-on", "--add-port=7443/tcp")
	f.set(t, "permanent/port", "")
	f.set(t, "running/port", "")
	if r := (Runtime{}).Firewalld(ctx, task); r.Status != "failed" || r.Value != nil {
		t.Fatalf("a failed run reports %+v", r)
	}
	f.set(t, "running/port", "7443/tcp\n")
	f.set(t, "permanent/port", "7443/tcp\n")
	os.Remove(filepath.Join(f.dir, "fail-on"))
	r = (Runtime{}).Firewalld(ctx, task)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"added": []any{"port 8080/tcp"}}) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).Firewalld(ctx, task); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestFirewalldSafeguardKeepsSSHReachable(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	f := newFirewalldFixture(t)
	for _, c := range []struct {
		rules   []FirewalldRule
		refused string
	}{
		{[]FirewalldRule{{Action: "allow", Port: "7443", Proto: "tcp"}}, "no longer admit sshd's TCP port 22"},
		{[]FirewalldRule{{Action: "allow", Port: "22", Proto: "udp"}}, "no longer admit sshd's TCP port 22"},
		{[]FirewalldRule{{Action: "allow", FirewalldService: "https"}}, "no longer admit sshd's TCP port 22"},
		// A rule that admits a source on every port names no port.
		{[]FirewalldRule{{Action: "allow", From: "192.0.2.50"}, {Action: "allow", Port: "443", Proto: "tcp"}}, "a rule with only from does not count"},
		// firewalld applies reject and drop rules before services and ports.
		{[]FirewalldRule{{Action: "allow", FirewalldService: "ssh"}, {Action: "reject", Port: "22", Proto: "tcp"}},
			`would reject sshd's TCP port 22 for every source in the running firewall through "rule port port=\"22\" protocol=\"tcp\" reject"`},
		{[]FirewalldRule{{Action: "allow", Port: "22", Proto: "tcp"}, {Action: "drop", FirewalldService: "ssh"}}, "would drop sshd's TCP port 22 for every source"},
		{[]FirewalldRule{{Action: "allow", FirewalldService: "ssh"}, {Action: "drop", Port: "1:1024", Proto: "tcp"}}, "would drop sshd's TCP port 22"},
	} {
		task := Firewalld{Zone: "public", RulesNotListed: "remove", Rules: c.rules}
		for _, dryrun := range []bool{true, false} {
			r := (Runtime{Dryrun: dryrun}).Firewalld(ctx, task)
			if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, c.refused) {
				t.Fatalf("%+v: %+v", c.rules, r)
			}
		}
	}
	if calls := f.calls(t); strings.Contains(calls, "--add") || strings.Contains(calls, "--remove") {
		t.Fatalf("changed the zone despite the safeguard: %s", calls)
	}
	absent := Firewalld{Zone: "public", State: "absent", Rules: []FirewalldRule{{FirewalldService: "ssh"}}}
	if r := (Runtime{Dryrun: true}).Firewalld(ctx, absent); r.Status != "failed" || !strings.Contains(r.Reason, "sshd") {
		t.Fatalf("%+v", r)
	}
	// SSH stays admitted through a port rule, a port range, a service whose
	// definition holds the port, or a port or service rule from one network,
	// and a reject or drop rule limited to one address leaves it admitted.
	for _, rules := range [][]FirewalldRule{
		{{Action: "allow", Port: "22", Proto: "tcp"}},
		{{Action: "allow", Port: "20:30", Proto: "tcp"}},
		{{Action: "allow", FirewalldService: "ssh", From: "10.0.0.0/8"}},
		{{Action: "allow", Port: "22", Proto: "tcp", From: "192.0.2.0/24"}},
		{{Action: "allow", FirewalldService: "ssh"}, {Action: "reject", Port: "22", Proto: "tcp", From: "198.51.100.7"}},
		{{Action: "allow", FirewalldService: "ssh"}, {Action: "drop", From: "198.51.100.0/24"}},
	} {
		task := Firewalld{Zone: "public", RulesNotListed: "remove", Rules: rules}
		if r := (Runtime{Dryrun: true}).Firewalld(ctx, task); r.Status != "succeeded" {
			t.Fatalf("%+v: %+v", rules, r)
		}
	}
}

func TestFirewalldNotInstalledNotRunningOrUnknownZone(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	task := Firewalld{Zone: "public", RulesNotListed: "keep", Rules: []FirewalldRule{{Action: "allow", FirewalldService: "https"}}}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if r := (Runtime{Dryrun: true}).Firewalld(ctx, task); r.Status != "not_checked" || !strings.Contains(r.Reason, missingProgramError("firewall-cmd").Error()) {
		t.Fatalf("%+v", r)
	}
	f := newFirewalldFixture(t)
	f.set(t, "stopped", "")
	if r := (Runtime{Dryrun: true}).Firewalld(ctx, task); r.Status != "not_checked" || !strings.Contains(r.Reason, "start it with a systemd_service task") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).Firewalld(ctx, task); r.Status != "failed" || !strings.Contains(r.Reason, "firewalld is not running") {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(f.dir, "stopped"))
	task.Zone = "internal"
	if r := (Runtime{}).Firewalld(ctx, task); r.Status != "failed" || !strings.Contains(r.Reason, "Create it with a firewalld_zone task first.") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).Firewalld(ctx, Firewalld{Zone: "public", Rules: []FirewalldRule{{Action: "allow", From: "10.0.0.0/8", FromInventoryGroup: &FromInventoryGroup{Group: "app", AddressVar: "private_address"}}}, RulesNotListed: "keep"}); r.FailureKind != "config" {
		t.Fatalf("%+v", r)
	}
}
