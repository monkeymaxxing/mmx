package modules

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"mmx/internal/protocol"
)

// FirewalldZone creates or deletes the zone itself. Its reload deliberately
// replaces runtime configuration with the stored configuration of every zone.
type FirewalldZone struct {
	Zone  string `yaml:"zone" json:"zone"`
	State string `yaml:"state,omitempty" json:"state,omitempty"`
}

func (f *FirewalldZone) Prepare(string) error {
	return validatePresence(f.State)
}
func (f *FirewalldZone) Arguments(arguments *ArgumentList) {
	arguments.Public("zone", &f.Zone, membershipZoneRule)
}
func (f *FirewalldZone) Run(ctx context.Context, task NodeTask) protocol.Result {
	run := task.Runtime
	if err := firewalldPrerequisites(ctx, run); err != nil {
		return run.notYet(err)
	}
	// A reload must not disturb a membership restore retained by an earlier job.
	if task.OwedRestores != nil {
		if err := task.OwedRestores.CheckChangeAllowed(ctx, ""); err != nil {
			return membershipRefusal(err)
		}
	}
	snapshot, err := inspectFirewalldMembership(ctx, run)
	if err != nil {
		return membershipRefusal(err)
	}
	present := [2]bool{slices.Contains(snapshot.Zones[0], f.Zone), slices.Contains(snapshot.Zones[1], f.Zone)}
	want := f.State != "absent"
	if !want {
		var members []string
		for layer, bindings := range snapshot.Members {
			for member, binding := range bindings {
				if binding.Zone == f.Zone {
					members = append(members, fmt.Sprintf("%s %s in the %s", member.Kind, member.Name, firewalldLayerDescriptions[layer]))
				}
			}
		}
		sort.Strings(members)
		if len(members) > 0 {
			return membershipRefusal(fmt.Errorf("Cannot remove firewalld zone %s while it has members: %s. Remove its interfaces and IP source networks with a firewalld_zone_membership task first.", f.Zone, strings.Join(members, "; ")))
		}
	}
	if present[0] == want && present[1] == want {
		result := protocol.Success("unchanged")
		result.Reason = fmt.Sprintf("Firewalld zone %s already has the requested state in both firewall layers. Firewalld was not reloaded.", f.Zone)
		return result
	}
	reload := !want || !present[0]
	var layers []string
	for layer := range present {
		if present[layer] != want {
			layers = append(layers, "the "+firewalldLayerDescriptions[layer])
		}
	}
	verb, past, preposition := "create", "Created", "in"
	if !want {
		verb, past, preposition = "remove", "Removed", "from"
	}
	reason := fmt.Sprintf("%s firewalld zone %s %s %s.", past, f.Zone, preposition, strings.Join(layers, " and "))
	if reload {
		reason += " The task reloaded firewalld. Every zone's running settings were replaced by its stored settings. Settings that existed only in the running firewall were dropped. Stored settings that were not running became active."
	}
	if run.Dryrun {
		result := protocol.Success("would_change")
		result.Reason = fmt.Sprintf("Would %s firewalld zone %s %s %s.", verb, f.Zone, preposition, strings.Join(layers, " and "))
		if reload {
			result.Reason += " The task would reload firewalld. Every zone's running settings would be replaced by its stored settings. Settings that exist only in the running firewall would be dropped. Stored settings that are not running would become active."
		}
		return result
	}
	change := "unknown"
	failed := func(err error) protocol.Result { result := protocol.Failed(err); result.Change = change; return result }
	if present[1] != want {
		verb := "new"
		if !want {
			verb = "delete"
		}
		if err := firewalldWrite(ctx, run, "--permanent", "--"+verb+"-zone="+f.Zone); err != nil {
			return failed(err)
		}
		change = "changed"
	}
	if reload {
		if err := firewalldWrite(ctx, run, "--reload"); err != nil {
			return failed(err)
		}
		change = "changed"
	}
	for layer := 0; layer < 2; layer++ {
		zones, err := firewalldRead(ctx, run, firewalldArguments(layer, "--get-zones")...)
		if err != nil {
			return failed(err)
		}
		if slices.Contains(strings.Fields(zones), f.Zone) != want {
			return failed(fmt.Errorf("Firewalld zone %s did not reach the requested state in the %s.", f.Zone, firewalldLayerDescriptions[layer]))
		}
	}
	result := protocol.Success("changed")
	result.Reason = reason
	return result
}

var firewalldZoneDescription = Description{
	Name: "firewalld_zone", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(FirewalldZone{}),
	Summary: "Create or remove a firewalld zone. When needed, the task reloads firewalld. A reload replaces every zone's running settings with its stored settings. Settings that exist only in the running firewall are dropped. Stored settings that are not running become active.",
	Fields: []Field{
		{Name: "zone", Required: true, Notes: "The name of the firewalld zone."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "Present creates the zone in stored configuration if missing. The task reloads firewalld if the running zone is missing. Absent refuses a zone with members in the running firewall or stored configuration. Otherwise, it deletes the stored zone and reloads firewalld. Firewalld can refuse deletion of a built-in zone."},
	},
	Example: "- firewalld_zone:\n    zone: appzone\n    state: present\n",
	Results: []string{"status", "change", "reason"},
	Dryrun:  "Report the proposed creation or removal and whether firewalld would reload. A reload would replace running settings with stored settings in every zone.",
}
