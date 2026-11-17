package modules

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestParseACL(t *testing.T) {
	t.Parallel()
	record := func(entries ...[3]uint32) []byte {
		b := binary.LittleEndian.AppendUint32(nil, aclVersion)
		for _, e := range entries {
			b = binary.LittleEndian.AppendUint16(b, uint16(e[0]))
			b = binary.LittleEndian.AppendUint16(b, uint16(e[1]))
			b = binary.LittleEndian.AppendUint32(b, e[2])
		}
		return b
	}
	// user::rwx user:1001:r-x group::r-- group:2000:rw- mask::rwx other::---
	b := record([3]uint32{0x01, 7, 0xffffffff}, [3]uint32{0x02, 5, 1001}, [3]uint32{0x04, 4, 0xffffffff},
		[3]uint32{0x08, 6, 2000}, [3]uint32{0x10, 7, 0xffffffff}, [3]uint32{0x20, 0, 0xffffffff})
	got, err := parseACL(b, "/srv/x")
	want := map[aclEntry]string{{false, 1001}: "r-x", {true, 2000}: "rw-"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	for _, bad := range [][]byte{nil, b[:7], append(binary.LittleEndian.AppendUint32(nil, 1), b[4:]...)} {
		if _, err := parseACL(bad, "/srv/x"); err == nil {
			t.Fatal("accepted a malformed record", bad)
		}
	}
}

func TestParseCapabilities(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]map[string]string{
		"":                                    {},
		"cap_net_raw=ep":                      {"cap_net_raw": "ep"},
		"cap_net_raw+ep":                      {"cap_net_raw": "ep"},
		"cap_net_bind_service,cap_net_raw=ep": {"cap_net_bind_service": "ep", "cap_net_raw": "ep"},
		"cap_net_raw=eip cap_net_raw-i":       {"cap_net_raw": "ep"},
		"cap_chown+ep-e cap_kill=i":           {"cap_chown": "p", "cap_kill": "i"},
		"cap_net_raw=":                        {},
	} {
		if got, err := parseCapabilities(text); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v %v", text, got, err)
		}
	}
	for _, text := range []string{"=ep", "all=ep", "cap_net_raw=ex", "net_raw=ep"} {
		if _, err := parseCapabilities(text); err == nil {
			t.Errorf("%q was accepted", text)
		}
	}
	if got := capabilityText(map[string]string{"cap_net_raw": "ep", "cap_chown": "ep"}); got != "cap_chown,cap_net_raw=ep" {
		t.Fatal(got)
	}
}

func TestCapabilityToolMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := LinuxCapabilityOnProgramFile{CapabilitiesNotListed: "remove", Path: "/usr/local/bin/pinger", Capabilities: []string{"cap_net_raw"}}
	if r := (Runtime{Dryrun: true}).LinuxCapabilityOnProgramFile(context.Background(), c); r.Status != "not_checked" || !strings.Contains(r.Reason, "libcap2-bin") {
		t.Fatal(r)
	}
	if r := (Runtime{}).LinuxCapabilityOnProgramFile(context.Background(), c); r.Status != "failed" || !strings.Contains(r.Reason, missingProgramError("getcap").Error()) {
		t.Fatal(r)
	}
	a := FileAccessControlList{Path: "/srv", Entries: []AccessControlEntry{}, EntriesNotListed: "remove"}
	if r := (Runtime{Dryrun: true}).FileAccessControlList(context.Background(), a); r.Status != "not_checked" || !strings.Contains(r.Reason, "install the package acl") {
		t.Fatal(r)
	}
}

// The whole lifecycle on files this test owns, with entries for the
// current account and group: a preview, the change, unchanged reruns, a
// failure after part of the work, another try, and removal.
func TestFileAccessControlListLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("access control lists are read on Linux nodes")
	}
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	for _, p := range []string{"sub/nested"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-m", "u:"+me.Uid+":r--", dir).CombinedOutput(); err != nil {
		t.Skip("the test directory's file system does not take access control lists:", string(out))
	}
	if err := exec.Command("setfacl", "-b", dir).Run(); err != nil {
		t.Fatal(err)
	}
	a := FileAccessControlList{Path: dir, Entries: []AccessControlEntry{{UserAccount: me.Username, Permissions: "r-x"}, {Group: group.Name, Permissions: "rwx"}},
		EntriesNotListed: "remove", AlsoApplyToEverythingInside: true, AlsoUseForNewFilesInside: true}
	named := func(path string, attribute string) map[aclEntry]string {
		t.Helper()
		m, err := readACL(path, attribute)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	uid, gid := mustID(t, me.Uid), mustID(t, me.Gid)
	want := map[aclEntry]string{{false, uid}: "r-x", {true, gid}: "rwx"}

	preview := (Runtime{Dryrun: true}).FileAccessControlList(ctx, a)
	if preview.Change != "would_change" || preview.Value.(map[string]any)["number_of_paths_changed"] != 3 || len(named(dir, aclAccess)) != 0 {
		t.Fatal(preview)
	}
	// A failure after part of the work: the default list cannot be written
	// while setfacl refuses -d, so the access entries stay and the next try finishes.
	shim := t.TempDir()
	real, _ := exec.LookPath("setfacl")
	script := "#!/bin/sh\nfor a; do if [ \"$a\" = -d ] || [ \"$a\" = -k ]; then echo refused >&2; exit 1; fi; done\nexec " + real + " \"$@\"\n"
	writeStandInProgram(t, filepath.Join(shim, "setfacl"), script)
	path := os.Getenv("PATH")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+path)
	failed := (Runtime{}).FileAccessControlList(ctx, a)
	if failed.Status != "failed" || failed.Change != "changed" || !reflect.DeepEqual(named(dir, aclAccess), want) || len(named(dir, aclDefault)) != 0 {
		t.Fatal(failed)
	}
	t.Setenv("PATH", path)
	if r := (Runtime{}).FileAccessControlList(ctx, a); r.Change != "changed" || r.Status != "succeeded" {
		t.Fatal(r)
	}
	for _, p := range []string{dir, filepath.Join(dir, "sub"), filepath.Join(dir, "sub/nested")} {
		if !reflect.DeepEqual(named(p, aclAccess), want) {
			t.Fatal(p, named(p, aclAccess))
		}
	}
	if !reflect.DeepEqual(named(dir, aclDefault), want) || len(named(outside, aclAccess)) != 0 || len(named(filepath.Join(dir, "sub/nested"), aclDefault)) != 0 {
		t.Fatal("default entries, or a file behind a link, are wrong")
	}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).FileAccessControlList(ctx, a); r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
	// A changed permission is updated; keep leaves an unlisted entry.
	keep := FileAccessControlList{Path: filepath.Join(dir, "sub/nested"), Entries: []AccessControlEntry{{UserAccount: me.Username, Permissions: "rw-"}}, EntriesNotListed: "keep"}
	if r := (Runtime{}).FileAccessControlList(ctx, keep); r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"updated": []string{"user_account " + me.Username + ": rw-"}}) {
		t.Fatal(r)
	}
	if got := named(filepath.Join(dir, "sub/nested"), aclAccess); got[aclEntry{true, gid}] != "rwx" {
		t.Fatal(got)
	}
	// absent removes every named entry and the default list.
	absent := FileAccessControlList{Path: dir, State: "absent", AlsoApplyToEverythingInside: true, AlsoUseForNewFilesInside: true}
	if r := (Runtime{}).FileAccessControlList(ctx, absent); r.Change != "changed" {
		t.Fatal(r)
	}
	for _, p := range []string{dir, filepath.Join(dir, "sub/nested")} {
		if len(named(p, aclAccess)) != 0 || len(named(p, aclDefault)) != 0 {
			t.Fatal(p)
		}
		if out, _ := exec.Command("getfacl", "--omit-header", "--absolute-names", p).Output(); strings.Contains(string(out), "mask") {
			t.Fatal("the mask stayed after the last named entry", string(out))
		}
	}
	if r := (Runtime{}).FileAccessControlList(ctx, absent); r.Change != "unchanged" {
		t.Fatal(r)
	}
	// A symbolic link at path, and the directory settings on a file, fail.
	for _, bad := range []FileAccessControlList{
		{Path: filepath.Join(dir, "link"), Entries: []AccessControlEntry{}, EntriesNotListed: "remove"},
		{Path: filepath.Join(dir, "sub/nested"), Entries: []AccessControlEntry{}, EntriesNotListed: "remove", AlsoUseForNewFilesInside: true},
	} {
		if r := (Runtime{}).FileAccessControlList(ctx, bad); r.Status != "failed" {
			t.Fatal(r)
		}
	}
	missing := FileAccessControlList{Path: dir, Entries: []AccessControlEntry{{UserAccount: "mmx-missing-user", Permissions: "r--"}}, EntriesNotListed: "keep"}
	if r := (Runtime{Dryrun: true}).FileAccessControlList(ctx, missing); r.Status != "not_checked" {
		t.Fatal(r)
	}
}

func mustID(t *testing.T, s string) uint32 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(n)
}

func TestCapabilitiesKeepUnlistedFlags(t *testing.T) {
	ctx := testContext(t)
	directory := t.TempDir()
	path, state := filepath.Join(directory, "program"), filepath.Join(directory, "capabilities")
	if err := os.WriteFile(path, nil, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("cap_chown=eip cap_net_raw=eip"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := standInPrograms(t, map[string]string{
		"getcap": "printf '%s ' \"$1\"; cat '" + state + "'",
		"setcap": "printf '%s' \"$1\" > '" + state + "'",
	})
	setWorkerEnvironment(t, "PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	task := LinuxCapabilityOnProgramFile{Path: path, Capabilities: []string{"cap_net_raw"}, CapabilitiesNotListed: "keep"}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).LinuxCapabilityOnProgramFile(ctx, task)
		expectedChange := "changed"
		if dryrun {
			expectedChange = "would_change"
		}
		if r.Status != "succeeded" || r.Change != expectedChange || r.Value.(map[string]any)["capabilities_after"] != "cap_chown=eip cap_net_raw=ep" {
			t.Fatalf("%+v", r)
		}
		data, _ := os.ReadFile(state)
		expected := "cap_chown=eip cap_net_raw=ep"
		if dryrun {
			expected = "cap_chown=eip cap_net_raw=eip"
		}
		if string(data) != expected {
			t.Fatalf("%q", data)
		}
	}
	if r := (Runtime{}).LinuxCapabilityOnProgramFile(ctx, task); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	task.CapabilitiesNotListed = "remove"
	if r := (Runtime{}).LinuxCapabilityOnProgramFile(ctx, task); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(state)
	if string(data) != "cap_net_raw=ep" {
		t.Fatalf("%q", data)
	}
	for _, choice := range []string{"", "replace"} {
		task.CapabilitiesNotListed = choice
		if err := ValidateLinuxCapabilityOnProgramFile(task); err == nil || !strings.Contains(err.Error(), "capabilities_not_listed") {
			t.Fatal(err)
		}
	}
	task.State, task.Capabilities, task.CapabilitiesNotListed = "absent", nil, "keep"
	if err := ValidateLinuxCapabilityOnProgramFile(task); err == nil || !strings.Contains(err.Error(), "accepts only path and state") {
		t.Fatal(err)
	}

}

func TestCapabilitiesKeepWithoutEffectiveFlagDoesNotPreviewSuccess(t *testing.T) {
	ctx := testContext(t)
	directory := t.TempDir()
	path, called := filepath.Join(directory, "program"), filepath.Join(directory, "setcap-called")
	if err := os.WriteFile(path, nil, 0755); err != nil {
		t.Fatal(err)
	}
	tools := standInPrograms(t, map[string]string{
		"getcap": "printf '%s cap_chown=p cap_net_raw=p\\n' \"$1\"",
		"setcap": "touch '" + called + "'; echo 'setcap: Invalid argument from mixed effective flags' >&2; exit 1",
	})
	setWorkerEnvironment(t, "PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	task := LinuxCapabilityOnProgramFile{Path: path, Capabilities: []string{"cap_net_raw"}, CapabilitiesNotListed: "keep"}
	result := (Runtime{Dryrun: true}).LinuxCapabilityOnProgramFile(ctx, task)
	if result.Status != "failed" || !strings.Contains(result.Reason, "one effective flag") {
		t.Fatalf("%+v", result)
	}
	// A refused preview describes no change.
	if result.Value != nil {
		t.Fatalf("a failed dryrun reports %v", result.Value)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatal("dryrun called setcap", err)
	}
	result = (Runtime{}).LinuxCapabilityOnProgramFile(ctx, task)
	if result.Status != "failed" || !strings.Contains(result.Reason, "setcap: Invalid argument from mixed effective flags") {
		t.Fatalf("%+v", result)
	}
	if _, err := os.Stat(called); err != nil {
		t.Fatal("apply did not let setcap report its failure", err)
	}
	// setcap failed, so the capabilities did not change: the result names no change.
	if result.Value != nil {
		t.Fatalf("a failed run reports a change that did not happen: %v", result.Value)
	}
}

// A failed access control list task reports in its value only the entries
// that a setfacl call set or removed, never an entry that did not change.
func TestFailedFileAccessControlListReportsOnlyWhatItDid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("access control lists are read on Linux nodes")
	}
	real, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if out, err := exec.Command("setfacl", "-m", "u:"+me.Uid+":r--", dir).CombinedOutput(); err != nil {
		t.Skip("the test directory's file system does not take access control lists:", string(out))
	}
	a := FileAccessControlList{Path: dir, Entries: []AccessControlEntry{{Group: group.Name, Permissions: "rwx"}}, EntriesNotListed: "remove"}
	shim := t.TempDir()
	path := os.Getenv("PATH")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+path)
	// setfacl refuses any call that has the argument flag.
	setShim := func(flag string) {
		t.Helper()
		writeStandInProgram(t, filepath.Join(shim, "setfacl"), "#!/bin/sh\nfor a; do if [ \"$a\" = "+flag+" ]; then echo refused >&2; exit 1; fi; done\nexec "+real+" \"$@\"\n")
	}
	// The call that sets entries fails first: nothing changed.
	setShim("-m")
	if r := (Runtime{}).FileAccessControlList(context.Background(), a); r.Status != "failed" || r.Value != nil {
		t.Fatalf("%+v", r)
	}
	// The call that sets the group entry works and the call that removes
	// the user's entry fails: the value lists the added entry only.
	setShim("-x")
	r := (Runtime{}).FileAccessControlList(context.Background(), a)
	if r.Status != "failed" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"added": []string{"group " + group.Name + ": rwx"}}) {
		t.Fatalf("%+v", r)
	}
}

// An account that can write the directory above path may swap path for a
// symbolic link after the task checked it. setfacl then must not follow the
// link and change the access control list of the file it points to.
func TestFileAccessControlListDoesNotFollowASwappedInLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("access control lists are read on Linux nodes")
	}
	real, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, outside := filepath.Join(dir, "uploads"), filepath.Join(t.TempDir(), "shadow")
	for _, p := range []string{path, outside} {
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command(real, "-m", "u:"+me.Uid+":r--", outside).CombinedOutput(); err != nil {
		t.Skip("the test directory's file system does not take access control lists:", string(out))
	}
	if err := exec.Command(real, "-b", outside).Run(); err != nil {
		t.Fatal(err)
	}
	// The swap happens between the task's check and setfacl's change.
	shim := t.TempDir()
	quoteShell := func(s string) string { return "'" + s + "'" }
	writeStandInProgram(t, filepath.Join(shim, "setfacl"), "#!/bin/sh\nif [ ! -L "+quoteShell(path)+" ]; then mv "+quoteShell(path)+" "+quoteShell(path+".moved")+" && ln -s "+quoteShell(outside)+" "+quoteShell(path)+"; fi\nexec "+real+" \"$@\"\n")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := (Runtime{}).FileAccessControlList(context.Background(), FileAccessControlList{Path: path, Entries: []AccessControlEntry{{UserAccount: me.Username, Permissions: "rwx"}}, EntriesNotListed: "keep"})
	if entries, err := readACL(outside, aclAccess); err != nil || len(entries) != 0 {
		t.Fatalf("setfacl followed the link: %v %v; task %+v", entries, err, r)
	}
	if r.Status != "failed" {
		t.Fatalf("%+v", r)
	}
}
