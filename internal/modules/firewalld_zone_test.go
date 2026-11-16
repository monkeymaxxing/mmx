package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirewalldZoneLifecycleAndReload(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	ctx := testContext(t)
	input := FirewalldZone{Zone: "appzone"}
	fixture.state["running"]["public"]["source"] = []string{"192.0.2.1"}
	fixture.save(t)
	preview := input.Run(ctx, NodeTask{Runtime: Runtime{Dryrun: true}})
	if preview.Change != "would_change" || !strings.Contains(preview.Reason, "Would create firewalld zone appzone") || !strings.Contains(preview.Reason, "The task would reload firewalld.") {
		t.Fatal(preview)
	}
	fixture.read(t)
	if _, exists := fixture.state["stored"]["appzone"]; exists {
		t.Fatal("dryrun created zone")
	}
	result := input.Run(ctx, NodeTask{})
	if result.Change != "changed" || !strings.Contains(result.Reason, "Created firewalld zone appzone") || !strings.Contains(result.Reason, "The task reloaded firewalld.") {
		t.Fatal(result)
	}
	fixture.assert(t, "running", "public", "source")
	if result = input.Run(ctx, NodeTask{}); result.Change != "unchanged" {
		t.Fatal(result)
	}
	fixture.state["running"]["appzone"]["interface"] = []string{"eth1"}
	fixture.save(t)
	input.State = "absent"
	for _, dryrun := range []bool{true, false} {
		result = input.Run(ctx, NodeTask{Runtime: Runtime{Dryrun: dryrun}})
		if result.Status != "failed" || !strings.Contains(result.Reason, "eth1") || !strings.Contains(result.Reason, "firewalld_zone_membership") {
			t.Fatal(result)
		}
	}
	fixture.state["running"]["appzone"]["interface"] = []string{}
	fixture.save(t)
	preview = input.Run(ctx, NodeTask{Runtime: Runtime{Dryrun: true}})
	if preview.Change != "would_change" || !strings.Contains(preview.Reason, "Would remove firewalld zone appzone") || !strings.Contains(preview.Reason, "The task would reload firewalld.") {
		t.Fatal(preview)
	}
	if result = input.Run(ctx, NodeTask{}); result.Change != "changed" || !strings.Contains(result.Reason, "Removed firewalld zone appzone") || !strings.Contains(result.Reason, "The task reloaded firewalld.") {
		t.Fatal(result)
	}
	fixture.read(t)
	for _, layer := range []string{"running", "stored"} {
		if _, exists := fixture.state[layer]["appzone"]; exists {
			t.Fatal("zone remains")
		}
	}
	if result = input.Run(ctx, NodeTask{}); result.Change != "unchanged" {
		t.Fatal(result)
	}
}
func TestFirewalldZoneRepairsOneLayerAndPassesNativeRefusal(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"running", "stored"} {
		t.Run(missing, func(t *testing.T) {
			fixture := newMembershipFixture(t)
			delete(fixture.state[missing], "internal")
			fixture.save(t)
			input := FirewalldZone{Zone: "internal"}
			result := input.Run(testContext(t), NodeTask{})
			if result.Status != "succeeded" || result.Change != "changed" {
				t.Fatal(result)
			}
			if strings.Contains(fixture.calls(t), "--reload") != (missing == "running") {
				t.Fatal(fixture.calls(t))
			}
			layer := "stored configuration"
			if missing == "running" {
				layer = "running firewall"
			}
			if !strings.Contains(result.Reason, "Created firewalld zone internal in the "+layer+".") || strings.Contains(result.Reason, "The task reloaded firewalld.") != (missing == "running") {
				t.Fatal(result)
			}
		})
	}
	fixture := newMembershipFixture(t)
	writeState(t, filepath.Join(fixture.root, "fail"), "--delete-zone=internal")
	result := (&FirewalldZone{Zone: "internal", State: "absent"}).Run(testContext(t), NodeTask{})
	if result.Status != "failed" || !strings.Contains(result.Reason, "native firewalld refusal") {
		t.Fatal(result)
	}
	os.Remove(filepath.Join(fixture.root, "fail"))
	writeState(t, filepath.Join(fixture.root, "stopped"), "")
	result = (&FirewalldZone{Zone: "internal"}).Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: true}})
	if result.Status != "not_checked" || !strings.Contains(result.Reason, "start it with a systemd_service task") {
		t.Fatal(result)
	}
}

func TestFirewalldZoneRefusesWhileMembershipRestoreIsOwed(t *testing.T) {
	t.Parallel()
	newMembershipFixture(t)
	service := &membershipServiceForTest{refusal: fmt.Errorf("A membership restore is still owed.")}
	for _, dryrun := range []bool{false, true} {
		result := (&FirewalldZone{Zone: "appzone"}).Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, "still owed") {
			t.Fatal(result)
		}
	}
}
