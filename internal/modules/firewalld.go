package modules

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
)

// firewalldEntry is one thing a zone admits, in firewalld's own terms: a
// service, a port, or a rich rule. identity is FirewalldRule.Identity
// of the rule it stands for, and action allow, reject, or drop.
type firewalldEntry struct {
	kind, value      string // "service" "ssh", "port" "7443/tcp", or "rich-rule" and its text
	identity, action string
}

// String is the entry as results show it: "service ssh", "port 7443/tcp", or
// the rich rule's own text, which starts with "rule".
func (e firewalldEntry) String() string {
	if e.kind == "rich-rule" {
		return e.value
	}
	return e.kind + " " + e.value
}

// firewalldZone is what one layer of a zone admits: the running firewall's,
// or the stored configuration's.
type firewalldZone []firewalldEntry

func (z firewalldZone) has(e firewalldEntry) bool {
	return slices.ContainsFunc(z, func(x firewalldEntry) bool { return x.kind == e.kind && x.value == e.value })
}

// firewalldPlan is what a firewalld task changes in each layer, and the
// entries it adds and removes as results list them.
type firewalldPlan struct {
	add, remove     [2][]firewalldEntry // [running, stored]
	added, removed  []firewalldEntry    // both layers' entries once, in order
	running, stored firewalldZone       // each layer as the plan leaves it
}

func (p firewalldPlan) changes() map[string]any {
	changes := map[string]any{}
	for kind, list := range map[string][]firewalldEntry{"added": p.added, "removed": p.removed} {
		if len(list) > 0 {
			texts := make([]any, len(list))
			for i, e := range list {
				texts[i] = e.String()
			}
			changes[kind] = texts
		}
	}
	return changes
}

// Firewalld manages the services, ports, and rich rules of one firewalld zone. Each change goes to the running firewall and to the stored
// configuration together, one rule at a time; firewalld is never reloaded.
func (run Runtime) Firewalld(ctx context.Context, f Firewalld) (result protocol.Result) {
	if err := ValidateFirewalld(f); err != nil {
		return protocol.ConfigurationError(err)
	}
	desired := make([]firewalldEntry, len(f.Rules))
	for i, r := range f.Rules {
		if r.FromInventoryGroup != nil {
			return protocol.ConfigurationError(fmt.Errorf("internal error: rules[%d].from_inventory_group was not expanded", i))
		}
		e, err := firewalldEntryFor(r) // checked by the argument rules
		if err != nil {
			return protocol.ConfigurationError(err)
		}
		desired[i] = e
	}
	if _, err := lookPath(ctx, "firewall-cmd"); err != nil {
		return run.notYet(missingProgramError("firewall-cmd"))
	}
	if r := Command(ctx, nil, "", "firewall-cmd", "--state"); r.Status != "succeeded" {
		return run.notYet(fmt.Errorf("firewalld is not running; start it with a systemd_service task first: %s%s", r.Stdout, r.Stderr))
	}
	running, stored, err := inspectFirewalld(ctx, f.Zone)
	if err != nil {
		return protocol.Failed(err)
	}
	plan := planFirewalld(f, desired, running, stored)
	if len(plan.added)+len(plan.removed) == 0 {
		return protocol.Success("unchanged")
	}
	if err := firewalldSafeguard(ctx, f.Zone, plan); err != nil {
		return protocol.ConfigurationError(err)
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = plan.changes()
		return r
	}
	if err := run.Mutation("firewalld"); err != nil {
		return protocol.Failed(err)
	}
	// From here the value lists only the rules whose firewall-cmd call
	// worked, so that a failed task never reports a change that did not
	// happen.
	done := firewalldPlan{}
	defer func() {
		switch changes := done.changes(); {
		case result.Status == "succeeded":
			// A run that worked did all of the plan, so it reports the plan.
			result.Value = plan.changes()
		case len(changes) > 0:
			result.Value = changes
		}
	}()
	change := "unknown"
	firewallCmd := func(args ...string) error {
		r := Command(ctx, nil, "", "firewall-cmd", args...)
		if r.Status != "succeeded" {
			return fmt.Errorf("firewall-cmd %s: %s: %s%s", strings.Join(args, " "), r.Reason, r.Stderr, r.Stdout)
		}
		change = "changed"
		return nil
	}
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = change
		return r
	}
	// Each rule goes to the running firewall, then to the stored
	// configuration. Additions come first, so a listed SSH rule exists before
	// anything is removed.
	layers := [2][]string{{"--zone=" + f.Zone}, {"--permanent", "--zone=" + f.Zone}}
	for _, step := range []struct {
		verb    string
		all     []firewalldEntry
		entries [2][]firewalldEntry
	}{{"add", plan.added, plan.add}, {"remove", plan.removed, plan.remove}} {
		for _, e := range step.all {
			for layer := range layers {
				if !slices.Contains(step.entries[layer], e) {
					continue
				}
				if err := firewallCmd(append(slices.Clone(layers[layer]), "--"+step.verb+"-"+e.kind+"="+e.value)...); err != nil {
					return failed(err)
				}
				if step.verb == "add" && !slices.Contains(done.added, e) {
					done.added = append(done.added, e)
				} else if step.verb == "remove" && !slices.Contains(done.removed, e) {
					done.removed = append(done.removed, e)
				}
			}
		}
	}
	// firewall-cmd reports success for an entry it already had or lacked;
	// check the result.
	running, stored, err = inspectFirewalld(ctx, f.Zone)
	if err == nil {
		if remaining := planFirewalld(f, desired, running, stored); len(remaining.added)+len(remaining.removed) > 0 {
			err = fmt.Errorf("firewalld did not reach the requested state; still differs: %v", remaining.changes())
		}
	}
	if err != nil {
		return failed(err)
	}
	return protocol.Success("changed")
}

// dashedPort writes a port or a first:last range in decimal as firewalld and
// semanage list it: 8443, or 8000-8010. The argument rules accept 08443 and
// +8443 for 8443. An empty port stays empty.
func dashedPort(p PortOrRange) string {
	if p == "" {
		return ""
	}
	first, last, isRange := strings.Cut(string(p), ":")
	a, _ := strconv.Atoi(first) // checked by the argument rules
	port := strconv.Itoa(a)
	if isRange {
		b, _ := strconv.Atoi(last)
		port += "-" + strconv.Itoa(b)
	}
	return port
}

// firewalldEntryFor writes a rule in firewalld's own form: an allow rule
// with only a service or a port is that service or port of the zone; every
// other rule is a rich rule.
func firewalldEntryFor(r FirewalldRule) (firewalldEntry, error) {
	identity, err := r.Identity()
	if err != nil {
		return firewalldEntry{}, err
	}
	port := dashedPort(r.Port)
	e := firewalldEntry{identity: identity, action: r.Action}
	switch {
	case r.Action == "allow" && r.From == "" && r.FirewalldService != "":
		e.kind, e.value = "service", r.FirewalldService
		return e, nil
	case r.Action == "allow" && r.From == "" && port != "":
		e.kind, e.value = "port", port+"/"+r.Proto
		return e, nil
	}
	text := "rule"
	if _, source, found := strings.Cut(identity, "from "); found {
		// firewalld needs the address family of a rule with a source.
		family := "ipv4"
		if strings.Contains(source, ":") {
			family = "ipv6"
		}
		text += ` family="` + family + `" source address="` + source + `"`
	}
	switch {
	case r.FirewalldService != "":
		text += ` service name="` + r.FirewalldService + `"`
	case port != "":
		text += ` port port="` + port + `" protocol="` + r.Proto + `"`
	}
	text += " " + map[string]string{"allow": "accept", "reject": "reject", "drop": "drop"}[r.Action]
	e.kind, e.value = "rich-rule", text
	return e, nil
}

// firewalldRichRule matches the rich rules firewalldEntryFor writes, as
// firewall-cmd --list-rich-rules prints them.
var firewalldRichRule = regexp.MustCompile(`^rule(?: family="ipv[46]")?(?: source address="([^"]+)")?(?: service name="([^"]+)"| port port="([0-9]+(?:-[0-9]+)?)" protocol="(tcp|udp)")? (accept|reject|drop)$`)

// readFirewalldRichRule gives a rich rule its identity and action. A rich
// rule of another form, such as one with a log or a destination, is its own
// identity and matches no listed rule.
func readFirewalldRichRule(text string) firewalldEntry {
	e := firewalldEntry{kind: "rich-rule", value: text, identity: "rich rule " + text}
	m := firewalldRichRule.FindStringSubmatch(text)
	if m == nil {
		return e
	}
	r := FirewalldRule{From: m[1], FirewalldService: m[2], Port: PortOrRange(strings.Replace(m[3], "-", ":", 1)), Proto: m[4]}
	if identity, err := r.Identity(); err == nil && identity != "" {
		e.identity = identity
		e.action = map[string]string{"accept": "allow", "reject": "reject", "drop": "drop"}[m[5]]
	}
	return e
}

// inspectFirewalld reads what the zone admits in the running firewall and in
// the stored configuration.
func inspectFirewalld(ctx context.Context, zone string) (running, stored firewalldZone, err error) {
	read := func(permanent bool) (firewalldZone, error) {
		var z firewalldZone
		for _, kind := range []string{"services", "ports", "rich-rules"} {
			args := []string{"--zone=" + zone, "--list-" + kind}
			if permanent {
				args = append([]string{"--permanent"}, args...)
			}
			r := Command(ctx, nil, "", "firewall-cmd", args...)
			if r.Status != "succeeded" {
				if strings.Contains(r.Stdout+r.Stderr, "INVALID_ZONE") {
					layer := "running firewall"
					if permanent {
						layer = "stored configuration"
					}
					return nil, fmt.Errorf("Firewalld zone %s does not exist in the %s. Create it with a firewalld_zone task first.", zone, layer)
				}
				return nil, fmt.Errorf("inspect firewalld zone %s: firewall-cmd %s: %s: %s%s", zone, strings.Join(args, " "), r.Reason, r.Stderr, r.Stdout)
			}
			if kind == "rich-rules" {
				for _, line := range strings.Split(r.Stdout, "\n") {
					if line = strings.TrimSpace(line); line != "" {
						z = append(z, readFirewalldRichRule(line))
					}
				}
				continue
			}
			for _, word := range strings.Fields(r.Stdout) {
				e := firewalldEntry{kind: strings.TrimSuffix(kind, "s"), value: word, identity: strings.TrimSuffix(kind, "s") + " " + word, action: "allow"}
				z = append(z, e)
			}
		}
		return z, nil
	}
	if running, err = read(false); err != nil {
		return nil, nil, err
	}
	stored, err = read(true)
	return running, stored, err
}

func planFirewalld(f Firewalld, desired []firewalldEntry, running, stored firewalldZone) firewalldPlan {
	var p firewalldPlan
	want := map[string]firewalldEntry{}
	for _, d := range desired {
		want[d.identity] = d
	}
	for layer, zone := range []firewalldZone{running, stored} {
		var result firewalldZone
		for _, e := range zone {
			d, listed := want[e.identity]
			switch {
			case f.State == "absent" && listed,
				f.State != "absent" && listed && (d.kind != e.kind || d.value != e.value),
				f.State != "absent" && !listed && f.RulesNotListed == "remove":
				p.remove[layer] = append(p.remove[layer], e)
				if !slices.Contains(p.removed, e) {
					p.removed = append(p.removed, e)
				}
			default:
				result = append(result, e)
			}
		}
		if f.State != "absent" {
			for _, d := range desired {
				if !zone.has(d) {
					p.add[layer] = append(p.add[layer], d)
					if !slices.Contains(p.added, d) {
						p.added = append(p.added, d)
					}
					result = append(result, d)
				}
			}
		}
		if layer == 0 {
			p.running = result
		} else {
			p.stored = result
		}
	}
	return p
}

// firewalldSafeguard refuses a change after which the zone, running or
// stored, no longer lets a TCP port sshd listens on through. An allow rule
// counts when it names the port, directly or through a service whose
// definition holds it; a rule that admits a source on every port does not.
// firewalld applies reject and drop rules before services and ports, so a
// reject or drop rule for the port without a source blocks SSH whatever
// allows it; one limited to a source blocks only that source.
func firewalldSafeguard(ctx context.Context, zone string, p firewalldPlan) error {
	ports, err := sshdPorts(ctx)
	if err != nil {
		return err
	}
	servicePorts := map[string][]string{}
	// coversSSH reports whether an entry names a port or a service that
	// covers sshd's port, and whether the entry is limited to a source.
	coversSSH := func(e firewalldEntry) (covers, fromSource bool) {
		element, source, _ := strings.Cut(e.identity, "from ")
		kind, name, _ := strings.Cut(strings.TrimSpace(element), " ")
		var specs []string
		switch kind {
		case "port":
			specs = []string{name}
		case "service":
			if _, read := servicePorts[name]; !read {
				r := Command(ctx, nil, "", "firewall-cmd", "--info-service="+name)
				for _, line := range strings.Split(r.Stdout, "\n") {
					if list, ok := strings.CutPrefix(strings.TrimSpace(line), "ports:"); ok {
						servicePorts[name] = strings.Fields(list)
					}
				}
			}
			specs = servicePorts[name]
		}
		for _, spec := range specs {
			for _, port := range ports {
				if firewalldTCPPort(spec, port) {
					return true, source != ""
				}
			}
		}
		return false, false
	}
	for _, layer := range []struct {
		name string
		zone firewalldZone
	}{{"running firewall", p.running}, {"stored configuration", p.stored}} {
		admitted := false
		for _, e := range layer.zone {
			covers, fromSource := coversSSH(e)
			switch {
			case !covers:
			case e.action == "allow":
				admitted = true
			case (e.action == "reject" || e.action == "drop") && !fromSource:
				return fmt.Errorf("firewalld zone %s would %s sshd's TCP port %d for every source in the %s through %q; firewalld applies reject and drop rules before services and ports, so remove that rule or limit it to an address with from", zone, e.action, ports[0], layer.name, e.String())
			}
		}
		if !admitted {
			return fmt.Errorf("firewalld zone %s would no longer admit sshd's TCP port %d in the %s; list that port, such as {action: allow, firewalld_service: ssh} or {action: allow, port: %d, protocol: tcp}; a rule with only from does not count", zone, ports[0], layer.name, ports[0])
		}
	}
	return nil
}

// firewalldTCPPort reports whether a port entry such as 22/tcp or
// 8000-8010/tcp covers TCP port.
func firewalldTCPPort(spec string, port int) bool {
	ports, proto, _ := strings.Cut(spec, "/")
	if proto != "tcp" {
		return false
	}
	first, last, isRange := strings.Cut(ports, "-")
	if !isRange {
		last = first
	}
	a, errA := strconv.Atoi(first)
	b, errB := strconv.Atoi(last)
	return errA == nil && errB == nil && a <= port && port <= b
}

// Firewalld manages the services, ports, and rich rules of one firewalld zone.
type Firewalld struct {
	Zone           string          `yaml:"zone" json:"zone"`
	Rules          []FirewalldRule `yaml:"rules" json:"rules"`
	RulesNotListed string          `yaml:"rules_not_listed,omitempty" json:"rules_not_listed,omitempty"`
	State          string          `yaml:"state,omitempty" json:"state,omitempty"`
}

// FirewalldRule has ufw's rule fields wherever firewalld has the same thing.
type FirewalldRule struct {
	Action             string              `yaml:"action,omitempty" json:"action,omitempty"`
	FirewalldService   string              `yaml:"firewalld_service,omitempty" json:"firewalld_service,omitempty"`
	Port               PortOrRange         `yaml:"port,omitempty" json:"port,omitempty"`
	Proto              string              `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	From               string              `yaml:"from,omitempty" json:"from,omitempty"`
	FromInventoryGroup *FromInventoryGroup `yaml:"from_inventory_group,omitempty" json:"from_inventory_group,omitempty"`
}

// firewalldName is a zone or firewalld service name as firewalld's own files name them.
var firewalldName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

func ValidateFirewalld(f Firewalld) error {
	if err := validatePresence(f.State); err != nil {
		return err
	}
	if f.State == "absent" {
		if f.RulesNotListed != "" || len(f.Rules) == 0 {
			return fmt.Errorf("absent firewalld accepts only zone and rules, and needs at least one rule")
		}
	} else {
		if f.Rules == nil {
			return invalid("rules", "firewalld.rules is required: list the zone's services, ports, or rich rules, including an SSH allowance")
		}
		if err := validateNotListed("rules", true, f.RulesNotListed); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i, r := range f.Rules {
		field := fmt.Sprintf("rules[%d]", i)
		if (r.Action != "" || f.State != "absent") && !slices.Contains(firewalldActions, r.Action) {
			return invalid(field+".action", "action must be allow, reject, or drop")
		}
		if r.FirewalldService != "" && r.Port != "" {
			return invalid(field, "a rule takes firewalld_service or port, not both")
		}
		if r.Port != "" && !slices.Contains(firewalldProtocols, r.Proto) {
			return invalid(field+".protocol", "a rule with port needs protocol: tcp or udp")
		}
		if r.Port == "" && r.Proto != "" {
			return invalid(field+".protocol", "protocol goes with port; a firewalld service names its own ports")
		}
		if err := validateRuleSource(field, r.From, r.FromInventoryGroup); err != nil {
			return err
		}
		if r.FirewalldService == "" && r.Port == "" && r.From == "" && r.FromInventoryGroup == nil {
			return invalid(field, "a rule needs firewalld_service, port, from, or from_inventory_group; a rule with only from admits everything from that address or network")
		}
		// Written values are checked by their argument rules; the rules of a
		// group are checked once they are known (checkInventoryGroupRules).
		identity, err := r.Identity()
		if Dynamic(r.FirewalldService) || Dynamic(string(r.Port)) || Dynamic(r.From) || r.FromInventoryGroup != nil || err != nil {
			continue
		}
		if seen[identity] {
			return invalid(field, "rule listed twice: "+identity)
		}
		seen[identity] = true
	}
	return nil
}

// Identity is what makes two firewalld rules one rule, whatever their action:
// the service, or the port and protocol, and the source. "service ssh",
// "port 8000-8010/udp from 10.0.0.0/8", or "from 10.0.0.0/8". The module
// reads the zone's entries into the same text.
func (r FirewalldRule) Identity() (string, error) {
	var parts []string
	switch {
	case r.FirewalldService != "":
		parts = append(parts, "service "+r.FirewalldService)
	case r.Port != "":
		port, err := portOrRange(string(r.Port), true)
		if err != nil {
			return "", err
		}
		parts = append(parts, "port "+strings.Replace(port, ":", "-", 1)+"/"+r.Proto)
	}
	if r.From != "" {
		from, err := sourceAddress(r.From)
		if err != nil {
			return "", err
		}
		parts = append(parts, "from "+from)
	}
	return strings.Join(parts, " "), nil
}

// firewalldDescription is what mmx describe-module shows for firewalld.
var firewalldDescription = Description{
	Name: "firewalld", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(Firewalld{}),
	Summary: "the services, ports, and rich rules of one firewalld zone",
	Fields: []Field{
		{Name: "zone", Required: true, Notes: "the zone, such as public; MMX never guesses it; whether firewalld runs is a systemd_service task"},
		{Name: "rules", Required: true, Notes: "each {action: allow|reject|drop, firewalld_service: a service name firewalld knows, such as ssh, or port: number or first:last with protocol: tcp|udp, from: address or network}; a rule needs firewalld_service, port, or from, and one with only from admits everything from there; from_inventory_group: {group, address_var} in place of from, as on ufw; an allow rule with only firewalld_service or port becomes that service or port of the zone, every other rule a rich rule; identity is firewalld_service or port, protocol, and from"},
		{Name: "rules_not_listed", Values: notListed, Required: true, Notes: "remove makes rules the complete set of services, ports, and rich rules; the zone target is unchanged and may admit additional traffic, such as with ACCEPT; the SSH safeguard refuses a change to rules: [] on every zone, including one without SSH traffic; keep adds what is missing; interfaces, sources, and other zones stay unchanged; omit with absent"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the listed rules whatever their action; accepts only zone and rules"},
	},
	Example: "- firewalld:\n    zone: public\n    rules:\n      - {action: allow, firewalld_service: ssh}\n      - {action: allow, port: 7443, protocol: tcp}\n      - {action: allow, port: 5432, protocol: tcp, from: 10.0.0.0/8}\n    rules_not_listed: remove\n",
	Results: []string{"status", "change", "value.added", "value.removed"},
	Dryrun:  "supported; lists rules to add or remove in firewalld's own syntax; refuses a change after which the zone no longer admits sshd's port through a port or firewalld_service rule, or rejects or drops it for every source",
}

// Prepare checks the inputs when the project loads.
func (f *Firewalld) Prepare(base string) error {
	return ValidateFirewalld(*f)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (f *Firewalld) Arguments(arguments *ArgumentList) {
	arguments.Public("zone", &f.Zone, firewalldNameRule("zone"))
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.FirewalldService != "" {
			arguments.Public(fmt.Sprintf("rules[%d].firewalld_service", i), &r.FirewalldService, firewalldNameRule("firewalld_service"))
		}
		if r.Port != "" {
			arguments.Public(fmt.Sprintf("rules[%d].port", i), (*string)(&r.Port), ufwPortRule(true))
		}
		if r.From != "" {
			arguments.Public(fmt.Sprintf("rules[%d].from", i), &r.From, ufwSourceRule)
		}
	}
}

// ExpandInventoryGroups gives each rule with from_inventory_group one rule
// for each node of the group. It reads the inventory that every node job
// carries, after the rules' own arguments are resolved.
func (f *Firewalld) ExpandInventoryGroups(inventory any) error {
	rules, err := expandRules("rules", f.Rules, inventory, func(r *FirewalldRule) **FromInventoryGroup { return &r.FromInventoryGroup }, func(r *FirewalldRule) *string { return &r.From })
	if err == nil {
		f.Rules = rules
	}
	return err
}

// NamesInventoryGroups reports whether a rule names from_inventory_group.
func (f *Firewalld) NamesInventoryGroups() bool {
	return slices.ContainsFunc(f.Rules, func(r FirewalldRule) bool { return r.FromInventoryGroup != nil })
}

// InventoryGroupRuleLists are the rule lists that may name
// from_inventory_group, by field; after ExpandInventoryGroups, the rules as
// the node gets them, which mmx explain shows.
func (f *Firewalld) InventoryGroupRuleLists() map[string][]any {
	rules := make([]any, len(f.Rules))
	for i, rule := range f.Rules {
		rules[i] = rule
	}
	return map[string][]any{"rules": rules}
}

// Run does the module's work on the node.
func (f *Firewalld) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Firewalld(ctx, *f)
}
