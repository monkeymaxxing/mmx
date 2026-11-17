package modules

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Output of ufw 0.36 on Debian 13 after rules of each kind were added.
const ufwShowAdded = `Added user rules (see 'ufw status' for running firewall):
ufw limit 22/tcp
ufw allow from 10.0.0.0/8 to any port 5432 proto tcp
ufw allow 8000:8010/udp
ufw allow OpenSSH
ufw allow 25/tcp comment 'mail server'
ufw allow in on eth0 to any port 99
ufw deny out 25
ufw route allow in on eth0 out on eth1
ufw allow in on incusbr0
ufw route allow 443/tcp
ufw allow from 2001:db8::/32 to any port 443 proto tcp
ufw allow from 192.0.2.0/24
ufw reject from 198.51.100.7 proto udp
`

func TestParseUFWRules(t *testing.T) {
	t.Parallel()
	in := func(action, target string) ufwRule { return ufwRule{false, action, target} }
	want := []ufwRule{
		in("limit", "22/tcp"), in("allow", "from 10.0.0.0/8 to any port 5432 proto tcp"), in("allow", "8000:8010/udp"),
		in("allow", "OpenSSH"), in("allow", "25/tcp"), in("allow", "in on eth0 to any port 99"), in("deny", "out 25"),
		{true, "allow", "in on eth0 out on eth1"}, in("allow", "in on incusbr0"), {true, "allow", "443/tcp"},
		in("allow", "from 2001:db8::/32 to any port 443 proto tcp"),
		in("allow", "from 192.0.2.0/24"), in("reject", "from 198.51.100.7 proto udp"),
	}
	if got := parseUFWRules(ufwShowAdded); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if got := parseUFWRules("Added user rules (see 'ufw status' for running firewall):\n(None)\n"); got != nil {
		t.Fatal(got)
	}
}

func TestPlanUFW(t *testing.T) {
	t.Parallel()
	state := ufwState{active: true, incoming: "deny", outgoing: "allow", routed: "deny", rules: parseUFWRules(ufwShowAdded)}
	rule := func(action, port, proto, from string) UFWRule {
		return UFWRule{Action: action, Port: PortOrRange(port), Proto: proto, From: from}
	}
	plan := func(u UFW) ufwPlan {
		desired, err := ufwDesiredRules(u)
		if err != nil {
			t.Fatal(err)
		}
		return planUFW(u, desired, state)
	}
	yes := true
	for _, tc := range []struct {
		name    string
		u       UFW
		changes map[string]any
	}{
		{"unchanged", UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{rule("limit", "22", "tcp", ""), rule("allow", "25", "tcp", "")}, DefaultIncoming: "deny", Enabled: &yes}, map[string]any{}},
		{"add and update", UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{rule("allow", "443", "tcp", ""), rule("allow", "22", "tcp", "")}},
			map[string]any{"added": []any{"allow 443/tcp"}, "updated": []any{"allow 22/tcp"}}},
		{"replace removes other incoming rules only", UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{rule("limit", "22", "tcp", ""), rule("allow", "8000:8010", "udp", "")}},
			map[string]any{"removed": []any{"allow from 10.0.0.0/8 to any port 5432 proto tcp", "allow OpenSSH", "allow 25/tcp", "allow in on eth0 to any port 99", "allow in on incusbr0",
				"allow from 2001:db8::/32 to any port 443 proto tcp", "allow from 192.0.2.0/24", "reject from 198.51.100.7 proto udp"}}},
		{"interface rules", UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Action: "allow", IncomingInterface: "incusbr0"}, {Action: "allow", IncomingInterface: "eth1", Port: "53", Proto: "udp"}}},
			map[string]any{"added": []any{"allow in on incusbr0 proto tcp", "allow in on incusbr0 proto udp", "allow in on eth1 to any port 53 proto udp"}}},
		{"routed rules are their own set", UFW{RoutedRulesNotListed: "remove", RoutedRules: []UFWRoutedRule{
			{Proto: "tcp_and_udp", Action: "allow", IncomingInterface: "incusbr0"}, {Proto: "tcp_and_udp", Action: "allow", OutgoingInterface: "incusbr0"}, {Action: "deny", Port: "443", Proto: "tcp"}}},
			map[string]any{"added": []any{"route allow in on incusbr0 proto tcp", "route allow in on incusbr0 proto udp", "route allow out on incusbr0 proto tcp", "route allow out on incusbr0 proto udp"}, "updated": []any{"route deny 443/tcp"},
				"removed": []any{"route allow in on eth0 out on eth1"}, "reordered_rules": []any{"route allow in on incusbr0 proto tcp", "route allow in on incusbr0 proto udp", "route allow out on incusbr0 proto tcp", "route allow out on incusbr0 proto udp", "route deny 443/tcp"}}},
		{"an incoming and a routed rule with the same target are two rules", UFW{RoutedRulesNotListed: "keep", RoutedRules: []UFWRoutedRule{{Action: "limit", Port: "22", Proto: "tcp"}}},
			map[string]any{"added": []any{"route limit 22/tcp"}}},
		{"absent routed rules", UFW{State: "absent", RoutedRules: []UFWRoutedRule{{Proto: "tcp", Port: "443"}}, IncomingRules: []UFWRule{{Proto: "tcp", Port: "25"}}},
			map[string]any{"removed": []any{"allow 25/tcp", "route allow 443/tcp"}}},
		{"rules without a port", UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{rule("allow", "", "udp", "192.0.2.0/24"), rule("deny", "", "udp", "198.51.100.7"), rule("allow", "", "tcp", "192.0.2.0/24")}},
			map[string]any{"added": []any{"allow from 192.0.2.0/24 proto udp", "allow from 192.0.2.0/24 proto tcp"}, "updated": []any{"deny from 198.51.100.7 proto udp"}}},
		{"absent whatever the action", UFW{State: "absent", IncomingRules: []UFWRule{{Port: "5432", Proto: "tcp", From: "10.0.0.0/8"}, {Port: "9", Proto: "tcp"}}},
			map[string]any{"removed": []any{"allow from 10.0.0.0/8 to any port 5432 proto tcp"}}},
		{"settings", UFW{DefaultIncoming: "reject", DefaultOutgoing: "deny", DefaultRouted: "allow", Enabled: new(bool)},
			map[string]any{"default_incoming": "reject", "default_outgoing": "deny", "default_routed": "allow", "enabled": false}},
		{"routed default unchanged", UFW{DefaultRouted: "deny"}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plan(tc.u).changes; !reflect.DeepEqual(got, tc.changes) {
				t.Fatalf("%#v", got)
			}
		})
	}
}

func TestUFWTCPPort(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]bool{
		"22/tcp": true, "22": true, "22/udp": false, "2222/tcp": false, "20:30/tcp": true, "23:30/tcp": false,
		"from 10.0.0.0/8 to any port 22 proto tcp": true, "from 10.0.0.0/8 to any port 22": true,
		"from 10.0.0.0/8 to any port 22 proto udp": false, "OpenSSH": false, "in on eth0 to any port 22": true,
		// A rule without a port names no port, so it never counts for SSH.
		"from 10.0.0.0/8": false, "from 10.0.0.0/8 proto tcp": false, "from 10.0.0.0/8 proto udp": false,
	} {
		if covers := ufwTCPPort(target, 22); covers != want {
			t.Error(target, covers)
		}
	}
}

// ufw applies the first rule that matches: the safeguard reads the resulting
// rules in order, and only a rule that names sshd's port counts.
func TestUFWSafeguardFollowsRuleOrder(t *testing.T) {
	ufwFixture(t, "active")
	rule := func(action, target string) ufwRule { return ufwRule{false, action, target} }
	for _, tc := range []struct {
		name    string
		rules   []ufwRule
		task    UFW
		refused string
	}{
		{"a source on every port is no SSH rule", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Action: "allow", From: "192.0.2.50"}, {Action: "allow", Port: "443", Proto: "tcp"}}},
			"A rule without a port does not count"},
		{"an interface without the node address is no SSH rule", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Action: "allow", IncomingInterface: "eth1", Port: "22", Proto: "tcp"}}},
			"which does not hold"},
		{"a routed rule is no SSH rule", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{}, RoutedRulesNotListed: "keep", RoutedRules: []UFWRoutedRule{{Action: "allow", Port: "22", Proto: "tcp"}}},
			"no rule allowing or limiting"},
		{"a listed outgoing allow is no SSH rule", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{}, OutgoingRulesNotListed: "keep", OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "22", Proto: "tcp"}}}, "no rule allowing or limiting"},
		{"an existing outgoing allow is no SSH rule", []ufwRule{rule("allow", "out 22/tcp")},
			UFW{OutgoingRulesNotListed: "keep", OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "443", Proto: "tcp"}}}, "no rule allowing or limiting"},
		{"an outgoing deny does not block SSH", []ufwRule{rule("deny", "out 22/tcp"), rule("allow", "22/tcp")},
			UFW{OutgoingRulesNotListed: "keep", OutgoingRules: []UFWOutgoingRule{{Action: "reject", Port: "22", Proto: "tcp"}}}, ""},
		{"a routed deny does not block SSH", []ufwRule{rule("allow", "22/tcp")},
			UFW{RoutedRulesNotListed: "keep", RoutedRules: []UFWRoutedRule{{Action: "deny", Port: "22", Proto: "tcp"}}}, ""},
		{"a port rule from one network counts", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Action: "allow", Port: "22", Proto: "tcp", From: "10.0.0.0/8"}}}, ""},
		{"a deny before the allow blocks SSH", []ufwRule{rule("deny", "22"), rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Action: "allow", Port: "443", Proto: "tcp"}}},
			`The ufw rule "deny 22" blocks sshd's TCP port 22 before any rule that would allow SSH`},
		{"a deny of a range before the allow blocks SSH", []ufwRule{rule("allow", "443/tcp")},
			UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Action: "reject", Port: "1:1024", Proto: "tcp"}, {Action: "allow", Port: "22", Proto: "tcp"}}},
			`"reject 1:1024/tcp"`},
		{"a deny after the allow does not", []ufwRule{rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Proto: "udp", Action: "deny", Port: "22"}}}, ""},
		{"a deny from one address before the allow does not", []ufwRule{rule("deny", "from 192.0.2.9 to any port 22"), rule("allow", "22/tcp")},
			UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Action: "deny", From: "198.51.100.7"}}}, ""},
		{"an allow changed in place to deny blocks SSH", []ufwRule{rule("allow", "22/tcp"), rule("allow", "443/tcp")},
			UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Action: "deny", Port: "22", Proto: "tcp"}}}, `"deny 22/tcp"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := ufwState{active: true, incoming: "deny", rules: tc.rules}
			desired, err := ufwDesiredRules(tc.task)
			if err != nil {
				t.Fatal(err)
			}
			err = (Runtime{ControllerConnection: ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1"}}).ufwSafeguard(context.Background(), planUFW(tc.task, desired, state), state, "10.20.0.2")
			if (tc.refused == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.refused)) {
				t.Fatal(err)
			}
		})
	}
}

// A fake ufw records its calls and keeps its logging level, low at first, in
// ufw.conf as ufw does; sshd -T reports port 22.
func ufwFixture(t *testing.T, status string) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	configuration := filepath.Join(bin, "ufw.conf")
	if err := os.WriteFile(configuration, []byte("ENABLED=no\nLOGLEVEL=low\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previousConfiguration := ufwConfiguration
	ufwConfiguration = configuration
	t.Cleanup(func() { ufwConfiguration = previousConfiguration })
	scripts := map[string]string{
		"ufw": `echo "$*" >> ` + calls + `
case "$*" in
 status) echo 'Status: ` + status + `';;
 "show added") printf "Added user rules (see 'ufw status' for running firewall):\nufw allow 443/tcp\n";;
 "logging "*) printf 'ENABLED=no\nLOGLEVEL=%s\n' "$2" > ` + configuration + `;;
esac`,
		"sshd": `printf 'port 22\nlistenaddress [::]:22\nlistenaddress 0.0.0.0:22\n'`,
	}
	for name, script := range scripts {
		writeStandInProgram(t, filepath.Join(bin, name), "#!/bin/sh\n"+script+"\n")
	}
	defaults := filepath.Join(bin, "defaults")
	if err := os.WriteFile(defaults, []byte("IPV6=no\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := ufwDefaults
	ufwDefaults = defaults
	t.Cleanup(func() { ufwDefaults = previous })
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestUFWSafeguardKeepsSSHReachable(t *testing.T) {
	ctx := context.Background()
	yes := true
	calls := ufwFixture(t, "inactive")
	enable := UFW{Enabled: &yes}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).UFW(ctx, enable, "")
		if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "sshd's TCP port 22") {
			t.Fatalf("%+v", r)
		}
	}
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "enable") {
		t.Fatalf("changed ufw despite the safeguard: %s", b)
	}
	enable.IncomingRules, enable.IncomingRulesNotListed = []UFWRule{{Action: "limit", Port: "22", Proto: "tcp"}}, "keep"
	preview := (Runtime{Dryrun: true}).UFW(ctx, enable, "")
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, map[string]any{"added": []any{"limit 22/tcp"}, "enabled": true}) {
		t.Fatalf("%+v", preview)
	}
	// Enabled with an open default, or adding rules while off, needs no SSH rule.
	for _, u := range []UFW{{Enabled: &yes, DefaultIncoming: "allow"}, {IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Action: "allow", Port: "80"}}}} {
		if r := (Runtime{Dryrun: true}).UFW(ctx, u, ""); r.Status != "succeeded" {
			t.Fatalf("%+v", r)
		}
	}

	// Removing the rule SSH depends on from an active firewall is refused too.
	ufwFixture(t, "active")
	for _, u := range []UFW{
		{State: "absent", IncomingRules: []UFWRule{{Port: "443", Proto: "tcp"}}},
		{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{}},
	} {
		if r := (Runtime{Dryrun: true}).UFW(ctx, u, ""); r.Status != "failed" || !strings.Contains(r.Reason, "sshd") {
			t.Fatalf("%+v", r)
		}
	}
}

func TestUFWNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	u := UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Action: "allow", Port: "80"}}}
	if r := (Runtime{Dryrun: true}).UFW(context.Background(), u, ""); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).UFW(context.Background(), u, ""); r.Status != "failed" || !strings.Contains(r.Reason, "not installed") {
		t.Fatalf("%+v", r)
	}
}

// Native normalized forms with explicitly selected protocols.
const ufwOutgoingShowAdded = `Added user rules (see 'ufw status' for running firewall):
ufw allow out 53/udp
ufw allow out 53/tcp
ufw allow out 443/tcp
ufw allow out 8000:8100/tcp
ufw allow out to 10.0.0.5 port 5432 proto tcp
ufw allow out to 10.1.0.0/16 proto tcp
ufw deny out 25/tcp
ufw allow out on eth0 to any port 123 proto udp
ufw reject out on eth1 proto tcp
ufw allow out on eth0 to 10.2.0.0/24 proto tcp
ufw limit out 2222/tcp
ufw allow in on eth1 to any port 22 proto tcp
ufw allow 80/tcp
ufw route allow in on incusbr0
`

func TestUFWOutgoingMeasuredRules(t *testing.T) {
	t.Parallel()
	outgoing := []UFWOutgoingRule{
		{Action: "allow", Port: "53", Proto: "udp"},
		{Proto: "tcp", Action: "allow", Port: "53"},
		{Action: "allow", Port: "443", Proto: "tcp"},
		{Action: "allow", Port: "8000:8100", Proto: "tcp"},
		{Action: "allow", To: "10.0.0.5", Port: "5432", Proto: "tcp"},
		{Proto: "tcp", Action: "allow", To: "10.1.0.0/16"},
		{Action: "deny", Port: "25", Proto: "tcp"},
		{Action: "allow", OutgoingInterface: "eth0", Port: "123", Proto: "udp"},
		{Proto: "tcp", Action: "reject", OutgoingInterface: "eth1"},
		{Proto: "tcp", Action: "allow", OutgoingInterface: "eth0", To: "10.2.0.0/24"},
		{Action: "limit", Port: "2222", Proto: "tcp"},
	}
	state := ufwState{rules: parseUFWRules(ufwOutgoingShowAdded)}
	if len(state.rules) != 14 {
		t.Fatal(state.rules)
	}
	for i, r := range outgoing {
		target, err := r.Target()
		want := ufwRule{false, r.Action, target}
		if err != nil || state.rules[i] != want {
			t.Fatalf("rule %d: %v, %v; want %v", i, state.rules[i], err, want)
		}
		if got := want.command(false); !reflect.DeepEqual(got, strings.Fields(want.String())) {
			t.Fatal(got)
		}
		if got := want.command(true); !reflect.DeepEqual(got, append([]string{"delete"}, strings.Fields(want.String())...)) {
			t.Fatal(got)
		}
	}
	wantIncomingAndRouted := []ufwRule{{false, "allow", "in on eth1 to any port 22 proto tcp"}, {false, "allow", "80/tcp"}, {true, "allow", "in on incusbr0"}}
	if !reflect.DeepEqual(state.rules[11:], wantIncomingAndRouted) {
		t.Fatal(state.rules[11:])
	}
	for _, choice := range []string{"keep", "remove"} {
		u := UFW{OutgoingRules: outgoing, OutgoingRulesNotListed: choice}
		desired, err := ufwDesiredRules(u)
		if err != nil || len(planUFW(u, desired, state).changes) != 0 {
			t.Fatal(err, planUFW(u, desired, state))
		}
	}
	for _, tc := range []struct {
		name    string
		u       UFW
		changes map[string]any
	}{
		{"keep adds and updates", UFW{OutgoingRulesNotListed: "keep", OutgoingRules: []UFWOutgoingRule{{Action: "reject", Port: "25", Proto: "tcp"}, {Action: "allow", Port: "54", Proto: "tcp"}}}, map[string]any{"updated": []any{"reject out 25/tcp"}, "added": []any{"allow out 54/tcp"}}},
		{"remove only outgoing", UFW{OutgoingRulesNotListed: "remove", OutgoingRules: outgoing[:10]}, map[string]any{"removed": []any{"limit out 2222/tcp"}}},
		{"empty removes all outgoing", UFW{OutgoingRulesNotListed: "remove", OutgoingRules: []UFWOutgoingRule{}}, map[string]any{"removed": []any{"allow out 53/udp", "allow out 53/tcp", "allow out 443/tcp", "allow out 8000:8100/tcp", "allow out to 10.0.0.5 port 5432 proto tcp", "allow out to 10.1.0.0/16 proto tcp", "deny out 25/tcp", "allow out on eth0 to any port 123 proto udp", "reject out on eth1 proto tcp", "allow out on eth0 to 10.2.0.0/24 proto tcp", "limit out 2222/tcp"}}},
		{"remove only incoming", UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{}}, map[string]any{"removed": []any{"allow in on eth1 to any port 22 proto tcp", "allow 80/tcp"}}},
		{"remove only routed", UFW{RoutedRulesNotListed: "remove", RoutedRules: []UFWRoutedRule{}}, map[string]any{"removed": []any{"route allow in on incusbr0"}}},
		{"absent regardless of action", UFW{State: "absent", OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "25", Proto: "tcp"}, {Proto: "tcp", OutgoingInterface: "eth1"}, {Proto: "tcp", To: "10.1.0.0/16"}}}, map[string]any{"removed": []any{"allow out to 10.1.0.0/16 proto tcp", "deny out 25/tcp", "reject out on eth1 proto tcp"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateUFW(tc.u); err != nil {
				t.Fatal(err)
			}
			desired, err := ufwDesiredRules(tc.u)
			if err != nil {
				t.Fatal(err)
			}
			if got := planUFW(tc.u, desired, state).changes; !reflect.DeepEqual(got, tc.changes) {
				t.Fatalf("%#v", got)
			}
		})
	}
}

func TestValidateUFWOutgoingRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		u    UFW
		want string
	}{
		{"missing policy", UFW{OutgoingRules: []UFWOutgoingRule{}}, "outgoing_rules_not_listed is required"},
		{"policy without list", UFW{DefaultOutgoing: "allow", OutgoingRulesNotListed: "remove"}, "requires outgoing_rules"},
		{"invalid policy", UFW{OutgoingRules: []UFWOutgoingRule{}, OutgoingRulesNotListed: "merge"}, "must be remove or keep"},
		{"empty rule", UFW{OutgoingRules: []UFWOutgoingRule{{Proto: "tcp_and_udp", Action: "allow"}}, OutgoingRulesNotListed: "keep"}, "needs port, to, outgoing_interface"},
		{"invalid action", UFW{OutgoingRules: []UFWOutgoingRule{{Proto: "tcp_and_udp", Action: "permit", Port: "53"}}, OutgoingRulesNotListed: "keep"}, "action must be"},
		{"invalid protocol", UFW{OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "53", Proto: "icmp"}}, OutgoingRulesNotListed: "keep"}, "protocol must be"},
		{"duplicate identity with another action", UFW{OutgoingRules: []UFWOutgoingRule{{Proto: "tcp_and_udp", Action: "allow", Port: "53"}, {Proto: "tcp_and_udp", Action: "deny", Port: "053"}}, OutgoingRulesNotListed: "keep"}, "outgoing rule listed twice: out 53"},
		{"absent with policy", UFW{State: "absent", OutgoingRules: []UFWOutgoingRule{{Proto: "tcp_and_udp", Port: "53"}}, OutgoingRulesNotListed: "keep"}, "accepts only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateUFW(tc.u); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestUFWLoggingLevel(t *testing.T) {
	ctx := context.Background()
	calls := ufwFixture(t, "inactive")
	u := UFW{LoggingLevel: "medium"}
	preview := (Runtime{Dryrun: true}).UFW(ctx, u, "")
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, map[string]any{"logging_level": "medium"}) {
		t.Fatalf("%+v", preview)
	}
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "logging") {
		t.Fatalf("a dryrun changed the logging level: %s", b)
	}
	if r := (Runtime{}).UFW(ctx, u, ""); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"logging_level": "medium"}) {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(ufwConfiguration); !strings.Contains(string(b), "LOGLEVEL=medium") {
		t.Fatalf("%s", b)
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).UFW(ctx, u, ""); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("rerun, dryrun %v: %+v", dryrun, r)
		}
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "logging") != 1 {
		t.Fatalf("%s", b)
	}
}

func TestUFWWithIPv6EnabledChecksInstalledProgram(t *testing.T) {
	ctx := context.Background()
	calls := ufwFixture(t, "inactive")
	if err := os.WriteFile(ufwDefaults, []byte("IPV6=yes\nDEFAULT_INPUT_POLICY=DROP\nDEFAULT_OUTPUT_POLICY=ACCEPT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pythonModules := t.TempDir()
	packageDirectory := filepath.Join(pythonModules, "ufw")
	if err := os.Mkdir(packageDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	// The packaged backend checks the invoking program with os.stat. Exercise
	// that check through Python, including dryrun and post-change inspection.
	for name, source := range map[string]string{
		"__init__.py": "",
		"backend_iptables.py": `import os
import sys

class UFWBackendIptables:
    def __init__(self, dryrun):
        os.stat(sys.argv[0])
        if not os.path.samefile(sys.argv[0], os.environ["UFW_TEST_PROGRAM"]):
            raise RuntimeError("backend must check the installed ufw program")

    def use_ipv6(self):
        return True

    def get_rules_ipv6(self):
        return []
`,
		"parser.py": "class UFWCommandRule: pass\nclass UFWCommandRouteRule: pass\n",
	} {
		if err := os.WriteFile(filepath.Join(packageDirectory, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PYTHONPATH", pythonModules)
	t.Setenv("UFW_TEST_PROGRAM", filepath.Join(filepath.Dir(calls), "ufw"))
	u := UFW{LoggingLevel: "medium"}
	for _, tc := range []struct {
		dryrun bool
		change string
	}{
		{true, "would_change"},
		{false, "changed"},
		{false, "unchanged"},
		{true, "unchanged"},
	} {
		if r := (Runtime{Dryrun: tc.dryrun}).UFW(ctx, u, ""); r.Status != "succeeded" || r.Change != tc.change {
			t.Fatalf("dryrun %v, want %s: %+v", tc.dryrun, tc.change, r)
		}
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "logging") != 1 {
		t.Fatalf("expected exactly one logging change: %s", b)
	}
}

func TestValidateUFWLoggingLevel(t *testing.T) {
	t.Parallel()
	for _, level := range ufwLogging {
		if err := ValidateUFW(UFW{LoggingLevel: level}); err != nil {
			t.Fatal(level, err)
		}
	}
	for _, tc := range []struct {
		u    UFW
		want string
	}{
		{UFW{LoggingLevel: "on"}, "logging_level must be off, low, medium, high, or full"},
		{UFW{State: "absent", LoggingLevel: "low", IncomingRules: []UFWRule{{Proto: "tcp_and_udp", Port: "22"}}}, "accepts only"},
	} {
		if err := ValidateUFW(tc.u); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: got %v, want %s", tc.u, err, tc.want)
		}
	}
}

func TestUFWRequiresProtocolForEveryRuleKind(t *testing.T) {
	t.Parallel()
	for _, input := range []UFW{
		{IncomingRules: []UFWRule{{Action: "allow", Port: "80"}}, IncomingRulesNotListed: "keep"},
		{RoutedRules: []UFWRoutedRule{{Action: "allow", Port: "80"}}, RoutedRulesNotListed: "keep"},
		{OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "80"}}, OutgoingRulesNotListed: "keep"},
	} {
		if err := ValidateUFW(input); err == nil || !strings.Contains(err.Error(), "protocol is required: write tcp, udp, or tcp_and_udp") {
			t.Fatal(err)
		}
	}
	input := UFW{IncomingRules: []UFWRule{{Action: "allow", Port: "8000:8010", Proto: "tcp_and_udp"}}, IncomingRulesNotListed: "keep"}
	if err := ValidateUFW(input); err != nil {
		t.Fatal(err)
	}
	rules, err := ufwDesiredRules(input)
	want := []ufwRule{{false, "allow", "8000:8010/tcp"}, {false, "allow", "8000:8010/udp"}}
	if err != nil || !reflect.DeepEqual(rules, want) {
		t.Fatal(rules, err)
	}
}

func TestUFWOwnedRuleOrderChangesWhileSSHStaysAllowed(t *testing.T) {
	calls := ufwFixture(t, "active")
	root := filepath.Dir(calls)
	path := filepath.Join(root, "rules")
	before := "ufw allow 22/tcp\nufw allow 80/tcp\nufw allow 443/tcp\nufw route allow 80/tcp\nufw route allow 443/tcp\nufw allow out 80/tcp\nufw allow out 443/tcp\n"
	writeState(t, path, before)
	setWorkerEnvironment(t, "UFW_TEST_RULES", path)
	writeStandInProgram(t, filepath.Join(root, "ufw"), `#!/bin/sh
exec /usr/bin/python3 - "$@" <<'PYTHON'
import os, sys
from pathlib import Path
path = Path(os.environ['UFW_TEST_RULES'])
rules = path.read_text().splitlines()
args = sys.argv[1:]
if args == ['status']:
    print('Status: active')
elif args == ['show', 'added']:
    print('\n'.join(rules))
else:
    routed = args[0] == 'route'
    if routed: args = args[1:]
    operation = args.pop(0) if args[0] in ('delete', 'prepend') else 'append'
    rule = 'ufw ' + ('route ' if routed else '') + ' '.join(args)
    if operation == 'delete':
        rules = [line for line in rules if line != rule]
    elif rule not in rules:
        if operation == 'prepend': rules.insert(0, rule)
        else: rules.append(rule)
    if 'ufw allow 22/tcp' not in rules:
        raise SystemExit('SSH allowance disappeared during reordering')
    path.write_text('\n'.join(rules) + '\n')
PYTHON
`)
	input := UFW{
		IncomingRules: []UFWRule{{Action: "allow", Port: "443", Proto: "tcp"}, {Action: "allow", Port: "22", Proto: "tcp"}, {Action: "allow", Port: "80", Proto: "tcp"}}, IncomingRulesNotListed: "remove",
		RoutedRules: []UFWRoutedRule{{Action: "allow", Port: "443", Proto: "tcp"}, {Action: "allow", Port: "80", Proto: "tcp"}}, RoutedRulesNotListed: "remove",
		OutgoingRules: []UFWOutgoingRule{{Action: "allow", Port: "443", Proto: "tcp"}, {Action: "allow", Port: "80", Proto: "tcp"}}, OutgoingRulesNotListed: "remove",
	}
	preview := (Runtime{Dryrun: true}).UFW(testContext(t), input, "")
	if preview.Change != "would_change" {
		t.Fatal(preview)
	}
	if readState(t, path) != before {
		t.Fatal("dryrun reordered rules")
	}
	// A run that works reports exactly what the dryrun listed.
	if result := (Runtime{}).UFW(testContext(t), input, ""); result.Status != "succeeded" || result.Change != "changed" || !reflect.DeepEqual(result.Value, preview.Value) {
		t.Fatalf("run %v, dryrun %v", result.Value, preview.Value)
	}
	if got := readState(t, path); got != "ufw allow 443/tcp\nufw allow 22/tcp\nufw allow 80/tcp\nufw route allow 443/tcp\nufw route allow 80/tcp\nufw allow out 443/tcp\nufw allow out 80/tcp\n" {
		t.Fatal(got)
	}
	if result := (Runtime{}).UFW(testContext(t), input, ""); result.Change != "unchanged" {
		t.Fatal(result)
	}
	input.IncomingRulesNotListed = "keep"
	input.IncomingRules[0], input.IncomingRules[2] = input.IncomingRules[2], input.IncomingRules[0]
	if result := (Runtime{}).UFW(testContext(t), input, ""); result.Change != "unchanged" {
		t.Fatal(result)
	}
	input.IncomingRules = append(input.IncomingRules, UFWRule{Action: "allow", Port: "53", Proto: "udp"})
	if result := (Runtime{}).UFW(testContext(t), input, ""); result.Status != "succeeded" {
		t.Fatal(result)
	}
	if !strings.HasSuffix(readState(t, path), "ufw allow 53/udp\n") {
		t.Fatal(readState(t, path))
	}
}

func TestUFWOrderUsesEachIPFamilyRatherThanShowAddedOrder(t *testing.T) {
	t.Parallel()
	input := UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{
		{Action: "deny", From: "2001:db8::/32", Port: "443", Proto: "tcp"},
		{Action: "allow", Port: "22", Proto: "tcp"},
		{Action: "allow", Port: "443", Proto: "tcp"},
	}}
	// show added always prints IPv4/dual-family rules before IPv6-only rules.
	state := ufwState{ipv6Enabled: true,
		rules:     parseUFWRules("ufw allow 22/tcp\nufw allow 443/tcp\nufw deny from 2001:db8::/32 to any port 443 proto tcp\n"),
		ipv6Rules: parseUFWRules("ufw allow 22/tcp\nufw allow 443/tcp\nufw deny from 2001:db8::/32 to any port 443 proto tcp\n"),
	}
	desired, err := ufwDesiredRules(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan := planUFW(input, desired, state); len(plan.order) != 1 {
		t.Fatal(plan)
	}
	state.ipv6Rules = parseUFWRules("ufw deny from 2001:db8::/32 to any port 443 proto tcp\nufw allow 22/tcp\nufw allow 443/tcp\n")
	if plan := planUFW(input, desired, state); len(plan.changes) != 0 {
		t.Fatal(plan)
	}
}

// On Ubuntu 24.04 and later, ssh.socket starts ssh.service on the first
// connection, and systemd removes sshd's privilege separation directory when
// the service stops, such as when an update of openssh-server restarts it.
// sshd -T then fails until the directory exists again; MMX creates it, as
// the service would, for the node HTTPS API's authorization and for the
// ufw and firewalld modules' check of sshd's ports.
func TestSSHDSettingsCreatesAMissingPrivilegeSeparationDirectory(t *testing.T) {
	dir := t.TempDir()
	privilegeSeparationDirectory := filepath.Join(dir, "run-sshd")
	program := filepath.Join(dir, "sshd")
	script := "#!/bin/sh\nif [ ! -d " + privilegeSeparationDirectory + " ]; then echo 'Missing privilege separation directory: " + privilegeSeparationDirectory + "' >&2; exit 255; fi\necho 'pubkeyauthentication yes'\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(directory string) { DebianSSHDPrivilegeSeparationDirectory = directory }(DebianSSHDPrivilegeSeparationDirectory)
	DebianSSHDPrivilegeSeparationDirectory = privilegeSeparationDirectory
	out, err := SSHDSettings(context.Background(), program)
	if err != nil || out != "pubkeyauthentication yes\n" {
		t.Fatal(out, err)
	}
	// Any other failure is reported with sshd's own words, and nothing is created.
	DebianSSHDPrivilegeSeparationDirectory = filepath.Join(dir, "other")
	if err := os.WriteFile(program, []byte("#!/bin/sh\necho 'sshd_config line 3: Bad configuration option: Foo' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SSHDSettings(context.Background(), program); err == nil || !strings.Contains(err.Error(), "Bad configuration option: Foo") {
		t.Fatal(err)
	}
	if _, err := os.Stat(DebianSSHDPrivilegeSeparationDirectory); !os.IsNotExist(err) {
		t.Fatal("a directory was created for another failure", err)
	}
}

func TestUFWSafeguardAdmitsOnlyTheControllerConnection(t *testing.T) {
	ufwFixture(t, "active")
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	loopback := ""
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if prefix, err := netip.ParsePrefix(address.String()); err == nil && prefix.Addr().String() == "127.0.0.1" {
				loopback = networkInterface.Name
			}
		}
	}
	if loopback == "" {
		t.Fatal("the kernel reports no interface holding 127.0.0.1")
	}
	allow := ufwRule{action: "allow", target: "22/tcp"}
	onLoopback := "in on " + loopback + " from 10.20.0.1 to any port 22 proto tcp"
	for _, test := range []struct {
		name, source, nodeAddress string
		rules                     []ufwRule
		refusal                   string
	}{
		{"unrestricted", "10.20.0.1", "", []ufwRule{allow}, ""},
		{"wrong port", "10.20.0.1", "", []ufwRule{{action: "allow", target: "2222/tcp"}}, "no rule allowing"},
		{"UDP", "10.20.0.1", "", []ufwRule{{action: "allow", target: "22/udp"}}, "no rule allowing"},
		{"exact source", "10.20.0.1", "", []ufwRule{{action: "allow", target: "from 10.20.0.1 to any port 22 proto tcp"}}, ""},
		{"source network", "10.20.0.1", "", []ufwRule{{action: "limit", target: "from 10.20.0.0/24 to any port 20:30 proto tcp"}}, ""},
		{"laptops exclude administration server", "10.20.0.1", "", []ufwRule{{action: "allow", target: "from 10.20.9.0/24 to any port 22 proto tcp"}}, "only from addresses that do not include"},
		{"laptops then administration server", "10.20.0.1", "", []ufwRule{{action: "allow", target: "from 10.20.9.0/24 to any port 22 proto tcp"}, {action: "allow", target: "from 10.20.0.1 to any port 22 proto tcp"}}, ""},
		{"IPv6 against IPv4", "2001:db8::1", "", []ufwRule{{action: "allow", target: "from 10.20.0.0/24 to any port 22 proto tcp"}}, "only from addresses that do not include"},
		{"IPv6 network", "2001:db8::1", "", []ufwRule{{action: "allow", target: "from 2001:db8::/64 to any port 22 proto tcp"}}, ""},
		{"mapped IPv4", "::ffff:10.20.0.1", "", []ufwRule{{action: "allow", target: "from 10.20.0.0/24 to any port 22 proto tcp"}}, ""},
		{"unknown source", "", "", []ufwRule{{action: "allow", target: "from 10.20.0.0/24 to any port 22 proto tcp"}}, "MMX cannot check that source"},
		{"unknown source unrestricted", "", "", []ufwRule{allow}, ""},
		{"interface holds address", "10.20.0.1", "127.0.0.1", []ufwRule{{action: "allow", target: onLoopback}}, ""},
		{"interface without source", "10.20.0.1", "127.0.0.1", []ufwRule{{action: "limit", target: "in on " + loopback + " to any port 22 proto tcp"}}, ""},
		{"wrong interface", "10.20.0.1", "192.0.2.99", []ufwRule{{action: "allow", target: onLoopback}}, "which does not hold"},
		{"missing interface", "10.20.0.1", "127.0.0.1", []ufwRule{{action: "allow", target: "in on mmxmissing0 to any port 22 proto tcp"}}, "which does not hold"},
		{"hostname inventory address", "10.20.0.1", "localhost", []ufwRule{{action: "allow", target: onLoopback}}, "is a host name"},
		{"source first deny", "10.20.0.1", "", []ufwRule{{action: "deny", target: "from 10.20.0.1 to any port 22 proto tcp"}, allow}, "before any rule that would allow SSH"},
		{"source first reject network", "10.20.0.1", "", []ufwRule{{action: "reject", target: "from 10.20.0.0/24 to any port 22 proto tcp"}, allow}, "before any rule that would allow SSH"},
		{"deny other source", "10.20.0.1", "", []ufwRule{{action: "deny", target: "from 10.20.9.0/24 to any port 22 proto tcp"}, allow}, ""},
		{"deny all sources first", "10.20.0.1", "", []ufwRule{{action: "deny", target: "22/tcp"}, allow}, "before any rule that would allow SSH"},
		{"deny after allow", "10.20.0.1", "", []ufwRule{allow, {action: "deny", target: "22/tcp"}}, ""},
		{"deny matching interface", "10.20.0.1", "127.0.0.1", []ufwRule{{action: "deny", target: onLoopback}, allow}, "before any rule that would allow SSH"},
		{"deny other interface", "10.20.0.1", "192.0.2.99", []ufwRule{{action: "deny", target: onLoopback}, allow}, ""},
		{"deny matching interface excludes source", "10.20.0.2", "127.0.0.1", []ufwRule{{action: "reject", target: onLoopback}, allow}, ""},
		// A deny or reject without a port covers every port, sshd's too.
		{"deny matching interface without a port", "10.20.0.1", "127.0.0.1", []ufwRule{{action: "deny", target: "in on " + loopback + " proto tcp"}, allow}, "before any rule that would allow SSH"},
		{"reject from the controller without a port", "10.20.0.1", "", []ufwRule{{action: "reject", target: "from 10.20.0.0/24"}, allow}, "before any rule that would allow SSH"},
		{"deny other interface without a port", "10.20.0.1", "192.0.2.99", []ufwRule{{action: "deny", target: "in on " + loopback}, allow}, ""},
		{"deny other source without a port", "10.20.0.1", "", []ufwRule{{action: "deny", target: "from 10.20.9.0/24"}, allow}, ""},
		{"deny UDP from the controller without a port", "10.20.0.1", "", []ufwRule{{action: "deny", target: "from 10.20.0.1 proto udp"}, allow}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (Runtime{ControllerConnection: ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: test.source}}).ufwSafeguard(context.Background(), ufwPlan{}, ufwState{active: true, incoming: "deny", rules: test.rules}, test.nodeAddress)
			if test.refusal == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.refusal) || !strings.Contains(err.Error(), "Add an incoming rule such as {action: allow, port: 22, protocol: tcp") || !strings.HasSuffix(err.Error(), ".") {
				t.Fatal(err)
			}
			if test.source == "" {
				if !strings.Contains(err.Error(), "MMX does not know the address") || !strings.Contains(err.Error(), "--connection ssh_only") {
					t.Fatal(err)
				}
			} else if !strings.Contains(err.Error(), test.source) {
				t.Fatal(err)
			}
		})
	}
}

func TestUFWReadsOwnInventoryAddressAndExpandedSources(t *testing.T) {
	ufwFixture(t, "inactive")
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	loopback := ""
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagLoopback != 0 {
			loopback = networkInterface.Name
			break
		}
	}
	inventory := map[string]any{
		"nodes":  map[string]any{"server01": map[string]any{"address": "127.0.0.1"}, "admin01": map[string]any{"address": "10.20.0.1"}},
		"groups": map[string]any{"controllers": []any{"admin01"}},
	}
	yes := true
	input := UFW{Enabled: &yes, IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Action: "allow", Port: "22", Proto: "tcp", IncomingInterface: loopback, FromInventoryGroup: &FromInventoryGroup{Group: "controllers", UseNodeAddress: true}}}}
	if err := input.ExpandInventoryGroups(inventory); err != nil {
		t.Fatal(err)
	}
	task := NodeTask{Runtime: Runtime{Dryrun: true, ControllerConnection: ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1"}}, NodeName: "server01", Scope: value.Scope{"inventory": inventory}}
	if result := input.Run(context.Background(), task); result.Status != "succeeded" || result.Change != "would_change" {
		t.Fatal(result)
	}
	task.NodeName = "admin01"
	if result := input.Run(context.Background(), task); result.Status != "failed" || !strings.Contains(result.Reason, "which does not hold") {
		t.Fatal(result)
	}
	task.NodeName, task.Runtime.ControllerConnection.SourceAddress = "server01", "10.20.9.5"
	if result := input.Run(context.Background(), task); result.Status != "failed" || !strings.Contains(result.Reason, "only from addresses that do not include") {
		t.Fatal(result)
	}
}

func TestUFWReorderingKeepsTheControllerAllowanceInstalled(t *testing.T) {
	for _, bindInterface := range []bool{false, true} {
		t.Run(fmt.Sprint(bindInterface), func(t *testing.T) {
			calls := ufwFixture(t, "active")
			root := filepath.Dir(calls)
			path := filepath.Join(root, "rules")
			writeState(t, path, "ufw allow 22/tcp\nufw allow 443/tcp\n")
			setWorkerEnvironment(t, "UFW_TEST_RULES", path)
			controllerRule := UFWRule{Action: "allow", Port: "22", Proto: "tcp", From: "10.20.0.1"}
			if bindInterface {
				interfaces, err := net.Interfaces()
				if err != nil {
					t.Fatal(err)
				}
				for _, networkInterface := range interfaces {
					if networkInterface.Flags&net.FlagLoopback != 0 {
						controllerRule.IncomingInterface = networkInterface.Name
						break
					}
				}
				if controllerRule.IncomingInterface == "" {
					t.Fatal("the kernel reports no loopback interface")
				}
			}
			controllerTarget, err := controllerRule.Target()
			if err != nil {
				t.Fatal(err)
			}
			setWorkerEnvironment(t, "UFW_TEST_CONTROLLER_RULE", "ufw allow "+controllerTarget)
			writeStandInProgram(t, filepath.Join(root, "ufw"), `#!/bin/sh
exec /usr/bin/python3 - "$@" <<'PYTHON'
import os, sys
from pathlib import Path
path = Path(os.environ['UFW_TEST_RULES'])
rules = path.read_text().splitlines()
args = sys.argv[1:]
if args == ['status']:
    print('Status: active')
elif args == ['show', 'added']:
    print('Added user rules:\n' + '\n'.join(rules))
else:
    operation = args.pop(0) if args[0] in ('delete', 'prepend') else 'append'
    rule = 'ufw ' + ' '.join(args)
    if operation == 'delete':
        rules = [line for line in rules if line != rule]
    elif rule not in rules:
        if operation == 'prepend': rules.insert(0, rule)
        else: rules.append(rule)
    if 'ufw allow 22/tcp' not in rules and os.environ['UFW_TEST_CONTROLLER_RULE'] not in rules:
        raise SystemExit('Controller SSH allowance disappeared during reordering')
    path.write_text('\n'.join(rules) + '\n')
PYTHON
`)
			input := UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{
				{Action: "allow", Port: "22", Proto: "tcp", From: "10.20.9.0/24"},
				controllerRule,
				{Action: "allow", Port: "443", Proto: "tcp"},
			}}
			result := (Runtime{ControllerConnection: ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1"}}).UFW(testContext(t), input, "127.0.0.1")
			if result.Status != "succeeded" || result.Change != "changed" {
				t.Fatal(result)
			}
			want := "ufw allow from 10.20.9.0/24 to any port 22 proto tcp\nufw allow " + controllerTarget + "\nufw allow 443/tcp\n"
			if got := readState(t, path); got != want {
				t.Fatal(got)
			}
		})
	}
}

func TestUFWSafeguardTrustsOnlyTheSSHPathSource(t *testing.T) {
	ufwFixture(t, "active")
	for _, test := range []struct {
		name       string
		connection ControllerConnection
		inventory  string
		sourceRule string
		unknown    string
	}{
		{"SSH", ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", ""},
		{"SSH different local address", ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.6"}, "10.0.0.5", "10.20.0.0/16", ""},
		{"SSH no local address", ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1"}, "server.example.com", "10.20.0.0/16", ""},
		{"HTTPS at SSH address", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", ""},
		{"HTTPS IPv6", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "2001:db8::1", NodeLocalAddress: "2001:db8::5"}, "2001:0db8:0:0::5", "2001:db8::/64", ""},
		{"HTTPS mapped local IP", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "::ffff:10.20.0.1", NodeLocalAddress: "::ffff:10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", ""},
		{"HTTPS loopback tunnel", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "127.0.0.1", NodeLocalAddress: "127.0.0.1"}, "10.0.0.5", "127.0.0.1", "the node job arrived over HTTPS at 127.0.0.1, not at the node's SSH address 10.0.0.5."},
		{"HTTPS WireGuard and SSH public", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.5"}, "203.0.113.5", "10.20.0.0/16", "the node job arrived over HTTPS at 10.20.0.5, not at the node's SSH address 203.0.113.5."},
		{"HTTPS no local address", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1"}, "10.0.0.5", "10.20.0.0/16", "the node job arrived over HTTPS, but the node's local address of the HTTPS connection was not recorded."},
		{"HTTPS inventory hostname", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}, "server.example.com", "10.20.0.0/16", "the node's inventory address \"server.example.com\" is a host name"},
		{"HTTPS inventory missing", ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}, "", "10.20.0.0/16", "the node's inventory address was not recorded."},
		{"SSH source missing", ControllerConnection{Kind: protocol.ConnectionSSH, NodeLocalAddress: "10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", "the IP address that the controller machine's connection came from was not recorded."},
		{"HTTPS source missing", ControllerConnection{Kind: protocol.ConnectionHTTPS, NodeLocalAddress: "10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", "the IP address that the controller machine's connection came from was not recorded."},
		{"connection kind missing", ControllerConnection{SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}, "10.0.0.5", "10.20.0.0/16", "the node job has no recorded SSH or HTTPS connection."},
		{"connection record missing", ControllerConnection{}, "10.0.0.5", "10.20.0.0/16", "the node job has no recorded SSH or HTTPS connection."},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := Runtime{ControllerConnection: test.connection}
			rule := ufwRule{action: "allow", target: "from " + test.sourceRule + " to any port 22 proto tcp"}
			state := ufwState{active: true, incoming: "deny", rules: []ufwRule{rule}}
			err := run.ufwSafeguard(context.Background(), ufwPlan{}, state, test.inventory)
			if test.unknown == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.unknown) || !strings.Contains(err.Error(), "MMX does not know the address that the controller machine's SSH comes from:") || !strings.Contains(err.Error(), "Run the command with --connection ssh_only, or use an incoming rule without from.") || !strings.HasSuffix(err.Error(), "Add an incoming rule such as {action: allow, port: 22, protocol: tcp}.") {
				t.Fatal(err)
			}
			// A rule without from does not need a known source. A deny with
			// from uses exactly the source check used for an allowance.
			rule.action = "deny"
			state.rules = []ufwRule{rule, {action: "allow", target: "22/tcp"}}
			err = run.ufwSafeguard(context.Background(), ufwPlan{}, state, test.inventory)
			if test.unknown == "" {
				if err == nil || !strings.Contains(err.Error(), "before any rule that would allow SSH") {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUFWRefusesAnHTTPSOnlyAllowanceBeforeMutation(t *testing.T) {
	ufwFixture(t, "active")
	input := UFW{IncomingRulesNotListed: "remove", IncomingRules: []UFWRule{{Action: "allow", Port: "22", Proto: "tcp", From: "10.20.0.0/16"}}}
	for _, dryrun := range []bool{false, true} {
		run := Runtime{
			Dryrun:               dryrun,
			ControllerConnection: ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.5"},
			BeforeMutation:       func(string) error { t.Fatal("a job with an unknown SSH source changed the firewall"); return nil },
		}
		result := run.UFW(context.Background(), input, "203.0.113.5")
		if result.Status != "failed" || result.FailureKind != "config" || !strings.Contains(result.Reason, "arrived over HTTPS at 10.20.0.5, not at the node's SSH address 203.0.113.5") {
			t.Fatal(result)
		}
	}
}

func TestUFWSafeguardHTTPSAtSSHAddressStillChecksTheSourceRule(t *testing.T) {
	ufwFixture(t, "active")
	run := Runtime{ControllerConnection: ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.0.0.5"}}
	err := run.ufwSafeguard(context.Background(), ufwPlan{}, ufwState{active: true, incoming: "deny", rules: []ufwRule{{action: "allow", target: "from 10.20.9.0/24 to any port 22 proto tcp"}}}, "10.0.0.5")
	if err == nil || !strings.Contains(err.Error(), "only from addresses that do not include 10.20.0.1, the address that the controller machine's connection came from.") || !strings.HasSuffix(err.Error(), "Add an incoming rule such as {action: allow, port: 22, protocol: tcp, from: 10.20.0.1}.") {
		t.Fatal(err)
	}
}

// A failed ufw task reports in its value only the changes whose ufw command
// worked: a node task in on_failure reads that value.
func TestFailedUFWTaskReportsOnlyWhatItDid(t *testing.T) {
	ctx := context.Background()
	calls := ufwFixture(t, "inactive")
	// ufw adds rules, and refuses every change of a default policy.
	writeStandInProgram(t, filepath.Join(filepath.Dir(calls), "ufw"), `#!/bin/sh
echo "$*" >> `+calls+`
case "$*" in
 status) echo 'Status: inactive';;
 "show added") printf "Added user rules (see 'ufw status' for running firewall):\nufw allow 443/tcp\n";;
 "default "*) echo 'ERROR: injected failure' >&2; exit 1;;
esac
`)
	u := UFW{IncomingRulesNotListed: "keep", IncomingRules: []UFWRule{{Action: "allow", Port: "80", Proto: "tcp"}}, DefaultOutgoing: "deny"}
	r := (Runtime{}).UFW(ctx, u, "")
	if r.Status != "failed" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"added": []any{"allow 80/tcp"}}) {
		t.Fatalf("a failed run reports %+v", r)
	}
	// The first command fails: nothing was done.
	u.IncomingRules = nil
	if r := (Runtime{}).UFW(ctx, u, ""); r.Status != "failed" || r.Value != nil {
		t.Fatalf("a failed run reports %+v", r)
	}
	// A preview lists what a run would change.
	u.IncomingRules = []UFWRule{{Action: "allow", Port: "80", Proto: "tcp"}}
	if r := (Runtime{Dryrun: true}).UFW(ctx, u, ""); r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"added": []any{"allow 80/tcp"}, "default_outgoing": "deny"}) {
		t.Fatalf("%+v", r)
	}
}
