package modules

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

const zypperRepositoryModule = "zypper_repository"

// zypperKey is an armored key whose id the stand-in gpg reads from the line
// KEYID and the id. The stand-in rpm imports and removes these ids.
func zypperKey(id string) string {
	return "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nKEYID" + id + "\n-----END PGP PUBLIC KEY BLOCK-----\n"
}

const zypperTestKeyID = "f7ffe6b0"

var zypperTestKey = zypperKey(zypperTestKeyID)

// zypperRepositoryNode gives zypper_repository a stand-in zypper, rpm, gpg,
// and runs a copy of the script that names the test's directories for
// /etc/zypp/repos.d and /etc/zypp/mmx_repository_keys, and the owner and
// group of the current account (nothing else of the script differs). The
// stand-in gpg prints the id of the key in the text it reads, as gpg 2.5
// printed it for a key on an openSUSE Leap 16.1 test node; a file
// imported-ID in the stand-ins' directory says that rpm holds the key ID.
type zypperRepositoryNode struct {
	t            *testing.T
	calls        func() []string
	repositories string
	keys         string
}

func newZypperRepositoryNode(t *testing.T) *zypperRepositoryNode {
	t.Helper()
	n := &zypperRepositoryNode{t: t, repositories: traversableTempDir(t), keys: traversableTempDir(t)}
	n.calls = stubTools(t, map[string]string{
		"gpg": `
cat > "$STUBS/gpg-input"
if ! grep -q 'BEGIN PGP PUBLIC KEY BLOCK' "$STUBS/gpg-input"; then echo 'gpg: no valid OpenPGP data found.' >&2; exit 2; fi
for id in $(grep -o 'KEYID[0-9a-f]*' "$STUBS/gpg-input" | sed 's/KEYID//'); do
  printf 'pub:-:2048:1:787BB87F%s:1794639712:::-:::scSC::::::23::0:\nfpr:::::::::8ACA7C16E14617DD8646912A787BB87F%s:\nuid:-::::1794639712::C19663B5692B8E63176A61AF00757F05122F4E7F::MMX test repo <t@example.invalid>::::::::::0:\n' "$id" "$id"
done`,
		"rpm": `
case "$1" in
-q) if [ -e "$STUBS/imported-${2#gpg-pubkey-}" ]; then echo "$2-6af80760"; else echo "package $2 is not installed"; exit 1; fi ;;
--import) if [ -e "$STUBS/import-fails" ]; then echo 'error: key import failed' >&2; exit 1; fi
          for id in $(grep -o 'KEYID[0-9a-f]*' "$2" | sed 's/KEYID//'); do touch "$STUBS/imported-$id"; done ;;
-e) rm -f "$STUBS/imported-${2#gpg-pubkey-}" ;;
esac`,
		"zypper": `
if [ "$2" = removerepo ]; then rm -f "$(cat "$STUBS/repositories")/$3.repo"; fi`,
	})
	storageStubFile(t, "repositories", n.repositories)
	return n
}

func (n *zypperRepositoryNode) imported(id string) bool {
	_, err := os.Stat(filepath.Join(stubToolsDirectory(n.t), "imported-"+id))
	return err == nil
}

func (n *zypperRepositoryNode) record(name string) string {
	b, _ := os.ReadFile(filepath.Join(n.keys, name+".imported_key_ids"))
	return string(b)
}

// module loads the script with the test's directories.
func (n *zypperRepositoryNode) module() Module {
	n.t.Helper()
	source := filepath.Join(officialExtraModulesDirectory, zypperRepositoryModule)
	script, err := os.ReadFile(filepath.Join(source, ModuleScriptFile))
	if err != nil {
		n.t.Fatal(err)
	}
	text := string(script)
	me, err := user.Current()
	if err != nil {
		n.t.Fatal(err)
	}
	group, err := user.LookupGroupId(me.Gid)
	if err != nil {
		n.t.Fatal(err)
	}
	for _, replacement := range [][2]string{
		{`FILE_OWNER = "root"`, `FILE_OWNER = "` + me.Username + `"`},
		{`FILE_GROUP = "root"`, `FILE_GROUP = "` + group.Name + `"`},
		{`REPOSITORIES_DIRECTORY = "/etc/zypp/repos.d"`, `REPOSITORIES_DIRECTORY = "` + n.repositories + `"`},
		{`KEY_DIRECTORY = "/etc/zypp/mmx_repository_keys"`, `KEY_DIRECTORY = "` + n.keys + `"`},
	} {
		if !strings.Contains(text, replacement[0]) {
			n.t.Fatalf("the script has no %q", replacement[0])
		}
		text = strings.ReplaceAll(text, replacement[0], replacement[1])
	}
	description, err := ReadModuleDescription(source, zypperRepositoryModule)
	if err != nil {
		n.t.Fatal(err)
	}
	directory := filepath.Join(n.t.TempDir(), zypperRepositoryModule)
	if err := os.MkdirAll(directory, 0755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ModuleScriptFile), []byte(text), 0644); err != nil {
		n.t.Fatal(err)
	}
	return Module{Kind: KindOfficialExtra, Description: description, Directory: directory}
}

func (n *zypperRepositoryNode) run(inputs map[string]any, dryrun bool) protocol.Result {
	n.t.Helper()
	scope, _ := value.Scope{}.Trace()
	r := Runtime{Dryrun: dryrun}.Starlark(testContext(n.t), zypperRepositoryModule, n.module(), inputs, scope)
	return reportedByTheEngine(r, scope)
}

func zypperRepositoryInputs() map[string]any {
	return map[string]any{
		"name":                          "vendor",
		"url":                           "https://download.example.org/leap/16.1/",
		"repository_metadata_is_signed": true,
		"signing_key_text":              zypperTestKey,
	}
}

func TestOfficialExtraZypperRepositoryRefusesInputs(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	with := func(changes map[string]any) map[string]any {
		inputs := zypperRepositoryInputs()
		for key, value := range changes {
			if value == nil {
				delete(inputs, key)
			} else {
				inputs[key] = value
			}
		}
		return inputs
	}
	for _, c := range []struct {
		inputs map[string]any
		reason string
	}{
		{with(map[string]any{"name": "bad/name"}), "zypper_repository.name: may hold only letters, digits, and _ - ."},
		{with(map[string]any{"name": ".hidden"}), "zypper_repository.name: must not start with a hyphen or a dot"},
		{with(map[string]any{"url": nil}), "zypper_repository.url: is required with state present"},
		{with(map[string]any{"repository_metadata_is_signed": nil}), "zypper_repository.repository_metadata_is_signed: is required with state present"},
		{with(map[string]any{"url": "http://example.org/", "repository_metadata_is_signed": false}), "zypper_repository.url: must start with https:// when repository_metadata_is_signed is false: nothing then stops a change of the list of packages on the way, such as an old list that keeps back updates"},
		{with(map[string]any{"url": "ftp://example.org/"}), "zypper_repository.url: must start with https:// or http://"},
		{with(map[string]any{"url": "https://example.org/\nenabled=0"}), "zypper_repository.url: must not hold a space, a line break, or a backslash"},
		{with(map[string]any{"signing_key_text": nil}), "zypper_repository.signing_key_text: exactly one of signing_key_text or signing_key_url is required with state present"},
		{with(map[string]any{"signing_key_url": "https://example.org/key.asc"}), "zypper_repository.signing_key_text: exactly one of signing_key_text or signing_key_url is required with state present"},
		{with(map[string]any{"signing_key_text": nil, "signing_key_url": "http://example.org/key.asc"}), "zypper_repository.signing_key_url: must start with https://"},
		{with(map[string]any{"signing_key_text": nil, "signing_key_url": "https://example.org/key.asc"}), "zypper_repository.signing_key_sha256: exactly one of signing_key_sha256 or dangerously_skip_checksum: true is required with signing_key_url"},
		{with(map[string]any{"signing_key_sha256": strings.Repeat("0", 64)}), "zypper_repository.signing_key_sha256: is for signing_key_url; remove it, or use signing_key_url"},
		{with(map[string]any{"signing_key_text": "not a key"}), "zypper_repository.signing_key_text: is not an ASCII-armored OpenPGP public key, which starts with -----BEGIN PGP PUBLIC KEY BLOCK-----"},
		{map[string]any{"name": "vendor", "state": "absent", "url": "https://example.org/"}, "zypper_repository.state: absent accepts only name and state; remove url"},
	} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, n.run(c.inputs, dryrun), "unchanged", c.reason)
		}
	}
	if got := n.calls(); got != nil {
		t.Fatalf("a refused task ran %q", got)
	}
}

func TestOfficialExtraZypperRepositoryWithoutZypper(t *testing.T) {
	t.Parallel()
	stubTools(t, map[string]string{})
	n := &zypperRepositoryNode{t: t, repositories: t.TempDir(), keys: t.TempDir()}
	reason := "zypper_repository: the program zypper is not installed; zypper_repository works only on a node whose package manager is zypper, such as openSUSE; give the node task a condition on the node fact package_manager"
	storageResultIs(t, n.run(zypperRepositoryInputs(), true), "not_checked", "unknown", reason)
	r := n.run(zypperRepositoryInputs(), false)
	storageResultIs(t, r, "failed", "unchanged", reason)
	if r.FailureKind != "missing_prerequisite" {
		t.Fatalf("failure kind %q", r.FailureKind)
	}
}

// A run writes the key, imports it into rpm, and writes the repository file
// with every setting spelled out; the next run is unchanged and runs no
// changing command; a dryrun changes nothing and says what a run would do.
func TestOfficialExtraZypperRepositoryAdds(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	inputs := zypperRepositoryInputs()

	dryrun := n.run(inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would import the signing key for the repository vendor; would add the repository vendor at https://download.example.org/leap/16.1/")
	if _, err := os.Stat(filepath.Join(n.repositories, "vendor.repo")); err == nil {
		t.Fatal("a dryrun wrote the repository file")
	}
	if dryrun.ModuleOutputs["created"] != true || !strings.Contains(dryrun.Diff, "would run: rpm --import "+filepath.Join(n.keys, "vendor.asc")) {
		t.Fatalf("dryrun %+v", dryrun)
	}

	run := n.run(inputs, false)
	storageResultIs(t, run, "succeeded", "changed", "imported the signing key f7ffe6b0; added the repository vendor at https://download.example.org/leap/16.1/")
	written, err := os.ReadFile(filepath.Join(n.repositories, "vendor.repo"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Written by MMX (zypper_repository). MMX overwrites changes.\n[vendor]\nname=vendor\nenabled=1\nautorefresh=1\nbaseurl=https://download.example.org/leap/16.1/\ntype=rpm-md\ngpgcheck=1\nrepo_gpgcheck=1\npkg_gpgcheck=1\ngpgkey=file://" + n.keys + "/vendor.asc\n"
	if string(written) != want {
		t.Fatalf("repository file:\n%s\nwant:\n%s", written, want)
	}
	if key, _ := os.ReadFile(filepath.Join(n.keys, "vendor.asc")); string(key) != zypperTestKey {
		t.Fatalf("key file %q", key)
	}
	if run.ModuleOutputs["created"] != true {
		t.Fatalf("created %+v", run.ModuleOutputs)
	}

	before := len(n.calls())
	again := n.run(inputs, false)
	storageResultIs(t, again, "succeeded", "unchanged", "")
	// A first dryrun cannot read the key's id (gpg's home does not exist yet),
	// so it does not know the record that a run writes; the next dryrun does.
	if again.Fingerprint != run.Fingerprint || n.run(inputs, true).Fingerprint != run.Fingerprint {
		t.Fatalf("fingerprints %s %s", run.Fingerprint, again.Fingerprint)
	}
	for _, call := range n.calls()[before:] {
		if strings.HasPrefix(call, "rpm --import") || strings.HasPrefix(call, "rpm -e") {
			t.Fatalf("an unchanged task ran %q", call)
		}
	}

	// Metadata that the repository does not sign changes the file.
	inputs["repository_metadata_is_signed"] = false
	changed := n.run(inputs, false)
	storageResultIs(t, changed, "succeeded", "changed", "changed the repository vendor at https://download.example.org/leap/16.1/")
	if !strings.Contains(changed.Diff, "repo_gpgcheck=0") || changed.ModuleOutputs["created"] != false {
		t.Fatalf("%+v", changed)
	}
}

// A key that rpm does not hold is imported again; a key file that is no key
// fails before anything is written; an import that fails is reported.
func TestOfficialExtraZypperRepositoryKeyProblems(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	inputs := zypperRepositoryInputs()
	n.run(inputs, false)
	// rpm loses the key.
	if err := os.Remove(filepath.Join(stubToolsDirectory(t), "imported-"+zypperTestKeyID)); err != nil {
		t.Fatal(err)
	}
	storageResultIs(t, n.run(inputs, false), "succeeded", "changed", "imported the signing key f7ffe6b0")

	m := newZypperRepositoryNode(t)
	storageStubFile(t, "import-fails", "")
	storageTaskFailed(t, m.run(inputs, false), "changed", "zypper_repository: importing the signing key into rpm: error: key import failed")
	if _, err := os.Stat(filepath.Join(m.repositories, "vendor.repo")); err == nil {
		t.Fatal("the repository file was written although the key import failed")
	}
}

// Removing a repository removes it with zypper, its key from rpm, and the
// key file; a repository that is gone is unchanged.
func TestOfficialExtraZypperRepositoryRemoves(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	n.run(zypperRepositoryInputs(), false)
	absent := map[string]any{"name": "vendor", "state": "absent"}

	storageResultIs(t, n.run(absent, true), "succeeded", "would_change", "would remove the repository vendor; would remove the signing key f7ffe6b0")
	if _, err := os.Stat(filepath.Join(n.repositories, "vendor.repo")); err != nil {
		t.Fatal("a dryrun removed the repository file")
	}
	storageResultIs(t, n.run(absent, false), "succeeded", "changed", "removed the repository vendor; removed the signing key f7ffe6b0")
	for _, name := range []string{filepath.Join(n.repositories, "vendor.repo"), filepath.Join(n.keys, "vendor.asc"), filepath.Join(stubToolsDirectory(t), "imported-"+zypperTestKeyID)} {
		if _, err := os.Stat(name); err == nil {
			t.Fatalf("%s is still there", name)
		}
	}
	storageResultIs(t, n.run(absent, false), "succeeded", "unchanged", "")
}

// A key that rpm held before the task, such as the distribution's own, is
// never removed with the repository. A key that two repositories share leaves
// rpm with the last of them. A new key replaces the old one, which leaves
// rpm unless another repository's file holds it.
func TestOfficialExtraZypperRepositoryOnlyRemovesKeysItImported(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	named := func(name string, key string) map[string]any {
		inputs := zypperRepositoryInputs()
		inputs["name"], inputs["signing_key_text"] = name, key
		return inputs
	}
	absent := func(name string) map[string]any { return map[string]any{"name": name, "state": "absent"} }

	// The distribution's key was in rpm before the task.
	storageStubFile(t, "imported-0d15ea5e", "")
	storageResultIs(t, n.run(named("own", zypperKey("0d15ea5e")), false), "succeeded", "changed", "added the repository own at https://download.example.org/leap/16.1/")
	if n.record("own") != "" {
		t.Fatalf("a key that rpm held before was recorded: %q", n.record("own"))
	}
	storageResultIs(t, n.run(absent("own"), false), "succeeded", "changed", "removed the repository own")
	if !n.imported("0d15ea5e") {
		t.Fatal("the key that rpm held before the task left rpm")
	}

	// Two repositories give one key.
	storageResultIs(t, n.run(named("first", zypperKey("5a5a5a5a")), false), "succeeded", "changed", "imported the signing key 5a5a5a5a; added the repository first at https://download.example.org/leap/16.1/")
	storageResultIs(t, n.run(named("second", zypperKey("5a5a5a5a")), false), "succeeded", "changed", "added the repository second at https://download.example.org/leap/16.1/")
	if n.record("first") != "5a5a5a5a\n" || n.record("second") != "5a5a5a5a\n" {
		t.Fatalf("records %q %q", n.record("first"), n.record("second"))
	}
	storageResultIs(t, n.run(absent("first"), false), "succeeded", "changed", "removed the repository first")
	if !n.imported("5a5a5a5a") {
		t.Fatal("a key that another repository holds left rpm")
	}
	storageResultIs(t, n.run(absent("second"), false), "succeeded", "changed", "removed the repository second; removed the signing key 5a5a5a5a")
	if n.imported("5a5a5a5a") || n.record("second") != "" {
		t.Fatal("the last repository did not remove the key")
	}

	// Rotation: the task gives another key; the old one leaves rpm.
	n.run(named("rotated", zypperKey("1111aaaa")), false)
	storageResultIs(t, n.run(named("rotated", zypperKey("2222bbbb")), false), "succeeded", "changed",
		"imported the signing key 2222bbbb; removed the signing key 1111aaaa (the key that the task replaced)")
	if n.imported("1111aaaa") || !n.imported("2222bbbb") || n.record("rotated") != "2222bbbb\n" {
		t.Fatalf("after the rotation: old %v, new %v, record %q", n.imported("1111aaaa"), n.imported("2222bbbb"), n.record("rotated"))
	}
	// A rotation to a key that another repository's file holds keeps that key.
	n.run(named("holder", zypperKey("3333cccc")), false)
	n.run(named("rotating", zypperKey("3333cccc")), false)
	n.run(named("rotating", zypperKey("4444dddd")), false)
	if !n.imported("3333cccc") {
		t.Fatal("a rotation removed a key that another repository holds")
	}
}

// rpm 6 names a key's package by the key's whole fingerprint, so asking for
// gpg-pubkey-<short id> finds nothing. The module finds the key in the list
// of installed keys: a second run is unchanged, and absent removes the key.
func TestOfficialExtraZypperRepositoryFindsKeysNamedByFingerprintInRpm6(t *testing.T) {
	t.Parallel()
	n := newZypperRepositoryNode(t)
	const prefix = "4f50a6114cd5c6976a7f1179"
	rpm6 := `case "$1" in
-q) if [ "$2" = --qf ]; then
      found=1
      for f in "$STUBS"/imported-*; do [ -e "$f" ] || continue; found=0; printf '%s%s\n' ` + prefix + ` "${f##*imported-}"; done
      [ "$found" = 0 ] || echo "package gpg-pubkey is not installed"; exit "$found"
    fi
    echo "package $2 is not installed"; exit 1 ;;
--import) for id in $(grep -o 'KEYID[0-9a-f]*' "$2" | sed 's/KEYID//'); do touch "$STUBS/imported-$id"; done ;;
-e) echo "$*" >> "$STUBS/removed"; rm -f "$STUBS/imported-${2##*` + prefix + `}" ;;
esac
`
	if err := os.WriteFile(filepath.Join(stubToolsDirectory(t), "bodies", "rpm"), []byte(rpm6), 0644); err != nil {
		t.Fatal(err)
	}
	inputs := zypperRepositoryInputs()
	storageResultIs(t, n.run(inputs, false), "succeeded", "changed", "imported the signing key f7ffe6b0; added the repository vendor at https://download.example.org/leap/16.1/")
	if !n.imported(zypperTestKeyID) || n.record("vendor") != zypperTestKeyID+"\n" {
		t.Fatal("the key was not imported and recorded", n.record("vendor"))
	}
	storageResultIs(t, n.run(inputs, false), "succeeded", "unchanged", "")
	storageResultIs(t, n.run(map[string]any{"name": "vendor", "state": "absent"}, false), "succeeded", "changed", "removed the repository vendor; removed the signing key "+zypperTestKeyID)
	if n.imported(zypperTestKeyID) {
		t.Fatal("the key stayed in rpm")
	}
	removed, _ := os.ReadFile(filepath.Join(stubToolsDirectory(t), "removed"))
	if string(removed) != "-e gpg-pubkey-"+prefix+zypperTestKeyID+"\n" {
		t.Fatalf("%q", removed)
	}
}
