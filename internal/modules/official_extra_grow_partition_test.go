package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const growPartitionModule = "grow_partition_into_free_space_after_it"

// Only the CHANGE, CHANGED, and NOCHANGE lines model supplied measurements.
// Error messages and malformed responses are synthetic failure injection.
func stubGrowPartition(t *testing.T) (string, func() []string) {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(disk, nil, 0600); err != nil {
		t.Fatal(err)
	}
	calls := stubTools(t, map[string]string{
		"lsblk": `if [ -f "$STUBS/not-block" ]; then exit 32; fi
if [ -f "$STUBS/read-fails" ]; then echo 'synthetic lsblk failure' >&2; exit 2; fi
printf '{"blockdevices":[{"path":"/dev/loop3","kname":"loop3","pkname":null,"type":"%s","size":629145600,"log-sec":512}]}' "$(cat "$STUBS/device-type" 2>/dev/null || echo loop)"`,
		"wipefs": `if [ -f "$STUBS/no-table" ]; then echo '{"signatures":[]}'; else echo '{"signatures":[{"type":"gpt","usage":"partition-table"}]}'; fi`,
		"sfdisk": `if [ -f "$STUBS/after-read-fails" ] && [ -f "$STUBS/changed" ]; then echo 'synthetic sfdisk failure' >&2; exit 2; fi
if [ -f "$STUBS/no-partition" ]; then echo '{"partitiontable":{"device":"/dev/loop3","partitions":[]}}'; exit; fi
size=405504
if [ -f "$STUBS/changed" ]; then size=1021919; fi
printf '{"partitiontable":{"device":"/dev/loop3","partitions":[{"node":"/dev/loop3p2","start":206848,"size":%s}]}}' "$size"`,
		"blockdev": `if [ "$1" = --getss ]; then echo 512; elif [ -f "$STUBS/stale-kernel" ]; then echo 207618048; else echo 523222528; fi`,
		"growpart": `if [ "$1" = --dry-run ]; then
 if [ -f "$STUBS/unknown" ]; then echo 'synthetic unknown output'; exit; fi
 if [ -f "$STUBS/preview-fails" ]; then echo 'synthetic growpart read failure' >&2; exit 2; fi
 if [ -f "$STUBS/unchanged" ] || [ -f "$STUBS/changed" ]; then echo 'NOCHANGE: partition 2 is size 1021919. it cannot be grown'; exit 1; fi
 echo 'CHANGE: partition=2 start=206848 old: size=405504 end=612351 new: size=1021919 end=1228766'
 echo '# === old sfdisk -d ==='
 exit
fi
if [ -f "$STUBS/fail-before" ]; then echo 'synthetic growpart failure before change' >&2; exit 2; fi
touch "$STUBS/changed"
if [ -f "$STUBS/fail-after" ]; then echo 'synthetic growpart failure after change' >&2; exit 2; fi
if [ -f "$STUBS/unknown-change" ]; then echo 'synthetic unknown change output'; exit; fi
echo 'CHANGED: partition=2 start=206848 old: size=405504 end=612351 new: size=1021919 end=1228766'`,
	})
	return disk, calls
}

func growPartitionInputs(disk string) map[string]any {
	return map[string]any{"device": disk, "partition_number": 2}
}

func TestOfficialExtraGrowPartitionChangesAndDryrun(t *testing.T) {
	t.Parallel()
	for _, dryrun := range []bool{true, false} {
		t.Run(fmt.Sprint(dryrun), func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			result := runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), dryrun)
			verb, change := "grew", "changed"
			if dryrun {
				verb, change = "would grow", "would_change"
			}
			storageResultIs(t, result, "succeeded", change, fmt.Sprintf("%s partition 2 of %s from 198 MiB to 523222528 bytes", verb, disk))
			expected := []string{"lsblk --json --bytes --nodeps -o PATH,KNAME,PKNAME,TYPE,SIZE,LOG-SEC " + disk,
				"wipefs --no-act --json --output TYPE,USAGE " + disk, "sfdisk --json /dev/loop3", "blockdev --getss " + disk, "growpart --dry-run /dev/loop3 2"}
			if !dryrun {
				expected = append(expected, "growpart /dev/loop3 2", "sfdisk --json /dev/loop3", "blockdev --getsize64 /dev/loop3p2")
			}
			if !reflect.DeepEqual(calls(), expected) {
				t.Fatalf("calls %v, want %v", calls(), expected)
			}
			if len(result.ModuleOutputs) != 0 {
				t.Fatalf("unexpected outputs: %v", result.ModuleOutputs)
			}
			if !dryrun {
				storageResultIs(t, runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), false), "succeeded", "unchanged", "")
			}
		})
	}
}

func TestOfficialExtraGrowPartitionNothingToGain(t *testing.T) {
	t.Parallel()
	disk, calls := stubGrowPartition(t)
	storageStubFile(t, "unchanged", "")
	storageResultIs(t, runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), false), "succeeded", "unchanged", "")
	if len(calls()) != 5 || calls()[4] != "growpart --dry-run /dev/loop3 2" {
		t.Fatal(calls())
	}
}

func TestOfficialExtraGrowPartitionPrerequisites(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"growpart", "disk", "no-table", "no-partition"} {
		t.Run(missing, func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			reason := growPartitionModule + ": "
			switch missing {
			case "growpart":
				if _, err := os.Stat("/usr/bin/growpart"); err == nil {
					t.Skip("the stand-ins' PATH also holds /usr/bin, where this machine has the real growpart")
				}
				if err := os.Remove(filepath.Join(storageStubDirectory(t), "growpart")); err != nil {
					t.Fatal(err)
				}
				reason += "the program growpart is not installed; install the package cloud-guest-utils (Debian and Ubuntu) or cloud-utils-growpart (the Red Hat family) in an earlier node task"
			case "disk":
				if err := os.Remove(disk); err != nil {
					t.Fatal(err)
				}
				reason += "the disk " + disk + " does not exist yet"
			case "no-table":
				storageStubFile(t, missing, "")
				reason += "the disk " + disk + " has no partition table; create it in an earlier node task"
			case "no-partition":
				storageStubFile(t, missing, "")
				reason += "the disk " + disk + " has no partition 2; create it in an earlier node task"
			}
			storageTaskWaitsForAnEarlierTask(t, growPartitionModule, growPartitionInputs(disk), reason)
			for _, call := range calls() {
				if strings.HasPrefix(call, "growpart ") {
					t.Fatal(calls())
				}
			}
		})
	}
}

func TestOfficialExtraGrowPartitionFailures(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"fail-before", "fail-after", "unknown", "unknown-change", "read-fails", "preview-fails", "after-read-fails", "stale-kernel"} {
		t.Run(fault, func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			storageStubFile(t, fault, "")
			if fault == "after-read-fails" {
				storageStubFile(t, "fail-after", "")
			}
			result := runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), false)
			if result.Status != "failed" || result.FailureKind != "" || !strings.HasPrefix(result.Reason, growPartitionModule+":") {
				t.Fatalf("%+v", result)
			}
			changed := fault != "unknown" && fault != "read-fails" && fault != "preview-fails"
			expectedChange := "unchanged"
			if changed {
				expectedChange = "changed"
			}
			if result.Change != expectedChange || result.NoOtherTry != (changed && fault != "fail-before") {
				t.Fatalf("%+v", result)
			}
			if fault == "fail-before" && !strings.Contains(result.Reason, "synthetic growpart failure before change. The partition still has 198 MiB") {
				t.Fatal(result.Reason)
			}
			if fault == "fail-after" && !strings.Contains(result.Reason, "synthetic growpart failure after change. The partition now has 523222528 bytes") {
				t.Fatal(result.Reason)
			}
			if strings.HasPrefix(fault, "unknown") && !strings.Contains(result.Reason, "synthetic unknown") {
				t.Fatal(result.Reason)
			}
			if !changed {
				for _, call := range calls() {
					if call == "growpart /dev/loop3 2" {
						t.Fatal(calls())
					}
				}
			}
		})
	}
}

func TestOfficialExtraGrowPartitionInvalidInputs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input   string
		value   any
		message string
	}{
		{"device", "", "device: must not be empty"}, {"device", "relative", "device: expected the absolute path of the whole disk, such as /dev/sdb"},
		{"partition_number", 0, "partition_number: must be 1 or more"}, {"partition_number", -1, "partition_number: must be 1 or more"},
	} {
		t.Run(fmt.Sprint(test.value), func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			inputs := growPartitionInputs(disk)
			inputs[test.input] = test.value
			storageTaskIsRefused(t, runOfficialExtraModule(t, growPartitionModule, inputs, false), "unchanged", growPartitionModule+"."+test.message)
			if len(calls()) != 0 {
				t.Fatal(calls())
			}
		})
	}
	for _, kind := range []string{"not-block", "part", "rom"} {
		t.Run(kind, func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			if kind == "not-block" {
				storageStubFile(t, kind, "")
			} else {
				storageStubFile(t, "device-type", kind)
			}
			result := runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), true)
			if result.FailureKind != "config" || result.Change != "unchanged" || len(calls()) != 1 {
				t.Fatalf("%+v calls %v", result, calls())
			}
		})
	}
}

func TestOfficialExtraGrowPartitionRejectsMalformedReads(t *testing.T) {
	t.Parallel()
	for _, program := range []string{"lsblk", "wipefs", "sfdisk", "blockdev"} {
		t.Run(program, func(t *testing.T) {
			disk, calls := stubGrowPartition(t)
			storageStubFile(t, "bodies/"+program, "echo 'synthetic malformed read'")
			result := runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), true)
			if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, "synthetic malformed read") {
				t.Fatalf("%+v", result)
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "growpart ") {
					t.Fatal(calls())
				}
			}
		})
	}
}

func TestOfficialExtraGrowPartitionUnsupportedSectors(t *testing.T) {
	t.Parallel()
	disk, calls := stubGrowPartition(t)
	storageStubFile(t, "bodies/lsblk", `echo '{"blockdevices":[{"path":"/dev/loop3","kname":"loop3","type":"loop","size":629145600,"log-sec":1024}]}'`)
	reason := fmt.Sprintf("%s: %s has sectors of 1024 bytes; the module knows disks with sectors of 512 and of 4096 bytes", growPartitionModule, disk)
	storageTaskIsRefused(t, runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), true), "unchanged", reason)
	if len(calls()) != 1 {
		t.Fatal(calls())
	}
}

func TestOfficialExtraGrowPartitionReads4096ByteSectors(t *testing.T) {
	t.Parallel()
	disk, _ := stubGrowPartition(t)
	// Synthetic sizes exercise arithmetic; this is not a node measurement.
	storageStubFile(t, "bodies/lsblk", `echo '{"blockdevices":[{"path":"/dev/loop3","kname":"loop3","type":"loop","size":5033164800,"log-sec":4096}]}'`)
	storageStubFile(t, "bodies/blockdev", `if [ "$1" = --getss ]; then echo 4096; else echo 4185780224; fi`)
	storageResultIs(t, runOfficialExtraModule(t, growPartitionModule, growPartitionInputs(disk), false), "succeeded", "changed",
		fmt.Sprintf("grew partition 2 of %s from 1584 MiB to 4087676 KiB", disk))
}
