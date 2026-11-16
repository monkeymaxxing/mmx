package facts

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// bootIDPath holds the kernel's ID of the current boot, the .node.boot_id fact.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// BootID reads the current boot's ID. A job that requested a reboot compares
// it with the boot it left.
func BootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	id := strings.TrimSpace(string(b))
	if err == nil && id == "" {
		err = fmt.Errorf("%s is empty", bootIDPath)
	}
	return id, err
}

func gatherSystem(s *Snapshot, read func(string) ([]byte, error)) {
	for _, field := range []struct{ key, path string }{{"kernel.release", "/proc/sys/kernel/osrelease"}, {"boot_id", bootIDPath}} {
		b, err := read(field.path)
		text := strings.TrimSpace(string(b))
		if err == nil && text == "" {
			err = fmt.Errorf("%s is empty", field.path)
		}
		s.put(field.key, text, err)
	}
	b, err := read("/sys/devices/system/cpu/online")
	count := 0
	if err == nil {
		count, err = cpuCount(string(b))
	}
	s.put("cpu.count", count, err)
	b, err = read("/proc/meminfo")
	for _, field := range []struct{ input, output string }{{"MemTotal", "memory.total_bytes"}, {"SwapTotal", "memory.swap_total_bytes"}} {
		var n int64
		e := err
		if e == nil {
			n, e = memoryBytes(string(b), field.input)
		}
		s.put(field.output, n, e)
		if field.input == "MemTotal" {
			s.put("memory.total_mib", n/(1024*1024), e)
		}
	}
	b, err = read("/etc/os-release")
	if os.IsNotExist(err) {
		b, err = read("/usr/lib/os-release")
	}
	var release map[string]any
	if err == nil {
		release, err = osRelease(string(b))
	}
	s.put("os", release, err)
	gatherDistribution(s, release, err)
	mode, err := selinuxMode(read)
	s.put("selinux.mode", mode, err)
	b, err = read("/proc/self/mountinfo")
	gatherBlockDevices(s, "", read, string(b), err)
	if err != nil {
		s.put("storage.mounts", nil, err)
	} else {
		gatherMounts(s, string(b), mountCapacity)
	}
}

func cpuCount(text string) (int, error) {
	total, last := 0, -1
	for _, part := range strings.Split(strings.TrimSpace(text), ",") {
		a, b, ranged := strings.Cut(part, "-")
		start, err := strconv.Atoi(a)
		end := start
		if ranged {
			var e error
			end, e = strconv.Atoi(b)
			if e != nil {
				err = e
			}
		}
		if err != nil || start <= last || end < start || end > math.MaxInt32 {
			return 0, fmt.Errorf("invalid CPU range in /sys/devices/system/cpu/online")
		}
		total += end - start + 1
		last = end
	}
	return total, nil
}
func memoryBytes(text, key string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != key+":" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("invalid %s in /proc/meminfo", key)
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || n < 0 || n > math.MaxInt64/1024 {
			return 0, fmt.Errorf("invalid %s size in /proc/meminfo", key)
		}
		return n * 1024, nil
	}
	return 0, fmt.Errorf("%s missing from /proc/meminfo", key)
}

// os-release uses shell quoting but has no variable expansion or executable code.
func osRelease(text string) (map[string]any, error) {
	out := map[string]any{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid os-release field: expected NAME=value")
		}
		v, err := releaseValue(raw)
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(key)] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("os-release contains no fields")
	}
	return out, nil
}
func releaseValue(raw string) (string, error) {
	quote := byte(0)
	if len(raw) > 0 && (raw[0] == '\'' || raw[0] == '"') {
		quote = raw[0]
		if len(raw) < 2 || raw[len(raw)-1] != quote {
			return "", fmt.Errorf("missing closing quote in an os-release value")
		}
		raw = raw[1 : len(raw)-1]
	}
	if quote == '\'' {
		return raw, nil
	}
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			if i+1 == len(raw) {
				return "", fmt.Errorf("incomplete escape sequence in an os-release value")
			}
			next := raw[i+1]
			if quote == 0 || strings.ContainsRune("$`\"\\", rune(next)) {
				i++
				out.WriteByte(next)
				continue
			}
		}
		out.WriteByte(raw[i])
	}
	return out.String(), nil
}
