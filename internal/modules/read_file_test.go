package modules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/value"
)

func readFile(path string, secret bool) ReadFile {
	return ReadFile{Path: path, ContentIsSecret: &secret}
}

func TestReadFileReadsTextAndBinaryAndChangesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	text, binary := filepath.Join(dir, "token"), filepath.Join(dir, "key.der")
	if err := os.WriteFile(text, []byte("K10-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte{0xff, 0x00, 0xfe}, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{false, true} {
		r := (Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { t.Fatal("read_file changed something"); return nil }}).readFileTask(context.Background(), readFile(text, false))
		if r.Status != "succeeded" || r.Change != "unchanged" || r.Content != "K10-token\n" {
			t.Fatal(dryrun, r)
		}
	}
	r := (Runtime{}).readFileTask(context.Background(), readFile(binary, false))
	if b, ok := r.Content.(value.Binary); !ok || string(b.Bytes) != "\xff\x00\xfe" {
		t.Fatal(r)
	}
	// Saved results carry binary content as base64 and restore it whole.
	saved, _ := json.Marshal(r)
	var restored struct{ Content any }
	json.Unmarshal(saved, &restored)
	if b, ok := value.Restore(restored.Content).(value.Binary); !ok || string(b.Bytes) != "\xff\x00\xfe" {
		t.Fatal(string(saved))
	}
}

func TestReadFileSecretContentRecordsItsFileAndStaysOutOfJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("read-file-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	r := (Runtime{}).readFileTask(context.Background(), readFile(path, true))
	secret, ok := r.Content.(*value.Secret)
	if r.Status != "succeeded" || !ok || secret.Data != "read-file-sentinel" || secret.FromNode == nil || secret.FromNode.ReadFilePath != path {
		t.Fatal(r)
	}
	if r.Secret {
		t.Fatal("a public path's diagnostics need not be suppressed")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "read-file-sentinel") || !strings.Contains(string(b), path) {
		t.Fatal(string(b))
	}
}

func TestReadFileRefusesMissingLargeAndSpecialFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if r := (Runtime{}).readFileTask(context.Background(), readFile(missing, true)); r.Status != "failed" || !strings.Contains(r.Reason, missing) {
		t.Fatal(r)
	}
	// An earlier task of the run may still create it.
	if r := (Runtime{Dryrun: true}).readFileTask(context.Background(), readFile(missing, true)); r.Status != "not_checked" || r.Change != "unknown" {
		t.Fatal(r)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, ReadFileLimit+1), 0600); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).readFileTask(context.Background(), readFile(large, false)); r.Status != "failed" || !strings.Contains(r.Reason, "up to 1 MiB") {
		t.Fatal(r.Status, r.Reason)
	}
	exact := filepath.Join(dir, "exact")
	if err := os.WriteFile(exact, make([]byte, ReadFileLimit), 0600); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).readFileTask(context.Background(), readFile(exact, false)); r.Status != "succeeded" {
		t.Fatal(r.Status, r.Reason)
	}
	if r := (Runtime{}).readFileTask(context.Background(), readFile(dir, false)); r.Status != "failed" || !strings.Contains(r.Reason, "not a regular file") {
		t.Fatal(r)
	}
	if r := (Runtime{}).readFileTask(context.Background(), ReadFile{Path: missing}); r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "content_is_secret is required") {
		t.Fatal(r)
	}
}

// read_file and content_from_node_paths read as root. An account that can
// write the directory of the path could put a link there to a file only
// root may read, so a link at the end of the path is followed only when root
// owns it, as the links of the system's own packages are.
func TestNodeFileReadsFollowOnlyLinksThatRootOwns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("every link a test makes as root belongs to root")
	}
	dir := t.TempDir()
	private := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(private, []byte("root:secret-hash\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	r := (Runtime{}).readFileTask(context.Background(), readFile(link, false))
	if r.Status != "failed" || !strings.Contains(r.Reason, "symbolic link that account ID") || strings.Contains(r.Reason, "secret-hash") || r.Content != nil {
		t.Fatalf("%+v", r)
	}
	target := filepath.Join(dir, "site.pem")
	f := File{Path: target, Mode: "0644", ContentFromNodePaths: []string{link}}
	if r := (Runtime{}).File(context.Background(), f, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "symbolic link that account ID") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the file was written from the link's target")
	}
	// The file itself, and a link that root owns, are read.
	if r := (Runtime{}).readFileTask(context.Background(), readFile(private, false)); r.Status != "succeeded" || r.Content != "root:secret-hash\n" {
		t.Fatalf("%+v", r)
	}
	for _, system := range []string{"/etc/os-release", "/etc/localtime"} {
		info, err := os.Lstat(system)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if content, err := ReadNodeFile(system); err != nil || content == nil {
			t.Fatalf("%s: %v", system, err)
		}
		return
	}
	t.Log("no root-owned system link to read on this machine")
}
