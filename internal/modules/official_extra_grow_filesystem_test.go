package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const growFilesystemModule = "grow_filesystem_to_fill_device"

// Geometry is from the supplied Debian 13 measurements: dumpe2fs 1.47.2
// and xfs_info 6.13.0. State files let a grow succeed, refuse, or stop early.
func growFilesystemNode(t *testing.T, filesystemType string) (string, func() []string) {
	t.Helper()
	device := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	calls := stubTools(t, map[string]string{
		"lsblk":    `echo '{"blockdevices":[{"path":"/dev/loop1","maj:min":"7:1"}]}'`,
		"wipefs":   `cat "$STUBS/signatures"`,
		"findmnt":  `cat "$STUBS/mounts"`,
		"blockdev": `if [ -e "$STUBS/read-fails" ]; then echo 'cannot read device' >&2; exit 1; fi; cat "$STUBS/device-bytes"`,
		"dumpe2fs": `if [ -e "$STUBS/after-read-fails" ] && [ -e "$STUBS/grew" ]; then echo 'cannot read superblock' >&2; exit 1; fi
if [ -e "$STUBS/geometry" ]; then cat "$STUBS/geometry"; else echo "Block count:              $(cat "$STUBS/blocks")"; echo 'Block size:               1024'; fi`,
		"xfs_info": `if [ -e "$STUBS/after-read-fails" ] && [ -e "$STUBS/grew" ]; then echo 'cannot read superblock' >&2; exit 1; fi
if [ -e "$STUBS/geometry" ]; then cat "$STUBS/geometry"; else echo "data     =                       bsize=4096   blocks=$(cat "$STUBS/blocks"), imaxpct=25"; fi`,
		"resize2fs":  growFilesystemTool,
		"xfs_growfs": growFilesystemTool,
	})
	storageStubFile(t, "signatures", `{"signatures":[{"type":"`+filesystemType+`"}]}`)
	storageStubFile(t, "mounts", `{"filesystems":[{"target":"/mnt/data","fstype":"`+filesystemType+`","options":"rw,relatime","maj:min":"7:1"}]}`)
	if filesystemType == "ext4" {
		storageStubFile(t, "blocks", "307200\n")
		storageStubFile(t, "new-blocks", "614400\n")
		storageStubFile(t, "device-bytes", "629145600\n")
	} else {
		storageStubFile(t, "blocks", "102400\n")
		storageStubFile(t, "new-blocks", "204800\n")
		storageStubFile(t, "device-bytes", "838860800\n")
	}
	return device, calls
}

const growFilesystemTool = `
if [ ! -e "$STUBS/no-growth" ]; then cp "$STUBS/new-blocks" "$STUBS/blocks"; touch "$STUBS/grew"; fi
if [ -e "$STUBS/grow-fails" ]; then echo 'growth refused' >&2; exit 1; fi
echo 'filesystem tool version' >&2
echo 'Nothing to do!'
`

func growFilesystemInputs(device, filesystemType string) map[string]any {
	return map[string]any{"device": device, "filesystem_type": filesystemType}
}

func TestOfficialExtraGrowFilesystemGrowthAndDryrun(t *testing.T) {
	t.Parallel()
	for _, filesystemType := range []string{"ext4", "xfs"} {
		t.Run(filesystemType, func(t *testing.T) {
			device, calls := growFilesystemNode(t, filesystemType)
			inputs := growFilesystemInputs(device, filesystemType)
			oldSize, newSize, geometryCall, growCall := "300 MiB", "600 MiB", "dumpe2fs -h "+device, "resize2fs "+device
			if filesystemType == "xfs" {
				oldSize, newSize, geometryCall, growCall = "400 MiB", "800 MiB", "xfs_info /mnt/data", "xfs_growfs /mnt/data"
			}
			what := "the " + filesystemType + " filesystem on " + device + " from " + oldSize + " to " + newSize
			reading := []string{"lsblk --json --nodeps -o PATH,MAJ:MIN " + device, "wipefs --no-act --json " + device, "findmnt --json --list --output TARGET,FSTYPE,OPTIONS,MAJ:MIN", "blockdev --getsize64 " + device, geometryCall}
			preview := runOfficialExtraModule(t, growFilesystemModule, inputs, true)
			storageResultIs(t, preview, "succeeded", "would_change", "would grow "+what)
			if !reflect.DeepEqual(calls(), reading) {
				t.Fatalf("dryrun calls %q", calls())
			}
			before := len(calls())
			result := runOfficialExtraModule(t, growFilesystemModule, inputs, false)
			storageResultIs(t, result, "succeeded", "changed", "grew "+what)
			if !reflect.DeepEqual(calls()[before:], append(append([]string{}, reading...), growCall, geometryCall)) {
				t.Fatalf("growth calls %q", calls()[before:])
			}
			if result.Fingerprint != preview.Fingerprint || len(result.ModuleOutputs) != 0 {
				t.Fatalf("result %+v", result)
			}
			for _, dryrun := range []bool{false, true} {
				before = len(calls())
				storageResultIs(t, runOfficialExtraModule(t, growFilesystemModule, inputs, dryrun), "succeeded", "unchanged", "")
				if !reflect.DeepEqual(calls()[before:], reading) {
					t.Fatalf("unchanged calls %q", calls()[before:])
				}
			}
			// A tail smaller than one block cannot be gained.
			storageStubFile(t, "device-bytes", map[string]string{"ext4": "629146623\n", "xfs": "838864895\n"}[filesystemType])
			storageResultIs(t, runOfficialExtraModule(t, growFilesystemModule, inputs, false), "succeeded", "unchanged", "")
		})
	}
}

func TestOfficialExtraGrowFilesystemPrerequisites(t *testing.T) {
	t.Parallel()
	for _, filesystemType := range []string{"ext4", "xfs"} {
		t.Run(filesystemType, func(t *testing.T) {
			device, calls := growFilesystemNode(t, filesystemType)
			inputs := growFilesystemInputs(device, filesystemType)
			storageStubFile(t, "mounts", `{"filesystems":[]}`)
			storageTaskWaitsForAnEarlierTask(t, growFilesystemModule, inputs, growFilesystemModule+": the "+filesystemType+" filesystem on "+device+" is not mounted; mount it for writing first with fstab_mount")
			storageStubFile(t, "mounts", `{"filesystems":[{"target":"/mnt/data","fstype":"`+filesystemType+`","options":"ro,relatime","maj:min":"7:1"}]}`)
			storageTaskWaitsForAnEarlierTask(t, growFilesystemModule, inputs, growFilesystemModule+": the "+filesystemType+" filesystem on "+device+" is mounted read-only; mount it for writing first with fstab_mount")
			for _, signature := range []string{"swap", ""} {
				signatures, found := `{"signatures":[]}`, "no filesystem signature"
				if signature != "" {
					signatures, found = `{"signatures":[{"type":"swap"}]}`, "swap"
				}
				storageStubFile(t, "signatures", signatures)
				storageTaskWaitsForAnEarlierTask(t, growFilesystemModule, inputs, growFilesystemModule+": the device "+device+" holds "+found+", not the expected "+filesystemType+" filesystem; create the expected filesystem in an earlier node task with create_filesystem_on_empty_device on an empty device")
			}
			storageTaskWaitsForAnEarlierTask(t, growFilesystemModule, growFilesystemInputs(device+"-missing", filesystemType), growFilesystemModule+": the device "+device+"-missing does not exist yet")
			for _, call := range calls() {
				if strings.HasPrefix(call, "resize2fs ") || strings.HasPrefix(call, "xfs_growfs ") {
					t.Fatalf("prerequisite ran %q", call)
				}
			}
		})
	}
}

func TestOfficialExtraGrowFilesystemDeviceLinkAndMultipleMounts(t *testing.T) {
	t.Parallel()
	device, calls := growFilesystemNode(t, "xfs")
	alias := device + "-link"
	if err := os.Symlink(device, alias); err != nil {
		t.Fatal(err)
	}
	// Device numbers match even when the input is a link. The second mount
	// is writable; JSON preserves its spaces without splitting the path.
	storageStubFile(t, "mounts", `{"filesystems":[{"target":"/mnt/read-only","fstype":"xfs","options":"ro","maj:min":"7:1"},{"target":"/mnt/writable data","fstype":"xfs","options":"rw","maj:min":"7:1"}]}`)
	result := runOfficialExtraModule(t, growFilesystemModule, growFilesystemInputs(alias, "xfs"), false)
	storageResultIs(t, result, "succeeded", "changed", "grew the xfs filesystem on "+alias+" from 400 MiB to 800 MiB")
	if got := calls(); got[len(got)-2] != "xfs_growfs /mnt/writable data" || got[len(got)-1] != "xfs_info /mnt/writable data" {
		t.Fatalf("calls %q", got)
	}
}

func TestOfficialExtraGrowFilesystemFailures(t *testing.T) {
	t.Parallel()
	for _, filesystemType := range []string{"ext4", "xfs"} {
		for _, scenario := range []string{"refused", "partial growth", "zero with no growth", "read after growth", "read before growth", "bad geometry"} {
			t.Run(filesystemType+"/"+scenario, func(t *testing.T) {
				device, calls := growFilesystemNode(t, filesystemType)
				inputs := growFilesystemInputs(device, filesystemType)
				switch scenario {
				case "refused":
					storageStubFile(t, "no-growth", "")
					storageStubFile(t, "grow-fails", "")
				case "partial growth":
					storageStubFile(t, "grow-fails", "")
				case "zero with no growth":
					storageStubFile(t, "no-growth", "")
				case "read after growth":
					storageStubFile(t, "after-read-fails", "")
				case "read before growth":
					storageStubFile(t, "read-fails", "")
				case "bad geometry":
					storageStubFile(t, "geometry", "unrecognized geometry\n")
				}
				result := runOfficialExtraModule(t, growFilesystemModule, inputs, false)
				if result.Status != "failed" || !strings.HasPrefix(result.Reason, growFilesystemModule+": ") {
					t.Fatalf("result %+v", result)
				}
				expectedWords := map[string]string{"refused": "growth refused", "partial growth": "growth refused", "zero with no growth": "Nothing to do!", "read after growth": "cannot read superblock", "read before growth": "cannot read device", "bad geometry": "unrecognized geometry"}[scenario]
				if !strings.Contains(result.Reason, expectedWords) {
					t.Fatalf("reason %q", result.Reason)
				}
				changed := scenario != "read before growth" && scenario != "bad geometry"
				if (result.Change == "changed") != changed {
					t.Fatalf("change %+v", result)
				}
				noOtherTry := scenario == "partial growth" || scenario == "read after growth"
				if result.NoOtherTry != noOtherTry {
					t.Fatalf("retry %+v", result)
				}
				if changed && len(calls()) != 7 {
					t.Fatalf("did not read after changing: %q", calls())
				}
			})
		}
	}
}

func TestOfficialExtraGrowFilesystemInvalidInputsAndMissingPrograms(t *testing.T) {
	t.Parallel()
	device, calls := growFilesystemNode(t, "ext4")
	for _, input := range []struct{ device, rule string }{{"", "must not be empty"}, {"relative", "expected the absolute path of a block device"}} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, growFilesystemModule, growFilesystemInputs(input.device, "ext4"), dryrun), "unchanged", growFilesystemModule+".device: "+input.rule)
		}
	}
	if calls() != nil {
		t.Fatalf("invalid inputs ran %q", calls())
	}
	for _, filesystemType := range []string{"ext4", "xfs"} {
		for _, program := range []string{"blockdev", "findmnt", "lsblk", "wipefs", "dumpe2fs", "resize2fs", "xfs_info", "xfs_growfs"} {
			if filesystemType == "ext4" && strings.HasPrefix(program, "xfs_") || filesystemType == "xfs" && (program == "dumpe2fs" || program == "resize2fs") {
				continue
			}
			t.Run(filesystemType+"/missing "+program, func(t *testing.T) {
				_, calls := growFilesystemNode(t, filesystemType)
				if err := os.Remove(filepath.Join(storageStubDirectory(t), program)); err != nil {
					t.Fatal(err)
				}
				// An installed system tool would invalidate this missing-tool test.
				for _, directory := range []string{"/usr/bin", "/bin"} {
					if _, err := os.Stat(filepath.Join(directory, program)); err == nil {
						t.Skip("system has " + program)
					}
				}
				packageName := "util-linux"
				if program == "dumpe2fs" || program == "resize2fs" {
					packageName = "e2fsprogs"
				}
				if strings.HasPrefix(program, "xfs_") {
					packageName = "xfsprogs"
				}
				storageTaskWaitsForAnEarlierTask(t, growFilesystemModule, growFilesystemInputs(device, filesystemType), growFilesystemModule+": the program "+program+" is not installed; install the package "+packageName+" in an earlier node task")
				if calls() != nil {
					t.Fatalf("missing program ran %q", calls())
				}
			})
		}
	}
}

func TestOfficialExtraGrowFilesystemReportsActualGrowth(t *testing.T) {
	t.Parallel()
	device, _ := growFilesystemNode(t, "ext4")
	// A tool may grow less than predicted. The reason must use its new size.
	storageStubFile(t, "new-blocks", "460800\n")
	result := runOfficialExtraModule(t, growFilesystemModule, growFilesystemInputs(device, "ext4"), false)
	storageResultIs(t, result, "succeeded", "changed", "grew the ext4 filesystem on "+device+" from 300 MiB to 450 MiB")
}

func TestOfficialExtraGrowFilesystemRejectsUnreadableReports(t *testing.T) {
	t.Parallel()
	for _, report := range []struct{ file, content, program string }{
		{"signatures", `{"signatures":[{"unknown":"ext4"}]}`, "wipefs"},
		{"mounts", `{"filesystems":[{"target":"/mnt/data","fstype":"ext4","options":null,"maj:min":"7:1"}]}`, "findmnt"},
		{"device-bytes", "not a number\n", "blockdev"},
		{"geometry", "Block count: 307200\nBlock size: 0\n", "dumpe2fs"},
	} {
		t.Run(report.program, func(t *testing.T) {
			device, calls := growFilesystemNode(t, "ext4")
			storageStubFile(t, report.file, report.content)
			for _, dryrun := range []bool{true, false} {
				result := runOfficialExtraModule(t, growFilesystemModule, growFilesystemInputs(device, "ext4"), dryrun)
				if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, report.program+" printed what MMX cannot read") || !strings.Contains(result.Reason, strings.Join(strings.Fields(report.content), " ")) {
					t.Fatalf("result %+v", result)
				}
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "resize2fs ") {
					t.Fatalf("unreadable report ran %q", call)
				}
			}
		})
	}
}
