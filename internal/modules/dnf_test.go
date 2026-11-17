package modules

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/value"
)

// dnfFixture fakes dnf and rpm. pkg-<name> holds an installed package's
// version as rpm prints it; <name>.available lists the versions the
// repositories offer; plan is what dnf --assumeno prints. calls records each
// dnf command that changes something.
func dnfFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "root/etc/dnf/plugins"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_DNF_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	scripts := map[string]string{
		"dnf": `F=$MMX_DNF_FIXTURE
for last; do :; done
printf 'dnf %s\n' "$*" >> "$F/all-calls"
[ ! -x "$F/repository-check" ] || "$F/repository-check" "$@"
# Inactive streams hide their packages, including transaction dependencies.
# A query succeeds with no matches; an installation cannot resolve them.
case " $* " in *" repoquery "*|*" install "*)
if [ -f "$F/stream-packages" ] && ! grep -q 'state=enabled' "$F/root/etc/dnf/modules.d/postgresql.module" 2>/dev/null; then
  while IFS= read -r name; do
    for arg; do
      case "$arg" in "$name"|"$name"-[0-9]*)
        case " $* " in *" repoquery "*) exit 0;; esac
        echo "No match for argument: $name" >&2
        echo "Error: Unable to find a match: $name (inactive module stream)" >&2
        exit 1;;
      esac
    done
    case " $* " in *" install "*)
      if awk -v name="$name" '$1 == name { found=1 } END { exit !found }' "$F/plan" 2>/dev/null; then
        echo "Error: dependency $name is filtered out by modular filtering" >&2
        exit 1
      fi;;
    esac
  done < "$F/stream-packages"
fi;;
esac
case " $* " in
*" updateinfo list "*)
  [ ! -f "$F/advisory-error" ] || { echo "advisory lookup failed" >&2; exit 1; }
  cat "$F/advisories" 2>/dev/null || true; exit 0;;
*" repoquery "*)
  cat "$F/$last.available" 2>/dev/null || true; exit 0;;
*" repolist "*) printf 'repo id         repo name    status\nappstream       AppStream    enabled\nelrepo-kernel   ELRepo       disabled\n'; exit 0;;
*" module list "*) cat "$F/modules" 2>/dev/null || true; exit 0;;
*" --assumeno "*) [ ! -x "$F/preview-check" ] || "$F/preview-check" "$@"; if [ "$last" = autoremove ]; then cat "$F/removable" 2>/dev/null || true; else cat "$F/plan" 2>/dev/null || true; fi; echo "Operation aborted."; exit 1;;
esac
printf 'dnf %s\n' "$*" >> "$F/calls"
case " $* " in
*" upgrade --security "*) cp "$F/tool.available" "$F/pkg-tool"; exit 0;;
*" autoremove "*) : > "$F/removable"; exit 0;;
esac
# An upgrade of the whole node empties the plan and, when it upgraded
# something, leaves an old kernel that autoremove removes.
if [ "$last" = upgrade ]; then
	[ ! -s "$F/plan" ] || printf 'Removing unused dependencies:\n kernel-old x86_64 5.14.0-1 @baseos 1 k\n' >> "$F/removable"
	: > "$F/plan"; exit 0
fi
command=
for word; do
	case "$word" in
	-*) continue;;
	esac
	if [ -z "$command" ]; then command=$word; continue; fi
	case "$command" in
	module) command=module-$word;;
	module-enable) mkdir -p "$F/root/etc/dnf/modules.d"; printf '[%s]\nname=%s\nstream=%s\nprofiles=\nstate=enabled\n' "${word%%:*}" "${word%%:*}" "${word#*:}" > "$F/root/etc/dnf/modules.d/${word%%:*}.module";;
	install)
		case "$word" in
		/*) name=$(cut -f1 "$word") version=$(cut -f2 "$word");;
		*) name=${word%%-[0-9]*}; version=${word#"$name"-}; [ "$version" != "$word" ] || version=$(tail -n 1 "$F/$name.available");;
		esac
		[ ! -f "$F/$name.provider" ] || name=$(cat "$F/$name.provider")
		echo "$version" > "$F/pkg-$name";;
	upgrade) tail -n 1 "$F/$word.available" > "$F/pkg-$word";;
	remove) rm -f "$F/pkg-$word";;
	esac
done
[ "$command" != install ] || [ ! -x "$F/after-install" ] || "$F/after-install"
`,
		"rpm": `F=$MMX_DNF_FIXTURE
case "$1 ${2:-}" in
"-q --whatprovides") for name; do :; done
	for f in "$F"/*.provider; do [ "$f" = "$F/$name.provider" ] && { cat "$f"; exit 0; }; done
	echo "no package provides $name"; exit 1;;
"-q "*) name=$4
	[ -f "$F/pkg-$name" ] || { echo "package $name is not installed"; exit 1; }
	cat "$F/pkg-$name";;
"-qp "*) cat "$4";;
"--eval "*)
	a=$(printf '%s' "$2" | cut -d'"' -f2) b=$(printf '%s' "$2" | cut -d'"' -f4)
	if [ "$a" = "$b" ]; then echo 0
	elif [ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n 1)" = "$a" ]; then echo 1
	else echo -1; fi;;
esac
`,
	}
	for name, script := range scripts {
		writeStandInProgram(t, filepath.Join(dir, name), "#!/bin/sh\nset -eu\n"+script)
	}
	return dir
}

func dnfIn(dir string) *DNF { return &DNF{Root: filepath.Join(dir, "root")} }

// The version lock plugin is what makes dnf 4 keep a version.
func withLockPlugin(t *testing.T, dir string) {
	t.Helper()
	put(t, filepath.Join(dir, "root/etc/dnf/plugins/versionlock.conf"), "[main]\nenabled = 1\n")
}

func TestDNFInspectsWithRPM(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-tool"), "(none):1.0-1.el9\n")
	put(t, filepath.Join(dir, "pkg-docker-ce"), "3:27.3.1-1.el9\n")
	for name, want := range map[string]PackageState{
		"tool":      {Name: "tool", Version: "1.0-1.el9", State: "installed"},
		"docker-ce": {Name: "docker-ce", Version: "3:27.3.1-1.el9", State: "installed"},
		"missing":   {Name: "missing", State: "absent"},
	} {
		if got, err := dnfIn(dir).Inspect(context.Background(), name); err != nil || got != want {
			t.Fatal(name, got, err)
		}
	}
}

// A plain installation installs only the missing names and deletes their
// entries in dnf 4's version lock list; other entries stay.
func TestDNFInstallsMissingPackagesAndDropsTheirLocks(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-one"), "1.0-1\n")
	put(t, filepath.Join(dir, "two.available"), "0:2.0-1\n")
	list := filepath.Join(dir, "root/etc/dnf/plugins/versionlock.list")
	put(t, list, "# admin\none-1.*.*\nonetwo-3.0-1.*\n!excluded-1.0\n"+versionLockMark("one")+"\none-1.0-1.*\n")
	manager, ctx, p := dnfIn(dir), context.Background(), Package{Manager: "dnf", Packages: []PackageEntry{{Name: "one", Version: newestWhenFirstInstalled}, {Name: "two", Version: newestWhenFirstInstalled}}}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(list); string(b) != "# admin\none-1.*.*\nonetwo-3.0-1.*\n!excluded-1.0\n" {
		t.Fatalf("%q", b)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y install one-1.0-1 two\n" {
		t.Fatal(calls)
	}
}

// dnf installs a package by a name it only provides, as nmap-ncat for nc on
// the Red Hat family; MMX checks packages by name, and its failure names the
// package to write.
func TestDNFNamesThePackageThatProvidesAName(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "nc.available"), "3:7.92-3.el9\n")
	put(t, filepath.Join(dir, "nc.provider"), "nmap-ncat\n")
	r := dnfIn(dir).Ensure(context.Background(), Runtime{}, Package{Manager: "dnf", Packages: []PackageEntry{{Name: "nc", Version: newestWhenFirstInstalled}}})
	if r.Status != "failed" || !strings.Contains(r.Reason, "there is no package named nc on this node: dnf installed nmap-ncat, which provides nc; name the package nmap-ncat instead") {
		t.Fatal(r)
	}
}

// On dnf 4, version writes the pattern into the version lock list and
// installs the newest matching version; refusals write nothing.
func TestDNFKeepsAPackageInsideAVersionPattern(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:1.1-1\n0:2.0-1\n")
	ctx, p := context.Background(), Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: "1.*"}}}
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "name: python3-dnf-plugin-versionlock") {
		t.Fatal(r)
	}
	withLockPlugin(t, dir)
	manager := dnfIn(dir)
	want := []any{map[string]any{"name": "tool", "to": "1.1-1"}}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) || fixtureCalls(t, dir) != "" {
		t.Fatal(r, fixtureCalls(t, dir))
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	list := filepath.Join(dir, "root/etc/dnf/plugins/versionlock.list")
	if b, _ := os.ReadFile(list); string(b) != versionLockMark("tool")+"\ntool-0:1.*\n" {
		t.Fatalf("%q", b)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y install tool-1.1-1\n" {
		t.Fatal(calls)
	}
	// The run looked the versions up in a fresh index; the dryrun in the
	// index the node has.
	if all := readState(t, filepath.Join(dir, "all-calls")); !strings.Contains(all, "dnf -q repoquery") || !strings.Contains(all, "dnf -q --refresh repoquery") {
		t.Fatal(all)
	}
	put(t, filepath.Join(dir, "pkg-tool"), "2.0-1\n")
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "2.0-1 is installed, which is newer than 1.1-1; to install the older version, add allow_downgrade: true") {
		t.Fatal(r)
	}
	p.Packages[0].Version = "3.*"
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "no version of tool in the node's repositories matches 3.*; available: 1.0-1, 1.1-1, 2.0-1") {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(list); string(b) != versionLockMark("tool")+"\ntool-0:1.*\n" {
		t.Fatalf("a refused task changed the lock: %q", b)
	}
}

// On dnf 5, as on Fedora 41 and newer, the program dnf is a link to dnf5.
// A dnf5 installed beside dnf 4 leaves dnf 4 in charge.
func dnfFiveFixture(t *testing.T) string {
	t.Helper()
	dir := dnfFixture(t)
	if err := os.Remove(filepath.Join(dir, "dnf")); err != nil {
		t.Fatal(err)
	}
	// dnf5 runs the fixture's dnf script; the link must end in a file named
	// dnf5, as it does on a node, not in the launcher of the stand-ins.
	setDnfFiveScript(t, dir, "exec /bin/sh \""+filepath.Join(dir, standInScriptsDirectory, "dnf")+"\" \"$@\"\n")
	if err := os.Symlink("dnf5", filepath.Join(dir, "dnf")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func setDnfFiveScript(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "dnf5"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDNFFiveIsTheProgramDnfLinkedToDnf5(t *testing.T) {
	dir := dnfFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "dnf5"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if isDnfFive(context.Background()) {
		t.Fatal("a dnf5 beside dnf 4 must not count")
	}
	dnfFiveFixture(t)
	if !isDnfFive(context.Background()) {
		t.Fatal("dnf is a link to dnf5")
	}
}

// On dnf 5 no plugin package is needed: version writes a table into dnf's
// own lock file, and a rule that ends in .* becomes a range of versions.
func TestDNFFiveKeepsAPackageInsideAVersionPattern(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:1.1-1\n0:2.0-1\n")
	ctx, p := context.Background(), Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: "1.*"}}}
	manager := dnfIn(dir)
	want := []any{map[string]any{"name": "tool", "to": "1.1-1"}}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) || fixtureCalls(t, dir) != "" {
		t.Fatal(r, fixtureCalls(t, dir))
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	table := filepath.Join(dir, "root", dnfFiveLockTable)
	wantTable := "version = \"1.0\"\n\n[[packages]]\nname = \"tool\"\ncomment = \"Written by MMX for package tool\"\n\n" +
		"[[packages.conditions]]\nkey = \"evr\"\ncomparator = \">=\"\nvalue = \"0:1\"\n\n" +
		"[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"<\"\nvalue = \"0:2~\"\n"
	if got := readState(t, table); got != wantTable {
		t.Fatalf("%q\nwant %q", got, wantTable)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y install tool-1.1-1\n" {
		t.Fatal(calls)
	}
	// A rule dnf 5's lock cannot hold is refused before anything changes,
	// also in a dryrun, and says what to write instead.
	p.Packages[0].Version = "1.*-1"
	for _, dryrun := range []bool{true, false} {
		r := dnfIn(dir).Ensure(ctx, Runtime{Dryrun: dryrun}, p)
		if r.Status != "failed" || !strings.Contains(r.Reason, "cannot be kept on this node. Its dnf is dnf 5") {
			t.Fatal(r)
		}
	}
	if got := readState(t, table); got != wantTable {
		t.Fatalf("a refused task changed the lock: %q", got)
	}
	// Dropping the rule removes only MMX's table.
	p.Packages[0].Version = newestWhenFirstInstalled
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if got := readState(t, table); got != "version = \"1.0\"\n" {
		t.Fatalf("%q", got)
	}
}

// The ranges that stand for dnf 4's patterns, and the rules dnf 5 cannot hold.
func TestDNFFiveLockBlocks(t *testing.T) {
	condition := func(comparator, value string) string {
		return "\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"" + comparator + "\"\nvalue = \"" + value + "\"\n"
	}
	head := "[[packages]]\nname = \"docker-ce\"\ncomment = \"Written by MMX for package docker-ce\"\n"
	for rule, want := range map[string]string{
		"3:27.3.*":    head + condition(">=", "3:27.3") + condition("<", "3:27.4~"),
		"16.*":        head + condition(">=", "0:16") + condition("<", "0:17~"),
		"1.9.*":       head + condition(">=", "0:1.9") + condition("<", "0:1.10~"),
		"3:27.3.1-1":  head + condition("=", "3:27.3.1-1"),
		"27.3.1-1.fc": head + condition("=", "0:27.3.1-1.fc"),
		"*":           "",
	} {
		if got, err := dnfFiveLockBlock("docker-ce", rule); err != nil || got != want {
			t.Errorf("%s: %q %v\nwant %q", rule, got, err, want)
		}
	}
	for _, rule := range []string{"*:16.*", "16*", "1.2.3-*", "*.el9", "1.*.3", "1.2a.*", "99999999999999999999.*"} {
		if got, err := dnfFiveLockBlock("docker-ce", rule); err == nil {
			t.Errorf("%s: accepted: %q", rule, got)
		}
	}
}

// dnfFiveText is dnfFiveLockText for a test that expects no refusal.
func dnfFiveText(t *testing.T, existing, name, block string) string {
	t.Helper()
	text, err := dnfFiveLockText([]byte(existing), name, block)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// MMX replaces only its own table and leaves the administrator's, also for
// the same package.
func TestDNFFiveLockTextKeepsAdministratorTables(t *testing.T) {
	admin := "version = \"1.0\"\n\n[[packages]]\nname = \"tool\"\ncomment = \"administrator\"\n\n[[packages.conditions]]\nkey = \"epoch\"\ncomparator = \"=\"\nvalue = \"0\"\n"
	block1, _ := dnfFiveLockBlock("tool", "1.*")
	block2, _ := dnfFiveLockBlock("tool", "2.*")
	one := dnfFiveText(t, admin, "tool", block1)
	if !strings.HasPrefix(one, admin+"\n[[packages]]") || strings.Count(one, "[[packages]]") != 2 {
		t.Fatalf("%q", one)
	}
	if again := dnfFiveText(t, one, "tool", block1); again != one {
		t.Fatalf("not idempotent: %q", again)
	}
	two := dnfFiveText(t, one, "tool", block2)
	if strings.Contains(two, "value = \"0:1\"") || !strings.Contains(two, "value = \"0:2\"") || !strings.HasPrefix(two, admin) || strings.Count(two, "[[packages]]") != 2 {
		t.Fatalf("%q", two)
	}
	if gone := dnfFiveText(t, two, "tool", ""); gone != admin {
		t.Fatalf("%q", gone)
	}
	if same := dnfFiveText(t, admin, "tool", ""); same != admin {
		t.Fatalf("%q", same)
	}
	if n, err := dnfFiveOwnedTables([]byte(two), "tool"); n != 1 || err != nil {
		t.Fatal(n, err)
	}
}

// An administrator's file with comments, inner spaces, and a trailing comment
// on a header: every table stays, comment lines stay with the table they
// precede, and MMX's table is recognised by its comment's value whatever the
// quotes and spaces.
func TestDNFFiveLockTextKeepsCommentsAndSpacedHeaders(t *testing.T) {
	admin := "# Locks of the web team.\nversion = \"1.0\"\n\n# nginx stays below 1.31.\n[[ packages ]]   # web\nname = 'nginx'\ncomment = \"web team\"\n\n  [[ packages . conditions ]] # first\n  key = \"evr\"\n  comparator = \"<\"\n  value = \"2:1.31\"\n\n# Keep tree.\n# Really.\n[[packages]]\nname = \"tree\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"=\"\nvalue = \"0:2.3.1-2.fc45\"\n"
	block, _ := dnfFiveLockBlock("tool", "1.*")
	text := dnfFiveText(t, admin, "tool", block)
	for _, kept := range []string{"# Locks of the web team.\nversion", "# nginx stays below 1.31.\n[[ packages ]]   # web\nname = 'nginx'", "[[ packages . conditions ]] # first", "# Keep tree.\n# Really.\n[[packages]]\nname = \"tree\""} {
		if !strings.Contains(text, kept) {
			t.Fatalf("lost %q in %q", kept, text)
		}
	}
	if _, _, err := parseDnfFiveLock(text); err != nil {
		t.Fatal(err)
	}
	// dnf rewrites the comment line with other quotes or spaces.
	rewritten := strings.Replace(text, "comment = \"Written by MMX for package tool\"", "comment   =   'Written by MMX for package tool'  # dnf", 1)
	if n, err := dnfFiveOwnedTables([]byte(rewritten), "tool"); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if again := dnfFiveText(t, rewritten, "tool", strings.Replace(block, "\"Written", "\"Written", 1)); strings.Count(again, "Written by MMX for package tool") != 1 {
		t.Fatalf("duplicated: %q", again)
	}
	if gone := dnfFiveText(t, text, "tool", ""); gone != admin {
		t.Fatalf("%q\nwant %q", gone, admin)
	}
}

// MMX does not edit a file it does not recognise, and says why.
func TestDNFFiveLockTextRefusesWhatItDoesNotRecognise(t *testing.T) {
	block, _ := dnfFiveLockBlock("tool", "1.*")
	for name, file := range map[string]string{
		"an inline array":     "version = \"1.0\"\npackages = [ { name = \"nginx\" } ]\n",
		"another table":       "version = \"1.0\"\n[options]\nx = \"y\"\n",
		"a multi-line string": "version = \"1.0\"\n\n[[packages]]\nname = \"nginx\"\ncomment = \"\"\"\nlong\n\"\"\"\n",
		"an unknown key":      "version = \"1.0\"\n\n[[packages]]\nname = \"nginx\"\nextra = \"1\"\n",
		"garbage":             "version = \"1.0\"\ngarbage\n",
		"a condition alone":   "version = \"1.0\"\n[[packages.conditions]]\nkey = \"evr\"\n",
	} {
		_, err := dnfFiveLockText([]byte(file), "tool", block)
		if err == nil || !strings.Contains(err.Error(), "does not recognise") || !strings.Contains(err.Error(), "fails every command") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The administrator's tables keep a version out as dnf 5 reads them: tables of
// a package are alternatives, the conditions of a table all hold.
func TestDNFFiveAdministratorTablesExcludeVersions(t *testing.T) {
	dir := dnfFixture(t)
	_ = dir
	file := "version = \"1.0\"\n\n[[packages]]\nname = \"nginx\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"<\"\nvalue = \"2:1.30.6\"\n\n" +
		"[[packages]]\nname = \"nginx\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"=\"\nvalue = \"2:1.31.0-1.fc45\"\n\n" +
		"[[packages]]\nname = \"tree\"\n\n[[packages.conditions]]\nkey = \"epoch\"\ncomparator = \">\"\nvalue = \"5\"\n\n" +
		"[[packages]]\nname = \"mixed\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"!=\"\nvalue = \"1.0-1\"\n\n[[packages.conditions]]\nkey = \"arch\"\ncomparator = \"=\"\nvalue = \"aarch64\"\n"
	ctx := context.Background()
	for _, c := range []struct {
		name, version string
		excluded      bool
	}{
		{"nginx", "2:1.30.5-2.fc45", false}, {"nginx", "2:1.30.6-2.fc45", true}, {"nginx", "2:1.31.0-1.fc45", false},
		{"nginx", "2:1.31.1-1.fc45", true}, {"nginx", "1:9", false}, {"tree", "2.3.1-2.fc45", true}, {"tree", "6:1", false},
		{"other", "1", false}, {"mixed", "1.0-1", true}, {"mixed", "2.0-1", false},
	} {
		got, err := dnfFiveExcludes(ctx, []byte(file), c.name, c.version)
		if err != nil || got != c.excluded {
			t.Errorf("%s %s: excluded %v, want %v (%v)", c.name, c.version, got, c.excluded, err)
		}
	}
}

// When a task changes the rule of a package MMX already keeps at a version,
// the old rule in dnf 5's lock file hides the new version, and the
// preview cannot stage the new rule. It runs with excludes off.
func TestDNFFiveRuleChangePreviewIgnoresTheOldRule(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:2.0-1\n")
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	old, _ := dnfFiveLockBlock("tool", "1.*")
	table := filepath.Join(dir, "root", dnfFiveLockTable)
	put(t, table, dnfFiveText(t, "", "tool", old))
	// dnf 5 refuses a preview that the lock excludes, unless excludes are off.
	writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
case " $* " in *" --setopt=disable_excludes=* "*) exit 0;; esac
echo "Argument 'tool-2.0-1' matches only packages excluded by versionlock." >&2
exit 1
`)
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: "2.*"}}}
	r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, p)
	if r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	// Dropping the rule needs the same: the old rule hides the newest version.
	p.Packages[0].Version = newestOnEveryRun
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, p); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	p.Packages[0].Version = "2.*"
	// An entry without an MMX rule gets dnf 5's refusal as it is.
	put(t, table, "version = \"1.0\"\n")
	r = dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, p)
	if r.Status != "failed" || !strings.Contains(r.Reason, "excluded by versionlock") {
		t.Fatal(r)
	}
}

// config-manager has the commands enable and disable on dnf 5, and comes in
// the package dnf5-plugins.
func TestDNFFiveSwitchesAnExistingRepository(t *testing.T) {
	dir := dnfFiveFixture(t)
	on := true
	r := Repository{Manager: "dnf", ExistingRepository: "elrepo-kernel", Enabled: &on}
	if got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil); got.Change != "changed" {
		t.Fatal(got)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -q config-manager enable elrepo-kernel\n" {
		t.Fatal(calls)
	}
	setDnfFiveScript(t, dir, "case \" $* \" in *\" repolist \"*) printf 'repo id repo name status\\nelrepo-kernel ELRepo disabled\\n';; *) echo 'Unknown argument \"config-manager\" for command \"dnf5\".' >&2; exit 2;; esac\n")
	got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil)
	if got.Status != "failed" || !strings.Contains(got.Reason, "packages: [{name: dnf5-plugins, version: newest_when_first_installed}]") {
		t.Fatal(got)
	}
}

// dnf 5 names an advisory's type in words, not as Severity/Sec.
func TestDNFFiveSecurityAdvisories(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "advisories"), "Name                   Type     Severity  Package           Issued\nFEDORA-2026-b9a53ff63c security Important  dnf5-5.4.8.0-2.fc45.x86_64 2026-11-13 20:22:22\n")
	if err := dnfIn(dir).checkAdvisories(context.Background(), Package{}); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "advisories"), "Name                   Type     Severity  Package           Issued\n")
	if err := dnfIn(dir).checkAdvisories(context.Background(), Package{}); err == nil {
		t.Fatal("a header alone lists no advisory")
	}
}

// A upgrade in dnf 5's table lists the package it replaces under the new
// one; that line is not a change of its own.
func TestDNFFiveReadsTransactionTables(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "pkg-dnf5"), "0:5.4.7.0-1.fc45\n")
	five := `Package                          Arch   Version          Repository                            Size
Upgrading:
 dnf5                            x86_64 0:5.4.8.0-2.fc45 updates                            3.7 MiB
   replacing dnf5                x86_64 0:5.4.7.0-1.fc45 9e2c3ceb60c14200b89a1505fba036c9   3.7 MiB
Installing weak dependencies:
 libdnf5-plugin-systemd-inhibit  x86_64 4:5.4.8.0-2.fc45 updates                          28.0 KiB
Removing unused dependencies:
 libibverbs       x86_64 0:64.0-3.fc45    fedora       1.3 MiB

Transaction Summary:
 Installing:         1 package
 Upgrading:          1 package
 Replacing:          1 package
`
	want := []any{map[string]any{"name": "dnf5", "from": "5.4.7.0-1.fc45", "to": "5.4.8.0-2.fc45"},
		map[string]any{"name": "libdnf5-plugin-systemd-inhibit", "to": "4:5.4.8.0-2.fc45"},
		map[string]any{"name": "libibverbs", "from": "64.0-3.fc45"}}
	got, err := dnfIn(dir).transactionChanges(context.Background(), five)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v\nwant %v", got, err, want)
	}
}

// dnf's transaction table becomes {name, from, to}; dnf does not show the
// version an upgrade replaces, so rpm gives it.
func TestDNFReadsTransactionTables(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-acl"), "2.3.1-4.el9\n")
	put(t, filepath.Join(dir, "pkg-rsyslog"), "8.2510.0-2.el9_8.2\n")
	four := `Dependencies resolved.
================================================================================
 Package             Arch     Version                         Repository   Size
================================================================================
Installing:
 nginx               x86_64   2:1.20.1-28.el9_8.6             appstream    38 k
Upgrading:
 acl                 x86_64   2.4.0-1.el9_8                   baseos       74 k
 rsyslog             x86_64   8.2604.0-4.el9_9.1              appstream   885 k
     replacing  rsyslog-logrotate.x86_64 8.2510.0-2.el9_8.2
Removing unused dependencies:
 rocky-logos-httpd   noarch   90.17-1.el9                     @appstream   24 k

Transaction Summary
================================================================================
`
	// The package that rsyslog obsoletes is removed, as Rocky Linux 9.9's
	// rsyslog removed rsyslog-logrotate.
	want := []any{map[string]any{"name": "nginx", "to": "2:1.20.1-28.el9_8.6"}, map[string]any{"name": "acl", "from": "2.3.1-4.el9", "to": "2.4.0-1.el9_8"},
		map[string]any{"name": "rsyslog", "from": "8.2510.0-2.el9_8.2", "to": "8.2604.0-4.el9_9.1"},
		map[string]any{"name": "rsyslog-logrotate", "from": "8.2510.0-2.el9_8.2"},
		map[string]any{"name": "rocky-logos-httpd", "from": "90.17-1.el9"}}
	got, err := dnfIn(dir).transactionChanges(context.Background(), four)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v\nwant %v", got, err, want)
	}
}

// Removal tells dnf whether to remove the packages only the named ones
// needed, and lists what dnf removes.
func TestDNFRemovalSettings(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-nginx"), "2:1.20.1-28.el9\n")
	put(t, filepath.Join(dir, "plan"), "Removing:\n nginx   x86_64   2:1.20.1-28.el9   @appstream   147 k\nTransaction Summary\n")
	manager, ctx := dnfIn(dir), context.Background()
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "nginx"}}, State: "absent"}
	want := []any{map[string]any{"name": "nginx", "from": "2:1.20.1-28.el9"}}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	put(t, filepath.Join(dir, "pkg-nginx"), "2:1.20.1-28.el9\n")
	p.AlsoRemovePackagesOnlyItNeeded = true
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "changed" {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	want2 := "dnf -y remove --setopt=clean_requirements_on_remove=false nginx\ndnf -y remove --setopt=clean_requirements_on_remove=true nginx\n"
	if calls := fixtureCalls(t, dir); calls != want2 {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, want2)
	}
}

// upgrade with names installs the missing ones and upgrades the others, with
// the repositories the task enables; an unknown repository id fails first. A
// run refreshes dnf's index once per node job before it reads the newest
// versions; a dryrun reads the index the node has.
func TestDNFUpgradesNamedPackages(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-old"), "1.0-1\n")
	put(t, filepath.Join(dir, "old.available"), "0:2.0-1\n")
	put(t, filepath.Join(dir, "new.available"), "0:1.0-1\n")
	put(t, filepath.Join(dir, "plan"), "Upgrading:\n old   x86_64   2.0-1   baseos   1 k\nTransaction Summary\n")
	ctx := context.Background()
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "new", Version: newestOnEveryRun}, {Name: "old", Version: newestOnEveryRun}}, RepositoriesEnabledOnlyForThisTask: []string{"elrepo-kernel"}}
	manager := dnfIn(dir)
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	want := "dnf -y --enablerepo=elrepo-kernel install new old-2.0-1\n"
	if calls := fixtureCalls(t, dir); calls != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, want)
	}
	// A second upgrade in the same node job reads the index the first one
	// refreshed.
	put(t, filepath.Join(dir, "plan"), "Transaction Summary\n")
	if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "dnf", Packages: []PackageEntry{{Name: "old", Version: newestOnEveryRun}}}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	all := readState(t, filepath.Join(dir, "all-calls"))
	if strings.Count(all, "--refresh") != 1 || !strings.Contains(all, "dnf --assumeno --enablerepo=elrepo-kernel install new old\n") || !strings.Contains(all, "dnf --assumeno --refresh --enablerepo=elrepo-kernel install new old\n") {
		t.Fatal(all)
	}
	p.RepositoriesEnabledOnlyForThisTask = []string{"missing"}
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "this node defines no repository missing; it has: appstream, elrepo-kernel") {
		t.Fatal(r)
	}
}

// A package file is installed by its path, checked against the task's name,
// and locked at its version.
func TestDNFInstallsAPackageFile(t *testing.T) {
	dir := dnfFixture(t)
	withLockPlugin(t, dir)
	file := filepath.Join(dir, "rclone.rpm")
	put(t, file, "rclone\t(none):1.68.2-1")
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "rclone", Version: "1.68.2-1", PackageFileSource: "rclone.rpm", PackageFile: &assets.Ref{Path: file}}}}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pkg-rclone")); string(b) != "(none):1.68.2-1\n" {
		t.Fatalf("%q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "root/etc/dnf/plugins/versionlock.list")); string(b) != versionLockMark("rclone")+"\nrclone-0:1.68.2-1\n" {
		t.Fatalf("%q", b)
	}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	p.Packages[0].Name = "restic"
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "holds the package rclone, not restic") {
		t.Fatal(r)
	}
}

// A node whose system is an image cannot install packages with dnf.
func TestDNFRefusesAnImageNode(t *testing.T) {
	dir := dnfFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, "root/run"), 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "root/run/ostree-booted"), "")
	r := dnfIn(dir).Ensure(context.Background(), Runtime{}, Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: newestWhenFirstInstalled}}})
	if r.Status != "failed" || !strings.Contains(r.Reason, "/run/ostree-booted") {
		t.Fatal(r)
	}
}

// A package task for another manager's module names the module to use.
func TestPackageTaskOnAnotherManagerNamesTheRightModule(t *testing.T) {
	r := (Runtime{}).Package(context.Background(), &DNF{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "nginx", Version: newestWhenFirstInstalled}}})
	if r.Status != "failed" || r.Reason != "apt_package installs with apt, but this node's package manager is dnf (the node fact package_manager); give the node task a condition so that it runs only on nodes that use apt" {
		t.Fatal(r)
	}
}

// A repository file has one section, always with gpgcheck=1 and the key MMX
// installed; a login makes it root-only.
func TestDNFRepositoryFile(t *testing.T) {
	off, priority := false, 50
	r := Repository{Manager: "dnf", Name: "epel", Description: "EPEL 9", Metalink: "https://mirrors.fedoraproject.org/metalink?repo=epel-9&arch=$basearch",
		Enabled: &off, Priority: &priority, IncludePkgs: []string{"htop", "tmux"}, Exclude: []string{"kernel*"},
		Username: "{{ .vars.user }}", Password: "{{ .secrets.password }}"}
	s := value.Scope{"vars": map[string]any{"user": "customer"}, "secrets": map[string]any{"password": value.Sensitive("p4ss")}}
	text, err := dnfRepositoryText(r, "/etc/pki/rpm-gpg/RPM-GPG-KEY-epel", s)
	want := "[epel]\nname=EPEL 9\nmetalink=https://mirrors.fedoraproject.org/metalink?repo=epel-9&arch=$basearch\nenabled=0\ngpgcheck=1\n" +
		"gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-epel\npriority=50\nincludepkgs=htop tmux\nexclude=kernel*\nusername=customer\npassword=p4ss\n"
	if err != nil || text != want {
		t.Fatalf("%q %v", text, err)
	}
	r = Repository{Manager: "dnf", Name: "docker", Description: "Docker", BaseURL: []string{"https://a.example/$releasever", "https://b.example/$releasever"}}
	text, err = dnfRepositoryText(r, "/k", nil)
	if err != nil || text != "[docker]\nname=Docker\nbaseurl=https://a.example/$releasever https://b.example/$releasever\nenabled=1\ngpgcheck=1\ngpgkey=file:///k\n" {
		t.Fatalf("%q %v", text, err)
	}
}

// existing_repository switches a repository through config-manager, and fails
// for an id the node does not define.
func TestDNFSwitchesAnExistingRepository(t *testing.T) {
	dir := dnfFixture(t)
	on := true
	r := Repository{Manager: "dnf", ExistingRepository: "elrepo-kernel", Enabled: &on}
	if got := dnfIn(dir).Source(context.Background(), Runtime{Dryrun: true}, r, nil); got.Change != "would_change" {
		t.Fatal(got)
	}
	if got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil); got.Change != "changed" {
		t.Fatal(got)
	}
	want := "dnf -q config-manager --set-enabled elrepo-kernel\n"
	if calls := fixtureCalls(t, dir); calls != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, want)
	}
	r.ExistingRepository = "appstream"
	if got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil); got.Change != "unchanged" {
		t.Fatal(got)
	}
	r.ExistingRepository = "crb"
	if got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil); got.Status != "failed" || !strings.Contains(got.Reason, "defines no repository crb; it has: appstream, elrepo-kernel") {
		t.Fatal(got)
	}
	writeStandInProgram(t, filepath.Join(dir, "dnf"), "#!/bin/sh\ncase \" $* \" in *\" repolist \"*) printf 'repo id repo name status\\nelrepo-kernel ELRepo disabled\\n';; *) echo 'No such command: config-manager' >&2; exit 1;; esac\n")
	r.ExistingRepository = "elrepo-kernel"
	got := dnfIn(dir).Source(context.Background(), Runtime{}, r, nil)
	if got.Status != "failed" || !strings.Contains(got.Reason, "packages: [{name: dnf-plugins-core, version: newest_when_first_installed}]") {
		t.Fatal(got)
	}

}

// enable_dnf_module_stream enables the stream once, before the installation,
// and refuses another or switched-off stream, a package installed from
// outside the stream, and a node without the module, changing nothing.
func TestDNFEnablesAModuleStream(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "stream-packages"), "postgresql-server\npostgresql\n")
	put(t, filepath.Join(dir, "modules"), "Rocky Linux 9 - AppStream\nName       Stream Profiles           Summary\npostgresql 15     client, server [d] PostgreSQL\npostgresql 16     client, server [d] PostgreSQL\n")
	put(t, filepath.Join(dir, "postgresql-server.available"), "0:16.15-1.module+el9\n")
	ctx := context.Background()
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "postgresql-server", Version: newestWhenFirstInstalled}}, EnableDnfModuleStream: "postgresql:16"}
	if r := dnfIn(dir).Ensure(ctx, Runtime{Dryrun: true}, p); r.Status != "not_checked" || r.Change != "unknown" || fixtureCalls(t, dir) != "" {
		t.Fatal(r, fixtureCalls(t, dir))
	}
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y module enable postgresql:16\ndnf -y install postgresql-server\n" {
		t.Fatal(calls)
	}
	refusals := map[string]func(){
		"has the stream 16 enabled, not 15": func() { p.EnableDnfModuleStream = "postgresql:15" },
		"is switched off on this node": func() {
			put(t, filepath.Join(dir, "root/etc/dnf/modules.d/postgresql.module"), "[postgresql]\nstream=\nstate=disabled\n")
		},
		"has no stream 18 on this node; it has: 15, 16": func() {
			put(t, filepath.Join(dir, "root/etc/dnf/modules.d/postgresql.module"), "[postgresql]\nstream=\nstate=\n")
			p.EnableDnfModuleStream = "postgresql:18"
		},
		"postgresql-server 16.15-1.module+el9 is already installed from outside the stream postgresql:15": func() {
			p.EnableDnfModuleStream = "postgresql:15"
		},
		"have no module postgresql, so no stream postgresql:16: on this node other versions are separate package names, such as postgresql16-server": func() {
			put(t, filepath.Join(dir, "modules"), "")
			p.EnableDnfModuleStream = "postgresql:16"
		},
	}
	for _, want := range []string{"has the stream 16 enabled, not 15", "is switched off on this node", "has no stream 18 on this node; it has: 15, 16",
		"postgresql-server 16.15-1.module+el9 is already installed from outside the stream postgresql:15",
		"have no module postgresql, so no stream postgresql:16: on this node other versions are separate package names, such as postgresql16-server"} {
		refusals[want]()
		before := fixtureCalls(t, dir)
		if r := dnfIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, want) || fixtureCalls(t, dir) != before {
			t.Fatal(want, r)
		}
	}
	two := dnfIn(dir)
	p.EnableDnfModuleStream = "postgresql:15"
	two.claim(p)
	p.EnableDnfModuleStream = "postgresql:16"
	if err := two.claim(p); err == nil || !strings.Contains(err.Error(), "enables both postgresql:15 and postgresql:16") {
		t.Fatal(err)
	}
}

// dnf hides inactive-stream packages and their dependencies. A real run
// enables the requested stream before resolving versions and dependencies;
// a dryrun leaves it inactive and cannot check the installation.
func TestDNFResolvesPackagesAfterEnablingTheStream(t *testing.T) {
	for _, policy := range []string{"16.*", newestOnEveryRun, newestWhenFirstInstalled} {
		t.Run(policy, func(t *testing.T) {
			dir := dnfFixture(t)
			withLockPlugin(t, dir)
			put(t, filepath.Join(dir, "stream-packages"), "postgresql-server\npostgresql\n")
			put(t, filepath.Join(dir, "modules"), "postgresql 16 server PostgreSQL\n")
			put(t, filepath.Join(dir, "postgresql-server.available"), "0:16.15-1.module+el9\n")
			put(t, filepath.Join(dir, "postgresql.available"), "0:16.15-1.module+el9\n")
			put(t, filepath.Join(dir, "plan"), "Installing:\n postgresql-server x86_64 16.15-1.module+el9 appstream 1 k\n postgresql x86_64 16.15-1.module+el9 appstream 1 k\nTransaction Summary\n")
			writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
F=$MMX_DNF_FIXTURE
# The stream must be real, while the proposed locks are still temporary.
grep -q 'state=enabled' "$F/root/etc/dnf/modules.d/postgresql.module"
[ ! -e "$F/root/etc/dnf/plugins/versionlock.list" ]
for arg; do case "$arg" in --setopt=pluginconfpath=*)
 plugins=${arg#*=}; plugins=${plugins#*=}
 list=$(sed -n 's/^locklist=//p' "$plugins/versionlock.conf")
 # A NEVR interpretation of postgresql-server-16.* can lock postgresql
 # as well as postgresql-server. An explicit epoch disambiguates the name.
 if grep -q '^postgresql-server-' "$list"; then
   grep -q '^postgresql-server-0:16.\*$' "$list" || {
     echo 'Error: postgresql dependency is filtered out by exclude filtering' >&2
     exit 1
   }
 fi;; esac; done
`)
			writeStandInProgram(t, filepath.Join(dir, "repository-check"), `#!/bin/sh
set -eu
case " $* " in *" module list "*|*" module enable "*|*" repoquery "*|*" install "*)
 case " $* " in *" --enablerepo=elrepo-kernel "*) :;; *) exit 1;; esac;;
esac
`)
			writeStandInProgram(t, filepath.Join(dir, "after-install"), `#!/bin/sh
printf '0:16.15-1.module+el9\n' > "$MMX_DNF_FIXTURE/pkg-postgresql"
`)
			task := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "postgresql-server", Version: policy}}, EnableDnfModuleStream: "postgresql:16", RepositoriesEnabledOnlyForThisTask: []string{"elrepo-kernel"}}
			// Dryrun cannot make the stream's packages visible.
			preview := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, task)
			if preview.Status != "not_checked" || preview.Change != "unknown" || fixtureCalls(t, dir) != "" {
				t.Fatal(preview, fixtureCalls(t, dir))
			}
			for _, path := range []string{"root/etc/dnf/modules.d/postgresql.module", "root/etc/dnf/plugins/versionlock.list", "pkg-postgresql-server", "pkg-postgresql"} {
				if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
					t.Fatalf("dryrun changed %s: %v", path, err)
				}
			}
			applied := dnfIn(dir).Ensure(context.Background(), Runtime{}, task)
			if applied.Status != "succeeded" || applied.Change != "changed" {
				t.Fatal(applied)
			}
			for _, name := range []string{"postgresql-server", "postgresql"} {
				installed, err := dnfIn(dir).Inspect(context.Background(), name)
				if err != nil || installed.Version != "16.15-1.module+el9" {
					t.Fatal(installed, err)
				}
			}
			calls := fixtureCalls(t, dir)
			if !strings.HasPrefix(calls, "dnf -y --enablerepo=elrepo-kernel module enable postgresql:16\n") {
				t.Fatal(calls)
			}
			put(t, filepath.Join(dir, "plan"), "")
			enabledPreview := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, task)
			if enabledPreview.Status != "succeeded" || enabledPreview.Change != "unchanged" || fixtureCalls(t, dir) != calls {
				t.Fatal(enabledPreview, fixtureCalls(t, dir))
			}
		})
	}
}

func TestDNFRefusedStreamVersionLeavesOnlyTheStreamEnabled(t *testing.T) {
	dir := dnfFixture(t)
	withLockPlugin(t, dir)
	put(t, filepath.Join(dir, "stream-packages"), "postgresql-server\npostgresql\n")
	put(t, filepath.Join(dir, "modules"), "postgresql 16 server PostgreSQL\n")
	put(t, filepath.Join(dir, "postgresql-server.available"), "0:16.15-1.module+el9\n")
	task := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "postgresql-server", Version: "99.*"}}, EnableDnfModuleStream: "postgresql:16"}
	for attempt, wantChange := range []string{"changed", "unknown"} {
		result := dnfIn(dir).Ensure(context.Background(), Runtime{}, task)
		if result.Status != "failed" || result.Change != wantChange || !strings.Contains(result.Reason, "no version of postgresql-server") {
			t.Fatal(attempt, result)
		}
		if state := readState(t, filepath.Join(dir, "root/etc/dnf/modules.d/postgresql.module")); !strings.Contains(state, "state=enabled") {
			t.Fatal(state)
		}
		for _, path := range []string{"root/etc/dnf/plugins/versionlock.list", "pkg-postgresql-server", "pkg-postgresql"} {
			if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
				t.Fatalf("refusal changed %s: %v", path, err)
			}
		}
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y module enable postgresql:16\n" {
		t.Fatal(calls)
	}
	// Correcting the version continues from the enabled stream.
	task.Packages[0].Version = "16.*"
	put(t, filepath.Join(dir, "plan"), "Installing:\n postgresql-server x86_64 16.15-1.module+el9 appstream 1 k\nTransaction Summary\n")
	resumed := dnfIn(dir).Ensure(context.Background(), Runtime{}, task)
	if resumed.Status != "succeeded" || resumed.Change != "changed" || strings.Count(fixtureCalls(t, dir), "module enable") != 1 {
		t.Fatal(resumed, fixtureCalls(t, dir))
	}
}

func TestDNFRefusedStreamTransactionKeepsTheEnabledStream(t *testing.T) {
	dir := dnfFixture(t)
	withLockPlugin(t, dir)
	put(t, filepath.Join(dir, "stream-packages"), "postgresql-server\npostgresql\n")
	put(t, filepath.Join(dir, "modules"), "postgresql 16 server PostgreSQL\n")
	put(t, filepath.Join(dir, "postgresql-server.available"), "0:16.15-1.module+el9\n")
	writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
grep -q 'state=enabled' "$MMX_DNF_FIXTURE/root/etc/dnf/modules.d/postgresql.module"
echo 'Error: postgresql-server requires a dependency that cannot be installed' >&2
exit 1
`)
	task := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "postgresql-server", Version: "16.*"}}, EnableDnfModuleStream: "postgresql:16"}
	result := dnfIn(dir).Ensure(context.Background(), Runtime{}, task)
	if result.Status != "failed" || result.Change != "changed" || !strings.Contains(result.Reason, "dependency that cannot be installed") {
		t.Fatal(result)
	}
	if state := readState(t, filepath.Join(dir, "root/etc/dnf/modules.d/postgresql.module")); !strings.Contains(state, "state=enabled") {
		t.Fatal(state)
	}
	for _, path := range []string{"root/etc/dnf/plugins/versionlock.list", "pkg-postgresql-server", "pkg-postgresql"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("refused transaction changed %s: %v", path, err)
		}
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y module enable postgresql:16\n" {
		t.Fatal(calls)
	}
}

func TestDNFChecksStreamPrerequisitesBeforeEnablement(t *testing.T) {
	for _, invalid := range []string{"missing versionlock plugin", "package file", "removal"} {
		t.Run(invalid, func(t *testing.T) {
			dir := dnfFixture(t)
			put(t, filepath.Join(dir, "modules"), "postgresql 16 server PostgreSQL\n")
			task := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "postgresql-server", Version: "16.*"}}, EnableDnfModuleStream: "postgresql:16"}
			switch invalid {
			case "package file":
				task.Packages[0].PackageFileSource = "postgresql.rpm"
			case "removal":
				task.State = "absent"
				task.Packages[0].Version = ""
			}
			result := dnfIn(dir).Ensure(context.Background(), Runtime{}, task)
			if result.Status != "failed" || result.Change != "unknown" || fixtureCalls(t, dir) != "" {
				t.Fatal(result, fixtureCalls(t, dir))
			}
			if _, err := os.Stat(filepath.Join(dir, "root/etc/dnf/modules.d/postgresql.module")); !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
}

func TestDNFSecurityUpgrade(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "advisories"), "i RLSA-2026:75768 Important/Sec. tool-2.0-1.x86_64\n")
	put(t, filepath.Join(dir, "plan"), "Upgrading:\n tool x86_64 2.0-1 baseos 10 k\nTransaction Summary\n")
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	put(t, filepath.Join(dir, "tool.available"), "2.0-1\n")
	put(t, filepath.Join(dir, "pkg-unrelated"), "1.0-1\n")
	security := true
	p := Package{Manager: "dnf", UpgradeAll: true, OnlySecurityUpdates: &security}
	manager := dnfIn(dir)
	want := []any{map[string]any{"name": "tool", "from": "1.0-1", "to": "2.0-1"}}
	for _, dryrun := range []bool{true, false} {
		r := manager.Ensure(context.Background(), Runtime{Dryrun: dryrun}, p)
		change := "changed"
		if dryrun {
			change = "would_change"
		}
		if r.Status != "succeeded" || r.Change != change || !reflect.DeepEqual(r.Value, want) {
			t.Fatal(r)
		}
		installed, err := manager.Inspect(context.Background(), "tool")
		if err != nil || (dryrun && installed.Version != "1.0-1") || (!dryrun && installed.Version != "2.0-1") {
			t.Fatal(installed, err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "all-calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"dnf --assumeno upgrade --security\n", "dnf --assumeno --refresh upgrade --security\n", "dnf -q --cacheonly updateinfo list --all\n", "dnf -y upgrade --security\n"} {
		if !strings.Contains(string(calls), command) {
			t.Fatal(command, string(calls))
		}
	}
	if got := fixtureCalls(t, dir); got != "dnf -y upgrade --security\n" {
		t.Fatal(got)
	}
	if installed, err := manager.Inspect(context.Background(), "unrelated"); err != nil || installed.Version != "1.0-1" {
		t.Fatal(installed, err)
	}
}

func TestDNFSecurityUpgradeAdvisoryAvailability(t *testing.T) {
	for _, advisory := range []string{"", "i ALAS2023-example Important/Sec. tool-1.0-1.x86_64\n", "ALAS2023-example bugfix tool-1.0-1.x86_64\n", "lookup-error"} {
		t.Run(advisory, func(t *testing.T) {
			dir := dnfFixture(t)
			put(t, filepath.Join(dir, "advisories"), advisory)
			if advisory == "lookup-error" {
				put(t, filepath.Join(dir, "advisory-error"), "yes")
			}
			security := true
			p := Package{Manager: "dnf", UpgradeAll: true, OnlySecurityUpdates: &security}
			for _, dryrun := range []bool{true, false} {
				r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: dryrun}, p)
				switch advisory {
				case "":
					if r.Status != "failed" || !strings.Contains(r.Reason, "the node's package sources list no advisories for its packages, so dnf cannot select security updates; use upgrade_all_packages_within_their_version_rules: true without only_security_updates there") {
						t.Fatal(r)
					}
				case "lookup-error":
					if r.Status != "failed" || !strings.Contains(r.Reason, "advisory lookup failed") {
						t.Fatal(r)
					}
				default:
					if r.Status != "succeeded" || r.Change != "unchanged" {
						t.Fatal(r)
					}
				}
				if fixtureCalls(t, dir) != "" {
					t.Fatal(fixtureCalls(t, dir))
				}
			}
		})
	}
}

func TestDNFWholeNodeUpgradeWithoutSecuritySelection(t *testing.T) {
	for _, selection := range []*bool{nil, new(bool)} {
		dir := dnfFixture(t)
		r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, Package{Manager: "dnf", UpgradeAll: true, OnlySecurityUpdates: selection})
		if r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(r)
		}
		calls, err := os.ReadFile(filepath.Join(dir, "all-calls"))
		if err != nil || string(calls) != "dnf --assumeno upgrade\n" {
			t.Fatal(string(calls), err)
		}
	}
	// With also_remove_packages_no_longer_needed, autoremove follows the
	// upgrade, and a run reports what autoremove removes after the upgrade:
	// here also the old kernel that the upgrade leaves. With nothing to
	// upgrade, autoremove still runs.
	dir := dnfFixture(t)
	ctx := context.Background()
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	put(t, filepath.Join(dir, "plan"), "Upgrading:\n tool x86_64 2.0-1 baseos 1 k\n")
	put(t, filepath.Join(dir, "removable"), "Removing unused dependencies:\n libold1 x86_64 1.2-3 @baseos 1 k\n")
	manager, p := dnfIn(dir), Package{Manager: "dnf", UpgradeAll: true, AlsoRemovePackagesNoLongerNeeded: true}
	tool, libold1 := map[string]any{"name": "tool", "from": "1.0-1", "to": "2.0-1"}, map[string]any{"name": "libold1", "from": "1.2-3"}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, []any{tool, libold1}) {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{tool, libold1, map[string]any{"name": "kernel-old", "from": "5.14.0-1"}}) {
		t.Fatal(r)
	}
	put(t, filepath.Join(dir, "removable"), "Removing unused dependencies:\n libold2 x86_64 2.0-1 @baseos 1 k\n")
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{map[string]any{"name": "libold2", "from": "2.0-1"}}) {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y upgrade\ndnf -y autoremove\ndnf -y upgrade\ndnf -y autoremove\n" {
		t.Fatal(calls)
	}
}

func TestDNFWholeNodeUpgradeMayEnableRepositories(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	put(t, filepath.Join(dir, "plan"), "Upgrading:\n tool x86_64 2.0-1 elrepo-kernel 1 k\nTransaction Summary\n")
	p := Package{Manager: "dnf", UpgradeAll: true, RepositoriesEnabledOnlyForThisTask: []string{"elrepo-kernel"}}
	for _, dryrun := range []bool{true, false} {
		if r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: dryrun}, p); r.Status != "succeeded" || r.Change != map[bool]string{true: "would_change", false: "changed"}[dryrun] {
			t.Fatal(r)
		}
	}
	calls := readState(t, filepath.Join(dir, "all-calls"))
	for _, command := range []string{"dnf --assumeno --enablerepo=elrepo-kernel upgrade", "dnf -y --enablerepo=elrepo-kernel upgrade"} {
		if !strings.Contains(calls, command) {
			t.Fatal(calls)
		}
	}
}

func TestDNFReplacesOnlyItsMarkedRuleAndKeepsAdministratorEntries(t *testing.T) {
	dir := dnfFixture(t)
	withLockPlugin(t, dir)
	path := filepath.Join(dir, "root", versionLockList)
	admin := "# administrator's version rule\ntool-1.*.*\n!tool-1.0-1.*\n"
	put(t, path, admin+versionLockMark("tool")+"\ntool-1.1-1.*\n")
	put(t, filepath.Join(dir, "pkg-tool"), "2.0-1\n")
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: "2.*"}}}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	want := admin + versionLockMark("tool") + "\ntool-0:2.*\n"
	if got := readState(t, path); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	p.Packages[0].Version = "*"
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	p.Packages[0].Version = newestWhenFirstInstalled
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if got := readState(t, path); got != admin {
		t.Fatalf("administrator rule changed: %q", got)
	}
}

// EPEL is installed by an earlier task in the real certbot example. A dryrun
// leaves its repository absent; a real run can resolve certbot afterwards.
func TestDNFRepositoryFromEarlierPackageTask(t *testing.T) {
	dir := dnfFixture(t)
	put(t, filepath.Join(dir, "epel-release.available"), "9-10.el9\n")
	put(t, filepath.Join(dir, "certbot.available"), "3.1-1.el9\n")
	writeStandInProgram(t, filepath.Join(dir, "repository-check"), `#!/bin/sh
set -eu
case " $* " in
 *" install "*certbot*)
   [ -f "$MMX_DNF_FIXTURE/pkg-epel-release" ] || {
     echo 'No match for argument: certbot' >&2
     echo 'Error: Unable to find a match: certbot' >&2
     exit 1
   };;
esac
`)
	manager := dnfIn(dir)
	epel := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "epel-release", Version: newestWhenFirstInstalled}}}
	certbot := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "certbot", Version: newestWhenFirstInstalled}}}
	// Without EPEL, a real command cannot resolve certbot. Even naming both
	// packages in one transaction cannot load repository files from an RPM
	// that has not been installed yet. Refuse without installing epel-release.
	combined := epel
	combined.Packages = append(append([]PackageEntry(nil), epel.Packages...), certbot.Packages...)
	refused := manager.Ensure(context.Background(), Runtime{}, combined)
	if refused.Status != "failed" || refused.Change != "unknown" || refused.FailureKind != "missing_prerequisite" || fixtureCalls(t, dir) != "" {
		t.Fatal(refused, fixtureCalls(t, dir))
	}
	for _, dryrun := range []bool{true, false} {
		first := manager.Ensure(context.Background(), Runtime{Dryrun: dryrun}, epel)
		if first.Status != "succeeded" {
			t.Fatal(first)
		}
		result := manager.Ensure(context.Background(), Runtime{Dryrun: dryrun}, certbot)
		if dryrun {
			if result.Status != "not_checked" || result.Change != "unknown" || !strings.Contains(result.Reason, "No match for argument: certbot") || fixtureCalls(t, dir) != "" {
				t.Fatal(result, fixtureCalls(t, dir))
			}
		} else {
			if result.Status != "succeeded" || result.Change != "changed" {
				t.Fatal(result)
			}
			installed, err := manager.Inspect(context.Background(), "certbot")
			if err != nil || installed.Version != "3.1-1.el9" {
				t.Fatal(installed, err)
			}
		}
	}
}

func TestDNFInstallPreviewUsesTaskOnlyRepositories(t *testing.T) {
	for _, policy := range []string{"2.*", newestOnEveryRun} {
		t.Run(policy, func(t *testing.T) {
			dir := dnfFixture(t)
			withLockPlugin(t, dir)
			put(t, filepath.Join(dir, "tool.available"), "2.0-1\n")
			put(t, filepath.Join(dir, "plan"), "Installing:\n tool x86_64 2.0-1 elrepo-kernel 1 k\nTransaction Summary\n")
			writeStandInProgram(t, filepath.Join(dir, "repository-check"), `#!/bin/sh
set -eu
case " $* " in *" install "*|*" repoquery "*)
 case " $* " in *" --enablerepo=elrepo-kernel "*) :;; *)
   echo 'No match for argument: tool' >&2; exit 1;;
 esac;;
esac
`)
			task := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: policy}}, RepositoriesEnabledOnlyForThisTask: []string{"elrepo-kernel"}}
			for _, dryrun := range []bool{true, false} {
				result := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: dryrun}, task)
				want := "changed"
				if dryrun {
					want = "would_change"
				}
				if result.Status != "succeeded" || result.Change != want {
					t.Fatal(result)
				}
			}
			installed, err := dnfIn(dir).Inspect(context.Background(), "tool")
			if err != nil || installed.Version != "2.0-1" {
				t.Fatal(installed, err)
			}
		})
	}
}

// keyFixture fakes gpg and rpm for the key store. A key file's lines are the
// ids of its keys; installed-<id> files are the keys rpm holds; rpm-calls
// records the commands that change the store.
func keyFixture(t *testing.T) (dir, keys string) {
	t.Helper()
	dir = t.TempDir()
	keys = filepath.Join(dir, "keys")
	if err := os.MkdirAll(keys, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_KEY_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeStandInProgram(t, filepath.Join(dir, "gpg"), "#!/bin/sh\nfor last; do :; done\nwhile read -r id; do printf 'pub:-:4096:1:00000000%s:1700000000:::-:::scESC::::::23::0:\\n' \"$id\"; done < \"$last\"\n")
	writeStandInProgram(t, filepath.Join(dir, "rpm"), `#!/bin/sh
F=$MMX_KEY_FIXTURE
case "$1" in
-q) [ -f "$F/installed-${2#gpg-pubkey-}" ];;
--import) echo "$*" >> "$F/rpm-calls"; while read -r id; do touch "$F/installed-$id"; done < "$2";;
-e) echo "$*" >> "$F/rpm-calls"; rm -f "$F/installed-${3#gpg-pubkey-}";;
esac
`)
	return dir, keys
}

func keyCalls(t *testing.T, dir string) string {
	t.Helper()
	calls, _ := os.ReadFile(filepath.Join(dir, "rpm-calls"))
	return string(calls)
}

func installedKey(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, "installed-"+id))
	return err == nil
}

// A key two repositories share is recorded for both, so the last repository
// to go takes it out and the first leaves it.
func TestDNFRepositoryRemovalKeepsAKeyAnotherKeyFileHolds(t *testing.T) {
	dir, keys := keyFixture(t)
	ctx := context.Background()
	keyA, keyB := filepath.Join(keys, "RPM-GPG-KEY-a"), filepath.Join(keys, "RPM-GPG-KEY-b")
	recordA, recordB := filepath.Join(keys, "a.imported_key_ids"), filepath.Join(keys, "b.imported_key_ids")
	put(t, keyA, "aabbccdd\n")
	put(t, keyB, "aabbccdd\n")
	recorded, failure := importKeyFile(ctx, keyA, recordA, nil, keys)
	if failure != nil || !reflect.DeepEqual(recorded, []string{"aabbccdd"}) {
		t.Fatal(recorded, failure)
	}
	put(t, recordA, "aabbccdd\n")
	// The second repository finds the key imported by the first and shares it.
	recordedB, failure := importKeyFile(ctx, keyB, recordB, nil, keys)
	if failure != nil || !reflect.DeepEqual(recordedB, []string{"aabbccdd"}) {
		t.Fatal(recordedB, failure)
	}
	if removed, failure := removeKeys(ctx, recorded, keyA, keys); failure != nil || removed || !installedKey(dir, "aabbccdd") {
		t.Fatal("removed a key another file holds", removed, failure)
	}
	if err := os.Remove(keyA); err != nil {
		t.Fatal(err)
	}
	if removed, failure := removeKeys(ctx, recordedB, keyB, keys); failure != nil || !removed || installedKey(dir, "aabbccdd") {
		t.Fatal("kept a key nothing holds", removed, failure)
	}
}

// A key rpm held before the task, such as the distribution's own, is not
// recorded, so removing the repository never removes it.
func TestDNFRepositoryKeepsAKeyRpmHeldBeforeTheTask(t *testing.T) {
	dir, keys := keyFixture(t)
	ctx := context.Background()
	put(t, filepath.Join(dir, "installed-11111111"), "")
	put(t, filepath.Join(keys, "RPM-GPG-KEY-a"), "11111111\n22222222\n")
	record, failure := importKeyFile(ctx, filepath.Join(keys, "RPM-GPG-KEY-a"), filepath.Join(keys, "a.imported_key_ids"), nil, keys)
	if failure != nil || !reflect.DeepEqual(record, []string{"22222222"}) {
		t.Fatal(record, failure)
	}
	if removed, failure := removeKeys(ctx, record, filepath.Join(keys, "RPM-GPG-KEY-a"), keys); failure != nil || !removed {
		t.Fatal(removed, failure)
	}
	if !installedKey(dir, "11111111") || installedKey(dir, "22222222") {
		t.Fatal("the key rpm held before the task must stay, the imported one go")
	}
}

// A key that changes is imported first; the old key goes after it, unless
// the new file holds it too or another key file does.
func TestDNFRepositoryKeyRotation(t *testing.T) {
	dir, keys := keyFixture(t)
	ctx := context.Background()
	own := filepath.Join(keys, "RPM-GPG-KEY-a")
	put(t, own, "11111111\n33333333\n")
	put(t, filepath.Join(keys, "RPM-GPG-KEY-other"), "33333333\n")
	record, failure := importKeyFile(ctx, own, filepath.Join(keys, "a.imported_key_ids"), nil, keys)
	if failure != nil || !reflect.DeepEqual(record, []string{"11111111", "33333333"}) {
		t.Fatal(record, failure)
	}
	put(t, own, "22222222\n")
	record, failure = importKeyFile(ctx, own, filepath.Join(keys, "a.imported_key_ids"), record, keys)
	if failure != nil || !reflect.DeepEqual(record, []string{"22222222"}) {
		t.Fatal(record, failure)
	}
	if installedKey(dir, "11111111") || !installedKey(dir, "22222222") || !installedKey(dir, "33333333") {
		t.Fatal("rotation must remove the old key only, and keep one another file holds")
	}
	calls := strings.Split(strings.TrimSpace(keyCalls(t, dir)), "\n")
	if calls[len(calls)-2] != "--import "+own || calls[len(calls)-1] != "-e --allmatches gpg-pubkey-11111111" {
		t.Fatalf("the new key must be imported before the old one is removed: %q", calls)
	}
}

// rpm 6, on Fedora 45, names a key's package by the key's whole fingerprint
// (gpg-pubkey-4f50a6114cd5c6976a7f1179655a4b02f577861e-6888bc98), so asking
// for the short id finds nothing. The module finds the key by the end of the
// version and removes it by the full name.
func TestDNFRepositoryFindsAKeyByItsFingerprintInRpm6(t *testing.T) {
	dir, keys := keyFixture(t)
	ctx := context.Background()
	writeStandInProgram(t, filepath.Join(dir, "rpm"), `#!/bin/sh
F=$MMX_KEY_FIXTURE
case "$1" in
-q) if [ "${2:-}" = "--qf" ]; then
		found=1
		for f in "$F"/installed-*; do [ -e "$f" ] || continue; found=0; printf '4f50a6114cd5c6976a7f1179%s\n' "${f##*installed-}"; done
		[ "$found" = 0 ] || echo "package gpg-pubkey is not installed"; exit "$found"
	fi
	exit 1;;
--import) echo "$*" >> "$F/rpm-calls"; while read -r id; do touch "$F/installed-$id"; done < "$2";;
-e) echo "$*" >> "$F/rpm-calls"; name=${3#gpg-pubkey-4f50a6114cd5c6976a7f1179}; rm -f "$F/installed-$name";;
esac
`)
	key := filepath.Join(keys, "RPM-GPG-KEY-a")
	put(t, key, "11111111\n")
	record, failure := importKeyFile(ctx, key, filepath.Join(keys, "a.imported_key_ids"), nil, keys)
	if failure != nil || !reflect.DeepEqual(record, []string{"11111111"}) {
		t.Fatal(record, failure)
	}
	if !rpmKeyInstalled(ctx, "11111111") || rpmKeyInstalled(ctx, "22222222") {
		t.Fatal("the key is found by the end of its fingerprint, and only by that")
	}
	if removed, failure := removeKeys(ctx, record, key, keys); failure != nil || !removed || installedKey(dir, "11111111") {
		t.Fatal(removed, failure)
	}
	if calls := keyCalls(t, dir); !strings.HasSuffix(calls, "-e --allmatches gpg-pubkey-4f50a6114cd5c6976a7f117911111111\n") {
		t.Fatal(calls)
	}
}

// dnf 5's install leaves an installed package alone, where dnf 4's install
// upgrades it; newest_on_every_run on dnf 5 names the newest version.
func TestDNFFiveUpgradesANewestOnEveryRunPackageByNamingItsNewestVersion(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:2.0-1\n")
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: newestOnEveryRun}}}
	want := []any{map[string]any{"name": "tool", "from": "1.0-1", "to": "2.0-1"}}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "changed" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y install tool-2.0-1\n" {
		t.Fatal(calls)
	}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
}

// The administrator's lock tables stay in force on dnf 5: newest_on_every_run
// takes the newest version they allow, because dnf 5's repoquery lists the
// versions they exclude.
func TestDNFFiveNewestOnEveryRunStaysInsideAdministratorTables(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "pkg-tool"), "1.0-1\n")
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:2.0-1\n0:3.0-1\n")
	table := filepath.Join(dir, "root", dnfFiveLockTable)
	put(t, table, "version = \"1.0\"\n\n[[packages]]\nname = \"tool\"\ncomment = \"administrator\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"<\"\nvalue = \"0:3.0\"\n")
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: newestOnEveryRun}}}
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Change != "changed" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dnf -y install tool-2.0-1\n" {
		t.Fatal(calls)
	}
	// Nothing newer is allowed: unchanged, and no install of 3.0.
	put(t, table, "version = \"1.0\"\n\n[[packages]]\nname = \"tool\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"<=\"\nvalue = \"0:2.0-1\"\n")
	if r := dnfIn(dir).Ensure(context.Background(), Runtime{}, p); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}

// A planned version outside an administrator's table fails before MMX writes
// a rule or installs anything.
func TestDNFFiveRefusesAVersionAnAdministratorTableExcludesBeforeWriting(t *testing.T) {
	dir := dnfFiveFixture(t)
	put(t, filepath.Join(dir, "tool.available"), "0:1.0-1\n0:2.0-1\n")
	table := filepath.Join(dir, "root", dnfFiveLockTable)
	original := "version = \"1.0\"\n\n[[packages]]\nname = \"tool\"\ncomment = \"administrator\"\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"<\"\nvalue = \"0:1.5\"\n"
	put(t, table, original)
	p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: "2.*"}}}
	for _, dryrun := range []bool{true, false} {
		r := dnfIn(dir).Ensure(context.Background(), Runtime{Dryrun: dryrun}, p)
		if r.Status != "failed" || !strings.Contains(r.Reason, "outside a version lock that an administrator wrote") {
			t.Fatal(r)
		}
		if got := readState(t, table); got != original {
			t.Fatalf("a refused task changed the lock file: %q", got)
		}
	}
	if calls := fixtureCalls(t, dir); calls != "" {
		t.Fatal(calls)
	}
}

// After dnf 5 deletes the last table, it writes "packages = []". That line is
// recognised, and goes when MMX adds a table, because TOML defines packages
// once.
func TestDNFFiveLockTextReplacesTheEmptyArrayDnfWrites(t *testing.T) {
	empty := "version = \"1.0\"\npackages = []\n"
	block, _ := dnfFiveLockBlock("tool", "1.*")
	text := dnfFiveText(t, empty, "tool", block)
	if strings.Contains(text, "packages = []") || !strings.Contains(text, "[[packages]]") {
		t.Fatalf("%q", text)
	}
	if same := dnfFiveText(t, empty, "tool", ""); same != empty {
		t.Fatalf("%q", same)
	}
	if gone := dnfFiveText(t, text, "tool", ""); gone != "version = \"1.0\"\n" {
		t.Fatalf("%q", gone)
	}
}

// dnf sends a repository login to every address of the repository, so a
// login goes only to https:// addresses; an address that a reference fills
// is checked once known. Without a login, any address stays as it is.
func TestDNFRepositoryLoginNeedsHTTPS(t *testing.T) {
	r := Repository{Manager: "dnf", Name: "vendor", Description: "Vendor", BaseURL: []string{"http://mirror.internal/vendor"}, SigningKeySource: "vendor.asc", Username: "u", Password: "{{ .secrets.p }}"}
	if err := ValidateRepository(r); err == nil || !strings.Contains(err.Error(), "dnf_repository.baseurl[0] http://mirror.internal/vendor does not start with https://") {
		t.Fatal(err)
	}
	r.BaseURL = []string{"{{ .vars.mirror }}"}
	if err := ValidateRepository(r); err != nil {
		t.Fatal("a reference was refused before its value is known:", err)
	}
	for _, address := range []string{"https://mirror.internal/vendor", "HTTPS://mirror.internal/vendor"} {
		r.BaseURL = []string{address}
		if err := ValidateRepository(r); err != nil {
			t.Fatal(address, err)
		}
	}
	r.BaseURL, r.Username, r.Password = []string{"http://mirror.internal/vendor"}, "", ""
	if err := ValidateRepository(r); err != nil {
		t.Fatal(err)
	}
}
