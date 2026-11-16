package modules

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test executable doubles as the three stand-in programs, selected only
// when launched through a fixture's symlink. Keep real process execution,
// stdin, argv, and exit codes without a shell and its text-processing children
// for every query. None of this code is compiled into MMX.
func init() {
	tool := filepath.Base(os.Args[0])
	switch tool {
	case "nmcli", "journalctl", "systemctl":
		exitStandIn(managerStandIn(tool, filepath.Dir(os.Args[0]), os.Args[1:]))
	}
}

func managerStandInPrograms(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for _, name := range []string{"nmcli", "journalctl", "systemctl"} {
		if err := os.Symlink(executable, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return directory
}

func managerStandIn(tool, directory string, args []string) int {
	// Fixture errors must fail the command, never silently return empty output.
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "NetworkManager test fixture:", err)
			exitStandIn(97)
		}
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(directory, name))
		must(err)
		return string(data)
	}
	write := func(name, text string) {
		must(os.WriteFile(filepath.Join(directory, name), []byte(text), 0600))
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			return false
		}
		must(err)
		return true
	}
	copyIfPresent := func(from, to string) {
		if exists(from) {
			write(to, read(from))
		}
	}
	command := strings.Join(args, " ")
	calls, err := os.OpenFile(filepath.Join(directory, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	must(err)
	_, err = fmt.Fprintln(calls, tool+" "+command)
	must(err)
	must(calls.Close())

	switch tool {
	case "systemctl":
		if exists("inactive-service") {
			fmt.Println("inactive")
			return 3
		}
		fmt.Println("active")
		return 0
	case "journalctl":
		switch {
		case strings.Contains(command, "--show-cursor"):
			if exists("fail-position") {
				return 2
			}
			fmt.Println("-- cursor: before")
		case command == "--sync":
			if exists("fail-sync") {
				return 3
			}
		default:
			if exists("fail-read") {
				return 4
			}
			if exists("journal") {
				fmt.Print(read("journal"))
			}
		}
		return 0
	}

	switch {
	case strings.HasPrefix(command, "--offline connection modify connection.id "):
		candidate, err := io.ReadAll(os.Stdin)
		must(err)
		write("candidate", string(candidate))
		if exists("fail-offline") {
			fmt.Fprint(os.Stderr, read("fail-offline"))
			return 1
		}
		if exists("canonical") {
			fmt.Print(read("canonical"))
		} else {
			fmt.Print(string(candidate))
		}
	case command == "-t --escape no -f UUID,FILENAME connection show":
		if exists("fail-query") {
			return 2
		}
		fmt.Print(read("profiles"))
	case strings.HasPrefix(command, "-t --escape no -f connection.type,connection.interface-name connection show uuid "):
		if len(args) != 9 {
			return 95
		}
		fmt.Print(read(args[8] + ".settings"))
	case command == "-t --escape no -f GENERAL.DEVICE,GENERAL.STATE,GENERAL.CON-UUID device show":
		if exists("fail-activity") {
			return 8
		}
		fmt.Print(read("active"))
	case strings.HasPrefix(command, "connection load "):
		if len(args) != 3 {
			return 95
		}
		if exists("load-output") {
			fmt.Print(read("load-output"))
		}
		if exists("fail-load") {
			return 6
		}
		copyIfPresent("after-load", "profiles")
	case strings.HasPrefix(command, "connection up uuid "), strings.HasPrefix(command, "connection down uuid "):
		if len(args) != 4 {
			return 95
		}
		action, uuid := args[1], args[3]
		loaded := false
		for _, line := range strings.Split(read("profiles"), "\n") {
			id, _, _ := strings.Cut(line, ":")
			loaded = loaded || id == uuid
		}
		if !loaded {
			fmt.Fprintf(os.Stderr, "Error: unknown connection '%s'.\n", uuid)
			return 10
		}
		if exists("fail-" + action) {
			fmt.Fprint(os.Stderr, read("fail-"+action))
			return 4
		}
		device := uuid
		if exists(uuid + ".settings") {
			for _, line := range strings.Split(read(uuid+".settings"), "\n") {
				if name, ok := strings.CutPrefix(line, "connection.interface-name:"); ok {
					device = name
				}
			}
		}
		var active strings.Builder
		for _, block := range strings.Split(read("active"), "GENERAL.DEVICE:")[1:] {
			name, _, _ := strings.Cut(block, "\n")
			if strings.Contains(block, "GENERAL.CON-UUID:"+uuid+"\n") || (action == "up" && name == device) {
				continue
			}
			active.WriteString("GENERAL.DEVICE:" + block)
		}
		if action == "up" {
			fmt.Fprintf(&active, "GENERAL.DEVICE:%s\nGENERAL.STATE:100 (connected)\nGENERAL.CON-UUID:%s\n", device, uuid)
		}
		write("active", active.String())
		copyIfPresent("after-"+action, "active")
	case strings.HasPrefix(command, "device delete "):
		if len(args) != 3 {
			return 95
		}
		if exists("delete-path") {
			must(os.Remove(read("delete-path")))
		}
	default:
		fmt.Fprintln(os.Stderr, "unexpected nmcli arguments:", args)
		return 94
	}
	return 0
}
