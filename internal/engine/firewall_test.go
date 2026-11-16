package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// A firewall rule with from_inventory_group reaches the node as one rule for
// each node of the group, from the .inventory values the job carries; a node
// that left the group loses its rule with rules_not_listed: remove.
func TestFirewallRuleForEachNodeOfAnInventoryGroup(t *testing.T) {
	bin := t.TempDir()
	rich := filepath.Join(bin, "rich-rules")
	put(t, rich, `rule family="ipv4" source address="10.0.1.13" service name="postgresql" accept`+"\n")
	// One list of rich rules stands for both of zone public's layers.
	fake := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    --state) echo running;;
    --info-service=ssh) printf 'ssh\n  ports: 22/tcp\n';;
    --list-services) echo ssh;;
    --list-ports) echo;;
    --list-rich-rules) cat ` + rich + `;;
    --add-rich-rule=*) grep -qxF -- "${a#*=}" ` + rich + ` || printf '%s\n' "${a#*=}" >> ` + rich + `;;
    --remove-rich-rule=*) grep -vxF -- "${a#*=}" ` + rich + ` > ` + rich + `.new; mv ` + rich + `.new ` + rich + `;;
  esac
done
`
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "firewall-cmd"), fake)
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "sshd"), "#!/bin/sh\nprintf 'listenaddress 0.0.0.0:22\\n'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	inventory := config.Inventory{
		Nodes: map[string]config.Node{
			"app01": {Address: "203.0.113.11", Vars: map[string]any{"private_address": "10.0.1.11"}},
			"app02": {Address: "203.0.113.12", Vars: map[string]any{"private_address": "10.0.1.12"}},
			"db01":  {Address: "203.0.113.20"},
		},
		Groups: map[string]config.NodeGroup{"app": {Nodes: []string{"app01", "app02"}}},
	}
	task := config.Task{Module: "firewalld", ModuleInputs: &modules.Firewalld{Zone: "{{ .vars.zone }}", RulesNotListed: "remove", Rules: []modules.FirewalldRule{
		{Action: "allow", FirewalldService: "ssh"},
		{Action: "allow", FirewalldService: "postgresql", FromInventoryGroup: &modules.FromInventoryGroup{Group: "app", AddressVar: "private_address"}},
	}}}
	j := job(task)
	j.Vars = map[string]any{"zone": "public"}
	j.Scope = map[string]any{"inventory": config.InventoryValues(inventory)}
	e := Engine{RuntimeRoot: filepath.Join(t.TempDir(), "state")}
	want := map[string]any{
		"added": []any{`rule family="ipv4" source address="10.0.1.11" service name="postgresql" accept`,
			`rule family="ipv4" source address="10.0.1.12" service name="postgresql" accept`},
		"removed": []any{`rule family="ipv4" source address="10.0.1.13" service name="postgresql" accept`},
	}

	j.Dryrun = true
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if got := r.Results[0]; got.Change != "would_change" || !reflect.DeepEqual(got.Value, want) {
		t.Fatalf("%+v", got)
	}
	j.Dryrun = false
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if got := r.Results[0]; got.Change != "changed" || !reflect.DeepEqual(got.Value, want) {
		t.Fatalf("%+v", got)
	}
	if got := read(t, rich); got != want["added"].([]any)[0].(string)+"\n"+want["added"].([]any)[1].(string)+"\n" {
		t.Fatalf("%q", got)
	}
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "unchanged" {
		t.Fatalf("%+v", r.Results[0])
	}
	// The written task keeps its group, so every node of a fleet stage
	// expands it for itself.
	if config.InputsOf[modules.Firewalld](task).Rules[1].From != "" || len(config.InputsOf[modules.Firewalld](task).Rules) != 2 || config.InputsOf[modules.Firewalld](task).Zone != "{{ .vars.zone }}" {
		t.Fatal(config.InputsOf[modules.Firewalld](task))
	}
}
