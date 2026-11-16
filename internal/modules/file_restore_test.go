package modules

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func prepareFileChangeForTest(t *testing.T, run Runtime, file File) *PreparedFileChange {
	t.Helper()
	prepared, result := run.PrepareFileChange(context.Background(), file, value.Scope{}, nil)
	if prepared == nil || result.Status != "succeeded" || result.Change != "would_change" {
		t.Fatal("prepare", result)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	return prepared
}

func writePreviousFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	if err := os.WriteFile(path, []byte("previous contents\x00\xff\n"), 0640); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2020, 2, 3, 4, 5, 6, 123456789, time.UTC)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestFileChangeRestoresSavedBytesAndMetadataWithoutPreparedState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	before := writePreviousFile(t, path)
	if os.Geteuid() == 0 {
		// Numeric owners need not have an account on the node.
		if err := os.Chown(path, 12345, 23456); err != nil {
			t.Fatal(err)
		}
		var err error
		before, err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	proposed := "proposed contents"
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed, Mode: "0600"})
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	metadata, err := prepared.SavePreviousFile(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	savedInfo, err := os.Stat(saved)
	if err != nil || savedInfo.Mode().Perm() != 0600 || savedInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Fatal("saved contents are not private to the saving account", savedInfo, err)
	}
	if result := prepared.Install(context.Background()); result.Status != "succeeded" || result.Change != "changed" {
		t.Fatal(result)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	// A new process has only saved metadata and bytes, not the prepared object.
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var fromRecord PreviousFileMetadata
	if err := protocol.Decode(body, &fromRecord); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		result := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, fromRecord)
		if result.Status != "succeeded" || result.Change != "changed" {
			t.Fatal(result)
		}
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != "previous contents\x00\xff\n" {
			t.Fatal("wrong restored contents", contents, err)
		}
		after, err := os.Stat(path)
		if err != nil || !before.ModTime().Equal(after.ModTime()) || permissionMode(before.Mode()) != permissionMode(after.Mode()) {
			t.Fatal("wrong restored mode or modification time", after, err)
		}
		beforeOwner, afterOwner := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
		if beforeOwner.Uid != afterOwner.Uid || beforeOwner.Gid != afterOwner.Gid {
			t.Fatal("wrong restored ownership", afterOwner)
		}
	}
}

func TestFileChangeRestoresPreviousAbsenceAndCanRepeat(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	proposed := "new file"
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed})
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	metadata, err := prepared.SavePreviousFile(context.Background(), saved)
	if err != nil || metadata.Existed {
		t.Fatal(metadata, err)
	}
	if _, err := os.Lstat(saved); !os.IsNotExist(err) {
		t.Fatal("absence created saved contents", err)
	}
	body, err := json.Marshal(metadata)
	if err != nil || string(body) != `{"existed":false}` {
		t.Fatal(string(body), err)
	}
	if result := prepared.Install(context.Background()); result.Status != "succeeded" {
		t.Fatal(result)
	}
	for _, change := range []string{"changed", "unchanged"} {
		result := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, metadata)
		if result.Status != "succeeded" || result.Change != change {
			t.Fatal(result)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("new file survived restore", err)
		}
	}
}

func TestFileChangeRemovalChecksCandidateBeforeRemovingAndRestores(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	writePreviousFile(t, path)
	checked := false
	prepared, result := (Runtime{}).PrepareFileChange(context.Background(), File{Path: path, State: "absent"}, value.Scope{}, func(_ context.Context, candidate string) error {
		checked = true
		if candidate != "" {
			t.Fatal("removal supplied candidate contents", candidate)
		}
		_, err := os.Stat(path)
		return err
	})
	if prepared == nil || !checked || result.Change != "would_change" {
		t.Fatal(result)
	}
	defer prepared.Close()
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	metadata, err := prepared.SavePreviousFile(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if result := prepared.Install(context.Background()); result.Status != "succeeded" || result.Change != "changed" {
		t.Fatal(result)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("destination was not removed", err)
	}
	if result := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, metadata); result.Status != "succeeded" {
		t.Fatal(result)
	}
}

func TestPreparedMetadataChangeReplacesInsteadOfEditingPreviousInode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	before := writePreviousFile(t, path)
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Mode: "0600"})
	if result := prepared.Install(context.Background()); result.Status != "failed" {
		t.Fatal("installed without saving previous state", result)
	}
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	metadata, err := prepared.SavePreviousFile(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if result := prepared.Install(context.Background()); result.Status != "succeeded" {
		t.Fatal(result)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("metadata change did not replace the file", after, err)
	}
	if result := prepared.Install(context.Background()); result.Status != "failed" {
		t.Fatal("installed the same prepared change twice", result)
	}
	if result := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, metadata); result.Status != "succeeded" {
		t.Fatal(result)
	}
}

func TestFileChangeRefusesStalePreviousStateAtSaveAndInstall(t *testing.T) {
	t.Parallel()
	for _, moment := range []string{"before save", "at installation"} {
		for _, alteration := range []string{"replace", "rewrite", "symlink", "permissions"} {
			t.Run(moment+"/"+alteration, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "network.conf")
				before := writePreviousFile(t, path)
				alter := func(string) error {
					switch alteration {
					case "replace", "symlink":
						if err := os.Rename(path, path+".previous"); err != nil {
							return err
						}
						if alteration == "symlink" {
							return os.Symlink(path+".previous", path)
						}
						return os.WriteFile(path, []byte("someone else's file"), 0640)
					case "rewrite":
						if err := os.WriteFile(path, []byte(strings.Repeat("x", int(before.Size()))), 0640); err != nil {
							return err
						}
						// Hash comparison must detect an edit even with the same mtime.
						return os.Chtimes(path, before.ModTime(), before.ModTime())
					default:
						return os.Chmod(path, 0600)
					}
				}
				run := Runtime{}
				if moment == "at installation" {
					run.BeforeMutation = alter
				}
				proposed := "proposed"
				prepared := prepareFileChangeForTest(t, run, File{Path: path, Literal: &proposed})
				saved := filepath.Join(t.TempDir(), "previous_file_contents")
				if moment == "before save" {
					if err := alter(path); err != nil {
						t.Fatal(err)
					}
					if _, err := prepared.SavePreviousFile(context.Background(), saved); err == nil {
						t.Fatal("saved a stale previous state")
					}
				} else {
					if _, err := prepared.SavePreviousFile(context.Background(), saved); err != nil {
						t.Fatal(err)
					}
					if result := prepared.Install(context.Background()); result.Status != "failed" {
						t.Fatal("installed over a changed destination", result)
					}
				}
				contents, err := os.ReadFile(path)
				if err != nil || string(contents) == proposed {
					t.Fatal("changed destination was overwritten", err)
				}
			})
		}
	}
}

func TestFileChangeCandidateValidationAndDryrunLeaveDestinationAlone(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"dryrun", "unchanged", "failed check", "altered candidate", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "network.conf")
			before := writePreviousFile(t, path)
			proposed := "proposed"
			if scenario == "unchanged" {
				proposed = "previous contents\x00\xff\n"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checked := false
			prepared, result := (Runtime{Dryrun: scenario == "dryrun"}).PrepareFileChange(ctx, File{Path: path, Literal: &proposed}, value.Scope{}, func(_ context.Context, candidate string) error {
				checked = true
				switch scenario {
				case "failed check":
					return errors.New("candidate configuration is invalid")
				case "altered candidate":
					return os.WriteFile(candidate, []byte("unchecked contents"), 0600)
				case "cancelled":
					cancel()
				}
				return nil
			})
			if prepared != nil {
				prepared.Close()
				t.Fatal("returned an installable change", result)
			}
			if scenario == "dryrun" || scenario == "unchanged" {
				if checked || result.Status != "succeeded" {
					t.Fatal("ran a check during dryrun or for unchanged content", result)
				}
			} else if result.Status != "failed" {
				t.Fatal("accepted failed preparation", result)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != "network.conf" {
				t.Fatal("left a candidate after preparation", entries, err)
			}
			after, err := os.Stat(path)
			if err != nil || !sameFileMetadata(before, after) {
				t.Fatal("preparation changed destination", err)
			}
		})
	}
}

func TestSavedPreviousFileCannotBeOverwrittenAndCorruptionCannotBeRestored(t *testing.T) {
	t.Parallel()
	for _, damage := range []string{"contents", "symlink", "missing"} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "network.conf")
			writePreviousFile(t, path)
			proposed := "proposed"
			prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed})
			saved := filepath.Join(t.TempDir(), "previous_file_contents")
			if err := os.WriteFile(saved, []byte("an earlier saved file"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := prepared.SavePreviousFile(context.Background(), saved); err == nil {
				t.Fatal("overwrote an earlier saved file")
			}
			if b, err := os.ReadFile(saved); err != nil || string(b) != "an earlier saved file" {
				t.Fatal("earlier saved file changed", err)
			}
			if err := os.Remove(saved); err != nil {
				t.Fatal(err)
			}
			metadata, err := prepared.SavePreviousFile(context.Background(), saved)
			if err != nil {
				t.Fatal(err)
			}
			if result := prepared.Install(context.Background()); result.Status != "succeeded" {
				t.Fatal(result)
			}
			switch damage {
			case "contents":
				err = os.WriteFile(saved, []byte("corrupted"), 0600)
			case "symlink":
				if err = os.Rename(saved, saved+".original"); err == nil {
					err = os.Symlink(saved+".original", saved)
				}
			case "missing":
				err = os.Remove(saved)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, metadata); result.Status != "failed" {
				t.Fatal("restored invalid saved contents", result)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != proposed {
				t.Fatal("failed restore touched destination", err)
			}
		})
	}
}

func TestPreviousFileMetadataRejectsIncompleteAndUnknownRecordFields(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{}`, `null`, `{"existed":null}`, `{"existed":false,"owner_user_id":0}`,
		`{"existed":false,"unknown":true}`, `{"existed":false} {}`,
		`{"existed":true}`, `{"existed":false,"access_time":"2020-01-01T00:00:00Z"}`,
	} {
		var metadata PreviousFileMetadata
		if err := protocol.Decode([]byte(body), &metadata); err == nil {
			t.Fatal("accepted invalid previous file record", body)
		}
	}
	path := filepath.Join(t.TempDir(), "network.conf")
	writePreviousFile(t, path)
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, State: "absent"})
	metadata, err := prepared.SavePreviousFile(context.Background(), filepath.Join(t.TempDir(), "previous_file_contents"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(string(body), metadata.ContentsSHA256, "bad hash", 1),
		strings.Replace(string(body), `"0640"`, `"8888"`, 1),
		strings.Replace(string(body), `"contents_size_bytes":`+"20", `"contents_size_bytes":-1`, 1),
		strings.Replace(string(body), `"modification_time":"`+metadata.ModificationTime.Format(time.RFC3339Nano)+`"`, `"modification_time":null`, 1),
	} {
		if invalid == string(body) {
			t.Fatal("test did not alter metadata", invalid)
		}
		var decoded PreviousFileMetadata
		if err := protocol.Decode([]byte(invalid), &decoded); err == nil {
			t.Fatal("accepted invalid metadata", invalid)
		}
	}
}

func TestFileRemovalReportsDirectorySyncFailureAfterUnlink(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory read permission used to fail the sync")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "network.conf")
	writePreviousFile(t, path)
	defer os.Chmod(directory, 0700)
	run := Runtime{BeforeMutation: func(string) error {
		// Unlink needs write and search permissions. Opening the directory to
		// sync it additionally needs read permission, which this removes.
		return os.Chmod(directory, 0300)
	}}
	result := run.File(context.Background(), File{Path: path, State: "absent"}, value.Scope{})
	if result.Status != "failed" || result.Change != "changed" {
		t.Fatal("did not report the sync failure after removal", result)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("test did not remove the file before the failure", err)
	}
}

// A package often installs a link where a project removes a file, such as
// nginx's sites-enabled/default; the refusal names the module for links, in
// a run and in a node task block's hash of the file.
func TestFileRefusesALinkAndNamesTheSymlinkModule(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path, target := filepath.Join(directory, "default"), filepath.Join(directory, "target")
	writePreviousFile(t, target)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	want := path + " is a symbolic link, and file manages only regular files; manage or remove a link with the symlink module (state: absent removes it)"
	file := File{Path: path, State: "absent"}
	if result := (Runtime{}).File(context.Background(), file, value.Scope{}); result.Status != "failed" || result.Reason != want {
		t.Fatal(result)
	}
	if err := file.HashConfiguration(context.Background(), sha256.New(), false, value.Scope{}); err == nil || err.Error() != want {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("removed the link", err)
	}
}

func TestFileChangeRefusesNonregularDestinationAtPreparationAndRestore(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			path, target := filepath.Join(directory, "network.conf"), filepath.Join(directory, "target")
			writePreviousFile(t, target)
			var err error
			if kind == "symlink" {
				err = os.Symlink(target, path)
			} else {
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			proposed := "proposed"
			for _, file := range []File{{Path: path, Literal: &proposed}, {Path: path, State: "absent"}} {
				prepared, result := (Runtime{}).PrepareFileChange(context.Background(), file, value.Scope{}, nil)
				if prepared != nil || result.Status != "failed" {
					t.Fatal("prepared a non-regular destination", result)
				}
			}
			result := (Runtime{}).RestorePreviousFile(context.Background(), path, "", PreviousFileMetadata{})
			if result.Status != "failed" {
				t.Fatal("removed a non-regular destination", result)
			}
			if contents, err := os.ReadFile(target); err != nil || string(contents) != "previous contents\x00\xff\n" {
				t.Fatal("followed the symlink", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("removed the conflicting entry", err)
			}
		})
	}
}

func TestFileChangeFailedOrCancelledSaveCannotEnableInstallation(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "network.conf")
		writePreviousFile(t, path)
		proposed := "proposed"
		prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed})
		ctx, cancel := context.WithCancel(context.Background())
		saved := filepath.Join(t.TempDir(), "missing", "previous_file_contents")
		if cancelled {
			cancel()
			saved = filepath.Join(t.TempDir(), "previous_file_contents")
		}
		_, err := prepared.SavePreviousFile(ctx, saved)
		cancel()
		if err == nil {
			t.Fatal("accepted failed save")
		}
		if result := prepared.Install(context.Background()); result.Status != "failed" {
			t.Fatal("installed after failed save", result)
		}
		if contents, err := os.ReadFile(path); err != nil || string(contents) == proposed {
			t.Fatal("changed destination after failed save", err)
		}
		if _, err := os.Lstat(saved); !os.IsNotExist(err) {
			t.Fatal("left saved contents after failed save", err)
		}
	}
}

type cancelAfterSavedBytes struct {
	context.Context
	path     string
	sawBytes bool
}

func (c *cancelAfterSavedBytes) Err() error {
	if info, err := os.Stat(c.path); err == nil && info.Size() > 0 {
		c.sawBytes = true
		return context.Canceled
	}
	return nil
}

func TestFileChangeRemovesPartialSaveAndStillRefusesInstallation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	previous := strings.Repeat("previous", 65536)
	if err := os.WriteFile(path, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	proposed := "proposed"
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed})
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	ctx := &cancelAfterSavedBytes{Context: context.Background(), path: saved}
	if _, err := prepared.SavePreviousFile(ctx, saved); !errors.Is(err, context.Canceled) || !ctx.sawBytes {
		t.Fatal("did not fail after writing saved bytes", err, ctx.sawBytes)
	}
	if _, err := os.Lstat(saved); !os.IsNotExist(err) {
		t.Fatal("partial save remains", err)
	}
	if result := prepared.Install(context.Background()); result.Status != "failed" {
		t.Fatal(result)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != previous {
		t.Fatal("destination changed", err)
	}
}

func TestFileChangeRemovesInterruptedCandidateWithoutPreparedObject(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	writePreviousFile(t, path)
	proposed := "proposed"
	prepared, result := (Runtime{}).PrepareFileChange(context.Background(), File{Path: path, Literal: &proposed}, value.Scope{}, nil)
	if prepared == nil {
		t.Fatal(result)
	}
	candidate := filepath.Join(filepath.Dir(path), prepared.candidate)
	// A dead process closes its descriptors, without calling PreparedFileChange.Close.
	if err := prepared.parent.Close(); err != nil {
		t.Fatal(err)
	}
	prepared = nil
	ordinary := filepath.Join(filepath.Dir(path), ".mmx-candidate-unrelated")
	if err := os.Mkdir(ordinary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFileChangeCandidates(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatal("interrupted candidate remains", err)
	}
	if _, err := os.Stat(ordinary); err != nil {
		t.Fatal("removed ordinary candidate", err)
	}
}

func TestRepeatedRestoreRemovesInterruptedPreviousCandidate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "network.conf")
	writePreviousFile(t, path)
	proposed := "proposed"
	prepared := prepareFileChangeForTest(t, Runtime{}, File{Path: path, Literal: &proposed})
	saved := filepath.Join(t.TempDir(), "previous_file_contents")
	metadata, err := prepared.SavePreviousFile(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if r := prepared.Install(context.Background()); r.Status != "succeeded" {
		t.Fatal(r)
	}
	stale := filepath.Join(filepath.Dir(path), fileChangeCandidateDirectory(path, true))
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "incomplete"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).RestorePreviousFile(context.Background(), path, saved, metadata); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("restore candidate remains", err)
	}
}

func TestRepeatedRestoreSyncsAnEarlierUnlink(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory read permission used to fail the sync")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "network.conf")
	writePreviousFile(t, path)
	defer os.Chmod(directory, 0700)
	run := Runtime{BeforeMutation: func(string) error { return os.Chmod(directory, 0300) }}
	result := run.RestorePreviousFile(context.Background(), path, "", PreviousFileMetadata{})
	if result.Status != "failed" || result.Change != "changed" {
		t.Fatal(result)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("unlink did not succeed", err)
	}
	// The repeated operation must sync even though the file is already absent.
	if r := (Runtime{}).RestorePreviousFile(context.Background(), path, "", PreviousFileMetadata{}); r.Status != "failed" {
		t.Fatal(r)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).RestorePreviousFile(context.Background(), path, "", PreviousFileMetadata{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}
