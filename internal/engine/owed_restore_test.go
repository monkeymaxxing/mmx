package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestEngineChecksReservedFileEvenWhenUnchangedOrDryrun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.conf")
	put(t, path, "same")
	for _, dryrun := range []bool{false, true} {
		e := Engine{CheckFileDestination: func(_ context.Context, destination string) error {
			if destination != path {
				t.Fatal(destination)
			}
			return fmt.Errorf("destination file is reserved by an owed restore")
		}}
		j := job(taskFile(path, "same"))
		j.Dryrun = dryrun
		r := e.Run(context.Background(), j)
		if r.Status != "failed" || !strings.Contains(r.Error, "owed restore") {
			t.Fatal(r)
		}
	}
}
func TestEngineBindsOwedRestoreServiceToTaskPathAndModule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.conf")
	var taskPath, moduleName string
	e := Engine{OwedRestoresForTask: func(path, module string, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
		taskPath, moduleName = path, module
		if secret == nil || secret() {
			t.Fatal("wrong secret trace")
		}
		return nil
	}}
	task := taskFile(path, "file contents")
	task.ID = "configure_network"
	r := e.Run(context.Background(), job(task))
	if r.Status != "succeeded" || taskPath != "configure_network" || moduleName != "file" {
		t.Fatal(r, taskPath, moduleName)
	}
}
func TestResultOmissionRetainsOwedRestoreOutcome(t *testing.T) {
	outcome := &protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", RestoreStatus: "failed", DestinationFile: "/etc/example.conf"}
	result := protocol.Result{Status: "failed", Change: "changed", Stdout: strings.Repeat("output", 100), OwedRestore: outcome}
	brief := omitDetails(result)
	if brief.Stdout != "" || brief.OwedRestore == nil || *brief.OwedRestore != *outcome {
		t.Fatal(brief)
	}
}

func TestResultOmissionRetainsNestedRestoreButDropsOrdinaryOutput(t *testing.T) {
	outcome := &protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", RestoreStatus: "succeeded"}
	protected := protocol.Result{Task: "network", Status: "failed", Change: "changed", OwedRestore: outcome}
	ordinary := protocol.Result{Task: "ordinary", Status: "succeeded", Change: "unchanged", Stdout: "large output"}
	grouped := protocol.Result{Task: "block", Items: []protocol.Result{ordinary, {Task: "nested", Items: []protocol.Result{protected}}}}
	brief := omitDetails(grouped)
	if len(brief.Items) != 1 || len(brief.Items[0].Items) != 1 || brief.Items[0].Items[0].OwedRestore == nil || *brief.Items[0].Items[0].OwedRestore != *outcome {
		t.Fatal(brief)
	}
}
