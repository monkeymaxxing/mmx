package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Many tests give a module stand-ins for a node's programs: shell scripts
// that print what the real program prints and note how they were called.
//
// Every stand-in is a symbolic link to one launcher script, which runs the
// stand-in's own script from the hidden directory .stand-in-scripts beside
// the link. One file for all stand-ins, and not a new executable file for
// each: macOS checks every new executable file at its first start, which
// takes up to half a second, and a link to a file that has started before
// starts at once.
//
// A test hands its stand-ins to a module through the context (testContext
// and setWorkerEnvironment), not through the PATH of the whole test process,
// so that it can run beside other tests.

// standInLauncher is the launcher script of this test process, made by
// TestMain.
var standInLauncher string

// standInScriptsDirectory is the directory, beside a stand-in's link, that
// holds the stand-in's own script.
const standInScriptsDirectory = ".stand-in-scripts"

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "mmx-modules-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	standInLauncher = filepath.Join(directory, "stand-in-launcher")
	// The launcher keeps $0 and the arguments, and the script reads them as
	// if it had been started itself.
	launcher := "#!/bin/sh\n. \"${0%/*}/" + standInScriptsDirectory + "/${0##*/}\"\n"
	// Readable by every account: a module may start a stand-in as another.
	err = os.Chmod(directory, 0755)
	if err == nil {
		err = os.WriteFile(standInLauncher, []byte(launcher), 0555)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// A wait that a test does not study ends sooner (wait_for.go).
	waitForCheckInterval = 100 * time.Millisecond
	code := m.Run()
	// A test that wrote to a stand-in's path wrote into the launcher of
	// every stand-in, which root, as on a test node, may do.
	if written, err := os.ReadFile(standInLauncher); err != nil || string(written) != launcher {
		fmt.Fprintln(os.Stderr, "a test changed the launcher of the stand-in programs; write a stand-in with writeStandInProgram")
		code = 1
	}
	os.RemoveAll(directory)
	os.Exit(code)
}

// exitStandIn ends this test program where it ran as a stand-in program,
// such as the mysql client. It uses syscall.Exit, not os.Exit: in a test
// program built with -race, os.Exit first waits a second (GORACE's
// atexit_sleep_ms), and tests start their stand-ins hundreds of times.
func exitStandIn(code int) {
	syscall.Exit(code)
}

// writeStandInProgram makes path a stand-in program that runs script with
// /bin/sh. Writing it again replaces the script. Never write to path itself
// or change its mode: it is a link to the launcher of every stand-in.
func writeStandInProgram(t *testing.T, path, script string) {
	t.Helper()
	if strings.HasPrefix(script, "#!") && !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("the stand-in %s must be a /bin/sh script", path)
	}
	scripts := filepath.Join(filepath.Dir(path), standInScriptsDirectory)
	if err := os.MkdirAll(scripts, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, filepath.Base(path)), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(path); err == nil && target == standInLauncher {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(standInLauncher, path); err != nil {
		t.Fatal(err)
	}
}

// workerEnvironmentsOfTests holds, by the name of a test, the environment
// variables that setWorkerEnvironment set for the commands of the test's
// modules.
var workerEnvironmentsOfTests = struct {
	sync.Mutex
	byTest map[string]map[string]string
}{byTest: map[string]map[string]string{}}

// setWorkerEnvironment is t.Setenv for only the commands that the modules of
// this test and of its subtests start under testContext. Unlike t.Setenv, it
// lets the test run beside others (t.Parallel).
func setWorkerEnvironment(t *testing.T, name, value string) {
	test := t.Name()
	workerEnvironmentsOfTests.Lock()
	defer workerEnvironmentsOfTests.Unlock()
	if workerEnvironmentsOfTests.byTest[test] == nil {
		workerEnvironmentsOfTests.byTest[test] = map[string]string{}
		t.Cleanup(func() {
			workerEnvironmentsOfTests.Lock()
			defer workerEnvironmentsOfTests.Unlock()
			delete(workerEnvironmentsOfTests.byTest, test)
		})
	}
	workerEnvironmentsOfTests.byTest[test][name] = value
}

// workerEnvironmentOfTest is what setWorkerEnvironment set for the test and
// for the tests that it is a subtest of; a subtest's own values come first.
func workerEnvironmentOfTest(test string) map[string]string {
	workerEnvironmentsOfTests.Lock()
	defer workerEnvironmentsOfTests.Unlock()
	variables := map[string]string{}
	for {
		for name, value := range workerEnvironmentsOfTests.byTest[test] {
			if _, set := variables[name]; !set {
				variables[name] = value
			}
		}
		parent := strings.LastIndex(test, "/")
		if parent < 0 {
			return variables
		}
		test = test[:parent]
	}
}

// testContext is the context under which the commands of a module get what
// setWorkerEnvironment set for the test, as it is when each command starts
// (testWorkerEnvironmentKey in process.go).
func testContext(t *testing.T) context.Context {
	test := t.Name()
	return context.WithValue(context.Background(), testWorkerEnvironmentKey{}, func() map[string]string {
		return workerEnvironmentOfTest(test)
	})
}

// workerPath is the PATH on which the modules of the test find programs.
func workerPath(t *testing.T) string {
	if path, set := workerEnvironmentOfTest(t.Name())["PATH"]; set {
		return path
	}
	return os.Getenv("PATH")
}

// putProgramsFirst makes the modules of the test find programs first in
// directory, then where they found them before.
func putProgramsFirst(t *testing.T, directory string) {
	setWorkerEnvironment(t, "PATH", directory+string(os.PathListSeparator)+workerPath(t))
}

// standInPrograms writes stand-ins for a node's programs into a new
// directory, which the modules of the test search first (putProgramsFirst).
// Each stand-in knows the directory as $dir, where it keeps its state and
// appends its call to $dir/calls.
func standInPrograms(t *testing.T, programs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range programs {
		text := "dir=" + dir + "\necho \"" + name + "${*:+ $*}\" >> \"$dir/calls\"\n" + script + "\n"
		writeStandInProgram(t, filepath.Join(dir, name), text)
	}
	putProgramsFirst(t, dir)
	return dir
}
