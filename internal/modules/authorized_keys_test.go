package modules

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	aliceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIORZZg3NlYdxJ28janK/DsJ1O0O2YgF7KN2aiEPskegs alice@laptop"
	bobKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEhTsh+JVq7mk31K1Fmmykip66tKN98E/piQ6A6iE7gx bob@laptop"
)

func parsedKeys(t *testing.T, lines ...string) []AuthorizedKey {
	t.Helper()
	keys := make([]AuthorizedKey, len(lines))
	for i, line := range lines {
		var err error
		if keys[i], err = ParseAuthorizedKey(line); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestPlanAuthorizedKeys(t *testing.T) {
	t.Parallel()
	alice, bob := parsedKeys(t, aliceKey)[0], parsedKeys(t, bobKey)[0]
	limited := `from="10.0.0.0/8" ` + aliceKey
	for _, tc := range []struct {
		name, current                   string
		wanted                          []string
		replace, absent, replaceOptions bool
		want                            string
		added, updated, removed         []string
	}{
		{name: "new file", wanted: []string{aliceKey}, want: aliceKey + "\n", added: []string{alice.Name()}},
		{name: "append keeps other lines", current: "# team\n" + bobKey + "\n", wanted: []string{aliceKey}, want: "# team\n" + bobKey + "\n" + aliceKey + "\n", added: []string{alice.Name()}},
		{name: "unchanged", current: bobKey + "\n" + aliceKey + "\n", wanted: []string{aliceKey}, want: bobKey + "\n" + aliceKey + "\n"},
		{name: "options rewrite the line in place", replaceOptions: true, current: aliceKey + "\n" + bobKey + "\n", wanted: []string{limited}, want: limited + "\n" + bobKey + "\n", updated: []string{alice.Name()}},
		{name: "repeated line of a listed key is dropped", replaceOptions: true, current: aliceKey + "\n" + limited + "\n", wanted: []string{aliceKey}, want: aliceKey + "\n", updated: []string{alice.Name()}},
		{name: "keep preserves restrictions", current: limited + "\n", wanted: []string{aliceKey}, want: limited + "\n"},
		{name: "keep preserves distinct duplicate restrictions", current: limited + "\n" + aliceKey + "\n", wanted: []string{aliceKey}, want: limited + "\n" + aliceKey + "\n"},
		{name: "explicit replacement removes restrictions", current: limited + "\n", wanted: []string{aliceKey}, replaceOptions: true, want: aliceKey + "\n", updated: []string{alice.Name()}},
		{name: "replace", current: "# team\n" + bobKey + "\n" + aliceKey + "\n", wanted: []string{aliceKey}, replace: true, want: aliceKey + "\n", removed: []string{bob.Name()}},
		{name: "replace empties", current: aliceKey + "\n", wanted: []string{}, replace: true, want: "", removed: []string{alice.Name()}},
		{name: "absent whatever its options", current: limited + "\n" + bobKey + "\n", wanted: []string{aliceKey}, absent: true, want: bobKey + "\n", removed: []string{alice.Name()}},
		{name: "absent missing key", current: bobKey + "\n", wanted: []string{aliceKey}, absent: true, want: bobKey + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, changes, err := planAuthorizedKeys(tc.current, parsedKeys(t, tc.wanted...), tc.replace, tc.absent, tc.replaceOptions)
			if err != nil {
				t.Fatal(err)
			}
			if text != tc.want {
				t.Fatalf("text %q, want %q", text, tc.want)
			}
			for kind, want := range map[string][]string{"added": tc.added, "updated": tc.updated, "removed": tc.removed} {
				var got []string
				list, _ := changes[kind].([]any)
				for _, name := range list {
					got = append(got, name.(string))
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: %v, want %v", kind, got, want)
				}
			}
		})
	}
}

// keysFixture gives the current account a temporary home.
func keysFixture(t *testing.T) (AuthorizedKeys, string) {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	lookupUser = func(name string) (*user.User, error) {
		if name != me.Username {
			return user.Lookup(name)
		}
		u := *me
		u.HomeDir = home
		return &u, nil
	}
	t.Cleanup(func() { lookupUser = user.Lookup })
	return AuthorizedKeys{ForUserAccount: me.Username, Keys: []string{aliceKey}, KeysNotListed: "keep"}, home
}

func TestAuthorizedKeysFile(t *testing.T) {
	ctx := context.Background()
	k, home := keysFixture(t)
	dir, path := filepath.Join(home, ".ssh"), filepath.Join(home, ".ssh", "authorized_keys")
	preview := (Runtime{Dryrun: true}).AuthorizedKeys(ctx, k)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.FileChange == nil || preview.FileChange.Action != "create" {
		t.Fatalf("%+v", preview)
	}
	if !reflect.DeepEqual(preview.Value, map[string]any{"added": []any{parsedKeys(t, aliceKey)[0].Name()}}) {
		t.Fatal(preview.Value)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("dryrun created .ssh")
	}
	applied := (Runtime{}).AuthorizedKeys(ctx, k)
	if applied.Status != "succeeded" || applied.Change != "changed" || applied.Value == nil {
		t.Fatalf("%+v", applied)
	}
	for p, mode := range map[string]os.FileMode{dir: 0700, path: 0600} {
		if info, err := os.Lstat(p); err != nil || info.Mode().Perm() != mode {
			t.Fatal(p, info.Mode(), err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != aliceKey+"\n" {
		t.Fatalf("%q", b)
	}
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "unchanged" || r.Value != nil {
		t.Fatalf("%+v", r)
	}

	// An existing file's restrictions survive a task that supplies a plain public key.
	restricted := `from="10.0.0.0/8",no-port-forwarding ` + aliceKey + "\n"
	if err := os.WriteFile(path, []byte(restricted), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).AuthorizedKeys(ctx, k)
		b, _ := os.ReadFile(path)
		if r.Status != "succeeded" || r.Change != "unchanged" || string(b) != restricted {
			t.Fatalf("%+v %q", r, b)
		}
	}
	k.ReplaceOptionsOfListedKeys = true
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != aliceKey+"\n" {
		t.Fatalf("%q", b)
	}
	k.ReplaceOptionsOfListedKeys = false

	// Loosened metadata is repaired without a key change.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "changed" || r.Value != nil {
		t.Fatalf("%+v", r)
	}
	if info, _ := os.Lstat(dir); info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}

	k.Keys, k.KeysNotListed = []string{bobKey}, "remove"
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "changed" || len(r.Value.(map[string]any)) != 2 {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != bobKey+"\n" {
		t.Fatalf("%q", b)
	}
	k.KeysNotListed, k.State = "", "absent"
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Fatalf("%q %v", b, err)
	}
}

func TestAuthorizedKeysRefusesLinksAndMissingAccounts(t *testing.T) {
	ctx := context.Background()
	k, home := keysFixture(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).AuthorizedKeys(ctx, k); r.Status != "failed" || !strings.Contains(r.Reason, "expected a directory") {
			t.Fatalf("%+v", r)
		}
	}
	if err := os.Remove(filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "keys"), filepath.Join(home, ".ssh", "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Status != "failed" || !strings.Contains(r.Reason, "expected a regular file") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "keys")); !os.IsNotExist(err) {
		t.Fatal("wrote through a link")
	}

	k.ForUserAccount = "mmx-missing-user"
	if r := (Runtime{Dryrun: true}).AuthorizedKeys(ctx, k); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Status != "failed" || !strings.Contains(r.Reason, "unknown user") {
		t.Fatalf("%+v", r)
	}
}

// Keys read from files join the written keys; a task that only takes keys
// away changes nothing for an account or home that does not exist.
func TestAuthorizedKeysFromFilesAndRemovalForMissingAccounts(t *testing.T) {
	ctx := context.Background()
	k, home := keysFixture(t)
	k.FileKeys = []FileKey{{Line: bobKey, Place: "key_files_on_controller_machine[0] files/bob.pub line 1"}}
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys")); string(b) != aliceKey+"\n"+bobKey+"\n" {
		t.Fatalf("%q", b)
	}
	k.FileKeys = append(k.FileKeys, FileKey{Line: "no-pty " + aliceKey, Place: "key_files_on_controller_machine[0] files/bob.pub line 2"})
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Status != "failed" || !strings.Contains(r.Reason, "in keys[0] and in key_files_on_controller_machine[0] files/bob.pub line 2") {
		t.Fatalf("%+v", r)
	}
	missing := AuthorizedKeys{ForUserAccount: "mmx-missing-user"}
	for _, task := range []AuthorizedKeys{
		{ForUserAccount: missing.ForUserAccount, Keys: []string{aliceKey}, State: "absent"},
		{ForUserAccount: missing.ForUserAccount, Keys: []string{}, KeysNotListed: "remove"},
	} {
		for _, dryrun := range []bool{true, false} {
			if r := (Runtime{Dryrun: dryrun}).AuthorizedKeys(ctx, task); r.Status != "succeeded" || r.Change != "unchanged" {
				t.Fatalf("%+v %+v", task, r)
			}
		}
	}
	if r := (Runtime{}).AuthorizedKeys(ctx, AuthorizedKeys{ForUserAccount: missing.ForUserAccount, Keys: []string{aliceKey}, KeysNotListed: "remove"}); r.Status != "failed" {
		t.Fatalf("a task that adds keys must fail for a missing account: %+v", r)
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	k.FileKeys, k.Keys, k.KeysNotListed = nil, []string{}, "remove"
	if r := (Runtime{}).AuthorizedKeys(ctx, k); r.Change != "unchanged" || r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
}

func TestAuthorizedKeysReplacementChoiceRequiresPresentKeys(t *testing.T) {
	task := AuthorizedKeys{ForUserAccount: "deploy", Keys: []string{aliceKey}, State: "absent", ReplaceOptionsOfListedKeys: true}
	if err := ValidateAuthorizedKeys(task); err == nil || !strings.Contains(err.Error(), "authorized_keys with state: absent accepts only") {
		t.Fatal(err)
	}
}

func TestAuthorizedKeysOptionsUnderBothListPolicies(t *testing.T) {
	nodeOptions, taskOptions := `from="10.0.0.0/8",no-port-forwarding`, `from="192.0.2.0/24"`
	for _, policy := range []string{"keep", "remove"} {
		for _, scenario := range []struct {
			name, stored, listed, expected, keyName string
			replace, refused                        bool
		}{
			{name: "plain key preserves restrictions", stored: nodeOptions + " " + aliceKey, listed: aliceKey, expected: nodeOptions + " " + aliceKey},
			{name: "same options preserve existing comment", stored: nodeOptions + " " + aliceKey, listed: nodeOptions + " " + strings.ReplaceAll(aliceKey, "alice@laptop", "other comment"), expected: nodeOptions + " " + aliceKey},
			{name: "differing options refused", stored: nodeOptions + " " + aliceKey, listed: taskOptions + " " + aliceKey, refused: true, keyName: "alice@laptop"},
			{name: "adding options refused", stored: aliceKey, listed: taskOptions + " " + aliceKey, refused: true, keyName: "alice@laptop"},
			{name: "unnamed key refusal identifies base64", stored: nodeOptions + " " + strings.TrimSuffix(aliceKey, " alice@laptop"), listed: taskOptions + " " + strings.TrimSuffix(aliceKey, " alice@laptop"), refused: true, keyName: "ssh-ed25519 " + strings.Fields(aliceKey)[1][:32]},
			{name: "explicit replacement writes new options", stored: nodeOptions + " " + aliceKey, listed: taskOptions + " " + aliceKey, expected: taskOptions + " " + aliceKey, replace: true},
			{name: "explicit replacement removes options", stored: nodeOptions + " " + aliceKey, listed: aliceKey, expected: aliceKey, replace: true},
			{name: "missing key gets requested options", listed: taskOptions + " " + aliceKey, expected: taskOptions + " " + aliceKey},
		} {
			t.Run(policy+"/"+scenario.name, func(t *testing.T) {
				task, home := keysFixture(t)
				task.Keys, task.KeysNotListed, task.ReplaceOptionsOfListedKeys = []string{scenario.listed}, policy, scenario.replace
				directory := filepath.Join(home, ".ssh")
				path := filepath.Join(directory, "authorized_keys")
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				before := bobKey + "\n"
				if scenario.stored != "" {
					before += scenario.stored + "\n"
				}
				if err := os.WriteFile(path, []byte(before), 0600); err != nil {
					t.Fatal(err)
				}
				if scenario.refused {
					if err := os.Chmod(directory, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
				expected := scenario.expected + "\n"
				if policy == "keep" {
					expected = bobKey + "\n" + expected
				}
				for _, dryrun := range []bool{true, false} {
					run := Runtime{Dryrun: dryrun}
					if scenario.refused {
						run.BeforeMutation = func(string) error { t.Fatal("refusal must precede every mutation"); return nil }
					}
					result := run.AuthorizedKeys(context.Background(), task)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if scenario.refused {
						if result.Status != "failed" || string(data) != before {
							t.Fatalf("result %+v, contents %q", result, data)
						}
						existingOptions := nodeOptions
						if scenario.stored == aliceKey {
							existingOptions = ""
						}
						for _, message := range []string{path, scenario.keyName, "options [" + existingOptions + "] on the node", "node task requests options [" + taskOptions + "]", "replace_options_of_listed_keys: true"} {
							if !strings.Contains(result.Reason, message) {
								t.Fatalf("missing %q in %+v", message, result)
							}
						}
						for file, mode := range map[string]os.FileMode{directory: 0755, path: 0644} {
							info, err := os.Stat(file)
							if err != nil || info.Mode().Perm() != mode {
								t.Fatalf("metadata changed for %s: %v %v", file, info, err)
							}
						}
						continue
					}
					expectedChange := "unchanged"
					if expected != before {
						if dryrun {
							expectedChange = "would_change"
						} else {
							expectedChange = "changed"
						}
					}
					if result.Status != "succeeded" || result.Change != expectedChange {
						t.Fatalf("%+v, expected %s", result, expectedChange)
					}
					expectedContents := expected
					if dryrun {
						expectedContents = before
					}
					if string(data) != expectedContents {
						t.Fatalf("contents %q, expected %q", data, expectedContents)
					}
				}
			})
		}
	}
}

// A task that fails before it writes the keys reports no keys added.
func TestFailedAuthorizedKeysTaskReportsNoKeysItDidNotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test needs an account that cannot give a file to another group")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	k, home := keysFixture(t)
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	// The account's primary group is one that this account is not in (group
	// 0), so setting the group of .ssh fails before the keys are written.
	if me.Gid == "0" {
		t.Skip("the account's group is group 0")
	}
	lookupUser = func(name string) (*user.User, error) {
		u := *me
		u.HomeDir, u.Gid = home, "0"
		return &u, nil
	}
	r := (Runtime{}).AuthorizedKeys(context.Background(), k)
	if r.Status != "failed" || r.Value != nil {
		t.Fatalf("a failed run reports %+v", r)
	}
	if _, err := os.Lstat(filepath.Join(home, ".ssh", "authorized_keys")); !os.IsNotExist(err) {
		t.Fatal("the keys were written", err)
	}
}
