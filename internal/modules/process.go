package modules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mmx/internal/protocol"

	"golang.org/x/sys/unix"
)

// outputLimit bounds each captured command stream, HTTP body, and module
// result. It equals moduleio.MaxResultBytes, so an oversized module result is
// exactly a truncated capture.
const outputLimit = 1 << 20

// capBuffer is the one bounded capture for command, HTTP, and module output.
// It keeps the first outputLimit bytes and records truncation, which predicates
// reject. It drains child pipes and never blocks a process at the limit.
// truncated means only that the output went past the limit.
type capBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := outputLimit - b.buffer.Len()
	if remaining > 0 {
		if remaining < len(p) {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	if n > remaining {
		b.truncated = true
	}
	return n, nil
}
func (b *capBuffer) Text() string {
	if b.truncated {
		return b.buffer.String() + protocol.TruncationMarker
	}
	return b.buffer.String()
}

func Command(ctx context.Context, env map[string]string, cwd string, name string, args ...string) protocol.Result {
	return runProcess(ctx, env, cwd, nil, nil, name, args...).Result
}

type processResult struct {
	Result protocol.Result
	Output []byte
	// Truncated says that the standard output went past outputLimit.
	Truncated bool
	// OutputReadFailure says why the result does not hold all that the
	// program wrote: a read failed, or a program that it started still held
	// an output open when it ended. Result has failed with it already; a
	// caller that decides success from the exit code checks it first.
	OutputReadFailure error
	Started           bool
	// ProgramNotFound says that the program is not on the node.
	ProgramNotFound bool
}

// taskUmask is the file mode creation mask of every process a node task
// starts: 022, which systemd gives its services, so a checkout, a script's
// directory, or software installed for the whole node is readable by other
// accounts, as its owner's tools expect. The node job worker keeps 077 for
// its own files.
const taskUmask = "022"

// testWorkerEnvironmentKey is the key of a context value that only tests
// set: a function that returns environment variables which replace those of
// the node job worker process for a command started under that context, as
// they are when the command starts. Their PATH is also where lookPath finds
// the command's program. A test gives its module stand-in programs this way,
// without changing the environment of the whole test process, so the test
// can run beside others. On a node no context carries the value, and
// commands get the worker's own environment.
type testWorkerEnvironmentKey struct{}

// replacedWorkerEnvironment is what a test has set under ctx in place of
// the node job worker process's environment; nil on a node.
func replacedWorkerEnvironment(ctx context.Context) map[string]string {
	if variables, ok := ctx.Value(testWorkerEnvironmentKey{}).(func() map[string]string); ok {
		return variables()
	}
	return nil
}

// workerEnvironment is the environment that a command started under ctx
// inherits from the node job worker process.
func workerEnvironment(ctx context.Context) []string {
	environment := os.Environ()
	for name, value := range replacedWorkerEnvironment(ctx) {
		// exec.Cmd uses the last value of a name that appears twice.
		environment = append(environment, name+"="+value)
	}
	return environment
}

// lookPath finds a program as exec.LookPath does, on the PATH of the
// environment that workerEnvironment gives a command started under ctx.
func lookPath(ctx context.Context, name string) (string, error) {
	path, replacedPath := replacedWorkerEnvironment(ctx)["PATH"]
	if !replacedPath || strings.Contains(name, "/") {
		return exec.LookPath(name)
	}
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		if program, err := exec.LookPath(filepath.Join(directory, name)); err == nil {
			return program, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// A module supplies stdin and needs raw bounded stdout. Shell callers keep their
// existing text result. Both use the same process-group and pipe lifetime rules.
// A non-nil account starts the process with its IDs instead of the runner's.
func runProcess(ctx context.Context, env map[string]string, cwd string, input io.Reader, account *syscall.Credential, name string, args ...string) processResult {
	environment := workerEnvironment(ctx)
	for k, v := range env {
		environment = append(environment, k+"="+v)
	}
	return runProcessWithOnlyThisEnvironment(ctx, environment, cwd, input, account, name, args...)
}

// runProcessWithOnlyThisEnvironment is runProcess for a program that gets
// exactly environment (NAME=value entries) and nothing of the node job
// worker process's own. It is for a tool whose target or login a variable
// of the environment could change, such as psql with PGHOST or PGPASSWORD.
// The program itself is still found on the worker's PATH.
func runProcessWithOnlyThisEnvironment(ctx context.Context, environment []string, cwd string, input io.Reader, account *syscall.Credential, name string, args ...string) processResult {
	// Go starts a process with this process's umask. A shell sets the task's
	// and replaces itself with the program, which keeps the process ID and
	// group. The program is found first, as exec.Command finds it, so a
	// missing one fails before anything starts.
	c := exec.CommandContext(ctx, name, args...)
	if program, err := lookPath(ctx, name); err == nil {
		c = exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", "umask " + taskUmask + ` && exec "$0" "$@"`, program}, args...)...)
	} else if !strings.Contains(name, "/") {
		// exec.CommandContext looked on the PATH of the whole process,
		// which a test may have replaced for this command.
		c.Err = programNotFound{name: name, cause: err}
	}
	c.Dir = cwd
	c.Env = environment
	// Killing only Bash leaves its child holding pipes open. Use an isolated
	// process group so timeouts terminate the entire command tree.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: account}
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	var out, stderr capBuffer
	err, outputReadFailure := runToItsEnd(ctx, c, input, &out, &stderr)
	r := protocol.Success("unchanged")
	r.Stdout = out.Text()
	r.Stderr = stderr.Text()
	r.StdoutTruncated, r.StderrTruncated = out.truncated, stderr.truncated
	if err != nil {
		r.Status = "failed"
		r.Reason = err.Error()
		r.ExitCode = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			r.ExitCode = exit.ExitCode()
		}
		if ctx.Err() != nil {
			r.Reason = ctx.Err().Error()
		}
	}
	// Output that could not be read whole fails the result, whatever else
	// ended it, so the loss is never mistaken for the program's own words.
	if outputReadFailure != nil {
		r.Status = "failed"
		r.Reason = strings.TrimPrefix(r.Reason+"; "+outputReadFailure.Error(), "; ")
	}
	return processResult{Result: r, Output: out.buffer.Bytes(), Truncated: out.truncated, OutputReadFailure: outputReadFailure, Started: c.Process != nil, ProgramNotFound: errors.As(err, new(programNotFound))}
}

// runToItsEnd starts c, feeds it input, reads its standard output into
// standardOutput and its standard error into standardError, and waits for
// the program's end. It returns the program's error and, apart from it, an
// error when the captures do not hold all that the program wrote.
//
// A program that the program started in the background with its output
// redirected keeps running, as after a script in a terminal. One that still
// holds an output open fails the result, and the programs left in the
// program's process group end. So do they all, at once, when the time limit
// or a stop ended the program, and when the program got input: a module may
// not leave programs running after its result.
func runToItsEnd(ctx context.Context, c *exec.Cmd, input io.Reader, standardOutput, standardError *capBuffer) (err, outputReadFailure error) {
	stdout, err := newProgramOutput(standardOutput, "standard output")
	if err != nil {
		return err, nil
	}
	defer stdout.close()
	stderr, err := newProgramOutput(standardError, "standard error")
	if err != nil {
		return err, nil
	}
	defer stderr.close()
	c.Stdout, c.Stderr = stdout.write, stderr.write
	// The node job worker writes input through a pipe of its own too, so
	// that waiting for the program never waits for a child that holds its
	// standard input without reading it.
	var inputRead, inputWrite *os.File
	if input != nil {
		if inputRead, inputWrite, err = os.Pipe(); err != nil {
			return err, nil
		}
		defer inputRead.Close()
		defer inputWrite.Close()
		c.Stdin = inputRead
	}
	if err := c.Start(); err != nil {
		return err, nil
	}
	// Only the program holds the other ends: without the worker's copy of
	// each, an output ends when the program's copies do.
	_ = stdout.write.Close()
	_ = stderr.write.Close()
	go stdout.readUntilItEnds(ctx)
	go stderr.readUntilItEnds(ctx)
	inputWritten := make(chan error, 1)
	if input != nil {
		_ = inputRead.Close()
		go func() {
			_, err := io.Copy(inputWrite, input)
			_ = inputWrite.Close()
			inputWritten <- err
		}()
	}
	err = c.Wait()
	var held []string
	switch {
	case ctx.Err() != nil:
		_ = c.Cancel()
	case input != nil:
		held = outputsStillHeld(ctx, waitForOutputsHeldByOtherPrograms, stdout, stderr)
		_ = c.Cancel()
	default:
		held = outputsStillHeld(ctx, waitForOutputsHeldByOtherPrograms, stdout, stderr)
		if len(held) > 0 {
			_ = c.Cancel()
		}
	}
	if input != nil {
		// Nobody reads the rest of the input once the program and its
		// process group have ended.
		_ = inputWrite.SetWriteDeadline(time.Now())
		inputErr := <-inputWritten
		if err == nil && inputErr != nil && !errors.Is(inputErr, syscall.EPIPE) && !errors.Is(inputErr, os.ErrDeadlineExceeded) {
			err = inputErr
		}
	}
	readEverythingThatTheProgramWrote(ctx, stdout, stderr)
	var outputHeld error
	if len(held) > 0 {
		// Ending the process group does not end a program that left it,
		// such as with setsid.
		outputHeld = fmt.Errorf("the command ended, but a child process kept the command's %s open; MMX stopped reading and terminated processes remaining in the command's process group; for a program that must keep running, use systemd_service or start the program with setsid and redirect the program's output", strings.Join(held, " and "))
	}
	return err, errors.Join(outputHeld, stdout.lost, stderr.lost)
}

// programOutput is a pipe through which the node job worker reads one
// output of a program, its standard output or its standard error, into to.
// The worker reads through pipes of its own and not through those of
// exec.Cmd: exec.Cmd stops reading its pipes a fixed time after the program
// ends, and on a busy machine what the program wrote before its end can
// still be in a pipe then. The worker would lose it, and with it a module's
// result or the reason that a program gives for its failure.
type programOutput struct {
	read, write *os.File
	to          *capBuffer
	what        string
	ended       chan struct{}
	// lost says why part of the output could not be read; set before ended
	// closes.
	lost error
}

func newProgramOutput(to *capBuffer, what string) (*programOutput, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &programOutput{read: read, write: write, to: to, what: what, ended: make(chan struct{})}, nil
}

// close closes what is still open of the pipe.
func (o *programOutput) close() {
	_ = o.read.Close()
	_ = o.write.Close()
}

// lose records that part of the output could not be read, which fails the
// result.
func (o *programOutput) lose(err error) {
	o.lost = fmt.Errorf("could not read all of the program's %s: %w", o.what, err)
}

// testProgramOutputHooksKey is the key of a context value that only tests
// set: the testProgramOutputHooks of a command started under that context.
// On a node no context carries it.
type testProgramOutputHooksKey struct{}

// testProgramOutputHooks are functions that the node job worker calls while
// it reads a program's outputs, so that a test can make the worker read
// late, as on a busy machine, or make a read fail. what names the output:
// "standard output" or "standard error". A nil function is not called.
type testProgramOutputHooks struct {
	// beforeReading runs before the worker reads anything of the output.
	beforeReading func(what string)
	// afterSettingReadDeadlines runs once readEverythingThatTheProgramWrote
	// has set the read deadline of every output.
	afterSettingReadDeadlines func()
	// afterEachReadOfWhatIsInThePipe runs after each read of the pipe that
	// returned data, once the worker has stopped waiting for the output's end.
	afterEachReadOfWhatIsInThePipe func(what string)
	// readFails returns an error that the output's reading ends with, as if
	// a read had failed, or nil.
	readFails func(what string) error
	// afterWaitingForTheOutputsEnd runs once endedBy's poll that waits until
	// the deadline has returned, before endedBy decides whether the output
	// has ended.
	afterWaitingForTheOutputsEnd func(what string)
}

func programOutputHooks(ctx context.Context) testProgramOutputHooks {
	hooks, _ := ctx.Value(testProgramOutputHooksKey{}).(testProgramOutputHooks)
	return hooks
}

// readUntilItEnds reads the output until its end, or until
// readEverythingThatTheProgramWrote stops waiting for its end.
func (o *programOutput) readUntilItEnds(ctx context.Context) {
	defer close(o.ended)
	hooks := programOutputHooks(ctx)
	if hooks.beforeReading != nil {
		hooks.beforeReading(o.what)
	}
	if err := o.readAll(hooks); err != nil {
		o.lose(err)
	} else if hooks.readFails != nil {
		if err := hooks.readFails(o.what); err != nil {
			o.lose(err)
		}
	}
}

// readAll reads the output into o.to and returns the error that ended the
// reading early.
func (o *programOutput) readAll(hooks testProgramOutputHooks) error {
	_, err := io.Copy(o.to, o.read)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	// The worker stopped waiting for the output's end. What the program
	// wrote is in the pipe: read all that is there now, and wait for
	// nothing more.
	if err := o.read.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	pipe, err := o.read.SyscallConn()
	if err != nil {
		return err
	}
	var readFailed error
	err = pipe.Read(func(descriptor uintptr) bool {
		buffer := make([]byte, 64<<10)
		// The pipe does not block: a read returns what is there, 0 at its
		// end, or EAGAIN once it is empty. A pipe can hold more than one
		// read takes. Reading stops at the capture's limit, which records
		// the truncation, so a process that keeps writing cannot hold the
		// worker here.
		for !o.to.truncated {
			n, err := syscall.Read(int(descriptor), buffer)
			if n > 0 {
				_, _ = o.to.Write(buffer[:n])
				if hooks.afterEachReadOfWhatIsInThePipe != nil {
					hooks.afterEachReadOfWhatIsInThePipe(o.what)
				}
				continue
			}
			if err == syscall.EINTR {
				continue
			}
			if err != nil && err != syscall.EAGAIN {
				readFailed = err
			}
			break
		}
		return true
	})
	if err != nil {
		return err
	}
	return readFailed
}

// waitForOutputsHeldByOtherPrograms is how long the node job worker waits,
// once a program has ended, for the programs that it started to close its
// outputs, such as a short-lived child that is still writing. One that holds
// an output open longer, such as a daemon started without redirecting its
// output, fails the result. The time is not a limit on what the worker
// reads: readUntilItEnds reads what the program wrote, however late it gets
// to.
const waitForOutputsHeldByOtherPrograms = 200 * time.Millisecond

// outputsStillHeld waits up to wait until no process holds any of outputs
// open for writing, and names those that one still holds then. It asks the
// system, not the worker's reading of the outputs, which can be late on a
// busy machine.
func outputsStillHeld(ctx context.Context, wait time.Duration, outputs ...*programOutput) []string {
	deadline := time.Now().Add(wait)
	var held []string
	for _, output := range outputs {
		if !output.endedBy(ctx, deadline) {
			held = append(held, output.what)
		}
	}
	return held
}

// endedBy reports whether no process holds the output open for writing by
// deadline. A pipe reports POLLHUP once that is so, also while what was
// written is still in it. A poll that waits can still return without
// POLLHUP after the last writer closed the pipe: macOS does so for a pipe
// that holds unread bytes. So a poll that does not wait decides, from the
// pipe as it is then.
func (o *programOutput) endedBy(ctx context.Context, deadline time.Time) bool {
	pipe, err := o.read.SyscallConn()
	if err != nil {
		return false
	}
	hooks := programOutputHooks(ctx)
	ended := false
	err = pipe.Control(func(descriptor uintptr) {
		ended = pipeHungUpBy(descriptor, deadline)
		if hooks.afterWaitingForTheOutputsEnd != nil {
			hooks.afterWaitingForTheOutputsEnd(o.what)
		}
		if !ended {
			ended = pipeHungUpBy(descriptor, time.Time{})
		}
	})
	return err == nil && ended
}

// pipeHungUpBy polls the read end of a pipe until no process holds the pipe
// open for writing, or until deadline, and reports whether none does. A
// deadline that has passed asks without waiting.
func pipeHungUpBy(descriptor uintptr, deadline time.Time) bool {
	for {
		milliseconds := (max(time.Until(deadline), 0) + time.Millisecond - 1) / time.Millisecond
		descriptors := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLHUP}}
		n, err := unix.Poll(descriptors, int(milliseconds))
		if err == unix.EINTR {
			continue
		}
		return err == nil && n == 1 && descriptors[0].Revents&unix.POLLHUP != 0
	}
}

// readEverythingThatTheProgramWrote returns once the worker has read what
// is in each output. runToItsEnd calls it once no process holds the outputs
// open, or once it has stopped waiting for those that do: the worker then
// stops waiting for each output's end, reads what is in it, and waits for
// nothing more.
func readEverythingThatTheProgramWrote(ctx context.Context, outputs ...*programOutput) {
	for _, output := range outputs {
		_ = output.read.SetReadDeadline(time.Now())
	}
	if hooks := programOutputHooks(ctx); hooks.afterSettingReadDeadlines != nil {
		hooks.afterSettingReadDeadlines()
	}
	for _, output := range outputs {
		<-output.ended
	}
}

// failureTextFromStandardError is what a failure says of a failed command
// whose standard output is data, such as docker inspect, docker compose
// config, podman inspect, and skopeo inspect: the end of what the command
// wrote to standard error, where it explains its failure, or else what the
// process itself failed with. Standard output never becomes part of the
// text: there such a command prints the environment values of a container,
// and a task's reason is not private.
func failureTextFromStandardError(r protocol.Result) string {
	if detail := lastLines(strings.TrimSpace(r.Stderr), 20); detail != "" {
		return detail
	}
	return r.Reason
}
