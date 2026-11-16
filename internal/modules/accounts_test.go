package modules

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mmx/internal/value"
)

func TestUserMembershipArguments(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{
		"getent": `case "$1/$2" in
 group/base) printf 'base:x:100:\n';;
 group/keep) printf 'keep:x:101:app\n';;
 group/add) printf 'add:x:102:\n';;
 *) exit 2;;
 esac`,
		"id": `case "$1" in -gn) echo base;; -nG) echo 'base keep';; *) exit 1;; esac`,
	}
	for name, script := range scripts {
		writeStandInProgram(t, filepath.Join(dir, name), "#!/bin/sh\n"+script)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	current := []string{"app", "x", "100", "100", "", "/srv/app", "/usr/sbin/nologin"}
	for _, tc := range []struct {
		name string
		u    User
		want []string
	}{
		{"unmanaged", User{Name: "app"}, nil},
		{"append", User{Name: "app", SupplementaryGroups: []string{"add"}}, []string{"--append", "--groups", "add"}},
		{"already present", User{Name: "app", SupplementaryGroups: []string{"keep"}}, nil},
		{"replace", User{Name: "app", SupplementaryGroups: []string{"add"}, SupplementaryGroupsNotListed: "remove"}, []string{"--groups", "add"}},
		{"clear", User{Name: "app", SupplementaryGroups: []string{}, SupplementaryGroupsNotListed: "remove"}, []string{"--groups", ""}},
		{"primary membership", User{Name: "app", SupplementaryGroups: []string{"base"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := userArguments(context.Background(), tc.u, current, map[string]any{})
			if err != nil || !reflect.DeepEqual(args, tc.want) {
				t.Fatal(args, err)
			}
		})
	}
	if _, err := userArguments(context.Background(), User{Name: "app", PrimaryGroup: "missing"}, current, map[string]any{}); err == nil {
		t.Fatal("accepted missing primary group")
	}
}

func TestAccountIDsAndLocalOwnership(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = -s ]; then exit 2; fi
case "$1/$2" in
 passwd/1200|passwd/app) printf 'app:x:1200:1200::/srv/app:/bin/sh\n';;
 group/1200|group/app) printf 'app:x:1200:\n';;
 *) exit 2;;
esac
`
	writeStandInProgram(t, filepath.Join(dir, "getent"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	for _, db := range []string{"passwd", "group"} {
		current, err := account(ctx, db, "app")
		if err != nil {
			t.Fatal(err)
		}
		if err := checkAccountID(ctx, db, "app", int64(1200), current); err != nil {
			t.Fatal(err)
		}
		if err := checkAccountID(ctx, db, "app", int64(1201), current); err == nil || !strings.Contains(err.Error(), "renumbering") {
			t.Fatal("accepted ID change", err)
		}
		if err := checkAccountID(ctx, db, "new", int64(1200), nil); err == nil || !strings.Contains(err.Error(), "already used") {
			t.Fatal("accepted collision", err)
		}
		if err := checkAccountID(ctx, db, "new", int64(1201), nil); err != nil {
			t.Fatal(err)
		}
		if r := (Runtime{Dryrun: true}).removeAccount(ctx, db, "app", current, false); r.Status != "failed" || !strings.Contains(r.Reason, "local account database entry") {
			t.Fatal("treated directory-service account as local", r)
		}
	}
	// IDs arrive resolved as integers (config.ResolveArguments).
	args, err := userArguments(ctx, User{Name: "new", UID: int64(1201)}, nil, nil)
	if err != nil || !strings.Contains(strings.Join(args, " "), "--uid 1201") {
		t.Fatal(args, err)
	}
}

func TestAccountInspectionRejectsCommandAndFormatFailures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(dir, "getent")
	for _, script := range []string{"exit 1", "printf broken", "printf 'wrong:x:10:10::/home/app:/bin/sh\\n'"} {
		writeStandInProgram(t, path, "#!/bin/sh\n"+script)
		if _, err := account(context.Background(), "passwd", "app"); err == nil {
			t.Fatal("accepted failed or malformed inspection", script)
		}
	}
}

func TestPasswordField(t *testing.T) {
	t.Parallel()
	for field, want := range map[string]struct {
		hash   string
		locked bool
	}{
		"":         {"", false},
		"!":        {"", false},
		"!!":       {"", false},
		"*":        {"", false},
		"!*":       {"", false},
		"$6$s$h":   {"$6$s$h", false},
		"!$6$s$h":  {"$6$s$h", true},
		"!!$y$j$h": {"$y$j$h", true},
	} {
		if hash, locked := passwordField(field); hash != want.hash || locked != want.locked {
			t.Errorf("%q: %q %v", field, hash, locked)
		}
	}
}

func TestHomeModesFollowLoginDefinitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "login.defs")
	original, vendor, dropIns := loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns
	loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns = path, filepath.Join(t.TempDir(), "vendor.login.defs"), nil
	t.Cleanup(func() { loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns = original, vendor, dropIns })
	for text, want := range map[string][2]uint64{
		"":                              {022, 0755},
		"UMASK\t077\n":                  {077, 0700},
		"UMASK 022\nHOME_MODE 0750\n":   {022, 0750},
		"# HOME_MODE 0700\nUMASK 027\n": {027, 0750},
	} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if umask, mode := homeModes(); umask != want[0] || mode != want[1] {
			t.Errorf("%q: %o %o", text, umask, mode)
		}
	}
}

// standInAccountTools returns the absolute path of the stand-in account tools,
// and skips the test without them: on a test node, where the test binary runs
// without its source tree, the node's real account tools would come first on
// PATH and change real accounts.
func standInAccountTools(t *testing.T, directory string) string {
	t.Helper()
	tools, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tools, "chpasswd")); err != nil {
		t.Skip("needs the stand-in account tools in " + directory)
	}
	return tools
}

// fakeAccounts puts the stand-ins for the native account tools first on PATH,
// over account files that start with root alone.
func fakeAccounts(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("account management runs on Linux nodes")
	}
	tools := standInAccountTools(t, "testdata/accounts")
	dir := t.TempDir()
	for name, text := range map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/sh\n", "group": "root:x:0:\n", "shadow": "root:*:20000:0:99999:7:::\n", "calls": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MMX_FAKE_ACCOUNTS", dir)
	// Without login.defs, a home directory gets 0755, as from useradd.
	original, vendor, skeletons, dropIns := loginDefinitions, vendorLoginDefinitions, skeletonDirectories, loginDefinitionsDropIns
	loginDefinitions, vendorLoginDefinitions, skeletonDirectories, loginDefinitionsDropIns = filepath.Join(dir, "login.defs"), filepath.Join(dir, "vendor.login.defs"), []string{"/etc/skel"}, nil
	t.Cleanup(func() {
		loginDefinitions, vendorLoginDefinitions, skeletonDirectories, loginDefinitionsDropIns = original, vendor, skeletons, dropIns
	})
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func fakeField(t *testing.T, dir, database, name string, field int) string {
	t.Helper()
	for _, line := range strings.Split(read(t, filepath.Join(dir, database)), "\n") {
		if fields := strings.Split(line, ":"); fields[0] == name {
			return fields[field]
		}
	}
	return ""
}

// A new account with a password hash: the preview changes nothing, a failure
// after useradd leaves an account the next try finishes, the hash reaches
// chpasswd only on standard input, and reruns are unchanged.
func TestUserLifecycleWithNativeTools(t *testing.T) {
	dir := fakeAccounts(t)
	ctx := context.Background()
	hash := "$6$mmxsalt$fakehashfakehash"
	scope := value.Scope{"secrets": map[string]any{"alice": value.Sensitive(hash)}}
	home := filepath.Join(t.TempDir(), "alice")
	create := true
	u := User{CreateHomeDirectory: &create, Name: "alice", FullName: "Alice Example", Home: home, Shell: "/bin/bash",
		PasswordHash: "{{ .secrets.alice }}", ApplyPasswordHash: "only_when_account_has_no_password"}
	calls := func() string { return read(t, filepath.Join(dir, "calls")) }

	preview := (Runtime{Dryrun: true}).User(ctx, u, scope)
	want := map[string]any{"account_created": true, "home_directory_created": home, "fields_changed": []string{"password_hash"}}
	if preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
		t.Fatal(preview)
	}
	if strings.Contains(calls(), "useradd") || strings.Contains(calls(), "chpasswd") {
		t.Fatal("preview changed the account", calls())
	}
	if err := os.WriteFile(filepath.Join(dir, "fail-chpasswd"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The value names the account and its home directory, which useradd
	// made, and not the password, which chpasswd did not set.
	failed := Runtime{}.User(ctx, u, scope)
	if failed.Status != "failed" || failed.Change != "changed" || fakeField(t, dir, "shadow", "alice", 1) != "!" ||
		!reflect.DeepEqual(failed.Value, map[string]any{"account_created": true, "home_directory_created": home}) {
		t.Fatal(failed)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatal("useradd did not create the home directory", err)
	}
	if err := os.Remove(filepath.Join(dir, "fail-chpasswd")); err != nil {
		t.Fatal(err)
	}
	// The account exists and has never had a password: the next try sets it.
	retried := Runtime{}.User(ctx, u, scope)
	if retried.Status != "succeeded" || retried.Change != "changed" || fakeField(t, dir, "shadow", "alice", 1) != hash {
		t.Fatal(retried)
	}
	if fakeField(t, dir, "passwd", "alice", 4) != "Alice Example" || strings.Count(calls(), "useradd ") != 1 {
		t.Fatal(calls())
	}
	if strings.Contains(calls(), hash) || !strings.Contains(calls(), "chpasswd --encrypted\n") || read(t, filepath.Join(dir, "chpasswd-input")) != "alice:"+hash+"\n" {
		t.Fatal("the hash did not reach chpasswd on standard input alone", calls())
	}
	if again := (Runtime{}).User(ctx, u, scope); again.Change != "unchanged" {
		t.Fatal(again)
	}

	// A password the person set stays; on_every_run sets the given one again.
	chosen := "$6$other$personchosen"
	if r := runProcess(ctx, nil, "", strings.NewReader("alice:"+chosen+"\n"), nil, "chpasswd", "--encrypted"); r.Result.Status != "succeeded" {
		t.Fatal(r.Result)
	}
	if r := (Runtime{}).User(ctx, u, scope); r.Change != "unchanged" || fakeField(t, dir, "shadow", "alice", 1) != chosen {
		t.Fatal(r)
	}
	u.ApplyPasswordHash = "on_every_run"
	if r := (Runtime{Dryrun: true}).User(ctx, u, scope); r.Change != "would_change" || fakeField(t, dir, "shadow", "alice", 1) != chosen {
		t.Fatal(r)
	}
	if r := (Runtime{}).User(ctx, u, scope); r.Change != "changed" || fakeField(t, dir, "shadow", "alice", 1) != hash {
		t.Fatal(r)
	}

	// The lock keeps the stored hash.
	locked := true
	u.PasswordLocked = &locked
	if r := (Runtime{}).User(ctx, u, scope); r.Change != "changed" || fakeField(t, dir, "shadow", "alice", 1) != "!"+hash ||
		!reflect.DeepEqual(r.Value, map[string]any{"fields_changed": []string{"password_locked"}}) {
		t.Fatal(r)
	}
	if r := (Runtime{}).User(ctx, u, scope); r.Change != "unchanged" {
		t.Fatal(r)
	}

	// A missing home directory of an existing account is made again with
	// mkhomedir_helper when the task asks for it; full_name changes with usermod.
	if err := os.Remove(home); err != nil {
		t.Fatal(err)
	}
	create = false
	if r := (Runtime{}).User(ctx, u, scope); r.Change != "unchanged" {
		t.Fatal("made a home directory the task of an existing account did not ask for", r)
	}
	create = true
	u.FullName, u.CreateHomeDirectory = "Alice Q. Example", &create
	r := (Runtime{}).User(ctx, u, scope)
	if r.Change != "changed" || !strings.Contains(calls(), "usermod --comment Alice Q. Example -- alice") || !strings.Contains(calls(), "mkhomedir_helper alice ") {
		t.Fatal(r, calls())
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() || info.Mode().Perm() != 0755 {
		t.Fatal("home directory was not made again with useradd's permissions", err)
	}

	// Unlocking an account without a hash would allow login without a password.
	unlocked := false
	bob := User{Name: "bob", Home: filepath.Join(t.TempDir(), "bob"), CreateHomeDirectory: &unlocked}
	if r := (Runtime{}).User(ctx, bob, scope); r.Change != "changed" {
		t.Fatal(r)
	}
	bob.PasswordLocked = &unlocked
	if r := (Runtime{}).User(ctx, bob, scope); r.Status != "failed" || !strings.Contains(r.Reason, "no password hash") || fakeField(t, dir, "shadow", "bob", 1) != "!" {
		t.Fatal(r)
	}
	if _, err := os.Stat(bob.Home); !os.IsNotExist(err) {
		t.Fatal("create_home_directory: false made a home directory", err)
	}
}

// Supplementary members: an account whose primary group it is is never
// touched, remove takes away unlisted accounts, keep only adds, and a missing
// account is not checked in a preview.
func TestGroupMembersWithNativeTools(t *testing.T) {
	dir := fakeAccounts(t)
	ctx := context.Background()
	for name, text := range map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/sh\nalice:x:1001:1001::/home/alice:/bin/sh\nbob:x:1002:1002::/home/bob:/bin/sh\nops:x:1003:3000::/home/ops:/bin/sh\n",
		"group":  "root:x:0:\nteam:x:3000:ops,bob\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	members := func() string { return fakeField(t, dir, "group", "team", 3) }
	g := Group{Name: "team", SupplementaryMembers: []string{"alice"}, SupplementaryMembersNotListed: "remove"}
	preview := (Runtime{Dryrun: true}).Group(ctx, g)
	if preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, map[string]any{"members_added": []string{"alice"}, "members_removed": []string{"bob"}}) || members() != "ops,bob" {
		t.Fatal(preview, members())
	}
	if r := (Runtime{}).Group(ctx, g); r.Change != "changed" || members() != "ops,alice" {
		t.Fatal(r, members())
	}
	if r := (Runtime{}).Group(ctx, g); r.Change != "unchanged" {
		t.Fatal(r)
	}
	g = Group{Name: "team", SupplementaryMembers: []string{"bob", "ops"}, SupplementaryMembersNotListed: "keep"}
	if r := (Runtime{}).Group(ctx, g); r.Change != "changed" || members() != "ops,alice,bob" {
		t.Fatal(r, members())
	}
	g = Group{Name: "team", SupplementaryMembers: []string{}, SupplementaryMembersNotListed: "remove"}
	if r := (Runtime{}).Group(ctx, g); r.Change != "changed" || members() != "ops" {
		t.Fatal(r, members())
	}
	g = Group{Name: "team", SupplementaryMembers: []string{"carol"}, SupplementaryMembersNotListed: "keep"}
	if r := (Runtime{Dryrun: true}).Group(ctx, g); r.Status != "not_checked" || !strings.Contains(r.Reason, "carol does not exist yet") {
		t.Fatal(r)
	}
	if r := (Runtime{}).Group(ctx, g); r.Status != "failed" {
		t.Fatal(r)
	}
	// A new group gets its members after groupadd.
	g = Group{Name: "new", SupplementaryMembers: []string{"alice"}, SupplementaryMembersNotListed: "remove"}
	if r := (Runtime{}).Group(ctx, g); r.Change != "changed" || fakeField(t, dir, "group", "new", 3) != "alice" ||
		!reflect.DeepEqual(r.Value, map[string]any{"group_created": true, "members_added": []string{"alice"}}) {
		t.Fatal(r)
	}
}

// A failed account task reports in its value only what it did, never a
// change that did not happen: a node task in on_failure reads that value.
// A preview still lists what a run would change.
func TestFailedAccountTaskReportsOnlyWhatItDid(t *testing.T) {
	dir := fakeAccounts(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nalice:x:1001:1001::/home/alice:/bin/sh\nbob:x:1002:1002::/home/bob:/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tools, err := filepath.Abs("testdata/accounts")
	if err != nil {
		t.Fatal(err)
	}
	// Programs in failing come first on PATH, before the stand-ins.
	failing := t.TempDir()
	t.Setenv("PATH", failing+string(os.PathListSeparator)+os.Getenv("PATH"))
	fail := func(program, script string) {
		writeStandInProgram(t, filepath.Join(failing, program), "#!/bin/sh\n"+script)
	}

	// groupadd is not there or fails: nothing was created.
	g := Group{Name: "team", SupplementaryMembers: []string{"alice", "bob"}, SupplementaryMembersNotListed: "remove"}
	if r := (Runtime{Dryrun: true}).Group(ctx, g); r.Change != "would_change" ||
		!reflect.DeepEqual(r.Value, map[string]any{"group_created": true, "members_added": []string{"alice", "bob"}}) {
		t.Fatal(r)
	}
	fail("groupadd", "echo 'groupadd: cannot lock /etc/group' >&2; exit 10")
	if r := (Runtime{}).Group(ctx, g); r.Status != "failed" || r.Value != nil || fakeField(t, dir, "group", "team", 0) != "" {
		t.Fatal(r)
	}
	// The group is created and gets alice; adding bob fails.
	if err := os.Remove(filepath.Join(failing, "groupadd")); err != nil {
		t.Fatal(err)
	}
	fail("gpasswd", `if [ "$2" = bob ]; then echo 'gpasswd: cannot lock /etc/group' >&2; exit 1; fi; exec `+tools+`/gpasswd "$@"`)
	if r := (Runtime{}).Group(ctx, g); r.Status != "failed" || r.Change != "changed" ||
		!reflect.DeepEqual(r.Value, map[string]any{"group_created": true, "members_added": []string{"alice"}}) {
		t.Fatal(r)
	}

	// useradd fails: no account was created.
	u := User{Name: "carol", CreateHomeDirectory: new(bool)}
	fail("useradd", "echo 'useradd: cannot lock /etc/passwd' >&2; exit 1")
	if r := (Runtime{}).User(ctx, u, value.Scope{}); r.Status != "failed" || r.Value != nil || fakeField(t, dir, "passwd", "carol", 0) != "" {
		t.Fatal(r)
	}
}

func TestUserRequiresHomeCreationChoice(t *testing.T) {
	systemAccount := true
	for _, system := range []*bool{nil, new(bool), &systemAccount} {
		if err := ValidateUser(User{Name: "account", System: system}); err == nil || !strings.Contains(err.Error(), "create_home_directory is required") {
			t.Fatal(err)
		}
	}
	for _, create := range []bool{false, true} {
		if err := ValidateUser(User{Name: "account", CreateHomeDirectory: &create}); err != nil {
			t.Fatal(err)
		}
	}
}

// The behavior of useradd with libeconf on openSUSE, measured on a guest: the
// file in /etc replaces the whole vendor file, login.defs.d drop-ins change
// single settings of whichever file applies, and a drop-in of the same name in
// /etc replaces the one in /usr/etc.
func TestLoginDefinitionsFollowLibeconf(t *testing.T) {
	root := t.TempDir()
	vendor, etc := filepath.Join(root, "usr-login.defs"), filepath.Join(root, "login.defs")
	vendorDropIns, etcDropIns := filepath.Join(root, "usr-login.defs.d"), filepath.Join(root, "login.defs.d")
	for _, directory := range []string{vendorDropIns, etcDropIns} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	originalFile, originalVendor, originalDropIns := loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns
	loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns = etc, vendor, []string{vendorDropIns, etcDropIns}
	t.Cleanup(func() {
		loginDefinitions, vendorLoginDefinitions, loginDefinitionsDropIns = originalFile, originalVendor, originalDropIns
	})
	check := func(step string, wantUmask, wantMode uint64) {
		t.Helper()
		if umask, mode := homeModes(); umask != wantUmask || mode != wantMode {
			t.Fatalf("%s: umask %04o mode %04o, want %04o %04o", step, umask, mode, wantUmask, wantMode)
		}
	}
	write(vendor, "HOME_MODE\t0700\nUMASK\t027\n")
	check("the vendor file alone", 027, 0700)
	write(etc, "UMASK\t022\n")
	check("the file in /etc replaces the vendor file, with its HOME_MODE", 022, 0755)
	write(filepath.Join(etcDropIns, "50.defs"), "HOME_MODE\t0750\n")
	check("a drop-in changes one setting of the file in /etc", 022, 0750)
	os.Remove(etc)
	check("a drop-in changes one setting of the vendor file", 027, 0750)
	write(filepath.Join(vendorDropIns, "50.defs"), "HOME_MODE\t0770\n")
	check("a drop-in of the same name in /etc replaces the one in /usr/etc", 027, 0750)
	write(filepath.Join(vendorDropIns, "60.defs"), "HOME_MODE\t0711\n")
	check("drop-ins apply in the order of their names", 027, 0711)
}

// useradd adds the skeleton of /etc/skel to the one of /usr/etc/skel on
// openSUSE; a single skeleton is used as it is.
func TestNewHomeSkeletonMergesBothDirectories(t *testing.T) {
	root := t.TempDir()
	vendor, etc := filepath.Join(root, "usr-skel"), filepath.Join(root, "skel")
	for _, directory := range []string{vendor, etc} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	original := skeletonDirectories
	skeletonDirectories = []string{vendor, etc}
	t.Cleanup(func() { skeletonDirectories = original })
	directory, remove, err := skeletonForNewHome()
	if err != nil || directory != etc {
		t.Fatalf("both empty: %s %v", directory, err)
	}
	remove()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(vendor, ".profile"), "vendor")
	directory, remove, err = skeletonForNewHome()
	if err != nil || directory != vendor {
		t.Fatalf("only the vendor skeleton has files: %s %v", directory, err)
	}
	remove()
	write(filepath.Join(vendor, ".config", "settings"), "vendor")
	write(filepath.Join(etc, ".bashrc"), "etc")
	write(filepath.Join(etc, ".profile"), "etc")
	directory, remove, err = skeletonForNewHome()
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	for path, want := range map[string]string{".profile": "etc", ".bashrc": "etc", ".config/settings": "vendor"} {
		got, err := os.ReadFile(filepath.Join(directory, path))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v, want %q", path, got, err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(directory, ".bashrc")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("the mode is not kept: %v %v", info, err)
	}
}
