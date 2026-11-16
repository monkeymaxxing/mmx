package main

import (
	"bytes"
	"context"
	"mmx/internal/node"
	"mmx/internal/protocol"
	"strings"
	"testing"
)

func TestOwedRestoreCommandsRejectPathTraversalAndMissingIdentity(t *testing.T) {
	for _, command := range []string{"activate-change", "run-owed-restore"} {
		for _, args := range [][]string{{command}, {command, "0123456789abcdef"}, {command, "--owed-restore-id", "../../record"}, {command, "--owed-restore-id", "0123456789ABCDEF"}, {command, "--owed-restore-id", "0123456789abcdef", "extra"}} {
			if _, err := owedRestoreCommandID(args); err == nil {
				t.Fatal(args)
			}
		}
		if id, err := owedRestoreCommandID([]string{command, "--owed-restore-id", "0123456789abcdef"}); err != nil || id != "0123456789abcdef" {
			t.Fatal(id, err)
		}
	}
}

func TestSSHReachabilityCommandSuppliesSSHKindAndRejectsClaimedKind(t *testing.T) {
	server := node.Server{Root: t.TempDir()}
	for _, extra := range []string{"", `,"received_connection_kind":"https"`} {
		body := `{"node_job_id":"` + strings.Repeat("a", 64) + `","owed_restore_id":"0123456789abcdef","confirmation_phase":"before_change"` + extra + `}`
		var output bytes.Buffer
		err := answerOverSSH(context.Background(), server, protocol.RequestConfirmReachability, strings.NewReader(body), &output)
		if extra != "" {
			if err == nil {
				t.Fatal("accepted claimed connection kind")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var answer protocol.ReachabilityConfirmationResponse
		if err := protocol.Decode(output.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		if answer.ReceivedConnectionKind != protocol.ConnectionSSH || answer.ConfirmationCounted {
			t.Fatal(answer)
		}
	}
}
