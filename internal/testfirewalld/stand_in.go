// Package testfirewalld supplies the firewall-cmd stand-in shared by module
// and node tests. Only test files import it; it is not part of either MMX binary.
package testfirewalld

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A fixture invokes its own compiled test program through a firewall-cmd
// symlink. Its state is beside that link, regardless of the working directory.
// No source-tree file, interpreter, or Go installation is needed on the guest.
func init() {
	if filepath.Base(os.Args[0]) == "firewall-cmd" {
		// syscall.Exit, not os.Exit: in a test program built with -race,
		// os.Exit first waits a second (GORACE's atexit_sleep_ms), and
		// tests start this stand-in hundreds of times.
		syscall.Exit(run(filepath.Dir(os.Args[0]), os.Args[1:]))
	}
}

// Install links firewall-cmd to the running test executable. The caller creates
// the fixture's state and puts directory on PATH, as for its other stand-ins.
func Install(t *testing.T, directory string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(directory, "firewall-cmd")); err != nil {
		t.Fatal(err)
	}
}

func run(directory string, args []string) int {
	// A fixture error must fail the command, not resemble a successful native
	// query. The test reads state independently of the production parser.
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "firewalld test fixture:", err)
			os.Exit(97)
		}
	}
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(directory, name))
		must(err)
		return data
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			return false
		}
		must(err)
		return true
	}
	calls, err := os.OpenFile(filepath.Join(directory, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	must(err)
	_, err = fmt.Fprintln(calls, strings.Join(args, " "))
	must(err)
	must(calls.Close())
	var state map[string]map[string]map[string][]string
	must(json.Unmarshal(read("state"), &state))
	if slices.Contains(args, "--state") {
		if exists("stopped") {
			fmt.Println("not running")
			return 252
		}
		fmt.Println("running")
		return 0
	}
	if exists("stopped") {
		fmt.Fprintln(os.Stderr, "FirewallD is not running")
		return 252
	}
	if exists("fail") && slices.Contains(args, string(read("fail"))) {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--change-") || strings.HasPrefix(arg, "--remove-") || strings.HasPrefix(arg, "--delete-") {
				fmt.Fprintln(os.Stderr, "native firewalld refusal")
				return 1
			}
		}
	}
	layerName, zone := "running", ""
	if slices.Contains(args, "--permanent") {
		layerName = "stored"
	}
	for _, arg := range args {
		if name, ok := strings.CutPrefix(arg, "--zone="); ok {
			zone = name
		}
	}
	layer := state[layerName]
	names := make([]string, 0, len(layer))
	for name := range layer {
		names = append(names, name)
	}
	slices.Sort(names)
	if slices.Contains(args, "--list-all-zones") {
		for _, name := range names {
			members := layer[name]
			fmt.Printf("%s (active)\n  target: default\n  interfaces: %s\n  sources: %s\n  services: ssh\n  ports: 7443/tcp\n  rich rules:\n", name, strings.Join(members["interface"], " "), strings.Join(members["source"], " "))
		}
		return 0
	}
	if slices.Contains(args, "--get-zones") {
		fmt.Println(strings.Join(names, " "))
		return 0
	}
	if slices.Contains(args, "--reload") {
		// Boot/reload replaces runtime with an independent copy of stored state.
		state["running"] = make(map[string]map[string][]string) //nolint:nilaway // Test callers decode object-shaped fixtures with allocated state maps before invoking the stand-in.
		for name, members := range state["stored"] {
			state["running"][name] = make(map[string][]string)
			for kind, values := range members {
				state["running"][name][kind] = slices.Clone(values)
			}
		}
	} else {
		for _, arg := range args {
			key, value, _ := strings.Cut(arg, "=")
			switch key {
			case "--permanent", "--zone":
			case "--new-zone":
				layer[value] = map[string][]string{"interface": {}, "source": {}} //nolint:nilaway // Test fixtures allocate both layer maps and zone operations preserve those maps.
			case "--delete-zone":
				delete(layer, value)
			case "--change-interface", "--change-source", "--remove-interface", "--remove-source":
				if exists("ignore") {
					continue
				}
				if layer[zone] == nil {
					must(fmt.Errorf("unknown zone %q", zone))
				}
				action, kind, _ := strings.Cut(strings.TrimPrefix(key, "--"), "-")
				remove := func(members map[string][]string) {
					if i := slices.Index(members[kind], value); i >= 0 {
						members[kind] = slices.Delete(members[kind], i, i+1)
					}
				}
				if action == "change" {
					for _, members := range layer {
						remove(members)
					}
					layer[zone][kind] = append(layer[zone][kind], value) //nolint:nilaway // A nil layer[zone] invokes must with an error, which exits before this write.
				} else {
					remove(layer[zone])
				}
			default:
				must(fmt.Errorf("unexpected firewall-cmd argument %q", arg))
			}
		}
	}
	data, err := json.Marshal(state)
	must(err)
	must(os.WriteFile(filepath.Join(directory, "state"), data, 0600))
	fmt.Println("success")
	return 0
}
