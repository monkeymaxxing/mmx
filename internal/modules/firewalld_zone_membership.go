package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"mmx/internal/protocol"
)

// FirewalldZoneMembership owns interface and IP source bindings, never rules.
type FirewalldZoneMembership struct {
	Zone                                  string   `yaml:"zone" json:"zone"`
	Interfaces                            []string `yaml:"interfaces,omitempty" json:"interfaces"`
	SourceNetworks                        []string `yaml:"source_networks,omitempty" json:"source_networks"`
	InterfacesNotListed                   string   `yaml:"interfaces_not_listed,omitempty" json:"interfaces_not_listed,omitempty"`
	SourceNetworksNotListed               string   `yaml:"source_networks_not_listed,omitempty" json:"source_networks_not_listed,omitempty"`
	State                                 string   `yaml:"state,omitempty" json:"state,omitempty"`
	RestoreUnlessControllerConfirmsWithin string   `yaml:"restore_unless_controller_confirms_within,omitempty" json:"restore_unless_controller_confirms_within,omitempty"`
}

func (f FirewalldZoneMembership) confirmationWait() (time.Duration, error) {
	if f.RestoreUnlessControllerConfirmsWithin == "" {
		return 2 * time.Minute, nil
	}
	wait, err := time.ParseDuration(f.RestoreUnlessControllerConfirmsWithin)
	if err != nil || wait <= 0 {
		return 0, fmt.Errorf("restore_unless_controller_confirms_within must be a positive duration")
	}
	return wait, nil
}
func (f *FirewalldZoneMembership) Prepare(string) error {
	if err := validatePresence(f.State); err != nil {
		return err
	}
	if _, err := f.confirmationWait(); err != nil {
		return err
	}
	if f.State == "absent" {
		if f.InterfacesNotListed != "" || f.SourceNetworksNotListed != "" || len(f.Interfaces)+len(f.SourceNetworks) == 0 {
			return fmt.Errorf("absent firewalld_zone_membership requires at least one entry in interfaces or source_networks and accepts no interfaces_not_listed or source_networks_not_listed")
		}
	} else {
		if f.Interfaces == nil && f.SourceNetworks == nil {
			return fmt.Errorf("firewalld_zone_membership requires interfaces or source_networks (use [] for an empty list)")
		}
		if err := validateNotListed("interfaces", f.Interfaces != nil, f.InterfacesNotListed); err != nil {
			return err
		}
		if err := validateNotListed("source_networks", f.SourceNetworks != nil, f.SourceNetworksNotListed); err != nil {
			return err
		}
	}
	return f.checkDuplicates()
}
func (f *FirewalldZoneMembership) Arguments(arguments *ArgumentList) {
	arguments.Public("zone", &f.Zone, membershipZoneRule)
	for i := range f.Interfaces {
		arguments.Public(fmt.Sprintf("interfaces[%d]", i), &f.Interfaces[i], membershipInterfaceRule)
	}
	for i := range f.SourceNetworks {
		arguments.Public(fmt.Sprintf("source_networks[%d]", i), &f.SourceNetworks[i], membershipSourceRule)
	}
}

var membershipZoneRule = rule("The firewalld zone name must start with a letter, digit, or underscore and contain only letters, digits, underscores, dots, plus signs, or hyphens.", firewalldName.MatchString)
var membershipInterfaceRule = rule("The network interface name must contain 1 to 15 letters, digits, underscores, dots, or hyphens and start with a letter, digit, or underscore.", interfaceName.MatchString)
var membershipSourceRule = parsed(func(text string) error {
	if _, err := sourceAddress(text); err != nil {
		return fmt.Errorf("The source network must be an IPv4 or IPv6 address or network, such as 10.0.0.0/8.")
	}
	return nil
})

func (f FirewalldZoneMembership) checkDuplicates() error {
	for _, list := range [][]string{f.Interfaces, f.SourceNetworks} {
		seen := map[string]bool{}
		for _, written := range list {
			if Dynamic(written) {
				continue
			}
			identity := written
			if normalized, err := sourceAddress(written); err == nil {
				identity = normalized
			}
			if seen[identity] {
				return fmt.Errorf("The firewalld zone member %q is listed twice.", written)
			}
			seen[identity] = true
		}
	}
	return nil
}

// Each member records both layers, including a layer which did not need a
// forward command. After reboot, runtime may have copied the proposed stored
// binding; restoration must still recover the old, possibly different layers.
type firewalldMember struct {
	Kind string `json:"member_kind"`
	Name string `json:"member_name"`
}
type firewalldBinding struct {
	Zone  string `json:"zone"`
	Value string `json:"member_spelling"`
}
type firewalldMemberChange struct {
	Member   firewalldMember     `json:"member"`
	Previous [2]firewalldBinding `json:"previous_bindings"`
	Proposed [2]firewalldBinding `json:"proposed_bindings"`
}

// Arrays alone cannot validate saved recovery data: encoding/json accepts
// missing and extra elements. An absent binding must explicitly name both
// empty fields, rather than arise from an omitted field or null layer.
func (change *firewalldMemberChange) UnmarshalJSON(data []byte) error {
	type bindingFields struct {
		Zone  *string `json:"zone"`
		Value *string `json:"member_spelling"`
	}
	var saved struct {
		Member   firewalldMember  `json:"member"`
		Previous []*bindingFields `json:"previous_bindings"`
		Proposed []*bindingFields `json:"proposed_bindings"`
	}
	if err := protocol.Decode(data, &saved); err != nil {
		return err
	}
	for _, bindings := range [][]*bindingFields{saved.Previous, saved.Proposed} {
		if len(bindings) != 2 {
			return fmt.Errorf("Each saved firewalld member requires both previous and proposed bindings, with exactly two layers in each list.")
		}
		for _, binding := range bindings {
			if binding == nil || binding.Zone == nil || binding.Value == nil {
				return fmt.Errorf("Each saved firewalld binding requires a zone and member spelling, including empty strings for no binding.")
			}
		}
	}
	change.Member = saved.Member
	for layer := 0; layer < 2; layer++ {
		change.Previous[layer] = firewalldBinding{*saved.Previous[layer].Zone, *saved.Previous[layer].Value}
		change.Proposed[layer] = firewalldBinding{*saved.Proposed[layer].Zone, *saved.Proposed[layer].Value}
	}
	return nil
}

type firewalldMembershipRestoreData struct {
	Changes []firewalldMemberChange `json:"membership_changes"`
}
type firewalldMembershipSnapshot struct {
	Zones   [2][]string
	Members [2]map[firewalldMember]firewalldBinding
}

var firewalldLayerNames = [2]string{"running_firewall", "stored_config"}
var firewalldLayerDescriptions = [2]string{"running firewall", "stored configuration"}

func firewalldArguments(layer int, args ...string) []string {
	if layer == 1 {
		return append([]string{"--permanent"}, args...)
	}
	return args
}
func firewalldRead(ctx context.Context, run Runtime, args ...string) (string, error) {
	output := run.RunCommand(ctx, append([]string{"firewall-cmd"}, args...), "")
	if err := commandFailure(output, "Reading firewalld configuration"); err != nil {
		return "", err
	}
	if strings.Contains(output.StandardOutput, protocol.TruncationMarker) || strings.Contains(output.StandardError, protocol.TruncationMarker) {
		return "", fmt.Errorf("Firewalld returned an incomplete configuration listing because its output exceeded the size limit.")
	}
	return strings.TrimSpace(output.StandardOutput), nil
}
func firewalldWrite(ctx context.Context, run Runtime, args ...string) error {
	output := run.RunCommandThatChangesTheNode(ctx, append([]string{"firewall-cmd"}, args...), "")
	return commandFailure(output, "Changing firewalld configuration")
}
func firewalldPrerequisites(ctx context.Context, run Runtime) error {
	if !run.FindProgram(ctx, "firewall-cmd").Found {
		return missingProgramError("firewall-cmd")
	}
	output := run.RunCommand(ctx, []string{"firewall-cmd", "--state"}, "")
	if err := commandFailure(output, "Reading firewalld state"); err != nil {
		return fmt.Errorf("firewalld is not running; start it with a systemd_service task first: %s%s", output.StandardOutput, output.StandardError)
	}
	return nil
}
func inspectFirewalldMembership(ctx context.Context, run Runtime) (firewalldMembershipSnapshot, error) {
	var snapshot firewalldMembershipSnapshot
	for layer := range snapshot.Zones {
		snapshot.Members[layer] = map[firewalldMember]firewalldBinding{}
		text, err := firewalldRead(ctx, run, firewalldArguments(layer, "--list-all-zones")...)
		if err != nil {
			return snapshot, err
		}
		zone := ""
		fields := map[string]bool{}
		finishZone := func() error {
			if zone != "" && (!fields["interfaces"] || !fields["sources"]) {
				return fmt.Errorf("Firewalld did not list both interfaces and sources for zone %s in the %s.", zone, firewalldLayerDescriptions[layer])
			}
			return nil
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if line[0] != ' ' && line[0] != '\t' {
				if err := finishZone(); err != nil {
					return snapshot, err
				}
				zone = strings.Fields(line)[0]
				if !firewalldName.MatchString(zone) || slices.Contains(snapshot.Zones[layer], zone) {
					return snapshot, fmt.Errorf("Firewalld returned an invalid or repeated zone name %q.", zone)
				}
				snapshot.Zones[layer] = append(snapshot.Zones[layer], zone)
				fields = map[string]bool{}
				continue
			}
			field, members, found := strings.Cut(strings.TrimSpace(line), ":")
			if !found || (field != "interfaces" && field != "sources") {
				continue
			}
			if zone == "" || fields[field] {
				return snapshot, fmt.Errorf("Firewalld returned an incomplete or repeated membership listing.")
			}
			fields[field] = true
			kind := "interface"
			if field == "sources" {
				kind = "source"
			}
			for _, member := range strings.Fields(members) {
				name := member
				if kind == "source" {
					if normalized, err := sourceAddress(member); err == nil {
						name = normalized
					}
				}
				key := firewalldMember{kind, name}
				if previous, exists := snapshot.Members[layer][key]; exists {
					return snapshot, fmt.Errorf("Firewalld lists member %s in both zone %s and zone %s in the %s.", name, previous.Zone, zone, firewalldLayerDescriptions[layer])
				}
				snapshot.Members[layer][key] = firewalldBinding{zone, member}
			}
		}
		if err := finishZone(); err != nil {
			return snapshot, err
		}
		if len(snapshot.Zones[layer]) == 0 {
			return snapshot, fmt.Errorf("Firewalld returned no zones in the %s.", firewalldLayerDescriptions[layer])
		}
	}
	return snapshot, nil
}
func requireFirewalldZone(snapshot firewalldMembershipSnapshot, zone string) error {
	for layer, zones := range snapshot.Zones {
		if !slices.Contains(zones, zone) {
			return fmt.Errorf("Firewalld zone %s does not exist in the %s. Create it with a firewalld_zone task first.", zone, firewalldLayerDescriptions[layer])
		}
	}
	return nil
}
func (f FirewalldZoneMembership) plan(snapshot firewalldMembershipSnapshot) (firewalldMembershipRestoreData, error) {
	desired := map[firewalldMember]bool{}
	var members []firewalldMember
	for _, name := range f.Interfaces {
		member := firewalldMember{"interface", name}
		desired[member] = true
		members = append(members, member)
	}
	for _, source := range f.SourceNetworks {
		name, _ := sourceAddress(source)
		member := firewalldMember{"source", name}
		desired[member] = true
		members = append(members, member)
	}
	if f.InterfacesNotListed == "remove" || f.SourceNetworksNotListed == "remove" {
		var unlisted []firewalldMember
		for _, layer := range snapshot.Members {
			for member, binding := range layer {
				if (member.Kind == "interface" && f.InterfacesNotListed != "remove") || (member.Kind == "source" && f.SourceNetworksNotListed != "remove") {
					continue
				}
				if binding.Zone != f.Zone || desired[member] || slices.Contains(unlisted, member) {
					continue
				}
				if member.Kind == "source" {
					if _, err := sourceAddress(member.Name); err != nil {
						return firewalldMembershipRestoreData{}, fmt.Errorf("Cannot remove unlisted members of firewalld zone %s: source %s is not an IP address or network. Remove this unsupported source with firewalld first.", f.Zone, member.Name)
					}
				}
				unlisted = append(unlisted, member)
			}
		}
		slices.SortFunc(unlisted, func(a, b firewalldMember) int { return strings.Compare(a.Kind+" "+a.Name, b.Kind+" "+b.Name) })
		members = append(members, unlisted...)
	}
	data := firewalldMembershipRestoreData{}
	for _, member := range members {
		change := firewalldMemberChange{Member: member}
		changed := false
		for layer := range snapshot.Members {
			before := snapshot.Members[layer][member]
			after := before
			if f.State != "absent" && desired[member] {
				after = firewalldBinding{f.Zone, member.Name}
				if before.Zone != "" {
					after.Value = before.Value
				}
			} else if before.Zone == f.Zone {
				after = firewalldBinding{}
			}
			change.Previous[layer], change.Proposed[layer] = before, after
			changed = changed || before.Zone != after.Zone
		}
		if changed {
			data.Changes = append(data.Changes, change)
		}
	}
	return data, nil
}
func (data firewalldMembershipRestoreData) value() map[string]any {
	value := map[string]any{}
	for layer, layerName := range firewalldLayerNames {
		changes := map[string]any{}
		for _, change := range data.Changes {
			before, after := change.Previous[layer], change.Proposed[layer]
			if before.Zone == after.Zone {
				continue
			}
			verb := "added"
			member := map[string]any{"member_kind": map[string]string{"interface": "interface", "source": "source_network"}[change.Member.Kind], "member_name": change.Member.Name}
			if after.Zone == "" {
				verb = "removed"
			} else if before.Zone != "" {
				verb = "moved"
				member["previous_zone"] = before.Zone
			}
			list, _ := changes[verb].([]any)
			changes[verb] = append(list, member)
		}
		if len(changes) > 0 {
			value[layerName] = changes
		}
	}
	return value
}

// The permanent firewalld command delegates to NetworkManager for an active
// profile. Refuse before any mutation rather than silently rewriting its file.
func refuseNetworkManagerInterfaces(ctx context.Context, run Runtime, interfaces []string, zone string, removing bool) error {
	if len(interfaces) == 0 {
		return nil
	}
	program := run.FindProgram(ctx, "nmcli")
	if program.Error != "" {
		return fmt.Errorf("Cannot check whether NetworkManager controls the interfaces: %s.", program.Error)
	}
	if !program.Found {
		state := run.RunCommand(ctx, []string{"systemctl", "show", "NetworkManager.service", "--property=ActiveState", "--value"}, "")
		if state.Error == "" && state.ExitCode == 0 && slices.Contains([]string{"inactive", "failed"}, strings.TrimSpace(state.StandardOutput)) {
			return nil
		}
		return fmt.Errorf("Cannot change firewalld membership for interfaces %s: nmcli is missing, and NetworkManager may control them. Install nmcli so MMX can check their profiles, or establish that NetworkManager.service is inactive.", strings.Join(interfaces, ", "))
	}
	state := run.RunCommand(ctx, []string{"nmcli", "-t", "-f", "RUNNING", "general"}, "")
	// nmcli documents exit status 8 for a stopped NetworkManager daemon.
	if state.Error == "" && state.ExitCode == 8 {
		return nil
	}
	if err := commandFailure(state, "Checking whether NetworkManager runs"); err != nil {
		return err
	}
	switch strings.TrimSpace(state.StandardOutput) {
	case "not running":
		return nil
	case "running":
	default:
		return fmt.Errorf("Cannot establish whether NetworkManager controls the listed interfaces: its running state is unknown.")
	}
	for _, card := range interfaces {
		output := run.RunCommand(ctx, []string{"nmcli", "-g", "GENERAL.CON-UUID", "device", "show", card}, "")
		// nmcli uses 10 for a missing device. Every other inspection failure is
		// unsafe to interpret as an unmanaged device.
		if output.Error == "" && output.ExitCode == 10 {
			continue
		}
		if err := commandFailure(output, "Checking whether NetworkManager controls interface "+card); err != nil {
			return err
		}
		if strings.Contains(output.StandardOutput, protocol.TruncationMarker) {
			return fmt.Errorf("NetworkManager returned an incomplete active profile identity for interface %s.", card)
		}
		uuid := strings.TrimSpace(output.StandardOutput)
		if uuid == "" || uuid == "--" {
			continue
		}
		profiles := run.RunCommand(ctx, []string{"nmcli", "-t", "-f", "UUID,FILENAME", "connection", "show"}, "")
		if err := commandFailure(profiles, "Reading NetworkManager profile filenames"); err != nil {
			return err
		}
		if strings.Contains(profiles.StandardOutput, protocol.TruncationMarker) {
			return fmt.Errorf("NetworkManager returned an incomplete list of profile filenames.")
		}
		filename := ""
		for _, line := range strings.Split(profiles.StandardOutput, "\n") {
			id, path, found := strings.Cut(line, ":")
			if found && id == uuid {
				filename = strings.NewReplacer(`\:`, ":", `\\`, `\`).Replace(path)
				break
			}
		}
		if !strings.HasPrefix(filepath.Clean(filename), "/etc/") {
			return fmt.Errorf("Cannot change firewalld zone membership of interface %s: NetworkManager made its active profile itself. The interface needs a profile file below /etc/ first; manage that profile with a networkmanager_connection_file task.", card)
		}
		instruction := "write zone=" + zone + " in its [connection] section"
		if removing {
			instruction = "remove the zone= line from its [connection] section"
		}
		oldFormat := ""
		if !strings.HasSuffix(filename, ".nmconnection") {
			oldFormat = fmt.Sprintf(" Convert this profile with nmcli connection migrate --plugin keyfile uuid %s. Then manage the resulting profile file.", uuid)
		}
		return fmt.Errorf("Cannot change firewalld zone membership of interface %s: NetworkManager controls it through profile %s. A permanent firewalld command would rewrite that profile. Use a networkmanager_connection_file task to %s.%s", card, filename, instruction, oldFormat)
	}
	return nil
}
func membershipRefusal(err error) protocol.Result {
	result := protocol.Failed(err)
	result.Change = "unchanged"
	result.NoOtherTry = true
	return result
}
func (f *FirewalldZoneMembership) Run(ctx context.Context, task NodeTask) protocol.Result {
	if err := f.checkDuplicates(); err != nil {
		return protocol.ConfigurationError(err)
	}
	if task.OwedRestores == nil {
		return membershipRefusal(fmt.Errorf("Changing firewalld zone membership requires the node's owed restore service."))
	}
	if err := task.OwedRestores.CheckChangeAllowed(ctx, ""); err != nil {
		return membershipRefusal(err)
	}
	if err := firewalldPrerequisites(ctx, task.Runtime); err != nil {
		result := task.Runtime.notYet(err)
		if !task.Runtime.Dryrun {
			result.Change = "unchanged"
		}
		return result
	}
	snapshot, err := inspectFirewalldMembership(ctx, task.Runtime)
	if err != nil {
		return membershipRefusal(err)
	}
	if err = requireFirewalldZone(snapshot, f.Zone); err != nil {
		return membershipRefusal(err)
	}
	data, err := f.plan(snapshot)
	if err != nil {
		return membershipRefusal(err)
	}
	if err = refuseNetworkManagerInterfaces(ctx, task.Runtime, f.Interfaces, f.Zone, f.State == "absent"); err != nil {
		return membershipRefusal(err)
	}
	// Unlisted interfaces removed by a whole-zone request have the same owner check.
	for _, change := range data.Changes {
		if change.Member.Kind == "interface" && !slices.Contains(f.Interfaces, change.Member.Name) {
			if err = refuseNetworkManagerInterfaces(ctx, task.Runtime, []string{change.Member.Name}, f.Zone, true); err != nil {
				return membershipRefusal(err)
			}
		}
	}
	if len(data.Changes) == 0 {
		result := protocol.Success("unchanged")
		result.Reason = fmt.Sprintf("Membership of firewalld zone %s already matches in the running firewall and stored configuration. No confirmation wait was needed.", f.Zone)
		return result
	}
	if task.Runtime.Dryrun {
		result := protocol.Success("would_change")
		result.Value = data.value()
		result.Reason = fmt.Sprintf("Would change membership of firewalld zone %s to match the requested state in both firewall layers. Reachability after the change was not checked.", f.Zone)
		return result
	}
	saved, err := json.Marshal(data)
	if err != nil {
		return membershipRefusal(err)
	}
	wait, _ := f.confirmationWait()
	result := task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{
		RestoreConfirmationWait: wait, ModuleRestoreData: saved,
		ChangeSubject: "firewalld zone membership", RestoreAfterUnit: "firewalld.service",
		RestoreSuccessSentence: "The previous firewalld zone membership is restored in the running firewall and stored configuration.",
	})
	if result.Status == "succeeded" && result.Change == "changed" {
		result.Value = data.value()
	} else {
		result.Value = nil
	}
	if result.Status == "succeeded" && result.Reason == "" {
		result.Reason = fmt.Sprintf("Membership of firewalld zone %s now matches in the running firewall and stored configuration. New connections confirmed reachability.", f.Zone)
	}
	return result
}

type firewalldMembershipActivation struct {
	data firewalldMembershipRestoreData
}

func newFirewalldMembershipActivation(destination string, saved json.RawMessage) (ChangeActivation, error) {
	if destination != "" {
		return nil, fmt.Errorf("A firewalld membership restore cannot have a destination path.")
	}
	activation := &firewalldMembershipActivation{}
	if err := protocol.Decode(saved, &activation.data); err != nil {
		return nil, err
	}
	if len(activation.data.Changes) == 0 {
		return nil, fmt.Errorf("A firewalld membership restore requires saved member bindings.")
	}
	seen := map[firewalldMember]bool{}
	for _, change := range activation.data.Changes {
		member := change.Member
		valid := member.Kind == "interface" && interfaceName.MatchString(member.Name)
		if member.Kind == "source" {
			normalized, err := sourceAddress(member.Name)
			valid = err == nil && normalized == member.Name
		}
		if !valid || seen[member] {
			return nil, fmt.Errorf("The saved firewalld member %q is invalid or repeated.", member.Name)
		}
		seen[member] = true
		for _, bindings := range [][2]firewalldBinding{change.Previous, change.Proposed} {
			for _, binding := range bindings {
				if binding.Zone == "" {
					if binding.Value != "" {
						return nil, fmt.Errorf("A saved absent firewalld binding cannot name a member.")
					}
					continue
				}
				if !firewalldName.MatchString(binding.Zone) {
					return nil, fmt.Errorf("The saved firewalld zone %q is invalid.", binding.Zone)
				}
				identity := binding.Value
				if member.Kind == "source" {
					identity, _ = sourceAddress(identity)
				}
				if identity != member.Name {
					return nil, fmt.Errorf("The saved firewalld binding does not match member %s.", member.Name)
				}
			}
		}
	}
	return activation, nil
}
func (a *firewalldMembershipActivation) PrepareActivation(ctx context.Context, run Runtime, restoring bool) error {
	if err := firewalldPrerequisites(ctx, run); err != nil {
		if restoring {
			return fmt.Errorf("Cannot restore firewalld zone membership: %w. On the node, start firewalld with systemctl start firewalld.service. Then start the restore service named by mmx status.", err)
		}
		return err
	}
	snapshot, err := inspectFirewalldMembership(ctx, run)
	if err != nil {
		return err
	}
	for _, change := range a.data.Changes {
		target := change.Proposed
		if restoring {
			target = change.Previous
		}
		for layer, binding := range target {
			if binding.Zone != "" && !slices.Contains(snapshot.Zones[layer], binding.Zone) {
				if restoring {
					return fmt.Errorf("Cannot restore firewalld zone membership: zone %s does not exist in the %s. On the node, run firewall-cmd --permanent --new-zone=%s if the stored zone is missing, then firewall-cmd --reload. Then start the restore service named by mmx status. The reload replaces every zone's running settings with its stored settings.", binding.Zone, firewalldLayerDescriptions[layer], binding.Zone)
				}
				return fmt.Errorf("Cannot apply firewalld zone membership: zone %s does not exist in the %s. Create it with a firewalld_zone task first.", binding.Zone, firewalldLayerDescriptions[layer])
			}
			if !restoring && snapshot.Members[layer][change.Member] != change.Previous[layer] {
				return fmt.Errorf("Firewalld membership of %s changed while reachability was being checked. The proposed membership was not applied.", change.Member.Name)
			}
		}
		if change.Member.Kind == "interface" {
			zone := target[1].Zone
			if err := refuseNetworkManagerInterfaces(ctx, run, []string{change.Member.Name}, zone, zone == ""); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *firewalldMembershipActivation) Activate(ctx context.Context, run Runtime, restoring bool) error {
	current, err := inspectFirewalldMembership(ctx, run)
	if err != nil {
		return err
	}
	// Complete each member in both layers; establish all bindings before removals.
	for _, remove := range []bool{false, true} {
		for _, change := range a.data.Changes {
			target := change.Proposed
			if restoring {
				target = change.Previous
			}
			for layer, binding := range target {
				before := current.Members[layer][change.Member]
				if (binding.Zone == "") != remove || before == binding {
					continue
				}
				// A reboot can load a different spelling of the same source from
				// the stored layer. Firewalld identifies sources by their spelling.
				// Remove that spelling before restoring the saved one; a failed
				// replacement remains owed and can resume from the absent binding.
				if !remove && before.Zone != "" && before.Value != binding.Value {
					if err := firewalldWrite(ctx, run, firewalldArguments(layer, "--zone="+before.Zone, "--remove-"+change.Member.Kind+"="+before.Value)...); err != nil {
						return err
					}
				}
				args := []string{"--zone=" + binding.Zone, "--change-" + change.Member.Kind + "=" + binding.Value}
				if remove {
					args = []string{"--zone=" + before.Zone, "--remove-" + change.Member.Kind + "=" + before.Value}
				}
				if err := firewalldWrite(ctx, run, firewalldArguments(layer, args...)...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (*firewalldMembershipActivation) RemoveObsoleteVirtualDevices(context.Context, Runtime, bool) error {
	return nil
}
func (a *firewalldMembershipActivation) CheckActivatedConfiguration(ctx context.Context, run Runtime, restoring bool) error {
	snapshot, err := inspectFirewalldMembership(ctx, run)
	if err != nil {
		return err
	}
	for _, change := range a.data.Changes {
		target := change.Proposed
		if restoring {
			target = change.Previous
		}
		for layer, binding := range target {
			actual := snapshot.Members[layer][change.Member]
			if actual != binding {
				return fmt.Errorf("Firewalld membership of %s does not match in the %s: expected zone %q and spelling %q, found zone %q and spelling %q. An empty zone means no explicit membership.", change.Member.Name, firewalldLayerDescriptions[layer], binding.Zone, binding.Value, actual.Zone, actual.Value)
			}
		}
	}
	return nil
}

var firewalldZoneMembershipDescription = Description{
	Name: "firewalld_zone_membership", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(FirewalldZoneMembership{}),
	Summary: "Manage the interfaces and IP source networks of one firewalld zone, with automatic restoration when new connections cannot reach the node.",
	Fields: []Field{
		{Name: "zone", Required: true, Notes: "The zone must exist in the running firewall and stored configuration. Create a missing zone with a firewalld_zone task."},
		{Name: "interfaces", Notes: "Names of network interfaces. If NetworkManager controls an interface, use a networkmanager_connection_file task. Firewalld would otherwise rewrite that interface's profile file."},
		{Name: "source_networks", Notes: "IPv4 or IPv6 addresses or networks. MAC addresses and ipsets are not supported. List interfaces or source networks, even when empty."},
		{Name: "interfaces_not_listed", Values: notListed, Notes: "Required with interfaces. Keep unlisted interfaces or remove them from both firewall layers. Omit with state: absent; an omitted interfaces list stays unchanged."},
		{Name: "source_networks_not_listed", Values: notListed, Notes: "Required with source_networks. Keep unlisted sources or remove them from both firewall layers. Omit with state: absent; an omitted source_networks list stays unchanged. Removing unlisted MAC addresses or ipsets is refused."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "Present adds or moves members into the zone. Absent removes listed members from this zone only. After a change, the controller process must confirm reachability over new connections. Otherwise, the node restores the previous membership in the running firewall and stored configuration."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "How long the node waits after the change for the controller process to confirm reachability before restoring the previous members. Applying and checking the change uses part of this time. The wait before the change has the same limit."},
	},
	Example:   "- firewalld_zone_membership:\n    zone: internal\n    interfaces: [eth1]\n    source_networks: [10.0.0.0/8]\n    interfaces_not_listed: keep\n    source_networks_not_listed: keep\n    restore_unless_controller_confirms_within: 2m\n",
	Results:   []string{"status", "change", "reason", "value.running_firewall", "value.stored_config"},
	Dryrun:    "Report additions, moves with the previous zone, and removals in each firewall layer. Refuse interfaces controlled by NetworkManager. Refuse missing zones. Reachability after the change is not checked.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}
