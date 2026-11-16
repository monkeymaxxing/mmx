package main

import (
	"bytes"
	"mmx/internal/controller"
	"mmx/internal/protocol"
	"strings"
	"testing"
)

func TestHumanMembershipRestoreNeverClaimsAFile(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "running", "required"} {
		restore := protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", ModuleName: "firewalld_zone_membership", ChangeSubject: "firewalld zone membership", RestoreStatus: status, RestoreServiceUnit: "mmx-owed-restore-0123456789abcdef.service"}
		run := &controller.Run{Entries: []controller.Entry{{FleetStage: "firewall", Node: "web01", Response: protocol.Response{Status: "failed", OwedRestores: []protocol.OwedRestore{restore}}}}}
		var output bytes.Buffer
		if err := (&presentation{mode: "nodes"}).human(&output, run, true); err != nil {
			t.Fatal(err)
		}
		text := output.String()
		if strings.Contains(text, "file") || !strings.Contains(text, "membership change 0123456789abcdef") || !strings.Contains(text, "firewalld zone membership") {
			t.Fatal(text)
		}
		if status == "failed" && !strings.Contains(text, "Previous membership restored in both firewall layers: no.") {
			t.Fatal(text)
		}
	}
}
