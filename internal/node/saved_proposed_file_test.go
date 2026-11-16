package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// activationWithSavedFilesForTest notes the saved files it is handed and
// their bytes at that moment.
type activationWithSavedFilesForTest struct {
	activationForTest
	mu     sync.Mutex
	handed [][4]string
}

func (a *activationWithSavedFilesForTest) UseSavedFiles(previous, proposed string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	read := func(path string) string {
		if path == "" {
			return ""
		}
		b, _ := os.ReadFile(path)
		return string(b)
	}
	a.handed = append(a.handed, [4]string{previous, proposed, read(previous), read(proposed)})
}

func TestSavedProposedFileReachesActivationAndRestoreAndIsRemoved(t *testing.T) {
	for _, test := range []struct {
		name                   string
		withSavedFiles, remove bool
	}{
		{"activation that uses saved files", true, false},
		{"removal", true, true},
		{"ordinary activation", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			withSavedFiles := &activationWithSavedFilesForTest{}
			if test.withSavedFiles {
				f.server.activationFactory = func(string, string, json.RawMessage) (modules.ChangeActivation, error) { return withSavedFiles, nil }
			}
			previous, proposed := "previous", "proposed"
			if err := os.WriteFile(f.destination, []byte(previous), 0640); err != nil {
				t.Fatal(err)
			}
			file := modules.File{Path: f.destination, Literal: &proposed}
			if test.remove {
				file = modules.File{Path: f.destination, State: "absent"}
			}
			prepared, result := (modules.Runtime{}).PrepareFileChange(context.Background(), file, value.Scope{}, nil)
			if prepared == nil {
				t.Fatal(result)
			}
			defer prepared.Close()
			service := f.server.changeService(f.jobID, "configure", "file", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			done := make(chan protocol.Result, 1)
			go func() {
				done <- service.ApplyChange(ctx, modules.ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{}`)})
			}()
			r := waitForRestorePhase(t, f.server, waitingBeforeChange)
			if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
				t.Fatal(a)
			}
			r = waitForRestorePhase(t, f.server, activatingConfig)
			directory := f.server.restoreDirectory(r.Outcome.OwedRestoreID)
			savedPrevious, savedProposed := filepath.Join(directory, "previous_file_contents"), filepath.Join(directory, "proposed_file_contents")
			info, err := os.Lstat(savedProposed)
			if test.withSavedFiles && !test.remove {
				if err != nil || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
					t.Fatal("proposed copy", info, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("saved a proposed copy that no activation asked for", err)
			}
			if err := f.server.ActivateChange(ctx, r.Outcome.OwedRestoreID); err != nil {
				t.Fatal(err)
			}
			// Without a confirmation after the change, the restore runs.
			f.clock.advance(2 * time.Minute)
			if err := f.server.RunOwedRestore(ctx, r.Outcome.OwedRestoreID); err != nil {
				t.Fatal(err)
			}
			if result := <-done; result.Status != "failed" || result.Change != "changed" {
				t.Fatal(result)
			}
			if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != previous {
				t.Fatal("previous file not restored", string(contents), err)
			}
			for _, saved := range []string{savedPrevious, savedProposed} {
				if _, err := os.Lstat(saved); !os.IsNotExist(err) {
					t.Fatal("cleanup kept", saved, err)
				}
			}
			if !test.withSavedFiles {
				return
			}
			handed := [4]string{savedPrevious, savedProposed, previous, proposed}
			if test.remove {
				handed = [4]string{savedPrevious, "", previous, ""}
			}
			withSavedFiles.mu.Lock()
			defer withSavedFiles.mu.Unlock()
			if len(withSavedFiles.handed) != 2 || withSavedFiles.handed[0] != handed || withSavedFiles.handed[1] != handed {
				t.Fatal("activation and restore were not both handed the saved files", withSavedFiles.handed)
			}
		})
	}
}
