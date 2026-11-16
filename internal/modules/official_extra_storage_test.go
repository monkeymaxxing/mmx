package modules

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// Tests of the official extra modules for storage: disk partitions through
// sfdisk, LVM, and ZFS. Each stand-in program prints what the real tool
// printed on a test guest, in a virtual machine with the tool on loop
// devices. Files in the stand-ins' directory say what the node holds, so one
// set of stand-ins serves every test of a module.

// storageStubDirectory is the directory of the stand-in programs that
// stubTools made: the first entry of PATH.
func storageStubDirectory(t *testing.T) string {
	t.Helper()
	return stubToolsDirectory(t)
}

// storageStubFile writes one file into the directory of the stand-in
// programs.
func storageStubFile(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(storageStubDirectory(t), name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// readStorageStubFile reads what a stand-in program kept, such as the
// standard input it got; a missing file is "".
func readStorageStubFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storageStubDirectory(t), name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func removeStorageStubFile(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(storageStubDirectory(t), name)); err != nil {
		t.Fatal(err)
	}
}

// changingStorageCalls are the calls among calls whose program changes the
// node; a call that only reads has one of the reading options.
func changingStorageCalls(calls []string) []string {
	var changing []string
	for _, call := range calls {
		program, arguments, _ := strings.Cut(call, " ")
		switch program {
		case "sfdisk":
			if !strings.Contains(arguments, "--json") && !strings.Contains(arguments, "--no-act") {
				changing = append(changing, call)
			}
		case "vgcreate", "vgextend", "vgreduce", "vgremove", "pvremove", "pvresize", "lvcreate", "lvremove":
			changing = append(changing, call)
		case "lvextend":
			if arguments != "--help" {
				changing = append(changing, call)
			}
		case "zfs":
			if verb, _, _ := strings.Cut(arguments, " "); verb == "create" || verb == "set" || verb == "destroy" {
				changing = append(changing, call)
			}
		}
	}
	return changing
}

func storageResultIs(t *testing.T, r protocol.Result, status, change, reason string) {
	t.Helper()
	if r.Status != status || r.Change != change || r.Reason != reason {
		t.Fatalf("got status %q, change %q, reason %q (failure kind %q, printed %q)\nwant status %q, change %q, reason %q", r.Status, r.Change, r.Reason, r.FailureKind, r.Stderr, status, change, reason)
	}
}

// storageTaskIsRefused: the task cannot succeed on the node as it is, so
// the script returned invalid_inputs and MMX does not try the task again.
func storageTaskIsRefused(t *testing.T, r protocol.Result, change, reason string) {
	t.Helper()
	storageResultIs(t, r, "failed", change, reason)
	if r.FailureKind != "config" {
		t.Fatalf("failure kind %q for %q; a failure that another try cannot fix is invalid_inputs", r.FailureKind, reason)
	}
}

// storageTaskFailed: a command failed for a reason that the module does not
// know, so MMX may try the task again.
func storageTaskFailed(t *testing.T, r protocol.Result, change, reason string) {
	t.Helper()
	storageResultIs(t, r, "failed", change, reason)
	if r.FailureKind != "" || r.NoOtherTry {
		t.Fatalf("failure kind %q, no other try %v for %q; a failed command is tried again", r.FailureKind, r.NoOtherTry, reason)
	}
}

// storageTaskFailedOnce: the task changed the node and then failed. The
// script asked for no other try (try_again: False), because the next try
// would find the changed node and could report success. The result stays an
// ordinary failure and keeps its change.
func storageTaskFailedOnce(t *testing.T, r protocol.Result, reason string) {
	t.Helper()
	storageResultIs(t, r, "failed", "changed", reason)
	if r.FailureKind != "" || !r.NoOtherTry {
		t.Fatalf("failure kind %q, no other try %v for %q; a failure that another try would hide is failed with try_again: False", r.FailureKind, r.NoOtherTry, reason)
	}
}

// storageTaskWaitsForAnEarlierTask: the node is in a state that an earlier
// node task of the same playbook may change, so the script returned
// missing_prerequisite. A dryrun, in which the earlier task has changed
// nothing yet, does not check the task; a run fails once and changes
// nothing.
func storageTaskWaitsForAnEarlierTask(t *testing.T, module string, inputs map[string]any, reason string) {
	t.Helper()
	storageResultIs(t, runOfficialExtraModule(t, module, inputs, true), "not_checked", "unknown", reason)
	run := runOfficialExtraModule(t, module, inputs, false)
	storageResultIs(t, run, "failed", "unchanged", reason)
	if run.FailureKind != "missing_prerequisite" {
		t.Fatalf("failure kind %q for %q; a state that an earlier node task may change is a missing prerequisite", run.FailureKind, reason)
	}
}

// --- create_partitions_on_empty_disk ---

// partitionTools are the stand-ins of create_partitions_on_empty_disk. A
// device is empty unless a file says otherwise: lsblk-disk is what lsblk
// prints for the disk, lsblk-link-<name> for /dev/disk/by-partlabel/<name>,
// wipefs-<last part of the device> what wipefs prints, and table what
// sfdisk --json prints. A successful write copies the files of after-sfdisk
// into place, and sfdisk-refuses is what sfdisk prints when it exits 1.
var partitionTools = map[string]string{
	"lsblk": `for last; do :; done
case " $* " in *" --nodeps "*) ;; *)
  # What is on the disk: the disk itself, and then every device on it.
  if [ -e "$STUBS/lsblk-on-the-disk" ]; then cat "$STUBS/lsblk-on-the-disk"; exit 0; fi
  printf '{\n   "blockdevices": [\n      {\n         "path": "/dev/loop0",\n         "kname": "loop0",\n         "type": "loop",\n         "mountpoint": null\n      }\n   ]\n}\n'
  exit 0;;
esac
case "$last" in /dev/disk/by-partlabel/*) file="$STUBS/lsblk-link-${last##*/}";; *) file="$STUBS/lsblk-disk";; esac
if [ -e "$file" ]; then cat "$file"; exit 0; fi
echo "lsblk: $last: not a block device" >&2
printf '{\n   "blockdevices": [\n\n   ]\n}\n'
exit 32`,
	"wipefs": `for last; do :; done
if [ -e "$STUBS/wipefs-fails" ]; then echo "wipefs: error: $last: probing initialization failed: No such file or directory" >&2; exit 1; fi
if [ -e "$STUBS/wipefs-${last##*/}" ]; then cat "$STUBS/wipefs-${last##*/}"; else printf '{\n   "signatures": [\n\n   ]\n}\n'; fi`,
	"sfdisk": `for last; do :; done
case " $* " in
*" --json "*)
  if [ -e "$STUBS/table" ]; then cat "$STUBS/table"; exit 0; fi
  echo "sfdisk: $last: does not contain a recognized partition table" >&2; exit 1;;
*" --no-act "*)
  cat > "$STUBS/sfdisk-no-act-input"
  if [ -e "$STUBS/sfdisk-refuses" ]; then cat "$STUBS/sfdisk-refuses" >&2; exit 1; fi
  echo "The partition table is unchanged (--no-act).";;
*)
  cat > "$STUBS/sfdisk-input"
  if [ -e "$STUBS/sfdisk-refuses" ]; then echo "Checking that no-one is using this disk right now ... FAILED"; cat "$STUBS/sfdisk-refuses" >&2; exit 1; fi
  if [ -d "$STUBS/after-sfdisk" ]; then cp "$STUBS"/after-sfdisk/* "$STUBS"/; fi
  echo "sfdisk: $last: device already locked, waiting to get lock ... OK" >&2
  echo "The partition table has been altered.";;
esac`,
	"udevadm": ``,
}

// What the tools printed for a loop device of 256 MiB with sectors of 512
// bytes (util-linux 2.37.2 and util-linux 2.42.4 print the same), and for
// one with sectors of 4096 bytes.
const (
	lsblkOfDiskWith512ByteSectors = `{
   "blockdevices": [
      {
         "path": "/dev/loop0",
         "kname": "loop0",
         "pkname": null,
         "type": "loop",
         "size": 268435456,
         "log-sec": 512
      }
   ]
}
`
	lsblkOfDiskWith4096ByteSectors = `{
   "blockdevices": [
      {
         "path": "/dev/loop1",
         "kname": "loop1",
         "pkname": null,
         "type": "loop",
         "size": 268435456,
         "log-sec": 4096
      }
   ]
}
`
	wipefsOfDiskWithGPT = `{
   "signatures": [
      {
         "device": "loop0",
         "offset": "0x200",
         "type": "gpt",
         "uuid": null,
         "label": null
      },{
         "device": "loop0",
         "offset": "0xffffe00",
         "type": "gpt",
         "uuid": null,
         "label": null
      },{
         "device": "loop0",
         "offset": "0x1fe",
         "type": "PMBR",
         "uuid": null,
         "label": null
      }
   ]
}
`
	wipefsOfExt4 = `{
   "signatures": [
      {
         "device": "database",
         "offset": "0x438",
         "type": "ext4",
         "uuid": "756099ad-0777-4ff8-9a69-ea99415cb493",
         "label": null
      }
   ]
}
`
	// sfdisk's words for a partition beyond the end of the disk.
	sfdiskDoesNotFit = "The last usable GPT sector is 131038, but 133119 is requested.\nFailed to add #1 partition: Invalid argument\n"
	// sfdisk's words for a disk that is in use.
	sfdiskDiskInUse = "This disk is currently in use - repartitioning is probably a bad idea.\nUmount all file systems, and swapoff all swap partitions on this disk.\nUse the --no-reread flag to suppress this check.\n\nsfdisk: Use the --force flag to overrule all checks.\n"
)

// lsblkOfPartition is what lsblk prints for a partition, as for
// /dev/disk/by-partlabel/database on the test guests.
func lsblkOfPartition(path, disk string) string {
	return `{
   "blockdevices": [
      {
         "path": "` + path + `",
         "kname": "` + strings.TrimPrefix(path, "/dev/") + `",
         "pkname": "` + disk + `",
         "type": "part",
         "size": 67108864,
         "log-sec": 512
      }
   ]
}
`
}

// sfdiskTable is what sfdisk --json prints for a GPT partition table. A
// partition is "name start size type"; without a name sfdisk prints no
// "name" key, and without partitions no "partitions" key.
func sfdiskTable(sectorBytes, firstLBA, lastLBA int, partitions ...[4]string) string {
	text := `{
   "partitiontable": {
      "label": "gpt",
      "id": "310701AF-FD34-414A-8E25-5E5619EC276E",
      "device": "/dev/loop0",
      "unit": "sectors",
      "firstlba": ` + strconv.Itoa(firstLBA) + `,
      "lastlba": ` + strconv.Itoa(lastLBA) + `,
      "sectorsize": ` + strconv.Itoa(sectorBytes)
	for i, p := range partitions {
		if i == 0 {
			text += `,
      "partitions": [
         {`
		} else {
			text += `,{`
		}
		text += `
            "node": "/dev/loop0p` + strconv.Itoa(i+1) + `",
            "start": ` + p[1] + `,
            "size": ` + p[2] + `,
            "type": "` + p[3] + `",
            "uuid": "FCE5071B-39AF-4943-B910-B41C691A78CA"`
		if p[0] != "" {
			text += `,
            "name": "` + p[0] + `"`
		}
		text += `
         }`
	}
	if len(partitions) > 0 {
		text += `
      ]`
	}
	return text + `
   }
}
`
}

const (
	linuxFilesystemGUID = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	linuxLVMGUID        = "E6D6D379-F507-44C2-A23C-238F2A3DF928"
)

// The task of the design's example on a disk of 256 MiB, and the table that
// the module writes for it: 64 MiB, then the rest up to the last whole MiB.
func partitionInputs(device string) map[string]any {
	return map[string]any{"device": device, "partitions": []any{
		map[string]any{"name": "database", "size": "64MiB"},
		map[string]any{"name": "backups", "size": "rest_of_disk", "type": "linux_lvm"},
	}}
}

var writtenPartitionTable = sfdiskTable(512, 2048, 524254,
	[4]string{"database", "2048", "131072", linuxFilesystemGUID}, [4]string{"backups", "133120", "389120", linuxLVMGUID})

const lsblkColumns = "lsblk --json --bytes --nodeps -o PATH,KNAME,PKNAME,TYPE,SIZE,LOG-SEC "

// The call that lists the disk and every device on it.
const lsblkOnTheDisk = "lsblk --json -o PATH,KNAME,TYPE,MOUNTPOINT "

// newPartitionDisk makes the stand-ins and a file that stands for the disk:
// inspect_path must find the device.
func newPartitionDisk(t *testing.T) (device string, calls func() []string) {
	t.Helper()
	calls = stubTools(t, partitionTools)
	device = filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	storageStubFile(t, "lsblk-disk", lsblkOfDiskWith512ByteSectors)
	return device, calls
}

// afterSfdiskWrites says what the node holds once sfdisk has written the
// table of partitionInputs.
func afterSfdiskWrites(t *testing.T) {
	t.Helper()
	storageStubFile(t, "after-sfdisk/table", writtenPartitionTable)
	storageStubFile(t, "after-sfdisk/wipefs-disk", wipefsOfDiskWithGPT)
	storageStubFile(t, "after-sfdisk/lsblk-link-database", lsblkOfPartition("/dev/loop0p1", "loop0"))
	storageStubFile(t, "after-sfdisk/lsblk-link-backups", lsblkOfPartition("/dev/loop0p2", "loop0"))
}

func TestCreatePartitionsWritesTheTableOnAnEmptyDiskOnce(t *testing.T) {
	t.Parallel()
	device, calls := newPartitionDisk(t)
	afterSfdiskWrites(t)
	script := "label: gpt\n" +
		"start=2048, size=131072, name=database, type=" + linuxFilesystemGUID + "\n" +
		"start=133120, size=389120, name=backups, type=" + linuxLVMGUID + "\n"
	described := "a GPT partition table on " + device + " with the partitions: database (64 MiB), backups (190 MiB, the rest of the disk)"

	// A dryrun asks sfdisk --no-act with the same script and writes nothing.
	dryrun := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would create "+described)
	wantDryrun := []string{
		lsblkColumns + device,
		"wipefs --no-act --json " + device,
		lsblkOnTheDisk + device,
		lsblkColumns + "/dev/disk/by-partlabel/database",
		lsblkColumns + "/dev/disk/by-partlabel/backups",
		"sfdisk --no-act --wipe never --wipe-partitions never --lock " + device,
	}
	if got := calls(); !reflect.DeepEqual(got, wantDryrun) {
		t.Fatalf("dryrun calls:\n%s", strings.Join(got, "\n"))
	}
	if got := readStorageStubFile(t, "sfdisk-no-act-input"); got != script {
		t.Fatalf("sfdisk --no-act got the script %q", got)
	}
	if got := readStorageStubFile(t, "sfdisk-input"); got != "" {
		t.Fatalf("a dryrun wrote: %q", got)
	}

	run := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false)
	storageResultIs(t, run, "succeeded", "changed", "created "+described)
	wantRun := []string{
		lsblkColumns + device,
		"wipefs --no-act --json " + device,
		lsblkOnTheDisk + device,
		lsblkColumns + "/dev/disk/by-partlabel/database",
		lsblkColumns + "/dev/disk/by-partlabel/backups",
		"sfdisk --wipe never --wipe-partitions never --lock " + device,
		"sfdisk --json " + device,
		"udevadm settle",
		lsblkColumns + "/dev/disk/by-partlabel/database",
		lsblkColumns + "/dev/disk/by-partlabel/backups",
		"wipefs --no-act --json /dev/disk/by-partlabel/database",
		"wipefs --no-act --json /dev/disk/by-partlabel/backups",
	}
	if got := calls()[len(wantDryrun):]; !reflect.DeepEqual(got, wantRun) {
		t.Fatalf("run calls:\n%s", strings.Join(got, "\n"))
	}
	if got := readStorageStubFile(t, "sfdisk-input"); got != script {
		t.Fatalf("sfdisk got the script %q", got)
	}
	created := map[string]any{"created": true}
	if run.Fingerprint == "" || run.Fingerprint != dryrun.Fingerprint || !reflect.DeepEqual(run.ModuleOutputs, created) || !reflect.DeepEqual(dryrun.ModuleOutputs, created) {
		t.Fatalf("fingerprint %q after %q, outputs %v after %v", run.Fingerprint, dryrun.Fingerprint, run.ModuleOutputs, dryrun.ModuleOutputs)
	}

	// The second run and its dryrun find exactly these partitions and their
	// links: no changing command, and not even sfdisk --no-act.
	before := len(calls())
	for _, dryrun := range []bool{false, true} {
		again := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), dryrun)
		storageResultIs(t, again, "succeeded", "unchanged", "")
		if again.Fingerprint != run.Fingerprint || !reflect.DeepEqual(again.ModuleOutputs, map[string]any{"created": false}) {
			t.Fatalf("an unchanged task has another fingerprint, or the outputs %v", again.ModuleOutputs)
		}
	}
	unchanged := []string{lsblkColumns + device, "wipefs --no-act --json " + device, "sfdisk --json " + device, "udevadm settle",
		lsblkColumns + "/dev/disk/by-partlabel/database", lsblkColumns + "/dev/disk/by-partlabel/backups"}
	if got := calls()[before:]; !reflect.DeepEqual(got, append(append([]string{}, unchanged...), unchanged...)) {
		t.Fatalf("unchanged calls:\n%s", strings.Join(got, "\n"))
	}
	// Another table is asked for: another fingerprint.
	other := partitionInputs(device)
	other["partitions"] = []any{map[string]any{"name": "database", "size": "64MiB"}}
	if r := runOfficialExtraModule(t, "create_partitions_on_empty_disk", other, true); r.Fingerprint == run.Fingerprint {
		t.Fatal("another list of partitions has the same fingerprint")
	}
}

// Older and newer versions of sfdisk end a last partition without a size at
// different sectors, and a disk can have sectors of 4096 bytes: each of
// these tables is the task's.
func TestCreatePartitionsTakesTheTablesOfEverySfdiskForUnchanged(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ lsblk, table string }{
		"util-linux 2.37 ends the rest at the last usable sector": {lsblkOfDiskWith512ByteSectors, sfdiskTable(512, 2048, 524254,
			[4]string{"database", "2048", "131072", linuxFilesystemGUID}, [4]string{"backups", "133120", "391135", linuxLVMGUID})},
		"util-linux 2.38 and later end it at the last whole MiB": {lsblkOfDiskWith512ByteSectors, writtenPartitionTable},
		"sectors of 4096 bytes": {lsblkOfDiskWith4096ByteSectors, sfdiskTable(4096, 256, 65530,
			[4]string{"database", "256", "16384", linuxFilesystemGUID}, [4]string{"backups", "16640", "48640", linuxLVMGUID})},
		"a type in small letters": {lsblkOfDiskWith512ByteSectors, sfdiskTable(512, 2048, 524254,
			[4]string{"database", "2048", "131072", strings.ToLower(linuxFilesystemGUID)}, [4]string{"backups", "133120", "389120", linuxLVMGUID})},
	} {
		t.Run(name, func(t *testing.T) {
			device, calls := newPartitionDisk(t)
			storageStubFile(t, "lsblk-disk", c.lsblk)
			storageStubFile(t, "wipefs-disk", wipefsOfDiskWithGPT)
			storageStubFile(t, "table", c.table)
			disk := "loop0"
			if c.lsblk == lsblkOfDiskWith4096ByteSectors {
				disk = "loop1"
			}
			storageStubFile(t, "lsblk-link-database", lsblkOfPartition("/dev/"+disk+"p1", disk))
			storageStubFile(t, "lsblk-link-backups", lsblkOfPartition("/dev/"+disk+"p2", disk))
			storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "succeeded", "unchanged", "")
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("an unchanged task ran %q", changing)
			}
		})
	}
}

// The script for a disk with sectors of 4096 bytes counts in those sectors.
func TestCreatePartitionsCountsInTheSectorsOfTheDisk(t *testing.T) {
	t.Parallel()
	device, _ := newPartitionDisk(t)
	storageStubFile(t, "lsblk-disk", lsblkOfDiskWith4096ByteSectors)
	r := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true)
	if r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	want := "label: gpt\nstart=256, size=16384, name=database, type=" + linuxFilesystemGUID + "\nstart=16640, size=48640, name=backups, type=" + linuxLVMGUID + "\n"
	if got := readStorageStubFile(t, "sfdisk-no-act-input"); got != want {
		t.Fatalf("script %q", got)
	}
}

// A GPT partition table keeps 33 sectors of 512 bytes, or 5 of 4096 bytes,
// at the end of the disk, so the last usable sector is the number of sectors
// minus 34, or minus 6. On a disk that ends exactly those sectors behind a
// whole MiB, the rest of the disk goes up to the end of that MiB, and a
// partition with a size that ends there fits. sfdisk --json printed these
// last usable sectors for loop devices of these sizes.
func TestCreatePartitionsUsesTheDiskUpToItsLastUsableSector(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		lsblk, disk     string
		sectorBytes     int
		firstLBA        int
		lastLBA         int
		start, rest     string
		restMiB, fitMiB string
	}{
		"128 MiB and 33 sectors of 512 bytes": {strings.Replace(lsblkOfDiskWith512ByteSectors, "268435456", "134234624", 1), "loop0", 512, 2048, 262143, "2048", "258048", "126", "127"},
		"64 MiB and 5 sectors of 4096 bytes":  {strings.Replace(lsblkOfDiskWith4096ByteSectors, "268435456", "67129344", 1), "loop1", 4096, 256, 16383, "256", "15872", "62", "63"},
	} {
		t.Run(name, func(t *testing.T) {
			device, _ := newPartitionDisk(t)
			storageStubFile(t, "lsblk-disk", c.lsblk)
			sectorsOfOneMiB := 1024 * 1024 / c.sectorBytes
			second := strconv.Itoa(2 * sectorsOfOneMiB)
			inputs := map[string]any{"device": device, "partitions": []any{
				map[string]any{"name": "database", "size": "1MiB"},
				map[string]any{"name": "backups", "size": "rest_of_disk"},
			}}
			afterSfdiskWrites(t)
			storageStubFile(t, "after-sfdisk/table", sfdiskTable(c.sectorBytes, c.firstLBA, c.lastLBA,
				[4]string{"database", c.start, strconv.Itoa(sectorsOfOneMiB), linuxFilesystemGUID}, [4]string{"backups", second, c.rest, linuxFilesystemGUID}))
			storageStubFile(t, "after-sfdisk/lsblk-link-database", lsblkOfPartition("/dev/"+c.disk+"p1", c.disk))
			storageStubFile(t, "after-sfdisk/lsblk-link-backups", lsblkOfPartition("/dev/"+c.disk+"p2", c.disk))
			storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", inputs, false), "succeeded", "changed",
				"created a GPT partition table on "+device+" with the partitions: database (1 MiB), backups ("+c.restMiB+" MiB, the rest of the disk)")
			want := "label: gpt\nstart=" + c.start + ", size=" + strconv.Itoa(sectorsOfOneMiB) + ", name=database, type=" + linuxFilesystemGUID +
				"\nstart=" + second + ", size=" + c.rest + ", name=backups, type=" + linuxFilesystemGUID + "\n"
			if got := readStorageStubFile(t, "sfdisk-input"); got != want {
				t.Fatalf("script %q", got)
			}
			storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", inputs, false), "succeeded", "unchanged", "")

			// One partition that ends at the last usable sector fits; one
			// MiB more does not.
			device, _ = newPartitionDisk(t)
			storageStubFile(t, "lsblk-disk", c.lsblk)
			whole := map[string]any{"device": device, "partitions": []any{map[string]any{"name": "database", "size": c.fitMiB + "MiB"}}}
			storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", whole, true), "succeeded", "would_change",
				"would create a GPT partition table on "+device+" with the partitions: database ("+c.fitMiB+" MiB)")
			fit, _ := strconv.Atoi(c.fitMiB)
			tooLarge := map[string]any{"device": device, "partitions": []any{map[string]any{"name": "database", "size": strconv.Itoa(fit+1) + "MiB"}}}
			storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", tooLarge, true), "unchanged",
				"create_partitions_on_empty_disk: the partitions do not fit on "+device+" ("+strconv.Itoa(fit+1)+" MiB): the partition database ("+strconv.Itoa(fit+1)+" MiB) does not fit: "+
					"the disk has room for "+c.fitMiB+" MiB of partitions, and the partitions up to this one need "+strconv.Itoa(fit+1)+" MiB. Nothing was written")
		})
	}
}

// Anything on the disk fails the task, in a dryrun as in a run, names what
// was found, and runs no sfdisk that writes or would write.
func TestCreatePartitionsNeverWritesOnADiskThatHoldsAnything(t *testing.T) {
	t.Parallel()
	const neverDestroys = ". create_partitions_on_empty_disk writes a partition table only on a disk that holds nothing, and never destroys data. To use this disk, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all "
	const neverChanges = ". The module writes a partition table only on a disk that holds nothing, and never changes an existing partition table"
	const asked = ". The task lists: database (64 MiB, linux_filesystem, from sector 2048), backups (190 MiB, linux_lvm, from sector 133120)"
	for name, c := range map[string]struct {
		lsblk, wipefs, table string
		reason               func(device string) string
	}{
		"an ext4 filesystem": {wipefs: wipefsOfExt4, reason: func(device string) string {
			return device + " is not empty; found: ext4 filesystem (ext4 signature at offset 0x438)" + neverDestroys + device
		}},
		"swap": {wipefs: strings.NewReplacer("ext4", "swap", "0x438", "0xff6").Replace(wipefsOfExt4), reason: func(device string) string {
			return device + " is not empty; found: swap area (swap signature at offset 0xff6)" + neverDestroys + device
		}},
		"an LVM physical volume": {wipefs: strings.NewReplacer("ext4", "LVM2_member", "0x438", "0x218").Replace(wipefsOfExt4), reason: func(device string) string {
			return device + " is not empty; found: LVM physical volume (LVM2_member signature at offset 0x218)" + neverDestroys + device
		}},
		"an MBR partition table": {wipefs: strings.NewReplacer("ext4", "dos", "0x438", "0x1fe").Replace(wipefsOfExt4), reason: func(device string) string {
			return device + " is not empty; found: MBR partition table (dos signature at offset 0x1fe)" + neverDestroys + device
		}},
		"a GPT partition table with another partition name": {wipefs: wipefsOfDiskWithGPT, table: sfdiskTable(512, 2048, 524254,
			[4]string{"database", "2048", "131072", linuxFilesystemGUID}, [4]string{"archive", "133120", "389120", linuxLVMGUID}), reason: func(device string) string {
			return device + " already has a GPT partition table with other partitions than the task lists. Found: database (64 MiB, linux_filesystem, from sector 2048), archive (190 MiB, linux_lvm, from sector 133120)" + asked + neverChanges
		}},
		"a GPT partition table with another size": {wipefs: wipefsOfDiskWithGPT, table: sfdiskTable(512, 2048, 524254,
			[4]string{"database", "2048", "262144", linuxFilesystemGUID}, [4]string{"backups", "264192", "258048", linuxLVMGUID}), reason: func(device string) string {
			return device + " already has a GPT partition table with other partitions than the task lists. Found: database (128 MiB, linux_filesystem, from sector 2048), backups (126 MiB, linux_lvm, from sector 264192)" + asked + neverChanges
		}},
		"a GPT partition table with another type and a partition without a name": {wipefs: wipefsOfDiskWithGPT, table: sfdiskTable(512, 2048, 524254,
			[4]string{"", "2048", "131072", "A19D880F-05FC-4D3B-A006-743F0F84911E"}, [4]string{"backups", "133120", "389120", "DEADBEEF-0000-4000-8000-000000000000"}), reason: func(device string) string {
			return device + " already has a GPT partition table with other partitions than the task lists. Found: a partition without a name (64 MiB, linux_raid, from sector 2048), backups (190 MiB, type DEADBEEF-0000-4000-8000-000000000000, from sector 133120)" + asked + neverChanges
		}},
		"a GPT partition table whose last partition ends more than 1 MiB before the end": {wipefs: wipefsOfDiskWithGPT, table: sfdiskTable(512, 2048, 524254,
			[4]string{"database", "2048", "131072", linuxFilesystemGUID}, [4]string{"backups", "133120", "387072", linuxLVMGUID}), reason: func(device string) string {
			return device + " already has a GPT partition table with other partitions than the task lists. Found: database (64 MiB, linux_filesystem, from sector 2048), backups (189 MiB, linux_lvm, from sector 133120)" + asked + neverChanges
		}},
		"a GPT partition table without partitions": {wipefs: wipefsOfDiskWithGPT, table: sfdiskTable(512, 2048, 524254), reason: func(device string) string {
			return device + " already has a GPT partition table with other partitions than the task lists. Found: no partition" + asked + neverChanges
		}},
		"a partition": {lsblk: lsblkOfPartition("/dev/sdb1", "sdb"), reason: func(device string) string {
			return device + " is a partition of the disk /dev/sdb, not a whole disk; the module writes a partition table only on a whole disk"
		}},
		"a read-only medium": {lsblk: strings.NewReplacer(`"loop"`, `"rom"`, "512", "2048").Replace(lsblkOfDiskWith512ByteSectors), reason: func(device string) string {
			return device + " is a read-only medium, such as a CD drive; the module writes a partition table only on a disk"
		}},
		"sectors of another size": {lsblk: strings.Replace(lsblkOfDiskWith512ByteSectors, "512", "2048", 1), reason: func(device string) string {
			return device + " has sectors of 2048 bytes; the module knows disks with sectors of 512 and of 4096 bytes"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			device, calls := newPartitionDisk(t)
			if c.lsblk != "" {
				storageStubFile(t, "lsblk-disk", c.lsblk)
			}
			if c.wipefs != "" {
				storageStubFile(t, "wipefs-disk", c.wipefs)
			}
			if c.table != "" {
				storageStubFile(t, "table", c.table)
			}
			for _, dryrun := range []bool{true, false} {
				r := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), dryrun)
				storageTaskIsRefused(t, r, "unchanged", "create_partitions_on_empty_disk: "+c.reason(device))
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "sfdisk ") && !strings.HasPrefix(call, "sfdisk --json ") {
					t.Fatalf("the task ran %q", call)
				}
			}
		})
	}
}

func TestCreatePartitionsFailsBeforeWritingWhenThePartitionsDoNotFitOrANameIsTaken(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		partitions []any
		link       string
		reason     func(device string) string
	}{
		"a size beyond the disk": {partitions: []any{map[string]any{"name": "database", "size": "64MiB"}, map[string]any{"name": "backups", "size": "1GiB"}}, reason: func(device string) string {
			return "the partitions do not fit on " + device + " (256 MiB): the partition backups (1 GiB) does not fit: the disk has room for 254 MiB of partitions, and the partitions up to this one need 1088 MiB. Nothing was written"
		}},
		"no MiB left for the rest of the disk": {partitions: []any{map[string]any{"name": "database", "size": "254MiB"}, map[string]any{"name": "backups", "size": "rest_of_disk"}}, reason: func(device string) string {
			return "the partitions do not fit on " + device + " (256 MiB): the partition backups with the size rest_of_disk needs at least 1 MiB, and the partitions before it leave less: the disk has room for 254 MiB of partitions. Nothing was written"
		}},
		"a name that another disk's partition has": {link: lsblkOfPartition("/dev/sda3", "sda"), reason: func(device string) string {
			return "the name database is already the name of the partition /dev/sda3 on the disk /dev/sda: /dev/disk/by-partlabel/database leads there, and two partitions with one name would share that link. Give the new partition another name. Nothing was written"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			device, calls := newPartitionDisk(t)
			inputs := partitionInputs(device)
			if c.partitions != nil {
				inputs["partitions"] = c.partitions
			}
			if c.link != "" {
				storageStubFile(t, "lsblk-link-database", c.link)
			}
			for _, dryrun := range []bool{true, false} {
				storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", inputs, dryrun), "unchanged", "create_partitions_on_empty_disk: "+c.reason(device))
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "sfdisk ") {
					t.Fatalf("the task ran %q", call)
				}
			}
		})
	}
}

// What sfdisk itself refuses fails a dryrun through sfdisk --no-act and a
// run through sfdisk, with sfdisk's words; a disk in use, which only a real
// write finds, gets the module's own words in place of sfdisk's advice to
// force. Each is a command that failed: MMX may try again.
func TestCreatePartitionsReportsWhatSfdiskRefuses(t *testing.T) {
	t.Parallel()
	device, _ := newPartitionDisk(t)
	storageStubFile(t, "sfdisk-refuses", sfdiskDoesNotFit)
	const words = "The last usable GPT sector is 131038, but 133119 is requested. Failed to add #1 partition: Invalid argument"
	storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true), "unchanged",
		"create_partitions_on_empty_disk: sfdisk would not write the partition table on "+device+": "+words)
	// MMX records every changing command that ran, so the failed write
	// reports changed.
	storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "changed",
		"create_partitions_on_empty_disk: sfdisk did not write the partition table on "+device+": "+words)
	storageStubFile(t, "sfdisk-refuses", sfdiskDiskInUse)
	storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "changed",
		"create_partitions_on_empty_disk: "+device+" is in use, and sfdisk wrote nothing: the kernel has a filesystem of the disk mounted, uses it as swap, or another device holds it")
}

// A disk can be in use although wipefs finds nothing on it: device-mapper
// holds it, as for an encrypted volume without a header. The real sfdisk
// refuses such a disk, and sfdisk --no-act does not, so the module asks
// lsblk what is on the disk and fails a dryrun as a run. These are the
// lists that lsblk printed for a loop device with a device-mapper device on
// it, on util-linux 2.37.2 and 2.42.4 alike.
func TestCreatePartitionsNeverWritesOnADiskInUse(t *testing.T) {
	t.Parallel()
	entry := func(path, kernelName, kind, mountPoint string) string {
		return `      {
         "path": "` + path + `",
         "kname": "` + kernelName + `",
         "type": "` + kind + `",
         "mountpoint": ` + mountPoint + `
      }`
	}
	list := func(entries ...string) string {
		return "{\n   \"blockdevices\": [\n" + strings.Join(entries, ",") + "\n   ]\n}\n"
	}
	for name, c := range map[string]struct{ lsblk, found string }{
		"a device-mapper device on the disk": {list(entry("/dev/loop0", "loop0", "loop", "null"), entry("/dev/mapper/mmx-holder", "dm-2", "dm", "null")),
			"the device /dev/mapper/mmx-holder (dm) is on it"},
		"partitions that the kernel still has": {list(entry("/dev/loop0", "loop0", "loop", "null"), entry("/dev/loop0p1", "loop0p1", "part", "null"), entry("/dev/loop0p2", "loop0p2", "part", `"/srv/old"`)),
			"the kernel still has the partition /dev/loop0p1 of an earlier partition table; the kernel still has the partition /dev/loop0p2 of an earlier partition table"},
		"a mount point of the disk itself": {list(entry("/dev/loop0", "loop0", "loop", `"/mnt"`)),
			"it is mounted at /mnt"},
	} {
		t.Run(name, func(t *testing.T) {
			device, calls := newPartitionDisk(t)
			storageStubFile(t, "lsblk-on-the-disk", c.lsblk)
			for _, dryrun := range []bool{true, false} {
				storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), dryrun), "unchanged",
					"create_partitions_on_empty_disk: "+device+" is in use, although it holds no signature: "+c.found+". The module writes a partition table only on a disk that nothing uses; "+
						"when nothing uses a partition of an earlier partition table any more, blockdev --rereadpt "+device+" makes the kernel forget it. Nothing was written")
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "sfdisk ") {
					t.Fatalf("the module ran %q on a disk in use", call)
				}
			}
		})
	}
	t.Run("lsblk prints a list that MMX cannot read", func(t *testing.T) {
		device, _ := newPartitionDisk(t)
		storageStubFile(t, "lsblk-on-the-disk", "{\n   \"blockdevices\": [\n   ]\n}\n")
		storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true), "unchanged",
			"create_partitions_on_empty_disk: lsblk printed what MMX cannot read for what is on "+device+": { \"blockdevices\": [ ] }")
	})
}

// After the write: a link that does not appear, a link that leads to another
// disk, a table that is not the task's, and an old filesystem inside a new
// partition. The task wrote the table, so each of these failures asks for no
// other try: the next try would find the table. The next run finds it too,
// and checks the links again: a disk that it did not write itself and whose
// links are missing is a node that no try changes.
func TestCreatePartitionsChecksTheNodeAfterTheWrite(t *testing.T) {
	t.Parallel()
	t.Run("a link does not appear", func(t *testing.T) {
		device, calls := newPartitionDisk(t)
		afterSfdiskWrites(t)
		removeStorageStubFile(t, "after-sfdisk/lsblk-link-backups")
		const missing = ", but /dev/disk/by-partlabel/backups does not exist, so a later node task cannot find the partition. " +
			"The kernel has not read the partition table: attach a loop device with losetup --partscan, and for another disk run partx --add "
		storageTaskFailedOnce(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false),
			"create_partitions_on_empty_disk: the partition table was written on "+device+missing+device)
		for _, dryrun := range []bool{false, true} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), dryrun), "unchanged",
				"create_partitions_on_empty_disk: "+device+" has the partition table that the task lists"+missing+device)
		}
		if changing := changingStorageCalls(calls()); len(changing) != 1 {
			t.Fatalf("the tries after the write ran %q", changing)
		}
		// Once the kernel has read the table, the next run changes nothing.
		storageStubFile(t, "lsblk-link-backups", lsblkOfPartition("/dev/loop0p2", "loop0"))
		storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "succeeded", "unchanged", "")
	})
	t.Run("a link leads to another disk", func(t *testing.T) {
		device, _ := newPartitionDisk(t)
		afterSfdiskWrites(t)
		storageStubFile(t, "after-sfdisk/lsblk-link-database", lsblkOfPartition("/dev/sdc1", "sdc"))
		const other = ", but /dev/disk/by-partlabel/database leads to /dev/sdc1 on the disk /dev/sdc: two partitions of this node have the name database, " +
			"and a later node task would work on the wrong one. Give one of the two another name by hand"
		storageTaskFailedOnce(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false),
			"create_partitions_on_empty_disk: the partition table was written on "+device+other)
		storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true), "unchanged",
			"create_partitions_on_empty_disk: "+device+" has the partition table that the task lists"+other)
	})
	t.Run("sfdisk wrote another table", func(t *testing.T) {
		device, _ := newPartitionDisk(t)
		afterSfdiskWrites(t)
		storageStubFile(t, "after-sfdisk/table", sfdiskTable(512, 2048, 524254, [4]string{"database", "2048", "131072", linuxFilesystemGUID}))
		storageTaskFailedOnce(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false),
			"create_partitions_on_empty_disk: sfdisk reported success, but the partition table of "+device+" is not the one the task lists: database (64 MiB, linux_filesystem, from sector 2048)")
	})
	// A read that fails after the write: the next try would find the table
	// and report a disk that needs nothing, without the note about an old
	// filesystem. So the task asks for no other try, and says that the
	// table is written.
	for name, c := range map[string]struct {
		break_ func(t *testing.T)
		reason func(device string) string
	}{
		"sfdisk cannot read the table that it wrote": {
			func(t *testing.T) { removeStorageStubFile(t, "after-sfdisk/table") },
			func(device string) string {
				return "sfdisk --json could not read the partition table of " + device + ": sfdisk: " + device + ": does not contain a recognized partition table"
			}},
		"lsblk prints no JSON for a link": {
			func(t *testing.T) { storageStubFile(t, "after-sfdisk/lsblk-link-backups", "NAME\nloop0p2\n") },
			func(device string) string {
				return "lsblk printed what MMX cannot read for /dev/disk/by-partlabel/backups: NAME loop0p2"
			}},
		"wipefs cannot probe a new partition": {
			func(t *testing.T) { storageStubFile(t, "after-sfdisk/wipefs-fails", "") },
			func(device string) string {
				return "wipefs --no-act could not probe /dev/disk/by-partlabel/database: wipefs: error: /dev/disk/by-partlabel/database: probing initialization failed: No such file or directory"
			}},
	} {
		t.Run("a read fails after the write: "+name, func(t *testing.T) {
			device, _ := newPartitionDisk(t)
			afterSfdiskWrites(t)
			c.break_(t)
			r := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false)
			storageTaskFailedOnce(t, r, "create_partitions_on_empty_disk: "+c.reason(device)+". This was after the partition table was written on "+device+"; the next run checks the disk again")
			if !reflect.DeepEqual(r.ModuleOutputs, map[string]any{"created": true}) {
				t.Fatalf("outputs %v", r.ModuleOutputs)
			}
		})
	}
	t.Run("an old filesystem inside a new partition", func(t *testing.T) {
		device, calls := newPartitionDisk(t)
		afterSfdiskWrites(t)
		storageStubFile(t, "after-sfdisk/wipefs-database", wipefsOfExt4)
		storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "succeeded", "changed",
			"created a GPT partition table on "+device+" with the partitions: database (64 MiB), backups (190 MiB, the rest of the disk); "+
				"the new partition database holds what was on the disk before: ext4 filesystem (ext4 signature at offset 0x438); the module wiped nothing")
		for _, call := range calls() {
			if strings.HasPrefix(call, "wipefs ") && !strings.HasPrefix(call, "wipefs --no-act ") {
				t.Fatalf("the module ran %q", call)
			}
		}
	})
}

func TestCreatePartitionsReadsThatFail(t *testing.T) {
	t.Parallel()
	t.Run("wipefs fails", func(t *testing.T) {
		device, calls := newPartitionDisk(t)
		storageStubFile(t, "wipefs-fails", "")
		storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), false), "unchanged",
			"create_partitions_on_empty_disk: wipefs --no-act could not probe "+device+": wipefs: error: "+device+": probing initialization failed: No such file or directory")
		if changing := changingStorageCalls(calls()); changing != nil {
			t.Fatalf("ran %q", changing)
		}
	})
	t.Run("the device is a file", func(t *testing.T) {
		device, _ := newPartitionDisk(t)
		removeStorageStubFile(t, "lsblk-disk")
		storageTaskIsRefused(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true), "unchanged",
			"create_partitions_on_empty_disk: "+device+" is not a block device; give the whole disk, such as /dev/sdb")
	})
	t.Run("lsblk prints no JSON", func(t *testing.T) {
		device, _ := newPartitionDisk(t)
		storageStubFile(t, "lsblk-disk", "NAME\nloop0\n")
		storageTaskFailed(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs(device), true), "unchanged",
			"create_partitions_on_empty_disk: lsblk printed what MMX cannot read for "+device+": NAME loop0")
	})
	t.Run("the device does not exist yet", func(t *testing.T) {
		_, calls := newPartitionDisk(t)
		const reason = "create_partitions_on_empty_disk: the device /dev/mmx-test-no-such-disk does not exist yet"
		storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs("/dev/mmx-test-no-such-disk"), true), "not_checked", "unknown", reason)
		run := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs("/dev/mmx-test-no-such-disk"), false)
		storageResultIs(t, run, "failed", "unchanged", reason)
		if run.FailureKind != "missing_prerequisite" || calls() != nil {
			t.Fatalf("failure kind %q, calls %q", run.FailureKind, calls())
		}
	})
}

func TestCreatePartitionsNamesAMissingProgramAndItsPackage(t *testing.T) {
	t.Parallel()
	for program, install := range map[string]string{
		"sfdisk":  "fdisk (Debian and Ubuntu) or util-linux (the Red Hat family)",
		"wipefs":  "util-linux",
		"lsblk":   "util-linux",
		"udevadm": "udev (Debian and Ubuntu) or systemd-udev (the Red Hat family)",
	} {
		if _, err := os.Stat("/usr/bin/" + program); err == nil {
			// The stand-ins' PATH also holds /usr/bin, where a Linux machine has the real program.
			continue
		}
		tools := map[string]string{}
		for name, body := range partitionTools {
			if name != program {
				tools[name] = body
			}
		}
		calls := stubTools(t, tools)
		reason := "create_partitions_on_empty_disk: the program " + program + " is not installed; install the package " + install + " in an earlier node task"
		storageResultIs(t, runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs("/dev/sdb"), true), "not_checked", "unknown", reason)
		run := runOfficialExtraModule(t, "create_partitions_on_empty_disk", partitionInputs("/dev/sdb"), false)
		storageResultIs(t, run, "failed", "unchanged", reason)
		if run.FailureKind != "missing_prerequisite" || calls() != nil {
			t.Fatalf("%s: failure kind %q, calls %q", program, run.FailureKind, calls())
		}
	}
}

func TestCreatePartitionsRejectsInputs(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, partitionTools)
	partition := func(name, size any) map[string]any { return map[string]any{"name": name, "size": size} }
	const nameRule = "expected the partition's name: 1 to 36 letters, digits, -, _, and ., and not . or .."
	const sizeRule = "expected a whole number and MiB, GiB, or TiB, such as 200GiB, or rest_of_disk for the last partition"
	for name, c := range map[string]struct {
		device     string
		partitions any
		reason     string
	}{
		"a relative device":          {"sdb", []any{partition("a", "1GiB")}, "device: expected the absolute path of the whole disk, such as /dev/sdb"},
		"an empty device":            {"", []any{partition("a", "1GiB")}, "device: must not be empty"},
		"no list":                    {"/dev/sdb", map[string]any{"name": "a"}, "partitions: expected a list of at least one partition, each with a name and a size"},
		"an empty list":              {"/dev/sdb", []any{}, "partitions: expected a list of at least one partition, each with a name and a size"},
		"a partition that is a text": {"/dev/sdb", []any{"database"}, "partitions[0]: expected a partition with name, size, and optionally type, such as {name: database, size: 200GiB}"},
		"an unknown key":             {"/dev/sdb", []any{map[string]any{"name": "a", "size": "1GiB", "label": "x"}}, "partitions[0]: unknown key label; a partition has name, size, and type"},
		"no name":                    {"/dev/sdb", []any{map[string]any{"size": "1GiB"}}, "partitions[0].name: " + nameRule},
		"an empty name":              {"/dev/sdb", []any{partition("", "1GiB")}, "partitions[0].name: must not be empty"},
		"the name .":                 {"/dev/sdb", []any{partition(".", "1GiB")}, "partitions[0].name: " + nameRule},
		"the name ..":                {"/dev/sdb", []any{partition("a", "1GiB"), partition("..", "1GiB")}, "partitions[1].name: " + nameRule},
		"a name with a space":        {"/dev/sdb", []any{partition("data base", "1GiB")}, "partitions[0].name: " + nameRule},
		"a name with a comma":        {"/dev/sdb", []any{partition("a,b", "1GiB")}, "partitions[0].name: " + nameRule},
		"a name outside ASCII":       {"/dev/sdb", []any{partition("däta", "1GiB")}, "partitions[0].name: " + nameRule},
		"a name of 37 characters":    {"/dev/sdb", []any{partition(strings.Repeat("a", 37), "1GiB")}, "partitions[0].name: " + nameRule},
		"a name that is a number":    {"/dev/sdb", []any{partition(1, "1GiB")}, "partitions[0].name: " + nameRule},
		"two equal names":            {"/dev/sdb", []any{partition("data", "1GiB"), partition("logs", "1GiB"), partition("data", "1GiB")}, "partitions[2].name: the name data is also the name of partitions[0]; each partition needs its own name, because later node tasks find it as /dev/disk/by-partlabel/data"},
		"no size":                    {"/dev/sdb", []any{map[string]any{"name": "a"}}, "partitions[0].size: " + sizeRule},
		"a size without a unit":      {"/dev/sdb", []any{partition("a", "200")}, "partitions[0].size: " + sizeRule},
		"a size that is a number":    {"/dev/sdb", []any{partition("a", 200)}, "partitions[0].size: " + sizeRule},
		"a size with a decimal unit": {"/dev/sdb", []any{partition("a", "200GB")}, "partitions[0].size: " + sizeRule},
		"a size with a short unit":   {"/dev/sdb", []any{partition("a", "200G")}, "partitions[0].size: " + sizeRule},
		"a size with a fraction":     {"/dev/sdb", []any{partition("a", "1.5GiB")}, "partitions[0].size: " + sizeRule},
		"a size of zero":             {"/dev/sdb", []any{partition("a", "0GiB")}, "partitions[0].size: " + sizeRule},
		"a size in KiB":              {"/dev/sdb", []any{partition("a", "2048KiB")}, "partitions[0].size: " + sizeRule},
		"the rest before the last":   {"/dev/sdb", []any{partition("a", "rest_of_disk"), partition("b", "1GiB")}, "partitions[0].size: only the last partition can have the size rest_of_disk"},
		"an unknown type":            {"/dev/sdb", []any{map[string]any{"name": "a", "size": "1GiB", "type": "8e00"}}, "partitions[0].type: expected one of linux_filesystem, linux_lvm, linux_swap, linux_raid, efi_system"},
		"more partitions than a GPT": {"/dev/sdb", manyPartitions(129), "partitions: a GPT partition table holds at most 128 partitions, and the task lists 129"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "create_partitions_on_empty_disk", map[string]any{"device": c.device, "partitions": c.partitions}, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "create_partitions_on_empty_disk."+c.reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("rejected inputs ran %q", got)
	}
}

func manyPartitions(count int) []any {
	var partitions []any
	for i := 0; i < count; i++ {
		partitions = append(partitions, map[string]any{"name": "p" + strconv.Itoa(i), "size": "1MiB"})
	}
	return partitions
}

// --- lvm_volume_group and lvm_logical_volume ---

// lvmReadingStub and lvmChangingStub are the bodies of the stand-ins for
// LVM's programs. The files report-vgs, report-pvs, and report-lvs hold what
// each report prints.
// A changing program that succeeds copies the files of after-<program> into
// place; <program>-refuses holds what a refusing program prints, with LVM's
// exit code 5, and <program>-fails-after-its-change what a program prints
// that changed the node and then failed.
func lvmReadingStub(program string) string {
	return `if [ -e "$STUBS/` + program + `-fails" ]; then cat "$STUBS/` + program + `-fails-output" 2>/dev/null; cat "$STUBS/` + program + `-fails" >&2; exit 5; fi
cat "$STUBS/report-` + program + `"`
}

func lvmChangingStub(program string) string {
	return `if [ -e "$STUBS/` + program + `-refuses" ]; then cat "$STUBS/` + program + `-refuses" >&2; exit 5; fi
if [ -d "$STUBS/after-` + program + `" ]; then cp "$STUBS"/after-` + program + `/* "$STUBS"/; fi
if [ -e "$STUBS/` + program + `-fails-after-its-change" ]; then cat "$STUBS/` + program + `-fails-after-its-change" >&2; exit 5; fi`
}

var lvmTools = map[string]string{
	"vgs": lvmReadingStub("vgs"), "pvs": lvmReadingStub("pvs"), "lvs": lvmReadingStub("lvs"),
	"vgcreate": lvmChangingStub("vgcreate"), "vgextend": lvmChangingStub("vgextend"), "vgreduce": lvmChangingStub("vgreduce"),
	"vgremove": lvmChangingStub("vgremove"), "pvremove": lvmChangingStub("pvremove"),
	"pvresize": lvmChangingStub("pvresize") + `
for last; do :; done
printf '  Physical volume "%s" changed\n' "$last"
echo '1 physical volume(s) resized or updated / 0 physical volume(s) not resized'`,
	"lvcreate": lvmChangingStub("lvcreate"), "lvremove": lvmChangingStub("lvremove"),
	"lvextend": `if [ "$1" = "--help" ]; then cat "$STUBS/lvextend-help"; exit 0; fi
` + lvmChangingStub("lvextend"),
	"lsblk": `for last; do :; done
if [ -e "$STUBS/lsblk-${last##*/}" ]; then cat "$STUBS/lsblk-${last##*/}"; exit 0; fi
echo "lsblk: $last: not a block device" >&2
printf '{\n   "blockdevices": [\n\n   ]\n}\n'
exit 32`,
	"wipefs": partitionTools["wipefs"],
}

// lvmReport is what vgs, pvs, or lvs prints with --reportformat json. LVM
// 2.03.11 and 2.03.16 print the report alone; LVM 2.03.31 and newer add a
// log.
func lvmReport(kind string, newLVM bool, rows ...string) string {
	text := "  {\n      \"report\": [\n          {\n              \"" + kind + "\": [\n"
	for i, row := range rows {
		text += "                  " + row
		if i < len(rows)-1 {
			text += ","
		}
		text += "\n"
	}
	text += "              ]\n          }\n      ]\n"
	if newLVM {
		text += "      ,\n      \"log\": [\n      ]\n"
	}
	return text + "  }\n"
}

func lsblkNumbers(path, numbers string) string {
	return "{\n   \"blockdevices\": [\n      {\n         \"path\": \"" + path + "\",\n         \"maj:min\": \"" + numbers + "\"\n      }\n   ]\n}\n"
}

func pvsRow(device, group, major, minor, used string) string {
	return `{"pv_name":"` + device + `", "vg_name":"` + group + `", "pv_major":"` + major + `", "pv_minor":"` + minor + `", "pv_used":"` + used + `", "pv_size":"205520896", "dev_size":"209715200", "pe_start":"1048576", "pv_pe_count":"49", "vg_extent_size":"4194304", "pv_mda_count":"1", "pv_mda_size":"1044480"}`
}

// lvsRow is LVM's row for an ordinary logical volume. open is "open" for a
// volume in use and "" for one that is not.
func lvsRow(name, group, size, open string) string {
	return lvsRowOfKind(name, group, size, open, "", "", "public")
}

// lvsRowOfKind is LVM's row for a logical volume with the columns that say
// which volumes depend on which, as LVM 2.03.11 and 2.03.43 printed them: a
// snapshot names its origin and has the role thicksnapshot; a volume in a
// thin pool names its pool, and a snapshot of such a volume has the role
// thinsnapshot.
func lvsRowOfKind(name, group, size, open, origin, pool, role string) string {
	return `{"lv_name":"` + name + `", "vg_name":"` + group + `", "lv_size":"` + size + `", "lv_path":"/dev/` + group + `/` + name + `", "lv_device_open":"` + open +
		`", "origin":"` + origin + `", "pool_lv":"` + pool + `", "lv_role":"` + role + `"}`
}

const (
	lvsColumnsForVolumeGroup = "lvs --reportformat json --units b --nosuffix -o lv_name,vg_name"
	vgsColumnsForVolumeGroup = "vgs --reportformat json --units b --nosuffix -o vg_name,lv_count"
	pvsColumns               = "pvs --reportformat json --units b --nosuffix -o pv_name,vg_name,pv_major,pv_minor,pv_used,pv_size,dev_size,pe_start,pv_pe_count,vg_extent_size,pv_mda_count,pv_mda_size"
	lsblkNumbersColumns      = "lsblk --json --nodeps -o PATH,MAJ:MIN "
	vgsColumnsForVolume      = "vgs --reportformat json --units b --nosuffix -o vg_name,vg_extent_size,vg_free"
	lvsColumnsForVolume      = "lvs --reportformat json --units b --nosuffix -o lv_name,vg_name,lv_size,lv_path,lv_device_open,origin,pool_lv,lv_role"
	// What LVM printed when it refused a device with a filesystem: it asks,
	// answers no itself without a terminal, and changes nothing.
	lvmRefusesSignature = "WARNING: ext4 signature detected on /dev/loop4 at offset 1080. Wipe it? [y/n]: [n]\n  Aborted wiping of ext4.\n  1 existing signature left on the device.\n"
)

// newLVMNode makes the stand-ins and the files that stand for devices; it
// returns each device's path by its name.
func newLVMNode(t *testing.T, newLVM bool, devices ...string) (paths map[string]string, calls func() []string) {
	t.Helper()
	calls = stubTools(t, lvmTools)
	directory := t.TempDir()
	paths = map[string]string{}
	for _, name := range devices {
		paths[name] = filepath.Join(directory, name)
		if err := os.WriteFile(paths[name], nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	storageStubFile(t, "report-vgs", lvmReport("vg", newLVM))
	storageStubFile(t, "report-pvs", lvmReport("pv", newLVM))
	storageStubFile(t, "report-lvs", lvmReport("lv", newLVM))
	return paths, calls
}

func TestLVMVolumeGroupIsCreatedOnEmptyDevicesOnce(t *testing.T) {
	t.Parallel()
	for name, newLVM := range map[string]bool{"LVM 2.03.11": false, "LVM 2.03.31": true} {
		t.Run(name, func(t *testing.T) {
			devices, calls := newLVMNode(t, newLVM, "loop0", "lvm")
			storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
			// A partition's link: LVM names the device by the kernel's path.
			storageStubFile(t, "lsblk-lvm", lsblkNumbers("/dev/loop2p1", "259:0"))
			storageStubFile(t, "after-vgcreate/report-vgs", lvmReport("vg", newLVM, `{"vg_name":"data", "lv_count":"0"}`))
			storageStubFile(t, "after-vgcreate/report-pvs", lvmReport("pv", newLVM, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop2p1", "data", "259", "0", "0")))
			inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"], devices["lvm"]}, "physical_volumes_not_listed": "remove"}
			on := "the volume group data on " + devices["loop0"] + ", " + devices["lvm"]
			reads := []string{vgsColumnsForVolumeGroup, pvsColumns,
				lsblkNumbersColumns + devices["loop0"], "wipefs --no-act --json " + devices["loop0"],
				lsblkNumbersColumns + devices["lvm"], "wipefs --no-act --json " + devices["lvm"]}

			dryrun := runOfficialExtraModule(t, "lvm_volume_group", inputs, true)
			storageResultIs(t, dryrun, "succeeded", "would_change", "would create "+on)
			if got := calls(); !reflect.DeepEqual(got, reads) {
				t.Fatalf("dryrun calls:\n%s", strings.Join(got, "\n"))
			}
			if !strings.Contains(dryrun.Diff, "would run: vgcreate data "+devices["loop0"]+" "+devices["lvm"]) {
				t.Fatalf("dryrun difference %q", dryrun.Diff)
			}
			run := runOfficialExtraModule(t, "lvm_volume_group", inputs, false)
			storageResultIs(t, run, "succeeded", "changed", "created "+on)
			if got := calls()[len(reads):]; !reflect.DeepEqual(got, append(append([]string{}, reads...), "vgcreate data "+devices["loop0"]+" "+devices["lvm"])) {
				t.Fatalf("run calls:\n%s", strings.Join(got, "\n"))
			}
			created := map[string]any{"created": true}
			if run.Fingerprint == "" || run.Fingerprint != dryrun.Fingerprint || !reflect.DeepEqual(run.ModuleOutputs, created) || !reflect.DeepEqual(dryrun.ModuleOutputs, created) {
				t.Fatalf("fingerprint %q after %q, outputs %v after %v", run.Fingerprint, dryrun.Fingerprint, run.ModuleOutputs, dryrun.ModuleOutputs)
			}
			before := len(calls())
			for _, dryrun := range []bool{false, true} {
				again := runOfficialExtraModule(t, "lvm_volume_group", inputs, dryrun)
				storageResultIs(t, again, "succeeded", "unchanged", "")
				if !reflect.DeepEqual(again.ModuleOutputs, map[string]any{"created": false}) {
					t.Fatalf("unchanged outputs %v", again.ModuleOutputs)
				}
			}
			// A device that the volume group has is not probed.
			unchanged := []string{vgsColumnsForVolumeGroup, pvsColumns, lsblkNumbersColumns + devices["loop0"], lsblkNumbersColumns + devices["lvm"]}
			if got := calls()[before:]; !reflect.DeepEqual(got, append(append([]string{}, unchanged...), unchanged...)) {
				t.Fatalf("unchanged calls:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

// The byte values measured on Debian 13: LVM's usable size is the number
// of extents times their size, rather than the device's byte size.
func TestLVMVolumeGroupGrowsPhysicalVolumesWithTheirDevices(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, deviceBytes, afterBytes, afterCount             string
		disable, dryrun, refuses, readFails, failsAfterChange bool
		twoMetadataAreas                                      bool
		change, reason                                        string
	}{
		{name: "grows", deviceBytes: "524288000", afterBytes: "520093696", afterCount: "124", change: "changed", reason: "grew"},
		{name: "less than one extent left", deviceBytes: "210763775", change: "unchanged"},
		{name: "exactly one more extent", deviceBytes: "210763776", afterBytes: "209715200", afterCount: "50", change: "changed", reason: "grew"},
		{name: "smaller device never shrinks", deviceBytes: "104857600", change: "unchanged"},
		// A second metadata area lies at the end of the device: the bytes
		// that give one more extent above give none here (measured).
		{name: "second metadata area takes the room of the last extent", deviceBytes: "210763776", twoMetadataAreas: true, change: "unchanged"},
		{name: "failed command grew the volume", deviceBytes: "524288000", afterBytes: "520093696", afterCount: "124", failsAfterChange: true, change: "changed"},
		{name: "disabled", deviceBytes: "524288000", disable: true, change: "unchanged"},
		{name: "dryrun", deviceBytes: "524288000", dryrun: true, change: "would_change", reason: "would grow"},
		{name: "command refuses", deviceBytes: "524288000", refuses: true, change: "changed"},
		{name: "success gains nothing", deviceBytes: "524288000", change: "changed"},
		{name: "read fails after growth", deviceBytes: "524288000", afterBytes: "520093696", afterCount: "124", readFails: true, change: "changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			devices, calls := newLVMNode(t, true, "loop0")
			device := devices["loop0"]
			storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
			storageStubFile(t, "report-vgs", lvmReport("vg", true, `{"vg_name":"data", "lv_count":"0"}`))
			before := strings.ReplaceAll(pvsRow("/dev/loop0", "data", "7", "0", "0"), `"dev_size":"209715200"`, `"dev_size":"`+test.deviceBytes+`"`)
			if test.twoMetadataAreas {
				before = strings.ReplaceAll(before, `"pv_mda_count":"1"`, `"pv_mda_count":"2"`)
			}
			storageStubFile(t, "report-pvs", lvmReport("pv", true, before))
			if test.afterCount != "" {
				after := strings.ReplaceAll(strings.ReplaceAll(before, `"pv_size":"205520896"`, `"pv_size":"`+test.afterBytes+`"`), `"pv_pe_count":"49"`, `"pv_pe_count":"`+test.afterCount+`"`)
				storageStubFile(t, "after-pvresize/report-pvs", lvmReport("pv", true, after))
			}
			if test.refuses {
				storageStubFile(t, "pvresize-refuses", "Failed to resize physical volume\n")
			}
			if test.failsAfterChange {
				storageStubFile(t, "pvresize-fails-after-its-change", "Failed after updating metadata\n")
			}
			if test.readFails {
				storageStubFile(t, "after-pvresize/pvs-fails", "Cannot read LVM metadata\n")
			}
			inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{device}, "physical_volumes_not_listed": "keep"}
			if test.disable {
				inputs["grow_physical_volumes_with_their_devices"] = false
			}
			result := runOfficialExtraModule(t, "lvm_volume_group", inputs, test.dryrun)
			described := "the physical volume " + device + " of the volume group data from 196 MiB to 496 MiB"
			if test.deviceBytes == "210763776" {
				described = "the physical volume " + device + " of the volume group data from 196 MiB to 200 MiB"
			}
			switch {
			case test.failsAfterChange:
				storageTaskFailedOnce(t, result, "lvm_volume_group: growing "+described+": Failed after updating metadata. The physical volume went from 196 MiB to 496 MiB")
			case test.refuses:
				storageTaskFailed(t, result, test.change, "lvm_volume_group: growing "+described+": Failed to resize physical volume. The physical volume went from 196 MiB to 196 MiB")
			case test.readFails:
				storageTaskFailedOnce(t, result, "lvm_volume_group: pvs could not read LVM's state: Cannot read LVM metadata. This was after pvresize tried growing "+described+": Physical volume \""+device+"\" changed 1 physical volume(s) resized or updated / 0 physical volume(s) not resized")
			case test.name == "success gains nothing":
				storageTaskFailed(t, result, test.change, "lvm_volume_group: pvresize reported success but gained no extents while growing "+described+": Physical volume \""+device+"\" changed 1 physical volume(s) resized or updated / 0 physical volume(s) not resized. The physical volume went from 196 MiB to 196 MiB; check pvs on the node, or set grow_physical_volumes_with_their_devices: false")
			default:
				reason := ""
				if test.reason != "" {
					reason = test.reason + " " + described
				}
				storageResultIs(t, result, "succeeded", test.change, reason)
			}
			changing := changingStorageCalls(calls())
			if test.change == "changed" {
				if !reflect.DeepEqual(changing, []string{"pvresize " + device}) {
					t.Fatalf("changing calls %q", changing)
				}
				if got := calls(); got[len(got)-1] != pvsColumns {
					t.Fatalf("no verification read: %q", got)
				}
			} else if changing != nil {
				t.Fatalf("ran %q", changing)
			}
			if test.name == "grows" || test.name == "exactly one more extent" {
				beforeCalls := len(calls())
				storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "succeeded", "unchanged", "")
				if got := changingStorageCalls(calls()[beforeCalls:]); got != nil {
					t.Fatalf("rerun changed: %q", got)
				}
			}
		})
	}
}

// A listed device that the volume group lacks is added, and a device that
// the task does not list leaves the volume group and loses LVM's label.
func TestLVMVolumeGroupAddsAndRemovesDevices(t *testing.T) {
	t.Parallel()
	devices, calls := newLVMNode(t, true, "loop0", "loop1")
	storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
	storageStubFile(t, "lsblk-loop1", lsblkNumbers("/dev/loop1", "7:1"))
	storageStubFile(t, "report-vgs", lvmReport("vg", true, `{"vg_name":"data", "lv_count":"1"}`, `{"vg_name":"other", "lv_count":"0"}`))
	// loop1 is a physical volume that belongs to no volume group, as an
	// interrupted run leaves it: LVM takes it without a question.
	storageStubFile(t, "report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "54525952"), pvsRow("/dev/loop1", "", "7", "1", "0"),
		pvsRow("/dev/loop2p1", "data", "259", "0", "0"), pvsRow("/dev/loop9", "other", "7", "9", "0")))
	inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"], devices["loop1"]}, "physical_volumes_not_listed": "remove"}
	changes := "add " + devices["loop1"] + " to the volume group data; would take /dev/loop2p1 out of the volume group data and remove LVM's label from it"

	dryrun := runOfficialExtraModule(t, "lvm_volume_group", inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would "+changes)
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("a dryrun ran %q", changing)
	}
	for _, command := range []string{"vgextend data " + devices["loop1"], "vgreduce data /dev/loop2p1", "pvremove /dev/loop2p1"} {
		if !strings.Contains(dryrun.Diff, "would run: "+command+"\n") {
			t.Fatalf("dryrun difference %q lacks %q", dryrun.Diff, command)
		}
	}
	run := runOfficialExtraModule(t, "lvm_volume_group", inputs, false)
	storageResultIs(t, run, "succeeded", "changed", "added "+devices["loop1"]+" to the volume group data; took /dev/loop2p1 out of the volume group data and removed LVM's label from it")
	want := []string{"vgextend data " + devices["loop1"], "vgreduce data /dev/loop2p1", "pvremove /dev/loop2p1"}
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, want) {
		t.Fatalf("changing calls %q", got)
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "wipefs ") {
			t.Fatalf("an unused physical volume was probed: %q", call)
		}
	}

	// keep leaves the device that the task does not list.
	inputs["physical_volumes_not_listed"] = "keep"
	before := len(calls())
	storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "succeeded", "changed", "added "+devices["loop1"]+" to the volume group data")
	if got := changingStorageCalls(calls()[before:]); !reflect.DeepEqual(got, []string{"vgextend data " + devices["loop1"]}) {
		t.Fatalf("changing calls with keep %q", got)
	}
}

// What the module finds before it changes anything fails a run, and no
// changing command runs. A state that no node task changes fails a dryrun
// too. A state that an earlier node task of the same playbook may change,
// such as logical volumes that it deletes first or a device that it takes
// out of another volume group, leaves a dryrun not checked.
func TestLVMVolumeGroupRefusesBeforeAnyChange(t *testing.T) {
	t.Parallel()
	const neverWipes = ". lvm_volume_group adds only a device that holds nothing or an unused LVM physical volume, and never wipes a device. To use this device, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all "
	for name, c := range map[string]struct {
		wipefs, pvs string
		state       string
		reason      func(devices map[string]string) string
		// an earlier node task may change the state that is in the way
		earlierTask bool
	}{
		"a device with a filesystem": {wipefs: wipefsOfExt4, reason: func(d map[string]string) string {
			return "the device " + d["loop1"] + " is not empty; found: ext4 filesystem (ext4 signature at offset 0x438)" + neverWipes + d["loop1"]
		}},
		"a device with a partition table": {wipefs: wipefsOfDiskWithGPT, reason: func(d map[string]string) string {
			return "the device " + d["loop1"] + " is not empty; found: GPT partition table (gpt signature at offset 0x200), protective MBR of a GPT partition table (PMBR signature at offset 0x1fe)" + neverWipes + d["loop1"]
		}},
		"a device of another volume group": {earlierTask: true, pvs: lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop1", "backup", "7", "1", "0")), reason: func(d map[string]string) string {
			return "the device " + d["loop1"] + " belongs to the volume group backup; a device is in one volume group only"
		}},
		"a device that holds data cannot leave": {earlierTask: true, pvs: lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop1", "", "7", "1", "0"), pvsRow("/dev/loop3", "data", "7", "3", "54525952")), reason: func(d map[string]string) string {
			return "the device /dev/loop3 of the volume group data still holds 52 MiB of logical volumes, so it cannot leave the volume group. Move that data to the other devices first, such as with pvmove /dev/loop3, or list the device in physical_volumes"
		}},
		"a volume group with logical volumes that the task wants absent": {earlierTask: true, state: "absent", reason: func(d map[string]string) string {
			return "the volume group data still has the logical volumes postgresql, logs, so it stays. Remove each of them first with lvm_logical_volume and state: deleted_with_all_its_data, which destroys its data"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			devices, calls := newLVMNode(t, true, "loop0", "loop1")
			storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
			storageStubFile(t, "lsblk-loop1", lsblkNumbers("/dev/loop1", "7:1"))
			storageStubFile(t, "report-vgs", lvmReport("vg", true, `{"vg_name":"data", "lv_count":"2"}`))
			storageStubFile(t, "report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0")))
			storageStubFile(t, "report-lvs", lvmReport("lv", true, `{"lv_name":"postgresql", "vg_name":"data"}`, `{"lv_name":"root", "vg_name":"system"}`, `{"lv_name":"logs", "vg_name":"data"}`))
			if c.pvs != "" {
				storageStubFile(t, "report-pvs", c.pvs)
			}
			if c.wipefs != "" {
				storageStubFile(t, "wipefs-loop1", c.wipefs)
			}
			inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"], devices["loop1"]}, "physical_volumes_not_listed": "remove"}
			if c.state != "" {
				inputs = map[string]any{"name": "data", "state": c.state}
			}
			if c.earlierTask {
				storageTaskWaitsForAnEarlierTask(t, "lvm_volume_group", inputs, "lvm_volume_group: "+c.reason(devices))
			} else {
				for _, dryrun := range []bool{true, false} {
					storageTaskIsRefused(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, dryrun), "unchanged", "lvm_volume_group: "+c.reason(devices))
				}
			}
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("a refused task ran %q", changing)
			}
		})
	}
}

func TestLVMVolumeGroupIsRemovedWithItsLabels(t *testing.T) {
	t.Parallel()
	_, calls := newLVMNode(t, false)
	inputs := map[string]any{"name": "data", "state": "absent"}
	// A volume group that does not exist is unchanged.
	storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "succeeded", "unchanged", "")
	if got := calls(); !reflect.DeepEqual(got, []string{vgsColumnsForVolumeGroup, pvsColumns}) {
		t.Fatalf("calls %q", got)
	}
	storageStubFile(t, "report-vgs", lvmReport("vg", false, `{"vg_name":"data", "lv_count":"0"}`))
	storageStubFile(t, "report-pvs", lvmReport("pv", false, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop2p1", "data", "259", "0", "0"), pvsRow("/dev/loop5", "other", "7", "5", "0")))
	storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, true), "succeeded", "would_change", "would remove the volume group data and LVM's label from /dev/loop0, /dev/loop2p1")
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("a dryrun ran %q", changing)
	}
	removed := runOfficialExtraModule(t, "lvm_volume_group", inputs, false)
	storageResultIs(t, removed, "succeeded", "changed", "removed the volume group data and LVM's label from /dev/loop0, /dev/loop2p1")
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"vgremove data", "pvremove /dev/loop0", "pvremove /dev/loop2p1"}) || !reflect.DeepEqual(removed.ModuleOutputs, map[string]any{"created": false}) {
		t.Fatalf("changing calls %q, outputs %v", got, removed.ModuleOutputs)
	}
}

// A read that fails is no missing volume group: older LVM writes its
// message on standard error, newer LVM into the report's log.
func TestLVMReadsThatFail(t *testing.T) {
	t.Parallel()
	const newLog = `  {
      "report": [
          {
              "vg": [
              ]
          }
      ]
      ,
      "log": [
          {"log_seq_num":"1", "log_type":"error", "log_context":"processing", "log_object_type":"vg", "log_object_name":"data", "log_object_id":"", "log_object_group":"", "log_object_group_id":"", "log_message":"Volume group \"data\" not found", "log_errno":"-1", "log_ret_code":"0"},
          {"log_seq_num":"2", "log_type":"error", "log_context":"processing", "log_object_type":"vg", "log_object_name":"data", "log_object_id":"", "log_object_group":"", "log_object_group_id":"", "log_message":"Cannot process volume group data", "log_errno":"-1", "log_ret_code":"0"},
          {"log_seq_num":"3", "log_type":"status", "log_context":"processing", "log_object_type":"vg", "log_object_name":"data", "log_object_id":"", "log_object_group":"", "log_object_group_id":"", "log_message":"failure", "log_errno":"-1", "log_ret_code":"5"}
      ]
  }
`
	const reason = `: vgs could not read LVM's state: Volume group "data" not found Cannot process volume group data`
	for name, c := range map[string]struct{ output, standardError string }{
		"LVM 2.03.11 writes on standard error":     {lvmReport("vg", false), "  Volume group \"data\" not found\n  Cannot process volume group data\n"},
		"LVM 2.03.31 writes into the report's log": {newLog, ""},
	} {
		t.Run(name, func(t *testing.T) {
			devices, calls := newLVMNode(t, true, "loop0")
			storageStubFile(t, "vgs-fails", c.standardError)
			storageStubFile(t, "vgs-fails-output", c.output)
			group := runOfficialExtraModule(t, "lvm_volume_group", map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"]}, "physical_volumes_not_listed": "keep"}, false)
			storageTaskFailed(t, group, "unchanged", "lvm_volume_group"+reason)
			for _, dryrun := range []bool{true, false} {
				volume := runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50GiB"}, dryrun)
				storageTaskFailed(t, volume, "unchanged", "lvm_logical_volume"+reason)
			}
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("ran %q", changing)
			}
		})
	}
	t.Run("a report that is no JSON", func(t *testing.T) {
		_, _ = newLVMNode(t, true)
		storageStubFile(t, "report-vgs", "  VG   #PV #LV\n  data   1   0\n")
		storageTaskFailed(t, runOfficialExtraModule(t, "lvm_volume_group", map[string]any{"name": "data", "state": "absent"}, false), "unchanged",
			"lvm_volume_group: vgs printed what MMX cannot read: VG   #PV #LV data   1   0")
	})
}

// LVM's own refusal is the second guard behind the module's probe, and a
// command that fails after an earlier one succeeded leaves the task changed.
func TestLVMVolumeGroupReportsWhatLVMRefuses(t *testing.T) {
	t.Parallel()
	devices, calls := newLVMNode(t, true, "loop0", "loop1")
	storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
	storageStubFile(t, "lsblk-loop1", lsblkNumbers("/dev/loop1", "7:1"))
	inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"], devices["loop1"]}, "physical_volumes_not_listed": "remove"}
	storageStubFile(t, "vgcreate-refuses", lvmRefusesSignature)
	storageTaskFailed(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "changed",
		"lvm_volume_group: creating the volume group data on "+devices["loop0"]+", "+devices["loop1"]+": WARNING: ext4 signature detected on /dev/loop4 at offset 1080. Wipe it? [y/n]: [n] Aborted wiping of ext4. 1 existing signature left on the device.")
	for _, call := range calls() {
		if strings.Contains(call, "--yes") || strings.Contains(call, "--force") || strings.Contains(call, " -y") || strings.Contains(call, " -f") {
			t.Fatalf("the module answered LVM's question: %q", call)
		}
	}
	// A device that device-mapper holds and that has no signature: the
	// module finds nothing on it, and LVM refuses it, because LVM opens a
	// new device for itself alone. These are the words of LVM 2.03.11;
	// LVM 2.03.43 adds two more lines.
	storageStubFile(t, "vgcreate-refuses", "  Can't open /dev/loop1 exclusively.  Mounted filesystem?\n  Can't open /dev/loop1 exclusively.  Mounted filesystem?\n")
	storageTaskFailed(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "changed",
		"lvm_volume_group: creating the volume group data on "+devices["loop0"]+", "+devices["loop1"]+": Can't open /dev/loop1 exclusively.  Mounted filesystem? Can't open /dev/loop1 exclusively.  Mounted filesystem?")
	removeStorageStubFile(t, "vgcreate-refuses")

	// vgextend succeeds, then vgreduce refuses: the device was added, the
	// task fails, and the next run takes the other device out.
	storageStubFile(t, "report-vgs", lvmReport("vg", true, `{"vg_name":"data", "lv_count":"0"}`))
	storageStubFile(t, "report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop3", "data", "7", "3", "0")))
	storageStubFile(t, "after-vgextend/report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop1", "data", "7", "1", "0"), pvsRow("/dev/loop3", "data", "7", "3", "0")))
	storageStubFile(t, "vgreduce-refuses", "  Physical volume \"/dev/loop3\" still in use\n")
	before := len(calls())
	storageTaskFailed(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "changed",
		`lvm_volume_group: taking /dev/loop3 out of the volume group data: Physical volume "/dev/loop3" still in use`)
	if got := changingStorageCalls(calls()[before:]); !reflect.DeepEqual(got, []string{"vgextend data " + devices["loop1"], "vgreduce data /dev/loop3"}) {
		t.Fatalf("changing calls %q", got)
	}
	removeStorageStubFile(t, "vgreduce-refuses")
	before = len(calls())
	storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "succeeded", "changed", "took /dev/loop3 out of the volume group data and removed LVM's label from it")
	if got := changingStorageCalls(calls()[before:]); !reflect.DeepEqual(got, []string{"vgreduce data /dev/loop3", "pvremove /dev/loop3"}) {
		t.Fatalf("changing calls of the next run %q", got)
	}
}

// A device that left the volume group, or a volume group that is gone, and
// LVM's label that could not be removed: the next try would find nothing to
// do and report success, so the script asks for no other try, and the reason
// says what was done and what is left to do by hand. LVM 2.03.11 and
// 2.03.31 word pvremove's failure differently.
func TestLVMVolumeGroupDoesNotHideALabelThatStays(t *testing.T) {
	t.Parallel()
	for name, said := range map[string]string{
		"LVM 2.03.11": "  Cannot use /dev/loop3: device is too small (pv_min_size)\n",
		"LVM 2.03.31": "  No PV found on device /dev/loop3.\n",
	} {
		t.Run(name, func(t *testing.T) {
			devices, calls := newLVMNode(t, true, "loop0")
			storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
			storageStubFile(t, "report-vgs", lvmReport("vg", true, `{"vg_name":"data", "lv_count":"0"}`))
			storageStubFile(t, "report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop3", "data", "7", "3", "0")))
			storageStubFile(t, "after-vgreduce/report-pvs", lvmReport("pv", true, pvsRow("/dev/loop0", "data", "7", "0", "0"), pvsRow("/dev/loop3", "", "7", "3", "0")))
			storageStubFile(t, "pvremove-refuses", said)
			words := strings.TrimSpace(said)
			inputs := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{devices["loop0"]}, "physical_volumes_not_listed": "remove"}
			storageTaskFailedOnce(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false),
				"lvm_volume_group: /dev/loop3 left the volume group data, and LVM's label stays on it; remove it by hand with pvremove /dev/loop3: "+words)
			if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"vgreduce data /dev/loop3", "pvremove /dev/loop3"}) {
				t.Fatalf("changing calls %q", got)
			}
			// This is what another try would have reported.
			storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs, false), "succeeded", "unchanged", "")

			// The same for a volume group that the task removes.
			storageStubFile(t, "report-pvs", lvmReport("pv", true, pvsRow("/dev/loop3", "data", "7", "3", "0")))
			storageStubFile(t, "after-vgremove/report-vgs", lvmReport("vg", true))
			storageTaskFailedOnce(t, runOfficialExtraModule(t, "lvm_volume_group", map[string]any{"name": "data", "state": "absent"}, false),
				"lvm_volume_group: the volume group data was removed, and LVM's label stays on /dev/loop3; remove it by hand with pvremove /dev/loop3: "+words)
			storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", map[string]any{"name": "data", "state": "absent"}, false), "succeeded", "unchanged", "")
		})
	}
}

func TestLVMVolumeGroupMissingDevicesAndPrograms(t *testing.T) {
	t.Parallel()
	devices, calls := newLVMNode(t, true, "loop0", "file")
	storageStubFile(t, "lsblk-loop0", lsblkNumbers("/dev/loop0", "7:0"))
	inputs := func(listed ...any) map[string]any {
		return map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": listed, "physical_volumes_not_listed": "keep"}
	}
	// A partition that an earlier node task of the run creates.
	const notYet = "lvm_volume_group: the device /dev/disk/by-partlabel/mmx-test-no-such-partition does not exist yet"
	storageResultIs(t, runOfficialExtraModule(t, "lvm_volume_group", inputs(devices["loop0"], "/dev/disk/by-partlabel/mmx-test-no-such-partition"), true), "not_checked", "unknown", notYet)
	run := runOfficialExtraModule(t, "lvm_volume_group", inputs(devices["loop0"], "/dev/disk/by-partlabel/mmx-test-no-such-partition"), false)
	storageResultIs(t, run, "failed", "unchanged", notYet)
	if run.FailureKind != "missing_prerequisite" {
		t.Fatalf("failure kind %q", run.FailureKind)
	}
	storageTaskIsRefused(t, runOfficialExtraModule(t, "lvm_volume_group", inputs(devices["file"]), false), "unchanged",
		"lvm_volume_group: "+devices["file"]+" is not a block device; physical_volumes lists disks and partitions, such as /dev/sdb1")
	// Two paths that lead to one device.
	storageStubFile(t, "lsblk-file", lsblkNumbers("/dev/loop0", "7:0"))
	same := runOfficialExtraModule(t, "lvm_volume_group", inputs(devices["loop0"], devices["file"]), false)
	storageResultIs(t, same, "failed", "unchanged", "lvm_volume_group.physical_volumes: physical_volumes[0] and physical_volumes[1] lead to the same device, /dev/loop0; list each device once")
	if same.FailureKind != "config" || changingStorageCalls(calls()) != nil {
		t.Fatalf("failure kind %q, changing calls %q", same.FailureKind, changingStorageCalls(calls()))
	}

	for module, programs := range map[string][]string{
		"lvm_volume_group":   {"vgs", "pvs", "lvs", "vgcreate", "vgextend", "vgreduce", "vgremove", "pvremove", "lsblk", "wipefs"},
		"lvm_logical_volume": {"vgs", "lvs", "lvcreate", "lvextend", "lvremove", "wipefs"},
	} {
		for _, program := range programs {
			if _, err := os.Stat("/usr/bin/" + program); err == nil {
				// The stand-ins' PATH also holds /usr/bin, where a Linux machine has the real program.
				continue
			}
			tools := map[string]string{}
			for name, body := range lvmTools {
				if name != program {
					tools[name] = body
				}
			}
			calls := stubTools(t, tools)
			install := "lvm2"
			if program == "lsblk" || program == "wipefs" {
				install = "util-linux"
			}
			reason := module + ": the program " + program + " is not installed; install the package " + install + " in an earlier node task"
			task := map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb"}, "physical_volumes_not_listed": "keep"}
			if module == "lvm_logical_volume" {
				task = map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50GiB"}
			}
			storageResultIs(t, runOfficialExtraModule(t, module, task, true), "not_checked", "unknown", reason)
			run := runOfficialExtraModule(t, module, task, false)
			storageResultIs(t, run, "failed", "unchanged", reason)
			if run.FailureKind != "missing_prerequisite" || calls() != nil {
				t.Fatalf("%s without %s: failure kind %q, calls %q", module, program, run.FailureKind, calls())
			}
		}
	}
}

func TestLVMVolumeGroupRejectsInputs(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, lvmTools)
	const nameRule = "name: expected the volume group's name: 1 to 127 letters, digits, +, _, ., and -, not starting with - and not . or .."
	for name, c := range map[string]struct {
		inputs map[string]any
		reason string
	}{
		"a name with a slash":           {map[string]any{"name": "data/one", "state": "absent"}, nameRule},
		"a name that starts with -":     {map[string]any{"name": "-data", "state": "absent"}, nameRule},
		"a name with a space":           {map[string]any{"name": "data one", "state": "absent"}, nameRule},
		"an empty name":                 {map[string]any{"name": "", "state": "absent"}, "name: must not be empty"},
		"the name ..":                   {map[string]any{"name": "..", "state": "absent"}, nameRule},
		"a name of 128 characters":      {map[string]any{"name": strings.Repeat("a", 128), "state": "absent"}, nameRule},
		"absent with growth":            {map[string]any{"name": "data", "state": "absent", "grow_physical_volumes_with_their_devices": false}, "state: absent accepts only name; remove grow_physical_volumes_with_their_devices"},
		"absent with devices":           {map[string]any{"name": "data", "state": "absent", "physical_volumes": []any{"/dev/sdb"}}, "state: absent accepts only name; remove physical_volumes"},
		"absent with the other key":     {map[string]any{"name": "data", "state": "absent", "physical_volumes_not_listed": "keep"}, "state: absent accepts only name; remove physical_volumes_not_listed"},
		"present without devices":       {map[string]any{"name": "data"}, "physical_volumes: is required for state present: the devices of the volume group, at least one, such as [/dev/disk/by-partlabel/lvm]"},
		"the other key without devices": {map[string]any{"name": "data", "physical_volumes_not_listed": "keep"}, "physical_volumes: is required with physical_volumes_not_listed"},
		"an empty device":               {map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb", ""}, "physical_volumes_not_listed": "keep"}, "physical_volumes[1]: must not be empty"},
		"present with an empty list":    {map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{}, "physical_volumes_not_listed": "keep"}, "physical_volumes: is required for state present: the devices of the volume group, at least one, such as [/dev/disk/by-partlabel/lvm]"},
		"devices without the other key": {map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb"}}, "physical_volumes_not_listed: is required with physical_volumes: remove takes a device that the task does not list out of the volume group, and keep leaves it there"},
		"a relative device":             {map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb", "sdc"}, "physical_volumes_not_listed": "keep"}, "physical_volumes[1]: expected the absolute path of a device, such as /dev/sdb1 or /dev/disk/by-partlabel/lvm"},
		"a device listed twice":         {map[string]any{"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb", "/dev/sdc", "/dev/sdb"}, "physical_volumes_not_listed": "keep"}, "physical_volumes[2]: the same path as physical_volumes[0]; list each device once"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "lvm_volume_group", c.inputs, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "lvm_volume_group."+c.reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	// What the description itself refuses, before the script runs.
	for name, inputs := range map[string]map[string]any{
		"an unknown state":             {"name": "data", "state": "removed"},
		"an unknown choice for others": {"grow_physical_volumes_with_their_devices": true, "name": "data", "physical_volumes": []any{"/dev/sdb"}, "physical_volumes_not_listed": "wipe"},
	} {
		if r := runOfficialExtraModule(t, "lvm_volume_group", inputs, true); r.FailureKind != "config" || !strings.Contains(r.Reason, "expected one of") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// A name with every character that LVM allows is taken.
	if r := runOfficialExtraModule(t, "lvm_volume_group", map[string]any{"name": "mmx-m.vg+1_a", "state": "absent"}, true); r.FailureKind == "config" {
		t.Errorf("an allowed name: %+v", r)
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("rejected inputs ran %q", changing)
	}
}

// wipefsOfLogicalVolume is what wipefs --no-act --json printed for a
// logical volume that holds an ext4 or xfs filesystem or a swap area, with
// util-linux 2.37.2 and 2.41.5 (Debian 13) alike. For a volume that holds
// nothing it prints the empty list, which is the stand-in's answer without a
// file.
func wipefsOfLogicalVolume(signature, offset string) string {
	return `{
   "signatures": [
      {
         "device": "postgresql",
         "offset": "` + offset + `",
         "type": "` + signature + `",
         "uuid": "64208315-a3c3-4c76-9f08-0c70f11c3688",
         "label": null
      }
   ]
}
`
}

// lvextendHelp is the part of lvextend --help that names the options for a
// filesystem: older LVM has no --fsmode.
func lvextendHelp(newLVM bool) string {
	if newLVM {
		return "  lvextend - Add space to a logical volume\n\n  Extend an LV by a specified size.\n  lvextend -L|--size [+]Size[m|UNIT] LV\n\t[ -r|--resizefs ]\n\t[    --fsmode String ]\n"
	}
	return "  lvextend - Add space to a logical volume\n\n  Extend an LV by a specified size.\n  lvextend -L|--size [+]Size[m|UNIT] LV\n\t[ -r|--resizefs ]\n"
}

// newLVMVolumeGroup is a node with the volume group data: extents of 4 MiB
// and 1006632960 bytes (960 MiB) free.
func newLVMVolumeGroup(t *testing.T, newLVM bool) (calls func() []string) {
	t.Helper()
	_, calls = newLVMNode(t, newLVM)
	storageStubFile(t, "report-vgs", lvmReport("vg", newLVM, `{"vg_name":"system", "vg_extent_size":"4194304", "vg_free":"0"}`, `{"vg_name":"data", "vg_extent_size":"4194304", "vg_free":"1006632960"}`))
	storageStubFile(t, "report-lvs", lvmReport("lv", newLVM, lvsRow("postgresql", "system", "8388608", "")))
	storageStubFile(t, "lvextend-help", lvextendHelp(newLVM))
	return calls
}

func TestLVMLogicalVolumeIsCreatedOnce(t *testing.T) {
	t.Parallel()
	for name, newLVM := range map[string]bool{"LVM 2.03.11": false, "LVM 2.03.31": true} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, newLVM)
			// 50 MiB is no whole number of extents of 4 MiB: LVM rounds up.
			storageStubFile(t, "after-lvcreate/report-lvs", lvmReport("lv", newLVM, lvsRow("postgresql", "system", "8388608", ""), lvsRow("postgresql", "data", "54525952", "")))
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50MiB"}
			const described = "the logical volume data/postgresql with 52 MiB, which is 50 MiB rounded up to whole extents of 4 MiB"
			outputs := map[string]any{"created": true, "device": "/dev/data/postgresql"}

			dryrun := runOfficialExtraModule(t, "lvm_logical_volume", inputs, true)
			storageResultIs(t, dryrun, "succeeded", "would_change", "would create "+described)
			if got := calls(); !reflect.DeepEqual(got, []string{vgsColumnsForVolume, lvsColumnsForVolume}) || !reflect.DeepEqual(dryrun.ModuleOutputs, outputs) {
				t.Fatalf("dryrun calls %q, outputs %v", got, dryrun.ModuleOutputs)
			}
			run := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
			storageResultIs(t, run, "succeeded", "changed", "created "+described)
			want := []string{vgsColumnsForVolume, lvsColumnsForVolume, "lvcreate --yes --wipesignatures y --name postgresql --size 50MiB data", lvsColumnsForVolume}
			if got := calls()[2:]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(run.ModuleOutputs, outputs) || run.Fingerprint == "" || run.Fingerprint != dryrun.Fingerprint {
				t.Fatalf("run calls %q, outputs %v", got, run.ModuleOutputs)
			}
			before := len(calls())
			for _, dryrun := range []bool{false, true} {
				again := runOfficialExtraModule(t, "lvm_logical_volume", inputs, dryrun)
				storageResultIs(t, again, "succeeded", "unchanged", "")
				if !reflect.DeepEqual(again.ModuleOutputs, map[string]any{"created": false, "device": "/dev/data/postgresql"}) || again.Fingerprint != run.Fingerprint {
					t.Fatalf("unchanged outputs %v", again.ModuleOutputs)
				}
			}
			if got := calls()[before:]; !reflect.DeepEqual(got, []string{vgsColumnsForVolume, lvsColumnsForVolume, vgsColumnsForVolume, lvsColumnsForVolume}) {
				t.Fatalf("unchanged calls %q", got)
			}
		})
	}
}

func TestLVMLogicalVolumeTakesTheRestAndLvcreateOptions(t *testing.T) {
	t.Parallel()
	calls := newLVMVolumeGroup(t, true)
	storageStubFile(t, "after-lvcreate/report-lvs", lvmReport("lv", true, lvsRow("archive", "data", "1006632960", "")))
	rest := map[string]any{"grow_filesystem_with_volume": true, "name": "archive", "volume_group": "data", "size": "all_free_space_when_created"}
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", rest, true), "succeeded", "would_change", "would create the logical volume data/archive with 960 MiB, the rest of the volume group")
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", rest, false), "succeeded", "changed", "created the logical volume data/archive with 960 MiB, the rest of the volume group")
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"lvcreate --yes --wipesignatures y --name archive --extents 100%FREE data"}) {
		t.Fatalf("changing calls %q", got)
	}
	// An existing volume is unchanged for the rest of the volume group,
	// whatever its size and whatever is free.
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", rest, false), "succeeded", "unchanged", "")

	// lvcreate's own options go before the volume group, and the reason
	// names the size that LVM reports.
	calls = newLVMVolumeGroup(t, true)
	storageStubFile(t, "after-lvcreate/report-lvs", lvmReport("lv", true, lvsRow("accounts", "data", "41943040", "")))
	mirrored := map[string]any{"grow_filesystem_with_volume": true, "name": "accounts", "volume_group": "data", "size": "40MiB", "lvcreate_options": []any{"--type", "raid1", "--mirrors", "1"}}
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", mirrored, false), "succeeded", "changed", "created the logical volume data/accounts with 40 MiB")
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"lvcreate --yes --wipesignatures y --name accounts --size 40MiB --type raid1 --mirrors 1 data"}) {
		t.Fatalf("changing calls %q", got)
	}
}

// A smaller volume grows, with its filesystem unless the task says no. The
// module first looks whether the volume holds an ext4 or xfs filesystem,
// and not at all when the task says that no filesystem grows. LVM 2.03.31
// and newer need --fsmode manage for an xfs filesystem that is not mounted;
// older LVM does not know the option.
func TestLVMLogicalVolumeGrows(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		newLVM  bool
		without bool
		want    []string
		words   string
	}{
		"LVM 2.03.11 with the filesystem": {false, false, []string{"wipefs --no-act --json /dev/data/postgresql", "lvextend --help", "lvextend --resizefs --size 100MiB data/postgresql"}, "together with the filesystem on it"},
		"LVM 2.03.31 with the filesystem": {true, false, []string{"wipefs --no-act --json /dev/data/postgresql", "lvextend --help", "lvextend --resizefs --fsmode manage --size 100MiB data/postgresql"}, "together with the filesystem on it"},
		"without the filesystem":          {true, true, []string{"lvextend --size 100MiB data/postgresql"}, "and not a filesystem on it, because grow_filesystem_with_volume is false"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, c.newLVM)
			storageStubFile(t, "report-lvs", lvmReport("lv", c.newLVM, lvsRow("postgresql", "data", "54525952", "open")))
			storageStubFile(t, "after-lvextend/report-lvs", lvmReport("lv", c.newLVM, lvsRow("postgresql", "data", "104857600", "open")))
			storageStubFile(t, "wipefs-postgresql", wipefsOfLogicalVolume("ext4", "0x438"))
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "100MiB"}
			if c.without {
				inputs["grow_filesystem_with_volume"] = false
			}
			change := "the logical volume data/postgresql from 52 MiB to 100 MiB, " + c.words
			dryrun := runOfficialExtraModule(t, "lvm_logical_volume", inputs, true)
			storageResultIs(t, dryrun, "succeeded", "would_change", "would grow "+change)
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("a dryrun ran %q", changing)
			}
			before := len(calls())
			run := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
			storageResultIs(t, run, "succeeded", "changed", "grew "+change)
			want := append(append([]string{vgsColumnsForVolume, lvsColumnsForVolume}, c.want...), lvsColumnsForVolume)
			if got := calls()[before:]; !reflect.DeepEqual(got, want) {
				t.Fatalf("run calls %q", got)
			}
			if !reflect.DeepEqual(run.ModuleOutputs, map[string]any{"created": false, "device": "/dev/data/postgresql"}) {
				t.Fatalf("outputs %v", run.ModuleOutputs)
			}
			storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "unchanged", "")
		})
	}
}

// With grow_filesystem_with_volume: true, a volume that holds something
// other than an ext2, ext3, ext4, or xfs filesystem is not grown: LVM would
// refuse and grow nothing, so the module looks first, and a dryrun fails as
// a run does. A volume that holds nothing grows alone.
func TestLVMLogicalVolumeGrowsOnlyAVolumeWithAFilesystemTogetherWithIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ wipefs, found string }{
		"a volume that holds swap":  {wipefsOfLogicalVolume("swap", "0xff6"), "swap area (swap signature at offset 0xff6)"},
		"a volume that holds btrfs": {wipefsOfLogicalVolume("btrfs", "0x10040"), "btrfs filesystem (btrfs signature at offset 0x10040)"},
		// More than one signature, as wipefs printed them for a device with a GPT partition table.
		"a volume that holds a partition table": {wipefsOfDiskWithGPT, "GPT partition table (gpt signature at offset 0x200), protective MBR of a GPT partition table (PMBR signature at offset 0x1fe)"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, true)
			storageStubFile(t, "report-lvs", lvmReport("lv", true, lvsRow("postgresql", "data", "54525952", "")))
			storageStubFile(t, "wipefs-postgresql", c.wipefs)
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "100MiB"}
			for _, dryrun := range []bool{true, false} {
				storageTaskIsRefused(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, dryrun), "unchanged",
					"lvm_logical_volume: the logical volume data/postgresql holds no ext2, ext3, ext4, or xfs filesystem (found: "+c.found+"), and grow_filesystem_with_volume is true: "+
						"LVM grows a volume together with its filesystem only for these four. To grow a volume without such a filesystem, such as swap, set grow_filesystem_with_volume: false. Nothing was changed")
			}
			if got := calls(); changingStorageCalls(got) != nil || got[len(got)-1] != "wipefs --no-act --json /dev/data/postgresql" {
				t.Fatalf("calls %q", got)
			}
			// The same task with false grows the volume and asks no wipefs.
			storageStubFile(t, "after-lvextend/report-lvs", lvmReport("lv", true, lvsRow("postgresql", "data", "104857600", "")))
			inputs["grow_filesystem_with_volume"] = false
			before := len(calls())
			storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "changed",
				"grew the logical volume data/postgresql from 52 MiB to 100 MiB, and not a filesystem on it, because grow_filesystem_with_volume is false")
			for _, call := range calls()[before:] {
				if strings.HasPrefix(call, "wipefs ") {
					t.Fatalf("the module probed a volume whose filesystem does not grow: %q", call)
				}
			}
		})
	}
	// A volume that holds nothing has no filesystem that could be left
	// small: it grows alone, with a plain lvextend, and the reason says so.
	// That is the volume of a first run that stopped before its filesystem
	// task, when the size was raised before the next run.
	for name, newLVM := range map[string]bool{"a volume that holds nothing, LVM 2.03.11": false, "a volume that holds nothing, LVM 2.03.31": true} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, newLVM)
			storageStubFile(t, "report-lvs", lvmReport("lv", newLVM, lvsRow("postgresql", "data", "54525952", "")))
			storageStubFile(t, "after-lvextend/report-lvs", lvmReport("lv", newLVM, lvsRow("postgresql", "data", "104857600", "")))
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "100MiB"}
			const change = "the logical volume data/postgresql from 52 MiB to 100 MiB, and no filesystem, because the volume holds none yet"
			storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, true), "succeeded", "would_change", "would grow "+change)
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("a dryrun ran %q", changing)
			}
			before := len(calls())
			run := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
			storageResultIs(t, run, "succeeded", "changed", "grew "+change)
			// No --resizefs, and no question to lvextend --help about --fsmode.
			want := []string{vgsColumnsForVolume, lvsColumnsForVolume, "wipefs --no-act --json /dev/data/postgresql", "lvextend --size 100MiB data/postgresql", lvsColumnsForVolume}
			if got := calls()[before:]; !reflect.DeepEqual(got, want) {
				t.Fatalf("run calls %q", got)
			}
			if !reflect.DeepEqual(run.ModuleOutputs, map[string]any{"created": false, "device": "/dev/data/postgresql"}) {
				t.Fatalf("outputs %v", run.ModuleOutputs)
			}
			storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "unchanged", "")
		})
	}
	// The xfs, ext2, and ext3 filesystems of a volume grow as the ext4 one
	// does: LVM 2.03.11 and 2.03.43 grew each of them on the test nodes.
	for signature, offset := range map[string]string{"xfs": "0x0", "ext2": "0x438", "ext3": "0x438"} {
		calls := newLVMVolumeGroup(t, true)
		storageStubFile(t, "report-lvs", lvmReport("lv", true, lvsRow("postgresql", "data", "335544320", "")))
		storageStubFile(t, "after-lvextend/report-lvs", lvmReport("lv", true, lvsRow("postgresql", "data", "419430400", "")))
		storageStubFile(t, "wipefs-postgresql", wipefsOfLogicalVolume(signature, offset))
		storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "400MiB"}, false), "succeeded", "changed",
			"grew the logical volume data/postgresql from 320 MiB to 400 MiB, together with the filesystem on it")
		if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"lvextend --resizefs --fsmode manage --size 400MiB data/postgresql"}) {
			t.Fatalf("%s: changing calls %q", signature, got)
		}
	}
}

// LVM's own refusal stays behind the module's look at the volume: the task
// fails with LVM's words, which older and newer LVM choose differently, and
// nothing has grown. When the filesystem's own program fails after the
// volume grew, the task asks for no other try.
func TestLVMLogicalVolumeReportsWhatLVMRefuses(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		newLVM bool
		words  string
		reason string
	}{
		"LVM 2.03.16": {false, "fsadm: Cannot get FSTYPE of \"/dev/mapper/data-postgresql\".\n  Filesystem check failed.\n", `fsadm: Cannot get FSTYPE of "/dev/mapper/data-postgresql". Filesystem check failed`},
		"LVM 2.03.31": {true, "  File system not found for --resizefs or --fs options.\n", "File system not found for --resizefs or --fs options"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, c.newLVM)
			storageStubFile(t, "report-lvs", lvmReport("lv", c.newLVM, lvsRow("postgresql", "data", "54525952", "")))
			storageStubFile(t, "wipefs-postgresql", wipefsOfLogicalVolume("ext4", "0x438"))
			storageStubFile(t, "lvextend-refuses", c.words)
			r := runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "100MiB"}, false)
			storageTaskFailed(t, r, "changed", "lvm_logical_volume: growing the logical volume data/postgresql from 52 MiB to 100 MiB, together with the filesystem on it: "+c.reason+". The logical volume still has 52 MiB")
			if got := changingStorageCalls(calls()); len(got) != 1 {
				t.Fatalf("changing calls %q", got)
			}
		})
	}
	// An ext4 filesystem that is mounted read-only: LVM grows the volume, and
	// resize2fs then fails. Another try would find the volume at its size
	// and report success, so the script asks for no other try; the result
	// stays a failure with its change and its outputs.
	for name, c := range map[string]struct {
		newLVM bool
		words  string
		reason string
	}{
		"LVM 2.03.11 grew the volume and resize2fs failed": {false, "resize2fs 1.46.5 (30-Dec-2021)\nresize2fs: Read-only file system While checking for on-line resizing support\nfsadm: ext4 resize failed.\n  /sbin/fsadm failed: 1\n",
			"resize2fs 1.46.5 (30-Dec-2021) resize2fs: Read-only file system While checking for on-line resizing support fsadm: ext4 resize failed. /sbin/fsadm failed: 1"},
		"LVM 2.03.31 grew the volume and resize2fs failed": {true, "resize2fs 1.47.2 (1-Jan-2025)\nresize2fs: Read-only file system While checking for on-line resizing support\n  Failed to extend file system with lvresize_fs_helper.\n  File system extend error.\n",
			"resize2fs 1.47.2 (1-Jan-2025) resize2fs: Read-only file system While checking for on-line resizing support Failed to extend file system with lvresize_fs_helper. File system extend error"},
	} {
		t.Run(name, func(t *testing.T) {
			newLVMVolumeGroup(t, c.newLVM)
			storageStubFile(t, "report-lvs", lvmReport("lv", c.newLVM, lvsRow("postgresql", "data", "54525952", "open")))
			storageStubFile(t, "after-lvextend/report-lvs", lvmReport("lv", c.newLVM, lvsRow("postgresql", "data", "104857600", "open")))
			storageStubFile(t, "wipefs-postgresql", wipefsOfLogicalVolume("ext4", "0x438"))
			storageStubFile(t, "lvextend-fails-after-its-change", c.words)
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "100MiB"}
			r := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
			storageTaskFailedOnce(t, r,
				"lvm_logical_volume: growing the logical volume data/postgresql from 52 MiB to 100 MiB, together with the filesystem on it: "+c.reason+
					". The logical volume now has 100 MiB, and its filesystem is still smaller: once the cause is gone, grow the filesystem by hand, with resize2fs for ext2, ext3, and ext4 or xfs_growfs for xfs. The next run finds the volume at its size and grows nothing")
			if !reflect.DeepEqual(r.ModuleOutputs, map[string]any{"created": false, "device": "/dev/data/postgresql"}) {
				t.Fatalf("outputs %v", r.ModuleOutputs)
			}
			storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "unchanged", "")
		})
	}
	t.Run("lvcreate refuses", func(t *testing.T) {
		newLVMVolumeGroup(t, true)
		storageStubFile(t, "lvcreate-refuses", "  Logical Volume \"postgresql\" already exists in volume group \"data\"\n")
		r := runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50MiB"}, false)
		storageTaskFailed(t, r, "changed", `lvm_logical_volume: creating the logical volume data/postgresql: Logical Volume "postgresql" already exists in volume group "data"`)
	})
	// The volume exists, and the read that follows fails. Another try would
	// find the volume and report created: false.
	t.Run("a read that fails right after lvcreate keeps created: true", func(t *testing.T) {
		newLVMVolumeGroup(t, true)
		storageStubFile(t, "after-lvcreate/report-lvs", "  LV VG\n  postgresql data\n")
		r := runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50MiB"}, false)
		storageTaskFailedOnce(t, r, "lvm_logical_volume: lvs printed what MMX cannot read: LV VG postgresql data. This was after lvcreate created the logical volume data/postgresql")
		if !reflect.DeepEqual(r.ModuleOutputs, map[string]any{"created": true, "device": "/dev/data/postgresql"}) {
			t.Fatalf("outputs %v", r.ModuleOutputs)
		}
	})
}

// What the module finds before it calls LVM fails a run, and no changing
// command runs. A volume that would have to shrink is a state that no node
// task changes, so it fails a dryrun too. Too little free space and a volume
// in use are states that an earlier node task of the same playbook may
// change: it adds a device to the volume group, or unmounts the filesystem.
// They leave a dryrun not checked.
func TestLVMLogicalVolumeRefusesBeforeAnyChange(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		lvs         string
		inputs      map[string]any
		reason      string
		earlierTask bool
	}{
		"a larger volume is never shrunk": {lvmReport("lv", true, lvsRow("postgresql", "data", "104857600", "")), map[string]any{"grow_filesystem_with_volume": true, "size": "50MiB"},
			"the logical volume data/postgresql has 100 MiB, and the task asks for 52 MiB. The module never shrinks a logical volume, because that destroys the data behind the new end. Write the size that the volume has, or shrink the filesystem and the volume by hand", false},
		"a new volume that does not fit": {"", map[string]any{"grow_filesystem_with_volume": true, "size": "50GiB"},
			"the volume group data has 960 MiB free, and the new logical volume data/postgresql needs 50 GiB", true},
		"a volume that cannot grow that far": {lvmReport("lv", true, lvsRow("postgresql", "data", "104857600", "")), map[string]any{"grow_filesystem_with_volume": true, "size": "2GiB"},
			"the logical volume data/postgresql has 100 MiB and needs 1948 MiB more for 2 GiB, and the volume group data has 960 MiB free", true},
		"a volume in use that the task wants deleted": {lvmReport("lv", true, lvsRow("postgresql", "data", "104857600", "open")), map[string]any{"state": "deleted_with_all_its_data"},
			"the logical volume data/postgresql is in use: a filesystem on it is mounted, or a program holds the device open. Unmount it or stop the program first; nothing was deleted", true},
	} {
		t.Run(name, func(t *testing.T) {
			calls := newLVMVolumeGroup(t, true)
			if c.lvs != "" {
				storageStubFile(t, "report-lvs", c.lvs)
			}
			inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data"}
			for key, value := range c.inputs {
				inputs[key] = value
			}
			if inputs["state"] == "deleted_with_all_its_data" {
				delete(inputs, "grow_filesystem_with_volume")
			}
			if c.earlierTask {
				storageTaskWaitsForAnEarlierTask(t, "lvm_logical_volume", inputs, "lvm_logical_volume: "+c.reason)
			} else {
				for _, dryrun := range []bool{true, false} {
					storageTaskIsRefused(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, dryrun), "unchanged", "lvm_logical_volume: "+c.reason)
				}
			}
			if changing := changingStorageCalls(calls()); changing != nil {
				t.Fatalf("a refused task ran %q", changing)
			}
		})
	}
	t.Run("no free space for the rest of the volume group", func(t *testing.T) {
		newLVMVolumeGroup(t, true)
		storageTaskWaitsForAnEarlierTask(t, "lvm_logical_volume", map[string]any{"grow_filesystem_with_volume": true, "name": "more", "volume_group": "system", "size": "all_free_space_when_created"},
			"lvm_logical_volume: the volume group system has no free space, so the new logical volume system/more cannot take the rest of it")
	})
}

func TestLVMLogicalVolumeIsDeletedWithAllItsData(t *testing.T) {
	t.Parallel()
	calls := newLVMVolumeGroup(t, false)
	inputs := map[string]any{"name": "postgresql", "volume_group": "data", "state": "deleted_with_all_its_data"}
	// A volume that does not exist is unchanged, and so is one in a volume
	// group that does not exist.
	for _, group := range []string{"data", "nowhere"} {
		inputs["volume_group"] = group
		missing := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
		storageResultIs(t, missing, "succeeded", "unchanged", "")
		if !reflect.DeepEqual(missing.ModuleOutputs, map[string]any{"created": false}) {
			t.Fatalf("outputs %v", missing.ModuleOutputs)
		}
	}
	inputs["volume_group"] = "data"
	storageStubFile(t, "report-lvs", lvmReport("lv", false, lvsRow("postgresql", "data", "104857600", "")))
	storageStubFile(t, "after-lvremove/report-lvs", lvmReport("lv", false))
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, true), "succeeded", "would_change", "would delete the logical volume data/postgresql with all its data (100 MiB)")
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("a dryrun ran %q", changing)
	}
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "changed", "deleted the logical volume data/postgresql with all its data (100 MiB)")
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"lvremove --yes data/postgresql"}) {
		t.Fatalf("changing calls %q", got)
	}
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "succeeded", "unchanged", "")
	// LVM's own refusal, for a volume that was opened after the module looked.
	storageStubFile(t, "report-lvs", lvmReport("lv", false, lvsRow("postgresql", "data", "104857600", "")))
	storageStubFile(t, "lvremove-refuses", "  Logical volume data/postgresql contains a filesystem in use.\n")
	storageTaskFailed(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, false), "changed", "lvm_logical_volume: deleting the logical volume data/postgresql: Logical volume data/postgresql contains a filesystem in use.")
}

// lvremove --yes on a volume with snapshots removes the snapshots too, and
// on a pool every volume in it: --yes answers LVM's question about them.
// The module reads which volumes depend on the one to delete and runs no
// lvremove while there is one. An earlier node task of the same playbook
// may delete them, so a dryrun does not check the task.
func TestLVMLogicalVolumeIsNotDeletedWithVolumesThatDependOnIt(t *testing.T) {
	t.Parallel()
	const deleteThemFirst = ". Delete each of them first with its own lvm_logical_volume task and state: deleted_with_all_its_data; nothing was deleted"
	deleted := func(name string) map[string]any {
		return map[string]any{"name": name, "volume_group": "data", "state": "deleted_with_all_its_data"}
	}
	calls := newLVMVolumeGroup(t, true)
	storageStubFile(t, "report-lvs", lvmReport("lv", true,
		lvsRowOfKind("origin", "data", "67108864", "", "", "", "public,origin,thickorigin,multithickorigin"),
		lvsRowOfKind("snap", "data", "16777216", "", "origin", "", "public,snapshot,thicksnapshot"),
		lvsRowOfKind("snap2", "data", "16777216", "", "origin", "", "public,snapshot,thicksnapshot"),
		// A volume of the same name in another volume group has its own snapshot.
		lvsRowOfKind("origin", "system", "67108864", "", "", "", "public,origin,thickorigin"),
		lvsRowOfKind("other", "system", "16777216", "", "origin", "", "public,snapshot,thicksnapshot"),
		`{"lv_name":"pool", "vg_name":"data", "lv_size":"67108864", "lv_path":"", "lv_device_open":"", "origin":"", "pool_lv":"", "lv_role":"private"}`,
		lvsRowOfKind("thin", "data", "33554432", "", "", "pool", "public,origin,thinorigin"),
		lvsRowOfKind("thinsnap", "data", "33554432", "unknown", "thin", "pool", "public,snapshot,thinsnapshot")))
	storageTaskWaitsForAnEarlierTask(t, "lvm_logical_volume", deleted("origin"),
		"lvm_logical_volume: other logical volumes depend on the logical volume data/origin, and LVM deletes them together with it: snap (a snapshot of it), snap2 (a snapshot of it)"+deleteThemFirst)
	storageTaskWaitsForAnEarlierTask(t, "lvm_logical_volume", deleted("pool"),
		"lvm_logical_volume: other logical volumes depend on the logical volume data/pool, and LVM deletes them together with it: thin (a volume in this pool), thinsnap (a volume in this pool)"+deleteThemFirst)
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
	// A snapshot itself is deleted, also one that is not active, for which
	// LVM prints unknown where it says whether a volume is open. A thin
	// volume with a snapshot is deleted too: LVM keeps that snapshot.
	for _, name := range []string{"snap", "thinsnap", "thin"} {
		storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", deleted(name), true), "succeeded", "would_change",
			"would delete the logical volume data/"+name+" with all its data ("+map[string]string{"snap": "16", "thinsnap": "32", "thin": "32"}[name]+" MiB)")
	}
	// The reason names the first five and counts the rest.
	rows := []string{`{"lv_name":"pool", "vg_name":"data", "lv_size":"67108864", "lv_path":"", "lv_device_open":"", "origin":"", "pool_lv":"", "lv_role":"private"}`}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		rows = append(rows, lvsRowOfKind(name, "data", "33554432", "", "", "pool", "public"))
	}
	storageStubFile(t, "report-lvs", lvmReport("lv", true, rows...))
	storageTaskWaitsForAnEarlierTask(t, "lvm_logical_volume", deleted("pool"),
		"lvm_logical_volume: other logical volumes depend on the logical volume data/pool, and LVM deletes them together with it: a (a volume in this pool), b (a volume in this pool), "+
			"c (a volume in this pool), d (a volume in this pool), e (a volume in this pool), and 2 more"+deleteThemFirst)
}

// A volume group that an earlier node task of the run creates.
func TestLVMLogicalVolumeWithoutItsVolumeGroup(t *testing.T) {
	t.Parallel()
	calls := newLVMVolumeGroup(t, true)
	inputs := map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "nowhere", "size": "50GiB"}
	const reason = "lvm_logical_volume: the volume group nowhere does not exist; create it in an earlier node task with lvm_volume_group"
	storageResultIs(t, runOfficialExtraModule(t, "lvm_logical_volume", inputs, true), "not_checked", "unknown", reason)
	run := runOfficialExtraModule(t, "lvm_logical_volume", inputs, false)
	storageResultIs(t, run, "failed", "unchanged", reason)
	if run.FailureKind != "missing_prerequisite" || changingStorageCalls(calls()) != nil {
		t.Fatalf("failure kind %q, changing calls %q", run.FailureKind, changingStorageCalls(calls()))
	}
}

func TestLVMLogicalVolumeRejectsInputs(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, lvmTools)
	const nameRule = "1 to 127 letters, digits, +, _, ., and -, not starting with - and not . or .."
	const sizeRule = "a whole number and MiB, GiB, or TiB, such as 50GiB, or all_free_space_when_created"
	ownOption := func(index int, option string) string {
		return "lvcreate_options[" + strconv.Itoa(index) + "]: the module writes lvcreate's option " + option + " itself, from the task's name and size; leave it out"
	}
	for name, c := range map[string]struct {
		inputs map[string]any
		reason string
	}{
		"a name with a slash":               {map[string]any{"grow_filesystem_with_volume": true, "name": "data/postgresql", "volume_group": "data", "size": "1GiB"}, "name: expected the logical volume's name: " + nameRule},
		"a volume group that starts with -": {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "-data", "size": "1GiB"}, "volume_group: expected the volume group's name: " + nameRule},
		"an empty name":                     {map[string]any{"grow_filesystem_with_volume": true, "name": "", "volume_group": "data", "size": "1GiB"}, "name: must not be empty"},
		"an empty volume group":             {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "", "size": "1GiB"}, "volume_group: must not be empty"},
		"deleted with a size":               {map[string]any{"name": "postgresql", "volume_group": "data", "state": "deleted_with_all_its_data", "size": "1GiB"}, "state: deleted_with_all_its_data accepts only name and volume_group; remove size"},
		"deleted with the filesystem input": {map[string]any{"name": "postgresql", "volume_group": "data", "state": "deleted_with_all_its_data", "grow_filesystem_with_volume": false}, "state: deleted_with_all_its_data accepts only name and volume_group; remove grow_filesystem_with_volume"},
		"deleted with options of lvcreate":  {map[string]any{"name": "postgresql", "volume_group": "data", "state": "deleted_with_all_its_data", "lvcreate_options": []any{"--type", "raid1"}}, "state: deleted_with_all_its_data accepts only name and volume_group; remove lvcreate_options"},
		"present without a size":            {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data"}, "size: is required for state present: " + sizeRule},
		"a size without a unit":             {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50"}, "size: expected " + sizeRule},
		"a size with a short unit":          {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "50G"}, "size: expected " + sizeRule},
		"a size with a fraction":            {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1.5GiB"}, "size: expected " + sizeRule},
		"a size of zero":                    {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "0MiB"}, "size: expected " + sizeRule},
		"the rest of the disk":              {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "rest_of_disk"}, "size: expected " + sizeRule},
		"an empty option":                   {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--type", ""}}, "lvcreate_options[1]: must not be empty"},
		"the option --size":                 {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--size", "2GiB"}}, ownOption(0, "--size")},
		"the option --size=":                {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--size=2GiB"}}, ownOption(0, "--size")},
		"the option -L with its value":      {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"-L2G"}}, ownOption(0, "-L")},
		"the option --extents":              {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--type", "raid1", "--extents", "100%VG"}}, ownOption(2, "--extents")},
		"the option -l":                     {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"-l", "10"}}, ownOption(0, "-l")},
		"the option --name":                 {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--name", "other"}}, ownOption(0, "--name")},
		"the option -n":                     {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"-n", "other"}}, ownOption(0, "-n")},
		"the option --wipesignatures":       {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--wipesignatures", "n"}}, ownOption(0, "--wipesignatures")},
		"the option --yes":                  {map[string]any{"grow_filesystem_with_volume": true, "name": "postgresql", "volume_group": "data", "size": "1GiB", "lvcreate_options": []any{"--yes"}}, ownOption(0, "--yes")},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "lvm_logical_volume", c.inputs, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "lvm_logical_volume."+c.reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	if r := runOfficialExtraModule(t, "lvm_logical_volume", map[string]any{"name": "postgresql", "volume_group": "data", "state": "absent"}, true); r.FailureKind != "config" || !strings.Contains(r.Reason, "expected one of present, deleted_with_all_its_data") {
		t.Errorf("an unknown state: %+v", r)
	}
	if got := calls(); got != nil {
		t.Fatalf("rejected inputs ran %q", got)
	}
}

// --- zfs_dataset ---

// zfsTools are the stand-ins of zfs_dataset. dataset-<name> holds the type
// of a dataset that exists, with _ for each / of the name; pool-<name> marks
// a pool. properties-<dataset>/<property> holds a property's value as zfs
// get prints it with -p, then without -p, then its source, with tabs
// between them. A property without a file is one that ZFS does not know,
// unless it has a colon in its name: that is a property of the user's own,
// which was never set. zfs set and zfs create store a value as written,
// unless stores-<property> says in which form ZFS keeps it.
// kernel-module-missing holds what zfs prints while it cannot load its
// kernel module.
var zfsTools = map[string]string{
	"zpool": `for last; do :; done
if [ -e "$STUBS/pool-$last" ]; then echo "$last"; exit 0; fi
echo "cannot open '$last': no such pool" >&2
exit 1`,
	"zfs": `for last; do :; done
key() { printf '%s' "$1" | tr '/' '_'; }
store() {
  mkdir -p "$STUBS/properties-$(key "$1")"
  if [ -e "$STUBS/stores-${2%%=*}" ]; then cat "$STUBS/stores-${2%%=*}" > "$STUBS/properties-$(key "$1")/${2%%=*}"
  else printf '%s\t%s\tlocal\n' "${2#*=}" "${2#*=}" > "$STUBS/properties-$(key "$1")/${2%%=*}"; fi
}
refuse() {
  if [ -e "$STUBS/$1" ]; then cat "$STUBS/$1" >&2; exit "$(cat "$STUBS/$1-exit-code" 2>/dev/null || echo 1)"; fi
}
if [ -e "$STUBS/kernel-module-missing" ]; then cat "$STUBS/kernel-module-missing" >&2; exit 1; fi
case "$1" in
list)
  case " $* " in *" -r "*) cat "$STUBS/below-$(key "$last")"; exit 0;; esac
  if [ -e "$STUBS/dataset-$(key "$last")" ]; then printf '%s\t%s\n' "$last" "$(cat "$STUBS/dataset-$(key "$last")")"; exit 0; fi
  echo "cannot open '$last': dataset does not exist" >&2
  exit 1;;
get)
  column=2
  names=
  shift
  while [ $# -gt 1 ]; do
    case "$1" in -p) column=1;; -H) ;; -o) shift;; *) names="$1";; esac
    shift
  done
  if [ -e "$STUBS/get-prints-$column" ]; then cat "$STUBS/get-prints-$column"; exit 0; fi
  directory="$STUBS/properties-$(key "$last")"
  # A property's second name, such as compress: zfs get prints its long one.
  long() { if [ -e "$STUBS/long-name-of-$1" ]; then cat "$STUBS/long-name-of-$1"; else printf '%s' "$1"; fi; }
  for name in $(printf '%s' "$names" | tr ',' ' '); do
    name="$(long "$name")"
    case "$name" in *:*) ;; *)
      if [ ! -e "$directory/$name" ]; then
        printf '%s\n' "bad property list: invalid property '$name'" "usage:" "	get [-rHp] [-d max] [-o \"all\" | field[,...]]" "	    [-t type[,...]] [-s source[,...]]" >&2
        exit 2
      fi;;
    esac
  done
  for name in $(printf '%s' "$names" | tr ',' ' '); do
    name="$(long "$name")"
    if [ -e "$directory/$name" ]; then printf '%s\t%s\t%s\n' "$name" "$(cut -f "$column" "$directory/$name")" "$(cut -f 3 "$directory/$name")"
    else printf '%s\t-\t-\n' "$name"; fi
  done;;
set)
  refuse set-refuses
  shift
  while [ $# -gt 1 ]; do store "$last" "$1"; shift; done
  if [ -e "$STUBS/set-warns" ]; then cat "$STUBS/set-warns" >&2; fi;;
create)
  refuse create-refuses
  echo filesystem > "$STUBS/dataset-$(key "$last")"
  shift
  while [ $# -gt 1 ]; do case "$1" in -o) ;; *) store "$last" "$1";; esac; shift; done
  if [ -d "$STUBS/after-create" ]; then cp "$STUBS"/after-create/* "$STUBS"/; fi
  refuse create-fails-after-creating;;
destroy)
  refuse destroy-refuses
  rm "$STUBS/dataset-$(key "$last")";;
esac`,
}

const (
	zfsListType  = "zfs list -H -p -o name,type "
	zfsGetExact  = "zfs get -H -p -o property,value,source "
	zfsGetShown  = "zfs get -H -o property,value,source "
	zfsListBelow = "zfs list -H -p -o name,type -r -t filesystem,volume,snapshot "
	// What ZFS 2.2 and 2.3 print when they created a filesystem with
	// sharenfs and the node has no NFS server; they exit 1. ZFS 2.4 prints
	// the same and exits 0.
	zfsCreatedButNotShared = "cannot share 'tank/clickhouse: system error': NFS share creation failed\nfilesystem successfully created, but not shared\n"
)

// zfsProperty says what zfs get prints for one property of a dataset.
func zfsProperty(t *testing.T, dataset, name, exact, shown, source string) {
	t.Helper()
	storageStubFile(t, "properties-"+strings.ReplaceAll(dataset, "/", "_")+"/"+name, exact+"\t"+shown+"\t"+source+"\n")
}

// newZFSNode is a node with the pool tank, whose own dataset has the
// properties of a new pool.
func newZFSNode(t *testing.T) (calls func() []string) {
	t.Helper()
	calls = stubTools(t, zfsTools)
	storageStubFile(t, "pool-tank", "")
	storageStubFile(t, "dataset-tank", "filesystem\n")
	for _, dataset := range []string{"tank", "tank/clickhouse"} {
		zfsProperty(t, dataset, "compression", "on", "on", "default")
		zfsProperty(t, dataset, "recordsize", "131072", "128K", "default")
		zfsProperty(t, dataset, "quota", "0", "none", "default")
		zfsProperty(t, dataset, "atime", "on", "on", "default")
		zfsProperty(t, dataset, "acltype", "off", "off", "default")
		zfsProperty(t, dataset, "sharenfs", "off", "off", "default")
	}
	return calls
}

func TestZFSDatasetIsCreatedOnce(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	// ZFS keeps a size as a number of bytes.
	storageStubFile(t, "stores-recordsize", "131072\t128K\tlocal\n")
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"recordsize": "128K", "compression": "lz4", "com.example:owner": "team storage"}}
	const names = "com.example:owner,compression,recordsize"
	const described = "the dataset tank/clickhouse with com.example:owner=team storage, compression=lz4, recordsize=128K"
	reads := []string{zfsListType + "tank/clickhouse", "zpool list -H -p -o name tank", zfsListType + "tank", zfsGetExact + names + " tank"}

	dryrun := runOfficialExtraModule(t, "zfs_dataset", inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would create "+described)
	if got := calls(); !reflect.DeepEqual(got, reads) || !reflect.DeepEqual(dryrun.ModuleOutputs, map[string]any{"created": true}) {
		t.Fatalf("dryrun calls:\n%s\noutputs %v", strings.Join(got, "\n"), dryrun.ModuleOutputs)
	}
	run := runOfficialExtraModule(t, "zfs_dataset", inputs, false)
	storageResultIs(t, run, "succeeded", "changed", "created "+described)
	want := append(append([]string{}, reads...), "zfs create -o com.example:owner=team storage -o compression=lz4 -o recordsize=128K tank/clickhouse",
		zfsGetExact+names+" tank/clickhouse", zfsGetShown+names+" tank/clickhouse")
	if got := calls()[len(reads):]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(run.ModuleOutputs, map[string]any{"created": true}) || run.Fingerprint == "" || run.Fingerprint != dryrun.Fingerprint {
		t.Fatalf("run calls:\n%s\noutputs %v", strings.Join(got, "\n"), run.ModuleOutputs)
	}
	before := len(calls())
	for _, dryrun := range []bool{false, true} {
		again := runOfficialExtraModule(t, "zfs_dataset", inputs, dryrun)
		storageResultIs(t, again, "succeeded", "unchanged", "")
		if !reflect.DeepEqual(again.ModuleOutputs, map[string]any{"created": false}) || again.Fingerprint != run.Fingerprint {
			t.Fatalf("unchanged outputs %v", again.ModuleOutputs)
		}
	}
	unchanged := []string{zfsListType + "tank/clickhouse", zfsGetExact + names + " tank/clickhouse", zfsGetShown + names + " tank/clickhouse"}
	if got := calls()[before:]; !reflect.DeepEqual(got, append(append([]string{}, unchanged...), unchanged...)) {
		t.Fatalf("unchanged calls:\n%s", strings.Join(got, "\n"))
	}
	// A dataset without listed properties is read once and never set.
	before = len(calls())
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse"}, false), "succeeded", "unchanged", "")
	if got := calls()[before:]; !reflect.DeepEqual(got, []string{zfsListType + "tank/clickhouse"}) {
		t.Fatalf("calls without properties %q", got)
	}
}

// A value is in effect when it equals what zfs get prints with -p or without
// it, or when both are sizes of the same number of bytes.
func TestZFSDatasetTakesAValueInAnyOfItsFormsForUnchanged(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	zfsProperty(t, "tank/clickhouse", "quota", "1610612736", "1.50G", "local")
	zfsProperty(t, "tank/clickhouse", "refquota", "0", "none", "default")
	zfsProperty(t, "tank/clickhouse", "compression", "lz4", "lz4", "inherited from tank")
	zfsProperty(t, "tank/clickhouse", "com.example:owner", "1K", "1K", "local")
	for _, properties := range []map[string]any{
		{"recordsize": "128K"}, {"recordsize": "128k"}, {"recordsize": "131072"}, {"recordsize": "128KiB"}, {"recordsize": "128KB"}, {"recordsize": "0.125M"},
		{"quota": "1.5G"}, {"quota": "1.50G"}, {"quota": "1536M"}, {"quota": "1610612736"}, {"quota": "1536MiB"}, {"quota": "1572864k"},
		{"refquota": "none"}, {"refquota": "0"}, {"compression": "lz4"}, {"atime": "on"}, {"com.example:owner": "1K"},
		{"recordsize": "128K", "quota": "1.5G", "compression": "lz4", "com.example:owner": "1K"},
	} {
		r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": properties}, false)
		if r.Status != "succeeded" || r.Change != "unchanged" {
			t.Errorf("%v: %+v", properties, r)
		}
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("unchanged tasks ran %q", changing)
	}
	// A property of the user's own is text: the same number of bytes in
	// another form is another value.
	before := len(calls())
	r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"com.example:owner": "1024"}}, false)
	storageResultIs(t, r, "succeeded", "changed", "set com.example:owner of the dataset tank/clickhouse from 1K to 1024")
	if got := changingStorageCalls(calls()[before:]); !reflect.DeepEqual(got, []string{"zfs set com.example:owner=1024 tank/clickhouse"}) {
		t.Fatalf("changing calls %q", got)
	}
}

func TestZFSDatasetSetsOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	storageStubFile(t, "stores-quota", "10737418240\t10G\tlocal\n")
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"recordsize": "128K", "compression": "lz4", "quota": "10G", "com.example:owner": "team storage"}}
	const names = "com.example:owner,compression,quota,recordsize"
	const changes = "set com.example:owner of the dataset tank/clickhouse to team storage; " +
		"%[1]s compression of the dataset tank/clickhouse from on to lz4; %[1]s quota of the dataset tank/clickhouse from none to 10G"
	dryrun := runOfficialExtraModule(t, "zfs_dataset", inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would "+strings.ReplaceAll(changes, "%[1]s", "would set"))
	reads := []string{zfsListType + "tank/clickhouse", zfsGetExact + names + " tank/clickhouse", zfsGetShown + names + " tank/clickhouse"}
	if got := calls(); !reflect.DeepEqual(got, reads) {
		t.Fatalf("dryrun calls:\n%s", strings.Join(got, "\n"))
	}
	run := runOfficialExtraModule(t, "zfs_dataset", inputs, false)
	storageResultIs(t, run, "succeeded", "changed", strings.ReplaceAll(changes, "%[1]s", "set"))
	// One zfs set for the three that differ, then the two reads of those three.
	const differing = "com.example:owner,compression,quota"
	want := append(append([]string{}, reads...), "zfs set com.example:owner=team storage compression=lz4 quota=10G tank/clickhouse",
		zfsGetExact+differing+" tank/clickhouse", zfsGetShown+differing+" tank/clickhouse")
	if got := calls()[len(reads):]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(run.ModuleOutputs, map[string]any{"created": false}) {
		t.Fatalf("run calls:\n%s", strings.Join(got, "\n"))
	}
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "unchanged", "")
}

// ZFS stores some values in another form, and the form differs between ZFS
// versions. The task fails once and names the stored form; with that form
// it is unchanged.
func TestZFSDatasetNamesAValueThatZFSStoresInAnotherForm(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	storageStubFile(t, "stores-acltype", "posix\tposix\tlocal\n")
	storageStubFile(t, "stores-compression", "gzip\tgzip\tlocal\n")
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"acltype": "posixacl", "atime": "off"}}
	// The module learns the stored form only after the change. The next try
	// would set the property again, so the task asks for no other try, and
	// the result keeps its change and its output.
	set := runOfficialExtraModule(t, "zfs_dataset", inputs, false)
	storageTaskFailedOnce(t, set,
		`zfs_dataset: ZFS took the value posixacl for the property acltype of the dataset tank/clickhouse and stores it as posix. Write acltype: "posix" in the task; as written, the task would set the property again on every run`)
	if got := changingStorageCalls(calls()); !reflect.DeepEqual(got, []string{"zfs set acltype=posixacl atime=off tank/clickhouse"}) || !reflect.DeepEqual(set.ModuleOutputs, map[string]any{"created": false}) {
		t.Fatalf("changing calls %q, outputs %v", got, set.ModuleOutputs)
	}
	inputs["properties"] = map[string]any{"acltype": "posix", "atime": "off"}
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "unchanged", "")

	// The same after a creation: the dataset exists, the reason says so, and
	// the result says that the task created it.
	created := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/logs", "properties": map[string]any{"compression": "gzip-6"}}, false)
	storageTaskFailedOnce(t, created,
		`zfs_dataset: ZFS took the value gzip-6 for the property compression of the dataset tank/logs and stores it as gzip. Write compression: "gzip" in the task; as written, the task would set the property again on every run. The dataset tank/logs was created`)
	if !reflect.DeepEqual(created.ModuleOutputs, map[string]any{"created": true}) {
		t.Fatalf("outputs %v", created.ModuleOutputs)
	}
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/logs", "properties": map[string]any{"compression": "gzip"}}, false), "succeeded", "unchanged", "")
}

// ZFS prints no line at all when it does not know one of the names, so the
// module names the property, before anything changes and in a dryrun too.
// ZFS takes some properties by a second name, such as compress for
// compression, and zfs get then prints the long name (ZFS 2.1.5 and 2.2.2 on
// the test nodes). The task must write the long name: the module refuses
// the second one before anything changes, for an existing dataset and, asked
// of the parent, for a new one.
func TestZFSDatasetNamesThePropertyThatASecondNameStandsFor(t *testing.T) {
	t.Parallel()
	const reason = "zfs_dataset.properties: compress is a second name of the ZFS property compression; write compression in the task, which is the name that zfs get prints"
	calls := newZFSNode(t)
	storageStubFile(t, "long-name-of-compress", "compression")
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"atime": "off", "compress": "lz4"}}
	for _, exists := range []bool{false, true} {
		if exists {
			storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
		}
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "zfs_dataset", inputs, dryrun)
			storageResultIs(t, r, "failed", "unchanged", reason)
			if r.FailureKind != "config" {
				t.Fatalf("failure kind %q", r.FailureKind)
			}
		}
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
}

func TestZFSDatasetNamesAnUnknownProperty(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	for _, name := range []string{"tank/clickhouse", "tank/new"} {
		inputs := map[string]any{"name": name, "properties": map[string]any{"compression": "lz4", "nosuchproperty": "1", "recordsize": "1M"}}
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "zfs_dataset", inputs, dryrun)
			dataset := "tank/clickhouse"
			if name == "tank/new" {
				// A new dataset has no properties yet: its parent is asked.
				dataset = "tank"
			}
			storageTaskIsRefused(t, r, "unchanged", "zfs_dataset.properties: ZFS on this node knows no property nosuchproperty; its command zfs get all "+dataset+
				" lists the properties of the dataset, and a property of your own needs a colon in its name, such as com.example:owner")
		}
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
}

// zfs create can fail after it created the dataset, and can succeed with a
// message; a refused value fails zfs set with exit code 1 on ZFS 2.1 and
// 255 from ZFS 2.2.
func TestZFSDatasetReportsWhatZFSSays(t *testing.T) {
	t.Parallel()
	const said = "cannot share 'tank/clickhouse: system error': NFS share creation failed filesystem successfully created, but not shared"
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"sharenfs": "on"}}
	t.Run("ZFS 2.2 and 2.3 exit 1 after they created the dataset", func(t *testing.T) {
		newZFSNode(t)
		storageStubFile(t, "create-fails-after-creating", zfsCreatedButNotShared)
		// Another try would find the dataset and report success, so the
		// script asks for no other try; the result stays a failure and
		// says that the task created the dataset.
		r := runOfficialExtraModule(t, "zfs_dataset", inputs, false)
		storageTaskFailedOnce(t, r, "zfs_dataset: ZFS created the dataset tank/clickhouse and then failed: "+said+". The dataset exists now, and the next run compares its properties")
		if !reflect.DeepEqual(r.ModuleOutputs, map[string]any{"created": true}) {
			t.Fatalf("outputs %v", r.ModuleOutputs)
		}
		// The next run finds the dataset with its property.
		removeStorageStubFile(t, "create-fails-after-creating")
		storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "unchanged", "")
	})
	t.Run("ZFS 2.4 exits 0 with the same message", func(t *testing.T) {
		newZFSNode(t)
		storageStubFile(t, "create-fails-after-creating", zfsCreatedButNotShared)
		storageStubFile(t, "create-fails-after-creating-exit-code", "0\n")
		storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "changed", "created the dataset tank/clickhouse with sharenfs=on; ZFS warned: "+said)
	})
	// The dataset exists, and the read that follows fails. Another try
	// would find the dataset and report created: false.
	t.Run("a read that fails right after the creation keeps created: true", func(t *testing.T) {
		newZFSNode(t)
		storageStubFile(t, "after-create/get-prints-1", "compression\n")
		r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"compression": "lz4"}}, false)
		storageTaskFailedOnce(t, r, "zfs_dataset: zfs get printed what MMX cannot read for the property compression of the dataset tank/clickhouse: compression. The dataset tank/clickhouse was created")
		if !reflect.DeepEqual(r.ModuleOutputs, map[string]any{"created": true}) {
			t.Fatalf("outputs %v", r.ModuleOutputs)
		}
	})
	t.Run("zfs create refuses a value and creates nothing", func(t *testing.T) {
		calls := newZFSNode(t)
		storageStubFile(t, "create-refuses", "cannot create 'tank/clickhouse': 'recordsize' must be power of 2 from 512B to 1M\n")
		r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"recordsize": "3K"}}, false)
		storageTaskFailed(t, r, "changed", "zfs_dataset: creating the dataset tank/clickhouse with recordsize=3K: cannot create 'tank/clickhouse': 'recordsize' must be power of 2 from 512B to 1M")
		if got := calls(); got[len(got)-1] != zfsListType+"tank/clickhouse" {
			t.Fatalf("the module did not look again after the failed zfs create: %q", got)
		}
	})
	for name, exitCode := range map[string]string{"ZFS 2.1 refuses a value with exit code 1": "1", "ZFS 2.2 and newer with exit code 255": "255"} {
		t.Run(name, func(t *testing.T) {
			newZFSNode(t)
			storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
			storageStubFile(t, "set-refuses", "cannot set property for 'tank/clickhouse': 'casesensitivity' is readonly\n")
			storageStubFile(t, "set-refuses-exit-code", exitCode+"\n")
			zfsProperty(t, "tank/clickhouse", "casesensitivity", "sensitive", "sensitive", "-")
			r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"casesensitivity": "insensitive"}}, false)
			storageTaskFailed(t, r, "changed", "zfs_dataset: setting casesensitivity=insensitive on the dataset tank/clickhouse: cannot set property for 'tank/clickhouse': 'casesensitivity' is readonly")
		})
	}
	t.Run("zfs set stores a value and cannot share the filesystem", func(t *testing.T) {
		newZFSNode(t)
		storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
		storageStubFile(t, "set-warns", "cannot share 'tank/clickhouse: system error': NFS share creation failed\nproperty may be set but unable to reshare filesystem\n")
		storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "changed",
			"set sharenfs of the dataset tank/clickhouse from off to on; ZFS warned: cannot share 'tank/clickhouse: system error': NFS share creation failed property may be set but unable to reshare filesystem")
	})
}

// What an earlier node task may provide: the pool, the parent dataset, and
// ZFS's kernel module.
func TestZFSDatasetWithoutItsPoolItsParentOrTheKernelModule(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	for name, c := range map[string]struct{ dataset, reason string }{
		"no pool":   {"nopool/clickhouse", "zfs_dataset: the ZFS pool nopool does not exist; create it in an earlier node task, such as a shell task that runs zpool create behind a guard"},
		"no parent": {"tank/missing/child", "zfs_dataset: the parent dataset tank/missing does not exist; create it in an earlier node task with zfs_dataset"},
	} {
		inputs := map[string]any{"name": c.dataset, "properties": map[string]any{"compression": "lz4"}}
		storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, true), "not_checked", "unknown", c.reason)
		run := runOfficialExtraModule(t, "zfs_dataset", inputs, false)
		storageResultIs(t, run, "failed", "unchanged", c.reason)
		if run.FailureKind != "missing_prerequisite" {
			t.Fatalf("%s: failure kind %q", name, run.FailureKind)
		}
	}
	// A parent that is a volume holds no dataset.
	storageStubFile(t, "dataset-tank_volume", "volume\n")
	storageTaskIsRefused(t, runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/volume/child"}, true), "unchanged",
		"zfs_dataset: the parent tank/volume is a volume; a dataset is created below a filesystem")
	// The programs are installed, and the kernel refuses or lacks the module:
	// a dryrun cannot check, and a run fails with ZFS's message, which ZFS
	// 2.1.5, 2.2.2, and 2.2.11 and newer each word differently.
	for _, said := range []string{
		"The ZFS modules are not loaded.\nTry running '/sbin/modprobe zfs' as root to load them.\n",
		"Failed to initialize the libzfs library.\n",
		"The ZFS modules cannot be auto-loaded.\nTry running 'modprobe zfs' as root to manually load them.\n",
	} {
		storageStubFile(t, "kernel-module-missing", said)
		notLoaded := "zfs_dataset: ZFS's kernel module is not loaded on this node, and the zfs program could not load it: " + strings.ReplaceAll(strings.TrimSpace(said), "\n", " ")
		for _, inputs := range []map[string]any{{"name": "tank/clickhouse"}, {"name": "tank/clickhouse", "state": "deleted_with_all_its_data"}} {
			storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, true), "not_checked", "unknown", notLoaded)
			storageTaskFailed(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "unchanged", notLoaded)
		}
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
}

// The module manages filesystems, and reads a value that someone else set.
func TestZFSDatasetLeavesOtherKindsAloneAndReadsOddValues(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	storageStubFile(t, "dataset-tank_volume", "volume\n")
	for _, inputs := range []map[string]any{
		{"name": "tank/volume", "properties": map[string]any{"compression": "lz4"}},
		{"name": "tank/volume", "state": "deleted_with_all_its_data"},
	} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, "zfs_dataset", inputs, dryrun), "unchanged",
				"zfs_dataset: tank/volume is a volume; the module manages filesystem datasets and leaves this one alone")
		}
	}
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
	// A value with a tab: the value is everything between the first and
	// the last tab of its line.
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	storageStubFile(t, "get-prints-1", "com.example:note\ta\tb\tlocal\ncompression\ton\tdefault\n")
	storageStubFile(t, "get-prints-2", "com.example:note\ta\tb\tlocal\ncompression\ton\tdefault\n")
	inputs := map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"com.example:note": "plain", "compression": "on"}}
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, true), "succeeded", "would_change", "would set com.example:note of the dataset tank/clickhouse from a\tb to plain")
	// A value with a line break gives more lines than properties.
	storageStubFile(t, "get-prints-1", "com.example:note\tfirst line\nsecond line\tlocal\ncompression\ton\tdefault\n")
	storageTaskIsRefused(t, runOfficialExtraModule(t, "zfs_dataset", inputs, true), "unchanged",
		"zfs_dataset: zfs get printed 3 lines for the 2 properties com.example:note, compression of the dataset tank/clickhouse. A value that holds a line break cannot be read: set that property by hand to a value without one")
}

func TestZFSDatasetIsDestroyedWithAllItsData(t *testing.T) {
	t.Parallel()
	calls := newZFSNode(t)
	inputs := map[string]any{"name": "tank/clickhouse", "state": "deleted_with_all_its_data"}
	// A dataset that does not exist is unchanged, also in a pool that does not exist.
	for _, name := range []string{"tank/clickhouse", "nopool/clickhouse"} {
		missing := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": name, "state": "deleted_with_all_its_data"}, false)
		storageResultIs(t, missing, "succeeded", "unchanged", "")
		if !reflect.DeepEqual(missing.ModuleOutputs, map[string]any{"created": false}) {
			t.Fatalf("outputs %v", missing.ModuleOutputs)
		}
	}
	if got := calls(); !reflect.DeepEqual(got, []string{zfsListType + "tank/clickhouse", zfsListType + "nopool/clickhouse"}) {
		t.Fatalf("calls %q", got)
	}
	// Child datasets and snapshots stay, and so does the dataset. An earlier
	// node task of the same playbook may delete them, so a dryrun does not
	// check the task, and a run fails once.
	const deleteThemFirst = ". ZFS destroys a dataset only without them: delete each of them first, a child dataset with its own zfs_dataset task and a snapshot with zfs destroy"
	storageStubFile(t, "dataset-tank_clickhouse", "filesystem\n")
	storageStubFile(t, "below-tank_clickhouse", "tank/clickhouse\tfilesystem\ntank/clickhouse/logs\tfilesystem\ntank/clickhouse/logs/old\tvolume\ntank/clickhouse@one\tsnapshot\n")
	storageTaskWaitsForAnEarlierTask(t, "zfs_dataset", inputs,
		"zfs_dataset: the dataset tank/clickhouse has child datasets or snapshots, so it stays: tank/clickhouse/logs (filesystem), tank/clickhouse/logs/old (volume), tank/clickhouse@one (snapshot)"+deleteThemFirst)
	// The reason names the first five and counts the rest.
	below := "tank/clickhouse\tfilesystem\n"
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		below += "tank/clickhouse/" + name + "\tfilesystem\n"
	}
	storageStubFile(t, "below-tank_clickhouse", below+"tank/clickhouse@one\tsnapshot\n")
	storageTaskWaitsForAnEarlierTask(t, "zfs_dataset", inputs,
		"zfs_dataset: the dataset tank/clickhouse has child datasets or snapshots, so it stays: tank/clickhouse/a (filesystem), tank/clickhouse/b (filesystem), tank/clickhouse/c (filesystem), "+
			"tank/clickhouse/d (filesystem), tank/clickhouse/e (filesystem), and 3 more"+deleteThemFirst)
	if changing := changingStorageCalls(calls()); changing != nil {
		t.Fatalf("ran %q", changing)
	}
	storageStubFile(t, "below-tank_clickhouse", "tank/clickhouse\tfilesystem\n")
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, true), "succeeded", "would_change", "would delete the dataset tank/clickhouse with all its data")
	// A dataset in use: ZFS's own message, and nothing is destroyed.
	storageStubFile(t, "destroy-refuses", "cannot unmount '/tank/clickhouse': pool or dataset is busy\n")
	storageTaskFailed(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "changed", "zfs_dataset: deleting the dataset tank/clickhouse: cannot unmount '/tank/clickhouse': pool or dataset is busy")
	removeStorageStubFile(t, "destroy-refuses")
	before := len(calls())
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "changed", "deleted the dataset tank/clickhouse with all its data")
	if got := calls()[before:]; !reflect.DeepEqual(got, []string{zfsListType + "tank/clickhouse", zfsListBelow + "tank/clickhouse", "zfs destroy tank/clickhouse"}) {
		t.Fatalf("calls %q", got)
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "zfs destroy ") && call != "zfs destroy tank/clickhouse" {
			t.Fatalf("the module ran %q", call)
		}
	}
	storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", inputs, false), "succeeded", "unchanged", "")
}

func TestZFSDatasetMissingProgramsAndRejectedInputs(t *testing.T) {
	t.Parallel()
	const install = "zfsutils-linux (Debian and Ubuntu) or zfs (the Red Hat family)"
	for _, program := range []string{"zfs", "zpool"} {
		if _, err := os.Stat("/usr/bin/" + program); err == nil {
			// The stand-ins' PATH also holds /usr/bin, where a machine with ZFS has the real program.
			continue
		}
		tools := map[string]string{}
		for name, body := range zfsTools {
			if name != program {
				tools[name] = body
			}
		}
		calls := stubTools(t, tools)
		reason := "zfs_dataset: the program " + program + " is not installed; install the package " + install + " in an earlier node task"
		storageResultIs(t, runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse"}, true), "not_checked", "unknown", reason)
		run := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse"}, false)
		storageResultIs(t, run, "failed", "unchanged", reason)
		if run.FailureKind != "missing_prerequisite" || calls() != nil {
			t.Fatalf("without %s: failure kind %q, calls %q", program, run.FailureKind, calls())
		}
	}

	calls := stubTools(t, zfsTools)
	const nameRule = "name: expected a dataset as ZFS names it: the pool and the datasets below it with / between them, such as tank/clickhouse"
	for name, c := range map[string]struct {
		inputs map[string]any
		reason string
	}{
		"an empty name":                   {map[string]any{"name": ""}, "name: must not be empty"},
		"deleted with properties":         {map[string]any{"name": "tank/clickhouse", "state": "deleted_with_all_its_data", "properties": map[string]any{"compression": "lz4"}}, "state: deleted_with_all_its_data accepts only name; remove properties"},
		"a name that starts with -":       {map[string]any{"name": "-r"}, nameRule},
		"a name that starts with /":       {map[string]any{"name": "/tank/clickhouse"}, nameRule},
		"a name that ends with /":         {map[string]any{"name": "tank/clickhouse/"}, nameRule},
		"an empty part":                   {map[string]any{"name": "tank//clickhouse"}, nameRule},
		"a snapshot":                      {map[string]any{"name": "tank/clickhouse@monday"}, "name: tank/clickhouse@monday names a snapshot; the module manages filesystem datasets, such as tank/clickhouse"},
		"a bookmark":                      {map[string]any{"name": "tank/clickhouse#monday"}, "name: tank/clickhouse#monday names a bookmark; the module manages filesystem datasets, such as tank/clickhouse"},
		"a pool's own dataset to destroy": {map[string]any{"name": "tank", "state": "deleted_with_all_its_data"}, "name: tank is a pool's own dataset, which goes only with its pool; the module deletes datasets below a pool, such as tank/clickhouse"},
		"a property name with =":          {map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"quota=1G": "x"}}, `properties: "quota=1G" is no name of a ZFS property; a name has no space, comma, or equals sign, such as compression or com.example:owner`},
		"a property name with a comma":    {map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"quota,atime": "x"}}, `properties: "quota,atime" is no name of a ZFS property; a name has no space, comma, or equals sign, such as compression or com.example:owner`},
		"an empty property name":          {map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"": "x"}}, `properties: "" is no name of a ZFS property; a name has no space, comma, or equals sign, such as compression or com.example:owner`},
		"a value with a tab":              {map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"com.example:note": "a\tb"}}, "properties.com.example:note: the value holds a tab or a line break, which the module cannot read back from zfs get; write the value without one"},
		"a value with a line break":       {map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"com.example:note": "a\nb"}}, "properties.com.example:note: the value holds a tab or a line break, which the module cannot read back from zfs get; write the value without one"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "zfs_dataset", c.inputs, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "zfs_dataset."+c.reason {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	if r := runOfficialExtraModule(t, "zfs_dataset", map[string]any{"name": "tank/clickhouse", "properties": map[string]any{"quota": 10}}, true); r.FailureKind != "config" || !strings.Contains(r.Reason, "expected a mapping of strings") {
		t.Errorf("a value that is a number: %+v", r)
	}
	if got := calls(); got != nil {
		t.Fatalf("rejected inputs ran %q", got)
	}
}
