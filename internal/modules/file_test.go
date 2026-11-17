package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/value"
)

func TestSymlinkChecksReservedDestinationBeforePreviewOrUnchanged(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"regular file", "matching symlink"} {
		for _, dryrun := range []bool{false, true} {
			t.Run(existing+map[bool]string{false: "/run", true: "/dryrun"}[dryrun], func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "reserved")
				var err error
				if existing == "regular file" {
					err = os.WriteFile(path, []byte("previous"), 0600)
				} else {
					err = os.Symlink("target", path)
				}
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				runtime := Runtime{Dryrun: dryrun, CheckFileDestination: func(_ context.Context, destination string) error {
					calls++
					if destination != path {
						t.Fatalf("checked %s, want %s", destination, path)
					}
					return errors.New("destination still owes a restore")
				}}
				result := runtime.Symlink(context.Background(), Symlink{Path: path, Target: "target", ReplaceExistingFile: true})
				if result.Status != "failed" || result.Reason != "destination still owes a restore" || calls != 1 {
					t.Fatal(result, calls)
				}
			})
		}
	}
}

func TestFileMetadataPreservesContentsAndIdentity(t *testing.T) {
	t.Parallel()
	for _, withContent := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata only", true: "unchanged content"}[withContent], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			content := "keep these bytes\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			f := File{Path: path, Mode: "0640"}
			if withContent {
				f.Content = &content
			}
			for _, dryrun := range []bool{true, false} {
				r := (Runtime{Dryrun: dryrun}).File(context.Background(), f, value.Scope{})
				want := "changed"
				if dryrun {
					want = "would_change"
				}
				if r.Status != "succeeded" || r.Change != want {
					t.Fatal(r)
				}
				after, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
					t.Fatal("metadata change replaced or rewrote the file")
				}
				mode := os.FileMode(0640)
				if dryrun {
					mode = 0600
				}
				if after.Mode().Perm() != mode {
					t.Fatal(after.Mode())
				}
				b, err := os.ReadFile(path)
				if err != nil || string(b) != content {
					t.Fatal("content changed", err)
				}
			}
			if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Change != "unchanged" {
				t.Fatal(r)
			}
		})
	}
}

func TestFileMetadataRejectsMissingAndReplacedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	f := File{Path: path, Mode: "0644"}
	if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created a file for metadata-only request")
	}
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	run := Runtime{BeforeMutation: func(string) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Symlink(target, path)
	}}
	if r := run.File(context.Background(), f, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("followed a replacement symlink", err)
	}
}

func TestFileMetadataStillRequiresCandidateValidation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config")
	content := "unchanged content"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	f := File{Path: path, Content: &content, Mode: "0644", CheckNewFile: &Check{Shell: &Shell{Script: "exit 1"}}}
	r := (Runtime{}).File(context.Background(), f, value.Scope{})
	if r.Status != "failed" || r.FailureKind != "validation" {
		t.Fatal(r)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("rejected validator changed metadata", err)
	}
	f.CheckNewFile.Shell.Script = "exit 0"
	r = (Runtime{}).File(context.Background(), f, value.Scope{})
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	after, err := os.Stat(path)
	if err != nil || after.Mode().Perm() != 0644 || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("successful validation did not preserve the existing file", err)
	}
}

func TestCheckNewFileReceivesNewFilePathBeforeReplacement(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "managed")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	content := "new"
	task := File{Mode: "0644", Path: path, Content: &content, CheckNewFile: &Check{Shell: &Shell{
		Script: `test "$(cat "$NEW_FILE")" = new && test "$(cat "$DESTINATION")" = old`,
		Env:    map[string]any{"NEW_FILE": "{{ .new_file.path }}", "DESTINATION": path},
	}}}
	result := (Runtime{}).File(context.Background(), task, value.Scope{})
	installed, err := os.ReadFile(path)
	if result.Status != "succeeded" || string(installed) != "new" || err != nil {
		t.Fatal(result, string(installed), err)
	}
}

// keep_existing_content creates a missing file and afterwards applies only
// metadata, whatever the task's content says.
func TestFileKeepsExistingContent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sentinel.conf")
	content := "first content\n"
	f := File{Path: path, Content: &content, Mode: "0640", IfFileExists: "keep_existing_content"}
	preview := (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
	if preview.Change != "would_change" || preview.FileChange == nil || preview.FileChange.Action != "create" {
		t.Fatal(preview)
	}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	// The program rewrites its own file; the task leaves those bytes alone.
	if err := os.WriteFile(path, []byte("rewritten by the program\n"), 0640); err != nil {
		t.Fatal(err)
	}
	other := "other content\n"
	f.Content = &other
	f.CheckNewFile = &Check{Shell: &Shell{Script: "exit 1"}}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).File(context.Background(), f, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(dryrun, r)
		}
	}
	f.Mode = "0600"
	preview = (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
	if preview.Change != "would_change" || preview.FileChange.Diff != "" || preview.FileChange.After.Size != int64(len("rewritten by the program\n")) {
		t.Fatal(preview, preview.FileChange)
	}
	if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Change != "changed" {
		t.Fatal(r)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 || read(t, path) != "rewritten by the program\n" {
		t.Fatal(info, err)
	}
}

// content_from_node_paths joins files on the node. Results show SHA-256
// values instead of the text, which may be a private key. Links are in
// TestNodeFileReadsFollowOnlyLinksThatRootOwns.
func TestFileContentFromNodePaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("fullchain.pem", "CERTIFICATE\n")
	put("privkey.pem", "PRIVATE KEY\n")
	path := filepath.Join(dir, "site.pem")
	put("site.pem", "old\n")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	f := File{Path: path, Mode: "0644", ContentFromNodePaths: []string{filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")}}
	preview := (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
	c := preview.FileChange
	if preview.Change != "would_change" || c == nil || c.Diff != "" || c.FullDiff != nil || !strings.Contains(c.DiffOmitted, "sha256") ||
		c.Before.SHA256 != sha("old\n") || c.After.SHA256 != sha("CERTIFICATE\nPRIVATE KEY\n") {
		t.Fatal(preview, c)
	}
	if read(t, path) != "old\n" {
		t.Fatal("dryrun changed the file")
	}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if read(t, path) != "CERTIFICATE\nPRIVATE KEY\n" {
		t.Fatal(read(t, path))
	}
	// A renewed certificate changes the file on the next run.
	put("fullchain.pem", "RENEWED\n")
	if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Change != "changed" || read(t, path) != "RENEWED\nPRIVATE KEY\n" {
		t.Fatal(r)
	}
	// A missing path is one an earlier task may create: not checked in dryrun.
	missing := f
	missing.ContentFromNodePaths = []string{filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "missing.pem")}
	if r := (Runtime{Dryrun: true}).File(context.Background(), missing, value.Scope{}); r.Status != "not_checked" {
		t.Fatal(r)
	}
	if r := (Runtime{}).File(context.Background(), missing, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "missing.pem") {
		t.Fatal(r)
	}
	// A directory or a named pipe is no regular file; the pipe never blocks.
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{dir, pipe} {
		wrong := f
		wrong.ContentFromNodePaths = []string{bad}
		if r := (Runtime{}).File(context.Background(), wrong, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "not a regular file") {
			t.Fatal(bad, r)
		}
	}
	if read(t, path) != "RENEWED\nPRIVATE KEY\n" {
		t.Fatal("a failed task changed the file")
	}
}

// backup_original_file_to copies the file once, with its metadata, before MMX
// first changes it, and never writes the copy again.
func TestFileBacksUpTheOriginalOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sshd_config")
	backup := filepath.Join(dir, "sshd_config.original")
	if err := os.WriteFile(path, []byte("distribution\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0604); err != nil {
		t.Fatal(err)
	}
	content := "managed 1\n"
	f := File{Mode: "0644", Path: path, Content: &content, BackupOriginalFileTo: backup}
	preview := (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
	if preview.Change != "would_change" || preview.FileChange.OriginalFileCopiedTo != backup {
		t.Fatal(preview, preview.FileChange)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatal("dryrun made the copy", err)
	}
	// The copy is made, then replacing the file fails: the copy stays, and the
	// next try replaces the file without copying again.
	refused := Runtime{BeforeMutation: func(resource string) error {
		if resource == path {
			return os.ErrPermission
		}
		return nil
	}}
	if r := refused.File(context.Background(), f, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
	if read(t, backup) != "distribution\n" || read(t, path) != "distribution\n" {
		t.Fatal("copy or file wrong after the refused replacement")
	}
	r := (Runtime{}).File(context.Background(), f, value.Scope{})
	if r.Change != "changed" || r.FileChange.OriginalFileCopiedTo != "" || read(t, path) != "managed 1\n" {
		t.Fatal(r, r.FileChange)
	}
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0604 {
		t.Fatal("copy lost the mode", info, err)
	}
	was, _ := os.Stat(path)
	if was.Sys().(*syscall.Stat_t).Uid != info.Sys().(*syscall.Stat_t).Uid {
		t.Fatal("copy lost the owner")
	}
	next := "managed 2\n"
	f.Content = &next
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Change != want {
			t.Fatal(want, r)
		}
	}
	if read(t, backup) != "distribution\n" {
		t.Fatal("the copy was written again")
	}
	// A file that does not exist yet has no original to keep.
	fresh := File{Mode: "0644", Path: filepath.Join(dir, "new.conf"), Content: &content, BackupOriginalFileTo: filepath.Join(dir, "new.conf.original")}
	if r := (Runtime{}).File(context.Background(), fresh, value.Scope{}); r.Change != "changed" {
		t.Fatal(r)
	}
	if _, err := os.Lstat(fresh.BackupOriginalFileTo); !os.IsNotExist(err) {
		t.Fatal("copied a file that did not exist", err)
	}
	// The first change made by a later task copies the file as it is then.
	other := "other\n"
	fresh.Content = &other
	if r := (Runtime{}).File(context.Background(), fresh, value.Scope{}); r.Change != "changed" || r.FileChange.OriginalFileCopiedTo != fresh.BackupOriginalFileTo {
		t.Fatal(r)
	}
	if read(t, fresh.BackupOriginalFileTo) != content {
		t.Fatal(read(t, fresh.BackupOriginalFileTo))
	}
}

// replace_existing_file lets a link take the place of a regular file in one
// rename; a directory is always an error, and without the setting so is a file.
func TestSymlinkReplacesARegularFileOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "current")
	if err := os.WriteFile(path, []byte("left by the package\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := Symlink{Path: path, Target: "releases/2"}
	if r := (Runtime{}).Symlink(context.Background(), link); r.Status != "failed" || !strings.Contains(r.Reason, "replace_existing_file") {
		t.Fatal(r)
	}
	link.ReplaceExistingFile = true
	preview := (Runtime{Dryrun: true}).Symlink(context.Background(), link)
	c := preview.FileChange
	if preview.Change != "would_change" || c.Action != "replace" || c.Before.Kind != "file" || c.After.Target != "releases/2" {
		t.Fatal(preview, c)
	}
	if read(t, path) != "left by the package\n" {
		t.Fatal("dryrun replaced the file")
	}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).Symlink(context.Background(), link); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if target, err := os.Readlink(path); err != nil || target != "releases/2" {
		t.Fatal(target, err)
	}
	directory := filepath.Join(dir, "directory")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	link.Path = directory
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).Symlink(context.Background(), link); r.Status != "failed" || !strings.Contains(r.Reason, "directory") {
			t.Fatal(r)
		}
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() {
		t.Fatal("replaced a directory", err)
	}
}

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func TestSecretFileReadPermissions(t *testing.T) {
	for _, template := range []bool{false, true} {
		for _, tc := range []struct {
			mode            string
			group, everyone bool
			refusal         string
		}{
			{"0600", false, false, ""}, {"0700", false, false, ""},
			{"0622", false, false, ""}, {"0601", false, false, ""},
			{"0640", false, false, "secret_readable_by_group: true"},
			{"0660", true, false, ""},
			{"0644", true, false, "dangerously_let_every_account_read_secret: true"},
			{"0644", false, true, ""}, {"0604", false, true, ""},
		} {
			t.Run(fmt.Sprintf("template=%t/mode=%s/group=%t/everyone=%t", template, tc.mode, tc.group, tc.everyone), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "password")
				content := "{{ .secrets.password }}"
				f := File{Path: path, Content: &content, Mode: tc.mode, SecretReadableByGroup: tc.group, DangerouslyLetEveryAccountReadSecret: tc.everyone}
				if template {
					f.Content = nil
					f.Template = "password.tmpl"
					f.TemplateText = content
				}
				for _, dryrun := range []bool{true, false} {
					r := (Runtime{Dryrun: dryrun}).File(context.Background(), f, value.Scope{"secrets": map[string]any{"password": value.Sensitive("file-secret-sentinel")}})
					if tc.refusal != "" {
						if r.Status != "failed" || !strings.Contains(r.Reason, tc.refusal) {
							t.Fatal(r)
						}
						if !strings.Contains(r.Reason, path+" holds a secret, and permissions_mode "+tc.mode) {
							t.Fatal("refusal must name the file and mode", r)
						}
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatal("refused write changed the path", err)
						}
					} else if r.Status != "succeeded" {
						t.Fatal(r)
					}
				}
				if tc.refusal == "" {
					b, err := os.ReadFile(path)
					if err != nil || string(b) != "file-secret-sentinel" {
						t.Fatal(string(b), err)
					}
					info, err := os.Stat(path)
					if err != nil || fmt.Sprintf("%04o", info.Mode().Perm()) != tc.mode {
						t.Fatal(info, err)
					}
				}
			})
		}
	}
}

func TestSecretFilePermissionMessagesNameReaders(t *testing.T) {
	for _, tc := range []struct {
		mode    os.FileMode
		group   Account
		machine string
		want    string
	}{
		{0660, "admin", "node", "permissions_mode 0660 lets group admin read it. If the group should read it, add secret_readable_by_group: true."},
		{0640, "", "node", "permissions_mode 0640 lets the file's group read it. If the group should read it, add secret_readable_by_group: true."},
		{0644, "admin", "node", "permissions_mode 0644 lets every account on the node read it. If every account should read it, add dangerously_let_every_account_read_secret: true."},
		{0644, "", "controller machine", "permissions_mode 0644 lets every account on the controller machine read it. If every account should read it, add dangerously_let_every_account_read_secret: true."},
	} {
		err := ValidateSecretFilePermissions("/etc/shop/database.env", tc.mode, tc.group, tc.machine, false, false)
		if err == nil || !strings.Contains(err.Error(), "/etc/shop/database.env holds a secret, and "+tc.want) {
			t.Fatal(err)
		}
	}
}

func TestFileContentRequiresPermissionsMode(t *testing.T) {
	text := "hello"
	for _, f := range []File{{Content: &text}, {Source: "hello"}, {Template: "hello.tmpl"}, {ContentFromNodePaths: []string{"/hello"}}, {URL: "https://example.com/hello", DangerouslySkipChecksum: true}} {
		if err := f.Prepare(t.TempDir()); err == nil || !strings.Contains(err.Error(), "permissions_mode is required") {
			t.Fatal(f, err)
		}
	}
	// MMX's own writers, such as ifupdown_interfaces_file, may leave the mode
	// to the file on the node.
	if err := ValidateFile(File{Path: "/etc/network/interfaces.d/eth1", Content: &text}); err != nil {
		t.Fatal("a writer inside MMX must not need a mode", err)
	}
	if err := (&File{Owner: "root"}).Prepare(t.TempDir()); err != nil {
		t.Fatal("metadata-only task must not require mode", err)
	}
}

func TestKeptSecretFileStillChecksReadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	content := "{{ .secrets.password }}"
	f := File{Path: path, Content: &content, Mode: "0644", IfFileExists: "keep_existing_content"}
	r := (Runtime{}).File(context.Background(), f, value.Scope{"secrets": map[string]any{"password": value.Sensitive("secret")}})
	if r.Status != "failed" || !strings.Contains(r.Reason, "dangerously_let_every_account_read_secret: true") {
		t.Fatal(r)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if got := read(t, path); got != "previous" {
		t.Fatal(got)
	}
}

func TestDownloadCarriesSecretReadPermissions(t *testing.T) {
	d := Download{URL: "https://example.com/secret", DangerouslySkipChecksum: true, Path: filepath.Join(t.TempDir(), "secret"), Mode: "0640", SecretReadableByGroup: true, DangerouslyLetEveryAccountReadSecret: true}
	inputs, err := d.ModuleInputs()
	if err != nil {
		t.Fatal(err)
	}
	f := inputs.(*File)
	if !f.SecretReadableByGroup || !f.DangerouslyLetEveryAccountReadSecret {
		t.Fatal("download lost its secret read acknowledgements")
	}
	f.Asset = fileAsset(t, []byte("downloaded bytes"))
	if r := (Runtime{}).File(context.Background(), *f, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
}

func TestPublicFileDoesNotInheritSecretReadPermissions(t *testing.T) {
	scope, _ := (value.Scope{"secrets": map[string]any{"token": value.Sensitive("private")}}).Trace()
	if _, err := scope.Resolve("{{ .secrets.token }}"); err != nil {
		t.Fatal(err)
	}
	text := "public content"
	f := File{Path: filepath.Join(t.TempDir(), "public"), Content: &text, Mode: "0644"}
	if r := (Runtime{}).File(context.Background(), f, scope); r.Status != "succeeded" {
		t.Fatal(r)
	}
}

// A check_new_file shell check with run_script_in_dryrun runs its script in
// a dryrun, as a check with a dryrun command does, so an unchanged file's
// dryrun counts the check as done.
func TestCheckNewFileRunsItsScriptInADryrun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "managed")
	content := "same"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	task := File{Mode: "0644", Path: path, Content: &content, CheckNewFile: &Check{Shell: &Shell{
		Script: `test "$(cat "$NEW_FILE")" = same`, RunScriptInDryrun: true,
		Env: map[string]any{"NEW_FILE": "{{ .new_file.path }}"},
	}}}
	preview := (Runtime{Dryrun: true}).File(context.Background(), task, value.Scope{})
	if preview.CheckNewFile == nil || preview.CheckNewFile.Status != "succeeded" {
		t.Fatal(preview, preview.CheckNewFile)
	}
}
