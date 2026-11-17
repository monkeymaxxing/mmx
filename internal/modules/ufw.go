package modules

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// ufwDefaults holds ufw's default policies, which it reports only while active.
var ufwDefaults = "/etc/default/ufw"

// ufwConfiguration holds ufw's logging level (LOGLEVEL), which ufw status
// verbose also reports only while active.
var ufwConfiguration = "/etc/ufw/ufw.conf"

// ufwRule is one incoming, routed, or outgoing rule as ufw prints it (ufw show added):
// its action and its target, the rest of the rule without a comment.
type ufwRule struct {
	routed         bool
	action, target string
}

func (r ufwRule) String() string {
	if r.routed {
		return "route " + r.action + " " + r.target
	}
	return r.action + " " + r.target
}

// command is the rule as ufw's command line takes it, with delete for removal.
func (r ufwRule) command(delete bool) []string {
	var args []string
	if r.routed {
		args = append(args, "route")
	}
	if delete {
		args = append(args, "delete")
	}
	return append(append(args, r.action), strings.Fields(r.target)...)
}

type ufwState struct {
	active                     bool
	incoming, outgoing, routed string
	loggingLevel               string
	rules                      []ufwRule
	ipv6Enabled                bool
	ipv6Rules                  []ufwRule
}

// ufwPlan is what a ufw task changes: rules to add (ufw replaces a rule with
// the same target but another action in place), rules to remove, and settings.
type ufwPlan struct {
	add, remove                []ufwRule
	order                      [][]ufwRule
	incoming, outgoing, routed string
	loggingLevel               string
	enable                     *bool
	changes                    map[string]any
}

// UFW manages incoming, routed, and outgoing rules, default policies, and whether the
// firewall is on through the node's ufw command. Changes apply immediately.
// A rule with from_inventory_group arrives as one rule for each node of the
// group.
func (run Runtime) UFW(ctx context.Context, u UFW, nodeInventoryAddress string) (result protocol.Result) {
	if err := ValidateUFW(u); err != nil {
		return protocol.ConfigurationError(err)
	}
	desired, err := ufwDesiredRules(u)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	ufwProgram, err := lookPath(ctx, "ufw")
	if err != nil {
		return run.notYet(missingProgramError("ufw"))
	}
	state, err := inspectUFW(ctx, ufwProgram)
	if err != nil {
		return protocol.Failed(err)
	}
	plan := planUFW(u, desired, state)
	if len(plan.changes) == 0 {
		return protocol.Success("unchanged")
	}
	if err := run.ufwSafeguard(ctx, plan, state, nodeInventoryAddress); err != nil {
		return protocol.ConfigurationError(err)
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = plan.changes
		return r
	}
	if err := run.Mutation("ufw"); err != nil {
		return protocol.Failed(err)
	}
	// From here the value lists only the changes whose ufw command worked,
	// so that a failed task never reports a change that did not happen.
	planned := plan.changes
	done := map[string]any{}
	defer func() {
		switch {
		case result.Status == "succeeded":
			// The commands run in their own order, such as the retained SSH
			// rule first while a kind of rules is rebuilt. A run that worked
			// did all of the plan, so it reports the plan, as a dryrun does.
			result.Value = planned
		case len(done) > 0:
			result.Value = done
		}
	}()
	// doneSetting records a setting that a command changed.
	doneSetting := func(key string) { done[key] = planned[key] }
	// doneRule records a rule that a command added, updated, or removed.
	doneRule := func(kind string, r ufwRule) {
		list, _ := done[kind].([]any)
		if !slices.Contains(list, any(r.String())) {
			done[kind] = append(list, r.String())
		}
	}
	doneAddedRule := func(r ufwRule) {
		kind := "updated"
		if added, _ := planned["added"].([]any); slices.Contains(added, any(r.String())) {
			kind = "added"
		}
		doneRule(kind, r)
	}
	// doneRebuiltRule records a rule that the rebuild of a kind of rules
	// put back in its new place.
	doneRebuiltRule := func(r ufwRule) {
		doneRule("reordered_rules", r)
		if slices.Contains(plan.add, r) {
			doneAddedRule(r)
		}
	}
	change := "unknown"
	ufw := func(args ...string) error {
		r := Command(ctx, nil, "", "ufw", args...)
		if r.Status != "succeeded" {
			return fmt.Errorf("ufw %s: %s: %s%s", strings.Join(args, " "), r.Reason, r.Stderr, r.Stdout)
		}
		change = "changed"
		return nil
	}
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = change
		return r
	}
	// Apply a less restrictive incoming policy or disabling first. Otherwise
	// removing the last SSH rule could block SSH before that setting applies.
	if plan.enable != nil && !*plan.enable {
		if err := ufw("disable"); err != nil {
			return failed(err)
		}
		doneSetting("enabled")
		state.active, plan.enable = false, nil
	}
	if plan.incoming == "allow" {
		if err := ufw("default", "allow", "incoming"); err != nil {
			return failed(err)
		}
		doneSetting("default_incoming")
		state.incoming, plan.incoming = "allow", ""
	}
	// Add first, so a listed SSH rule exists before anything is removed. An
	// added rule replaces only the rule with its own target, never one planned
	// for removal.
	orderedKind := func(rule ufwRule) bool {
		for _, ordered := range plan.order {
			if ufwRuleKind(ordered[0]) == ufwRuleKind(rule) {
				return true
			}
		}
		return false
	}
	for _, r := range plan.add {
		if orderedKind(r) {
			continue
		}
		if err := ufw(r.command(false)...); err != nil {
			return failed(err)
		}
		doneAddedRule(r)
	}
	for _, r := range plan.remove {
		if orderedKind(r) {
			continue
		}
		if err := ufw(r.command(true)...); err != nil {
			return failed(err)
		}
		doneRule("removed", r)
	}
	// Rebuild only the kinds whose resulting order differs. Keep a listed SSH
	// allowance in place while rebuilding incoming rules on an active firewall.
	for _, ordered := range plan.order {
		pivot := -1
		if state.active && state.incoming != "allow" && ufwRuleKind(ordered[0]) == "incoming" {
			ports, err := sshdPorts(ctx)
			if err != nil {
				return failed(err)
			}
			for i, rule := range ordered {
				if rule.action != "allow" && rule.action != "limit" {
					continue
				}
				for _, port := range ports {
					if ufwTCPPort(rule.target, port) && run.ufwSSHRuleMismatch(rule, nodeInventoryAddress, port) == "" {
						pivot = i
						break
					}
				}
				if pivot >= 0 {
					break
				}
			}
		}
		if pivot >= 0 {
			if err := ufw(ordered[pivot].command(false)...); err != nil {
				return failed(err)
			}
			doneRebuiltRule(ordered[pivot])
		}
		// Remove blocking rules before other allowances, so a retained SSH
		// allowance cannot become hidden behind a deny while rules move.
		for _, blocking := range []bool{true, false} {
			for _, rule := range state.rules {
				if ufwRuleKind(rule) != ufwRuleKind(ordered[0]) {
					continue
				}
				if (rule.action == "deny" || rule.action == "reject") != blocking {
					continue
				}
				if pivot >= 0 && rule.target == ordered[pivot].target {
					continue
				}
				if err := ufw(rule.command(true)...); err != nil {
					return failed(err)
				}
				if slices.Contains(plan.remove, rule) {
					doneRule("removed", rule)
				}
			}
		}
		// Prepending avoids numeric positions, which differ between IPv4 and
		// IPv6 rules. The prefix is built backwards before the retained rule.
		for i := pivot - 1; i >= 0; i-- {
			args := ordered[i].command(false)
			at := 0
			if ordered[i].routed {
				at = 1
			}
			args = slices.Insert(args, at, "prepend")
			if err := ufw(args...); err != nil {
				return failed(err)
			}
			doneRebuiltRule(ordered[i])
		}
		for i := pivot + 1; i < len(ordered); i++ {
			if err := ufw(ordered[i].command(false)...); err != nil {
				return failed(err)
			}
			doneRebuiltRule(ordered[i])
		}
	}
	for _, p := range []struct{ policy, direction string }{{plan.incoming, "incoming"}, {plan.outgoing, "outgoing"}, {plan.routed, "routed"}} {
		if p.policy != "" {
			if err := ufw("default", p.policy, p.direction); err != nil {
				return failed(err)
			}
			doneSetting("default_" + p.direction)
		}
	}
	if plan.loggingLevel != "" {
		if err := ufw("logging", plan.loggingLevel); err != nil {
			return failed(err)
		}
		doneSetting("logging_level")
	}
	if plan.enable != nil {
		args := []string{"disable"}
		if *plan.enable {
			args = []string{"--force", "enable"}
		}
		if err := ufw(args...); err != nil {
			return failed(err)
		}
		doneSetting("enabled")
	}
	// ufw reports success for a rule it did not add or delete; check the result.
	after, err := inspectUFW(ctx, ufwProgram)
	if err == nil {
		if remaining := planUFW(u, desired, after).changes; len(remaining) > 0 {
			err = fmt.Errorf("ufw did not reach the requested state; remaining differences: %v", remaining)
		}
	}
	if err != nil {
		return failed(err)
	}
	return protocol.Success("changed")
}

// ufwDesiredRules are the task's incoming, routed, and outgoing rules, in
// ufw's own syntax.
func ufwRuleProtocols(protocol string) []string {
	if protocol == "tcp_and_udp" {
		return []string{"tcp", "udp"}
	}
	return []string{protocol}
}

func ufwDesiredRules(u UFW) ([]ufwRule, error) {
	var desired []ufwRule
	// A native rule without a protocol can also admit ICMP. Expand the
	// explicit pair to admit only the protocols the task names.
	for i, rule := range u.IncomingRules {
		if rule.FromInventoryGroup != nil {
			return nil, fmt.Errorf("internal error: incoming_rules[%d].from_inventory_group was not expanded", i)
		}
		for _, protocol := range ufwRuleProtocols(rule.Proto) {
			rule.Proto = protocol
			target, err := rule.Target()
			if err != nil {
				return nil, err
			}
			desired = append(desired, ufwRule{false, rule.Action, target})
		}
	}
	for _, rule := range u.RoutedRules {
		for _, protocol := range ufwRuleProtocols(rule.Proto) {
			rule.Proto = protocol
			target, err := rule.Target()
			if err != nil {
				return nil, err
			}
			desired = append(desired, ufwRule{true, rule.Action, target})
		}
	}
	for _, rule := range u.OutgoingRules {
		for _, protocol := range ufwRuleProtocols(rule.Proto) {
			rule.Proto = protocol
			target, err := rule.Target()
			if err != nil {
				return nil, err
			}
			desired = append(desired, ufwRule{false, rule.Action, target})
		}
	}
	return desired, nil
}

func inspectUFW(ctx context.Context, ufwProgram string) (ufwState, error) {
	var s ufwState
	r := Command(ctx, nil, "", "ufw", "status")
	if r.Status != "succeeded" {
		return s, fmt.Errorf("inspect ufw: %s: %s", r.Reason, r.Stderr)
	}
	s.active = strings.HasPrefix(r.Stdout, "Status: active")
	r = Command(ctx, nil, "", "ufw", "show", "added")
	if r.Status != "succeeded" {
		return s, fmt.Errorf("inspect ufw rules: %s: %s", r.Reason, r.Stderr)
	}
	s.rules = parseUFWRules(r.Stdout)
	defaults, err := os.ReadFile(ufwDefaults)
	if err != nil {
		return s, err
	}
	policies := map[string]string{"ACCEPT": "allow", "DROP": "deny", "REJECT": "reject"}
	for _, line := range strings.Split(string(defaults), "\n") {
		key, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "IPV6":
			s.ipv6Enabled = strings.Trim(v, `"`) == "yes"
		case "DEFAULT_INPUT_POLICY":
			s.incoming = policies[strings.Trim(v, `"`)]
		case "DEFAULT_OUTPUT_POLICY":
			s.outgoing = policies[strings.Trim(v, `"`)]
		case "DEFAULT_FORWARD_POLICY":
			s.routed = policies[strings.Trim(v, `"`)]
		}
	}
	if s.ipv6Enabled {
		// show added deduplicates dual-family rules after concatenating the
		// IPv4 and IPv6 lists. Read IPv6 order through ufw's own reader and
		// formatter; it cannot be recovered from that deduplicated output.
		r := Command(ctx, nil, "", "/usr/bin/python3", "-c", ufwIPv6RuleListing, ufwProgram)
		if r.Status != "succeeded" {
			return s, fmt.Errorf("inspect ufw IPv6 rule order: %s: %s", r.Reason, r.Stderr)
		}
		enabled, listing, _ := strings.Cut(r.Stdout, "\n")
		switch enabled {
		case "ipv6_enabled":
			s.ipv6Rules = parseUFWRules(listing)
		case "ipv6_disabled":
			s.ipv6Enabled = false
		default:
			return s, fmt.Errorf("inspect ufw IPv6 rule order: ufw's reader did not report whether IPv6 is enabled")
		}
	}
	configuration, err := os.ReadFile(ufwConfiguration)
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(configuration), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "LOGLEVEL="); ok {
			s.loggingLevel = strings.Trim(v, `"`)
		}
	}
	return s, nil
}

// The packaged ufw command uses this same Python backend to read its rules.
// Dryrun construction reads configuration without changing the firewall.
const ufwIPv6RuleListing = `import gettext
import sys
gettext.install("ufw")
from ufw.backend_iptables import UFWBackendIptables
from ufw.parser import UFWCommandRule, UFWCommandRouteRule
# The backend checks the invoking program's ownership and permissions.
sys.argv[0] = sys.argv[1]
backend = UFWBackendIptables(True)
print("ipv6_enabled" if backend.use_ipv6() else "ipv6_disabled")
for rule in backend.get_rules_ipv6():
    if rule.forward:
        print("ufw route " + UFWCommandRouteRule.get_command(rule))
    else:
        print("ufw " + UFWCommandRule.get_command(rule))
`

// ufwRulesForFamily keeps rules that can match this IP family. A rule with
// no literal address applies to both families; an explicit address or network
// limits it to that address family.
func ufwRulesForFamily(rules []ufwRule, ipv6 bool) []ufwRule {
	var kept []ufwRule
	for _, rule := range rules {
		fields := strings.Fields(rule.target)
		matches := true
		for i, field := range fields {
			if (field != "from" && field != "to") || i+1 >= len(fields) || fields[i+1] == "any" {
				continue
			}
			address := strings.SplitN(fields[i+1], "/", 2)[0]
			if parsed, err := netip.ParseAddr(address); err == nil && parsed.Is6() != ipv6 {
				matches = false
			}
		}
		if matches {
			kept = append(kept, rule)
		}
	}
	return kept
}

// parseUFWRules reads ufw show added: one "ufw [route] ACTION TARGET" line
// per rule, in ufw's order. Outgoing targets start with out.
func parseUFWRules(out string) []ufwRule {
	var rules []ufwRule
	for _, line := range strings.Split(out, "\n") {
		line, ok := strings.CutPrefix(line, "ufw ")
		if !ok {
			continue
		}
		line, _, _ = strings.Cut(line, " comment '")
		line, routed := strings.CutPrefix(line, "route ")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rules = append(rules, ufwRule{routed, fields[0], strings.Join(fields[1:], " ")})
	}
	return rules
}

func planUFW(u UFW, desired []ufwRule, s ufwState) ufwPlan {
	p := ufwPlan{changes: map[string]any{}}
	note := func(kind string, r ufwRule) {
		list, _ := p.changes[kind].([]any)
		p.changes[kind] = append(list, r.String())
	}
	// A rule's identity is whether it is routed and its target.
	type identity struct {
		routed bool
		target string
	}
	existing := map[identity]string{}
	for _, r := range s.rules {
		existing[identity{r.routed, r.target}] = r.action
	}
	listed := map[identity]bool{}
	for _, d := range desired {
		listed[identity{d.routed, d.target}] = true
		action, found := existing[identity{d.routed, d.target}]
		switch {
		case u.State == "absent":
		case !found:
			p.add = append(p.add, d)
			note("added", d)
		case action != d.action:
			p.add = append(p.add, d)
			note("updated", d)
		}
	}
	for _, r := range s.rules {
		notListed := u.IncomingRulesNotListed
		if r.routed {
			notListed = u.RoutedRulesNotListed
		} else if strings.HasPrefix(r.target, "out ") {
			notListed = u.OutgoingRulesNotListed
		}
		id := identity{r.routed, r.target}
		if (u.State == "absent" && listed[id]) || (notListed == "remove" && !listed[id]) {
			p.remove = append(p.remove, r)
			note("removed", r)
		}
	}
	if u.State != "absent" {
		resulting := ufwRulesAfterEdits(p, s)
		for _, kind := range []struct{ name, policy string }{
			{"incoming", u.IncomingRulesNotListed}, {"routed", u.RoutedRulesNotListed}, {"outgoing", u.OutgoingRulesNotListed},
		} {
			if kind.policy != "remove" {
				continue
			}
			var want, have []ufwRule
			for _, rule := range desired {
				if ufwRuleKind(rule) == kind.name {
					want = append(want, rule)
				}
			}
			for _, rule := range resulting {
				if ufwRuleKind(rule) == kind.name {
					have = append(have, rule)
				}
			}
			differs := !slices.Equal(ufwRulesForFamily(want, false), ufwRulesForFamily(have, false))
			if s.ipv6Enabled {
				var haveIPv6 []ufwRule
				for _, rule := range ufwRulesAfterEdits(p, ufwState{rules: s.ipv6Rules}) {
					if ufwRuleKind(rule) == kind.name {
						haveIPv6 = append(haveIPv6, rule)
					}
				}
				differs = differs || !slices.Equal(ufwRulesForFamily(want, true), ufwRulesForFamily(haveIPv6, true))
			}
			if differs {
				p.order = append(p.order, want)
				for _, rule := range want {
					note("reordered_rules", rule)
				}
			}
		}
	}
	if u.DefaultIncoming != "" && u.DefaultIncoming != s.incoming {
		p.incoming = u.DefaultIncoming
		p.changes["default_incoming"] = u.DefaultIncoming
	}
	if u.DefaultOutgoing != "" && u.DefaultOutgoing != s.outgoing {
		p.outgoing = u.DefaultOutgoing
		p.changes["default_outgoing"] = u.DefaultOutgoing
	}
	if u.DefaultRouted != "" && u.DefaultRouted != s.routed {
		p.routed = u.DefaultRouted
		p.changes["default_routed"] = u.DefaultRouted
	}
	if u.LoggingLevel != "" && u.LoggingLevel != s.loggingLevel {
		p.loggingLevel = u.LoggingLevel
		p.changes["logging_level"] = u.LoggingLevel
	}
	if u.Enabled != nil && *u.Enabled != s.active {
		p.enable = u.Enabled
		p.changes["enabled"] = *u.Enabled
	}
	return p
}

// ufwSafeguard reads the resulting incoming rules in ufw's first-match order.
// A rule must name an sshd TCP port, admit the node-observed controller source,
// and, if interface-bound, hold the node's inventory IP address.
func (run Runtime) ufwSafeguard(ctx context.Context, p ufwPlan, s ufwState, nodeInventoryAddress string) error {
	enabled, incoming := s.active, s.incoming
	if p.enable != nil {
		enabled = *p.enable
	}
	if p.incoming != "" {
		incoming = p.incoming
	}
	if !enabled || incoming == "allow" {
		return nil
	}
	ports, err := sshdPorts(ctx)
	if err != nil {
		return err
	}
	source, unknownReason := run.ufwSSHSourceAddress(nodeInventoryAddress)
	example := fmt.Sprintf("{action: allow, port: %d, protocol: tcp}", ports[0])
	sshFrom := "from the controller machine"
	fix := ""
	if source.IsValid() {
		example = fmt.Sprintf("{action: allow, port: %d, protocol: tcp, from: %s}", ports[0], source)
		sshFrom = fmt.Sprintf("from %s, the address that the controller machine's connection came from", source)
	} else {
		fix = " MMX does not know the address that the controller machine's SSH comes from: " + unknownReason +
			" Run the command with --connection ssh_only, or use an incoming rule without from."
	}
	fix += fmt.Sprintf(" Add an incoming rule such as %s.", example)
	mismatch := ""
	for _, r := range ufwResultingRules(p, s) {
		for _, port := range ports {
			blocking := r.action == "deny" || r.action == "reject"
			if !ufwTCPPort(r.target, port) && !(blocking && ufwEveryTCPPort(r.target)) {
				continue
			}
			if reason := run.ufwSSHRuleMismatch(r, nodeInventoryAddress, port); reason != "" {
				if mismatch == "" && (r.action == "allow" || r.action == "limit") {
					mismatch = reason
				}
				continue
			}
			if r.action == "allow" || r.action == "limit" {
				return nil
			}
			if r.action == "deny" || r.action == "reject" {
				return fmt.Errorf("The ufw rule %q blocks sshd's TCP port %d before any rule that would allow SSH %s. ufw applies the first matching rule. Remove the blocking rule or place the SSH allowance before the blocking rule.%s", r.String(), port, sshFrom, fix)
			}
		}
	}
	if mismatch != "" {
		return errors.New(mismatch + fix)
	}
	return fmt.Errorf("The ufw change would leave the firewall enabled with default incoming policy %s and no rule allowing or limiting sshd's TCP port %d %s. A rule without a port does not count.%s", incoming, ports[0], sshFrom, fix)
}

// ufwSSHSourceAddress trusts HTTPS's source only when HTTPS arrived at the
// inventory IP used for SSH. A different HTTPS path cannot prove SSH's source.
func (run Runtime) ufwSSHSourceAddress(nodeInventoryAddress string) (netip.Addr, string) {
	connection := run.ControllerConnection
	switch connection.Kind {
	case protocol.ConnectionSSH:
	case protocol.ConnectionHTTPS:
		inventoryAddress, err := netip.ParseAddr(nodeInventoryAddress)
		if err != nil {
			if nodeInventoryAddress == "" {
				return netip.Addr{}, "the node's inventory address was not recorded."
			}
			return netip.Addr{}, fmt.Sprintf("the node's inventory address %q is a host name, so MMX cannot tell whether HTTPS arrived at the node's SSH address.", nodeInventoryAddress)
		}
		localAddress, err := netip.ParseAddr(connection.NodeLocalAddress)
		if err != nil {
			return netip.Addr{}, "the node job arrived over HTTPS, but the node's local address of the HTTPS connection was not recorded."
		}
		if localAddress.Unmap() != inventoryAddress.Unmap() {
			return netip.Addr{}, fmt.Sprintf("the node job arrived over HTTPS at %s, not at the node's SSH address %s.", localAddress.Unmap(), inventoryAddress.Unmap())
		}
	default:
		return netip.Addr{}, "the node job has no recorded SSH or HTTPS connection."
	}
	address, err := netip.ParseAddr(connection.SourceAddress)
	if err != nil {
		return netip.Addr{}, "the IP address that the controller machine's connection came from was not recorded."
	}
	return address.Unmap(), ""
}

// ufwSSHRuleMismatch is shared by the safeguard and the SSH allowance kept
// installed while reordering. The port and rule kind are checked by callers.
func (run Runtime) ufwSSHRuleMismatch(rule ufwRule, nodeInventoryAddress string, port int) string {
	fields := strings.Fields(rule.target)
	for i, field := range fields {
		if field == "from" && i+1 < len(fields) {
			source := fields[i+1]
			controller, _ := run.ufwSSHSourceAddress(nodeInventoryAddress)
			admitted := false
			if controller.IsValid() {
				if network, err := netip.ParsePrefix(source); err == nil {
					admitted = network.Contains(controller)
				} else if address, err := netip.ParseAddr(source); err == nil {
					admitted = address.Unmap() == controller
				} else {
					admitted = source == "any"
				}
			}
			if !admitted {
				if !controller.IsValid() {
					return fmt.Sprintf("The ufw rule %q names a source for sshd's TCP port %d, but MMX cannot check that source against the controller machine's SSH connection.", rule.String(), port)
				}
				return fmt.Sprintf("The ufw rule %q allows sshd's TCP port %d only from addresses that do not include %s, the address that the controller machine's connection came from.", rule.String(), port, controller)
			}
		}
	}
	if len(fields) >= 3 && fields[0] == "in" && fields[1] == "on" {
		name := fields[2]
		nodeAddress, err := netip.ParseAddr(nodeInventoryAddress)
		if err != nil {
			if nodeInventoryAddress == "" {
				return fmt.Sprintf("The ufw rule %q allows sshd's TCP port %d only on %s. MMX cannot tell which interface SSH arrives on because the node's inventory address was not recorded.", rule.String(), port, name)
			}
			return fmt.Sprintf("The ufw rule %q allows sshd's TCP port %d only on %s. MMX cannot tell which interface SSH arrives on because the node's inventory address %q is a host name.", rule.String(), port, name, nodeInventoryAddress)
		}
		if networkInterface, err := net.InterfaceByName(name); err == nil {
			if addresses, err := networkInterface.Addrs(); err == nil {
				for _, address := range addresses {
					if prefix, err := netip.ParsePrefix(address.String()); err == nil && prefix.Addr().Unmap() == nodeAddress.Unmap() {
						return ""
					}
				}
			}
		}
		return fmt.Sprintf("The ufw rule %q allows sshd's TCP port %d only on %s, which does not hold %s, the node's inventory address.", rule.String(), port, name, nodeInventoryAddress)
	}
	return ""
}

func ufwRuleKind(rule ufwRule) string {
	if rule.routed {
		return "routed"
	}
	if strings.HasPrefix(rule.target, "out ") {
		return "outgoing"
	}
	return "incoming"
}

// ufwRulesAfterEdits retains native order for updates and appends new rules.
func ufwRulesAfterEdits(p ufwPlan, s ufwState) []ufwRule {
	var rules []ufwRule
	for _, rule := range s.rules {
		if !slices.Contains(p.remove, rule) {
			rules = append(rules, rule)
		}
	}
	for _, added := range p.add {
		i := slices.IndexFunc(rules, func(rule ufwRule) bool { return rule.routed == added.routed && rule.target == added.target })
		if i >= 0 {
			rules[i] = added //nolint:nilaway // i >= 0 means IndexFunc found the rule in rules, so rules is not empty.
		} else {
			rules = append(rules, added)
		}
	}
	return rules
}

// ufwResultingRules gives the incoming rules in their final order, including
// the order requested by a task that owns the incoming list.
func ufwResultingRules(p ufwPlan, s ufwState) []ufwRule {
	for _, ordered := range p.order {
		if len(ordered) > 0 && ufwRuleKind(ordered[0]) == "incoming" {
			return ordered
		}
	}
	var rules []ufwRule
	for _, rule := range ufwRulesAfterEdits(p, s) {
		if ufwRuleKind(rule) == "incoming" {
			rules = append(rules, rule)
		}
	}
	return rules
}

// DebianSSHDPrivilegeSeparationDirectory is the privilege separation
// directory that Debian and Ubuntu build sshd with. systemd creates it only
// while ssh.service runs. With ssh.socket, as on Ubuntu 24.04 and later, it
// is missing until a connection starts the service, such as after an update
// of openssh-server restarted it.
var DebianSSHDPrivilegeSeparationDirectory = "/run/sshd"

// SSHDSettings returns sshd's effective settings, as `program -T arguments`
// prints them. sshd refuses to print them without its privilege separation
// directory; a missing Debian one is created, as ssh.service would create it.
func SSHDSettings(ctx context.Context, program string, arguments ...string) (string, error) {
	run := func() protocol.Result { return Command(ctx, nil, "", program, append([]string{"-T"}, arguments...)...) }
	r := run()
	if r.Status != "succeeded" && strings.Contains(r.Stderr, "Missing privilege separation directory: "+DebianSSHDPrivilegeSeparationDirectory) {
		err := os.Mkdir(DebianSSHDPrivilegeSeparationDirectory, 0o755)
		if err == nil {
			err = os.Chmod(DebianSSHDPrivilegeSeparationDirectory, 0o755)
		}
		if err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("sshd -T: %s; creating %s failed: %v", strings.TrimSpace(r.Stderr), DebianSSHDPrivilegeSeparationDirectory, err)
		}
		r = run()
	}
	if r.Status != "succeeded" {
		return "", fmt.Errorf("sshd -T: %s: %s", r.Reason, strings.TrimSpace(r.Stderr))
	}
	return r.Stdout, nil
}

// sshdPorts are the TCP ports sshd listens on.
func sshdPorts(ctx context.Context) ([]int, error) {
	settings, err := SSHDSettings(ctx, "sshd")
	if err != nil {
		return nil, fmt.Errorf("read sshd's ports: %w", err)
	}
	var ports []int
	for _, line := range strings.Split(settings, "\n") {
		if address, ok := strings.CutPrefix(line, "listenaddress "); ok {
			if _, port, err := net.SplitHostPort(address); err == nil {
				if n, err := strconv.Atoi(port); err == nil {
					ports = append(ports, n)
				}
			}
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("sshd -T reports no listening port")
	}
	return ports, nil
}

// ufwEveryTCPPort reports whether an incoming rule target names no port and
// no protocol other than TCP, such as "in on eth0" or "from 10.0.0.0/8 proto
// tcp": the rule covers every TCP port. Only a deny or reject counts this way;
// an allowance must name sshd's port.
func ufwEveryTCPPort(target string) bool {
	fields := strings.Fields(target)
	if len(fields) == 0 || fields[0] != "in" && fields[0] != "from" {
		return false // the short form "22/tcp" always names a port
	}
	for i, field := range fields {
		if field == "port" || field == "proto" && i+1 < len(fields) && fields[i+1] != "tcp" {
			return false
		}
	}
	return true
}

// ufwTCPPort reports whether a rule target in the forms Target writes names
// a port range that covers TCP port. A rule from an address or network without a port names no port.
func ufwTCPPort(target string, port int) bool {
	fromSource := false
	fields := strings.Fields(target)
	if len(fields) >= 3 && fields[0] == "in" && fields[1] == "on" {
		fields = fields[3:]
	}
	if len(fields) >= 2 && fields[0] == "from" {
		fromSource = true
		fields = fields[2:]
	}
	spec := ""
	switch {
	case len(fields) == 1:
		if fromSource {
			return false
		}
		spec = fields[0]
	case len(fields) >= 4 && fields[0] == "to" && fields[1] == "any" && fields[2] == "port":
		spec = fields[3]
		if len(fields) == 6 && fields[4] == "proto" {
			spec += "/" + fields[5]
		} else if len(fields) != 4 {
			return false
		}
	default:
		return false
	}
	ports, proto, _ := strings.Cut(spec, "/")
	if proto != "" && proto != "tcp" {
		return false
	}
	first, last, isRange := strings.Cut(ports, ":")
	if !isRange {
		last = first
	}
	a, errA := strconv.Atoi(first)
	b, errB := strconv.Atoi(last)
	return errA == nil && errB == nil && a <= port && port <= b
}

// UFW manages incoming traffic to the node, routed traffic through it, and
// outgoing traffic that starts on the node.
type UFW struct {
	DefaultIncoming        string            `yaml:"default_incoming,omitempty" json:"default_incoming,omitempty"`
	DefaultOutgoing        string            `yaml:"default_outgoing,omitempty" json:"default_outgoing,omitempty"`
	DefaultRouted          string            `yaml:"default_routed,omitempty" json:"default_routed,omitempty"`
	IncomingRules          []UFWRule         `yaml:"incoming_rules,omitempty" json:"incoming_rules"`
	IncomingRulesNotListed string            `yaml:"incoming_rules_not_listed,omitempty" json:"incoming_rules_not_listed,omitempty"`
	RoutedRules            []UFWRoutedRule   `yaml:"routed_rules,omitempty" json:"routed_rules"`
	RoutedRulesNotListed   string            `yaml:"routed_rules_not_listed,omitempty" json:"routed_rules_not_listed,omitempty"`
	OutgoingRules          []UFWOutgoingRule `yaml:"outgoing_rules,omitempty" json:"outgoing_rules"`
	OutgoingRulesNotListed string            `yaml:"outgoing_rules_not_listed,omitempty" json:"outgoing_rules_not_listed,omitempty"`
	LoggingLevel           string            `yaml:"logging_level,omitempty" json:"logging_level,omitempty"`
	Enabled                *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	State                  string            `yaml:"state,omitempty" json:"state,omitempty"`
}

type UFWRule struct {
	Action            string      `yaml:"action,omitempty" json:"action,omitempty"`
	IncomingInterface string      `yaml:"incoming_interface,omitempty" json:"incoming_interface,omitempty"`
	Port              PortOrRange `yaml:"port,omitempty" json:"port,omitempty"`
	Proto             string      `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	From              string      `yaml:"from,omitempty" json:"from,omitempty"`
	// FromInventoryGroup stands for one rule for each node of the group; the
	// node's task receives those rules (expandInventoryGroups).
	FromInventoryGroup *FromInventoryGroup `yaml:"from_inventory_group,omitempty" json:"from_inventory_group,omitempty"`
}

// UFWRoutedRule is one rule for traffic the node forwards from one network
// to another: the interface it arrives on and leaves by, where it comes from
// and goes to, and the destination port.
type UFWRoutedRule struct {
	Action            string      `yaml:"action,omitempty" json:"action,omitempty"`
	IncomingInterface string      `yaml:"incoming_interface,omitempty" json:"incoming_interface,omitempty"`
	OutgoingInterface string      `yaml:"outgoing_interface,omitempty" json:"outgoing_interface,omitempty"`
	From              string      `yaml:"from,omitempty" json:"from,omitempty"`
	To                string      `yaml:"to,omitempty" json:"to,omitempty"`
	Port              PortOrRange `yaml:"port,omitempty" json:"port,omitempty"`
	Proto             string      `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

// UFWOutgoingRule is one rule for traffic that starts on the node itself.
type UFWOutgoingRule struct {
	Action            string      `yaml:"action,omitempty" json:"action,omitempty"`
	OutgoingInterface string      `yaml:"outgoing_interface,omitempty" json:"outgoing_interface,omitempty"`
	To                string      `yaml:"to,omitempty" json:"to,omitempty"`
	Port              PortOrRange `yaml:"port,omitempty" json:"port,omitempty"`
	Proto             string      `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

// PortOrRange is a port number or a first:last range, as ufw, firewalld, and
// selinux_port take it. YAML accepts a number or text for it (checkTypes).
type PortOrRange string

func ValidateUFW(u UFW) error {
	if err := validatePresence(u.State); err != nil {
		return err
	}
	if u.State == "absent" {
		if u.DefaultIncoming != "" || u.DefaultOutgoing != "" || u.DefaultRouted != "" || u.IncomingRulesNotListed != "" || u.RoutedRulesNotListed != "" || u.OutgoingRulesNotListed != "" || u.LoggingLevel != "" || u.Enabled != nil || len(u.IncomingRules)+len(u.RoutedRules)+len(u.OutgoingRules) == 0 {
			return fmt.Errorf("absent ufw accepts only incoming_rules, routed_rules, and outgoing_rules, and needs at least one rule")
		}
	} else if u.IncomingRules == nil && u.RoutedRules == nil && u.OutgoingRules == nil && u.DefaultIncoming == "" && u.DefaultOutgoing == "" && u.DefaultRouted == "" && u.LoggingLevel == "" && u.Enabled == nil {
		return fmt.Errorf("ufw needs incoming_rules, routed_rules, outgoing_rules, a default policy, logging_level, or enabled")
	}
	if u.State != "absent" {
		if err := validateNotListed("incoming_rules", u.IncomingRules != nil, u.IncomingRulesNotListed); err != nil {
			return err
		}
		if err := validateNotListed("routed_rules", u.RoutedRules != nil, u.RoutedRulesNotListed); err != nil {
			return err
		}
		if err := validateNotListed("outgoing_rules", u.OutgoingRules != nil, u.OutgoingRulesNotListed); err != nil {
			return err
		}
	}
	for field, policy := range map[string]string{"default_incoming": u.DefaultIncoming, "default_outgoing": u.DefaultOutgoing, "default_routed": u.DefaultRouted} {
		if policy != "" && !slices.Contains(ufwPolicies, policy) {
			return invalid(field, field+" must be allow, deny, or reject")
		}
	}
	if u.LoggingLevel != "" && !slices.Contains(ufwLogging, u.LoggingLevel) {
		return invalid("logging_level", "logging_level must be off, low, medium, high, or full")
	}
	checkAction := func(field, action, protocol string) error {
		if (action != "" || u.State != "absent") && !slices.Contains(ufwActions, action) {
			return invalid(field+".action", "action must be allow, deny, reject, or limit")
		}
		if protocol == "" {
			return invalid(field+".protocol", "protocol is required: write tcp, udp, or tcp_and_udp")
		}
		if !slices.Contains(ufwProtocols, protocol) {
			return invalid(field+".protocol", "protocol must be tcp, udp, or tcp_and_udp")
		}
		return nil
	}
	seen := map[string]bool{}
	for i, r := range u.IncomingRules {
		field := fmt.Sprintf("incoming_rules[%d]", i)
		if err := checkAction(field, r.Action, r.Proto); err != nil {
			return err
		}
		if err := validateRuleSource(field, r.From, r.FromInventoryGroup); err != nil {
			return err
		}
		if r.Port == "" && r.From == "" && r.FromInventoryGroup == nil && r.IncomingInterface == "" {
			return invalid(field, "an incoming rule requires at least one of port, from, and incoming_interface; a rule with only from selects every port from the given address or network, and a rule with only incoming_interface selects traffic arriving on the given interface, for the selected protocols")
		}
		// Written values are checked by their argument rules; the rules of a
		// group are checked once they are known (checkInventoryGroupRules).
		if Dynamic(string(r.Port)) || Dynamic(r.From) || Dynamic(r.IncomingInterface) || r.FromInventoryGroup != nil {
			continue
		}
		for _, protocol := range ufwRuleProtocols(r.Proto) {
			r.Proto = protocol
			target, err := r.Target()
			if err != nil {
				continue
			}
			if seen[target] {
				return invalid(field, "rule listed twice: "+target)
			}
			seen[target] = true
		}
	}
	seenRouted := map[string]bool{}
	for i, r := range u.RoutedRules {
		field := fmt.Sprintf("routed_rules[%d]", i)
		if err := checkAction(field, r.Action, r.Proto); err != nil {
			return err
		}
		if r.IncomingInterface == "" && r.OutgoingInterface == "" && r.From == "" && r.To == "" && r.Port == "" {
			return invalid(field, "a routed rule needs at least one of incoming_interface, outgoing_interface, from, to, and port")
		}
		if Dynamic(r.IncomingInterface) || Dynamic(r.OutgoingInterface) || Dynamic(r.From) || Dynamic(r.To) || Dynamic(string(r.Port)) {
			continue
		}
		for _, protocol := range ufwRuleProtocols(r.Proto) {
			r.Proto = protocol
			target, err := r.Target()
			if err != nil {
				continue
			}
			if seenRouted[target] {
				return invalid(field, "routed rule listed twice: "+target)
			}
			seenRouted[target] = true
		}
	}
	seenOutgoing := map[string]bool{}
	for i, r := range u.OutgoingRules {
		field := fmt.Sprintf("outgoing_rules[%d]", i)
		if err := checkAction(field, r.Action, r.Proto); err != nil {
			return err
		}
		if r.OutgoingInterface == "" && r.To == "" && r.Port == "" {
			return invalid(field, "an outgoing rule needs port, to, outgoing_interface, or several of them")
		}
		if Dynamic(r.OutgoingInterface) || Dynamic(r.To) || Dynamic(string(r.Port)) {
			continue
		}
		for _, protocol := range ufwRuleProtocols(r.Proto) {
			r.Proto = protocol
			target, err := r.Target()
			if err != nil {
				continue
			}
			if seenOutgoing[target] {
				return invalid(field, "outgoing rule listed twice: "+target)
			}
			seenOutgoing[target] = true
		}
	}
	return nil
}

// Target is the rule's identity in ufw's own syntax, without its action:
// "22/tcp", "from 10.0.0.0/8 to any port 5432 proto tcp", "in on eth1 to any
// port 22 proto tcp", or, without a port, "from 10.0.0.0/8". ufw prints added
// rules the same way (ufw show added), so the module compares this text.
func (r UFWRule) Target() (string, error) {
	return ufwTarget(r.IncomingInterface, "", r.From, "", r.Port, r.Proto)
}

// Target is the routed rule's identity in ufw's own syntax, without "route"
// and its action: "in on incusbr0", "in on eth1 out on eth0 to 10.1.2.3 port
// 80 proto tcp", or "443/tcp". ufw show added prints routed rules the same way.
func (r UFWRoutedRule) Target() (string, error) {
	return ufwTarget(r.IncomingInterface, r.OutgoingInterface, r.From, r.To, r.Port, r.Proto)
}

// Target includes out so an outgoing rule cannot share an incoming identity.
func (r UFWOutgoingRule) Target() (string, error) {
	target, err := ufwTarget("", r.OutgoingInterface, "", r.To, r.Port, r.Proto)
	if err != nil {
		return "", err
	}
	if r.OutgoingInterface == "" {
		target = "out " + target
	}
	return target, nil
}

// ufwTarget writes a rule as ufw's own normalized form does: a rule with
// only a port and protocol in the short form ("22/tcp"), every other rule
// as interfaces, source, destination, port, and protocol in that order.
func ufwTarget(incomingInterface, outgoingInterface, from, to string, port PortOrRange, protocol string) (string, error) {
	ranged := protocol != ""
	if protocol == "tcp_and_udp" {
		protocol = ""
	}
	portText := ""
	if port != "" {
		var err error
		if portText, err = portOrRange(string(port), ranged); err != nil {
			return "", err
		}
	}
	if incomingInterface == "" && outgoingInterface == "" && from == "" && to == "" {
		if protocol != "" {
			portText += "/" + protocol
		}
		return portText, nil
	}
	var parts []string
	for _, i := range []struct{ direction, name string }{{"in", incomingInterface}, {"out", outgoingInterface}} {
		if i.name == "" {
			continue
		}
		if !interfaceName.MatchString(i.name) {
			return "", errors.New(interfaceNameRule)
		}
		parts = append(parts, i.direction+" on "+i.name)
	}
	if from != "" {
		address, err := sourceAddress(from)
		if err != nil {
			return "", err
		}
		parts = append(parts, "from "+address)
	}
	if to != "" || portText != "" {
		destination := "any"
		if to != "" {
			address, err := sourceAddress(to)
			if err != nil {
				return "", errors.New("to must be an address or a network such as 10.0.0.0/8")
			}
			destination = address
		}
		parts = append(parts, "to "+destination)
	}
	if portText != "" {
		parts = append(parts, "port "+portText)
	}
	if protocol != "" {
		parts = append(parts, "proto "+protocol)
	}
	return strings.Join(parts, " "), nil
}

// interfaceName is a network interface name as ufw accepts it: Linux's limit
// of 15 characters, and no alias such as eth0:1, which ufw refuses.
var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

const interfaceNameRule = "an interface must be a network interface name such as eth0 or incusbr0: up to 15 letters, digits, '_', '.', and '-'"

// portOrRange is a port number, or with a protocol a first:last range, in decimal.
func portOrRange(s string, ranged bool) (string, error) {
	number := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 1 && n <= 65535
	}
	first, last, isRange := strings.Cut(s, ":")
	a, ok := number(first)
	if !isRange {
		if !ok {
			return "", errors.New("port must be a number from 1 to 65535, or first:last")
		}
		return strconv.Itoa(a), nil
	}
	b, okLast := number(last)
	if !ok || !okLast || a >= b {
		return "", errors.New("port range must be first:last with first below last")
	}
	if !ranged {
		return "", errors.New("a port range requires protocol: tcp or udp")
	}
	return fmt.Sprintf("%d:%d", a, b), nil
}

// sourceAddress is an address or a network in the form ufw stores it, which
// firewalld also accepts.
func sourceAddress(s string) (string, error) {
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return a.String(), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || p != p.Masked() {
		return "", errors.New("from must be an address or a network such as 10.0.0.0/8")
	}
	if p.IsSingleIP() {
		return p.Addr().String(), nil
	}
	return p.String(), nil
}

// ufwDescription is what mmx describe-module shows for ufw.
var ufwDescription = Description{
	Name: "ufw", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(UFW{}),
	Summary: "the ufw firewall: incoming rules, routed rules, outgoing rules, default policies, its logging level, and whether it is enabled",
	Fields: []Field{
		{Name: "incoming_rules", Notes: "rules for traffic addressed to the node itself. Each rule has action (allow, deny, reject, or limit) and protocol (tcp, udp, or tcp_and_udp), plus at least one traffic selector: port (number or first:last), from (address or network), or incoming_interface (such as eth1). An omitted selector matches every port, source address, or incoming interface. Without port, a rule selects every port for the selected protocols. Instead of from, use from_inventory_group: {group, address_var} or {group, use_node_address: true} to create a rule for each group member, using vars.<address_var> or the node's inventory address. A rule is identified by incoming_interface, port, protocol, and from. Requires incoming_rules_not_listed"},
		{Name: "incoming_rules_not_listed", Values: notListed, Notes: "required with incoming_rules; remove makes incoming_rules the whole ordered set of incoming rules, and incoming_rules: [] removes them all; keep appends missing rules and leaves existing order and other rules alone; omit with absent"},
		{Name: "routed_rules", Notes: "rules for traffic the node forwards between networks, such as container and VM traffic. Each rule has action (allow, deny, reject, or limit) and protocol (tcp, udp, or tcp_and_udp), plus at least one traffic selector: incoming_interface, outgoing_interface, from (source address or network), to (destination address or network), or port (destination port, number or first:last). An omitted address or interface matches any address or interface. Every rule field except action identifies the rule. Requires routed_rules_not_listed. The kernel forwards traffic only when forwarding is enabled (net.ipv4.ip_forward)"},
		{Name: "routed_rules_not_listed", Values: notListed, Notes: "required with routed_rules; remove makes routed_rules the whole ordered set of routed rules, and routed_rules: [] removes them all; keep appends missing rules and leaves existing order and other rules alone; omit with absent"},
		{Name: "outgoing_rules", Notes: "rules for traffic that starts on the node. Each rule has action (allow, deny, reject, or limit) and protocol (tcp, udp, or tcp_and_udp), plus at least one traffic selector: port (destination port, number or first:last), to (destination address or network), or outgoing_interface. An omitted selector matches every port, destination address, or outgoing interface. A rule is identified by outgoing_interface, port, protocol, and to. Requires outgoing_rules_not_listed"},
		{Name: "outgoing_rules_not_listed", Values: notListed, Notes: "required with outgoing_rules; remove makes outgoing_rules the whole ordered set of outgoing rules, and outgoing_rules: [] removes them all; keep appends missing rules and leaves existing order and other rules alone; omit with absent"},
		{Name: "default_incoming", Values: ufwPolicies, Notes: "default policy for traffic to the node that no incoming rule matches; omit to leave the default incoming policy unchanged"},
		{Name: "default_outgoing", Values: ufwPolicies, Notes: "default policy for traffic from the node; omit to leave the default outgoing policy unchanged"},
		{Name: "default_routed", Values: ufwPolicies, Notes: "default policy for forwarded traffic that no routed rule matches; omit to leave the default routed policy unchanged"},
		{Name: "logging_level", Values: ufwLogging, Notes: "how much ufw writes to its own log of the traffic it handles, as ufw logging sets it: low logs blocked packets, each higher level more; off turns the log off; omit to leave it unchanged"},
		{Name: "enabled", Notes: "true enables ufw now and at boot; false disables ufw; omit to leave the enabled state unchanged"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the listed rules whatever their action; accepts only incoming_rules, routed_rules, and outgoing_rules"},
	},
	Example: "- ufw:\n    default_incoming: deny\n    incoming_rules:\n      - {action: limit, port: 22, protocol: tcp}\n      - {action: allow, port: 443, protocol: tcp}\n      - {action: allow, incoming_interface: incusbr0, protocol: tcp_and_udp}\n    incoming_rules_not_listed: remove\n    routed_rules:\n      - {action: allow, incoming_interface: incusbr0, protocol: tcp_and_udp}\n      - {action: allow, outgoing_interface: incusbr0, protocol: tcp_and_udp}\n    routed_rules_not_listed: remove\n    outgoing_rules:\n      - {action: allow, port: 53, protocol: udp}\n      - {action: allow, port: 443, protocol: tcp}\n    outgoing_rules_not_listed: keep\n    logging_level: low\n    enabled: true\n",
	Results: []string{"status", "change", "value.added", "value.updated", "value.removed", "value.reordered_rules", "value.default_incoming", "value.default_outgoing", "value.default_routed", "value.logging_level", "value.enabled"},
	Dryrun:  "supported; lists rules to add, update, remove, or reorder (routed rules start with route; outgoing rules start with out after the action) and policy, logging level, or state changes; refuses a change that would leave SSH blocked",
}

// Prepare checks the inputs when the project loads.
func (u *UFW) Prepare(base string) error {
	return ValidateUFW(*u)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (u *UFW) Arguments(arguments *ArgumentList) {
	for i := range u.IncomingRules {
		r := &u.IncomingRules[i]
		if r.IncomingInterface != "" {
			arguments.Public(fmt.Sprintf("incoming_rules[%d].incoming_interface", i), &r.IncomingInterface, ufwInterfaceRule)
		}
		if r.Port != "" {
			arguments.Public(fmt.Sprintf("incoming_rules[%d].port", i), (*string)(&r.Port), ufwPortRule(r.Proto != ""))
		}
		if r.From != "" {
			arguments.Public(fmt.Sprintf("incoming_rules[%d].from", i), &r.From, ufwSourceRule)
		}
	}
	for i := range u.RoutedRules {
		r := &u.RoutedRules[i]
		if r.IncomingInterface != "" {
			arguments.Public(fmt.Sprintf("routed_rules[%d].incoming_interface", i), &r.IncomingInterface, ufwInterfaceRule)
		}
		if r.OutgoingInterface != "" {
			arguments.Public(fmt.Sprintf("routed_rules[%d].outgoing_interface", i), &r.OutgoingInterface, ufwInterfaceRule)
		}
		if r.From != "" {
			arguments.Public(fmt.Sprintf("routed_rules[%d].from", i), &r.From, ufwSourceRule)
		}
		if r.To != "" {
			arguments.Public(fmt.Sprintf("routed_rules[%d].to", i), &r.To, ufwDestinationRule)
		}
		if r.Port != "" {
			arguments.Public(fmt.Sprintf("routed_rules[%d].port", i), (*string)(&r.Port), ufwPortRule(r.Proto != ""))
		}
	}
	for i := range u.OutgoingRules {
		r := &u.OutgoingRules[i]
		if r.OutgoingInterface != "" {
			arguments.Public(fmt.Sprintf("outgoing_rules[%d].outgoing_interface", i), &r.OutgoingInterface, ufwInterfaceRule)
		}
		if r.To != "" {
			arguments.Public(fmt.Sprintf("outgoing_rules[%d].to", i), &r.To, ufwDestinationRule)
		}
		if r.Port != "" {
			arguments.Public(fmt.Sprintf("outgoing_rules[%d].port", i), (*string)(&r.Port), ufwPortRule(r.Proto != ""))
		}
	}
}

// ExpandInventoryGroups gives each incoming rule with from_inventory_group one
// rule for each node of the group. It reads the inventory that every node job
// carries, after the rules' own arguments are resolved.
func (u *UFW) ExpandInventoryGroups(inventory any) error {
	rules, err := expandRules("incoming_rules", u.IncomingRules, inventory, func(r *UFWRule) **FromInventoryGroup { return &r.FromInventoryGroup }, func(r *UFWRule) *string { return &r.From })
	if err == nil {
		u.IncomingRules = rules
	}
	return err
}

// NamesInventoryGroups reports whether an incoming rule names
// from_inventory_group.
func (u *UFW) NamesInventoryGroups() bool {
	return slices.ContainsFunc(u.IncomingRules, func(r UFWRule) bool { return r.FromInventoryGroup != nil })
}

// InventoryGroupRuleLists are the rule lists that may name
// from_inventory_group, by field; after ExpandInventoryGroups, the rules as
// the node gets them, which mmx explain shows.
func (u *UFW) InventoryGroupRuleLists() map[string][]any {
	rules := make([]any, len(u.IncomingRules))
	for i, rule := range u.IncomingRules {
		rules[i] = rule
	}
	return map[string][]any{"incoming_rules": rules}
}

// Run does the module's work on the node.
func (u *UFW) Run(ctx context.Context, task NodeTask) protocol.Result {
	inventory, _ := task.Scope["inventory"].(map[string]any)
	nodes, _ := inventory["nodes"].(map[string]any)
	node, _ := nodes[task.NodeName].(map[string]any)
	address, _ := node["address"].(string)
	return task.Runtime.UFW(ctx, *u, address)
}

// expandRules replaces each rule with from_inventory_group by one rule for
// each node of the group, with from set to that node's address.
func expandRules[R any](list string, rules []R, inventory any, group func(*R) **FromInventoryGroup, from func(*R) *string) ([]R, error) {
	if rules == nil {
		return nil, nil
	}
	expanded := make([]R, 0, len(rules))
	for i, rule := range rules {
		g := *group(&rule)
		if g == nil {
			expanded = append(expanded, rule)
			continue
		}
		addresses, err := groupAddresses(inventory, *g)
		if err != nil {
			return nil, &FieldError{fmt.Sprintf("%s[%d].from_inventory_group", list, i), err}
		}
		for _, address := range addresses {
			r := rule
			*from(&r), *group(&r) = address, nil //nolint:nilaway // Both callbacks return addresses of struct fields; clearing optional FromInventoryGroup is valid, and its consumers check for nil.
			expanded = append(expanded, r)
		}
	}
	return expanded, nil
}

// FromInventoryGroup on a ufw rule stands for one rule for each
// node of an inventory group, from the address that node's vars give under
// AddressVar, or from the node's inventory address with UseNodeAddress.
type FromInventoryGroup struct {
	Group          string `yaml:"group" json:"group"`
	AddressVar     string `yaml:"address_var,omitempty" json:"address_var,omitempty"`
	UseNodeAddress bool   `yaml:"use_node_address,omitempty" json:"use_node_address,omitempty"`
}

// validateRuleSource checks a rule's from and from_inventory_group fields.
func validateRuleSource(field, from string, group *FromInventoryGroup) error {
	if group == nil {
		return nil
	}
	if from != "" {
		return invalid(field, "a rule takes from or from_inventory_group, not both")
	}
	if !Name.MatchString(group.Group) {
		return invalid(field+".from_inventory_group.group", "from_inventory_group.group must name an inventory group")
	}
	if group.UseNodeAddress {
		if group.AddressVar != "" {
			return invalid(field+".from_inventory_group", "from_inventory_group takes address_var or use_node_address: true, not both")
		}
		return nil
	}
	if !Name.MatchString(group.AddressVar) {
		return invalid(field+".from_inventory_group.address_var", "from_inventory_group.address_var must name a variable in the vars of each node of the group, or use_node_address: true must take each node's address")
	}
	return nil
}

// groupAddresses is the address each node of the group gives under the
// variable, or its inventory address, in node name order, read from
// .inventory values (InventoryValues). Validation and the node read the same
// values.
func groupAddresses(inventory any, g FromInventoryGroup) ([]string, error) {
	values, _ := inventory.(map[string]any)
	groups, _ := values["groups"].(map[string]any)
	nodes, _ := values["nodes"].(map[string]any)
	members, ok := groups[g.Group].([]any)
	if !ok {
		return nil, fmt.Errorf("the inventory has no group %s", g.Group)
	}
	addresses := make([]string, 0, len(members))
	for _, member := range members {
		name, _ := member.(string)
		node, _ := nodes[name].(map[string]any)
		if g.UseNodeAddress {
			text, _ := node["address"].(string)
			if _, err := sourceAddress(text); err != nil {
				return nil, fmt.Errorf("node %s of group %s: its address %q is not an IP address, and a firewall rule needs one; give the group's IP addresses in a variable and name it with address_var", name, g.Group, text)
			}
			addresses = append(addresses, text)
			continue
		}
		vars, _ := node["vars"].(map[string]any)
		written, found := vars[g.AddressVar]
		if !found {
			return nil, fmt.Errorf("node %s in inventory group %s has no variable %s in vars; set the address variable in vars for every node in the inventory group", name, g.Group, g.AddressVar)
		}
		text, err := value.Scalar(written)
		if err == nil {
			_, err = sourceAddress(text)
		}
		if err != nil {
			return nil, fmt.Errorf("node %s of group %s: vars.%s must be an address or a network such as 10.0.1.11 or 10.0.0.0/8", name, g.Group, g.AddressVar)
		}
		addresses = append(addresses, text)
	}
	return addresses, nil
}
