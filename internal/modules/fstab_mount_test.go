package modules

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// Stand-ins for the storage programs keep the node's tables in files of the
// fixture's directory: fstab, swaps (/proc/swaps), mountinfo, and the
// signatures wipefs finds per device in signatures/<device name>. Each call is
// recorded in calls. A file named <program>-fails makes that program fail;
// findmnt-rejects makes findmnt --verify reject the new file, and busy makes
// umount fail as for a busy mount.
const storageStandIns = `
findmnt) [ "$1" = --verify ] || exit 2
  if [ -e "$dir/findmnt-rejects" ]; then echo "/srv/data"; echo "   [E] unreachable on boot required source: UUID=1234"; echo "0 parse errors, 1 error, 0 warnings" >&2; exit 1; fi;;
mkswap) printf SWAPSPACE2 | dd of="$1" bs=1 seek=$(( $(getconf PAGESIZE) - 10 )) conv=notrunc 2>/dev/null;;
swapon) echo "$1 file 1024 0 -2" >> "$dir/swaps";;
swapoff) awk -v p="$1" '$1 != p' "$dir/swaps" > "$dir/swaps.new" && mv "$dir/swaps.new" "$dir/swaps";;
systemctl) ;;
findfs) [ -s "$dir/findfs" ] || { echo "findfs: unable to resolve '$1'" >&2; exit 1; }; cat "$dir/findfs";;
wipefs) device=$(basename "$3"); if [ -e "$dir/signatures/$device" ]; then cat "$dir/signatures/$device"; else printf '{"signatures": [\n]}\n'; fi;;
mkfs.ext4|mkfs.xfs) for last; do :; done
  printf '{"signatures": [{"device": "x", "offset": "0x438", "type": "%s", "uuid": null, "label": null}]}\n' "${program#mkfs.}" > "$dir/signatures/$(basename "$last")";;
umount) [ ! -e "$dir/busy" ] || { echo "umount: $1: target is busy." >&2; exit 32; }
  awk -v p="$1" '{ line[NR] = $0; t = $5; gsub(/\\040/, " ", t); if (t == p) last = NR } END { for (i = 1; i <= NR; i++) if (i != last) print line[i] }' "$dir/mountinfo" > "$dir/mountinfo.new" && mv "$dir/mountinfo.new" "$dir/mountinfo";;
mount) if [ "$1" = --options-mode ]; then shift 2; fi
  if [ "$1" = -o ]; then options=${2#remount,} path=$3; else path=$1; options=$(awk -v p="$path" '{ t = $2; gsub(/\\040/, " ", t) } t == p { print $4 }' "$dir/fstab"); fi
  flags=rw atime=relatime extra=
  for o in $(echo "$options" | tr , ' '); do case $o in
    ro|rw) flags=$o;; noatime|relatime) atime=$o;;
    nodev|nosuid|noexec) case "$extra," in *",$o,"*) ;; *) extra="$extra,$o";; esac;;
    dev|suid|exec) extra=$(echo "$extra" | sed "s/,no$o//");;
  esac; done
  if [ "$1" = -o ]; then
    awk -v p="$path" -v o="$flags$extra,$atime" '{ t = $5; gsub(/\\040/, " ", t) } t == p { $6 = o } { print }' "$dir/mountinfo" > "$dir/mountinfo.new" && mv "$dir/mountinfo.new" "$dir/mountinfo"
  else
    awk -v p="$path" -v o="$flags$extra,$atime" '{ t = $2; gsub(/\\040/, " ", t) } t == p { printf "99 1 8:1 / %s %s - %s %s rw\n", $2, o, $3, $1 }' "$dir/fstab" >> "$dir/mountinfo"
  fi;;
`

type storageFixture struct{ dir string }

func newStorageFixture(t *testing.T) storageFixture {
	t.Helper()
	dir := t.TempDir()
	f := storageFixture{dir}
	if err := os.Mkdir(filepath.Join(dir, "signatures"), 0700); err != nil {
		t.Fatal(err)
	}
	f.set(t, "fstab", "# /etc/fstab: static file system information.\nUUID=root-uuid / ext4 errors=remount-ro 0 1\n\n# swap was on /dev/vda3 during installation\n")
	if err := os.Chmod(filepath.Join(dir, "fstab"), 0644); err != nil {
		t.Fatal(err)
	}
	f.set(t, "swaps", "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n")
	f.set(t, "mountinfo", "1 0 8:1 / / rw,relatime - ext4 /dev/vda1 rw,errors=remount-ro\n")
	f.set(t, "calls", "")
	f.set(t, "systemd-booted", "")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, program := range []string{"findmnt", "mkswap", "swapon", "swapoff", "systemctl", "findfs", "wipefs", "mkfs.ext4", "mkfs.xfs", "mount", "umount"} {
		script := "#!/bin/sh\ndir=" + dir + "\nprogram=" + program + "\necho \"$program $*\" >> \"$dir/calls\"\n" +
			"[ ! -e \"$dir/$program-fails\" ] || { echo \"$program: injected failure\" >&2; exit 1; }\ncase $program in" + storageStandIns + "esac\n"
		writeStandInProgram(t, filepath.Join(bin, program), script)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for variable, value := range map[*string]string{&fstabPath: filepath.Join(dir, "fstab"), &procSwapsPath: filepath.Join(dir, "swaps"),
		&mountinfoPath: filepath.Join(dir, "mountinfo"), &systemdBootedMarker: filepath.Join(dir, "systemd-booted")} {
		previous := *variable
		*variable = value
		t.Cleanup(func() { *variable = previous })
	}
	user, group := swapFileUserID, swapFileGroupID
	swapFileUserID, swapFileGroupID = os.Getuid(), os.Getgid()
	t.Cleanup(func() { swapFileUserID, swapFileGroupID = user, group })
	return f
}

func (f storageFixture) set(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f storageFixture) remove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
		t.Fatal(err)
	}
}

func (f storageFixture) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// calls returns and forgets the recorded calls, without the probes and checks
// that only read.
func (f storageFixture) calls(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, call := range strings.Split(strings.TrimSpace(f.read(t, "calls")), "\n") {
		if call != "" && !strings.HasPrefix(call, "wipefs ") && !strings.HasPrefix(call, "findfs ") {
			out = append(out, call)
		}
	}
	f.set(t, "calls", "")
	return out
}

func requireResult(t *testing.T, r protocol.Result, status, change, reason string) {
	t.Helper()
	if r.Status != status || r.Change != change || !strings.Contains(r.Reason, reason) {
		t.Fatalf("want %s %s %q, got %+v", status, change, reason, r)
	}
}

func dataMount(t *testing.T, f storageFixture) FstabMount {
	t.Helper()
	path := filepath.Join(f.dir, "srv data")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	return FstabMount{MountPoint: path, MountSource: "/dev/sdb1", FilesystemType: "ext4", Options: []string{"noatime"}, CheckFilesystemAtBoot: "never", State: "mounted"}
}

func TestFstabMountWritesTheLineAndMounts(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	before := f.read(t, "fstab")
	escaped := strings.ReplaceAll(m.MountPoint, " ", `\040`)
	line := "/dev/sdb1 " + escaped + " ext4 noatime 0 0\n"

	preview := (Runtime{Dryrun: true}).FstabMount(ctx, m)
	requireResult(t, preview, "succeeded", "would_change", "would mount "+m.MountPoint)
	if preview.FileChange == nil || !strings.Contains(preview.FileChange.Diff, "+"+strings.TrimSuffix(line, "\n")) {
		t.Fatalf("%+v", preview.FileChange)
	}
	if f.read(t, "fstab") != before || len(f.calls(t)) != 0 {
		t.Fatal("dryrun changed the node")
	}

	made := (Runtime{}).FstabMount(ctx, m)
	requireResult(t, made, "succeeded", "changed", "mounted "+m.MountPoint)
	// Every other line and comment stays as it was; the new line is added.
	if got := f.read(t, "fstab"); got != before+line {
		t.Fatalf("%q", got)
	}
	if want := []string{"findmnt --verify --tab-file " + filepath.Dir(fstabPath), "systemctl daemon-reload", "mount " + m.MountPoint}; !callsStartWith(f.calls(t), want) {
		t.Fatal("calls")
	}
	if live, _ := liveMountAt(m.MountPoint); live == nil || live.source != "/dev/sdb1" || live.mountOptions != "rw,noatime" {
		t.Fatalf("%+v", live)
	}
	for _, dryrun := range []bool{false, true} {
		requireResult(t, (Runtime{Dryrun: dryrun}).FstabMount(ctx, m), "succeeded", "unchanged", "")
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatal(calls)
	}
}

// callsStartWith compares calls with prefixes, one each, in order.
func callsStartWith(calls, prefixes []string) bool {
	if len(calls) != len(prefixes) {
		return false
	}
	for i := range calls {
		if !strings.HasPrefix(calls[i], prefixes[i]) {
			return false
		}
	}
	return true
}

// Changed options remount before the line is written, so a run cut off
// between the two remounts again; a live mount whose flags drifted from the
// listed options is remounted with the corrections.
func TestFstabMountRemountsWhenOptionsDiffer(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "succeeded", "changed", "")
	f.calls(t)

	m.Options = []string{"nodev", "nosuid"}
	preview := (Runtime{Dryrun: true}).FstabMount(ctx, m)
	requireResult(t, preview, "succeeded", "would_change", "would remount "+m.MountPoint+" with options nodev,nosuid")
	r := (Runtime{}).FstabMount(ctx, m)
	requireResult(t, r, "succeeded", "changed", "remounted")
	if calls := f.calls(t); !callsStartWith(calls, []string{"mount --options-mode ignore -o remount,nodev,nosuid,nosuid,nodev,relatime " + m.MountPoint, "findmnt --verify", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	if live, _ := liveMountAt(m.MountPoint); live.mountOptions != "rw,nodev,nosuid,relatime" && live.mountOptions != "rw,nosuid,nodev,relatime" {
		t.Fatal(live.mountOptions)
	}
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "succeeded", "unchanged", "")

	// Someone remounted it read-only: the listed options say rw.
	f.set(t, "mountinfo", strings.Replace(f.read(t, "mountinfo"), "rw,nodev", "ro,nodev", 1))
	f.calls(t)
	r = (Runtime{}).FstabMount(ctx, m)
	requireResult(t, r, "succeeded", "changed", "remounted")
	if calls := f.calls(t); len(calls) != 1 || calls[0] != "mount --options-mode ignore -o remount,nodev,nosuid,rw "+m.MountPoint {
		t.Fatal(calls)
	}
}

// A live mount of another source or type fails the task and changes nothing.
func TestFstabMountRefusesAnotherLiveSource(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "succeeded", "changed", "")
	before := f.read(t, "fstab")
	f.calls(t)
	for _, other := range []func(*FstabMount){
		func(m *FstabMount) { m.MountSource = "/dev/sdc1" },
		func(m *FstabMount) { m.FilesystemType = "xfs" },
		func(m *FstabMount) { m.MountSource = "UUID=0a1b" },
	} {
		task := m
		other(&task)
		for _, dryrun := range []bool{true, false} {
			r := (Runtime{Dryrun: dryrun}).FstabMount(ctx, task)
			requireResult(t, r, "failed", "unchanged", "has /dev/sdb1 (ext4) mounted, and the task asks for "+task.MountSource+" ("+task.FilesystemType+")")
			if !strings.Contains(r.Reason, "unmounted_and_removed_from_fstab") {
				t.Fatal(r.Reason)
			}
		}
	}
	if f.read(t, "fstab") != before || len(f.calls(t)) != 0 {
		t.Fatal("a refused task changed the node")
	}
	// A UUID that findfs resolves to the mounted device is the same source.
	f.set(t, "findfs", "/dev/sdb1\n")
	task := m
	task.MountSource = "UUID=0a1b"
	r := (Runtime{}).FstabMount(ctx, task)
	requireResult(t, r, "succeeded", "changed", "wrote the line")
	if !strings.Contains(f.read(t, "fstab"), "UUID=0a1b ") {
		t.Fatal(f.read(t, "fstab"))
	}
}

func TestFstabMountUnmountsAndRemovesTheLine(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	before := f.read(t, "fstab")
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "succeeded", "changed", "")
	f.calls(t)
	gone := FstabMount{MountPoint: m.MountPoint, State: "unmounted_and_removed_from_fstab"}

	f.set(t, "busy", "")
	r := (Runtime{}).FstabMount(ctx, gone)
	requireResult(t, r, "failed", "unchanged", "target is busy")
	if live, _ := liveMountAt(m.MountPoint); live == nil || f.read(t, "fstab") == before {
		t.Fatal("a busy unmount changed the node")
	}
	f.remove(t, "busy")
	f.calls(t)

	requireResult(t, (Runtime{Dryrun: true}).FstabMount(ctx, gone), "succeeded", "would_change", "would unmount "+m.MountPoint+", then remove the line")
	requireResult(t, (Runtime{}).FstabMount(ctx, gone), "succeeded", "changed", "unmounted "+m.MountPoint+", removed the line")
	if f.read(t, "fstab") != before {
		t.Fatalf("%q", f.read(t, "fstab"))
	}
	if live, _ := liveMountAt(m.MountPoint); live != nil {
		t.Fatal("still mounted")
	}
	if _, err := os.Stat(m.MountPoint); err != nil {
		t.Fatal("the directory was removed")
	}
	requireResult(t, (Runtime{}).FstabMount(ctx, gone), "succeeded", "unchanged", "")
}

// The line is written only after findmnt --verify accepts the new file; a
// failure after the line was written keeps it and names it; the next run
// does only what is missing.
func TestFstabMountAfterFailures(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	before := f.read(t, "fstab")

	f.set(t, "findmnt-rejects", "")
	r := (Runtime{}).FstabMount(ctx, m)
	requireResult(t, r, "failed", "unchanged", "findmnt --verify did not accept the new "+fstabPath+", so it was not written: /srv/data")
	if r.FailureKind != "validation" || !strings.Contains(r.Reason, "unreachable on boot required source") || f.read(t, "fstab") != before {
		t.Fatalf("%+v", r)
	}
	f.remove(t, "findmnt-rejects")

	f.set(t, "mount-fails", "")
	r = (Runtime{}).FstabMount(ctx, m)
	requireResult(t, r, "failed", "changed", "mount "+m.MountPoint+" failed: mount: injected failure; already done: wrote the line of "+m.MountPoint)
	f.remove(t, "mount-fails")
	f.calls(t)
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "succeeded", "changed", "mounted")
	if calls := f.calls(t); len(calls) != 1 || calls[0] != "mount "+m.MountPoint {
		t.Fatal(calls)
	}

	// Two lines for the mount point: the task does not choose one.
	f.set(t, "fstab", f.read(t, "fstab")+"/dev/sdc1 "+strings.ReplaceAll(m.MountPoint, " ", `\040`)+" xfs defaults 0 0\n")
	requireResult(t, (Runtime{}).FstabMount(ctx, m), "failed", "unchanged", "has 2 lines for "+m.MountPoint+" (lines 5, 6)")
}

func TestFstabMountWaitsForTheMountPointAndPrograms(t *testing.T) {
	ctx := context.Background()
	f := newStorageFixture(t)
	m := dataMount(t, f)
	missing := m
	missing.MountPoint = filepath.Join(f.dir, "not yet")
	for _, dryrun := range []bool{true, false} {
		want := map[bool]string{true: "not_checked", false: "failed"}[dryrun]
		requireResult(t, (Runtime{Dryrun: dryrun}).FstabMount(ctx, missing), want, map[bool]string{true: "unknown", false: "unknown"}[dryrun], "the mount point "+missing.MountPoint+" does not exist yet")
	}
	link := m
	link.MountPoint = filepath.Join(f.dir, "link")
	if err := os.Symlink(m.MountPoint, link.MountPoint); err != nil {
		t.Fatal(err)
	}
	requireResult(t, (Runtime{}).FstabMount(ctx, link), "failed", "unchanged", "is a symbolic link")
	t.Setenv("PATH", t.TempDir())
	requireResult(t, (Runtime{Dryrun: true}).FstabMount(ctx, m), "not_checked", "unknown", missingProgramError("findmnt").Error())
}

func TestFstabMountRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    FstabMount
		want string
	}{
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4"}, "state is required: mounted or unmounted_and_removed_from_fstab"},
		{FstabMount{MountPoint: "/srv/data", FilesystemType: "ext4", CheckFilesystemAtBoot: "never", State: "mounted"}, "mount_source is required with state: mounted"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", CheckFilesystemAtBoot: "never", State: "mounted"}, "filesystem_type is required with state: mounted"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext 4", CheckFilesystemAtBoot: "never", State: "mounted"}, "filesystem_type is required"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4", Options: []string{"noatime,nodev"}, CheckFilesystemAtBoot: "never", State: "mounted"}, `"noatime,nodev" is not one mount option`},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4", Options: []string{"bind"}, CheckFilesystemAtBoot: "never", State: "mounted"}, "does not manage bind mounts"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4", Options: []string{"remount"}, CheckFilesystemAtBoot: "never", State: "mounted"}, "MMX remounts the filesystem"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4", CheckFilesystemAtBoot: "sometimes", State: "mounted"}, "check_filesystem_at_boot is required"},
		{FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", State: "unmounted_and_removed_from_fstab"}, "accepts only mount_point and state"},
	} {
		r := (Runtime{}).FstabMount(context.Background(), tc.m)
		if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, tc.want) {
			t.Errorf("%+v: %+v", tc.m, r)
		}
	}
	for _, tc := range []struct {
		rule  func(any) error
		value string
	}{{mountPointRule, "/"}, {mountPointRule, "srv/data"}, {mountSourceRule, "two\nlines"}} {
		if tc.rule(tc.value) == nil {
			t.Errorf("%q passed", tc.value)
		}
	}
}

func TestFstabLinesAndMountFlags(t *testing.T) {
	t.Parallel()
	l, ok := parseFstabLine(`  /dev/sdb1	/srv/my\040data  ext4 noatime`)
	if !ok || l != (fstabLine{"/dev/sdb1", "/srv/my data", "ext4", "noatime", "0", "0"}) {
		t.Fatalf("%+v", l)
	}
	if l.text() != `/dev/sdb1 /srv/my\040data ext4 noatime 0 0` || fstabUnescape(fstabEscape(`a b\c`)) != `a b\c` {
		t.Fatal(l.text())
	}
	for _, line := range []string{"# comment", "", "only two"} {
		if _, ok := parseFstabLine(line); ok {
			t.Fatal(line)
		}
	}
	for _, tc := range []struct {
		options []string
		live    string
		want    []string
	}{
		{nil, "rw,relatime", nil},
		{[]string{"defaults"}, "rw,nosuid,noatime", []string{"suid", "relatime"}},
		{[]string{"ro", "noexec", "strictatime"}, "rw,noexec,relatime", []string{"ro", "strictatime"}},
		{[]string{"user"}, "rw,relatime", []string{"nosuid", "nodev", "noexec"}},
		{[]string{"user", "exec"}, "rw,nosuid,nodev,relatime", nil},
		{[]string{"nodiratime", "nosymfollow", "atime"}, "rw,noatime", []string{"nodiratime", "nosymfollow", "relatime"}},
	} {
		if got := expectedMountFlags(tc.options).corrections(liveMountFlags(tc.live)); !slices.Equal(got, tc.want) {
			t.Errorf("%v on %s: %v", tc.options, tc.live, got)
		}
	}
	live := &liveMount{mountOptions: "rw,relatime", filesystemOptions: "rw,sync,lazytime"}
	if got := filesystemFlagsMissing([]string{"sync", "dirsync", "nolazytime", "commit=30"}, live); !reflect.DeepEqual(got, []string{"dirsync", "nolazytime"}) {
		t.Fatal(got)
	}
}

func TestFstabMountRequiresExplicitMountChoices(t *testing.T) {
	m := FstabMount{MountPoint: "/srv/data", MountSource: "/dev/sdb1", FilesystemType: "ext4", State: "mounted", Options: []string{"defaults"}}
	if err := ValidateFstabMount(m); err == nil || !strings.Contains(err.Error(), "check_filesystem_at_boot is required") {
		t.Fatal(err)
	}
	for _, item := range []struct{ choice, number string }{{"never", "0"}, {"as_root_filesystem", "1"}, {"after_root_filesystem", "2"}} {
		m.CheckFilesystemAtBoot = item.choice
		if err := ValidateFstabMount(m); err != nil {
			t.Fatal(err)
		}
		if got := m.line().checkOrder; got != item.number {
			t.Fatal(got)
		}
	}
	m.Options = nil
	if err := ValidateFstabMount(m); err == nil || !strings.Contains(err.Error(), "options is required") {
		t.Fatal(err)
	}
}
