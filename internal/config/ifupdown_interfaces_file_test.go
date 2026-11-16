package config

import (
	"strings"
	"testing"
)

func TestIfupdownInterfacesFileChecksItsPathAndRejectsTaskTimingWhenTheProjectLoads(t *testing.T) {
	for _, test := range []struct{ task, reason string }{
		{"{path: /etc/network/interfaces, state: absent}", "cannot remove /etc/network/interfaces"},
		{"{path: /etc/network/interfaces.d/60-second.cfg, content: example}", "source-directory reads only such names"},
		{"{path: /etc/network/other, content: example}", "source-directory reads only such names"},
		{"{path: /etc/network/interfaces.d/60-second, state: absent, content: example}", "accepts only path, state"},
		{"{path: /etc/network/interfaces, content: example, permissions_mode: invalid}", "permissions_mode"},
		{"{path: /etc/network/interfaces, content: example}\n    retry: true", "retry"},
		{"{path: /etc/network/interfaces, content: example}\n    time_limit: 1s", "time_limit"},
	} {
		path := fixture(t, "fleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - ifupdown_interfaces_file: "+test.task+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), test.reason) {
			t.Fatal(test.task, err)
		}
	}
	path := fixture(t, "fleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - ifupdown_interfaces_file: {path: /etc/network/interfaces.d/60-second, state: absent, permissions_mode: \"0600\"}\n  - ifupdown_interfaces_file: {path: /etc/network/interfaces, content: example, permissions_mode: \"0600\"}\n")
	if _, _, err := Load(path, "example", "", nil); err != nil {
		t.Fatal(err)
	}
}
