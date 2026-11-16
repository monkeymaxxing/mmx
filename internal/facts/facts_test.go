package facts

import (
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCapacityUnitsAndInvalidObservations(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int64
		bad  bool
	}{
		{"MemTotal: 2050 kB\nSwapTotal: 0 kB\n", 2050 * 1024, false},
		{"MemTotal: 2 MB", 0, true}, {"MemTotal:", 0, true}, {"MemTotal: -1 kB", 0, true},
		{"MemTotal: 9223372036854775807 kB", 0, true}, {"SwapTotal: 0 kB", 0, true},
	} {
		n, err := memoryBytes(tc.text, "MemTotal")
		if (err != nil) != tc.bad || (!tc.bad && n != tc.want) {
			t.Fatal(tc, n, err)
		}
	}
	if n, err := memoryBytes("SwapTotal: 0 kB", "SwapTotal"); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	for _, tc := range []struct {
		text string
		want int
	}{{"0-3,8,10-12\n", 8}, {"0", 1}, {"", 0}, {"3-1", 0}, {"0-2,2", 0}, {"-1", 0}, {"0-x", 0}} {
		n, err := cpuCount(tc.text)
		if n != tc.want || (err != nil) != (tc.want == 0) {
			t.Fatal(tc, n, err)
		}
	}
}
func TestOSReleaseUsesLiteralShellQuoting(t *testing.T) {
	got, err := osRelease("# comment\nID=debian\nPRETTY_NAME=\"A \\\"quoted\\\" \\$name\"\nVERSION='literal $HOME'\nOTHER=with\\ space\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": "debian", "pretty_name": "A \"quoted\" $name", "version": "literal $HOME", "other": "with space"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for _, bad := range []string{"", "NAME", "NAME=\"open", "NAME=end\\"} {
		if _, err := osRelease(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
func TestMountsSortingEscapesAndNoNetworkCapacityCalls(t *testing.T) {
	text := "5 1 8:1 / /z rw - ext4 /dev/root ro\n" +
		"6 1 0:1 / /network rw - nfs server:/export rw\n" +
		"7 1 0:2 / /fuse rw - fuse.sshfs server:/ rw\n" +
		"8 1 8:2 / /a\\040space rw - xfs /dev/disk\\134name rw\n" +
		"9 1 0:3 / /virtual rw - tmpfs tmpfs rw\n" +
		"10 1 8:3 / /gone rw - ext4 /dev/gone rw\n" +
		"12 1 8:4 / /stack rw - ext4 /dev/under rw\n" +
		"11 1 0:4 / /stack rw - nfs server:/over rw\n"
	s := Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
	calls := []string{}
	gatherMounts(&s, text, func(path string) (int64, int64, error) {
		calls = append(calls, path)
		if path == "/gone" {
			return 0, 0, errors.New("mount disappeared")
		}
		return 200, 0, nil
	})
	if !reflect.DeepEqual(calls, []string{"/a space", "/gone", "/z"}) {
		t.Fatal(calls)
	}
	mounts := s.Values["storage"].(map[string]any)["mounts"].([]any)
	first := mounts[0].(map[string]any)
	if first["source"] != "/dev/disk\\name" || first["available_bytes"] != int64(0) {
		t.Fatal(first)
	}
	if mounts[len(mounts)-1].(map[string]any)["read_only"] != true {
		t.Fatal(mounts)
	}
	if mounts[4].(map[string]any)["id"] != 11 || mounts[5].(map[string]any)["id"] != 12 {
		t.Fatal("mount ID order", mounts)
	}
	if len(s.Unavailable) != 2 || s.Unavailable["/node/storage/mounts/2/total_bytes"] != "mount disappeared" {
		t.Fatal(s.Unavailable)
	}
	for _, m := range mounts {
		m := m.(map[string]any)
		if m["filesystem"] == "nfs" || m["filesystem"] == "tmpfs" {
			if _, ok := m["total_bytes"]; ok {
				t.Fatal(m)
			}
		}
	}
	for _, bad := range []string{"invalid", "1 2 8:1 / /bad\\z99 rw - ext4 /dev/sda rw"} {
		if _, err := parseMounts(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	empty, err := parseMounts("")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}
func TestCollectionPreservesKnownValuesAlongsideFailures(t *testing.T) {
	read := func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/kernel/osrelease":
			return []byte("6.example\n"), nil
		case "/proc/sys/kernel/random/boot_id":
			return []byte("boot\n"), nil
		case "/sys/devices/system/cpu/online":
			return []byte("0-3"), nil
		case "/proc/meminfo":
			return []byte("MemTotal: 4097 kB\nSwapTotal: 0 kB"), nil
		case "/etc/os-release":
			return nil, os.ErrNotExist
		case "/usr/lib/os-release":
			return []byte("ID=example"), nil
		case "/proc/self/mountinfo":
			return []byte("1 0 0:1 / / rw - tmpfs tmpfs rw"), nil
		}
		return nil, errors.New("unexpected read")
	}
	links := func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "lo", MTU: 65536}, {Index: 3, Name: "failed", MTU: 1500}}, nil
	}
	addresses := func(i net.Interface) ([]net.Addr, error) {
		if i.Name == "failed" {
			return nil, errors.New("link disappeared")
		}
		return []net.Addr{&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)}, &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
	}
	s := collect("app", read, func() (string, error) { return "machine", nil }, links, addresses, func(h string) string { return h + ".example.net" }, func() (string, error) { return "kvm", nil })
	if s.Values["name"] != "app" || s.Values["hostname"] != "machine" || s.Values["arch"] == "" || s.CollectedAt.IsZero() {
		t.Fatal(s)
	}
	if s.Values["os"].(map[string]any)["id"] != "example" || s.Values["memory"].(map[string]any)["total_mib"] != int64(4) {
		t.Fatal(s)
	}
	if s.Values["fully_qualified_name"] != "machine.example.net" || s.Values["virtualization"].(map[string]any)["type"] != "kvm" {
		t.Fatal(s)
	}
	// An unknown distribution keeps its ID; what MMX cannot derive is unavailable.
	if s.Values["distribution"].(map[string]any)["id"] != "example" || !strings.Contains(s.Unavailable["/node/distribution/family"], `"example"`) || s.Unavailable["/node/package_manager"] == "" || s.Unavailable["/node/distribution/version"] == "" {
		t.Fatal(s)
	}
	observedLinks := s.Values["network"].(map[string]any)["interfaces"].(map[string]any)
	lo := observedLinks["lo"].(map[string]any)
	if lo["mac"] != "" || lo["addresses"].([]any)[0].(map[string]any)["address"] != "::1" {
		t.Fatal("the addresses are not in the kernel's order", lo)
	}
	if s.Unavailable["/node/network/interfaces/failed/addresses"] != "link disappeared" {
		t.Fatal(s.Unavailable)
	}
	failed := collect("app", func(string) ([]byte, error) { return nil, errors.New("unreadable") }, func() (string, error) { return "", errors.New("hostname failed") }, func() ([]net.Interface, error) { return nil, errors.New("network failed") }, addresses, func(string) string { return "unused" }, func() (string, error) { return "", errors.New("no systemd-detect-virt") })
	for _, path := range []string{"hostname", "fully_qualified_name", "virtualization/type", "distribution/id", "distribution/family", "package_manager", "cpu/count", "memory/total_bytes", "memory/swap_total_bytes", "storage/mounts", "network/interfaces", "network/default_routes", "network/default_ipv4_address", "network/default_ipv6_address"} {
		if failed.Unavailable["/node/"+path] == "" {
			t.Fatal(path, failed)
		}
	}
	if len(failed.Values["memory"].(map[string]any)) != 0 {
		t.Fatal("invented zero memory")
	}
	for path := range failed.Unavailable {
		if !strings.HasPrefix(path, "/node/") {
			t.Fatal(path)
		}
	}
}

// The os-release excerpts are from the test images of each distribution.
func TestDistributionFacts(t *testing.T) {
	for _, tc := range []struct {
		release string
		want    map[string]any
		missing []string
	}{
		{"ID=debian\nVERSION_ID=\"13\"\nVERSION_CODENAME=trixie\n", map[string]any{"id": "debian", "version": "13", "major_version": 13, "codename": "trixie", "family": "debian", "package_manager": "apt"}, nil},
		{"ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n", map[string]any{"id": "ubuntu", "version": "24.04", "major_version": 24, "codename": "noble", "family": "debian", "package_manager": "apt"}, nil},
		{"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.8\"\n", map[string]any{"id": "rocky", "version": "9.8", "major_version": 9, "family": "redhat", "package_manager": "dnf"}, []string{"codename"}},
		{"ID=\"almalinux\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"10.2\"\n", map[string]any{"id": "almalinux", "major_version": 10, "family": "redhat", "package_manager": "dnf"}, []string{"codename"}},
		{"ID=fedora\nVERSION_ID=43\nVERSION_CODENAME=\"\"\n", map[string]any{"id": "fedora", "major_version": 43, "family": "redhat", "package_manager": "dnf"}, []string{"codename"}},
		// A rolling release has no VERSION_ID.
		{"ID=arch\nIMAGE_ID=archlinux\n", map[string]any{"id": "arch", "family": "arch", "package_manager": "pacman"}, []string{"version", "major_version", "codename"}},
		{"ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\nVERSION_ID=\"16.1\"\n", map[string]any{"id": "opensuse-leap", "major_version": 16, "family": "suse", "package_manager": "zypper"}, []string{"codename"}},
		// A distribution that is not listed takes the family of its first known ID_LIKE word.
		{"ID=\"manjaro\"\nID_LIKE=arch\n", map[string]any{"id": "manjaro", "family": "arch", "package_manager": "pacman"}, []string{"version"}},
		// A distribution MMX does not list takes its family from ID_LIKE.
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\nVERSION_ID=\"22\"\n", map[string]any{"id": "linuxmint", "family": "debian", "package_manager": "apt"}, []string{"codename"}},
		// Unknown distributions have no family or package manager.
		{"ID=unknown\nVERSION_ID=\"1\"\n", map[string]any{"id": "unknown", "version": "1", "major_version": 1}, []string{"family", "package_manager", "codename"}},
	} {
		release, err := osRelease(tc.release)
		s := Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
		gatherDistribution(&s, release, err)
		got, _ := s.Values["distribution"].(map[string]any)
		for key, want := range tc.want {
			value := got[key]
			if key == "package_manager" {
				value = s.Values[key]
			}
			if value != want {
				t.Fatal(tc.release, key, value, s.Unavailable)
			}
		}
		for _, key := range tc.missing {
			path := "/node/distribution/" + key
			if key == "package_manager" {
				path = "/node/package_manager"
			}
			if _, set := got[key]; set || s.Unavailable[path] == "" {
				t.Fatal(tc.release, key, "should be unavailable with a reason", s)
			}
		}
	}
}

// A keepalived virtual address that joins the interface later is listed
// after the server's own address and must not become the default address:
// the addresses keep the kernel's order, not the order of their text.
func TestInterfaceAddressesKeepTheKernelsOrder(t *testing.T) {
	links := func() ([]net.Interface, error) { return []net.Interface{{Index: 2, Name: "eth0", MTU: 1500}}, nil }
	addresses := func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("2.31.78.62"), Mask: net.CIDRMask(32, 32)}, &net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(32, 32)}}, nil
	}
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	s := collect("lb01", read, func() (string, error) { return "lb01", nil }, links, addresses, func(h string) string { return h }, func() (string, error) { return "kvm", nil })
	listed := s.Values["network"].(map[string]any)["interfaces"].(map[string]any)["eth0"].(map[string]any)["addresses"].([]any)
	if listed[0].(map[string]any)["address"] != "2.31.78.62" {
		t.Fatal(listed)
	}
}

func TestDefaultAddresses(t *testing.T) {
	address := func(family, value string) any {
		return map[string]any{"family": family, "address": value, "prefix_length": 24}
	}
	interfaces := map[string]any{
		"eth0": map[string]any{"addresses": []any{address("ipv4", "192.0.2.10"), address("ipv6", "fe80::1"), address("ipv6", "2001:db8::10")}},
		"eth1": map[string]any{"addresses": []any{address("ipv4", "198.51.100.7")}},
		"eth2": map[string]any{},
	}
	routes := []any{
		map[string]any{"family": "ipv4", "interface": "eth1", "metric": int64(200)},
		map[string]any{"family": "ipv4", "interface": "eth0", "metric": int64(100)},
		map[string]any{"family": "ipv6", "interface": "eth0", "metric": int64(1024)},
	}
	if a, err := defaultAddress("ipv4", routes, interfaces); a != "192.0.2.10" || err != nil {
		t.Fatal(a, err)
	}
	if a, err := defaultAddress("ipv6", routes, interfaces); a != "2001:db8::10" || err != nil {
		t.Fatal("a link-local address was chosen", a, err)
	}
	// A route that names its source address, as a DHCP client's route does,
	// gives that address, wherever the interface lists it.
	sourced := []any{map[string]any{"family": "ipv4", "interface": "eth0", "metric": int64(100), "source_address": "192.0.2.77"}}
	if a, err := defaultAddress("ipv4", sourced, interfaces); a != "192.0.2.77" || err != nil {
		t.Fatal(a, err)
	}
	for _, tc := range []struct {
		routes []any
		want   string
	}{
		{nil, "no ipv4 default route"},
		{[]any{map[string]any{"family": "ipv4", "interface": "eth2", "metric": int64(0)}}, "addresses of interface eth2 are unavailable"},
		{[]any{map[string]any{"family": "ipv6", "interface": "eth1", "metric": int64(0)}}, "no ipv4 default route"},
	} {
		if a, err := defaultAddress("ipv4", tc.routes, interfaces); a != "" || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(tc, a, err)
		}
	}
}
