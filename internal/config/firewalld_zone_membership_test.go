package config

import (
	"encoding/json"
	"mmx/internal/modules"
	"strings"
	"testing"
)

func TestFirewalldZoneMembershipConfiguration(t *testing.T) {
	for _, task := range []string{
		`firewalld_zone: {zone: appzone}`,
		`firewalld_zone: {zone: appzone, state: absent}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, source_networks: ["2001:db8::/32", 10.0.0.1], source_networks_not_listed: remove}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [], interfaces_not_listed: remove}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [eth1], state: absent}`,
		`firewalld_zone_membership: {zone: "{{ .vars.zone }}", source_networks: ["{{ .vars.office }}"], source_networks_not_listed: keep}`,
	} {
		project, _, err := loadFirewallTask(t, "", task)
		if err != nil {
			t.Fatal(task, err)
		}
		loaded := project.FleetStages[0].NodeTasks[0]
		if loaded.Module != "firewalld_zone_membership" {
			continue
		}
		if loaded.Retry != nil && !loaded.Retry.Disabled {
			t.Fatal("membership retries", loaded.Retry)
		}
		bytes, _ := json.Marshal(loaded)
		var saved Task
		if err := json.Unmarshal(bytes, &saved); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(task, "interfaces: []") && InputsOf[modules.FirewalldZoneMembership](saved).Interfaces == nil {
			t.Fatal("saved empty list lost")
		}
	}
	for _, task := range []string{
		`firewalld_zone: {zone: "--bad"}`,
		`firewalld_zone_membership: {interfaces: [eth1], interfaces_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [eth1]}`,
		`firewalld_zone_membership: {zone: internal, interfaces_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [eth1, eth1], interfaces_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, source_networks: [10.0.0.1, 10.0.0.1/32], source_networks_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, source_networks: [10.0.0.1/8], source_networks_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, source_networks: ["ipset:foo"], source_networks_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, source_networks: ["fe80::1%eth1"], source_networks_not_listed: keep}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: keep, state: absent}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [], state: absent}`,
		`firewalld_zone_membership: {zone: internal, interfaces: [], interfaces_not_listed: keep, restore_unless_controller_confirms_within: 0s}`,
	} {
		if _, _, err := loadFirewallTask(t, "", task); err == nil {
			t.Fatal("accepted", task)
		}
	}
	for _, policy := range []string{"retry: {max_tries: 2}", "time_limit: 5s", "time_limit_per_try: 5s"} {
		if _, _, err := loadFirewallTask(t, "", `firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: keep}, `+policy); err == nil {
			t.Fatal("accepted", policy)
		}
	}
}

func TestFirewalldZoneConfigurationErrorsNameTheirFields(t *testing.T) {
	for _, test := range []struct{ task, field string }{
		{`firewalld_zone: {zone: appzone, state: broken}`, "state"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: keep, state: broken}`, "state"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [eth1]}`, "interfaces_not_listed"},
		{`firewalld_zone_membership: {zone: internal, source_networks: [10.0.0.0/8]}`, "source_networks_not_listed"},
		{`firewalld_zone_membership: {zone: internal, source_networks: [], source_networks_not_listed: broken}`, "source_networks_not_listed"},
		{`firewalld_zone_membership: {zone: internal, source_networks: [10.0.0.0/8], source_networks_not_listed: keep, state: absent}`, "source_networks_not_listed"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [], interfaces_not_listed: keep, source_networks_not_listed: remove}`, "requires source_networks"},
		{`firewalld_zone_membership: {zone: internal, source_networks: [], source_networks_not_listed: keep, interfaces_not_listed: remove}`, "requires interfaces"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: broken}`, "interfaces_not_listed"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [eth1], interfaces_not_listed: keep, state: absent}`, "interfaces_not_listed"},
		{`firewalld_zone_membership: {zone: internal, state: absent}`, "interfaces or source_networks"},
		{`firewalld_zone_membership: {zone: internal, interfaces_not_listed: keep}`, "interfaces or source_networks"},
		{`firewalld_zone_membership: {zone: internal, interfaces: [], interfaces_not_listed: keep, restore_unless_controller_confirms_within: invalid}`, "restore_unless_controller_confirms_within"},
	} {
		_, _, err := loadFirewallTask(t, "", test.task)
		if err == nil || !strings.Contains(err.Error(), test.field) {
			t.Fatalf("%s: expected an error naming %s, got %v", test.task, test.field, err)
		}
	}
}
