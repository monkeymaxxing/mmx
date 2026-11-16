package modules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mmx/internal/moduleio"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestCommandTimeoutKillsChildren(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := Command(ctx, nil, "", "/bin/bash", "-c", "sleep 30 & wait")
	if r.Status != "failed" || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout did not stop process tree: %+v", r)
	}
}

// A command's result holds all that the program wrote, however late the
// node job worker gets to read it, as on a busy machine. Here the worker
// reads nothing of an output until it has set the read deadlines of both, so
// all of each output comes through the reads of what is in the pipe. A late
// reader is no sign that another program holds an output open: an exit code
// of 0 succeeds, and one of 3 fails with only its exit status.
//
// The test replaces one that began to read 3 seconds after the program's
// end. That test proved the fault of an earlier build, where exec.Cmd
// closed a program's pipes 2 seconds after its end, only with temporary
// instrumentation of that build: a hook on the first write into the capture,
// once a first part was read, that waited for the program's end and 3
// seconds. Then an exit code of 0 failed with exec.ErrWaitDelay, and with an
// exit code of 3 the second part was lost without a word.
func TestCommandResultHoldsAllThatTheProgramWrote(t *testing.T) {
	t.Parallel()
	for _, exitCode := range []int{0, 3} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			t.Parallel()
			deadlinesSet := make(chan struct{})
			reads := map[string]*atomic.Int32{"standard output": {}, "standard error": {}}
			readingLate := context.WithValue(context.Background(), testProgramOutputHooksKey{}, testProgramOutputHooks{
				beforeReading:                  func(string) { <-deadlinesSet },
				afterSettingReadDeadlines:      func() { close(deadlinesSet) },
				afterEachReadOfWhatIsInThePipe: func(what string) { reads[what].Add(1) },
			})
			script := `printf 'the result'; printf 'the reason' >&2; exit ` + strconv.Itoa(exitCode)
			returned := make(chan protocol.Result, 1)
			go func() {
				returned <- runProcess(readingLate, nil, "", nil, nil, "/bin/sh", "-c", script).Result
			}()
			r := waitAtMostAMinute(t, returned, "the command's result, read late")
			wantStatus, wantReason := "succeeded", ""
			if exitCode != 0 {
				wantStatus, wantReason = "failed", "exit status 3"
			}
			if r.Status != wantStatus || r.Reason != wantReason || r.ExitCode != exitCode || r.Stdout != "the result" || r.Stderr != "the reason" || r.StdoutTruncated || r.StderrTruncated {
				t.Fatalf("%+v", r)
			}
			if reads["standard output"].Load() == 0 || reads["standard error"].Load() == 0 {
				t.Fatal("an output did not come through the reads of what is in the pipe")
			}
		})
	}
}

// Once the node job worker has stopped waiting for an output's end, it
// reads what is in the pipe until the pipe is empty, in as many reads as
// that takes. A pipe holds no more than its capacity, which can be as small
// as 4 KiB, so a program that has ended cannot leave several hundred KiB in
// it. Here the test is the writer that keeps the pipe open: after each read
// it puts the next 4 KiB part into the empty pipe, and after the last part
// it closes the pipe. 400 KiB of each output arrive whole and in order.
func TestCommandOutputReadingWhatIsInThePipeTakesAsManyReadsAsItNeeds(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"standard output", "standard error"} {
		var capture capBuffer
		output, err := newProgramOutput(&capture, what)
		if err != nil {
			t.Fatal(err)
		}
		defer output.close()
		const parts = 100
		var written bytes.Buffer
		reads := 0
		writeThePart := func(part int) {
			if part == parts {
				_ = output.write.Close()
				return
			}
			text := fmt.Sprintf("%s, part %03d: %s\n", what, part, strings.Repeat("x", 4000))
			written.WriteString(text)
			if _, err := output.write.WriteString(text); err != nil {
				t.Fatal(err)
			}
		}
		writeThePart(0)
		// As readEverythingThatTheProgramWrote stops waiting.
		if err := output.read.SetReadDeadline(time.Now()); err != nil {
			t.Fatal(err)
		}
		output.readUntilItEnds(context.WithValue(context.Background(), testProgramOutputHooksKey{}, testProgramOutputHooks{
			afterEachReadOfWhatIsInThePipe: func(string) { reads++; writeThePart(reads) },
		}))
		if output.lost != nil || capture.truncated || reads != parts || capture.buffer.String() != written.String() {
			t.Fatalf("%s: %d reads, %d of %d bytes, lost: %v", what, reads, capture.buffer.Len(), written.Len(), output.lost)
		}
	}
}

// A poll that waits for the end of an output can return without it, although
// the last writer closed the pipe while the poll waited: macOS does so for a
// pipe that still holds unread bytes, and a command that only wrote "hi"
// failed as if a program it started held its output. Here the test closes
// the writer after the poll has returned and before the node job worker
// decides, with bytes still in the pipe: the output has ended. A writer that
// stays open still holds the output.
func TestCommandOutputThatEndsAfterTheWaitForItsEndHasEnded(t *testing.T) {
	// Not in parallel: a program that another test starts meanwhile
	// inherits the pipe's write end until it runs its own program, and the
	// poll that decides sees a writer then.
	for _, writerClosesAfterThePoll := range []bool{true, false} {
		var capture capBuffer
		output, err := newProgramOutput(&capture, "standard output")
		if err != nil {
			t.Fatal(err)
		}
		defer output.close()
		if _, err := output.write.WriteString("still in the pipe"); err != nil {
			t.Fatal(err)
		}
		polls := 0
		ctx := context.WithValue(context.Background(), testProgramOutputHooksKey{}, testProgramOutputHooks{
			afterWaitingForTheOutputsEnd: func(string) {
				polls++
				if writerClosesAfterThePoll {
					_ = output.write.Close()
				}
			},
		})
		held := outputsStillHeld(ctx, 0, output)
		wantHeld := !writerClosesAfterThePoll
		if polls != 1 || (len(held) > 0) != wantHeld {
			t.Fatalf("writer closed after the poll: %v; polls: %d; held: %v", writerClosesAfterThePoll, polls, held)
		}
	}
}

// A program that a command starts in the background, with its output
// redirected, keeps running after the command's result, as after the end of
// a script in a terminal. The program answers a question of the test that
// comes after the result, through a FIFO, and then ends; the test sees its
// end through a second FIFO that the program holds open.
func TestCommandLeavesARedirectedBackgroundProgramRunning(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	alive, question := filepath.Join(directory, "alive"), filepath.Join(directory, "question")
	ready, answer := filepath.Join(directory, "ready"), filepath.Join(directory, "answer")
	aliveFIFO := fifoThatEndsWithItsWriters(t, alive)
	questionFIFO := fifoToTalkToAProgram(t, question)
	script := `(exec 3>"$1" 4<"$2"; : >"$3"; read -r line <&4; printf '%s' "$line" >"$4") >/dev/null 2>&1 &
while [ ! -e "$3" ]; do sleep 0.01; done`
	returned := make(chan protocol.Result, 1)
	go func() {
		returned <- runProcess(context.Background(), nil, "", nil, nil, "/bin/sh", "-c", script, "sh", alive, question, ready, answer).Result
	}()
	r := waitAtMostAMinute(t, returned, "the command's result, while its background program ran")
	if r.Status != "succeeded" || r.Reason != "" {
		t.Fatalf("%+v", r)
	}
	if _, err := questionFIFO.WriteString("asked after the result\n"); err != nil {
		t.Fatal(err)
	}
	waitForTheEndOfEveryWriter(t, aliveFIFO, "the end of the background program")
	if text, err := os.ReadFile(answer); err != nil || string(text) != "asked after the result" {
		t.Fatalf("the background program did not answer after the command's result: %q, %v", text, err)
	}
}

// A program that a command starts in its own process group with setsid,
// and that still holds the command's output when the command ends, fails
// the command's result at once: MMX cannot end it with the command's process
// group, and does not wait for it. The result keeps what the program wrote
// before the command ended. The test ends the program itself by closing a
// FIFO that the program reads, and sees its end through a second FIFO.
func TestCommandFailsWhenAProgramOutsideItsProcessGroupHoldsItsOutput(t *testing.T) {
	t.Parallel()
	testProgram, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	alive, question, ready := filepath.Join(directory, "alive"), filepath.Join(directory, "question"), filepath.Join(directory, "ready")
	aliveFIFO := fifoThatEndsWithItsWriters(t, alive)
	questionFIFO := fifoToTalkToAProgram(t, question)
	environment := map[string]string{
		"MMX_NEW_SESSION_FIXTURE": "1",
		"MMX_NEW_SESSION_SCRIPT": `exec 3>"$ALIVE" 4<"$QUESTION"
printf 'written before the command ended'; printf 'written before the command ended' >&2
: >"$READY"; read -r line <&4`,
		"ALIVE": alive, "QUESTION": question, "READY": ready,
	}
	script := `"$0" -test.run='^TestNewSessionProcessFixture$' && while [ ! -e "$READY" ]; do sleep 0.01; done`
	returned := make(chan protocol.Result, 1)
	go func() {
		returned <- runProcess(context.Background(), environment, "", nil, nil, "/bin/sh", "-c", script, testProgram).Result
	}()
	r := waitAtMostAMinute(t, returned, "the command's result, while a program outside its process group held its output")
	held := "a child process kept the command's standard output and standard error open"
	if r.Status != "failed" || !strings.Contains(r.Reason, held) || r.ExitCode != 0 || r.Stdout != "written before the command ended" || r.Stderr != "written before the command ended" || r.StdoutTruncated || r.StderrTruncated {
		t.Fatalf("%+v", r)
	}
	_ = questionFIFO.Close()
	waitForTheEndOfEveryWriter(t, aliveFIFO, "the end of the program outside the process group")
}

// The test program is also the program that
// TestCommandFailsWhenAProgramOutsideItsProcessGroupHoldsItsOutput starts:
// it starts MMX_NEW_SESSION_SCRIPT in a session and process group of its
// own, as setsid does, with its own standard output and standard error, and
// ends without waiting for it. macOS has no setsid command.
func TestNewSessionProcessFixture(t *testing.T) {
	if os.Getenv("MMX_NEW_SESSION_FIXTURE") != "1" {
		return
	}
	program := exec.Command("/bin/sh", "-c", os.Getenv("MMX_NEW_SESSION_SCRIPT"))
	program.Stdout, program.Stderr = os.Stdout, os.Stderr
	program.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := program.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitStandIn(5)
	}
	exitStandIn(0)
}

// A shell task whose output MMX could not read whole fails, whatever its
// exit code and result rules say, and writes no marker file. Neither a read
// that failed nor a program that still held the output is taken for the
// command's own words, or for output over 1 MiB. A check whose output could
// not be read fails the task too.
func TestShellFailsWhenItsOutputIsIncomplete(t *testing.T) {
	t.Parallel()
	readFailure := "the test's read failure"
	for _, tc := range []struct {
		name, failingOutput string
		shell               Shell
		wantReason          string
		wantExitCode        int
		wantChange          string
	}{
		{
			name: "a failed read of standard output, exit code 0", failingOutput: "standard output",
			// failed_when would replace the reason, and changed_when reads
			// the part of the output that came.
			shell: Shell{Script: "printf 'part of the result'", FailedWhen: map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "part"}},
				ChangedWhen: map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "changed"}}},
			wantReason: "could not read all of the program's standard output: " + readFailure, wantChange: "unchanged",
		},
		{
			name: "a failed read of standard error, accepted exit code 3", failingOutput: "standard error",
			shell:      Shell{Script: "printf 'part of the result'; exit 3", SuccessExitCodes: []int{3}},
			wantReason: "exit status 3; could not read all of the program's standard error: " + readFailure, wantExitCode: 3, wantChange: "changed",
		},
		{
			name:       "a program it started holds its output, exit code 0",
			shell:      Shell{Script: `(exec 3>"$ALIVE" && : >"$READY" && exec sleep 1000) & while [ ! -e "$READY" ]; do sleep 0.01; done`},
			wantReason: "the command ended, but a child process kept the command's standard output and standard error open", wantChange: "changed",
		},
		{
			name: "a failed read of a check's standard output", failingOutput: "standard output",
			shell:      Shell{Script: `: >"$READY"`, OnlyIf: &Guard{Script: "printf yes"}},
			wantReason: "node task command condition failed: could not read all of the program's standard output: " + readFailure, wantChange: "unchanged",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			alive, ready, marker := filepath.Join(directory, "alive"), filepath.Join(directory, "ready"), filepath.Join(directory, "marker")
			aliveFIFO := fifoThatEndsWithItsWriters(t, alive)
			sh := tc.shell
			sh.Env = map[string]any{"ALIVE": alive, "READY": ready}
			sh.RunOnlyOnceMarkerFile = marker
			ctx := context.WithValue(context.Background(), testProgramOutputHooksKey{}, testProgramOutputHooks{
				readFails: func(what string) error {
					if what == tc.failingOutput {
						return errors.New(readFailure)
					}
					return nil
				},
			})
			returned := make(chan protocol.Result, 1)
			go func() { returned <- (Runtime{}).Shell(ctx, sh, value.Scope{}, false) }()
			r := waitAtMostAMinute(t, returned, "the shell task's result")
			if r.Status != "failed" || !strings.HasPrefix(r.Reason, tc.wantReason) || r.ExitCode != tc.wantExitCode || r.Change != tc.wantChange || r.FailureKind != "" || r.StdoutTruncated || r.StderrTruncated {
				t.Fatalf("%+v", r)
			}
			if fileExists(marker) {
				t.Fatal("the marker file was written")
			}
			if tc.shell.OnlyIf != nil && fileExists(ready) {
				t.Fatal("the script ran after its check failed")
			}
			// The program that held the output ended with the command's
			// process group; in the other cases nothing opened the FIFO.
			if fileExists(ready) {
				waitForTheEndOfEveryWriter(t, aliveFIFO, "the end of the program that held the output")
			}
		})
	}
}

// fifoThatEndsWithItsWriters makes a FIFO at path and opens it for reading.
// A program that the test starts opens it for writing and holds it as long
// as it lives; a read of the returned file ends once every writer has ended.
func fifoThatEndsWithItsWriters(t *testing.T, path string) *os.File {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// A reader that does not wait for a writer, then reads that wait.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	fifo := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { _ = fifo.Close() })
	return fifo
}

// fifoToTalkToAProgram makes a FIFO at path through which the test writes
// to a program that reads it. The test holds the FIFO's only writer: once
// the test closes it, the program reads the FIFO's end. The test also holds
// a reader, so neither the test nor the program waits for the other to open
// it.
func fifoToTalkToAProgram(t *testing.T, path string) *os.File {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fifo.Close() })
	return fifo
}

// waitForTheEndOfEveryWriter waits until every program that held the FIFO
// open for writing has ended.
func waitForTheEndOfEveryWriter(t *testing.T, fifo *os.File, what string) {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(fifo)
		ended <- err
	}()
	if err := waitAtMostAMinute(t, ended, what); err != nil {
		t.Fatal(err)
	}
}

// A program that leaves its standard input unread to a child it started
// returns its result at once, and the child ends with it. The child holds a
// FIFO of the test open for as long as it lives, so the test waits for the
// FIFO to close, not for the child's process ID to disappear.
func TestCommandEndsAChildThatHoldsStandardInput(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	alive, ready := filepath.Join(directory, "child_alive"), filepath.Join(directory, "child_ready")
	if err := syscall.Mkfifo(alive, 0600); err != nil {
		t.Fatal(err)
	}
	// A reader that does not wait for a writer, so the child can open the
	// FIFO, then reads that wait for the end of every writer.
	fd, err := syscall.Open(alive, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	fifo := os.NewFile(uintptr(fd), alive)
	defer fifo.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// More than a pipe holds, so writing the input waits for a reader. The
	// program ends once its child holds the FIFO and standard input. The
	// child's output goes elsewhere, so only standard input ties it to the
	// program.
	input := strings.NewReader(strings.Repeat("x", 4<<20))
	script := `(exec 3>"$1" >/dev/null 2>&1 && : >"$2" && exec sleep 1000) 0<&0 &
while [ ! -e "$2" ]; do sleep 0.01; done`
	returned := make(chan protocol.Result, 1)
	go func() {
		returned <- runProcess(ctx, nil, "", input, nil, "/bin/sh", "-c", script, "sh", alive, ready).Result
	}()
	r := waitAtMostAMinute(t, returned, "the program's result, while its child held standard input")
	if r.Status != "succeeded" || !fileExists(ready) {
		t.Fatalf("%+v", r)
	}
	childEnded := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(fifo)
		childEnded <- err
	}()
	if err := waitAtMostAMinute(t, childEnded, "the end of the child that held standard input"); err != nil {
		t.Fatal(err)
	}
}

// An HTTP body shares the command-output capture: past the limit it is marked
// truncated, and a predicate consuming it fails instead of matching partial text.
func TestHTTPBodyUsesTheBoundedCapture(t *testing.T) {
	t.Parallel()
	if outputLimit != moduleio.MaxResultBytes {
		t.Fatal("an oversized module result must be exactly a truncated capture")
	}
	for _, size := range []int{1 << 20, 1<<20 + 1} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(bytes.Repeat([]byte("x"), size)) }))
		r := SendHTTPRequest(context.Background(), HTTP{URL: server.URL})
		server.Close()
		truncated := size > 1<<20
		if r.Status != "succeeded" || r.ResponseBodyTruncated != truncated || strings.HasSuffix(r.ResponseBody, protocol.TruncationMarker) != truncated {
			t.Fatalf("size %d: %s truncated=%v: %s", size, r.Status, r.ResponseBodyTruncated, r.Reason)
		}
		_, err := value.Scope{"this_node_task": protocol.ResultValue(r)}.Predicate(map[string]any{"contains": []any{"{{ .this_node_task.response_body }}", "y"}})
		if (err != nil) != truncated {
			t.Fatalf("size %d: predicate error %v", size, err)
		}
	}
}

// A task's processes create files with umask 022, whatever the runner's own
// umask is; the node job worker keeps 077 for its files.
func TestTaskProcessesUseTheConventionalUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	dir := t.TempDir()
	if r := runProcess(context.Background(), nil, dir, nil, nil, "/bin/sh", "-c", "touch file && mkdir directory").Result; r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	for name, want := range map[string]os.FileMode{"file": 0o644, "directory": 0o755} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", name, info.Mode(), err)
		}
	}
	// A missing program still fails before anything starts.
	if p := runProcess(context.Background(), nil, dir, nil, nil, "mmx-no-such-program"); p.Started || !strings.Contains(p.Result.Reason, missingProgramError("mmx-no-such-program").Error()) {
		t.Fatalf("%+v", p)
	}
}
