package modules

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A changing primitive in a dryrun reports what it would change and never
// reaches the mutation boundary; in a run it changes once and then reports
// unchanged.
func TestChangingPrimitivesChangeNothingInADryrun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dryrun := Runtime{Dryrun: true, BeforeMutation: func(string) error { t.Fatal("a dryrun reached the mutation boundary"); return nil }}
	file, directory, link := filepath.Join(dir, "greeting"), filepath.Join(dir, "data"), filepath.Join(dir, "current")
	owner, group := currentAccount(t)
	for _, c := range []PathChange{
		dryrun.WriteFile(ctx, file, "Hello, Barbara!\n", owner, group, "0644"),
		dryrun.MakeDirectory(ctx, directory, owner, group, "0755"),
		dryrun.MakeLink(ctx, link, file),
	} {
		if c.Error != "" || !c.Changed {
			t.Fatal(c)
		}
	}
	for _, path := range []string{file, directory, link} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("a dryrun created", path)
		}
	}
	if out := dryrun.RunCommandThatChangesTheNode(ctx, []string{"touch", filepath.Join(dir, "ran")}, ""); out.Ran || out.Error != "" {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
		t.Fatal("a dryrun ran a command that changes the node")
	}

	run := Runtime{}
	for i, want := range []bool{true, false} {
		for _, c := range []PathChange{
			run.WriteFile(ctx, file, "Hello, Barbara!\n", owner, group, "0644"),
			run.MakeDirectory(ctx, directory, owner, group, "0755"),
			run.MakeLink(ctx, link, file),
		} {
			if c.Error != "" || c.Changed != want {
				t.Fatal(i, c)
			}
		}
	}
	if text := run.ReadFile(file); text.Error != "" || text.Content != "Hello, Barbara!\n" {
		t.Fatal(text)
	}
	if c := dryrun.WriteFile(ctx, file, "Hello, Barbara!\n", owner, group, "0644"); c.Changed || c.Error != "" {
		t.Fatal("a dryrun reported a change for a file that matches", c)
	}
	if c := dryrun.WriteFile(ctx, file, "Hello, Ann!\n", owner, group, "0644"); !c.Changed || c.Error != "" {
		t.Fatal("a dryrun missed a change", c)
	}
	if out := run.RunCommandThatChangesTheNode(ctx, []string{"touch", filepath.Join(dir, "ran")}, ""); !out.Ran || out.ExitCode != 0 {
		t.Fatal(out)
	}
	if c := run.RemovePath(ctx, directory); !c.Changed || c.Error != "" {
		t.Fatal(c)
	}
	if c := run.RemovePath(ctx, directory); c.Changed || c.Error != "" {
		t.Fatal("removing a missing path changed something", c)
	}
	if c := run.RemovePath(ctx, dir); c.Changed || !strings.Contains(c.Error, "not empty") {
		t.Fatal("removed a directory that is not empty", c)
	}
}

func currentAccount(t *testing.T) (string, string) {
	t.Helper()
	info := Runtime{}.InspectPath(t.TempDir())
	if info.Error != "" {
		t.Fatal(info.Error)
	}
	return info.Owner, info.Group
}

// Reading primitives run in a dryrun too and describe what is there.
func TestReadingPrimitives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dryrun := Runtime{Dryrun: true}
	file := filepath.Join(dir, "greeting")
	if err := os.WriteFile(file, []byte("hello\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	info := dryrun.InspectPath(file)
	if !info.Exists || info.Type != "file" || info.PermissionsMode != "0640" || info.SHA256 != dryrun.SHA256("hello\n") {
		t.Fatal(info)
	}
	if link := dryrun.InspectPath(filepath.Join(dir, "link")); link.Type != "link" || link.LinkTarget != file {
		t.Fatal(link)
	}
	if missing := dryrun.InspectPath(filepath.Join(dir, "missing")); missing.Exists || missing.Error != "" {
		t.Fatal(missing)
	}
	if text := dryrun.ReadFile("relative"); text.Error == "" {
		t.Fatal("read a relative path")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, primitiveFileLimit+1), 0600); err != nil {
		t.Fatal(err)
	}
	if text := dryrun.ReadFile(big); !strings.Contains(text.Error, "more than 1 MiB") {
		t.Fatal(text.Error)
	}
	out := dryrun.RunCommand(ctx, []string{"cat"}, "standard input")
	if !out.Ran || out.ExitCode != 0 || out.StandardOutput != "standard input" {
		t.Fatal(out)
	}
	if out := dryrun.RunCommand(ctx, []string{"false"}, ""); out.ExitCode != 1 || out.Error != "" {
		t.Fatal(out)
	}
	if out := dryrun.RunCommand(ctx, []string{"/no/such/program"}, ""); out.Error == "" {
		t.Fatal(out)
	}
}

// A request that only reads is sent in a dryrun; one that changes something
// is not. No redirect is followed, and no error holds the address.
func TestHTTPRequestPrimitive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	received := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(r.Method + " " + r.Header.Get("X-Person")))
	}))
	defer server.Close()
	dryrun := Runtime{Dryrun: true}
	if r := dryrun.HTTPRequest(ctx, "POST", server.URL+"/hooks/secret-path", nil, "body", ""); r.Sent || r.Error != "" || received != 0 {
		t.Fatal("a dryrun sent a request that changes something", r)
	}
	r := dryrun.HTTPRequest(ctx, "GET", server.URL+"/", map[string]string{"X-Person": "Barbara"}, "", "")
	if !r.Sent || r.HTTPStatusCode != 200 || r.Body != "GET Barbara" || r.Headers["content-type"] != "text/plain" {
		t.Fatal(r)
	}
	if r := (Runtime{}).HTTPRequest(ctx, "POST", server.URL+"/", nil, "", ""); !r.Sent || r.Body != "POST " {
		t.Fatal(r)
	}
	if r := dryrun.HTTPRequest(ctx, "GET", server.URL+"/moved", nil, "", ""); r.HTTPStatusCode != http.StatusFound {
		t.Fatal("followed a redirect", r)
	}
	server.Close()
	if r := dryrun.HTTPRequest(ctx, "GET", server.URL+"/hooks/secret-path?token=secret", nil, "", ""); r.Error == "" || strings.Contains(r.Error, "secret") {
		t.Fatal("the error holds the address", r.Error)
	}
}
